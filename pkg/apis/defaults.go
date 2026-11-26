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
	"fmt"
	"hash/fnv"
)

// Normalize returns a deep copy of c with deprecated fields folded into their
// canonical replacements and cleared, without applying fallback defaults.
// It never mutates c, so it is safe to call on shared cached configurations.
// When normalization reports errors (for example, a deprecated field that
// conflicts with an explicit canonical field), it still returns the cloned
// NetworkConfig with the conflicting deprecated field left untouched.
func (c *NetworkConfig) Normalize() (*NetworkConfig, []error) {
	if c == nil {
		return nil, nil
	}
	out := c.Clone()
	errs := out.Interface.normalize("interface")
	return out, errs
}

// normalize folds deprecated fields on InterfaceConfig into their canonical
// replacements in place and clears them. When a deprecated field conflicts with
// an explicit canonical field or accompanies an unrecognized addressing mode,
// normalize leaves the deprecated field untouched.
func (c *InterfaceConfig) normalize(fieldPath string) []error {
	if c.DHCP == nil {
		return nil
	}
	if *c.DHCP {
		if c.Addressing != "" && c.Addressing != AddressingModeDHCP {
			return []error{fmt.Errorf("%s: dhcp is deprecated and conflicts with addressing '%s'", fieldPath, c.Addressing)}
		}
		c.Addressing = AddressingModeDHCP
		c.DHCP = nil
		return nil
	}
	switch c.Addressing {
	case "":
		c.Addressing = AddressingModeStatic
		c.DHCP = nil
	case AddressingModeStatic, AddressingModeUnnumbered:
		c.DHCP = nil
	case AddressingModeDHCP:
		return []error{fmt.Errorf("%s: dhcp is deprecated and conflicts with addressing '%s'", fieldPath, c.Addressing)}
	}
	return nil
}

// Default applies fallback default values to the NetworkConfig.
// Fallback defaults run once after all configuration sources have been merged,
// so a derived value never overrides an explicit value from another source.
func (c *NetworkConfig) Default() {
	if c == nil {
		return
	}
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

// Default applies fallback default values to the InterfaceConfig.
func (c *InterfaceConfig) Default() {
	if c == nil {
		return
	}
}

// Default applies fallback default values to the VRFConfig.
func (c *VRFConfig) Default() {
	if c == nil {
		return
	}
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
	return int(h.Sum32()%RouteTableRange) + RouteTableOffset
}

// Default applies fallback default values to the IPVlanConfig.
func (c *IPVlanConfig) Default() {
	if c == nil {
		return
	}
	if c.Mode == "" {
		c.Mode = IPVlanModeL2
	}
	if c.Flag == "" {
		c.Flag = IPVlanFlagBridge
	}
}
