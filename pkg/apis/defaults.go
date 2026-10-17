package apis

import (
	"hash/fnv"

	"k8s.io/utils/ptr"
)

// Default applies default values to the NetworkConfig.
func (c *NetworkConfig) Default() {
	c.Interface.Default()
	if c.Interface.Type == InterfaceTypeIPVLAN {
		if c.Interface.IPVlan == nil {
			c.Interface.IPVlan = &IPVlanConfig{}
		}
		c.Interface.IPVlan.Default()
	}
	if c.Interface.VRF != nil {
		c.Interface.VRF.Default()
	}
}

// DefaultedCopy returns a copy of the NetworkConfig with the defaults applied,
// leaving the receiver as it is. Default replaces the pointer fields of the
// interface rather than writing through them, but fills in the IPVLAN and VRF
// blocks in place, so those two are copied first.
func (c *NetworkConfig) DefaultedCopy() *NetworkConfig {
	out := *c
	if c.Interface.IPVlan != nil {
		ipvlan := *c.Interface.IPVlan
		out.Interface.IPVlan = &ipvlan
	}
	if c.Interface.VRF != nil {
		vrf := *c.Interface.VRF
		out.Interface.VRF = &vrf
	}
	out.Default()
	return &out
}

// Default applies default values to the InterfaceConfig.
func (c *InterfaceConfig) Default() {
	// Fold the deprecated DHCP field into Addressing when Addressing is unset.
	if c.Addressing == "" && c.DHCP != nil && *c.DHCP {
		c.Addressing = AddressingModeDHCP
	}
	if c.Addressing == AddressingModeSLAAC {
		c.defaultSLAAC()
	}
}

// defaultSLAAC fills in the per-interface IPv6 sysctls that autoconfiguration
// needs. The kernel resets all of them when the interface moves into the Pod, so
// SLAAC only works if they are requested explicitly.
func (c *InterfaceConfig) defaultSLAAC() {
	if c.AcceptRA == nil {
		// 2 rather than 1: the kernel ignores router advertisements on an
		// interface with forwarding enabled unless accept_ra is 2, and a Pod
		// namespace may have forwarding on.
		c.AcceptRA = ptr.To[int32](2)
	}
	if c.DisableIPv6 == nil {
		// The interface takes the Pod namespace default, and some CNI plugins
		// disable IPv6 there on IPv4-only clusters; autoconfiguration cannot run
		// without it.
		c.DisableIPv6 = ptr.To(false)
	}
	if c.AddrGenMode == nil && c.IsSubinterface() {
		// An IPVLAN child shares its parent's hardware address, so an EUI-64
		// interface identifier would give it the parent's own addresses.
		c.AddrGenMode = ptr.To[int32](3)
	}
	if c.DADTransmits == nil && c.skipsDuplicateAddressDetection() {
		// Duplicate Address Detection holds the link-local address tentative
		// for about a second, the kernel solicits a router only after that, and
		// the autoconfigured address then waits another second: about two
		// seconds, more than the runtime's sandbox deadline leaves.
		c.DADTransmits = ptr.To[int32](0)
	}
	if c.RouterSolicitationDelay == nil {
		// Solicit immediately instead of waiting out the kernel's default
		// one second spreading delay.
		c.RouterSolicitationDelay = ptr.To[int32](0)
	}
	if c.RouterSolicitationInterval == nil {
		// Repeat an unanswered solicitation after a second instead of the
		// kernel's four, so a lost solicitation or a router that delays its
		// answer (RFC 4861 section 6.2.6) does not fail the sandbox.
		c.RouterSolicitationInterval = ptr.To[int32](1)
	}
}

// skipsDuplicateAddressDetection reports whether SLAAC may skip duplicate
// address detection (RFC 4862 section 5.4) because the address cannot collide:
//
//   - with a random interface identifier (addrGenMode 3), the default on an
//     IPVLAN child, a collision takes two equal random 64-bit identifiers;
//   - a passthrough interface that keeps its hardware address derives the
//     address the host held on the same link, and the host already ran
//     detection on it.
//
// Otherwise the address is new to the link and the kernel default stays: with
// a hardwareAddr on a passthrough interface, or a stable or EUI-64 identifier
// on a subinterface. Set dadTransmits: 0 explicitly to opt out.
func (c *InterfaceConfig) skipsDuplicateAddressDetection() bool {
	if c.AddrGenMode != nil && *c.AddrGenMode == 3 {
		return true
	}
	return !c.IsSubinterface() && c.HardwareAddr == nil
}

// Default applies default values to the VRFConfig.
func (c *VRFConfig) Default() {
	if c.Table == nil && c.Name != "" {
		// Derive a deterministic table ID from the VRF name to ensure interfaces
		// joining the same VRF automatically share the same table ID.
		tableID := TableIDForName(c.Name)
		c.Table = &tableID
	}
}

// TableIDForName derives a deterministic DRANET-managed routing table ID from a
// name (e.g. a VRF name or a device identifier). VRF and policy based routing
// share this scheme so their tables come from the same reserved range.
func TableIDForName(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int((h.Sum32() % 1000) + RouteTableOffset)
}

// Default applies default values to the IPVlanConfig.
func (c *IPVlanConfig) Default() {
	if c.Mode == "" {
		c.Mode = IPVlanModeL2
	}
	if c.Flag == "" {
		c.Flag = IPVlanFlagBridge
	}
}
