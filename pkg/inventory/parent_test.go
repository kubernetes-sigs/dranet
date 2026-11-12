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
	"github.com/google/go-cmp/cmp"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
	"testing"
)

func TestInheritParentAttributes(t *testing.T) {
	makeDevice := func(index, parent int, kind string) hostDevice {
		return hostDevice{linkIndex: index, parentIndex: parent, Device: resourceapi.Device{
			Name: kind, Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrType:          {StringValue: ptr.To(kind)},
				apis.AttrInterfaceName: {StringValue: ptr.To(kind)},
				apis.AttrMac:           {StringValue: ptr.To("child-mac")},
				apis.AttrMTU:           {IntValue: ptr.To(int64(1500))},
				apis.AttrState:         {StringValue: ptr.To("unknown")},
				apis.AttrVirtual:       {BoolValue: ptr.To(true)},
			},
		}}
	}
	for _, tc := range []struct {
		name    string
		kind    string
		parent  int
		inherit bool
	}{
		{"ipvlan", "ipvlan", 1, true}, {"macvlan", "macvlan", 1, true},
		{"nested", "ipvlan", 2, true}, {"vlan parent", "macvlan", 3, true},
		{"missing parent", "ipvlan", 99, false}, {"no parent", "macvlan", 0, false},
		{"cycle", "ipvlan", 4, false}, {"self cycle", "macvlan", 10, false},
		{"veth peer", "veth", 1, false}, {"veth ancestor", "ipvlan", 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			physical := makeDevice(1, 0, "device")
			physical.Attributes[apis.AttrPCIAddress] = resourceapi.DeviceAttribute{StringValue: ptr.To("0000:01:00.0")}
			physical.Attributes[apis.AttrRDMA] = resourceapi.DeviceAttribute{BoolValue: ptr.To(true)}
			physical.Attributes[apis.AttrRDMADevice] = resourceapi.DeviceAttribute{StringValue: ptr.To("mlx5_0")}
			physical.Attributes[apis.AttrIPv4] = resourceapi.DeviceAttribute{StringValue: ptr.To("192.0.2.1/24")}
			physical.Attributes[apis.AttrMac] = resourceapi.DeviceAttribute{StringValue: ptr.To("parent-mac")}
			physical.Attributes[apis.AttrMTU] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(9000))}
			hardware := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrNUMANode:     {IntValue: ptr.To(int64(1))},
				apis.AttrPCIVendor:    {StringValue: ptr.To("vendor")},
				apis.AttrPCIDevice:    {StringValue: ptr.To("product")},
				apis.AttrPCISubsystem: {StringValue: ptr.To("subsystem")},
				deviceattribute.StandardDeviceAttributeNUMANode: {IntValues: []int64{1, 2}},
				deviceattribute.StandardDeviceAttributePCIeRoot: {StringValue: ptr.To("pci0000:00")},
			}
			for key, value := range hardware {
				physical.Attributes[key] = value
			}
			child := makeDevice(10, tc.parent, tc.kind)
			want := child.Device.DeepCopy()
			if tc.inherit {
				for key, value := range hardware {
					want.Attributes[key] = *value.DeepCopy()
				}
			}
			// Children precede their parents, and two siblings share one physical NIC.
			devices := []hostDevice{child, makeDevice(11, 1, "macvlan"), makeDevice(2, 1, "macvlan"), makeDevice(3, 1, "vlan"), makeDevice(4, 5, "ipvlan"), makeDevice(5, 4, "macvlan"), makeDevice(6, 1, "veth"), physical}
			beforeParent := physical.Device.DeepCopy()
			inheritParentAttributes(devices)
			if diff := cmp.Diff(*want, devices[0].Device); diff != "" {
				t.Fatalf("child mismatch (-want +got): %s", diff)
			}
			if diff := cmp.Diff(*beforeParent, devices[len(devices)-1].Device); diff != "" {
				t.Fatalf("parent changed: %s", diff)
			}
			for key, value := range hardware {
				if diff := cmp.Diff(value, devices[1].Attributes[key]); diff != "" {
					t.Errorf("sibling attribute %s: %s", key, diff)
				}
			}
			if tc.inherit {
				devices[0].Attributes[deviceattribute.StandardDeviceAttributeNUMANode].IntValues[0] = 99
				if physical.Attributes[deviceattribute.StandardDeviceAttributeNUMANode].IntValues[0] != 1 {
					t.Fatal("child aliases parent NUMA list")
				}
			}
		})
	}
}
