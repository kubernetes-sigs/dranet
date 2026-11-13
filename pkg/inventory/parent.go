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
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"sigs.k8s.io/dranet/pkg/apis"
)

// inheritParentAttributes runs after PCI enrichment, before uplink filtering.
// Only hardware metadata is copied: PCI identity, link attributes and RDMA
// capabilities must continue to describe the child itself.
func inheritParentAttributes(devices []hostDevice) {
	byIndex := make(map[int]*hostDevice, len(devices))
	for i := range devices {
		for _, index := range devices[i].linkIndices {
			if index != 0 {
				byIndex[index] = &devices[i]
			}
		}
	}
	for i := range devices {
		child := &devices[i]
		kind := child.Attributes[apis.AttrType].StringValue
		if kind == nil || (*kind != "ipvlan" && *kind != "macvlan") {
			continue
		}
		if child.Attributes[apis.AttrPCIAddress].StringValue != nil {
			continue
		}
		seen := make(map[int]bool)
		for _, index := range child.linkIndices {
			seen[index] = true
		}
		for index := child.parentIndex; index != 0; {
			if seen[index] {
				break
			}
			seen[index] = true
			parent, ok := byIndex[index]
			if !ok {
				break
			}
			if parent.Attributes[apis.AttrPCIAddress].StringValue != nil {
				for _, key := range []resourceapi.QualifiedName{
					apis.AttrNUMANode, apis.AttrPCIVendor, apis.AttrPCIDevice, apis.AttrPCISubsystem,
					deviceattribute.StandardDeviceAttributeNUMANode, deviceattribute.StandardDeviceAttributePCIeRoot,
				} {
					if _, exists := child.Attributes[key]; exists {
						continue
					}
					if value, exists := parent.Attributes[key]; exists {
						child.Attributes[key] = *value.DeepCopy()
					}
				}
				break
			}
			kind := parent.Attributes[apis.AttrType].StringValue
			if kind == nil || (*kind != "ipvlan" && *kind != "macvlan" && *kind != "vlan") {
				break
			}
			index = parent.parentIndex
		}
	}
}
