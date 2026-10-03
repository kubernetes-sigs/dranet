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

package apis

import (
	resourceapi "k8s.io/api/resource/v1"
)

// DeviceIdentifiers contains locally discovered hardware identifiers of a
// device, the keys a provider uses to match it against its own metadata.
type DeviceIdentifiers struct {
	MAC        string `json:"mac_address,omitempty"`
	PCIAddress string `json:"pci_address,omitempty"`
	// Name is the local network interface name, or empty if unavailable.
	Name string `json:"name"`
}

// DeviceIdentifiersFromDevice extracts the identifiers from the device attributes.
func DeviceIdentifiersFromDevice(device *resourceapi.Device) DeviceIdentifiers {
	id := DeviceIdentifiers{}
	if device == nil {
		return id
	}
	if attr, ok := device.Attributes[AttrInterfaceName]; ok && attr.StringValue != nil {
		id.Name = *attr.StringValue
	}
	if attr, ok := device.Attributes[AttrMac]; ok && attr.StringValue != nil {
		id.MAC = *attr.StringValue
	}
	if attr, ok := device.Attributes[AttrPCIAddress]; ok && attr.StringValue != nil {
		id.PCIAddress = *attr.StringValue
	}
	return id
}
