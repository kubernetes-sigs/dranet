---
title: "OCI OKE RDMA with DRANET"
date: 2026-09-08T00:00:00Z
---

DRANET gives pods IPvlan children of the RDMA NICs of an OKE bare metal GPU node, so the RDMA NICs stay on the host. On Oracle Kubernetes Engine (OKE), Oracle Cloud Agent (OCA) configures each Ethernet RDMA NIC (`rdma0`, `rdma1`, and so on; the number of RDMA NICs depends on the shape) with one IPv4 address from the RDMA network. OCA uses `10.224.0.0/12` as that network by default.

The examples for the OKE shapes live under [examples/oci_oke_examples](https://github.com/kubernetes-sigs/dranet/tree/main/examples/oci_oke_examples).

## What the OKE provider publishes

The provider reads the OCI Instance Metadata Service (IMDS) at startup and refreshes it every 5 minutes. Every device gets these node-level attributes when IMDS supplies them: `oke.dra.net/hpcIslandId`, `oke.dra.net/networkBlockId`, `oke.dra.net/localBlockId`, `oke.dra.net/rackId`, `oke.dra.net/gpuMemoryFabricId`, `oke.dra.net/shape`, `oke.dra.net/rdmaFabricIpv6`, and `oke.dra.net/rdmaFabricPlanes`.

## The oke-rdma profile

The provider advertises the `oke-rdma` profile for each Ethernet RDMA NIC. A claim for an RDMA NIC gets an IPvlan child of the RDMA NIC with its own address in `10.222.0.0/15`, a route table with a source rule, and the ARP settings of the RDMA NIC. The RDMA NIC itself stays on the host with its address and settings. One pod uses an RDMA NIC at a time. The child keeps the parent name inside the pod.

The child address has its own GID index on the RDMA NIC, different from the index of the parent address. Do not set `NCCL_IB_GID_INDEX` for a job that uses the children. NCCL selects the GID index itself.

The profile needs these conditions:

- Each RDMA NIC has the `rdmaN` name or an address in the OCA RDMA network. Some shapes, for example BM.Optimized3.36, keep the operating system name of the RDMA NIC. On such a shape the provider identifies the RDMA NIC by that address. A claim that is prepared before OCA assigns the address moves the RDMA NIC into the pod.
- The RDMA subsystem runs in shared network namespace mode (`netns_mode=1` for `ib_core`). In exclusive mode the claim fails with the message `use shared RDMA mode`.
- All nodes that share one RDMA network use one primary VNIC subnet.
- The child range holds 2^(prefix length - 15) RDMA NICs per node for the primary VNIC subnet prefix length: 16 RDMA NICs need a /19 or smaller subnet. A claim for an RDMA NIC outside the range fails with an error that names the RDMA NIC and the subnet.
- No VCN subnet, pod CIDR, or service CIDR uses `10.222.0.0/15`.

The profile rejects `interface.type: Passthrough`, DHCP, unnumbered addressing, and addresses in the claim. A claim with its own `routes`, `rules`, or a VRF owns the routing.

Native InfiniBand RDMA NICs and RDMA NICs on an IPv6 fabric get no profile and move into the pod as before.

The profile needs `--profile-provider=cloud`, the default. With `webhook`, the webhook receives the `oke-rdma` profile and must resolve it itself. With `none` the profile is removed but the IPvlan type stays, so a claim needs its own `addresses` and the RDMA NIC stays on the host. The profile validation does not run under `none`, so a claim that sets `interface.type: Passthrough` explicitly moves the RDMA NIC into the pod, and the OCA routing of that RDMA NIC is not restored on return.

## Oracle Cloud Agent configuration

The provider assumes the OCA defaults. Mount the OCA configuration directory on an image that uses another RDMA network. With the Helm chart:

```yaml
extraVolumes:
  - name: oca-hpc-config
    hostPath:
      path: /etc/oracle-cloud-agent/plugins/oci-hpc/oci-hpc-configure
      type: DirectoryOrCreate
extraVolumeMounts:
  - name: oca-hpc-config
    mountPath: /etc/oracle-cloud-agent/plugins/oci-hpc/oci-hpc-configure
    readOnly: true
```

The provider reads the files when the process starts. Restart the DRANET pods after a change.

## Example

A DeviceClass for all DRANET devices and a claim for one RDMA NIC:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: dra.net
spec:
  selectors:
  - cel:
      expression: device.driver == "dra.net"
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: rdma15
spec:
  devices:
    requests:
    - name: rdma15
      exactly:
        deviceClassName: dra.net
        selectors:
        - cel:
            expression: has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName == "rdma15"
```

Inside a pod that references the claim:

```text
rdma15  10.223.237.19/15
table 115: 10.222.0.0/15 dev rdma15 scope link src 10.223.237.19
rule: from 10.223.237.19 lookup 115
```

For a training job, request every RDMA NIC of the node in one claim, one request per RDMA NIC.
