# OKE BM.GPU.GB200-v3.4 RoCEv2 dranet Demo

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

NIC mapping: NIC0=mlx5_0/rdma0 (NUMA 0), ... NIC3=mlx5_3/rdma3 (NUMA 0), NIC4=mlx5_5/rdma4 (NUMA 1), ... NIC7=mlx5_8/rdma7 (NUMA 1)

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
| `oke.dra.net/hpcIslandId` | HPC Island -- largest topology grouping (~2000 nodes) |
| `oke.dra.net/networkBlockId` | Network Block -- mid-level grouping (~64-128 nodes) |
| `oke.dra.net/localBlockId` | Local Block -- closest grouping (~8-32 nodes) |
| `oke.dra.net/rackId` | Physical rack identifier |
| `oke.dra.net/gpuMemoryFabricId` | GPU memory fabric ID (populated on GB200/GB300) |

> **Note:** Topology data must be enabled for your OCI tenancy. dranet logs
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

**dranet fix:** The OKE cloud provider returns `EnableIPv6: true` for RDMA
devices on GPU fabric shapes. When set, dranet:

1. Soft-fails the initial IPv6 address application (EACCES due to disabled IPv6)
2. Enables IPv6 per-interface via `net.ipv6.conf.<ifname>.disable_ipv6=0`
3. Re-applies the IPv6 address, populating the routable GID at index 3

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | Three `ResourceClaimTemplate` objects for the three test cases |
| `mpi-job.yaml` | `MPIJob` that runs `nccl_tests/all_reduce_perf` across 2 workers |
| `resourceslice-gpu.yaml` | Live GPU `ResourceSlice` from a GB200 node (reference) |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from a GB200 node (reference) |

## Installation

### 1. Uninstall the existing dranet

```bash
helm uninstall dranet -n kube-system
kubectl wait --for=delete pod -l k8s-app=dranet -n kube-system --timeout=120s
```

### 2. Install your local dranet build

Build and push your image, then install from the local Helm chart:

```bash
helm install dranet ./deployments/helm/dranet \
  --namespace kube-system \
  --set image.repository=<your-registry>/dranet \
  --set image.tag=<your-tag> \
  --set image.pullPolicy=Always
kubectl rollout status daemonset/dranet -n kube-system
```

## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply ResourceClaimTemplates
kubectl apply -f resource-claim-template.yaml

# Select a test case: edit mpi-job.yaml resourceClaimTemplateName to one of:
#   1nic-aligned | 2nic-aligned | 1nic-unaligned
kubectl apply -f mpi-job.yaml

# Wait for workers then stream launcher logs
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=300s
launcher=$(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -f "${launcher}"
```

## ResourceClaimTemplates

Three templates are defined, each allocating 1 GPU + N NICs per worker pod.
Update `mpi-job.yaml` `resourceClaimTemplateName:` to switch between them.

### `1nic-aligned` -- 1 GPU + 1 NIC, same NUMA

gpu-0 (`0008:06:00.0`, NUMA 0) + rdma3 (`0002:03:00.1`, NUMA 0). NCCL enables
GDR via C2C with `NCCL_NET_GDR_C2C=1`, transport: `NET/IB/0/GDRDMA(PCI)`.

### `2nic-aligned` -- 1 GPU + 2 NICs, same NUMA

gpu-0 (`0008:06:00.0`) + rdma2 + rdma3 (both NUMA 0, PCIe domain `0002`).
Doubles available RoCEv2 bandwidth and NCCL channels (8 vs 4).

### `1nic-unaligned` -- 1 GPU + 1 NIC, cross-NUMA

gpu-0 (`0008:06:00.0`, NUMA 0) + rdma4 (`0010:03:00.0`, NUMA 1). GDR is
disabled by NCCL; expect significantly lower bandwidth due to cross-NUMA memory
traffic and fewer NCCL channels (2 vs 4).

## Running the full test suite

Each test requires deleting the previous MPIJob since the resource claims are
immutable. Between tests, orphaned NICs may need PCI rebinding (see next section).

```bash
# --- Test 1: 1nic-aligned ---
# Ensure resourceClaimTemplateName: 1nic-aligned in mpi-job.yaml
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml
# Wait for results ...
kubectl delete mpijob nccl-test-dra

# --- Recover orphaned NICs before next test ---
# See "Recovering orphaned RDMA NICs" below

# --- Test 2: 1nic-unaligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 1nic-unaligned
kubectl apply -f mpi-job.yaml
# Wait for results ...
kubectl delete mpijob nccl-test-dra

# --- Recover orphaned NICs before next test ---

# --- Test 3: 2nic-aligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 2nic-aligned
kubectl apply -f mpi-job.yaml
# Wait for results ...
kubectl delete mpijob nccl-test-dra
```

## Recovering orphaned RDMA NICs

When a pod is deleted, dranet may not return the RDMA NIC from the pod namespace
to the host namespace. The NIC disappears from both the host and the ResourceSlice
(`ifName: null, rdma: false`). This is a pre-existing dranet bug, not
OKE-specific.

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
           not attrs.get('dra.net/virtual', {}).get('bool', True):
            print(f'{node}: {d[\"name\"]}  ifName={attrs.get(\"dra.net/ifName\", {}).get(\"string\", \"?\")}')
"
```

**Recover via PCI rebind** (requires a privileged debug pod on each GPU node):

```bash
# Start a debug pod (or use an existing one)
kubectl debug node/<node-ip> --image=busybox -it -- sh

# Inside the debug pod, rebind the orphaned NIC's PCI address:
chroot /host
echo "0002:03:00.1" > /sys/bus/pci/drivers/mlx5_core/unbind
sleep 2
echo "0002:03:00.1" > /sys/bus/pci/drivers/mlx5_core/bind
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

Repeat the unbind/bind for every orphaned NIC on **every GPU node**. Wait ~15
seconds for dranet to rescan, then verify the NIC reappears in the ResourceSlice.

## Benchmark Results

2-node `all_reduce_perf` (`-b 512M -e 8G -f 2 -g 1`), 1 GPU per worker.
Transport: `NET/IB/GDRDMA(PCI)` for NUMA-aligned, `NET/IB` for cross-NUMA.

| Template | GPU | NIC(s) | NUMA relation | Channels | GDR | Avg busbw |
|---|---|---|---|---|---|---|
| `1nic-aligned` | gpu-0 (NUMA 0) | rdma3 (NUMA 0) | same | 4 | yes | **~46 GB/s** |
| `2nic-aligned` | gpu-0 (NUMA 0) | rdma2 + rdma3 (NUMA 0) | same | 8 | yes | **~96 GB/s** |
| `1nic-unaligned` | gpu-0 (NUMA 0) | rdma4 (NUMA 1) | cross | 2 | no | **~25 GB/s** |

### Key observations

**NUMA alignment enables GDR (~1.7x):**
Cross-NUMA placement degrades performance from ~42 GB/s to ~25 GB/s with the
same NIC count. Two compounding penalties:

1. **GDR disabled** -- NCCL falls back from `GDRDMA(PCI)` to staging through
   host memory when the NIC is on a different NUMA node from the GPU. On GB200
   this is controlled by `NCCL_NET_GDR_C2C=1` which only enables GDR when NCCL
   detects a viable C2C path (same NUMA node).
2. **Fewer channels** -- NCCL allocates 2 channels for cross-NUMA NICs vs 4
   for NUMA-local NICs.

**2 NICs doubles bandwidth (~2.2x):**
Adding a second NUMA-aligned NIC increases bandwidth from ~42 GB/s to ~91 GB/s.
NCCL doubles the channel count (8 vs 4) and stripes data across both NICs.
The `count: 2` + CEL-selector pattern in the `2nic-aligned` template is the
idiomatic DRA approach for multi-device allocation.

**Isolation confirmed:**
In all cases, the pod sees only the allocated `/dev/infiniband/uverbs*` and
`/dev/infiniband/umad*` devices -- without `privileged: true`. Isolation is
enforced by the dranet NRI plugin injecting only the char devices that correspond
to the DRA-allocated NIC(s).
