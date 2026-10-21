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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/packet"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// readBufferLen bounds a received packet. Neighbor Discovery messages are a
// few hundred bytes at most; the largest IPv6 payload keeps a jumbo-frame link
// from truncating an unusually long advertisement.
const readBufferLen = 65535

// Conn sends and receives Neighbor Discovery messages on one interface over a
// packet socket. It does not use the kernel's IPv6 stack, so it needs neither
// an address nor IPv6 enabled on the interface, only the link to be up.
//
// A Conn is not safe for concurrent use.
type Conn struct {
	ifi *net.Interface
	pc  *packet.Conn
	buf []byte
}

// Listen opens a Conn on ifi that receives the given ICMPv6 message types and
// nothing else: a BPF filter drops the rest in the kernel. The Conn joins the
// Ethernet multicast groups the messages are sent to (all-nodes for
// advertisements, all-routers for solicitations), so they are received even
// when the kernel's IPv6 stack is disabled on the interface and the NIC would
// otherwise filter them out. Sending needs no group.
func Listen(ifi *net.Interface, types ...uint8) (*Conn, error) {
	if ifi == nil {
		return nil, errors.New("slaac: nil interface")
	}
	if len(types) == 0 {
		return nil, errors.New("slaac: no message types to receive")
	}
	filter, err := bpf.Assemble(filterProgram(ifi.Index, types))
	if err != nil {
		return nil, fmt.Errorf("slaac: assemble filter: %w", err)
	}
	pc, err := packet.Listen(ifi, packet.Datagram, unix.ETH_P_IPV6, &packet.Config{Filter: filter})
	if err != nil {
		return nil, fmt.Errorf("slaac: open packet socket on %s: %w", ifi.Name, err)
	}
	c := &Conn{ifi: ifi, pc: pc, buf: make([]byte, readBufferLen)}
	for _, t := range types {
		var group netip.Addr
		switch t {
		case TypeRouterAdvertisement:
			group = AllNodes
		case TypeRouterSolicitation:
			group = AllRouters
		default:
			continue
		}
		if err := c.joinGroup(MulticastHardwareAddr(group)); err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("slaac: join %v on %s: %w", group, ifi.Name, err)
		}
	}
	return c, nil
}

// joinGroup adds the Ethernet multicast address hw to the interface for the
// lifetime of the socket; the kernel drops the membership when the socket
// closes.
func (c *Conn) joinGroup(hw net.HardwareAddr) error {
	rc, err := c.pc.SyscallConn()
	if err != nil {
		return err
	}
	mreq := unix.PacketMreq{
		Ifindex: int32(c.ifi.Index),
		Type:    unix.PACKET_MR_MULTICAST,
		Alen:    uint16(len(hw)),
	}
	copy(mreq.Address[:], hw)
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptPacketMreq(int(fd), unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq)
	}); err != nil {
		return err
	}
	return serr
}

// Close closes the packet socket.
func (c *Conn) Close() error {
	return c.pc.Close()
}

// SetReadDeadline bounds the next ReadFrom; a zero time removes the bound.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.pc.SetReadDeadline(t)
}

// WriteTo sends m from src to dst in a frame addressed to the hardware
// address to. src may be the unspecified address, as a solicitation from a
// host without an address is (RFC 4861 section 6.3.7). A nil to is allowed
// for a multicast dst, whose Ethernet address follows from the group.
func (c *Conn) WriteTo(m Message, src, dst netip.Addr, to net.HardwareAddr) error {
	if !src.Is6() || src.Is4In6() || !dst.Is6() || dst.Is4In6() {
		return fmt.Errorf("slaac: %v -> %v is not an IPv6 pair", src, dst)
	}
	if to == nil {
		if !dst.IsMulticast() {
			return fmt.Errorf("slaac: a hardware address is needed to send to the unicast address %v", dst)
		}
		to = MulticastHardwareAddr(dst)
	}
	icmp, err := MarshalMessage(m)
	if err != nil {
		return err
	}
	if _, err := c.pc.WriteTo(marshalPacket(src, dst, icmp), &packet.Addr{HardwareAddr: to}); err != nil {
		return fmt.Errorf("slaac: send %T on %s: %w", m, c.ifi.Name, err)
	}
	return nil
}

// ReadFrom returns the next valid message with the address it was sent from.
// Packets that fail the checks of RFC 4861 section 6.1 are dropped silently,
// as the RFC prescribes, and the read continues. An expired read deadline
// surfaces as an error wrapping os.ErrDeadlineExceeded.
func (c *Conn) ReadFrom() (Message, netip.Addr, error) {
	for {
		n, from, err := c.pc.ReadFrom(c.buf)
		if err != nil {
			return nil, netip.Addr{}, err
		}
		// An IPVLAN child of the interface, such as one in another Pod, can
		// hand the interface a frame by sending it to its own source address:
		// the hardware address it shares with the interface, or a group
		// address. No router on the link sends from either.
		if a, ok := from.(*packet.Addr); ok && len(a.HardwareAddr) > 0 &&
			(a.HardwareAddr[0]&0x01 != 0 || bytes.Equal(a.HardwareAddr, c.ifi.HardwareAddr)) {
			continue
		}
		m, src, _, err := parsePacket(c.buf[:n])
		if err != nil {
			continue
		}
		return m, src, nil
	}
}
