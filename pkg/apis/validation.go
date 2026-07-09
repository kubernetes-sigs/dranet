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
	"net"
	"net/netip"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/json"
)

const (
	// MinMTU is the minimum practical MTU (e.g., for IPv4).
	MinMTU = 68
	// MaxInterfaceNameLen is typically IFNAMSIZ-1 (usually 15 on Linux).
	MaxInterfaceNameLen = 15
)

// ValidateConfig unmarshals and validates the NetworkConfig from a runtime.RawExtension.
// It performs strict unmarshalling and then calls specific validation functions for each part of the config.
// Returns the parsed NetworkConfig and a slice of errors if any validation fails.
func ValidateConfig(raw *runtime.RawExtension) (*NetworkConfig, []error) {
	if raw == nil || raw.Raw == nil || len(raw.Raw) == 0 {
		return nil, nil // No configuration provided, so no validation errors.
	}

	var config NetworkConfig
	var allErrors []error

	// Strict unmarshalling to catch unknown fields.
	strictErrs, err := json.UnmarshalStrict(raw.Raw, &config)
	if err != nil {
		allErrors = append(allErrors, fmt.Errorf("failed to unmarshal JSON data: %w", err))
		// If basic unmarshalling fails, we can't proceed with further validation.
		return nil, allErrors
	}
	if len(strictErrs) > 0 {
		for _, e := range strictErrs {
			allErrors = append(allErrors, fmt.Errorf("failed to unmarshal strict JSON data: %w", e))
		}
	}

	// Apply defaults
	config.Default()

	// Validate InterfaceConfig
	allErrors = append(allErrors, validateInterfaceConfig(&config.Interface, "interface")...)

	// Validate Routes
	if len(config.Routes) > 0 {
		allErrors = append(allErrors, validateRoutes(config.Routes, "routes")...)
	}

	// Validate Rules
	if len(config.Rules) > 0 {
		if config.Interface.VRF != nil {
			allErrors = append(allErrors, fmt.Errorf("rules are not supported when VRF is enabled"))
		} else {
			allErrors = append(allErrors, validateRules(config.Rules, "rules")...)
		}
	}

	// Validate EthtoolConfig if present
	if config.Ethtool != nil {
		allErrors = append(allErrors, validateEthtoolConfig(config.Ethtool, "ethtool")...)
	}

	// Validate Neighbors
	if len(config.Neighbors) > 0 {
		allErrors = append(allErrors, validateNeighborConfig(config.Neighbors, "neighbors")...)
	}

	if len(allErrors) > 0 {
		return &config, allErrors // Return partially parsed config with errors
	}

	return &config, nil
}

// isValidLinuxInterfaceName checks if the provided name is a valid Linux interface name.
// Basic checks: length, no '/', no whitespace, not '.' or '..'.
func isValidLinuxInterfaceName(name string, fieldPath string) (allErrors []error) {
	if name == "" {
		// Allow empty name, as DraNet might derive it. If a name is provided, it must be valid.
		return nil
	}
	if len(name) > MaxInterfaceNameLen {
		allErrors = append(allErrors, fmt.Errorf("%s: name '%s' exceeds maximum length of %d characters", fieldPath, name, MaxInterfaceNameLen))
	}
	if strings.Contains(name, "/") {
		allErrors = append(allErrors, fmt.Errorf("%s: name '%s' cannot contain '/'", fieldPath, name))
	}
	if strings.ContainsAny(name, " \t\n\v\f\r") { // Check for any whitespace
		allErrors = append(allErrors, fmt.Errorf("%s: name '%s' cannot contain whitespace", fieldPath, name))
	}
	if name == "." || name == ".." {
		allErrors = append(allErrors, fmt.Errorf("%s: name '%s' cannot be '.' or '..'", fieldPath, name))
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			// If '/' or whitespace, it's already covered by more specific checks above.
			// Avoid adding a generic "invalid character" error for these specific cases.
			if r == '/' || unicode.IsSpace(r) {
				continue
			}
			// This is a more restrictive set for safety, Linux itself might allow more.
			// Common practice avoids special characters other than '-', '_', '.'
			// The kernel function dev_valid_name also disallows non-printable chars.
			allErrors = append(allErrors, fmt.Errorf("%s: name '%s' contains invalid character '%c'. Only letters, digits, '-', '_', '.' are recommended", fieldPath, name, r))
		}
	}
	return allErrors
}

// validateInterfaceConfig validates the InterfaceConfig part of the NetworkConfig.
func validateInterfaceConfig(cfg *InterfaceConfig, fieldPath string) (allErrors []error) {
	if cfg == nil {
		return
	}

	allErrors = append(allErrors, isValidLinuxInterfaceName(cfg.Name, fieldPath+".name")...)

	for i, addr := range cfg.Addresses {
		if _, err := netip.ParsePrefix(addr); err != nil {
			allErrors = append(allErrors, fmt.Errorf("%s.addresses[%d]: invalid IP CIDR format '%s': %w", fieldPath, i, addr, err))
		}
	}

	if cfg.DHCP != nil && *cfg.DHCP && len(cfg.Addresses) > 0 {
		allErrors = append(allErrors, fmt.Errorf("%s: dhcp and addresses are mutually exclusive", fieldPath))
	}

	if cfg.MTU != nil {
		if *cfg.MTU < MinMTU {
			allErrors = append(allErrors, fmt.Errorf("%s.mtu: must be at least %d, got %d", fieldPath, MinMTU, *cfg.MTU))
		}
	}

	if cfg.HardwareAddr != nil {
		if _, err := net.ParseMAC(*cfg.HardwareAddr); err != nil {
			allErrors = append(allErrors, fmt.Errorf("%s.hardwareAddress: invalid Hardware Address format '%s': %w", fieldPath, *cfg.HardwareAddr, err))
		}
	}

	if cfg.GSOMaxSize != nil && *cfg.GSOMaxSize <= 0 {
		allErrors = append(allErrors, fmt.Errorf("%s.gsoMaxSize: must be positive, got %d", fieldPath, *cfg.GSOMaxSize))
	}

	if cfg.GROMaxSize != nil && *cfg.GROMaxSize <= 0 {
		allErrors = append(allErrors, fmt.Errorf("%s.groMaxSize: must be positive, got %d", fieldPath, *cfg.GROMaxSize))
	}

	if cfg.GSOIPv4MaxSize != nil && *cfg.GSOIPv4MaxSize <= 0 {
		allErrors = append(allErrors, fmt.Errorf("%s.gsov4MaxSize: must be positive, got %d", fieldPath, *cfg.GSOIPv4MaxSize))
	}

	if cfg.GROIPv4MaxSize != nil && *cfg.GROIPv4MaxSize <= 0 {
		allErrors = append(allErrors, fmt.Errorf("%s.grov4MaxSize: must be positive, got %d", fieldPath, *cfg.GROIPv4MaxSize))
	}

	if cfg.VRF != nil {
		allErrors = append(allErrors, validateVRFConfig(cfg.VRF, fieldPath+".vrf")...)
	}

	return allErrors
}

func validateVRFConfig(cfg *VRFConfig, fieldPath string) (allErrors []error) {
	if cfg.Name == "" {
		allErrors = append(allErrors, fmt.Errorf("%s.name: cannot be empty", fieldPath))
	}

	if cfg.Table != nil {
		if *cfg.Table <= 0 {
			allErrors = append(allErrors, fmt.Errorf("%s.table: must be a positive integer, got %d", fieldPath, *cfg.Table))
		}
		// Avoid reserved Linux routing tables
		if *cfg.Table == 253 || *cfg.Table == 254 || *cfg.Table == 255 {
			allErrors = append(allErrors, fmt.Errorf("%s.table: cannot use reserved table ID %d", fieldPath, *cfg.Table))
		}
	}

	return allErrors
}

// sameIPFamily reports whether a and b are both IPv4 or both IPv6.
func sameIPFamily(a, b net.IP) bool {
	return (a.To4() != nil) == (b.To4() != nil)
}

// validateRoutes validates a slice of RouteConfig.
func validateRoutes(routes []RouteConfig, fieldPath string) (allErrors []error) {
	for i, route := range routes {
		currentFieldPath := fmt.Sprintf("%s[%d]", fieldPath, i)

		// dstIP is retained so the gateway and source can be checked against the
		// destination's IP family. The kernel (via netlink RouteAdd) rejects a
		// route whose gateway or source is a different family than the destination,
		// so validation rejects the mismatch here rather than letting it fail at
		// apply time.
		var dstIP net.IP
		if route.Destination == "" {
			allErrors = append(allErrors, fmt.Errorf("%s.destination: cannot be empty", currentFieldPath))
		} else {
			if ip, _, err := net.ParseCIDR(route.Destination); err != nil {
				allErrors = append(allErrors, fmt.Errorf("%s.destination: invalid CIDR format '%s' (host routes use /32 or /128)", currentFieldPath, route.Destination))
			} else {
				dstIP = ip
			}
		}

		scopeIsLink := false
		if route.Scope != unix.RT_SCOPE_UNIVERSE && route.Scope != unix.RT_SCOPE_LINK {
			allErrors = append(allErrors, fmt.Errorf("%s.scope: invalid scope '%d', only Link (%d) or Universe (%d) allowed", currentFieldPath, route.Scope, unix.RT_SCOPE_LINK, unix.RT_SCOPE_UNIVERSE))
		}
		if route.Scope == unix.RT_SCOPE_LINK {
			scopeIsLink = true
		}

		if route.Gateway != "" {
			gwIP := net.ParseIP(route.Gateway)
			if gwIP == nil {
				allErrors = append(allErrors, fmt.Errorf("%s.gateway: invalid IP address format '%s'", currentFieldPath, route.Gateway))
			} else if dstIP != nil && !sameIPFamily(dstIP, gwIP) {
				allErrors = append(allErrors, fmt.Errorf("%s.gateway: '%s' must be the same IP family as destination '%s'", currentFieldPath, route.Gateway, route.Destination))
			}
		} else if !scopeIsLink { // Gateway is required if scope is Universe
			allErrors = append(allErrors, fmt.Errorf("%s.gateway: must be specified for Universe scope routes", currentFieldPath))
		}

		if route.Source != "" {
			srcIP := net.ParseIP(route.Source)
			if srcIP == nil {
				allErrors = append(allErrors, fmt.Errorf("%s.source: invalid IP address format '%s'", currentFieldPath, route.Source))
			} else if dstIP != nil && !sameIPFamily(dstIP, srcIP) {
				allErrors = append(allErrors, fmt.Errorf("%s.source: '%s' must be the same IP family as destination '%s'", currentFieldPath, route.Source, route.Destination))
			}
		}

		if route.Table < 0 {
			allErrors = append(allErrors, fmt.Errorf("%s.table: must be a non-negative integer, got %d", currentFieldPath, route.Table))
		}
	}
	return allErrors
}

// validateRules validates a slice of RuleConfig.
func validateRules(rules []RuleConfig, fieldPath string) (allErrors []error) {
	for i, rule := range rules {
		currentFieldPath := fmt.Sprintf("%s[%d]", fieldPath, i)

		if rule.Priority < 0 || rule.Priority > 32767 {
			allErrors = append(allErrors, fmt.Errorf("%s.priority: must be an integer between 0 and 32767, got %d", currentFieldPath, rule.Priority))
		}

		if rule.Table < 0 {
			allErrors = append(allErrors, fmt.Errorf("%s.table: must be a non-negative integer, got %d", currentFieldPath, rule.Table))
		}

		var srcIP, dstIP net.IP
		if rule.Source != "" {
			if ip, _, err := net.ParseCIDR(rule.Source); err != nil {
				allErrors = append(allErrors, fmt.Errorf("%s.source: invalid CIDR format '%s'", currentFieldPath, rule.Source))
			} else {
				srcIP = ip
			}
		}

		if rule.Destination != "" {
			if ip, _, err := net.ParseCIDR(rule.Destination); err != nil {
				allErrors = append(allErrors, fmt.Errorf("%s.destination: invalid CIDR format '%s'", currentFieldPath, rule.Destination))
			} else {
				dstIP = ip
			}
		}

		// The kernel (via netlink RuleAdd) rejects a rule whose source and
		// destination are different IP families, so reject it here rather than
		// letting it fail at apply time.
		if srcIP != nil && dstIP != nil && !sameIPFamily(srcIP, dstIP) {
			allErrors = append(allErrors, fmt.Errorf("%s: source '%s' and destination '%s' must be the same IP family", currentFieldPath, rule.Source, rule.Destination))
		}
	}
	return allErrors
}

// validateEthtoolConfig validates the EthtoolConfig part of the NetworkConfig.
func validateEthtoolConfig(cfg *EthtoolConfig, fieldPath string) (allErrors []error) {
	return allErrors
}

// ValidateRDMAOnlyConfig checks that a NetworkConfig does not contain
// network-specific fields that are meaningless (and unsupported) for an
// RDMA-only device (i.e. a device with no network interface). Callers should
// invoke this after confirming the allocated device has no ifName.
func ValidateRDMAOnlyConfig(raw *runtime.RawExtension) []error {
	if raw == nil || raw.Raw == nil || len(raw.Raw) == 0 {
		return nil
	}
	var config NetworkConfig
	var allErrors []error
	strictErrs, err := json.UnmarshalStrict(raw.Raw, &config)
	if err != nil {
		return []error{fmt.Errorf("failed to unmarshal JSON data: %w", err)}
	}
	for _, e := range strictErrs {
		allErrors = append(allErrors, fmt.Errorf("failed to unmarshal strict JSON data: %w", e))
	}
	if config.Interface.Name != "" || len(config.Interface.Addresses) > 0 ||
		config.Interface.MTU != nil || config.Interface.HardwareAddr != nil ||
		config.Interface.DHCP != nil || config.Interface.GSOMaxSize != nil ||
		config.Interface.GROMaxSize != nil || config.Interface.GSOIPv4MaxSize != nil ||
		config.Interface.GROIPv4MaxSize != nil || config.Interface.DisableEBPFPrograms != nil {
		allErrors = append(allErrors, fmt.Errorf("interface configuration is not supported for RDMA-only devices (no network interface present)"))
	}
	if len(config.Routes) > 0 {
		allErrors = append(allErrors, fmt.Errorf("routes are not supported for RDMA-only devices (no network interface present)"))
	}
	if len(config.Rules) > 0 {
		allErrors = append(allErrors, fmt.Errorf("rules are not supported for RDMA-only devices (no network interface present)"))
	}
	if config.Ethtool != nil {
		allErrors = append(allErrors, fmt.Errorf("ethtool configuration is not supported for RDMA-only devices (no network interface present)"))
	}
	if len(config.Neighbors) > 0 {
		allErrors = append(allErrors, fmt.Errorf("neighbors are not supported for RDMA-only devices (no network interface present)"))
	}
	return allErrors
}

// validateNeighborConfig validates a slice of NeighborConfig.
func validateNeighborConfig(neighbors []NeighborConfig, fieldPath string) (allErrors []error) {
	for i, neighbor := range neighbors {
		currentFieldPath := fmt.Sprintf("%s[%d]", fieldPath, i)

		if neighbor.Destination == "" {
			allErrors = append(allErrors, fmt.Errorf("%s.destination: cannot be empty", currentFieldPath))
		} else if net.ParseIP(neighbor.Destination) == nil {
			allErrors = append(allErrors, fmt.Errorf("%s.destination: invalid IP address format '%s'", currentFieldPath, neighbor.Destination))
		}

		if neighbor.HardwareAddr == "" {
			allErrors = append(allErrors, fmt.Errorf("%s.hardwareAddress: cannot be empty", currentFieldPath))
		} else if _, err := net.ParseMAC(neighbor.HardwareAddr); err != nil {
			allErrors = append(allErrors, fmt.Errorf("%s.hardwareAddress: invalid Hardware Address format '%s': %w", currentFieldPath, neighbor.HardwareAddr, err))
		}
	}
	return allErrors
}
