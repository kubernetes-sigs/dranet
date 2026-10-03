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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

// DeviceIdentifiers contains locally discovered hardware identifiers
// that a cloud provider can use to match against its metadata.
type DeviceIdentifiers = apis.DeviceIdentifiers

// CloudInstance defines the generic interface for all cloud providers.
type CloudInstance interface {
	// GetDeviceAttributes takes multiple identifiers, allowing the provider
	// to choose the best way to match the local device to cloud metadata.
	GetDeviceAttributes(id DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute

	// GetDeviceConfig allows a cloud provider to return an infrastructure-specific
	// network configuration for a given device.
	GetDeviceConfig(id DeviceIdentifiers) *apis.NetworkConfig
}

// ProfileProvider is an optional interface implemented by cloud or webhook providers
// that support on-demand, stateful network configurations based on user profiles.
type ProfileProvider interface {
	// GetProfileConfig resolves a logical profile name for a given hardware device
	// and claim. It performs stateful operations (like allocating an IP address)
	// and returns the resulting network config to be merged with the base config.
	GetProfileConfig(id DeviceIdentifiers, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error)

	// ReleaseProfileConfig frees any stateful resources (like IP leases) that were
	// previously allocated for the given claim and profile.
	ReleaseProfileConfig(id DeviceIdentifiers, claimUID types.UID, config *apis.NetworkConfig) error
}

// RuntimeHookProvider is an optional interface implemented by profile providers
// that need to run a binary on the node when the Pod's containers are created,
// after DRANET attached and configured the devices. It is called at prepare
// time, after GetProfileConfig, with the resolved configuration.
type RuntimeHookProvider interface {
	// GetRuntimeHook returns the hook for the device, or nil when the profile
	// needs none.
	GetRuntimeHook(id DeviceIdentifiers, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.RuntimeHook, error)
}
