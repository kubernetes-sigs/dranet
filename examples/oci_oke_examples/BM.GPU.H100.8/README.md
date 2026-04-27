# OKE BM.GPU.H100.8 RoCEv2 DRANET Demo

End-to-end demo of topology-aware GPU + RoCEv2 NIC allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Oracle Kubernetes Engine (OKE) with [BM.GPU.H100.8](https://docs.oracle.com/en-us/iaas/Content/Compute/References/computeshapes.htm#bm-gpu) shapes.

## Context

### Shape: BM.GPU.H100.8

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 8 x NVIDIA H100 | 80 GB HBM3, Hopper architecture, NVLink/NVSwitch all-to-all |
| NIC | 16 x Mellanox ConnectX-7 (8 dual-port cards) | 400 Gb/s RoCEv2 per port |
| NUMA nodes | 2 | 4 GPUs + 8 NICs per NUMA node |

### NIC layout

| Device | ifName | PCI Address |
|---|---|---|
| pci-0000-0c-00-0 | rdma0 | 0000:0c:00.0 |
| pci-0000-0c-00-1 | rdma1 | 0000:0c:00.1 |
| pci-0000-2a-00-0 | rdma2 | 0000:2a:00.0 |
| pci-0000-2a-00-1 | rdma3 | 0000:2a:00.1 |
| pci-0000-41-00-0 | rdma4 | 0000:41:00.0 |
| pci-0000-41-00-1 | rdma5 | 0000:41:00.1 |
| pci-0000-58-00-0 | rdma6 | 0000:58:00.0 |
| pci-0000-58-00-1 | rdma7 | 0000:58:00.1 |
| pci-0000-86-00-0 | rdma8 | 0000:86:00.0 |
| pci-0000-86-00-1 | rdma9 | 0000:86:00.1 |
| pci-0000-a5-00-0 | rdma10 | 0000:a5:00.0 |
| pci-0000-a5-00-1 | rdma11 | 0000:a5:00.1 |
| pci-0000-bd-00-0 | rdma12 | 0000:bd:00.0 |
| pci-0000-bd-00-1 | rdma13 | 0000:bd:00.1 |
| pci-0000-d5-00-0 | rdma14 | 0000:d5:00.0 |
| pci-0000-d5-00-1 | rdma15 | 0000:d5:00.1 |

### OKE topology attributes (oke.dra.net)

Each device carries node-level topology attributes sourced from the
OCI Instance Metadata Service (`GET /opc/v2/host/`):

| Attribute | Description |
|---|---|
| `oke.dra.net/hpcIslandId` | HPC Island -- largest topology grouping (~2000 nodes) |
| `oke.dra.net/networkBlockId` | Network Block -- mid-level grouping (~64-128 nodes) |
| `oke.dra.net/localBlockId` | Local Block -- closest grouping (~8-32 nodes) |
| `oke.dra.net/rackId` | Physical rack identifier |

> **Note:** The full set of topology attributes requires RDMA topology data to be
> enabled for your OCI tenancy. When `rdmaTopologyData` is absent from IMDS, only
> `networkBlockId` and `rackId` (from the top-level host metadata) are populated.
> The `gpuMemoryFabricId` attribute is specific to GB200/GB300 shapes and is not
> present on H100.

## Prerequisites

- Kubernetes 1.34+ with `DynamicResourceAllocation` enabled
- [NVIDIA DRA GPU driver](https://github.com/NVIDIA/k8s-dra-driver) installed
- [MPI Operator](https://github.com/kubeflow/mpi-operator) installed

## Files

| File | Description |
|---|---|
| `deviceclass.yaml` | `DeviceClass` for DRANET NIC devices |
| `resource-claim-template.yaml` | `ResourceClaimTemplate` for 1 GPU + 1 RDMA NIC |
| `mpi-job.yaml` | `MPIJob` that runs `nccl_tests/all_reduce_perf` across 2 workers |

## Usage

### Create the DeviceClass

The `dra.net` DeviceClass is required for DRA to match NIC ResourceClaims
against DRANET's ResourceSlice devices.

```bash
kubectl apply -f deviceclass.yaml
```

### Verify devices

```bash
# Verify DRANET ResourceSlices are published
kubectl get resourceslice -l driver=dra.net

# Verify GPU ResourceSlices (from NVIDIA DRA driver)
kubectl get resourceslice -l driver=gpu.nvidia.com

# List RDMA NIC devices and their attributes
kubectl get resourceslice -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for rs in data['items']:
    if rs['spec'].get('driver') != 'dra.net': continue
    node = rs['spec']['nodeName']
    for d in rs['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        ifname = attrs.get('dra.net/ifName', {}).get('string', '?')
        pci = attrs.get('dra.net/pciAddress', {}).get('string', '?')
        rdma = attrs.get('dra.net/rdma', {}).get('bool', False)
        nb = attrs.get('oke.dra.net/networkBlockId', {}).get('string', '')
        rack = attrs.get('oke.dra.net/rackId', {}).get('string', '')
        if rdma and ifname.startswith('rdma'):
            print(f'{node}: {d[\"name\"]}  ifName={ifname}  pci={pci}  networkBlock={nb[:20]}  rack={rack[:20]}')
"
```

### Setup MPI and run

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply DeviceClass and ResourceClaimTemplates
kubectl apply -f deviceclass.yaml
kubectl apply -f resource-claim-template.yaml

# Launch the MPIJob
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

## Recovering orphaned RDMA NICs

When a pod is deleted, DRANET may not return the RDMA NIC from the pod namespace
to the host namespace (see [#137](https://github.com/kubernetes-sigs/dranet/issues/137)).

**Symptoms:** Workers stuck in `Pending` with `cannot allocate all claims`.

**Recover via PCI rebind** (requires a privileged debug pod on each GPU node):

```bash
kubectl debug node/<node-name> --image=busybox -it -- sh
chroot /host
# Replace with the actual PCI address of the orphaned NIC
echo "0000:0c:00.0" > /sys/bus/pci/drivers/mlx5_core/unbind
sleep 2
echo "0000:0c:00.0" > /sys/bus/pci/drivers/mlx5_core/bind
```

Wait ~15 seconds for DRANET to rescan, then verify the NIC reappears in the
ResourceSlice.
