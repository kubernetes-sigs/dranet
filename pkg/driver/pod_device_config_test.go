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
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

// mustNewPodConfigStore creates a PodConfigStore with no checkpointer for use in tests.
func mustNewPodConfigStore() *PodConfigStore {
	s, err := NewPodConfigStore(nil)
	if err != nil {
		panic(fmt.Sprintf("NewPodConfigStore(nil) should never fail: %v", err))
	}
	return s
}

func TestNewPodConfigStore(t *testing.T) {
	store := mustNewPodConfigStore()
	if store == nil {
		t.Fatal("mustNewPodConfigStore() returned nil")
	}
	if store.configs == nil {
		t.Error("mustNewPodConfigStore() did not initialize configs map")
	}
	if store.allocatedIPs == nil {
		t.Error("mustNewPodConfigStore() did not initialize allocatedIPs map")
	}
}

func TestPodConfigStore_SetAndGet(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID := types.UID("test-pod-uid-1")
	deviceName := "eth0"
	config := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
			Routes: []apis.RouteConfig{
				{Destination: "0.0.0.0/0", Gateway: "192.168.1.1"},
			},
			Ethtool: &apis.EthtoolConfig{
				Features: map[string]bool{"tx-checksumming": true},
			},
		},
		RDMADevice: RDMAConfig{LinkDev: "mlx5_0"},
	}

	// Test Get on non-existent item
	_, found := store.GetDeviceConfig(podUID, deviceName)
	if found {
		t.Errorf("Get() found a config before Set(), expected not found")
	}

	store.SetDeviceConfig(podUID, deviceName, config)

	retrievedConfig, found := store.GetDeviceConfig(podUID, deviceName)
	if !found {
		t.Fatalf("Get() did not find config after Set(), expected found")
	}
	if !reflect.DeepEqual(retrievedConfig, config) {
		t.Errorf("Get() retrieved %+v, want %+v", retrievedConfig, config)
	}

	// Test Get with different deviceName
	_, found = store.GetDeviceConfig(podUID, "eth1")
	if found {
		t.Errorf("Get() found config for wrong deviceName 'eth1', expected not found")
	}

	// Test Get with different podUID
	_, found = store.GetDeviceConfig(types.UID("other-pod-uid"), deviceName)
	if found {
		t.Errorf("Get() found config for wrong podUID, expected not found")
	}

	// Test overwriting
	newConfig := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-new"},
			Ethtool:   &apis.EthtoolConfig{PrivateFlags: map[string]bool{"custom-flag": false}},
		},
	}
	store.SetDeviceConfig(podUID, deviceName, newConfig)
	retrievedConfig, found = store.GetDeviceConfig(podUID, deviceName)
	if !found {
		t.Fatalf("Get() did not find config after overwrite, expected found")
	}
	if !reflect.DeepEqual(retrievedConfig, newConfig) {
		t.Errorf("Get() retrieved %+v after overwrite, want %+v", retrievedConfig, newConfig)
	}
}

// TestPodConfigStore_NetNs verifies that NetNS path can be stored and retrieved correctly in memory.
func TestPodConfigStore_NetNs(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID := types.UID("test-pod-uid-1")
	netns := "/var/run/netns/test-ns"

	// Test Get on non-existent item
	podCfg, found := store.GetPodConfig(podUID)
	if found {
		t.Errorf("GetPodConfig() found a config before SetDeviceConfig(), expected not found")
	}

	// Add a dummy device config so the pod exists in the store
	store.SetDeviceConfig(podUID, "dummy-device", DeviceConfig{})

	// Verify that NetNS is empty initially
	podCfg, found = store.GetPodConfig(podUID)
	if !found {
		t.Fatalf("GetPodConfig() did not find config after SetDeviceConfig()")
	}
	if podCfg.NetNS != "" {
		t.Errorf("NetNS should be empty initially, got %s", podCfg.NetNS)
	}

	store.SetPodNetNs(podUID, netns)

	podCfg, found = store.GetPodConfig(podUID)
	if !found {
		t.Fatalf("GetPodConfig() did not find config after SetPodNetNs(), expected found")
	}
	if podCfg.NetNS != netns {
		t.Errorf("GetPodConfig() retrieved NetNS %s, want %s", podCfg.NetNS, netns)
	}

	// Test Get with different podUID
	_, found = store.GetPodConfig(types.UID("other-pod-uid"))
	if found {
		t.Errorf("GetPodConfig() found config for wrong podUID, expected not found")
	}

	// Test overwriting
	newNetNs := "/var/run/netns/new-ns"
	store.SetPodNetNs(podUID, newNetNs)
	podCfg, found = store.GetPodConfig(podUID)
	if !found {
		t.Fatalf("GetPodConfig() did not find config after overwrite, expected found")
	}
	if podCfg.NetNS != newNetNs {
		t.Errorf("GetPodConfig() retrieved NetNS %s after overwrite, want %s", podCfg.NetNS, newNetNs)
	}
}

func TestPodConfigStore_DeletePod(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID1 := types.UID("test-pod-uid-1")
	podUID2 := types.UID("test-pod-uid-2")
	dev1 := "eth0"
	dev2 := "eth1"
	config1 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1eth0"}}}
	config2 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1eth1"}}}
	config3 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p2eth0"}}}

	store.SetDeviceConfig(podUID1, dev1, config1)
	store.SetDeviceConfig(podUID1, dev2, config2)
	store.SetDeviceConfig(podUID2, dev1, config3)

	store.DeletePod(podUID1)

	_, found := store.GetDeviceConfig(podUID1, dev1)
	if found {
		t.Errorf("Get() found config for podUID1 device %s after DeletePod(), expected not found", dev1)
	}
	_, found = store.GetDeviceConfig(podUID1, dev2)
	if found {
		t.Errorf("Get() found config for podUID1 device %s after DeletePod(), expected not found", dev2)
	}

	retrievedConfig3, found := store.GetDeviceConfig(podUID2, dev1)
	if !found {
		t.Errorf("Get() did not find config for podUID2 after deleting podUID1, expected found")
	}
	if !reflect.DeepEqual(retrievedConfig3, config3) {
		t.Errorf("Get() for podUID2 retrieved %+v, want %+v", retrievedConfig3, config3)
	}

	// Test deleting non-existent pod
	store.DeletePod(types.UID("non-existent-pod")) // Should not panic
}

func TestPodConfigStore_GetPodConfigs(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID1 := types.UID("test-pod-uid-1")
	podUID2 := types.UID("test-pod-uid-2")
	dev1 := "eth0"
	dev2 := "eth1"
	config1 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1eth0"}}}
	config2 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1eth1"}}}
	config3 := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p2eth0"}}}

	store.SetDeviceConfig(podUID1, dev1, config1)
	store.SetDeviceConfig(podUID1, dev2, config2)
	store.SetDeviceConfig(podUID2, dev1, config3)

	expectedPod1Config := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		dev1: config1,
		dev2: config2,
	}}

	pod1Config, found := store.GetPodConfig(podUID1)
	if !found {
		t.Fatalf("GetPodConfigs() did not find configs for podUID1, expected found")
	}
	if !reflect.DeepEqual(pod1Config, expectedPod1Config) {
		t.Errorf("GetPodConfigs() for podUID1 returned %+v, want %+v", pod1Config, expectedPod1Config)
	}

	// Test GetPodConfigs for non-existent pod
	_, found = store.GetPodConfig(types.UID("non-existent-pod"))
	if found {
		t.Errorf("GetPodConfigs() found configs for non-existent pod, expected not found")
	}

	// Modify returned map and check if original is unchanged
	pod1Config.DeviceConfigs["newDev"] = DeviceConfig{}
	originalPod1Configs, _ := store.GetPodConfig(podUID1)
	if !reflect.DeepEqual(originalPod1Configs, expectedPod1Config) {
		t.Errorf("Original map in store was modified after GetPodConfigs() returned map was changed. Original: %+v, Expected: %+v", originalPod1Configs, expectedPod1Config)
	}
}

func TestPodConfigStore_ThreadSafety(t *testing.T) {
	store := mustNewPodConfigStore()
	numGoroutines := 100
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			podUID := types.UID(fmt.Sprintf("pod-%d", i))
			deviceName := fmt.Sprintf("eth%d", i%2)
			config := DeviceConfig{NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: fmt.Sprintf("dev-%d", i)}}}
			store.SetDeviceConfig(podUID, deviceName, config)
			retrieved, _ := store.GetDeviceConfig(podUID, deviceName)
			if !reflect.DeepEqual(retrieved, config) {
				t.Errorf("goroutine %d: Get() retrieved %+v, want %+v", i, retrieved, config)
			}
			if i%10 == 0 {
				store.DeletePod(podUID)
				_, found := store.GetDeviceConfig(podUID, deviceName)
				if found {
					t.Errorf("goroutine %d: Get() found config after DeletePod()", i)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestPodConfigStore_DeleteClaim(t *testing.T) {
	claim1 := types.NamespacedName{Namespace: "ns1", Name: "claim1"}
	claim2 := types.NamespacedName{Namespace: "ns1", Name: "claim2"}

	podUID1 := types.UID("pod-uid-1")
	podUID2 := types.UID("pod-uid-2")
	podUID3 := types.UID("pod-uid-3")

	dev1 := "eth0"
	dev2 := "eth1"

	config1_1 := DeviceConfig{Claim: claim1, NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1d1c1"}}} // Pod1, Dev1, Claim1
	config1_2 := DeviceConfig{Claim: claim1, NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p1d2c1"}}} // Pod1, Dev2, Claim1
	config2_1 := DeviceConfig{Claim: claim1, NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p2d1c1"}}} // Pod2, Dev1, Claim1
	config3_1 := DeviceConfig{Claim: claim2, NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "p3d1c2"}}} // Pod3, Dev1, Claim2

	tests := []struct {
		name                string
		initialConfigs      func() *PodConfigStore
		claimToDelete       types.NamespacedName
		expectedPodsAfter   map[types.UID]PodConfig
		checkSpecificConfig func(t *testing.T, store *PodConfigStore)
	}{
		{
			name: "delete claim associated with one pod, one device",
			initialConfigs: func() *PodConfigStore {
				s := mustNewPodConfigStore()
				s.SetDeviceConfig(podUID3, dev1, config3_1) // Pod3 has Claim2
				s.SetDeviceConfig(podUID1, dev1, config1_1) // Pod1 has Claim1
				return s
			},
			claimToDelete: claim2, // Delete Claim2
			expectedPodsAfter: map[types.UID]PodConfig{
				podUID1: {DeviceConfigs: map[string]DeviceConfig{dev1: config1_1}}, // Pod1 (Claim1) should remain
			},
		},
		{
			name: "delete claim associated with multiple pods",
			initialConfigs: func() *PodConfigStore {
				s := mustNewPodConfigStore()
				s.SetDeviceConfig(podUID1, dev1, config1_1) // Pod1, Dev1, Claim1
				s.SetDeviceConfig(podUID1, dev2, config1_2) // Pod1, Dev2, Claim1
				s.SetDeviceConfig(podUID2, dev1, config2_1) // Pod2, Dev1, Claim1
				s.SetDeviceConfig(podUID3, dev1, config3_1) // Pod3, Dev1, Claim2
				return s
			},
			claimToDelete: claim1, // Delete Claim1
			expectedPodsAfter: map[types.UID]PodConfig{
				podUID3: {DeviceConfigs: map[string]DeviceConfig{dev1: config3_1}}, // Pod3 (Claim2) should remain
			},
		},
		{
			name: "delete non-existent claim",
			initialConfigs: func() *PodConfigStore {
				s := mustNewPodConfigStore()
				s.SetDeviceConfig(podUID1, dev1, config1_1)
				return s
			},
			claimToDelete: types.NamespacedName{Namespace: "ns-other", Name: "claim-non-existent"},
			expectedPodsAfter: map[types.UID]PodConfig{
				podUID1: {DeviceConfigs: map[string]DeviceConfig{dev1: config1_1}}, // Pod1 should remain
			},
		},
		{
			name: "delete claim from empty store",
			initialConfigs: func() *PodConfigStore {
				return mustNewPodConfigStore()
			},
			claimToDelete:     claim1,
			expectedPodsAfter: map[types.UID]PodConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := tt.initialConfigs()
			store.DeleteClaim(tt.claimToDelete)

			if !reflect.DeepEqual(store.configs, tt.expectedPodsAfter) {
				t.Errorf("configs mismatch after DeleteClaim.\nGot:    %+v\nWanted: %+v", store.configs, tt.expectedPodsAfter)
			}
			if tt.checkSpecificConfig != nil {
				tt.checkSpecificConfig(t, store)
			}
		})
	}
}

func TestPodConfigStore_NoDuplicateDevices(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID := types.UID("test-pod-uid-1")
	deviceName1 := "eth0"
	config1 := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
		},
		RDMADevice: RDMAConfig{
			LinkDev: "mlx5_0",
			DevChars: []LinuxDevice{{
				Path: "/dev/infiniband/rdma_cm",
			}, {
				Path: "/dev/infiniband/uverbs1",
			}},
		},
	}
	deviceName2 := "eth1"
	config2 := DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth2-pod"},
		},
		RDMADevice: RDMAConfig{
			LinkDev: "mlx5_1",
			DevChars: []LinuxDevice{{
				Path: "/dev/infiniband/rdma_cm",
			}, {
				Path: "/dev/infiniband/uverbs2",
			}},
		},
	}

	// Set the same device config multiple times
	store.SetDeviceConfig(podUID, deviceName1, config1)
	store.SetDeviceConfig(podUID, deviceName2, config2)
	store.SetDeviceConfig(podUID, deviceName1, config1)

	podConfigs, found := store.GetPodConfig(podUID)
	if !found {
		t.Fatalf("GetPodConfigs() did not find configs for podUID, expected found")
	}

	if len(podConfigs.DeviceConfigs) != 2 {
		t.Errorf("Expected 2 device config, but got %d", len(podConfigs.DeviceConfigs))
	}

	if _, ok := podConfigs.DeviceConfigs[deviceName1]; !ok {
		t.Errorf("Device %s not found in pod configs", deviceName2)
	}
	if _, ok := podConfigs.DeviceConfigs[deviceName2]; !ok {
		t.Errorf("Device %s not found in pod configs", deviceName2)
	}
}

// TestPodConfigStore_AllocatedIPs validates the lifecycle of allocated IP addresses
// managed by the store, including empty initialization, registration of IPv4/IPv6 via
// SetDeviceConfig, IP retrieving via GetAllocatedIPs, and cleanups via DeleteClaim.
func TestPodConfigStore_AllocatedIPs(t *testing.T) {
	store := mustNewPodConfigStore()
	podUID1 := types.UID("pod-1")
	podUID2 := types.UID("pod-2")
	claim1 := types.NamespacedName{Namespace: "ns1", Name: "claim1"}
	claim2 := types.NamespacedName{Namespace: "ns1", Name: "claim2"}

	// 1. Verify the list is initially empty.
	ips := store.GetAllocatedIPs()
	if len(ips) != 0 {
		t.Errorf("Expected 0 allocated IPs initially, got %d", len(ips))
	}

	// 2. Set Device Config with IPv6 prefix address.
	config1 := DeviceConfig{
		Claim: claim1,
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Addresses: []string{"2001:db8::3/128"},
			},
		},
	}
	if err := store.SetDeviceConfig(podUID1, "eth0", config1); err != nil {
		t.Fatalf("SetDeviceConfig failed: %v", err)
	}

	// 3. Set Device Config with IPv4 raw address.
	config2 := DeviceConfig{
		Claim: claim2,
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Addresses: []string{"192.168.1.100"},
			},
		},
	}
	if err := store.SetDeviceConfig(podUID2, "eth0", config2); err != nil {
		t.Fatalf("SetDeviceConfig failed: %v", err)
	}

	// 4. Get allocated IPs and verify the existance.
	ips = store.GetAllocatedIPs()
	if len(ips) != 2 {
		t.Errorf("Expected 2 allocated IPs, got %d", len(ips))
	}

	addr1 := netip.MustParseAddr("2001:db8::3")
	if uid, exists := ips[addr1]; !exists || uid != podUID1 {
		t.Errorf("Expected %s to be allocated to %s, got existence=%t uid=%s", addr1, podUID1, exists, uid)
	}

	addr2 := netip.MustParseAddr("192.168.1.100")
	if uid, exists := ips[addr2]; !exists || uid != podUID2 {
		t.Errorf("Expected %s to be allocated to %s, got existence=%t uid=%s", addr2, podUID2, exists, uid)
	}

	// 5. Overwrite device config for podUID2 with a different IP and verify the old IP is released.
	config2New := DeviceConfig{
		Claim: claim2,
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Addresses: []string{"192.168.1.101"},
			},
		},
	}
	if err := store.SetDeviceConfig(podUID2, "eth0", config2New); err != nil {
		t.Fatalf("SetDeviceConfig overwrite failed: %v", err)
	}

	ips = store.GetAllocatedIPs()
	if uid, exists := ips[addr2]; exists {
		t.Errorf("Expected old IP %s to be released after overwrite, but still owned by %s", addr2, uid)
	}
	addr2New := netip.MustParseAddr("192.168.1.101")
	if uid, exists := ips[addr2New]; !exists || uid != podUID2 {
		t.Errorf("Expected new IP %s to be allocated to %s, got existence=%t uid=%s", addr2New, podUID2, exists, uid)
	}

	// 6. DeleteClaim should free associated IP matched to the deleted claim but keep other IPs remain.
	store.DeleteClaim(claim1)
	ips = store.GetAllocatedIPs()

	if uid, exists := ips[addr1]; exists {
		t.Errorf("Expected %s to be deleted, but it still exists with uid=%s", addr1, uid)
	}
	if uid, exists := ips[addr2New]; !exists || uid != podUID2 {
		t.Errorf("Expected %s to be allocated to %s, got existence=%t uid=%s", addr2New, podUID2, exists, uid)
	}
}
