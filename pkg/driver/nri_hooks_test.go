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
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/inventory"
)

func TestCreateContainerNoDuplicateDevices(t *testing.T) {
	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
	}

	podUID := types.UID("test-pod")
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod",
		Namespace: "test-ns",
	}
	ctr := &api.Container{
		Name: "test-container",
	}

	// Setup pod config with duplicate RDMA devices
	rdmaDevChars := []LinuxDevice{
		{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
	}

	deviceCfg := DeviceConfig{
		RDMADevice: RDMAConfig{
			DevChars: rdmaDevChars,
		},
	}
	np.podConfigStore.SetDeviceConfig(podUID, "eth0", deviceCfg)
	np.podConfigStore.SetDeviceConfig(podUID, "eth1", deviceCfg)

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}

	if len(adjust.Linux.Devices) != 1 {
		t.Errorf("CreateContainer should not adjust the same device multiple times\n%v", adjust.Linux.Devices)
	}
}

func TestCreateContainerUsesPersistedConfigAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pod_configs.db")
	podUID := types.UID("test-pod")
	deviceCfg := DeviceConfig{
		RDMADevice: RDMAConfig{
			DevChars: []LinuxDevice{
				{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
			},
		},
	}

	// Simulate NodePrepareResource storing config before the driver restarts.
	cp1, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() error: %v", err)
	}
	store1, err := newPodConfigStoreWithCheckpointer(cp1)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	store1.SetDeviceConfig(podUID, "eth0", deviceCfg) //nolint:errcheck
	if err := store1.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	cp2, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() after restart error: %v", err)
	}
	storeAfterRestart, err := newPodConfigStoreWithCheckpointer(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() after restart error: %v", err)
	}
	defer storeAfterRestart.Close()

	np := &NetworkDriver{podConfigStore: storeAfterRestart}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "test-pod", Namespace: "test-ns"}
	ctr := &api.Container{Name: "test-container"}

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}
	if adjust == nil || adjust.Linux == nil {
		t.Fatalf("CreateContainer returned nil container adjustment")
	}
	if len(adjust.Linux.Devices) != 1 {
		t.Fatalf("expected 1 injected RDMA char device after restart, got %d", len(adjust.Linux.Devices))
	}
	if got := adjust.Linux.Devices[0].Path; got != "/dev/infiniband/uverbs0" {
		t.Fatalf("unexpected injected device path %q", got)
	}
}

func TestRunPodSandboxUsesPersistedConfigAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dranet.db")
	podUID := types.UID("test-pod-sandbox")
	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		// Set a host interface name so runPodSandbox takes the netdev path,
		// which will fail (no real interface) — proving the config was found.
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "nonexistent0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
		},
	}

	// Simulate NodePrepareResource storing config before the driver restarts.
	cp1, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() error: %v", err)
	}
	store1, err := newPodConfigStoreWithCheckpointer(cp1)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	if err := store1.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Reopen store to simulate driver restart.
	cp2, err := newBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("newBoltCheckpointer() after restart error: %v", err)
	}
	storeAfterRestart, err := newPodConfigStoreWithCheckpointer(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() after restart error: %v", err)
	}
	defer storeAfterRestart.Close()

	np := &NetworkDriver{
		podConfigStore: storeAfterRestart,
		netdb:          inventory.New(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod-sandbox",
		Namespace: "test-ns",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/test"},
			},
		},
	}

	// RunPodSandbox should find the persisted config and attempt netdev
	// operations, which will fail (no real interface). An error proves the
	// config was found — a nil return would mean the config was missing.
	err = np.RunPodSandbox(context.Background(), pod)
	if err == nil {
		t.Fatal("expected RunPodSandbox to error (config found, netdev ops fail), got nil (config missing?)")
	}
}

// TestRunPodSandboxSubinterfaceCreation tests the subinterface creation
// during RunPodSandbox. It verifies the creation call by checking
// the expected error message is returned from nsCreateSubinterface.
func TestRunPodSandboxSubinterfaceCreation(t *testing.T) {
	podUID := types.UID("test-pod-subinterface")
	store := mustNewPodConfigStore()

	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "nonexistent-parent"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:      "eth0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"2001:db8::10/64"},
			},
		},
	}

	if err := store.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  record.NewFakeRecorder(100),
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "test-pod-subinterface",
		Namespace: "test-ns",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/proc/self/ns/net"},
			},
		},
	}

	// Verify that the subinterface creation path is called by passing a non-existent parent interface
	// and asserting that the call fails with a "could not find parent interface on host" error.
	err := np.RunPodSandbox(context.Background(), pod)
	if err == nil {
		t.Fatal("expected RunPodSandbox to error, got nil")
	}

	if !strings.Contains(err.Error(), "could not find parent interface nonexistent-parent on host") {
		t.Errorf("expected error to contain 'could not find parent interface nonexistent-parent on host', got: %v", err)
	}
}

func TestSynchronizeStoresNetNSOnlyForConfiguredPods(t *testing.T) {
	store := mustNewPodConfigStore()

	// Pod 1: Has device config (configured)
	store.SetDeviceConfig("configured-pod", "eth0", DeviceConfig{}) //nolint:errcheck

	// Pod 2: Does not have device config (unconfigured)

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
	}

	pods := []*api.PodSandbox{
		{
			Uid:       "configured-pod",
			Name:      "configured",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{
					{Type: "network", Path: "/var/run/netns/configured"},
				},
			},
		},
		{
			Uid:       "unconfigured-pod",
			Name:      "unconfigured",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{
					{Type: "network", Path: "/var/run/netns/unconfigured"},
				},
			},
		},
	}

	_, err := np.Synchronize(context.Background(), pods, nil)
	if err != nil {
		t.Fatalf("Synchronize() error: %v", err)
	}

	// Case 1: Configured pod should have its NetNS stored
	podConfig1, found1 := store.GetPodConfig("configured-pod")
	if !found1 {
		t.Error("configured-pod should have its config stored")
	}
	if podConfig1.NetNS != "/var/run/netns/configured" {
		t.Errorf("expected NetNS /var/run/netns/configured, got %q", podConfig1.NetNS)
	}

	// Case 2: Unconfigured pod should NOT have its NetNS stored
	podConfig2, found2 := store.GetPodConfig("unconfigured-pod")
	if found2 && podConfig2.NetNS != "" {
		t.Error("unconfigured-pod should NOT have its NetNS stored")
	}

	// Also verify it didn't create a skeleton config for unconfigured-pod
	_, found3 := store.GetPodConfig("unconfigured-pod")
	if found3 {
		t.Error("unconfigured-pod should NOT have any PodConfig in the store")
	}
}

func TestCreateContainerMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}

			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}
			ctr := &api.Container{
				Name: "test-container",
			}

			np.CreateContainer(context.Background(), pod, ctr)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="CreateContainer",status="noop",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodCreateContainer, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

func TestRunPodSandboxMetrics(t *testing.T) {
	podUID := types.UID("test-pod")
	podUIDHostNetwork := types.UID("test-pod-host-network")

	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		pod            *api.PodSandbox
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			pod: &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
				Linux: &api.LinuxPodSandbox{
					Namespaces: []*api.LinuxNamespace{
						{
							Type: "network",
							Path: "/var/run/netns/test",
						},
					},
				},
			},
			expectSuccess: true,
		},
		{
			name:           "Failure - Host Network",
			podConfigStore: mustNewPodConfigStore(),
			pod: &api.PodSandbox{
				Uid:       string(podUIDHostNetwork),
				Name:      "test-pod-host-network",
				Namespace: "test-ns",
				Linux:     &api.LinuxPodSandbox{}, // No network namespace
			},
			expectSuccess: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
				eventRecorder:  record.NewFakeRecorder(100),
			}
			if !tc.expectSuccess {
				tc.podConfigStore.SetDeviceConfig(podUIDHostNetwork, "eth0", DeviceConfig{})
			}

			np.RunPodSandbox(context.Background(), tc.pod)
			status := statusSuccess
			if !tc.expectSuccess {
				status = statusFailed
			}
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox",le="+Inf"} 1
						`
			expected = strings.Replace(expected, `method="RunPodSandbox"`, `method="RunPodSandbox",status="`+status+`"`, -1)
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

func TestStopPodSandboxMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}

			np.StopPodSandbox(context.Background(), pod)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="StopPodSandbox",status="success",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodStopPodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

// TestNeedsRescanAfterDetach checks the gating logic that decides whether
// stopPodSandbox needs to fire an explicit inventory rescan after returning
// a device's RDMA / netdev to init_net.
func TestNeedsRescanAfterDetach(t *testing.T) {
	testCases := []struct {
		name           string
		rdmaDetached   bool
		netdevDetached bool
		want           bool
	}{
		{
			name:           "neither detached: nothing new in init_net",
			rdmaDetached:   false,
			netdevDetached: false,
			want:           false,
		},
		{
			name:           "netdev only: NEWLINK covers any rescan need",
			rdmaDetached:   false,
			netdevDetached: true,
			want:           false,
		},
		{
			name:           "RDMA only (IB-only success, or SR-IOV with netdev failure): rescan needed",
			rdmaDetached:   true,
			netdevDetached: false,
			want:           true,
		},
		{
			name:           "both detached (SR-IOV success): NEWLINK covers it",
			rdmaDetached:   true,
			netdevDetached: true,
			want:           false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRescanAfterDetach(tc.rdmaDetached, tc.netdevDetached); got != tc.want {
				t.Errorf("needsRescanAfterDetach(%v, %v) = %v, want %v",
					tc.rdmaDetached, tc.netdevDetached, got, tc.want)
			}
		})
	}
}

// TestStopPodSandboxRescanGating verifies the integration paths in
// stopPodSandbox where a rescan must NOT be requested. The success path
// (where a detach actually completes) cannot be exercised here because the
// real netlink calls fail against synthetic netns paths; that condition is
// covered by TestNeedsRescanAfterDetach.
func TestStopPodSandboxRescanGating(t *testing.T) {
	testCases := []struct {
		name              string
		setupDeviceConfig bool
		deviceConfig      DeviceConfig
		setupNetNs        bool
		rdmaSharedMode    bool
	}{
		{
			name:              "no device config: early return at NRI level",
			setupDeviceConfig: false,
			setupNetNs:        false,
		},
		{
			name:              "host network pod: stopPodSandbox skips before the loop",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        false,
		},
		{
			name:              "shared RDMA mode: RDMA branch is skipped",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        true,
			rdmaSharedMode:    true,
		},
		{
			name:              "exclusive RDMA + fake netns: detach fails, no rescan",
			setupDeviceConfig: true,
			deviceConfig:      DeviceConfig{RDMADevice: RDMAConfig{LinkDev: "mlx5_0"}},
			setupNetNs:        true,
		},
		{
			name:              "subinterface configured: no rescan triggered",
			setupDeviceConfig: true,
			deviceConfig: DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{
						Name: "ipvl-eth0",
						Type: apis.InterfaceTypeIPVLAN,
					},
				},
			},
			setupNetNs: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			netdb := newFakeInventoryDB()
			np := &NetworkDriver{
				podConfigStore: mustNewPodConfigStore(),
				netdb:          netdb,
				rdmaSharedMode: tc.rdmaSharedMode,
				eventRecorder:  record.NewFakeRecorder(100),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}
			if tc.setupDeviceConfig {
				np.podConfigStore.SetDeviceConfig(podUID, "eth0", tc.deviceConfig)
			}
			if tc.setupDeviceConfig && tc.setupNetNs {
				np.podConfigStore.SetPodNetNs(podUID, "/dummy/netns")
			}

			if err := np.StopPodSandbox(context.Background(), pod); err != nil {
				t.Fatalf("StopPodSandbox() error = %v", err)
			}
			if got := netdb.rescanCalls.Load(); got != 0 {
				t.Errorf("RequestRescan call count = %d, want 0", got)
			}
		})
	}
}

func TestRemovePodSandboxMetrics(t *testing.T) {
	testCases := []struct {
		name           string
		podConfigStore *PodConfigStore
		expectSuccess  bool
	}{
		{
			name:           "Success",
			podConfigStore: mustNewPodConfigStore(),
			expectSuccess:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nriPluginRequestsTotal.Reset()
			nriPluginRequestsLatencySeconds.Reset()
			np := &NetworkDriver{
				podConfigStore: tc.podConfigStore,
				netdb:          inventory.New(),
			}
			podUID := types.UID("test-pod")
			pod := &api.PodSandbox{
				Uid:       string(podUID),
				Name:      "test-pod",
				Namespace: "test-ns",
			}

			np.RemovePodSandbox(context.Background(), pod)
			expected := `
						# HELP dranet_driver_nri_plugin_requests_latency_seconds NRI plugin request latency in seconds.
						# TYPE dranet_driver_nri_plugin_requests_latency_seconds histogram
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.005"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.01"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.025"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.05"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.25"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="0.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="1"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="2.5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="5"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="10"} 1
						dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RemovePodSandbox",status="success",le="+Inf"} 1
						`
			if err := testutil.CollectAndCompare(nriPluginRequestsLatencySeconds, strings.NewReader(expected), "dranet_driver_nri_plugin_requests_latency_seconds_bucket"); err != nil {
				t.Fatalf("CollectAndCompare failed: %v", err)
			}
			if tc.expectSuccess {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusNoop)); got != float64(1) {
					t.Errorf("Expected 1 success, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusFailed)); got != float64(0) {
					t.Errorf("Expected 0 failures, got %f", got)
				}
			} else {
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusSuccess)); got != float64(0) {
					t.Errorf("Expected 0 successes, got %f", got)
				}
				if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRemovePodSandbox, statusFailed)); got != float64(1) {
					t.Errorf("Expected 1 failure, got %f", got)
				}
			}
		})
	}
}

// TestCreateContainerSubinterfaceRDMAInjection verifies that the
// RDMA character devices are correctly injected into the container
// adjustment when the pod is configured to use a subinterface.
func TestCreateContainerSubinterfaceRDMAInjection(t *testing.T) {
	podUID := types.UID("test-pod-subinterface")
	deviceCfg := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:      "ipvl-eth0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"2001:db8::10/64"},
			},
		},
		RDMADevice: RDMAConfig{
			DevChars: []LinuxDevice{
				{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192},
				{Path: "/dev/infiniband/rdma_cm", Type: "c", Major: 10, Minor: 58},
			},
		},
	}

	store := mustNewPodConfigStore()
	if err := store.SetDeviceConfig(podUID, "eth0", deviceCfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}

	np := &NetworkDriver{podConfigStore: store}
	pod := &api.PodSandbox{Uid: string(podUID), Name: "test-pod-subinterface", Namespace: "test-ns"}
	ctr := &api.Container{Name: "test-container"}

	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer failed: %v", err)
	}
	if adjust == nil || adjust.Linux == nil {
		t.Fatalf("CreateContainer returned nil container adjustment")
	}
	if len(adjust.Linux.Devices) != 2 {
		t.Fatalf("expected 2 injected RDMA char devices, got %d", len(adjust.Linux.Devices))
	}

	expectedPaths := []string{"/dev/infiniband/uverbs0", "/dev/infiniband/rdma_cm"}
	for i, expectedPath := range expectedPaths {
		if got := adjust.Linux.Devices[i].Path; got != expectedPath {
			t.Errorf("expected device %d path to be %q, got %q", i, expectedPath, got)
		}
	}
}

func TestRequestBudget(t *testing.T) {
	t.Run("no deadline returns parent context", func(t *testing.T) {
		np := &NetworkDriver{}
		ctx, cancel := np.requestBudget(context.Background())
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("expected no deadline when parent context has no deadline")
		}
	})

	t.Run("deadline larger than margin subtracts default margin", func(t *testing.T) {
		np := &NetworkDriver{}
		parent, parentCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer parentCancel()
		parentDeadline, _ := parent.Deadline()

		ctx, cancel := np.requestBudget(parent)
		defer cancel()
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected deadline on budget context")
		}
		want := parentDeadline.Add(-defaultNRIReplyMargin)
		if diff := got.Sub(want).Abs(); diff > 5*time.Millisecond {
			t.Fatalf("budget deadline = %v, want %v (diff %v)", got, want, diff)
		}
	})

	t.Run("deadline smaller than or equal to margin is already expired", func(t *testing.T) {
		np := &NetworkDriver{}
		parent, parentCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer parentCancel()

		ctx, cancel := np.requestBudget(parent)
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("expected budget context to be immediately expired when remaining <= margin, got %v", ctx.Err())
		}
		if parent.Err() != nil {
			t.Fatalf("parent context should still be active, got %v", parent.Err())
		}
	})
}

func TestRunPodSandboxAlreadyWithinReplyMarginFailsImmediately(t *testing.T) {
	origAttachNetdev := attachNetdev
	defer func() {
		attachNetdev = origAttachNetdev
	}()

	var calls atomic.Int32
	attachNetdev = func(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
		calls.Add(1)
		return &resourceapi.NetworkDeviceData{InterfaceName: interfaceConfig.Name}, nil, nil
	}

	store := mustNewPodConfigStore()
	podUID := types.UID("pod-no-budget")
	if err := store.SetDeviceConfig(podUID, "eth0", DeviceConfig{
		Claim: types.NamespacedName{Namespace: "default", Name: "claim"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}

	recorder := record.NewFakeRecorder(10)
	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  recorder,
		nriReplyMargin: 200 * time.Millisecond,
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-no-budget",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/pod-no-budget"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := np.RunPodSandbox(ctx, pod)
	if !errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("RunPodSandbox error = %v, want %v", err, errNRIBudgetExceeded)
	}
	if ctx.Err() != nil {
		t.Fatalf("parent NRI context already expired (%v)", ctx.Err())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("attachNetdev calls = %d, want 0 when already within reply margin", got)
	}

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "NetworkDeviceAttachTimeout") || !strings.Contains(ev, "attached 0/1") {
			t.Fatalf("unexpected event: %s", ev)
		}
	default:
		t.Fatal("expected NetworkDeviceAttachTimeout warning event")
	}
}

func TestRunPodSandboxFailsBeforeRuntimeDeadline(t *testing.T) {
	origAttachNetdev := attachNetdev
	defer func() {
		attachNetdev = origAttachNetdev
	}()

	var calls int
	attachNetdev = func(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
		calls++
		time.Sleep(60 * time.Millisecond)
		return &resourceapi.NetworkDeviceData{InterfaceName: interfaceConfig.Name}, nil, nil
	}

	store := mustNewPodConfigStore()
	podUID := types.UID("pod-timeout")
	for _, dev := range []string{"eth0", "eth1", "eth2"} {
		cfg := DeviceConfig{
			Claim: types.NamespacedName{Namespace: "default", Name: "claim"},
			NetworkInterfaceConfigInHost: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Name: dev},
			},
			NetworkInterfaceConfigInPod: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Name: dev},
			},
		}
		if err := store.SetDeviceConfig(podUID, dev, cfg); err != nil {
			t.Fatalf("SetDeviceConfig(%s): %v", dev, err)
		}
	}

	recorder := record.NewFakeRecorder(10)
	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  recorder,
		nriReplyMargin: 100 * time.Millisecond,
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-timeout",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/pod-timeout"},
			},
		},
	}

	// 250ms runtime deadline minus 100ms reply margin leaves a 150ms budget.
	// After 2 devices (120ms), only 30ms of budget remains (< 60ms/device), so
	// RunPodSandbox stops before starting the 3rd device and fails closed well
	// before the 250ms runtime deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := np.RunPodSandbox(ctx, pod)
	if !errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("RunPodSandbox error = %v, want %v", err, errNRIBudgetExceeded)
	}
	if ctx.Err() != nil {
		t.Fatalf("parent NRI context already expired (%v); RunPodSandbox must return before the runtime deadline", ctx.Err())
	}
	if calls != 2 {
		t.Fatalf("attachNetdev calls = %d, want 2 (should stop before starting 3rd device)", calls)
	}

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "NetworkDeviceAttachTimeout") {
			t.Fatalf("unexpected event: %s", ev)
		}
	default:
		t.Fatal("expected NetworkDeviceAttachTimeout warning event")
	}
}

func TestRunPodSandboxReplyMargin(t *testing.T) {
	origAttachNetdev := attachNetdev
	defer func() {
		attachNetdev = origAttachNetdev
	}()

	attachNetdev = func(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
		time.Sleep(180 * time.Millisecond)
		return &resourceapi.NetworkDeviceData{InterfaceName: interfaceConfig.Name}, nil, nil
	}

	store := mustNewPodConfigStore()
	podUID := types.UID("pod-custom-margin")
	if err := store.SetDeviceConfig(podUID, "eth0", DeviceConfig{
		Claim: types.NamespacedName{Namespace: "default", Name: "claim"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  record.NewFakeRecorder(10),
	}
	WithNRIReplyMargin(350 * time.Millisecond)(np)

	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-custom-margin",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/pod-custom-margin"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := np.RunPodSandbox(ctx, pod)
	elapsed := time.Since(start)
	if !errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("RunPodSandbox error = %v, want %v", err, errNRIBudgetExceeded)
	}
	if ctx.Err() != nil {
		t.Fatalf("parent NRI context already expired (%v)", ctx.Err())
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("RunPodSandbox took %v, expected ~180ms (well before 500ms timeout)", elapsed)
	}
}

func TestRunPodSandboxNonTimeoutDeviceError(t *testing.T) {
	origAttachNetdev := attachNetdev
	defer func() {
		attachNetdev = origAttachNetdev
	}()

	wantErr := errors.New("simulated netlink failure")
	attachNetdev = func(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
		return nil, nil, wantErr
	}

	store := mustNewPodConfigStore()
	podUID := types.UID("pod-device-error")
	if err := store.SetDeviceConfig(podUID, "eth0", DeviceConfig{
		Claim: types.NamespacedName{Namespace: "default", Name: "claim"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}

	recorder := record.NewFakeRecorder(10)
	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
		eventRecorder:  recorder,
	}
	pod := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-device-error",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/pod-device-error"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := np.RunPodSandbox(ctx, pod)
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("RunPodSandbox error = %v, want error containing %q", err, wantErr)
	}
	if errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("RunPodSandbox error should not wrap errNRIBudgetExceeded on non-timeout error: %v", err)
	}
}

func TestRunPodSandboxIntegrationRealNetns(t *testing.T) {
	userns.Run(t, testRunPodSandboxIntegrationRealNetns, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func newTestNamedNetns(t *testing.T) (string, string, func()) {
	t.Helper()
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer origns.Close()

	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rnd)
	nsPath := path.Join("/run/netns", nsName)
	nsHandle, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("netns.NewNamed(%s): %v", nsName, err)
	}
	if err := netns.Set(origns); err != nil {
		t.Fatalf("netns.Set(origns): %v", err)
	}
	var cleaned atomic.Bool
	cleanup := func() {
		if cleaned.CompareAndSwap(false, true) {
			_ = netns.DeleteNamed(nsName)
			_ = nsHandle.Close()
		}
	}
	t.Cleanup(cleanup)
	return nsName, nsPath, cleanup
}

func testRunPodSandboxIntegrationRealNetns(t *testing.T) {
	ifNames := []string{"dranet0", "dranet1", "dranet2"}
	ips := []string{"192.0.2.11/24", "192.0.2.12/24", "192.0.2.13/24"}
	for i, ifName := range ifNames {
		la := netlink.NewLinkAttrs()
		la.Name = ifName
		dummy := &netlink.Dummy{LinkAttrs: la}
		if err := netlink.LinkAdd(dummy); err != nil {
			t.Fatalf("LinkAdd(%s): %v", ifName, err)
		}
		t.Cleanup(func() {
			if l, err := nlwrap.LinkByName(ifName); err == nil {
				_ = netlink.LinkDel(l)
			}
		})
		link, err := nlwrap.LinkByName(ifName)
		if err != nil {
			t.Fatalf("LinkByName(%s): %v", ifName, err)
		}
		addr, err := netlink.ParseAddr(ips[i])
		if err != nil {
			t.Fatalf("ParseAddr(%s): %v", ips[i], err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("AddrAdd(%s): %v", ifName, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("LinkSetUp(%s): %v", ifName, err)
		}
	}

	podUID := types.UID("pod-real-netns-uid")
	claimUID := types.UID("claim-real-netns-uid")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-real",
			Namespace: "default",
			UID:       claimUID,
		},
		Status: resourceapi.ResourceClaimStatus{
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "pod-real", UID: podUID},
			},
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Request: "req0", Driver: "dra.net", Pool: "node-1", Device: "dranet0"},
						{Request: "req1", Driver: "dra.net", Pool: "node-1", Device: "dranet1"},
						{Request: "req2", Driver: "dra.net", Pool: "node-1", Device: "dranet2"},
					},
				},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(claim)
	eventCreated := make(chan *v1.Event, 10)
	fakeClient.PrependReactor("create", "events", func(action ktesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(ktesting.CreateAction)
		if ok {
			if ev, ok := createAction.GetObject().(*v1.Event); ok {
				eventCreated <- ev
			}
		}
		return false, nil, nil
	})
	statusPatched := make(chan ktesting.PatchAction, 10)
	fakeClient.PrependReactor("patch", "resourceclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if patchAction, ok := action.(ktesting.PatchAction); ok && patchAction.GetSubresource() == "status" {
			statusPatched <- patchAction
		}
		return false, nil, nil
	})

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: fakeClient.CoreV1().Events("")})
	t.Cleanup(eventBroadcaster.Shutdown)
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "dra.net", Host: "node-1"})

	netdb := newFakeInventoryDB()
	netdb.GetNetInterfaceNameFunc = func(deviceName string) (string, error) {
		return deviceName, nil
	}
	netdb.GetDeviceFunc = func(deviceName string) (resourceapi.Device, bool) {
		return resourceapi.Device{Name: deviceName}, true
	}

	np := &NetworkDriver{
		driverName:     "dra.net",
		nodeName:       "node-1",
		kubeClient:     fakeClient,
		podConfigStore: mustNewPodConfigStore(),
		netdb:          netdb,
		eventRecorder:  eventRecorder,
		rdmaSharedMode: true,
		nriReplyMargin: 100 * time.Millisecond,
	}

	// 1. PrepareResourceClaims captures addresses/routes and sets host links DOWN.
	prepRes, err := np.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatalf("PrepareResourceClaims error: %v", err)
	}
	if res := prepRes[claimUID]; res.Err != nil {
		t.Fatalf("PrepareResourceClaims claim error: %v", res.Err)
	}
	for _, ifName := range ifNames {
		link, err := nlwrap.LinkByName(ifName)
		if err != nil {
			t.Fatalf("LinkByName(%s) after PrepareResourceClaims: %v", ifName, err)
		}
		if link.Attrs().Flags&net.FlagUp != 0 {
			t.Fatalf("expected %s to be DOWN after PrepareResourceClaims", ifName)
		}
	}

	// 2. Attempt #1: RunPodSandbox with 60ms per device and 250ms deadline (150ms budget).
	// Attaches 2 of 3 devices and fails before starting the 3rd device.
	_, nsPath1, cleanupNs1 := newTestNamedNetns(t)
	podAttempt1 := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-real",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: nsPath1},
			},
		},
	}

	origAttachNetdev := attachNetdev
	attachNetdev = func(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
		time.Sleep(60 * time.Millisecond)
		return origAttachNetdev(h, hostIfName, interfaceConfig)
	}

	attempt1Ctx, attempt1Cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	err = np.RunPodSandbox(attempt1Ctx, podAttempt1)
	attachNetdev = origAttachNetdev
	if attempt1Ctx.Err() != nil {
		attempt1Cancel()
		t.Fatalf("attempt #1 parent NRI context already expired (%v)", attempt1Ctx.Err())
	}
	// Simulate the NRI RPC completing and canceling its request context immediately.
	attempt1Cancel()
	if !errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("attempt #1 RunPodSandbox error = %v, want %v", err, errNRIBudgetExceeded)
	}

	// Verify the Warning event is still published to the API server via EventBroadcaster
	// even though attempt1Ctx was canceled immediately upon RunPodSandbox returning.
	select {
	case ev := <-eventCreated:
		if ev.Reason != "NetworkDeviceAttachTimeout" {
			t.Fatalf("event Reason = %q, want NetworkDeviceAttachTimeout", ev.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for NetworkDeviceAttachTimeout event to be published to fakeClient")
	}

	// Simulate the container runtime tearing down the failed sandbox (StopPodSandbox + deleting the netns).
	if err := np.StopPodSandbox(context.Background(), podAttempt1); err != nil {
		t.Fatalf("StopPodSandbox after failed attempt #1 error: %v", err)
	}
	cleanupNs1()

	for _, ifName := range ifNames {
		if _, err := nlwrap.LinkByName(ifName); err != nil {
			t.Fatalf("device %s was not returned to host netns after tearing down failed sandbox #1: %v", ifName, err)
		}
	}

	// 3. Attempt #2 (Kubelet retry): new sandbox netns, enough budget for all 3 devices.
	_, nsPath2, cleanupNs2 := newTestNamedNetns(t)
	defer cleanupNs2()
	podAttempt2 := &api.PodSandbox{
		Uid:       string(podUID),
		Name:      "pod-real",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: nsPath2},
			},
		},
	}

	attempt2Ctx, attempt2Cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = np.RunPodSandbox(attempt2Ctx, podAttempt2)
	// Cancel the NRI request context immediately after RunPodSandbox returns to prove
	// the asynchronous ResourceClaim ApplyStatus call is not cut off by context termination.
	attempt2Cancel()
	if err != nil {
		t.Fatalf("attempt #2 RunPodSandbox error = %v", err)
	}

	select {
	case patchAction := <-statusPatched:
		patchBytes := string(patchAction.GetPatch())
		for _, ifName := range ifNames {
			if !strings.Contains(patchBytes, ifName) {
				t.Fatalf("ResourceClaim status patch missing device %s: %s", ifName, patchBytes)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async ResourceClaim ApplyStatus after attempt2Ctx cancellation")
	}

	// Verify all 3 interfaces are inside pod netns #2, UP, with their IP addresses.
	podNs2, err := netns.GetFromPath(nsPath2)
	if err != nil {
		t.Fatalf("netns.GetFromPath(%s): %v", nsPath2, err)
	}
	defer podNs2.Close()
	nhPod2, err := nlwrap.NewHandleAt(podNs2)
	if err != nil {
		t.Fatalf("nlwrap.NewHandleAt(podNs2): %v", err)
	}
	defer nhPod2.Close()

	for i, ifName := range ifNames {
		link, err := nhPod2.LinkByName(ifName)
		if err != nil {
			t.Fatalf("device %s not found in pod netns #2: %v", ifName, err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Fatalf("device %s in pod netns #2 is not UP", ifName)
		}
		addrs, err := nhPod2.AddrList(link, netlink.FAMILY_V4)
		if err != nil || len(addrs) != 1 || addrs[0].IPNet.String() != ips[i] {
			t.Fatalf("device %s in pod netns #2 addresses = %v (err %v), want %s", ifName, addrs, err, ips[i])
		}
	}

	// 4. StopPodSandbox + UnprepareResourceClaims returns all devices to host netns and sets them UP.
	if err := np.StopPodSandbox(context.Background(), podAttempt2); err != nil {
		t.Fatalf("StopPodSandbox error: %v", err)
	}
	unprepRes, err := np.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-real"}, UID: claimUID},
	})
	if err != nil {
		t.Fatalf("UnprepareResourceClaims error: %v", err)
	}
	if unprepErr := unprepRes[claimUID]; unprepErr != nil {
		t.Fatalf("UnprepareResourceClaims claim error: %v", unprepErr)
	}

	for _, ifName := range ifNames {
		link, err := nlwrap.LinkByName(ifName)
		if err != nil {
			t.Fatalf("device %s not found in host netns after StopPodSandbox: %v", ifName, err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Fatalf("device %s in host netns is not UP after teardown", ifName)
		}
	}
}
