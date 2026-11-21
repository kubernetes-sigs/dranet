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
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/adaptation"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/dranet/pkg/apis"
)

// fakePluginHelper is a mock implementation of the pluginHelper interface for testing.
type fakePluginHelper struct {
	lastResources      atomic.Pointer[resourceslice.DriverResources]
	onPublish          func(resourceslice.DriverResources)
	publishErr         error
	publishCalled      chan struct{}
	statusMu           sync.RWMutex
	registrationStatus *registerapi.RegistrationStatus
	stopCalled         atomic.Bool
}

func newFakePluginHelper() *fakePluginHelper {
	return &fakePluginHelper{
		publishCalled: make(chan struct{}, 1),
		registrationStatus: &registerapi.RegistrationStatus{
			PluginRegistered: true,
		},
	}
}

func (m *fakePluginHelper) PublishResources(_ context.Context, res resourceslice.DriverResources) error {
	if m.onPublish != nil {
		m.onPublish(res)
	}
	m.lastResources.Store(&res)
	if m.publishCalled != nil {
		select {
		case m.publishCalled <- struct{}{}:
		default:
		}
	}
	return m.publishErr
}

func (m *fakePluginHelper) publishedDevices(node string) []resourcev1.Device {
	res := m.lastResources.Load()
	if res == nil {
		return nil
	}
	pool, ok := res.Pools[node]
	if !ok || len(pool.Slices) == 0 {
		return nil
	}
	return pool.Slices[0].Devices
}

func (m *fakePluginHelper) Stop() {
	m.stopCalled.Store(true)
}

func (m *fakePluginHelper) RegistrationStatus() *registerapi.RegistrationStatus {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	return m.registrationStatus
}

func (m *fakePluginHelper) setRegistrationStatus(status *registerapi.RegistrationStatus) {
	m.statusMu.Lock()
	m.registrationStatus = status
	m.statusMu.Unlock()
}

// fakeInventoryDB is a mock implementation of the inventoryDB interface for testing.
type fakeInventoryDB struct {
	resources                chan []resourcev1.Device
	rescanCalls              atomic.Int32
	RequestRescanFunc        func()
	GetDeviceFunc            func(deviceName string) (resourcev1.Device, bool)
	GetDeviceConfigFunc      func(deviceName string) (*apis.NetworkConfig, bool)
	GetNetInterfaceNameFunc  func(deviceName string) (string, error)
	IsIBOnlyDeviceFunc       func(deviceName string) bool
	GetRDMADeviceNameFunc    func(deviceName string) (string, error)
	GetProfileConfigFunc     func(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error)
	ReleaseProfileConfigFunc func(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error
	profileCalls             atomic.Int32
	releaseProfileCalls      atomic.Int32
}

func newFakeInventoryDB() *fakeInventoryDB {
	return &fakeInventoryDB{
		resources: make(chan []resourcev1.Device, 1),
	}
}

func (m *fakeInventoryDB) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (m *fakeInventoryDB) GetResources(_ context.Context) <-chan []resourcev1.Device {
	return m.resources
}

func (m *fakeInventoryDB) GetDevice(deviceName string) (resourcev1.Device, bool) {
	if m.GetDeviceFunc != nil {
		return m.GetDeviceFunc(deviceName)
	}
	return resourcev1.Device{}, false
}

func (m *fakeInventoryDB) GetNetInterfaceName(deviceName string) (string, error) {
	if m.GetNetInterfaceNameFunc != nil {
		return m.GetNetInterfaceNameFunc(deviceName)
	}
	return "", nil
}

func (m *fakeInventoryDB) IsIBOnlyDevice(deviceName string) bool {
	if m.IsIBOnlyDeviceFunc != nil {
		return m.IsIBOnlyDeviceFunc(deviceName)
	}
	return false
}

func (m *fakeInventoryDB) GetRDMADeviceName(deviceName string) (string, error) {
	if m.GetRDMADeviceNameFunc != nil {
		return m.GetRDMADeviceNameFunc(deviceName)
	}
	return "", nil
}

func (m *fakeInventoryDB) GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool) {
	if m.GetDeviceConfigFunc != nil {
		return m.GetDeviceConfigFunc(deviceName)
	}
	return nil, false
}

func (m *fakeInventoryDB) RequestRescan() {
	m.rescanCalls.Add(1)
	if m.RequestRescanFunc != nil {
		m.RequestRescanFunc()
	}
}

func (m *fakeInventoryDB) GetProfileConfig(deviceName string, claim *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	m.profileCalls.Add(1)
	if m.GetProfileConfigFunc != nil {
		return m.GetProfileConfigFunc(deviceName, claim, config)
	}
	return nil, nil
}

func (m *fakeInventoryDB) ReleaseProfileConfig(deviceName string, claimUID types.UID, config *apis.NetworkConfig) error {
	m.releaseProfileCalls.Add(1)
	if m.ReleaseProfileConfigFunc != nil {
		return m.ReleaseProfileConfigFunc(deviceName, claimUID, config)
	}
	return nil
}

// fakeNriStub is a fake implementation of stub.Stub for unit tests.
type fakeNriStub struct {
	stub.Stub

	mu          sync.Mutex
	startFn     func(context.Context) error
	onClose     func()
	done        chan struct{}
	stopCalled  atomic.Bool
	startCalled atomic.Int32
}

func (m *fakeNriStub) Start(ctx context.Context) error {
	m.startCalled.Add(1)
	if m.startFn != nil {
		if err := m.startFn(ctx); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.done = make(chan struct{})
	m.mu.Unlock()
	return nil
}

func (m *fakeNriStub) Wait() {
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (m *fakeNriStub) Stop() {
	m.stopCalled.Store(true)
	m.closeSession(false)
}

func (m *fakeNriStub) disconnect() {
	m.closeSession(true)
}

func (m *fakeNriStub) closeSession(notifyOnClose bool) {
	m.mu.Lock()
	done := m.done
	m.done = nil
	onClose := m.onClose
	m.mu.Unlock()
	if done != nil {
		close(done)
	}
	if notifyOnClose && onClose != nil {
		onClose()
	}
}

func (m *fakeNriStub) Run(_ context.Context) error {
	return nil
}

func TestStop(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	fakeDra := newFakePluginHelper()
	fakeNri := &fakeNriStub{}

	np := &NetworkDriver{
		podConfigStore: mustNewPodConfigStore(),
		clock:          fakeClock,
	}
	np.setDRAPlugin(fakeDra)
	np.setNRIPlugin(fakeNri)

	podUID1 := types.UID("pod-1")
	podUID2 := types.UID("pod-2")

	// Pod 1: Prepared but no NRI activity
	np.podConfigStore.SetDeviceConfig(podUID1, "random-dev-1", DeviceConfig{})

	// Pod 2: Prepared and has recent NRI activity
	np.podConfigStore.SetDeviceConfig(podUID2, "random-dev-1", DeviceConfig{})
	np.podConfigStore.UpdateLastNRIActivity(podUID2, fakeClock.Now())

	cancelCalled := false
	cancel := func() {
		cancelCalled = true
	}

	// Run Stop in a separate goroutine because it will block
	stopDone := make(chan struct{})
	go func() {
		np.Stop(cancel)
		close(stopDone)
	}()

	// 1. Initially, Stop should be waiting.
	select {
	case <-stopDone:
		t.Fatal("Stop() returned prematurely while pods were still in flight")
	case <-time.After(100 * time.Millisecond):
		// Success: still waiting
	}

	if !fakeDra.stopCalled.Load() {
		t.Errorf("draPlugin.Stop() should have been called immediately")
	}

	// 2. Advance clock slightly.
	fakeClock.Step(5 * time.Second)

	select {
	case <-stopDone:
		t.Fatal("Stop() returned while Pod 1 was still waiting for activity")
	case <-time.After(100 * time.Millisecond):
		// Success: still waiting
	}

	// 3. Advance clock again to satisfy grace period for both pods.
	fakeClock.Step(5 * time.Second)

	// 4. Now Stop should finish.
	select {
	case <-stopDone:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() timed out after all pods were stable")
	}

	if !cancelCalled {
		t.Errorf("cancel() was not called")
	}
	if !fakeNri.stopCalled.Load() {
		t.Errorf("nriPlugin.Stop() was not called")
	}
}

func TestStartOrdersNRIBeforeDRA(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events []string
	var eventsMu sync.Mutex
	recordEvent := func(ev string) {
		eventsMu.Lock()
		events = append(events, ev)
		eventsMu.Unlock()
	}

	syncGate := make(chan struct{})
	nriStarted := make(chan struct{})
	draStarted := make(chan struct{})

	np := &NetworkDriver{
		driverName:     "test.driver",
		nodeName:       "node-1",
		netdb:          newFakeInventoryDB(),
		podConfigStore: mustNewPodConfigStore(),
		clock:          testingclock.NewFakeClock(time.Now()),
	}
	newStub := func(onSynced, onClose func()) (stub.Stub, error) {
		session := &nriSession{NetworkDriver: np, onSynced: onSynced}
		return &fakeNriStub{
			onClose: onClose,
			startFn: func(ctx context.Context) error {
				recordEvent("nri-start")
				close(nriStarted)
				go func() {
					<-syncGate
					recordEvent("nri-sync")
					if _, err := session.Synchronize(ctx, nil, nil); err != nil {
						t.Errorf("Synchronize failed: %v", err)
					}
				}()
				return nil
			},
		}, nil
	}

	fakeDra := newFakePluginHelper()
	startErr := make(chan error, 1)
	go func() {
		startErr <- np.start(ctx, newStub, func(context.Context) (pluginHelper, error) {
			if !np.nriConnected.Load() {
				t.Error("DRA started before NRI was marked connected")
			}
			recordEvent("dra-start")
			close(draStarted)
			return fakeDra, nil
		})
	}()

	<-nriStarted

	// DRA must not start until NRI Synchronize completes.
	select {
	case <-draStarted:
		t.Fatal("DRA started before NRI Synchronize completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(syncGate)

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("start() failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for start() to finish")
	}

	eventsMu.Lock()
	got := append([]string(nil), events...)
	eventsMu.Unlock()
	want := []string{"nri-start", "nri-sync", "dra-start"}
	if len(got) != len(want) {
		t.Fatalf("startup events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("startup event[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestStartStopsNRIWhenDRAFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeNri := &fakeNriStub{}
	np := &NetworkDriver{
		driverName:     "test.driver",
		nodeName:       "node-1",
		netdb:          newFakeInventoryDB(),
		podConfigStore: mustNewPodConfigStore(),
		clock:          testingclock.NewFakeClock(time.Now()),
	}
	newStub := func(onSynced, onClose func()) (stub.Stub, error) {
		session := &nriSession{NetworkDriver: np, onSynced: onSynced}
		fakeNri.onClose = onClose
		fakeNri.startFn = func(ctx context.Context) error {
			go func() {
				_, _ = session.Synchronize(ctx, nil, nil)
			}()
			return nil
		}
		return fakeNri, nil
	}

	err := np.start(ctx, newStub, func(context.Context) (pluginHelper, error) {
		return nil, errors.New("kubelet socket unavailable")
	})
	if err == nil {
		t.Fatal("start() succeeded, want DRA startup error")
	}
	if !fakeNri.stopCalled.Load() {
		t.Error("expected nriPlugin.Stop() when DRA startup fails")
	}
	if np.nriConnected.Load() {
		t.Error("expected nriConnected=false when DRA startup fails")
	}
}

func TestRunNRIReconnectBackoffAndReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeClock := testingclock.NewFakeClock(time.Now())
	attemptCh := make(chan int, 20)

	// Outage 1: 3 failed reconnects, 4th succeeds (streak resets).
	// Outage 2: 3 failed reconnects, 4th succeeds (6 total failures > maxAttempts=5).
	// Outage 3: maxAttempts consecutive failures -> runNRI returns error.
	outcomes := []error{
		nil, // initial session
		errors.New("outage-1-err-1"),
		errors.New("outage-1-err-2"),
		errors.New("outage-1-err-3"),
		nil, // outage 1 recovered
		errors.New("outage-2-err-1"),
		errors.New("outage-2-err-2"),
		errors.New("outage-2-err-3"),
		nil, // outage 2 recovered
		errors.New("outage-3-err-1"),
		errors.New("outage-3-err-2"),
		errors.New("outage-3-err-3"),
		errors.New("outage-3-err-4"),
		errors.New("outage-3-err-5"),
	}

	np := &NetworkDriver{
		driverName:     "test.driver",
		podConfigStore: mustNewPodConfigStore(),
		clock:          fakeClock,
	}
	var idx atomic.Int32
	var activeStubMu sync.Mutex
	var activeStub *fakeNriStub
	newStub := func(onSynced, onClose func()) (stub.Stub, error) {
		session := &nriSession{NetworkDriver: np, onSynced: onSynced}
		s := &fakeNriStub{onClose: onClose}
		s.startFn = func(ctx context.Context) error {
			n := int(idx.Add(1)) - 1
			attemptCh <- n
			if n >= len(outcomes) {
				return errors.New("unexpected extra attempt")
			}
			if err := outcomes[n]; err != nil {
				return err
			}
			activeStubMu.Lock()
			activeStub = s
			activeStubMu.Unlock()
			go func() {
				_, _ = session.Synchronize(ctx, nil, nil)
			}()
			return nil
		}
		return s, nil
	}
	disconnectActive := func() {
		activeStubMu.Lock()
		s := activeStub
		activeStubMu.Unlock()
		s.disconnect()
	}

	// Establish initial synchronized session.
	current, done, err := np.connectNRI(ctx, newStub, false)
	if err != nil {
		t.Fatalf("initial connectNRI failed: %v", err)
	}
	<-attemptCh
	if !np.nriConnected.Load() {
		t.Fatal("expected nriConnected=true after initial connect")
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- np.runNRI(ctx, current, done, newStub)
	}()

	waitAttempt := func(want int) {
		t.Helper()
		select {
		case got := <-attemptCh:
			if got != want {
				t.Fatalf("attempt index = %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for attempt %d", want)
		}
	}
	waitWaiter := func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if fakeClock.HasWaiters() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("timed out waiting for backoff timer")
	}
	waitConnected := func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if np.nriConnected.Load() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("timed out waiting for nriConnected=true")
	}

	// Outage 1: verify exponential backoff 1s, 2s, 4s then recovery.
	disconnectActive()
	waitAttempt(1)
	waitWaiter()
	fakeClock.Step(1 * time.Second)
	waitAttempt(2)
	waitWaiter()
	fakeClock.Step(2 * time.Second)
	waitAttempt(3)
	waitWaiter()
	fakeClock.Step(4 * time.Second)
	waitAttempt(4)
	waitConnected()

	// Outage 2: streak must have reset so backoff restarts at 1s and succeeds.
	disconnectActive()
	waitAttempt(5)
	waitWaiter()
	fakeClock.Step(1 * time.Second)
	waitAttempt(6)
	waitWaiter()
	fakeClock.Step(2 * time.Second)
	waitAttempt(7)
	waitWaiter()
	fakeClock.Step(4 * time.Second)
	waitAttempt(8)
	waitConnected()

	// Outage 3: maxAttempts consecutive failures returns an error.
	disconnectActive()
	for i, d := range []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		waitAttempt(9 + i)
		waitWaiter()
		fakeClock.Step(d)
	}
	waitAttempt(13)

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("runNRI returned nil after maxAttempts consecutive failures, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runNRI to exit after maxAttempts failures")
	}
}

// unixSocketProxy forwards a stable Unix socket path to a swappable backend
// runtime socket and can drop all live connections to simulate containerd exit.
type unixSocketProxy struct {
	ln     net.Listener
	mu     sync.Mutex
	target string
	conns  map[net.Conn]struct{}
	closed bool
}

func newUnixSocketProxy(t *testing.T, listenPath string) *unixSocketProxy {
	t.Helper()
	ln, err := net.Listen("unix", listenPath)
	if err != nil {
		t.Fatalf("listen %s: %v", listenPath, err)
	}
	p := &unixSocketProxy{
		ln:    ln,
		conns: make(map[net.Conn]struct{}),
	}
	go p.acceptLoop()
	t.Cleanup(p.close)
	return p
}

func (p *unixSocketProxy) setTarget(target string) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

func (p *unixSocketProxy) dropConnections() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (p *unixSocketProxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.ln.Close()
	p.dropConnections()
}

func (p *unixSocketProxy) track(c net.Conn) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

func (p *unixSocketProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

func (p *unixSocketProxy) acceptLoop() {
	for {
		clientConn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		target := p.target
		p.mu.Unlock()
		if target == "" {
			_ = clientConn.Close()
			continue
		}
		backendConn, err := net.Dial("unix", target)
		if err != nil {
			_ = clientConn.Close()
			continue
		}
		p.track(clientConn)
		p.track(backendConn)
		go func() {
			_, _ = io.Copy(backendConn, clientConn)
			p.untrack(clientConn)
			p.untrack(backendConn)
		}()
		go func() {
			_, _ = io.Copy(clientConn, backendConn)
			p.untrack(clientConn)
			p.untrack(backendConn)
		}()
	}
}

func startTestNRIRuntime(t *testing.T, sockPath, emptyDir string, syncPods []*api.PodSandbox) *adaptation.Adaptation {
	t.Helper()
	syncFn := func(ctx context.Context, cb adaptation.SyncCB) error {
		_, err := cb(ctx, syncPods, nil)
		return err
	}
	updateFn := func(context.Context, []*api.ContainerUpdate) ([]*api.ContainerUpdate, error) {
		return nil, nil
	}
	rt, err := adaptation.New("containerd", "2.0.0", syncFn, updateFn,
		adaptation.WithSocketPath(sockPath),
		adaptation.WithPluginPath(emptyDir),
		adaptation.WithPluginConfigPath(emptyDir),
	)
	if err != nil {
		t.Fatalf("adaptation.New(%s): %v", sockPath, err)
	}
	if err := rt.Start(); err != nil {
		t.Fatalf("rt.Start(%s): %v", sockPath, err)
	}
	rt.BlockPluginSync().Unblock()
	t.Cleanup(rt.Stop)
	return rt
}

func TestEndToEndNRISessionAndRuntimeRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Use /tmp so Unix socket paths stay under Linux's 108-byte sun_path limit.
	sockDir, err := os.MkdirTemp("/tmp", "dranet-e2e-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	emptyDir := filepath.Join(sockDir, "empty")
	if err := os.MkdirAll(emptyDir, 0750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	clientSock := filepath.Join(sockDir, "nri.sock")
	rt1Sock := filepath.Join(sockDir, "rt1.sock")
	rt2Sock := filepath.Join(sockDir, "rt2.sock")

	rt1 := startTestNRIRuntime(t, rt1Sock, emptyDir, nil)
	proxy := newUnixSocketProxy(t, clientSock)
	proxy.setTarget(rt1Sock)

	fakeDB := newFakeInventoryDB()
	fakeDB.IsIBOnlyDeviceFunc = func(string) bool { return true }
	fakeDB.GetRDMADeviceNameFunc = func(string) (string, error) { return "mlx5_0", nil }

	fakeClock := testingclock.NewFakeClock(time.Now())
	store := mustNewPodConfigStore()
	np := &NetworkDriver{
		driverName:     "dra.net",
		nodeName:       "node-1",
		kubeClient:     fake.NewSimpleClientset(),
		eventRecorder:  record.NewFakeRecorder(100),
		netdb:          fakeDB,
		rdmaSharedMode: true,
		podConfigStore: store,
		clock:          fakeClock,
	}

	newStub := func(onSynced, onClose func()) (stub.Stub, error) {
		return np.newNRIStub(onSynced, onClose, stub.WithSocketPath(clientSock))
	}

	fakeDra := newFakePluginHelper()
	var draSawNRIConnected bool
	if err := np.start(ctx, newStub, func(context.Context) (pluginHelper, error) {
		draSawNRIConnected = np.nriConnected.Load()
		return fakeDra, nil
	}); err != nil {
		t.Fatalf("np.start: %v", err)
	}
	if !draSawNRIConnected {
		t.Fatal("DRA started before real NRI stub synchronized")
	}
	if err := np.CheckDRA(); err != nil {
		t.Fatalf("CheckDRA() after start = %v, want nil", err)
	}
	if err := np.CheckNRI(); err != nil {
		t.Fatalf("CheckNRI() after start = %v, want nil", err)
	}

	newIBClaim := func(claimUID, claimName, podUID, podName string) *resourcev1.ResourceClaim {
		return &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID(claimUID), Namespace: "default", Name: claimName},
			Status: resourcev1.ResourceClaimStatus{
				ReservedFor: []resourcev1.ResourceClaimConsumerReference{
					{Resource: "pods", Name: podName, UID: types.UID(podUID)},
				},
				Allocation: &resourcev1.AllocationResult{
					Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{
							{Driver: np.driverName, Device: "ib0", Request: "req0"},
						},
					},
				},
			},
		}
	}

	// 1. Prepare claim for pod-1 and exercise promoted RunPodSandbox/CreateContainer hooks on runtime 1.
	claim1 := newIBClaim("claim-1-uid", "claim-1", "pod-1-uid", "pod-1")
	res, err := np.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim1})
	if err != nil || res[claim1.UID].Err != nil {
		t.Fatalf("PrepareResourceClaims(claim1) = (%v, %v), want success", res, err)
	}
	pod1 := &api.PodSandbox{
		Id:        "pod-1-sb",
		Uid:       "pod-1-uid",
		Name:      "pod-1",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/var/run/netns/pod1"}},
		},
	}
	if err := rt1.RunPodSandbox(ctx, &api.RunPodSandboxRequest{Pod: pod1}); err != nil {
		t.Fatalf("rt1.RunPodSandbox(pod-1): %v", err)
	}
	if _, err := rt1.CreateContainer(ctx, &api.CreateContainerRequest{
		Pod:       pod1,
		Container: &api.Container{Id: "ctr-1", Name: "app"},
	}); err != nil {
		t.Fatalf("rt1.CreateContainer(pod-1): %v", err)
	}

	// 2. Simulate containerd restart: drop all connections and stop runtime 1.
	proxy.setTarget("")
	proxy.dropConnections()
	rt1.Stop()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	waitFor("NRI disconnect and reconnect backoff timer", func() bool {
		return !np.nriConnected.Load() && fakeClock.HasWaiters()
	})
	if err := np.CheckDRA(); err != nil {
		t.Fatalf("CheckDRA() during NRI outage = %v, want nil", err)
	}
	if err := np.CheckNRI(); err == nil {
		t.Fatal("CheckNRI() during NRI outage = nil, want error")
	}

	// While NRI is disconnected, PrepareResourceClaims must reject new claims.
	claim2 := newIBClaim("claim-2-uid", "claim-2", "pod-2-uid", "pod-2")
	if _, err := np.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim2}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("PrepareResourceClaims while disconnected = %v, want not connected error", err)
	}

	// 3. Start runtime 2 with pod-1 live and verify reconnect + Synchronize + new pod lifecycle.
	pod1Reloaded := &api.PodSandbox{
		Id:        "pod-1-sb",
		Uid:       "pod-1-uid",
		Name:      "pod-1",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/var/run/netns/pod1-reloaded"}},
		},
	}
	rt2 := startTestNRIRuntime(t, rt2Sock, emptyDir, []*api.PodSandbox{pod1Reloaded})
	proxy.setTarget(rt2Sock)
	fakeClock.Step(nriInitialBackoff)

	waitFor("NRI reconnect and synchronize on runtime 2", func() bool {
		return np.nriConnected.Load()
	})
	if err := np.CheckNRI(); err != nil {
		t.Fatalf("CheckNRI() after reconnect = %v, want nil", err)
	}

	pod1Cfg, ok := store.GetPodConfig("pod-1-uid")
	if !ok || pod1Cfg.NetNS != "/var/run/netns/pod1-reloaded" {
		t.Fatalf("pod-1 config after Synchronize = %+v (ok=%v), want NetNS=/var/run/netns/pod1-reloaded", pod1Cfg, ok)
	}

	// PrepareResourceClaims and NRI hooks succeed on runtime 2 after reconnect.
	res, err = np.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim2})
	if err != nil || res[claim2.UID].Err != nil {
		t.Fatalf("PrepareResourceClaims(claim2) after reconnect = (%v, %v), want success", res, err)
	}
	pod2 := &api.PodSandbox{
		Id:        "pod-2-sb",
		Uid:       "pod-2-uid",
		Name:      "pod-2",
		Namespace: "default",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/var/run/netns/pod2"}},
		},
	}
	if err := rt2.RunPodSandbox(ctx, &api.RunPodSandboxRequest{Pod: pod2}); err != nil {
		t.Fatalf("rt2.RunPodSandbox(pod-2): %v", err)
	}
	if _, err := rt2.CreateContainer(ctx, &api.CreateContainerRequest{
		Pod:       pod2,
		Container: &api.Container{Id: "ctr-2", Name: "app"},
	}); err != nil {
		t.Fatalf("rt2.CreateContainer(pod-2): %v", err)
	}

	// Clean up prepared claims so Stop() does not wait for the grace period.
	if _, err := np.UnprepareResourceClaims(ctx, []kubeletplugin.NamespacedObject{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-1"}, UID: claim1.UID},
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "claim-2"}, UID: claim2.UID},
	}); err != nil {
		t.Fatalf("UnprepareResourceClaims: %v", err)
	}

	np.Stop(cancel)
	if np.nriConnected.Load() {
		t.Error("expected nriConnected=false after Stop()")
	}
	if err := np.CheckDRA(); err == nil {
		t.Error("CheckDRA() after Stop() = nil, want error")
	}
	if err := np.CheckNRI(); err == nil {
		t.Error("CheckNRI() after Stop() = nil, want error")
	}
}

func TestCheckDRAAndCheckNRI(t *testing.T) {
	var nilDriver *NetworkDriver
	if err := nilDriver.CheckDRA(); err == nil {
		t.Error("nil CheckDRA() = nil, want error")
	}
	if err := nilDriver.CheckNRI(); err == nil {
		t.Error("nil CheckNRI() = nil, want error")
	}

	fakeDra := newFakePluginHelper()
	np := &NetworkDriver{}
	np.setDRAPlugin(fakeDra)

	if err := np.CheckDRA(); err != nil {
		t.Errorf("CheckDRA() when registered = %v, want nil", err)
	}

	fakeDra.setRegistrationStatus(nil)
	if err := np.CheckDRA(); err == nil {
		t.Error("CheckDRA() with nil status = nil, want error")
	}

	fakeDra.setRegistrationStatus(&registerapi.RegistrationStatus{PluginRegistered: false})
	if err := np.CheckDRA(); err == nil {
		t.Error("CheckDRA() when unregistered = nil, want error")
	}

	fakeDra.setRegistrationStatus(&registerapi.RegistrationStatus{PluginRegistered: false, Error: "socket error"})
	if err := np.CheckDRA(); err == nil || !strings.Contains(err.Error(), "socket error") {
		t.Errorf("CheckDRA() with registration error = %v, want socket error", err)
	}

	fakeDra.setRegistrationStatus(&registerapi.RegistrationStatus{PluginRegistered: true})
	np.draStopped.Store(true)
	if err := np.CheckDRA(); err == nil {
		t.Error("CheckDRA() when stopped = nil, want error")
	}

	if err := np.CheckNRI(); err == nil {
		t.Error("CheckNRI() when disconnected = nil, want error")
	}

	lastErr := errors.New("dial unix /var/run/nri/nri.sock: connection refused")
	np.lastNRIError.Store(&lastErr)
	if err := np.CheckNRI(); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("CheckNRI() with last error = %v, want connection refused", err)
	}

	np.nriConnected.Store(true)
	if err := np.CheckNRI(); err != nil {
		t.Errorf("CheckNRI() when connected = %v, want nil", err)
	}
}
