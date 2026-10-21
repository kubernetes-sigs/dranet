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
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// equateNetip lets go-cmp compare the netip types by value.
var equateNetip = cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})

var testMAC = net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}

// testRA is an advertisement with every option the parser understands,
// testRABytes its encoding as a router would put it on the wire.
var testRA = &RouterAdvertisement{
	CurrentHopLimit:        64,
	ManagedConfiguration:   true,
	OtherConfiguration:     true,
	RouterPreference:       High,
	RouterLifetime:         1800 * time.Second,
	SourceLinkLayerAddress: testMAC,
	MTU:                    1500,
	Prefixes: []PrefixInformation{{
		Prefix:            netip.MustParsePrefix("2001:db8::/64"),
		OnLink:            true,
		Autonomous:        true,
		ValidLifetime:     86400 * time.Second,
		PreferredLifetime: 14400 * time.Second,
	}},
	Routes: []RouteInformation{{
		Prefix:     netip.MustParsePrefix("2001:db8:1::/48"),
		Preference: High,
		Lifetime:   600 * time.Second,
	}},
}

var testRABytes = []byte{
	// ICMPv6 header: type 134, code 0, checksum 0.
	134, 0, 0, 0,
	// Cur hop limit 64, flags M|O|Prf=High, router lifetime 1800.
	64, 0xc8, 0x07, 0x08,
	// Reachable time, retrans timer.
	0, 0, 0, 0, 0, 0, 0, 0,
	// Source link-layer address.
	1, 1, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55,
	// MTU 1500.
	5, 1, 0, 0, 0x00, 0x00, 0x05, 0xdc,
	// Prefix information 2001:db8::/64, L|A, valid 86400, preferred 14400.
	3, 4, 64, 0xc0,
	0x00, 0x01, 0x51, 0x80,
	0x00, 0x00, 0x38, 0x40,
	0, 0, 0, 0,
	0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	// Route information 2001:db8:1::/48, high preference, lifetime 600.
	24, 2, 48, 0x08,
	0x00, 0x00, 0x02, 0x58,
	0x20, 0x01, 0x0d, 0xb8, 0x00, 0x01, 0x00, 0x00,
}

func TestMarshalRouterSolicitation(t *testing.T) {
	for name, tc := range map[string]struct {
		rs         *RouterSolicitation
		want       []byte
		wantParsed net.HardwareAddr
	}{
		"without source link-layer address": {
			rs:   &RouterSolicitation{},
			want: []byte{133, 0, 0, 0, 0, 0, 0, 0},
		},
		"with source link-layer address": {
			rs:         &RouterSolicitation{SourceLinkLayerAddress: testMAC},
			want:       []byte{133, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			wantParsed: testMAC,
		},
		// A link-layer address that does not fill a unit is padded to one.
		// The padding cannot be told from the address on the way back, so
		// the parser returns the whole option body.
		"with 8-byte link-layer address": {
			rs:         &RouterSolicitation{SourceLinkLayerAddress: net.HardwareAddr{1, 2, 3, 4, 5, 6, 7, 8}},
			want:       []byte{133, 0, 0, 0, 0, 0, 0, 0, 1, 2, 1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0},
			wantParsed: net.HardwareAddr{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := MarshalMessage(tc.rs)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("MarshalMessage = % x\nwant % x", got, tc.want)
			}
			parsed, err := ParseMessage(got)
			if err != nil {
				t.Fatalf("ParseMessage: %v", err)
			}
			rs, ok := parsed.(*RouterSolicitation)
			if !ok {
				t.Fatalf("ParseMessage returned %T, want *RouterSolicitation", parsed)
			}
			if !bytes.Equal(rs.SourceLinkLayerAddress, tc.wantParsed) {
				t.Errorf("SourceLinkLayerAddress = %v, want %v", rs.SourceLinkLayerAddress, tc.wantParsed)
			}
		})
	}
}

func TestMarshalRouterAdvertisement(t *testing.T) {
	got, err := MarshalMessage(testRA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, testRABytes) {
		t.Errorf("MarshalMessage = % x\nwant % x", got, testRABytes)
	}
}

func TestParseRouterAdvertisement(t *testing.T) {
	m, err := ParseMessage(testRABytes)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(testRA, m, equateNetip); diff != "" {
		t.Errorf("ParseMessage mismatch (-want +got):\n%s", diff)
	}
}

// Fields the parser does not understand must not break it, and fields the
// RFCs tell it to ignore must be ignored without rejecting the message.
func TestParseRouterAdvertisementLenient(t *testing.T) {
	header := []byte{134, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	for name, tc := range map[string]struct {
		options []byte
		want    *RouterAdvertisement
	}{
		"unknown option is skipped": {
			// RDNSS (25) with one server.
			options: append([]byte{25, 3, 0, 0, 0, 0, 0x0e, 0x10}, make([]byte, 16)...),
			want:    &RouterAdvertisement{},
		},
		"reserved router preference reads as medium": {
			options: nil,
			want:    &RouterAdvertisement{},
		},
		"prefix longer than 128 bits is ignored": {
			options: append([]byte{3, 4, 129, 0xc0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0}, make([]byte, 16)...),
			want:    &RouterAdvertisement{},
		},
		"prefix option of the wrong length is ignored": {
			options: []byte{3, 1, 64, 0xc0, 0, 0, 0, 1},
			want:    &RouterAdvertisement{},
		},
		"route with reserved preference is ignored": {
			options: []byte{24, 1, 0, prefReserved << rioPrefShift, 0, 0, 0, 1},
			want:    &RouterAdvertisement{},
		},
		"route longer than its option is ignored": {
			// /72 needs 16 prefix bytes but the option carries 8.
			options: []byte{24, 2, 72, 0, 0, 0, 0, 1, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0},
			want:    &RouterAdvertisement{},
		},
		"default route information": {
			options: []byte{24, 1, 0, Low.mustEncode() << rioPrefShift, 0, 0, 0, 1},
			want: &RouterAdvertisement{Routes: []RouteInformation{{
				Prefix: netip.MustParsePrefix("::/0"), Preference: Low, Lifetime: time.Second,
			}}},
		},
		"route with a full prefix": {
			options: append([]byte{24, 3, 128, 0, 0, 0, 0, 1}, netip.MustParseAddr("2001:db8::1").AsSlice()...),
			want: &RouterAdvertisement{Routes: []RouteInformation{{
				Prefix: netip.MustParsePrefix("2001:db8::1/128"), Lifetime: time.Second,
			}}},
		},
		"infinite lifetimes": {
			options: append([]byte{3, 4, 64, 0xc0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}, netip.MustParseAddr("2001:db8::").AsSlice()...),
			want: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: netip.MustParsePrefix("2001:db8::/64"), OnLink: true, Autonomous: true,
				ValidLifetime: Infinity, PreferredLifetime: Infinity,
			}}},
		},
		"prefix bits past the prefix length are masked": {
			options: append([]byte{3, 4, 64, 0xc0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0}, netip.MustParseAddr("2001:db8::1").AsSlice()...),
			want: &RouterAdvertisement{Prefixes: []PrefixInformation{{
				Prefix: netip.MustParsePrefix("2001:db8::/64"), OnLink: true, Autonomous: true,
				ValidLifetime: time.Second, PreferredLifetime: time.Second,
			}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := append(append([]byte(nil), header...), tc.options...)
			if name == "reserved router preference reads as medium" {
				b[5] = prefReserved << raPrefShift
			}
			m, err := ParseMessage(b)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, m, equateNetip); diff != "" {
				t.Errorf("ParseMessage mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// What RFC 4861 sections 6.1.1 and 6.1.2 make a receiver discard must be an
// error, so Conn never hands a malformed message to the caller.
func TestParseMessageInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		b       []byte
		wantErr error
	}{
		"shorter than the ICMPv6 header": {b: []byte{134, 0, 0}, wantErr: errMessageTooShort},
		"advertisement without body":     {b: []byte{134, 0, 0, 0, 0, 0}, wantErr: errMessageTooShort},
		"solicitation without body":      {b: []byte{133, 0, 0, 0, 0}, wantErr: errMessageTooShort},
		"non-zero code":                  {b: []byte{134, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		"unsupported type":               {b: []byte{135, 0, 0, 0, 0, 0, 0, 0}},
		"option with zero length":        {b: []byte{133, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0}, wantErr: errOptionLength},
		"option past the end":            {b: []byte{133, 0, 0, 0, 0, 0, 0, 0, 1, 2, 0, 0, 0, 0, 0, 0}, wantErr: errOptionLength},
		"trailing option bytes":          {b: []byte{133, 0, 0, 0, 0, 0, 0, 0, 1}, wantErr: errOptionLength},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseMessage(tc.b)
			if err == nil {
				t.Fatal("ParseMessage accepted an invalid message")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseMessage error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// Values the wire format cannot carry must be refused rather than truncated
// into a different advertisement.
func TestMarshalRouterAdvertisementInvalid(t *testing.T) {
	for name, ra := range map[string]*RouterAdvertisement{
		"router lifetime over 16 bits": {RouterLifetime: 70000 * time.Second},
		"negative reachable time":      {ReachableTime: -time.Second},
		"invalid router preference":    {RouterPreference: 2},
		"prefix lifetime over 32 bits": {Prefixes: []PrefixInformation{{Prefix: netip.MustParsePrefix("2001:db8::/64"), ValidLifetime: Infinity + time.Second}}},
		"IPv4 prefix":                  {Prefixes: []PrefixInformation{{Prefix: netip.MustParsePrefix("10.0.0.0/8")}}},
		"IPv4-mapped route":            {Routes: []RouteInformation{{Prefix: netip.MustParsePrefix("::ffff:10.0.0.0/104")}}},
		"invalid route preference":     {Routes: []RouteInformation{{Prefix: netip.MustParsePrefix("2001:db8::/64"), Preference: -2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MarshalMessage(ra); err == nil {
				t.Error("MarshalMessage accepted an invalid advertisement")
			}
		})
	}
}

func TestPreference(t *testing.T) {
	for _, p := range []Preference{Low, Medium, High} {
		bits, err := p.encode()
		if err != nil {
			t.Fatal(err)
		}
		if got := decodePreference(bits); got != p {
			t.Errorf("decodePreference(encode(%d)) = %d", p, got)
		}
	}
	if got := decodePreference(prefReserved); got != Medium {
		t.Errorf("decodePreference(reserved) = %d, want Medium", got)
	}
}

func (p Preference) mustEncode() uint8 {
	bits, err := p.encode()
	if err != nil {
		panic(err)
	}
	return bits
}
