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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	bolt "go.etcd.io/bbolt"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
)

func newTestBoltCheckpointer(t *testing.T) *BoltCheckpointer {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cp, err := NewBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("NewBoltCheckpointer() error: %v", err)
	}
	t.Cleanup(func() { cp.Close() })
	return cp
}

func newTestStoreWithBolt(t *testing.T) (*PodConfigStore, *BoltCheckpointer) {
	t.Helper()
	cp := newTestBoltCheckpointer(t)
	store, err := NewPodConfigStore(cp)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	return store, cp
}

func TestBoltCheckpointer_StoreAndGetOrCreate(t *testing.T) {
	cp := newTestBoltCheckpointer(t)
	podUID := types.UID("test-pod-uid-1")
	deviceName := "eth0"
	config := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
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

	// Empty checkpoint.
	data, err := cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate() error: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty checkpoint, got %d entries", len(data))
	}

	// Store and read back.
	if err := cp.Store(podUID, deviceName, config); err != nil {
		t.Fatalf("Store() error: %v", err)
	}
	data, err = cp.GetOrCreate()
	if err != nil {
		t.Fatalf("GetOrCreate() error: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(data))
	}
	if diff := cmp.Diff(config, data[podUID][deviceName]); diff != "" {
		t.Errorf("Store()/GetOrCreate() mismatch (-want +got):\n%s", diff)
	}
}

func TestBoltCheckpointer_DeletePod(t *testing.T) {
	cp := newTestBoltCheckpointer(t)
	cp.Store("pod-1", "eth0", DeviceConfig{})
	cp.Store("pod-2", "eth0", DeviceConfig{})

	if err := cp.DeletePod("pod-1"); err != nil {
		t.Fatalf("DeletePod() error: %v", err)
	}
	data, _ := cp.GetOrCreate()
	if _, ok := data["pod-1"]; ok {
		t.Error("pod-1 should have been deleted")
	}
	if _, ok := data["pod-2"]; !ok {
		t.Error("pod-2 should still exist")
	}

	// Delete non-existent pod — should not error.
	if err := cp.DeletePod("non-existent"); err != nil {
		t.Errorf("DeletePod(non-existent) error: %v", err)
	}
}

func TestBoltCheckpointer_DeviceConfigsBucketStructure(t *testing.T) {
	cp := newTestBoltCheckpointer(t)
	config := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
	}
	cp.Store("pod-1", "eth0", config)

	// Verify the bucket structure: pod_configs -> pod-1 -> device_configs -> eth0
	err := cp.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(podConfigsBucket)
		if root == nil {
			return fmt.Errorf("missing root bucket")
		}
		podBucket := root.Bucket([]byte("pod-1"))
		if podBucket == nil {
			return fmt.Errorf("missing pod bucket")
		}
		devBucket := podBucket.Bucket(deviceConfigsKey)
		if devBucket == nil {
			return fmt.Errorf("missing device_configs sub-bucket")
		}
		data := devBucket.Get([]byte("eth0"))
		if data == nil {
			return fmt.Errorf("missing eth0 entry in device_configs")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPodConfigStore_Persistence verifies the full layered flow:
// write through PodConfigStore → bolt, close, reopen, verify state restored.
func TestPodConfigStore_Persistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persist.db")

	config := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns1", Name: "claim1"},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
			Routes:    []apis.RouteConfig{{Destination: "10.0.0.0/8", Gateway: "10.0.0.1"}},
		},
		RDMADevice: RDMAConfig{
			LinkDev: "mlx5_0",
			DevChars: []LinuxDevice{
				{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 0},
			},
		},
	}

	// Write data via PodConfigStore and close.
	cp1, err := NewBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("NewBoltCheckpointer() error: %v", err)
	}
	store1, err := NewPodConfigStore(cp1)
	if err != nil {
		t.Fatalf("NewPodConfigStore() error: %v", err)
	}
	store1.SetDeviceConfig("pod-1", "eth0", config)
	store1.SetPodNetNs("pod-1", "/var/run/netns/test-ns")
	store1.Close()

	// Reopen and verify data was restored from checkpoint.
	cp2, err := NewBoltCheckpointer(dbPath)
	if err != nil {
		t.Fatalf("NewBoltCheckpointer() reopen error: %v", err)
	}
	store2, err := NewPodConfigStore(cp2)
	if err != nil {
		t.Fatalf("NewPodConfigStore() reopen error: %v", err)
	}
	defer store2.Close()

	retrieved, found := store2.GetDeviceConfig("pod-1", "eth0")
	if !found {
		t.Fatalf("GetDeviceConfig() after reopen: not found")
	}
	if diff := cmp.Diff(config, retrieved); diff != "" {
		t.Errorf("GetDeviceConfig() after reopen mismatch (-want +got):\n%s", diff)
	}

	podConfig, found := store2.GetPodConfig("pod-1")
	if !found {
		t.Fatalf("GetPodConfig() after reopen: not found")
	}
	if len(podConfig.DeviceConfigs) != 1 {
		t.Errorf("Expected 1 device config after reopen, got %d", len(podConfig.DeviceConfigs))
	}

	// Verify NetNS is NOT restored (in-memory only)
	if podConfig.NetNS != "" {
		t.Errorf("Expected NetNS to be empty after reopen, but got %s", podConfig.NetNS)
	}
}

// TestPodConfigStore_DeletePodCheckpoints verifies that DeletePod and
// DeleteClaim propagate to the checkpointer.
func TestPodConfigStore_DeletePodCheckpoints(t *testing.T) {
	store, cp := newTestStoreWithBolt(t)

	store.SetDeviceConfig("pod-1", "eth0", DeviceConfig{Claim: types.NamespacedName{Namespace: "ns", Name: "c1"}})
	store.SetDeviceConfig("pod-2", "eth0", DeviceConfig{Claim: types.NamespacedName{Namespace: "ns", Name: "c1"}})
	store.SetDeviceConfig("pod-3", "eth0", DeviceConfig{Claim: types.NamespacedName{Namespace: "ns", Name: "c2"}})

	store.DeletePod("pod-1")

	// Verify deleted from bolt.
	data, _ := cp.GetOrCreate()
	if _, ok := data["pod-1"]; ok {
		t.Error("pod-1 should have been deleted from checkpoint")
	}
	if _, ok := data["pod-2"]; !ok {
		t.Error("pod-2 should still be in checkpoint")
	}

	// DeleteClaim should propagate.
	store.DeleteClaim(types.NamespacedName{Namespace: "ns", Name: "c1"})
	data, _ = cp.GetOrCreate()
	if _, ok := data["pod-2"]; ok {
		t.Error("pod-2 should have been deleted from checkpoint via DeleteClaim")
	}
	if _, ok := data["pod-3"]; !ok {
		t.Error("pod-3 should still be in checkpoint")
	}
}

func TestPodConfigStore_ThreadSafetyWithBolt(t *testing.T) {
	store, _ := newTestStoreWithBolt(t)
	numGoroutines := 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			podUID := types.UID(fmt.Sprintf("pod-%d", i))
			deviceName := fmt.Sprintf("eth%d", i%2)
			config := DeviceConfig{
				NetworkInterfaceConfigInPod: apis.NetworkConfig{
					Interface: apis.InterfaceConfig{Name: fmt.Sprintf("dev-%d", i)},
				},
			}
			store.SetDeviceConfig(podUID, deviceName, config)
			retrieved, _ := store.GetDeviceConfig(podUID, deviceName)
			if diff := cmp.Diff(config, retrieved); diff != "" {
				t.Errorf("goroutine %d: Get() mismatch (-want +got):\n%s", i, diff)
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

func TestBoltCheckpointer_Errors(t *testing.T) {
	t.Run("creates missing parent directory", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "nested", "path", "test.db")

		cp, err := NewBoltCheckpointer(dbPath)
		if err != nil {
			t.Fatalf("NewBoltCheckpointer() error: %v", err)
		}
		defer cp.Close()

		if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
			t.Fatalf("expected parent directory to exist: %v", err)
		}
	})

	t.Run("invalid db path", func(t *testing.T) {
		tempDir := t.TempDir()
		invalidDbPath := filepath.Join(tempDir, "is_a_dir")
		if err := os.Mkdir(invalidDbPath, 0755); err != nil {
			t.Fatalf("failed to mkdir: %v", err)
		}
		_, err := NewBoltCheckpointer(invalidDbPath)
		if err == nil {
			t.Fatal("expected error when opening a directory as bolt db")
		}
	})

	t.Run("corrupted JSON data fails GetOrCreate", func(t *testing.T) {
		cp := newTestBoltCheckpointer(t)

		// Inject invalid JSON directly into the bolt bucket.
		err := cp.db.Update(func(tx *bolt.Tx) error {
			root := tx.Bucket(podConfigsBucket)
			podBucket, err := root.CreateBucketIfNotExists([]byte("pod-corrupt"))
			if err != nil {
				return err
			}
			devBucket, err := podBucket.CreateBucketIfNotExists(deviceConfigsKey)
			if err != nil {
				return err
			}
			return devBucket.Put([]byte("net1"), []byte("{invalid-json"))
		})
		if err != nil {
			t.Fatalf("failed to insert invalid json: %v", err)
		}

		_, err = cp.GetOrCreate()
		if err == nil {
			t.Fatal("GetOrCreate() should fail on corrupted data")
		}
	})

	t.Run("missing root bucket", func(t *testing.T) {
		cp := newTestBoltCheckpointer(t)

		// Delete the root bucket.
		if err := cp.db.Update(func(tx *bolt.Tx) error {
			return tx.DeleteBucket(podConfigsBucket)
		}); err != nil {
			t.Fatalf("failed to delete bucket: %v", err)
		}

		// Store should fail.
		if err := cp.Store("pod-1", "eth0", DeviceConfig{}); err == nil {
			t.Error("expected error from Store with missing root bucket")
		}

		// GetOrCreate should return empty, not error.
		data, err := cp.GetOrCreate()
		if err != nil {
			t.Fatalf("GetOrCreate() error: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("expected empty, got %d", len(data))
		}

		// DeletePod should not error.
		if err := cp.DeletePod("pod-1"); err != nil {
			t.Errorf("DeletePod() error: %v", err)
		}
	})
}

// TestPodConfigStore_NoCheckpointer verifies that PodConfigStore works
// correctly without a checkpointer (pure in-memory).
func TestPodConfigStore_NoCheckpointer(t *testing.T) {
	store, err := NewPodConfigStore(nil)
	if err != nil {
		t.Fatalf("NewPodConfigStore(nil) error: %v", err)
	}

	store.SetDeviceConfig("pod-1", "eth0", DeviceConfig{
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	})

	config, found := store.GetDeviceConfig("pod-1", "eth0")
	if !found {
		t.Fatal("expected to find config")
	}
	if config.NetworkInterfaceConfigInPod.Interface.Name != "eth0" {
		t.Errorf("unexpected name: %s", config.NetworkInterfaceConfigInPod.Interface.Name)
	}

	store.DeletePod("pod-1")
	if _, found := store.GetDeviceConfig("pod-1", "eth0"); found {
		t.Error("expected not found after delete")
	}

	if err := store.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}
