---
title: "NIXL KV Transfer"
date: 2026-06-25T00:00:00Z
---

This example demonstrates topologically-aware GPU + RDMA NIC allocation using Kubernetes Dynamic Resource Allocation (DRA). The workload uses [NIXL](https://github.com/ai-dynamo/nixl) over UCX/RDMA to copy a GPU-resident buffer between two pods running on two GPU nodes. The buffer is sized like an inference KV-cache handoff, so the result isolates the RDMA transfer path used by disaggregated prefill/decode serving without requiring a full vLLM router/model stack.

Both GPUs and NICs are allocated through DRA: GPUs via the `gpu.nvidia.com` DeviceClass and RDMA NICs via DRANET's `dranet.net` DeviceClass. By keeping the same 4-GPU set across runs and changing only the NIC NUMA placement, the example measures how same-NUMA versus cross-NUMA GPU/NIC alignment affects KV transfer bandwidth and latency.

Full source: [examples/nixl-kv-transfer](https://github.com/kubernetes-sigs/dranet/tree/main/examples/nixl-kv-transfer).

## Tested topology

The included templates were tested on two GPU nodes with:

| Resource | Count | Detail |
|---|---:|---|
| GPU | 8 x NVIDIA H100 | 80 GB HBM3 each |
| NIC | 8 x Mellanox ConnectX VF | RDMA-capable |
| NUMA nodes | 2 | 4 GPU + 4 NIC per NUMA node |

The manifests are intentionally cloud-provider agnostic. The checked-in `ResourceClaimTemplate` values match the tested 8-GPU H100 topology; adapt the GPU `pciBusID` and NIC `numaNode` selectors to your hardware before running on another SKU. Some GPU DRA drivers publish GPU `pciBusID` but not GPU `numaNode`, so this example selects GPUs by `pciBusID`; DRANET publishes NIC `numaNode` directly.

## ResourceClaimTemplates

The core DRA manifests are two `ResourceClaimTemplate`s. Both keep compute fixed (the same 4 GPUs on NUMA 0) and keep the aggregate NIC count fixed (4 NICs). The only intended difference is whether each visible GPU reaches a same-NUMA or remote-NUMA NIC.

| Template | GPU selection | NIC selection | Purpose |
|---|---|---|---|
| `h100-4gpu-4nic-numa-aligned` | 4 GPUs on NUMA 0 | 4 NICs on NUMA 0 | Same-NUMA GPU/NIC path |
| `h100-4gpu-4nic-numa-unaligned` | Same 4 GPUs on NUMA 0 | 4 NICs on NUMA 1 | Cross-NUMA GPU/NIC path |

`resource-claim-template-aligned.yaml` selects NUMA-aligned GPUs and NICs:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: h100-4gpu-4nic-numa-aligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 4
          selectors:
          - cel:
              expression: 'device.attributes["resource.kubernetes.io"]["pciBusID"] in ["0001:00:00.0","0002:00:00.0","0003:00:00.0","0008:00:00.0"]'
      - name: nic
        exactly:
          deviceClassName: dranet.net
          count: 4
          selectors:
          - cel:
              expression: 'device.attributes["dra.net"]["rdma"] == true && device.attributes["dra.net"]["numaNode"] == 0'
```

`resource-claim-template-unaligned.yaml` uses the same GPUs but cross-NUMA NICs (note `numaNode == 1`):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: h100-4gpu-4nic-numa-unaligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 4
          selectors:
          - cel:
              expression: 'device.attributes["resource.kubernetes.io"]["pciBusID"] in ["0001:00:00.0","0002:00:00.0","0003:00:00.0","0008:00:00.0"]'
      - name: nic
        exactly:
          deviceClassName: dranet.net
          count: 4
          selectors:
          - cel:
              expression: 'device.attributes["dra.net"]["rdma"] == true && device.attributes["dra.net"]["numaNode"] == 1'
```

## Run

Apply everything via kustomize. This creates both `ResourceClaimTemplate`s, the `nixl-benchmark` ConfigMap (generated from `nixl_benchmark.py` and `run_bench.sh`), the headless Service, and both pods:

```bash
kubectl apply -k .
```

The pods default to the `h100-4gpu-4nic-numa-aligned` template. The initiator and target manifests, the headless Service, the benchmark, and the run script are in the [example directory](https://github.com/kubernetes-sigs/dranet/tree/main/examples/nixl-kv-transfer). The initiator log contains one `RESULT` JSON object with `avg_GBps`, `avg_seconds`, and the `p50`/`p95`/`p99` latencies.

## Verify allocation

Confirm that only the allocated RDMA devices are visible inside each pod:

```bash
kubectl get resourceclaims -o yaml | grep -E 'name:|device:|driver:|request:'
kubectl exec nixl-kv-initiator -- ls /dev/infiniband
kubectl exec nixl-kv-target -- ls /dev/infiniband
```

In the aligned case the visible NICs are NODE-local to the selected GPUs; in the unaligned case they are SYS/cross-NUMA.

## Benchmark results

Observed on the tested 8 x H100 node topology with 1 GiB NIXL `WRITE` transfers, 20 warmup iterations, and 100 timed iterations per run (three-run mean). Full results are in the [example README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/nixl-kv-transfer/README.md).

| Template | NICs | Avg bandwidth | Avg latency |
|---|---|---:|---:|
| `h100-4gpu-4nic-numa-aligned` | `mlx5_0`..`mlx5_3` | 39.07 GB/s | 27.49 ms |
| `h100-4gpu-4nic-numa-unaligned` | `mlx5_4`..`mlx5_7` | 27.54 GB/s | 38.99 ms |

Same GPUs, same NIC count, same transfer size: same-NUMA GPU/NIC allocation delivers about 1.42x higher bandwidth and about 29.5% lower latency for this transfer. This is the inference KV-cache handoff path, so the bandwidth gap surfaces as decode tail latency under concurrency.
