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

	"dario.cat/mergo"
	"k8s.io/utils/ptr"
)

// MergeNetworkConfig combines a provider configuration with a user configuration.
// A nil input is treated as an empty configuration.
// User values, including explicit pointer zero values, override provider values.
// Slices are combined, duplicates keep user values, and neither input is changed.
func MergeNetworkConfig(user, cloud *NetworkConfig) *NetworkConfig {
	if user == nil {
		user = &NetworkConfig{}
	}
	if cloud == nil {
		cloud = &NetworkConfig{}
	}

	merged := deepCopyNetworkConfig(cloud)
	userCopy := deepCopyNetworkConfig(user)

	// Merge the user configuration on top, overriding cloud settings and appending slices.
	if err := mergo.Merge(merged, userCopy, mergo.WithOverride, mergo.WithAppendSlice); err != nil {
		return &NetworkConfig{}
	}
	applyUserPointerOverrides(&merged.Interface, &userCopy.Interface)

	// Deduplicate slices where order or uniqueness matters.
	// For addresses, we just unique them.
	merged.Interface.Addresses = deduplicateStrings(merged.Interface.Addresses)

	// For Routes, deduplicate by destination and table (user wins, which were appended last, so we
	// iterate backwards). Routes to the same destination in different tables are distinct entries
	// (policy routing) and are all kept.
	merged.Routes = deduplicateRoutes(merged.Routes)
	merged.Rules = deduplicateRules(merged.Rules)
	merged.Neighbors = deduplicateNeighbors(merged.Neighbors)

	// Drop the meaningless IPVLAN config if the resolved type is not ipvlan.
	if merged.Interface.Type != InterfaceTypeIPVLAN {
		merged.Interface.IPVlan = nil
	}

	return merged
}

func deepCopyNetworkConfig(config *NetworkConfig) *NetworkConfig {
	if config == nil {
		return nil
	}

	// Keep this copy in sync with mutable fields in types.go.
	copy := *config
	copy.Interface.Addresses = slices.Clone(config.Interface.Addresses)
	copy.Interface.DHCP = clonePointer(config.Interface.DHCP)
	copy.Interface.MTU = clonePointer(config.Interface.MTU)
	copy.Interface.HardwareAddr = clonePointer(config.Interface.HardwareAddr)
	copy.Interface.GSOMaxSize = clonePointer(config.Interface.GSOMaxSize)
	copy.Interface.GROMaxSize = clonePointer(config.Interface.GROMaxSize)
	copy.Interface.GSOIPv4MaxSize = clonePointer(config.Interface.GSOIPv4MaxSize)
	copy.Interface.GROIPv4MaxSize = clonePointer(config.Interface.GROIPv4MaxSize)
	copy.Interface.DisableEBPFPrograms = clonePointer(config.Interface.DisableEBPFPrograms)
	copy.Interface.Forwarding = clonePointer(config.Interface.Forwarding)
	copy.Interface.ARPIgnore = clonePointer(config.Interface.ARPIgnore)
	copy.Interface.ARPAnnounce = clonePointer(config.Interface.ARPAnnounce)
	if config.Interface.VRF != nil {
		vrf := *config.Interface.VRF
		vrf.Table = clonePointer(config.Interface.VRF.Table)
		copy.Interface.VRF = &vrf
	}
	if config.Interface.IPVlan != nil {
		ipvlan := *config.Interface.IPVlan
		copy.Interface.IPVlan = &ipvlan
	}

	copy.Routes = slices.Clone(config.Routes)
	copy.Rules = slices.Clone(config.Rules)
	copy.Neighbors = slices.Clone(config.Neighbors)
	if config.Ethtool != nil {
		ethtool := *config.Ethtool
		ethtool.Features = maps.Clone(config.Ethtool.Features)
		ethtool.PrivateFlags = maps.Clone(config.Ethtool.PrivateFlags)
		copy.Ethtool = &ethtool
	}

	return &copy
}

func applyUserPointerOverrides(merged, user *InterfaceConfig) {
	overridePointer(&merged.DHCP, user.DHCP)
	overridePointer(&merged.MTU, user.MTU)
	overridePointer(&merged.HardwareAddr, user.HardwareAddr)
	overridePointer(&merged.GSOMaxSize, user.GSOMaxSize)
	overridePointer(&merged.GROMaxSize, user.GROMaxSize)
	overridePointer(&merged.GSOIPv4MaxSize, user.GSOIPv4MaxSize)
	overridePointer(&merged.GROIPv4MaxSize, user.GROIPv4MaxSize)
	overridePointer(&merged.DisableEBPFPrograms, user.DisableEBPFPrograms)
	overridePointer(&merged.Forwarding, user.Forwarding)
	overridePointer(&merged.ARPIgnore, user.ARPIgnore)
	overridePointer(&merged.ARPAnnounce, user.ARPAnnounce)
	if user.VRF != nil && user.VRF.Table != nil {
		overridePointer(&merged.VRF.Table, user.VRF.Table)
	}
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	return ptr.To(*value)
}

func overridePointer[T any](destination **T, source *T) {
	if source != nil {
		*destination = clonePointer(source)
	}
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
