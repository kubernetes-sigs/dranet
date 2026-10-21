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
	"math"
	"net"
	"net/netip"
	"time"
)

// ICMPv6 message types of Neighbor Discovery (RFC 4861 section 4).
const (
	TypeRouterSolicitation  uint8 = 133
	TypeRouterAdvertisement uint8 = 134
)

// Neighbor Discovery option types (RFC 4861 section 4.6, RFC 4191 section 2.3).
const (
	optTypeSourceLinkLayerAddress uint8 = 1
	optTypePrefixInformation      uint8 = 3
	optTypeMTU                    uint8 = 5
	optTypeRouteInformation       uint8 = 24
)

// Infinity is the lifetime that never expires: all ones in the 32-bit
// lifetime fields of the options (RFC 4861 section 4.6.2).
const Infinity = time.Duration(math.MaxUint32) * time.Second

const (
	// icmpHeaderLen is the length of the ICMPv6 header: type, code and
	// checksum (RFC 4443 section 2.1).
	icmpHeaderLen = 4
	// optUnit is the unit of the length field of an option.
	optUnit = 8
	// raBodyLen is the fixed part of a Router Advertisement after the ICMPv6
	// header, rsBodyLen the one of a Router Solicitation.
	raBodyLen = 12
	rsBodyLen = 4
)

var (
	// errMessageTooShort rejects a message shorter than its fixed part.
	errMessageTooShort = errors.New("slaac: message too short")
	// errOptionLength rejects an option whose length field is zero or runs
	// past the end of the message (RFC 4861 sections 6.1.1 and 6.1.2).
	errOptionLength = errors.New("slaac: invalid option length")
)

// Message is a Neighbor Discovery message.
type Message interface {
	// Type returns the ICMPv6 type of the message.
	Type() uint8
	// marshalBody encodes the message after the ICMPv6 header.
	marshalBody() ([]byte, error)
}

// MarshalMessage encodes m as an ICMPv6 message. The checksum field is left
// zero: it depends on the IPv6 addresses the message is sent with, so the
// sender fills it in (RFC 4443 section 2.3).
func MarshalMessage(m Message) ([]byte, error) {
	body, err := m.marshalBody()
	if err != nil {
		return nil, err
	}
	b := make([]byte, icmpHeaderLen+len(body))
	b[0] = m.Type()
	copy(b[icmpHeaderLen:], body)
	return b, nil
}

// ParseMessage decodes an ICMPv6 message into a Router Solicitation or a
// Router Advertisement. It applies the checks of RFC 4861 sections 6.1.1 and
// 6.1.2 that depend on the message alone: code zero, minimum length and
// well-formed options. The checksum and the IPv6 header are the sender's and
// receiver's business, see Conn.
func ParseMessage(b []byte) (Message, error) {
	if len(b) < icmpHeaderLen {
		return nil, fmt.Errorf("%w: %d bytes, want at least %d", errMessageTooShort, len(b), icmpHeaderLen)
	}
	if b[1] != 0 {
		return nil, fmt.Errorf("slaac: ICMPv6 code %d, want 0", b[1])
	}
	switch b[0] {
	case TypeRouterSolicitation:
		return parseRouterSolicitation(b[icmpHeaderLen:])
	case TypeRouterAdvertisement:
		return parseRouterAdvertisement(b[icmpHeaderLen:])
	default:
		return nil, fmt.Errorf("slaac: unsupported ICMPv6 type %d", b[0])
	}
}

// RouterSolicitation asks the routers on the link for an immediate Router
// Advertisement (RFC 4861 section 4.1).
type RouterSolicitation struct {
	// SourceLinkLayerAddress is the sender's hardware address. It must be
	// empty when the solicitation is sent from the unspecified address
	// (RFC 4861 section 4.1).
	SourceLinkLayerAddress net.HardwareAddr
}

// Type implements Message.
func (*RouterSolicitation) Type() uint8 { return TypeRouterSolicitation }

func (rs *RouterSolicitation) marshalBody() ([]byte, error) {
	b := make([]byte, rsBodyLen) // Reserved
	return appendLinkLayerAddressOption(b, optTypeSourceLinkLayerAddress, rs.SourceLinkLayerAddress), nil
}

func parseRouterSolicitation(b []byte) (*RouterSolicitation, error) {
	if len(b) < rsBodyLen {
		return nil, fmt.Errorf("%w: router solicitation body of %d bytes, want at least %d", errMessageTooShort, len(b), rsBodyLen)
	}
	opts, err := parseOptions(b[rsBodyLen:])
	if err != nil {
		return nil, err
	}
	rs := &RouterSolicitation{}
	for _, o := range opts {
		if o.typ == optTypeSourceLinkLayerAddress {
			rs.SourceLinkLayerAddress = parseLinkLayerAddress(o.body)
		}
	}
	return rs, nil
}

// Preference is the preference of a router or a route (RFC 4191 section 2.1).
type Preference int8

// Preference values, ordered so that a higher value is preferred.
const (
	Low    Preference = -1
	Medium Preference = 0
	High   Preference = 1
)

// prefReserved is the two-bit encoding RFC 4191 reserves. A receiver treats
// it as Medium in a Router Advertisement and ignores a Route Information
// option carrying it (sections 2.2 and 2.3).
const prefReserved = 0b10

func (p Preference) encode() (uint8, error) {
	switch p {
	case High:
		return 0b01, nil
	case Medium:
		return 0b00, nil
	case Low:
		return 0b11, nil
	default:
		return 0, fmt.Errorf("slaac: invalid preference %d", p)
	}
}

func decodePreference(bits uint8) Preference {
	switch bits & 0b11 {
	case 0b01:
		return High
	case 0b11:
		return Low
	default:
		return Medium
	}
}

// RouterAdvertisement is what a router tells the hosts on its link: whether
// it is a default router, which prefixes are on-link or usable for address
// autoconfiguration, and which other parameters to use (RFC 4861 section 4.2).
type RouterAdvertisement struct {
	// CurrentHopLimit is the hop limit hosts should use, zero if unspecified.
	CurrentHopLimit uint8
	// ManagedConfiguration is the M flag: addresses are available through
	// DHCPv6.
	ManagedConfiguration bool
	// OtherConfiguration is the O flag: other configuration, such as DNS,
	// is available through DHCPv6.
	OtherConfiguration bool
	// RouterPreference ranks the router among the default routers (RFC 4191).
	RouterPreference Preference
	// RouterLifetime is how long the router may be used as a default router.
	// Zero means it is not a default router, though its prefixes still apply.
	RouterLifetime time.Duration
	// ReachableTime and RetransmitTimer are the Neighbor Unreachability
	// Detection parameters, zero if unspecified.
	ReachableTime   time.Duration
	RetransmitTimer time.Duration
	// SourceLinkLayerAddress is the router's hardware address, if included.
	SourceLinkLayerAddress net.HardwareAddr
	// MTU is the link MTU the router recommends, zero if not included.
	MTU uint32
	// Prefixes are the Prefix Information options, Routes the Route
	// Information options, in message order.
	Prefixes []PrefixInformation
	Routes   []RouteInformation
}

// Type implements Message.
func (*RouterAdvertisement) Type() uint8 { return TypeRouterAdvertisement }

// PrefixInformation is a Prefix Information option (RFC 4861 section 4.6.2).
type PrefixInformation struct {
	// Prefix is the advertised prefix. Only the first Prefix.Bits() bits are
	// significant on the wire.
	Prefix netip.Prefix
	// OnLink is the L flag: addresses in the prefix are reachable directly on
	// the link.
	OnLink bool
	// Autonomous is the A flag: the prefix can be used for stateless address
	// autoconfiguration.
	Autonomous bool
	// ValidLifetime and PreferredLifetime bound how long an address formed
	// from the prefix stays valid and preferred. Infinity never expires.
	ValidLifetime     time.Duration
	PreferredLifetime time.Duration
}

// RouteInformation is a Route Information option (RFC 4191 section 2.3): a
// prefix reachable through the advertising router.
type RouteInformation struct {
	Prefix     netip.Prefix
	Preference Preference
	// Lifetime is how long the route may be used, Infinity never expires.
	Lifetime time.Duration
}

// Flag bits of the Router Advertisement (RFC 4861 section 4.2, RFC 4191
// section 2.2) and of the Prefix Information option (RFC 4861 section 4.6.2).
const (
	raFlagManaged = 0x80
	raFlagOther   = 0x40
	raPrefShift   = 3
	raPrefMask    = 0b11 << raPrefShift

	pioFlagOnLink     = 0x80
	pioFlagAutonomous = 0x40
	// rioPrefShift places the preference bits of a Route Information option.
	rioPrefShift = 3
)

func (ra *RouterAdvertisement) marshalBody() ([]byte, error) {
	lifetime, err := secondsUint16(ra.RouterLifetime)
	if err != nil {
		return nil, fmt.Errorf("slaac: router lifetime: %w", err)
	}
	reachable, err := millisUint32(ra.ReachableTime)
	if err != nil {
		return nil, fmt.Errorf("slaac: reachable time: %w", err)
	}
	retrans, err := millisUint32(ra.RetransmitTimer)
	if err != nil {
		return nil, fmt.Errorf("slaac: retransmit timer: %w", err)
	}
	pref, err := ra.RouterPreference.encode()
	if err != nil {
		return nil, err
	}

	b := make([]byte, raBodyLen)
	b[0] = ra.CurrentHopLimit
	if ra.ManagedConfiguration {
		b[1] |= raFlagManaged
	}
	if ra.OtherConfiguration {
		b[1] |= raFlagOther
	}
	b[1] |= pref << raPrefShift
	binary.BigEndian.PutUint16(b[2:4], lifetime)
	binary.BigEndian.PutUint32(b[4:8], reachable)
	binary.BigEndian.PutUint32(b[8:12], retrans)

	b = appendLinkLayerAddressOption(b, optTypeSourceLinkLayerAddress, ra.SourceLinkLayerAddress)
	if ra.MTU != 0 {
		b = append(b, optTypeMTU, 1, 0, 0)
		b = binary.BigEndian.AppendUint32(b, ra.MTU)
	}
	for _, p := range ra.Prefixes {
		if b, err = appendPrefixInformation(b, p); err != nil {
			return nil, err
		}
	}
	for _, r := range ra.Routes {
		if b, err = appendRouteInformation(b, r); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func appendPrefixInformation(b []byte, p PrefixInformation) ([]byte, error) {
	if !p.Prefix.IsValid() || !p.Prefix.Addr().Is6() || p.Prefix.Addr().Is4In6() {
		return nil, fmt.Errorf("slaac: prefix information option needs an IPv6 prefix, got %v", p.Prefix)
	}
	valid, err := secondsUint32(p.ValidLifetime)
	if err != nil {
		return nil, fmt.Errorf("slaac: prefix %v valid lifetime: %w", p.Prefix, err)
	}
	preferred, err := secondsUint32(p.PreferredLifetime)
	if err != nil {
		return nil, fmt.Errorf("slaac: prefix %v preferred lifetime: %w", p.Prefix, err)
	}
	var flags uint8
	if p.OnLink {
		flags |= pioFlagOnLink
	}
	if p.Autonomous {
		flags |= pioFlagAutonomous
	}
	b = append(b, optTypePrefixInformation, 4, uint8(p.Prefix.Bits()), flags)
	b = binary.BigEndian.AppendUint32(b, valid)
	b = binary.BigEndian.AppendUint32(b, preferred)
	b = append(b, 0, 0, 0, 0) // Reserved2
	addr := p.Prefix.Masked().Addr().As16()
	return append(b, addr[:]...), nil
}

func appendRouteInformation(b []byte, r RouteInformation) ([]byte, error) {
	if !r.Prefix.IsValid() || !r.Prefix.Addr().Is6() || r.Prefix.Addr().Is4In6() {
		return nil, fmt.Errorf("slaac: route information option needs an IPv6 prefix, got %v", r.Prefix)
	}
	lifetime, err := secondsUint32(r.Lifetime)
	if err != nil {
		return nil, fmt.Errorf("slaac: route %v lifetime: %w", r.Prefix, err)
	}
	pref, err := r.Preference.encode()
	if err != nil {
		return nil, err
	}
	// The option carries only as many prefix bytes as the prefix length
	// needs, rounded up to 8-byte units (RFC 4191 section 2.3).
	prefixBytes := 0
	switch bits := r.Prefix.Bits(); {
	case bits == 0:
	case bits <= 64:
		prefixBytes = 8
	default:
		prefixBytes = 16
	}
	b = append(b, optTypeRouteInformation, uint8(1+prefixBytes/optUnit), uint8(r.Prefix.Bits()), pref<<rioPrefShift)
	b = binary.BigEndian.AppendUint32(b, lifetime)
	addr := r.Prefix.Masked().Addr().As16()
	return append(b, addr[:prefixBytes]...), nil
}

func parseRouterAdvertisement(b []byte) (*RouterAdvertisement, error) {
	if len(b) < raBodyLen {
		return nil, fmt.Errorf("%w: router advertisement body of %d bytes, want at least %d", errMessageTooShort, len(b), raBodyLen)
	}
	opts, err := parseOptions(b[raBodyLen:])
	if err != nil {
		return nil, err
	}
	ra := &RouterAdvertisement{
		CurrentHopLimit:      b[0],
		ManagedConfiguration: b[1]&raFlagManaged != 0,
		OtherConfiguration:   b[1]&raFlagOther != 0,
		RouterPreference:     decodePreference(b[1] >> raPrefShift),
		RouterLifetime:       time.Duration(binary.BigEndian.Uint16(b[2:4])) * time.Second,
		ReachableTime:        time.Duration(binary.BigEndian.Uint32(b[4:8])) * time.Millisecond,
		RetransmitTimer:      time.Duration(binary.BigEndian.Uint32(b[8:12])) * time.Millisecond,
	}
	for _, o := range opts {
		switch o.typ {
		case optTypeSourceLinkLayerAddress:
			ra.SourceLinkLayerAddress = parseLinkLayerAddress(o.body)
		case optTypeMTU:
			// Reserved(2) MTU(4).
			if len(o.body) == 6 {
				ra.MTU = binary.BigEndian.Uint32(o.body[2:6])
			}
		case optTypePrefixInformation:
			if p, ok := parsePrefixInformation(o.body); ok {
				ra.Prefixes = append(ra.Prefixes, p)
			}
		case optTypeRouteInformation:
			if r, ok := parseRouteInformation(o.body); ok {
				ra.Routes = append(ra.Routes, r)
			}
		}
		// Unknown options are skipped (RFC 4861 section 4.6).
	}
	return ra, nil
}

// parsePrefixInformation decodes the body of a Prefix Information option. A
// malformed option is ignored rather than failing the whole advertisement,
// as the kernel does (RFC 4861 section 6.3.4 only invalidates the option).
func parsePrefixInformation(body []byte) (PrefixInformation, bool) {
	// PrefixLength(1) Flags(1) Valid(4) Preferred(4) Reserved2(4) Prefix(16).
	if len(body) != 30 {
		return PrefixInformation{}, false
	}
	bits := int(body[0])
	if bits > 128 {
		return PrefixInformation{}, false
	}
	addr := netip.AddrFrom16([16]byte(body[14:30]))
	return PrefixInformation{
		Prefix:            netip.PrefixFrom(addr, bits).Masked(),
		OnLink:            body[1]&pioFlagOnLink != 0,
		Autonomous:        body[1]&pioFlagAutonomous != 0,
		ValidLifetime:     time.Duration(binary.BigEndian.Uint32(body[2:6])) * time.Second,
		PreferredLifetime: time.Duration(binary.BigEndian.Uint32(body[6:10])) * time.Second,
	}, true
}

// parseRouteInformation decodes the body of a Route Information option. An
// option with a prefix length the option is too short for, or with the
// reserved preference, is ignored (RFC 4191 section 3.1).
func parseRouteInformation(body []byte) (RouteInformation, bool) {
	// PrefixLength(1) Resvd|Prf|Resvd(1) Lifetime(4) Prefix(0, 8 or 16).
	if len(body) < 6 {
		return RouteInformation{}, false
	}
	bits := int(body[0])
	prefixBytes := len(body) - 6
	if bits > 128 || bits > prefixBytes*8 || prefixBytes > 16 {
		return RouteInformation{}, false
	}
	if (body[1]>>rioPrefShift)&0b11 == prefReserved {
		return RouteInformation{}, false
	}
	var raw [16]byte
	copy(raw[:], body[6:])
	return RouteInformation{
		Prefix:     netip.PrefixFrom(netip.AddrFrom16(raw), bits).Masked(),
		Preference: decodePreference(body[1] >> rioPrefShift),
		Lifetime:   time.Duration(binary.BigEndian.Uint32(body[2:6])) * time.Second,
	}, true
}

// option is a type-length-value option with the type and length stripped.
type option struct {
	typ  uint8
	body []byte
}

// parseOptions splits the options of a message. An option with a zero length
// or one that runs past the message invalidates the message (RFC 4861
// sections 6.1.1 and 6.1.2).
func parseOptions(b []byte) ([]option, error) {
	var opts []option
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, fmt.Errorf("%w: %d trailing bytes", errOptionLength, len(b))
		}
		n := int(b[1]) * optUnit
		if n == 0 {
			return nil, fmt.Errorf("%w: option type %d has length 0", errOptionLength, b[0])
		}
		if n > len(b) {
			return nil, fmt.Errorf("%w: option type %d claims %d bytes, %d left", errOptionLength, b[0], n, len(b))
		}
		opts = append(opts, option{typ: b[0], body: b[2:n]})
		b = b[n:]
	}
	return opts, nil
}

// appendLinkLayerAddressOption appends a link-layer address option for hw,
// padded to a whole number of 8-byte units, or nothing when hw is empty.
func appendLinkLayerAddressOption(b []byte, typ uint8, hw net.HardwareAddr) []byte {
	if len(hw) == 0 {
		return b
	}
	n := (2 + len(hw) + optUnit - 1) / optUnit
	b = append(b, typ, uint8(n))
	b = append(b, hw...)
	return append(b, make([]byte, n*optUnit-2-len(hw))...)
}

// parseLinkLayerAddress returns the hardware address of a link-layer address
// option body. Ethernet addresses fill their unit exactly; for other link
// types the padding cannot be told apart from the address, so the whole body
// is returned.
func parseLinkLayerAddress(body []byte) net.HardwareAddr {
	return net.HardwareAddr(append([]byte(nil), body...))
}

func secondsUint16(d time.Duration) (uint16, error) {
	s := d / time.Second
	if d < 0 || s > math.MaxUint16 {
		return 0, fmt.Errorf("%v does not fit in 16 bits of seconds", d)
	}
	return uint16(s), nil
}

func secondsUint32(d time.Duration) (uint32, error) {
	s := d / time.Second
	if d < 0 || s > math.MaxUint32 {
		return 0, fmt.Errorf("%v does not fit in 32 bits of seconds", d)
	}
	return uint32(s), nil
}

func millisUint32(d time.Duration) (uint32, error) {
	ms := d / time.Millisecond
	if d < 0 || ms > math.MaxUint32 {
		return 0, fmt.Errorf("%v does not fit in 32 bits of milliseconds", d)
	}
	return uint32(ms), nil
}
