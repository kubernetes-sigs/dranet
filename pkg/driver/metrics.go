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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	statusSuccess = "success"
	statusFailed  = "failed"
	statusNoop    = "noop"
)

const (
	methodPrepareResourceClaims   = "PrepareResourceClaims"
	methodUnprepareResourceClaims = "UnprepareResourceClaims"
	methodRunPodSandbox           = "RunPodSandbox"
	methodStopPodSandbox          = "StopPodSandbox"
	methodRemovePodSandbox        = "RemovePodSandbox"
	methodCreateContainer         = "CreateContainer"
	methodStartContainer          = "StartContainer"
)

// Results of attaching a Pod's devices, as seen by RunPodSandbox and by the
// attach job; see sandboxAttachTotal and podAttachDurationSeconds.
const (
	attachResultAttached  = "attached"
	attachResultDeferred  = "deferred"
	attachResultRejected  = "rejected"
	attachResultFailed    = "failed"
	attachResultCancelled = "cancelled"
)

// Results of an OCI hook barrier wait; see hookWaitsTotal.
const (
	hookWaitReleased  = "released"
	hookWaitFailed    = "failed"
	hookWaitAbandoned = "abandoned"
)

// Kinds of OCI createRuntime hooks added to containers; see containerHooksTotal.
const (
	hookTypeBarrier  = "barrier"
	hookTypeProvider = "provider"
)

var registerMetricsOnce sync.Once

func registerMetrics() {
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(draPluginRequestsTotal)
		prometheus.MustRegister(draPluginRequestsLatencySeconds)
		prometheus.MustRegister(nriPluginRequestsTotal)
		prometheus.MustRegister(nriPluginRequestsLatencySeconds)
		prometheus.MustRegister(publishedDevicesTotal)
		prometheus.MustRegister(lastPublishedTime)
		prometheus.MustRegister(nriRequestTimeoutSeconds)
		prometheus.MustRegister(deviceAttachDurationSeconds)
		prometheus.MustRegister(podAttachDurationSeconds)
		prometheus.MustRegister(sandboxAttachTotal)
		prometheus.MustRegister(attachJobsRunning)
		prometheus.MustRegister(attachDeadlineExceededTotal)
		prometheus.MustRegister(hookWaitsTotal)
		prometheus.MustRegister(hookWaitDurationSeconds)
		prometheus.MustRegister(containerHooksTotal)
		prometheus.MustRegister(runtimeHooksCompletedTotal)
	})
}

var (
	draPluginRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "dra_plugin_requests_total",
		Help:      "Total number of DRA plugin requests.",
	}, []string{"method", "status"})
	draPluginRequestsLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "dra_plugin_requests_latency_seconds",
		Help:      "DRA plugin request latency in seconds.",
	}, []string{"method"})
	nriPluginRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_requests_total",
		Help:      "Total number of NRI plugin requests.",
	}, []string{"method", "status"})
	nriPluginRequestsLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_requests_latency_seconds",
		Help:      "NRI plugin request latency in seconds.",
	}, []string{"method", "status"})
	publishedDevicesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "published_devices_total",
		Help:      "Total number of published devices.",
	}, []string{"feature"})
	lastPublishedTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "last_published_time_seconds",
		Help:      "The timestamp of the last successful resource publication.",
	})

	// Device attach and the OCI hook barrier. Attaching a Pod's devices has to
	// fit in the container runtime's NRI request timeout; these metrics show
	// how close a node is to that limit and what happened when it was crossed.
	nriRequestTimeoutSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_request_timeout_seconds",
		Help: "The container runtime's NRI plugin request timeout, taken from the deadline of the last RunPodSandbox request " +
			"(containerd plugin_request_timeout, 2s by default). Attaching all the devices of a Pod has to fit in it: " +
			"compare it with dranet_driver_device_attach_duration_seconds multiplied by the devices a Pod claims, and raise it in the runtime configuration when they are close.",
	})
	deviceAttachDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "device_attach_duration_seconds",
		Help: "Time to move one network device into a Pod's network namespace and configure it, by result (attached, failed). " +
			"The sum over a Pod's devices must stay below dranet_driver_nri_request_timeout_seconds, or RunPodSandbox defers the attach.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"result"})
	podAttachDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "pod_attach_duration_seconds",
		Help: "Time an attach job took to attach the pending network devices of a Pod, by result: attached, " +
			"failed (a device error; the Pod's events have it), cancelled (the sandbox was stopped or --device-attach-timeout passed first). " +
			"Values above dranet_driver_nri_request_timeout_seconds are Pods whose containers waited at the OCI hook barrier; raise the runtime's plugin_request_timeout to avoid it.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60},
	}, []string{"result"})
	sandboxAttachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "sandbox_attach_total",
		Help: "RunPodSandbox requests for Pods with network devices, by result: attached (every device attached within the request), " +
			"deferred (the request ran out of time; the attach continued and the containers waited through the OCI hook), " +
			"rejected (the request ran out of time with --device-attach-timeout=0; the sandbox failed and the kubelet recreated it), " +
			"failed (a device did not attach; the sandbox failed). " +
			"deferred and rejected mean the runtime's plugin_request_timeout is too short for the devices of this node: raise it.",
	}, []string{"result"})
	attachJobsRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "attach_jobs_running",
		Help:      "Pods whose network devices are being attached right now.",
	})
	attachDeadlineExceededTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "attach_deadline_exceeded_total",
		Help: "Container creations refused because the Pod's network devices were not attached within --device-attach-timeout of the sandbox creation. " +
			"The kubelet keeps retrying and the Pod does not start: delete the Pod to start over, and look at its NetworkDeviceAttachFailed event for the device error.",
	})
	hookWaitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "hook_waits_total",
		Help: "Waits of the OCI hook barrier (dranet-hook) for a Pod's devices, by result: released (all devices attached, the container started), " +
			"failed (the attach failed; the container start failed with the reason and the kubelet replaced the container), " +
			"abandoned (the runtime killed the hook at its timeout, the Pod's remaining attach time, before the attach ended). " +
			"failed and abandoned both come with a NetworkDeviceAttach event on the Pod.",
	}, []string{"result"})
	hookWaitDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "hook_wait_duration_seconds",
		Help: "Time containers spent at the OCI hook barrier waiting for the Pod's network devices. " +
			"This is start latency added to the Pod; raise the runtime's plugin_request_timeout so the attach completes in RunPodSandbox and no container waits.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60},
	})
	containerHooksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "container_hooks_total",
		Help: "OCI createRuntime hooks added to containers, by type: barrier (dranet-hook, added when the Pod's devices were still being attached) " +
			"and provider (a profile provider's runtime hook, added to a Pod's containers until one of them starts). " +
			"A hook is counted on every container it is added to, so kubelet retries count again.",
	}, []string{"type"})
	runtimeHooksCompletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "runtime_hooks_completed_total",
		Help: "Pods whose profile provider runtime hooks ran: a container carrying them reached its start. " +
			"When this lags behind dranet_driver_container_hooks_total{type=\"provider\"}, the hooks fail or time out; the Pod's Failed events have the hook's stderr.",
	})
)
