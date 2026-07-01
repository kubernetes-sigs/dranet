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
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/prometheus/client_golang/prometheus/testutil"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
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
			netdb:         newFakeInventoryDB(),
			driverName:    "test.driver",
			eventRecorder: record.NewFakeRecorder(100),
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
		fakeDB.GetProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
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
		fakeDB.GetProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
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
		fakeDB.GetProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
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

	t.Run("Early Store Profile Release on Subsequent Failure", func(t *testing.T) {
		fakeDB := newFakeInventoryDB()
		fakeDB.GetProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
			return &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{"10.0.0.1/24"},
				},
			}, nil
		}
		fakeDB.GetDeviceConfigFunc = func(deviceName string) (*apis.NetworkConfig, bool) {
			return &apis.NetworkConfig{Profile: "my-profile"}, true
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

		// Verify the early device config was stored so Kubelet's call to UnprepareResourceClaims will clean it up
		podCfg, ok := np.podConfigStore.GetPodConfig("pod-uid-leak")
		if !ok {
			t.Fatalf("Expected pod config to be stored early")
		}
		devCfg := podCfg.DeviceConfigs["device-1"]
		if devCfg.NetworkInterfaceConfigInPod.Profile != "my-profile" {
			t.Errorf("Expected profile 'my-profile' to be saved for cleanup, got '%v'", devCfg.NetworkInterfaceConfigInPod.Profile)
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
		expectedAddresses []string
		expectedMTU       int32
		expectedProfile   string
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
			fakeDB.GetProfileConfigFunc = func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
				id := cloudprovider.DeviceIdentifiers{Name: deviceName}
				return provider.GetProfileConfig(id, claimUID, config)
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

			mergedConf, err := np.getDeviceNetworkConfig("device-1", "claim-uid-1", tc.userConf)

			if tc.expectedError {
				if err == nil {
					t.Fatalf("Expected an error but got nil")
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
		})
	}
}

func TestGetIPFromRange(t *testing.T) {
	type testCase struct {
		name          string
		ipRange       string
		initIPAM      bool
		checkContains bool
		wantErr       string
	}

	tests := []testCase{
		{
			name:     "IPAM database not initialized",
			ipRange:  "192.168.1.0/24",
			initIPAM: false,
			wantErr:  "IPAM database not initialized",
		},
		{
			name:     "invalid IP range",
			ipRange:  "invalid-cidr",
			initIPAM: true,
			wantErr:  "invalid IP range",
		},
		{
			name:          "successful allocation",
			ipRange:       "2001:db8::/64",
			initIPAM:      true,
			checkContains: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := mustNewPodConfigStore()
			var localIPAM *LocalIPAM
			if tc.initIPAM {
				localIPAM = newLocalIPAM(store)
			}
			np := &NetworkDriver{
				localIPAM:      localIPAM,
				podConfigStore: store,
			}

			ipWithMask, err := np.getIPFromRange(tc.ipRange, "pod-test", "eth0")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("getIPFromRange() unexpected error: %v", err)
			}

			if tc.checkContains {
				cidr, err := netip.ParsePrefix(tc.ipRange)
				if err != nil {
					t.Fatalf("Failed to parse tc.ipRange %q: %v", tc.ipRange, err)
				}
				prefix, err := netip.ParsePrefix(ipWithMask)
				if err != nil {
					t.Fatalf("Failed to parse returned IP prefix %q: %v", ipWithMask, err)
				}
				if !cidr.Contains(prefix.Addr()) {
					t.Errorf("Expected returned IP %s to be within CIDR %s", prefix.Addr(), cidr)
				}
			}
		})
	}
}

func TestAddSourceBasedRoutingRule(t *testing.T) {
	tests := []struct {
		name      string
		deviceCfg DeviceConfig
		wantRules []apis.RuleConfig
	}{
		{
			name: "No default route, rules should not change",
			deviceCfg: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Addresses: []string{"2001:db8::3/128"},
					},
					Routes: []apis.RouteConfig{
						{Destination: "2001:db8::/64", Table: 100},
					},
				},
			},
			wantRules: nil,
		},
		{
			name: "Default route with Table=0, rules should not change",
			deviceCfg: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Addresses: []string{"2001:db8::3/128"},
					},
					Routes: []apis.RouteConfig{
						{Destination: "::/0", Table: 0},
					},
				},
			},
			wantRules: nil,
		},
		{
			name: "Default route IPv6 with custom Table, rule should be added",
			deviceCfg: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Addresses: []string{"2001:db8::3/128"},
					},
					Routes: []apis.RouteConfig{
						{Destination: "::/0", Table: 100},
					},
				},
			},
			wantRules: []apis.RuleConfig{
				{Source: "2001:db8::3/128", Table: 100, Priority: 32000},
			},
		},
		{
			name: "Default route IPv4 with custom Table, rule should be added",
			deviceCfg: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Addresses: []string{"192.168.1.3/32"},
					},
					Routes: []apis.RouteConfig{
						{Destination: "0.0.0.0/0", Table: 101},
					},
				},
			},
			wantRules: []apis.RuleConfig{
				{Source: "192.168.1.3/32", Table: 101, Priority: 32000},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addSourceBasedRoutingRule(&tt.deviceCfg)
			gotRules := tt.deviceCfg.NetworkInterfaceConfigInPod.Rules
			if len(gotRules) != len(tt.wantRules) {
				t.Fatalf("Expected %d rules, got %d", len(tt.wantRules), len(gotRules))
			}
			for i := range gotRules {
				if gotRules[i].Source != tt.wantRules[i].Source || gotRules[i].Table != tt.wantRules[i].Table || gotRules[i].Priority != tt.wantRules[i].Priority {
					t.Errorf("gotRules[%d] = %+v, want %+v", i, gotRules[i], tt.wantRules[i])
				}
			}
		})
	}
}
