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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vishvananda/netlink"
	resourcev1 "k8s.io/api/resource/v1"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

func TestPublishResourcesPrometheusMetrics(t *testing.T) {
	testCases := []struct {
		name          string
		devices       []resourcev1.Device
		expectedRdma  float64
		expectedTotal float64
	}{
		{
			name:          "No devices",
			devices:       []resourcev1.Device{},
			expectedRdma:  0,
			expectedTotal: 0,
		},
		{
			name: "Only RDMA devices",
			devices: []resourcev1.Device{
				{Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					apis.AttrRDMA: {BoolValue: func() *bool { b := true; return &b }()},
				}},
			},
			expectedRdma:  1,
			expectedTotal: 1,
		},
		{
			name: "Only non-RDMA devices",
			devices: []resourcev1.Device{
				{Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					apis.AttrRDMA: {BoolValue: func() *bool { b := false; return &b }()},
				}},
			},
			expectedRdma:  0,
			expectedTotal: 1,
		},
		{
			name: "Mixed RDMA and non-RDMA devices",
			devices: []resourcev1.Device{
				{Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					apis.AttrRDMA: {BoolValue: func() *bool { b := true; return &b }()},
				}},
				{Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					apis.AttrRDMA: {BoolValue: func() *bool { b := true; return &b }()},
				}},
				{Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					apis.AttrRDMA: {BoolValue: func() *bool { b := false; return &b }()},
				}},
			},
			expectedRdma:  2,
			expectedTotal: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			publishedDevicesTotal.Reset()
			np := &NetworkDriver{}
			np.publishResourcesPrometheusMetrics(tc.devices)

			if got := testutil.ToFloat64(publishedDevicesTotal.WithLabelValues("rdma")); got != tc.expectedRdma {
				t.Errorf("Expected %f for RDMA devices, got %f", tc.expectedRdma, got)
			}
			if got := testutil.ToFloat64(publishedDevicesTotal.WithLabelValues("total")); got != tc.expectedTotal {
				t.Errorf("Expected %f for Total devices, got %f", tc.expectedTotal, got)
			}
		})
	}
}

func TestPrepareResourceClaimsMetrics(t *testing.T) {
	ctx := context.Background()

	t.Run("Success Case", func(t *testing.T) {
		draPluginRequestsTotal.Reset()
		draPluginRequestsLatencySeconds.Reset()

		np := &NetworkDriver{}
		if _, err := np.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{}); err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}

		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodPrepareResourceClaims, statusSuccess)); got != float64(1) {
			t.Errorf("Expected 1 success, got %f", got)
		}
		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodPrepareResourceClaims, statusFailed)); got != float64(0) {
			t.Errorf("Expected 0 failures, got %f", got)
		}

		expected := `
			# HELP dranet_driver_dra_plugin_requests_latency_seconds DRA plugin request latency in seconds.
			# TYPE dranet_driver_dra_plugin_requests_latency_seconds histogram
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.005"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.01"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.025"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.05"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.1"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.25"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="0.5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="1"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="2.5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="10"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="PrepareResourceClaims",le="+Inf"} 1
		`
		if err := testutil.CollectAndCompare(draPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_dra_plugin_requests_latency_seconds_bucket"); err != nil {
			t.Fatalf("CollectAndCompare failed: %v", err)
		}
	})

	t.Run("Failure Case", func(t *testing.T) {
		draPluginRequestsTotal.Reset()
		draPluginRequestsLatencySeconds.Reset()

		np := &NetworkDriver{
			netdb:          newFakeInventoryDB(),
			driverName:     "test.driver",
			eventRecorder:  record.NewFakeRecorder(100),
			podConfigStore: mustNewPodConfigStore(),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-1"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-1"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: "device-does-not-exist"},
							},
						},
					},
				},
			},
		}

		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-1"].Err == nil {
			t.Errorf("Expected an error for claim-uid-1, but got none")
		}

		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodPrepareResourceClaims, statusSuccess)); got != float64(0) {
			t.Errorf("Expected 0 successes, got %f", got)
		}
		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodPrepareResourceClaims, statusFailed)); got != float64(1) {
			t.Errorf("Expected 1 failure, got %f", got)
		}

		if count := testutil.CollectAndCount(draPluginRequestsLatencySeconds); count != 1 {
			t.Errorf("Expected 1 latency metric, got %d", count)
		}
	})
}

func TestUnprepareResourceClaimsMetrics(t *testing.T) {
	ctx := context.Background()

	t.Run("Success Case", func(t *testing.T) {
		draPluginRequestsTotal.Reset()
		draPluginRequestsLatencySeconds.Reset()

		np := &NetworkDriver{
			podConfigStore: mustNewPodConfigStore(),
			netdb:          newFakeInventoryDB(),
		}
		claimName := types.NamespacedName{Name: "test-claim", Namespace: "test-ns"}
		np.podConfigStore.SetDeviceConfig("pod-uid-1", "device-a", DeviceConfig{Claim: claimName})

		claims := []kubeletplugin.NamespacedObject{
			{NamespacedName: claimName, UID: "claim-uid-1"},
		}

		if _, err := np.UnprepareResourceClaims(ctx, claims); err != nil {
			t.Fatalf("UnprepareResourceClaims failed: %v", err)
		}

		// Verify the claim was removed from the store
		if _, ok := np.podConfigStore.GetPodConfig("pod-uid-1"); ok {
			t.Errorf("Pod config should have been removed, but was found")
		}

		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodUnprepareResourceClaims, statusSuccess)); got != float64(1) {
			t.Errorf("Expected 1 success, got %f", got)
		}
		if got := testutil.ToFloat64(draPluginRequestsTotal.WithLabelValues(methodUnprepareResourceClaims, statusFailed)); got != float64(0) {
			t.Errorf("Expected 0 failures, got %f", got)
		}

		expected := `
			# HELP dranet_driver_dra_plugin_requests_latency_seconds DRA plugin request latency in seconds.
			# TYPE dranet_driver_dra_plugin_requests_latency_seconds histogram
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.005"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.01"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.025"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.05"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.1"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.25"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="0.5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="1"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="2.5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="5"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="10"} 1
			dranet_driver_dra_plugin_requests_latency_seconds_bucket{method="UnprepareResourceClaims",le="+Inf"} 1
		`
		if err := testutil.CollectAndCompare(draPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_dra_plugin_requests_latency_seconds_bucket"); err != nil {
			t.Fatalf("CollectAndCompare failed: %v", err)
		}
	})
}

func TestClaimPrepareFailedEvent(t *testing.T) {
	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(10)

	np := &NetworkDriver{
		netdb:          newFakeInventoryDB(),
		driverName:     "test.driver",
		eventRecorder:  fakeRecorder,
		podConfigStore: mustNewPodConfigStore(),
	}

	claims := []*resourcev1.ResourceClaim{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-claim",
				Namespace: "default",
				UID:       "claim-uid-1",
			},
			Status: resourcev1.ResourceClaimStatus{
				ReservedFor: []resourcev1.ResourceClaimConsumerReference{
					{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-1"},
				},
				Allocation: &resourcev1.AllocationResult{
					Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{
							{Driver: "test.driver", Device: "device-does-not-exist"},
						},
					},
				},
			},
		},
	}

	res, err := np.PrepareResourceClaims(ctx, claims)
	if err != nil {
		t.Fatalf("PrepareResourceClaims returned unexpected error: %v", err)
	}
	if res["claim-uid-1"].Err == nil {
		t.Fatal("expected per-claim error, got none")
	}

	select {
	case event := <-fakeRecorder.Events:
		if !strings.Contains(event, "ClaimPrepareFailed") {
			t.Errorf("expected ClaimPrepareFailed event, got: %s", event)
		}
	default:
		t.Error("expected a ClaimPrepareFailed event to be emitted, but none was received")
	}
}

func TestPublishResourcesMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fakeDraPlugin := newFakePluginHelper()
	fakeNetDB := newFakeInventoryDB()

	np := &NetworkDriver{
		draPlugin: fakeDraPlugin,
		netdb:     fakeNetDB,
		nodeName:  "test-node",
	}

	go np.PublishResources(ctx)

	t.Run("Success", func(t *testing.T) {
		lastPublishedTime.Set(0)
		fakeNetDB.resources <- []resourcev1.Device{}
		<-fakeDraPlugin.publishCalled

		if testutil.ToFloat64(lastPublishedTime) == 0 {
			t.Errorf("lastPublishedTime should have been updated, but it is 0")
		}
	})

	t.Run("Failure", func(t *testing.T) {
		lastPublishedTime.Set(0)
		fakeDraPlugin.publishErr = fmt.Errorf("mock publish error")
		fakeNetDB.resources <- []resourcev1.Device{}
		<-fakeDraPlugin.publishCalled

		if testutil.ToFloat64(lastPublishedTime) != 0 {
			t.Errorf("lastPublishedTime should not have been updated, but it is %f", testutil.ToFloat64(lastPublishedTime))
		}
	})
}

func TestValidateVFMTU(t *testing.T) {
	testCases := []struct {
		name         string
		requestedMTU int
		pfMTU        int
		wantErr      bool
	}{
		{
			name:         "requested MTU below PF MTU is allowed",
			requestedMTU: 1500,
			pfMTU:        9000,
			wantErr:      false,
		},
		{
			name:         "requested MTU equal to PF MTU is allowed",
			requestedMTU: 9000,
			pfMTU:        9000,
			wantErr:      false,
		},
		{
			name:         "requested MTU above PF MTU is rejected",
			requestedMTU: 9000,
			pfMTU:        1500,
			wantErr:      true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVFMTU("eth1", "eth0", tc.requestedMTU, tc.pfMTU)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateVFMTU() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDynamicProfiles(t *testing.T) {
	ctx := context.Background()

	t.Run("Success Case", func(t *testing.T) {
		fakeDB := newFakeInventoryDB()
		fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			return &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{"10.0.0.1/24"},
				},
			}, nil
		}
		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
		}
		fakeDB.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
			return "eth0", nil
		}
		fakeDB.IsIBOnlyDeviceFunc = func(deviceName string) bool {
			return true
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-1", Namespace: "default", Name: "claim1"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-1"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: "device-1", Request: "req-1"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{},
						},
					},
				},
			},
		}

		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-1"].Err != nil {
			t.Fatalf("Expected no error, got %v", res["claim-uid-1"].Err)
		}

		// Verify merge success
		podCfg, ok := np.podConfigStore.GetPodConfig("pod-uid-1")
		if !ok {
			t.Fatalf("Expected pod config to be stored")
		}
		devCfg := podCfg.DeviceConfigs["device-1"]
		if len(devCfg.NetworkInterfaceConfigInPod.Interface.Addresses) == 0 || devCfg.NetworkInterfaceConfigInPod.Interface.Addresses[0] != "10.0.0.1/24" {
			t.Errorf("Expected address 10.0.0.1/24 to be merged into pod config, got %v", devCfg.NetworkInterfaceConfigInPod.Interface.Addresses)
		}
	})

	t.Run("Unsupported Provider Case", func(t *testing.T) {
		fakeDB := newFakeInventoryDB()
		fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			return nil, fmt.Errorf("current cloud provider does not support dynamic profiles")
		}
		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
		}
		fakeDB.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
			return "eth0", nil
		}
		fakeDB.IsIBOnlyDeviceFunc = func(deviceName string) bool {
			return true
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-unsupported", Namespace: "default", Name: "claim-unsup"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-unsupported"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: "device-1", Request: "req-1"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{},
						},
					},
				},
			},
		}

		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-unsupported"].Err == nil || !strings.Contains(res["claim-uid-unsupported"].Err.Error(), "does not support dynamic profiles") {
			t.Fatalf("Expected unsupported profile error, got %v", res["claim-uid-unsupported"].Err)
		}
	})

	t.Run("Allocation Failure Case", func(t *testing.T) {
		fakeDB := newFakeInventoryDB()
		fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			return nil, fmt.Errorf("ipam allocation failed")
		}
		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
		}
		fakeDB.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
			return "eth0", nil
		}
		fakeDB.IsIBOnlyDeviceFunc = func(deviceName string) bool {
			return true
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-fail", Namespace: "default", Name: "claim-fail"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-fail"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: "device-1", Request: "req-1"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{},
						},
					},
				},
			},
		}

		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-fail"].Err == nil || !strings.Contains(res["claim-uid-fail"].Err.Error(), "ipam allocation failed") {
			t.Fatalf("Expected ipam allocation failed error, got %v", res["claim-uid-fail"].Err)
		}
	})

	t.Run("Teardown Success Case", func(t *testing.T) {
		released := false
		fakeDB := newFakeInventoryDB()
		fakeDB.ReleaseProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
			released = true
			if config.Profile != "my-profile" {
				t.Errorf("Expected profile 'my-profile', got %v", config.Profile)
			}
			if claimUID != "claim-uid-td" {
				t.Errorf("Expected claimUID 'claim-uid-td', got %v", claimUID)
			}
			return nil
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
		}

		claimName := types.NamespacedName{Namespace: "default", Name: "claim-td"}
		// Inject a profile in pod config store
		np.podConfigStore.SetDeviceConfig("pod-uid-td", "device-1", DeviceConfig{
			Claim:                       claimName,
			NetworkInterfaceConfigInPod: apis.NetworkConfig{Profile: "my-profile"},
		})

		claims := []kubeletplugin.NamespacedObject{
			{NamespacedName: claimName, UID: "claim-uid-td"},
		}

		_, err := np.UnprepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("UnprepareResourceClaims failed: %v", err)
		}

		if !released {
			t.Errorf("Expected releaseProfileConfigFunc to be called")
		}
	})

	t.Run("Profile Release on Subsequent Failure", func(t *testing.T) {
		released := false
		var releasedConfig *apis.NetworkConfig
		fakeDB := newFakeInventoryDB()
		fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			return &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{"10.0.0.1/24"},
				},
			}, nil
		}
		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
		}
		fakeDB.ReleaseProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
			released = true
			releasedConfig = config
			if claimUID != "claim-uid-leak" {
				t.Errorf("Expected claimUID 'claim-uid-leak', got %v", claimUID)
			}
			return nil
		}
		// Cause a failure AFTER GetProfileConfig
		fakeDB.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
			return "", fmt.Errorf("simulated failure getting interface name")
		}
		fakeDB.IsIBOnlyDeviceFunc = func(deviceName string) bool {
			return false
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-leak", Namespace: "default", Name: "claim-leak"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-leak"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: "device-1", Request: "req-1"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{},
						},
					},
				},
			},
		}

		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-leak"].Err == nil || !strings.Contains(res["claim-uid-leak"].Err.Error(), "simulated failure") {
			t.Fatalf("Expected simulated failure, got %v", res["claim-uid-leak"].Err)
		}

		// Ensure the deferred cleanup released allocated profile resources and left no store state.
		if !released {
			t.Errorf("Expected the profile config to be released")
		}
		if releasedConfig == nil || len(releasedConfig.Interface.Addresses) != 1 || releasedConfig.Interface.Addresses[0] != "10.0.0.1/24" {
			t.Errorf("Expected the allocated address to be released, got %v", releasedConfig)
		}
		if _, ok := np.podConfigStore.GetPodConfig("pod-uid-leak"); ok {
			t.Errorf("Expected no pod config to be stored for a failed preparation")
		}
	})

	t.Run("Retry Only Reallocates Failed Devices", func(t *testing.T) {
		// deviceA is committed on the first call as an IB-only device; deviceB fails
		// after its profile is allocated. On the second call, deviceA is short-circuited
		// and reuses the stored config, while deviceB allocates a profile again.
		const (
			deviceA = "ib-dev-a"
			deviceB = "net-dev-b"
		)

		fakeDB := newFakeInventoryDB()

		profileCallsByDevice := map[string]int{}
		// Count the profile calls and derive the address from the
		// count, so a second call yields a different address.
		fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			profileCallsByDevice[deviceName]++
			return &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{fmt.Sprintf("10.0.0.%d/24", profileCallsByDevice[deviceName])},
				},
			}, nil
		}

		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
		}
		fakeDB.IsIBOnlyDeviceFunc = func(deviceName string) bool {
			return deviceName == deviceA
		}
		fakeDB.GetRDMADeviceNameFunc = func(deviceName string) (string, error) {
			return "rdma0", nil
		}
		fakeDB.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
			return "", fmt.Errorf("simulated failure getting interface name")
		}

		np := &NetworkDriver{
			netdb:          fakeDB,
			driverName:     "test.driver",
			podConfigStore: mustNewPodConfigStore(),
			eventRecorder:  record.NewFakeRecorder(100),
		}

		claims := []*resourcev1.ResourceClaim{
			{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-retry", Namespace: "default", Name: "claim-retry"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-retry"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: "test.driver", Device: deviceA, Request: "req-a"},
								{Driver: "test.driver", Device: deviceB, Request: "req-b"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{},
						},
					},
				},
			},
		}

		// The first call prepares deviceA and fails on deviceB.
		res, err := np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}
		if res["claim-uid-retry"].Err == nil || !strings.Contains(res["claim-uid-retry"].Err.Error(), "simulated failure") {
			t.Fatalf("Expected simulated failure, got %v", res["claim-uid-retry"].Err)
		}

		// The second call reuses deviceA and resolves deviceB again.
		res, err = np.PrepareResourceClaims(ctx, claims)
		if err != nil {
			t.Fatalf("PrepareResourceClaims failed: %v", err)
		}

		if got := profileCallsByDevice[deviceA]; got != 1 {
			t.Errorf("Expected deviceA to allocate a profile once, got %d", got)
		}
		if got := profileCallsByDevice[deviceB]; got != 2 {
			t.Errorf("Expected deviceB to allocate a profile twice, got %d", got)
		}
		podCfg, _ := np.podConfigStore.GetPodConfig("pod-uid-retry")
		addrs := podCfg.DeviceConfigs[deviceA].NetworkInterfaceConfigInPod.Interface.Addresses
		if len(addrs) != 1 || addrs[0] != "10.0.0.1/24" {
			t.Errorf("Expected deviceA to keep its first address 10.0.0.1/24, got %v", addrs)
		}
	})
}

func TestGetDeviceNetworkConfigWithWebhook(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name              string
		userConf          *apis.NetworkConfig
		cloudConfResponse *apis.NetworkConfig
		profileResponse   *apis.NetworkConfig
		profileStatusCode int
		expectedError     bool
		errContains       string
		expectedAddresses []string
		expectedMTU       int32
		expectedProfile   string
		expectedIPVlan    *apis.IPVlanConfig
	}{
		{
			name:              "No configurations provided",
			userConf:          &apis.NetworkConfig{},
			cloudConfResponse: nil,
			profileResponse:   nil,
			expectedError:     false,
		},
		{
			name: "User configuration only",
			userConf: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1400)},
			},
			expectedMTU:   1400,
			expectedError: false,
		},
		{
			name:     "Cloud configuration only",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1500)},
			},
			expectedMTU:   1500,
			expectedError: false,
		},
		{
			name: "User configuration overrides Cloud configuration",
			userConf: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1400)},
			},
			cloudConfResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1500)},
			},
			expectedMTU:   1400,
			expectedError: false,
		},
		{
			name:     "Profile configuration adds IP address",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Profile: "cloud-profile",
			},
			profileResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{"192.168.1.10/24"},
				},
			},
			profileStatusCode: http.StatusOK,
			expectedAddresses: []string{"192.168.1.10/24"},
			expectedProfile:   "cloud-profile",
			expectedError:     false,
		},
		{
			name: "User configuration overrides Profile configuration",
			userConf: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1400)},
			},
			cloudConfResponse: &apis.NetworkConfig{
				Profile: "cloud-profile",
			},
			profileResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					MTU:       ptr.To[int32](1500),
					Addresses: []string{"192.168.1.10/24"},
				},
			},
			profileStatusCode: http.StatusOK,
			expectedAddresses: []string{"192.168.1.10/24"},
			expectedMTU:       1400,
			expectedProfile:   "cloud-profile",
			expectedError:     false,
		},
		{
			name:     "Webhook blocks Profile configuration",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Profile: "cloud-profile",
			},
			profileStatusCode: http.StatusForbidden,
			expectedError:     true,
		},
		{
			// The merged configuration never goes through ValidateConfig, so an
			// ipvlan block contributed by a provider is only defaulted if
			// getDeviceNetworkConfig does it. Without that, addIPVlan rejects the
			// empty mode with "unsupported ipvlan mode".
			name:     "Cloud provider ipvlan block is defaulted",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Type:       apis.InterfaceTypeIPVLAN,
					Addressing: apis.AddressingModeUnnumbered,
					IPVlan:     &apis.IPVlanConfig{},
				},
			},
			expectedIPVlan: &apis.IPVlanConfig{Mode: apis.IPVlanModeL2, Flag: apis.IPVlanFlagBridge},
			expectedError:  false,
		},
		{
			// Neither configuration is invalid on its own: the conflict only
			// exists once they have been merged.
			name: "User rules combined with cloud provider VRF is rejected",
			userConf: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{{Source: "10.0.0.0/8", Table: 100}},
			},
			cloudConfResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{VRF: &apis.VRFConfig{Name: "vrf0"}},
			},
			expectedError: true,
			errContains:   "rules are not supported when VRF is enabled",
		},
		{
			name:     "Invalid cloud provider MTU is rejected",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{MTU: ptr.To[int32](1)},
			},
			expectedError: true,
			errContains:   "mtu: must be at least",
		},
		{
			// Validation has to run after the profile merge, not only after the
			// cloud provider merge.
			name:     "Invalid Profile configuration is rejected",
			userConf: &apis.NetworkConfig{},
			cloudConfResponse: &apis.NetworkConfig{
				Profile: "cloud-profile",
			},
			profileResponse: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Addresses: []string{"not-a-cidr"}},
			},
			profileStatusCode: http.StatusOK,
			expectedError:     true,
			errContains:       "invalid IP CIDR format",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == webhook.PathHealth {
					json.NewEncoder(w).Encode(webhook.Capabilities{CloudProvider: true, ProfileProvider: true})
					return
				}
				if r.URL.Path == webhook.PathGetDeviceAttributes {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == webhook.PathGetDeviceConfig {
					if tc.cloudConfResponse != nil {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(tc.cloudConfResponse)
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				if r.URL.Path == webhook.PathGetProfileConfig {
					if tc.profileStatusCode != 0 && tc.profileStatusCode != http.StatusOK {
						w.WriteHeader(tc.profileStatusCode)
						w.Write([]byte(`{"error": "forbidden"}`))
						return
					}
					if tc.profileResponse != nil {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(tc.profileResponse)
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			provider, err := webhook.NewWebhookProvider(ctx, srv.URL)
			if err != nil {
				t.Fatalf("Failed to create webhook provider: %v", err)
			}

			fakeDB := newFakeInventoryDB()
			fakeDB.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
				id := cloudprovider.DeviceIdentifiers{Name: deviceName}
				return provider.GetProfileConfig(id, claim, config)
			}
			fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
				id := cloudprovider.DeviceIdentifiers{Name: deviceName}
				conf := provider.GetDeviceConfig(id)
				return conf, conf != nil
			}

			np := &NetworkDriver{
				netdb:          fakeDB,
				driverName:     "test.driver",
				podConfigStore: mustNewPodConfigStore(),
			}

			claim := &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-1"}}
			mergedConf, err := np.getDeviceNetworkConfig("device-1", claim, tc.userConf)

			if tc.expectedError {
				if err == nil {
					t.Fatalf("Expected an error but got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("Expected error containing %q, got %v", tc.errContains, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if mergedConf == nil {
				t.Fatalf("Merged configuration is nil")
			}

			if tc.expectedMTU > 0 {
				if mergedConf.Interface.MTU == nil || *mergedConf.Interface.MTU != tc.expectedMTU {
					t.Errorf("Expected MTU %d, got %v", tc.expectedMTU, mergedConf.Interface.MTU)
				}
			} else if mergedConf.Interface.MTU != nil {
				t.Errorf("Expected nil MTU, got %d", *mergedConf.Interface.MTU)
			}

			if len(tc.expectedAddresses) > 0 {
				if len(mergedConf.Interface.Addresses) != len(tc.expectedAddresses) {
					t.Errorf("Expected addresses %v, got %v", tc.expectedAddresses, mergedConf.Interface.Addresses)
				} else {
					for i, addr := range tc.expectedAddresses {
						if mergedConf.Interface.Addresses[i] != addr {
							t.Errorf("Expected address %v, got %v", addr, mergedConf.Interface.Addresses[i])
						}
					}
				}
			} else if len(mergedConf.Interface.Addresses) > 0 {
				t.Errorf("Expected no addresses, got %v", mergedConf.Interface.Addresses)
			}

			if tc.expectedProfile != "" {
				if mergedConf.Profile != tc.expectedProfile {
					t.Errorf("Expected profile %s, got %s", tc.expectedProfile, mergedConf.Profile)
				}
			} else if mergedConf.Profile != "" {
				t.Errorf("Expected empty profile, got %s", mergedConf.Profile)
			}

			if tc.expectedIPVlan != nil {
				if diff := cmp.Diff(tc.expectedIPVlan, mergedConf.Interface.IPVlan); diff != "" {
					t.Errorf("Unexpected ipvlan config (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestMergeDevices(t *testing.T) {
	stringAttr := func(val string) resourcev1.DeviceAttribute {
		return resourcev1.DeviceAttribute{
			StringValue: &val,
		}
	}

	qtyCap := func(val string) resourcev1.DeviceCapacity {
		return resourcev1.DeviceCapacity{
			Value: k8sresource.MustParse(val),
		}
	}

	pciDev := resourcev1.Device{
		Name: "0000:c0:14.0",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(apis.AttrPCIAddress): stringAttr("0000:c0:14.0"),
		},
	}

	pciDevSnapshot := resourcev1.Device{
		Name: "0000:c0:14.0",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
			resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
			resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
		},
	}

	tests := []struct {
		name     string
		live     []resourcev1.Device
		snapshot []resourcev1.Device
		expected []resourcev1.Device
	}{
		{
			name:     "Only live devices returned",
			live:     []resourcev1.Device{pciDev},
			snapshot: nil,
			expected: []resourcev1.Device{pciDev},
		},
		{
			name:     "Snapshot device returned when not live",
			live:     nil,
			snapshot: []resourcev1.Device{pciDevSnapshot},
			expected: []resourcev1.Device{pciDevSnapshot},
		},
		{
			name: "Live device attribute takes precedence over snapshot while netdev is in the pod",
			live: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress): stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrNUMANode):   stringAttr("1"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
				},
			}},
			snapshot: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-snap"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("1G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
			expected: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("1"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-snap"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
		},
		{
			name: "Snapshot not backfilled when live device has a netdev",
			live: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("9000"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
				},
			}},
			snapshot: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/24"),
					resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::5/64"),
					resourcev1.QualifiedName(apis.AttrTCFilterNames): stringAttr("old-filter"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("1G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
			expected: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("9000"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
				},
			}},
		},
		{
			name: "Snapshot merged when live ifName is empty",
			live: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr(""),
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("1"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
				},
			}},
			snapshot: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-snap"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("1G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
			expected: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr(""),
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("1"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
		},
		{
			name: "Snapshot merged when live ifName has a nil StringValue",
			live: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): {},
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("1"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
				},
			}},
			snapshot: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-snap"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("1G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
			expected: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): {},
					resourcev1.QualifiedName(apis.AttrNUMANode):      stringAttr("1"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("1500"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
					"other-capacity":    qtyCap("50"),
				},
			}},
		},
		{
			name: "Snapshot addresses published by a live device are dropped per address",
			live: []resourcev1.Device{
				pciDev,
				{
					Name: "0000:c0:15.0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth2"),
						resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/16,10.0.9.9/24"),
						resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::5/64"),
					},
				},
			},
			snapshot: []resourcev1.Device{
				{
					Name: "0000:c0:14.0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
						resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/24,192.168.1.9/24"),
						resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::5/64"),
					},
				},
				{
					Name: "macvlan-pod",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("macvlan0"),
						resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.9.9/24"),
						resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::7/64,fd00::5/64"),
					},
				},
			},
			expected: []resourcev1.Device{
				{
					Name: "0000:c0:14.0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
						resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("192.168.1.9/24"),
					},
					Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{},
				},
				{
					Name: "0000:c0:15.0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth2"),
						resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/16,10.0.9.9/24"),
						resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::5/64"),
					},
				},
				{
					Name: "macvlan-pod",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("macvlan0"),
						resourcev1.QualifiedName(apis.AttrIPv6):          stringAttr("fd00::7/64"),
					},
				},
			},
		},
		{
			name: "Snapshot addresses kept while netdev is in the pod",
			live: []resourcev1.Device{pciDev},
			snapshot: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
					resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/24"),
				},
			}},
			expected: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth1"),
					resourcev1.QualifiedName(apis.AttrIPv4):          stringAttr("10.0.0.5/24"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mergeDevices(tc.live, tc.snapshot)
			if diff := cmp.Diff(tc.expected, result); diff != "" {
				t.Errorf("mergeDevices result mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMergeDevicesDoesNotModifySnapshot(t *testing.T) {
	addr := "10.0.0.5/24,192.168.1.9/24"
	live := []resourcev1.Device{{
		Name: "live-dev",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(apis.AttrInterfaceName): {StringValue: ptr.To("eth2")},
			resourcev1.QualifiedName(apis.AttrIPv4):          {StringValue: ptr.To("10.0.0.5/24")},
		},
	}}
	snapshot := []resourcev1.Device{{
		Name: "pod-dev",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(apis.AttrIPv4): {StringValue: ptr.To(addr)},
		},
	}}
	want := snapshot[0].DeepCopy()

	mergeDevices(live, snapshot)

	if diff := cmp.Diff(*want, snapshot[0]); diff != "" {
		t.Errorf("mergeDevices modified the snapshot (-want +got):\n%s", diff)
	}
}

func TestUnprepareResourceClaimRequestsRescan(t *testing.T) {
	netdb := newFakeInventoryDB()
	np := &NetworkDriver{
		netdb:          netdb,
		podConfigStore: mustNewPodConfigStore(),
	}
	claimName := types.NamespacedName{Name: "test-claim", Namespace: "test-ns"}
	np.podConfigStore.SetDeviceConfig("pod-uid-1", "device-a", DeviceConfig{Claim: claimName})

	claims := []kubeletplugin.NamespacedObject{
		{NamespacedName: claimName, UID: "claim-uid-1"},
	}
	if _, err := np.UnprepareResourceClaims(context.Background(), claims); err != nil {
		t.Fatalf("UnprepareResourceClaims failed: %v", err)
	}

	if got := netdb.rescanCalls.Load(); got != 1 {
		t.Errorf("RequestRescan call count = %d, want 1", got)
	}
}

// TODO: To further improve test coverage, consider constructing and mounting fake
// sysfs paths for RDMA character devices (e.g., /dev/infiniband/uverbs0). This
// would allow testing of device discovery and character device aggregation logic
// that currently depends on the host's physical hardware.
func TestPrepareResourceClaim(t *testing.T) {
	userns.Run(t, testPrepareResourceClaim_Namespaced, syscall.CLONE_NEWNET)
}

func testPrepareResourceClaim_Namespaced(t *testing.T) {
	ctx := t.Context()
	const testDriverName = "test.driver"

	// We are in a fresh, isolated netns for all these test cases.
	// Create a shared dummy interface that tests can rely on.
	la := netlink.NewLinkAttrs()
	la.Name = "dummy0"
	dummy := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(dummy); err != nil && !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("Failed to create shared dummy interface: %v", err)
	}

	testCases := []struct {
		name          string
		claim         *resourcev1.ResourceClaim
		setupDB       func(*fakeInventoryDB)
		wantErr       string
		wantErrAlso   string
		wantPodConfig *PodConfig
		check         func(t *testing.T, db *fakeInventoryDB)
	}{
		{
			name: "single IB-only device builds RDMA config successfully",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-ib-single", Namespace: "default", Name: "claim-ib-single"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-ib-single"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "ib-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return true }
				db.GetRDMADeviceNameFunc = func(deviceName string) (string, error) {
					return "fake_mlx5_0", nil
				}
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
			},
			wantPodConfig: &PodConfig{
				DeviceConfigs: map[string]DeviceConfig{
					"ib-dev-0": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-ib-single",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "ib-dev-0"},
						RDMADevice: RDMAConfig{
							LinkDev: "fake_mlx5_0",
						},
					},
				},
			},
		},
		{
			name: "multiple IB-only devices in single claim build independent RDMA configs without accumulation",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-ib-multi", Namespace: "default", Name: "claim-ib-multi"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-ib-multi"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								// Two requests for two separate IB devices within the same claim
								{Driver: testDriverName, Device: "ib-dev-0", Request: "req-0"},
								{Driver: testDriverName, Device: "ib-dev-1", Request: "req-1"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return true }
				db.GetRDMADeviceNameFunc = func(deviceName string) (string, error) {
					switch deviceName {
					case "ib-dev-0":
						return "fake_mlx5_0", nil
					case "ib-dev-1":
						return "fake_mlx5_1", nil
					default:
						return "", fmt.Errorf("unexpected device %s", deviceName)
					}
				}
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
			},
			wantPodConfig: &PodConfig{
				DeviceConfigs: map[string]DeviceConfig{
					"ib-dev-0": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-ib-multi",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "ib-dev-0"},
						RDMADevice: RDMAConfig{
							LinkDev: "fake_mlx5_0",
						},
					},
					"ib-dev-1": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-ib-multi",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "ib-dev-1"},
						RDMADevice: RDMAConfig{
							LinkDev: "fake_mlx5_1",
						},
					},
				},
			},
		},
		{
			name: "single network device builds config successfully",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-net-single", Namespace: "default", Name: "claim-net-single"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-net-single"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				// Return the shared 'dummy0' created at the start of the test
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
					return "dummy0", nil
				}
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
			},
			wantPodConfig: &PodConfig{
				DeviceConfigs: map[string]DeviceConfig{
					"net-dev-0": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-net-single",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "net-dev-0"},
						NetworkInterfaceConfigInHost: apis.NetworkConfig{
							Interface: apis.InterfaceConfig{
								Name: "dummy0",
							},
						},
						NetworkInterfaceConfigInPod: apis.NetworkConfig{
							Interface: apis.InterfaceConfig{
								Name: "dummy0",
							},
						},
					},
				},
			},
		},
		{
			name: "no pods allocated to claim",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-empty", Namespace: "default", Name: "claim-empty"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{},
				},
			},
		},
		{
			name: "multiple pods allocated to claim returns error",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-multi-pod", Namespace: "default", Name: "claim-multi-pod"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "pod-1", UID: "pod-uid-1"},
						{APIGroup: "", Resource: "pods", Name: "pod-2", UID: "pod-uid-2"},
					},
				},
			},
			wantErr: "driver only supports one pod per claim, got 2",
		},
		{
			name: "unsupported consumer reference returns error",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-unsupported-ref", Namespace: "default", Name: "claim-unsupported-ref"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "apps", Resource: "deployments", Name: "dep-1", UID: "dep-uid-1"},
					},
				},
			},
			wantErr: "driver only supports Pods",
		},
		{
			name: "devices managed by other drivers are ignored",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-other-driver", Namespace: "default", Name: "claim-other-driver"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-1"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								// This result specifies a different driver, so it should be safely ignored
								{Driver: "other.driver.io", Device: "gpu-0", Request: "gpu-req"},
							},
						},
					},
				},
			},
		},
		{
			name: "device interface lookup failure returns error",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-net-fail", Namespace: "default", Name: "claim-net-fail"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-net-fail"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				// Simulate failure when retrieving the interface name
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
					return "", fmt.Errorf("interface not found in inventory")
				}
			},
			wantErr: "failed to get network interface name for device net-dev-0",
		},
		{
			name: "subinterface resolving with no addresses returns error",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-noaddr", Namespace: "default", Name: "claim-subif-noaddr"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-noaddr"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				// Cloud advertises an ipvlan subinterface but resolves no addresses.
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
			},
			wantErr: "resolved with no addresses",
		},
		{
			name: "unnumbered subinterface is allowed without addresses",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-unnum", Namespace: "default", Name: "claim-subif-unnum"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-unnum"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				// Cloud advertises an unnumbered ipvlan subinterface.
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN", Addressing: apis.AddressingModeUnnumbered}}, true
				}
			},
			wantPodConfig: &PodConfig{
				DeviceConfigs: map[string]DeviceConfig{
					"net-dev-0": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-subif-unnum",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "net-dev-0"},
						NetworkInterfaceConfigInHost: apis.NetworkConfig{
							Interface: apis.InterfaceConfig{
								Name: "dummy0",
							},
						},
						NetworkInterfaceConfigInPod: apis.NetworkConfig{
							Interface: apis.InterfaceConfig{
								Name:       "dummy0",
								Type:       "IPVLAN",
								Addressing: apis.AddressingModeUnnumbered,
								// The merged config is defaulted, so the ipvlan
								// block the provider left unset is populated.
								IPVlan: &apis.IPVlanConfig{Mode: apis.IPVlanModeL2, Flag: apis.IPVlanFlagBridge},
							},
						},
					},
				},
			},
		},
		{
			name: "provider-selected subinterface rejects a user hardwareAddr",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-hwaddr", Namespace: "default", Name: "claim-subif-hwaddr"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-hwaddr"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{{
								Source: resourcev1.AllocationConfigSourceClaim,
								DeviceConfiguration: resourcev1.DeviceConfiguration{
									Opaque: &resourcev1.OpaqueDeviceConfiguration{
										Driver:     testDriverName,
										Parameters: runtime.RawExtension{Raw: []byte(`{"interface":{"hardwareAddr":"00:11:22:33:44:55"}}`)},
									},
								},
							}},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				// The user config passed validation as passthrough; the provider selects IPVLAN.
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN", Addresses: []string{"10.24.3.5/32"}}}, true
				}
			},
			wantErr: "hardwareAddr and dhcp are not supported for subinterface types",
		},
		{
			name: "provider-selected subinterface rejects user dhcp before any dhcp request",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-dhcp", Namespace: "default", Name: "claim-subif-dhcp"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-dhcp"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{{
								Source: resourcev1.AllocationConfigSourceClaim,
								DeviceConfiguration: resourcev1.DeviceConfiguration{
									Opaque: &resourcev1.OpaqueDeviceConfiguration{
										Driver:     testDriverName,
										Parameters: runtime.RawExtension{Raw: []byte(`{"interface":{"addressing":"DHCP"}}`)},
									},
								},
							}},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
			},
			wantErr: "hardwareAddr and dhcp are not supported for subinterface types",
		},
		{
			name: "profile-selected subinterface rejects a user hardwareAddr and releases the profile",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-hwaddr-profile", Namespace: "default", Name: "claim-subif-hwaddr-profile"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-hwaddr-profile"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{{
								Source: resourcev1.AllocationConfigSourceClaim,
								DeviceConfiguration: resourcev1.DeviceConfiguration{
									Opaque: &resourcev1.OpaqueDeviceConfiguration{
										Driver:     testDriverName,
										Parameters: runtime.RawExtension{Raw: []byte(`{"interface":{"hardwareAddr":"00:11:22:33:44:55"}}`)},
									},
								},
							}},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				// Only the profile resolution introduces the subinterface type.
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed"}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN", Addresses: []string{"10.24.3.5/32"}}}, nil
				}
			},
			wantErr: "hardwareAddr and dhcp are not supported for subinterface types",
			// Nothing is stored, and the allocation is released right away.
			check: func(t *testing.T, db *fakeInventoryDB) {
				if got := db.releaseProfileCalls.Load(); got != 1 {
					t.Errorf("ReleaseProfileConfig calls = %d, want 1", got)
				}
			},
		},
		{
			name: "provider-selected subinterface with a profile rejects hardwareAddr and releases the allocated profile",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-hwaddr-noalloc", Namespace: "default", Name: "claim-subif-hwaddr-noalloc"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-hwaddr-noalloc"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{{
								Source: resourcev1.AllocationConfigSourceClaim,
								DeviceConfiguration: resourcev1.DeviceConfiguration{
									Opaque: &resourcev1.OpaqueDeviceConfiguration{
										Driver:     testDriverName,
										Parameters: runtime.RawExtension{Raw: []byte(`{"interface":{"hardwareAddr":"00:11:22:33:44:55"}}`)},
									},
								},
							}},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed", Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Addresses: []string{"10.24.3.5/32"}}}, nil
				}
			},
			wantErr: "hardwareAddr and dhcp are not supported for subinterface types",
			check: func(t *testing.T, db *fakeInventoryDB) {
				if got := db.profileCalls.Load(); got != 1 {
					t.Errorf("GetProfileConfig calls = %d, want 1", got)
				}
				if got := db.releaseProfileCalls.Load(); got != 1 {
					t.Errorf("ReleaseProfileConfig calls = %d, want 1", got)
				}
			},
		},
		{
			// The claim is valid, so only the merged validation can catch the
			// profile output, after the profile was allocated.
			name: "profile output with an invalid acceptRA is rejected and the allocated profile is released",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-acceptra-range", Namespace: "default", Name: "claim-subif-acceptra-range"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-acceptra-range"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed", Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Addresses: []string{"10.24.3.5/32"}, AcceptRA: ptr.To[int32](3)}}, nil
				}
			},
			wantErr: "acceptRA: must be between 0 and 2, got 3",
			check: func(t *testing.T, db *fakeInventoryDB) {
				if got := db.profileCalls.Load(); got != 1 {
					t.Errorf("GetProfileConfig calls = %d, want 1", got)
				}
				if got := db.releaseProfileCalls.Load(); got != 1 {
					t.Errorf("ReleaseProfileConfig calls = %d, want 1", got)
				}
			},
		},
		{
			// The claim is valid, so only the merged validation can catch the
			// profile output, after the profile was allocated.
			name: "profile output with acceptRA below the IPv6 MTU is rejected and the allocated profile is released",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-acceptra-mtu", Namespace: "default", Name: "claim-subif-acceptra-mtu"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-acceptra-mtu"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed", Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Addresses: []string{"10.24.3.5/32"}, AcceptRA: ptr.To[int32](2), MTU: ptr.To[int32](1279)}}, nil
				}
			},
			wantErr: "acceptRA: requires an mtu of at least 1280, got 1279",
			check: func(t *testing.T, db *fakeInventoryDB) {
				if got := db.profileCalls.Load(); got != 1 {
					t.Errorf("GetProfileConfig calls = %d, want 1", got)
				}
				if got := db.releaseProfileCalls.Load(); got != 1 {
					t.Errorf("ReleaseProfileConfig calls = %d, want 1", got)
				}
			},
		},
		{
			name: "profile-selected subinterface reports a failed profile release",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-hwaddr-relfail", Namespace: "default", Name: "claim-subif-hwaddr-relfail"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-hwaddr-relfail"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
							Config: []resourcev1.DeviceAllocationConfiguration{{
								Source: resourcev1.AllocationConfigSourceClaim,
								DeviceConfiguration: resourcev1.DeviceConfiguration{
									Opaque: &resourcev1.OpaqueDeviceConfiguration{
										Driver:     testDriverName,
										Parameters: runtime.RawExtension{Raw: []byte(`{"interface":{"hardwareAddr":"00:11:22:33:44:55"}}`)},
									},
								},
							}},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed"}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: "IPVLAN", Addresses: []string{"10.24.3.5/32"}}}, nil
				}
				db.ReleaseProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
					return fmt.Errorf("release boom")
				}
			},
			// The validation error must survive next to the release error.
			wantErr:     "hardwareAddr and dhcp are not supported for subinterface types",
			wantErrAlso: "failed to release profile config: release boom",
			check: func(t *testing.T, db *fakeInventoryDB) {
				if got := db.releaseProfileCalls.Load(); got != 1 {
					t.Errorf("ReleaseProfileConfig calls = %d, want 1", got)
				}
			},
		},
		{
			name: "subinterface with provider-advertised profile resolves addresses and PBR",
			claim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-subif-implicit", Namespace: "default", Name: "claim-subif-implicit"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{
						{APIGroup: "", Resource: "pods", Name: "test-pod", UID: "pod-uid-subif-implicit"},
					},
					Allocation: &resourcev1.AllocationResult{
						Devices: resourcev1.DeviceAllocationResult{
							Results: []resourcev1.DeviceRequestAllocationResult{
								{Driver: testDriverName, Device: "net-dev-0", Request: "req-0"},
							},
						},
					},
				},
			},
			setupDB: func(db *fakeInventoryDB) {
				db.IsIBOnlyDeviceFunc = func(deviceName string) bool { return false }
				db.GetNetInterfaceNameFunc = func(deviceName string) (string, error) { return "dummy0", nil }
				db.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
					return resourcev1.Device{Name: deviceName}, true
				}
				// The cloud advertises an ipvlan subinterface and its own managed
				// profile, so resolution runs without the user setting profile.
				db.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
					return &apis.NetworkConfig{Profile: "cloud-managed", Interface: apis.InterfaceConfig{Type: "IPVLAN"}}, true
				}
				db.GetProfileConfigFunc = func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
					if !config.Interface.IsSubinterface() {
						return nil, nil
					}
					return &apis.NetworkConfig{
						Interface: apis.InterfaceConfig{Addresses: []string{"10.24.3.5/32"}},
						Routes: []apis.RouteConfig{
							{Destination: "10.24.3.1/32", Scope: 253, Table: 1100},
							{Destination: "0.0.0.0/0", Gateway: "10.24.3.1", Table: 1100},
						},
						Rules: []apis.RuleConfig{
							{Source: "10.24.3.5/32", Table: 1100, Priority: apis.SourceRoutingRulePriority},
						},
					}, nil
				}
			},
			wantPodConfig: &PodConfig{
				DeviceConfigs: map[string]DeviceConfig{
					"net-dev-0": {
						Claim: types.NamespacedName{
							Namespace: "default",
							Name:      "claim-subif-implicit",
						},
						DeviceSnapshot: &resourcev1.Device{Name: "net-dev-0"},
						NetworkInterfaceConfigInHost: apis.NetworkConfig{
							Interface: apis.InterfaceConfig{
								Name: "dummy0",
							},
						},
						NetworkInterfaceConfigInPod: apis.NetworkConfig{
							Profile: "cloud-managed",
							Interface: apis.InterfaceConfig{
								Name:      "dummy0",
								Type:      "IPVLAN",
								Addresses: []string{"10.24.3.5/32"},
								// The merged config is defaulted, so the ipvlan
								// block the provider left unset is populated.
								IPVlan: &apis.IPVlanConfig{Mode: apis.IPVlanModeL2, Flag: apis.IPVlanFlagBridge},
							},
							Routes: []apis.RouteConfig{
								{Destination: "10.24.3.1/32", Scope: 253, Table: 1100},
								{Destination: "0.0.0.0/0", Gateway: "10.24.3.1", Table: 1100},
							},
							Rules: []apis.RuleConfig{
								{Source: "10.24.3.5/32", Table: 1100, Priority: apis.SourceRoutingRulePriority},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDB := newFakeInventoryDB()
			if tc.setupDB != nil {
				tc.setupDB(fakeDB)
			}

			np := &NetworkDriver{
				netdb:          fakeDB,
				driverName:     testDriverName,
				podConfigStore: mustNewPodConfigStore(),
				eventRecorder:  record.NewFakeRecorder(100),
			}

			gotResult := np.prepareResourceClaim(ctx, tc.claim)

			if tc.wantErr != "" {
				if gotResult.Err == nil || !strings.Contains(gotResult.Err.Error(), tc.wantErr) {
					t.Fatalf("prepareResourceClaim() error = %v, want error containing %q", gotResult.Err, tc.wantErr)
				}
			} else if gotResult.Err != nil {
				t.Fatalf("prepareResourceClaim() unexpected error = %v", gotResult.Err)
			}
			if tc.wantErrAlso != "" {
				if gotResult.Err == nil || !strings.Contains(gotResult.Err.Error(), tc.wantErrAlso) {
					t.Fatalf("prepareResourceClaim() error = %v, want error also containing %q", gotResult.Err, tc.wantErrAlso)
				}
			}

			var gotPodConfig *PodConfig
			if len(tc.claim.Status.ReservedFor) > 0 {
				podUID := tc.claim.Status.ReservedFor[0].UID
				if podCfg, ok := np.podConfigStore.GetPodConfig(podUID); ok {
					gotPodConfig = &podCfg
				}
			}

			opts := []cmp.Option{cmpopts.EquateEmpty(), cmpopts.IgnoreFields(PodConfig{}, "LastNRIActivity")}
			if diff := cmp.Diff(tc.wantPodConfig, gotPodConfig, opts...); diff != "" {
				t.Errorf("PodConfig mismatch (-want +got):\n%s", diff)
			}
			if tc.check != nil {
				tc.check(t, fakeDB)
			}
		})
	}
}

func TestClearStaleRouteSources(t *testing.T) {
	testCases := []struct {
		name        string
		routes      []apis.RouteConfig
		addresses   []string
		wantSources []string
	}{
		{
			name:        "source not assigned in the pod is cleared",
			routes:      []apis.RouteConfig{{Destination: "172.17.14.0/24", Source: "172.17.14.28"}},
			addresses:   []string{"172.17.14.101/24"},
			wantSources: []string{""},
		},
		{
			name:        "source assigned in the pod is kept",
			routes:      []apis.RouteConfig{{Destination: "172.17.14.0/24", Source: "172.17.14.28"}},
			addresses:   []string{"172.17.14.28/24"},
			wantSources: []string{"172.17.14.28"},
		},
		{
			name:        "route without source is untouched",
			routes:      []apis.RouteConfig{{Destination: "0.0.0.0/0", Gateway: "172.17.14.1"}},
			addresses:   nil,
			wantSources: []string{""},
		},
		{
			name:        "source is cleared when the pod has no addresses",
			routes:      []apis.RouteConfig{{Destination: "10.0.0.0/8", Gateway: "172.17.14.1", Source: "172.17.14.28"}},
			addresses:   nil,
			wantSources: []string{""},
		},
		{
			name:        "IPv6 source matches a non-canonical pod address",
			routes:      []apis.RouteConfig{{Destination: "fd00:10::/64", Source: "fd00:10::1c"}},
			addresses:   []string{"fd00:0010:0000:0000::001c/64"},
			wantSources: []string{"fd00:10::1c"},
		},
		{
			name:        "IPv6 source not assigned in the pod is cleared",
			routes:      []apis.RouteConfig{{Destination: "fd00:10::/64", Source: "fd00:10::1c"}},
			addresses:   []string{"fd00:10::65/64"},
			wantSources: []string{""},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clearStaleRouteSources(tc.routes, tc.addresses)
			for i, want := range tc.wantSources {
				if tc.routes[i].Source != want {
					t.Errorf("route %d source = %q, want %q", i, tc.routes[i].Source, want)
				}
			}
		})
	}
}

// TestPrepareResourceClaimsVRFTableConflicts checks that devices of one Pod
// cannot resolve the same VRF name to different routing tables, whether they
// come from one claim or from several. The VRF comes from a profile merged into
// the device config, so the test does not need a kernel: the devices are
// IB-only, which skips every netlink step of prepareDevice.
func TestPrepareResourceClaimsVRFTableConflicts(t *testing.T) {
	derived := apis.TableIDForName("blue")

	type claimSpec struct {
		uid     types.UID
		podUID  types.UID
		devices []string
		wantErr string // substring of the claim's error; empty means success
	}
	tests := []struct {
		name string
		// vrfs maps a device to the VRF its profile resolves to.
		vrfs   map[string]*apis.VRFConfig
		claims []claimSpec
		// wantStored lists the "pod/device" entries expected in the store after all claims.
		wantStored []string
		// wantReleased is the number of profile releases expected.
		wantReleased int32
	}{
		{
			name: "same claim, same vrf, different tables",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue", Table: ptr.To(100)},
				"dev-2": {Name: "blue", Table: ptr.To(200)},
			},
			claims:       []claimSpec{{uid: "c1", podUID: "pod-1", devices: []string{"dev-1", "dev-2"}, wantErr: "conflicts"}},
			wantStored:   []string{"pod-1/dev-1"},
			wantReleased: 1,
		},
		{
			name: "separate claims of the same pod, same vrf, different tables",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue", Table: ptr.To(100)},
				"dev-2": {Name: "blue", Table: ptr.To(200)},
			},
			claims: []claimSpec{
				{uid: "c1", podUID: "pod-1", devices: []string{"dev-1"}},
				{uid: "c2", podUID: "pod-1", devices: []string{"dev-2"}, wantErr: "conflicts"},
			},
			wantStored:   []string{"pod-1/dev-1"},
			wantReleased: 1,
		},
		{
			name: "explicit table differs from the one derived from the name",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue"},
				"dev-2": {Name: "blue", Table: ptr.To(derived + 1)},
			},
			claims: []claimSpec{
				{uid: "c1", podUID: "pod-1", devices: []string{"dev-1"}},
				{uid: "c2", podUID: "pod-1", devices: []string{"dev-2"}, wantErr: "conflicts"},
			},
			wantStored:   []string{"pod-1/dev-1"},
			wantReleased: 1,
		},
		{
			name: "different pods may use different tables for the same vrf name",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue", Table: ptr.To(100)},
				"dev-2": {Name: "blue", Table: ptr.To(200)},
			},
			claims: []claimSpec{
				{uid: "c1", podUID: "pod-1", devices: []string{"dev-1"}},
				{uid: "c2", podUID: "pod-2", devices: []string{"dev-2"}},
			},
			wantStored: []string{"pod-1/dev-1", "pod-2/dev-2"},
		},
		{
			name: "shared vrf with the same explicit table",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue", Table: ptr.To(100)},
				"dev-2": {Name: "blue", Table: ptr.To(100)},
			},
			claims:     []claimSpec{{uid: "c1", podUID: "pod-1", devices: []string{"dev-1", "dev-2"}}},
			wantStored: []string{"pod-1/dev-1", "pod-1/dev-2"},
		},
		{
			name: "shared vrf with tables derived from the name",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue"},
				"dev-2": {Name: "blue"},
			},
			claims: []claimSpec{
				{uid: "c1", podUID: "pod-1", devices: []string{"dev-1"}},
				{uid: "c2", podUID: "pod-1", devices: []string{"dev-2"}},
			},
			wantStored: []string{"pod-1/dev-1", "pod-1/dev-2"},
		},
		{
			name: "different vrf names with different tables",
			vrfs: map[string]*apis.VRFConfig{
				"dev-1": {Name: "blue", Table: ptr.To(100)},
				"dev-2": {Name: "red", Table: ptr.To(200)},
			},
			claims:     []claimSpec{{uid: "c1", podUID: "pod-1", devices: []string{"dev-1", "dev-2"}}},
			wantStored: []string{"pod-1/dev-1", "pod-1/dev-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDB := newFakeInventoryDB()
			fakeDB.GetDeviceConfigFunc = func(string) (*apis.NetworkConfig, bool) {
				return &apis.NetworkConfig{Profile: "vrf-profile"}, true
			}
			fakeDB.GetProfileConfigFunc = func(device string, _ *resourcev1.ResourceClaim, _ *apis.NetworkConfig) (*apis.NetworkConfig, error) {
				vrf := *tt.vrfs[device] // the driver must not rely on the profile's config staying untouched
				return &apis.NetworkConfig{Interface: apis.InterfaceConfig{VRF: &vrf}}, nil
			}
			fakeDB.IsIBOnlyDeviceFunc = func(string) bool { return true }

			np := &NetworkDriver{
				netdb:          fakeDB,
				driverName:     "test.driver",
				podConfigStore: mustNewPodConfigStore(),
				eventRecorder:  record.NewFakeRecorder(100),
			}

			for _, spec := range tt.claims {
				var results []resourcev1.DeviceRequestAllocationResult
				for _, dev := range spec.devices {
					results = append(results, resourcev1.DeviceRequestAllocationResult{Driver: "test.driver", Device: dev, Request: "req"})
				}
				claim := &resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{UID: spec.uid, Namespace: "default", Name: string(spec.uid)},
					Status: resourcev1.ResourceClaimStatus{
						ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: string(spec.podUID), UID: spec.podUID}},
						Allocation:  &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: results}},
					},
				}
				res, err := np.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
				if err != nil {
					t.Fatalf("PrepareResourceClaims(%s) = %v", spec.uid, err)
				}
				gotErr := res[spec.uid].Err
				switch {
				case spec.wantErr == "" && gotErr != nil:
					t.Fatalf("claim %s: unexpected error: %v", spec.uid, gotErr)
				case spec.wantErr != "" && gotErr == nil:
					t.Fatalf("claim %s succeeded, want an error mentioning %q", spec.uid, spec.wantErr)
				case spec.wantErr != "" && !strings.Contains(gotErr.Error(), spec.wantErr):
					t.Fatalf("claim %s error = %q, want it to mention %q", spec.uid, gotErr, spec.wantErr)
				case spec.wantErr != "" && !strings.Contains(gotErr.Error(), `vrf "blue"`):
					t.Errorf("claim %s error = %q, want it to name the vrf", spec.uid, gotErr)
				}
			}

			var gotStored []string
			for _, podUID := range np.podConfigStore.ListPods() {
				podCfg, _ := np.podConfigStore.GetPodConfig(podUID)
				for dev := range podCfg.DeviceConfigs {
					gotStored = append(gotStored, fmt.Sprintf("%s/%s", podUID, dev))
				}
			}
			slices.Sort(gotStored)
			if diff := cmp.Diff(tt.wantStored, gotStored, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored devices mismatch (-want +got):\n%s", diff)
			}
			if got := fakeDB.releaseProfileCalls.Load(); got != tt.wantReleased {
				t.Errorf("profile releases = %d, want %d", got, tt.wantReleased)
			}
		})
	}
}

// A conflicting device must keep being rejected when kubelet retries the claim,
// and an already prepared device must still be reused without a second profile
// allocation.
func TestPrepareResourceClaimsVRFConflictRetry(t *testing.T) {
	fakeDB := newFakeInventoryDB()
	fakeDB.GetDeviceConfigFunc = func(string) (*apis.NetworkConfig, bool) {
		return &apis.NetworkConfig{Profile: "vrf-profile"}, true
	}
	tables := map[string]int{"dev-1": 100, "dev-2": 200}
	fakeDB.GetProfileConfigFunc = func(device string, _ *resourcev1.ResourceClaim, _ *apis.NetworkConfig) (*apis.NetworkConfig, error) {
		return &apis.NetworkConfig{Interface: apis.InterfaceConfig{VRF: &apis.VRFConfig{Name: "blue", Table: ptr.To(tables[device])}}}, nil
	}
	fakeDB.IsIBOnlyDeviceFunc = func(string) bool { return true }
	np := &NetworkDriver{
		netdb:          fakeDB,
		driverName:     "test.driver",
		podConfigStore: mustNewPodConfigStore(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "c1", Namespace: "default", Name: "c1"},
		Status: resourcev1.ResourceClaimStatus{
			ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: "pod-1", UID: "pod-1"}},
			Allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{
				{Driver: "test.driver", Device: "dev-1", Request: "req"},
				{Driver: "test.driver", Device: "dev-2", Request: "req"},
			}}},
		},
	}
	for attempt := 1; attempt <= 3; attempt++ {
		res, err := np.PrepareResourceClaims(context.Background(), []*resourcev1.ResourceClaim{claim})
		if err != nil {
			t.Fatalf("attempt %d: PrepareResourceClaims() = %v", attempt, err)
		}
		if res["c1"].Err == nil {
			t.Fatalf("attempt %d: claim with conflicting vrf tables succeeded", attempt)
		}
		podCfg, _ := np.podConfigStore.GetPodConfig("pod-1")
		if len(podCfg.DeviceConfigs) != 1 {
			t.Fatalf("attempt %d: %d devices stored, want only dev-1", attempt, len(podCfg.DeviceConfigs))
		}
		if _, ok := podCfg.DeviceConfigs["dev-1"]; !ok {
			t.Fatalf("attempt %d: dev-1 is not stored", attempt)
		}
		// dev-1 is allocated once and then reused; dev-2 allocates and releases on every attempt.
		if got, want := fakeDB.profileCalls.Load(), int32(1+attempt); got != want {
			t.Errorf("attempt %d: profile allocations = %d, want %d", attempt, got, want)
		}
		if got, want := fakeDB.releaseProfileCalls.Load(), int32(attempt); got != want {
			t.Errorf("attempt %d: profile releases = %d, want %d", attempt, got, want)
		}
	}
}
