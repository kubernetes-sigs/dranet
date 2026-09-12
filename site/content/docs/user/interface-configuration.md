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

The kernel resets both ARP settings to the network namespace default when an interface
moves into a Pod, so a value configured on the host does not survive the move and has to
be requested here. Setups that attach several interfaces sharing one IP subnet, such as
multi-NIC RDMA nodes, typically need `arpIgnore: 1` and `arpAnnounce: 2`. Without them an
interface can answer ARP for another interface's address, or send requests with a source
address from the wrong subnet, which makes neighbor resolution pick the wrong link.

Linux uses the maximum of the namespace-wide `conf/all` value and the per-interface value
for both settings. A per-interface setting cannot reduce the effective value below
`conf/all`. New IPv4 network namespaces normally inherit `conf/all` and `conf/default`
from the initial network namespace, subject to `net.core.devconf_inherit_init_net`.
DRANET only changes the per-interface value.

##### IPVLAN subinterfaces

An `IPVLAN` subinterface is created inside the Pod network namespace on top of the
host device. The `mtu`, GSO and GRO sizes, `arpIgnore`, and `arpAnnounce` settings apply
to the child. They work the same way as on a passthrough interface. The child inherits
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
