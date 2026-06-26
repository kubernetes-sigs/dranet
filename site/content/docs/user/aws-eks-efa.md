---
title: "AWS EKS with GPUDirect and EFA"
date: 2026-06-25T00:00:00Z
---

This page walks through topology-aware GPU + EFA allocation on Amazon EKS using [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/). The example targets `p4d.24xlarge` instances (8 NVIDIA A100 GPUs and 4 Elastic Fabric Adapters spread across 4 PCIe root complexes), and the same pattern applies to other EKS nodes with NVIDIA GPUs and EFA devices.

The full source for this example lives at [examples/aws_eks_examples/gpu-efa](https://github.com/kubernetes-sigs/dranet/tree/main/examples/aws_eks_examples/gpu-efa).

## How it works

Both the NVIDIA GPU DRA driver (`gpu.nvidia.com`) and dranet (`dra.net`) publish the `resource.kubernetes.io/pcieRoot` attribute for the devices they manage. Because both drivers expose the same attribute, a `ResourceClaimTemplate` can use a CEL constraint to co-locate a GPU and an EFA adapter on the same PCIe root complex. That direct PCIe path enables GPU Direct RDMA (GDRDMA) and avoids cross-root traffic over the CPU/PCIe switch fabric.

## Prerequisites

EKS 1.34+ with EFA-enabled worker nodes (see [Manage EFA devices on Amazon EKS](https://docs.aws.amazon.com/eks/latest/userguide/device-management-efa.html)).

Install the MPI Operator:

```sh
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"
```

Install the NVIDIA GPU DRA driver and the EFA DRA driver (dranet). The AWS-supported `aws-dranet` Helm chart provisions the `efa.networking.k8s.aws` `DeviceClass` and a DaemonSet that publishes only EFA devices:

```sh
helm repo add eks https://aws.github.io/eks-charts
helm repo update
helm install aws-dranet eks/aws-dranet --namespace kube-system
```

See the [example README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/aws_eks_examples/gpu-efa/README.md) for the full NVIDIA DRA driver install command and node labeling.

## DeviceClass

The `aws-dranet` chart creates the `efa.networking.k8s.aws` `DeviceClass` automatically; it is shown here for reference:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: efa.networking.k8s.aws
spec:
  selectors:
  - cel:
      expression: |
        device.driver == "dra.net" &&
        device.attributes["dra.net"].pciDevice == 'Elastic Fabric Adapter (EFA)'
```

The GPU and EFA `ResourceSlice` objects are published by the drivers on each node (not applied by users); they expose `pciBusID`, `rdmaDevice`, and `resource.kubernetes.io/pcieRoot`, which is what enables cross-driver topology-aware co-selection.

## Request a GPU + EFA device

`resource-claim-template.yaml` defines two templates. `gpu-efa-aligned` requests 1 GPU + 1 EFA and constrains both to the same `resource.kubernetes.io/pcieRoot`; `gpu-efa-unaligned` requests the same devices without the constraint, for comparison.

```yaml
# Unaligned: GPU and EFA may be allocated on different PCIe root complexes
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: gpu-efa-unaligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 1
      - name: efa
        exactly:
          deviceClassName: efa.networking.k8s.aws
          count: 1
---
# Aligned: GPU and EFA are co-located on the same PCIe root complex
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: gpu-efa-aligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 1
      - name: efa
        exactly:
          deviceClassName: efa.networking.k8s.aws
          count: 1
      constraints:
      - requests: [gpu, efa]
        matchAttribute: resource.kubernetes.io/pcieRoot
```

## Run the workload

The `mpi-job.yaml` `MPIJob` runs NCCL `all_reduce_perf` across 2 workers, each requesting a `gpu-efa-aligned` claim. Apply the templates and the job:

```sh
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml
```

The full `MPIJob` manifest, including the NCCL/EFA environment variables, is in the [example directory](https://github.com/kubernetes-sigs/dranet/tree/main/examples/aws_eks_examples/gpu-efa).

## Benchmark results

2-node `all_reduce_perf`, 1 GPU per worker, EFA transport via `aws-ofi-nccl`. Full results are in the [example README](https://github.com/kubernetes-sigs/dranet/blob/main/examples/aws_eks_examples/gpu-efa/README.md).

| Template | GPU (PCIe root) | EFA (PCIe root) | GDR | Avg busbw |
|---|---|---|---|---:|
| `gpu-efa-aligned` | gpu-0 (`pci0000:10`) | rdmap16s27 (`pci0000:10`) | Yes | ~11.35 GB/s |
| `gpu-efa-unaligned` | gpu-0 (`pci0000:10`) | rdmap160s27 (`pci0000:a0`) | No | ~6.04 GB/s |

Cross-PCIe-root placement degrades performance by roughly 1.9x with the same GPU and EFA count: GDR is disabled when the EFA adapter has no direct PCIe path to the GPU, and data must traverse the CPU/PCIe switch fabric. DRA enables the topology-aware placement because `resource.kubernetes.io/pcieRoot` is published by both drivers, so CEL selectors co-locate GPU and EFA without hardcoding device names.
