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
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/bpf"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

var (
	testRouter = netip.MustParseAddr("fe80::1")
	testHost   = netip.MustParseAddr("fe80::211:22ff:fe33:4455")
)

func mustPacket(t *testing.T, src, dst netip.Addr, m Message) []byte {
	t.Helper()
	icmp, err := MarshalMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	return marshalPacket(src, dst, icmp)
}

// The checksum must agree with an independent implementation: x/net/icmp
// computes it from the same pseudo-header when given one.
func TestChecksum(t *testing.T) {
	for name, tc := range map[string]struct {
		src, dst netip.Addr
		m        Message
	}{
		"solicitation from the unspecified address": {netip.IPv6Unspecified(), AllRouters, &RouterSolicitation{}},
		"solicitation with an option":               {testHost, AllRouters, &RouterSolicitation{SourceLinkLayerAddress: testMAC}},
		"advertisement":                             {testRouter, AllNodes, testRA},
	} {
		t.Run(name, func(t *testing.T) {
			pkt := mustPacket(t, tc.src, tc.dst, tc.m)
			payload := pkt[ipv6HeaderLen:]

			want, err := (&icmp.Message{
				Type: ipv6.ICMPType(tc.m.Type()),
				Body: &icmp.RawBody{Data: payload[icmpHeaderLen:]},
			}).Marshal(icmp.IPv6PseudoHeader(tc.src.AsSlice(), tc.dst.AsSlice()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, want) {
				t.Errorf("checksummed message = % x\nwant % x", payload, want)
			}
			// A message with a correct checksum sums to zero when verified.
			if got := checksum(tc.src, tc.dst, payload); got != 0 {
				t.Errorf("checksum over a checksummed message = %#x, want 0", got)
			}
		})
	}
}

func TestParsePacket(t *testing.T) {
	pkt := mustPacket(t, testRouter, AllNodes, testRA)
	// Short frames arrive padded to the Ethernet minimum.
	padded := append(append([]byte(nil), pkt...), make([]byte, 20)...)

	m, src, dst, err := parsePacket(padded)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*RouterAdvertisement); !ok {
		t.Errorf("message is %T, want *RouterAdvertisement", m)
	}
	if src != testRouter || dst != AllNodes {
		t.Errorf("addresses = %v -> %v, want %v -> %v", src, dst, testRouter, AllNodes)
	}
}

// Everything RFC 4861 section 6.1 tells a receiver to discard must be
// rejected, so a forged or forwarded message never configures a Pod.
func TestParsePacketInvalid(t *testing.T) {
	corrupt := func(mutate func(pkt []byte)) []byte {
		pkt := mustPacket(t, testRouter, AllNodes, testRA)
		mutate(pkt)
		return pkt
	}
	for name, pkt := range map[string][]byte{
		"shorter than the IPv6 header": make([]byte, ipv6HeaderLen-1),
		"IPv4":                         corrupt(func(p []byte) { p[0] = 4 << 4 }),
		"not ICMPv6":                   corrupt(func(p []byte) { p[6] = unix.IPPROTO_UDP }),
		"hop limit below 255":          corrupt(func(p []byte) { p[7] = 64 }),
		"payload longer than the frame": corrupt(func(p []byte) {
			binary.BigEndian.PutUint16(p[4:6], uint16(len(p)-ipv6HeaderLen+1))
		}),
		"corrupted body":                      corrupt(func(p []byte) { p[len(p)-1] ^= 0xff }),
		"corrupted checksum":                  corrupt(func(p []byte) { p[ipv6HeaderLen+2] ^= 0x01 }),
		"advertisement from a global address": mustPacket(t, netip.MustParseAddr("2001:db8::1"), AllNodes, testRA),
		"solicitation from the unspecified address with a link-layer address": mustPacket(t,
			netip.IPv6Unspecified(), AllRouters, &RouterSolicitation{SourceLinkLayerAddress: testMAC}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parsePacket(pkt); err == nil {
				t.Error("parsePacket accepted an invalid packet")
			} else if !errors.Is(err, errInvalidPacket) && !errors.Is(err, errMessageTooShort) {
				t.Logf("parsePacket error: %v", err)
			}
		})
	}
}

// The filter decides in the kernel what reaches the socket, so it must accept
// exactly the requested message types and nothing that merely looks alike.
func TestMessageFilter(t *testing.T) {
	udp := mustPacket(t, testRouter, AllNodes, testRA)
	udp[6] = unix.IPPROTO_UDP
	lowHopLimit := mustPacket(t, testRouter, AllNodes, testRA)
	lowHopLimit[7] = 64
	ipv4 := mustPacket(t, testRouter, AllNodes, testRA)
	ipv4[0] = 4 << 4

	for name, tc := range map[string]struct {
		types  []uint8
		pkt    []byte
		accept bool
	}{
		"advertisement wanted":          {[]uint8{TypeRouterAdvertisement}, mustPacket(t, testRouter, AllNodes, testRA), true},
		"solicitation not wanted":       {[]uint8{TypeRouterAdvertisement}, mustPacket(t, testHost, AllRouters, &RouterSolicitation{}), false},
		"solicitation wanted":           {[]uint8{TypeRouterSolicitation}, mustPacket(t, testHost, AllRouters, &RouterSolicitation{}), true},
		"second of two types":           {[]uint8{TypeRouterSolicitation, TypeRouterAdvertisement}, mustPacket(t, testRouter, AllNodes, testRA), true},
		"first of two types":            {[]uint8{TypeRouterSolicitation, TypeRouterAdvertisement}, mustPacket(t, testHost, AllRouters, &RouterSolicitation{}), true},
		"neither of two types":          {[]uint8{TypeRouterSolicitation, TypeRouterAdvertisement}, mustPacket(t, testHost, AllRouters, &echoRequest{}), false},
		"not ICMPv6":                    {[]uint8{TypeRouterAdvertisement}, udp, false},
		"hop limit below 255":           {[]uint8{TypeRouterAdvertisement}, lowHopLimit, false},
		"not IPv6":                      {[]uint8{TypeRouterAdvertisement}, ipv4, false},
		"shorter than the IPv6 header":  {[]uint8{TypeRouterAdvertisement}, make([]byte, 20), false},
		"IPv6 header without a payload": {[]uint8{TypeRouterAdvertisement}, mustPacket(t, testRouter, AllNodes, testRA)[:ipv6HeaderLen], false},
	} {
		t.Run(name, func(t *testing.T) {
			// The VM does not emulate the interface index and packet type
			// extensions, so linkFilter is tested on real links, see
			// TestListenDropsOtherLinks.
			vm, err := bpf.NewVM(messageFilter(tc.types))
			if err != nil {
				t.Fatal(err)
			}
			n, err := vm.Run(tc.pkt)
			if err != nil {
				t.Fatal(err)
			}
			if got := n > 0; got != tc.accept {
				t.Errorf("filter accepted = %v (returned %d), want %v", got, n, tc.accept)
			}
		})
	}
}

// echoRequest is an ICMPv6 message of a type the filter never passes.
type echoRequest struct{}

func (*echoRequest) Type() uint8                  { return 128 }
func (*echoRequest) marshalBody() ([]byte, error) { return make([]byte, 4), nil }

func TestMulticastHardwareAddr(t *testing.T) {
	for group, want := range map[string]net.HardwareAddr{
		"ff02::1":           {0x33, 0x33, 0x00, 0x00, 0x00, 0x01},
		"ff02::2":           {0x33, 0x33, 0x00, 0x00, 0x00, 0x02},
		"ff02::1:ff33:4455": {0x33, 0x33, 0xff, 0x33, 0x44, 0x55},
	} {
		if got := MulticastHardwareAddr(netip.MustParseAddr(group)); !bytes.Equal(got, want) {
			t.Errorf("MulticastHardwareAddr(%s) = %v, want %v", group, got, want)
		}
	}
}
