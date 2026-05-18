# NIXL KV Transfer + dranet Example

End-to-end example of topologically-aware GPU + RDMA NIC allocation with
Kubernetes Dynamic Resource Allocation (DRA). The workload uses NIXL over
UCX/RDMA to copy a GPU-resident buffer between two pods on two GPU nodes. The
buffer is sized like an inference KV-cache handoff, so the result isolates the
transfer path used by disaggregated prefill/decode serving without requiring a
full vLLM router/model stack.

Both GPUs and NICs are allocated via DRA (`gpu.nvidia.com` + `dra.net`). The
same 4-GPU set is used in both runs; only the NIC NUMA placement changes.

The manifests are intentionally cloud-provider agnostic. The checked-in
`ResourceClaimTemplate` values match the tested 8-GPU H100 topology; adapt the
GPU `pciBusID` and NIC `numaNode` selectors to your hardware before running on
another SKU.

## Tested Topology

The included templates were tested on two GPU nodes with:

| Resource | Count | Detail |
|---|---:|---|
| GPU | 8 x NVIDIA H100 | 80 GB HBM3 each |
| NIC | 8 x Mellanox ConnectX VF | RDMA-capable |
| NUMA nodes | 2 | 4 GPU + 4 NIC per NUMA node |

Default mapping used by the templates:

| NUMA | GPUs | NICs |
|---:|---|---|
| 0 | `0001:00:00.0`, `0002:00:00.0`, `0003:00:00.0`, `0008:00:00.0` | `mlx5_0`..`mlx5_3` |
| 1 | `0009:00:00.0`, `000a:00:00.0`, `000b:00:00.0`, `000c:00:00.0` | `mlx5_4`..`mlx5_7` |

Verify the mapping on your cluster before running. Some GPU DRA drivers publish
GPU `pciBusID` but not GPU `numaNode`, so this example selects GPUs by
`pciBusID`; dranet publishes NIC `numaNode` directly.

## Prerequisites

- A Kubernetes cluster with at least two GPU nodes connected by RDMA.
- dranet running on those nodes and publishing NIC devices through a DRA
  `DeviceClass`, typically `dranet.net` or your cluster's equivalent.
- A GPU DRA driver publishing GPU devices through `gpu.nvidia.com` or your
  cluster's equivalent GPU `DeviceClass`.
- The NRI device-injection path enabled so only the DRA-allocated RDMA devices
  are visible inside the benchmark pods.

Inspect published devices before editing the templates:

```bash
kubectl get resourceslices
kubectl get deviceclasses
```

## Files

| File | Description |
|---|---|
| `resource-claim-template-aligned.yaml` | `ResourceClaimTemplate` selecting NUMA-aligned GPUs + NICs |
| `resource-claim-template-unaligned.yaml` | `ResourceClaimTemplate` selecting cross-NUMA GPUs + NICs |
| `nixl-kv-service.yaml` | Headless Service used for the NIXL side-channel handshake |
| `nixl-kv-target.yaml` | Target Pod; registers GPU memory and waits for the initiator |
| `nixl-kv-initiator.yaml` | Initiator Pod; posts the NIXL transfers and prints `RESULT` |
| `nixl_benchmark.py` | Python NIXL benchmark mounted into the pods through a ConfigMap |
| `run_bench.sh` | Pod entrypoint: installs `nixl` and execs the benchmark |
| `kustomization.yaml` | Bundles the templates, pods, Service, and ConfigMap |

## ResourceClaimTemplates

| Template | GPU selection | NIC selection | Purpose |
|---|---|---|---|
| `h100-4gpu-4nic-numa-aligned` | 4 GPUs on NUMA 0 | 4 NICs on NUMA 0 | Same-NUMA GPU/NIC path |
| `h100-4gpu-4nic-numa-unaligned` | Same 4 GPUs on NUMA 0 | 4 NICs on NUMA 1 | Cross-NUMA GPU/NIC path |

The two templates keep compute fixed and keep aggregate NIC count fixed. The
only intended difference is whether each visible GPU reaches a same-NUMA or
remote-NUMA NIC.

If your cluster uses a different NIC `DeviceClass` name, update
`deviceClassName: dranet.net` in both `resource-claim-template-aligned.yaml`
and `resource-claim-template-unaligned.yaml`. If your GPU DRA driver publishes
a reliable GPU NUMA attribute, you can replace the `pciBusID` selector with a
NUMA selector.

## Run

Apply everything via kustomize. This creates both `ResourceClaimTemplate`s, the
`nixl-benchmark` ConfigMap (generated from `nixl_benchmark.py` + `run_bench.sh`),
the headless Service, and both pods:

```bash
kubectl apply -k .
```

`ResourceClaimTemplate.spec` is immutable. If you already created templates with
the same names and need to change their selectors, delete the old templates only
after confirming no active pods are using claims derived from them.

The pods default to the `h100-4gpu-4nic-numa-aligned` template. To run each
placement case three times, swap the template name in the pod manifests with
`sed` between runs. The manifest defaults to a 1 GiB transfer, 20 warmup
iterations, and 100 timed iterations per run.

```bash
for run in 1 2 3; do
  for tpl in h100-4gpu-4nic-numa-aligned h100-4gpu-4nic-numa-unaligned; do
    echo "=== run ${run}: ${tpl} ==="

    for pod in nixl-kv-target.yaml nixl-kv-initiator.yaml; do
      sed "s/resourceClaimTemplateName:.*/resourceClaimTemplateName: ${tpl}/" \
        "${pod}" | kubectl apply -f -
    done
    kubectl apply -f nixl-kv-service.yaml

    kubectl wait --for=condition=Ready \
      pod/nixl-kv-target pod/nixl-kv-initiator \
      --timeout=15m

    kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
      pod/nixl-kv-initiator --timeout=15m
    kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
      pod/nixl-kv-target --timeout=15m

    kubectl logs pod/nixl-kv-initiator | tee "results-run${run}-${tpl}.txt"
    kubectl logs pod/nixl-kv-target >> "results-run${run}-${tpl}.txt"

    kubectl delete pod/nixl-kv-target pod/nixl-kv-initiator svc/nixl-kv-target \
      --ignore-not-found --wait=true
  done
done
```

The initiator log contains one `RESULT` JSON object. The key fields are
`avg_GBps`, `avg_seconds`, `p50_seconds`, `p95_seconds`, and `p99_seconds`.

## Verify Allocation

Inspect the resolved claims:

```bash
kubectl get resourceclaims -o yaml | grep -E 'name:|device:|driver:|request:'
```

Confirm that only the allocated RDMA devices are visible inside each pod:

```bash
kubectl exec nixl-kv-initiator -- ls /dev/infiniband
kubectl exec nixl-kv-target -- ls /dev/infiniband
```

The pod logs also print `nvidia-smi topo -m`. In the aligned case, the visible
NICs should be NODE-local to the selected GPUs. In the unaligned case, the
visible NICs should be SYS/cross-NUMA relative to the selected GPUs.

The Service in `nixl-kv-service.yaml` is headless (`clusterIP: None`) so the
initiator resolves `nixl-kv-target` to the target pod IP. NIXL's listener should
not use a normal ClusterIP service for this side-channel connection.

## Benchmark Results

Observed on the tested 8 x H100 node topology with 1 GiB NIXL `WRITE`
transfers, 20 warmup iterations, and 100 timed iterations per run:

| Template | Run | Avg bandwidth | Avg latency | p50 | p95 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| `h100-4gpu-4nic-numa-aligned` | 1 | 39.07 GB/s | 27.48 ms | 27.48 ms | 27.50 ms | 27.50 ms |
| `h100-4gpu-4nic-numa-aligned` | 2 | 39.07 GB/s | 27.48 ms | 27.48 ms | 27.49 ms | 27.49 ms |
| `h100-4gpu-4nic-numa-aligned` | 3 | 39.05 GB/s | 27.49 ms | 27.49 ms | 27.51 ms | 27.51 ms |
| `h100-4gpu-4nic-numa-unaligned` | 1 | 27.54 GB/s | 38.99 ms | 38.98 ms | 39.08 ms | 39.11 ms |
| `h100-4gpu-4nic-numa-unaligned` | 2 | 27.54 GB/s | 39.00 ms | 38.99 ms | 39.08 ms | 39.19 ms |
| `h100-4gpu-4nic-numa-unaligned` | 3 | 27.54 GB/s | 38.99 ms | 38.99 ms | 39.06 ms | 39.10 ms |

Three-run mean:

| Template | NICs selected | Avg bandwidth | Avg latency | p50 | p95 | p99 |
|---|---|---:|---:|---:|---:|---:|
| `h100-4gpu-4nic-numa-aligned` | `mlx5_0`..`mlx5_3` | 39.07 GB/s | 27.49 ms | 27.49 ms | 27.50 ms | 27.50 ms |
| `h100-4gpu-4nic-numa-unaligned` | `mlx5_4`..`mlx5_7` | 27.54 GB/s | 38.99 ms | 38.99 ms | 39.08 ms | 39.13 ms |

Same GPUs, same NIC count, same transfer size: same-NUMA GPU/NIC allocation is
about 1.42x higher bandwidth and about 29.5% lower latency for this transfer.

## Inference Relevance

Disaggregated inference transfers KV cache from prefill workers to decode
workers. This benchmark does not run the model; it directly measures the NIXL
VRAM transfer that sits on that critical path.

Approximate KV payload size:

```text
KV bytes = 2 * layers * prompt_tokens * kv_heads * head_dim * dtype_bytes
```

Using the observed 1 GiB result, a 4 GiB KV handoff would take roughly:

| Placement | Estimated transfer time |
|---|---:|
| Same-NUMA GPU/NIC | 110 ms |
| Cross-NUMA GPU/NIC | 156 ms |

At concurrency, the slower transfer path also queues earlier, which is where a
microbenchmark bandwidth gap becomes visible as inference tail latency.

## Notes

- The example uses `pytorch/pytorch:2.8.0-cuda12.8-cudnn9-runtime` and installs
  `nixl` at pod start. This avoids UCX library mixing seen with some larger
  CUDA framework images.
- To test a larger simulated KV handoff, edit `--block-size` in
  `nixl-kv-target.yaml` and `nixl-kv-initiator.yaml`, for example `4294967296`
  for 4 GiB if GPU memory headroom allows it.
