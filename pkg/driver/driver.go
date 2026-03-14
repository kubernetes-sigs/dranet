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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/inventory"

	"github.com/containerd/nri/pkg/stub"
	"sigs.k8s.io/dranet/internal/nlwrap"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/utils/clock"
)

const (
	kubeletPluginRegistryPath = "/var/lib/kubelet/plugins_registry"
	kubeletPluginPath         = "/var/lib/kubelet/plugins"
)

const (
	// maxAttempts indicates the number of times the driver will try to recover itself before failing
	maxAttempts = 5
)

// This interface is our internal contract for the behavior we need from a *kubeletplugin.Helper, created specifically so we can fake it in tests.
type pluginHelper interface {
	PublishResources(context.Context, resourceslice.DriverResources) error
	Stop()
	RegistrationStatus() *registerapi.RegistrationStatus
}

// This interface is our internal contract for the behavior we need from a *inventory.DB, created specifically so we can fake it in tests.
type inventoryDB interface {
	Run(context.Context) error
	GetResources(context.Context) <-chan []resourceapi.Device
	GetNetInterfaceName(string) (string, error)
	IsIBOnlyDevice(deviceName string) bool
	GetRDMADeviceName(deviceName string) (string, error)
	GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool)
	AddPodNetNs(podKey string, netNs string)
	RemovePodNetNs(podKey string)
	GetPodNetNs(podKey string) (netNs string)
}

// WithFilter
func WithFilter(filter cel.Program) Option {
	return func(o *NetworkDriver) {
		o.celProgram = filter
	}
}

// WithInventory sets the inventory database for the driver.
func WithInventory(db inventoryDB) Option {
	return func(o *NetworkDriver) {
		o.netdb = db
	}
}

type NetworkDriver struct {
	driverName string
	nodeName   string
	kubeClient kubernetes.Interface
	draPlugin  pluginHelper
	nriPlugin  stub.Stub

	// contains the host interfaces
	netdb      inventoryDB
	celProgram cel.Program

	// Cache the rdma shared mode state
	rdmaSharedMode bool
	podConfigStore *PodConfigStore

	// allocatedPods tracks all pods for which this driver has successfully
	// prepared resource claims. It maps the Pod's UID to its most recent NRI
	// activity timestamp.
	//
	// An entry is added when a Pod's ResourceClaim is prepared, updated
	// when a container is created for that Pod, and removed only when the
	// ResourceClaim is unprepared (typically when the Pod is deleted).
	//
	// This map is used during Stop() to ensure that the driver doesn't shut
	// down while pods are still being initialized in the container runtime.
	allocatedPodsMu sync.Mutex
	allocatedPods   map[types.UID]time.Time

	clock clock.WithTicker // Injectable clock for testing
}

type Option func(*NetworkDriver)

func Start(ctx context.Context, driverName string, kubeClient kubernetes.Interface, nodeName string, opts ...Option) (*NetworkDriver, error) {
	registerMetrics()

	rdmaNetnsMode, err := nlwrap.RdmaSystemGetNetnsMode()
	if err != nil {
		klog.Infof("failed to determine the RDMA subsystem's network namespace mode, assume shared mode: %v", err)
		rdmaNetnsMode = apis.RdmaNetnsModeShared
	} else {
		klog.Infof("RDMA subsystem in mode: %s", rdmaNetnsMode)
	}

	plugin := &NetworkDriver{
		driverName:     driverName,
		nodeName:       nodeName,
		kubeClient:     kubeClient,
		rdmaSharedMode: rdmaNetnsMode == apis.RdmaNetnsModeShared,
		podConfigStore: NewPodConfigStore(),
		allocatedPods:  make(map[types.UID]time.Time),
		clock:          clock.RealClock{},
	}

	for _, o := range opts {
		o(plugin)
	}

	driverPluginPath := filepath.Join(kubeletPluginPath, driverName)
	err = os.MkdirAll(driverPluginPath, 0750)
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin path %s: %v", driverPluginPath, err)
	}

	kubeletOpts := []kubeletplugin.Option{
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.KubeClient(kubeClient),
	}
	d, err := kubeletplugin.Start(ctx, plugin, kubeletOpts...)
	if err != nil {
		return nil, fmt.Errorf("start kubelet plugin: %w", err)
	}
	plugin.draPlugin = d
	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
		status := plugin.draPlugin.RegistrationStatus()
		if status == nil {
			return false, nil
		}
		return status.PluginRegistered, nil
	})
	if err != nil {
		return nil, err
	}

	// register the NRI plugin
	nriOpts := []stub.Option{
		stub.WithPluginName(driverName),
		stub.WithPluginIdx("00"),
		// https://github.com/containerd/nri/pull/173
		// Otherwise it silently exits the program
		stub.WithOnClose(func() {
			klog.Infof("%s NRI plugin closed", driverName)
		}),
	}
	stub, err := stub.New(plugin, nriOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin stub: %v", err)
	}
	plugin.nriPlugin = stub

	go func() {
		for i := 0; i < maxAttempts; i++ {
			err = plugin.nriPlugin.Run(ctx)
			if err != nil {
				klog.Infof("NRI plugin failed with error %v", err)
			}
			select {
			case <-ctx.Done():
				return
			default:
				klog.Infof("Restarting NRI plugin %d out of %d", i, maxAttempts)
			}
		}
		klog.Fatalf("NRI plugin failed for %d times to be restarted", maxAttempts)
	}()

	// register the host network interfaces
	if plugin.netdb == nil {
		plugin.netdb = inventory.New()
	}
	go func() {
		for i := 0; i < maxAttempts; i++ {
			err = plugin.netdb.Run(ctx)
			if err != nil {
				klog.Infof("Network Device DB failed with error %v", err)
			}
			select {
			case <-ctx.Done():
				return
			default:
				klog.Infof("Restarting Network Device DB %d out of %d", i, maxAttempts)
			}
		}
		klog.Fatalf("Network Device DB failed for %d times to be restarted", maxAttempts)
	}()

	// publish available resources
	go plugin.PublishResources(ctx)

	return plugin, nil
}

// Stop handles the graceful termination of the Network Driver by coordinating the
// shutdown of its DRA and NRI plugin components.
//
// The shutdown follows a sequence to prevent workload pods from starting
// without their required devices during driver restarts or upgrades. It first
// shuts down the DRA plugin to stop accepting new resource claims. It then
// enters a wait-loop to ensure that all pods currently in the process of being
// prepared have a chance to hit their NRI hooks and finish initialization. The
// driver waits indefinitely for this activity, deferring the final termination
// timeout to the Kubernetes terminationGracePeriodSeconds (defaulting to 30s)
// configured for the pod. Once the wait criteria are met, it stops the NRI
// plugin stub and exits.
func (np *NetworkDriver) Stop(ctxCancel context.CancelFunc) {
	klog.Info("Stopping driver...")

	// Halt new PrepareResourceClaims requests to stabilize the set of pods
	// requiring NRI processing.
	np.draPlugin.Stop()

	// Wait for prepared pods to finish NRI initialization.
	gracePeriod := 10 * time.Second
	pollInterval := 5 * time.Second
	ticker := np.clock.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		done := func() bool {
			np.allocatedPodsMu.Lock()
			defer np.allocatedPodsMu.Unlock()

			pendingCount := len(np.allocatedPods)

			// waitingForActivity tracks pods that have been prepared but haven't
			// triggered any NRI hook activity yet (e.g. because images are still
			// being pulled).
			waitingForActivity := 0

			// waitingForGrace tracks pods that have recently triggered an NRI hook.
			// We wait for a grace period after each hook to ensure subsequent
			// containers in the same pod (like sidecars or the main app container)
			// also have a chance to be processed.
			waitingForGrace := 0

			for _, lastActivity := range np.allocatedPods {
				if lastActivity.IsZero() {
					waitingForActivity++
				} else if np.clock.Since(lastActivity) < gracePeriod {
					waitingForGrace++
				}
			}

			if pendingCount == 0 {
				klog.Info("No pods with allocated devices found on this node. Proceeding with shutdown.")
				return true
			}
			if waitingForActivity == 0 && waitingForGrace == 0 {
				klog.Info("All prepared pods have hit NRI hooks and passed the grace period. Proceeding with shutdown.")
				return true
			}

			klog.Infof("Waiting for %d prepared pods to finish NRI initialization: %d haven't hit NRI yet, %d still in grace period...",
				pendingCount, waitingForActivity, waitingForGrace)
			return false
		}()

		if done {
			break
		}

		<-ticker.C()
	}

	// Now that we've finished waiting for the pods, cancel the top level
	// context. That should also be sufficient to also stop nriPlugin,
	// but we can still explicitly try closing it.
	ctxCancel()

	// Stop NRI Plugin.
	np.nriPlugin.Stop()
	klog.Info("Driver stopped.")
}
