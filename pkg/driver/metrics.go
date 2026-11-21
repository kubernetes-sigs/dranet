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
		// The runtime fails or ignores a request that exceeds its
		// plugin_request_timeout (NRI default 2s, dranet installers set 5s).
		// Buckets at 2 and 5 count those requests exactly, and 1.5 and 3
		// show requests that approach either limit.
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 3, 5, 10, 30},
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
	// nriPluginDisconnectsTotal counts containerd closing an already-open
	// ttrpc connection (stub.WithOnClose), e.g. on plugin_request_timeout
	// with a runtime that does not support the NRI failure policy
	// (stub.WithFailurePolicy in driver.go). A
	// strict subset of nriPluginReconnectAttemptsTotal below: it only fires
	// once a connection exists, so it misses retries from a failed
	// stub.connect() or stub.register() (socket not up yet, registration
	// timeout). Also distinct from nri_plugin_requests_total{status="failed"}:
	// a disconnect can happen after the handler already returned success.
	nriPluginDisconnectsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_disconnects_total",
		Help:      "Total number of times containerd closed its NRI plugin connection to dranet (e.g. on plugin_request_timeout).",
	})
	// nriPluginReconnectAttemptsTotal and netdbReconnectAttemptsTotal count
	// every retry of each subsystem's loop in Start(), regardless of cause --
	// a superset of nriPluginDisconnectsTotal above. Consecutive failures are
	// capped at maxAttempts before klog.Fatalf; for NRI, the failure streak
	// resets after each synchronized session so routine runtime restarts do not
	// crash the driver.
	nriPluginReconnectAttemptsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_plugin_reconnect_attempts_total",
		Help:      "Total number of times the NRI plugin connection loop has been retried.",
	})
	netdbReconnectAttemptsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "netdb_reconnect_attempts_total",
		Help:      "Total number of times the host network device database's connection loop has been restarted. Resets on process restart; approaches maxAttempts before the process exits.",
	})
)
