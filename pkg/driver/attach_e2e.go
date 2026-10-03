//go:build e2e

/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"os"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/apis"
)

// Compiled only into the end-to-end image (-tags e2e). DRANET_E2E_ATTACH_DELAY
// slows every device attach down, so the tests reproduce hardware whose attach
// does not fit in the container runtime's NRI request timeout.
func init() {
	value := os.Getenv("DRANET_E2E_ATTACH_DELAY")
	if value == "" {
		return
	}
	delay, err := time.ParseDuration(value)
	if err != nil || delay <= 0 {
		klog.Fatalf("DRANET_E2E_ATTACH_DELAY=%q is not a positive duration: %v", value, err)
	}
	klog.Warningf("end-to-end build: every network device attach is delayed by %s", delay)
	attach := attachNetdev
	attachNetdev = func(hostIfName string, containerNsPath string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
		time.Sleep(delay)
		return attach(hostIfName, containerNsPath, interfaceConfig)
	}
}
