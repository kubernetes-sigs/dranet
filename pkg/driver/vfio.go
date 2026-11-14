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

package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/features"
	"sigs.k8s.io/dranet/pkg/inventory"
)

// VFIOState records a PCI function that DRANET bound to vfio-pci for a claim.
// It is written to the checkpoint before the first sysfs write, so DRANET
// always knows which driver to bind back, even after a restart.
type VFIOState struct {
	PCIAddress     string `json:"pciAddress"`
	OriginalDriver string `json:"originalDriver"`
	// Bound is set once the function is confirmed on vfio-pci and DevChars
	// are recorded. A record with Bound unset is resumed by the next prepare.
	Bound    bool          `json:"bound,omitempty"`
	DevChars []LinuxDevice `json:"devChars,omitempty"`
}

const (
	vfioContainerDevice = "/dev/vfio/vfio"
	iommufdDevice       = "/dev/iommu"

	sysBusPCIDevices = "bus/pci/devices"
	sysBusPCIDrivers = "bus/pci/drivers"
	sysBusPCIProbe   = "bus/pci/drivers_probe"
)

// pciBinder performs the sysfs operations of a rebind. The sysfs
// implementation is used in production and a fake simulates the kernel in
// tests.
type pciBinder interface {
	Exists(pciAddress string) bool
	Driver(pciAddress string) string
	Override(pciAddress string) string
	SetOverride(pciAddress, driver string) error
	Unbind(pciAddress string) error
	Probe(pciAddress string) error
	Blocker(pciAddress string) string
	HostAddresses(pciAddress string) ([]string, error)
	DevChars(pciAddress string) ([]LinuxDevice, error)
}

type sysfsBinder struct {
	root string
}

func (b sysfsBinder) devicePath(pciAddress string, elem ...string) string {
	return filepath.Join(append([]string{b.root, sysBusPCIDevices, pciAddress}, elem...)...)
}

func (b sysfsBinder) Exists(pciAddress string) bool {
	_, err := os.Stat(b.devicePath(pciAddress))
	return err == nil
}

func (b sysfsBinder) Driver(pciAddress string) string {
	return inventory.BoundDriver(b.root, pciAddress)
}

// Override returns the driver_override of the function, or "" when it is not
// set, which the kernel reports as "(null)".
func (b sysfsBinder) Override(pciAddress string) string {
	content, err := os.ReadFile(b.devicePath(pciAddress, "driver_override"))
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(content))
	if value == "(null)" {
		return ""
	}
	return value
}

// SetOverride sets the driver the next probe binds the function to. An empty
// driver clears the override; the kernel ignores an empty write, so a newline
// is written instead.
func (b sysfsBinder) SetOverride(pciAddress, driver string) error {
	value := driver
	if value == "" {
		value = "\n"
	}
	if err := os.WriteFile(b.devicePath(pciAddress, "driver_override"), []byte(value), 0o200); err != nil {
		return fmt.Errorf("set driver_override=%q on %s: %w", driver, pciAddress, err)
	}
	return nil
}

func (b sysfsBinder) Unbind(pciAddress string) error {
	driver := b.Driver(pciAddress)
	if driver == "" {
		return nil
	}
	if err := os.WriteFile(filepath.Join(b.root, sysBusPCIDrivers, driver, "unbind"), []byte(pciAddress), 0o200); err != nil {
		return fmt.Errorf("unbind %s from %s: %w", pciAddress, driver, err)
	}
	return nil
}

func (b sysfsBinder) Probe(pciAddress string) error {
	if err := os.WriteFile(filepath.Join(b.root, sysBusPCIProbe), []byte(pciAddress), 0o200); err != nil {
		return fmt.Errorf("probe %s: %w", pciAddress, err)
	}
	return nil
}

func (b sysfsBinder) Blocker(pciAddress string) string {
	return inventory.VFIOBlocker(b.root, pciAddress)
}

func (b sysfsBinder) HostAddresses(pciAddress string) ([]string, error) {
	entries, err := os.ReadDir(b.devicePath(pciAddress, "net"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list network interfaces of %s: %w", pciAddress, err)
	}
	var addresses []string
	for _, e := range entries {
		link, err := nlwrap.LinkByName(e.Name())
		if err != nil {
			return nil, fmt.Errorf("get link %s of %s: %w", e.Name(), pciAddress, err)
		}
		addrs, err := nlwrap.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, fmt.Errorf("list addresses of %s: %w", e.Name(), err)
		}
		for _, addr := range addrs {
			if addr.IP.IsGlobalUnicast() {
				addresses = append(addresses, addr.IPNet.String())
			}
		}
	}
	return addresses, nil
}

// DevChars returns the character devices a container needs to use the
// function: its VFIO group and the VFIO container, plus its VFIO cdev and the
// iommufd device when the kernel provides both nodes.
func (b sysfsBinder) DevChars(pciAddress string) ([]LinuxDevice, error) {
	group, err := inventory.IOMMUGroup(b.root, pciAddress)
	if err != nil {
		return nil, err
	}
	groupDev, err := GetDeviceInfo("/dev/vfio/" + group)
	if err != nil {
		return nil, err
	}
	container, err := GetDeviceInfo(vfioContainerDevice)
	if err != nil {
		return nil, err
	}
	devChars := []LinuxDevice{groupDev, container}

	cdevs, err := os.ReadDir(b.devicePath(pciAddress, "vfio-dev"))
	if os.IsNotExist(err) {
		return devChars, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list vfio-dev of %s: %w", pciAddress, err)
	}
	if len(cdevs) != 1 {
		return nil, fmt.Errorf("expected one vfio-dev entry for %s, found %d", pciAddress, len(cdevs))
	}
	cdev, err := GetDeviceInfo("/dev/vfio/devices/" + cdevs[0].Name())
	if err != nil {
		klog.V(2).Infof("using only the VFIO group of %s: %v", pciAddress, err)
		return devChars, nil
	}
	iommufd, err := GetDeviceInfo(iommufdDevice)
	if err != nil {
		klog.V(2).Infof("using only the VFIO group of %s: %v", pciAddress, err)
		return devChars, nil
	}
	return append(devChars, cdev, iommufd), nil
}

func (np *NetworkDriver) pciBinder() pciBinder {
	if np.vfioBinder != nil {
		return np.vfioBinder
	}
	return sysfsBinder{root: inventory.SysfsRoot}
}

// vfioBind binds the function to vfio-pci. The override is written before the
// unbind so no probe in between can bind the function back to its kernel
// driver, and it stays for the life of the claim. On failure the function is
// bound back to the original driver.
func vfioBind(b pciBinder, pciAddress, original string) error {
	if b.Driver(pciAddress) == inventory.VFIODriver && b.Override(pciAddress) == inventory.VFIODriver {
		return nil
	}
	if err := b.SetOverride(pciAddress, inventory.VFIODriver); err != nil {
		return err
	}
	rollback := func(cause error) error {
		if err := vfioRestore(b, pciAddress, original); err != nil {
			return fmt.Errorf("%w; binding %s back also failed: %v", cause, original, err)
		}
		return cause
	}
	if b.Driver(pciAddress) != inventory.VFIODriver {
		if err := b.Unbind(pciAddress); err != nil {
			return rollback(err)
		}
		if err := b.Probe(pciAddress); err != nil {
			return rollback(err)
		}
	}
	if got := b.Driver(pciAddress); got != inventory.VFIODriver {
		return rollback(fmt.Errorf("%s is bound to %q after probe, not %s; IOMMU may be disabled", pciAddress, got, inventory.VFIODriver))
	}
	return nil
}

// vfioRestore binds the function back to the driver recorded before the
// rebind. The override names that driver during the probe, because the
// kernel probes a VF whose PF has sriov_drivers_autoprobe disabled only
// through driver_override, and it is cleared once the driver is confirmed. A
// function that no longer exists, because its PF driver was reloaded or its
// VFs were removed, has nothing to restore.
func vfioRestore(b pciBinder, pciAddress, original string) error {
	if !b.Exists(pciAddress) {
		klog.Infof("PCI device %s no longer exists, nothing to restore", pciAddress)
		return nil
	}
	if b.Driver(pciAddress) != original {
		if err := b.SetOverride(pciAddress, original); err != nil {
			return err
		}
		if err := b.Unbind(pciAddress); err != nil {
			return err
		}
		if err := b.Probe(pciAddress); err != nil {
			return err
		}
		if got := b.Driver(pciAddress); got != original {
			return fmt.Errorf("%s is bound to %q after restore, want %q", pciAddress, got, original)
		}
	}
	if b.Override(pciAddress) != "" {
		return b.SetOverride(pciAddress, "")
	}
	return nil
}

// restoreVFIODevices binds the original driver back for every function given
// through VFIO to the pods that use the claim.
func (np *NetworkDriver) restoreVFIODevices(claim types.NamespacedName) error {
	var errs []error
	for _, podUID := range np.podConfigStore.ListPods() {
		podCfg, ok := np.podConfigStore.GetPodConfig(podUID)
		if !ok || !podUsesClaim(podCfg, claim) {
			continue
		}
		for deviceName, devCfg := range podCfg.DeviceConfigs {
			state := devCfg.VFIO
			if state == nil {
				continue
			}
			if err := vfioRestore(np.pciBinder(), state.PCIAddress, state.OriginalDriver); err != nil {
				errs = append(errs, fmt.Errorf("device %s: restore %s to %s: %w", deviceName, state.PCIAddress, state.OriginalDriver, err))
			}
		}
	}
	return errors.Join(errs...)
}

func podUsesClaim(podCfg PodConfig, claim types.NamespacedName) bool {
	for _, devCfg := range podCfg.DeviceConfigs {
		if devCfg.Claim == claim {
			return true
		}
	}
	return false
}

// prepareVFIODevice binds the allocated function to vfio-pci and records the
// VFIO character devices for CreateContainer. A stored record whose rebind
// was not confirmed, left by a failed or interrupted prepare, is resumed.
func (np *NetworkDriver) prepareVFIODevice(claim *resourceapi.ResourceClaim, podUID types.UID, result resourceapi.DeviceRequestAllocationResult, conf *apis.NetworkConfig, stored *DeviceConfig) error {
	if !features.DefaultFeatureGate.Enabled(features.VFIODriverRebind) {
		return fmt.Errorf("device %s: vfio requires the %s feature gate", result.Device, features.VFIODriverRebind)
	}
	b := np.pciBinder()

	var deviceCfg DeviceConfig
	if stored != nil {
		deviceCfg = *stored
	} else {
		device, ok := np.netdb.GetDevice(result.Device)
		if !ok {
			return fmt.Errorf("device %s: not found in the inventory", result.Device)
		}
		pciAttr, ok := device.Attributes[apis.AttrPCIAddress]
		if !ok || pciAttr.StringValue == nil {
			return fmt.Errorf("device %s: vfio requires a PCI device", result.Device)
		}
		pciAddress := *pciAttr.StringValue
		if reason := b.Blocker(pciAddress); reason != "" {
			return fmt.Errorf("device %s (%s) cannot be given through VFIO: %s", result.Device, pciAddress, reason)
		}
		addresses, err := b.HostAddresses(pciAddress)
		if err != nil {
			return fmt.Errorf("device %s (%s): %w", result.Device, pciAddress, err)
		}
		if len(addresses) > 0 {
			return fmt.Errorf("device %s (%s) cannot be given through VFIO: its network interface has host addresses %v", result.Device, pciAddress, addresses)
		}
		original := b.Driver(pciAddress)
		if original == "" || original == inventory.VFIODriver {
			return fmt.Errorf("device %s (%s) cannot be given through VFIO: it is bound to %q, not to a kernel network driver", result.Device, pciAddress, original)
		}
		deviceCfg = DeviceConfig{
			Claim:                       types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name},
			DeviceSnapshot:              &device,
			NetworkInterfaceConfigInPod: *conf,
			VFIO:                        &VFIOState{PCIAddress: pciAddress, OriginalDriver: original},
		}
		if err := np.podConfigStore.SetDeviceConfig(podUID, result.Device, deviceCfg); err != nil {
			return fmt.Errorf("failed to persist device config for pod %s device %s: %v", podUID, result.Device, err)
		}
	}

	state := *deviceCfg.VFIO
	if err := vfioBind(b, state.PCIAddress, state.OriginalDriver); err != nil {
		return fmt.Errorf("device %s: bind %s to %s: %w", result.Device, state.PCIAddress, inventory.VFIODriver, err)
	}
	devChars, err := b.DevChars(state.PCIAddress)
	if err != nil {
		return fmt.Errorf("device %s: %w", result.Device, err)
	}
	state.DevChars = devChars
	state.Bound = true
	deviceCfg.VFIO = &state
	if err := np.podConfigStore.SetDeviceConfig(podUID, result.Device, deviceCfg); err != nil {
		return fmt.Errorf("failed to persist device config for pod %s device %s: %v", podUID, result.Device, err)
	}
	klog.V(2).Infof("device %s (%s) bound to %s for pod %s, original driver %s", result.Device, state.PCIAddress, inventory.VFIODriver, podUID, state.OriginalDriver)
	return nil
}
