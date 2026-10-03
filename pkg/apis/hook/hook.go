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

// Package hook is the contract between the DRANET daemon and the OCI
// createRuntime hooks it adds to the containers of a Pod: the hook process
// environment and the daemon's wait endpoint. It has no dependencies, so a
// hook binary stays small; the device model carried in EnvDevices is
// apis.HookDevice in the parent package.
package hook

const (
	// SocketPath is the unix socket the daemon serves WaitPath on.
	// /var/run/dranet is a host path shared between the daemon and the host.
	SocketPath = "/var/run/dranet/hook.sock"
	// WaitPath blocks until the Pod's devices are attached. Query parameter:
	// pod=<Pod UID>. The response body is a WaitResponse: 200 when all
	// devices are attached, 500 when the attach failed or gave up, 400 for a
	// malformed request.
	WaitPath = "/wait"

	// Environment of the hook process.
	EnvPodUID        = "DRANET_POD_UID"
	EnvPodNamespace  = "DRANET_POD_NAMESPACE"
	EnvPodName       = "DRANET_POD_NAME"
	EnvContainerName = "DRANET_CONTAINER_NAME"
	// EnvNetNS is the path of the Pod's network namespace.
	EnvNetNS = "DRANET_NETNS"
	// EnvSocket is the unix socket serving WaitPath.
	EnvSocket = "DRANET_SOCKET"
	// EnvDevices is a JSON array with one apis.HookDevice per device of the Pod.
	EnvDevices = "DRANET_DEVICES"
)

// WaitResponse is the body of a WaitPath response.
type WaitResponse struct {
	PodUID string `json:"podUID"`
	// Attached and Total count the Pod's devices that need attaching.
	Attached int `json:"attached"`
	Total    int `json:"total"`
	// Error is set when the attach failed or gave up.
	Error string `json:"error,omitempty"`
}
