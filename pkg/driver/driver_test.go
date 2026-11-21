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
	registrationStatus *registerapi.RegistrationStatus
	stopCalled         atomic.Bool
}

func newFakePluginHelper() *fakePluginHelper {
	return &fakePluginHelper{
		publishCalled: make(chan struct{}, 1),
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
	return m.registrationStatus
}

// fakeInventoryDB is a mock implementation of the inventoryDB interface for testing.
type fakeInventoryDB struct {
	resources                chan []resourcev1.Device
	rescanCalls              atomic.Int32
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

// fakeNriStub is a fake implementation of stub.Stub for unit tests. Each
// successful Start is one connection. The connection stays open until
// disconnect or Stop.
type fakeNriStub struct {
	stub.Stub

	mu         sync.Mutex
	startFn    func(context.Context) error
	done       chan struct{}
	stopCalled atomic.Bool
}

func (m *fakeNriStub) Start(ctx context.Context) error {
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
	m.disconnect()
}

// disconnect closes the connection, as when the container runtime stops.
func (m *fakeNriStub) disconnect() {
	m.mu.Lock()
	done := m.done
	m.done = nil
	m.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func TestStop(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	fakeDra := newFakePluginHelper()
	fakeNri := &fakeNriStub{}

	np := &NetworkDriver{
		draPlugin:      fakeDra,
		nriPlugin:      fakeNri,
		podConfigStore: mustNewPodConfigStore(),
		clock:          fakeClock,
	}

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

// TestStartConnectsNRIBeforeDRA checks that Start does not start the DRA plugin
// before the NRI plugin has a connection to the container runtime. When the
// DRA plugin is registered, the kubelet can create the sandbox of a pod that
// has its claims prepared, and the runtime must call the NRI hooks for it.
func TestStartConnectsNRIBeforeDRA(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use /tmp so Unix socket paths stay under Linux's 108-byte sun_path limit.
	rootDir, err := os.MkdirTemp("/tmp", "dranet-start-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	registryDir := filepath.Join(rootDir, "plugins_registry")
	if err := os.MkdirAll(registryDir, 0750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The DRA plugin makes its registration socket when it starts.
	registrationSockets := func() int {
		entries, err := os.ReadDir(registryDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		return len(entries)
	}

	// The NRI plugin connects when the test closes connect.
	connect := make(chan struct{})
	fakeNri := &fakeNriStub{startFn: func(ctx context.Context) error {
		select {
		case <-connect:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	np := &NetworkDriver{
		driverName:     "test.driver",
		nodeName:       "node-1",
		kubeClient:     fake.NewSimpleClientset(),
		nriPlugin:      fakeNri,
		netdb:          newFakeInventoryDB(),
		podConfigStore: mustNewPodConfigStore(),
		kubeletRootDir: rootDir,
		clock:          testingclock.NewFakeClock(time.Now()),
	}
	startErr := make(chan error, 1)
	go func() { startErr <- np.Start(ctx) }()

	time.Sleep(200 * time.Millisecond)
	if n := registrationSockets(); n != 0 {
		t.Fatalf("the DRA plugin started before the NRI plugin connected: %d registration sockets", n)
	}

	close(connect)
	deadline := time.Now().Add(5 * time.Second)
	for registrationSockets() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if registrationSockets() == 0 {
		t.Fatal("the DRA plugin did not start after the NRI plugin connected")
	}

	// There is no kubelet to register the DRA plugin, so stop Start here.
	cancel()
	if err := <-startErr; err == nil {
		t.Error("Start() = nil without a kubelet, want error")
	}
	fakeNri.Stop()
	if np.draPlugin != nil {
		np.draPlugin.Stop()
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

	// Each Start of the stub is one connection attempt, with these outcomes.
	var idx atomic.Int32
	fakeNri := &fakeNriStub{}
	fakeNri.startFn = func(context.Context) error {
		n := int(idx.Add(1)) - 1
		attemptCh <- n
		if n >= len(outcomes) {
			return errors.New("unexpected extra attempt")
		}
		return outcomes[n]
	}
	np := &NetworkDriver{
		driverName:     "test.driver",
		nriPlugin:      fakeNri,
		podConfigStore: mustNewPodConfigStore(),
		clock:          fakeClock,
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- np.runNRI(ctx)
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

	// reconnect checks that runNRI waits exactly backoff before the attempt
	// with the given index.
	reconnect := func(backoff time.Duration, attempt int) {
		t.Helper()
		waitWaiter()
		fakeClock.Step(backoff - time.Millisecond)
		select {
		case got := <-attemptCh:
			t.Fatalf("attempt %d before the end of the %v backoff", got, backoff)
		case <-time.After(5 * time.Millisecond):
		}
		fakeClock.Step(time.Millisecond)
		waitAttempt(attempt)
	}
	// waitDisconnected checks that runNRI clears nriConnected when the
	// connection closes.
	waitDisconnected := func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !np.nriConnected.Load() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("timed out waiting for nriConnected=false")
	}

	// Initial connection: no wait before the first attempt.
	waitAttempt(0)
	waitConnected()

	// Outage 1: exponential backoff 1s, 2s, 4s, 8s, then recovery.
	fakeNri.disconnect()
	waitDisconnected()
	reconnect(1*time.Second, 1)
	reconnect(2*time.Second, 2)
	reconnect(4*time.Second, 3)
	reconnect(8*time.Second, 4)
	waitConnected()

	// Outage 2: the connection reset the failures and the backoff, so the
	// backoff starts at 1s again and 6 failures in total do not stop runNRI.
	fakeNri.disconnect()
	waitDisconnected()
	reconnect(1*time.Second, 5)
	reconnect(2*time.Second, 6)
	reconnect(4*time.Second, 7)
	reconnect(8*time.Second, 8)
	waitConnected()

	// Outage 3: maxAttempts failures in sequence return an error.
	fakeNri.disconnect()
	waitDisconnected()
	for i, d := range []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		reconnect(d, 9+i)
	}

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
		draPlugin:      newFakePluginHelper(),
		driverName:     "dra.net",
		nodeName:       "node-1",
		kubeClient:     fake.NewSimpleClientset(),
		eventRecorder:  record.NewFakeRecorder(100),
		netdb:          fakeDB,
		rdmaSharedMode: true,
		podConfigStore: store,
		clock:          fakeClock,
	}

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

	// Start needs a kubelet, so the test makes the stub of the driver and runs
	// the NRI loop.
	nriStub, err := stub.New(np, stub.WithPluginName(np.driverName), stub.WithPluginIdx("00"), stub.WithSocketPath(clientSock))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	np.nriPlugin = nriStub
	runErr := make(chan error, 1)
	go func() { runErr <- np.runNRI(ctx) }()
	waitFor("NRI connection to runtime 1", func() bool { return np.CheckNRI() == nil })

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

	// 1. Prepare claim for pod-1 and exercise the RunPodSandbox/CreateContainer hooks on runtime 1.
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

	waitFor("NRI disconnect and reconnect backoff timer", func() bool {
		return !np.nriConnected.Load() && fakeClock.HasWaiters()
	})
	if err := np.CheckNRI(); err == nil {
		t.Fatal("CheckNRI() during NRI outage = nil, want error")
	}

	// While NRI is disconnected, PrepareResourceClaims must reject new claims.
	claim2 := newIBClaim("claim-2-uid", "claim-2", "pod-2-uid", "pod-2")
	if _, err := np.PrepareResourceClaims(ctx, []*resourcev1.ResourceClaim{claim2}); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("PrepareResourceClaims while disconnected = %v, want not connected error", err)
	}

	// 3. Start runtime 2 with pod-1 live and verify that the same stub connects
	// again, Synchronize and the new pod lifecycle.
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

	waitFor("NRI connection to runtime 2", func() bool { return np.CheckNRI() == nil })
	// The runtime calls Synchronize immediately after it accepts the connection.
	waitFor("Synchronize of pod-1 on runtime 2", func() bool {
		pod1Cfg, ok := store.GetPodConfig("pod-1-uid")
		return ok && pod1Cfg.NetNS == "/var/run/netns/pod1-reloaded"
	})

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
	if err := <-runErr; err != nil {
		t.Errorf("runNRI() after Stop() = %v, want nil", err)
	}
	if err := np.CheckNRI(); err == nil {
		t.Error("CheckNRI() after Stop() = nil, want error")
	}
}
