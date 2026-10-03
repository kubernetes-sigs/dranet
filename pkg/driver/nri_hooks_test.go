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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/dranet/pkg/apis"
	hookapi "sigs.k8s.io/dranet/pkg/apis/hook"
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

func TestRequestBudget(t *testing.T) {
	// No deadline: the budget context only ends with the parent.
	ctx, cancel := requestBudget(context.Background())
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline without a request deadline")
	}
	cancel()

	// With a deadline: ends nriDeadlineMargin earlier.
	parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelParent()
	ctx, cancel = requestBudget(parent)
	defer cancel()
	parentDeadline, _ := parent.Deadline()
	budgetDeadline, ok := ctx.Deadline()
	if !ok || !budgetDeadline.Equal(parentDeadline.Add(-nriDeadlineMargin)) {
		t.Fatalf("expected the budget to end %s before the request deadline, got %v (request %v)", nriDeadlineMargin, budgetDeadline, parentDeadline)
	}
}

// slowAttach makes every netdev attach take at least delay for the rest of the
// test, the way slow hardware does.
func slowAttach(t *testing.T, delay time.Duration) {
	t.Helper()
	attach := attachNetdev
	attachNetdev = func(hostIfName string, containerNsPath string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
		time.Sleep(delay)
		return attach(hostIfName, containerNsPath, interfaceConfig)
	}
	t.Cleanup(func() { attachNetdev = attach })
}

// slowAttachDriver returns a driver whose device attach takes at least delay
// per device. The device does not exist, so an attempt ends with a netlink
// error after the delay.
func slowAttachDriver(t *testing.T, podUID types.UID, delay, attachTimeout time.Duration, recorder *record.FakeRecorder) *NetworkDriver {
	t.Helper()
	if delay > 0 {
		slowAttach(t, delay)
	}
	np := &NetworkDriver{
		podConfigStore:      mustNewPodConfigStore(),
		netdb:               inventory.New(),
		eventRecorder:       recorder,
		deviceAttachTimeout: attachTimeout,
		hookPath:            "/opt/dranet/bin/dranet-hook",
	}
	if err := np.podConfigStore.SetDeviceConfig(podUID, "dev0", nonexistentDeviceConfig()); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}
	return np
}

// shortRequest returns a request context whose budget ends before a slow
// device attach can complete.
func shortRequest() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), nriDeadlineMargin+50*time.Millisecond)
}

func waitForJob(t *testing.T, np *NetworkDriver, podUID types.UID) *attachJob {
	t.Helper()
	job := np.getAttachJob(podUID)
	if job == nil {
		t.Fatal("expected an attach job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !job.wait(ctx) {
		t.Fatal("attach job did not finish")
	}
	return job
}

// TestRunPodSandboxFailsBeforeRuntimeDeadline verifies that with deferral
// disabled, when the attach does not fit in the request budget, RunPodSandbox
// returns an error before the runtime deadline, so the runtime fails the
// sandbox instead of disconnecting the plugin.
func TestRunPodSandboxFailsBeforeRuntimeDeadline(t *testing.T) {
	podUID := types.UID("test-pod-deadline")
	recorder := record.NewFakeRecorder(10)
	np := slowAttachDriver(t, podUID, 500*time.Millisecond, 0, recorder)
	pod := deadlineTestPod(podUID)

	ctx, cancel := shortRequest()
	defer cancel()
	start := time.Now()
	err := np.RunPodSandbox(ctx, pod)
	if !errors.Is(err, errNRIBudgetExceeded) {
		t.Fatalf("expected errNRIBudgetExceeded, got %v", err)
	}
	if deadline, _ := ctx.Deadline(); time.Now().After(deadline) {
		t.Fatalf("RunPodSandbox returned after the request deadline (%s)", time.Since(start))
	}
	expectEvent(t, recorder, "NetworkDeviceAttachTimeout")
	if got := testutil.ToFloat64(nriPluginRequestsTotal.WithLabelValues(methodRunPodSandbox, statusFailed)); got < 1 {
		t.Errorf("expected RunPodSandbox to be counted as failed, got %f", got)
	}
	if got := testutil.ToFloat64(sandboxAttachTotal.WithLabelValues(attachResultRejected)); got < 1 {
		t.Errorf("expected the sandbox attach to be counted as rejected, got %f", got)
	}
	// The job was cancelled; the device operation in flight ends on its own
	// and nothing is recorded as attached.
	job := waitForJob(t, np, podUID)
	if job.err == nil {
		t.Fatal("expected the cancelled job to end with an error")
	}
	if cfg, _ := np.podConfigStore.GetDeviceConfig(podUID, "dev0"); cfg.Attached {
		t.Fatal("expected no device recorded as attached")
	}
}

// TestRunPodSandboxDefersToHook covers the deferred path: RunPodSandbox
// returns with the job running, CreateContainer injects the OCI hook while the
// job runs, retries a failed job until the deadline, and fails terminally
// after it; a new sandbox starts over.
func TestRunPodSandboxDefersToHook(t *testing.T) {
	podUID := types.UID("test-pod-defer")
	recorder := record.NewFakeRecorder(10)
	np := slowAttachDriver(t, podUID, 500*time.Millisecond, 10*time.Second, recorder)
	pod := deadlineTestPod(podUID)
	ctr := &api.Container{Name: "ctr"}

	ctx, cancel := shortRequest()
	defer cancel()
	if err := np.RunPodSandbox(ctx, pod); err != nil {
		t.Fatalf("expected RunPodSandbox to defer without error, got %v", err)
	}
	expectEvent(t, recorder, "NetworkDeviceAttachDeferred")
	if job := np.getAttachJob(podUID); job == nil || job.deadline.IsZero() {
		t.Fatal("expected a job with an attach deadline")
	}
	if got := testutil.ToFloat64(sandboxAttachTotal.WithLabelValues(attachResultDeferred)); got < 1 {
		t.Errorf("expected the sandbox attach to be counted as deferred, got %f", got)
	}
	barrierHooks := testutil.ToFloat64(containerHooksTotal.WithLabelValues(hookTypeBarrier))

	// While the job runs, the container gets the hook and no error. Several
	// containers of the Pod created at once share the job and none waits for
	// the lock of another: each returns within its own request budget.
	ctx2, cancel2 := shortRequest()
	defer cancel2()
	adjust, _, err := np.CreateContainer(ctx2, pod, ctr)
	if err != nil {
		t.Fatalf("expected CreateContainer to inject the hook, got %v", err)
	}
	if adjust == nil || adjust.Hooks == nil || len(adjust.Hooks.CreateRuntime) != 1 {
		t.Fatalf("expected one createRuntime hook, got %#v", adjust)
	}
	hook := adjust.Hooks.CreateRuntime[0]
	if hook.Path != np.hookPath || hook.Timeout.GetValue() <= 0 {
		t.Fatalf("unexpected hook %#v", hook)
	}
	if got := testutil.ToFloat64(containerHooksTotal.WithLabelValues(hookTypeBarrier)); got != barrierHooks+1 {
		t.Errorf("expected one more barrier hook counted, got %f, had %f", got, barrierHooks)
	}
	// The environment carries what DRANET decided for the Pod, so the hook
	// does not need another channel to learn it.
	env := map[string]string{}
	for _, kv := range hook.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env[hookapi.EnvPodUID] != string(podUID) || env[hookapi.EnvPodNamespace] != pod.Namespace || env[hookapi.EnvPodName] != pod.Name ||
		env[hookapi.EnvNetNS] != getNetworkNamespace(pod) || env[hookapi.EnvSocket] != hookapi.SocketPath {
		t.Fatalf("unexpected hook environment %v", hook.Env)
	}
	for k := range env {
		if strings.Contains(strings.ToLower(k), "container") {
			t.Fatalf("the hook environment must not name the container, got %s", k)
		}
	}
	var devices []apis.HookDevice
	if err := json.Unmarshal([]byte(env[hookapi.EnvDevices]), &devices); err != nil {
		t.Fatalf("decode %s: %v", hookapi.EnvDevices, err)
	}
	wantDevice := apis.HookDevice{
		Name:      "dev0",
		Claim:     apis.HookClaimRef{Namespace: "ns", Name: "claim1"},
		Host:      apis.DeviceIdentifiers{Name: "nonexistent0"},
		Interface: "eth0-pod",
		Config:    &apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth0-pod"}},
	}
	if len(devices) != 1 || !reflect.DeepEqual(devices[0], wantDevice) {
		t.Fatalf("unexpected hook devices %#v, want %#v", devices, wantDevice)
	}
	running := np.getAttachJob(podUID)
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := shortRequest()
			defer cancel()
			adjust, _, err := np.CreateContainer(ctx, pod, &api.Container{Name: "ctr" + string(rune('a'+i))})
			if deadline, _ := ctx.Deadline(); time.Now().After(deadline) {
				results <- errors.New("CreateContainer returned after its request deadline")
				return
			}
			if err == nil && (adjust == nil || adjust.Hooks == nil || len(adjust.Hooks.CreateRuntime) != 1) {
				err = errors.New("expected the hook to be injected")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent CreateContainer: %v", err)
		}
	}
	if np.getAttachJob(podUID) != running {
		t.Fatal("concurrent CreateContainer calls must share the running job")
	}

	// The job fails (the device does not exist); before the deadline the
	// next container creation starts a new job.
	first := waitForJob(t, np, podUID)
	if first.err == nil || errors.Is(first.err, context.Canceled) {
		t.Fatalf("expected the job to fail on the device, got %v", first.err)
	}
	ctx3, cancel3 := shortRequest()
	defer cancel3()
	if _, _, err := np.CreateContainer(ctx3, pod, ctr); err != nil {
		t.Fatalf("expected CreateContainer to retry the attach, got %v", err)
	}
	if np.getAttachJob(podUID) == first {
		t.Fatal("expected a new attach job")
	}
	waitForJob(t, np, podUID)

	// Past the deadline: terminal error and event, no new job. The job is
	// finished, so nothing else reads its deadline.
	np.getAttachJob(podUID).deadline = time.Now().Add(-time.Second)
	deadlineExceeded := testutil.ToFloat64(attachDeadlineExceededTotal)
	_, _, err = np.CreateContainer(context.Background(), pod, ctr)
	if err == nil || !strings.Contains(err.Error(), "gave up attaching") {
		t.Fatalf("expected a terminal error after the deadline, got %v", err)
	}
	expectEvent(t, recorder, "NetworkDeviceAttachFailed")
	if got := testutil.ToFloat64(attachDeadlineExceededTotal); got != deadlineExceeded+1 {
		t.Errorf("expected the deadline to be counted as exceeded once, got %f, had %f", got, deadlineExceeded)
	}

	// A new sandbox starts over: progress and deadline are reset.
	ctx4, cancel4 := shortRequest()
	defer cancel4()
	if err := np.RunPodSandbox(ctx4, pod); err != nil {
		t.Fatalf("expected RunPodSandbox to defer again, got %v", err)
	}
	if job := np.getAttachJob(podUID); !time.Now().Before(job.deadline) {
		t.Fatalf("expected a fresh deadline, got %v", job.deadline)
	}
	waitForJob(t, np, podUID)
}

// TestHookWaitHandler covers the endpoint the OCI hook blocks on.
func TestHookWaitHandler(t *testing.T) {
	podUID := types.UID("test-pod-hook")
	np := slowAttachDriver(t, podUID, 0, 10*time.Second, record.NewFakeRecorder(10))
	pod := deadlineTestPod(podUID)

	get := func(pod string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		np.handleHookWait(rec, httptest.NewRequest(http.MethodGet, hookapi.WaitPath+"?pod="+pod, nil))
		return rec
	}

	if rec := get(""); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a pod, got %d", rec.Code)
	}
	if rec := get("unknown"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for an unknown pod, got %d", rec.Code)
	}
	// Pending devices and no job (the driver restarted): the hook does not
	// start one, the container start fails and CreateContainer will.
	if rec := get(string(podUID)); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "no network device attach in progress") {
		t.Fatalf("expected 500 without a job, got %d %q", rec.Code, rec.Body.String())
	}

	// A failing job: the hook gets the reason.
	ctx, cancel := shortRequest()
	defer cancel()
	_ = np.RunPodSandbox(ctx, pod)
	waitForJob(t, np, podUID)
	failedWaits := testutil.ToFloat64(hookWaitsTotal.WithLabelValues(hookWaitFailed))
	releasedWaits := testutil.ToFloat64(hookWaitsTotal.WithLabelValues(hookWaitReleased))
	rec := get(string(podUID))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "nonexistent0") {
		t.Fatalf("expected 500 with the attach error, got %d %q", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(hookWaitsTotal.WithLabelValues(hookWaitFailed)); got != failedWaits+1 {
		t.Errorf("expected one more failed hook wait, got %f, had %f", got, failedWaits)
	}

	// All attached: released immediately.
	if err := np.podConfigStore.SetDeviceAttached(podUID, "dev0", true); err != nil {
		t.Fatal(err)
	}
	if rec := get(string(podUID)); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when all devices are attached, got %d %q", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(hookWaitsTotal.WithLabelValues(hookWaitReleased)); got != releasedWaits+1 {
		t.Errorf("expected one more released hook wait, got %f, had %f", got, releasedWaits)
	}
}

// TestCreateContainerRuntimeHooks covers the profile providers' hooks: they
// run after the barrier, once per distinct path and arguments, with each
// device's data in the shared environment, whether or not the attach was
// deferred; they are added to containers until one is created, and their
// timeouts are bounded as a chain.
func TestCreateContainerRuntimeHooks(t *testing.T) {
	podUID := types.UID("test-pod-runtime-hooks")
	pod := deadlineTestPod(podUID)
	ctr := &api.Container{Name: "ctr"}
	acme := &apis.RuntimeHook{Path: "/opt/acme/bin/acme-hook", Args: []string{"--post"}, TimeoutSeconds: 5}
	acmeB := &apis.RuntimeHook{Path: "/opt/acme/bin/acme-hook", Args: []string{"--post"}, TimeoutSeconds: 5, Data: json.RawMessage(`{"rail":1}`)}
	other := &apis.RuntimeHook{Path: "/opt/other/hook"}

	newDriver := func(t *testing.T, attached bool, hooks ...*apis.RuntimeHook) *NetworkDriver {
		t.Helper()
		np := &NetworkDriver{
			podConfigStore:      mustNewPodConfigStore(),
			netdb:               inventory.New(),
			eventRecorder:       record.NewFakeRecorder(10),
			deviceAttachTimeout: 10 * time.Second,
			hookPath:            "/opt/dranet/bin/dranet-hook",
		}
		for i, h := range hooks {
			cfg := nonexistentDeviceConfig()
			cfg.Attached = attached
			cfg.RuntimeHook = h
			if err := np.podConfigStore.SetDeviceConfig(podUID, "dev"+string(rune('0'+i)), cfg); err != nil {
				t.Fatal(err)
			}
		}
		return np
	}
	hookPaths := func(adjust *api.ContainerAdjustment) []string {
		var paths []string
		for _, h := range adjust.GetHooks().GetCreateRuntime() {
			paths = append(paths, h.Path+" "+strings.Join(h.Args[1:], " "))
		}
		return paths
	}

	t.Run("after the barrier, deduplicated, with data", func(t *testing.T) {
		np := newDriver(t, false, acme, acmeB, other)
		slowAttach(t, 500*time.Millisecond)
		ctx, cancel := shortRequest()
		defer cancel()
		if err := np.RunPodSandbox(ctx, pod); err != nil {
			t.Fatalf("RunPodSandbox: %v", err)
		}
		ctx2, cancel2 := shortRequest()
		defer cancel2()
		adjust, _, err := np.CreateContainer(ctx2, pod, ctr)
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		want := []string{np.hookPath + " wait", "/opt/acme/bin/acme-hook --post", "/opt/other/hook "}
		if got := hookPaths(adjust); !reflect.DeepEqual(got, want) {
			t.Fatalf("hooks = %v, want %v", got, want)
		}
		for _, h := range adjust.Hooks.CreateRuntime {
			if len(h.Env) == 0 || !reflect.DeepEqual(h.Env, adjust.Hooks.CreateRuntime[0].Env) {
				t.Fatalf("every hook must get the same environment, got %#v", h)
			}
		}
		if got := adjust.Hooks.CreateRuntime[2].Timeout.GetValue(); got != apis.RuntimeHookDefaultTimeoutSeconds {
			t.Fatalf("default timeout = %d, want %d", got, apis.RuntimeHookDefaultTimeoutSeconds)
		}
		var devices []apis.HookDevice
		for _, kv := range adjust.Hooks.CreateRuntime[0].Env {
			if v, ok := strings.CutPrefix(kv, hookapi.EnvDevices+"="); ok {
				if err := json.Unmarshal([]byte(v), &devices); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(devices) != 3 || string(devices[1].Data) != `{"rail":1}` || devices[0].Data != nil {
			t.Fatalf("unexpected device data in %#v", devices)
		}
		waitForJob(t, np, podUID)
	})

	t.Run("without the barrier when the devices are attached", func(t *testing.T) {
		np := newDriver(t, true, acme)
		ctx, cancel := shortRequest()
		defer cancel()
		adjust, _, err := np.CreateContainer(ctx, pod, ctr)
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		if got := hookPaths(adjust); !reflect.DeepEqual(got, []string{"/opt/acme/bin/acme-hook --post"}) {
			t.Fatalf("hooks = %v", got)
		}
	})

	t.Run("once per pod: until a container carrying them starts", func(t *testing.T) {
		np := newDriver(t, true, acme)
		// The first start failed (no StartContainer): the retry, or the next
		// container, carries the hooks again.
		for _, name := range []string{"init", "init"} {
			adjust, _, err := np.CreateContainer(context.Background(), pod, &api.Container{Name: name})
			if err != nil || len(hookPaths(adjust)) != 1 {
				t.Fatalf("expected the hooks on %s before any container started, got %v, %v", name, hookPaths(adjust), err)
			}
		}
		if err := np.StartContainer(context.Background(), pod, &api.Container{Name: "init"}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sidecar", "app", "app"} {
			adjust, _, err := np.CreateContainer(context.Background(), pod, &api.Container{Name: name})
			if err != nil || adjust.GetHooks() != nil {
				t.Fatalf("expected no hooks on %s after the first container started, got %#v, %v", name, adjust.GetHooks(), err)
			}
		}
		cfg, _ := np.podConfigStore.GetDeviceConfig(podUID, "dev0")
		if !cfg.RuntimeHookDone {
			t.Fatal("expected the completion to be recorded with the device")
		}
		// A new sandbox runs them again (the reset also clears Attached;
		// restore it so only the hook state is under test).
		if _, err := np.podConfigStore.ResetAttachProgress(podUID); err != nil {
			t.Fatal(err)
		}
		if err := np.podConfigStore.SetDeviceAttached(podUID, "dev0", true); err != nil {
			t.Fatal(err)
		}
		adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
		if err != nil || len(hookPaths(adjust)) != 1 {
			t.Fatalf("expected the hooks again after a reset, got %v, %v", hookPaths(adjust), err)
		}
	})

	t.Run("StartContainer without hooks is a no-op", func(t *testing.T) {
		np := newDriver(t, true, nil)
		if err := np.StartContainer(context.Background(), pod, ctr); err != nil {
			t.Fatal(err)
		}
		if err := np.StartContainer(context.Background(), deadlineTestPod("unknown"), ctr); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no hooks when the provider set none", func(t *testing.T) {
		np := newDriver(t, true, nil)
		adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
		if err != nil {
			t.Fatalf("CreateContainer: %v", err)
		}
		if adjust.GetHooks() != nil {
			t.Fatalf("expected no hooks, got %#v", adjust.Hooks)
		}
	})

	t.Run("chain over the maximum fails the container", func(t *testing.T) {
		var hooks []*apis.RuntimeHook
		for i := 0; i*apis.RuntimeHookMaxTimeoutSeconds <= apis.RuntimeHookChainMaxTimeoutSeconds; i++ {
			hooks = append(hooks, &apis.RuntimeHook{Path: "/opt/slow/hook" + string(rune('a'+i)), TimeoutSeconds: apis.RuntimeHookMaxTimeoutSeconds})
		}
		np := newDriver(t, true, hooks...)
		_, _, err := np.CreateContainer(context.Background(), pod, ctr)
		if err == nil || !strings.Contains(err.Error(), "over the maximum") {
			t.Fatalf("expected the chain cap error, got %v", err)
		}
	})
}

// TestCreateContainerSkipsResumeWhenAttached verifies that a pod whose devices
// are all attached gets its RDMA char devices without any attach work, even
// with no request budget.
func TestCreateContainerSkipsResumeWhenAttached(t *testing.T) {
	podUID := types.UID("test-pod-attached")
	np := &NetworkDriver{
		podConfigStore:      mustNewPodConfigStore(),
		eventRecorder:       record.NewFakeRecorder(10),
		deviceAttachTimeout: time.Second,
	}
	cfg := nonexistentDeviceConfig()
	cfg.Attached = true
	cfg.RDMADevice.DevChars = []LinuxDevice{{Path: "/dev/infiniband/uverbs0", Type: "c", Major: 231, Minor: 192}}
	if err := np.podConfigStore.SetDeviceConfig(podUID, "dev0", cfg); err != nil {
		t.Fatalf("SetDeviceConfig() error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nriDeadlineMargin/2)
	defer cancel()
	adjust, _, err := np.CreateContainer(ctx, deadlineTestPod(podUID), &api.Container{Name: "ctr"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(adjust.Linux.Devices) != 1 {
		t.Fatalf("expected the RDMA char device, got %#v", adjust.Linux.Devices)
	}
}

func nonexistentDeviceConfig() DeviceConfig {
	return DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "nonexistent0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0-pod"},
		},
	}
}

func deadlineTestPod(podUID types.UID) *api.PodSandbox {
	return &api.PodSandbox{
		Uid:       string(podUID),
		Name:      string(podUID),
		Namespace: "test-ns",
		Linux: &api.LinuxPodSandbox{
			Namespaces: []*api.LinuxNamespace{
				{Type: "network", Path: "/var/run/netns/test"},
			},
		},
	}
}

func expectEvent(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, reason) {
			t.Fatalf("expected event %s, got %q", reason, ev)
		}
	default:
		t.Fatalf("expected a %s event", reason)
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
