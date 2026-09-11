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
	"slices"

	"k8s.io/utils/ptr"
)

// MergeNetworkConfig combines a provider configuration with a user configuration.
// A nil input is treated as an empty configuration.
// User values, including explicit pointer zero values, override provider values.
// Slices are combined, duplicates keep user values, and neither input is changed.
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

	// Provider entries come first and user entries last, so the dedupe
	// functions, which keep the last occurrence, keep the user entry.
	merged := &NetworkConfig{
		Profile:   firstNonEmpty(user.Profile, cloud.Profile),
		Interface: mergeInterfaceConfig(&user.Interface, &cloud.Interface),
		Routes:    deduplicateRoutes(append(slices.Clone(cloud.Routes), user.Routes...)),
		Rules:     deduplicateRules(append(slices.Clone(cloud.Rules), user.Rules...)),
		Neighbors: deduplicateNeighbors(append(slices.Clone(cloud.Neighbors), user.Neighbors...)),
		Ethtool:   mergeEthtool(user.Ethtool, cloud.Ethtool),
	}

	// Drop the meaningless IPVLAN config if the resolved type is not ipvlan.
	if merged.Interface.Type != InterfaceTypeIPVLAN {
		merged.Interface.IPVlan = nil
	}

	return merged
}

func mergeInterfaceConfig(user, cloud *InterfaceConfig) InterfaceConfig {
	return InterfaceConfig{
		Name:                firstNonEmpty(user.Name, cloud.Name),
		Type:                firstNonEmpty(user.Type, cloud.Type),
		Addressing:          firstNonEmpty(user.Addressing, cloud.Addressing),
		Addresses:           deduplicateStrings(append(slices.Clone(cloud.Addresses), user.Addresses...)),
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
		VRF:                 mergeVRF(user.VRF, cloud.VRF),
		IPVlan:              mergeIPVlan(user.IPVlan, cloud.IPVlan),
	}
}

func mergeVRF(user, cloud *VRFConfig) *VRFConfig {
	if user == nil && cloud == nil {
		return nil
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

// deduplicateStrings compacts a slice of strings keeping the last occurrence
func deduplicateStrings(s []string) []string {
	seen := make(map[string]bool)
	var res []string
	for i := len(s) - 1; i >= 0; i-- {
		if !seen[s[i]] {
			seen[s[i]] = true
			res = append([]string{s[i]}, res...)
		}
	}
	return res
}

func deduplicateRoutes(routes []RouteConfig) []RouteConfig {
	// A route is identified by its destination and table: the same destination in
	// different tables is a distinct route (policy routing) and must be kept.
	type routeKey struct {
		destination string
		table       int
	}
	seen := make(map[routeKey]bool)
	var res []RouteConfig
	for i := len(routes) - 1; i >= 0; i-- {
		key := routeKey{destination: routes[i].Destination, table: routes[i].Table}
		if !seen[key] {
			seen[key] = true
			res = append([]RouteConfig{routes[i]}, res...)
		}
	}
	return res
}

func deduplicateRules(rules []RuleConfig) []RuleConfig {
	seen := make(map[RuleConfig]bool)
	var res []RuleConfig
	for i := len(rules) - 1; i >= 0; i-- {
		if !seen[rules[i]] {
			seen[rules[i]] = true
			res = append([]RuleConfig{rules[i]}, res...)
		}
	}
	return res
}

func deduplicateNeighbors(neighbors []NeighborConfig) []NeighborConfig {
	seen := make(map[string]bool)
	var res []NeighborConfig
	for i := len(neighbors) - 1; i >= 0; i-- {
		dest := neighbors[i].Destination
		if !seen[dest] {
			seen[dest] = true
			res = append([]NeighborConfig{neighbors[i]}, res...)
		}
	}
	return res
}
