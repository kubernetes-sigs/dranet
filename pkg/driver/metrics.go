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
		prometheus.MustRegister(runtimeProviderRequestsTotal)
		prometheus.MustRegister(runtimeProviderLatencySeconds)
	})
}

var (
	runtimeProviderRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "runtime_provider_requests_total",
		Help:      "Runtime provider calls made before a Pod's first container started, by outcome. Failures fail the container start and the kubelet retries.",
	}, []string{"status"})
	runtimeProviderLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "dranet",
		Subsystem: "driver",
		Name:      "runtime_provider_latency_seconds",
		Help:      "Time the runtime provider held a container's start, in seconds.",
		Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 20, 30, 45, 60},
	}, []string{"status"})
)

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
)
