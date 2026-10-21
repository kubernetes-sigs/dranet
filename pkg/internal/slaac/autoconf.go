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
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
)

// identifierBits is the length of the interface identifier of an
// autoconfigured address; a prefix must leave exactly this many bits
// (RFC 4862 section 5.5.3 d).
const identifierBits = 64

// InterfaceIdentifier is the low 64 bits of an autoconfigured address.
type InterfaceIdentifier [identifierBits / 8]byte

// StableIdentifier returns an interface identifier that is a hash of names,
// like a stable identifier (RFC 7217) without the secret: the same for the
// same names, different for different ones, and independent of the hardware
// address, which an IPVLAN child shares with its parent. A hash that is a
// reserved identifier (RFC 5453) is hashed again with a counter.
func StableIdentifier(names ...string) InterfaceIdentifier {
	for counter := uint32(0); ; counter++ {
		h := sha256.New()
		for _, name := range names {
			// The terminator keeps ("ab", "c") and ("a", "bc") apart.
			h.Write([]byte(name))
			h.Write([]byte{0})
		}
		_ = binary.Write(h, binary.BigEndian, counter)
		id := InterfaceIdentifier(h.Sum(nil)[:len(InterfaceIdentifier{})])
		if !id.reserved() {
			return id
		}
	}
}

// reserved reports whether id is a reserved interface identifier (RFC 5453):
// the Subnet-Router anycast identifier, the identifiers of the IANA Ethernet
// block (00-00-5E, as modified EUI-64), and the reserved subnet anycast
// identifiers.
func (id InterfaceIdentifier) reserved() bool {
	v := binary.BigEndian.Uint64(id[:])
	return v == 0 ||
		v>>24 == 0x02005efffe ||
		(v >= 0xfdffffffffffff80 && v <= 0xfdffffffffffffff)
}

// Address forms the address with the prefix's first 64 bits and the
// identifier (RFC 4862 section 5.5.3 d). The prefix must be a /64.
func (id InterfaceIdentifier) Address(prefix netip.Prefix) (netip.Addr, error) {
	if !prefix.IsValid() || !prefix.Addr().Is6() || prefix.Addr().Is4In6() || prefix.Bits() != 128-identifierBits {
		return netip.Addr{}, fmt.Errorf("slaac: an address needs an IPv6 /%d prefix, got %v", 128-identifierBits, prefix)
	}
	a := prefix.Masked().Addr().As16()
	copy(a[8:], id[:])
	return netip.AddrFrom16(a), nil
}

// Route is a route a host installs from a Router Advertisement.
type Route struct {
	// Destination is the prefix the route covers; ::/0 for a default route.
	Destination netip.Prefix
	// Gateway is the router the destination is reached through. It is the
	// zero Addr for an on-link prefix, which is reached directly.
	Gateway netip.Addr
}

// Autoconf is what a host configures from a Router Advertisement.
type Autoconf struct {
	// Addresses are the autoconfigured addresses, each with the prefix
	// length to configure it with: the advertised prefix length when the
	// prefix is on-link, so that the connected route comes with the address
	// as it does for a kernel-autoconfigured one, and /128 when it is not,
	// since the prefix is then reached through the router.
	Addresses []netip.Prefix
	// Routes are the default route, the more specific routes through the
	// router (RFC 4191) and the on-link prefixes no address was formed from.
	Routes []Route
}

// Autoconfigure derives what a host configures from an advertisement sent by
// router, forming addresses with id. It applies RFC 4862 section 5.5.3 to the
// prefixes usable for autoconfiguration, RFC 4861 section 6.3.4 to the
// on-link prefixes and the default router, and RFC 4191 section 3.1 to the
// Route Information options. Lifetimes are not tracked: the result is a
// snapshot for a host that configures itself once.
func Autoconfigure(ra *RouterAdvertisement, router netip.Addr, id InterfaceIdentifier) Autoconf {
	var ac Autoconf
	for _, p := range ra.Prefixes {
		prefix := p.Prefix.Masked()
		// The link-local prefix is never autoconfigured from nor routed
		// (RFC 4862 section 5.5.3 b, RFC 4861 section 6.3.4), and neither is
		// any other prefix that is not global unicast.
		if !routable(prefix) || prefix.Bits() == 0 {
			continue
		}
		// A zero valid lifetime only ever withdraws a prefix.
		if p.ValidLifetime == 0 {
			continue
		}
		formed := false
		// RFC 4862 section 5.5.3 c and d: a preferred lifetime beyond the
		// valid one or a prefix that leaves no room for the identifier is
		// ignored for autoconfiguration. A deprecated prefix, with a zero
		// preferred lifetime, is skipped too: the address is configured
		// without lifetimes, so it would be preferred.
		if p.Autonomous && p.PreferredLifetime > 0 && p.PreferredLifetime <= p.ValidLifetime {
			if addr, err := id.Address(prefix); err == nil {
				bits := 128
				if p.OnLink {
					bits = prefix.Bits()
				}
				// A prefix advertised twice forms one address.
				if !slices.ContainsFunc(ac.Addresses, func(a netip.Prefix) bool { return a.Addr() == addr }) {
					ac.Addresses = append(ac.Addresses, netip.PrefixFrom(addr, bits))
				}
				formed = true
			}
		}
		// An on-link prefix is reached directly. When an address was formed
		// from it, the address's own prefix length already says so.
		if p.OnLink && !formed {
			ac.Routes = appendRoute(ac.Routes, Route{Destination: prefix})
		}
	}
	defaultRoute := netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	if ra.RouterLifetime > 0 {
		ac.Routes = appendRoute(ac.Routes, Route{Destination: defaultRoute, Gateway: router})
	}
	for _, r := range ra.Routes {
		if r.Lifetime == 0 || !routable(r.Prefix.Masked()) {
			continue
		}
		ac.Routes = appendRoute(ac.Routes, Route{Destination: r.Prefix.Masked(), Gateway: router})
	}
	return ac
}

// routable reports whether a host may install a route to prefix from an
// advertisement: the default route, or a global unicast IPv6 prefix. An
// IPv4-mapped prefix would become an IPv4 route.
func routable(prefix netip.Prefix) bool {
	addr := prefix.Addr()
	return prefix.Bits() == 0 || (addr.IsGlobalUnicast() && !addr.Is4In6())
}

// appendRoute adds r unless an equal route is already there: a router may
// advertise ::/0 both through its lifetime and as a Route Information option.
func appendRoute(routes []Route, r Route) []Route {
	if slices.Contains(routes, r) {
		return routes
	}
	return append(routes, r)
}
