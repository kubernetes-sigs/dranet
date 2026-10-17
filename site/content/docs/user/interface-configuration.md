---
title: "Interface Configuration"
date: 2025-05-25T11:30:40Z
---

To configure network interfaces in DRANET, users can provide custom configurations through the parameters field of a ResourceClaim or ResourceClaimTemplate. This configuration adheres to the NetworkConfig structure, which defines the desired state for network interfaces and their associated routes.

### Network Configuration Overview

The primary structure for custom network configuration is NetworkConfig. It encompasses settings for the network interface itself and any specific routes and rules to be applied within the Pod's network namespace.

```go
type NetworkConfig struct {
	// Interface defines core properties of the network interface.
	// Settings here are typically managed by `ip link` commands.
	Interface InterfaceConfig `json:"interface"`

	// Routes defines static routes to be configured for this interface.
	Routes []RouteConfig `json:"routes,omitempty"`

	// Rules defines routing rules to be configured for this interface.
	Rules []RuleConfig `json:"rules,omitempty"`

	// Neighbors defines permanent neighbor (ARP/NDP) entries to be added for this interface.
	Neighbors []NeighborConfig `json:"neighbors,omitempty"`

	// Ethtool defines hardware offload features and other settings managed by `ethtool`.
	Ethtool *EthtoolConfig `json:"ethtool,omitempty"`
}
```

#### Interface Configuration

The InterfaceConfig structure allows you to specify details for a single network interface.

```go
type InterfaceConfig struct {
	// Name is the desired logical name of the interface inside the Pod (e.g., "net0", "eth_app").
	// If not specified, DRANET may use or derive a name from the original interface.
	Name string `json:"name,omitempty"`

	// Type selects how the allocated device is presented to the Pod:
	//   - "Passthrough" (default): the network device itself is moved into the
	//     Pod's network namespace.
	//   - "IPVLAN": the device stays in the host namespace and an IPVLAN
	//     subinterface is created on top of it inside the Pod.
	// It may be set by the cloud provider or in the user's ResourceClaim.
	// If empty, it is treated as "Passthrough".
	Type InterfaceType `json:"type,omitempty"`

	// Addresses is a list of IP addresses in CIDR format (e.g., "192.168.1.10/24")
	// to be assigned to the interface.
	Addresses []string `json:"addresses,omitempty"`

	// MTU is the Maximum Transmission Unit for the interface.
	// An IPVLAN subinterface must not exceed the parent MTU. When unset, the
	// child inherits the parent MTU.
	MTU *int32 `json:"mtu,omitempty"`

	// HardwareAddr is the MAC address of the interface. Passthrough only: an
	// IPVLAN subinterface always uses its parent's MAC address.
	HardwareAddr *string `json:"hardwareAddr,omitempty"`

	// GSOMaxSize sets the maximum Generic Segmentation Offload size for IPv6.
	// Managed by `ip link set <dev> gso_max_size <val>`. For enabling Big TCP.
	GSOMaxSize *int32 `json:"gsoMaxSize,omitempty"`

	// GROMaxSize sets the maximum Generic Receive Offload size for IPv6.
	// Managed by `ip link set <dev> gro_max_size <val>`. For enabling Big TCP.
	GROMaxSize *int32 `json:"groMaxSize,omitempty"`

	// GSOv4MaxSize sets the maximum Generic Segmentation Offload size.
	// Managed by `ip link set <dev> gso_ipv4_max_size <val>`. For enabling Big TCP.
	GSOIPv4MaxSize *int32 `json:"gsoIPv4MaxSize,omitempty"`

	// GROv4MaxSize sets the maximum Generic Receive Offload size.
	// Managed by `ip link set <dev> gro_ipv4_max_size <val>`. For enabling Big TCP.
	GROIPv4MaxSize *int32 `json:"groIPv4MaxSize,omitempty"`

	// ARPIgnore controls which ARP requests the interface answers through
	// /proc/sys/net/ipv4/conf/<iface>/arp_ignore. Valid values are 0-3 and 8.
	// Linux uses max(conf/all, conf/<iface>) as the effective value.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly.
	ARPIgnore *int32 `json:"arpIgnore,omitempty"`

	// ARPAnnounce controls the source address used in ARP requests through
	// /proc/sys/net/ipv4/conf/<iface>/arp_announce. Valid values are 0-2.
	// Linux uses max(conf/all, conf/<iface>) as the effective value.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly.
	ARPAnnounce *int32 `json:"arpAnnounce,omitempty"`

	// AcceptRA controls IPv6 router advertisement acceptance through
	// /proc/sys/net/ipv6/conf/<iface>/accept_ra. Valid values are 0-2.
	// Moving the interface resets it to the destination namespace default, so it
	// must be requested explicitly.
	AcceptRA *int32 `json:"acceptRA,omitempty"`

	// DADTransmits controls how many Duplicate Address Detection probes the
	// interface sends for a new IPv6 address through
	// /proc/sys/net/ipv6/conf/<iface>/dad_transmits.
	DADTransmits *int32 `json:"dadTransmits,omitempty"`

	// RouterSolicitationDelay controls how long the interface waits before
	// sending its first Router Solicitation through
	// /proc/sys/net/ipv6/conf/<iface>/router_solicitation_delay, in seconds.
	RouterSolicitationDelay *int32 `json:"routerSolicitationDelay,omitempty"`

	// RouterSolicitationInterval controls how long the interface waits before
	// it repeats an unanswered Router Solicitation through
	// /proc/sys/net/ipv6/conf/<iface>/router_solicitation_interval, in seconds.
	RouterSolicitationInterval *int32 `json:"routerSolicitationInterval,omitempty"`

	// DisableIPv6 turns IPv6 off or on for the interface through
	// /proc/sys/net/ipv6/conf/<iface>/disable_ipv6.
	DisableIPv6 *bool `json:"disableIPv6,omitempty"`

	// AddrGenMode selects how the kernel generates the interface identifier of
	// the interface's IPv6 addresses through
	// /proc/sys/net/ipv6/conf/<iface>/addr_gen_mode.
	AddrGenMode *int32 `json:"addrGenMode,omitempty"`
}
```

* **name** (string, optional): The logical name that the interface will have inside the Pod (e.g., "eth0", "enp0s3"). If not specified, DRANET will keep the original name if compliant.
* **type** (string, optional): How the device is presented to the Pod. `Passthrough` (the default) moves the device into the Pod. `IPVLAN` keeps the device on the host and creates an IPVLAN subinterface on top of it inside the Pod.
* **addresses** ([]string, optional): A list of IP addresses in CIDR format (e.g., "192.168.1.10/24", "2001:db8::1/64") to be assigned to the interface.
* **mtu** (int32, optional): The Maximum Transmission Unit for the interface. For an `IPVLAN` subinterface the value must not exceed the parent MTU. When omitted, the child inherits the parent MTU.
* **hardwareAddr** (string, optional): The MAC address of the interface. Passthrough only. An `IPVLAN` subinterface always uses its parent's MAC address, so the field is rejected.
* **gsoMaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv6.
* **groMaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv6.
* **gsoIPv4MaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv4.
* **groIPv4MaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv4.
* **arpIgnore** (int32, optional): Which ARP requests the interface answers. Valid values are 0, 1, 2, 3, and 8. Sets `/proc/sys/net/ipv4/conf/<iface>/arp_ignore`.
* **arpAnnounce** (int32, optional): The source address the interface uses in ARP requests, from 0 to 2. Sets `/proc/sys/net/ipv4/conf/<iface>/arp_announce`.
* **acceptRA** (int32, optional): Whether the interface accepts IPv6 router advertisements. `0` rejects them, `1` accepts them when forwarding is off, and `2` accepts them also when forwarding is on. Sets `/proc/sys/net/ipv6/conf/<iface>/accept_ra`.
* **dadTransmits** (int32, optional): How many Duplicate Address Detection probes the interface sends for a new IPv6 address. Sets `/proc/sys/net/ipv6/conf/<iface>/dad_transmits`.
* **routerSolicitationDelay** (int32, optional): How many seconds the interface waits before sending its first Router Solicitation. Sets `/proc/sys/net/ipv6/conf/<iface>/router_solicitation_delay`.
* **routerSolicitationInterval** (int32, optional): How many seconds the interface waits before it repeats a Router Solicitation that got no answer; at least `1`. Sets `/proc/sys/net/ipv6/conf/<iface>/router_solicitation_interval`. `addressing: SLAAC` defaults it to `1`.
* **disableIPv6** (bool, optional): Whether IPv6 is off on the interface. Sets `/proc/sys/net/ipv6/conf/<iface>/disable_ipv6`. An interface takes the Pod namespace default when it moves or is created there, and some CNI plugins disable IPv6 in the Pod namespace on IPv4-only clusters (the OCI VCN-Native CNI on OKE does), so an interface that needs IPv6 in the Pod has to set `false`. `addressing: SLAAC` defaults it to `false`.
* **addrGenMode** (int32, optional): How the kernel generates the interface identifier of the interface's IPv6 addresses: `0` from the hardware address (EUI-64), `1` no link-local address, `2` from a stable secret, `3` random. Sets `/proc/sys/net/ipv6/conf/<iface>/addr_gen_mode`. `2` needs a `stable_secret` in the Pod namespace, which a new namespace does not have, so the kernel rejects it unless the Pod sets one. `addressing: SLAAC` on an IPVLAN interface defaults it to `3`.
* **addressing** (string, optional): How the interface gets its addresses: `Static` (the default, from `addresses` or a provider profile), `DHCP`, `SLAAC`, or `Unnumbered` (subinterfaces only). See [IPv6 autoconfiguration](#ipv6-autoconfiguration-slaac).

The kernel resets both ARP settings to the network namespace default when an interface
moves into a Pod, so a value configured on the host does not survive the move and has to
be requested here. The kernel resets `accept_ra` the same way when the interface moves.
The kernel creates IPv6 settings only for an interface with an MTU of 1280 or more.
A claim with `acceptRA`, `dadTransmits`, `routerSolicitationDelay`,
`routerSolicitationInterval`, `disableIPv6`, `addrGenMode` or `addressing: SLAAC`
is rejected when its `mtu` is below 1280, or when it sets no `mtu` and the interface would
keep a smaller MTU from the host. The check runs before the interface is touched. When the
interface has no `accept_ra` sysctl, for example on a node that boots with
`ipv6.disable=1`, `acceptRA: 0` is already satisfied, and `1` or `2` fail with an error
that says so.
Setups that attach several interfaces sharing one IP subnet, such as
multi-NIC RDMA nodes, typically need `arpIgnore: 1` and `arpAnnounce: 2`. Without them an
interface can answer ARP for another interface's address, or send requests with a source
address from the wrong subnet, which makes neighbor resolution pick the wrong link.

Linux uses the maximum of the namespace-wide `conf/all` value and the per-interface value
for both ARP settings. A per-interface ARP setting cannot reduce the effective value below
`conf/all`. New network namespaces normally inherit the IPv4 `conf/all` and `conf/default`
values from the initial network namespace, subject to `net.core.devconf_inherit_init_net`.
The IPv6 values start at the kernel defaults (`accept_ra` is 1) unless that sysctl is 1 or 3.
DRANET only changes the per-interface value.

##### IPVLAN subinterfaces

An `IPVLAN` subinterface is created inside the Pod network namespace on top of the
host device. The `mtu`, GSO and GRO sizes, `arpIgnore`, `arpAnnounce`, and `acceptRA` settings
apply to the child. They work the same way as on a passthrough interface. The child inherits
the TSO maximum of its parent. The kernel rejects a GSO size above that maximum. It also
rejects a GRO size above the global kernel maximum. A later change of the parent MTU
resets the child MTU to the new parent value.

A minimal claim configuration that requests an IPVLAN subinterface:

```yaml
config:
- opaque:
    driver: dra.net
    parameters:
      interface:
        type: "IPVLAN"
        name: "net1"
        addresses:
        - "192.0.2.10/24"
        mtu: 1400
        arpIgnore: 1
        arpAnnounce: 2
```

Two settings are not supported for subinterfaces and are rejected by validation:

* `hardwareAddr`: the child always uses the parent MAC address.
* DHCP addressing: unsupported and untested. The DHCP client runs on the host parent
  interface before the subinterface exists.

#### IPv6 autoconfiguration (SLAAC)

On fabrics where the routers hand out IPv6 prefixes, the host interfaces get their
addresses and default routes from Router Advertisements rather than from any
configuration DRANET can read off the node:

```
rdma0  inet6 fdcd:8200:cde5:20b7:a20:e7ff:fe96:708/64 dynamic mngtmpaddr proto kernel_ra
default via fe80::b061:4eff:fe0c:d0b7 dev rdma0 proto ra metric 1024 expires 1536sec
```

By default DRANET copies the host's addresses into the Pod as static ones. That works,
but the copy has no lifetimes and no relationship to the advertisement that produced it.
`addressing: SLAAC` instead lets the Pod autoconfigure itself:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: rdma-slaac
spec:
  devices:
    requests:
    - name: rdma
      exactly:
        deviceClassName: dra.net
    config:
    - requests: ["rdma"]
      opaque:
        driver: dra.net
        parameters:
          interface:
            addressing: SLAAC
            arpIgnore: 1
            arpAnnounce: 2
```

With `addressing: SLAAC` DRANET:

* inherits no IPv6 addresses from the host, and leaves out the host routes the Pod will
  re-learn from advertisements (`proto ra`). IPv4 has no autoconfiguration to defer to,
  so a dual-stack interface still inherits its IPv4 addresses, or takes IPv4 `addresses`
  from the claim, as any passthrough interface does;
* defaults `disableIPv6: false`, `acceptRA: 2`, `routerSolicitationDelay: 0` and
  `routerSolicitationInterval: 1`, and `dadTransmits: 0` when the interface keeps its
  hardware address, all of which you can override. `disableIPv6: false` matters on
  clusters whose CNI disables IPv6 in the Pod namespace: without it the interface never gets
  an address. The kernel resets these when the interface moves, so they have to be
  requested. `routerSolicitationDelay: 0` and `dadTransmits: 0` bring address acquisition
  down from about two seconds to the router's reply time, which is what makes it fit the
  runtime's deadline; `routerSolicitationInterval: 1` fits a second solicitation in the wait
  if the first is lost or the router delays its answer (RFC 4861 allows up to 500 ms).
  Skipping duplicate address detection is a deliberate trade-off: with the host's hardware
  address the Pod derives the address the host itself held on the same link and already
  verified. With a `hardwareAddr` the address is new to the link, so the kernel default
  stays and detection runs. Detection holds the link-local address tentative, and the
  kernel solicits only after that, so the address takes about two seconds, more than the
  default `--slaac-ready-timeout`; raise the timeout or set `dadTransmits: 0` explicitly;
* waits, after bringing the interface up inside the Pod, for a global unicast IPv6
  address that has finished duplicate address detection, and records it in the
  ResourceClaim's `status.devices[].networkData.ips` along with a `SLAACReady` condition.

DRANET waits after it has moved the interface and applied its link-level configuration
(ethtool, eBPF, VRF), and before it installs routes, rules and neighbours. Enslaving the
interface to a VRF cycles the link and drops the address it had, so the address reported is
the one that survives; and a route through a gateway in the advertised prefix can only be
installed once the advertisement has put the prefix on the link. If no address arrives in
time, the interface goes back to the host under its original name and up, and the sandbox
fails, so the kubelet retries rather than starting a workload on an interface with no
source address. The rollback covers that interface: devices the request attached before
it are returned by the runtime's StopPodSandbox for the failed sandbox, as after any other
attach failure.

How long the wait needs depends on how quickly the router answers a solicitation, so on
fabrics whose routers rate-limit or delay their answers, raise `--slaac-ready-timeout`.

The wait is bounded by `--slaac-ready-timeout` (1.5s by default) and by the deadline of
the runtime request that triggered it, minus a reserve for that rollback. The reserve is
the larger of `--slaac-rollback-reserve` (500ms by default) and the time the move of that
interface took, because moving it back costs about as much.

Attaching several NICs one at a time can outrun the runtime's timeout for an NRI plugin
request, 2s by default in both containerd and CRI-O, so the check looks at how much of that request is
left. Before moving a SLAAC interface at all, it checks that the move and, if need be, the
move back still fit in the request, estimated from the longest move the request has done so
far; if they do not, the sandbox fails without the interface being touched, with a message
pointing at the runtime's NRI plugin request timeout. With more than the reserve remaining after
the move it waits for the smaller of the two and rolls back on timeout. With the reserve or
less remaining there is time to roll back but not to wait, so it checks once, rolls back,
and the kubelet retries. Only once the deadline has actually passed does it log a warning
and finish the wait on `--slaac-ready-timeout` instead: the runtime is no longer waiting for
the request, so an error would not fail the sandbox and taking the interface back would only
leave the Pod without a NIC it is about to use. For the same reason a wait that fails after
the deadline leaves the interface in place without an address: DRANET records the failure
on the claim and in an event, leaves out that interface's routes, and goes on with the
other devices of the Pod.

A failed wait is also recorded on the ResourceClaim, as a `SLAACReady` condition with
status `False` and the reason the interface was not ready, next to the Pod's
`NetworkDeviceNotReady` event.

A rolled-back interface is back on the host under its original name and up, as after any
detach; settings the host had on it beyond that, such as a VRF membership, are not restored.

`SLAAC` cannot be combined with IPv6 `addresses`, `acceptRA: 0`, `disableIPv6: true`,
`addrGenMode: 1` (no link-local address, so no solicitation) or `addrGenMode: 2` (no
`stable_secret` in the Pod namespace).

##### SLAAC on an IPVLAN subinterface

With `type: IPVLAN` the parent NIC stays on the host and the Pod gets a child that
autoconfigures its own address from the parent link's router advertisements:

```yaml
interface:
  type: IPVLAN
  addressing: SLAAC
```

The child shares its parent's hardware address, so three things differ from a passthrough
interface:

* `addrGenMode` defaults to `3` (random). EUI-64 would give the child the parent's own
  addresses, so `0` is rejected as well;
* duplicate address detection is skipped by default, because a random 64-bit interface
  identifier only collides with another random one. With detection on, the link-local
  address is held tentative and the kernel solicits only after that, so the address takes
  about two seconds, more than the default `--slaac-ready-timeout`; without it, about the
  router's reply time (0.1 to 0.5 seconds measured). Set `dadTransmits` to keep detection,
  and raise the timeout with it;
* the child inherits no host addresses, IPv4 included: the parent keeps them on the host,
  and a copy would give the Pod the host's own address. IPv4 `addresses` from the claim
  are applied as usual.

A child has nothing to roll back to the host, so DRANET does not wait for each child in
turn: it creates all of the Pod's subinterfaces first and waits for their addresses
together, after the last device, within the same `--slaac-ready-timeout` and request
deadline. A Pod with one child per RDMA NIC then gets its addresses in the time one of them
takes. If any of them has no address in time, DRANET deletes all of them and the sandbox
fails, so the kubelet retries; after the request deadline it leaves them in place,
completes the ones that got an address, and records the others on the claim.

#### Route Configuration (RouteConfig)

The RouteConfig structure defines individual network routes to be added to the Pod's network namespace, associated with the configured interface.

```go
type RouteConfig struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Source      string `json:"source,omitempty"`
	Scope       uint8  `json:"scope,omitempty"`
	Table       int    `json:"table,omitempty"`
}
```

* **destination** (string, optional): The destination network in CIDR format (e.g., "0.0.0.0/0" for a default route, "10.0.0.0/8" for a specific subnet).  
* **gateway** (string, optional): The IP address of the gateway for the route. This field is mandatory for routes with Universe scope (0).  
* **source** (string, optional): An optional source IP address for policy routing.  
* **scope** (uint8, optional): The scope of the route. Only Link (253) or Universe (0) are allowed.  
  * Link (253): Routes directly to a device without a gateway (e.g., for directly connected subnets).  
  * Universe (0): Routes to a network via a gateway.
* **table** (int, optional): The routing table to use for the route. Defaults to the main table (254) if not specified.

#### Rule Configuration (RuleConfig)

The RuleConfig structure defines individual routing rules to be added to the Pod's network namespace.

```go
type RuleConfig struct {
	// Priority is the priority of the rule.
	Priority int `json:"priority,omitempty"`
	// Source is the source IP address for the rule.
	Source string `json:"source,omitempty"`
	// Destination is the destination IP address for the rule.
	Destination string `json:"destination,omitempty"`
	// Table is the routing table to use for the rule.
	Table int `json:"table,omitempty"`
}
```

* **priority** (int, optional): The priority of the rule. Lower values mean higher priority. Defaults to a kernel-assigned value if not specified.
* **source** (string, optional): The source IP address or CIDR for the rule (e.g., "192.168.1.0/24").
* **destination** (string, optional): The destination IP address or CIDR for the rule (e.g., "10.0.0.0/8").
* **table** (int, optional): The routing table to use for the rule. Defaults to the main table (254) if not specified.

#### Neighbor Configuration (NeighborConfig)

The NeighborConfig structure defines permanent neighbor entries (ARP for IPv4, NDP for IPv6) to be added to the Pod's network namespace.

```go
type NeighborConfig struct {
	// Destination is the target IP address.
	Destination string `json:"destination,omitempty"`
	// HardwareAddr is the MAC address of the neighbor.
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}
```

* **ipAddress** (string, required): The IP address of the neighbor (e.g., "192.168.1.1", "2001:db8::1").
* **hardwareAddr** (string, required): The MAC address of the neighbor (e.g., "00:11:22:33:44:55").

#### Ethtool Configuration (EthtoolConfig)

The EthtoolConfig structure allows for the configuration of hardware offload features and other settings managed by ethtool.

```go
// EthtoolConfig defines ethtool-based optimizations for a network interface.
// These settings correspond to features typically toggled using `ethtool -K <dev> <feature> on|off`.
type EthtoolConfig struct {
	// Features is a map of ethtool feature names to their desired state (true for on, false for off).
	// Example: {"tcp-segmentation-offload": true, "rx-checksum": true}
	Features map[string]bool `json:"features,omitempty"`

	// PrivateFlags is a map of device-specific private flag names to their desired state.
	// Example: {"my-custom-flag": true}
	PrivateFlags map[string]bool `json:"privateFlags,omitempty"`
}
```

* **features** (map[string]bool, optional): A map of ethtool feature names to their desired state (true for on, false for off). For example, {"tcp-segmentation-offload": true, "rx-checksum": true}.
* **privateFlags** (map[string]bool, optional): A map of device-specific private flag names to their desired state. For example, {"my-custom-flag": true}.

### Example: Customizing a Network Interface and Routes

Below is an example of a ResourceClaim that allocates a dummy interface, renames it to "dranet0", assigns a static IP address, configures two routes (one to a subnet via a gateway and another link-scoped route), and adds a permanent IPv4 neighbor entry. It also disables several ethtool features.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: dummy-interface-advanced
spec:
  devices:
    requests:
    - name: req-dummy-advanced
      exactly:
        deviceClassName: dra.net
        selectors:
          - cel:
              expression: device.attributes["dra.net"].ifName == "dummy3"
    config:
    - opaque:
        driver: dra.net
        parameters:
          interface:
            name: "dranet0"
            addresses:
            - "169.254.169.14/24"
            mtu: 4321
            hardwareAddr: "00:11:22:33:44:55"
          routes:
          - destination: "169.254.169.0/24"
            gateway: "169.254.169.1"
          - destination: "169.254.169.1/32"
            scope: 253
          neighbors:
          - ipAddress: "192.168.1.1"
            hardwareAddr: "00:11:22:33:44:55"
          ethtool:
            features:
              tcp-segmentation-offload: false
              generic-receive-offload: false
              large-receive-offload: false
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-advanced-cfg
  labels:
    app: pod
spec:
  containers:
  - name: ctr1
    image: registry.k8s.io/e2e-test-images/agnhost:2.54
    # Keep the container running
    command: ["sleep", "infinity"]
  resourceClaims:
  - name: dummy1
    resourceClaimName: dummy-interface-advanced
```
