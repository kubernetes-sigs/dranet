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
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/dranet/pkg/apis"

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

// TestAddLinkAttributesIPLengthCap covers the per-attribute string-value
// limit on AttrIPv4 / AttrIPv6 (see resourceapi.DeviceAttributeMaxValueLength).
// The kube-proxy IPVS dummy interface (kube-ipvs0) accumulates every cluster
// ServiceIP, and if dranet joins them all into a single comma-separated
// attribute the resulting ResourceSlice is rejected by the API server with
// "Too long: may not be more than 64 bytes". addLinkAttributes must truncate
// the joined value so it fits within the cap while still publishing as many
// addresses as possible, and the other family's attribute must not be
// affected when only one family overflows.
func TestAddLinkAttributesIPLengthCap(t *testing.T) {
	userns.Run(t, testAddLinkAttributesIPLengthCap_Namespaced, syscall.CLONE_NEWNET)
}

func testAddLinkAttributesIPLengthCap_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	// Build IP address sets of various sizes.
	// One IPv4 "/32" is 12 bytes (e.g. "10.96.0.1/32"); 5 of them joined by
	// commas total 64 bytes — the boundary case. 6 of them spill over.
	manyV4 := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		manyV4 = append(manyV4, fmt.Sprintf("10.96.0.%d/32", i+1))
	}
	manyV4Set := sets.New(manyV4...)
	manyV6 := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		// Each "fd00::N/128" is 11–12 bytes; 6 of them comfortably overflow.
		manyV6 = append(manyV6, fmt.Sprintf("fd00::%x/128", i+1))
	}
	manyV6Set := sets.New(manyV6...)

	tests := []struct {
		name           string
		ipv4           []string
		ipv6           []string
		wantV4Set      bool
		wantV6Set      bool
		wantV4Value    string // when non-empty, the attribute must match exactly
		wantV6Value    string
		wantV4Pool     sets.Set[string] // when non-nil, every comma-split entry must be in this set
		wantV6Pool     sets.Set[string]
		wantV4Truncate bool // when true, attribute must be set AND shorter than the full join
		wantV6Truncate bool
	}{
		{
			name: "no IPs - neither attribute set",
		},
		{
			name:        "single IPv4 - attribute set",
			ipv4:        []string{"10.0.0.1/24"},
			wantV4Set:   true,
			wantV4Value: "10.0.0.1/24",
		},
		{
			name:        "single IPv6 - attribute set",
			ipv6:        []string{"fd00::1/64"},
			wantV6Set:   true,
			wantV6Value: "fd00::1/64",
		},
		{
			name:           "many IPv4 overflow - attribute truncated",
			ipv4:           manyV4,
			wantV4Set:      true,
			wantV4Pool:     manyV4Set,
			wantV4Truncate: true,
		},
		{
			name:           "many IPv6 overflow - attribute truncated",
			ipv6:           manyV6,
			wantV6Set:      true,
			wantV6Pool:     manyV6Set,
			wantV6Truncate: true,
		},
		{
			// kube-ipvs0-style mix: lots of v4 ClusterIPs overflow the limit
			// but a single v6 host address still fits — AttrIPv4 is truncated
			// to fit, and AttrIPv6 is published in full.
			name:           "v4 overflow does not drop v6",
			ipv4:           manyV4,
			ipv6:           []string{"fd00::1/64"},
			wantV4Set:      true,
			wantV4Pool:     manyV4Set,
			wantV4Truncate: true,
			wantV6Set:      true,
			wantV6Value:    "fd00::1/64",
		},
		{
			// Mirror of the above: the v6 set overflows but a single v4
			// address still fits. AttrIPv6 is truncated, AttrIPv4 is
			// published in full. Covers the tamilmani1989 review case.
			name:           "v6 overflow does not drop v4",
			ipv4:           []string{"10.0.0.1/24"},
			ipv6:           manyV6,
			wantV4Set:      true,
			wantV4Value:    "10.0.0.1/24",
			wantV6Set:      true,
			wantV6Pool:     manyV6Set,
			wantV6Truncate: true,
		},
	}

	for i, tt := range tests {
		tt := tt
		ifName := fmt.Sprintf("attrcap%d", i)
		t.Run(tt.name, func(t *testing.T) {
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
			if err := netlink.LinkAdd(dummy); err != nil {
				t.Fatalf("failed to add dummy %s: %v", ifName, err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })

			link, err := netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("failed to look up %s: %v", ifName, err)
			}
			// Bring the link up so global-scope addresses stay usable.
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatalf("failed to set %s up: %v", ifName, err)
			}

			for _, cidr := range tt.ipv4 {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s, %s): %v", ifName, cidr, err)
				}
			}
			for _, cidr := range tt.ipv6 {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s, %s): %v", ifName, cidr, err)
				}
			}

			// Re-fetch the link so its address list is current.
			link, err = netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("re-fetch %s: %v", ifName, err)
			}

			device := &resourceapi.Device{
				Name:       ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			}
			addLinkAttributes(device, link)

			// Always-set attributes — sanity check we didn't break the rest
			// of addLinkAttributes while editing the IP block.
			if got, ok := device.Attributes[apis.AttrInterfaceName]; !ok || got.StringValue == nil || *got.StringValue != ifName {
				t.Errorf("AttrInterfaceName = %+v, want %q", got, ifName)
			}

			gotV4, hasV4 := device.Attributes[apis.AttrIPv4]
			if hasV4 != tt.wantV4Set {
				t.Errorf("AttrIPv4 present = %v, want %v (value=%+v)", hasV4, tt.wantV4Set, gotV4)
			}
			if hasV4 && gotV4.StringValue != nil {
				checkIPAttribute(t, "AttrIPv4", *gotV4.StringValue, tt.wantV4Value, tt.wantV4Pool, tt.wantV4Truncate, tt.ipv4)
			}

			gotV6, hasV6 := device.Attributes[apis.AttrIPv6]
			if hasV6 != tt.wantV6Set {
				t.Errorf("AttrIPv6 present = %v, want %v (value=%+v)", hasV6, tt.wantV6Set, gotV6)
			}
			if hasV6 && gotV6.StringValue != nil {
				checkIPAttribute(t, "AttrIPv6", *gotV6.StringValue, tt.wantV6Value, tt.wantV6Pool, tt.wantV6Truncate, tt.ipv6)
			}
		})
	}
}

// checkIPAttribute asserts the invariants every published IP attribute must
// satisfy: it fits within the DRA cap, every comma-split entry came from the
// originally-provided pool (no fabricated values), the exact value matches
// when one is specified, and when truncation is expected the result is
// strictly shorter than joining the full input.
func checkIPAttribute(t *testing.T, name, got, wantExact string, wantPool sets.Set[string], wantTruncated bool, fullInput []string) {
	t.Helper()
	if len(got) > resourceapi.DeviceAttributeMaxValueLength {
		t.Errorf("%s value length %d exceeds DRA cap %d: %q",
			name, len(got), resourceapi.DeviceAttributeMaxValueLength, got)
	}
	if wantExact != "" && got != wantExact {
		t.Errorf("%s = %q, want %q", name, got, wantExact)
	}
	if wantPool != nil {
		for _, entry := range strings.Split(got, ",") {
			if !wantPool.Has(entry) {
				t.Errorf("%s contains %q which is not in the input pool", name, entry)
			}
		}
	}
	if wantTruncated {
		fullLen := 0
		for i, ip := range fullInput {
			fullLen += len(ip)
			if i > 0 {
				fullLen++
			}
		}
		if len(got) >= fullLen {
			t.Errorf("%s = %q (len=%d) expected to be a truncated prefix of the %d-byte full join",
				name, got, len(got), fullLen)
		}
		if got == "" {
			t.Errorf("%s expected to be a non-empty truncated value", name)
		}
	}
}

// TestAddLinkAttributesIPBoundaryLength exercises the exact DRA cap: a joined
// string of length DeviceAttributeMaxValueLength is published in full, while
// adding one more address that would push us past the limit causes the
// attribute to be truncated rather than dropped.
func TestAddLinkAttributesIPBoundaryLength(t *testing.T) {
	userns.Run(t, testAddLinkAttributesIPBoundaryLength_Namespaced, syscall.CLONE_NEWNET)
}

func testAddLinkAttributesIPBoundaryLength_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	// 5 × "10.96.0.N/32" (12 bytes each) joined with commas = 5*12 + 4 = 64 bytes.
	addrsExactlyAtLimit := []string{
		"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32",
	}
	addrsJustOverLimit := append([]string{}, addrsExactlyAtLimit...)
	// "10.96.0.10/32" is 13 bytes; appending it and a comma makes the join >64.
	addrsJustOverLimit = append(addrsJustOverLimit, "10.96.0.10/32")

	cases := []struct {
		name          string
		addrs         []string
		wantSet       bool
		wantTruncated bool // when true, value must be set and < full join length
	}{
		{name: "exactly at limit", addrs: addrsExactlyAtLimit, wantSet: true},
		{name: "just over limit", addrs: addrsJustOverLimit, wantSet: true, wantTruncated: true},
	}

	for i, tc := range cases {
		tc := tc
		ifName := fmt.Sprintf("attrbnd%d", i)
		t.Run(tc.name, func(t *testing.T) {
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
			if err := netlink.LinkAdd(dummy); err != nil {
				t.Fatalf("LinkAdd %s: %v", ifName, err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })
			link, err := netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("LinkByName %s: %v", ifName, err)
			}
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatalf("LinkSetUp %s: %v", ifName, err)
			}
			for _, cidr := range tc.addrs {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s,%s): %v", ifName, cidr, err)
				}
			}
			link, _ = netlink.LinkByName(ifName)

			device := &resourceapi.Device{
				Name:       ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			}
			addLinkAttributes(device, link)

			got, has := device.Attributes[apis.AttrIPv4]
			if has != tc.wantSet {
				t.Fatalf("AttrIPv4 present = %v, want %v", has, tc.wantSet)
			}
			if !has || got.StringValue == nil {
				return
			}
			if len(*got.StringValue) > resourceapi.DeviceAttributeMaxValueLength {
				t.Errorf("AttrIPv4 joined length = %d, exceeds DRA cap %d (value=%q)",
					len(*got.StringValue), resourceapi.DeviceAttributeMaxValueLength, *got.StringValue)
			}
			// Defense in depth: the value must contain a comma for the
			// multi-address case, which proves we used Join correctly.
			if len(tc.addrs) > 1 && !strings.Contains(*got.StringValue, ",") {
				t.Errorf("AttrIPv4 = %q, expected comma-joined value", *got.StringValue)
			}
			// Every entry must come from the input set: truncation must
			// preserve a prefix of sorted inputs, not fabricate values.
			pool := sets.New(tc.addrs...)
			for _, entry := range strings.Split(*got.StringValue, ",") {
				if !pool.Has(entry) {
					t.Errorf("AttrIPv4 contains %q which is not in the input addrs", entry)
				}
			}
			fullLen := 0
			for i, ip := range tc.addrs {
				fullLen += len(ip)
				if i > 0 {
					fullLen++
				}
			}
			if !tc.wantTruncated && len(*got.StringValue) != fullLen {
				t.Errorf("AttrIPv4 length = %d, want full-join length %d (value=%q)",
					len(*got.StringValue), fullLen, *got.StringValue)
			}
			if tc.wantTruncated && len(*got.StringValue) >= fullLen {
				t.Errorf("AttrIPv4 length = %d, expected < %d (truncated) (value=%q)",
					len(*got.StringValue), fullLen, *got.StringValue)
			}
		})
	}
}

// TestBuildIPList exercises the truncation helper directly, away from netns
// plumbing, so the byte-arithmetic boundaries are easy to read and the test
// runs on any platform (not just linux).
func TestBuildIPList(t *testing.T) {
	cases := []struct {
		name     string
		ips      []string
		maxBytes int
		want     string
		wantKept int
	}{
		{
			name:     "empty input",
			ips:      nil,
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "single ip fits",
			ips:      []string{"10.0.0.1/24"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.0.0.1/24",
			wantKept: 1,
		},
		{
			name:     "all fit exactly at limit",
			ips:      []string{"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.96.0.1/32,10.96.0.2/32,10.96.0.3/32,10.96.0.4/32,10.96.0.5/32",
			wantKept: 5,
		},
		{
			name:     "stops before the address that would overflow",
			ips:      []string{"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32", "10.96.0.6/32"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.96.0.1/32,10.96.0.2/32,10.96.0.3/32,10.96.0.4/32,10.96.0.5/32",
			wantKept: 5,
		},
		{
			name:     "first ip larger than limit returns empty",
			ips:      []string{"10.96.0.1/32"},
			maxBytes: 5,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "non-positive limit returns empty",
			ips:      []string{"10.0.0.1/24"},
			maxBytes: 0,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "ipv6 truncation stops at the right boundary",
			ips:      []string{"fd00::1/128", "fd00::2/128", "fd00::3/128", "fd00::4/128", "fd00::5/128", "fd00::6/128"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			// 5 × 11 + 4 commas = 59 bytes; the 6th would push to 71.
			want:     "fd00::1/128,fd00::2/128,fd00::3/128,fd00::4/128,fd00::5/128",
			wantKept: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, kept := buildIPList(tc.ips, tc.maxBytes)
			if got != tc.want {
				t.Errorf("buildIPList = %q, want %q", got, tc.want)
			}
			if kept != tc.wantKept {
				t.Errorf("buildIPList kept = %d, want %d", kept, tc.wantKept)
			}
			if len(got) > tc.maxBytes && tc.maxBytes > 0 {
				t.Errorf("buildIPList result length %d exceeds maxBytes %d", len(got), tc.maxBytes)
			}
		})
	}
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
