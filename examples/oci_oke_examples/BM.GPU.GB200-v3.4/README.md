# OKE BM.GPU.GB200-v3.4 RoCEv2 DRANET Demo

End-to-end demo of topologically-aware GPU + RoCEv2 NIC allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Oracle Kubernetes Engine (OKE) with [BM.GPU.GB200-v3.4](https://docs.oracle.com/en-us/iaas/Content/Compute/References/computeshapes.htm#bm-gpu) shapes.

## Context

### Shape: BM.GPU.GB200-v3.4

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 4 x NVIDIA GB200 | 189 GB HBM3e, Blackwell architecture, NVLink-18 all-to-all |
| NIC | 8 x Mellanox ConnectX-8 | 400 Gb/s RoCEv2, 4x NDR per NIC |
| NUMA nodes | 2 | 2 GPUs + 4 NICs per NUMA node |

### GPU-NIC topology

On GB200, GPUs connect to the Grace CPU via **NVLink C2C** (chip-to-chip), while
NICs connect via PCIe. Because GPUs and NICs are on fundamentally different
interconnects, `nvidia-smi topo -m` reports **SYS** for every GPU-NIC pair:

|      | GPU0 | GPU1 | GPU2 | GPU3 | NIC0 | NIC1 | NIC2 | NIC3 | NIC4 | NIC5 | NIC6 | NIC7 |
|------|------|------|------|------|------|------|------|------|------|------|------|------|
| GPU0 | X    | NV18 | NV18 | NV18 | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  |
| GPU1 | NV18 | X    | NV18 | NV18 | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  |
| GPU2 | NV18 | NV18 | X    | NV18 | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  |
| GPU3 | NV18 | NV18 | NV18 | X    | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  | SYS  |

NIC mapping: NIC0=mlx5_0/rdma0 (NUMA 0), NIC1=mlx5_1/rdma1 (NUMA 0), NIC2=mlx5_2/rdma2 (NUMA 0), NIC3=mlx5_3/rdma3 (NUMA 0), NIC4=mlx5_5/rdma4 (NUMA 1), ..., NIC7=mlx5_8/rdma7 (NUMA 1)

> **Key difference from Azure GB300:** On Azure, GPU-NIC pairs on the same NUMA
> node have **NODE** affinity. On OKE GB200, all pairs report **SYS** because the
> C2C link is not visible to the PCIe topology. Despite this, NCCL enables GDR
> via the `NCCL_NET_GDR_C2C=1` flag for NUMA-local NICs, achieving comparable
> bandwidth. The practical performance difference is NUMA-local vs cross-NUMA.

### DRA device attributes

**GPU** (driver: `gpu.nvidia.com`):

| Device | pciBusID | pcieRoot | NUMA |
|---|---|---|---|
| gpu-0 | 0008:06:00.0 | pci0008:00 | 0 |
| gpu-1 | 0009:06:00.0 | pci0009:00 | 0 |
| gpu-2 | 0018:06:00.0 | pci0018:00 | 1 |
| gpu-3 | 0019:06:00.0 | pci0019:00 | 1 |

**NIC** (driver: `dra.net`):

| Device | ifName | pciAddress | NUMA | pcieRoot |
|---|---|---|---|---|
| pci-0000-03-00-0 | rdma0 | 0000:03:00.0 | 0 | pci0000:00 |
| pci-0000-03-00-1 | rdma1 | 0000:03:00.1 | 0 | pci0000:00 |
| pci-0002-03-00-0 | rdma2 | 0002:03:00.0 | 0 | pci0002:00 |
| pci-0002-03-00-1 | rdma3 | 0002:03:00.1 | 0 | pci0002:00 |
| pci-0010-03-00-0 | rdma4 | 0010:03:00.0 | 1 | pci0010:00 |
| pci-0010-03-00-1 | rdma5 | 0010:03:00.1 | 1 | pci0010:00 |
| pci-0012-03-00-0 | rdma6 | 0012:03:00.0 | 1 | pci0012:00 |
| pci-0012-03-00-1 | rdma7 | 0012:03:00.1 | 1 | pci0012:00 |

### OKE topology attributes (oke.dra.net)

Each NIC device carries node-level RDMA topology attributes sourced from the
OCI Instance Metadata Service (`GET /opc/v2/host/`):

| Attribute | Description |
|---|---|
| `oke.dra.net/hpcIslandId` | HPC Island — largest topology grouping (~2000 nodes) |
| `oke.dra.net/networkBlockId` | Network Block — mid-level grouping (~64-128 nodes) |
| `oke.dra.net/localBlockId` | Local Block — closest grouping (~8-32 nodes) |
| `oke.dra.net/rackId` | Physical rack identifier |
| `oke.dra.net/gpuMemoryFabricId` | GPU memory fabric ID (populated on GB200/GB300) |

> **Note:** Topology data must be enabled for your OCI tenancy. DRANET logs
> `"Please turn on TopologyData for your Tenancy"` at startup if the `/host/`
> endpoint does not provide `rdmaTopologyData`.

### RoCEv2 and IPv6 on OKE

The ConnectX-8 NICs use **RoCEv2** (RDMA over Converged Ethernet v2). On OKE,
each RDMA NIC receives a globally-routable IPv6 address via Router Advertisement.
This address populates a routable GID in the NIC's GID table, which NCCL uses
for inter-node communication (`NCCL_IB_GID_INDEX=3`).

**Challenge:** In single-stack IPv4 Kubernetes clusters, the container runtime
sets `net.ipv6.conf.all.disable_ipv6=1` in pod namespaces. This prevents the
RA-assigned IPv6 address from being applied to RDMA NICs in the pod, leaving
only link-local GIDs (which are not routable on the OKE fabric).

**DRANET fix:** The OKE cloud provider returns `EnableIPv6: true` for RDMA
devices on GPU fabric shapes. When set, DRANET:

1. Soft-fails the initial IPv6 address application (EACCES due to disabled IPv6)
2. Enables IPv6 per-interface via `net.ipv6.conf.<ifname>.disable_ipv6=0`
3. Re-applies the IPv6 address, populating the routable GID at index 3

**NCCL configuration note:** Set `NCCL_IB_DATA_DIRECT=0` to prevent NCCL from
selecting the Data Direct DMA interface (`mlx5_N_dma`) and instead use the
standard IB verbs path, which correctly uses the GID configured by DRANET.

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | `ResourceClaimTemplate` objects: `1nic-aligned`, `1nic-unaligned`, `2nic-aligned`, `4nic-aligned`, `4gpu-8nic` |
| `mpi-job.yaml` | `MPIJob` running `all_reduce_perf` across 2 workers (per-node benchmarks) |
| `multinode-mpi-job.yaml` | `MPIJob` running `all_reduce_perf` across 16 workers with `4gpu-8nic` + ComputeDomain |
| `compute-domain.yaml` | `ComputeDomain` object that provisions IMEX channels for `NCCL_MNNVL_ENABLE=1` |
| `resourceslice-gpu.yaml` | Live GPU `ResourceSlice` from a GB200 node (reference) |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from a GB200 node (reference) |
| `placement-group/` | OKE topology-aware scheduling examples |


## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply ResourceClaimTemplates
kubectl apply -f resource-claim-template.yaml

# --- Per-node benchmarks (2 workers, 1 GPU/rank) ---
# Edit mpi-job.yaml resourceClaimTemplateName to: 1nic-aligned | 2nic-aligned | 1nic-unaligned
kubectl apply -f mpi-job.yaml
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=300s
kubectl logs -f $(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')

# --- 16-node multinode benchmark (4 GPUs/rank, MNNVL enabled) ---
# Requires NVIDIA compute-domain.nvidia.com DRA driver on the cluster.
kubectl apply -f compute-domain.yaml
# Wait for the channel ResourceClaimTemplate to be created by the controller
kubectl wait --for=jsonpath='.metadata.name'=nccl-test-compute-domain-channel \
  resourceclaimtemplate/nccl-test-compute-domain-channel --timeout=30s 2>/dev/null || \
  kubectl get resourceclaimtemplate nccl-test-compute-domain-channel
kubectl apply -f multinode-mpi-job.yaml
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-multinode,training.kubeflow.org/job-role=worker \
  --timeout=300s
kubectl logs -f $(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-multinode,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
```

## ResourceClaimTemplates

Four templates are defined, each allocating 1 GPU + N NICs per worker pod.
Update `mpi-job.yaml` `resourceClaimTemplateName:` to switch between them.

The templates use NUMA-based CEL selectors with the `virtual == false` guard
(required because some virtual RDMA devices do not carry the `numaNode` attribute):

### `1nic-aligned` — 1 GPU + 1 NIC, same NUMA

gpu-0 (`0008:06:00.0`, NUMA 0) + any 1 RDMA NIC from NUMA 0. NCCL enables
GDR via C2C with `NCCL_NET_GDR_C2C=1`. Transport: `NET/IB/GDRDMA(PCI)`.

### `2nic-aligned` — 1 GPU + 2 NICs, same NUMA

gpu-0 (`0008:06:00.0`) + any 2 RDMA NICs from NUMA 0. Doubles available
RoCEv2 bandwidth and NCCL channels. Idiomatic DRA multi-device allocation
via `count: 2` + pool selector.

### `4nic-aligned` — 1 GPU + 4 NICs, same NUMA

gpu-0 (`0008:06:00.0`) + all 4 RDMA NICs from NUMA 0 (rdma0–rdma3). Delivers
~111 GB/s busbw at 16 nodes (1 GPU per rank, `NCCL_MNNVL_ENABLE=0`).

### `4gpu-8nic` — 4 GPUs + 8 NICs, both NUMA nodes

All 4 GPUs + all 8 physical RDMA NICs. Used with a `ComputeDomain` channel
claim (see `compute-domain.yaml`) to enable `NCCL_MNNVL_ENABLE=1`. Delivers
~700 GB/s busbw at 16 nodes × 4 GPUs = 64 ranks.

### `1nic-unaligned` — 1 GPU + 1 NIC, cross-NUMA

gpu-0 (`0008:06:00.0`, NUMA 0) + any 1 RDMA NIC from NUMA 1. GDR is disabled
by NCCL; expect lower bandwidth due to cross-NUMA memory traffic.

## Running the full test suite

Each test requires deleting the previous MPIJob since the resource claims are
immutable. Between tests, orphaned NICs may need PCI rebinding (see next section).

```bash
# --- Test 1: 1nic-aligned ---
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml   # resourceClaimTemplateName: 1nic-aligned
# Wait for results ...
kubectl delete mpijob nccl-test-dra

# --- Test 2: 1nic-unaligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 1nic-unaligned
kubectl apply -f mpi-job.yaml
kubectl delete mpijob nccl-test-dra

# --- Test 3: 2nic-aligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 2nic-aligned
kubectl apply -f mpi-job.yaml
kubectl delete mpijob nccl-test-dra
```

## Recovering orphaned RDMA NICs

When a pod is deleted, DRANET may not return the RDMA NIC from the pod namespace
to the host namespace. The NIC disappears from both the host and the ResourceSlice.
This is a known DRANET bug. [This issue](https://github.com/kubernetes-sigs/dranet/issues/137) tracks the DRANET progress, while [this PR](https://github.com/containerd/nri/pull/286) addresses the upstream NRI changes.

**Symptoms:** Workers stuck in `Pending` with `cannot allocate all claims`.

**Check which NICs are missing:**

```bash
kubectl get resourceslice -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for rs in data['items']:
    if rs['spec'].get('driver') != 'dra.net': continue
    node = rs['spec']['nodeName']
    for d in rs['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        if attrs.get('dra.net/rdma', {}).get('bool') and \
           not attrs.get('dra.net/virtual', {}).get('bool', True) and \
           not attrs.get('dra.net/ifName', {}).get('string', ''):
            pci = attrs.get('dra.net/pciAddress', {}).get('string', '?')
            print(f'{node}: {d[\"name\"]} pci={pci}')
"
```

**Recover via PCI rebind** (requires a privileged pod on each affected GPU node):

```bash
kubectl debug node/<node-ip> --image=<any-cached-image> -it -- bash
# Inside the debug pod:
chroot /host
echo "0000:03:00.0" > /sys/bus/pci/drivers/mlx5_core/unbind && sleep 1
echo "0000:03:00.0" > /sys/bus/pci/drivers/mlx5_core/bind
```

Common PCI addresses on BM.GPU.GB200-v3.4:

| ifName | PCI Address | NUMA |
|---|---|---|
| rdma0 | 0000:03:00.0 | 0 |
| rdma1 | 0000:03:00.1 | 0 |
| rdma2 | 0002:03:00.0 | 0 |
| rdma3 | 0002:03:00.1 | 0 |
| rdma4 | 0010:03:00.0 | 1 |
| rdma5 | 0010:03:00.1 | 1 |
| rdma6 | 0012:03:00.0 | 1 |
| rdma7 | 0012:03:00.1 | 1 |

Wait ~15 seconds for DRANET to rescan, then verify the NIC reappears in the ResourceSlice.

## Benchmark Results

### 2-node `all_reduce_perf` (`-b 512M -e 8G -f 2 -g 1`)

1 GPU per worker. Transport: `NET/IB/GDRDMA(PCI)` for NUMA-aligned, `NET/IB` for cross-NUMA.
Settings: `NCCL_MIN_NCHANNELS=8`, `NCCL_IB_QPS_PER_CONNECTION=2`, `NCCL_IB_DATA_DIRECT=0`.

| Template | GPU | NIC(s) | NUMA relation | Channels | GDR | Avg busbw | % theoretical |
|---|---|---|---|---|---|---|---|
| `1nic-aligned` | gpu-0 (NUMA 0) | any 1 RDMA (NUMA 0) | same | 8 | yes | **~46 GB/s** | ~92% of 50 GB/s |
| `2nic-aligned` | gpu-0 (NUMA 0) | any 2 RDMA (NUMA 0) | same | 8 | yes | **~96 GB/s** | ~96% of 100 GB/s |
| `1nic-unaligned` | gpu-0 (NUMA 0) | any 1 RDMA (NUMA 1) | cross | 2 | no | **~25 GB/s** | ~50% of 50 GB/s |

### 16-node `all_reduce_perf`, 1 GPU per rank (`-b 512M -e 8G -f 2 -g 1`)

16 workers, 1 GPU + RDMA NICs per worker via DRA. `NCCL_MNNVL_ENABLE=0`.
Transport: `NET/IB/GDRDMA(PCI)`, GDR active on all nodes.

| NICs/worker | `NCCL_MIN_NCHANNELS` | `NCCL_IB_QPS_PER_CONNECTION` | 8 GB busbw |
|---|---|---|---|
| 2 NUMA-0 NICs | 8 | 2 | ~56 GB/s |
| 4 NUMA-0 NICs | 8 | 1 | ~111 GB/s |
| 4 NUMA-0 NICs | 4 | 2 | ~111 GB/s |

### 16-node `all_reduce_perf`, 4 GPUs per rank (`-b 8 -e 32G -f 2 -g 1`)

16 workers × 4 GPUs = 64 ranks. DRA: `4gpu-8nic` + `ComputeDomain` channel claim.
`NCCL_MNNVL_ENABLE=1`, `NCCL_CUMEM_ENABLE=1`, `NCCL_NET_PLUGIN=none`.
NCCL uses NVLink-18 intra-node and the NVLink fabric inter-node via IMEX channels.

| Size | algbw (GB/s) | busbw (GB/s) |
|---|---|---|
| 512 MB | 334.48 | 658.51 |
| 1 GB | 386.55 | 761.03 |
| 2 GB | 416.32 | 819.63 |
| 4 GB | 431.27 | 849.06 |
| 8 GB | 455.47 | 896.71 |
| 16 GB | 469.22 | **923.78** |
| 32 GB | 473.68 | **932.56** |

~930 GB/s sustained busbw at 16 nodes. The 8× ConnectX-8 NICs at 400 Gb/s
provide 400 GB/s total NIC bandwidth, but busbw exceeds this because NCCL
routes most traffic over the NVLink fabric (MNNVL), using RoCEv2 only where
the NVLink topology does not provide a direct path. At 128 nodes the ring
efficiency factor approaches 2.0× (vs 1.875× at 16 nodes), consistent with
the ~1 TB/s near-line-rate results reported on full-rack configurations.

> **Note:** `NCCL_MNNVL_ENABLE=1` requires a `ComputeDomain` channel resource
> claim in each worker pod to provision `/dev/nvidia-caps-imex-channels`. Without
> it, NCCL fails with "unhandled system error" at startup.

### Key observations

**NUMA alignment enables GDR (~1.7× over cross-NUMA):**
Cross-NUMA placement degrades bandwidth from ~46 GB/s to ~25 GB/s with the
same NIC count. Two compounding penalties:

1. **GDR disabled** — NCCL falls back from `GDRDMA(PCI)` to staging through
   host memory when the NIC is on a different NUMA node from the GPU. On GB200
   this is controlled by `NCCL_NET_GDR_C2C=1` which only enables GDR when NCCL
   detects a viable C2C path (same NUMA node).
2. **Fewer channels** — NCCL allocates 2 channels for cross-NUMA NICs vs 8
   for NUMA-local NICs with `NCCL_MIN_NCHANNELS=8`.

**More NICs scales linearly:**
Adding NUMA-aligned NICs scales busbw proportionally — 1 NIC → ~46 GB/s,
2 NICs → ~96 GB/s, 4 NICs → ~111 GB/s (2-node vs 16-node, respectively).
The `count: N` + CEL-selector pattern in `ResourceClaimTemplate` is the
idiomatic DRA approach for multi-device allocation from a homogeneous pool.

**~92–96% of theoretical at 2 nodes:**
Peak single-NIC bandwidth is 400 Gb/s = 50 GB/s. The 1nic-aligned result
(~46 GB/s) achieves ~92% of theoretical, demonstrating that DRANET's DRA-based
NIC injection and IPv6 GID configuration adds negligible overhead.

**Channel × QPS equivalence at 16 nodes (1-GPU mode):**
At scale, total QP fanout governs throughput. `NCCL_MIN_NCHANNELS=8` with
`NCCL_IB_QPS_PER_CONNECTION=1` and `NCCL_MIN_NCHANNELS=4` with
`NCCL_IB_QPS_PER_CONNECTION=2` produce identical ~111 GB/s busbw.
Increasing to 8ch×2QPS causes worker disconnects on this fabric.

**MNNVL + CUMEM_ENABLE unlocks the NVLink fabric (~8× improvement):**
`NCCL_MNNVL_ENABLE=1` with a `ComputeDomain` IMEX channel claim enables NCCL
to use the NVLink fabric for inter-node communication. Adding `NCCL_CUMEM_ENABLE=1`
(CUDA memory manager integration) and `NCCL_NET_PLUGIN=none` raises busbw from
~111 GB/s (RoCEv2, 1 GPU/rank) to ~930 GB/s (NVLink + RoCEv2, 4 GPUs/rank)
at 16 nodes, approaching line rate at full-rack scale.

**Isolation confirmed:**
In all cases, the pod sees only the allocated `/dev/infiniband/uverbs*` and
`/dev/infiniband/umad*` devices — without `privileged: true`. Isolation is
enforced by the DRANET NRI plugin injecting only the char devices that correspond
to the DRA-allocated NIC(s).
