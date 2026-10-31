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

package alibaba

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ipam"
)

const testPCIAddress = "0000:00:0b.0"

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name         string
		instance     AlibabaInstance
		id           cloudprovider.DeviceIdentifiers
		wantInstType string
		wantERDMA    bool
	}{
		{
			name: "GPU instance with eRDMA, matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.gn8is-2x.8xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.gn8is-2x.8xlarge",
			wantERDMA:    true,
		},
		{
			name: "GPU instance with eRDMA, non-matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.gn8is-2x.8xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: "0000:00:0c.0"},
			wantInstType: "ecs.gn8is-2x.8xlarge",
			wantERDMA:    false,
		},
		{
			name: "regular ECS instance without eRDMA",
			instance: AlibabaInstance{
				InstanceType:      "ecs.g6.xlarge",
				ERDMAPCIAddresses: sets.New[string](),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.g6.xlarge",
			wantERDMA:    false,
		},
		{
			name: "bare metal with eRDMA, matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.ebmgn8is.32xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.ebmgn8is.32xlarge",
			wantERDMA:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := tt.instance.GetDeviceAttributes(tt.id)
			if tt.wantInstType != "" {
				instAttr, ok := attrs[AttrInstanceType]
				if !ok {
					t.Fatal("missing instanceType attribute")
				}
				if instAttr.StringValue == nil || *instAttr.StringValue != tt.wantInstType {
					t.Errorf("instanceType = %v, want %s", instAttr.StringValue, tt.wantInstType)
				}
			}
			erdmaAttr, ok := attrs[AttrERDMA]
			if tt.wantERDMA {
				if !ok {
					t.Fatal("missing erdma attribute")
				}
				if erdmaAttr.BoolValue == nil || !*erdmaAttr.BoolValue {
					t.Error("expected erdma=true")
				}
			} else {
				if ok {
					t.Errorf("unexpected erdma attribute: %v", erdmaAttr)
				}
			}
		})
	}
}

func TestGetDeviceConfig(t *testing.T) {
	userns.Run(t, testGetDeviceConfig_Namespaced, syscall.CLONE_NEWNET)
}

func testGetDeviceConfig_Namespaced(t *testing.T) {
	addBond := func(name string, mode netlink.BondMode) {
		t.Helper()
		bond := netlink.NewLinkBond(netlink.LinkAttrs{Name: name})
		bond.Mode = mode
		if err := netlink.LinkAdd(bond); err != nil {
			t.Fatalf("failed to add bond %s: %v", name, err)
		}
	}
	addAddr := func(ifName, cidr string) {
		t.Helper()
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			t.Fatalf("failed to look up %s: %v", ifName, err)
		}
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("failed to parse address %s: %v", cidr, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("failed to add address %s to %s: %v", cidr, ifName, err)
		}
	}

	// An eFLO RDMA bond is 802.3ad and carries the IPv6 address the
	// subinterface block is derived from.
	addBond("bond0", netlink.BOND_MODE_802_3AD)
	addAddr("bond0", "2001:db8:1234:5678::1/64")
	// An 802.3ad bond without addresses is not an eFLO device.
	addBond("bond1", netlink.BOND_MODE_802_3AD)
	// An IPv4-only 802.3ad bond keeps the default handling.
	addBond("bond2", netlink.BOND_MODE_802_3AD)
	addAddr("bond2", "10.0.0.1/24")
	// A non-LACP bond with IPv6 is not an eFLO device either.
	addBond("bond3", netlink.BOND_MODE_ACTIVE_BACKUP)
	addAddr("bond3", "2001:db8:1234:5678::2/64")
	// A regular NIC.
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to add dummy eth0: %v", err)
	}

	tests := []struct {
		name        string
		id          cloudprovider.DeviceIdentifiers
		wantNil     bool
		wantProfile string
		wantType    apis.InterfaceType
	}{
		{
			name:    "no interface name -> nil config",
			id:      cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantNil: true,
		},
		{
			name:    "regular NIC, not a bond -> nil config",
			id:      cloudprovider.DeviceIdentifiers{Name: "eth0"},
			wantNil: true,
		},
		{
			name:    "unconfigured LACP bond is not an eflo device -> nil config",
			id:      cloudprovider.DeviceIdentifiers{Name: "bond1", PCIAddress: testPCIAddress},
			wantNil: true,
		},
		{
			name:    "IPv4-only LACP bond is not an eflo device -> nil config",
			id:      cloudprovider.DeviceIdentifiers{Name: "bond2", PCIAddress: testPCIAddress},
			wantNil: true,
		},
		{
			name:    "non-LACP bond with IPv6 is not an eflo device -> nil config",
			id:      cloudprovider.DeviceIdentifiers{Name: "bond3", PCIAddress: testPCIAddress},
			wantNil: true,
		},
		{
			name:        "LACP bond with an IPv6 address -> IPVlan subinterface with eflo profile",
			id:          cloudprovider.DeviceIdentifiers{Name: "bond0", PCIAddress: testPCIAddress},
			wantNil:     false,
			wantProfile: alibabaEfloSubinterfaceProfile,
			wantType:    apis.InterfaceTypeIPVLAN,
		},
	}
	instance := &AlibabaInstance{
		InstanceType:      "ecs.gn8is-2x.8xlarge",
		ERDMAPCIAddresses: sets.New[string](testPCIAddress),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := instance.GetDeviceConfig(tt.id)
			if tt.wantNil {
				if config != nil {
					t.Errorf("expected nil config, got %v", config)
				}
				return
			}
			if config == nil {
				t.Fatalf("expected non-nil config, got nil")
			}
			if config.Profile != tt.wantProfile {
				t.Errorf("Profile = %q, want %q", config.Profile, tt.wantProfile)
			}
			if config.Interface.Type != tt.wantType {
				t.Errorf("Interface.Type = %q, want %q", config.Interface.Type, tt.wantType)
			}
			if len(config.Interface.Addresses) != 0 {
				t.Errorf("expected no static Addresses in GetDeviceConfig, got %v", config.Interface.Addresses)
			}
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	userns.Run(t, testGetProfileConfig_Namespaced, syscall.CLONE_NEWNET)
}

func testGetProfileConfig_Namespaced(t *testing.T) {
	// An eFLO RDMA parent: an 802.3ad bond carrying the global IPv6 address
	// the subinterface block is derived from, plus IPv6 routes. Everything
	// getNICIPv6Config needs is read back from the kernel.
	bond := netlink.NewLinkBond(netlink.LinkAttrs{Name: "bond0"})
	bond.Mode = netlink.BOND_MODE_802_3AD
	if err := netlink.LinkAdd(bond); err != nil {
		t.Fatalf("failed to add bond0: %v", err)
	}
	link, err := netlink.LinkByName("bond0")
	if err != nil {
		t.Fatalf("failed to look up bond0: %v", err)
	}
	// A bond only gets carrier (and thus a link-local address) once it has a
	// slave, so enslave a dummy like the physical ports of a real bond.
	slave := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "bond0-port0"}}
	if err := netlink.LinkAdd(slave); err != nil {
		t.Fatalf("failed to add bond slave: %v", err)
	}
	if err := netlink.LinkSetDown(slave); err != nil {
		t.Fatalf("failed to set bond slave down: %v", err)
	}
	if err := netlink.LinkSetMasterByIndex(slave, link.Attrs().Index); err != nil {
		t.Fatalf("failed to enslave bond slave: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to bring bond0 up: %v", err)
	}
	// addrconf installs the link-local address asynchronously once the bond
	// has carrier; the fabric route below depends on the resulting fe80::/64
	// route being present.
	if err := wait.PollUntilContextTimeout(context.Background(), 50*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			return false, nil
		}
		for _, a := range addrs {
			if a.IP.IsLinkLocalUnicast() {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("bond0 did not get a link-local address: %v", err)
	}
	addr, err := netlink.ParseAddr("2001:db8:1234:5678::1/64")
	if err != nil {
		t.Fatalf("failed to parse bond0 address: %v", err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to bond0: %v", err)
	}
	_, fabric, err := net.ParseCIDR("2001:db8:1000::/36")
	if err != nil {
		t.Fatalf("failed to parse fabric CIDR: %v", err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		Dst:       fabric,
		Gw:        net.ParseIP("fe80::1"),
		LinkIndex: link.Attrs().Index,
	}); err != nil {
		t.Fatalf("failed to add fabric route via fe80::1: %v", err)
	}

	const addrHostPrefix = "2001:db8:1234:5678:0:f:0:c0"
	wantTable := apis.TableIDForName("bond0")
	wantParentRoutes := []apis.RouteConfig{
		{Destination: "fe80::1/128", Scope: unix.RT_SCOPE_LINK, Table: wantTable},
		{Destination: "2001:db8:1000::/36", Gateway: "fe80::1", Table: wantTable},
	}
	// The kernel does not guarantee a dump order for routes; compare as sets.
	sortedRoutes := func(routes []apis.RouteConfig) []apis.RouteConfig {
		sorted := make([]apis.RouteConfig, len(routes))
		copy(sorted, routes)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Destination != sorted[j].Destination {
				return sorted[i].Destination < sorted[j].Destination
			}
			return sorted[i].Gateway < sorted[j].Gateway
		})
		return sorted
	}

	t.Run("allocates an eflo range address with no user addresses", func(t *testing.T) {
		instance := &AlibabaInstance{localIPAM: ipam.NewLocalIPAM(nil)}
		id := cloudprovider.DeviceIdentifiers{Name: "bond0"}
		req := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
		got, err := instance.GetProfileConfig(id, nil, req)
		if err != nil {
			t.Fatalf("GetProfileConfig() error = %v", err)
		}
		if got == nil {
			t.Fatal("expected allocated config, got nil")
		}
		if len(got.Interface.Addresses) != 1 {
			t.Fatalf("expected 1 address, got %v", got.Interface.Addresses)
		}
		addr := got.Interface.Addresses[0]
		if !strings.HasPrefix(addr, addrHostPrefix) {
			t.Errorf("allocated address %q not within eflo /124 range %s*", addr, addrHostPrefix)
		}
		if !reflect.DeepEqual(sortedRoutes(got.Routes), sortedRoutes(wantParentRoutes)) {
			t.Errorf("routes = %+v, want %+v", got.Routes, wantParentRoutes)
		}
		wantRules := []apis.RuleConfig{{Source: addr, Table: wantTable, Priority: apis.SourceRoutingRulePriority}}
		if !reflect.DeepEqual(got.Rules, wantRules) {
			t.Errorf("rules = %+v, want %+v", got.Rules, wantRules)
		}
	})

	t.Run("static user address is reserved and released", func(t *testing.T) {
		instance := &AlibabaInstance{localIPAM: ipam.NewLocalIPAM(nil)}
		id := cloudprovider.DeviceIdentifiers{Name: "bond0"}
		req := &apis.NetworkConfig{Interface: apis.InterfaceConfig{
			Type:      apis.InterfaceTypeIPVLAN,
			Addresses: []string{"2001:db8:1234:5678:0:f:0:c01/128"},
		}}
		got, err := instance.GetProfileConfig(id, nil, req)
		if err != nil {
			t.Fatalf("GetProfileConfig() with static address error = %v", err)
		}
		if got == nil || !reflect.DeepEqual(sortedRoutes(got.Routes), sortedRoutes(wantParentRoutes)) {
			t.Errorf("expected parent routes in table %d for static address, got %v", wantTable, got)
		}
		wantRules := []apis.RuleConfig{{Source: "2001:db8:1234:5678:0:f:0:c01/128", Table: wantTable, Priority: apis.SourceRoutingRulePriority}}
		if got == nil || !reflect.DeepEqual(got.Rules, wantRules) {
			t.Errorf("rules = %+v, want %+v", got.Rules, wantRules)
		}
		if _, err := instance.GetProfileConfig(id, nil, req); err == nil {
			t.Fatal("expected the reserved static address to be rejected")
		}
		if err := instance.ReleaseProfileConfig(id, types.UID("claim-1"), req); err != nil {
			t.Fatalf("ReleaseProfileConfig() error = %v", err)
		}
		if _, err := instance.GetProfileConfig(id, nil, req); err != nil {
			t.Fatalf("expected static address reuse after release: %v", err)
		}
	})

	t.Run("user-owned routing suppresses synthesized PBR", func(t *testing.T) {
		instance := &AlibabaInstance{localIPAM: ipam.NewLocalIPAM(nil)}
		id := cloudprovider.DeviceIdentifiers{Name: "bond0"}
		req := &apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN},
			Routes:    []apis.RouteConfig{{Destination: "2001:db8:9999::/48"}},
		}
		got, err := instance.GetProfileConfig(id, nil, req)
		if err != nil {
			t.Fatalf("GetProfileConfig() error = %v", err)
		}
		if got == nil || len(got.Interface.Addresses) != 1 {
			t.Fatalf("expected allocated address only, got %v", got)
		}
		if len(got.Routes) != 0 || len(got.Rules) != 0 {
			t.Errorf("expected no synthesized routes/rules when user owns routing, got %+v", got)
		}
	})

	t.Run("non-subinterface request is a no-op", func(t *testing.T) {
		instance := &AlibabaInstance{localIPAM: ipam.NewLocalIPAM(nil)}
		id := cloudprovider.DeviceIdentifiers{Name: "bond0"}
		req := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypePassthrough}}
		got, err := instance.GetProfileConfig(id, nil, req)
		if err != nil {
			t.Fatalf("GetProfileConfig() error = %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for passthrough request, got %v", got)
		}
	})

	t.Run("uninitialized IPAM errors instead of silent success", func(t *testing.T) {
		instance := &AlibabaInstance{localIPAM: nil}
		id := cloudprovider.DeviceIdentifiers{Name: "bond0"}
		req := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
		got, err := instance.GetProfileConfig(id, nil, req)
		if err == nil {
			t.Fatal("expected error when profile IPAM is not initialized, got nil")
		}
		if got != nil {
			t.Errorf("expected nil config on error, got %v", got)
		}
	})

}

func TestEfloRouteConfigs(t *testing.T) {
	_, connected, _ := net.ParseCIDR("2001:db8:1234:5678::/64")
	_, linkLocal, _ := net.ParseCIDR("fe80:db8:1234:5678::/64")
	_, fabric, _ := net.ParseCIDR("2001:db8:1000::/36")
	routes := []netlink.Route{
		{Dst: connected},
		{Dst: linkLocal},
		{Dst: fabric, Gw: net.ParseIP("fe80::1")},
		// A default route may be represented without a destination. Reusing
		// the gateway must not create a duplicate on-link host route.
		{Gw: net.ParseIP("fe80::1")},
	}

	got, err := efloRouteConfigs("bond0", routes)
	if err != nil {
		t.Fatalf("efloRouteConfigs() error = %v", err)
	}
	want := []apis.RouteConfig{
		{Destination: "fe80::1/128", Scope: unix.RT_SCOPE_LINK},
		{Destination: "2001:db8:1000::/36", Gateway: "fe80::1"},
		{Destination: "::/0", Gateway: "fe80::1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("efloRouteConfigs() = %+v, want %+v", got, want)
	}
}

func TestEfloRouteConfigsWithoutGateway(t *testing.T) {
	_, connected, _ := net.ParseCIDR("2001:db8:1234:5678::/64")
	got, err := efloRouteConfigs("bond0", []netlink.Route{{Dst: connected}})
	if err != nil {
		t.Fatalf("efloRouteConfigs() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("connected-only parent must not produce child routes: %+v", got)
	}
}

func TestEfloSourceRoutingGatewayNeighbors(t *testing.T) {
	config := &apis.NetworkConfig{Interface: apis.InterfaceConfig{
		Type: apis.InterfaceTypeIPVLAN, Addresses: []string{"2001:db8::10/128"},
	}}
	routes := []apis.RouteConfig{
		{Destination: "fe80::1/128", Scope: unix.RT_SCOPE_LINK},
		{Destination: "2001:db8::/32", Gateway: "fe80::1"},
		{Destination: "::/0", Gateway: "fe80::1"},
		{Destination: "2001:db8:ffff::/48", Gateway: "fe80::2"},
	}
	got := efloSourceRoutingConfig("bond0", config, nil, routes)
	want := []apis.NeighborConfig{
		{Destination: "fe80::1", HardwareAddr: "00:00:5e:00:01:01"},
		{Destination: "fe80::2", HardwareAddr: "00:00:5e:00:01:01"},
	}
	if !reflect.DeepEqual(got.Neighbors, want) {
		t.Fatalf("neighbors = %+v, want %+v", got.Neighbors, want)
	}
	config.Routes = []apis.RouteConfig{{Destination: "::/0", Gateway: "fe80::99"}}
	if got := efloSourceRoutingConfig("bond0", config, nil, routes); got != nil {
		t.Fatalf("user-owned routing must not inherit fabric neighbors: %+v", got)
	}
}

func TestEfloRDMASubinterfaceRange(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "typical /64 prefix",
			cidr: "2001:db8:1234:5678::/64",
			want: "2001:db8:1234:5678:0:f:0:c00/124",
		},
		{
			name: "prefix with non-zero host bits gets masked away",
			cidr: "2001:db8:1234:5678::1/64",
			want: "2001:db8:1234:5678:0:f:0:c00/124",
		},
		{
			name: "subnet with /96 still supported",
			cidr: "2001:db8:1234:5678::/96",
			want: "2001:db8:1234:5678::c00/124",
		},
		{
			name: "subnet with /80 still supported",
			cidr: "2001:db8:1234:5678:abcd::/80",
			want: "2001:db8:1234:5678:abcd:f:0:c00/124",
		},
		{
			name: "minimum supported prefix /124",
			cidr: "2001:db8:1234:5678::/124",
			want: "2001:db8:1234:5678::/124",
		},
		{
			name:    "IPv4 rejected",
			cidr:    "10.0.0.0/24",
			wantErr: true,
		},
		{
			name:    "prefix longer than /124 rejected",
			cidr:    "2001:db8:1234:5678::/125",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, prefix, err := net.ParseCIDR(tt.cidr)
			if err != nil {
				t.Fatalf("failed to parse test CIDR: %v", err)
			}
			got, err := efloRDMASubinterfaceRange(prefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("efloRDMASubinterfaceRange() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("efloRDMASubinterfaceRange() = %q, want %q", got, tt.want)
			}
			if err == nil {
				// Keep the eFLO range within the parent subnet.
				parent, err := netip.ParsePrefix(tt.cidr)
				if err != nil {
					t.Fatalf("failed to parse test CIDR: %v", err)
				}
				rangePrefix, err := netip.ParsePrefix(got)
				if err != nil {
					t.Fatalf("derived range %q is not a valid prefix: %v", got, err)
				}
				if !parent.Contains(rangePrefix.Addr()) {
					t.Errorf("derived range %q is outside parent prefix %q", got, tt.cidr)
				}
			}
		})
	}
}

func TestDetectERDMAPCIAddresses(t *testing.T) {
	orig := detectERDMAPCIAddresses
	t.Cleanup(func() { detectERDMAPCIAddresses = orig })

	detectERDMAPCIAddresses = func() sets.Set[string] {
		return sets.New[string](testPCIAddress)
	}
	got := detectERDMAPCIAddresses()
	if !got.Has(testPCIAddress) {
		t.Errorf("expected %s in result, got %v", testPCIAddress, got)
	}

	detectERDMAPCIAddresses = func() sets.Set[string] {
		return sets.New[string]()
	}
	got = detectERDMAPCIAddresses()
	if got.Len() != 0 {
		t.Errorf("expected empty set, got %v", got)
	}
}
