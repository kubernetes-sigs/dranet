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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

const (
	// ipv6HeaderLen is the length of the fixed IPv6 header (RFC 8200 section 3).
	ipv6HeaderLen = 40
	// ndpHopLimit is the hop limit every Neighbor Discovery message carries;
	// a receiver drops anything else, as it proves the message did not come
	// from the link (RFC 4861 sections 6.1.1 and 6.1.2).
	ndpHopLimit = 255
)

var (
	// AllRouters and AllNodes are the link-local multicast groups a
	// solicitation and an advertisement are sent to (RFC 4291 section 2.7.1).
	AllRouters = netip.MustParseAddr("ff02::2")
	AllNodes   = netip.MustParseAddr("ff02::1")
)

var errInvalidPacket = errors.New("slaac: invalid packet")

// MulticastHardwareAddr maps an IPv6 multicast address to the Ethernet
// multicast address that carries it: 33:33 followed by the low 32 bits of the
// group (RFC 2464 section 7).
func MulticastHardwareAddr(group netip.Addr) net.HardwareAddr {
	a := group.As16()
	return net.HardwareAddr{0x33, 0x33, a[12], a[13], a[14], a[15]}
}

// marshalPacket builds the IPv6 packet that carries icmp from src to dst, with
// the hop limit Neighbor Discovery requires and the ICMPv6 checksum filled in.
func marshalPacket(src, dst netip.Addr, icmp []byte) []byte {
	b := make([]byte, ipv6HeaderLen+len(icmp))
	b[0] = 6 << 4
	binary.BigEndian.PutUint16(b[4:6], uint16(len(icmp)))
	b[6] = unix.IPPROTO_ICMPV6
	b[7] = ndpHopLimit
	s, d := src.As16(), dst.As16()
	copy(b[8:24], s[:])
	copy(b[24:40], d[:])
	payload := b[ipv6HeaderLen:]
	copy(payload, icmp)
	payload[2], payload[3] = 0, 0
	binary.BigEndian.PutUint16(payload[2:4], checksum(src, dst, payload))
	return b
}

// parsePacket validates an IPv6 packet as a Neighbor Discovery message and
// returns the message with its source and destination. It enforces what
// RFC 4861 sections 6.1.1 and 6.1.2 require of the IP layer: hop limit 255, a
// correct ICMPv6 checksum, a link-local source for an advertisement, and no
// source link-layer address in a solicitation from the unspecified address.
// Extension headers are not supported: Neighbor Discovery messages carry none.
func parsePacket(b []byte) (msg Message, src, dst netip.Addr, err error) {
	if len(b) < ipv6HeaderLen {
		return nil, src, dst, fmt.Errorf("%w: %d bytes, shorter than the IPv6 header", errInvalidPacket, len(b))
	}
	if b[0]>>4 != 6 {
		return nil, src, dst, fmt.Errorf("%w: IP version %d", errInvalidPacket, b[0]>>4)
	}
	if b[6] != unix.IPPROTO_ICMPV6 {
		return nil, src, dst, fmt.Errorf("%w: next header %d is not ICMPv6", errInvalidPacket, b[6])
	}
	if b[7] != ndpHopLimit {
		return nil, src, dst, fmt.Errorf("%w: hop limit %d, want %d", errInvalidPacket, b[7], ndpHopLimit)
	}
	// Short frames arrive padded to the Ethernet minimum; the payload length
	// tells where the message ends.
	payloadLen := int(binary.BigEndian.Uint16(b[4:6]))
	if payloadLen > len(b)-ipv6HeaderLen {
		return nil, src, dst, fmt.Errorf("%w: payload length %d exceeds the %d bytes received", errInvalidPacket, payloadLen, len(b)-ipv6HeaderLen)
	}
	src = netip.AddrFrom16([16]byte(b[8:24]))
	dst = netip.AddrFrom16([16]byte(b[24:40]))
	payload := b[ipv6HeaderLen : ipv6HeaderLen+payloadLen]
	// A checksum computed over a message that includes its own checksum
	// comes out as zero when the message is intact.
	if checksum(src, dst, payload) != 0 {
		return nil, src, dst, fmt.Errorf("%w: bad ICMPv6 checksum", errInvalidPacket)
	}
	msg, err = ParseMessage(payload)
	if err != nil {
		return nil, src, dst, err
	}
	switch m := msg.(type) {
	case *RouterAdvertisement:
		if !src.IsLinkLocalUnicast() {
			return nil, src, dst, fmt.Errorf("%w: router advertisement from %v, which is not link-local", errInvalidPacket, src)
		}
	case *RouterSolicitation:
		if src.IsUnspecified() && len(m.SourceLinkLayerAddress) != 0 {
			return nil, src, dst, fmt.Errorf("%w: router solicitation from the unspecified address carries a source link-layer address", errInvalidPacket)
		}
	}
	return msg, src, dst, nil
}

// checksum computes the ICMPv6 checksum of payload sent from src to dst: the
// one's complement of the one's complement sum of the IPv6 pseudo-header and
// the message (RFC 8200 section 8.1, RFC 4443 section 2.3).
func checksum(src, dst netip.Addr, payload []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	s, d := src.As16(), dst.As16()
	add(s[:])
	add(d[:])
	var pseudo [8]byte // upper-layer packet length, three zero bytes, next header
	binary.BigEndian.PutUint32(pseudo[0:4], uint32(len(payload)))
	pseudo[7] = unix.IPPROTO_ICMPV6
	add(pseudo[:])
	add(payload)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// filterProgram returns a classic BPF program for a datagram packet socket on
// the interface with index ifIndex, whose data starts at the IPv6 header, that
// passes only the frames the kernel's IPv6 stack would receive on that
// interface and that carry ICMPv6 messages of the given types. The filter runs
// in the kernel, so the traffic of a busy link never reaches user space;
// without it every IPv6 frame on the interface would be copied up while the
// socket is open.
func filterProgram(ifIndex int, types []uint8) []bpf.Instruction {
	return append(linkFilter(ifIndex), messageFilter(types)...)
}

// linkFilter drops the frames that do not belong to the link of the interface
// with index ifIndex. An advertisement carries nothing that ties it to the
// solicitation, so one from another link would configure the Pod for a link
// it is not on. A socket bound to an interface also receives:
//   - the frames of its VLAN devices, delivered to the VLAN device, which
//     another link's router sends;
//   - frames addressed to another host, which the kernel's IPv6 stack drops
//     too (ipv6_rcv): unicast frames for another hardware address on a
//     promiscuous interface, and frames of a VLAN the host has no device for,
//     whose tag the NIC stripped.
func linkFilter(ifIndex int) []bpf.Instruction {
	return []bpf.Instruction{
		bpf.LoadExtension{Num: bpf.ExtInterfaceIndex},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(ifIndex), SkipTrue: 1},
		bpf.RetConstant{Val: 0},
		bpf.LoadExtension{Num: bpf.ExtType},
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: unix.PACKET_OTHERHOST, SkipFalse: 1},
		bpf.RetConstant{Val: 0},
	}
}

// messageFilter returns the part of the filter that passes only ICMPv6
// messages of the given types with the Neighbor Discovery hop limit.
func messageFilter(types []uint8) []bpf.Instruction {
	// Layout, with n = len(types):
	//   0-2  version is 6, else drop
	//   3-4  next header is ICMPv6, else drop
	//   5-6  hop limit is 255, else drop
	//   7    load the ICMPv6 type
	//   8+i  type equals types[i]: accept
	//   8+n  drop
	//   9+n  accept
	n := len(types)
	drop, accept := 8+n, 9+n
	// skipTo returns how many instructions the one at index from skips to
	// land on the one at index to.
	skipTo := func(from, to int) uint8 { return uint8(to - from - 1) }
	prog := []bpf.Instruction{
		bpf.LoadAbsolute{Off: 0, Size: 1},
		bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xf0},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 0x60, SkipTrue: skipTo(2, drop)},
		bpf.LoadAbsolute{Off: 6, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: unix.IPPROTO_ICMPV6, SkipTrue: skipTo(4, drop)},
		bpf.LoadAbsolute{Off: 7, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: ndpHopLimit, SkipTrue: skipTo(6, drop)},
		bpf.LoadAbsolute{Off: ipv6HeaderLen, Size: 1},
	}
	for i, t := range types {
		prog = append(prog, bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(t), SkipTrue: skipTo(8+i, accept)})
	}
	return append(prog,
		bpf.RetConstant{Val: 0},
		bpf.RetConstant{Val: 0xffff},
	)
}
