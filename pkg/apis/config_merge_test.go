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
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

func TestMergeNetworkConfigAddrGenMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		user *int32
		want int32
	}{
		{name: "inherit provider", want: 1},
		{name: "explicit zero overrides provider", user: ptr.To[int32](0), want: 0},
		{name: "nonzero override", user: ptr.To[int32](3), want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := &NetworkConfig{Interface: InterfaceConfig{AddrGenMode: tc.user}}
			cloud := &NetworkConfig{Interface: InterfaceConfig{AddrGenMode: ptr.To[int32](1)}}
			got := MergeNetworkConfig(user, cloud)
			if got.Interface.AddrGenMode == nil || *got.Interface.AddrGenMode != tc.want {
				t.Fatalf("addrGenMode = %v, want %d", got.Interface.AddrGenMode, tc.want)
			}
			if *cloud.Interface.AddrGenMode != 1 {
				t.Fatal("merge mutated provider addrGenMode")
			}
			*got.Interface.AddrGenMode = 2
			if *cloud.Interface.AddrGenMode != 1 {
				t.Fatal("result aliases provider addrGenMode")
			}
			if tc.user != nil && *tc.user != tc.want {
				t.Fatal("result aliases user addrGenMode")
			}
		})
	}
}

func TestMergeNetworkConfig(t *testing.T) {
	tests := []struct {
		name  string
		user  *NetworkConfig
		cloud *NetworkConfig
		want  *NetworkConfig
	}{
		{
			name: "nil cloud config",
			user: &NetworkConfig{
				Interface: InterfaceConfig{Name: "eth0"},
			},
			cloud: nil,
			want: &NetworkConfig{
				Interface: InterfaceConfig{Name: "eth0"},
			},
		},
		{
			name: "scalar overrides",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-user",
					MTU:  ptr.To[int32](1400),
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-cloud",
					MTU:  ptr.To[int32](1500),
					DHCP: ptr.To(true),
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-user",
					MTU:  ptr.To[int32](1400),
					DHCP: ptr.To(true),
				},
			},
		},
		{
			name:  "user profile overrides provider profile",
			user:  &NetworkConfig{Profile: "user-profile"},
			cloud: &NetworkConfig{Profile: "provider-profile"},
			want:  &NetworkConfig{Profile: "user-profile"},
		},
		{
			name:  "empty user profile keeps provider profile",
			user:  &NetworkConfig{},
			cloud: &NetworkConfig{Profile: "provider-profile"},
			want:  &NetworkConfig{Profile: "provider-profile"},
		},
		{
			name: "merge slices without duplicates",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "1.1.1.1/32", Gateway: "10.0.0.1"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32"},
				},
				Routes: []RouteConfig{
					{Destination: "2.2.2.2/32", Gateway: "10.0.0.1"},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32", "1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "2.2.2.2/32", Gateway: "10.0.0.1"},
					{Destination: "1.1.1.1/32", Gateway: "10.0.0.1"},
				},
			},
		},
		{
			name: "conflict resolution (user wins)",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1"},
				},
				Ethtool: &EthtoolConfig{
					Features: map[string]bool{"tcp-segmentation-offload": false},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32", "2.2.2.2/32"},
				},
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.254"}, // Conflicting dest
					{Destination: "10.0.0.0/8", Gateway: "10.0.0.254"},
				},
				Ethtool: &EthtoolConfig{
					Features: map[string]bool{"tcp-segmentation-offload": true, "rx-checksum": true},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32", "1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "10.0.0.254"},
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1"},
				},
				Ethtool: &EthtoolConfig{
					Features:     map[string]bool{"tcp-segmentation-offload": false, "rx-checksum": true},
					PrivateFlags: nil,
				},
			},
		},
		{
			name: "same destination in different tables is kept across user and cloud (policy routing)",
			user: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 100},
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 200},
				},
			},
			cloud: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "192.168.1.254"},
					// Same destination as the user routes but a distinct table:
					// this cloud route must be preserved, not deduplicated away.
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.254", Table: 300},
				},
			},
			want: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "192.168.1.254"},
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.254", Table: 300},
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 100},
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 200},
				},
			},
		},
		{
			name: "same destination and table resolves to user (override preserved)",
			user: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 100},
				},
			},
			cloud: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.254", Table: 100},
				},
			},
			want: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 100},
				},
			},
		},
		{
			name: "merge rules and remove exact duplicates",
			user: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 100, Source: "192.0.2.2/32", Table: 100},
					{Priority: 200, Source: "192.0.2.3/32", Table: 200},
				},
			},
			cloud: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 50, Source: "192.0.2.1/32", Table: 50},
					{Priority: 100, Source: "192.0.2.2/32", Table: 100},
				},
			},
			want: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 50, Source: "192.0.2.1/32", Table: 50},
					{Priority: 100, Source: "192.0.2.2/32", Table: 100},
					{Priority: 200, Source: "192.0.2.3/32", Table: 200},
				},
			},
		},
		{
			name: "user requests ipvlan interface type, cloud provider configs merge in",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Name:       "eth0",
					Type:       InterfaceTypeIPVLAN,
					Addressing: AddressingModeUnnumbered,
					IPVlan:     &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addressing: AddressingModeDHCP,
					MTU:        ptr.To[int32](1500),
					IPVlan:     &IPVlanConfig{Flag: IPVlanFlagBridge},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Name:       "eth0",
					Type:       InterfaceTypeIPVLAN,
					Addressing: AddressingModeUnnumbered,
					MTU:        ptr.To[int32](1500),
					IPVlan:     &IPVlanConfig{Mode: IPVlanModeL2, Flag: IPVlanFlagBridge},
				},
			},
		},
		{
			name: "empty user type and addressing keep provider values",
			user: &NetworkConfig{},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:       InterfaceTypeIPVLAN,
					Addressing: AddressingModeUnnumbered,
					IPVlan:     &IPVlanConfig{Mode: IPVlanModeL2, Flag: IPVlanFlagBridge},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:       InterfaceTypeIPVLAN,
					Addressing: AddressingModeUnnumbered,
					IPVlan:     &IPVlanConfig{Mode: IPVlanModeL2, Flag: IPVlanFlagBridge},
				},
			},
		},
		{
			name: "user requests passthrough type, cloud provider configures ipvlan type, user wins",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Type: InterfaceTypePassthrough,
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:   InterfaceTypeIPVLAN,
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2, Flag: IPVlanFlagBridge},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Type: InterfaceTypePassthrough,
				},
			},
		},
		{
			name: "user sets ipvlan without ipvlan type preserves ipvlan for validation",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
			cloud: &NetworkConfig{},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
		},
		{
			name: "user sets passthrough and ipvlan preserves user ipvlan for validation",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:   InterfaceTypePassthrough,
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:   InterfaceTypeIPVLAN,
					IPVlan: &IPVlanConfig{Flag: IPVlanFlagBridge},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Type:   InterfaceTypePassthrough,
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
		},
		{
			name: "provider sets ipvlan without ipvlan type preserves ipvlan for validation",
			user: &NetworkConfig{},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					IPVlan: &IPVlanConfig{Mode: IPVlanModeL2},
				},
			},
		},
		{
			name: "VRF same name keeps provider table",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
			},
		},
		{
			name: "VRF different names drops provider table so foreign table is not inherited",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b"},
				},
			},
		},
		{
			name: "VRF different names keeps user explicit table",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b", Table: ptr.To(200)},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b", Table: ptr.To(200)},
				},
			},
		},
		{
			name: "VRF table-only user overrides provider table and keeps provider name",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Table: ptr.To(200)},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(200)},
				},
			},
		},
		{
			name: "VRF name-only user combines with table-only provider",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Table: ptr.To(100)},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-b", Table: ptr.To(100)},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeNetworkConfig(tt.user, tt.cloud)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("MergeNetworkConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMergeNetworkConfigExplicitZeroPointerOverrides(t *testing.T) {
	user := &NetworkConfig{
		Interface: InterfaceConfig{
			DHCP:                ptr.To(false),
			MTU:                 ptr.To[int32](0),
			HardwareAddr:        ptr.To(""),
			GSOMaxSize:          ptr.To[int32](0),
			GROMaxSize:          ptr.To[int32](0),
			GSOIPv4MaxSize:      ptr.To[int32](0),
			GROIPv4MaxSize:      ptr.To[int32](0),
			DisableEBPFPrograms: ptr.To(false),
			Forwarding:          ptr.To(false),
			ARPIgnore:           ptr.To[int32](0),
			ARPAnnounce:         ptr.To[int32](0),
			VRF:                 &VRFConfig{Table: ptr.To(0)},
		},
	}
	cloud := &NetworkConfig{
		Interface: InterfaceConfig{
			DHCP:                ptr.To(true),
			MTU:                 ptr.To[int32](1500),
			HardwareAddr:        ptr.To("02:00:00:00:00:01"),
			GSOMaxSize:          ptr.To[int32](65536),
			GROMaxSize:          ptr.To[int32](65536),
			GSOIPv4MaxSize:      ptr.To[int32](65536),
			GROIPv4MaxSize:      ptr.To[int32](65536),
			DisableEBPFPrograms: ptr.To(true),
			Forwarding:          ptr.To(true),
			ARPIgnore:           ptr.To[int32](1),
			ARPAnnounce:         ptr.To[int32](2),
			VRF:                 &VRFConfig{Name: "provider-vrf", Table: ptr.To(100)},
		},
	}
	want := &NetworkConfig{
		Interface: InterfaceConfig{
			DHCP:                ptr.To(false),
			MTU:                 ptr.To[int32](0),
			HardwareAddr:        ptr.To(""),
			GSOMaxSize:          ptr.To[int32](0),
			GROMaxSize:          ptr.To[int32](0),
			GSOIPv4MaxSize:      ptr.To[int32](0),
			GROIPv4MaxSize:      ptr.To[int32](0),
			DisableEBPFPrograms: ptr.To(false),
			Forwarding:          ptr.To(false),
			ARPIgnore:           ptr.To[int32](0),
			ARPAnnounce:         ptr.To[int32](0),
			VRF:                 &VRFConfig{Name: "provider-vrf", Table: ptr.To(0)},
		},
	}

	preserved := MergeNetworkConfig(&NetworkConfig{}, cloud)
	if diff := cmp.Diff(cloud, preserved); diff != "" {
		t.Fatalf("nil user pointers did not preserve cloud values (-want +got):\n%s", diff)
	}

	got := MergeNetworkConfig(user, cloud)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("MergeNetworkConfig() mismatch (-want +got):\n%s", diff)
	}
}

func TestMergeNetworkConfigDoesNotAliasInputs(t *testing.T) {
	user := &NetworkConfig{
		Interface: InterfaceConfig{
			Addresses: []string{"192.0.2.2/24"},
			Type:      InterfaceTypeIPVLAN,
			MTU:       ptr.To[int32](1400),
			VRF:       &VRFConfig{Table: ptr.To(0)},
			IPVlan:    &IPVlanConfig{Mode: IPVlanModeL2},
		},
		Rules: []RuleConfig{{Priority: 200, Source: "192.0.2.2/32", Table: 100}},
		Ethtool: &EthtoolConfig{
			Features: map[string]bool{"rx-checksum": false},
		},
	}
	cloud := &NetworkConfig{
		Interface: InterfaceConfig{
			Addresses:    []string{"192.0.2.1/24"},
			DHCP:         ptr.To(true),
			HardwareAddr: ptr.To("02:00:00:00:00:01"),
			VRF:          &VRFConfig{Name: "provider-vrf", Table: ptr.To(100)},
			IPVlan:       &IPVlanConfig{Flag: IPVlanFlagBridge},
		},
		Routes:    []RouteConfig{{Destination: "192.0.2.0/24", Table: 100}},
		Rules:     []RuleConfig{{Priority: 100, Source: "192.0.2.1/32", Table: 100}},
		Neighbors: []NeighborConfig{{Destination: "192.0.2.3", HardwareAddr: "02:00:00:00:00:03"}},
		Ethtool: &EthtoolConfig{
			Features:     map[string]bool{"rx-checksum": true},
			PrivateFlags: map[string]bool{"provider-flag": true},
		},
	}
	userBefore := cloneNetworkConfigForTest(t, user)
	cloudBefore := cloneNetworkConfigForTest(t, cloud)

	merged := MergeNetworkConfig(user, cloud)
	if diff := cmp.Diff(userBefore, user); diff != "" {
		t.Fatalf("user config changed during merge (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(cloudBefore, cloud); diff != "" {
		t.Fatalf("cloud config changed during merge (-want +got):\n%s", diff)
	}

	merged.Interface.Addresses[0] = "198.51.100.1/24"
	*merged.Interface.DHCP = false
	*merged.Interface.MTU = 9000
	*merged.Interface.HardwareAddr = "02:00:00:00:00:ff"
	merged.Interface.VRF.Name = "changed"
	*merged.Interface.VRF.Table = 200
	merged.Interface.IPVlan.Mode = "changed"
	merged.Interface.IPVlan.Flag = "changed"
	merged.Routes[0].Destination = "198.51.100.0/24"
	merged.Rules[0].Priority = 300
	merged.Neighbors[0].Destination = "198.51.100.3"
	merged.Ethtool.Features["rx-checksum"] = true
	merged.Ethtool.PrivateFlags["provider-flag"] = false

	if diff := cmp.Diff(userBefore, user); diff != "" {
		t.Fatalf("merged config aliases user config (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(cloudBefore, cloud); diff != "" {
		t.Fatalf("merged config aliases cloud config (-want +got):\n%s", diff)
	}
}

func TestMergeNetworkConfigNilCloudDoesNotAliasUser(t *testing.T) {
	user := &NetworkConfig{
		Interface: InterfaceConfig{
			Addresses: []string{"192.0.2.2/24"},
			Type:      InterfaceTypeIPVLAN,
			DHCP:      ptr.To(false),
			VRF:       &VRFConfig{Table: ptr.To(100)},
			IPVlan:    &IPVlanConfig{Mode: IPVlanModeL2, Flag: IPVlanFlagBridge},
		},
		Rules: []RuleConfig{{Priority: 100, Table: 100}},
		Ethtool: &EthtoolConfig{
			Features: map[string]bool{"rx-checksum": false},
		},
	}
	want := cloneNetworkConfigForTest(t, user)

	merged := MergeNetworkConfig(user, nil)
	merged.Interface.Addresses[0] = "198.51.100.2/24"
	*merged.Interface.DHCP = true
	*merged.Interface.VRF.Table = 200
	merged.Interface.IPVlan.Mode = "changed"
	merged.Rules[0].Priority = 200
	merged.Ethtool.Features["rx-checksum"] = true

	if diff := cmp.Diff(want, user); diff != "" {
		t.Fatalf("merged config aliases user config (-want +got):\n%s", diff)
	}
}

func TestMergeNetworkConfigNilInputs(t *testing.T) {
	if got := MergeNetworkConfig(nil, nil); got == nil {
		t.Fatal("MergeNetworkConfig(nil, nil) returned nil")
	}

	cloud := &NetworkConfig{Interface: InterfaceConfig{MTU: ptr.To[int32](1500)}}
	got := MergeNetworkConfig(nil, cloud)
	if diff := cmp.Diff(cloud, got); diff != "" {
		t.Fatalf("nil user did not preserve cloud values (-want +got):\n%s", diff)
	}
	if got.Interface.MTU == cloud.Interface.MTU {
		t.Fatal("nil user path aliases the cloud pointer")
	}

	user := &NetworkConfig{Interface: InterfaceConfig{Addresses: []string{"192.0.2.1/24", "192.0.2.1/24"}}}
	got = MergeNetworkConfig(user, nil)
	if diff := cmp.Diff([]string{"192.0.2.1/24"}, got.Interface.Addresses); diff != "" {
		t.Fatalf("nil cloud path did not deduplicate (-want +got):\n%s", diff)
	}
}

func TestMergeNetworkConfigConcurrent(t *testing.T) {
	addrs := make([]string, 1, 4)
	addrs[0] = "192.0.2.1/24"
	routes := make([]RouteConfig, 1, 4)
	routes[0] = RouteConfig{Destination: "10.0.0.0/8", Gateway: "192.0.2.254"}
	rules := make([]RuleConfig, 1, 4)
	rules[0] = RuleConfig{Priority: 100, Source: "192.0.2.1/32", Table: 100}
	neighbors := make([]NeighborConfig, 1, 4)
	neighbors[0] = NeighborConfig{Destination: "192.0.2.254", HardwareAddr: "02:00:00:00:00:fe"}

	cloud := &NetworkConfig{
		Interface: InterfaceConfig{
			Addresses:   addrs,
			DHCP:        ptr.To(true),
			MTU:         ptr.To[int32](1500),
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
		},
		Routes:    routes,
		Rules:     rules,
		Neighbors: neighbors,
		Ethtool: &EthtoolConfig{
			Features:     map[string]bool{"rx-checksum": true},
			PrivateFlags: map[string]bool{"cloud-flag": true},
		},
	}
	cloudBefore := cloneNetworkConfigForTest(t, cloud)

	type result struct {
		got  int32
		want int32
	}
	results := make(chan result, 64)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := int32(i % 4)
			user := &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"198.51.100.1/24"},
					ARPIgnore: ptr.To(want),
				},
				Routes:    []RouteConfig{{Destination: "198.51.100.0/24", Gateway: "198.51.100.254"}},
				Rules:     []RuleConfig{{Priority: 200, Source: "198.51.100.1/32", Table: 200}},
				Neighbors: []NeighborConfig{{Destination: "198.51.100.254", HardwareAddr: "02:00:00:00:00:01"}},
				Ethtool: &EthtoolConfig{
					Features:     map[string]bool{"rx-checksum": false},
					PrivateFlags: map[string]bool{"user-flag": false},
				},
			}
			merged := MergeNetworkConfig(user, cloud)
			// Mutate returned slices and maps to surface any aliasing under -race.
			merged.Interface.Addresses[0] = "203.0.113.1/24"
			merged.Routes[0].Gateway = "203.0.113.254"
			merged.Rules[0].Table = 300
			merged.Neighbors[0].HardwareAddr = "02:00:00:00:00:ff"
			merged.Ethtool.Features["rx-checksum"] = true
			merged.Ethtool.PrivateFlags["cloud-flag"] = false
			results <- result{got: *merged.Interface.ARPIgnore, want: want}
		}()
	}
	wg.Wait()
	close(results)

	for result := range results {
		if result.got != result.want {
			t.Errorf("arpIgnore = %d, want %d", result.got, result.want)
		}
	}
	if diff := cmp.Diff(cloudBefore, cloud); diff != "" {
		t.Fatalf("cloud config changed during concurrent merges (-want +got):\n%s", diff)
	}
}

func TestMergeNetworkConfigs(t *testing.T) {
	t.Run("zero layers returns empty config", func(t *testing.T) {
		got := MergeNetworkConfigs()
		if diff := cmp.Diff(&NetworkConfig{}, got); diff != "" {
			t.Fatalf("MergeNetworkConfigs() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("single layer deep copies and deduplicates", func(t *testing.T) {
		only := &NetworkConfig{
			Interface: InterfaceConfig{
				Addresses: []string{"192.0.2.1/24", "192.0.2.1/24"},
				MTU:       ptr.To[int32](1500),
			},
		}
		got := MergeNetworkConfigs(only)
		want := &NetworkConfig{
			Interface: InterfaceConfig{
				Addresses: []string{"192.0.2.1/24"},
				MTU:       ptr.To[int32](1500),
			},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("MergeNetworkConfigs(only) mismatch (-want +got):\n%s", diff)
		}
		*got.Interface.MTU = 9000
		if *only.Interface.MTU != 1500 {
			t.Fatal("MergeNetworkConfigs(only) aliased input pointer")
		}
	})

	tests := []struct {
		name   string
		layers []*NetworkConfig
		want   *NetworkConfig
	}{
		{
			// Bottom-up folding merges DeviceClass's table override onto Cloud's
			// "red" VRF first, then Claim's different "blue" VRF replaces that
			// unit wholesale so Cloud/DeviceClass's foreign table does not leak.
			name: "3-layer VRF: claim different name does not inherit deviceClass table for cloud VRF",
			layers: []*NetworkConfig{
				{Interface: InterfaceConfig{VRF: &VRFConfig{Name: "blue"}}},
				{Interface: InterfaceConfig{VRF: &VRFConfig{Table: ptr.To(100)}}},
				{Interface: InterfaceConfig{VRF: &VRFConfig{Name: "red"}}},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{VRF: &VRFConfig{Name: "blue"}},
			},
		},
		{
			// Bottom-up folding attaches DeviceClass's partial IPVlan block to
			// Cloud's IPVLAN type first, so Claim switching to Passthrough drops
			// the entire accumulated IPVlan block cleanly.
			name: "3-layer IPVlan: claim Passthrough drops accumulated deviceClass and cloud IPVlan",
			layers: []*NetworkConfig{
				{Interface: InterfaceConfig{Type: InterfaceTypePassthrough}},
				{Interface: InterfaceConfig{IPVlan: &IPVlanConfig{Mode: IPVlanModeL2}}},
				{Interface: InterfaceConfig{Type: InterfaceTypeIPVLAN, IPVlan: &IPVlanConfig{Flag: IPVlanFlagBridge}}},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{Type: InterfaceTypePassthrough},
			},
		},
		{
			// Bottom-up folding lets DeviceClass's Passthrough drop Cloud's IPVlan
			// before Claim switches back to IPVLAN, so Cloud's stale IPVlan is not
			// resurrected across the intermediate Passthrough reset.
			name: "3-layer Type reset: deviceClass Passthrough prevents resurrecting cloud IPVlan",
			layers: []*NetworkConfig{
				{Interface: InterfaceConfig{Type: InterfaceTypeIPVLAN}},
				{Interface: InterfaceConfig{Type: InterfaceTypePassthrough}},
				{Interface: InterfaceConfig{Type: InterfaceTypeIPVLAN, IPVlan: &IPVlanConfig{Mode: IPVlanModeL2}}},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{Type: InterfaceTypeIPVLAN},
			},
		},
		{
			name: "3-layer precedence: claim > deviceClass > cloud",
			layers: []*NetworkConfig{
				{
					Interface: InterfaceConfig{
						MTU:       ptr.To[int32](1400),
						ARPIgnore: ptr.To[int32](0),
					},
				},
				{
					Interface: InterfaceConfig{
						MTU:         ptr.To[int32](1450),
						ARPIgnore:   ptr.To[int32](1),
						ARPAnnounce: ptr.To[int32](2),
						VRF:         &VRFConfig{Table: ptr.To(200)},
					},
				},
				{
					Interface: InterfaceConfig{
						MTU:         ptr.To[int32](1500),
						ARPAnnounce: ptr.To[int32](1),
						AcceptRA:    ptr.To[int32](0),
						VRF:         &VRFConfig{Name: "tenant-a", Table: ptr.To(100)},
					},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					MTU:         ptr.To[int32](1400),
					ARPIgnore:   ptr.To[int32](0),
					ARPAnnounce: ptr.To[int32](2),
					AcceptRA:    ptr.To[int32](0),
					VRF:         &VRFConfig{Name: "tenant-a", Table: ptr.To(200)},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeNetworkConfigs(tt.layers...)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("MergeNetworkConfigs() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMergeNetworkConfigCanonicalDeduplication(t *testing.T) {
	tests := []struct {
		name  string
		user  *NetworkConfig
		cloud *NetworkConfig
		want  *NetworkConfig
	}{
		{
			name: "addresses: non-canonical IPv6 spelling and prefix length override keep user entry",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2001:db8::1/128", "2001:db8::2/64", "10.0.0.1/16"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2001:0db8::1/64", "2001:DB8:0::2/64", "10.0.0.1/24", "10.0.0.2/24"},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"10.0.0.1/24", "10.0.0.2/24", "2001:db8::1/128", "2001:db8::2/64", "10.0.0.1/16"},
				},
			},
		},
		{
			name: "routes: table 0 and 254 collide and unmasked/IPv6 CIDRs canonicalize",
			user: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/24", Gateway: "192.168.1.1", Table: 254},
					{Destination: "10.1.0.0/24", Gateway: "192.168.1.1", Table: 0},
					{Destination: "2001:db8::/64", Gateway: "fe80::1"},
				},
			},
			cloud: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.5/24", Gateway: "192.168.1.254", Table: 0},
					{Destination: "10.1.0.0/24", Gateway: "192.168.1.254", Table: 254},
					{Destination: "2001:0db8::1/64", Gateway: "fe80::fe"},
				},
			},
			want: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/24", Gateway: "192.168.1.1", Table: 254},
					{Destination: "10.1.0.0/24", Gateway: "192.168.1.1", Table: 0},
					{Destination: "2001:db8::/64", Gateway: "fe80::1"},
				},
			},
		},
		{
			name: "routes: VRF collapses route tables so user overrides cloud route",
			user: &NetworkConfig{
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 200},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.254", Table: 100},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					VRF: &VRFConfig{Name: "vrf-a", Table: ptr.To(100)},
				},
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "192.168.1.1", Table: 200},
				},
			},
		},
		{
			name: "rules: canonical CIDRs and table 0 vs 254 deduplicate keeping user",
			user: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 100, Source: "2001:db8::/64", Destination: "10.0.0.0/24", Table: 254},
				},
			},
			cloud: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 100, Source: "2001:0db8::1/64", Destination: "10.0.0.5/24", Table: 0},
					{Priority: 200, Source: "2001:db8::/64", Table: 100},
				},
			},
			want: &NetworkConfig{
				Rules: []RuleConfig{
					{Priority: 200, Source: "2001:db8::/64", Table: 100},
					{Priority: 100, Source: "2001:db8::/64", Destination: "10.0.0.0/24", Table: 254},
				},
			},
		},
		{
			name: "neighbors: canonical IPv6 and IPv4-mapped IPv6 deduplicate keeping user",
			user: &NetworkConfig{
				Neighbors: []NeighborConfig{
					{Destination: "2001:db8::1", HardwareAddr: "02:00:00:00:00:01"},
					{Destination: "192.0.2.1", HardwareAddr: "02:00:00:00:00:02"},
				},
			},
			cloud: &NetworkConfig{
				Neighbors: []NeighborConfig{
					{Destination: "2001:0DB8::1", HardwareAddr: "02:00:00:00:00:fe"},
					{Destination: "::ffff:192.0.2.1", HardwareAddr: "02:00:00:00:00:fd"},
				},
			},
			want: &NetworkConfig{
				Neighbors: []NeighborConfig{
					{Destination: "2001:db8::1", HardwareAddr: "02:00:00:00:00:01"},
					{Destination: "192.0.2.1", HardwareAddr: "02:00:00:00:00:02"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeNetworkConfig(tt.user, tt.cloud)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("MergeNetworkConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func cloneNetworkConfigForTest(t *testing.T, config *NetworkConfig) *NetworkConfig {
	t.Helper()

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal NetworkConfig: %v", err)
	}
	copy := &NetworkConfig{}
	if err := json.Unmarshal(data, copy); err != nil {
		t.Fatalf("unmarshal NetworkConfig: %v", err)
	}
	return copy
}
