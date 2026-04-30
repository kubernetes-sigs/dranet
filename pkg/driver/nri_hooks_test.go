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
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
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
	store1, err := NewPodConfigStore(cp1)
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
	storeAfterRestart, err := NewPodConfigStore(cp2)
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
	store1, err := NewPodConfigStore(cp1)
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
	storeAfterRestart, err := NewPodConfigStore(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() after restart error: %v", err)
	}
	defer storeAfterRestart.Close()

	np := &NetworkDriver{
		podConfigStore: storeAfterRestart,
		netdb:          inventory.New(),
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

func TestSynchronizePrunesStaleConfigs(t *testing.T) {
	store := mustNewPodConfigStore()
	store.SetDeviceConfig("live-pod", "eth0", DeviceConfig{})   //nolint:errcheck
	store.SetDeviceConfig("stale-pod", "eth0", DeviceConfig{})  //nolint:errcheck
	store.SetDeviceConfig("stale-pod2", "eth0", DeviceConfig{}) //nolint:errcheck

	np := &NetworkDriver{
		podConfigStore: store,
		netdb:          inventory.New(),
	}

	// Synchronize with only "live-pod" present in the runtime.
	pods := []*api.PodSandbox{
		{
			Uid:       "live-pod",
			Name:      "live",
			Namespace: "default",
			Linux:     &api.LinuxPodSandbox{},
		},
	}
	_, err := np.Synchronize(context.Background(), pods, nil)
	if err != nil {
		t.Fatalf("Synchronize() error: %v", err)
	}

	// live-pod should still be in the store.
	if _, found := store.GetPodConfig("live-pod"); !found {
		t.Error("live-pod should still exist after sync")
	}
	// stale pods should be pruned.
	if _, found := store.GetPodConfig("stale-pod"); found {
		t.Error("stale-pod should have been pruned")
	}
	if _, found := store.GetPodConfig("stale-pod2"); found {
		t.Error("stale-pod2 should have been pruned")
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
			}

			// For the failure case, a pod config must exist.
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
			setupNetNs:        true,
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
			if tc.setupNetNs {
				netdb.AddPodNetNs(podKey(pod), "/dummy/netns")
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
