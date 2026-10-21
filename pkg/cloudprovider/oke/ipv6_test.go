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

package oke

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

// The rail 3 VF of a BM.GPU.B300.8 node on a multi-planar IPv6 fabric: its RA
// address in the host's /64 and the rail router of its VRF table.
const (
	testRailParent = "fdcd:8150:5654:70c1:2:b8ff:fe96:6195/64"
	testRailPrefix = "fdcd:8150:5654:70c1::/64"
	testRailRouter = "fe80::b061:4eff:fe05:60c1"
)

// fakeIPv6Addresses serves the CIDRs, each with optional IFA_F flags, as the
// IPv6 addresses of ifName.
func fakeIPv6Addresses(t *testing.T, ifName string, addrs ...ipv6HostAddr) {
	t.Helper()
	original := hostIPv6Addresses
	hostIPv6Addresses = func(name string) ([]ipv6HostAddr, error) {
		if name != ifName {
			return nil, errors.New("no such interface")
		}
		return addrs, nil
	}
	t.Cleanup(func() { hostIPv6Addresses = original })
}

func hostAddr(cidr string, flags int) ipv6HostAddr {
	return ipv6HostAddr{Prefix: netip.MustParsePrefix(cidr), Flags: flags}
}

func fakeIPv6Routers(t *testing.T, routers map[string][]ipv6Router) {
	t.Helper()
	original := hostIPv6DefaultRouters
	hostIPv6DefaultRouters = func(ifName string) ([]ipv6Router, error) {
		r, ok := routers[ifName]
		if !ok {
			return nil, errors.New("no such interface")
		}
		return r, nil
	}
	t.Cleanup(func() { hostIPv6DefaultRouters = original })
}

func testClaim(uid string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}}
}

func TestRailVFIndex(t *testing.T) {
	tests := []struct {
		ifName    string
		wantIndex int
		wantOK    bool
	}{
		{ifName: "rdma_vf_rail0", wantIndex: 0, wantOK: true},
		{ifName: "rdma_vf_rail7", wantIndex: 7, wantOK: true},
		{ifName: "rdma_vf_rail15", wantIndex: 15, wantOK: true},
		{ifName: "rdma_vf_rail", wantOK: false},
		{ifName: "rdma_vf_railx", wantOK: false},
		{ifName: "rdma_p0_rail0", wantOK: false},
		{ifName: "rdma0", wantOK: false},
		{ifName: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.ifName, func(t *testing.T) {
			gotIndex, gotOK := railVFIndex(tt.ifName)
			if gotIndex != tt.wantIndex || gotOK != tt.wantOK {
				t.Errorf("railVFIndex(%q) = %d, %v, want %d, %v", tt.ifName, gotIndex, gotOK, tt.wantIndex, tt.wantOK)
			}
		})
	}
}

func TestGetDeviceConfigRailVF(t *testing.T) {
	profile := &apis.NetworkConfig{Profile: okeRDMAProfile, Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
	guard := &apis.NetworkConfig{Profile: okeRDMAProfile}
	ipv6Fabric := &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 4}}
	tests := []struct {
		name         string
		ifNames      []string
		hardwareType int
		metadata     *okeMetadata
		want         *apis.NetworkConfig
	}{
		{
			name:     "rail VF on an IPv6 fabric gets the profile",
			ifNames:  []string{"rdma_vf_rail3"},
			metadata: ipv6Fabric,
			want:     profile,
		},
		{
			// The inventory can scan before IMDS answers; without the profile
			// a claim would move the VF into the pod.
			name:    "rail VF before the fabric metadata gets the profile",
			ifNames: []string{"rdma_vf_rail3"},
			want:    profile,
		},
		{
			name:     "rail VF on an IPv4 fabric gets the profile",
			ifNames:  []string{"rdma_vf_rail3"},
			metadata: &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false}},
			want:     profile,
		},
		{
			name:         "non-Ethernet rail VF gets no profile",
			ifNames:      []string{"rdma_vf_rail3"},
			hardwareType: unix.ARPHRD_INFINIBAND,
			metadata:     ipv6Fabric,
		},
		{
			name:     "PF function with plane netdevs gets the guard",
			ifNames:  []string{"rdma3", "rdma_p0_rail3"},
			metadata: ipv6Fabric,
			want:     guard,
		},
		{
			name:    "PF function before the fabric metadata gets the guard",
			ifNames: []string{"rdma3", "rdma_p0_rail3"},
			want:    guard,
		},
		{
			name:     "function with several non-RDMA netdevs gets no profile",
			ifNames:  []string{"eth2", "eth3"},
			metadata: ipv6Fabric,
		},
		{
			name:     "lone plane netdev gets no profile",
			ifNames:  []string{"rdma_p1_rail3"},
			metadata: ipv6Fabric,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			hardwareType := tt.hardwareType
			if hardwareType == 0 {
				hardwareType = unix.ARPHRD_ETHER
			}
			for _, ifName := range tt.ifNames {
				fakeInterface(t, ifName, "0000:11:00.4", hardwareType)
			}
			instance := newOKEInstance(tt.metadata, nil)
			instance.addressFallback = false
			got := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: tt.ifNames[0], PCIAddress: "0000:11:00.4"})
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetDeviceConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The PCI function of an RDMA netdev that shares it with other netdevs stays
// on the host on either fabric.
func TestGetProfileConfigSharedRDMAFunction(t *testing.T) {
	for name, metadata := range map[string]*okeMetadata{
		"IPv6 fabric": {RDMAFabric: &rdmaFabric{IPv6: true, Planes: 4}},
		"IPv4 fabric": {RDMAFabric: &rdmaFabric{}, PrimaryVNIC: testVNIC()},
	} {
		t.Run(name, func(t *testing.T) {
			fakeSysfs(t)
			fakeInterface(t, "rdma3", "0000:11:00.0", unix.ARPHRD_ETHER)
			fakeInterface(t, "rdma_p0_rail3", "0000:11:00.0", unix.ARPHRD_ETHER)
			instance := newOKEInstance(metadata, nil)
			config := &apis.NetworkConfig{Profile: okeRDMAProfile}
			_, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{Name: "rdma3", PCIAddress: "0000:11:00.0"}, testClaim("uid"), config)
			if err == nil || !strings.Contains(err.Error(), "carries several netdevs") {
				t.Fatalf("GetProfileConfig() error = %v, want the shared-function error", err)
			}
		})
	}
}

// A rail VF gets the profile before the fabric is known, and an IPv4 fabric
// refuses it rather than giving it an OCA address.
func TestGetProfileConfigRailVFOnIPv4Fabric(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma_vf_rail3", "0000:11:00.4", unix.ARPHRD_ETHER)
	fakeInterfaceAddresses(t, "rdma_vf_rail3", "10.224.13.19/12")
	fakeARP(t, "rdma_vf_rail3", "1", "2")
	instance := newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{}, PrimaryVNIC: testVNIC()}, nil)
	config := &apis.NetworkConfig{Profile: okeRDMAProfile}
	_, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{Name: "rdma_vf_rail3", PCIAddress: "0000:11:00.4"}, testClaim("uid"), config)
	if err == nil || !strings.Contains(err.Error(), "on an IPv6 RDMA fabric only") {
		t.Fatalf("GetProfileConfig() error = %v, want the IPv4 rail VF error", err)
	}
}

func TestIPv6ProfileConfig(t *testing.T) {
	ipv6Fabric := &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 4}}
	profileConfig := func(iface apis.InterfaceConfig) *apis.NetworkConfig {
		return &apis.NetworkConfig{Profile: okeRDMAProfile, Interface: iface}
	}
	parent := []ipv6HostAddr{hostAddr(testRailParent, 0), hostAddr("fe80::2:b8ff:fe96:6195/64", 0)}
	router := []ipv6Router{{Gateway: netip.MustParseAddr(testRailRouter), Metric: 1024}}
	// The child addresses of the test claim, computed outside the code under
	// test: SHA-256 of "<claim UID>\x00<parent>\x000", first 8 bytes.
	const (
		railChild  = "fdcd:8150:5654:70c1:a74b:74bb:baf9:3c66"
		rdma5Child = "fdcd:8150:5654:70c1:f71d:b1a0:d312:2b96"
	)
	tests := []struct {
		name         string
		ifName       string
		hardwareType int
		addresses    []ipv6HostAddr
		routers      []ipv6Router
		config       *apis.NetworkConfig
		claim        *resourceapi.ResourceClaim
		wantChild    string
		wantRouter   string
		// wantTable is the routing table of a resolved profile; 0 expects no routes.
		wantTable int
		wantErr   string
	}{
		{
			name:      "rail VF",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			wantChild: railChild,
			wantTable: 119,
		},
		{
			name:      "rdmaN NIC",
			ifName:    "rdma5",
			addresses: parent,
			routers:   router,
			wantChild: rdma5Child,
			wantTable: 105,
		},
		{
			name:       "lowest metric router wins",
			ifName:     "rdma_vf_rail3",
			addresses:  parent,
			routers:    []ipv6Router{{Gateway: netip.MustParseAddr("fe80::1"), Metric: 2048}, router[0]},
			wantChild:  railChild,
			wantTable:  119,
			wantRouter: testRailRouter,
		},
		{
			name:       "metric tie takes the lowest address",
			ifName:     "rdma_vf_rail3",
			addresses:  parent,
			routers:    []ipv6Router{router[0], {Gateway: netip.MustParseAddr("fe80::1"), Metric: 1024}},
			wantChild:  railChild,
			wantTable:  119,
			wantRouter: "fe80::1",
		},
		{
			// After the fabric renumbers, the old prefix stays deprecated for its
			// valid lifetime; it neither names the prefix nor makes two.
			name:      "deprecated old prefix is skipped",
			ifName:    "rdma_vf_rail3",
			addresses: append([]ipv6HostAddr{hostAddr("fdcd:8150:5654:70c2::5/64", unix.IFA_F_DEPRECATED)}, parent...),
			routers:   router,
			wantChild: railChild,
			wantTable: 119,
		},
		{
			name:      "user routes keep only the address",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config: &apis.NetworkConfig{Profile: okeRDMAProfile,
				Routes: []apis.RouteConfig{{Destination: "::/0", Gateway: testRailRouter}}},
			wantChild: railChild,
		},
		{
			name:      "user rules keep only the address",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config: &apis.NetworkConfig{Profile: okeRDMAProfile,
				Rules: []apis.RuleConfig{{Priority: 100, Source: "fdcd:8150:5654:70c1::/64", Table: 300}}},
			wantChild: railChild,
		},
		{
			// Joining a VRF would drop the child's static IPv6 address.
			name:      "user VRF is rejected",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config:    profileConfig(apis.InterfaceConfig{VRF: &apis.VRFConfig{Name: "vrf_r3"}}),
			wantErr:   "does not support interface.vrf",
		},
		{
			name:      "claim acceptRA 0 is accepted",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config:    profileConfig(apis.InterfaceConfig{AcceptRA: ptr.To[int32](0)}),
			wantChild: railChild,
			wantTable: 119,
		},
		{
			name:      "claim acceptRA 2 is rejected",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config:    profileConfig(apis.InterfaceConfig{AcceptRA: ptr.To[int32](2)}),
			wantErr:   "needs acceptRA 0",
		},
		{
			name:      "no global address yet",
			ifName:    "rdma_vf_rail3",
			addresses: parent[1:],
			routers:   router,
			wantErr:   "no usable global IPv6 address",
		},
		{
			name:      "tentative address is not usable yet",
			ifName:    "rdma_vf_rail3",
			addresses: []ipv6HostAddr{hostAddr(testRailParent, unix.IFA_F_TENTATIVE)},
			routers:   router,
			wantErr:   "no usable global IPv6 address",
		},
		{
			name:      "failed duplicate address detection is not usable",
			ifName:    "rdma_vf_rail3",
			addresses: []ipv6HostAddr{hostAddr(testRailParent, unix.IFA_F_DADFAILED|unix.IFA_F_TENTATIVE)},
			routers:   router,
			wantErr:   "no usable global IPv6 address",
		},
		{
			name:      "address outside a /64",
			ifName:    "rdma_vf_rail3",
			addresses: []ipv6HostAddr{hostAddr("fdcd:8150:5654:70c1::5/128", 0)},
			routers:   router,
			wantErr:   "no usable global IPv6 address",
		},
		{
			name:      "two usable prefixes are ambiguous",
			ifName:    "rdma_vf_rail3",
			addresses: append([]ipv6HostAddr{hostAddr("fdcd:8150:5654:70c2::5/64", 0)}, parent...),
			routers:   router,
			wantErr:   "in 2 prefixes",
		},
		{
			name:      "no default route yet",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			wantErr:   "no IPv6 default route",
		},
		{
			name:      "index above the largest shape",
			ifName:    "rdma_vf_rail16",
			addresses: parent,
			routers:   router,
			wantErr:   "above the largest supported index 15",
		},
		{
			name:      "unindexed parent",
			ifName:    "ens3f0",
			addresses: parent,
			routers:   router,
			wantErr:   "needs an rdma<N> or rdma_vf_rail<N> parent",
		},
		{
			name:         "non-Ethernet parent",
			ifName:       "rdma_vf_rail3",
			hardwareType: unix.ARPHRD_INFINIBAND,
			addresses:    parent,
			routers:      router,
			wantErr:      "requires an Ethernet parent",
		},
		{
			name:      "claim without a UID",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			claim:     &resourceapi.ResourceClaim{},
			wantErr:   "needs the claim UID",
		},
		{
			name:      "passthrough is rejected",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config:    profileConfig(apis.InterfaceConfig{Type: apis.InterfaceTypePassthrough}),
			wantErr:   "does not move an RDMA NIC into a pod",
		},
		{
			name:      "claim addresses are rejected",
			ifName:    "rdma_vf_rail3",
			addresses: parent,
			routers:   router,
			config:    profileConfig(apis.InterfaceConfig{Addresses: []string{"fdcd:8150:5654:70c1::5/64"}}),
			wantErr:   "assigns the child address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			hardwareType := tt.hardwareType
			if hardwareType == 0 {
				hardwareType = unix.ARPHRD_ETHER
			}
			fakeInterface(t, tt.ifName, "0000:11:00.4", hardwareType)
			fakeIPv6Addresses(t, tt.ifName, tt.addresses...)
			fakeIPv6Routers(t, map[string][]ipv6Router{tt.ifName: tt.routers})
			config := tt.config
			if config == nil {
				config = profileConfig(apis.InterfaceConfig{})
			}
			claim := tt.claim
			if claim == nil {
				claim = testClaim("3f1c9b52-1d4e-4c84-9a3e-0d4d1f0c7a11")
			}
			instance := newOKEInstance(ipv6Fabric, nil)
			id := cloudprovider.DeviceIdentifiers{Name: tt.ifName, PCIAddress: "0000:11:00.4"}
			got, err := instance.GetProfileConfig(id, claim, config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetProfileConfig() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetProfileConfig() returned error: %v", err)
			}

			want := &apis.NetworkConfig{Interface: apis.InterfaceConfig{
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{tt.wantChild + "/64"},
				AcceptRA:  ptr.To[int32](0),
			}}
			if tt.wantTable != 0 {
				gateway := tt.wantRouter
				if gateway == "" {
					gateway = testRailRouter
				}
				want.Routes = []apis.RouteConfig{
					{Destination: testRailPrefix, Scope: unix.RT_SCOPE_LINK, Table: tt.wantTable},
					{Destination: "::/0", Gateway: gateway, Table: tt.wantTable},
				}
				want.Rules = []apis.RuleConfig{{Priority: apis.SourceRoutingRulePriority, Source: tt.wantChild + "/128", Table: tt.wantTable}}
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("GetProfileConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestIPv6DefaultRouters(t *testing.T) {
	gw := func(s string) net.IP { return net.ParseIP(s) }
	_, defaultDst, _ := net.ParseCIDR("::/0")
	_, prefixDst, _ := net.ParseCIDR("fdcd:8150:5654:70c1::/64")
	routes := []netlink.Route{
		// The RA default route of the rail, in its VRF table.
		{LinkIndex: 7, Gw: gw("fe80::b061:4eff:fe05:60c1"), Priority: 1024, Table: 103, Type: unix.RTN_UNICAST},
		// The same, with an explicit ::/0 destination.
		{LinkIndex: 7, Dst: defaultDst, Gw: gw("fe80::2"), Priority: 2048, Type: unix.RTN_UNICAST},
		// Another interface's default route.
		{LinkIndex: 8, Gw: gw("fe80::3"), Priority: 1024, Type: unix.RTN_UNICAST},
		// Not a default route.
		{LinkIndex: 7, Dst: prefixDst, Gw: gw("fe80::4"), Type: unix.RTN_UNICAST},
		// A default route without a gateway.
		{LinkIndex: 7, Priority: 1024, Type: unix.RTN_UNICAST},
		// Not unicast.
		{LinkIndex: 7, Gw: gw("fe80::5"), Type: unix.RTN_UNREACHABLE},
		// Multipath: no interface of its own, one nexthop through this link.
		{Priority: 512, Type: unix.RTN_UNICAST, MultiPath: []*netlink.NexthopInfo{
			{LinkIndex: 8, Gw: gw("fe80::6")},
			{LinkIndex: 7, Gw: gw("fe80::7")},
			nil,
		}},
	}
	got, unreadable := ipv6DefaultRouters(routes, 7)
	if unreadable != 0 {
		t.Errorf("ipv6DefaultRouters() unreadable = %d, want 0", unreadable)
	}
	want := []ipv6Router{
		{Gateway: netip.MustParseAddr("fe80::b061:4eff:fe05:60c1"), Metric: 1024},
		{Gateway: netip.MustParseAddr("fe80::2"), Metric: 2048},
		{Gateway: netip.MustParseAddr("fe80::7"), Metric: 512},
	}
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Errorf("ipv6DefaultRouters() mismatch (-want +got):\n%s", diff)
	}
}

func TestChildIPv6Address(t *testing.T) {
	prefix := netip.MustParsePrefix(testRailPrefix)
	a := childIPv6Address(prefix, "claim-a", "rdma_vf_rail3", nil)
	// Pinned: SHA-256 of "claim-a\x00rdma_vf_rail3\x000", computed outside the code.
	if want := netip.MustParseAddr("fdcd:8150:5654:70c1:fdda:4222:1136:b61f"); a != want {
		t.Errorf("childIPv6Address() = %s, want %s", a, want)
	}
	if again := childIPv6Address(prefix, "claim-a", "rdma_vf_rail3", nil); again != a {
		t.Errorf("same claim and parent gave %s and %s, want one stable address", a, again)
	}
	if b := childIPv6Address(prefix, "claim-b", "rdma_vf_rail3", nil); b == a {
		t.Errorf("two claims on one parent both got %s", a)
	}
	if c := childIPv6Address(prefix, "claim-a", "rdma_vf_rail4", nil); c == a {
		t.Errorf("one claim on two parents got %s twice", a)
	}
	if !prefix.Contains(a) {
		t.Errorf("child %s is outside %s", a, prefix)
	}
	// An address the parent holds is skipped for the next hash.
	if d := childIPv6Address(prefix, "claim-a", "rdma_vf_rail3", []netip.Addr{a}); d == a || !prefix.Contains(d) {
		t.Errorf("childIPv6Address() with %s taken = %s", a, d)
	}
}

func TestReservedInterfaceID(t *testing.T) {
	tests := []struct {
		iid  uint64
		want bool
	}{
		{iid: 0, want: true},
		{iid: 0x02005efffe000000, want: true},
		{iid: 0x02005efffe005213, want: true},
		{iid: 0x02005efffeffffff, want: true},
		{iid: 0xfdffffffffffff80, want: true},
		{iid: 0xfdffffffffffffff, want: true},
		{iid: 0xfe00000000000000, want: false},
		{iid: 0xffffffffffffffff, want: false},
		{iid: 1, want: false},
		{iid: 0x02005efffd000000, want: false},
		{iid: 0x02005effff000000, want: false},
		{iid: 0xfdffffffffffff7f, want: false},
		{iid: 0x0002b8fffe966195, want: false},
	}
	for _, tt := range tests {
		if got := reservedInterfaceID(tt.iid); got != tt.want {
			t.Errorf("reservedInterfaceID(%#016x) = %v, want %v", tt.iid, got, tt.want)
		}
	}
}

// A default route through a nexthop object carries neither a gateway nor an
// interface in the route itself; it is counted, not guessed at.
func TestIPv6DefaultRoutersUnreadable(t *testing.T) {
	routes := []netlink.Route{
		{Priority: 1024, Type: unix.RTN_UNICAST},
		{LinkIndex: 8, Gw: net.ParseIP("fe80::3"), Priority: 1024, Type: unix.RTN_UNICAST},
	}
	got, unreadable := ipv6DefaultRouters(routes, 7)
	if len(got) != 0 || unreadable != 1 {
		t.Errorf("ipv6DefaultRouters() = %v, %d unreadable, want none and 1", got, unreadable)
	}
}
