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
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestStableIdentifier(t *testing.T) {
	got := StableIdentifier("claim-uid", "rdma0")
	if again := StableIdentifier("claim-uid", "rdma0"); again != got {
		t.Errorf("StableIdentifier() = % x, then % x for the same names", got, again)
	}
	for name, other := range map[string]InterfaceIdentifier{
		"another claim":        StableIdentifier("other-uid", "rdma0"),
		"another device":       StableIdentifier("claim-uid", "rdma1"),
		"another split":        StableIdentifier("claim-ui", "drdma0"),
		"another empty device": StableIdentifier("claim-uid", ""),
		"one name":             StableIdentifier("claim-uidrdma0"),
	} {
		if other == got {
			t.Errorf("%s: StableIdentifier() = % x, the same as for (claim-uid, rdma0)", name, other)
		}
	}
}

func TestReservedIdentifier(t *testing.T) {
	for v, want := range map[uint64]bool{
		0x0000000000000000: true, // Subnet-Router anycast
		0x0000000000000001: false,
		0x02005efffe000000: true, // IANA Ethernet block
		0x02005efffeffffff: true,
		0x02005efffdffffff: false,
		0x02005effff000000: false,
		0xfdffffffffffff7f: false,
		0xfdffffffffffff80: true, // reserved subnet anycast
		0xfdffffffffffffff: true,
		0xfe00000000000000: false,
	} {
		var id InterfaceIdentifier
		binary.BigEndian.PutUint64(id[:], v)
		if got := id.reserved(); got != want {
			t.Errorf("reserved(%016x) = %t, want %t", v, got, want)
		}
	}
}

func TestInterfaceIdentifierAddress(t *testing.T) {
	id := InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde}
	got, err := id.Address(netip.MustParsePrefix("2001:db8:0:1::/64"))
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("2001:db8:0:1:3656:78ff:fe9a:bcde"); got != want {
		t.Errorf("Address = %v, want %v", got, want)
	}
	// Bits of the prefix past its length do not leak into the address.
	got, err = id.Address(netip.MustParsePrefix("2001:db8:0:1:ffff::/64"))
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("2001:db8:0:1:3656:78ff:fe9a:bcde"); got != want {
		t.Errorf("Address = %v, want %v", got, want)
	}
	for _, prefix := range []string{"2001:db8::/48", "2001:db8::/80", "10.0.0.0/8", "::ffff:10.0.0.0/96"} {
		if _, err := id.Address(netip.MustParsePrefix(prefix)); err == nil {
			t.Errorf("Address(%s) accepted a prefix an identifier cannot complete", prefix)
		}
	}
}

func TestAutoconfigure(t *testing.T) {
	var (
		id           = InterfaceIdentifier{0x36, 0x56, 0x78, 0xff, 0xfe, 0x9a, 0xbc, 0xde}
		router       = netip.MustParseAddr("fe80::1")
		prefix       = netip.MustParsePrefix("2001:db8::/64")
		address      = netip.MustParseAddr("2001:db8::3656:78ff:fe9a:bcde")
		defaultRoute = netip.MustParsePrefix("::/0")
		hour         = time.Hour
	)
	pio := func(p string, onLink, autonomous bool) PrefixInformation {
		return PrefixInformation{Prefix: netip.MustParsePrefix(p), OnLink: onLink, Autonomous: autonomous, ValidLifetime: hour, PreferredLifetime: hour}
	}

	for name, tc := range map[string]struct {
		ra   *RouterAdvertisement
		want Autoconf
	}{
		"on-link autonomous prefix from a default router": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Prefixes: []PrefixInformation{pio("2001:db8::/64", true, true)}},
			want: Autoconf{
				Addresses: []netip.Prefix{netip.PrefixFrom(address, 64)},
				Routes:    []Route{{Destination: defaultRoute, Gateway: router}},
			},
		},
		// The prefix is reached through the router, so the address must not
		// bring a connected route with it.
		"autonomous prefix that is not on-link": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", false, true)}},
			want: Autoconf{Addresses: []netip.Prefix{netip.PrefixFrom(address, 128)}},
		},
		"on-link prefix without autoconfiguration": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", true, false)}},
			want: Autoconf{Routes: []Route{{Destination: prefix}}},
		},
		"prefix with neither flag": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", false, false)}},
			want: Autoconf{},
		},
		"router that is not a default router": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/64", true, true)}},
			want: Autoconf{Addresses: []netip.Prefix{netip.PrefixFrom(address, 64)}},
		},
		// RFC 4862 section 5.5.3 d: the identifier must fit exactly; the
		// on-link flag still stands on its own.
		"autonomous prefix that is not a /64": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("2001:db8::/56", true, true)}},
			want: Autoconf{Routes: []Route{{Destination: netip.MustParsePrefix("2001:db8::/56")}}},
		},
		// The address would be configured without lifetimes, so preferred.
		"deprecated prefix": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetime: hour, PreferredLifetime: 0,
			}}},
			want: Autoconf{Routes: []Route{{Destination: prefix}}},
		},
		"prefix advertised twice": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{
				pio("2001:db8::/64", true, true),
				pio("2001:db8::/64", true, true),
			}},
			want: Autoconf{Addresses: []netip.Prefix{netip.PrefixFrom(address, 64)}},
		},
		"multicast prefix": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("ff02::/64", true, true)}},
			want: Autoconf{},
		},
		"IPv4-mapped prefix": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("::ffff:10.0.0.0/104", true, false)}},
			want: Autoconf{},
		},
		"default prefix": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("::/0", true, false)}},
			want: Autoconf{},
		},
		"link-local prefix": {
			ra:   &RouterAdvertisement{Prefixes: []PrefixInformation{pio("fe80::/64", true, true)}},
			want: Autoconf{},
		},
		"withdrawn prefix": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetime: 0, PreferredLifetime: 0,
			}}},
			want: Autoconf{},
		},
		// RFC 4862 section 5.5.3 c.
		"preferred lifetime longer than valid lifetime": {
			ra: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetime: hour, PreferredLifetime: 2 * hour,
			}}},
			want: Autoconf{Routes: []Route{{Destination: prefix}}},
		},
		"several prefixes": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Prefixes: []PrefixInformation{
				pio("2001:db8::/64", true, true),
				pio("2001:db8:1::/64", true, true),
				pio("2001:db8:2::/48", true, false),
			}},
			want: Autoconf{
				Addresses: []netip.Prefix{
					netip.PrefixFrom(address, 64),
					netip.PrefixFrom(netip.MustParseAddr("2001:db8:1::3656:78ff:fe9a:bcde"), 64),
				},
				Routes: []Route{
					{Destination: netip.MustParsePrefix("2001:db8:2::/48")},
					{Destination: defaultRoute, Gateway: router},
				},
			},
		},
		"route information": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Routes: []RouteInformation{
				{Prefix: netip.MustParsePrefix("2001:db8:ffff::/48"), Lifetime: hour},
				{Prefix: netip.MustParsePrefix("2001:db8:fffe::/48"), Lifetime: 0},
				{Prefix: netip.MustParsePrefix("fe80::/64"), Lifetime: hour},
				{Prefix: netip.MustParsePrefix("ff00::/8"), Lifetime: hour},
				{Prefix: netip.MustParsePrefix("::ffff:10.0.0.0/104"), Lifetime: hour},
			}},
			want: Autoconf{Routes: []Route{
				{Destination: defaultRoute, Gateway: router},
				{Destination: netip.MustParsePrefix("2001:db8:ffff::/48"), Gateway: router},
			}},
		},
		// A default route advertised both ways is one route.
		"route information for the default route": {
			ra: &RouterAdvertisement{RouterLifetime: hour, Routes: []RouteInformation{
				{Prefix: defaultRoute, Lifetime: hour},
			}},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
		// A router with a zero lifetime can still offer a default route by
		// Route Information (RFC 4191 section 3.1).
		"default route only by route information": {
			ra:   &RouterAdvertisement{Routes: []RouteInformation{{Prefix: defaultRoute, Lifetime: hour}}},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
		"managed configuration without prefixes": {
			ra:   &RouterAdvertisement{ManagedConfiguration: true, RouterLifetime: hour},
			want: Autoconf{Routes: []Route{{Destination: defaultRoute, Gateway: router}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Autoconfigure(tc.ra, router, id)
			if diff := cmp.Diff(tc.want, got, equateNetip); diff != "" {
				t.Errorf("Autoconfigure mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
