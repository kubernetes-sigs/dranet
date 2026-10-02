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

package driver

import (
	"context"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"

	userns "sigs.k8s.io/dranet/internal/testutils"
)

func TestGetDHCPRetransmitsDiscover(t *testing.T) {
	userns.Run(t, testGetDHCPRetransmitsDiscover, syscall.CLONE_NEWNET)
}

// A server that drops the first DISCOVER, as one doing a ping check on the
// candidate address does, must still get the client an address within the
// prepare timeout.
func testGetDHCPRetransmitsDiscover(t *testing.T) {
	serverIP := net.IPv4(192, 0, 2, 1)
	clientIP := net.IPv4(192, 0, 2, 10)
	mask := net.CIDRMask(24, 32)

	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "vethc"}, PeerName: "veths"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("add veth: %v", err)
	}
	for _, name := range []string{"vethc", "veths"} {
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", name, err)
		}
	}
	server, err := netlink.LinkByName("veths")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(server, &netlink.Addr{IPNet: &net.IPNet{IP: serverIP, Mask: mask}}); err != nil {
		t.Fatalf("add server address: %v", err)
	}

	var discovers atomic.Int32
	handler := func(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
		var msgType dhcpv4.MessageType
		switch m.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			if discovers.Add(1) == 1 {
				return
			}
			msgType = dhcpv4.MessageTypeOffer
		case dhcpv4.MessageTypeRequest:
			msgType = dhcpv4.MessageTypeAck
		default:
			return
		}
		reply, err := dhcpv4.NewReplyFromRequest(m,
			dhcpv4.WithMessageType(msgType),
			dhcpv4.WithYourIP(clientIP),
			dhcpv4.WithServerIP(serverIP),
			dhcpv4.WithNetmask(mask),
			dhcpv4.WithLeaseTime(3600),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)),
		)
		if err != nil {
			t.Errorf("build reply: %v", err)
			return
		}
		if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
			t.Errorf("send reply: %v", err)
		}
	}
	srv, err := server4.NewServer("veths", &net.UDPAddr{Port: dhcpv4.ServerPort}, handler)
	if err != nil {
		t.Fatalf("start DHCP server: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	ip, _, lease, err := getDHCP(ctx, "vethc")
	if err != nil {
		t.Fatalf("getDHCP after %v: %v", time.Since(start), err)
	}
	if want := "192.0.2.10/24"; ip != want {
		t.Errorf("ip = %s, want %s", ip, want)
	}
	if lease == nil || lease.ServerID != serverIP.String() {
		t.Errorf("lease = %+v, want server %s", lease, serverIP)
	}
	if n := discovers.Load(); n < 2 {
		t.Errorf("server saw %d DISCOVER, want at least 2", n)
	}
}
