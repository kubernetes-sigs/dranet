---
title: "Metrics and Health Checks"
date: 2026-11-11T00:00:00Z
---

DRANET serves a health check and Prometheus metrics over HTTP on every node.
This page lists the endpoints and the metrics that DRANET exports, and explains
how to read them.

## Endpoints

Both endpoints are served on the address set with `--bind-address`, which
defaults to `:9177`. The DRANET DaemonSet uses the host network, so the
endpoints are reachable on port `9177` of each node's IP address.

| Path | Description |
|------|-------------|
| `/healthz` | Evaluates the logical AND of the DRA (`dra`) and NRI (`nri`) checks, returning `200 OK` (`ok`) only when both are healthy and `503 Service Unavailable` with per-check details otherwise. Supports `?verbose` and `?exclude=<check>` like Kubernetes apiserver healthz endpoints. The startup and readiness probes of the DaemonSet use it. |
| `/healthz/dra` | Returns `200 OK` when the DRA kubelet plugin is started and registered with kubelet, and `503 Service Unavailable` otherwise. |
| `/healthz/nri` | Returns `200 OK` when the NRI plugin is connected to the container runtime, and `503 Service Unavailable` otherwise. |
| `/metrics` | Prometheus metrics in the text exposition format. |

DRANET binds the address before it starts the driver. If the port is already in
use, for example by a DRANET pod that is still shutting down, the process exits
and the kubelet restarts it.

To scrape the metrics with the Prometheus Operator, install DRANET with the
Helm chart and set `prometheus.monitor.enabled=true` to create a `PodMonitor`.
See the [Helm chart README](https://github.com/kubernetes-sigs/dranet/blob/main/deployments/helm/dranet/README.md#configuration)
for this and for annotation-based scraping.

## Metrics

All DRANET metrics use the `dranet_driver_` prefix. The `/metrics` endpoint also
serves the standard Go runtime (`go_*`) and process (`process_*`) metrics of the
Prometheus client library.

### DRA requests

The kubelet calls DRANET through the DRA plugin API to prepare the
`ResourceClaims` of a pod before it starts and to unprepare them after it
stops.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dranet_driver_dra_plugin_requests_total` | Counter | `method`, `status` | Number of DRA requests handled. |
| `dranet_driver_dra_plugin_requests_latency_seconds` | Histogram | `method` | Time spent handling a DRA request. Uses the default Prometheus buckets. |

- `method` is `PrepareResourceClaims` or `UnprepareResourceClaims`.
- `status` is `success` or `failed`. A single request can carry several claims.
  The request counts as `failed` if any one of its claims fails.

### NRI requests

The container runtime calls DRANET through NRI at pod and container lifecycle
events. This is where DRANET moves the network interfaces into the pod network
namespace, configures them, and injects the RDMA character devices into the
containers.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dranet_driver_nri_plugin_requests_total` | Counter | `method`, `status` | Number of NRI requests handled. |
| `dranet_driver_nri_plugin_requests_latency_seconds` | Histogram | `method`, `status` | Time spent handling an NRI request. |

- `method` is `RunPodSandbox`, `CreateContainer`, `StopPodSandbox` or
  `RemovePodSandbox`.
- `status` is `success`, `failed` or `noop`. DRANET receives the events of
  every pod on the node. `noop` means that the pod has no devices allocated by
  DRANET, so there was nothing to do.

The container runtime fails or ignores an NRI request that takes longer than its
`plugin_request_timeout`. The NRI default is 2 seconds, and the containerd
configuration in
[`examples/dranetctl-install.yaml`](https://github.com/kubernetes-sigs/dranet/blob/main/examples/dranetctl-install.yaml)
sets it to 5 seconds. The latency histogram has buckets at `2` and
`5` seconds to count the requests that reach either limit, and at `1.5` and `3`
seconds to show requests that come close. The full list of buckets is `0.01`,
`0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `1.5`, `2`, `3`, `5`, `10` and
`30` seconds.

### Published devices

DRANET scans the node for network devices and publishes them in a
`ResourceSlice`. It scans again on every netlink link change, and at least once
every `--inventory-max-poll-interval`, which defaults to one minute.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dranet_driver_published_devices_total` | Gauge | `feature` | Number of devices in the last `ResourceSlice` update that DRANET attempted, after the `--filter` expression is applied. It is set even if the update fails. |
| `dranet_driver_last_published_time_seconds` | Gauge | none | Unix time of the last successful `ResourceSlice` update. |

- `feature` is `total` for all devices, or `rdma` for the devices with
  `dra.net/rdma: true`.

While a node has devices to publish, DRANET updates its `ResourceSlice` after
every scan, so `dranet_driver_last_published_time_seconds` advances at least
once per `--inventory-max-poll-interval`. A value that stops advancing on such a
node means that publishing fails, for example because the API server rejects
the `ResourceSlice`. When a node has no devices, DRANET does not update its
`ResourceSlice` again until a device appears, so the value stops advancing as
well. On a node where DRANET has never found a device,
`dranet_driver_last_published_time_seconds` stays `0` and
`dranet_driver_published_devices_total` is absent.

### Connection loops

DRANET runs its NRI plugin connection and its device inventory in loops that
DRANET restarts when they stop. When either loop fails 5 consecutive times, the
DRANET process exits and the kubelet restarts the container (for NRI, the
consecutive failure counter resets after each successful connection so routine
container runtime restarts do not crash DRANET). These counters reset when the
process restarts.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dranet_driver_nri_plugin_disconnects_total` | Counter | none | Number of times the container runtime closed an open NRI connection to DRANET, for example after a request timed out on a runtime that does not support the NRI failure policy. |
| `dranet_driver_nri_plugin_reconnect_attempts_total` | Counter | none | Number of times the NRI plugin loop stopped, for any reason. |
| `dranet_driver_netdb_reconnect_attempts_total` | Counter | none | Number of times the device inventory loop stopped, for any reason. |

`dranet_driver_nri_plugin_disconnects_total` counts only connections that were
open, while `dranet_driver_nri_plugin_reconnect_attempts_total` also counts
attempts that failed to connect or register. A disconnect can happen after
DRANET has already handled a request successfully, so it does not show up in
`dranet_driver_nri_plugin_requests_total{status="failed"}`.

### Pods without their devices

A pod can start without the network devices that it claims if the container
runtime creates its sandbox while the runtime does not have the DRANET NRI
plugin. That needs a fault on the node at that moment: DRANET is killed or
panics, or the container runtime restarts. A graceful stop of DRANET, for
example an update, is safe. The kubelet does not prepare the claims again for
such a pod, so nothing attaches the devices later.

DRANET cannot prevent this, because it is not connected when it happens. Each
time DRANET connects to the container runtime, it looks at the running pods that
it prepared, and it reports the ones that do not have a claimed network
interface in their network namespace:

- a `Warning` event with the reason `NetworkDeviceMissing` on the Pod;
- an error in the DRANET log;
- the metric below.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dranet_driver_pods_missing_devices_total` | Counter | none | Number of times DRANET found a running pod without a network device that the pod claims. |

Delete the pod to create it again with its devices. DRANET does not check the
devices that have no network interface in the pod, for example RDMA only
devices.

## Example queries

Failed NRI requests per second, by method:

```promql
sum by (method) (rate(dranet_driver_nri_plugin_requests_total{status="failed"}[5m]))
```

99th percentile `RunPodSandbox` latency, to compare with the runtime's
`plugin_request_timeout`:

```promql
histogram_quantile(0.99,
  sum by (le) (rate(dranet_driver_nri_plugin_requests_latency_seconds_bucket{method="RunPodSandbox", status!="noop"}[5m])))
```

Nodes that have devices to publish but have not updated their `ResourceSlice`
in the last 5 minutes:

```promql
(time() - dranet_driver_last_published_time_seconds > 300)
  and ignoring (feature) dranet_driver_published_devices_total{feature="total"} > 0
```

Nodes where a DRANET loop has stopped, so the process may exit soon:

```promql
dranet_driver_nri_plugin_reconnect_attempts_total > 0
  or dranet_driver_netdb_reconnect_attempts_total > 0
```

Nodes where DRANET found a pod that runs without a claimed network device in
the last hour:

```promql
increase(dranet_driver_pods_missing_devices_total[1h]) > 0
```
