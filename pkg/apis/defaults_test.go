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
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

func TestNetworkConfigNormalize(t *testing.T) {
	t.Run("nil receiver returns nil without errors", func(t *testing.T) {
		var nilCfg *NetworkConfig
		got, errs := nilCfg.Normalize()
		if got != nil || len(errs) > 0 {
			t.Fatalf("nil.Normalize() = (%#v, %v), want (nil, nil)", got, errs)
		}
	})

	tests := []struct {
		name        string
		in          *NetworkConfig
		want        *NetworkConfig
		wantErrText string
	}{
		{
			name: "absent dhcp leaves config unchanged",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0"}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0"}},
		},
		{
			name: "dhcp true folds into Addressing DHCP and clears dhcp",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true)}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", Addressing: AddressingModeDHCP}},
		},
		{
			name: "dhcp true with matching Addressing DHCP clears dhcp",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeDHCP}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", Addressing: AddressingModeDHCP}},
		},
		{
			name:        "dhcp true conflicts with Addressing Static",
			in:          &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeStatic}},
			want:        &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeStatic}},
			wantErrText: "dhcp is deprecated and conflicts with addressing 'Static'",
		},
		{
			name:        "dhcp true conflicts with Addressing Unnumbered",
			in:          &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeUnnumbered}},
			want:        &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeUnnumbered}},
			wantErrText: "dhcp is deprecated and conflicts with addressing 'Unnumbered'",
		},
		{
			name: "dhcp false with empty Addressing folds into Static and clears dhcp",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false)}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic}},
		},
		{
			name: "dhcp false with Addressing Static clears dhcp",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: AddressingModeStatic}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic}},
		},
		{
			name: "dhcp false with Addressing Unnumbered keeps Unnumbered and clears dhcp",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: AddressingModeUnnumbered}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", Addressing: AddressingModeUnnumbered}},
		},
		{
			name:        "dhcp false conflicts with Addressing DHCP",
			in:          &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: AddressingModeDHCP}},
			want:        &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: AddressingModeDHCP}},
			wantErrText: "dhcp is deprecated and conflicts with addressing 'DHCP'",
		},
		{
			name: "dhcp false with unknown Addressing leaves dhcp untouched",
			in:   &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: "bogus"}},
			want: &NetworkConfig{Interface: InterfaceConfig{Name: "eth0", DHCP: ptr.To(false), Addressing: "bogus"}},
		},
		{
			name: "normalize does not derive fallback defaults for IPVlan or VRF",
			in: &NetworkConfig{
				Interface: InterfaceConfig{
					Type: InterfaceTypeIPVLAN,
					DHCP: ptr.To(false),
					VRF:  &VRFConfig{Name: "tenant-a"},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:       InterfaceTypeIPVLAN,
					Addressing: AddressingModeStatic,
					VRF:        &VRFConfig{Name: "tenant-a"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.in.Clone()
			got, errs := tt.in.Normalize()

			if diff := cmp.Diff(before, tt.in); diff != "" {
				t.Fatalf("Normalize() mutated its input (-want +got):\n%s", diff)
			}
			if tt.wantErrText != "" {
				if len(errs) == 0 || !strings.Contains(errs[0].Error(), tt.wantErrText) {
					t.Fatalf("Normalize() errors = %v, want substring %q", errs, tt.wantErrText)
				}
			} else if len(errs) > 0 {
				t.Fatalf("Normalize() unexpected errors: %v", errs)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Normalize() mismatch (-want +got):\n%s", diff)
			}

			// Valid normalizations must be idempotent.
			if tt.wantErrText == "" {
				again, errs2 := got.Normalize()
				if len(errs2) > 0 {
					t.Fatalf("second Normalize() unexpected errors: %v", errs2)
				}
				if diff := cmp.Diff(got, again); diff != "" {
					t.Errorf("Normalize() is not idempotent (-first +second):\n%s", diff)
				}
			}
		})
	}
}

func TestDefaultNilReceivers(t *testing.T) {
	var nc *NetworkConfig
	var ic *InterfaceConfig
	var vrf *VRFConfig
	var ipvlan *IPVlanConfig

	// None of these should panic on a nil receiver.
	nc.Default()
	ic.Default()
	vrf.Default()
	ipvlan.Default()
}

func TestTableIDForName(t *testing.T) {
	// Names that one pod can use together: interface names (Alibaba bonds),
	// VRF names and MAC addresses (GCE NICs).
	var names []string
	for i := range 64 {
		names = append(names,
			fmt.Sprintf("eth%d", i),
			fmt.Sprintf("bond%d", i),
			fmt.Sprintf("vrf%d", i),
			fmt.Sprintf("vrf-%d", i),
			fmt.Sprintf("42:01:0a:00:00:%02x", i),
		)
	}

	tables := make(map[int]string, len(names))
	for _, name := range names {
		table := TableIDForName(name)
		if table < RouteTableOffset || table >= RouteTableOffset+RouteTableRange {
			t.Errorf("TableIDForName(%q) = %d, want a table in [%d, %d)", name, table, RouteTableOffset, RouteTableOffset+RouteTableRange)
		}
		if table > math.MaxInt32 {
			t.Errorf("TableIDForName(%q) = %d, want a table that fits in an int32", name, table)
		}
		if again := TableIDForName(name); again != table {
			t.Errorf("TableIDForName(%q) = %d, then %d, want a deterministic table", name, table, again)
		}
		if other, ok := tables[table]; ok {
			t.Errorf("TableIDForName(%q) = TableIDForName(%q) = %d, want different tables", name, other, table)
		}
		tables[table] = name
	}
}
