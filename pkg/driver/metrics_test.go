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

package driver

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
)

func TestUpdateClaimDeviceMetrics(t *testing.T) {
	store := mustNewPodConfigStore()
	np := &NetworkDriver{podConfigStore: store}

	np.updateClaimDeviceMetrics()
	if got := testutil.ToFloat64(claimDevicesAllocated); got != 0 {
		t.Errorf("claimDevicesAllocated on empty store = %v, want 0", got)
	}
	if got := testutil.ToFloat64(claimDevicesReady); got != 0 {
		t.Errorf("claimDevicesReady on empty store = %v, want 0", got)
	}

	podUID := types.UID("test-pod")
	if err := store.SetDeviceConfig(podUID, "dev0", DeviceConfig{}); err != nil {
		t.Fatalf("SetDeviceConfig(dev0) failed: %v", err)
	}
	if err := store.SetDeviceConfig(podUID, "dev1", DeviceConfig{}); err != nil {
		t.Fatalf("SetDeviceConfig(dev1) failed: %v", err)
	}
	np.updateClaimDeviceMetrics()
	if got := testutil.ToFloat64(claimDevicesAllocated); got != 2 {
		t.Errorf("claimDevicesAllocated after allocating 2 devices = %v, want 2", got)
	}
	if got := testutil.ToFloat64(claimDevicesReady); got != 0 {
		t.Errorf("claimDevicesReady after allocating 2 devices = %v, want 0", got)
	}

	// Only dev0 completes: a hang on dev1 must not
	// prevent the gauge from reflecting dev0's true, already-confirmed state.
	if err := store.SetDeviceReady(podUID, "dev0", true); err != nil {
		t.Fatalf("SetDeviceReady(dev0, true) failed: %v", err)
	}
	np.updateClaimDeviceMetrics()
	if got := testutil.ToFloat64(claimDevicesAllocated); got != 2 {
		t.Errorf("claimDevicesAllocated after dev0 ready = %v, want 2", got)
	}
	if got := testutil.ToFloat64(claimDevicesReady); got != 1 {
		t.Errorf("claimDevicesReady after dev0 ready = %v, want 1", got)
	}
}
