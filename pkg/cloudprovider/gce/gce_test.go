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
	"net/netip"
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

// addDummyInterface creates a dummy link named ifName carrying the given addresses.
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

// addIPv6Route brings ifName up and adds an on-link IPv6 route to dst via gw in the given table.
func addIPv6Route(t *testing.T, ifName, dst, gw string, table int) {
	t.Helper()
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to look up %s: %v", ifName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set up %s: %v", ifName, err)
	}
	_, dstNet, err := net.ParseCIDR(dst)
	if err != nil {
		t.Fatalf("failed to parse route destination %s: %v", dst, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dstNet,
		Gw:        net.ParseIP(gw),
		Table:     table,
		Flags:     int(netlink.FLAG_ONLINK),
	}); err != nil {
		t.Fatalf("failed to add IPv6 route %s via %s on %s: %v", dst, gw, ifName, err)
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
	const mac = "00:11:22:33:44:55"
	dualStackIface := gceNetworkInterface{
		Mac:       mac,
		IPAliases: []string{"10.24.3.0/24"},
		IPv6:      []string{"2001:db8:1234:5678::/64"},
	}
	bareIface := gceNetworkInterface{Mac: mac}
	ipvlanConfig := func() *apis.NetworkConfig {
		return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
	}

	// Host NIC values deliberately differ from the metadata so each override
	// case shows which source was used.
	addDummyInterface(t, "eth-host", "fd36:0:4:2047:c00::1/72")
	addIPv6Route(t, "eth-host", "::/0", "fe80::1", 0)
	addDummyInterface(t, "eth-noroute", "fd36:0:4:2048:c00::1/72")
	metadataIPv6Iface := gceNetworkInterface{
		Mac:         mac,
		IPv6:        []string{"fd36:0:4:2047:1000::/72"},
		GatewayIPv6: "fe80::99",
	}
	irdmaIface := metadataIPv6Iface
	irdmaIface.NicType = "IRDMA"

	tests := []struct {
		name        string
		mac         string
		machineType string
		ifName      string
		iface       gceNetworkInterface
		config      *apis.NetworkConfig
		wantAddrs   int
		wantRange   string
		wantGateway string
		wantErr     bool
		nilIPAM     bool
	}{
		{
			name:      "dual-stack subinterface allocates one address per family",
			mac:       mac,
			iface:     dualStackIface,
			config:    ipvlanConfig(),
			wantAddrs: 2,
		},
		{
			name:   "empty MAC returns nil",
			mac:    "",
			iface:  dualStackIface,
			config: ipvlanConfig(),
		},
		{
			name:   "MAC not found returns nil",
			mac:    "aa:bb:cc:dd:ee:ff",
			iface:  dualStackIface,
			config: ipvlanConfig(),
		},
		{
			name:   "non-subinterface config returns nil",
			mac:    mac,
			iface:  dualStackIface,
			config: &apis.NetworkConfig{},
		},
		{
			name: "unnumbered subinterface skips IPAM and routing",
			mac:  mac,
			iface: gceNetworkInterface{
				Mac:         mac,
				IPAliases:   []string{"10.24.3.0/24"},
				IPv6:        []string{"2001:db8:1234:5678::/64"},
				GatewayIPv6: "fe80::99",
			},
			config: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Type:       apis.InterfaceTypeIPVLAN,
					Addressing: apis.AddressingModeUnnumbered,
				},
			},
			nilIPAM:   true,
			wantAddrs: 0,
		},
		{
			name:    "subinterface without cloud ranges returns error",
			mac:     mac,
			iface:   bareIface,
			config:  ipvlanConfig(),
			wantErr: true,
		},
		{
			name:    "subinterface without IPAM returns error",
			mac:     mac,
			iface:   dualStackIface,
			config:  ipvlanConfig(),
			nilIPAM: true,
			wantErr: true,
		},
		{
			name:      "static addresses are reserved not allocated",
			mac:       mac,
			iface:     dualStackIface,
			config:    &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN, Addresses: []string{"10.24.3.5/32"}}},
			wantAddrs: 0,
		},
		{
			name:    "invalid IPv6 metadata returns error",
			mac:     mac,
			iface:   gceNetworkInterface{Mac: mac, IPv6: []string{"not-an-ip"}},
			config:  ipvlanConfig(),
			wantErr: true,
		},
		{
			name:        "A5X uses host prefix and gateway",
			mac:         mac,
			machineType: "a5x-highgpu-4g-metal",
			ifName:      "eth-host",
			iface:       metadataIPv6Iface,
			config:      ipvlanConfig(),
			wantAddrs:   1,
			wantRange:   "fd36:0:4:2047:c00:c0de::/96",
			wantGateway: "fe80::1",
		},
		{
			name:        "regular machine types with IPv6 keep metadata prefix and gateway",
			mac:         mac,
			machineType: "a3-ultragpu-8g",
			ifName:      "eth-host",
			iface:       metadataIPv6Iface,
			config:      ipvlanConfig(),
			wantAddrs:   1,
			wantRange:   "fd36:0:4:2047:1000:c0de::/96",
			wantGateway: "fe80::99",
		},
		{
			name:        "A5X host NIC without default route returns error",
			mac:         mac,
			machineType: "a5x-highgpu-4g-metal",
			ifName:      "eth-noroute",
			iface:       metadataIPv6Iface,
			config:      ipvlanConfig(),
			wantErr:     true,
		},
		{
			name:        "A5X missing host NIC returns error",
			mac:         mac,
			machineType: "a5x-highgpu-4g-metal",
			ifName:      "missing-nic",
			iface:       metadataIPv6Iface,
			config:      ipvlanConfig(),
			wantErr:     true,
		},
		{
			name:        "A5X IRDMA subinterface returns error",
			mac:         mac,
			machineType: "a5x-highgpu-4g-metal",
			ifName:      "eth-host",
			iface:       irdmaIface,
			config:      ipvlanConfig(),
			wantErr:     true,
		},
		{
			name:        "A5X IRDMA passthrough returns nil",
			mac:         mac,
			machineType: "a5x-highgpu-4g-metal",
			ifName:      "eth-host",
			iface:       irdmaIface,
			config:      &apis.NetworkConfig{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &GCEInstance{Type: tt.machineType, Interfaces: []gceNetworkInterface{tt.iface}}
			if !tt.nilIPAM {
				instance.localIPAM = ipam.NewLocalIPAM(nil)
			}

			got, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: tt.mac, Name: tt.ifName}, nil, tt.config)
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
			if tt.wantRange != "" {
				addr, err := netip.ParsePrefix(got.Interface.Addresses[0])
				if err != nil {
					t.Fatalf("allocated address %q is not a prefix: %v", got.Interface.Addresses[0], err)
				}
				if !netip.MustParsePrefix(tt.wantRange).Contains(addr.Addr()) {
					t.Errorf("allocated address %s not in %s", addr, tt.wantRange)
				}
			}
			if tt.wantGateway != "" {
				table := apis.TableIDForName(tt.mac)
				wantRoutes := []apis.RouteConfig{
					{Destination: tt.wantGateway + "/128", Scope: 253, Table: table},
					{Destination: "::/0", Gateway: tt.wantGateway, Table: table},
				}
				if diff := cmp.Diff(wantRoutes, got.Routes); diff != "" {
					t.Errorf("Routes mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestGetProfileConfigSourceRouting verifies that when the VM metadata carries
// the gateways, the profile returns the full policy based routing configuration
// (per-device table routes plus per-address source rules) through the existing
// Routes/Rules API, so the driver applies it with no special casing.
func TestGetProfileConfigSourceRouting(t *testing.T) {
	const mac = "00:11:22:33:44:55"
	iface := gceNetworkInterface{
		Mac:         mac,
		IPAliases:   []string{"10.24.3.0/24"},
		IPv6:        []string{"2001:db8:1234:5678::/64"},
		Gateway:     "10.24.3.1",
		GatewayIPv6: "fe80::1",
	}
	instance := &GCEInstance{Interfaces: []gceNetworkInterface{iface}, localIPAM: ipam.NewLocalIPAM(nil)}
	ipvlanConfig := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}

	got, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: mac}, nil, ipvlanConfig)
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

	// A config that already carries routes or rules owns its routing:
	// the profile must only return the allocated addresses.
	userOwned := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN},
		Routes:    []apis.RouteConfig{{Destination: "0.0.0.0/0", Gateway: "10.24.3.1"}},
	}
	got, err = instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: mac}, nil, userOwned)
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
			name:      "unaligned prefix /72 keeps its last group intact",
			baseIPStr: "fd36:0:6:2018:c00::/72",
			want:      "fd36:0:6:2018:c00:c0de::/96",
		},
		{
			name:      "too large CIDR prefix /112",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2345::/112",
			wantErr:   true,
		},
		{
			name:      "unaligned prefix /100 rounds up past the limit",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2000::/100",
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
	tests := []struct {
		name    string
		iface   gceNetworkInterface
		want    [][2]string // {start, end} per range, in order.
		wantErr bool
	}{
		{
			name:  "IPv4 alias only",
			iface: gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}},
			want:  [][2]string{{"10.24.3.1", "10.24.3.254"}},
		},
		{
			name:  "IPv6 only",
			iface: gceNetworkInterface{IPv6: []string{"2001:db8:1234:5678::/64"}},
			want:  [][2]string{{"2001:db8:1234:5678:c0de::1", "2001:db8:1234:5678:c0de:ffff:ffff:fffe"}},
		},
		{
			name:  "dual stack orders IPv6 before IPv4",
			iface: gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}, IPv6: []string{"2001:db8:1234:5678::/64"}},
			want: [][2]string{
				{"2001:db8:1234:5678:c0de::1", "2001:db8:1234:5678:c0de:ffff:ffff:fffe"},
				{"10.24.3.1", "10.24.3.254"},
			},
		},
		{
			name:  "no IPv6 or aliases yields no ranges",
			iface: gceNetworkInterface{Mac: "00:11:22:33:44:55"},
			want:  nil,
		},
		{
			name:    "invalid IPv6 base returns error",
			iface:   gceNetworkInterface{IPv6: []string{"not-an-ip"}},
			wantErr: true,
		},
		{
			name:    "invalid IPv4 alias returns error",
			iface:   gceNetworkInterface{IPAliases: []string{"bad-cidr"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GCEInstance{}
			got, err := g.subinterfaceRanges(tt.iface)
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

func TestGetNICIPv6Prefix(t *testing.T) {
	userns.Run(t, testGetNICIPv6Prefix_Namespaced, syscall.CLONE_NEWNET)
}

func testGetNICIPv6Prefix_Namespaced(t *testing.T) {
	addDummyInterface(t, "eth-global", "fd36:0:4:2047:c00::1/72")
	addDummyInterface(t, "eth-invalid", "fe80::5/64", "2001:db8::1/112")
	addDummyInterface(t, "eth-mixed", "fe80::5/64", "2001:db8::1/112", "2001:db8:1:2::1/64")
	addDummyInterface(t, "eth-none")

	tests := []struct {
		name    string
		ifName  string
		want    []string
		wantErr bool
	}{
		{
			name:   "global prefix is returned masked in CIDR form",
			ifName: "eth-global",
			want:   []string{"fd36:0:4:2047:c00::/72"},
		},
		{
			name:   "link-local and too-long prefixes are skipped",
			ifName: "eth-mixed",
			want:   []string{"2001:db8:1:2::/64"},
		},
		{
			name:    "return error when no valid address is found",
			ifName:  "eth-invalid",
			wantErr: true,
		},
		{
			name:    "no IPv6 address returns error",
			ifName:  "eth-none",
			wantErr: true,
		},
		{
			name:    "missing interface returns error",
			ifName:  "missing-nic",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getNICIPv6Prefix(tt.ifName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getNICIPv6Prefix() error = %v, wantErr %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("getNICIPv6Prefix() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetIPv6DefaultGateway(t *testing.T) {
	userns.Run(t, testGetIPv6DefaultGateway_Namespaced, syscall.CLONE_NEWNET)
}

func testGetIPv6DefaultGateway_Namespaced(t *testing.T) {
	addDummyInterface(t, "eth-main", "2001:db8:1::1/64")
	addIPv6Route(t, "eth-main", "::/0", "fe80::1", 0)
	addDummyInterface(t, "eth-policy", "2001:db8:2::1/64")
	addIPv6Route(t, "eth-policy", "::/0", "fe80::2", 144)
	addDummyInterface(t, "eth-nondefault", "2001:db8:3::1/64")
	addIPv6Route(t, "eth-nondefault", "2001:db8:ff::/64", "fe80::3", 0)
	addDummyInterface(t, "eth-noroute", "2001:db8:4::1/64")

	tests := []struct {
		name    string
		ifName  string
		want    string
		wantErr bool
	}{
		{
			name:   "default route in main table",
			ifName: "eth-main",
			want:   "fe80::1",
		},
		{
			name:   "default route in a policy routing table",
			ifName: "eth-policy",
			want:   "fe80::2",
		},
		{
			name:    "only a non-default route returns error",
			ifName:  "eth-nondefault",
			wantErr: true,
		},
		{
			name:    "no gateway route returns error",
			ifName:  "eth-noroute",
			wantErr: true,
		},
		{
			name:    "missing interface returns error",
			ifName:  "missing-nic",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getIPv6DefaultGateway(tt.ifName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getIPv6DefaultGateway() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("getIPv6DefaultGateway() = %q, want %q", got, tt.want)
			}
		})
	}
}
