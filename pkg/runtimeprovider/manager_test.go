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

package runtimeprovider

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
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ocihook"
)

type fakeProvider struct {
	mu       sync.Mutex
	requests []*cloudprovider.ConfigurePodRequest
	err      error
	block    chan struct{}
}

func (p *fakeProvider) ConfigurePod(ctx context.Context, req *cloudprovider.ConfigurePodRequest) error {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	block, err := p.block, p.err
	p.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return fmt.Errorf("provider interrupted: %w", ctx.Err())
		}
	}
	return err
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

type providerFunc func(context.Context, *cloudprovider.ConfigurePodRequest) error

func (f providerFunc) ConfigurePod(ctx context.Context, req *cloudprovider.ConfigurePodRequest) error {
	return f(ctx, req)
}

const (
	testPID    = 4242
	testSocket = "/var/run/dranet/hook.sock"
)

func newTestManager(providers ...cloudprovider.RuntimeProvider) *Manager {
	m := New(testSocket, ocihook.DefaultTimeout, providers...)
	m.SetIdentity(ocihook.Identity{InitPIDNamespace: true, PID: testPID, Args: []string{"/dranet"}})
	return m
}

func sampleRequest(podUID types.UID) func() (*cloudprovider.ConfigurePodRequest, error) {
	return func() (*cloudprovider.ConfigurePodRequest, error) {
		return &cloudprovider.ConfigurePodRequest{
			Pod:   cloudprovider.PodReference{Namespace: "team", Name: "workload", UID: podUID},
			NetNS: "/var/run/netns/cni-1234",
		}, nil
	}
}

func TestManagerRunsOncePerSandboxBeforeTheFirstContainer(t *testing.T) {
	provider := &fakeProvider{}
	m := newTestManager(provider)
	podUID := types.UID("pod-uid")

	hook, err := m.Adjustment(podUID, "ctr-1", sampleRequest(podUID))
	if err != nil || hook == nil {
		t.Fatalf("Adjustment(ctr-1) = %v, %v", hook, err)
	}
	want := &api.Hook{
		Path:    fmt.Sprintf("/proc/%d/exe", testPID),
		Args:    []string{"dranet", "oci-hook", "--socket=" + testSocket, "--container-id=ctr-1", "--timeout=28s"},
		Timeout: api.Int(30),
	}
	if diff := cmp.Diff(want, hook, cmp.Comparer(func(a, b *api.Hook) bool {
		return a.Path == b.Path && strings.Join(a.Args, " ") == strings.Join(b.Args, " ") && a.Timeout.GetValue() == b.Timeout.GetValue() && len(a.Env) == len(b.Env)
	})); diff != "" {
		t.Fatalf("hook mismatch (-want +got):\n%s", diff)
	}

	// Until the hook succeeds, a second container also carries its own hook.
	if hook2, err := m.Adjustment(podUID, "ctr-2", sampleRequest(podUID)); err != nil || hook2 == nil || !strings.Contains(strings.Join(hook2.Args, " "), "--container-id=ctr-2") {
		t.Fatalf("Adjustment(ctr-2) = %v, %v", hook2, err)
	}

	if _, outcome, err := m.Handle(context.Background(), "ctr-1"); err != nil || outcome != OutcomeSuccess {
		t.Fatalf("Handle(ctr-1) = %v, %v", outcome, err)
	}
	if provider.calls() != 1 {
		t.Fatalf("provider called %d times, want 1", provider.calls())
	}

	// Second container's hook is now a no-op, and subsequent containers get no hook.
	if _, outcome, err := m.Handle(context.Background(), "ctr-2"); err != nil || outcome != OutcomeNoop {
		t.Fatalf("Handle(ctr-2) = %v, %v", outcome, err)
	}
	if hook3, err := m.Adjustment(podUID, "ctr-3", sampleRequest(podUID)); err != nil || hook3 != nil {
		t.Fatalf("Adjustment(ctr-3) after completion = %v, %v", hook3, err)
	}

	// Resetting the sandbox runs the hook again.
	m.Reset(podUID)
	if hook4, err := m.Adjustment(podUID, "ctr-4", sampleRequest(podUID)); err != nil || hook4 == nil {
		t.Fatalf("Adjustment(ctr-4) after Reset = %v, %v", hook4, err)
	}
	if _, outcome, err := m.Handle(context.Background(), "ctr-4"); err != nil || outcome != OutcomeSuccess || provider.calls() != 2 {
		t.Fatalf("Handle(ctr-4) = %v, %v, calls=%d", outcome, err, provider.calls())
	}
}

func TestManagerMultipleProvidersRunInOrder(t *testing.T) {
	var order []string
	var secondErr error
	first := providerFunc(func(_ context.Context, _ *cloudprovider.ConfigurePodRequest) error {
		order = append(order, "cloud")
		return nil
	})
	second := providerFunc(func(_ context.Context, _ *cloudprovider.ConfigurePodRequest) error {
		order = append(order, "profile")
		return secondErr
	})
	m := newTestManager(first, second)
	podUID := types.UID("pod-uid")

	secondErr = errors.New("overlay not ready")
	if _, err := m.Adjustment(podUID, "ctr-1", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := m.Handle(context.Background(), "ctr-1"); err == nil || outcome != OutcomeFailed || !strings.Contains(err.Error(), "overlay not ready") {
		t.Fatalf("Handle when second fails = %v, %v", outcome, err)
	}
	if diff := cmp.Diff([]string{"cloud", "profile"}, order); diff != "" {
		t.Fatalf("call order (-want +got):\n%s", diff)
	}

	order = nil
	secondErr = nil
	if _, err := m.Adjustment(podUID, "ctr-2", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := m.Handle(context.Background(), "ctr-2"); err != nil || outcome != OutcomeSuccess {
		t.Fatalf("Handle on retry = %v, %v", outcome, err)
	}
	if diff := cmp.Diff([]string{"cloud", "profile"}, order); diff != "" {
		t.Fatalf("call order on retry (-want +got):\n%s", diff)
	}
}

func TestManagerHonorsDeadlineAndExpiresStalePendingHooks(t *testing.T) {
	provider := &fakeProvider{block: make(chan struct{})}
	defer close(provider.block)
	m := newTestManager(provider)
	podUID := types.UID("pod-uid")

	if _, err := m.Adjustment(podUID, "ctr-timeout", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := m.Handle(ctx, "ctr-timeout"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Handle with expired deadline = %v", err)
	}

	now := time.Now()
	m.SetClock(func() time.Time { return now })
	if _, err := m.Adjustment(podUID, "ctr-stale", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(PendingHookTTL + time.Second)
	if _, err := m.Adjustment(podUID, "ctr-fresh", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Handle(context.Background(), "ctr-stale"); err == nil || !strings.Contains(err.Error(), "no runtime provider call is pending") {
		t.Fatalf("Handle(ctr-stale) = %v", err)
	}
}

func TestManagerResolvesExecutableAndRunningPodsFromSynchronize(t *testing.T) {
	m := New(testSocket, ocihook.DefaultTimeout, &fakeProvider{})
	m.SetIdentity(ocihook.Identity{PID: 1, Args: []string{"/dranet", "--v=4"}, PodUID: "dranet-pod-uid", ContainerName: "dranet"})
	podUID := types.UID("workload-uid")

	if _, err := m.Adjustment(podUID, "ctr-1", sampleRequest(podUID)); err == nil || !strings.Contains(err.Error(), "not reported its container") {
		t.Fatalf("Adjustment before Synchronize = %v", err)
	}

	dranetPod := &api.PodSandbox{Id: "dranet-sb", Uid: "dranet-pod-uid"}
	runningPod := &api.PodSandbox{Id: "running-sb", Uid: "running-uid"}
	pendingPod := &api.PodSandbox{Id: "pending-sb", Uid: string(podUID)}
	self := &api.Container{Id: "dranet-ctr", PodSandboxId: "dranet-sb", Name: "dranet", State: api.ContainerState_CONTAINER_RUNNING, Pid: 31337, Args: []string{"/dranet", "--v=4"}}
	runningCtr := &api.Container{Id: "running-ctr", PodSandboxId: "running-sb", State: api.ContainerState_CONTAINER_RUNNING}
	m.Synchronize([]*api.PodSandbox{dranetPod, runningPod, pendingPod}, []*api.Container{self, runningCtr})

	if hook, err := m.Adjustment("running-uid", "ctr-restarted", sampleRequest("running-uid")); err != nil || hook != nil {
		t.Fatalf("Adjustment for already-running pod = %v, %v", hook, err)
	}
	hook, err := m.Adjustment(podUID, "ctr-1", sampleRequest(podUID))
	if err != nil || hook == nil || hook.Path != "/proc/31337/exe" {
		t.Fatalf("Adjustment after Synchronize = %v, %v, want /proc/31337/exe", hook, err)
	}
}

func TestFindSelfContainer(t *testing.T) {
	args := []string{"/dranet", "--v=4", "--hostname-override=node-1"}
	pods := []*api.PodSandbox{
		{Id: "sb-dranet", Uid: "uid-dranet"},
		{Id: "sb-other", Uid: "uid-other"},
	}
	running := func(id, sandbox, name string, pid uint32, args ...string) *api.Container {
		return &api.Container{Id: id, PodSandboxId: sandbox, Name: name, State: api.ContainerState_CONTAINER_RUNNING, Pid: pid, Args: args}
	}
	testCases := []struct {
		name       string
		identity   ocihook.Identity
		containers []*api.Container
		wantID     string
		wantErr    string
	}{
		{
			name:       "own pod and arguments",
			identity:   ocihook.Identity{Args: args, PodUID: "uid-dranet", ContainerName: "dranet"},
			containers: []*api.Container{running("other", "sb-other", "dranet", 10, args...), running("me", "sb-dranet", "dranet", 20, args...)},
			wantID:     "me",
		},
		{
			name:       "arguments only, when the pod is unknown",
			identity:   ocihook.Identity{Args: args},
			containers: []*api.Container{running("sidecar", "sb-dranet", "sidecar", 10, "/sidecar"), running("me", "sb-dranet", "dranet", 20, args...)},
			wantID:     "me",
		},
		{
			name:     "the container name decides between two with the same arguments",
			identity: ocihook.Identity{Args: args, PodUID: "uid-dranet", ContainerName: "dranet"},
			containers: []*api.Container{
				running("twin", "sb-dranet", "dranet-debug", 10, args...),
				running("me", "sb-dranet", "dranet", 20, args...),
			},
			wantID: "me",
		},
		{
			name:       "a stopped container with the same arguments is a previous instance",
			identity:   ocihook.Identity{Args: args, PodUID: "uid-dranet"},
			containers: []*api.Container{{Id: "old", PodSandboxId: "sb-dranet", State: api.ContainerState_CONTAINER_STOPPED, Pid: 10, Args: args}, running("me", "sb-dranet", "dranet", 20, args...)},
			wantID:     "me",
		},
		{
			name:       "a wrapper hides the process from the runtime",
			identity:   ocihook.Identity{Args: args, PodUID: "uid-dranet"},
			containers: []*api.Container{running("me", "sb-dranet", "dranet", 20, append([]string{"/tini", "--"}, args...)...)},
			wantErr:    "no running container with the arguments",
		},
		{
			name:       "the runtime does not know the pod",
			identity:   ocihook.Identity{Args: args, PodUID: "uid-missing"},
			containers: []*api.Container{running("me", "sb-dranet", "dranet", 20, args...)},
			wantErr:    "did not report Pod uid-missing",
		},
		{
			name:       "the runtime has no PID for the container",
			identity:   ocihook.Identity{Args: args, PodUID: "uid-dranet"},
			containers: []*api.Container{running("me", "sb-dranet", "dranet", 0, args...)},
			wantErr:    "no running container",
		},
		{
			name:       "two indistinguishable containers",
			identity:   ocihook.Identity{Args: args},
			containers: []*api.Container{running("a", "sb-dranet", "dranet", 10, args...), running("b", "sb-other", "dranet", 20, args...)},
			wantErr:    "2 running containers",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctr, err := FindSelfContainer(tc.identity, pods, tc.containers)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ctr.GetId() != tc.wantID {
				t.Fatalf("container = %s, want %s", ctr.GetId(), tc.wantID)
			}
		})
	}
}

func TestManagerEndToEndOverSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dranet-hook")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "hook.sock")

	provider := &fakeProvider{}
	m := newTestManager(provider)
	m.SetSocket(socket)
	server, err := ocihook.Listen(socket, ocihook.DefaultTimeout, func(ctx context.Context, containerID string) error {
		_, _, err := m.Handle(ctx, containerID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()

	podUID := types.UID("pod-uid")
	hook, err := m.Adjustment(podUID, "ctr-1", sampleRequest(podUID))
	if err != nil || hook == nil {
		t.Fatalf("Adjustment = %v, %v", hook, err)
	}
	var stderr bytes.Buffer
	if code := ocihook.Run(hook.Args[2:], strings.NewReader(`{"id":"ctr-1","status":"creating"}`), &stderr); code != ocihook.ExitSuccess {
		t.Fatalf("hook exit %d, stderr %q", code, stderr.String())
	}
	if provider.calls() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls())
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestResetWhileHandleInFlightDoesNotMarkNewSandboxDone(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	provider := providerFunc(func(_ context.Context, _ *cloudprovider.ConfigurePodRequest) error {
		close(inFlight)
		<-release
		return nil
	})
	m := newTestManager(provider)
	podUID := types.UID("pod-uid")

	if _, err := m.Adjustment(podUID, "ctr-old-sandbox", sampleRequest(podUID)); err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan error, 1)
	go func() {
		_, _, err := m.Handle(context.Background(), "ctr-old-sandbox")
		doneCh <- err
	}()
	<-inFlight
	// While the old sandbox's ConfigurePod is in flight, a new sandbox
	// starts and resets the Pod's state.
	m.Reset(podUID)
	close(release)
	if err := <-doneCh; err != nil {
		t.Fatal(err)
	}
	if m.IsDone(podUID) {
		t.Fatal("in-flight completion from an older sandbox marked the new sandbox done")
	}
}
