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

package oke

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	OKEAttrPrefix = "oke.dra.net"

	// RDMA topology attributes (from /opc/v2/host/).
	AttrOKEHPCIslandId     = OKEAttrPrefix + "/" + "hpcIslandId"
	AttrOKENetworkBlockId  = OKEAttrPrefix + "/" + "networkBlockId"
	AttrOKELocalBlockId    = OKEAttrPrefix + "/" + "localBlockId"
	AttrOKERackId          = OKEAttrPrefix + "/" + "rackId"
	AttrOKEGpuMemoryFabric = OKEAttrPrefix + "/" + "gpuMemoryFabricId"

	// Instance shape (from /opc/v2/instance/).
	AttrOKEShape = OKEAttrPrefix + "/" + "shape"

	// RDMA fabric attributes (from the rdmaFabricData object in /opc/v2/host/).
	AttrOKERDMAFabricIPv6   = OKEAttrPrefix + "/" + "rdmaFabricIpv6"
	AttrOKERDMAFabricPlanes = OKEAttrPrefix + "/" + "rdmaFabricPlanes"

	// imdsEndpoint is the Oracle Cloud Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/opc/v2"

	imdsInitialRetryInterval = 1 * time.Second
	imdsInitialWait          = 15 * time.Second
	imdsRefreshInterval      = 5 * time.Minute
	// imdsRequestTimeout limits each request, at startup and in the background.
	imdsRequestTimeout = 5 * time.Second

	// Oracle Cloud Agent names the RDMA fabric interfaces rdmaN.
	rdmaNicPrefix = "rdma"

	// okeRDMAProfile is advertised for every Ethernet RDMA NIC and resolved
	// by this provider into an IPvlan child with a deterministic address.
	okeRDMAProfile = "oke-rdma"
	// okeRDMASBRTableBase plus the RDMA NIC index is the routing table of a child.
	// TableIDForName hashes into 1000 to 1999 and can collide between two
	// RDMA NICs of one pod; the RDMA NIC index cannot.
	okeRDMASBRTableBase = 100
	// okeMaxIPv4RDMANics is the largest RDMA NIC count of an OKE shape on an
	// IPv4 RDMA fabric. It bounds the RDMA NIC index, and with it the routing
	// tables 100 to 115.
	okeMaxIPv4RDMANics = 16
	// okeRDMAParentIPv4CIDR is the OCA RDMA network when rdma_network.json
	// is not mounted.
	okeRDMAParentIPv4CIDR = "10.224.0.0/12"
	// okeRDMAChildIPv4CIDR holds one child block per parent at the parent's offset.
	// A /12 holds all 16 RDMA NICs of a /16 subnet, the largest OCI subnet. It
	// avoids the parent 10.224.0.0/12 and the OKE pod CIDR 10.240.0.0/12.
	okeRDMAChildIPv4CIDR = "10.208.0.0/12"
	// okeChildrenPerNIC is the size of the block of child addresses that each
	// RDMA NIC owns. The child is the first address of the block.
	okeChildrenPerNIC = 1

	// Oracle Cloud Agent files that describe the RDMA address assignment.
	ocaRDMANetworkFileName     = "rdma_network.json"
	ocaRDMAFeaturesFileName    = "rdma_features_enable.json"
	ocaIPv4AddressModeClassic  = "classic"
	ocaIPv4AddressModeSerial   = "host-serial"
	ocaIPv4AddressModeTopology = "topology"
)

// Tests point these at a temporary directory or a fake.
var (
	sysClassNet         = "/sys/class/net"
	sysfsPCIDevices     = "/sys/bus/pci/devices"
	procSysNetIPv4Conf  = "/proc/sys/net/ipv4/conf"
	ocaConfigDir        = "/etc/oracle-cloud-agent/plugins/oci-hpc/oci-hpc-configure"
	ibCoreNetnsModeFile = "/sys/module/ib_core/parameters/netns_mode"
	interfaceAddresses  = func(ifName string) ([]net.Addr, error) {
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			return nil, err
		}
		return iface.Addrs()
	}
)

var okeRDMAChildIPv4Range = netip.MustParsePrefix(okeRDMAChildIPv4CIDR)

// imdsHostRDMATopologyData contains the RDMA topology fields from the
// /opc/v2/host/ response. IMDS populates them only for instances in a
// dedicated pool, with RDMA topology data enabled for the tenancy.
type imdsHostRDMATopologyData struct {
	CustomerGpuMemoryFabric string `json:"customerGpuMemoryFabric"`
	CustomerHPCIslandId     string `json:"customerHPCIslandId"`
	CustomerHostId          string `json:"customerHostId"`
	CustomerLocalBlock      string `json:"customerLocalBlock"`
	CustomerNetworkBlock    string `json:"customerNetworkBlock"`
}

// imdsRDMAFabricData contains the RDMA fabric fields embedded in the
// /opc/v2/host/ response.
type imdsRDMAFabricData struct {
	IPv6   *bool  `json:"ipv6"`
	Planes *int64 `json:"planes"`
}

// imdsHostMetadata contains the fields used from the /opc/v2/host/ response.
type imdsHostMetadata struct {
	NetworkBlockId   string                    `json:"networkBlockId"`
	RackId           string                    `json:"rackId"`
	RDMATopologyData *imdsHostRDMATopologyData `json:"rdmaTopologyData"`
	RDMAFabricData   *imdsRDMAFabricData       `json:"rdmaFabricData"`
}

// imdsInstanceMetadata contains the fields we care about from /opc/v2/instance/.
type imdsInstanceMetadata struct {
	Shape string `json:"shape"`
}

// imdsVNICMetadata contains the fields used from one /opc/v2/vnics/ entry.
type imdsVNICMetadata struct {
	PrivateIP       string `json:"privateIp"`
	SubnetCidrBlock string `json:"subnetCidrBlock"`
}

// rdmaFabric describes the RDMA fabric of the instance.
type rdmaFabric struct {
	IPv6   bool
	Planes int64
}

// primaryVNIC is the IPv4 address and subnet of the primary VNIC. OCA derives
// the RDMA NIC addresses from them.
type primaryVNIC struct {
	IPv4   netip.Addr
	Subnet netip.Prefix
}

// ocaRDMAConfig is the input Oracle Cloud Agent used to assign the RDMA
// addresses, read from its optional configuration files.
type ocaRDMAConfig struct {
	parentIPv4Range netip.Prefix
	// ipv4AddressMode is empty when the features file was not read.
	ipv4AddressMode string
}

type ocaRDMANetworkFile struct {
	RDMANetwork []struct {
		DefaultSettings struct {
			RDMANetwork string `json:"rdma_network"`
		} `json:"default-settings"`
	} `json:"rdma-network"`
}

type ocaRDMAFeaturesFile struct {
	HostSerialIPv4 bool `json:"host_serial_based_rdma_ips"`
	TopologyIPv4   bool `json:"ipv4_topology_based_rdma_ips"`
}

// okeMetadata is one immutable snapshot of the instance metadata.
type okeMetadata struct {
	HPCIslandId    string
	NetworkBlockId string
	LocalBlockId   string
	RackId         string
	// GpuMemoryFabric is set only on shapes that support it.
	GpuMemoryFabric string
	Shape           string
	RDMAFabric      *rdmaFabric
	// PrimaryVNIC is read once, on a node with Ethernet RDMA NICs. It is nil
	// until that read succeeds.
	PrimaryVNIC *primaryVNIC
}

type metadataFetcher func(context.Context) (*okeMetadata, error)

var (
	_ cloudprovider.CloudInstance   = (*OKEInstance)(nil)
	_ cloudprovider.ProfileProvider = (*OKEInstance)(nil)
)

// OKEInstance holds the OKE instance metadata and refreshes it from IMDS in
// the background.
type OKEInstance struct {
	metadata      atomic.Pointer[okeMetadata]
	fetchMetadata metadataFetcher
	// The next two are set once in start, before the refresh loop starts.
	ocaConfig ocaRDMAConfig
	// addressFallback identifies RDMA NICs by their OCA address on a node
	// whose RDMA NICs do not carry the rdmaN name.
	addressFallback bool
	// layoutValidated is set once the classic host layout check has passed.
	layoutValidated      atomic.Bool
	initialRetryInterval time.Duration
	initialWait          time.Duration
	refreshInterval      time.Duration
}

func newOKEInstance(metadata *okeMetadata, fetch metadataFetcher) *OKEInstance {
	instance := &OKEInstance{
		fetchMetadata:        fetch,
		ocaConfig:            defaultOCARDMAConfig(),
		addressFallback:      true,
		initialRetryInterval: imdsInitialRetryInterval,
		initialWait:          imdsInitialWait,
		refreshInterval:      imdsRefreshInterval,
	}
	if metadata != nil {
		instance.metadata.Store(metadata)
	}
	return instance
}

// GetDeviceAttributes returns OKE-specific attributes for a device.
// These are node-level attributes applied to all devices since the OCI IMDS
// host endpoint exposes per-node metadata, not per-NIC metadata.
func (o *OKEInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	metadata := o.metadata.Load()
	if metadata == nil {
		return attributes
	}

	if metadata.HPCIslandId != "" {
		attributes[AttrOKEHPCIslandId] = resourceapi.DeviceAttribute{StringValue: &metadata.HPCIslandId}
	}
	if metadata.NetworkBlockId != "" {
		attributes[AttrOKENetworkBlockId] = resourceapi.DeviceAttribute{StringValue: &metadata.NetworkBlockId}
	}
	if metadata.LocalBlockId != "" {
		attributes[AttrOKELocalBlockId] = resourceapi.DeviceAttribute{StringValue: &metadata.LocalBlockId}
	}
	if metadata.RackId != "" {
		attributes[AttrOKERackId] = resourceapi.DeviceAttribute{StringValue: &metadata.RackId}
	}
	if metadata.GpuMemoryFabric != "" {
		attributes[AttrOKEGpuMemoryFabric] = resourceapi.DeviceAttribute{StringValue: &metadata.GpuMemoryFabric}
	}
	if metadata.Shape != "" {
		attributes[AttrOKEShape] = resourceapi.DeviceAttribute{StringValue: &metadata.Shape}
	}
	if metadata.RDMAFabric != nil {
		attributes[AttrOKERDMAFabricIPv6] = resourceapi.DeviceAttribute{BoolValue: &metadata.RDMAFabric.IPv6}
		attributes[AttrOKERDMAFabricPlanes] = resourceapi.DeviceAttribute{IntValue: &metadata.RDMAFabric.Planes}
	}

	return attributes
}

// ocidSuffix returns the unique identifier suffix of an OCI OCID, the segment
// after the last '.'. DRA string attributes are capped at 64 bytes, but full
// OCIDs are ~90+ characters; the suffix is always 60 characters and is unique
// per resource within a tenancy, making it safe to use as an attribute value.
func ocidSuffix(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "ocid") {
		return "", fmt.Errorf("not a valid OCID (missing 'ocid' prefix): %q", s)
	}
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return "", fmt.Errorf("not a valid OCID (missing '.' separator): %q", s)
	}
	suffix := s[i+1:]
	if len(suffix) > 60 {
		suffix = suffix[len(suffix)-60:]
	}
	return suffix, nil
}

// GetDeviceConfig advertises the OKE profile with the IPvlan type for an
// Ethernet RDMA NIC, so the RDMA NIC never moves into a pod. An RDMA NIC on an IPv6
// fabric gets no profile and moves into the pod as before, because the
// profile has no IPv6 support yet. A native InfiniBand NIC gets the profile
// only when a claim would take it from the host, so GetProfileConfig fails
// that claim.
func (o *OKEInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	if infiniBandHostError(id) != nil {
		return &apis.NetworkConfig{Profile: okeRDMAProfile}
	}
	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil || !o.isRDMANic(ifName) {
		return nil
	}
	if metadata := o.metadata.Load(); metadata != nil && metadata.RDMAFabric != nil && metadata.RDMAFabric.IPv6 {
		return nil
	}
	return &apis.NetworkConfig{
		Profile:   okeRDMAProfile,
		Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN},
	}
}

// GetProfileConfig resolves the OKE profile into an IPvlan child with the
// deterministic address and its source routing. It returns an error, which
// Kubernetes retries, until the metadata and the OCA parent address exist.
func (o *OKEInstance) GetProfileConfig(id cloudprovider.DeviceIdentifiers, _ *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	if config == nil {
		return nil, errors.New("OKE profile configuration is required")
	}
	if config.Profile != okeRDMAProfile {
		return nil, fmt.Errorf("unsupported OKE profile %q", config.Profile)
	}
	// Report an InfiniBand NIC first: the other checks do not apply to it.
	if err := infiniBandHostError(id); err != nil {
		return nil, err
	}
	if err := validateOKEProfileRequest(&config.Interface); err != nil {
		return nil, err
	}

	metadata := o.metadata.Load()
	switch {
	case metadata == nil || metadata.RDMAFabric == nil:
		return nil, errors.New("OKE RDMA fabric data is not available yet")
	case metadata.RDMAFabric.IPv6:
		return nil, errors.New("the OKE profile does not support an IPv6 RDMA fabric yet")
	case metadata.PrimaryVNIC == nil:
		return nil, errors.New("OKE primary VNIC metadata is not available yet")
	}
	mode := o.ocaConfig.ipv4AddressMode
	if mode != "" && mode != ocaIPv4AddressModeClassic {
		return nil, fmt.Errorf("the OKE profile does not support OCA IPv4 address mode %q", mode)
	}

	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil {
		return nil, err
	}
	hardwareType, err := interfaceHardwareType(ifName)
	if err != nil {
		return nil, err
	}
	if hardwareType != unix.ARPHRD_ETHER {
		return nil, fmt.Errorf("the OKE profile requires an Ethernet parent, but %s has ARPHRD type %d", ifName, hardwareType)
	}
	// The sysctl reads are cheap and can reject the request before any
	// address read, which costs a netlink dump.
	arpIgnore, arpAnnounce, err := parentARPPolicy(ifName, config.Interface.ARPIgnore, config.Interface.ARPAnnounce)
	if err != nil {
		return nil, err
	}

	vnic, parentRange := metadata.PrimaryVNIC, o.ocaConfig.parentIPv4Range
	addresses, err := interfaceIPv4Addresses(ifName)
	if err != nil {
		return nil, err
	}
	index, err := resolveRDMANicIndex(ifName, addressIn(addresses, parentRange), vnic, parentRange)
	if err != nil {
		return nil, err
	}
	parentAddr, childAddr, err := deriveRDMAIPv4(vnic, index, parentRange)
	if err != nil {
		return nil, fmt.Errorf("could not derive the OKE child address for %s: %w", ifName, err)
	}
	if err := checkParentAddress(ifName, addresses, parentAddr); err != nil {
		return nil, err
	}
	// Without the features file the address mode is a guess. Check the whole
	// host once before trusting it.
	if mode == "" && !o.layoutValidated.Load() {
		if err := o.validateClassicHostLayout(vnic); err != nil {
			return nil, fmt.Errorf("could not validate the OCA classic IPv4 layout: %w", err)
		}
		o.layoutValidated.Store(true)
	}

	result := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			Type:        apis.InterfaceTypeIPVLAN,
			Addresses:   []string{netip.PrefixFrom(childAddr, okeRDMAChildIPv4Range.Bits()).String()},
			ARPIgnore:   arpIgnore,
			ARPAnnounce: arpAnnounce,
		},
	}
	// Routing is user-owned when the claim carries routes, rules, or a VRF.
	if len(config.Routes) > 0 || len(config.Rules) > 0 || config.Interface.VRF != nil {
		return result, nil
	}
	table := okeRDMASBRTableBase + index
	result.Routes = []apis.RouteConfig{{
		Destination: okeRDMAChildIPv4CIDR,
		Source:      childAddr.String(),
		Scope:       unix.RT_SCOPE_LINK,
		Table:       table,
	}}
	result.Rules = []apis.RuleConfig{{
		Priority: apis.SourceRoutingRulePriority,
		Source:   netip.PrefixFrom(childAddr, childAddr.BitLen()).String(),
		Table:    table,
	}}
	return result, nil
}

// ReleaseProfileConfig has nothing to free: the child address is deterministic.
func (o *OKEInstance) ReleaseProfileConfig(cloudprovider.DeviceIdentifiers, types.UID, *apis.NetworkConfig) error {
	return nil
}

// infiniBandHostError returns why a claim would take a native InfiniBand NIC,
// or its RDMA device, from the host. The host health checks need both.
// The inventory sets the MAC only while the IPoIB interface is attached to the
// device, which is the --move-ib-interfaces=true case.
func infiniBandHostError(id cloudprovider.DeviceIdentifiers) error {
	rdmaDevice := infiniBandRDMADevice(id.PCIAddress)
	if rdmaDevice == "" {
		return nil
	}
	if id.MAC != "" {
		return fmt.Errorf("the native InfiniBand NIC of %s stays on the host on OKE; run DRANET with --move-ib-interfaces=false and claim the RDMA device", rdmaDevice)
	}
	if rdmaExclusiveNetnsMode() {
		return fmt.Errorf("the native InfiniBand RDMA device %s stays on the host on OKE, but exclusive network namespace mode moves it into the pod; use shared mode (ib_core netns_mode=1)", rdmaDevice)
	}
	return nil
}

// infiniBandRDMADevice returns the first RDMA device name, in lexical order,
// associated with pciAddress that has at least one InfiniBand port.
// It reads RDMA sysfs entries directly because a device may have zero or
// multiple IPoIB interfaces. It returns "" if no match is found or filesystem
// errors prevent discovery.
func infiniBandRDMADevice(pciAddress string) string {
	if pciAddress == "" {
		return ""
	}
	base := filepath.Join(sysfsPCIDevices, pciAddress, "infiniband")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		ports, _ := filepath.Glob(filepath.Join(base, entry.Name(), "ports", "*", "link_layer"))
		for _, port := range ports {
			if data, err := os.ReadFile(port); err == nil && strings.TrimSpace(string(data)) == "InfiniBand" {
				return entry.Name()
			}
		}
	}
	return ""
}

// validateOKEProfileRequest rejects the claim settings the OKE profile does
// not support. An RDMA NIC stays on the host, so a passthrough request is an
// error. The driver folds the deprecated dhcp field into Addressing first.
func validateOKEProfileRequest(iface *apis.InterfaceConfig) error {
	if iface.Type == apis.InterfaceTypePassthrough {
		return errors.New("the OKE profile does not move an RDMA NIC into a pod; remove interface.type: Passthrough")
	}
	if iface.Addressing == apis.AddressingModeDHCP {
		return errors.New("the OKE profile does not support DHCP addressing")
	}
	if iface.Addressing == apis.AddressingModeUnnumbered {
		return errors.New("the OKE profile does not support unnumbered addressing")
	}
	if len(iface.Addresses) > 0 {
		return errors.New("the OKE profile assigns the child address; remove interface.addresses")
	}
	return nil
}

// resolveRDMANicIndex returns the RDMA NIC index from the rdmaN name, or
// inverts it from the live OCA address of an RDMA NIC without that name.
func resolveRDMANicIndex(ifName string, address netip.Addr, vnic *primaryVNIC, parentRange netip.Prefix) (int, error) {
	if index, ok := rdmaNicIndex(ifName); ok {
		return index, nil
	}
	if !address.IsValid() {
		return 0, fmt.Errorf("interface %s has no address in the OCA RDMA network %s yet", ifName, parentRange)
	}
	index, err := rdmaNicIndexFromAddress(vnic, address, parentRange)
	if err != nil {
		return 0, fmt.Errorf("interface %s: %w", ifName, err)
	}
	klog.V(2).Infof("OKE RDMA NIC index %d of %s comes from its address %s", index, ifName, address)
	return index, nil
}

// parentARPPolicy fills the ARP settings the claim leaves unset from the
// parent, so the child answers ARP the way OCA configured the RDMA NIC.
func parentARPPolicy(ifName string, arpIgnore, arpAnnounce *int32) (*int32, *int32, error) {
	var err error
	if arpIgnore == nil {
		if arpIgnore, err = readARPSysctl(ifName, "arp_ignore"); err != nil {
			return nil, nil, err
		}
	}
	if arpAnnounce == nil {
		if arpAnnounce, err = readARPSysctl(ifName, "arp_announce"); err != nil {
			return nil, nil, err
		}
	}
	var missing []string
	if arpIgnore == nil {
		missing = append(missing, "interface.arpIgnore")
	}
	if arpAnnounce == nil {
		missing = append(missing, "interface.arpAnnounce")
	}
	if len(missing) > 0 {
		fields := strings.Join(missing, " and ")
		return nil, nil, fmt.Errorf("the OKE profile could not read %s from host interface %s; set %s in the claim", fields, ifName, fields)
	}
	return arpIgnore, arpAnnounce, nil
}

// readARPSysctl returns nil without an error when the setting does not exist.
func readARPSysctl(ifName, setting string) (*int32, error) {
	name := filepath.Join(procSysNetIPv4Conf, ifName, setting)
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", name, err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", name, err)
	}
	if setting == "arp_ignore" && value != 0 && value != 1 && value != 2 && value != 3 && value != 8 {
		return nil, fmt.Errorf("%s has unsupported value %d", name, value)
	}
	if setting == "arp_announce" && (value < 0 || value > 2) {
		return nil, fmt.Errorf("%s has unsupported value %d", name, value)
	}
	return ptr.To(int32(value)), nil
}

// OnOKE returns true if running on an Oracle Cloud Infrastructure instance.
// Detection is done by probing the OCI IMDS v2 endpoint.
func OnOKE(ctx context.Context) bool {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wait.PollUntilContextCancel(pollCtx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+"/instance/", nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("Authorization", "Bearer Oracle")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}) == nil
}

// queryIMDS decodes the JSON response of one OCI IMDS v2 endpoint into out.
func queryIMDS(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("could not create OCI IMDS request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer Oracle")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach OCI IMDS endpoint %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OCI IMDS endpoint %s returned status %d", url, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("could not parse OCI IMDS response from %s: %w", url, err)
	}
	return nil
}

// fabric returns the value when both fields are present, otherwise nil.
// A partial object is treated like an absent one.
func (d *imdsRDMAFabricData) fabric() *rdmaFabric {
	if d == nil || d.IPv6 == nil || d.Planes == nil {
		return nil
	}
	return &rdmaFabric{IPv6: *d.IPv6, Planes: *d.Planes}
}

// metadataFromIMDS builds a snapshot from the endpoints that answered. Either
// response can be nil.
func metadataFromIMDS(host *imdsHostMetadata, instance *imdsInstanceMetadata) *okeMetadata {
	metadata := &okeMetadata{}
	if instance != nil {
		metadata.Shape = instance.Shape
	}
	if host == nil {
		return metadata
	}

	metadata.NetworkBlockId = host.NetworkBlockId
	metadata.RackId = host.RackId
	metadata.RDMAFabric = host.RDMAFabricData.fabric()
	if data := host.RDMAFabricData; data != nil && metadata.RDMAFabric == nil {
		klog.V(2).Infof("Ignoring incomplete rdmaFabricData from OCI IMDS: ipv6 present=%t, planes present=%t", data.IPv6 != nil, data.Planes != nil)
	}

	// rdmaTopologyData is absent without a dedicated pool. Fall back to the
	// top-level networkBlockId and rackId when they are present.
	topo := host.RDMATopologyData
	if topo == nil {
		return metadata
	}

	// A malformed OCID leaves its field empty, so the merge keeps the last
	// valid value and the rest of the snapshot stays usable.
	suffix := func(name, ocid string) string {
		s, err := ocidSuffix(ocid)
		if err != nil {
			klog.Warningf("Ignoring invalid %s from OCI IMDS: %v", name, err)
		}
		return s
	}
	metadata.HPCIslandId = suffix("HPCIslandId", topo.CustomerHPCIslandId)
	metadata.NetworkBlockId = suffix("NetworkBlockId", topo.CustomerNetworkBlock)
	metadata.LocalBlockId = suffix("LocalBlockId", topo.CustomerLocalBlock)
	metadata.GpuMemoryFabric = suffix("GpuMemoryFabric", topo.CustomerGpuMemoryFabric)
	return metadata
}

// fetchOKEMetadata reads the host, instance, and vnics endpoints independently
// and builds a snapshot from the ones that answered. The vnics read happens
// only while the primary VNIC is unknown on a node with RDMA NICs. A host or VNIC
// failure returns the snapshot together with the error, so start keeps polling
// for the required data. An instance failure is only logged.
func fetchOKEMetadata(ctx context.Context, client *http.Client, endpoint string, needVNIC bool) (*okeMetadata, error) {
	host := &imdsHostMetadata{}
	hostErr := queryIMDS(ctx, client, endpoint+"/host/", host)
	if hostErr != nil {
		// A decode error can leave the struct half filled.
		host = nil
	}

	instance := &imdsInstanceMetadata{}
	instanceErr := queryIMDS(ctx, client, endpoint+"/instance/", instance)
	if instanceErr != nil {
		instance = nil
	}

	var vnic *primaryVNIC
	var vnicErr error
	if needVNIC {
		var vnics []imdsVNICMetadata
		if vnicErr = queryIMDS(ctx, client, endpoint+"/vnics/", &vnics); vnicErr == nil {
			vnic, vnicErr = parsePrimaryVNIC(vnics)
		}
		if vnicErr == nil && vnic == nil {
			vnicErr = errors.New("OCI IMDS primary VNIC has no IPv4 address yet")
		}
	}

	if host == nil && instance == nil && vnic == nil {
		return nil, errors.Join(hostErr, instanceErr, vnicErr)
	}
	if instanceErr != nil {
		klog.Warningf("Could not query OCI IMDS instance metadata: %v", instanceErr)
	}
	metadata := metadataFromIMDS(host, instance)
	metadata.PrimaryVNIC = vnic
	return metadata, errors.Join(hostErr, vnicErr)
}

// parsePrimaryVNIC returns the IPv4 data of the first entry, the primary VNIC.
// OCA reads the same entry. It returns nil without an error when the primary
// VNIC has no IPv4 address.
func parsePrimaryVNIC(vnics []imdsVNICMetadata) (*primaryVNIC, error) {
	if len(vnics) == 0 {
		return nil, errors.New("OCI IMDS vnics response is empty")
	}
	primary := vnics[0]
	if primary.PrivateIP == "" {
		return nil, nil
	}
	ip, err := netip.ParseAddr(primary.PrivateIP)
	if err != nil {
		return nil, fmt.Errorf("could not parse primary VNIC address %q: %w", primary.PrivateIP, err)
	}
	if !ip.Is4() {
		return nil, nil
	}
	if primary.SubnetCidrBlock == "" {
		return nil, errors.New("OCI IMDS vnics response does not contain subnetCidrBlock")
	}
	subnet, err := netip.ParsePrefix(primary.SubnetCidrBlock)
	if err != nil {
		return nil, fmt.Errorf("could not parse primary VNIC subnet %q: %w", primary.SubnetCidrBlock, err)
	}
	if !subnet.Addr().Is4() {
		return nil, nil
	}
	return &primaryVNIC{IPv4: ip, Subnet: subnet.Masked()}, nil
}

// mergeMetadata keeps the last non-empty value for a field that next omits.
func mergeMetadata(current, next *okeMetadata) *okeMetadata {
	if current == nil {
		return next
	}

	merged := *next
	merged.HPCIslandId = cmp.Or(merged.HPCIslandId, current.HPCIslandId)
	merged.NetworkBlockId = cmp.Or(merged.NetworkBlockId, current.NetworkBlockId)
	merged.LocalBlockId = cmp.Or(merged.LocalBlockId, current.LocalBlockId)
	merged.RackId = cmp.Or(merged.RackId, current.RackId)
	merged.GpuMemoryFabric = cmp.Or(merged.GpuMemoryFabric, current.GpuMemoryFabric)
	merged.Shape = cmp.Or(merged.Shape, current.Shape)
	if merged.RDMAFabric == nil {
		merged.RDMAFabric = current.RDMAFabric
	}
	if merged.PrimaryVNIC == nil {
		merged.PrimaryVNIC = current.PrimaryVNIC
	}
	return &merged
}

// refreshMetadata stores the snapshot of a full or partial read, then returns
// the read error.
func (o *OKEInstance) refreshMetadata(ctx context.Context) error {
	if o.fetchMetadata == nil {
		return errors.New("OKE metadata fetcher is not configured")
	}

	next, err := o.fetchMetadata(ctx)
	if next != nil {
		// start calls this before it starts refreshLoop. After startup,
		// refreshLoop is the only writer, so the read and write stay serialized.
		o.metadata.Store(mergeMetadata(o.metadata.Load(), next))
	}
	return err
}

// refreshLoop refreshes the metadata every refresh interval until ctx ends.
// A failed read keeps the last known values of the fields it did not update.
func (o *OKEInstance) refreshLoop(ctx context.Context) {
	for {
		timer := time.NewTimer(wait.Jitter(o.refreshInterval, 0.1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}

		if err := o.refreshMetadata(ctx); err != nil && ctx.Err() == nil {
			klog.Warningf("Could not refresh OCI IMDS host metadata, keeping the last known values: %v", err)
		}
	}
}

// GetInstance reads the OKE instance topology, shape, and RDMA fabric metadata
// from IMDS. It returns after the first successful host read or after the
// startup window. It keeps refreshing the metadata in the background until ctx
// ends.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	// A dedicated client, so the per-request timeout does not affect other callers.
	client := &http.Client{Timeout: imdsRequestTimeout}
	instance, err := newOKEInstance(nil, nil).start(ctx, client, imdsEndpoint)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// start polls IMDS for the startup window and runs the refresh loop until
// ctx ends.
func (o *OKEInstance) start(ctx context.Context, client *http.Client, endpoint string) (*OKEInstance, error) {
	o.fetchMetadata = func(ctx context.Context) (*okeMetadata, error) {
		// The primary VNIC is read until known, and only on a node with
		// RDMA NICs. The RDMA NIC check repeats, because an RDMA NIC can appear or get its
		// OCA address after startup.
		current := o.metadata.Load()
		needVNIC := (current == nil || current.PrimaryVNIC == nil) && o.hasRDMANic()
		return fetchOKEMetadata(ctx, client, endpoint, needVNIC)
	}
	// The OCA files are static per boot. Read them once, before the RDMA NIC
	// check, because the parent range identifies unnamed RDMA NICs.
	o.ocaConfig = loadOCARDMAConfig(ocaConfigDir)
	o.addressFallback = !hasNamedRDMANic()
	// The driver rejects IPvlan children in exclusive mode. Say so once here
	// instead of on the first failed claim.
	if o.hasRDMANic() && rdmaExclusiveNetnsMode() {
		klog.Warningf("The RDMA subsystem runs in exclusive network namespace mode; the OKE profile creates IPvlan children, so claims for RDMA NICs fail until the node uses shared mode (ib_core netns_mode=1)")
	}

	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, o.initialRetryInterval, o.initialWait, true, func(ctx context.Context) (bool, error) {
		lastErr = o.refreshMetadata(ctx)
		if lastErr != nil {
			klog.Infof("could not get OCI IMDS host metadata, retrying: %v", lastErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		klog.Warningf("OCI IMDS host metadata is not available after %s, retrying in the background: %v", o.initialWait, lastErr)
	}
	go o.refreshLoop(ctx)
	return o, nil
}

// rdmaExclusiveNetnsMode reports whether the ib_core module parameter says
// exclusive mode. A missing or unreadable value counts as shared, like the
// driver's own fallback.
func rdmaExclusiveNetnsMode() bool {
	data, err := os.ReadFile(ibCoreNetnsModeFile)
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(data))
	return value == "N" || value == "0"
}

// hasNamedRDMANic reports whether the node has an Ethernet rdmaN interface.
func hasNamedRDMANic() bool {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if _, ok := rdmaNicIndex(entry.Name()); !ok {
			continue
		}
		if hardwareType, err := interfaceHardwareType(entry.Name()); err == nil && hardwareType == unix.ARPHRD_ETHER {
			return true
		}
	}
	return false
}

// hasRDMANic reports whether the node has an Ethernet RDMA NIC.
func (o *OKEInstance) hasRDMANic() bool {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if o.isRDMANic(entry.Name()) {
			return true
		}
	}
	return false
}

// isRDMANic reports whether ifName is an Ethernet RDMA NIC: it has the rdmaN
// name or, on a node without such names, an address in the OCA RDMA network.
// The VCN NICs are Mellanox devices too, so hardware is not enough.
func (o *OKEInstance) isRDMANic(ifName string) bool {
	hardwareType, err := interfaceHardwareType(ifName)
	if err != nil || hardwareType != unix.ARPHRD_ETHER {
		return false
	}
	if _, ok := rdmaNicIndex(ifName); ok {
		return true
	}
	if !o.addressFallback {
		return false
	}
	addresses, err := interfaceIPv4Addresses(ifName)
	return err == nil && addressIn(addresses, o.ocaConfig.parentIPv4Range).IsValid()
}

// rdmaNicIndex returns the RDMA NIC index encoded in an rdmaN name. OCA
// assigns the name and the RDMA NIC address from the same table entry.
func rdmaNicIndex(ifName string) (int, bool) {
	digits, ok := strings.CutPrefix(ifName, rdmaNicPrefix)
	if !ok {
		return 0, false
	}
	index, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int(index), true
}

// interfaceIPv4Addresses returns the non-link-local IPv4 addresses of ifName.
func interfaceIPv4Addresses(ifName string) ([]netip.Addr, error) {
	addresses, err := interfaceAddresses(ifName)
	if err != nil {
		return nil, fmt.Errorf("could not read addresses for %s: %w", ifName, err)
	}
	var result []netip.Addr
	for _, address := range addresses {
		ipNet, ok := address.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipNet.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !addr.Is4() || addr.IsLinkLocalUnicast() {
			continue
		}
		result = append(result, addr)
	}
	return result, nil
}

// addressIn returns the first address inside prefix, or an invalid address.
func addressIn(addresses []netip.Addr, prefix netip.Prefix) netip.Addr {
	for _, address := range addresses {
		if prefix.Contains(address) {
			return address
		}
	}
	return netip.Addr{}
}

// checkParentAddress requires the derived OCA address among the live ones.
// No address is retryable, because OCA may assign it later. A different
// address means the layout is not the one derived.
func checkParentAddress(ifName string, addresses []netip.Addr, expected netip.Addr) error {
	for _, address := range addresses {
		if address == expected {
			return nil
		}
	}
	if len(addresses) > 0 {
		return fmt.Errorf("interface %s has address %s, but the derived OCA address is %s", ifName, addresses[0], expected)
	}
	return fmt.Errorf("interface %s does not have the expected OCA address %s yet", ifName, expected)
}

// validateClassicHostLayout checks every RDMA NIC that holds an OCA address
// against the classic formula. One RDMA NIC can match by coincidence under
// another OCA address mode; a whole host cannot.
func (o *OKEInstance) validateClassicHostLayout(vnic *primaryVNIC) error {
	parentRange := o.ocaConfig.parentIPv4Range
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return fmt.Errorf("could not read network interfaces: %w", err)
	}
	checked := 0
	for _, entry := range entries {
		ifName := entry.Name()
		if !o.isRDMANic(ifName) {
			continue
		}
		addresses, err := interfaceIPv4Addresses(ifName)
		if err != nil {
			return err
		}
		address := addressIn(addresses, parentRange)
		if !address.IsValid() {
			// OCA has not configured this RDMA NIC yet.
			continue
		}
		index, err := resolveRDMANicIndex(ifName, address, vnic, parentRange)
		if err != nil {
			return err
		}
		expected, _, err := deriveOCAParentIPv4(vnic, index, parentRange)
		if err != nil {
			return fmt.Errorf("could not derive the OCA address for %s: %w", ifName, err)
		}
		if address != expected {
			return fmt.Errorf("interface %s has address %s, but the derived OCA address is %s", ifName, address, expected)
		}
		checked++
	}
	if checked == 0 {
		return errors.New("no Ethernet RDMA NIC holds an OCA address yet")
	}
	return nil
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	bytes := addr.As4()
	return binary.BigEndian.Uint32(bytes[:])
}

func uint32ToIPv4(value uint32) netip.Addr {
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes)
}

func validateVNIC(vnic *primaryVNIC) error {
	if vnic == nil {
		return errors.New("OKE primary VNIC metadata is not available")
	}
	if !vnic.IPv4.Is4() || !vnic.Subnet.Addr().Is4() {
		return errors.New("OKE primary VNIC has no IPv4 address")
	}
	if !vnic.Subnet.Contains(vnic.IPv4) {
		return fmt.Errorf("primary VNIC address %s is outside its subnet %s", vnic.IPv4, vnic.Subnet)
	}
	return nil
}

// deriveOCAParentIPv4 computes the address that OCA classic mode assigns to
// one RDMA NIC,
// parent = base + nicIndex*subnetSize + (vnicIP - vnicSubnetBase),
// and the offset of that address inside the OCA RDMA network.
func deriveOCAParentIPv4(vnic *primaryVNIC, nicIndex int, parentRange netip.Prefix) (netip.Addr, uint64, error) {
	if err := validateVNIC(vnic); err != nil {
		return netip.Addr{}, 0, err
	}
	if !parentRange.IsValid() || !parentRange.Addr().Is4() || parentRange != parentRange.Masked() {
		return netip.Addr{}, 0, fmt.Errorf("OCA RDMA network %s is not a masked IPv4 prefix", parentRange)
	}
	subnet := vnic.Subnet
	if subnet.Overlaps(parentRange) {
		return netip.Addr{}, 0, fmt.Errorf("primary VNIC subnet %s overlaps the OCA RDMA network %s", subnet, parentRange)
	}
	blockSize := uint64(1) << (32 - subnet.Bits())
	hostPosition := uint64(ipv4ToUint32(vnic.IPv4) - ipv4ToUint32(subnet.Addr()))
	offset := uint64(nicIndex)*blockSize + hostPosition
	if offset >= uint64(1)<<(32-parentRange.Bits()) {
		return netip.Addr{}, 0, fmt.Errorf("RDMA NIC index %d with a /%d primary VNIC subnet is outside the OCA RDMA network %s", nicIndex, subnet.Bits(), parentRange)
	}
	return uint32ToIPv4(ipv4ToUint32(parentRange.Addr()) + uint32(offset)), offset, nil
}

// deriveRDMAIPv4 computes the OCA parent and the Dranet child address of one
// RDMA NIC. The child keeps the parent offset inside the child range.
func deriveRDMAIPv4(vnic *primaryVNIC, nicIndex int, parentRange netip.Prefix) (netip.Addr, netip.Addr, error) {
	// An index above the largest shape would leave tables 100 to 115 and could
	// reach a reserved table. It runs first, so every such index gets this error.
	if nicIndex >= okeMaxIPv4RDMANics {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("RDMA NIC index %d is above the largest supported index %d", nicIndex, okeMaxIPv4RDMANics-1)
	}
	parent, offset, err := deriveOCAParentIPv4(vnic, nicIndex, parentRange)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	childRange := okeRDMAChildIPv4Range
	if parentRange.Overlaps(childRange) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("OCA RDMA network %s overlaps the Dranet child range %s", parentRange, childRange)
	}
	// A VNIC subnet inside the child range would collide with child addresses.
	if vnic.Subnet.Overlaps(childRange) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("primary VNIC subnet %s overlaps the Dranet child range %s", vnic.Subnet, childRange)
	}
	start := offset * okeChildrenPerNIC
	if start+okeChildrenPerNIC > uint64(1)<<(32-childRange.Bits()) {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("RDMA NIC index %d with a /%d primary VNIC subnet is outside the child range %s", nicIndex, vnic.Subnet.Bits(), childRange)
	}
	child := uint32ToIPv4(ipv4ToUint32(childRange.Addr()) + uint32(start))
	return parent, child, nil
}

// rdmaNicIndexFromAddress inverts the classic formula for a parent without an
// rdmaN name. The remainder of the offset must equal the host position of the
// primary VNIC, which rejects an address that OCA did not derive for this host.
func rdmaNicIndexFromAddress(vnic *primaryVNIC, address netip.Addr, parentRange netip.Prefix) (int, error) {
	if err := validateVNIC(vnic); err != nil {
		return 0, err
	}
	if !parentRange.Contains(address) {
		return 0, fmt.Errorf("address %s is outside the OCA RDMA network %s", address, parentRange)
	}
	subnet := vnic.Subnet
	blockSize := uint64(1) << (32 - subnet.Bits())
	hostPosition := uint64(ipv4ToUint32(vnic.IPv4) - ipv4ToUint32(subnet.Addr()))
	offset := uint64(ipv4ToUint32(address) - ipv4ToUint32(parentRange.Addr()))
	if offset%blockSize != hostPosition {
		return 0, fmt.Errorf("address %s does not match the primary VNIC position %d in a /%d subnet", address, hostPosition, subnet.Bits())
	}
	return int(offset / blockSize), nil
}

func defaultOCARDMAConfig() ocaRDMAConfig {
	return ocaRDMAConfig{parentIPv4Range: netip.MustParsePrefix(okeRDMAParentIPv4CIDR)}
}

// parseOCARDMANetwork reads rdma-network[0].default-settings.rdma_network,
// the only field OCA uses for the RDMA network.
func parseOCARDMANetwork(data []byte) (netip.Prefix, error) {
	var file ocaRDMANetworkFile
	if err := json.Unmarshal(data, &file); err != nil {
		return netip.Prefix{}, fmt.Errorf("could not parse %s: %w", ocaRDMANetworkFileName, err)
	}
	if len(file.RDMANetwork) == 0 {
		return netip.Prefix{}, fmt.Errorf("%s has no rdma-network entries", ocaRDMANetworkFileName)
	}
	value := file.RDMANetwork[0].DefaultSettings.RDMANetwork
	if value == "" {
		return netip.Prefix{}, fmt.Errorf("%s does not contain rdma_network", ocaRDMANetworkFileName)
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("could not parse rdma_network %q: %w", value, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("rdma_network %q is not an IPv4 prefix", value)
	}
	if prefix != prefix.Masked() {
		return netip.Prefix{}, fmt.Errorf("rdma_network %q is not a masked prefix", value)
	}
	return prefix, nil
}

// parseOCARDMAFeatures maps the OCA feature flags to the IPv4 address mode.
func parseOCARDMAFeatures(data []byte) (string, error) {
	var file ocaRDMAFeaturesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", fmt.Errorf("could not parse %s: %w", ocaRDMAFeaturesFileName, err)
	}
	if file.HostSerialIPv4 {
		return ocaIPv4AddressModeSerial, nil
	}
	if file.TopologyIPv4 {
		return ocaIPv4AddressModeTopology, nil
	}
	return ocaIPv4AddressModeClassic, nil
}

// loadOCARDMAConfig reads the optional OCA files from dir. A missing or
// unusable file keeps the default, because a wrong guess only fails claims
// through the parent address check and never disables the provider.
func loadOCARDMAConfig(dir string) ocaRDMAConfig {
	config := defaultOCARDMAConfig()

	networkPath := filepath.Join(dir, ocaRDMANetworkFileName)
	data, err := os.ReadFile(networkPath)
	switch {
	case err == nil:
		prefix, parseErr := parseOCARDMANetwork(data)
		if parseErr != nil {
			klog.Warningf("Could not use OCA RDMA network file %s; using %s: %v", networkPath, config.parentIPv4Range, parseErr)
		} else {
			config.parentIPv4Range = prefix
			klog.Infof("Using OCA RDMA network %s from %s", prefix, networkPath)
		}
	case errors.Is(err, os.ErrNotExist):
		klog.V(2).Infof("OCA RDMA network file %s is not mounted; using %s", networkPath, config.parentIPv4Range)
	default:
		klog.Warningf("Could not read OCA RDMA network file %s; using %s: %v", networkPath, config.parentIPv4Range, err)
	}

	featuresPath := filepath.Join(dir, ocaRDMAFeaturesFileName)
	data, err = os.ReadFile(featuresPath)
	switch {
	case err == nil:
		mode, parseErr := parseOCARDMAFeatures(data)
		if parseErr != nil {
			klog.Warningf("Could not use OCA RDMA features file %s; assuming classic IPv4 mode: %v", featuresPath, parseErr)
		} else {
			config.ipv4AddressMode = mode
			klog.Infof("Using OCA RDMA IPv4 address mode %s from %s", mode, featuresPath)
		}
	case errors.Is(err, os.ErrNotExist):
		klog.V(2).Infof("OCA RDMA features file %s is not mounted; assuming classic IPv4 mode", featuresPath)
	default:
		klog.Warningf("Could not read OCA RDMA features file %s; assuming classic IPv4 mode: %v", featuresPath, err)
	}

	return config
}

func interfaceHardwareType(ifName string) (int, error) {
	name := filepath.Join(sysClassNet, ifName, "type")
	data, err := os.ReadFile(name)
	if err != nil {
		return 0, fmt.Errorf("could not read %s: %w", name, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("could not parse %s: %w", name, err)
	}
	return value, nil
}

func interfaceNameForPCIAddress(pciAddress string) (string, error) {
	if pciAddress == "" {
		return "", errors.New("device has no PCI address")
	}
	entries, err := os.ReadDir(filepath.Join(sysfsPCIDevices, pciAddress, "net"))
	if err != nil {
		return "", fmt.Errorf("could not read network interfaces for PCI device %s: %w", pciAddress, err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("expected one network interface for PCI device %s, got %d", pciAddress, len(entries))
	}
	return entries[0].Name(), nil
}
