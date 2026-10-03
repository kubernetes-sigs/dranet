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
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	hookapi "sigs.k8s.io/dranet/pkg/apis/hook"
)

// The container runtime bounds every NRI request with its plugin request
// timeout (containerd plugin_request_timeout, 2s by default) and treats a late
// reply as a plugin failure: it disconnects the plugin and continues as if the
// request had succeeded, so a Pod would start without its devices. Attaching
// many devices can exceed that budget, so the attach runs as a background job
// per Pod. RunPodSandbox starts the job and waits on it for as long as its
// request budget allows. When it has to return before the job is done,
// CreateContainer adds an OCI createRuntime hook to the container (see package
// apis/hook); the runtime runs it when it starts the container and it blocks
// on hook.WaitPath until the job is done, with the hook timeout set from the
// Pod's attach deadline. CreateContainer also restarts a failed job until the
// deadline. The Pod's containers therefore never start with a subset of its
// devices.
//
// The kubelet serializes the NRI requests of a Pod, so jobs are only ever
// started by one request at a time; attachJobsMu guards the map and is never
// held while waiting.

// nriDeadlineMargin is the time reserved for the reply to reach the runtime
// before its request timer fires.
const nriDeadlineMargin = 200 * time.Millisecond

// requestBudget returns a context that ends nriDeadlineMargin before the
// request deadline of ctx, or when ctx ends if it has no deadline.
func requestBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-nriDeadlineMargin))
}

// attachJob attaches a Pod's pending devices in the background. The driver
// keeps the last job of a Pod, running or finished, until the sandbox is
// removed or replaced.
type attachJob struct {
	// deadline is when the attach must be complete; zero when it had to fit
	// in the RunPodSandbox request.
	deadline time.Time
	cancel   context.CancelFunc
	done     chan struct{}
	// Written before done is closed.
	pending int
	total   int
	err     error
}

// finished reports whether the job has ended.
func (j *attachJob) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// wait blocks until the job ends or ctx is done, and reports whether it ended.
func (j *attachJob) wait(ctx context.Context) bool {
	select {
	case <-j.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (np *NetworkDriver) getAttachJob(podUID types.UID) *attachJob {
	np.attachJobsMu.Lock()
	defer np.attachJobsMu.Unlock()
	return np.attachJobs[podUID]
}

func (np *NetworkDriver) deleteAttachJob(podUID types.UID) {
	np.attachJobsMu.Lock()
	defer np.attachJobsMu.Unlock()
	delete(np.attachJobs, podUID)
}

// startAttachJob starts attaching the Pod's pending devices to ns in the
// background. The job ends by itself at deadline, if not zero. The caller
// holds attachJobsMu and has made sure no job is running for the Pod.
func (np *NetworkDriver) startAttachJob(pod *api.PodSandbox, podConfig PodConfig, ns string, deadline time.Time) *attachJob {
	podUID := types.UID(pod.GetUid())
	logger := klog.LoggerWithValues(klog.Background(), "pod", klog.KRef(pod.Namespace, pod.Name), "podUID", pod.Uid, "attachJob", true)
	ctx := klog.NewContext(context.Background(), logger)
	var cancel context.CancelFunc
	if deadline.IsZero() {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	}
	job := &attachJob{deadline: deadline, cancel: cancel, done: make(chan struct{})}
	if np.attachJobs == nil {
		np.attachJobs = map[types.UID]*attachJob{}
	}
	np.attachJobs[podUID] = job

	go func() {
		defer cancel()
		defer close(job.done)
		attachJobsRunning.Inc()
		defer attachJobsRunning.Dec()
		start := time.Now()
		job.pending, job.total, job.err = np.attachDevices(ctx, pod, podConfig, ns)
		result := attachResultAttached
		if job.err != nil {
			result = attachResultFailed
		} else if job.pending > 0 {
			// Only a cancelled or expired context leaves devices pending.
			job.err = fmt.Errorf("attached %d/%d network devices of pod %s/%s: %w", job.total-job.pending, job.total, pod.GetNamespace(), pod.GetName(), ctx.Err())
			result = attachResultCancelled
		}
		podAttachDurationSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
		logger.V(2).Info("Attach job finished", "attached", job.total-job.pending, "total", job.total, "duration", time.Since(start), "err", job.err)
	}()
	return job
}

// stopAttachJob cancels the Pod's job, if running, and waits for it within
// the request budget of ctx. The job stops at the next device boundary; a
// device operation that outlives the budget finishes in the background and
// the kernel returns the device to the host when the namespace goes away.
func (np *NetworkDriver) stopAttachJob(ctx context.Context, podUID types.UID) {
	job := np.getAttachJob(podUID)
	if job == nil || job.finished() {
		return
	}
	job.cancel()
	waitCtx, cancel := requestBudget(ctx)
	defer cancel()
	if !job.wait(waitCtx) {
		klog.FromContext(ctx).Info("Attach job did not stop within the request budget, continuing without it", "podUID", podUID)
	}
}

// serveHookSocket serves hook.WaitPath on a unix socket at path until ctx ends.
func (np *NetworkDriver) serveHookSocket(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(hookapi.WaitPath, np.handleHookWait)
	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			klog.Errorf("hook socket server on %s stopped: %v", path, err)
		}
	}()
	klog.Infof("Serving the device attach hook on %s", path)
	return nil
}

// handleHookWait blocks until the Pod's devices are attached (200), the attach
// failed (500, body has the reason), or the client goes away (the runtime
// killed the hook at its timeout). It only reports on the job CreateContainer
// started: with no job, for example after a driver restart, the container
// start fails, the kubelet recreates the container and CreateContainer
// starts a new job.
func (np *NetworkDriver) handleHookWait(w http.ResponseWriter, r *http.Request) {
	podUID := types.UID(r.URL.Query().Get("pod"))
	if podUID == "" {
		http.Error(w, "missing pod query parameter", http.StatusBadRequest)
		return
	}
	logger := klog.LoggerWithValues(klog.FromContext(r.Context()), "podUID", podUID)
	logger.V(2).Info("Hook waiting for the pod's network devices")
	start := time.Now()
	respond := func(status int, err error) {
		hookWaitDurationSeconds.Observe(time.Since(start).Seconds())
		resp := hookapi.WaitResponse{PodUID: string(podUID)}
		if podConfig, ok := np.podConfigStore.GetPodConfig(podUID); ok {
			pending, total := np.pendingDevices(podConfig)
			resp.Attached, resp.Total = total-pending, total
		}
		if err != nil {
			hookWaitsTotal.WithLabelValues(hookWaitFailed).Inc()
			logger.Error(err, "Hook wait failed")
			resp.Error = err.Error()
		} else {
			hookWaitsTotal.WithLabelValues(hookWaitReleased).Inc()
			logger.V(2).Info("Hook released, all network devices attached")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.Error(err, "Failed to write the hook response")
		}
	}
	podConfig, ok := np.podConfigStore.GetPodConfig(podUID)
	if !ok {
		respond(http.StatusInternalServerError, fmt.Errorf("pod %s is not known to the driver", podUID))
		return
	}
	if pending, _ := np.pendingDevices(podConfig); pending == 0 {
		respond(http.StatusOK, nil)
		return
	}
	job := np.getAttachJob(podUID)
	if job == nil {
		respond(http.StatusInternalServerError, fmt.Errorf("no network device attach in progress for pod %s; the kubelet recreates the container and the attach resumes", podUID))
		return
	}
	if !job.wait(r.Context()) {
		hookWaitsTotal.WithLabelValues(hookWaitAbandoned).Inc()
		hookWaitDurationSeconds.Observe(time.Since(start).Seconds())
		logger.Info("Hook gave up waiting for the pod's network devices")
		return
	}
	if job.err != nil {
		respond(http.StatusInternalServerError, job.err)
		return
	}
	respond(http.StatusOK, nil)
}
