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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

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
	cloud := &NetworkConfig{
		Interface: InterfaceConfig{
			DHCP:        ptr.To(true),
			MTU:         ptr.To[int32](1500),
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
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
			user := &NetworkConfig{Interface: InterfaceConfig{ARPIgnore: ptr.To(want)}}
			merged := MergeNetworkConfig(user, cloud)
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

// These lists keep the merge helpers in sync with the configuration types.
var deepCopyInterfacePointerFields = []string{
	"DHCP",
	"MTU",
	"HardwareAddr",
	"GSOMaxSize",
	"GROMaxSize",
	"GSOIPv4MaxSize",
	"GROIPv4MaxSize",
	"DisableEBPFPrograms",
	"Forwarding",
	"ARPIgnore",
	"ARPAnnounce",
	"VRF",
	"IPVlan",
}

var zeroOverrideInterfaceFields = []string{
	"DHCP",
	"MTU",
	"HardwareAddr",
	"GSOMaxSize",
	"GROMaxSize",
	"GSOIPv4MaxSize",
	"GROIPv4MaxSize",
	"DisableEBPFPrograms",
	"Forwarding",
	"ARPIgnore",
	"ARPAnnounce",
}

var nestedZeroOverrideFields = []string{
	"VRF.Table",
}

func TestMergeFieldListsCoverMutableFields(t *testing.T) {
	expectedDeepCopies := []string{
		"Interface.Addresses",
		"Interface.VRF.Table",
		"Routes",
		"Rules",
		"Neighbors",
		"Ethtool",
		"Ethtool.Features",
		"Ethtool.PrivateFlags",
	}
	for _, name := range deepCopyInterfacePointerFields {
		expectedDeepCopies = append(expectedDeepCopies, "Interface."+name)
	}

	wantDeepCopy := listAsSet(expectedDeepCopies)
	gotDeepCopy := listAsSet(mutableFieldPaths(reflect.TypeOf(NetworkConfig{}), ""))
	for path := range gotDeepCopy {
		if !wantDeepCopy[path] {
			t.Errorf("mutable field %s is not covered by deepCopyNetworkConfig", path)
		}
	}
	for path := range wantDeepCopy {
		if !gotDeepCopy[path] {
			t.Errorf("deepCopyNetworkConfig lists unknown mutable field %s", path)
		}
	}

	zeroOverride := listAsSet(zeroOverrideInterfaceFields)
	nested := listAsSet(nestedZeroOverrideFields)
	interfaceType := reflect.TypeOf(InterfaceConfig{})
	for i := range interfaceType.NumField() {
		field := interfaceType.Field(i)
		if field.Type.Kind() != reflect.Pointer {
			continue
		}
		if field.Type.Elem().Kind() != reflect.Struct {
			if !zeroOverride[field.Name] {
				t.Errorf("field %s is not covered by applyUserPointerOverrides", field.Name)
			}
			continue
		}
		nestedType := field.Type.Elem()
		for j := range nestedType.NumField() {
			nestedField := nestedType.Field(j)
			if nestedField.Type.Kind() != reflect.Pointer {
				continue
			}
			path := field.Name + "." + nestedField.Name
			if !nested[path] {
				t.Errorf("field %s is not covered by applyUserPointerOverrides", path)
			}
		}
	}

	for _, name := range zeroOverrideInterfaceFields {
		if _, ok := interfaceType.FieldByName(name); !ok {
			t.Errorf("zeroOverrideInterfaceFields lists unknown field %s", name)
		}
	}
	for _, path := range nestedZeroOverrideFields {
		if !fieldPathExists(interfaceType, path) {
			t.Errorf("nestedZeroOverrideFields lists unknown field %s", path)
		}
	}
}

func TestDeepCopyClonesEveryListedPointerField(t *testing.T) {
	paths := append(append([]string{}, deepCopyInterfacePointerFields...), nestedZeroOverrideFields...)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			user := configWithInterfacePointer(t, path)
			provider := configWithInterfacePointer(t, path)
			for _, tc := range []struct {
				name   string
				user   *NetworkConfig
				cloud  *NetworkConfig
				source *NetworkConfig
			}{
				{name: "nil provider", user: user, source: user},
				{name: "user selected", user: user, cloud: ipvlanConfig(), source: user},
				{name: "provider selected", user: &NetworkConfig{}, cloud: provider, source: provider},
			} {
				merged := MergeNetworkConfig(tc.user, tc.cloud)
				mergedPtr := mustFieldPath(t, reflect.ValueOf(merged).Elem().FieldByName("Interface"), path)
				if mergedPtr.IsNil() {
					t.Fatalf("%s: merged %s is nil", tc.name, path)
				}
				sourcePtr := mustFieldPath(t, reflect.ValueOf(tc.source).Elem().FieldByName("Interface"), path)
				if mergedPtr.Pointer() == sourcePtr.Pointer() {
					t.Errorf("%s: merged %s aliases its source", tc.name, path)
				}
			}
		})
	}
}

func configWithInterfacePointer(t *testing.T, path string) *NetworkConfig {
	t.Helper()
	config := ipvlanConfig()
	setFieldPointer(t, reflect.ValueOf(config).Elem().FieldByName("Interface"), path, false)
	return config
}

func ipvlanConfig() *NetworkConfig {
	return &NetworkConfig{Interface: InterfaceConfig{Type: InterfaceTypeIPVLAN}}
}

func TestExplicitZeroOverridesEveryListedField(t *testing.T) {
	paths := append(append([]string{}, zeroOverrideInterfaceFields...), nestedZeroOverrideFields...)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			user := &NetworkConfig{}
			setFieldPointer(t, reflect.ValueOf(user).Elem().FieldByName("Interface"), path, true)
			cloud := &NetworkConfig{}
			setFieldPointer(t, reflect.ValueOf(cloud).Elem().FieldByName("Interface"), path, false)

			merged := MergeNetworkConfig(user, cloud)
			mergedPtr := mustFieldPath(t, reflect.ValueOf(merged).Elem().FieldByName("Interface"), path)
			if mergedPtr.IsNil() {
				t.Fatalf("merged %s is nil", path)
			}
			if !mergedPtr.Elem().IsZero() {
				t.Errorf("merged %s = %v, want the explicit user zero value", path, mergedPtr.Elem().Interface())
			}
		})
	}
}

func mutableFieldPaths(structType reflect.Type, prefix string) []string {
	var paths []string
	for i := range structType.NumField() {
		field := structType.Field(i)
		path := field.Name
		if prefix != "" {
			path = prefix + "." + field.Name
		}

		switch field.Type.Kind() {
		case reflect.Pointer:
			paths = append(paths, path)
			if field.Type.Elem().Kind() == reflect.Struct {
				paths = append(paths, mutableFieldPaths(field.Type.Elem(), path)...)
			}
		case reflect.Slice:
			paths = append(paths, path)
			if field.Type.Elem().Kind() == reflect.Struct {
				paths = append(paths, mutableFieldPaths(field.Type.Elem(), path+"[]")...)
			}
		case reflect.Map:
			paths = append(paths, path)
		case reflect.Struct:
			paths = append(paths, mutableFieldPaths(field.Type, path)...)
		}
	}
	return paths
}

func listAsSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

func fieldPathExists(structType reflect.Type, path string) bool {
	for _, part := range strings.Split(path, ".") {
		if structType.Kind() == reflect.Pointer {
			structType = structType.Elem()
		}
		field, ok := structType.FieldByName(part)
		if !ok {
			return false
		}
		structType = field.Type
	}
	return true
}

// setFieldPointer sets the pointer at path to a new zero or non-zero value,
// allocating any struct pointer along the way.
func setFieldPointer(t *testing.T, structValue reflect.Value, path string, zero bool) {
	t.Helper()

	field := mustFieldPath(t, structValue, path)
	value := reflect.New(field.Type().Elem())
	if !zero {
		switch elem := value.Elem(); elem.Kind() {
		case reflect.Bool:
			elem.SetBool(true)
		case reflect.String:
			elem.SetString("non-zero")
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			elem.SetInt(7)
		case reflect.Struct:
			// A struct pointer only needs to be non-nil here.
		default:
			t.Fatalf("unsupported pointer kind %s for %s; extend setFieldPointer", elem.Kind(), path)
		}
	}
	field.Set(value)
}

// mustFieldPath walks a dotted field path, allocating struct pointers as needed.
func mustFieldPath(t *testing.T, structValue reflect.Value, path string) reflect.Value {
	t.Helper()

	current := structValue
	parts := strings.Split(path, ".")
	for i, part := range parts {
		if current.Kind() == reflect.Pointer {
			if current.IsNil() {
				if !current.CanSet() {
					t.Fatalf("cannot allocate %s while resolving %s", parts[i-1], path)
				}
				current.Set(reflect.New(current.Type().Elem()))
			}
			current = current.Elem()
		}
		current = current.FieldByName(part)
		if !current.IsValid() {
			t.Fatalf("field %s not found while resolving %s", part, path)
		}
	}
	return current
}
