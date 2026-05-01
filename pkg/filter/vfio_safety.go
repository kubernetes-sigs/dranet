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

package filter

import (
	"os"
	"path/filepath"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// MarkVFIOUnsafe annotates devices that cannot safely be used for VFIO
// passthrough by setting dra.net/vfioUnsafe=true. This allows the
// scheduler to still allocate them for non-VFIO use (bridge, macvlan)
// while enabling claims to exclude them with a CEL selector.
//
// A device is marked unsafe if:
//   - Its IOMMU group contains devices bound to different drivers
//   - Its network interface carries the host's default route
func MarkVFIOUnsafe(devices []resourcev1.Device) []resourcev1.Device {
	defaultGW := defaultGatewayInterfaces()
	for i := range devices {
		pciAddr := getPCIAddr(&devices[i])
		if pciAddr == "" {
			continue
		}
		ifName := getIfName(&devices[i])

		unsafe := false
		reason := ""

		if hasSharedIommuGroup(pciAddr) {
			unsafe = true
			reason = "shared IOMMU group"
		} else if ifName != "" && defaultGW[ifName] {
			unsafe = true
			reason = "default gateway interface"
		}

		if unsafe {
			klog.V(2).Infof("Marking %s (pci=%s, if=%s) as vfio-unsafe: %s",
				devices[i].Name, pciAddr, ifName, reason)
			val := true
			if devices[i].Attributes == nil {
				devices[i].Attributes = make(map[resourcev1.QualifiedName]resourcev1.DeviceAttribute)
			}
			devices[i].Attributes["dra.net/vfioUnsafe"] = resourcev1.DeviceAttribute{BoolValue: &val}
			devices[i].Attributes["dra.net/vfioUnsafeReason"] = resourcev1.DeviceAttribute{StringValue: &reason}
		}
	}
	return devices
}

func getPCIAddr(dev *resourcev1.Device) string {
	if dev.Attributes == nil {
		return ""
	}
	for _, key := range []resourcev1.QualifiedName{
		"resource.kubernetes.io/pciBusID",
		"dra.net/pciAddress",
	} {
		if attr, ok := dev.Attributes[key]; ok && attr.StringValue != nil {
			return *attr.StringValue
		}
	}
	return ""
}

func getIfName(dev *resourcev1.Device) string {
	if dev.Attributes == nil {
		return ""
	}
	if attr, ok := dev.Attributes["dra.net/ifName"]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

// hasSharedIommuGroup returns true if the PCI device's IOMMU group
// contains other devices bound to a different kernel driver.
func hasSharedIommuGroup(pciAddr string) bool {
	groupLink := filepath.Join("/sys/bus/pci/devices", pciAddr, "iommu_group", "devices")
	entries, err := os.ReadDir(groupLink)
	if err != nil {
		return false
	}
	if len(entries) <= 1 {
		return false
	}
	for _, e := range entries {
		if e.Name() == pciAddr {
			continue
		}
		siblingDriver := filepath.Join("/sys/bus/pci/devices", e.Name(), "driver")
		if target, err := os.Readlink(siblingDriver); err == nil {
			driver := filepath.Base(target)
			if driver != "vfio-pci" {
				klog.V(4).Infof("IOMMU group sibling %s bound to %s (not vfio-pci)", e.Name(), driver)
				return true
			}
		}
	}
	return false
}

// defaultGatewayInterfaces returns the set of interface names that carry
// a default route (0.0.0.0/0 or ::/0). Reads /proc/1/net/route to access
// the host's routing table when running inside a container.
func defaultGatewayInterfaces() map[string]bool {
	result := make(map[string]bool)

	for _, path := range []string{"/proc/1/net/route", "/proc/net/route"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 8 {
				continue
			}
			if fields[1] == "00000000" && fields[7] == "00000000" {
				result[fields[0]] = true
			}
		}
		break
	}

	for _, path := range []string{"/proc/1/net/ipv6_route", "/proc/net/ipv6_route"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			if fields[0] == "00000000000000000000000000000000" && fields[1] == "00" {
				result[fields[len(fields)-1]] = true
			}
		}
		break
	}

	if len(result) > 0 {
		names := make([]string, 0, len(result))
		for k := range result {
			names = append(names, k)
		}
		klog.V(2).Infof("Default gateway interfaces: %v", names)
	}

	return result
}
