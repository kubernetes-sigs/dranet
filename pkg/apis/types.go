/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apis

// NetworkConfig represents the desired state of all network interfaces and their associated routes,
// along with ethtool and sysctl configurations to be applied within the Pod's network namespace.
type NetworkConfig struct {
	// Profile references a pre-configured set of network and hardware
	// parameters resolved by the provider plugin (e.g., dynamic IPAM).
	// This separates user intent from infrastructure implementation.
	Profile string `json:"profile,omitempty"`

	// Interface defines core properties of the network interface.
	// Settings here are typically managed by `ip link` commands.
	Interface InterfaceConfig `json:"interface"`

	// SubInterface defines the properties of the subinterfaces created on the network interface.
	// When specified, new subinterfaces will be created in the pod namespace based
	// on this config, while the original interface stays in the host namespace.
	SubInterface *SubInterfaceConfig `json:"subInterface,omitempty"`

	// Routes defines static routes to be configured for this interface.
	Routes []RouteConfig `json:"routes,omitempty"`

	// Rules defines routing rules to be configured for this interface.
	// Rules are not supported when VRF (Interface.VRF) is enabled.
	Rules []RuleConfig `json:"rules,omitempty"`

	// Neighbors defines permanent neighbor (ARP/NDP) entries to be added for this interface.
	Neighbors []NeighborConfig `json:"neighbors,omitempty"`

	// Ethtool defines hardware offload features and other settings managed by `ethtool`.
	Ethtool *EthtoolConfig `json:"ethtool,omitempty"`
}

// InterfaceConfig represents the configuration for a single network interface.
// These are fundamental properties, often managed using `ip link` commands.
type InterfaceConfig struct {
	// Name is the desired logical name of the interface inside the Pod (e.g., "net0", "eth_app").
	// If not specified, DraNet may use or derive a name from the original interface.
	Name string `json:"name,omitempty"`

	// Addresses is a list of IP addresses in CIDR format (e.g., "192.168.1.10/24")
	// to be assigned to the interface.
	Addresses []string `json:"addresses,omitempty"`

	// DHCP, if true, indicates that the interface should be configured via DHCP.
	// This is mutually exclusive with the 'addresses' field.
	DHCP *bool `json:"dhcp,omitempty"`

	// MTU is the Maximum Transmission Unit for the interface.
	MTU *int32 `json:"mtu,omitempty"`

	// HardwareAddr is the MAC address of the interface.
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

	// DisableEBPFPrograms, if true, attempts to detach all eBPF programs
	// (both TC and TCX) from the network interface assigned to the Pod.
	DisableEBPFPrograms *bool `json:"disableEbpfPrograms,omitempty"`

	// Forwarding, if true, enables IP forwarding on this specific interface.
	// This sets /proc/sys/net/ipv4/conf/<iface>/forwarding and the ipv6 counterpart.
	Forwarding *bool `json:"forwarding,omitempty"`

	// VRF specifies the Virtual Routing and Forwarding domain this interface should belong to.
	// If provided, the interface will be enslaved to a VRF device with this name.
	// This enables grouping multiple network interfaces into the same VRF.
	VRF *VRFConfig `json:"vrf,omitempty"`
}

// VRFConfig represents the configuration for a Virtual Routing and Forwarding domain.
type VRFConfig struct {
	// Name is the name of the VRF device to create (e.g., "vrf0").
	// If not specified, a name will be automatically generated based on the interface index.
	Name string `json:"name,omitempty"`

	// Table is the routing table ID to use for this VRF.
	// If not specified, a unique table ID will be automatically assigned (typically interface index + 100).
	// Common reserved tables: 255 (local), 254 (main), 253 (default).
	Table *int `json:"table,omitempty"`
}

// SubInterfaceConfig defines the properties of the subinterfaces.
// The presence of a valid `Type` field activates this configuration,
// triggering the creation of the subinterface.
type SubInterfaceConfig struct {
	// Type indicates the network type of the subinterface.
	// A valid Type activates the configuration and creation of
	// the subinterface. It can either be populated by the cloud
	// provider or explicitly requested in the user's ResourceClaim.
	// For now, we only support type "ipvlan".
	Type SubInterfaceType `json:"type,omitempty"`

	// Name is the desired logical name of the subinterface inside the Pod.
	// If not specified, it will be derived by adding a type prefix to the parent interface name.
	// e.g. for ipvlan type, Name will be "ipvlan-<parent_interface_name>"
	Name string `json:"name,omitempty"`

	// Addresses is a list of IP addresses in CIDR format to be assigned to the subinterface.
	// Addresses should be populated dynamically rather than being statically defined by the user.
	// If no custom routes or rules are specified, automatic source-based routing is configured
	// for these addresses on the subinterface.
	Addresses []string `json:"addresses,omitempty"`

	// IPRange is a range of IP addresses from which the node
	// local IPAM can generate an IP address for the subinterface.
	// It needs to be provided by the cloud provider in GetDeviceConfig.
	IPRange string `json:"ipRange,omitempty"`

	// IPVlanConfig defines the IPVlan-specific configuration for the subinterface.
	// It is valid only when the subinterface type is IPVlan.
	IPVlanConfig *IPVlanConfig `json:"ipvlan,omitempty"`
}

// SubInterfaceType specifies the available subinterface network types.
// Currently the supported type is "ipvlan".
type SubInterfaceType string

const (
	SubInterfaceTypeIPVlan SubInterfaceType = "ipvlan"
)

// IPVlanConfig contains the IPVlan-specific configuration.
// Currently, the default and only option is l2 mode and bridge
// flag. New options will be added if required in the future.
type IPVlanConfig struct {
	// Mode defines how traffic is routed to the IPVlan child interfaces.
	// Currently the supported mode is:
	// - "l2": child interfaces handle their own L2 protocols like ARP.
	Mode string `json:"mode,omitempty"`
	// Flag defines the link behavior of the IPVlan child interfaces.
	// Currently the supported flag is:
	// - "bridge": child interfaces can talk directly to each other internally.
	Flag string `json:"flag,omitempty"`
}

// RouteConfig represents a network route configuration.
type RouteConfig struct {
	// Destination is the target network in CIDR format (e.g., "0.0.0.0/0", "10.0.0.0/8").
	Destination string `json:"destination,omitempty"`
	// Gateway is the IP address of the gateway for this route.
	Gateway string `json:"gateway,omitempty"`
	// Source is an optional source IP address for policy routing.
	Source string `json:"source,omitempty"`
	// Scope is the scope of the route (e.g., link, host, global).
	// Refers to Linux route scopes (e.g., 0 for RT_SCOPE_UNIVERSE, 253 for RT_SCOPE_LINK).
	Scope uint8 `json:"scope,omitempty"`
	// Table is the routing table to use for the route.
	// 0 usually means "unspecified" and defaults to the 'main' table (254) in Linux.
	//
	// IMPORTANT: If VRF is enabled on the interface, this field is IGNORED.
	// Dranet will automatically assign ALL routes for the interface to the VRF's table
	// to ensure they are reachable via the VRF device.
	//
	// Common reserved tables:
	// - 255: local (handled by kernel)
	// - 254: main (default table for most routes)
	// - 253: default
	// - 0: unspec
	Table int `json:"table,omitempty"`
}

// RuleConfig represents a network rule configuration.
type RuleConfig struct {
	// Priority is the priority of the rule.
	Priority int `json:"priority,omitempty"`
	// Source is the source IP address for the rule.
	Source string `json:"source,omitempty"`
	// Destination is the destination IP address for the rule.
	Destination string `json:"destination,omitempty"`
	// Table is the routing table ID to look up if the rule matches.
	Table int `json:"table,omitempty"`
}

// NeighborConfig represents a neighbor (ARP/NDP) entry.
type NeighborConfig struct {
	// Destination is the target IP address.
	Destination string `json:"destination,omitempty"`
	// HardwareAddr is the MAC address of the neighbor.
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}

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
