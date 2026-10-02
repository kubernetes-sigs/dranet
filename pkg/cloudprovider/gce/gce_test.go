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

package gce

import (
	"net"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ipam"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func addDummyInterface(t *testing.T, ifName string, cidrs ...string) {
	t.Helper()
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to add dummy %s: %v", ifName, err)
	}
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to look up %s: %v", ifName, err)
	}
	for _, cidr := range cidrs {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("failed to parse address %s: %v", cidr, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("failed to add address %s to %s: %v", cidr, ifName, err)
		}
	}
}

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		mac      string
		instance *GCEInstance
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "instance with no interfaces",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type:       "machine-type-a",
				Interfaces: []gceNetworkInterface{},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "MAC not found in instance interfaces, no topology",
			mac:  "00:11:22:33:44:FF", // MAC that won't be found
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "MAC not found in instance interfaces, has topology",
			mac:  "00:11:22:33:44:FF", // MAC that won't be found
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
				},
				Topology: "/block/subblock/host",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEBlock:       {StringValue: ptr.To("block")},
				AttrGCESubBlock:    {StringValue: ptr.To("subblock")},
				AttrGCEHost:        {StringValue: ptr.To("host")},
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, valid network",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
					{Mac: "AA:BB:CC:DD:EE:FF", Network: "projects/67890/networks/other-network"},
				},
				Topology: "/block/subblock/host",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEBlock:                {StringValue: ptr.To("block")},
				AttrGCESubBlock:             {StringValue: ptr.To("subblock")},
				AttrGCEHost:                 {StringValue: ptr.To("host")},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, invalid network string for GCE parsing",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "invalid-gce-network-string"},
				},
			},
			want: nil, // GetDeviceAttributes returns nil for invalid network string
		},
		{
			name: "GCE provider, MAC found, valid network, invalid topology",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
					{Mac: "AA:BB:CC:DD:EE:FF", Network: "projects/67890/networks/other-network"},
				},
				Topology: "/block/subblock",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, with IP aliases",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{
						Mac:       "00:11:22:33:44:55",
						Network:   "projects/12345/networks/test-network",
						IPAliases: []string{"10.0.0.1/24", "10.0.0.2/24"},
					},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEIPAliases:            {StringValue: ptr.To("10.0.0.1/24,10.0.0.2/24")},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{MAC: tt.mac})
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	userns.Run(t, testGetProfileConfig_Namespaced, syscall.CLONE_NEWNET)
}

func testGetProfileConfig_Namespaced(t *testing.T) {
	const (
		dualStackMAC = "00:11:22:33:44:55"
		bareMAC      = "00:11:22:33:44:66"
	)

	addDummyInterface(t, "eth-dualstack", "2001:db8:1234:5678::1/64")
	addDummyInterface(t, "eth-bare")

	interfaces := []gceNetworkInterface{
		{Mac: dualStackMAC, IPAliases: []string{"10.24.3.0/24"}},
		{Mac: bareMAC},
	}
	ipvlanConfig := func() *apis.NetworkConfig {
		return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
	}

	tests := []struct {
		name      string
		id        cloudprovider.DeviceIdentifiers
		config    *apis.NetworkConfig
		wantAddrs int
		wantErr   bool
		nilIPAM   bool
	}{
		{
			name:      "dual-stack subinterface allocates one address per family",
			id:        cloudprovider.DeviceIdentifiers{MAC: dualStackMAC, Name: "eth-dualstack"},
			config:    ipvlanConfig(),
			wantAddrs: 2,
		},
		{
			name:   "empty MAC returns nil",
			id:     cloudprovider.DeviceIdentifiers{MAC: "", Name: "eth-dualstack"},
			config: ipvlanConfig(),
		},
		{
			name:   "MAC not found returns nil",
			id:     cloudprovider.DeviceIdentifiers{MAC: "aa:bb:cc:dd:ee:ff", Name: "eth-dualstack"},
			config: ipvlanConfig(),
		},
		{
			name:   "non-subinterface config returns nil",
			id:     cloudprovider.DeviceIdentifiers{MAC: dualStackMAC, Name: "eth-dualstack"},
			config: &apis.NetworkConfig{},
		},
		{
			name:    "subinterface without ranges returns error",
			id:      cloudprovider.DeviceIdentifiers{MAC: bareMAC, Name: "eth-bare"},
			config:  ipvlanConfig(),
			wantErr: true,
		},
		{
			name:    "subinterface without IPAM returns error",
			id:      cloudprovider.DeviceIdentifiers{MAC: dualStackMAC, Name: "eth-dualstack"},
			config:  ipvlanConfig(),
			nilIPAM: true,
			wantErr: true,
		},
		{
			name:      "static addresses are reserved not allocated",
			id:        cloudprovider.DeviceIdentifiers{MAC: dualStackMAC, Name: "eth-dualstack"},
			config:    &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN, Addresses: []string{"10.24.3.5/32"}}},
			wantAddrs: 0,
		},
		{
			name:    "non-existent interface name returns error",
			id:      cloudprovider.DeviceIdentifiers{MAC: dualStackMAC, Name: "missing-nic"},
			config:  ipvlanConfig(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		instance := &GCEInstance{Interfaces: interfaces}
		t.Run(tt.name, func(t *testing.T) {
			if !tt.nilIPAM {
				instance.localIPAM = ipam.NewLocalIPAM(nil)
			}

			got, err := instance.GetProfileConfig(tt.id, nil, tt.config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetProfileConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantAddrs == 0 {
				if got != nil {
					t.Fatalf("GetProfileConfig() = %#v, want nil", got)
				}
				return
			}
			if got == nil || len(got.Interface.Addresses) != tt.wantAddrs {
				t.Fatalf("GetProfileConfig() addresses = %v, want %d", got, tt.wantAddrs)
			}
		})
	}
}

// TestGetProfileConfigSourceRouting verifies that when the VM metadata carries
// the gateways, the profile returns the full policy based routing configuration
// (per-device table routes plus per-address source rules) through the existing
// Routes/Rules API, so the driver applies it with no special casing.
func TestGetProfileConfigSourceRouting(t *testing.T) {
	userns.Run(t, testGetProfileConfigSourceRouting_Namespaced, syscall.CLONE_NEWNET)
}

func testGetProfileConfigSourceRouting_Namespaced(t *testing.T) {
	addDummyInterface(t, "eth0", "2001:db8:1234:5678::1/64")

	const mac = "00:11:22:33:44:55"
	iface := gceNetworkInterface{
		Mac:         mac,
		IPAliases:   []string{"10.24.3.0/24"},
		Gateway:     "10.24.3.1",
		GatewayIPv6: "fe80::1",
	}
	instance := &GCEInstance{Interfaces: []gceNetworkInterface{iface}, localIPAM: ipam.NewLocalIPAM(nil)}
	ipvlanConfig := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}

	got, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: mac, Name: "eth0"}, nil, ipvlanConfig)
	if err != nil {
		t.Fatalf("GetProfileConfig() error = %v", err)
	}
	if got == nil || len(got.Interface.Addresses) != 2 {
		t.Fatalf("GetProfileConfig() = %#v, want 2 allocated addresses", got)
	}

	table := apis.TableIDForName(mac)
	wantRoutes := []apis.RouteConfig{
		{Destination: "10.24.3.1/32", Scope: 253, Table: table},
		{Destination: "0.0.0.0/0", Gateway: "10.24.3.1", Table: table},
		{Destination: "fe80::1/128", Scope: 253, Table: table},
		{Destination: "::/0", Gateway: "fe80::1", Table: table},
	}
	if diff := cmp.Diff(wantRoutes, got.Routes); diff != "" {
		t.Errorf("Routes mismatch (-want +got):\n%s", diff)
	}

	if len(got.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d: %+v", len(got.Rules), got.Rules)
	}
	// Allocated addresses are already host prefixes, so rule sources match them.
	for i, rule := range got.Rules {
		if rule.Source != got.Interface.Addresses[i] || rule.Table != table || rule.Priority != apis.SourceRoutingRulePriority {
			t.Errorf("rule[%d] = %+v, want source %s table %d priority %d", i, rule, got.Interface.Addresses[i], table, apis.SourceRoutingRulePriority)
		}
	}

	// When the parent interface (e.g. A5X lbvf) has a permanent link-local
	// IPv6 neighbor, its IP takes precedence over MDS gatewayIpv6.
	addDummyInterface(t, "lbvf", "fd36:0:4:2047:c00::/72")
	lbvfLink, err := netlink.LinkByName("lbvf")
	if err != nil {
		t.Fatalf("failed to look up lbvf: %v", err)
	}
	if err := netlink.LinkSetUp(lbvfLink); err != nil {
		t.Fatalf("failed to set up lbvf: %v", err)
	}
	nonLinkLocalMAC, _ := net.ParseMAC("02:32:00:00:00:99")
	if err := netlink.NeighAdd(&netlink.Neigh{
		LinkIndex:    lbvfLink.Attrs().Index,
		Family:       netlink.FAMILY_V6,
		State:        netlink.NUD_PERMANENT,
		IP:           net.ParseIP("fd36:0:4:2047:c00::2"),
		HardwareAddr: nonLinkLocalMAC,
	}); err != nil {
		t.Fatalf("failed to add global neighbor on lbvf: %v", err)
	}
	gwMAC, _ := net.ParseMAC("02:32:00:00:00:00")
	if err := netlink.NeighAdd(&netlink.Neigh{
		LinkIndex:    lbvfLink.Attrs().Index,
		Family:       netlink.FAMILY_V6,
		State:        netlink.NUD_PERMANENT,
		IP:           net.ParseIP("fe80::1"),
		HardwareAddr: gwMAC,
	}); err != nil {
		t.Fatalf("failed to add permanent link-local neighbor on lbvf: %v", err)
	}

	lbvfIface := gceNetworkInterface{
		Mac:         mac,
		GatewayIPv6: "fe80::2",
	}
	lbvfInstance := &GCEInstance{Interfaces: []gceNetworkInterface{lbvfIface}, localIPAM: ipam.NewLocalIPAM(nil)}
	lbvfGot, err := lbvfInstance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: mac, Name: "lbvf"}, nil, ipvlanConfig)
	if err != nil {
		t.Fatalf("GetProfileConfig(lbvf) error = %v", err)
	}
	wantLbvfRoutes := []apis.RouteConfig{
		{Destination: "fe80::1/128", Scope: 253, Table: table},
		{Destination: "::/0", Gateway: "fe80::1", Table: table},
	}
	if diff := cmp.Diff(wantLbvfRoutes, lbvfGot.Routes); diff != "" {
		t.Errorf("lbvf Routes mismatch (-want +got):\n%s", diff)
	}

	// A config that already carries routes or rules owns its routing:
	// the profile must only return the allocated addresses.
	userOwned := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN},
		Routes:    []apis.RouteConfig{{Destination: "0.0.0.0/0", Gateway: "10.24.3.1"}},
	}
	got, err = instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: mac, Name: "eth0"}, nil, userOwned)
	if err != nil {
		t.Fatalf("GetProfileConfig() error = %v", err)
	}
	if got == nil || len(got.Interface.Addresses) != 2 {
		t.Fatalf("GetProfileConfig() = %#v, want 2 allocated addresses", got)
	}
	if len(got.Routes) != 0 || len(got.Rules) != 0 {
		t.Errorf("expected no synthesized routes/rules for user-owned routing, got routes %+v rules %+v", got.Routes, got.Rules)
	}
}

func TestGetIPv6Range(t *testing.T) {
	tests := []struct {
		name      string
		baseIPStr string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid CIDR prefix /64",
			baseIPStr: "2001:db8:1234:5678::/64",
			want:      "2001:db8:1234:5678:c0de::/80",
			wantErr:   false,
		},
		{
			name:      "too large CIDR prefix /112",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2345::/112",
			wantErr:   true,
		},
		{
			name:      "plain IP, no CIDR prefix",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2345:6789",
			wantErr:   true,
		},
		{
			name:      "invalid IP format",
			baseIPStr: "invalid-ip",
			wantErr:   true,
		},
		{
			name:      "IPv4 CIDR input",
			baseIPStr: "192.168.1.0/24",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getIPv6Range(tt.baseIPStr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getIPv6Range() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("getIPv6Range() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubinterfaceRanges(t *testing.T) {
	userns.Run(t, testSubinterfaceRanges_Namespaced, syscall.CLONE_NEWNET)
}

func testSubinterfaceRanges_Namespaced(t *testing.T) {
	addDummyInterface(t, "eth-v6", "fd36:0:4:2047:c00::/72")
	addDummyInterface(t, "eth-v4")

	tests := []struct {
		name    string
		ifName  string
		iface   gceNetworkInterface
		want    [][2]string // {start, end} per range, in order.
		wantErr bool
	}{
		{
			name:   "IPv4 alias only",
			ifName: "eth-v4",
			iface:  gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}},
			want:   [][2]string{{"10.24.3.1", "10.24.3.254"}},
		},
		{
			name:   "IPv6 from host interface",
			ifName: "eth-v6",
			iface:  gceNetworkInterface{},
			want:   [][2]string{{"fd36:0:4:2047:cc0:de00:0:1", "fd36:0:4:2047:cc0:deff:ffff:fffe"}},
		},
		{
			name:   "host interface IPv6 overrides mismatched metadata IPv6",
			ifName: "eth-v6",
			iface:  gceNetworkInterface{IPv6: []string{"fd36:0:4:2047:1000::/72"}},
			want:   [][2]string{{"fd36:0:4:2047:cc0:de00:0:1", "fd36:0:4:2047:cc0:deff:ffff:fffe"}},
		},
		{
			name:   "dual stack orders IPv6 before IPv4",
			ifName: "eth-v6",
			iface:  gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}},
			want: [][2]string{
				{"fd36:0:4:2047:cc0:de00:0:1", "fd36:0:4:2047:cc0:deff:ffff:fffe"},
				{"10.24.3.1", "10.24.3.254"},
			},
		},
		{
			name:   "no IPv6 or aliases yields no ranges",
			ifName: "eth-v4",
			iface:  gceNetworkInterface{Mac: "00:11:22:33:44:55"},
			want:   nil,
		},
		{
			name:    "non-existent interface returns error",
			ifName:  "missing-nic",
			iface:   gceNetworkInterface{},
			wantErr: true,
		},
		{
			name:    "invalid IPv4 alias returns error",
			ifName:  "eth-v4",
			iface:   gceNetworkInterface{IPAliases: []string{"bad-cidr"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GCEInstance{}
			got, err := g.subinterfaceRanges(tt.ifName, tt.iface)
			if (err != nil) != tt.wantErr {
				t.Fatalf("subinterfaceRanges() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("subinterfaceRanges() got %d ranges, want %d (%v)", len(got), len(tt.want), got)
			}
			for i, r := range got {
				if r.Start.String() != tt.want[i][0] || r.End.String() != tt.want[i][1] {
					t.Errorf("range[%d] = [%s, %s], want [%s, %s]", i, r.Start, r.End, tt.want[i][0], tt.want[i][1])
				}
			}
		})
	}
}

func TestWithReservedAddresses(t *testing.T) {
	g := &GCEInstance{localIPAM: ipam.NewLocalIPAM(nil)}
	WithReservedAddresses([]string{"10.0.0.5/32"})(g)

	if err := g.localIPAM.Reserve([]string{"10.0.0.5/32"}); err == nil {
		t.Errorf("expected 10.0.0.5/32 to already be reserved, but it was accepted again")
	}
	if err := g.localIPAM.Reserve([]string{"10.0.0.6/32"}); err != nil {
		t.Errorf("expected 10.0.0.6/32 to be free, got error: %v", err)
	}
}
