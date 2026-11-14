---
title: "VFIO"
date: 2026-11-14T00:00:00Z
---

By default DRANET gives a Pod the network interface of the allocated device: it
moves the netdev into the Pod's network namespace. A claim with a `vfio` config
gets the whole PCI function instead. DRANET binds the function to `vfio-pci`
and adds the VFIO character devices to the Pod's containers, so a process in
the Pod, for example a VMM or a DPDK application, can drive the function
directly. When the claim is released, DRANET binds the original driver back.

Because the function stays on its kernel driver until a claim asks for VFIO,
the same functions can serve both kinds of claims without being split between
them ahead of time.

### Requirements

* The `VFIODriverRebind` feature gate (alpha) on the DRANET daemon:
  `--feature-gates=VFIODriverRebind=true`.
* An IOMMU on the node (for example `intel_iommu=on` or `amd_iommu=on`) and the
  `vfio-pci` module loaded. DRANET does not load kernel modules.
* A container runtime with CDI enabled, if the workload reads the PCI address
  from the device metadata (see [PCI address in the workload](#pci-address-in-the-workload)).
  CDI is enabled by default in containerd 2.x and CRI-O.
* The checkpoint database (`--db-path`) on a host path, as in the default
  deployment, so the original driver of each function survives a daemon restart.

### Device model

There is one device per PCI function, published while the function is on its
kernel driver with all its usual attributes. With the gate enabled, each PCI
device also gets:

| Attribute | Meaning |
|---|---|
| `resource.kubernetes.io/pciBusID` | The standard PCI address attribute. |
| `dra.net/vfio` | `true` when the function can be bound to `vfio-pci` safely. |

`dra.net/vfio` is `true` only when all of the following hold:

| Condition | Why |
|---|---|
| `vfio-pci` is loaded. | Without it the bind cannot succeed. |
| The function has an IOMMU group, and no other member of the group is bound to a driver (PCI bridges excepted). | A group can only be handed out whole, and a claim cannot ask for several functions as one unit. |
| The function is not a PF with VFs. | Rebinding the PF removes every VF, including ones held by other claims. |
| Its network interface is not part of a bond, bridge, VLAN or other stacked device. | Rebinding would remove the interface from under the stacked device. |
| Its network interface has no host IP addresses. | Rebinding would cut the node's own connectivity. Default-route interfaces are not published at all. |

Functions that are already bound to `vfio-pci` when DRANET scans them are not
published, as before. DRANET never rebinds a function for which it has no
record, because it would not know which driver to bind back.

### Lifecycle

| Step | What DRANET does |
|---|---|
| Prepare | Checks the conditions above again. Writes the currently bound driver to the checkpoint. Then sets `driver_override=vfio-pci`, unbinds the function and probes it, and confirms it is bound to `vfio-pci`. Records `/dev/vfio/<group>` and `/dev/vfio/vfio`, plus `/dev/vfio/devices/vfioN` and `/dev/iommu` on iommufd kernels. |
| CreateContainer | Adds the recorded character devices to each container, the same way as the RDMA character devices. |
| RunPodSandbox | Reports the `Ready` condition with reason `VFIODeviceReady`. There is no network namespace work. |
| Unprepare | Clears `driver_override`, unbinds the function and probes it, and confirms it is back on the recorded driver. |

The override is written before the unbind and kept for the life of the claim,
so nothing that probes the function in between can bind it back to its kernel
driver.

### Restarts and failures

| Situation | Behavior |
|---|---|
| DRANET restarts during prepare | The checkpoint already has the original driver. kubelet retries the prepare and DRANET completes the bind. |
| DRANET restarts while the claim is allocated | The record is kept in the checkpoint. The next unprepare binds the original driver back. |
| The bind fails, for example because the IOMMU is disabled | DRANET clears the override and probes the function back to its driver. Prepare fails with a `ClaimPrepareFailed` event. |
| Binding the original driver back fails | DRANET keeps the record and returns the error, so kubelet retries the unprepare. |
| The PF driver is reloaded or the VF count changes while allocated | The function no longer exists. The workload loses it, and DRANET drops the record without trying to restore it. |
| `dra.net/vfio` turns `false` between allocation and prepare | Prepare fails. Recreate the Pod to get a new allocation. |
| The gate is disabled while claims are allocated | Allocated functions are still bound back on release. New `vfio` claims are refused. |

### The ResourceSlice while a claim is allocated

After the bind, the function is on `vfio-pci`, so the next scan drops it from
the ResourceSlice, in the same way a netdev moved into a Pod loses its
attributes. With the `PersistentResourceSliceAttributes` feature gate, DRANET
keeps publishing it from the snapshot taken during prepare, before the bind.
Scheduling is not affected either way: the scheduler does not look up an
allocated device again.

### Selecting devices

An administrator can create a DeviceClass for VFIO, so users only pick the class:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: dranet-vfio
spec:
  selectors:
  - cel:
      expression: device.driver == "dra.net" && device.attributes["dra.net"].vfio == true
  config:
  - opaque:
      driver: dra.net
      parameters:
        vfio: {}
```

Before allocation the function is on its kernel driver, so claims can still
select on attributes such as `dra.net/ifName`, `dra.net/rdma`,
`resource.kubernetes.io/numaNode` or `resource.kubernetes.io/pcieRoot`.
`dra.net/rdma` describes the hardware: a function given through VFIO gets no
RDMA character devices on the host side.

### PCI address in the workload

The Pod gets no network interface, so the workload needs the PCI address of
the function it got. DRANET reports `resource.kubernetes.io/pciBusID` in the
standard [DRA device metadata](https://kubernetes.io/docs/tasks/configure-pod-container/assign-resources/access-dra-device-metadata/)
(KEP-5304). The kubelet plugin library writes the metadata file and mounts it
into the containers through CDI. The file is written in the
`metadata.resource.k8s.io/v1beta1` and `v1alpha1` versions.

### Not supported

* Creating VFs or changing `sriov_numvfs`.
* PF-side VF settings such as MAC address, VLAN, trust or rate limits.
* Functions bound to `vfio-pci` before DRANET scans them.
* VFIO no-IOMMU mode (`vfio.enable_unsafe_noiommu_mode`): functions have no
  IOMMU group before the bind, so `dra.net/vfio` is `false`.
* User-space drivers other than `vfio-pci`.
* Pods using the host network.

An example is in [examples/vfio](https://github.com/kubernetes-sigs/dranet/tree/main/examples/vfio).
