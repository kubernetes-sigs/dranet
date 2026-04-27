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
	"time"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/inventory"

	"github.com/containerd/nri/pkg/stub"
	"sigs.k8s.io/dranet/internal/nlwrap"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/utils/clock"
)

const (
	kubeletPluginPath = "/var/lib/kubelet/plugins"
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
	RequestRescan()
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

// WithDBPath sets the path for the persistent pod config database.
// If not set, an in-memory store is used.
func WithDBPath(path string) Option {
	return func(o *NetworkDriver) {
		o.dbPath = path
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
	dbPath         string // path for persistent bbolt database; empty means in-memory

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
		clock:          clock.RealClock{},
	}

	for _, o := range opts {
		o(plugin)
	}

	// Initialize the pod config store with optional bbolt checkpoint backend.
	var checkpointer Checkpointer
	if plugin.dbPath != "" {
		var err error
		checkpointer, err = newBoltCheckpointer(plugin.dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open pod config database at %s: %v", plugin.dbPath, err)
		}
	}
	store, err := NewPodConfigStore(checkpointer)
	if err != nil {
		if checkpointer != nil {
			checkpointer.Close()
		}
		return nil, fmt.Errorf("failed to initialize pod config store: %v", err)
	}
	plugin.podConfigStore = store

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

// Stop handles the graceful termination of the Network Driver by coordinating
// the shutdown of its DRA and NRI plugin components.
//
// The shutdown follows a specific sequence to prevent workload pods from starting
// without their required devices during driver restarts or upgrades:
//
//  1. First, it shuts down the DRA plugin. This stops the driver from accepting
//     new resource claims and from preparing new pods.
//  2. Next, it enters a wait-loop to ensure that all pods currently in the
//     process of being prepared have a chance to hit their NRI hooks and finish
//     initialization.
//     - It tracks the most recent NRI activity for each prepared pod.
//     - It waits for a grace period (e.g., 10s) after the last activity for any
//     pod, ensuring subsequent containers in the same pod (like sidecars or
//     main app containers) also have a chance to be processed.
//     - The loop has a fallback timeout (e.g., 5m) to prevent the driver from
//     hanging indefinitely. However, in practice, the Kubernetes
//     terminationGracePeriodSeconds of the driver's own Pod (defaulting to
//     30s) will likely kill the driver before this fallback timeout is
//     reached.
//  3. Finally, it cancels the top-level context and stops the NRI plugin stub.
func (np *NetworkDriver) Stop(ctxCancel context.CancelFunc) {
	klog.Info("Stopping driver...")

	// Step 1: Halt the DRA plugin.
	// This stops the driver from handling new NodePrepareResources requests,
	// stabilizing the set of pods that require NRI processing.
	np.draPlugin.Stop()

	// Step 2: Wait for prepared pods to finish NRI initialization.
	gracePeriod := 10 * time.Second
	pollInterval := 5 * time.Second
	fallbackTimeout := 5 * time.Minute
	ticker := np.clock.NewTicker(pollInterval)
	defer ticker.Stop()

	// defaultPreviousActivity is used as a baseline for pods that have been
	// prepared but haven't recorded any NRI activity yet.
	defaultPreviousActivity := np.clock.Now()

	for {
		done := func() bool {
			// Check if we've exceeded the absolute maximum time we're willing to wait.
			if np.clock.Since(defaultPreviousActivity) >= fallbackTimeout {
				klog.Warningf("Fallback timeout of %v reached. Proceeding with shutdown despite pending pods.", fallbackTimeout)
				return true
			}

			// Get the current set of prepared pods and their latest NRI activity timestamps.
			activities := np.podConfigStore.GetPodNRIActivities()
			pendingCount := len(activities)
			if pendingCount == 0 {
				klog.Info("No pods with allocated devices found on this node. Proceeding with shutdown.")
				return true
			}

			// waitingForGrace tracks pods that are still considered "active" in the NRI phase.
			// A pod is considered active if:
			//  1. It recently triggered an NRI hook (within the gracePeriod).
			//  2. It was prepared but hasn't triggered an NRI hook yet (using
			//     defaultPreviousActivity).
			// We wait for this grace period after each hook to ensure
			// subsequent containers in the same pod (like sidecars or the main
			// app container) also have a chance to be processed before we shut
			// down the NRI plugin.
			waitingForGrace := 0
			for _, previousActivity := range activities {
				if previousActivity.IsZero() {
					previousActivity = defaultPreviousActivity
				}
				if np.clock.Since(previousActivity) < gracePeriod {
					waitingForGrace++
				}
			}

			// If no pods have had recent activity (or are still waiting for their first hook),
			// we assume all in-flight pod initializations are complete.
			if waitingForGrace == 0 {
				klog.Info("All prepared pods have passed the grace period. Proceeding with shutdown.")
				return true
			}

			klog.Infof("Waiting for %d prepared pods to finish NRI initialization: %d still in grace period...", pendingCount, waitingForGrace)
			return false
		}()

		if done {
			break
		}

		<-ticker.C()
	}

	// Step 3: Cancel context and stop NRI Plugin.
	// Canceling the top-level context should be sufficient to stop the nriPlugin
	// background tasks, but we also explicitly close it below.
	ctxCancel()

	np.nriPlugin.Stop()

	// Close the pod config store.
	if err := np.podConfigStore.Close(); err != nil {
		klog.Errorf("Failed to close pod config database: %v", err)
	}

	klog.Info("Driver stopped.")
}
