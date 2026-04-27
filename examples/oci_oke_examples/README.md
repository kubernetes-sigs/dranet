# OCI OKE Examples

End-to-end examples for topologically-aware GPU + RDMA NIC allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Oracle Kubernetes Engine (OKE) with dranet.

## Examples

| Shape | Network | Example |
|---|---|---|
| [BM.GPU.H100.8](BM.GPU.H100.8/) | RoCEv2 (8x ConnectX-7, 100 Gb/s) | NUMA-aligned GPU + NIC allocation, NCCL all_reduce benchmark |
| [BM.GPU.GB200-v3.4](BM.GPU.GB200-v3.4/) | RoCEv2 (8x ConnectX-8, 400 Gb/s) | NUMA-aligned GPU + NIC allocation, NCCL all_reduce benchmark, IPv6 GID |
| [BM.GPU.GB200-v3.4 Placement Group](BM.GPU.GB200-v3.4/placement-group/) | RoCEv2 | OKE topology-aware scheduling using hpcIslandId / networkBlockId / localBlockId |

## Key dranet Features Demonstrated

- **NUMA alignment**: CEL selectors constrain NIC allocation to GPUs on the same NUMA node, enabling GDR
- **OKE topology attributes**: `oke.dra.net/{hpcIslandId,networkBlockId,localBlockId,rackId,gpuMemoryFabricId}` exposed as DRA device attributes
- **IPv6 GID support**: dranet enables per-interface IPv6 on RoCEv2 NICs in IPv4-only clusters, populating the routable GID at index 3 required by NCCL
- **Device isolation**: NRI plugin injects only allocated `/dev/infiniband/uverbs*` devices without `privileged: true`
