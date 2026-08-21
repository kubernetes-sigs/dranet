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

	t.Run("Early Store Profile Release on Subsequent Failure", func(t *testing.T) {
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
			name: "Live device attribute takes precedence over snapshot",
			live: []resourcev1.Device{{
				Name: "0000:c0:14.0",
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(apis.AttrPCIAddress):    stringAttr("0000:c0:14.0"),
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-live"),
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
					resourcev1.QualifiedName(apis.AttrInterfaceName): stringAttr("eth-live"),
					resourcev1.QualifiedName(apis.AttrMTU):           stringAttr("9000"),
					resourcev1.QualifiedName(apis.AttrMac):           stringAttr("aa:bb:cc:dd:ee:ff"),
				},
				Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
					"network-bandwidth": qtyCap("10G"),
					"other-capacity":    qtyCap("50"),
				},
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
		wantPodConfig *PodConfig
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
		})
	}
}
