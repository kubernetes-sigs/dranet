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

import (
	"maps"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"
	"k8s.io/utils/ptr"
)

// MergeNetworkConfigs folds layers ordered from highest precedence to lowest
// precedence (for example: ResourceClaim, DeviceClass, Cloud DeviceConfig).
//
// Layers are folded bottom-up (right to left) so dependent-field and unit rules
// (VRF replacement, IPVlan drop on Interface.Type change) evaluate against the
// accumulated lower-precedence baseline rather than leaking fields across
// intermediate layers.
//
// Callers must normalize inputs first (via Normalize or ValidateIntake) so
// deprecated aliases such as Interface.DHCP are already folded into canonical
// fields before merging.
func MergeNetworkConfigs(layers ...*NetworkConfig) *NetworkConfig {
	switch len(layers) {
	case 0:
		return &NetworkConfig{}
	case 1:
		return MergeNetworkConfig(layers[0], nil)
	}
	merged := layers[len(layers)-1]
	for i := len(layers) - 2; i >= 0; i-- {
		merged = MergeNetworkConfig(layers[i], merged)
	}
	return merged
}

// MergeNetworkConfig combines a provider configuration with a user configuration.
// A nil input is treated as an empty configuration.
// User values, including explicit pointer zero values, override provider values.
// Slices are combined with canonical deduplication so user entries win on
// duplicates, and neither input is changed.
//
// Callers must normalize inputs first (via Normalize or ValidateIntake) so
// deprecated aliases such as Interface.DHCP are already folded into canonical
// fields before merging. When merging three or more static layers, use
// MergeNetworkConfigs so layers fold bottom-up.
//
// Unit and dependent-field rules:
//   - VRF is a unit keyed by Name: when both sides set different non-empty VRF
//     names, the user VRF replaces the provider VRF wholesale so a foreign
//     routing table is never inherited.
//   - IPVlan depends on Interface.Type == IPVLAN: if the user explicitly changes
//     a provider IPVLAN interface to another type, the provider IPVlan block is
//     dropped. An IPVlan block set without an IPVLAN type is preserved so final
//     validation rejects the misconfiguration.
//
// Every field is merged by name on purpose. The coverage tests in
// config_merge_coverage_test.go fail when a field is not merged or is aliased.
func MergeNetworkConfig(user, cloud *NetworkConfig) *NetworkConfig {
	if user == nil {
		user = &NetworkConfig{}
	}
	if cloud == nil {
		cloud = &NetworkConfig{}
	}

	mergedInterface := mergeInterfaceConfig(&user.Interface, &cloud.Interface)
	// Provider entries come first and user entries last, so the dedupe
	// functions, which keep the last occurrence, keep the user entry.
	return &NetworkConfig{
		Profile:   firstNonEmpty(user.Profile, cloud.Profile),
		Interface: mergedInterface,
		Routes:    deduplicateRoutes(append(slices.Clone(cloud.Routes), user.Routes...), mergedInterface.VRF != nil),
		Rules:     deduplicateRules(append(slices.Clone(cloud.Rules), user.Rules...)),
		Neighbors: deduplicateNeighbors(append(slices.Clone(cloud.Neighbors), user.Neighbors...)),
		Ethtool:   mergeEthtool(user.Ethtool, cloud.Ethtool),
	}
}

// Clone returns a deep copy of c that shares no pointers, slices, or maps with
// c, preserving slices verbatim without deduplicating entries.
func (c *NetworkConfig) Clone() *NetworkConfig {
	if c == nil {
		return nil
	}
	return &NetworkConfig{
		Profile:   c.Profile,
		Interface: cloneInterfaceConfig(&c.Interface),
		Routes:    slices.Clone(c.Routes),
		Rules:     slices.Clone(c.Rules),
		Neighbors: slices.Clone(c.Neighbors),
		Ethtool:   mergeEthtool(c.Ethtool, nil),
	}
}

func cloneInterfaceConfig(in *InterfaceConfig) InterfaceConfig {
	return InterfaceConfig{
		Name:                in.Name,
		Type:                in.Type,
		Addressing:          in.Addressing,
		Addresses:           slices.Clone(in.Addresses),
		DHCP:                overridePtr(in.DHCP, nil),
		MTU:                 overridePtr(in.MTU, nil),
		HardwareAddr:        overridePtr(in.HardwareAddr, nil),
		GSOMaxSize:          overridePtr(in.GSOMaxSize, nil),
		GROMaxSize:          overridePtr(in.GROMaxSize, nil),
		GSOIPv4MaxSize:      overridePtr(in.GSOIPv4MaxSize, nil),
		GROIPv4MaxSize:      overridePtr(in.GROIPv4MaxSize, nil),
		DisableEBPFPrograms: overridePtr(in.DisableEBPFPrograms, nil),
		Forwarding:          overridePtr(in.Forwarding, nil),
		ARPIgnore:           overridePtr(in.ARPIgnore, nil),
		ARPAnnounce:         overridePtr(in.ARPAnnounce, nil),
		ARPNotify:           overridePtr(in.ARPNotify, nil),
		AddrGenMode:         overridePtr(in.AddrGenMode, nil),
		AcceptRA:            overridePtr(in.AcceptRA, nil),
		VRF:                 mergeVRF(in.VRF, nil),
		IPVlan:              mergeIPVlan(in.IPVlan, nil),
	}
}

func mergeInterfaceConfig(user, cloud *InterfaceConfig) InterfaceConfig {
	cloudIPVlan := cloud.IPVlan
	if cloud.Type == InterfaceTypeIPVLAN && user.Type != "" && user.Type != InterfaceTypeIPVLAN {
		cloudIPVlan = nil
	}
	return InterfaceConfig{
		Name:                firstNonEmpty(user.Name, cloud.Name),
		Type:                firstNonEmpty(user.Type, cloud.Type),
		Addressing:          firstNonEmpty(user.Addressing, cloud.Addressing),
		Addresses:           deduplicateAddresses(append(slices.Clone(cloud.Addresses), user.Addresses...)),
		DHCP:                overridePtr(user.DHCP, cloud.DHCP),
		MTU:                 overridePtr(user.MTU, cloud.MTU),
		HardwareAddr:        overridePtr(user.HardwareAddr, cloud.HardwareAddr),
		GSOMaxSize:          overridePtr(user.GSOMaxSize, cloud.GSOMaxSize),
		GROMaxSize:          overridePtr(user.GROMaxSize, cloud.GROMaxSize),
		GSOIPv4MaxSize:      overridePtr(user.GSOIPv4MaxSize, cloud.GSOIPv4MaxSize),
		GROIPv4MaxSize:      overridePtr(user.GROIPv4MaxSize, cloud.GROIPv4MaxSize),
		DisableEBPFPrograms: overridePtr(user.DisableEBPFPrograms, cloud.DisableEBPFPrograms),
		Forwarding:          overridePtr(user.Forwarding, cloud.Forwarding),
		ARPIgnore:           overridePtr(user.ARPIgnore, cloud.ARPIgnore),
		ARPAnnounce:         overridePtr(user.ARPAnnounce, cloud.ARPAnnounce),
		ARPNotify:           overridePtr(user.ARPNotify, cloud.ARPNotify),
		AddrGenMode:         overridePtr(user.AddrGenMode, cloud.AddrGenMode),
		AcceptRA:            overridePtr(user.AcceptRA, cloud.AcceptRA),
		VRF:                 mergeVRF(user.VRF, cloud.VRF),
		IPVlan:              mergeIPVlan(user.IPVlan, cloudIPVlan),
	}
}

// mergeVRF treats VRFConfig as a unit keyed by Name: when both sides name
// different VRFs, the user VRF replaces the provider VRF wholesale so it never
// inherits a foreign routing table. When names match or only one side sets a
// name, fields merge normally.
func mergeVRF(user, cloud *VRFConfig) *VRFConfig {
	if user == nil && cloud == nil {
		return nil
	}
	if user != nil && cloud != nil && user.Name != "" && cloud.Name != "" && user.Name != cloud.Name {
		return &VRFConfig{
			Name:  user.Name,
			Table: overridePtr(user.Table, nil),
		}
	}
	if user == nil {
		user = &VRFConfig{}
	}
	if cloud == nil {
		cloud = &VRFConfig{}
	}
	return &VRFConfig{
		Name:  firstNonEmpty(user.Name, cloud.Name),
		Table: overridePtr(user.Table, cloud.Table),
	}
}

func mergeIPVlan(user, cloud *IPVlanConfig) *IPVlanConfig {
	if user == nil && cloud == nil {
		return nil
	}
	if user == nil {
		user = &IPVlanConfig{}
	}
	if cloud == nil {
		cloud = &IPVlanConfig{}
	}
	return &IPVlanConfig{
		Mode: firstNonEmpty(user.Mode, cloud.Mode),
		Flag: firstNonEmpty(user.Flag, cloud.Flag),
	}
}

func mergeEthtool(user, cloud *EthtoolConfig) *EthtoolConfig {
	if user == nil && cloud == nil {
		return nil
	}
	if user == nil {
		user = &EthtoolConfig{}
	}
	if cloud == nil {
		cloud = &EthtoolConfig{}
	}
	return &EthtoolConfig{
		Features:     mergeBoolMaps(user.Features, cloud.Features),
		PrivateFlags: mergeBoolMaps(user.PrivateFlags, cloud.PrivateFlags),
	}
}

// mergeBoolMaps returns a new map with every provider entry and every user
// entry, where a user entry replaces a provider entry with the same key.
func mergeBoolMaps(user, cloud map[string]bool) map[string]bool {
	if user == nil && cloud == nil {
		return nil
	}
	merged := make(map[string]bool, len(user)+len(cloud))
	maps.Copy(merged, cloud)
	maps.Copy(merged, user)
	return merged
}

// overridePtr returns a copy of the user value when set, even when it points
// at a zero value such as false or 0, and otherwise a copy of the cloud value.
func overridePtr[T any](user, cloud *T) *T {
	if user != nil {
		return ptr.To(*user)
	}
	if cloud != nil {
		return ptr.To(*cloud)
	}
	return nil
}

// firstNonEmpty returns the user value when it is not empty, else the cloud value.
func firstNonEmpty[T ~string](user, cloud T) T {
	if user != "" {
		return user
	}
	return cloud
}

type addressKey struct {
	valid  bool
	prefix netip.Prefix
	raw    string
}

// canonicalAddressKey matches validateInterfaceConfig and Linux kernel address
// uniqueness: IPv6 addresses collide regardless of prefix length, while IPv4
// addresses collide on the same IP and prefix length. Unparseable strings fall
// back to raw string equality so validation still reports them.
func canonicalAddressKey(addr string) addressKey {
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		return addressKey{raw: addr}
	}
	if prefix.Addr().Is6() && !prefix.Addr().Is4In6() {
		prefix = netip.PrefixFrom(prefix.Addr(), 128)
	}
	return addressKey{valid: true, prefix: prefix}
}

type cidrKey struct {
	valid  bool
	prefix netip.Prefix
	raw    string
}

// canonicalCIDRKey matches net.ParseCIDR masking used by netlink route and rule
// programming. Unparseable or empty strings fall back to raw equality.
func canonicalCIDRKey(cidr string) cidrKey {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return cidrKey{raw: cidr}
	}
	return cidrKey{valid: true, prefix: prefix.Masked()}
}

type ipKey struct {
	valid bool
	addr  netip.Addr
	raw   string
}

// canonicalIPKey matches net.ParseIP used by netlink neighbor programming.
func canonicalIPKey(ip string) ipKey {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ipKey{raw: ip}
	}
	return ipKey{valid: true, addr: addr.Unmap()}
}

// deduplicateAddresses compacts a slice of CIDR strings keeping the last
// occurrence for each canonical kernel address key.
func deduplicateAddresses(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[addressKey]bool, len(s))
	res := make([]string, 0, len(s))
	for i := len(s) - 1; i >= 0; i-- {
		key := canonicalAddressKey(s[i])
		if !seen[key] {
			seen[key] = true
			res = append(res, s[i])
		}
	}
	slices.Reverse(res)
	return res
}

func deduplicateRoutes(routes []RouteConfig, hasVRF bool) []RouteConfig {
	if len(routes) == 0 {
		return nil
	}
	// A route is identified by its canonical masked destination CIDR and
	// effective routing table. Table 0 defaults to the main table (254) in Linux.
	// When VRF is enabled on the interface, all routes are programmed into the
	// VRF table, so distinct table values collapse into the same destination key.
	type routeKey struct {
		destination cidrKey
		table       int
	}
	seen := make(map[routeKey]bool, len(routes))
	res := make([]RouteConfig, 0, len(routes))
	for i := len(routes) - 1; i >= 0; i-- {
		table := routes[i].Table
		if hasVRF {
			table = 0
		} else if table == 0 {
			table = unix.RT_TABLE_MAIN
		}
		key := routeKey{destination: canonicalCIDRKey(routes[i].Destination), table: table}
		if !seen[key] {
			seen[key] = true
			res = append(res, routes[i])
		}
	}
	slices.Reverse(res)
	return res
}

func deduplicateRules(rules []RuleConfig) []RuleConfig {
	if len(rules) == 0 {
		return nil
	}
	// Key explicitly by canonical field values rather than using RuleConfig
	// directly as a map key so adding a pointer field to RuleConfig can never
	// silently switch deduplication to pointer-identity comparison.
	type ruleKey struct {
		priority    int
		source      cidrKey
		destination cidrKey
		table       int
	}
	seen := make(map[ruleKey]bool, len(rules))
	res := make([]RuleConfig, 0, len(rules))
	for i := len(rules) - 1; i >= 0; i-- {
		table := rules[i].Table
		if table == 0 {
			table = unix.RT_TABLE_MAIN
		}
		key := ruleKey{
			priority:    rules[i].Priority,
			source:      canonicalCIDRKey(rules[i].Source),
			destination: canonicalCIDRKey(rules[i].Destination),
			table:       table,
		}
		if !seen[key] {
			seen[key] = true
			res = append(res, rules[i])
		}
	}
	slices.Reverse(res)
	return res
}

func deduplicateNeighbors(neighbors []NeighborConfig) []NeighborConfig {
	if len(neighbors) == 0 {
		return nil
	}
	seen := make(map[ipKey]bool, len(neighbors))
	res := make([]NeighborConfig, 0, len(neighbors))
	for i := len(neighbors) - 1; i >= 0; i-- {
		key := canonicalIPKey(neighbors[i].Destination)
		if !seen[key] {
			seen[key] = true
			res = append(res, neighbors[i])
		}
	}
	slices.Reverse(res)
	return res
}
