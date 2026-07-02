/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package inventory

import (
	"context"
	"fmt"
	"maps"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/names"

	"github.com/Mellanox/rdmamap"
	"github.com/jaypipes/ghw"
	"github.com/vishvananda/netlink"
	"golang.org/x/time/rate"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/internal/nlwrap"
)

const (
	// defaultMinPollInterval is the default minimum interval between two
	// consecutive polls of the inventory.
	defaultMinPollInterval = 2 * time.Second
	// defaultMaxPollInterval is the default maximum interval between two
	// consecutive polls of the inventory.
	defaultMaxPollInterval = 1 * time.Minute
	// defaultPollBurst is the default number of polls that can be run in a
	// burst.
	defaultPollBurst = 5
)

var (
	// ignoredInterfaceNames is a set of network interface names that are typically
	// created by CNI plugins or are otherwise not relevant for DRA resource exposure.
	ignoredInterfaceNames = sets.New("cilium_net", "cilium_host", "docker0")

	// nonNetdevDrivers is the set of well-known kernel drivers that bind
	// to PCI network devices without creating a kernel netdev or RDMA link
	// (userspace I/O and passthrough drivers). See
	// isAllocatableNetworkDevice for why this list is used.
	nonNetdevDrivers = sets.New("vfio-pci", "uio_pci_generic", "igb_uio", "pci-stub")
)

type DB struct {
	instance cloudprovider.CloudInstance
	profProv cloudprovider.ProfileProvider
	// TODO: it is not common but may happen in edge cases that the default
	// gateway changes revisit once we have more evidence this can be a
	// potential problem or break some use cases.
	gwInterfaces sets.Set[string]

	mu sync.RWMutex
	// deviceStore is an in-memory cache of the available devices on the node.
	// It is keyed by the normalized PCI address of the device. The value is a
	// resourceapi.Device object that contains the device's attributes.
	// The deviceStore is periodically updated by the Run method.
	deviceStore map[string]resourceapi.Device
	// deviceConfigStore caches cloud-provider network configuration per device.
	// This helps us avoid repeatedly querying the provider APIs. Keyed by device name.
	deviceConfigStore map[string]*apis.NetworkConfig

	rateLimiter     *rate.Limiter
	maxPollInterval time.Duration
	notifications   chan []resourceapi.Device
	rescanCh        chan struct{}
	hasDevices      bool

	// moveIBInterfaces controls whether IPoIB network interfaces are
	// associated with their PCI devices. When true (default), IPoIB interfaces
	// are treated like regular network interfaces and moved into pod namespaces.
	// When false, IPoIB interfaces are skipped and the underlying device is
	// exposed as an IB-only RDMA device.
	moveIBInterfaces bool
}

type Option func(*DB)

func WithRateLimiter(limiter *rate.Limiter) Option {
	return func(db *DB) {
		db.rateLimiter = limiter
	}
}

func WithMaxPollInterval(d time.Duration) Option {
	return func(db *DB) {
		db.maxPollInterval = d
	}
}

func WithMoveIBInterfaces(move bool) Option {
	return func(db *DB) {
		db.moveIBInterfaces = move
	}
}

func WithCloudInstance(instance cloudprovider.CloudInstance) Option {
	return func(db *DB) {
		db.instance = instance
	}
}

func WithProfileProvider(profProv cloudprovider.ProfileProvider) Option {
	return func(db *DB) {
		db.profProv = profProv
	}
}

func New(opts ...Option) *DB {
	db := &DB{

		deviceStore:       map[string]resourceapi.Device{},
		deviceConfigStore: map[string]*apis.NetworkConfig{},
		rateLimiter:       rate.NewLimiter(rate.Every(defaultMinPollInterval), defaultPollBurst),
		notifications:     make(chan []resourceapi.Device),
		rescanCh:          make(chan struct{}, 1),
		maxPollInterval:   defaultMaxPollInterval,
		moveIBInterfaces:  true,
	}
	for _, o := range opts {
		o(db)
	}
	return db
}

func (db *DB) Run(ctx context.Context) error {
	defer close(db.notifications)

	// Resources are published periodically or if there is a netlink notification
	// indicating a new interfaces was added or changed
	nlChannel := make(chan netlink.LinkUpdate)
	doneCh := make(chan struct{})
	defer close(doneCh)
	if err := netlink.LinkSubscribe(nlChannel, doneCh); err != nil {
		klog.Error(err, "error subscribing to netlink interfaces, only syncing periodically", "interval", db.maxPollInterval.String())
	}

	db.gwInterfaces = getExcludedUplinkInterfaces()
	klog.V(2).Infof("Excluded uplink interfaces and children: %v", db.gwInterfaces.UnsortedList())

	for {
		err := db.rateLimiter.Wait(ctx)
		if err != nil {
			klog.Error(err, "unexpected rate limited error trying to get system interfaces")
		}

		filteredDevices := db.scan()
		if len(filteredDevices) > 0 || db.hasDevices {
			db.hasDevices = len(filteredDevices) > 0
			db.notifications <- filteredDevices
		}

		select {
		// trigger a reconcile
		case <-nlChannel:
			// drain the channel so we only sync once
			for len(nlChannel) > 0 {
				<-nlChannel
			}
		case <-db.rescanCh:
			klog.V(3).Infof("Triggering inventory rescan due to manual request")
		case <-time.After(db.maxPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// scan discovers the available devices on the node.
// It discovers PCI, network, and RDMA devices, adds cloud attributes,
// filters out default interfaces, and updates the device store.
func (db *DB) scan() []resourceapi.Device {
	pciInfo, err := ghw.PCI(
		ghw.WithDisableTools(),
	)
	if err != nil {
		klog.Errorf("Could not get PCI devices: %v", err)
	}

	// Phase 1: Discovery — find devices, set identity attributes only
	// (PCIAddress, RDMADevice, ifName, MAC, …).
	devices := db.discoverPCIDevices(pciInfo)
	devices = db.discoverStandaloneRDMADevices(devices)
	devices = db.discoverNetworkInterfaces(devices)

	// Phase 2: Enrichment — set derived PCI and RDMA attributes uniformly
	// for all devices, regardless of which discovery path found them.
	devices = db.addPCIAttributes(devices, pciInfo)
	devices = db.addRDMAAttributes(devices)
	devices = db.addCloudAttributes(devices)

	// Remove default interface.
	filteredDevices := []resourceapi.Device{}
	for _, device := range devices {
		ifName := device.Attributes[apis.AttrInterfaceName].StringValue
		if ifName != nil && db.gwInterfaces.Has(string(*ifName)) {
			klog.V(4).Infof("Ignoring interface %s from discovery since it is an uplink interface or a child of one", *ifName)
			continue
		}
		filteredDevices = append(filteredDevices, device)
	}

	sort.Slice(filteredDevices, func(i, j int) bool {
		return filteredDevices[i].Name < filteredDevices[j].Name
	})

	klog.V(4).Infof("Found %d devices", len(filteredDevices))
	db.updateDeviceStore(filteredDevices)
	return filteredDevices
}

func (db *DB) GetResources(ctx context.Context) <-chan []resourceapi.Device {
	return db.notifications
}

// RequestRescan queues a non-blocking rescan of the inventory. If a rescan is
// already pending the call is a no-op. This is used when RDMA devices may have
// returned to the host namespace via kernel namespace cleanup rather than an
// explicit move, so there is no NEWLINK event to trigger the normal path.
func (db *DB) RequestRescan() {
	select {
	case db.rescanCh <- struct{}{}:
	default:
	}
}

func (db *DB) discoverPCIDevices(pciInfo *ghw.PCIInfo) []resourceapi.Device {
	devices := []resourceapi.Device{}

	if pciInfo == nil {
		return devices
	}

	for _, pciDev := range pciInfo.Devices {
		if !isNetworkDevice(pciDev) {
			continue
		}
		if !isAllocatableNetworkDevice(pciDev) {
			klog.Warningf("PCI network device %s is bound to driver %q which does not provide a netdev; not publishing it", pciDev.Address, pciDev.Driver)
			continue
		}
		device := resourceapi.Device{
			Name:       names.NormalizePCIAddress(pciDev.Address),
			Attributes: make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute),
			Capacity:   make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity),
		}
		device.Attributes[apis.AttrPCIAddress] = resourceapi.DeviceAttribute{StringValue: &pciDev.Address}
		devices = append(devices, device)
	}
	return devices
}

// discoveryNetworkInterfaces updates the devices based on information retried
// from network interfaces. For each network interface, the two possible
// outcomes are:
//   - If the network interface is associated with some parent PCI device, the
//     existing PCI device is modified with additional attributes related to the
//     network interface.
//   - For Network interfaces which are not associated with a PCI Device (like
//     virtual interfaces), they are added as their own device.
func (db *DB) discoverNetworkInterfaces(pciDevices []resourceapi.Device) []resourceapi.Device {
	links, err := nlwrap.LinkList()
	if err != nil {
		klog.Errorf("Could not list network interfaces: %v", err)
		return pciDevices
	}

	pciDeviceMap := make(map[string]*resourceapi.Device)
	for i := range pciDevices {
		pciDeviceMap[pciDevices[i].Name] = &pciDevices[i]
	}

	otherDevices := []resourceapi.Device{}

	for _, link := range links {
		ifName := link.Attrs().Name
		if ignoredInterfaceNames.Has(ifName) {
			klog.V(4).Infof("Network Interface %s is in the list of ignored interfaces, excluding it from discovery", ifName)
			continue
		}

		// skip loopback interfaces
		if link.Attrs().Flags&net.FlagLoopback != 0 {
			klog.V(4).Infof("Network Interface %s is a loopback interface, excluding it from discovery", ifName)
			continue
		}

		// When moveIBInterfaces is false, skip IPoIB interfaces.
		// The underlying PCI device will be discovered as an IB-only RDMA
		// device (no netdev) via addRDMAAttributes. Associating the IPoIB
		// netdev with the PCI device would mask the IB-only nature of the
		// device and prevent correct RDMA char device injection into pods.
		// When moveIBInterfaces is true (default), IPoIB interfaces
		// are associated with their PCI device so they can be moved into pod namespace.
		if link.Type() == "ipoib" && !db.moveIBInterfaces {
			klog.V(4).Infof("Network Interface %s is IPoIB, skipping netdev association (will be discovered as IB-only RDMA device)", ifName)
			continue
		}

		pciAddr, err := pciAddressForNetInterface(ifName)
		if err == nil {
			// It's a PCI device.

			normalizedAddress := names.NormalizePCIAddress(pciAddr.String())
			var exists bool
			device, exists := pciDeviceMap[normalizedAddress]
			if !exists {
				// We don't expect this to happen.
				klog.Errorf("Network interface %s has PCI address %q, but it was not found in initial PCI scan.", ifName, pciAddr)
				continue
			}
			addLinkAttributes(device, link)
		} else {
			// Not a PCI device.

			if !isVirtual(ifName, sysnetPath) {
				// If we failed to identify the PCI address of the network
				// interface and the network interface is also not a virtual
				// device, use a best-effort strategy where the network
				// interface is assumed to be virtual.
				klog.Warningf("PCI address not found for non-virtual interface %s, proceeding as if it were virtual. Error: %v", ifName, err)
			}
			newDevice := &resourceapi.Device{
				Name:       names.NormalizeInterfaceName(ifName),
				Attributes: make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute),
			}
			addLinkAttributes(newDevice, link)
			otherDevices = append(otherDevices, *newDevice)
		}
	}

	return append(pciDevices, otherDevices...)
}

// buildIPList joins ips with commas, stopping before any address that would
// push the result past maxBytes. It returns the (possibly truncated) joined
// string and the number of addresses that were included.
func buildIPList(ips []string, maxBytes int) (string, int) {
	if len(ips) == 0 {
		return "", 0
	}

	var builder strings.Builder
	kept := 0

	for i, ip := range ips {
		addedLength := len(ip)
		if i > 0 {
			addedLength++ // comma separator
		}

		if builder.Len()+addedLength > maxBytes {
			break
		}

		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(ip)
		kept++
	}

	return builder.String(), kept
}

func addLinkAttributes(device *resourceapi.Device, link netlink.Link) {
	ifName := link.Attrs().Name
	device.Attributes[apis.AttrInterfaceName] = resourceapi.DeviceAttribute{StringValue: &ifName}
	device.Attributes[apis.AttrMac] = resourceapi.DeviceAttribute{StringValue: ptr.To(link.Attrs().HardwareAddr.String())}
	device.Attributes[apis.AttrMTU] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(link.Attrs().MTU))}
	device.Attributes[apis.AttrEncapsulation] = resourceapi.DeviceAttribute{StringValue: ptr.To(link.Attrs().EncapType)}
	device.Attributes[apis.AttrAlias] = resourceapi.DeviceAttribute{StringValue: ptr.To(link.Attrs().Alias)}
	device.Attributes[apis.AttrState] = resourceapi.DeviceAttribute{StringValue: ptr.To(link.Attrs().OperState.String())}
	device.Attributes[apis.AttrType] = resourceapi.DeviceAttribute{StringValue: ptr.To(link.Type())}

	v4 := sets.Set[string]{}
	v6 := sets.Set[string]{}
	if ips, err := nlwrap.AddrList(link, netlink.FAMILY_ALL); err == nil && len(ips) > 0 {
		for _, address := range ips {
			if !address.IP.IsGlobalUnicast() {
				continue
			}

			if address.IP.To4() == nil && address.IP.To16() != nil {
				v6.Insert(address.IPNet.String())
			} else if address.IP.To4() != nil {
				v4.Insert(address.IPNet.String())
			}
		}
		// DRA enforces a per-attribute string limit (see
		// resourceapi.DeviceAttributeMaxValueLength). Interfaces like the
		// kube-proxy IPVS dummy (kube-ipvs0) accumulate every cluster
		// ServiceIP and would overflow this limit, causing the whole slice to
		// be rejected. Build the attribute incrementally and stop once the
		// next address would push us past the cap. Until List-typed device
		// attributes land (kubernetes/enhancements#5491) this prefix is the
		// best we can publish; sort first so the truncation is deterministic.
		if v4.Len() > 0 {
			ips := v4.UnsortedList()
			sort.Strings(ips)
			joined, kept := buildIPList(ips, resourceapi.DeviceAttributeMaxValueLength)
			if joined != "" {
				device.Attributes[apis.AttrIPv4] = resourceapi.DeviceAttribute{StringValue: ptr.To(joined)}
			}
			if kept < len(ips) {
				klog.V(4).Infof("Truncated %s attribute on %s: kept %d of %d addresses to stay within DRA's %d-byte limit",
					apis.AttrIPv4, ifName, kept, len(ips), resourceapi.DeviceAttributeMaxValueLength)
			}
		}
		if v6.Len() > 0 {
			ips := v6.UnsortedList()
			sort.Strings(ips)
			joined, kept := buildIPList(ips, resourceapi.DeviceAttributeMaxValueLength)
			if joined != "" {
				device.Attributes[apis.AttrIPv6] = resourceapi.DeviceAttribute{StringValue: ptr.To(joined)}
			}
			if kept < len(ips) {
				klog.V(4).Infof("Truncated %s attribute on %s: kept %d of %d addresses to stay within DRA's %d-byte limit",
					apis.AttrIPv6, ifName, kept, len(ips), resourceapi.DeviceAttributeMaxValueLength)
			}
		}
	}

	isEbpf := false
	filterNames, ok := getTcFilters(link)
	if ok {
		isEbpf = true
		device.Attributes[apis.AttrTCFilterNames] = resourceapi.DeviceAttribute{StringValue: ptr.To(strings.Join(filterNames, ","))}
	}

	programNames, ok := getTcxFilters(link)
	if ok {
		isEbpf = true
		device.Attributes[apis.AttrTCXProgramNames] = resourceapi.DeviceAttribute{StringValue: ptr.To(strings.Join(programNames, ","))}
	}
	device.Attributes[apis.AttrEBPF] = resourceapi.DeviceAttribute{BoolValue: &isEbpf}

	isSRIOV := sriovTotalVFs(ifName) > 0
	device.Attributes[apis.AttrSRIOV] = resourceapi.DeviceAttribute{BoolValue: &isSRIOV}
	if isSRIOV {
		vfs := int64(sriovNumVFs(ifName))
		device.Attributes[apis.AttrSRIOVVfs] = resourceapi.DeviceAttribute{IntValue: &vfs}
	}

	isSriovVirtualFunction := isSriovVf(ifName, sysnetPath)
	if isSriovVirtualFunction {
		device.Attributes[apis.AttrIsSriovVf] = resourceapi.DeviceAttribute{BoolValue: &isSriovVirtualFunction}
	}

	if isVirtual(ifName, sysnetPath) {
		device.Attributes[apis.AttrVirtual] = resourceapi.DeviceAttribute{BoolValue: ptr.To(true)}
	} else {
		device.Attributes[apis.AttrVirtual] = resourceapi.DeviceAttribute{BoolValue: ptr.To(false)}
	}
}

func (db *DB) addRDMAAttributes(devices []resourceapi.Device) []resourceapi.Device {
	for i := range devices {
		isRDMA := false
		if ifName := devices[i].Attributes[apis.AttrInterfaceName].StringValue; ifName != nil && *ifName != "" {
			// Try rdmamap library first
			isRDMA = rdmamap.IsRDmaDeviceForNetdevice(*ifName)

			// Fallback to sysfs check if rdmamap fails. This is particularly
			// needed for InfiniBand interfaces where rdmamap has a bug comparing
			// against node GUID instead of port GUID:
			// https://github.com/Mellanox/rdmamap/issues/15
			if !isRDMA {
				isRDMA = isRdmaDeviceInSysfs(*ifName)
			}
		} else if pciAddr := devices[i].Attributes[apis.AttrPCIAddress].StringValue; pciAddr != nil && *pciAddr != "" {
			// IB-only device: has RDMA capability but no netdev interface.
			// If AttrRDMADevice was already set (e.g., by discoverStandaloneRDMADevices),
			// trust it directly; otherwise look it up via rdmamap.
			if rdmaDevAttr, ok := devices[i].Attributes[apis.AttrRDMADevice]; ok && rdmaDevAttr.StringValue != nil && *rdmaDevAttr.StringValue != "" {
				isRDMA = true
			} else {
				rdmaDevices := rdmamap.GetRdmaDevicesForPcidev(*pciAddr)
				isRDMA = len(rdmaDevices) != 0
				if isRDMA {
					rdmaDevName := rdmaDevices[0]
					devices[i].Attributes[apis.AttrRDMADevice] = resourceapi.DeviceAttribute{StringValue: &rdmaDevName}
				}
			}
		}
		devices[i].Attributes[apis.AttrRDMA] = resourceapi.DeviceAttribute{BoolValue: &isRDMA}
	}
	return devices
}

func (db *DB) addCloudAttributes(devices []resourceapi.Device) []resourceapi.Device {
	for i := range devices {
		device := &devices[i]
		maps.Copy(device.Attributes, db.getProviderAttributes(device, db.instance))
	}
	return devices
}

func (db *DB) getProviderAttributes(device *resourceapi.Device, instance cloudprovider.CloudInstance) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	if instance == nil {
		klog.Warningf("instance metadata is nil, cannot get provider attributes.")
		return nil
	}

	if device == nil {
		klog.Warningf("device is nil, cannot get provider attributes.")
		return nil
	}

	id := cloudprovider.DeviceIdentifiers{
		Name: device.Name,
	}
	if macAttr, ok := device.Attributes[apis.AttrMac]; ok && macAttr.StringValue != nil {
		id.MAC = *macAttr.StringValue
	}
	if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
		id.PCIAddress = *pciAttr.StringValue
	}

	return instance.GetDeviceAttributes(id)
}

func (db *DB) updateDeviceStore(devices []resourceapi.Device) {
	deviceStore := map[string]resourceapi.Device{}
	deviceConfigStore := map[string]*apis.NetworkConfig{}

	for _, device := range devices {
		deviceStore[device.Name] = device

		// Cache the configuration if the provider returns one.
		if db.instance != nil {
			id := cloudprovider.DeviceIdentifiers{
				Name: device.Name,
			}
			if macAttr, ok := device.Attributes[apis.AttrMac]; ok && macAttr.StringValue != nil {
				id.MAC = *macAttr.StringValue
			}
			if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
				id.PCIAddress = *pciAttr.StringValue
			}

			if conf := db.instance.GetDeviceConfig(id); conf != nil {
				deviceConfigStore[device.Name] = conf
			}
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.deviceStore = deviceStore
	db.deviceConfigStore = deviceConfigStore
}

func (db *DB) GetDevice(deviceName string) (resourceapi.Device, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	device, exists := db.deviceStore[deviceName]
	return device, exists
}

func (db *DB) getProfileProvider() cloudprovider.ProfileProvider {
	return db.profProv
}

// GetProfileConfig resolves a dynamic profile by querying the underlying cloud provider.
func (db *DB) GetProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	p := db.getProfileProvider()
	if p == nil {
		return nil, fmt.Errorf("current cloud provider does not support dynamic profiles")
	}

	db.mu.RLock()
	device, exists := db.deviceStore[deviceName]
	db.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("device %s not found in inventory", deviceName)
	}

	id := cloudprovider.DeviceIdentifiers{Name: deviceName}
	if macAttr, ok := device.Attributes[apis.AttrMac]; ok && macAttr.StringValue != nil {
		id.MAC = *macAttr.StringValue
	}
	if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
		id.PCIAddress = *pciAttr.StringValue
	}

	return p.GetProfileConfig(id, claimUID, config)
}

// ReleaseProfileConfig delegates the teardown of a dynamic profile to the cloud provider.
func (db *DB) ReleaseProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
	p := db.getProfileProvider()
	if p == nil {
		return nil // Provider doesn't support profiles, nothing to release
	}

	db.mu.RLock()
	device, exists := db.deviceStore[deviceName]
	db.mu.RUnlock()

	id := cloudprovider.DeviceIdentifiers{Name: deviceName}
	if exists {
		// Device might have been removed from the node during teardown,
		// but we populate identifiers if we still have them to aid cleanup.
		if macAttr, ok := device.Attributes[apis.AttrMac]; ok && macAttr.StringValue != nil {
			id.MAC = *macAttr.StringValue
		}
		if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
			id.PCIAddress = *pciAttr.StringValue
		}
	}

	return p.ReleaseProfileConfig(id, claimUID, config)
}

// GetDeviceConfig returns the network configuration associated with the device, if any.
func (db *DB) GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	conf, exists := db.deviceConfigStore[deviceName]
	return conf, exists
}

// GetNetInterfaceName returns the network interface name for a given device. It
// first attempts to retrieve the name from the local device store. If the
// device is not found, it triggers a rescan of the system's devices and retries
// the lookup. This can happen when a device was recently released by a previous
// pod and a scan had not happened yet. This ensures that the function can find
// newly added devices that were not present in the store at the time of the
// initial call.
func (db *DB) GetNetInterfaceName(deviceName string) (string, error) {
	name, err := db.getNetInterfaceNameWithoutRescan(deviceName)
	if err != nil {
		klog.V(3).Infof("Device %q not found in local store, rescanning.", deviceName)
		db.scan()
		name, err = db.getNetInterfaceNameWithoutRescan(deviceName)
	}
	return name, err
}

// getNetInterfaceNameWithoutRescan returns the network interface name for a
// given device from the local device store without triggering a rescan if the
// device is not found.
func (db *DB) getNetInterfaceNameWithoutRescan(deviceName string) (string, error) {
	device, exists := db.GetDevice(deviceName)
	if !exists {
		return "", fmt.Errorf("device %s not found in store", deviceName)
	}
	if device.Attributes[apis.AttrInterfaceName].StringValue == nil {
		return "", fmt.Errorf("device %s has no interface name in local store", deviceName)
	}
	return *device.Attributes[apis.AttrInterfaceName].StringValue, nil
}

// IsIBOnlyDevice returns true if the device has RDMA capability but no netdev
// interface (i.e. an InfiniBand-only device). Derived from existing attributes:
// a device with a non-empty rdmaDevice and no ifName is IB-only.
func (db *DB) IsIBOnlyDevice(deviceName string) bool {
	device, exists := db.GetDevice(deviceName)
	if !exists {
		return false
	}
	rdmaAttr := device.Attributes[apis.AttrRDMADevice]
	ifAttr := device.Attributes[apis.AttrInterfaceName]
	return rdmaAttr.StringValue != nil && *rdmaAttr.StringValue != "" &&
		(ifAttr.StringValue == nil || *ifAttr.StringValue == "")
}

// GetRDMADeviceName returns the RDMA link name (e.g. "mlx5_0") for an IB-only
// device. It returns an error if the device is not found or has no RDMA device
// name recorded.
func (db *DB) GetRDMADeviceName(deviceName string) (string, error) {
	device, exists := db.GetDevice(deviceName)
	if !exists {
		return "", fmt.Errorf("device %s not found in store", deviceName)
	}
	attr, ok := device.Attributes[apis.AttrRDMADevice]
	if !ok || attr.StringValue == nil {
		return "", fmt.Errorf("device %s has no RDMA device name in local store", deviceName)
	}
	return *attr.StringValue, nil
}

// isNetworkDevice checks the class is 0x2, defined for all types of network controllers
// https://pcisig.com/sites/default/files/files/PCI_Code-ID_r_1_11__v24_Jan_2019.pdf
func isNetworkDevice(dev *ghw.PCIDevice) bool {
	return dev.Class.ID == "02"
}

// isAllocatableNetworkDevice reports whether dranet can ever prepare this
// PCI network device for a pod. A device that is allocatable is either
// providing a kernel netdev or RDMA link in some namespace; a device that is
// not has nothing dranet can move and would trap any pod allocated to it in
// FailedPrepareDynamicResources.
//
// The natural test would be "does /sys/bus/pci/devices/$BDF/net have any
// entries", but those entries are netns-tagged and only visible from the
// namespace the netdev currently lives in (see the kernel's
// Documentation/networking/sysfs-tagging.rst). A device whose netdev has
// already been moved into a pod's namespace therefore looks identical from
// the host to a device whose driver creates no netdev at all, and we must
// keep publishing the former so Cluster Autoscaler can size new nodes from
// the running ResourceSlice.
//
// The kernel driver bound at /sys/bus/pci/devices/$BDF/driver is the one
// signal that distinguishes the two: it is not namespaced and stays bound
// while the netdev is in another namespace. We treat the device as not
// allocatable if no driver is bound, or if the bound driver is one of the
// well-known userspace/passthrough drivers that never create a netdev or
// RDMA link.
func isAllocatableNetworkDevice(dev *ghw.PCIDevice) bool {
	if dev.Driver == "" {
		return false
	}
	return !nonNetdevDrivers.Has(dev.Driver)
}

// discoverStandaloneRDMADevices finds RDMA devices in /sys/class/infiniband/
// that were not already discovered through the PCI class 0x02 scan. This
// handles devices like eRDMA (PCI class 0x00ff) whose PCI class does not match
// the standard network device class. Each standalone RDMA device is exposed as
// an IB-only device (no netdev) with its RDMA device name.
//
// Only identity attributes (PCIAddress, RDMADevice) are set here; PCI
// attributes like NUMA node, vendor, and product are populated uniformly by
// enrichPCIDeviceAttributes in the enrichment phase.
//
// Unlike /sys/class/net, /sys/class/infiniband entries are not netns-tagged:
// they remain visible from the host even after the device is allocated to a pod,
// so this scan is stable across the full device lifecycle.
func (db *DB) discoverStandaloneRDMADevices(devices []resourceapi.Device) []resourceapi.Device {
	knownPCIAddresses := sets.New[string]()
	for _, device := range devices {
		if pciAttr, ok := device.Attributes[apis.AttrPCIAddress]; ok && pciAttr.StringValue != nil {
			knownPCIAddresses.Insert(names.NormalizePCIAddress(*pciAttr.StringValue))
		}
	}

	rdmaDevNames := rdmamap.GetRdmaDeviceList()
	for _, rdmaDevName := range rdmaDevNames {
		pciAddr, err := pciAddressForRDMADevice(sysInfinibandPath, rdmaDevName)
		if err != nil {
			klog.Warningf("Skipping RDMA device %s: %v", rdmaDevName, err)
			continue
		}
		normalizedAddr := names.NormalizePCIAddress(pciAddr.String())
		if knownPCIAddresses.Has(normalizedAddr) {
			continue
		}

		klog.V(2).Infof("Found standalone RDMA device %s at PCI %s", rdmaDevName, pciAddr)
		device := resourceapi.Device{
			Name:       normalizedAddr,
			Attributes: make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute),
			Capacity:   make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity),
		}
		device.Attributes[apis.AttrPCIAddress] = resourceapi.DeviceAttribute{StringValue: ptr.To(pciAddr.String())}
		device.Attributes[apis.AttrRDMADevice] = resourceapi.DeviceAttribute{StringValue: ptr.To(rdmaDevName)}

		devices = append(devices, device)
		knownPCIAddresses.Insert(normalizedAddr)
	}

	return devices
}

// addPCIAttributes sets PCI-related attributes (NUMA node, vendor, product,
// subsystem, PCIe root) for every device that has a PCI address. This runs
// after all discovery steps so that attributes are populated uniformly
// regardless of which discovery path found the device.
//
// ghw is the sole data source: it reads vendor/product from pcidb and NUMA
// from /sys/bus/pci/devices/<BDF>/numa_node for all PCI devices regardless of
// class. If a device is not found in ghw (e.g., modalias parsing failure),
// its PCI attributes are simply left unset — the device is still published
// with its identity attributes from discovery.
func (db *DB) addPCIAttributes(devices []resourceapi.Device, pciInfo *ghw.PCIInfo) []resourceapi.Device {
	pciMap := make(map[string]*ghw.PCIDevice)
	if pciInfo != nil {
		for _, d := range pciInfo.Devices {
			pciMap[names.NormalizePCIAddress(d.Address)] = d
		}
	}

	for i := range devices {
		pciAddrAttr, ok := devices[i].Attributes[apis.AttrPCIAddress]
		if !ok || pciAddrAttr.StringValue == nil {
			continue
		}
		normalizedAddr := names.NormalizePCIAddress(*pciAddrAttr.StringValue)

		pciDev, inGhw := pciMap[normalizedAddr]
		if !inGhw {
			continue
		}

		if _, hasAttr := devices[i].Attributes[apis.AttrNUMANode]; !hasAttr && pciDev.Node != nil {
			devices[i].Attributes[apis.AttrNUMANode] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(pciDev.Node.ID))}
		}
		if _, hasAttr := devices[i].Attributes[apis.AttrPCIVendor]; !hasAttr && pciDev.Vendor != nil {
			devices[i].Attributes[apis.AttrPCIVendor] = resourceapi.DeviceAttribute{StringValue: &pciDev.Vendor.Name}
		}
		if _, hasAttr := devices[i].Attributes[apis.AttrPCIDevice]; !hasAttr && pciDev.Product != nil {
			devices[i].Attributes[apis.AttrPCIDevice] = resourceapi.DeviceAttribute{StringValue: &pciDev.Product.Name}
		}
		if _, hasAttr := devices[i].Attributes[apis.AttrPCISubsystem]; !hasAttr && pciDev.Subsystem != nil {
			devices[i].Attributes[apis.AttrPCISubsystem] = resourceapi.DeviceAttribute{StringValue: &pciDev.Subsystem.ID}
		}

		if _, hasAttr := devices[i].Attributes[deviceattribute.StandardDeviceAttributePCIeRoot]; !hasAttr {
			pcieRootAttr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(*pciAddrAttr.StringValue)
			if err != nil {
				klog.V(4).Infof("Could not get PCIe root for PCI device %s: %v", normalizedAddr, err)
			} else {
				devices[i].Attributes[pcieRootAttr.Name] = pcieRootAttr.Value
			}
		}
	}
	return devices
}
