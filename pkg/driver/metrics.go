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
	attachResultAttached = "attached"
	attachResultFailed   = "failed"
	attachResultRejected = "rejected_timeout"
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
		prometheus.MustRegister(nriRequestTimeoutSeconds)
		prometheus.MustRegister(deviceAttachDurationSeconds)
		prometheus.MustRegister(sandboxAttachTotal)
		prometheus.MustRegister(publishedDevicesTotal)
		prometheus.MustRegister(lastPublishedTime)
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
	nriRequestTimeoutSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "nri_request_timeout_seconds",
		Help:      "The effective NRI plugin request timeout in seconds observed on incoming runtime requests.",
	})
	deviceAttachDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "device_attach_duration_seconds",
		Help:      "Time in seconds to move and configure a single network device in a pod namespace.",
		Buckets:   []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"result"})
	sandboxAttachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "sandbox_attach_total",
		Help:      "Total number of pod sandboxes by network device attachment outcome.",
	}, []string{"result"})
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
)
