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
	"context"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

// DeviceIdentifiers contains locally discovered hardware identifiers
// that a cloud provider can use to match against its metadata.
type DeviceIdentifiers struct {
	MAC        string `json:"mac_address,omitempty"`
	PCIAddress string `json:"pci_address,omitempty"`
	// Name is the local network interface name, or empty if unavailable.
	Name string `json:"name"`
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

// RuntimeProvider is an optional interface implemented by providers that
// configure a Pod's network namespace themselves, once DRANET has attached and
// configured the Pod's devices and before the Pod's first container starts.
// It is the provider's cloud-init: DRANET pauses the container runtime, calls
// ConfigurePod with everything it knows about the Pod, and lets the container
// start only when the call succeeds.
type RuntimeProvider interface {
	// ConfigurePod configures the Pod's network namespace. It runs once per Pod
	// sandbox, before the first container starts, and again on the next
	// container creation when it fails, so it must be idempotent. The context
	// carries the deadline; an error fails the container start with its
	// message, and the kubelet retries.
	ConfigurePod(ctx context.Context, req *ConfigurePodRequest) error
}

// ConfigurePodRequest describes a Pod whose devices DRANET attached and
// configured, for RuntimeProvider.ConfigurePod.
type ConfigurePodRequest struct {
	// Pod identifies the Pod.
	Pod PodReference `json:"pod"`
	// NetNS is the path of the Pod's network namespace on the node.
	NetNS string `json:"netns"`
	// Claims are the Pod's ResourceClaims served by DRANET, sorted by
	// namespace and name, with their devices.
	Claims []PodClaim `json:"claims"`
}

// PodReference identifies a Pod.
type PodReference struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

// PodClaim is one ResourceClaim of the Pod with the devices DRANET
// prepared for it.
type PodClaim struct {
	// Claim is the ResourceClaim as DRANET received it when it prepared the
	// devices, without managedFields. Only its namespace and name are set
	// for devices prepared by a DRANET version that did not keep the claim.
	Claim *resourceapi.ResourceClaim `json:"claim"`
	// Devices are the claim's devices, sorted by name.
	Devices []PodDevice `json:"devices"`
}

// PodDevice is one device of a claim.
type PodDevice struct {
	// Name is the device name in the ResourceSlice and in the claim's
	// allocation.
	Name string `json:"name"`
	// Device identifies the device on the node, as the providers see it.
	Device DeviceIdentifiers `json:"device"`
	// Config is the network configuration DRANET applied to the device in
	// the Pod's network namespace; unset for RDMA-only devices.
	Config *apis.NetworkConfig `json:"config,omitempty"`
	// RDMADevice is the RDMA link device (e.g. mlx5_0), if the device has one.
	RDMADevice string `json:"rdmaDevice,omitempty"`
}

// DeviceIdentifiersFromDevice extracts the identifiers from the device attributes.
func DeviceIdentifiersFromDevice(device *resourceapi.Device) DeviceIdentifiers {
	id := DeviceIdentifiers{}
	if device == nil {
		return id
	}
	if attr, ok := device.Attributes[apis.AttrInterfaceName]; ok && attr.StringValue != nil {
		id.Name = *attr.StringValue
	}
	if attr, ok := device.Attributes[apis.AttrMac]; ok && attr.StringValue != nil {
		id.MAC = *attr.StringValue
	}
	if attr, ok := device.Attributes[apis.AttrPCIAddress]; ok && attr.StringValue != nil {
		id.PCIAddress = *attr.StringValue
	}
	return id
}
