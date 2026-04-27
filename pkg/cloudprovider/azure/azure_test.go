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

package azure

import (
	"net"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		instance *AzureInstance
		id       cloudprovider.DeviceIdentifiers
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "instance with placementGroupId and vmSize",
			instance: &AzureInstance{
				PlacementGroupID: "739e6cfb-2607-462e-9e2b-21d24b31f5ed",
				VMSize:           "Standard_ND128isr_GB300_v6",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzurePlacementGroupID: {StringValue: ptr.To("739e6cfb-2607-462e-9e2b-21d24b31f5ed")},
				AttrAzureVMSize:           {StringValue: ptr.To("Standard_ND128isr_GB300_v6")},
			},
		},
		{
			name: "instance with only vmSize, no placementGroupId",
			instance: &AzureInstance{
				PlacementGroupID: "",
				VMSize:           "Standard_D4s_v3",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzureVMSize: {StringValue: ptr.To("Standard_D4s_v3")},
			},
		},
		{
			name: "instance with no metadata",
			instance: &AzureInstance{
				PlacementGroupID: "",
				VMSize:           "",
			},
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		{
			name: "attributes are node-level, same for any device identifier",
			instance: &AzureInstance{
				PlacementGroupID: "ab1690bb-a478-4039-b89a-8a3f4264d4b4",
				VMSize:           "Standard_ND128isr_GB300_v6",
			},
			id: cloudprovider.DeviceIdentifiers{
				Name:       "0001-00-00-0",
				MAC:        "00:11:22:33:44:55",
				PCIAddress: "0001:00:00.0",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzurePlacementGroupID: {StringValue: ptr.To("ab1690bb-a478-4039-b89a-8a3f4264d4b4")},
				AttrAzureVMSize:           {StringValue: ptr.To("Standard_ND128isr_GB300_v6")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(tt.id)
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestGetDeviceConfig(t *testing.T) {
	userns.Run(t, testGetDeviceConfig_Namespaced, syscall.CLONE_NEWNET)
}

func testGetDeviceConfig_Namespaced(t *testing.T) {
	// Bring up loopback.
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	// Create a dummy interface with a known MAC for the IPv6 test case.
	mac, _ := net.ParseMAC("00:0d:3a:f8:06:ec")
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1", HardwareAddr: mac}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to add dummy eth1: %v", err)
	}
	link, err := netlink.LinkByName("eth1")
	if err != nil {
		t.Fatalf("failed to look up eth1: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to bring eth1 up: %v", err)
	}

	// Add an IPv6 address so the kernel accepts the route.
	v6Addr, _ := netlink.ParseAddr("fda0:ad0a:cdcb:2000::5/64")
	if err := netlink.AddrAdd(link, v6Addr); err != nil {
		t.Fatalf("failed to add IPv6 address to eth1: %v", err)
	}

	// Install an IPv6 default route via a link-local gateway on eth1.
	_, v6Default, _ := net.ParseCIDR("::/0")
	gw := net.ParseIP("fe80::1234:5678:9abc")
	if err := netlink.RouteAdd(&netlink.Route{
		Family:    netlink.FAMILY_V6,
		Dst:       v6Default,
		Gw:        gw,
		LinkIndex: link.Attrs().Index,
		Table:     unix.RT_TABLE_MAIN,
	}); err != nil {
		t.Fatalf("failed to add IPv6 default route: %v", err)
	}

	tests := []struct {
		name     string
		instance *AzureInstance
		id       cloudprovider.DeviceIdentifiers
		want     *apis.NetworkConfig
	}{
		{
			name: "no MAC returns nil",
			instance: &AzureInstance{
				PlacementGroupID: "test-group",
				VMSize:           "Standard_D4s_v3",
			},
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: nil,
		},
		{
			name: "MAC not found in interfaces returns nil",
			instance: &AzureInstance{
				VMSize: "Standard_D4s_v3",
				Interfaces: []networkInterface{
					{
						MacAddress: "001122334455",
						IPv4: ipv4Config{
							Subnet: []subnet{{Address: "10.0.0.0", Prefix: "24"}},
						},
					},
				},
			},
			id:   cloudprovider.DeviceIdentifiers{MAC: "aa:bb:cc:dd:ee:ff"},
			want: nil,
		},
		{
			name: "IPv4 only - generates rules and routes",
			instance: &AzureInstance{
				VMSize: "Standard_D4s_v3",
				Interfaces: []networkInterface{
					{
						MacAddress: "001122334455",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.9.255.5"}},
							Subnet:    []subnet{{Address: "10.9.255.0", Prefix: "24"}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{MAC: "00:11:22:33:44:55"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "10.9.255.0/24", Table: 100},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.9.255.1", Table: 100},
				},
			},
		},
		{
			name: "IPv4 and IPv6 - generates rules and routes for both",
			instance: &AzureInstance{
				VMSize: "Standard_ND128isr_GB300_v6",
				Interfaces: []networkInterface{
					{
						MacAddress: "000D3AF806EC",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.144.133.132"}},
							Subnet:    []subnet{{Address: "10.144.133.128", Prefix: "26"}},
						},
						IPv6: ipv6Config{
							IPAddress: []ipv6Address{{PrivateIPAddress: "fda0:ad0a:cdcb:2000::5"}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{Name: "eth1", MAC: "00:0d:3a:f8:06:ec"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "10.144.133.128/26", Table: 100},
					{Source: "fda0:ad0a:cdcb:2000::5/128", Table: 100},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.144.133.129", Table: 100},
					{Destination: "::/0", Gateway: "fe80::1234:5678:9abc", Table: 100},
				},
			},
		},
		{
			name: "second NIC gets table 101",
			instance: &AzureInstance{
				VMSize: "Standard_ND128isr_GB300_v6",
				Interfaces: []networkInterface{
					{
						MacAddress: "AABBCCDDEEFF",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.0.0.5"}},
							Subnet:    []subnet{{Address: "10.0.0.0", Prefix: "24"}},
						},
					},
					{
						MacAddress: "000D3AF806EC",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.144.133.132"}},
							Subnet:    []subnet{{Address: "10.144.133.128", Prefix: "26"}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{MAC: "00:0d:3a:f8:06:ec"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "10.144.133.128/26", Table: 101},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.144.133.129", Table: 101},
				},
			},
		},
		{
			name: "IPv6 with empty address is ignored",
			instance: &AzureInstance{
				VMSize: "Standard_D4s_v3",
				Interfaces: []networkInterface{
					{
						MacAddress: "001122334455",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.0.0.5"}},
							Subnet:    []subnet{{Address: "10.0.0.0", Prefix: "24"}},
						},
						IPv6: ipv6Config{
							IPAddress: []ipv6Address{{PrivateIPAddress: ""}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{MAC: "00:11:22:33:44:55"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "10.0.0.0/24", Table: 100},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1", Table: 100},
				},
			},
		},
		{
			name: "MAC comparison is case-insensitive and separator-agnostic",
			instance: &AzureInstance{
				VMSize: "Standard_D4s_v3",
				Interfaces: []networkInterface{
					{
						MacAddress: "AABBCCDDEEFF",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "192.168.1.10"}},
							Subnet:    []subnet{{Address: "192.168.1.0", Prefix: "24"}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{MAC: "aa:bb:cc:dd:ee:ff"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "192.168.1.0/24", Table: 100},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "192.168.1.1", Table: 100},
				},
			},
		},
		{
			name: "IPv6 link-local address is ignored",
			instance: &AzureInstance{
				VMSize: "Standard_D4s_v3",
				Interfaces: []networkInterface{
					{
						MacAddress: "001122334455",
						IPv4: ipv4Config{
							IPAddress: []ipv4Address{{PrivateIPAddress: "10.0.0.5"}},
							Subnet:    []subnet{{Address: "10.0.0.0", Prefix: "24"}},
						},
						IPv6: ipv6Config{
							IPAddress: []ipv6Address{{PrivateIPAddress: "fe80::1"}},
						},
					},
				},
			},
			id: cloudprovider.DeviceIdentifiers{MAC: "00:11:22:33:44:55"},
			want: &apis.NetworkConfig{
				Rules: []apis.RuleConfig{
					{Source: "10.0.0.0/24", Table: 100},
				},
				Routes: []apis.RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1", Table: 100},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceConfig(tt.id)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetDeviceConfig() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestNormalizeMAC(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"00:11:22:33:44:55", "001122334455"},
		{"001122334455", "001122334455"},
		{"00-11-22-33-44-55", "001122334455"},
		{"AA:BB:CC:DD:EE:FF", "aabbccddeeff"},
		{"AABBCCDDEEFF", "aabbccddeeff"},
	}
	for _, tt := range tests {
		got := normalizeMAC(tt.input)
		if got != tt.want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSubnetFirstAddress(t *testing.T) {
	tests := []struct {
		addr    string
		prefix  string
		want    string
		wantErr bool
	}{
		{"10.9.255.0", "24", "10.9.255.1", false},
		{"10.144.133.128", "26", "10.144.133.129", false},
		{"192.168.0.0", "24", "192.168.0.1", false},
		{"10.0.0.0", "8", "10.0.0.1", false},
		{"10.0.2.0", "23", "10.0.2.1", false},
		{"10.0.0.32", "27", "10.0.0.33", false},
		// overflow
		{"255.255.255.255", "32", "", true},
		// subnet too small for gateway
		{"10.4.5.6", "32", "", true},
		// invalid: not a network base address
		{"10.0.0.255", "24", "", true},
		{"10.0.0.21", "27", "", true},
		// non-IPv4
		{"::1", "128", "", true},
		// invalid inputs
		{"invalid", "24", "", true},
		{"", "24", "", true},
		{"10.0.0.0", "", "", true},
		{"", "", "", true},
		{"10.0.0.0", "bad", "", true},
	}
	for _, tt := range tests {
		got, err := subnetFirstAddress(tt.addr, tt.prefix)
		if (err != nil) != tt.wantErr {
			t.Errorf("subnetFirstAddress(%q, %q) error = %v, wantErr %v", tt.addr, tt.prefix, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("subnetFirstAddress(%q, %q) = %q, want %q", tt.addr, tt.prefix, got, tt.want)
		}
	}
}
