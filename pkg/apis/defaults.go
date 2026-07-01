package apis

import (
	"hash/fnv"
)

// Default applies default values to the NetworkConfig.
func (c *NetworkConfig) Default() {
	if c.Interface.VRF != nil {
		c.Interface.VRF.Default()
	}
	if c.Interface.SubInterface != nil {
		c.Interface.SubInterface.Default()
	}
}

// Default applies default values to the VRFConfig.
func (c *VRFConfig) Default() {
	if c.Table == nil && c.Name != "" {
		// Derive a deterministic table ID from the VRF name to ensure interfaces
		// joining the same VRF automatically share the same table ID.
		h := fnv.New32a()
		h.Write([]byte(c.Name))
		// Use the constant from this package
		tableID := int((h.Sum32() % 1000) + VRFTableOffset)
		c.Table = &tableID
	}
}

// Default applies default values to the SubInterfaceConfig.
func (c *SubInterfaceConfig) Default() {
	if c.Type == "" {
		c.Type = SubInterfaceTypeIPVlan
	}
	if c.Type == SubInterfaceTypeIPVlan {
		if c.IPVlanConfig == nil {
			c.IPVlanConfig = &IPVlanConfig{}
		}
		if c.IPVlanConfig.Mode == "" {
			c.IPVlanConfig.Mode = "l2"
		}
		if c.IPVlanConfig.Flag == "" {
			c.IPVlanConfig.Flag = "bridge"
		}
	}
}
