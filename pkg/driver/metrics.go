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
		prometheus.MustRegister(nriPluginDisconnectsTotal)
		prometheus.MustRegister(nriPluginReconnectAttemptsTotal)
		prometheus.MustRegister(netdbReconnectAttemptsTotal)
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
	// nriPluginDisconnectsTotal counts every time containerd closes its ttrpc
	// connection to this plugin (stub.WithOnClose), e.g. after an NRI request
	// exceeds containerd's plugin_request_timeout. Distinct from
	// nri_plugin_requests_total{status="failed"}: a disconnect can happen after
	// the local handler already returned success, since containerd gives up and
	// tears down the connection independently of whether/how the handler finishes.
	// Also narrower than nriPluginReconnectAttemptsTotal below: stub.WithOnClose
	// is only wired once the ttrpc client exists, so this only fires for a
	// connection that was established and later torn down. It does not fire for
	// a retry caused by stub.connect() or stub.register() failing before that
	// point (e.g. containerd's NRI socket not up yet, or registration timing
	// out) -- see (*stub).Start() in github.com/containerd/nri/pkg/stub.
	nriPluginDisconnectsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_disconnects_total",
		Help:      "Total number of times containerd closed its NRI plugin connection to dranet (e.g. on plugin_request_timeout).",
	})
	// nriPluginReconnectAttemptsTotal and netdbReconnectAttemptsTotal count
	// restarts of each subsystem's loop in Start() (the NRI plugin connection,
	// and the host network device database, respectively), incremented
	// directly in the retry loop so every retry is counted regardless of why
	// Run() failed -- unlike nriPluginDisconnectsTotal above, which only
	// covers one specific failure mode on the NRI side. Both loops are capped
	// at maxAttempts before the process exits via klog.Fatalf, so either
	// value approaching maxAttempts is an early warning ahead of that exit,
	// whereas the container restart itself only becomes visible afterwards,
	// via kube_pod_container_status_restarts_total. Counters, not Gauges:
	// within a process's life these only ever increase, and they reset the
	// same way a Gauge would anyway -- on process restart.
	nriPluginReconnectAttemptsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_reconnect_attempts_total",
		Help:      "Total number of times the NRI plugin connection loop has been restarted. Resets on process restart; approaches maxAttempts before the process exits.",
	})
	netdbReconnectAttemptsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "netdb_reconnect_attempts_total",
		Help:      "Total number of times the host network device database's connection loop has been restarted. Resets on process restart; approaches maxAttempts before the process exits.",
	})
)
