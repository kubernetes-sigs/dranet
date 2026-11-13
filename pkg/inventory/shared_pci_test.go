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
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestInheritParentAttributesSharedPCI(t *testing.T) {
	userns.Run(t, testInheritParentAttributesSharedPCI, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testInheritParentAttributesSharedPCI(t *testing.T) {
	// Replace sysfs only in this subprocess's mount namespace. The fixture
	// makes two real netdevs resolve to one PCI function during discovery.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", "/sys", "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sysnetPath, 0755); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"macvlan", "ipvlan"} {
		t.Run(kind, func(t *testing.T) {
			for _, parentName := range []string{"parent0", "parent1"} {
				parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: parentName}}
				if err := netlink.LinkAdd(parent); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := netlink.LinkDel(parent); err != nil {
						t.Error(err)
					}
				})
				childName := parentName + "child"
				attrs := netlink.LinkAttrs{Name: childName, ParentIndex: parent.Attrs().Index}
				var child netlink.Link
				if kind == "macvlan" {
					child = &netlink.Macvlan{LinkAttrs: attrs, Mode: netlink.MACVLAN_MODE_BRIDGE}
				} else {
					child = &netlink.IPVlan{LinkAttrs: attrs, Mode: netlink.IPVLAN_MODE_L2}
				}
				if err := netlink.LinkAdd(child); err != nil {
					t.Fatal(err)
				}
				for name, target := range map[string]string{
					parentName: "/sys/devices/pci0000:00/0000:01:00.0/net/" + parentName,
					childName:  "/sys/devices/virtual/net/" + childName,
				} {
					if err := os.MkdirAll(target, 0755); err != nil {
						t.Fatal(err)
					}
					linkPath := filepath.Join(sysnetPath, name)
					if err := os.Symlink(target, linkPath); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := os.Remove(linkPath); err != nil {
							t.Error(err)
						}
					})
				}
			}

			hardware := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrNUMANode:     {IntValue: ptr.To(int64(1))},
				apis.AttrPCIVendor:    {StringValue: ptr.To("vendor")},
				apis.AttrPCIDevice:    {StringValue: ptr.To("product")},
				apis.AttrPCISubsystem: {StringValue: ptr.To("subsystem")},
				deviceattribute.StandardDeviceAttributeNUMANode: {IntValue: ptr.To(int64(1))},
				deviceattribute.StandardDeviceAttributePCIeRoot: {StringValue: ptr.To("pci0000:00")},
			}
			physical := hostDevice{Device: resourceapi.Device{
				Name: "pci-0000-01-00-0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:01:00.0")},
				},
			}}
			for key, value := range hardware {
				physical.Attributes[key] = value
			}
			db := &DB{}
			devices := db.discoverNetworkInterfaces([]hostDevice{physical})
			if len(devices) != 3 {
				t.Fatalf("discovered %d devices, want one PCI device and two children", len(devices))
			}
			wantChildren := make(map[string]resourceapi.Device)
			for _, device := range devices[1:] {
				want := device.Device.DeepCopy()
				for key, value := range hardware {
					want.Attributes[key] = *value.DeepCopy()
				}
				wantChildren[want.Name] = *want
			}
			inheritParentAttributes(devices)
			for _, name := range []string{"parent0child", "parent1child"} {
				found := false
				for _, device := range devices[1:] {
					if device.Name == name {
						found = true
						if diff := cmp.Diff(wantChildren[name], device.Device); diff != "" {
							t.Errorf("%s did not inherit hardware attributes (-want +got):\n%s", name, diff)
						}
					}
				}
				if !found {
					t.Errorf("child %s was not discovered as an independent device", name)
				}
			}
		})
	}
}
