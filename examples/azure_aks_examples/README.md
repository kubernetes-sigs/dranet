# AKS + dranet Examples

End-to-end examples of GPU + RDMA NIC allocation on Azure Kubernetes Service
(AKS) using [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/).

| Example | VM size | GPU | NIC | Scheduling |
|---|---|---|---|---|
| [`mi300x/`](mi300x/) | `Standard_ND96isr_MI300X_v5` | 8 × AMD Instinct MI300X | 8 × ConnectX-7 VF (RoCE/IB) | GPUs and NICs both via DRA (`gpu.amd.com` + `dra.net`) |
| [`gb300/`](gb300/) | `Standard_ND128isr_GB300_v6` | 4 × NVIDIA GB300 | 4 × ConnectX VF (IB-only) | GPUs and NICs both via DRA (`gpu.nvidia.com` + `dra.net`) |

Each example directory contains the `ResourceClaimTemplate`, `MPIJob`, and
reference `ResourceSlice` objects for its VM size. The per-example READMEs
cover VM-specific topology, templates, and benchmark results; this README
covers the prerequisites and concepts that apply to both.

## Cluster prerequisites

Both examples assume the following are already installed on the AKS cluster:

- **dranet DaemonSet** running on the GPU nodes. If the nodes do not have NRI
  enabled in containerd by default, see `install-containerd-1.7yaml`. Pass
  `--move-ib-interfaces=false` on the dranet DaemonSet if the ConnectX VFs are
  in IB mode so dranet publishes `rdmaDevice` attributes (and does not try to
  move IPoIB netdevs into the pod netns).
- **GPU DRA driver** exposing GPUs as DRA devices to the scheduler:
  - MI300X: `amdgpu` kernel driver + the `k8s-gpu-dra-driver` publishing
    GPUs via the `gpu.amd.com` DRA driver.
  - GB300: `k8s-gpu-dra-driver` publishing GPUs via the `gpu.nvidia.com` DRA
    driver.
- **MPI Operator v0.7.0**:

  ```bash
  kubectl apply --server-side -k \
    "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"
  ```

Verify dranet and the GPU DRA driver are live — scheduling on both examples is
driven entirely by the `ResourceSlice` objects each driver publishes:

```bash
kubectl get resourceslices                             # NIC slices (dra.net) and GPU slices (gpu.amd.com / gpu.nvidia.com)
```

## dranet on Azure

### Azure placement group and VM metadata

dranet queries the Azure Instance Metadata Service (IMDS) at startup and
attaches Azure-specific attributes to every device it publishes in the node's
`ResourceSlice`:

| Attribute | Source | Example |
|---|---|---|
| `azure.dra.net/placementGroupId` | IMDS `compute/placementGroupId` | `c6c749e8-a38b-470e-8c94-2a7d00001bf0` |
| `azure.dra.net/vmSize` | IMDS `compute/vmSize` | `Standard_ND128isr_GB300_v6` |
| `azure.dra.net/interconnectGroupId` | IMDS `compute/interconnectGroupId` | `2deed8b4-d1e9-42be-a40a-9882201aa9f5` |
| `azure.dra.net/interconnectSubgroupId` | IMDS `compute/interconnectSubgroupId` | `0f0ba375-aaf1-4ac8-a579-a3b180d47de5` |

### InterconnectGroup (ICG) and InterconnectBlock (ICB)

Azure organizes GPU VMs with InfiniBand into a two-level topology:

- **InterconnectGroup (ICG)** (`Microsoft.Network/interconnectGroups`) —
  defines the overall IB connectivity scope and contains one or more
  subgroups. Its `resourceGuid` is exposed via IMDS as
  `compute/interconnectGroupId`.
- **Subgroup** — each subgroup maps to a single InterconnectBlock. VMs in the
  same subgroup share the same physical IB fabric. The subgroup's
  `internalSubgroupId` is exposed via IMDS as `compute/interconnectSubgroupId`.
- **InterconnectBlock (ICB)** (`Microsoft.Compute/interconnectBlocks`) — the
  Compute-side resource representing a fixed-capacity block of VMs (e.g. 18
  for `Standard_ND128isr_GB300_v6`) wired to the same IB fabric.

Use `interconnectSubgroupId` in CEL selectors to pin a multi-node job to a
single IB fabric:

```yaml
- cel:
    expression: >-
      device.attributes["azure.dra.net"]["interconnectSubgroupId"] == "0f0ba375-aaf1-4ac8-a579-a3b180d47de5"
```

VMs in **different placement groups do not share an InfiniBand fabric** —
cross-placement-group RDMA traffic fails with transport errors. This is not
detectable from Kubernetes node labels or GPU-driver attributes. The
`placementGroupId` attribute lets CEL selectors in a `ResourceClaimTemplate`
constrain a multi-node job to a single IB fabric; see `gb300/`'s
`ib-same-fabric` template for the pattern. The same predicate can be added to
any MI300X template.

Look up the placement group IDs published by dranet in the current cluster:

```bash
kubectl get resourceslice
```

### IB-only NIC discovery and isolation

On Azure GPU SKUs, ConnectX VFs are often in **InfiniBand mode** with no
Ethernet netdev. dranet discovers them by:

1. Skipping IPoIB interfaces during netdev discovery
   (`--move-ib-interfaces=false`).
2. Recording the RDMA link name (`rdmaDevice`) on the PCI device; a device is
   IB-only when it has a non-empty `rdmaDevice` and no `ifName`.
3. At pod start, the dranet NRI plugin injects exactly the allocated
   `/dev/infiniband/uverbsN` (and `rdma_cm`) character devices into the
   container.

Without step 3, all `uverbs*` devices would be visible in every pod via
`privileged: true`, providing no isolation between workloads. With dranet,
isolation is enforced without `privileged: true` — each worker only sees the
`uverbs*` corresponding to its DRA-allocated NIC(s).

## Usage pattern

Both examples follow the same flow:

```bash
# 1. Apply the claim templates
kubectl apply -f resource-claim-template.yaml

# 2. Select a test case by editing `resourceClaimTemplateName:` in mpi-job.yaml,
#    then apply it
kubectl apply -f mpi-job.yaml

# 3. Wait for workers, then stream launcher logs
kubectl wait --for=condition=ready \
  pod -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=600s
launcher=$(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
kubectl logs -f "${launcher}"

# 4. Verify DRA-allocated device isolation in a worker
kubectl exec nccl-test-dra-worker-0 -- ls /dev/infiniband/

# Inspect the allocated devices on the claim
kubectl get resourceclaims
kubectl get resourceclaims -o yaml | grep -E 'name:|device:' | head -40
```

## Notes common to both examples

- `NCCL_IB_HCA=mlx5` tells the collective library to use all `mlx5_*` HCAs
  visible in the pod (see the [NCCL environment variables reference][nccl-env]).
  RCCL is NCCL-compatible and honors the same `NCCL_IB_HCA` variable (there
  is no separate `RCCL_IB_HCA`; see the [RCCL environment variables
  reference][rccl-env]), so the same setting applies to both the GB300
  (NCCL) and MI300X (RCCL) jobs. Because dranet only exposes the
  DRA-allocated HCAs, the collective library automatically restricts itself
  to those — no per-workload HCA allow-list is needed.

[nccl-env]: https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html
[rccl-env]: https://rocm.docs.amd.com/projects/rccl/en/develop/api-reference/env-variables.html
- **`/dev/shm`**: NCCL/RCCL needs >64 MiB of shared memory for init. Both
  MPIJobs mount `emptyDir: {medium: Memory, sizeLimit: 8Gi}` at `/dev/shm`;
  without this, init fails with `No space left on device (28)` while creating
  `/dev/shm/nccl-*`.
- **Launcher warmup**: each launcher sleeps ~60 s before `mpirun` so workers
  have time to start `sshd`. Without it, mpirun hits
  `ORTE does not know how to route a message` and the MPIJob backs off.
