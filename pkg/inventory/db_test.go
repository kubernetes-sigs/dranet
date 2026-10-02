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

package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jaypipes/ghw"
	"github.com/jaypipes/pcidb"
	"github.com/vishvananda/netlink"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/gce"

	"github.com/jaypipes/ghw/pkg/topology"
	userns "sigs.k8s.io/dranet/internal/testutils"
)

func TestIsAllocatableNetworkDevice(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		want   bool
	}{
		{name: "unbound", driver: "", want: false},
		{name: "vfio passthrough", driver: "vfio-pci", want: false},
		{name: "uio generic", driver: "uio_pci_generic", want: false},
		{name: "dpdk uio", driver: "igb_uio", want: false},
		{name: "pci stub", driver: "pci-stub", want: false},
		{name: "gve", driver: "gve", want: true},
		{name: "mlx5", driver: "mlx5_core", want: true},
		{name: "virtio", driver: "virtio_net", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := &ghw.PCIDevice{Driver: tc.driver}
			if got := isAllocatableNetworkDevice(dev); got != tc.want {
				t.Errorf("isAllocatableNetworkDevice(driver=%q) = %v, want %v", tc.driver, got, tc.want)
			}
		})
	}
}

// TestAddLinkAttributesIPLengthCap covers the per-attribute string-value
// limit on AttrIPv4 / AttrIPv6 (see resourceapi.DeviceAttributeMaxValueLength).
// The kube-proxy IPVS dummy interface (kube-ipvs0) accumulates every cluster
// ServiceIP, and if dranet joins them all into a single comma-separated
// attribute the resulting ResourceSlice is rejected by the API server with
// "Too long: may not be more than 64 bytes". addLinkAttributes must truncate
// the joined value so it fits within the cap while still publishing as many
// addresses as possible, and the other family's attribute must not be
// affected when only one family overflows.
func TestAddLinkAttributesIPLengthCap(t *testing.T) {
	userns.Run(t, testAddLinkAttributesIPLengthCap_Namespaced, syscall.CLONE_NEWNET)
}

func testAddLinkAttributesIPLengthCap_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	// Build IP address sets of various sizes.
	// One IPv4 "/32" is 12 bytes (e.g. "10.96.0.1/32"); 5 of them joined by
	// commas total 64 bytes — the boundary case. 6 of them spill over.
	manyV4 := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		manyV4 = append(manyV4, fmt.Sprintf("10.96.0.%d/32", i+1))
	}
	manyV4Set := sets.New(manyV4...)
	manyV6 := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		// Each "fd00::N/128" is 11–12 bytes; 6 of them comfortably overflow.
		manyV6 = append(manyV6, fmt.Sprintf("fd00::%x/128", i+1))
	}
	manyV6Set := sets.New(manyV6...)

	tests := []struct {
		name           string
		ipv4           []string
		ipv6           []string
		wantV4Set      bool
		wantV6Set      bool
		wantV4Value    string // when non-empty, the attribute must match exactly
		wantV6Value    string
		wantV4Pool     sets.Set[string] // when non-nil, every comma-split entry must be in this set
		wantV6Pool     sets.Set[string]
		wantV4Truncate bool // when true, attribute must be set AND shorter than the full join
		wantV6Truncate bool
	}{
		{
			name: "no IPs - neither attribute set",
		},
		{
			name:        "single IPv4 - attribute set",
			ipv4:        []string{"10.0.0.1/24"},
			wantV4Set:   true,
			wantV4Value: "10.0.0.1/24",
		},
		{
			name:        "single IPv6 - attribute set",
			ipv6:        []string{"fd00::1/64"},
			wantV6Set:   true,
			wantV6Value: "fd00::1/64",
		},
		{
			name:           "many IPv4 overflow - attribute truncated",
			ipv4:           manyV4,
			wantV4Set:      true,
			wantV4Pool:     manyV4Set,
			wantV4Truncate: true,
		},
		{
			name:           "many IPv6 overflow - attribute truncated",
			ipv6:           manyV6,
			wantV6Set:      true,
			wantV6Pool:     manyV6Set,
			wantV6Truncate: true,
		},
		{
			// kube-ipvs0-style mix: lots of v4 ClusterIPs overflow the limit
			// but a single v6 host address still fits — AttrIPv4 is truncated
			// to fit, and AttrIPv6 is published in full.
			name:           "v4 overflow does not drop v6",
			ipv4:           manyV4,
			ipv6:           []string{"fd00::1/64"},
			wantV4Set:      true,
			wantV4Pool:     manyV4Set,
			wantV4Truncate: true,
			wantV6Set:      true,
			wantV6Value:    "fd00::1/64",
		},
		{
			// Mirror of the above: the v6 set overflows but a single v4
			// address still fits. AttrIPv6 is truncated, AttrIPv4 is
			// published in full. Covers the tamilmani1989 review case.
			name:           "v6 overflow does not drop v4",
			ipv4:           []string{"10.0.0.1/24"},
			ipv6:           manyV6,
			wantV4Set:      true,
			wantV4Value:    "10.0.0.1/24",
			wantV6Set:      true,
			wantV6Pool:     manyV6Set,
			wantV6Truncate: true,
		},
	}

	for i, tt := range tests {
		tt := tt
		ifName := fmt.Sprintf("attrcap%d", i)
		t.Run(tt.name, func(t *testing.T) {
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
			if err := netlink.LinkAdd(dummy); err != nil {
				t.Fatalf("failed to add dummy %s: %v", ifName, err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })

			link, err := netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("failed to look up %s: %v", ifName, err)
			}
			// Bring the link up so global-scope addresses stay usable.
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatalf("failed to set %s up: %v", ifName, err)
			}

			for _, cidr := range tt.ipv4 {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s, %s): %v", ifName, cidr, err)
				}
			}
			for _, cidr := range tt.ipv6 {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s, %s): %v", ifName, cidr, err)
				}
			}

			// Re-fetch the link so its address list is current.
			link, err = netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("re-fetch %s: %v", ifName, err)
			}

			device := &resourceapi.Device{
				Name:       ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			}
			addLinkAttributes(device, link)

			// Always-set attributes — sanity check we didn't break the rest
			// of addLinkAttributes while editing the IP block.
			if got, ok := device.Attributes[apis.AttrInterfaceName]; !ok || got.StringValue == nil || *got.StringValue != ifName {
				t.Errorf("AttrInterfaceName = %+v, want %q", got, ifName)
			}

			gotV4, hasV4 := device.Attributes[apis.AttrIPv4]
			if hasV4 != tt.wantV4Set {
				t.Errorf("AttrIPv4 present = %v, want %v (value=%+v)", hasV4, tt.wantV4Set, gotV4)
			}
			if hasV4 && gotV4.StringValue != nil {
				checkIPAttribute(t, "AttrIPv4", *gotV4.StringValue, tt.wantV4Value, tt.wantV4Pool, tt.wantV4Truncate, tt.ipv4)
			}

			gotV6, hasV6 := device.Attributes[apis.AttrIPv6]
			if hasV6 != tt.wantV6Set {
				t.Errorf("AttrIPv6 present = %v, want %v (value=%+v)", hasV6, tt.wantV6Set, gotV6)
			}
			if hasV6 && gotV6.StringValue != nil {
				checkIPAttribute(t, "AttrIPv6", *gotV6.StringValue, tt.wantV6Value, tt.wantV6Pool, tt.wantV6Truncate, tt.ipv6)
			}
		})
	}
}

// checkIPAttribute asserts the invariants every published IP attribute must
// satisfy: it fits within the DRA cap, every comma-split entry came from the
// originally-provided pool (no fabricated values), the exact value matches
// when one is specified, and when truncation is expected the result is
// strictly shorter than joining the full input.
func checkIPAttribute(t *testing.T, name, got, wantExact string, wantPool sets.Set[string], wantTruncated bool, fullInput []string) {
	t.Helper()
	if len(got) > resourceapi.DeviceAttributeMaxValueLength {
		t.Errorf("%s value length %d exceeds DRA cap %d: %q",
			name, len(got), resourceapi.DeviceAttributeMaxValueLength, got)
	}
	if wantExact != "" && got != wantExact {
		t.Errorf("%s = %q, want %q", name, got, wantExact)
	}
	if wantPool != nil {
		for _, entry := range strings.Split(got, ",") {
			if !wantPool.Has(entry) {
				t.Errorf("%s contains %q which is not in the input pool", name, entry)
			}
		}
	}
	if wantTruncated {
		fullLen := 0
		for i, ip := range fullInput {
			fullLen += len(ip)
			if i > 0 {
				fullLen++
			}
		}
		if len(got) >= fullLen {
			t.Errorf("%s = %q (len=%d) expected to be a truncated prefix of the %d-byte full join",
				name, got, len(got), fullLen)
		}
		if got == "" {
			t.Errorf("%s expected to be a non-empty truncated value", name)
		}
	}
}

// TestAddLinkAttributesIPBoundaryLength exercises the exact DRA cap: a joined
// string of length DeviceAttributeMaxValueLength is published in full, while
// adding one more address that would push us past the limit causes the
// attribute to be truncated rather than dropped.
func TestAddLinkAttributesIPBoundaryLength(t *testing.T) {
	userns.Run(t, testAddLinkAttributesIPBoundaryLength_Namespaced, syscall.CLONE_NEWNET)
}

func testAddLinkAttributesIPBoundaryLength_Namespaced(t *testing.T) {
	if err := netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}}); err != nil {
		t.Fatalf("failed to bring lo up: %v", err)
	}

	// 5 × "10.96.0.N/32" (12 bytes each) joined with commas = 5*12 + 4 = 64 bytes.
	addrsExactlyAtLimit := []string{
		"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32",
	}
	addrsJustOverLimit := append([]string{}, addrsExactlyAtLimit...)
	// "10.96.0.10/32" is 13 bytes; appending it and a comma makes the join >64.
	addrsJustOverLimit = append(addrsJustOverLimit, "10.96.0.10/32")

	cases := []struct {
		name          string
		addrs         []string
		wantSet       bool
		wantTruncated bool // when true, value must be set and < full join length
	}{
		{name: "exactly at limit", addrs: addrsExactlyAtLimit, wantSet: true},
		{name: "just over limit", addrs: addrsJustOverLimit, wantSet: true, wantTruncated: true},
	}

	for i, tc := range cases {
		tc := tc
		ifName := fmt.Sprintf("attrbnd%d", i)
		t.Run(tc.name, func(t *testing.T) {
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifName}}
			if err := netlink.LinkAdd(dummy); err != nil {
				t.Fatalf("LinkAdd %s: %v", ifName, err)
			}
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })
			link, err := netlink.LinkByName(ifName)
			if err != nil {
				t.Fatalf("LinkByName %s: %v", ifName, err)
			}
			if err := netlink.LinkSetUp(link); err != nil {
				t.Fatalf("LinkSetUp %s: %v", ifName, err)
			}
			for _, cidr := range tc.addrs {
				addr, err := netlink.ParseAddr(cidr)
				if err != nil {
					t.Fatalf("ParseAddr(%q): %v", cidr, err)
				}
				if err := netlink.AddrAdd(link, addr); err != nil {
					t.Fatalf("AddrAdd(%s,%s): %v", ifName, cidr, err)
				}
			}
			link, _ = netlink.LinkByName(ifName)

			device := &resourceapi.Device{
				Name:       ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			}
			addLinkAttributes(device, link)

			got, has := device.Attributes[apis.AttrIPv4]
			if has != tc.wantSet {
				t.Fatalf("AttrIPv4 present = %v, want %v", has, tc.wantSet)
			}
			if !has || got.StringValue == nil {
				return
			}
			if len(*got.StringValue) > resourceapi.DeviceAttributeMaxValueLength {
				t.Errorf("AttrIPv4 joined length = %d, exceeds DRA cap %d (value=%q)",
					len(*got.StringValue), resourceapi.DeviceAttributeMaxValueLength, *got.StringValue)
			}
			// Defense in depth: the value must contain a comma for the
			// multi-address case, which proves we used Join correctly.
			if len(tc.addrs) > 1 && !strings.Contains(*got.StringValue, ",") {
				t.Errorf("AttrIPv4 = %q, expected comma-joined value", *got.StringValue)
			}
			// Every entry must come from the input set: truncation must
			// preserve a prefix of sorted inputs, not fabricate values.
			pool := sets.New(tc.addrs...)
			for _, entry := range strings.Split(*got.StringValue, ",") {
				if !pool.Has(entry) {
					t.Errorf("AttrIPv4 contains %q which is not in the input addrs", entry)
				}
			}
			fullLen := 0
			for i, ip := range tc.addrs {
				fullLen += len(ip)
				if i > 0 {
					fullLen++
				}
			}
			if !tc.wantTruncated && len(*got.StringValue) != fullLen {
				t.Errorf("AttrIPv4 length = %d, want full-join length %d (value=%q)",
					len(*got.StringValue), fullLen, *got.StringValue)
			}
			if tc.wantTruncated && len(*got.StringValue) >= fullLen {
				t.Errorf("AttrIPv4 length = %d, expected < %d (truncated) (value=%q)",
					len(*got.StringValue), fullLen, *got.StringValue)
			}
		})
	}
}

// TestBuildIPList exercises the truncation helper directly, away from netns
// plumbing, so the byte-arithmetic boundaries are easy to read and the test
// runs on any platform (not just linux).
func TestBuildIPList(t *testing.T) {
	cases := []struct {
		name     string
		ips      []string
		maxBytes int
		want     string
		wantKept int
	}{
		{
			name:     "empty input",
			ips:      nil,
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "single ip fits",
			ips:      []string{"10.0.0.1/24"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.0.0.1/24",
			wantKept: 1,
		},
		{
			name:     "all fit exactly at limit",
			ips:      []string{"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.96.0.1/32,10.96.0.2/32,10.96.0.3/32,10.96.0.4/32,10.96.0.5/32",
			wantKept: 5,
		},
		{
			name:     "stops before the address that would overflow",
			ips:      []string{"10.96.0.1/32", "10.96.0.2/32", "10.96.0.3/32", "10.96.0.4/32", "10.96.0.5/32", "10.96.0.6/32"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			want:     "10.96.0.1/32,10.96.0.2/32,10.96.0.3/32,10.96.0.4/32,10.96.0.5/32",
			wantKept: 5,
		},
		{
			name:     "first ip larger than limit returns empty",
			ips:      []string{"10.96.0.1/32"},
			maxBytes: 5,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "non-positive limit returns empty",
			ips:      []string{"10.0.0.1/24"},
			maxBytes: 0,
			want:     "",
			wantKept: 0,
		},
		{
			name:     "ipv6 truncation stops at the right boundary",
			ips:      []string{"fd00::1/128", "fd00::2/128", "fd00::3/128", "fd00::4/128", "fd00::5/128", "fd00::6/128"},
			maxBytes: resourceapi.DeviceAttributeMaxValueLength,
			// 5 × 11 + 4 commas = 59 bytes; the 6th would push to 71.
			want:     "fd00::1/128,fd00::2/128,fd00::3/128,fd00::4/128,fd00::5/128",
			wantKept: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, kept := buildIPList(tc.ips, tc.maxBytes)
			if got != tc.want {
				t.Errorf("buildIPList = %q, want %q", got, tc.want)
			}
			if kept != tc.wantKept {
				t.Errorf("buildIPList kept = %d, want %d", kept, tc.wantKept)
			}
			if len(got) > tc.maxBytes && tc.maxBytes > 0 {
				t.Errorf("buildIPList result length %d exceeds maxBytes %d", len(got), tc.maxBytes)
			}
		})
	}
}

// mockCloudInstance implements cloudprovider.CloudInstance for testing
type mockCloudInstance struct {
	deviceAttributes map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
}

func (m *mockCloudInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	// Look up by MAC if present, otherwise fall back to the (interface) name.
	if id.MAC != "" {
		return m.deviceAttributes[id.MAC]
	}
	if id.Name != "" {
		return m.deviceAttributes[id.Name]
	}
	return nil
}

func (m *mockCloudInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	return nil
}

type identifierCheckingProvider struct {
	mockCloudInstance
	check func(string, cloudprovider.DeviceIdentifiers)
}

func (p *identifierCheckingProvider) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	p.check("GetDeviceConfig", id)
	return nil
}

func (p *identifierCheckingProvider) GetProfileConfig(id cloudprovider.DeviceIdentifiers, _ *resourceapi.ResourceClaim, _ *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	p.check("GetProfileConfig", id)
	return nil, nil
}

func (p *identifierCheckingProvider) ReleaseProfileConfig(id cloudprovider.DeviceIdentifiers, _ types.UID, _ *apis.NetworkConfig) error {
	p.check("ReleaseProfileConfig", id)
	return nil
}

func TestProviderIdentifiers(t *testing.T) {
	for _, ifName := range []string{"enP22s22f0np0", ""} {
		t.Run(fmt.Sprintf("ifName=%q", ifName), func(t *testing.T) {
			device := resourceapi.Device{
				Name: "pci-0000-00-01-0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrMac:        {StringValue: ptr.To("00:11:22:33:44:55")},
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:01.0")},
				},
			}
			if ifName != "" {
				device.Attributes[apis.AttrInterfaceName] = resourceapi.DeviceAttribute{StringValue: ptr.To(ifName)}
			}
			want := cloudprovider.DeviceIdentifiers{Name: ifName, MAC: "00:11:22:33:44:55", PCIAddress: "0000:00:01.0"}
			var calls []string
			provider := &identifierCheckingProvider{check: func(method string, got cloudprovider.DeviceIdentifiers) {
				calls = append(calls, method)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s identifiers mismatch (-want +got):\n%s", method, diff)
				}
			}}
			db := New(WithCloudInstance(provider), WithProfileProvider(provider))
			db.updateDeviceStore([]resourceapi.Device{device})
			claim := &resourceapi.ResourceClaim{}
			claim.UID = "claim-1"
			config := &apis.NetworkConfig{Profile: "test-profile"}
			if _, err := db.GetProfileConfig(device.Name, claim, config); err != nil {
				t.Fatal(err)
			}
			if err := db.ReleaseProfileConfig(device.Name, claim.UID, config); err != nil {
				t.Fatal(err)
			}

			// Cleanup must still reach the provider after the device leaves inventory.
			db.updateDeviceStore(nil)
			want = cloudprovider.DeviceIdentifiers{}
			if err := db.ReleaseProfileConfig(device.Name, claim.UID, config); err != nil {
				t.Fatal(err)
			}
			wantCalls := []string{"GetDeviceConfig", "GetProfileConfig", "ReleaseProfileConfig", "ReleaseProfileConfig"}
			if diff := cmp.Diff(wantCalls, calls); diff != "" {
				t.Errorf("provider calls mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetProviderAttributes(t *testing.T) {
	tests := []struct {
		name     string
		device   *resourceapi.Device
		instance cloudprovider.CloudInstance
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name:     "nil instance",
			device:   &resourceapi.Device{Name: "dev1"},
			instance: nil,
			want:     nil,
		},
		{
			name:     "nil device",
			device:   nil,
			instance: &mockCloudInstance{},
			want:     nil,
		},
		{
			name:     "device with no identifiers",
			device:   &resourceapi.Device{},
			instance: &mockCloudInstance{},
			want:     nil,
		},
		{
			name: "instance with no matching MAC",
			device: &resourceapi.Device{
				Name: "dev1",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrMac: {StringValue: ptr.To("00:11:22:33:44:FF")},
				},
			},
			instance: &mockCloudInstance{
				deviceAttributes: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"00:11:22:33:44:55": {
						gce.AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
					},
				},
			},
			want: nil,
		},
		{
			name: "MAC found",
			device: &resourceapi.Device{
				Name: "dev1",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrMac: {StringValue: ptr.To("00:11:22:33:44:55")},
				},
			},
			instance: &mockCloudInstance{
				deviceAttributes: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"00:11:22:33:44:55": {
						gce.AttrGCENetworkName: {StringValue: ptr.To("test-network")},
						gce.AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
					},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				gce.AttrGCENetworkName: {StringValue: ptr.To("test-network")},
				gce.AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			// The interface name (from the ifName attribute) must reach
			// GetDeviceAttributes, not the DRA device name. They differ when the
			// interface name is not a DNS-1123 label (e.g. it has capital letters),
			// so the device name is a normalized "net-<hash>".
			name: "interface name is passed, not the normalized device name",
			device: &resourceapi.Device{
				Name: "net-mr2w23lzkjqws3bq", // normalized iface name
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrInterfaceName: {StringValue: ptr.To("enP22s22f0np0")},
				},
			},
			instance: &mockCloudInstance{
				deviceAttributes: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"enP22s22f0np0": {
						gce.AttrGCENetworkName: {StringValue: ptr.To("test-network")},
					},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				gce.AttrGCENetworkName: {StringValue: ptr.To("test-network")},
			},
		},
		{
			name: "Device Name used (future proofing)",
			device: &resourceapi.Device{
				Name: "dev-pci-1", // PCI device without MAC
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:01.0")},
				},
			},
			instance: &mockCloudInstance{
				// Mock implementation currently only checks MAC, so this returns nil
				// This test case ensures we can pass non-MAC devices without crashing
				deviceAttributes: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			},
			want: nil,
		},
	}

	db := &DB{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := db.getProviderAttributes(tt.device, tt.instance)
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("getProviderAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestAddPCIAttributes(t *testing.T) {
	pciInfo := &ghw.PCIInfo{
		Devices: []*ghw.PCIDevice{
			{
				Address:   "0000:00:1f.0",
				Class:     &pcidb.Class{ID: "02"},
				Vendor:    &pcidb.Vendor{Name: "Intel Corp."},
				Product:   &pcidb.Product{Name: "Ethernet Controller", ID: "0x1234"},
				Subsystem: &pcidb.Product{ID: "0x5678"},
				Node:      &topology.Node{ID: 0},
			},
		},
	}

	t.Run("enriches device missing attributes from ghw", func(t *testing.T) {
		devices := []resourceapi.Device{
			{
				Name: "0000:00:1f.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
				},
			},
		}
		db := &DB{}
		result := db.addPCIAttributes(devices, pciInfo)

		if v, ok := result[0].Attributes[apis.AttrNUMANode]; !ok || v.IntValue == nil || *v.IntValue != 0 {
			t.Errorf("AttrNUMANode not set from ghw, got: %v", v)
		}
		if v, ok := result[0].Attributes[apis.AttrPCIVendor]; !ok || v.StringValue == nil || *v.StringValue != "Intel Corp." {
			t.Errorf("AttrPCIVendor not set from ghw, got: %v", v)
		}
		if v, ok := result[0].Attributes[apis.AttrPCIDevice]; !ok || v.StringValue == nil || *v.StringValue != "Ethernet Controller" {
			t.Errorf("AttrPCIDevice not set from ghw, got: %v", v)
		}
		if v, ok := result[0].Attributes[apis.AttrPCISubsystem]; !ok || v.StringValue == nil || *v.StringValue != "0x5678" {
			t.Errorf("AttrPCISubsystem not set from ghw, got: %v", v)
		}
	})

	t.Run("truncates long product name", func(t *testing.T) {
		longName := strings.Repeat("a", resourceapi.DeviceAttributeMaxValueLength+10)
		longPCIInfo := &ghw.PCIInfo{
			Devices: []*ghw.PCIDevice{
				{
					Address: "0000:00:1f.0",
					Class:   &pcidb.Class{ID: "02"},
					Product: &pcidb.Product{Name: longName, ID: "0x1234"},
				},
			},
		}
		devices := []resourceapi.Device{
			{
				Name: "0000:00:1f.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
				},
			},
		}
		db := &DB{}
		result := db.addPCIAttributes(devices, longPCIInfo)

		v, ok := result[0].Attributes[apis.AttrPCIDevice]
		if !ok || v.StringValue == nil {
			t.Fatalf("AttrPCIDevice not set, got: %v", v)
		}
		if len(*v.StringValue) != resourceapi.DeviceAttributeMaxValueLength {
			t.Errorf("AttrPCIDevice length = %d, want %d", len(*v.StringValue), resourceapi.DeviceAttributeMaxValueLength)
		}
		if *v.StringValue != longName[:resourceapi.DeviceAttributeMaxValueLength] {
			t.Errorf("AttrPCIDevice = %q, want prefix of long name", *v.StringValue)
		}
	})

	t.Run("truncates long vendor name", func(t *testing.T) {
		longName := strings.Repeat("b", resourceapi.DeviceAttributeMaxValueLength+10)
		longPCIInfo := &ghw.PCIInfo{
			Devices: []*ghw.PCIDevice{
				{
					Address: "0000:00:1f.0",
					Class:   &pcidb.Class{ID: "02"},
					Vendor:  &pcidb.Vendor{Name: longName},
				},
			},
		}
		devices := []resourceapi.Device{
			{
				Name: "0000:00:1f.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
				},
			},
		}
		db := &DB{}
		result := db.addPCIAttributes(devices, longPCIInfo)

		v, ok := result[0].Attributes[apis.AttrPCIVendor]
		if !ok || v.StringValue == nil {
			t.Fatalf("AttrPCIVendor not set, got: %v", v)
		}
		if len(*v.StringValue) != resourceapi.DeviceAttributeMaxValueLength {
			t.Errorf("AttrPCIVendor length = %d, want %d", len(*v.StringValue), resourceapi.DeviceAttributeMaxValueLength)
		}
		if *v.StringValue != longName[:resourceapi.DeviceAttributeMaxValueLength] {
			t.Errorf("AttrPCIVendor = %q, want prefix of long name", *v.StringValue)
		}
	})

	t.Run("does not overwrite existing attributes", func(t *testing.T) {
		devices := []resourceapi.Device{
			{
				Name: "0000:00:1f.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
					apis.AttrNUMANode:   {IntValue: ptr.To(int64(99))},
				},
			},
		}
		db := &DB{}
		result := db.addPCIAttributes(devices, pciInfo)

		if v, ok := result[0].Attributes[apis.AttrNUMANode]; !ok || v.IntValue == nil || *v.IntValue != 99 {
			t.Errorf("AttrNUMANode overwritten, expected 99, got: %v", v)
		}
	})

	t.Run("skips devices without PCIAddress", func(t *testing.T) {
		devices := []resourceapi.Device{
			{
				Name:       "eth0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			},
		}
		db := &DB{}
		result := db.addPCIAttributes(devices, pciInfo)

		if len(result[0].Attributes) != 0 {
			t.Errorf("expected no attributes added for non-PCI device, got: %v", result[0].Attributes)
		}
	})

	t.Run("nil pciInfo does not panic", func(t *testing.T) {
		devices := []resourceapi.Device{
			{
				Name: "0000:00:1f.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
				},
			},
		}
		db := &DB{}
		_ = db.addPCIAttributes(devices, nil)
	})

	t.Run("publishes standardized scalar NUMA attribute by default", func(t *testing.T) {
		root := t.TempDir()
		writeTestSysfsFile(t, root, "bus/pci/devices/0000:00:1f.0/numa_node", "0\n")
		devices := []resourceapi.Device{{
			Name: "pci-0000-00-1f-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
			},
		}}
		db := &DB{
			deviceAttributeMachineModifiers: []deviceattribute.MachineModifier{deviceattribute.WithFSFromRoot(root)},
		}

		result := db.addPCIAttributes(devices, pciInfo)
		got, ok := result[0].Attributes[deviceattribute.StandardDeviceAttributeNUMANode]
		if !ok || got.IntValue == nil || *got.IntValue != 0 {
			t.Fatalf("standardized NUMA attribute = %v, want scalar 0", got)
		}
		if got.IntValues != nil {
			t.Errorf("standardized NUMA attribute has list value %v in scalar mode", got.IntValues)
		}
	})

	t.Run("publishes standardized list NUMA attribute", func(t *testing.T) {
		root := t.TempDir()
		writeTestSysfsFile(t, root, "bus/pci/devices/0000:00:1f.0/numa_node", "1\n")
		writeTestSysfsFile(t, root, "devices/system/node/node1/distance", "12 10 12\n")
		devices := []resourceapi.Device{{
			Name: "pci-0000-00-1f-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:1f.0")},
			},
		}}
		db := &DB{
			numaAttributeForm: deviceattribute.ListAttribute,
			deviceAttributeMachineModifiers: []deviceattribute.MachineModifier{
				deviceattribute.WithFSFromRoot(root),
			},
		}

		result := db.addPCIAttributes(devices, pciInfo)
		got, ok := result[0].Attributes[deviceattribute.StandardDeviceAttributeNUMANode]
		if !ok {
			t.Fatal("standardized NUMA attribute was not published")
		}
		if diff := cmp.Diff([]int64{1, 0, 2}, got.IntValues); diff != "" {
			t.Errorf("standardized NUMA list mismatch (-want, +got):\n%s", diff)
		}
		if got.IntValue != nil {
			t.Errorf("standardized NUMA attribute has scalar value %d in list mode", *got.IntValue)
		}
	})

	t.Run("publishes standardized NUMA attribute without ghw metadata", func(t *testing.T) {
		root := t.TempDir()
		writeTestSysfsFile(t, root, "bus/pci/devices/0000:01:00.0/numa_node", "2\n")
		devices := []resourceapi.Device{{
			Name: "pci-0000-01-00-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:01:00.0")},
			},
		}}
		db := &DB{
			deviceAttributeMachineModifiers: []deviceattribute.MachineModifier{deviceattribute.WithFSFromRoot(root)},
		}

		result := db.addPCIAttributes(devices, nil)
		got, ok := result[0].Attributes[deviceattribute.StandardDeviceAttributeNUMANode]
		if !ok || got.IntValue == nil || *got.IntValue != 2 {
			t.Fatalf("standardized NUMA attribute = %v, want scalar 2", got)
		}
		if _, ok := result[0].Attributes[apis.AttrNUMANode]; ok {
			t.Error("legacy NUMA attribute should require ghw metadata")
		}
	})
}

func writeTestSysfsFile(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fake sysfs directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fake sysfs file: %v", err)
	}
}

func TestAddRDMAAttributesStandalone(t *testing.T) {
	t.Run("uses existing AttrRDMADevice without rdmamap lookup", func(t *testing.T) {
		devices := []resourceapi.Device{
			{
				Name: "0000:1b:00.0",
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:1b:00.0")},
					apis.AttrRDMADevice: {StringValue: ptr.To("erdma_0")},
				},
			},
		}
		db := &DB{}
		result := db.addRDMAAttributes(devices)

		if v, ok := result[0].Attributes[apis.AttrRDMA]; !ok || v.BoolValue == nil || !*v.BoolValue {
			t.Errorf("AttrRDMA = %v, want true", v)
		}
		if v, ok := result[0].Attributes[apis.AttrRDMADevice]; !ok || v.StringValue == nil || *v.StringValue != "erdma_0" {
			t.Errorf("AttrRDMADevice = %v, want erdma_0 (should not be overwritten)", v)
		}
	})

	t.Run("netdev-linked device uses existing AttrRDMADevice without lookup", func(t *testing.T) {
		// A value already resolved elsewhere must be preserved: add*Attributes
		// only fills missing attributes.
		ifName := "dranettest1"
		devices := []resourceapi.Device{
			{
				Name: ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrInterfaceName: {StringValue: ptr.To(ifName)},
					apis.AttrRDMADevice:    {StringValue: ptr.To("bnxt_re12")},
				},
			},
		}
		db := &DB{}
		result := db.addRDMAAttributes(devices)

		if v, ok := result[0].Attributes[apis.AttrRDMA]; !ok || v.BoolValue == nil || !*v.BoolValue {
			t.Errorf("AttrRDMA = %v, want true", v)
		}
		if v, ok := result[0].Attributes[apis.AttrRDMADevice]; !ok || v.StringValue == nil || *v.StringValue != "bnxt_re12" {
			t.Errorf("AttrRDMADevice = %v, want bnxt_re12 (should not be overwritten)", v)
		}
	})

	t.Run("netdev without RDMA device is rdma=false", func(t *testing.T) {
		// A name that will not resolve via rdmamap or sysfs.
		ifName := "dranet-nonexistent-iface"
		devices := []resourceapi.Device{
			{
				Name: ifName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrInterfaceName: {StringValue: ptr.To(ifName)},
					apis.AttrPCIAddress:    {StringValue: ptr.To("0000:1c:00.0")},
				},
			},
		}
		db := &DB{}
		result := db.addRDMAAttributes(devices)

		if v, ok := result[0].Attributes[apis.AttrRDMA]; !ok || v.BoolValue == nil || *v.BoolValue {
			t.Errorf("AttrRDMA = %v, want false", v)
		}
		if _, ok := result[0].Attributes[apis.AttrRDMADevice]; ok {
			t.Errorf("AttrRDMADevice should not be set for a non-RDMA netdev")
		}
	})
}

func TestIsIBOnlyDevice(t *testing.T) {
	// A synthetic sysfs holding one RoCE VF (network class, Ethernet link
	// layer), one IB HCA (network class, InfiniBand link layer), one VPI
	// adapter (network class, InfiniBand on port 1 and Ethernet on port 2) and
	// one eRDMA device (not a network class).
	root := t.TempDir()
	ibRoot := filepath.Join(root, "class/infiniband")
	pciRoot := filepath.Join(root, "bus/pci/devices")
	writeTestSysfsFile(t, root, "class/infiniband/rocep231s0/ports/1/link_layer", "Ethernet\n")
	writeTestSysfsFile(t, root, "bus/pci/devices/0000:e7:00.0/class", "0x020000\n")
	writeTestSysfsFile(t, root, "class/infiniband/mlx5_0/ports/1/link_layer", "InfiniBand\n")
	writeTestSysfsFile(t, root, "bus/pci/devices/0000:1a:00.0/class", "0x020700\n")
	writeTestSysfsFile(t, root, "class/infiniband/mlx4_0/ports/1/link_layer", "InfiniBand\n")
	writeTestSysfsFile(t, root, "class/infiniband/mlx4_0/ports/2/link_layer", "Ethernet\n")
	writeTestSysfsFile(t, root, "bus/pci/devices/0000:1c:00.0/class", "0x028000\n")
	writeTestSysfsFile(t, root, "class/infiniband/erdma_0/ports/1/link_layer", "Ethernet\n")
	writeTestSysfsFile(t, root, "bus/pci/devices/0000:1b:00.0/class", "0x00ff00\n")

	device := func(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) *DB {
		return &DB{
			deviceStore:    map[string]resourceapi.Device{"dev": {Name: "dev", Attributes: attrs}},
			infinibandPath: ibRoot,
			pciDevicePath:  pciRoot,
		}
	}

	testCases := []struct {
		name  string
		attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
		want  bool
	}{
		{
			name: "RoCE VF whose netdev is in a pod namespace is not IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice: {StringValue: ptr.To("rocep231s0")},
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:e7:00.0")},
			},
			want: false,
		},
		{
			name: "RoCE VF with its netdev on the host is not IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice:    {StringValue: ptr.To("rocep231s0")},
				apis.AttrPCIAddress:    {StringValue: ptr.To("0000:e7:00.0")},
				apis.AttrInterfaceName: {StringValue: ptr.To("enp231s0")},
			},
			want: false,
		},
		{
			name: "IB HCA with no netdev is IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice: {StringValue: ptr.To("mlx5_0")},
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:1a:00.0")},
			},
			want: true,
		},
		{
			name: "VPI adapter with an Ethernet second port in a pod namespace is not IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice: {StringValue: ptr.To("mlx4_0")},
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:1c:00.0")},
			},
			want: false,
		},
		{
			name: "eRDMA device is IB-only although its link layer is Ethernet",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice: {StringValue: ptr.To("erdma_0")},
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:1b:00.0")},
			},
			want: true,
		},
		{
			name: "RDMA device with no PCI address is IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrRDMADevice: {StringValue: ptr.To("erdma_0")},
			},
			want: true,
		},
		{
			name: "device without RDMA capability is not IB-only",
			attrs: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:e7:00.0")},
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := device(tc.attrs).IsIBOnlyDevice("dev"); got != tc.want {
				t.Errorf("IsIBOnlyDevice() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unknown device is not IB-only", func(t *testing.T) {
		if (&DB{}).IsIBOnlyDevice("absent") {
			t.Error("IsIBOnlyDevice() = true for a device that is not in the store")
		}
	})
}
