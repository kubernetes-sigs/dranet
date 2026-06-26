---
title: "Azure AKS with DRANET"
date: 2026-06-25T00:00:00Z
---

DRANET allocates GPUs and RDMA NICs together on Azure Kubernetes Service (AKS) using [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/). This page covers two end-to-end AKS examples, NVIDIA GB300 and AMD MI300X, where both the GPUs and the ConnectX VFs are scheduled through DRA, with topology-aware NIC selection expressed as CEL in the `ResourceClaimTemplate`.

The full source lives under [examples/azure_aks_examples](https://github.com/kubernetes-sigs/dranet/tree/main/examples/azure_aks_examples).

| Example | GPU | NIC | DRA drivers |
|---|---|---|---|
| GB300 | 4 x NVIDIA GB300 | 4 x ConnectX VF (IB-only) | `gpu.nvidia.com` + `dra.net` |
| MI300X | 8 x AMD Instinct MI300X | 8 x ConnectX-7 VF | `gpu.amd.com` + `dra.net` |

## Shared setup

Both examples assume the dranet DaemonSet runs on the GPU nodes, a GPU DRA driver publishes the GPUs as DRA devices, and the MPI Operator v0.7.0 is installed:

```bash
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"
```

Scheduling is driven entirely by the `ResourceSlice` objects each driver publishes; verify they are present with `kubectl get resourceslices`.

### dranet on Azure

dranet queries the Azure Instance Metadata Service (IMDS) at startup and attaches Azure-specific attributes to every device it publishes, including `azure.dra.net/placementGroupId` and `azure.dra.net/vmSize`. VMs in **different placement groups do not share an InfiniBand fabric**, and this is not visible from node labels or GPU-driver attributes. The `placementGroupId` attribute lets a CEL selector constrain a multi-node job to a single IB fabric.

On Azure GPU SKUs the ConnectX VFs are often in **InfiniBand mode** with no Ethernet netdev. dranet discovers them by recording the RDMA link name (`rdmaDevice`) on the PCI device (a device is IB-only when it has a non-empty `rdmaDevice` and no `ifName`), and at pod start injects exactly the allocated `/dev/infiniband/uverbsN` character devices into the container. This enforces per-workload NIC isolation without `privileged: true`.

### Usage pattern

Both examples apply the claim templates, then select a test case by editing `resourceClaimTemplateName:` in `mpi-job.yaml`:

```bash
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml
kubectl exec nccl-test-dra-worker-0 -- ls /dev/infiniband/   # confirms per-pod NIC isolation
```

## NVIDIA GB300

Each ND GB300 v6 node has 4 NVIDIA GB300 GPUs and 4 Mellanox ConnectX IB NICs across 2 NUMA nodes (2 GPU + 2 NIC each). The GPU DRA devices carry `pciBusID` and NUMA attributes; the NIC DRA devices carry `pciAddress`, `rdmaDevice`, and `numaNode`.

Four `ResourceClaimTemplate`s are defined (`1nic-aligned`, `1nic-unaligned`, `2nic-aligned`, `ib-same-fabric`). The aligned variant co-locates the GPU and NIC on the same NUMA node:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: 1nic-aligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 1
          selectors:
          - cel:
              expression: 'device.attributes["resource.kubernetes.io"].pciBusID == "0008:06:00.0"'
      - name: nic
        exactly:
          deviceClassName: dranet.net
          count: 1
          selectors:
          - cel:
              expression: 'device.attributes["dra.net"]["rdmaDevice"] == "mlx5_0"'
```

The `2nic-aligned` template requests `count: 2` NICs constrained to the same NUMA node, and `ib-same-fabric` constrains the NICs to a specific Azure placement group. See [`gb300/`](https://github.com/kubernetes-sigs/dranet/tree/main/examples/azure_aks_examples/gb300) for all four templates and the `MPIJob`.

### GB300 results

2-node `all_reduce_perf`, 1 GPU per worker, GDR via nvidia-peermem. Full results in the [GB300 README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/azure_aks_examples/gb300/README.md).

| Template | NIC(s) | NUMA relation | GDR | Avg busbw |
|---|---|---|---|---:|
| `1nic-aligned` | mlx5_0 (NUMA 0) | NODE | yes | ~56 GB/s |
| `2nic-aligned` | mlx5_0 + mlx5_1 (NUMA 0) | NODE | yes | ~112 GB/s |
| `1nic-unaligned` | mlx5_2 (NUMA 1) | SYS | no | ~25 GB/s |

NUMA alignment matters ~4.5x here: cross-NUMA (SYS) placement drops from ~56 to ~25 GB/s with the same NIC count, because GDR is disabled and every transfer crosses the inter-socket interconnect.

## AMD MI300X

Each ND MI300X v5 node has 8 AMD Instinct MI300X GPUs and 8 ConnectX-7 VFs across 2 NUMA nodes (4 GPU + 4 NIC each). GPUs are allocated via `gpu.amd.com` and the NICs via dranet (`dra.net`). Three `ResourceClaimTemplate`s select NICs by NUMA node: `4nic-same-numa` (NUMA 0), `4nic-cross-numa` (NUMA 1), and `8nic-all`. See [`mi300x/`](https://github.com/kubernetes-sigs/dranet/tree/main/examples/azure_aks_examples/mi300x) for the templates and `MPIJob`.

### MI300X results

2-node `all_reduce_perf` across MI300X nodes, NIC set controlled by the DRA template. Full results in the [MI300X README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/azure_aks_examples/mi300x/README.md).

| Template | GPUs x NICs | Avg busbw | Peak busbw |
|---|---|---:|---:|
| `4nic-cross-numa` | 4 x 4 | 6.10 GB/s | 6.19 GB/s |
| `4nic-same-numa` | 4 x 4 | 42.53 GB/s | 49.28 GB/s |
| `8nic-all` | 8 x 8 | 67.73 GB/s | 78.81 GB/s |

Same-NUMA NIC selection is ~7x faster than cross-NUMA at 4 GPU x 4 NIC, and scaling to all 8 NICs reaches ~78 GB/s, with no change except the claim template.
