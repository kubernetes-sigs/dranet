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

// Package runtimeprovider coordinates the OCI createRuntime hooks that pause a
// Pod's first container while configured cloudprovider.RuntimeProvider
// implementations configure the Pod's network namespace.
package runtimeprovider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/set"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ocihook"
)

// PendingHookTTL is how long a container created by NRI may take to run its
// hook before DRANET forgets it. The runtime executes the hook as soon as the
// kubelet starts the container, normally within seconds.
const PendingHookTTL = 10 * time.Minute

// hookArgv0 is the name the hook process sees in its arguments.
const hookArgv0 = "dranet"

// Outcome describes how Handle finished for metrics and logging.
type Outcome int

const (
	OutcomeFailed Outcome = iota
	OutcomeSuccess
	OutcomeNoop
)

// Manager owns the OCI hook server and the per-Pod / per-container state for
// configured RuntimeProviders.
type Manager struct {
	providers []cloudprovider.RuntimeProvider
	socket    string
	timeout   time.Duration
	identity  ocihook.Identity
	clock     func() time.Time

	isDone  func(types.UID) bool
	setDone func(types.UID, bool) error

	mu sync.Mutex
	// executable is the path of the DRANET executable for the container
	// runtime, empty until known; executableErr says why it is not.
	executable    string
	executableErr error
	// pending holds the containers that carry the hook and have not run it
	// yet, by container ID.
	pending map[string]pendingHook
	// done holds the Pods whose hook succeeded for their current sandbox.
	done set.Set[types.UID]
	// generation increments whenever a Pod's sandbox state is reset so an
	// in-flight hook from a previous sandbox cannot mark the new one done.
	generation map[types.UID]uint64
}

type pendingHook struct {
	podUID  types.UID
	gen     uint64
	request *cloudprovider.ConfigurePodRequest
	created time.Time
}

// New creates a Manager that runs providers in order for each Pod sandbox, or
// returns nil when no providers are given.
func New(socket string, timeout time.Duration, providers ...cloudprovider.RuntimeProvider) *Manager {
	if len(providers) == 0 {
		return nil
	}
	return &Manager{
		providers:  providers,
		socket:     socket,
		timeout:    timeout,
		clock:      time.Now,
		pending:    map[string]pendingHook{},
		done:       set.New[types.UID](),
		generation: map[types.UID]uint64{},
	}
}

// SetPersistence wires optional checkpoint callbacks so completion state
// survives daemon restarts.
func (m *Manager) SetPersistence(isDone func(types.UID) bool, setDone func(types.UID, bool) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isDone = isDone
	m.setDone = setDone
}

// SetClock overrides the time source for testing TTL expiration.
func (m *Manager) SetClock(clock func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock = clock
}

// SetSocket overrides the UNIX socket path for testing.
func (m *Manager) SetSocket(socket string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.socket = socket
}

// Start learns how the container runtime can execute DRANET and serves OCI
// hooks with handler until ctx is done.
func (m *Manager) Start(ctx context.Context, handler ocihook.Handler) error {
	identity, err := ocihook.SelfIdentity()
	if err != nil {
		return fmt.Errorf("runtime provider: identify this process: %w", err)
	}
	m.SetIdentity(identity)
	server, err := ocihook.Listen(m.socket, m.timeout, handler)
	if err != nil {
		return fmt.Errorf("runtime provider: %w", err)
	}
	klog.Infof("Runtime provider: serving the OCI hooks on %s with a %v timeout, executable %s", m.socket, m.timeout, m.describeExecutable())
	go func() {
		if err := server.Serve(ctx); err != nil {
			klog.Fatalf("Runtime provider: the OCI hook server failed: %v", err)
		}
	}()
	return nil
}

// SetIdentity records what the daemon knows about itself and resolves the
// executable when the process is in the initial PID namespace, where its own
// PID is the one the runtime sees; otherwise Synchronize resolves it from the
// runtime's view of the containers.
func (m *Manager) SetIdentity(identity ocihook.Identity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.identity = identity
	if identity.InitPIDNamespace {
		m.executable = ocihook.HostExecutable(identity.PID)
		m.executableErr = nil
		return
	}
	m.executable = ""
	m.executableErr = fmt.Errorf("DRANET runs in its own PID namespace and the container runtime has not reported its container yet")
}

func (m *Manager) describeExecutable() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.executable != "" {
		return m.executable
	}
	return "unknown (" + m.executableErr.Error() + ")"
}

// Synchronize takes the runtime's view of the Pods and containers, on NRI
// registration: it finds the daemon's own container to learn the PID the
// runtime executes the hook with, and the Pods with a running container,
// whose hook ran before this daemon started.
func (m *Manager) Synchronize(pods []*api.PodSandbox, containers []*api.Container) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.identity.InitPIDNamespace {
		self, err := FindSelfContainer(m.identity, pods, containers)
		if err != nil {
			m.executable = ""
			m.executableErr = err
			klog.Errorf("Runtime provider: cannot determine how the container runtime can execute DRANET, container creation will fail for Pods with DRANET devices: %v", err)
		} else {
			m.executable = ocihook.HostExecutable(int(self.Pid))
			m.executableErr = nil
			klog.Infof("Runtime provider: DRANET is container %s with PID %d in the runtime, the OCI hooks execute %s", self.Id, self.Pid, m.executable)
		}
	}
	sandboxes := map[string]types.UID{}
	for _, pod := range pods {
		sandboxes[pod.GetId()] = types.UID(pod.GetUid())
	}
	for _, ctr := range containers {
		if ctr.GetState() != api.ContainerState_CONTAINER_RUNNING {
			continue
		}
		if podUID, ok := sandboxes[ctr.GetPodSandboxId()]; ok {
			m.done.Insert(podUID)
		}
	}
}

// FindSelfContainer returns the daemon's own container among the ones the
// runtime reports: a running container of the daemon's Pod, when the Pod is
// known, started with the daemon's arguments.
func FindSelfContainer(identity ocihook.Identity, pods []*api.PodSandbox, containers []*api.Container) (*api.Container, error) {
	sandboxes := set.New[string]()
	if identity.PodUID != "" {
		for _, pod := range pods {
			if pod.GetUid() == identity.PodUID {
				sandboxes.Insert(pod.GetId())
			}
		}
		if sandboxes.Len() == 0 {
			return nil, fmt.Errorf("the runtime did not report Pod %s, the Pod of this process", identity.PodUID)
		}
	}
	var candidates []*api.Container
	for _, ctr := range containers {
		if ctr.GetState() != api.ContainerState_CONTAINER_RUNNING || ctr.GetPid() == 0 {
			continue
		}
		if identity.PodUID != "" && !sandboxes.Has(ctr.GetPodSandboxId()) {
			continue
		}
		if !slices.Equal(ctr.GetArgs(), identity.Args) {
			continue
		}
		candidates = append(candidates, ctr)
	}
	if len(candidates) > 1 && identity.ContainerName != "" {
		candidates = slices.DeleteFunc(candidates, func(ctr *api.Container) bool { return ctr.GetName() != identity.ContainerName })
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return nil, fmt.Errorf("the runtime reported no running container with the arguments %q of this process (Pod %q, container %q); the DRANET container must run the dranet executable directly", identity.Args, identity.PodUID, identity.ContainerName)
	default:
		ids := make([]string, 0, len(candidates))
		for _, ctr := range candidates {
			ids = append(ids, ctr.GetId())
		}
		return nil, fmt.Errorf("the runtime reported %d running containers with the arguments of this process: %s", len(candidates), strings.Join(ids, ", "))
	}
}

// Reset forgets what the hook did for a Pod when its sandbox starts, stops or
// goes away: a new sandbox runs the hook again.
func (m *Manager) Reset(podUID types.UID) {
	m.mu.Lock()
	m.generation[podUID]++
	m.done.Delete(podUID)
	for id, p := range m.pending {
		if p.podUID == podUID {
			delete(m.pending, id)
		}
	}
	setDone := m.setDone
	m.mu.Unlock()
	if setDone != nil {
		if err := setDone(podUID, false); err != nil {
			klog.Errorf("Runtime provider: failed to reset checkpoint state for pod %s: %v", podUID, err)
		}
	}
}

// IsDone reports whether the Pod's current sandbox has already completed its
// runtime provider hook.
func (m *Manager) IsDone(podUID types.UID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done.Has(podUID) {
		return true
	}
	if m.isDone != nil && m.isDone(podUID) {
		m.done.Insert(podUID)
		return true
	}
	return false
}

// PendingCount returns the number of containers currently waiting to run their
// OCI hook.
func (m *Manager) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// Adjustment returns the OCI hook to add to a container being created in pod,
// or nil when the Pod's hook already ran for this sandbox. buildRequest is
// called only when the hook is needed.
func (m *Manager) Adjustment(podUID types.UID, containerID string, buildRequest func() (*cloudprovider.ConfigurePodRequest, error)) (*api.Hook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expirePendingLocked()
	if m.isDone != nil && m.isDone(podUID) {
		m.done.Insert(podUID)
	}
	if m.done.Has(podUID) {
		return nil, nil
	}
	if m.executable == "" {
		return nil, fmt.Errorf("runtime provider: %w", m.executableErr)
	}
	if containerID == "" {
		return nil, fmt.Errorf("runtime provider: the runtime did not provide the container ID")
	}
	req, err := buildRequest()
	if err != nil {
		return nil, err
	}
	m.pending[containerID] = pendingHook{
		podUID:  podUID,
		gen:     m.generation[podUID],
		request: req,
		created: m.clock(),
	}
	return &api.Hook{
		Path:    m.executable,
		Args:    append([]string{hookArgv0}, ocihook.Args(m.socket, containerID, m.timeout)...),
		Timeout: api.Int(int(m.timeout.Seconds())),
	}, nil
}

func (m *Manager) expirePendingLocked() {
	now := m.clock()
	for id, p := range m.pending {
		if now.Sub(p.created) > PendingHookTTL {
			delete(m.pending, id)
		}
	}
}

func (m *Manager) claim(containerID string) (pendingHook, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[containerID]
	if !ok {
		return pendingHook{}, false, fmt.Errorf("no runtime provider call is pending for container %s: DRANET restarted since the container was created, or the container took over %v to start", containerID, PendingHookTTL)
	}
	delete(m.pending, containerID)
	if !m.done.Has(p.podUID) && m.isDone != nil && m.isDone(p.podUID) {
		m.done.Insert(p.podUID)
	}
	return p, m.done.Has(p.podUID), nil
}

func (m *Manager) complete(podUID types.UID, gen uint64) {
	m.mu.Lock()
	if m.generation[podUID] != gen {
		m.mu.Unlock()
		return
	}
	m.done.Insert(podUID)
	setDone := m.setDone
	m.mu.Unlock()
	if setDone != nil {
		if err := setDone(podUID, true); err != nil {
			klog.Errorf("Runtime provider: failed to checkpoint completion for pod %s: %v", podUID, err)
		}
	}
}

// Handle serves one hook process: it claims the container's pending request,
// runs each configured RuntimeProvider in order, and returns the Pod reference,
// outcome, and any error.
func (m *Manager) Handle(ctx context.Context, containerID string) (cloudprovider.PodReference, Outcome, error) {
	p, done, err := m.claim(containerID)
	if err != nil {
		klog.Errorf("Runtime provider: OCI hook of container %s rejected: %v", containerID, err)
		return cloudprovider.PodReference{}, OutcomeFailed, err
	}
	pod := p.request.Pod
	logger := klog.LoggerWithValues(klog.Background(), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.UID, "container", containerID)
	if done {
		logger.V(2).Info("Runtime provider already configured the pod, letting the container start")
		return pod, OutcomeNoop, nil
	}
	start := time.Now()
	logger.V(2).Info("Calling runtime provider(s)", "providers", len(m.providers), "netns", p.request.NetNS, "claims", len(p.request.Claims))
	for _, provider := range m.providers {
		if err := provider.ConfigurePod(ctx, p.request); err != nil {
			err = fmt.Errorf("runtime provider failed for pod %s/%s: %w", pod.Namespace, pod.Name, err)
			logger.Error(err, "Runtime provider failed, the container will not start", "duration", time.Since(start))
			return pod, OutcomeFailed, err
		}
	}
	m.complete(pod.UID, p.gen)
	logger.V(2).Info("Runtime provider configured the pod", "duration", time.Since(start))
	return pod, OutcomeSuccess, nil
}
