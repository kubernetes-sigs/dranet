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
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Contract of the OCI createRuntime hooks DRANET adds to the containers of a
// Pod: DRANET's own hook waits on the daemon's socket until the Pod's devices
// are attached; a profile provider's hooks run after it. The environment and
// the wait endpoint are in the dependency-free package apis/hook, so hook
// binaries stay small; the models below describe the Pod's devices to them.

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
	// Data is the opaque data the profile provider attached to its runtime
	// hook for this device (RuntimeHook.Data). DRANET does not interpret it.
	Data json.RawMessage `json:"data,omitempty"`
}

// HookClaimRef identifies a ResourceClaim.
type HookClaimRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// The hooks of a container run inside the start of the Pod's first container,
// which the kubelet bounds with its runtime request timeout (2m by default),
// so every timeout below is capped and the caps add up to well under it.
const (
	// DeviceAttachTimeoutMaxSeconds is the most --device-attach-timeout may
	// be; DRANET's own hook gets the Pod's remaining attach time as timeout.
	DeviceAttachTimeoutMaxSeconds = 60
	// RuntimeHookDefaultTimeoutSeconds applies when the provider sets none.
	RuntimeHookDefaultTimeoutSeconds = 10
	// RuntimeHookMaxTimeoutSeconds is the most a provider hook may ask for.
	RuntimeHookMaxTimeoutSeconds = 30
	// RuntimeHookChainMaxTimeoutSeconds bounds the sum of the timeouts of all
	// the hooks of one container: the longest barrier plus one provider hook
	// at its maximum always fit.
	RuntimeHookChainMaxTimeoutSeconds = DeviceAttachTimeoutMaxSeconds + RuntimeHookMaxTimeoutSeconds
)

// RuntimeHook is a profile provider's decision to run a binary on the node
// once per Pod, when the Pod's first container starts and after DRANET
// attached and configured the Pod's devices, to apply post-configuration to
// the Pod's network namespace. The binary receives the hook.Env*
// environment, with Data in its device's entry of hook.EnvDevices. It may
// only change network state in the namespace at hook.EnvNetNS; it must not
// use the OCI container state on its stdin. It must be idempotent: it runs
// again when the kubelet replaces a container that failed to start. A
// non-zero exit fails the container start with its stderr in the Pod's
// events.
type RuntimeHook struct {
	// Path is the absolute path of the binary on the host.
	Path string `json:"path"`
	// Args are the arguments of the binary, after its name.
	Args []string `json:"args,omitempty"`
	// TimeoutSeconds is how long the runtime lets the hook run before it
	// kills it and fails the container start. Defaults to
	// RuntimeHookDefaultTimeoutSeconds; at most RuntimeHookMaxTimeoutSeconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Data is passed to the hook as is, in its device's HookDevice.Data.
	Data json.RawMessage `json:"data,omitempty"`
}

// Validate checks the hook and applies the timeout default.
func (h *RuntimeHook) Validate() error {
	if h.Path == "" {
		return fmt.Errorf("runtime hook path is empty")
	}
	if !filepath.IsAbs(h.Path) {
		return fmt.Errorf("runtime hook path %q is not absolute", h.Path)
	}
	if h.TimeoutSeconds < 0 {
		return fmt.Errorf("runtime hook timeout %d is negative", h.TimeoutSeconds)
	}
	if h.TimeoutSeconds == 0 {
		h.TimeoutSeconds = RuntimeHookDefaultTimeoutSeconds
	}
	if h.TimeoutSeconds > RuntimeHookMaxTimeoutSeconds {
		return fmt.Errorf("runtime hook timeout %ds is over the maximum of %ds", h.TimeoutSeconds, RuntimeHookMaxTimeoutSeconds)
	}
	if len(h.Data) > 0 && !json.Valid(h.Data) {
		return fmt.Errorf("runtime hook data is not valid JSON")
	}
	return nil
}
