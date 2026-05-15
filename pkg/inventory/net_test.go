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
	"net"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"

	userns "sigs.k8s.io/dranet/internal/testutils"
)

func TestGetDefaultGwInterfaces(t *testing.T) {
	userns.Run(t, testGetDefaultGwInterfaces_Namespaced, syscall.CLONE_NEWNET)
}

func testGetDefaultGwInterfaces_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	interfaceNames := []string{"eth0", "eth1", "eth2", "wg0"}
	links := make(map[string]netlink.Link)

	// Standardize the subnets so the kernel knows the Gateways are reachable
	ipv4Subnet, _ := netlink.ParseAddr("192.168.1.2/24")
	ipv6Subnet, _ := netlink.ParseAddr("fd00::2/64")

	for _, name := range interfaceNames {
		dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(dummy); err != nil {
			t.Fatalf("failed to add dummy %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(dummy); err != nil {
			t.Fatalf("failed to set %s up: %v", name, err)
		}

		link, _ := netlink.LinkByName(name)

		// Assign IPs to the dummy interfaces to satisfy the kernel's subnet reachability checks
		if err := netlink.AddrAdd(link, ipv4Subnet); err != nil {
			t.Fatalf("failed to add IPv4 address to %s: %v", name, err)
		}
		if err := netlink.AddrAdd(link, ipv6Subnet); err != nil {
			t.Fatalf("failed to add IPv6 address to %s: %v", name, err)
		}

		links[name] = link
	}

	_, defaultIPv4, _ := net.ParseCIDR("0.0.0.0/0")
	_, defaultIPv6, _ := net.ParseCIDR("::/0")
	_, nonDefaultUnspecifiedIPv4, _ := net.ParseCIDR("0.0.0.0/8")
	_, nonDefaultUnspecifiedIPv6, _ := net.ParseCIDR("::/8")
	_, specificNetwork, _ := net.ParseCIDR("10.0.0.0/8")

	// Define Mock Gateway IPs in the same subnets we just assigned to the interfaces
	gwIPv4 := net.ParseIP("192.168.1.1")
	gwIPv6 := net.ParseIP("fd00::1")

	tests := []struct {
		name           string
		setupRoutes    func() error
		expectedResult sets.Set[string]
	}{
		{
			name:           "Empty routing table",
			setupRoutes:    func() error { return nil },
			expectedResult: sets.New[string](),
		},
		{
			name: "Single IPv4 default route",
			setupRoutes: func() error {
				return netlink.RouteAdd(&netlink.Route{
					Family:    netlink.FAMILY_V4,
					Dst:       defaultIPv4,
					Gw:        gwIPv4, // Added Gateway
					LinkIndex: links["eth0"].Attrs().Index,
					Priority:  100,
					Table:     unix.RT_TABLE_MAIN,
				})
			},
			expectedResult: sets.New[string]("eth0"),
		},
		{
			name: "Lower metric wins (IPv4)",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 200, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth1"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth1"),
		},
		{
			name: "Lower metric wins (IPv6)",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: defaultIPv6, Gw: gwIPv6, LinkIndex: links["eth1"].Attrs().Index, Priority: 200, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: defaultIPv6, Gw: gwIPv6, LinkIndex: links["eth2"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth2"),
		},
		{
			name: "Independent families (IPv4 and IPv6 on different interfaces)",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: defaultIPv6, Gw: gwIPv6, LinkIndex: links["eth2"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth0", "eth2"),
		},
		{
			name: "Same interface wins both families",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: defaultIPv6, Gw: gwIPv6, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth0"),
		},
		{
			name: "ECMP: Multiple standard routes with the same metric",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAppend(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth1"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth0", "eth1"),
		},
		{
			name: "Multipath: Single route with multiple nexthops",
			setupRoutes: func() error {
				return netlink.RouteAdd(&netlink.Route{
					Family:   netlink.FAMILY_V4,
					Dst:      defaultIPv4,
					Priority: 100,
					Table:    unix.RT_TABLE_MAIN,
					MultiPath: []*netlink.NexthopInfo{
						// Removed Weight, Added Gw
						{LinkIndex: links["eth1"].Attrs().Index, Gw: gwIPv4},
						{LinkIndex: links["eth2"].Attrs().Index, Gw: gwIPv4},
					},
				})
			},
			expectedResult: sets.New[string]("eth1", "eth2"),
		},
		{
			name: "Point-to-Point Interface (No Gateway IP)",
			setupRoutes: func() error {
				// Intentionally leaving Gw == nil here because Point-to-Point links don't have gateways
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, LinkIndex: links["wg0"].Attrs().Index, Priority: 50, Scope: netlink.SCOPE_LINK, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("wg0"),
		},
		{
			name: "P2P default with Priority 0 wins over gateway default",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, LinkIndex: links["wg0"].Attrs().Index, Priority: 0, Scope: netlink.SCOPE_LINK, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("wg0"),
		},
		{
			name: "Ignores non-default routes",
			setupRoutes: func() error {
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: specificNetwork, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string](),
		},
		{
			name: "Ignores 0.0.0.0/8 even when metric is lower",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: nonDefaultUnspecifiedIPv4, Gw: gwIPv4, LinkIndex: links["eth1"].Attrs().Index, Priority: 10, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V4, Dst: defaultIPv4, Gw: gwIPv4, LinkIndex: links["eth0"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth0"),
		},
		{
			name: "Ignores ::/8 even when metric is lower",
			setupRoutes: func() error {
				if err := netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: nonDefaultUnspecifiedIPv6, Gw: gwIPv6, LinkIndex: links["eth1"].Attrs().Index, Priority: 10, Table: unix.RT_TABLE_MAIN}); err != nil {
					return err
				}
				return netlink.RouteAdd(&netlink.Route{Family: netlink.FAMILY_V6, Dst: defaultIPv6, Gw: gwIPv6, LinkIndex: links["eth2"].Attrs().Index, Priority: 100, Table: unix.RT_TABLE_MAIN})
			},
			expectedResult: sets.New[string]("eth2"),
		},
		{
			name: "Ignores default routes in custom routing tables",
			setupRoutes: func() error {
				return netlink.RouteAdd(&netlink.Route{
					Family:    netlink.FAMILY_V4,
					Dst:       defaultIPv4,
					Gw:        gwIPv4,
					LinkIndex: links["eth0"].Attrs().Index,
					Priority:  100,
					Table:     123,
				})
			},
			expectedResult: sets.New[string](),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Flush the main routing table to guarantee a clean slate
			routes, _ := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
			for _, r := range routes {
				if r.Dst == nil || r.Dst.IP.IsUnspecified() {
					netlink.RouteDel(&r)
				}
			}

			// Apply the test-specific topology
			if err := tt.setupRoutes(); err != nil {
				t.Fatalf("setupRoutes failed: %v", err)
			}

			got := getDefaultGwInterfaces()
			if diff := cmp.Diff(tt.expectedResult, got); diff != "" {
				t.Errorf("getDefaultGwInterfaces() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetExcludedUplinkInterfaces(t *testing.T) {
	userns.Run(t, testGetExcludedUplinkInterfaces_Namespaced, syscall.CLONE_NEWNET)
}

func testGetExcludedUplinkInterfaces_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	_, defaultIPv4, _ := net.ParseCIDR("0.0.0.0/0")
	gwIPv4 := net.ParseIP("192.168.1.1")
	bridgeAddr, _ := netlink.ParseAddr("192.168.1.2/24")

	tests := []struct {
		name           string
		setup          func(t *testing.T)
		expectedResult sets.Set[string]
	}{
		{
			name: "Default-route uplink only",
			setup: func(t *testing.T) {
				addBridgeUplink(t, "br0", bridgeAddr, defaultIPv4, gwIPv4)
			},
			expectedResult: sets.New[string]("br0"),
		},
		{
			name: "Uplink with one child VF",
			setup: func(t *testing.T) {
				br := addBridgeUplink(t, "br0", bridgeAddr, defaultIPv4, gwIPv4)
				addChildDummy(t, "vf0", br.Attrs().Index)
			},
			expectedResult: sets.New[string]("br0", "vf0"),
		},
		{
			name: "Recursive child relationship",
			setup: func(t *testing.T) {
				br := addBridgeUplink(t, "br0", bridgeAddr, defaultIPv4, gwIPv4)
				// bond attached to the uplink bridge, then a dummy attached
				// to the bond. This reproduces the vf -> vf-child -> uplink
				// chain described in the plan. A bond is used as the
				// intermediate link because the kernel rejects bridge-in-
				// bridge nesting (ELOOP).
				vf0 := addChildBond(t, "vf0", br.Attrs().Index)
				addChildDummy(t, "vf0child", vf0.Attrs().Index)
			},
			expectedResult: sets.New[string]("br0", "vf0", "vf0child"),
		},
		{
			name: "Unrelated secondary NIC is not excluded",
			setup: func(t *testing.T) {
				addBridgeUplink(t, "br0", bridgeAddr, defaultIPv4, gwIPv4)
				// Standalone dummy with no master - must remain allocatable.
				eth1 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth1"}}
				if err := netlink.LinkAdd(eth1); err != nil {
					t.Fatalf("failed to add eth1: %v", err)
				}
				if err := netlink.LinkSetUp(eth1); err != nil {
					t.Fatalf("failed to set eth1 up: %v", err)
				}
			},
			expectedResult: sets.New[string]("br0"),
		},
		{
			// macvlan links to its lower device through ParentIndex, not
			// MasterIndex. It carries its own forwarding state and can be
			// relocated into a pod netns without stranding the host uplink,
			// so the exclusion walk (which follows MasterIndex) must leave
			// it allocatable even when the parent is the default-gw uplink.
			name: "Macvlan child of uplink stays allocatable",
			setup: func(t *testing.T) {
				uplink := addDummyUplink(t, "eth0", bridgeAddr, defaultIPv4, gwIPv4)
				addMacvlanChild(t, "mv0", uplink.Attrs().Index)
			},
			expectedResult: sets.New[string]("eth0"),
		},
		{
			// Same reasoning as the macvlan case. ipvlan is split into its
			// own subtest because the kernel refuses to host both a macvlan
			// and an ipvlan on the same lower device simultaneously.
			name: "IPVlan child of uplink stays allocatable",
			setup: func(t *testing.T) {
				uplink := addDummyUplink(t, "eth0", bridgeAddr, defaultIPv4, gwIPv4)
				addIPVlanChild(t, "iv0", uplink.Attrs().Index)
			},
			expectedResult: sets.New[string]("eth0"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Flush the main routing table and tear down any links from the
			// previous subtest so each scenario starts from a clean netns.
			routes, _ := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
			for _, r := range routes {
				if r.Dst == nil || r.Dst.IP.IsUnspecified() {
					netlink.RouteDel(&r)
				}
			}
			links, _ := netlink.LinkList()
			for _, l := range links {
				if l.Attrs().Name == "lo" {
					continue
				}
				_ = netlink.LinkDel(l)
			}

			tt.setup(t)

			got := getExcludedUplinkInterfaces()
			if diff := cmp.Diff(tt.expectedResult, got); diff != "" {
				t.Errorf("getExcludedUplinkInterfaces() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// addBridgeUplink creates a bridge, assigns it an address, brings it up, and
// installs an IPv4 default route through it so it looks like the active
// default-gateway uplink.
func addBridgeUplink(t *testing.T, name string, addr *netlink.Addr, defaultDst *net.IPNet, gw net.IP) netlink.Link {
	t.Helper()
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatalf("failed to add bridge %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up bridge %s: %v", name, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		Family:    netlink.FAMILY_V4,
		Dst:       defaultDst,
		Gw:        gw,
		LinkIndex: link.Attrs().Index,
		Priority:  100,
		Table:     unix.RT_TABLE_MAIN,
	}); err != nil {
		t.Fatalf("failed to install default route via %s: %v", name, err)
	}
	return link
}

// addDummyUplink creates a dummy interface, assigns it an address, brings it
// up, and installs an IPv4 default route through it so it looks like the
// active default-gateway uplink. Used when we need a parent that can host
// macvlan/ipvlan children (which attach via ParentIndex, not MasterIndex).
func addDummyUplink(t *testing.T, name string, addr *netlink.Addr, defaultDst *net.IPNet, gw net.IP) netlink.Link {
	t.Helper()
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to add dummy uplink %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up dummy uplink %s: %v", name, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		Family:    netlink.FAMILY_V4,
		Dst:       defaultDst,
		Gw:        gw,
		LinkIndex: link.Attrs().Index,
		Priority:  100,
		Table:     unix.RT_TABLE_MAIN,
	}); err != nil {
		t.Fatalf("failed to install default route via %s: %v", name, err)
	}
	return link
}

// addMacvlanChild creates a macvlan attached to parentIndex via ParentIndex.
// MasterIndex stays zero, which is the distinction the exclusion walk relies
// on to leave these children in the inventory.
func addMacvlanChild(t *testing.T, name string, parentIndex int) netlink.Link {
	t.Helper()
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{Name: name, ParentIndex: parentIndex},
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		t.Fatalf("failed to add macvlan %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up macvlan %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	return link
}

// addIPVlanChild creates an ipvlan attached to parentIndex via ParentIndex,
// with MasterIndex left at zero for the same reason as addMacvlanChild.
func addIPVlanChild(t *testing.T, name string, parentIndex int) netlink.Link {
	t.Helper()
	iv := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{Name: name, ParentIndex: parentIndex},
		Mode:      netlink.IPVLAN_MODE_L2,
	}
	if err := netlink.LinkAdd(iv); err != nil {
		t.Fatalf("failed to add ipvlan %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up ipvlan %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	return link
}

// addChildDummy creates a dummy interface and attaches it to the link at
// masterIndex so it shows up with MasterIndex == masterIndex.
func addChildDummy(t *testing.T, name string, masterIndex int) netlink.Link {
	t.Helper()
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to add dummy %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up dummy %s: %v", name, err)
	}
	if err := netlink.LinkSetMasterByIndex(link, masterIndex); err != nil {
		t.Fatalf("failed to attach %s to index %d: %v", name, masterIndex, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	return link
}

// addChildBond creates a bond attached to masterIndex so it can itself act
// as a parent for a further descendant link. A bond is used here (rather
// than a bridge) because the kernel refuses to nest a bridge inside another
// bridge (ELOOP), but a bond can be enrolled as a bridge port.
func addChildBond(t *testing.T, name string, masterIndex int) netlink.Link {
	t.Helper()
	bond := netlink.NewLinkBond(netlink.LinkAttrs{Name: name})
	if err := netlink.LinkAdd(bond); err != nil {
		t.Fatalf("failed to add bond %s: %v", name, err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to look up bond %s: %v", name, err)
	}
	if err := netlink.LinkSetMasterByIndex(link, masterIndex); err != nil {
		t.Fatalf("failed to attach bond %s to index %d: %v", name, masterIndex, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set %s up: %v", name, err)
	}
	return link
}
