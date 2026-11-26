# Network-aware scheduling on a rail-optimized fabric

This example schedules a job of two Pods on nodes that are near in the
network, and gives each Pod one interface on each rail. It uses two things
that exist today:

- **Kubernetes 1.37:** a `PodGroup` with a topology constraint. All the Pods
  of the group go to nodes with the same value of a node label.
- **DRANET:** a ResourceClaim for each Pod, with one interface for each rail.

The lab is an emulated fabric: the switches are Nokia SR Linux containers,
the nodes are the nodes of a kind cluster, and
[netlab](https://netlab.tools/plugins/kind/) connects them.

## The fabric

```
                    spine
          /       /       \       \
      leaf1    leaf2     leaf3    leaf4
      rail 1   rail 2    rail 1   rail 2
      \__ group a __/    \__ group b __/
```

| Node | Leaf group | `eth1` (rail 1) | `eth2` (rail 2) |
|---|---|---|---|
| `tas-worker` | a | `leaf1` | `leaf2` |
| `tas-worker2` | a | `leaf1` | `leaf2` |
| `tas-worker3` | b | `leaf3` | `leaf4` |

- A leaf group is the set of leaves that serves the same nodes. In a
  rail-optimized fabric, interface k of each node of a group goes to leaf k
  of the group.
- Two nodes of one group reach each other through one leaf on each rail.
  Two nodes of different groups reach each other through the spine, also on
  the same rail.
- Each leaf is one subnet with a gateway. The TTL of a ping reply shows the
  path: 64 on one leaf, 61 through a leaf, the spine and a leaf.

## Requirements

- Docker, [kind](https://kind.sigs.k8s.io/) and `kubectl`.
- netlab with the kind plugin, which is in release 26.02 and later:
  `pip install networklab`. The example does not use Ansible.
- [containerlab](https://containerlab.dev/install/), in a version that the
  netlab release supports. The user must be able to run it without a
  password.
- The image `ghcr.io/nokia/srlinux` (2.4 GB), which containerlab pulls.

The example was tested with netlab 26.10 and containerlab 0.83.0.

The lab starts 5 switches and 4 kind nodes. If `kube-proxy` fails with
`too many open files`, the host has no free inotify instances: increase
`fs.inotify.max_user_instances`.

## Start the lab

```
./up.sh
```

`up.sh` builds a DRANET image from this repository, starts the fabric and
the kind cluster, gives the fabric interfaces of the nodes their addresses,
and installs DRANET. The kind cluster has the feature gates
`GenericWorkload` and `TopologyAwareWorkloadScheduling`, and the API
`scheduling.k8s.io/v1beta1` (see `kind.yaml`).

The cluster has its own kubeconfig file. `up.sh` prints the `export
KUBECONFIG=...` line to use it. Run the commands of this page in this
directory.

DRANET publishes the fabric interfaces of each node:

```
$ kubectl get resourceslices -o custom-columns='NODE:.spec.nodeName,DEVICES:.spec.devices[*].name'
NODE                DEVICES
tas-control-plane   <none>
tas-worker          eth1,eth2
tas-worker2         eth1,eth2
tas-worker3         eth1,eth2
```

## 1. Describe the fabric

The cluster does not know the fabric. Give it the two facts that the
scheduling needs.

The leaf group of each node, as a node label:

```
kubectl label node tas-worker tas-worker2 fabric.example.com/leaf-group=a
kubectl label node tas-worker3 fabric.example.com/leaf-group=b
```

The rail of each interface, as a DeviceClass for each rail. In this lab the
name of an interface tells its rail:

```
kubectl apply -f manifests/rails.yaml
```

## 2. A job without a topology constraint

Make one node of group a unavailable, so that each group has one free node:

```
kubectl cordon tas-worker2
```

`job-plain` is a `PodGroup` of two Pods that are scheduled together. Each
Pod has a claim for one interface on each rail.

```
$ kubectl apply -f manifests/job-plain.yaml
$ kubectl wait --for=condition=ready pod -l app=job-plain
$ ./show-paths.sh job-plain-0 job-plain-1
job-plain-0: node tas-worker, leaf group a
job-plain-1: node tas-worker3, leaf group b
rail1: 198.18.1.12 -> 198.18.3.12: ttl=61
rail2: 198.18.2.12 -> 198.18.4.12: ttl=61
```

The Pods have interfaces on the same rails, but the nodes are in different
leaf groups. The traffic goes through the spine. Which of the two Pods is on
which node can be different in your run.

Remove the job. DRANET does not put the addresses back on the nodes when the
interfaces return, so configure the nodes again:

```
kubectl delete -f manifests/job-plain.yaml
kubectl wait --for=delete pod -l app=job-plain
./configure-nodes.sh
```

## 3. The same job with a topology constraint

`job-tas` adds the constraint to the `PodGroup`:

```yaml
  schedulingConstraints:
    topology:
    - key: fabric.example.com/leaf-group
```

No leaf group has two free nodes, so the job does not start:

```
$ kubectl apply -f manifests/job-tas.yaml
$ kubectl get podgroup job-tas -o jsonpath='{.status.conditions[0].message}{"\n"}'
pod group is unschedulable, 0/2 placements are available, first placement status: minCount (2) cannot be satisfied: 1 scheduled, 0 remaining, ...
```

Make the node of group a available again. The job starts in group a:

```
$ kubectl uncordon tas-worker2
$ kubectl wait --for=condition=ready pod -l app=job-tas
$ ./show-paths.sh job-tas-0 job-tas-1
job-tas-0: node tas-worker, leaf group a
job-tas-1: node tas-worker2, leaf group a
rail1: 198.18.1.12 -> 198.18.1.13: ttl=64
rail2: 198.18.2.12 -> 198.18.2.13: ttl=64
```

The traffic of each rail stays on one leaf.

## Remove the lab

```
./down.sh
```

containerlab changes the host while the lab runs: it adds the switches to
`/etc/hosts` and adds a file to `/etc/ssh/ssh_config.d/`. It removes the two
when the lab stops.

## What the example does by hand

- **The node label.** Nothing in the cluster knows the leaf group of a node.
  On a cloud, the provider sets labels of this type. On bare metal, the
  administrator or another tool must set them.
- **The rail of an interface.** The DeviceClass selects by interface name.
  This is correct only if all the nodes are cabled in the same order and
  find their network cards in the same order.

## Limits

- The topology constraint is alpha in Kubernetes 1.37. A `PodGroup` can have
  one constraint, and it is a requirement: there is no preference and no
  second level.
- The interfaces of the fabric are veth interfaces. They have no PCI device,
  no SR-IOV and no RDMA. DRANET does not publish a veth interface while a
  Pod has it.
- The TTL shows the path of the traffic, not its speed.
