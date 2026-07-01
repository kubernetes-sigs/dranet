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
	"net/netip"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/pkg/apis"
)

// PodConfig holds all the device configurations for a Pod, and can be extended
// with fields that are not specific to a single device.
type PodConfig struct {
	// DeviceConfigs maps the allocated network device names to their respective
	// configurations.
	DeviceConfigs map[string]DeviceConfig

	// LastNRIActivity timestamp is updated whenever an NRI hook processes
	// a container for this Pod. Used to track pod initialization progress.
	LastNRIActivity time.Time

	// NetNS is the path to the Pod's network namespace as observed by the
	// container runtime.
	NetNS string
}

// DeviceConfig holds the set of configurations to be applied for a single
// network device allocated to a Pod. This includes network interface settings,
// routes for the Pod's network namespace, and RDMA configurations.
type DeviceConfig struct {
	Claim types.NamespacedName `json:"claim"`

	// NetworkInterfaceConfigInHost is the config of the network interface as
	// seen in the host's network namespace BEFORE it was moved to the pod's
	// network namespace.
	NetworkInterfaceConfigInHost apis.NetworkConfig `json:"networkInterfaceConfigInHost"`

	// NetworkInterfaceConfigInPod contains all network-related configurations
	// (interface, routes, ethtool, sysctl) to be applied for this device in the
	// Pod's namespace.
	NetworkInterfaceConfigInPod apis.NetworkConfig `json:"networkInterfaceConfigInPod"`

	// RDMADevice holds RDMA-specific configurations if the network device
	// has associated RDMA capabilities.
	RDMADevice RDMAConfig `json:"rdmaDevice,omitempty"`
}

// RDMAConfig contains parameters for setting up an RDMA device associated
// with a network interface.
type RDMAConfig struct {
	// LinkDev is the name of the RDMA link device (e.g., "mlx5_0").
	// Depending on the type of device (RoCE, IB) it may have a network device
	// associated. For IB-only devices there is no associated network interface.
	LinkDev string `json:"linkDev,omitempty"`

	// DevChars is a list of user-space RDMA character
	// devices (e.g., "/dev/infiniband/uverbs0", "/dev/infiniband/rdma_cm")
	// that should be made available to the Pod.
	DevChars []LinuxDevice `json:"devChars,omitempty"`
}

type LinuxDevice struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Major    int64  `json:"major"`
	Minor    int64  `json:"minor"`
	FileMode uint32 `json:"fileMode"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
}

// Checkpointer is the persistence interface for the PodConfigStore.
// It provides durable storage so that pod device configurations survive
// daemon restarts. Following the kubelet DRA checkpoint pattern
// (pkg/kubelet/cm/dra/state), the in-memory PodConfigStore is the source
// of truth and the Checkpointer is a write-through backend.
// Note: Pod-level metadata like NetNS is not persisted (rebuilt on restart
// via Synchronize() which queries the container runtime).
type Checkpointer interface {
	// GetOrCreate returns all persisted pod device configs, or an empty map
	// if the checkpoint does not yet exist. Used at startup to restore state.
	GetOrCreate() (map[types.UID]map[string]DeviceConfig, error)
	// Store persists the device config for a single pod/device pair.
	Store(podUID types.UID, deviceName string, config DeviceConfig) error
	// DeletePod removes all persisted state for the given pod.
	DeletePod(podUID types.UID) error
	// Close releases any resources held by the checkpointer.
	Close() error
}

// PodConfigStore provides a thread-safe, centralized store for all network
// device configurations across multiple Pods. It is indexed by the Pod's UID.
// All reads are served from memory. If a Checkpointer is provided, mutations
// are written through to durable storage.
type PodConfigStore struct {
	mu           sync.RWMutex
	configs      map[types.UID]PodConfig
	checkpointer Checkpointer // nil when no persistence is configured
	allocatedIPs map[string]types.UID
}

// NewPodConfigStore creates a new PodConfigStore. If a Checkpointer is
// provided, existing device configs are loaded from the checkpoint into memory.
// Pod-level state is not persisted; NetNS is rebuilt through Synchronize() on
// driver startup, while LastNRIActivity resets to its zero value.
func NewPodConfigStore(checkpointer Checkpointer) (*PodConfigStore, error) {
	s := &PodConfigStore{
		configs:      make(map[types.UID]PodConfig),
		checkpointer: checkpointer,
		allocatedIPs: make(map[string]types.UID),
	}

	if checkpointer != nil {
		saved, err := checkpointer.GetOrCreate()
		if err != nil {
			return nil, err
		}
		for podUID, devices := range saved {
			klog.Infof("PodConfigStore: loaded checkpoint for pod %s (%d devices)", podUID, len(devices))
			s.configs[podUID] = PodConfig{
				DeviceConfigs: devices,
			}
			for _, devCfg := range devices {
				for _, addrStr := range devCfg.NetworkInterfaceConfigInPod.Interface.Addresses {
					s.allocatedIPs[addrStr] = podUID
				}
			}
		}
	}

	return s, nil
}

// Close closes the underlying checkpointer, if any.
func (s *PodConfigStore) Close() error {
	if s.checkpointer != nil {
		return s.checkpointer.Close()
	}
	return nil
}

// UpdateLastNRIActivity updates the LastNRIActivity timestamp for a given Pod UID.
// If the PodConfig doesn't exist, it does nothing.
func (s *PodConfigStore) UpdateLastNRIActivity(podUID types.UID, timestamp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if podConfig, ok := s.configs[podUID]; ok {
		podConfig.LastNRIActivity = timestamp
		s.configs[podUID] = podConfig
	}
}

// GetPodNRIActivities returns a map of Pod UIDs to their last NRI activity timestamp.
func (s *PodConfigStore) GetPodNRIActivities() map[types.UID]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	activities := make(map[types.UID]time.Time, len(s.configs))
	for uid, config := range s.configs {
		activities[uid] = config.LastNRIActivity
	}
	return activities
}

// SetDeviceConfig stores the configuration for a specific device under a given Pod UID.
// If a configuration for the Pod UID or device name already exists, it will be overwritten.
// The write is persisted through the checkpointer if one is configured.
// Persistence is attempted before updating in-memory state to ensure RAM and
// disk don't diverge if the checkpoint write fails.
func (s *PodConfigStore) SetDeviceConfig(podUID types.UID, deviceName string, config DeviceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.checkpointer != nil {
		if err := s.checkpointer.Store(podUID, deviceName, config); err != nil {
			klog.Errorf("failed to checkpoint device config for pod %s device %s: %v", podUID, deviceName, err)
			return err
		}
	}

	podConfig, ok := s.configs[podUID]
	if !ok {
		podConfig = PodConfig{
			DeviceConfigs: make(map[string]DeviceConfig),
		}
		s.configs[podUID] = podConfig
	} else {
		// remove old IP allocations for this device
		if oldConfig, exists := podConfig.DeviceConfigs[deviceName]; exists {
			for _, addrStr := range oldConfig.NetworkInterfaceConfigInPod.Interface.Addresses {
				delete(s.allocatedIPs, addrStr)
			}
		}
	}
	podConfig.DeviceConfigs[deviceName] = config

	for _, addrStr := range config.NetworkInterfaceConfigInPod.Interface.Addresses {
		s.allocatedIPs[addrStr] = podUID
	}
	return nil
}

// GetDeviceConfig retrieves the configuration for a specific device under a given Pod UID.
// It returns the Config and true if found, otherwise an empty Config and false.
func (s *PodConfigStore) GetDeviceConfig(podUID types.UID, deviceName string) (DeviceConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if podConfig, ok := s.configs[podUID]; ok {
		config, found := podConfig.DeviceConfigs[deviceName]
		return config, found
	}
	return DeviceConfig{}, false
}

// DeletePod removes all configurations and allocated IPs associated with a given
// Pod UID. Checkpoint deletion is attempted first, but the in-memory entry is
// removed regardless of checkpoint outcome.
func (s *PodConfigStore) DeletePod(podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.checkpointer != nil {
		if err := s.checkpointer.DeletePod(podUID); err != nil {
			klog.Errorf("failed to delete checkpoint for pod %s: %v", podUID, err)
		}
	}
	if podConfig, ok := s.configs[podUID]; ok {
		for _, devCfg := range podConfig.DeviceConfigs {
			for _, addrStr := range devCfg.NetworkInterfaceConfigInPod.Interface.Addresses {
				delete(s.allocatedIPs, addrStr)
			}
		}
	}
	delete(s.configs, podUID)
}

// ListPods returns the UIDs of all pods in the store.
func (s *PodConfigStore) ListPods() []types.UID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uids := make([]types.UID, 0, len(s.configs))
	for uid := range s.configs {
		uids = append(uids, uid)
	}
	return uids
}

// GetPodConfig retrieves all configurations for a given Pod UID.
// It is indexed by the Pod's UID.
func (s *PodConfigStore) GetPodConfig(podUID types.UID) (PodConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	podConfig, found := s.configs[podUID]
	if !found {
		return PodConfig{}, false
	}
	// Return a copy to prevent external modification of the internal map
	configsCopy := make(map[string]DeviceConfig, len(podConfig.DeviceConfigs))
	for k, v := range podConfig.DeviceConfigs {
		configsCopy[k] = v
	}
	return PodConfig{
		DeviceConfigs:   configsCopy,
		LastNRIActivity: podConfig.LastNRIActivity,
		NetNS:           podConfig.NetNS,
	}, true
}

// SetPodNetNs stores the Pod's network namespace path in the pod-level config.
// This is in-memory only; pod NetNS is rebuilt from the container runtime on
// driver restart via Synchronize().
func (s *PodConfigStore) SetPodNetNs(podUID types.UID, netns string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	podCfg, ok := s.configs[podUID]
	if !ok {
		klog.Errorf("SetPodNetNs: pod UID %s not found in store; skipping NetNS update", podUID)
		return
	}
	klog.V(3).Infof("SetPodNetNs: setting NetNS for pod %s to %q", podUID, netns)
	podCfg.NetNS = netns
	s.configs[podUID] = podCfg
}

// DeleteClaim removes all configurations and allocated IPs associated with a given
// claim and returns the list of Pod UIDs that were associated with it.
// Like DeletePod, checkpoint failures do not prevent in-memory cleanup.
// See DeletePod for rationale on this intentional asymmetry with SetDeviceConfig.
func (s *PodConfigStore) DeleteClaim(claim types.NamespacedName) []types.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	podsToDelete := []types.UID{}
	for uid, podConfig := range s.configs {
		for _, config := range podConfig.DeviceConfigs {
			if config.Claim == claim {
				podsToDelete = append(podsToDelete, uid)
				break // Found a match for this pod, no need to check other devices for the same pod
			}
		}
	}

	for _, uid := range podsToDelete {
		if s.checkpointer != nil {
			if err := s.checkpointer.DeletePod(uid); err != nil {
				klog.Errorf("failed to delete checkpoint for pod %s: %v", uid, err)
			}
		}
		if podConfig, ok := s.configs[uid]; ok {
			for _, devCfg := range podConfig.DeviceConfigs {
				for _, addrStr := range devCfg.NetworkInterfaceConfigInPod.Interface.Addresses {
					delete(s.allocatedIPs, addrStr)
				}
			}
		}
		delete(s.configs, uid)
	}
	return podsToDelete
}

// GetAllocatedIPs returns a copy of all currently allocated IP addresses as
// netip.Addr values. Creating the copy avoids lock contention on s.mu during
// candidate address generation loops.
func (s *PodConfigStore) GetAllocatedIPs() map[netip.Addr]types.UID {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ipsCopy := make(map[netip.Addr]types.UID, len(s.allocatedIPs))
	for ipStr, uid := range s.allocatedIPs {
		if prefix, err := netip.ParsePrefix(ipStr); err == nil {
			// For CIDR prefix format, retrieve the raw IP address.
			ipsCopy[prefix.Addr()] = uid
		} else if addr, err := netip.ParseAddr(ipStr); err == nil {
			// For raw IP address, parse and store directly.
			ipsCopy[addr] = uid
		}
	}
	return ipsCopy
}
