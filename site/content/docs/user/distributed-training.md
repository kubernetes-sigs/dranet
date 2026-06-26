---
title: "Distributed Training with NUMA-aligned GPUs and NICs"
date: 2026-06-25T00:00:00Z
---

This example runs a PyTorch DistributedDataParallel (DDP) benchmark over NCCL and reports Model FLOPs Utilization (MFU). It follows the DRA + MPIJob pattern and compares **NUMA-aligned vs NUMA-unaligned** NIC placement for a 4 GPU / 4 NIC run on H100 nodes.

The full source, including the launcher scripts, the benchmark, and both MPIJob manifests, lives at [examples/distributed_training](https://github.com/kubernetes-sigs/dranet/tree/main/examples/distributed_training).

## Why NUMA alignment matters

On a multi-socket GPU node, both the GPUs and the RDMA NICs attach to specific NUMA nodes via the PCIe topology. When a GPU and the NIC it uses for inter-node communication sit on the *same* NUMA node, traffic stays local to that socket (`NODE` path in `nvidia-smi topo -m`). When they sit on *different* NUMA nodes, traffic crosses the inter-socket interconnect (`SYS` path), adding latency and reducing bandwidth.

This example demonstrates the effect by allocating the **same four GPUs** in both runs and varying only **which four NICs** are bound to them. DRANET makes this controllable because it publishes per-device attributes (including `rdma` and `numaNode`) that you select against in a `ResourceClaimTemplate` using CEL.

## DeviceClass

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: dranet.net
spec:
  selectors:
  - cel:
      expression: device.driver == "dra.net"
```

## ResourceClaimTemplates

Both templates request the **same four GPUs** (selected by `pciBusID`) and four NICs requiring `rdma == true`, differing only in the NIC `numaNode`:

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
              expression: >-
                device.attributes["resource.kubernetes.io"]["pciBusID"] in
                ["0001:00:00.0", "0002:00:00.0", "0003:00:00.0", "0008:00:00.0"]
      - name: nic
        exactly:
          deviceClassName: dranet.net
          count: 4
          selectors:
          - cel:
              expression: >-
                device.attributes["dra.net"]["rdma"] == true &&
                device.attributes["dra.net"]["numaNode"] == 0
---
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
              expression: >-
                device.attributes["resource.kubernetes.io"]["pciBusID"] in
                ["0001:00:00.0", "0002:00:00.0", "0003:00:00.0", "0008:00:00.0"]
      - name: nic
        exactly:
          deviceClassName: dranet.net
          count: 4
          selectors:
          - cel:
              expression: >-
                device.attributes["dra.net"]["rdma"] == true &&
                device.attributes["dra.net"]["numaNode"] == 1
```

The two `MPIJob` manifests (`mpi-job-4gpu-4nic-numa-aligned.yaml` / `-unaligned.yaml`) and the launcher, worker, and benchmark scripts are in the [example directory](https://github.com/kubernetes-sigs/dranet/tree/main/examples/distributed_training). The aligned and unaligned manifests are otherwise identical; they differ only in which `ResourceClaimTemplate` they reference.

## Running the example

Install the MPI Operator if the cluster does not already have the `MPIJob` CRD:

```bash
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"
```

`kubectl apply -k .` builds the benchmark ConfigMap and applies the device class and templates. Then apply one of the MPIJobs:

```bash
kubectl apply -k .
kubectl apply -f mpi-job-4gpu-4nic-numa-aligned.yaml
```

Repeat with `mpi-job-4gpu-4nic-numa-unaligned.yaml` for the unaligned run. Each launcher prints a final `MFU_RESULT` line.

## Results

Cluster: 2 x `Standard_ND96isr_H100_v5` workers, 4 H100 GPUs per worker, 4 RDMA NICs per worker, BF16 DDP over NCCL. Full methodology and per-step output are in the [example README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/distributed_training/README.md).

| Case | Avg step | TFLOP/s per GPU | MFU |
|---|---:|---:|---:|
| NUMA aligned | 55.496 ms | 356.62 | 36.06% |
| NUMA unaligned | 60.951 ms | 324.70 | 32.83% |

The aligned run was ~8.95% faster per step and ~9.84% higher MFU, with no change except the NIC NUMA placement chosen by the `ResourceClaimTemplate`. That gap is the cost of crossing the inter-socket interconnect that NUMA-aware device selection avoids.
