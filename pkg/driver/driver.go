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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"cel.dev/cel-go/cel"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/inventory"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"sigs.k8s.io/dranet/internal/nlwrap"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/utils/clock"
)

const (
	// maxAttempts indicates the number of times the driver will try to recover itself before failing
	maxAttempts = 5

	nriInitialBackoff = time.Second
	nriMaxBackoff     = 16 * time.Second
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
	GetDevice(deviceName string) (resourceapi.Device, bool)
	GetNetInterfaceName(string) (string, error)
	IsIBOnlyDevice(deviceName string) bool
	GetRDMADeviceName(deviceName string) (string, error)
	GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool)
	RequestRescan()
	GetProfileConfig(deviceName string, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error)
	ReleaseProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error
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

// WithKubeletRootDir sets the kubelet data directory (its --root-dir). The
// driver's registration socket lives under <dir>/plugins_registry and its
// dra.sock under <dir>/plugins. Set this when the kubelet runs with a
// non-default --root-dir.
func WithKubeletRootDir(dir string) Option {
	return func(o *NetworkDriver) {
		o.kubeletRootDir = dir
	}
}

type NetworkDriver struct {
	draPlugin     pluginHelper
	draStatus     atomic.Pointer[pluginHelper]
	driverName    string
	eventRecorder record.EventRecorder
	nodeName      string
	nriPlugin     atomic.Pointer[stub.Stub]
	kubeClient    kubernetes.Interface

	// contains the host interfaces
	netdb      inventoryDB
	celProgram cel.Program

	// Cache the rdma shared mode state
	rdmaSharedMode bool
	podConfigStore *PodConfigStore

	// publishMu serializes ResourceSlice publication and guards lastLive and
	// hasLive so UnprepareResourceClaims can synchronously drop deleted claim
	// snapshots without waiting on an inventory scan or racing an in-flight
	// PublishResources pass.
	publishMu sync.Mutex
	lastLive  []resourceapi.Device
	hasLive   bool

	// kubeletRootDir is the kubelet data directory (its --root-dir). Set when the
	// kubelet runs with a non-default --root-dir.
	kubeletRootDir string

	clock clock.WithTicker // Injectable clock for testing

	draStopped   atomic.Bool
	nriConnected atomic.Bool
	nriSession   atomic.Uint64
	lastNRIError atomic.Pointer[error]
}

type Option func(*NetworkDriver)

// nriSession wraps NetworkDriver for a single NRI stub connection so
// Synchronize completion is scoped to that connection attempt without
// polluting NetworkDriver's NRI hook handlers.
type nriSession struct {
	*NetworkDriver
	onSynced func()
}

func (s *nriSession) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	updates, err := s.NetworkDriver.Synchronize(ctx, pods, containers)
	if err == nil && s.onSynced != nil {
		s.onSynced()
	}
	return updates, err
}

type nriStubFactory func(onSynced, onClose func()) (stub.Stub, error)

// New constructs a NetworkDriver without starting its NRI, DRA, or inventory
// loops so callers can wire health checks (CheckDRA, CheckNRI) before Start.
func New(driverName string, kubeClient kubernetes.Interface, nodeName string, podConfigStore *PodConfigStore, opts ...Option) *NetworkDriver {
	registerMetrics()

	rdmaNetnsMode, err := nlwrap.RdmaSystemGetNetnsMode()
	if err != nil {
		klog.Infof("failed to determine the RDMA subsystem's network namespace mode, assume shared mode: %v", err)
		rdmaNetnsMode = apis.RdmaNetnsModeShared
	} else {
		klog.Infof("RDMA subsystem in mode: %s", rdmaNetnsMode)
	}

	var eventRecorder record.EventRecorder
	if kubeClient != nil {
		eventBroadcaster := record.NewBroadcaster()
		eventBroadcaster.StartStructuredLogging(0)
		eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
		eventRecorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: driverName, Host: nodeName})
	}

	plugin := &NetworkDriver{
		driverName:     driverName,
		nodeName:       nodeName,
		kubeClient:     kubeClient,
		rdmaSharedMode: rdmaNetnsMode == apis.RdmaNetnsModeShared,
		clock:          clock.RealClock{},
		eventRecorder:  eventRecorder,
		podConfigStore: podConfigStore,
	}

	for _, o := range opts {
		o(plugin)
	}

	return plugin
}

// Start constructs and starts a NetworkDriver.
func Start(ctx context.Context, driverName string, kubeClient kubernetes.Interface, nodeName string, podConfigStore *PodConfigStore, opts ...Option) (*NetworkDriver, error) {
	plugin := New(driverName, kubeClient, nodeName, podConfigStore, opts...)
	if err := plugin.Start(ctx); err != nil {
		return nil, err
	}
	return plugin, nil
}

// Start connects and synchronizes the NRI plugin, registers the DRA kubelet
// plugin, and starts the inventory and ResourceSlice publication loops.
func (np *NetworkDriver) Start(ctx context.Context) error {
	if np.netdb == nil {
		np.netdb = inventory.New()
	}

	newStub := func(onSynced, onClose func()) (stub.Stub, error) {
		return np.newNRIStub(onSynced, onClose)
	}

	startDRA := func(ctx context.Context) (pluginHelper, error) {
		driverPluginPath := filepath.Join(np.kubeletRootDir, "plugins", np.driverName)
		if err := os.MkdirAll(driverPluginPath, 0750); err != nil {
			return nil, fmt.Errorf("failed to create plugin path %s: %v", driverPluginPath, err)
		}

		// Derive the registration and plugin data directories from the kubelet root
		// dir so they are correct when the kubelet uses a non-default --root-dir. At
		// the default this matches the kubeletplugin defaults, so existing deployments
		// are unaffected.
		kubeletOpts := []kubeletplugin.Option{
			kubeletplugin.DriverName(np.driverName),
			kubeletplugin.NodeName(np.nodeName),
			kubeletplugin.KubeClient(np.kubeClient),
			kubeletplugin.RegistrarDirectoryPath(filepath.Join(np.kubeletRootDir, "plugins_registry")),
			kubeletplugin.PluginDataDirectoryPath(driverPluginPath),
			kubeletplugin.HealthService(false),
		}
		return kubeletplugin.Start(ctx, np, kubeletOpts...)
	}

	return np.start(ctx, newStub, startDRA)
}

// newNRIStub creates a fresh NRI stub per connection attempt: in
// containerd/nri v0.12.5, stub.Start leaves a closed conn attached if
// RegisterPlugin fails before stub.started is set, which makes retrying Start
// on the same stub fail permanently.
func (np *NetworkDriver) newNRIStub(onSynced, onClose func(), extraOpts ...stub.Option) (stub.Stub, error) {
	nriOpts := []stub.Option{
		stub.WithPluginName(np.driverName),
		stub.WithPluginIdx("00"),
		// https://github.com/containerd/nri/pull/329
		// Fail the pod or container request if the plugin times out or
		// loses its connection, and keep the plugin registered on timeout.
		// Otherwise the runtime proceeds without the plugin and the pod
		// starts without the network interfaces it requested.
		stub.WithFailurePolicy(&api.FailurePolicy{
			FailRequest:   true,
			KeepConnected: true,
		}),
		// https://github.com/containerd/nri/pull/173
		// Otherwise it silently exits the program
		stub.WithOnClose(onClose),
	}
	nriOpts = append(nriOpts, extraOpts...)
	return stub.New(&nriSession{NetworkDriver: np, onSynced: onSynced}, nriOpts...)
}

func (np *NetworkDriver) start(ctx context.Context, newNRIStub nriStubFactory, startDRA func(context.Context) (pluginHelper, error)) error {
	// Connect and synchronize NRI before starting the DRA kubelet plugin so
	// kubelet cannot prepare claims before the runtime has dranet registered.
	nriStub, nriDone, err := np.connectNRI(ctx, newNRIStub, false)
	if err != nil {
		return fmt.Errorf("start NRI plugin: %w", err)
	}

	d, err := startDRA(ctx)
	if err != nil {
		np.stopNRIPlugin()
		<-nriDone
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	np.setDRAPlugin(d)

	err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
		status := np.draPlugin.RegistrationStatus()
		if status == nil {
			return false, nil
		}
		return status.PluginRegistered, nil
	})
	if err != nil {
		np.draStopped.Store(true)
		np.draPlugin.Stop()
		np.stopNRIPlugin()
		<-nriDone
		return err
	}

	go func() {
		if err := np.runNRI(ctx, nriStub, nriDone, newNRIStub); err != nil {
			klog.Fatalf("NRI plugin failed to reconnect: %v", err)
		}
	}()

	go func() {
		for i := 0; i < maxAttempts; i++ {
			if err := np.netdb.Run(ctx); err != nil {
				klog.Infof("Network Device DB failed with error %v", err)
			}
			select {
			case <-ctx.Done():
				return
			default:
				netdbReconnectAttemptsTotal.Inc()
				klog.Infof("Restarting Network Device DB %d out of %d", i+1, maxAttempts)
			}
		}
		klog.Fatalf("Network Device DB failed for %d times to be restarted", maxAttempts)
	}()

	// publish available resources
	go np.PublishResources(ctx)

	return nil
}

func (np *NetworkDriver) setDRAPlugin(d pluginHelper) {
	np.draPlugin = d
	np.draStatus.Store(&d)
}

func (np *NetworkDriver) setNRIPlugin(s stub.Stub) {
	np.nriPlugin.Store(&s)
}

func (np *NetworkDriver) stopNRIPlugin() {
	np.nriConnected.Store(false)
	if p := np.nriPlugin.Load(); p != nil && *p != nil {
		(*p).Stop()
	}
}

func (np *NetworkDriver) onNRIClose(session uint64) {
	nriPluginDisconnectsTotal.Inc()
	klog.Infof("%s NRI plugin closed", np.driverName)
	if np.nriSession.Load() == session {
		np.nriConnected.Store(false)
		closeErr := errors.New("connection closed")
		np.lastNRIError.Store(&closeErr)
	}
}

func (np *NetworkDriver) startNRI(ctx context.Context, newNRIStub nriStubFactory) (stub.Stub, <-chan struct{}, error) {
	synced := make(chan struct{})
	markSynced := sync.OnceFunc(func() {
		close(synced)
	})

	var closed atomic.Bool
	session := np.nriSession.Add(1)
	s, err := newNRIStub(markSynced, func() {
		closed.Store(true)
		np.onNRIClose(session)
	})
	if err != nil {
		return nil, nil, err
	}
	if err := s.Start(ctx); err != nil {
		s.Stop()
		return nil, nil, err
	}

	done := make(chan struct{})
	go func() {
		s.Wait()
		close(done)
	}()

	// stub.Start returns after Configure, before the runtime invokes
	// Synchronize and adds the plugin to its active list.
	select {
	case <-synced:
		np.setNRIPlugin(s)
		np.lastNRIError.Store(nil)
		np.nriConnected.Store(true)
		if closed.Load() {
			np.nriConnected.Store(false)
			closeErr := errors.New("connection closed")
			np.lastNRIError.Store(&closeErr)
			s.Stop()
			<-done
			return nil, nil, fmt.Errorf("NRI plugin connection closed right after synchronize")
		}
		return s, done, nil
	case <-done:
		s.Stop()
		return nil, nil, fmt.Errorf("NRI plugin connection closed before synchronize completed")
	case <-ctx.Done():
		s.Stop()
		<-done
		return nil, nil, ctx.Err()
	}
}

func (np *NetworkDriver) connectNRI(ctx context.Context, newNRIStub nriStubFactory, isReconnect bool) (stub.Stub, <-chan struct{}, error) {
	backoff := nriInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if isReconnect || attempt > 1 {
			nriPluginReconnectAttemptsTotal.Inc()
			klog.Infof("Connecting NRI plugin attempt %d of %d", attempt, maxAttempts)
		}

		s, done, err := np.startNRI(ctx, newNRIStub)
		if err == nil {
			return s, done, nil
		}
		lastErr = err
		np.lastNRIError.Store(&lastErr)
		klog.Infof("NRI plugin connect attempt %d of %d failed: %v", attempt, maxAttempts, lastErr)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if attempt == maxAttempts {
			break
		}

		timer := np.clock.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C():
		}
		backoff = min(backoff*2, nriMaxBackoff)
	}
	return nil, nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

func (np *NetworkDriver) runNRI(ctx context.Context, current stub.Stub, done <-chan struct{}, newNRIStub nriStubFactory) error {
	for {
		select {
		case <-done:
		case <-ctx.Done():
			np.nriConnected.Store(false)
			current.Stop()
			<-done
			return nil
		}

		np.nriConnected.Store(false)
		current.Stop()

		var err error
		current, done, err = np.connectNRI(ctx, newNRIStub, true)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// CheckDRA reports whether the DRA kubelet plugin is started and registered
// with kubelet.
func (np *NetworkDriver) CheckDRA() error {
	if np == nil {
		return errors.New("DRA plugin is not started")
	}
	p := np.draStatus.Load()
	if p == nil || *p == nil {
		return errors.New("DRA plugin is not started")
	}
	if np.draStopped.Load() {
		return errors.New("DRA plugin is stopped")
	}
	status := (*p).RegistrationStatus()
	if status == nil {
		return errors.New("DRA plugin registration status is unknown")
	}
	if status.Error != "" {
		return fmt.Errorf("DRA plugin registration error: %s", status.Error)
	}
	if !status.PluginRegistered {
		return errors.New("DRA plugin is not registered with kubelet")
	}
	return nil
}

// CheckNRI reports whether the NRI plugin is connected and synchronized with
// the container runtime.
func (np *NetworkDriver) CheckNRI() error {
	if np == nil {
		return errors.New("NRI plugin is not started")
	}
	if np.nriConnected.Load() {
		return nil
	}
	if errPtr := np.lastNRIError.Load(); errPtr != nil && *errPtr != nil {
		return fmt.Errorf("NRI plugin is not connected: %w", *errPtr)
	}
	return errors.New("NRI plugin is not connected")
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
	np.draStopped.Store(true)
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

	np.stopNRIPlugin()

	klog.Info("Driver stopped.")
}
