---
title: "Feature Gates"
date: 2026-11-11T00:00:00Z
---

Feature gates turn alpha DRANET features on or off. Alpha features are
disabled by default and may change or be removed in a later release.

Set them with the `--feature-gates` flag of the DRANET binary, as a
comma-separated list of `<name>=<true|false>` pairs:

```sh
/dranet --feature-gates=PersistentResourceSliceAttributes=true,DRAListTypeAttributes=true
```

With the Helm chart, set the same list in `args.featureGates`:

```yaml
args:
  featureGates: "PersistentResourceSliceAttributes=true"
```

DRANET stops at startup if the list names an unknown feature gate.

| Feature gate | Default | Stage | Since |
|--------------|---------|-------|-------|
| [`DRAListTypeAttributes`](#dralisttypeattributes) | `false` | Alpha | v1.5.0 |
| [`PersistentResourceSliceAttributes`](#persistentresourcesliceattributes) | `false` | Alpha | v1.4.0 |

## DRAListTypeAttributes

Publishes the standardized `resource.kubernetes.io/numaNode` attribute as a
list of integers instead of a single integer. The list starts with the
device's NUMA node, followed by the other NUMA nodes on the same socket that
are equally close to the device. The `dra.net/numaNode` attribute is
unchanged.

List-valued attributes need the `DRAListTypeAttributes` feature gate of the
Kubernetes control plane as well. Enable the gate in Kubernetes and in DRANET
together. If it is enabled only in DRANET, the API server cannot handle the
list-valued attributes and the `ResourceSlice` operations fail. See
[Quick Start](/docs/quick-start/) for more about the attribute.

## PersistentResourceSliceAttributes

Keeps the attributes of allocated devices in the `ResourceSlice` while the
devices are in use by a pod.

When DRANET gives a network interface to a pod, it moves the interface into the
pod network namespace. The next scan of the host does not find the interface
any more. Without this feature gate, DRANET publishes the device without the
attributes it reads from the interface, such as `dra.net/ifName`,
`dra.net/mac` or `dra.net/ipv4`. A device that exists only as a network
interface, without a PCI device behind it, disappears from the
`ResourceSlice`.

With the feature gate enabled, when DRANET prepares a `ResourceClaim`, it
stores a snapshot of the allocated device as the last scan found it, before
the interface leaves the host. Until the claim is unprepared, DRANET adds the
snapshot to each `ResourceSlice` update:

- If the scan still finds the device, DRANET adds the attributes and
  capacities of the snapshot that the scan did not find. The values from the
  scan take precedence.
- If the scan does not find the device, DRANET publishes the snapshot as it
  is.

The `--filter` expression applies to the result, as it does to scanned
devices.

DRANET keeps the snapshots in the database set with `--db-path`, so they
survive a restart of DRANET. If `--db-path` is empty, DRANET keeps them in
memory and they are lost when DRANET restarts.
