# OKE GB200 Placement Group / Topology-Aware Scheduling

Demonstrates how DRANET exposes OCI RDMA topology information as DRA device
attributes, enabling workloads to constrain scheduling to nodes that share a
common fabric domain.

## Problem

On OKE, BM.GPU.GB200-v3.4 nodes are organized into a multi-level topology
hierarchy. Nodes that are further apart in this hierarchy may share less
RDMA fabric bandwidth or, in the worst case, be in separate network islands
where RDMA traffic degrades significantly.

This hierarchy is not visible from standard Kubernetes node labels or NVIDIA
GFD attributes. Without topology awareness, multi-node NCCL jobs can be
scheduled across fabric boundaries.

## Solution

DRANET queries the OCI Instance Metadata Service (IMDS) at
`/opc/v2/host/` and attaches the following attributes to every RDMA device
in the node's ResourceSlice:

| Attribute | Grouping | Description |
|---|---|---|
| `oke.dra.net/hpcIslandId` | ~2000 nodes | Largest topology grouping |
| `oke.dra.net/networkBlockId` | ~64–128 nodes | Mid-level grouping |
| `oke.dra.net/localBlockId` | ~8–32 nodes | Closest grouping |
| `oke.dra.net/rackId` | ~8 nodes | Physical rack |
| `oke.dra.net/gpuMemoryFabricId` | varies | GPU memory fabric (NVLink domain) |

Workloads can use CEL selectors in ResourceClaimTemplates to constrain
allocation to devices sharing the same topology attribute, ensuring all workers
land on nodes within a common fabric domain.

## Example: Same-LocalBlock Scheduling

A ResourceClaimTemplate that restricts NIC allocation to devices sharing the
same `localBlockId`, ensuring all nodes are in the closest network grouping:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: 2nic-aligned-local-block
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
          count: 2
          selectors:
          - cel:
              expression: >-
                device.attributes["dra.net"].rdma == true &&
                device.attributes["dra.net"].virtual == false &&
                device.attributes["dra.net"].numaNode == 0 &&
                device.attributes["oke.dra.net"].localBlockId == "<localBlockId>"
```

Replace `<localBlockId>` with the value from your cluster's ResourceSlice:

```bash
kubectl get resourceslice -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
seen = set()
for rs in data['items']:
    if rs['spec'].get('driver') != 'dra.net': continue
    node = rs['spec']['nodeName']
    for d in rs['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        lid = attrs.get('oke.dra.net/localBlockId', {}).get('string', '')
        if lid and lid not in seen:
            seen.add(lid)
            print(f'{node}: localBlockId={lid}')
            break
"
```

## Topology Hierarchy on GB200

On a 16-node BM.GPU.GB200-v3.4 cluster, the observed topology is:

```
hpcIslandId      ← all 16 nodes in the same island
  networkBlockId ← all 16 nodes in the same network block
    localBlockId ← all 16 nodes in the same local block
      rackId     ← nodes spread across 4 racks (~4 nodes/rack)
```

All nodes in the cluster share the same `gpuMemoryFabricId`, meaning they
are all connected via the same NVLink fabric domain.

## Example: Same-NetworkBlock Scheduling (looser constraint)

For larger clusters spanning multiple local blocks, use `networkBlockId`:

```yaml
- cel:
    expression: >-
      device.attributes["dra.net"].rdma == true &&
      device.attributes["dra.net"].virtual == false &&
      device.attributes["dra.net"].numaNode == 0 &&
      device.attributes["oke.dra.net"].networkBlockId == "<networkBlockId>"
```

## Example: Same-GPU-Memory-Fabric (NVLink domain)

For jobs that rely on NVLink peer access between nodes, constrain to a single
`gpuMemoryFabricId`:

```yaml
- cel:
    expression: >-
      device.attributes["dra.net"].rdma == true &&
      device.attributes["dra.net"].virtual == false &&
      device.attributes["dra.net"].numaNode == 0 &&
      device.attributes["oke.dra.net"].gpuMemoryFabricId == "<gpuMemoryFabricId>"
```

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | ResourceClaimTemplates using topology constraints |

## Verification

Check topology attributes in your ResourceSlices:

```bash
kubectl get resourceslice -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
seen = set()
for rs in data['items']:
    if rs['spec'].get('driver') != 'dra.net': continue
    node = rs['spec']['nodeName']
    if node in seen: continue
    for d in rs['spec'].get('devices', []):
        attrs = d.get('attributes', {})
        oke = {k.split('/')[-1]: v.get('string','') for k, v in attrs.items() if k.startswith('oke.dra.net')}
        if oke:
            seen.add(node)
            print(f'{node}:')
            for k, v in sorted(oke.items()):
                print(f'  {k}: {v}')
            break
"
```
