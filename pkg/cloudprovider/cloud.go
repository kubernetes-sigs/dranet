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

package cloudprovider

import (
	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
)

// DeviceIdentifiers contains locally discovered hardware identifiers
// that a cloud provider can use to match against its metadata.
type DeviceIdentifiers struct {
	MAC        string
	PCIAddress string
	Name       string
	RDMA       bool
}

// NonUplinkChecker is an optional extension to CloudInstance. A provider may
// implement this to exempt specific device classes from the default-gateway
// uplink filter, even when those interfaces carry a default route. This is
// needed on platforms where infrastructure daemons (e.g. RA) inject default
// routes onto workload RDMA NICs as a side-effect of their setup.
type NonUplinkChecker interface {
	// IsNonUplink returns true if the device should be included in the
	// ResourceSlice regardless of any default gateway route on that interface.
	IsNonUplink(id DeviceIdentifiers) bool
}

// CloudInstance defines the generic interface for all cloud providers.
type CloudInstance interface {
	// GetDeviceAttributes takes multiple identifiers, allowing the provider
	// to choose the best way to match the local device to cloud metadata.
	GetDeviceAttributes(id DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute

	// GetDeviceConfig allows a cloud provider to return an infrastructure-specific
	// network configuration for a given device.
	GetDeviceConfig(id DeviceIdentifiers) *apis.NetworkConfig
}
