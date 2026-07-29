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

package inventory

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// IsLACPBond reports whether ifName is a bonding master negotiated in
// 802.3ad (LACP) mode. LACP bonds cannot be moved into a pod network
// namespace without breaking link aggregation with the switch, so dranet
// must create a child interface (e.g. IPVlan) on top of them instead of
// moving the bond itself (see issue #239).
//
// The bonding/mode sysfs attribute only exists for bond masters, so a missing
// file (non-bond or bonding unloaded) is reported as false rather than an
// error. The attribute value is of the form "<mode-name> <mode-number>",
// e.g. "802.3ad 4" for LACP.
func IsLACPBond(ifName string) bool {
	modePath := filepath.Join(sysnetPath, ifName, "bonding/mode")
	modeBytes, err := os.ReadFile(modePath)
	if err != nil {
		// Not a bond, or bonding module not loaded.
		return false
	}
	mode := strings.TrimSpace(string(bytes.TrimSpace(modeBytes)))
	klog.V(5).Infof("interface %s bonding mode: %q", ifName, mode)
	return strings.Contains(mode, "802.3ad")
}
