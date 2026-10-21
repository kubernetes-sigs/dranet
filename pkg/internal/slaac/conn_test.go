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

package slaac

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	userns "sigs.k8s.io/dranet/internal/testutils"
)

// testLink is a veth pair: the host end is solicited on, the router end
// answers.
type testLink struct {
	host, router *net.Interface
}

// newTestLink creates the veth pair in the test's network namespace. The
// host's IPv6 is left to the kernel defaults, except that the kernel sends no
// solicitations of its own: the router would answer those too, and hide a
// Solicit that does not retransmit.
func newTestLink(t *testing.T) testLink {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "host0"}, PeerName: "router0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("add veth: %v", err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/host0/router_solicitations", []byte("0"), 0o644); err != nil {
		t.Fatalf("turn off the kernel's solicitations on host0: %v", err)
	}
	for _, name := range []string{"host0", "router0"} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
	}
	host, err := net.InterfaceByName("host0")
	if err != nil {
		t.Fatal(err)
	}
	router, err := net.InterfaceByName("router0")
	if err != nil {
		t.Fatal(err)
	}
	return testLink{host: host, router: router}
}

// fakeRouter answers Router Solicitations on the router end of the link.
type fakeRouter struct {
	t    *testing.T
	conn *Conn
	addr netip.Addr

	mu       sync.Mutex
	received []*RouterSolicitation
	sources  []netip.Addr
}

func newFakeRouter(t *testing.T, ifi *net.Interface) *fakeRouter {
	t.Helper()
	conn, err := Listen(ifi, TypeRouterSolicitation)
	if err != nil {
		t.Fatalf("router listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &fakeRouter{t: t, conn: conn, addr: netip.MustParseAddr("fe80::1")}
}

// serve answers every solicitation with reply(solicitation number) until the
// connection is closed. A nil reply drops the solicitation.
func (r *fakeRouter) serve(reply func(n int) *RouterAdvertisement) {
	go func() {
		for n := 1; ; n++ {
			m, src, err := r.conn.ReadFrom()
			if err != nil {
				return
			}
			rs, ok := m.(*RouterSolicitation)
			if !ok {
				r.t.Errorf("router received %T, want *RouterSolicitation", m)
				continue
			}
			r.mu.Lock()
			r.received = append(r.received, rs)
			r.sources = append(r.sources, src)
			r.mu.Unlock()

			ra := reply(n)
			if ra == nil {
				continue
			}
			// Answer the way routers do: by multicast, unless the host gave
			// an address and a link-layer address to answer to directly
			// (RFC 4861 section 6.2.6).
			dst, to := AllNodes, net.HardwareAddr(nil)
			if !src.IsUnspecified() && rs.SourceLinkLayerAddress != nil {
				dst, to = src, rs.SourceLinkLayerAddress
			}
			if err := r.conn.WriteTo(ra, r.addr, dst, to); err != nil {
				r.t.Errorf("router send: %v", err)
			}
		}
	}()
}

func (r *fakeRouter) solicitations() ([]*RouterSolicitation, []netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*RouterSolicitation(nil), r.received...), append([]netip.Addr(nil), r.sources...)
}

func TestSolicit(t *testing.T) {
	userns.Run(t, testSolicit, syscall.CLONE_NEWNET)
}

// A solicitation from the unspecified address, as sent on an interface
// without a usable link-local address, carries no link-layer address and is
// answered by multicast.
func testSolicit(t *testing.T) {
	link := newTestLink(t)
	router := newFakeRouter(t, link.router)
	router.serve(func(int) *RouterAdvertisement { return testRA })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ra, from, err := Solicit(ctx, link.host, netip.IPv6Unspecified(), nil)
	if err != nil {
		t.Fatalf("Solicit: %v", err)
	}
	if from != router.addr {
		t.Errorf("advertisement from %v, want %v", from, router.addr)
	}
	if len(ra.Prefixes) != 1 || ra.Prefixes[0].Prefix != testRA.Prefixes[0].Prefix {
		t.Errorf("advertisement prefixes = %+v, want %+v", ra.Prefixes, testRA.Prefixes)
	}
	received, sources := router.solicitations()
	if len(received) != 1 {
		t.Fatalf("router received %d solicitations, want 1", len(received))
	}
	if !sources[0].IsUnspecified() {
		t.Errorf("solicitation from %v, want the unspecified address", sources[0])
	}
	if received[0].SourceLinkLayerAddress != nil {
		t.Errorf("solicitation from the unspecified address carried link-layer address %v", received[0].SourceLinkLayerAddress)
	}
}

func TestListenJoinsGroups(t *testing.T) {
	userns.Run(t, testListenJoinsGroups, syscall.CLONE_NEWNET)
}

// A NIC drops the multicast frames of groups nobody on it joined, and with
// IPv6 disabled the kernel joins none, so Listen joins the group of the
// messages it receives for as long as it is open.
func testListenJoinsGroups(t *testing.T) {
	link := newTestLink(t)
	// Disabled once the link is up, the kernel leaves all-nodes.
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/host0/disable_ipv6", []byte("1"), 0o644); err != nil {
		t.Fatalf("disable IPv6 on host0: %v", err)
	}
	for _, tc := range []struct {
		typ   uint8
		group string
	}{
		{TypeRouterAdvertisement, "333300000001"},
		{TypeRouterSolicitation, "333300000002"},
	} {
		if slices.Contains(multicastMemberships(t, "host0"), tc.group) {
			t.Fatalf("host0 is already a member of %s", tc.group)
		}
		c, err := Listen(link.host, tc.typ)
		if err != nil {
			t.Fatal(err)
		}
		if got := multicastMemberships(t, "host0"); !slices.Contains(got, tc.group) {
			t.Errorf("listening for type %d, host0 is a member of %v, want %s among them", tc.typ, got, tc.group)
		}
		c.Close()
		if got := multicastMemberships(t, "host0"); slices.Contains(got, tc.group) {
			t.Errorf("after Close, host0 is still a member of %s", tc.group)
		}
	}
}

// multicastMemberships returns the Ethernet multicast addresses ifName
// receives, in the hex form of /proc/net/dev_mcast.
func multicastMemberships(t *testing.T, ifName string) []string {
	t.Helper()
	b, err := os.ReadFile("/proc/net/dev_mcast")
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	for _, line := range strings.Split(string(b), "\n") {
		// index, name, users, global users, address.
		if f := strings.Fields(line); len(f) == 5 && f[1] == ifName {
			groups = append(groups, f[4])
		}
	}
	return groups
}

func TestListenDropsOtherLinks(t *testing.T) {
	userns.Run(t, testListenDropsOtherLinks, syscall.CLONE_NEWNET)
}

// Listen receives only the advertisements of its own link: not those of a
// VLAN on the interface, with or without a VLAN device on the host, nor those
// an IPVLAN child of the interface, such as one in another Pod, sends to the
// hardware address it shares with the interface.
func testListenDropsOtherLinks(t *testing.T) {
	link := newTestLink(t)
	addLink := func(l netlink.Link) *net.Interface {
		t.Helper()
		if err := netlink.LinkAdd(l); err != nil {
			t.Fatalf("add %s: %v", l.Attrs().Name, err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
		ifi, err := net.InterfaceByName(l.Attrs().Name)
		if err != nil {
			t.Fatal(err)
		}
		return ifi
	}
	routerVLAN := addLink(&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "router0.100", ParentIndex: link.router.Index}, VlanId: 100})

	host, err := Listen(link.host, TypeRouterAdvertisement)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	send := func(ifi *net.Interface, from netip.Addr, to net.HardwareAddr) {
		t.Helper()
		c, err := Listen(ifi, TypeRouterSolicitation)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.WriteTo(testRA, from, AllNodes, to); err != nil {
			t.Fatal(err)
		}
	}
	notReceived := func(what string) {
		t.Helper()
		if err := host.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if m, from, err := host.ReadFrom(); err == nil {
			t.Errorf("host received %T from %v, sent %s", m, from, what)
		}
	}

	// Without a VLAN device on the host, the frame is for another host.
	send(routerVLAN, netip.MustParseAddr("fe80::100"), nil)
	notReceived("on VLAN 100 without a VLAN device on the host")

	// With one, the frame is for the VLAN device.
	addLink(&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "host0.100", ParentIndex: link.host.Index}, VlanId: 100})
	send(routerVLAN, netip.MustParseAddr("fe80::100"), nil)
	notReceived("on VLAN 100 with a VLAN device on the host")

	// An IPVLAN child reaches its parent with a frame sent to its own source
	// address: the hardware address it shares with the parent, or a group
	// address it forges as the source.
	child := addLink(&netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: "child0", ParentIndex: link.host.Index}, Mode: netlink.IPVLAN_MODE_L2})
	send(child, netip.MustParseAddr("fe80::bad"), link.host.HardwareAddr)
	notReceived("by an IPVLAN child to the hardware address of its parent")
	raw, err := packet.Listen(child, packet.Raw, unix.ETH_P_IPV6, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	icmp, err := MarshalMessage(testRA)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []net.HardwareAddr{MulticastHardwareAddr(AllNodes), {0xff, 0xff, 0xff, 0xff, 0xff, 0xff}} {
		frame := append(append(append([]byte{}, group...), group...), 0x86, 0xdd)
		frame = append(frame, marshalPacket(netip.MustParseAddr("fe80::bad"), AllNodes, icmp)...)
		if _, err := raw.WriteTo(frame, &packet.Addr{HardwareAddr: group}); err != nil {
			t.Fatal(err)
		}
		notReceived("by an IPVLAN child from and to " + group.String())
	}

	// An advertisement of the link itself arrives.
	send(link.router, netip.MustParseAddr("fe80::1"), nil)
	if err := host.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, from, err := host.ReadFrom(); err != nil || from != netip.MustParseAddr("fe80::1") {
		t.Fatalf("host read = %v, %v; want the advertisement from fe80::1", from, err)
	}
}

func TestSolicitFromLinkLocal(t *testing.T) {
	userns.Run(t, testSolicitFromLinkLocal, syscall.CLONE_NEWNET)
}

// A solicitation from a link-local address carries the interface's hardware
// address, and the advertisement the router unicasts back to it is received.
func testSolicitFromLinkLocal(t *testing.T) {
	link := newTestLink(t)
	router := newFakeRouter(t, link.router)
	router.serve(func(int) *RouterAdvertisement { return testRA })

	src := netip.MustParseAddr("fe80::2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := Solicit(ctx, link.host, src, nil); err != nil {
		t.Fatalf("Solicit: %v", err)
	}
	received, sources := router.solicitations()
	if len(received) != 1 {
		t.Fatalf("router received %d solicitations, want 1", len(received))
	}
	if sources[0] != src {
		t.Errorf("solicitation from %v, want %v", sources[0], src)
	}
	if !bytes.Equal(received[0].SourceLinkLayerAddress, link.host.HardwareAddr) {
		t.Errorf("solicitation link-layer address = %v, want %v", received[0].SourceLinkLayerAddress, link.host.HardwareAddr)
	}
}

func TestSolicitRetransmits(t *testing.T) {
	userns.Run(t, testSolicitRetransmits, syscall.CLONE_NEWNET)
}

// A router that misses the first solicitation must still be heard from
// within the retransmission schedule.
func testSolicitRetransmits(t *testing.T) {
	link := newTestLink(t)
	router := newFakeRouter(t, link.router)
	router.serve(func(n int) *RouterAdvertisement {
		if n == 1 {
			return nil
		}
		return testRA
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := Solicit(ctx, link.host, netip.IPv6Unspecified(), nil); err != nil {
		t.Fatalf("Solicit after %v: %v", time.Since(start), err)
	}
	if elapsed := time.Since(start); elapsed < solicitInterval {
		t.Errorf("advertisement after %v, before the first retransmission at %v", elapsed, solicitInterval)
	}
	if received, _ := router.solicitations(); len(received) != 2 {
		t.Errorf("router received %d solicitations, want 2", len(received))
	}
}

func TestSolicitRejected(t *testing.T) {
	userns.Run(t, testSolicitRejected, syscall.CLONE_NEWNET)
}

// Advertisements the caller turns down are counted, and the error says who
// sent them, so a link whose routers answer with the wrong configuration is
// told apart from a silent one.
func testSolicitRejected(t *testing.T) {
	link := newTestLink(t)
	router := newFakeRouter(t, link.router)
	router.serve(func(int) *RouterAdvertisement { return &RouterAdvertisement{ManagedConfiguration: true} })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var seen int
	_, _, err := Solicit(ctx, link.host, netip.IPv6Unspecified(), func(ra *RouterAdvertisement, from netip.Addr) bool {
		seen++
		if !ra.ManagedConfiguration || from != router.addr {
			t.Errorf("accept got %+v from %v", ra, from)
		}
		return false
	})
	var serr *SolicitError
	if !errors.As(err, &serr) {
		t.Fatalf("Solicit error = %v, want *SolicitError", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Solicit error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if seen != 1 || serr.Rejected != 1 || !slices.Equal(serr.Routers, []netip.Addr{router.addr}) {
		t.Errorf("accept called %d times, rejected %d from %v, want 1 from [%v]", seen, serr.Rejected, serr.Routers, router.addr)
	}
	if serr.Sent != 1 {
		t.Errorf("Sent = %d, want 1 within %v", serr.Sent, 500*time.Millisecond)
	}
}

func TestSolicitWithoutRouter(t *testing.T) {
	userns.Run(t, testSolicitWithoutRouter, syscall.CLONE_NEWNET)
}

// Without a router the wait ends with the context, and a cancellation
// interrupts the read at once.
func testSolicitWithoutRouter(t *testing.T) {
	link := newTestLink(t)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := Solicit(ctx, link.host, netip.IPv6Unspecified(), nil)
	var serr *SolicitError
	if !errors.As(err, &serr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Solicit error = %v, want *SolicitError wrapping context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > solicitInterval {
		t.Errorf("Solicit returned after %v, the cancellation did not interrupt the read", elapsed)
	}
	if serr.Sent != 1 || serr.Rejected != 0 {
		t.Errorf("SolicitError = %+v, want 1 sent and nothing rejected", serr)
	}
}

// The router end must not hear its own advertisement as a solicitation, and
// the host must not receive solicitations: the filter keeps each side to its
// own message type.
func TestListenFiltersTypes(t *testing.T) {
	userns.Run(t, testListenFiltersTypes, syscall.CLONE_NEWNET)
}

func testListenFiltersTypes(t *testing.T) {
	link := newTestLink(t)
	host, err := Listen(link.host, TypeRouterAdvertisement)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	router, err := Listen(link.router, TypeRouterSolicitation)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	// A solicitation from the host and an advertisement from the router.
	if err := host.WriteTo(&RouterSolicitation{}, netip.IPv6Unspecified(), AllRouters, nil); err != nil {
		t.Fatal(err)
	}
	if err := router.WriteTo(testRA, netip.MustParseAddr("fe80::1"), AllNodes, nil); err != nil {
		t.Fatal(err)
	}

	if err := host.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	m, _, err := host.ReadFrom()
	if err != nil {
		t.Fatalf("host read: %v", err)
	}
	if _, ok := m.(*RouterAdvertisement); !ok {
		t.Errorf("host received %T, want *RouterAdvertisement", m)
	}
	if err := router.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	m, _, err = router.ReadFrom()
	if err != nil {
		t.Fatalf("router read: %v", err)
	}
	if _, ok := m.(*RouterSolicitation); !ok {
		t.Errorf("router received %T, want *RouterSolicitation", m)
	}

	// Nothing else is pending on either side.
	for name, c := range map[string]*Conn{"host": host, "router": router} {
		if err := c.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if m, _, err := c.ReadFrom(); err == nil {
			t.Errorf("%s received an unexpected %T", name, m)
		}
	}
}
