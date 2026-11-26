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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
)

const (
	// SysfsRoot is the sysfs mount point. Helpers in this file take the root
	// as an argument so tests can point them at a synthetic tree.
	SysfsRoot = "/sys"
	// VFIODriver is the kernel driver that exposes a PCI function through VFIO.
	VFIODriver = "vfio-pci"

	sysBusPCIDevices     = "bus/pci/devices"
	sysBusPCIDrivers     = "bus/pci/drivers"
	sysKernelIOMMUGroups = "kernel/iommu_groups"
)

// BoundDriver returns the driver bound to the PCI function, or "" if none is.
func BoundDriver(sysRoot, pciAddress string) string {
	target, err := os.Readlink(filepath.Join(sysRoot, sysBusPCIDevices, pciAddress, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// IOMMUGroup returns the IOMMU group of the PCI function.
func IOMMUGroup(sysRoot, pciAddress string) (string, error) {
	target, err := os.Readlink(filepath.Join(sysRoot, sysBusPCIDevices, pciAddress, "iommu_group"))
	if err != nil {
		return "", fmt.Errorf("read iommu_group of %s: %w", pciAddress, err)
	}
	return filepath.Base(target), nil
}

func iommuGroupMembers(sysRoot, group string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(sysRoot, sysKernelIOMMUGroups, group, "devices"))
	if err != nil {
		return nil, fmt.Errorf("list iommu group %s: %w", group, err)
	}
	members := make([]string, 0, len(entries))
	for _, e := range entries {
		members = append(members, e.Name())
	}
	return members, nil
}

func hasVirtualFunctions(sysRoot, pciAddress string) bool {
	matches, err := filepath.Glob(filepath.Join(sysRoot, sysBusPCIDevices, pciAddress, "virtfn*"))
	return err == nil && len(matches) > 0
}

// upperDevices returns the devices stacked on the network interfaces of the
// PCI function, such as a bond, bridge, VLAN or macvlan, from their upper_*
// links in sysfs.
func upperDevices(sysRoot, pciAddress string) []string {
	matches, err := filepath.Glob(filepath.Join(sysRoot, sysBusPCIDevices, pciAddress, "net", "*", "upper_*"))
	if err != nil {
		return nil
	}
	uppers := make([]string, 0, len(matches))
	for _, m := range matches {
		uppers = append(uppers, strings.TrimPrefix(filepath.Base(m), "upper_"))
	}
	return uppers
}

// groupSharingAllowed lists the drivers that may stay bound to the other
// members of the IOMMU group: no driver, and the drivers the kernel allows
// next to VFIO because they do no DMA of their own. Members on vfio-pci are
// not allowed, because the group can be opened by one user only.
var groupSharingAllowed = map[string]bool{"": true, "pcieport": true, "pci-stub": true}

// VFIOBlocker returns why the PCI function cannot be bound to vfio-pci, or ""
// when it can. It covers the conditions visible in sysfs; the caller checks
// that the function does not carry host network configuration.
func VFIOBlocker(sysRoot, pciAddress string) string {
	if _, err := os.Stat(filepath.Join(sysRoot, sysBusPCIDrivers, VFIODriver)); err != nil {
		return "the vfio-pci driver is not loaded"
	}
	if hasVirtualFunctions(sysRoot, pciAddress) {
		return "it has virtual functions, which binding it to vfio-pci would remove"
	}
	if uppers := upperDevices(sysRoot, pciAddress); len(uppers) > 0 {
		return fmt.Sprintf("its network interface is part of %s, which binding it to vfio-pci would break", strings.Join(uppers, ", "))
	}
	group, err := IOMMUGroup(sysRoot, pciAddress)
	if err != nil {
		return "it has no IOMMU group"
	}
	members, err := iommuGroupMembers(sysRoot, group)
	if err != nil {
		return fmt.Sprintf("cannot list IOMMU group %s: %v", group, err)
	}
	var shared []string
	for _, m := range members {
		if m == pciAddress {
			continue
		}
		if driver := BoundDriver(sysRoot, m); !groupSharingAllowed[driver] {
			shared = append(shared, fmt.Sprintf("%s (%s)", m, driver))
		}
	}
	if len(shared) > 0 {
		return fmt.Sprintf("its IOMMU group %s is shared with %s", group, strings.Join(shared, ", "))
	}
	return ""
}

func (db *DB) addVFIOAttributes(device *hostDevice, pciAddress string) {
	if attr, err := deviceattribute.GetPCIBusIDAttribute(pciAddress); err == nil {
		device.Attributes[attr.Name] = attr.Value
	}
	reason := VFIOBlocker(db.vfioSysRoot, pciAddress)
	if reason == "" {
		if _, ok := device.Attributes[apis.AttrIPv4]; ok {
			reason = "its network interface has host IPv4 addresses"
		} else if _, ok := device.Attributes[apis.AttrIPv6]; ok {
			reason = "its network interface has host IPv6 addresses"
		}
	}
	if reason != "" {
		klog.V(4).Infof("PCI device %s cannot be given through VFIO: %s", pciAddress, reason)
	}
	device.Attributes[apis.AttrVFIO] = resourceapi.DeviceAttribute{BoolValue: ptr.To(reason == "")}
}
