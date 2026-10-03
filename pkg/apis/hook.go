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

// Contract of the OCI createRuntime hook DRANET adds to the containers of a
// Pod whose devices are still being attached: it waits on the daemon's socket
// until they are attached. The environment and the wait endpoint are in the
// dependency-free package apis/hook, so hook binaries stay small; the models
// below describe the Pod's devices to them.

// HookDevice describes one device of the Pod to the hook: what DRANET
// decided to attach, in the same models the providers receive. It is carried
// in the hook.EnvDevices environment variable, as a JSON array.
type HookDevice struct {
	// Name is the device name in the ResourceSlice and the ResourceClaim.
	Name string `json:"name"`
	// Claim is the ResourceClaim the device is allocated to.
	Claim HookClaimRef `json:"claim"`
	// Host identifies the device on the host, before the attach.
	Host DeviceIdentifiers `json:"host"`
	// Interface is the network interface name inside the Pod. Empty for
	// devices without a network interface.
	Interface string `json:"interface,omitempty"`
	// RDMALinkDev is the RDMA link device, if any. It is moved into the Pod
	// only when the RDMA subsystem is in exclusive network namespace mode.
	RDMALinkDev string `json:"rdmaLinkDev,omitempty"`
	// Config is the network configuration DRANET applies inside the Pod.
	Config *NetworkConfig `json:"config,omitempty"`
}

// DeviceAttachTimeoutMaxSeconds is the most --device-attach-timeout may be.
// DRANET's hook gets the Pod's remaining attach time as its timeout, and the
// hooks of a container run inside the start of the Pod's first container,
// which the kubelet bounds with its runtime request timeout (2m by default).
const DeviceAttachTimeoutMaxSeconds = 60

// HookClaimRef identifies a ResourceClaim.
type HookClaimRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
