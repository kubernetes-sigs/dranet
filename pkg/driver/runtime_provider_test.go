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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/google/go-cmp/cmp"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ocihook"
	"sigs.k8s.io/dranet/pkg/runtimeprovider"
)

// fakeRuntimeProvider records the ConfigurePod calls and answers as told.
type fakeRuntimeProvider struct {
	mu       sync.Mutex
	requests []*cloudprovider.ConfigurePodRequest
	err      error
}

func (p *fakeRuntimeProvider) ConfigurePod(_ context.Context, req *cloudprovider.ConfigurePodRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return p.err
}

func (p *fakeRuntimeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

const (
	hookTestPID    = 4242
	hookTestSocket = "/var/run/dranet/hook.sock"
)

// newHookDriver returns a driver with runtime provider(s), running
// in the initial PID namespace, with a Pod of two claims prepared.
func newHookDriver(t *testing.T, providers ...cloudprovider.RuntimeProvider) (*NetworkDriver, *api.PodSandbox) {
	t.Helper()
	np := &NetworkDriver{
		podConfigStore:  mustNewPodConfigStore(),
		eventRecorder:   record.NewFakeRecorder(10),
		runtimeProvider: runtimeprovider.New(hookTestSocket, ocihook.DefaultTimeout, providers...),
	}
	np.bindRuntimeProviderPersistence()
	np.runtimeProvider.SetIdentity(ocihook.Identity{InitPIDNamespace: true, PID: hookTestPID, Args: []string{"/dranet"}})
	pod := &api.PodSandbox{Id: "sandbox-1", Uid: "pod-uid", Name: "workload", Namespace: "team", Linux: &api.LinuxPodSandbox{
		Namespaces: []*api.LinuxNamespace{{Type: "network", Path: "/var/run/netns/cni-1234"}},
	}}
	claimA := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "claim-a", UID: "claim-a-uid"}}
	claimB := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "claim-b", UID: "claim-b-uid"}}
	for name, config := range map[string]DeviceConfig{
		"nic-1": {
			Claim: types.NamespacedName{Namespace: "team", Name: "claim-b"}, ResourceClaim: claimB,
			DeviceSnapshot: &resourceapi.Device{Name: "nic-1", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrInterfaceName: {StringValue: ptr.To("eth1")},
				apis.AttrMac:           {StringValue: ptr.To("aa:bb:cc:dd:ee:01")},
				apis.AttrPCIAddress:    {StringValue: ptr.To("0000:00:01.0")},
			}},
			NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth1"}},
			NetworkInterfaceConfigInPod:  apis.NetworkConfig{Profile: "fabric", Interface: apis.InterfaceConfig{Name: "net1", Addresses: []string{"10.0.0.1/24"}}},
			RDMADevice:                   RDMAConfig{LinkDev: "mlx5_1"},
		},
		"nic-0": {
			Claim: types.NamespacedName{Namespace: "team", Name: "claim-b"}, ResourceClaim: claimB,
			// Prepared by a version without the device snapshot: the host
			// interface name identifies the device.
			NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth0"}},
			NetworkInterfaceConfigInPod:  apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth0"}},
		},
		"ib-0": {
			Claim: types.NamespacedName{Namespace: "team", Name: "claim-a"}, ResourceClaim: claimA,
			DeviceSnapshot: &resourceapi.Device{Name: "ib-0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:02.0")},
			}},
			RDMADevice: RDMAConfig{LinkDev: "mlx5_2"},
		},
	} {
		if err := np.podConfigStore.SetDeviceConfig(types.UID(pod.Uid), name, config); err != nil {
			t.Fatal(err)
		}
	}
	return np, pod
}

func wantConfigurePodRequest() *cloudprovider.ConfigurePodRequest {
	return &cloudprovider.ConfigurePodRequest{
		Pod:   cloudprovider.PodReference{Namespace: "team", Name: "workload", UID: "pod-uid"},
		NetNS: "/var/run/netns/cni-1234",
		Claims: []cloudprovider.PodClaim{
			{
				Claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "claim-a", UID: "claim-a-uid"}},
				Devices: []cloudprovider.PodDevice{{
					Name: "ib-0", Device: cloudprovider.DeviceIdentifiers{PCIAddress: "0000:00:02.0"},
					RDMADevice: "mlx5_2",
				}},
			},
			{
				Claim: &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "claim-b", UID: "claim-b-uid"}},
				Devices: []cloudprovider.PodDevice{
					{
						Name: "nic-0", Device: cloudprovider.DeviceIdentifiers{Name: "eth0"},
						Config: &apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth0"}},
					},
					{
						Name: "nic-1", Device: cloudprovider.DeviceIdentifiers{Name: "eth1", MAC: "aa:bb:cc:dd:ee:01", PCIAddress: "0000:00:01.0"},
						Config:     &apis.NetworkConfig{Profile: "fabric", Interface: apis.InterfaceConfig{Name: "net1", Addresses: []string{"10.0.0.1/24"}}},
						RDMADevice: "mlx5_1",
					},
				},
			},
		},
	}
}

func createRuntimeHooks(t *testing.T, np *NetworkDriver, pod *api.PodSandbox, ctr *api.Container) []*api.Hook {
	t.Helper()
	adjust, _, err := np.CreateContainer(context.Background(), pod, ctr)
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", ctr.Name, err)
	}
	return adjust.GetHooks().GetCreateRuntime()
}

func TestRuntimeProviderRunsOncePerSandboxBeforeTheFirstContainer(t *testing.T) {
	provider := &fakeRuntimeProvider{}
	np, pod := newHookDriver(t, provider)
	first := &api.Container{Id: "ctr-1", Name: "init"}
	second := &api.Container{Id: "ctr-2", Name: "app"}

	// The container the kubelet creates first carries the hook, which runs
	// the DRANET executable as the runtime sees it, against the daemon.
	hooks := createRuntimeHooks(t, np, pod, first)
	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}
	want := &api.Hook{
		Path:    fmt.Sprintf("/proc/%d/exe", hookTestPID),
		Args:    []string{"dranet", "oci-hook", "--socket=" + hookTestSocket, "--container-id=ctr-1", "--timeout=28s"},
		Timeout: api.Int(30),
	}
	if diff := cmp.Diff(want, hooks[0], cmp.Comparer(func(a, b *api.Hook) bool {
		return a.Path == b.Path && strings.Join(a.Args, " ") == strings.Join(b.Args, " ") && a.Timeout.GetValue() == b.Timeout.GetValue() && len(a.Env) == len(b.Env)
	})); diff != "" {
		t.Fatalf("hook mismatch (-want +got):\n%s", diff)
	}
	if hooks := createRuntimeHooks(t, np, pod, second); len(hooks) != 1 || !strings.Contains(strings.Join(hooks[0].Args, " "), "--container-id=ctr-2") {
		t.Fatalf("second container hooks = %v, want its own hook", hooks)
	}

	if err := np.handleRuntimeHook(context.Background(), "ctr-1"); err != nil {
		t.Fatalf("handle(ctr-1): %v", err)
	}
	if provider.calls() != 1 {
		t.Fatalf("provider called %d times, want 1", provider.calls())
	}
	if diff := cmp.Diff(wantConfigurePodRequest(), provider.requests[0]); diff != "" {
		t.Fatalf("request mismatch (-want +got):\n%s", diff)
	}

	// Done for the sandbox: the second container's hook has nothing to do,
	// and later containers do not carry it.
	if err := np.handleRuntimeHook(context.Background(), "ctr-2"); err != nil {
		t.Fatalf("handle(ctr-2): %v", err)
	}
	if hooks := createRuntimeHooks(t, np, pod, &api.Container{Id: "ctr-3", Name: "sidecar"}); len(hooks) != 0 {
		t.Fatalf("hooks after completion = %v, want none", hooks)
	}
	if provider.calls() != 1 {
		t.Fatalf("provider called %d times after completion, want 1", provider.calls())
	}
}

func TestRuntimeProviderFailureEmitsEventAndIsRetried(t *testing.T) {
	provider := &fakeRuntimeProvider{err: errors.New("fabric not ready")}
	np, pod := newHookDriver(t, provider)
	recorder := np.eventRecorder.(*record.FakeRecorder)

	createRuntimeHooks(t, np, pod, &api.Container{Id: "ctr-1", Name: "app"})
	err := np.handleRuntimeHook(context.Background(), "ctr-1")
	if err == nil || !strings.Contains(err.Error(), "fabric not ready") || !strings.Contains(err.Error(), "team/workload") {
		t.Fatalf("handle = %v, want the provider's reason with the pod", err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "RuntimeProviderFailed") || !strings.Contains(event, "fabric not ready") {
			t.Fatalf("event = %q", event)
		}
	default:
		t.Fatal("no event recorded for the failed hook")
	}
}

func TestRuntimeProviderNeedsTheNetworkNamespace(t *testing.T) {
	provider := &fakeRuntimeProvider{}
	np, pod := newHookDriver(t, provider)
	hostNetwork := &api.PodSandbox{Id: pod.Id, Uid: pod.Uid, Name: pod.Name, Namespace: pod.Namespace}
	if _, _, err := np.CreateContainer(context.Background(), hostNetwork, &api.Container{Id: "ctr-1", Name: "app"}); err == nil || !strings.Contains(err.Error(), "network namespace") {
		t.Fatalf("CreateContainer without a network namespace = %v", err)
	}
	// The namespace recorded by RunPodSandbox or Synchronize is as good.
	np.podConfigStore.SetPodNetNs(types.UID(pod.Uid), "/var/run/netns/from-store")
	if hooks := createRuntimeHooks(t, np, hostNetwork, &api.Container{Id: "ctr-1", Name: "app"}); len(hooks) != 1 {
		t.Fatalf("hooks with the stored namespace = %v, want one", hooks)
	}
	if err := np.handleRuntimeHook(context.Background(), "ctr-1"); err != nil {
		t.Fatalf("handle(ctr-1): %v", err)
	}
	if provider.calls() != 1 || provider.requests[0].NetNS != "/var/run/netns/from-store" {
		t.Fatalf("requests = %+v", provider.requests)
	}
}

func TestRuntimeProviderWithoutAProviderChangesNothing(t *testing.T) {
	np, pod := newHookDriver(t, &fakeRuntimeProvider{})
	np.runtimeProvider = nil
	adjust, _, err := np.CreateContainer(context.Background(), pod, &api.Container{Id: "ctr-1", Name: "app"})
	if err != nil || len(adjust.GetHooks().GetCreateRuntime()) != 0 {
		t.Fatalf("adjustment without a provider = %v, %v", adjust, err)
	}
}

func TestRuntimeProviderStateFollowsTheSandboxAndTheClaims(t *testing.T) {
	provider := &fakeRuntimeProvider{}
	np, pod := newHookDriver(t, provider)
	podUID := types.UID(pod.Uid)
	complete := func() {
		t.Helper()
		createRuntimeHooks(t, np, pod, &api.Container{Id: fmt.Sprintf("ctr-%d", provider.calls()), Name: "app"})
		if err := np.handleRuntimeHook(context.Background(), fmt.Sprintf("ctr-%d", provider.calls())); err != nil {
			t.Fatal(err)
		}
		if hooks := createRuntimeHooks(t, np, pod, &api.Container{Id: "probe", Name: "app"}); len(hooks) != 0 {
			t.Fatalf("hooks after completion = %v", hooks)
		}
	}
	complete()
	// The kubelet replaces the sandbox: RunPodSandbox runs the hook again
	// even when DRANET's own configuration of the new sandbox fails, since
	// the kubelet retries that as well.
	if err := np.RunPodSandbox(context.Background(), pod); err == nil {
		t.Fatal("RunPodSandbox succeeded without the interfaces")
	}
	if hooks := createRuntimeHooks(t, np, pod, &api.Container{Id: "ctr-new-sandbox", Name: "app"}); len(hooks) != 1 {
		t.Fatalf("hooks after RunPodSandbox = %v, want one", hooks)
	}
	if err := np.handleRuntimeHook(context.Background(), "ctr-new-sandbox"); err != nil {
		t.Fatal(err)
	}
	for _, stop := range []func() error{
		func() error { return np.StopPodSandbox(context.Background(), pod) },
		func() error { return np.RemovePodSandbox(context.Background(), pod) },
	} {
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		if np.runtimeProvider.IsDone(podUID) {
			t.Fatal("the pod stayed done after its sandbox went away")
		}
		complete()
	}
	// Unpreparing the claims removes the Pod and everything the hook kept.
	np.netdb = newFakeInventoryDB()
	if _, err := np.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{NamespacedName: types.NamespacedName{Namespace: "team", Name: "claim-a"}}}); err != nil {
		t.Fatal(err)
	}
	if np.runtimeProvider.IsDone(podUID) {
		t.Fatal("the pod stayed done after its claims were unprepared")
	}
}

func TestBuildConfigurePodRequestWithoutClaimSnapshot(t *testing.T) {
	pod := &api.PodSandbox{Uid: "pod-uid", Name: "workload", Namespace: "team"}
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"nic": {Claim: types.NamespacedName{Namespace: "team", Name: "old-claim"}, NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: "eth9"}}},
	}}
	got := buildConfigurePodRequest(pod, "/var/run/netns/x", podConfig)
	want := &cloudprovider.ConfigurePodRequest{
		Pod:   cloudprovider.PodReference{Namespace: "team", Name: "workload", UID: "pod-uid"},
		NetNS: "/var/run/netns/x",
		Claims: []cloudprovider.PodClaim{{
			Claim:   &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "old-claim"}},
			Devices: []cloudprovider.PodDevice{{Name: "nic", Device: cloudprovider.DeviceIdentifiers{Name: "eth9"}, Config: &apis.NetworkConfig{}}},
		}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("request mismatch (-want +got):\n%s", diff)
	}
}

func TestClaimSnapshotStripsManagedFields(t *testing.T) {
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:     "team",
			Name:          "claim-a",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-apiserver"}},
		},
	}
	got := claimSnapshot(claim)
	if len(got.ManagedFields) != 0 {
		t.Fatalf("claimSnapshot kept ManagedFields: %+v", got.ManagedFields)
	}
	if len(claim.ManagedFields) != 1 {
		t.Fatal("claimSnapshot mutated the input claim")
	}
}

// TestRuntimeProviderEndToEnd runs the whole semaphore: the driver adjusts the
// container, the hook process (ocihook.Run, as the runtime would exec it)
// signals the daemon's socket, the driver runs the provider and answers.
func TestRuntimeProviderEndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("", "dranet-hook")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "hook.sock")

	provider := &fakeRuntimeProvider{}
	np, pod := newHookDriver(t, provider)
	WithRuntimeProvider(socket, ocihook.DefaultTimeout, provider)(np)
	np.bindRuntimeProviderPersistence()
	np.runtimeProvider.SetIdentity(ocihook.Identity{InitPIDNamespace: true, PID: hookTestPID, Args: []string{"/dranet"}})
	server, err := ocihook.Listen(socket, ocihook.DefaultTimeout, np.handleRuntimeHook)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()

	run := func(ctr *api.Container) (int, string) {
		t.Helper()
		hooks := createRuntimeHooks(t, np, pod, ctr)
		if len(hooks) != 1 {
			t.Fatalf("hooks = %v, want one", hooks)
		}
		var stderr bytes.Buffer
		code := ocihook.Run(hooks[0].Args[2:], strings.NewReader(`{"id":"`+ctr.Id+`","status":"creating"}`), &stderr)
		return code, stderr.String()
	}
	if code, stderr := run(&api.Container{Id: "ctr-1", Name: "app"}); code != ocihook.ExitSuccess {
		t.Fatalf("hook exit %d, stderr %q", code, stderr)
	}
	if provider.calls() != 1 || !cmp.Equal(wantConfigurePodRequest(), provider.requests[0]) {
		t.Fatalf("provider requests: %+v", provider.requests)
	}

	np.resetRuntimeProvider(types.UID(pod.Uid))
	provider.mu.Lock()
	provider.err = errors.New("switch port is down")
	provider.mu.Unlock()
	code, stderr := run(&api.Container{Id: "ctr-2", Name: "app"})
	if code != ocihook.ExitFailure || !strings.Contains(stderr, "switch port is down") || !strings.Contains(stderr, "ctr-2") {
		t.Fatalf("failed hook exit %d, stderr %q", code, stderr)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeProviderStatePersistsAcrossDaemonRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dranet.db")
	store1, err := NewPodConfigStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeRuntimeProvider{}
	np1, pod := newHookDriver(t, provider)
	// Copy prepared devices into the BoltDB-backed store.
	if origCfg, ok := np1.podConfigStore.GetPodConfig(types.UID(pod.Uid)); ok {
		for name, devCfg := range origCfg.DeviceConfigs {
			if err := store1.SetDeviceConfig(types.UID(pod.Uid), name, devCfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	np1.podConfigStore = store1
	np1.bindRuntimeProviderPersistence()

	createRuntimeHooks(t, np1, pod, &api.Container{Id: "init-1", Name: "init"})
	if err := np1.handleRuntimeHook(context.Background(), "init-1"); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a daemon restart after the init container already exited (so
	// Synchronize sees no CONTAINER_RUNNING container for this Pod).
	store2, err := NewPodConfigStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	np2 := &NetworkDriver{
		podConfigStore:  store2,
		eventRecorder:   record.NewFakeRecorder(10),
		runtimeProvider: runtimeprovider.New(hookTestSocket, ocihook.DefaultTimeout, provider),
	}
	np2.bindRuntimeProviderPersistence()
	np2.runtimeProvider.SetIdentity(ocihook.Identity{InitPIDNamespace: true, PID: hookTestPID, Args: []string{"/dranet"}})
	if _, err := np2.Synchronize(context.Background(), []*api.PodSandbox{pod}, []*api.Container{
		{Id: "init-1", PodSandboxId: pod.Id, State: api.ContainerState_CONTAINER_STOPPED},
	}); err != nil {
		t.Fatal(err)
	}

	// Because completion was persisted in the checkpoint, the app container
	// created after the restart does not carry the hook or re-run the provider.
	if hooks := createRuntimeHooks(t, np2, pod, &api.Container{Id: "app-1", Name: "app"}); len(hooks) != 0 {
		t.Fatalf("hooks after daemon restart for already-configured sandbox = %v, want none", hooks)
	}
	if provider.calls() != 1 {
		t.Fatalf("provider called %d times, want 1", provider.calls())
	}

	// Stopping the sandbox clears the persisted flag on disk too.
	if err := np2.RemovePodSandbox(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	store3, err := NewPodConfigStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store3.Close()
	np3 := &NetworkDriver{
		podConfigStore:  store3,
		eventRecorder:   record.NewFakeRecorder(10),
		runtimeProvider: runtimeprovider.New(hookTestSocket, ocihook.DefaultTimeout, provider),
	}
	np3.bindRuntimeProviderPersistence()
	np3.runtimeProvider.SetIdentity(ocihook.Identity{InitPIDNamespace: true, PID: hookTestPID, Args: []string{"/dranet"}})
	if hooks := createRuntimeHooks(t, np3, pod, &api.Container{Id: "new-sandbox-ctr", Name: "app"}); len(hooks) != 1 {
		t.Fatalf("hooks after sandbox removal and daemon restart = %v, want 1", hooks)
	}
	if err := np3.handleRuntimeHook(context.Background(), "new-sandbox-ctr"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantConfigurePodRequest(), provider.requests[1]); diff != "" {
		t.Fatalf("request from checkpointed claims mismatch (-want +got):\n%s", diff)
	}
}
