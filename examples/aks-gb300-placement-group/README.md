# AKS GB300 Placement Group Support

Demonstrates how dranet exposes Azure placement group information as DRA device
attributes, enabling workloads to make scheduling decisions based on InfiniBand
fabric connectivity.

## Problem

On Azure, VMSS (Virtual Machine Scale Sets) with `singlePlacementGroup: true`
place VMs in the same [placement groups][az-ib]. VMs in **different placement
groups do not share an InfiniBand fabric**, and cross-placement-group RDMA
traffic fails with transport errors.

This is not detectable from Kubernetes node labels or NVIDIA GFD attributes.
Without placement group awareness, multi-node NCCL jobs can be scheduled across
IB fabric boundaries.

[az-ib]: https://learn.microsoft.com/en-us/azure/virtual-machines/setup-infiniband#cluster-configuration-options

## Solution

dranet queries the Azure Instance Metadata Service (IMDS) at startup and
attaches two attributes to every device in the node's ResourceSlice:

| Attribute | Source | Example |
|---|---|---|
| `azure.dra.net/placementGroupId` | IMDS `compute/placementGroupId` | `739e6cfb-2607-462e-9e2b-21d24b31f5ed` |
| `azure.dra.net/vmSize` | IMDS `compute/vmSize` | `Standard_ND128isr_GB300_v6` |

Workloads can use CEL selectors in ResourceClaimTemplates to constrain
allocation to devices sharing the same `placementGroupId`, ensuring all workers
land on nodes within the same IB fabric.

## Example: Same-Fabric Scheduling with CEL

A ResourceClaimTemplate can enforce that all allocated NICs share the same
placement group:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: ib-same-fabric
spec:
  spec:
    devices:
      requests:
      - name: nic
        exactly:
          deviceClassName: dranet.net
          selectors:
          - cel:
              expression: >-
                device.attributes["azure.dra.net"]["placementGroupId"] == "739e6cfb-2607-462e-9e2b-21d24b31f5ed"
          - cel:
              expression: >-
                device.attributes["dra.net"]["rdma"] == true
```

This ensures the scheduler only allocates RDMA devices on nodes within the
specified placement group, preventing cross-fabric scheduling failures.

## Verification

Create an AKS cluster with 2 `Standard_ND128isr_GB300_v6` GPU nodes in 2 placement groups:

| Node | placementGroupId | IB Devices |
|---|---|---|
| vmss000000 | `739e6cfb-2607-462e-9e2b-21d24b31f5ed` | mlx5_0 – mlx5_3 |
| vmss000001 | `ab1690bb-a478-4039-b89a-8a3f4264d4b4` | mlx5_0 – mlx5_3 |

Confirmed that the two nodes have different `placementGroupId` values and that
cross-placement-group IB is non-functional:

- **Intra-node `ib_write_bw`**: 449.43 Gb/s (working)
- **Cross-node `ib_write_bw`**: transport retry counter exceeded, error 12 (broken)
- **Cross-node NCCL `all_reduce_perf`**: hangs at RDMA data transfer
