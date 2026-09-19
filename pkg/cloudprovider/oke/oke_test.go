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

package oke

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

// fakeAddressTable holds the addresses that fakeSysfs serves per interface.
// An interface without an entry returns a read error.
var fakeAddressTable map[string][]string

// fakeSysfs points the sysfs, sysctl, and OCA paths at a temporary directory
// and the address reader at fakeAddressTable for one test.
func fakeSysfs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	originalClassNet, originalPCIDevices, originalNetnsMode := sysClassNet, sysfsPCIDevices, ibCoreNetnsModeFile
	originalSysctl, originalOCADir, originalAddresses := procSysNetIPv4Conf, ocaConfigDir, interfaceAddresses
	sysClassNet = filepath.Join(dir, "class", "net")
	sysfsPCIDevices = filepath.Join(dir, "bus", "pci", "devices")
	ibCoreNetnsModeFile = filepath.Join(dir, "module", "ib_core", "netns_mode")
	procSysNetIPv4Conf = filepath.Join(dir, "proc", "ipv4", "conf")
	ocaConfigDir = filepath.Join(dir, "oca")
	fakeAddressTable = map[string][]string{}
	interfaceAddresses = func(ifName string) ([]net.Addr, error) {
		cidrs, ok := fakeAddressTable[ifName]
		if !ok {
			return nil, fmt.Errorf("interface %s has no fake addresses", ifName)
		}
		var addresses []net.Addr
		for _, cidr := range cidrs {
			ip, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, err
			}
			addresses = append(addresses, &net.IPNet{IP: ip, Mask: ipNet.Mask})
		}
		return addresses, nil
	}
	t.Cleanup(func() {
		sysClassNet, sysfsPCIDevices, ibCoreNetnsModeFile = originalClassNet, originalPCIDevices, originalNetnsMode
		procSysNetIPv4Conf, ocaConfigDir, interfaceAddresses = originalSysctl, originalOCADir, originalAddresses
		fakeAddressTable = nil
	})
}

// fakeInterface adds an interface with the given ARPHRD type to the fake sysfs.
// A non-empty pciAddress also links the interface to that PCI device.
func fakeInterface(t *testing.T, ifName, pciAddress string, hardwareType int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(sysClassNet, ifName), 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysClassNet, ifName, "type"), []byte(strconv.Itoa(hardwareType)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
	if pciAddress == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
}

// fakeNetnsMode writes the ib_core netns_mode parameter.
func fakeNetnsMode(t *testing.T, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(ibCoreNetnsModeFile), 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
	if err := os.WriteFile(ibCoreNetnsModeFile, []byte(value+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
}

// fakeInterfaceAddresses serves the CIDRs as the addresses of ifName. No
// CIDR means an interface without addresses.
func fakeInterfaceAddresses(t *testing.T, ifName string, cidrs ...string) {
	t.Helper()
	fakeAddressTable[ifName] = append([]string{}, cidrs...)
}

// fakeARP writes the ARP sysctls of ifName. An empty value skips the file.
func fakeARP(t *testing.T, ifName, arpIgnore, arpAnnounce string) {
	t.Helper()
	dir := filepath.Join(procSysNetIPv4Conf, ifName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
	for name, value := range map[string]string{"arp_ignore": arpIgnore, "arp_announce": arpAnnounce} {
		if value == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() returned error: %v", err)
		}
	}
}

// fakeOCAFile writes one OCA configuration file into the fake directory.
func fakeOCAFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(ocaConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ocaConfigDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
}

// Primary VNIC of the B4.8 test node 10.140.77.19 in subnet 10.140.64.0/19.
func testVNIC() *primaryVNIC {
	return &primaryVNIC{IPv4: netip.MustParseAddr("10.140.77.19"), Subnet: netip.MustParsePrefix("10.140.64.0/19")}
}

// compareVNIC lets cmp compare the netip values inside primaryVNIC.
var compareVNIC = cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})

// fakeRDMANic describes one more interface on the node under test.
type fakeRDMANic struct {
	ifName       string
	hardwareType int
	addresses    []string
}

// arpValue maps a test row value to the sysctl file content: "" keeps the
// OCA default, "-" leaves the file missing.
func arpValue(value, defaultValue string) string {
	switch value {
	case "":
		return defaultValue
	case "-":
		return ""
	default:
		return value
	}
}

// The profile resolution of RDMA NIC 0 on the test node with the OCA ARP policy.
var testRDMA0Config = &apis.NetworkConfig{
	Interface: apis.InterfaceConfig{
		Type:        apis.InterfaceTypeIPVLAN,
		Addresses:   []string{"10.222.13.19/15"},
		ARPIgnore:   ptr.To[int32](1),
		ARPAnnounce: ptr.To[int32](2),
	},
	Routes: []apis.RouteConfig{{Destination: "10.222.0.0/15", Source: "10.222.13.19", Scope: unix.RT_SCOPE_LINK, Table: 100}},
	Rules:  []apis.RuleConfig{{Priority: apis.SourceRoutingRulePriority, Source: "10.222.13.19/32", Table: 100}},
}

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		instance *OKEInstance
		id       cloudprovider.DeviceIdentifiers
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "full topology with gpu memory fabric and shape (GB200/GB300 shapes)",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:     "fake-island-id",
				NetworkBlockId:  "fake-network-block-id",
				LocalBlockId:    "fake-local-block-id",
				RackId:          "fake-rack-id",
				GpuMemoryFabric: "fake-gpu-memory-fabric-id",
				Shape:           "BM.GPU.GB200.4",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:     {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId:  {StringValue: ptr.To("fake-network-block-id")},
				AttrOKELocalBlockId:    {StringValue: ptr.To("fake-local-block-id")},
				AttrOKERackId:          {StringValue: ptr.To("fake-rack-id")},
				AttrOKEGpuMemoryFabric: {StringValue: ptr.To("fake-gpu-memory-fabric-id")},
				AttrOKEShape:           {StringValue: ptr.To("BM.GPU.GB200.4")},
			},
		},
		{
			name: "H100 fallback: only networkBlockId and rackId (no rdmaTopologyData)",
			instance: newOKEInstance(&okeMetadata{
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
				AttrOKERackId:         {StringValue: ptr.To("fake-rack-id")},
			},
		},
		{
			name: "partial topology (only hpcIslandId and networkBlockId)",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:    {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
			},
		},
		{
			name:     "no metadata",
			instance: newOKEInstance(nil, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want:     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		{
			name: "attributes are node-level, same for any device identifier",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{
				Name:       "pci-0000-0c-00-0",
				MAC:        "a0:88:c2:a7:c5:04",
				PCIAddress: "0000:0c:00.0",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:    {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
				AttrOKERackId:         {StringValue: ptr.To("fake-rack-id")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(tt.id)
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestGetDeviceAttributesRDMAFabric(t *testing.T) {
	tests := []struct {
		name     string
		instance *OKEInstance
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name:     "IPv4 fabric with zero planes",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(false)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](0)},
			},
		},
		{
			name:     "IPv6 fabric with multiple planes",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](8)},
			},
		},
		{
			name:     "fabric data is absent",
			instance: newOKEInstance(&okeMetadata{RackId: "fake-rack-id"}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERackId: {StringValue: ptr.To("fake-rack-id")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestOCIDSuffix(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "valid OCID extracts 60-char suffix",
			input: "ocid1.hpcisland.oc1.test-region-1.aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
			want:  "aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
		},
		{
			name:  "OCID with suffix longer than 60 chars is truncated to last 60",
			input: "ocid1.hpcisland.oc1.test-region-1.xaaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
			want:  "aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
		},
		{
			name:  "empty string returns empty (field not present on shape)",
			input: "",
			want:  "",
		},
		{
			name:    "non-OCID string returns error",
			input:   "fakehexhash",
			wantErr: true,
		},
		{
			name:    "non-OCID dotted string returns error",
			input:   "some.dotted.value",
			wantErr: true,
		},
		{
			name:    "OCID without dot separator returns error",
			input:   "ocid1-hpcisland-no-dots",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ocidSuffix(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ocidSuffix(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ocidSuffix(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ocidSuffix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetDeviceConfig(t *testing.T) {
	profile := &apis.NetworkConfig{Profile: okeRDMAProfile, Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}
	rdma0ID := cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"}
	tests := []struct {
		name         string
		ifName       string
		pciAddress   string
		hardwareType int
		addresses    []string
		metadata     *okeMetadata
		namedNICs    bool
		id           cloudprovider.DeviceIdentifiers
		want         *apis.NetworkConfig
	}{
		{
			name: "device without a PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
		},
		{
			name:         "Ethernet RDMA NIC gets the profile",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           rdma0ID,
			want:         profile,
		},
		{
			name:         "IPv4 fabric keeps the profile",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			metadata:     &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false}},
			id:           rdma0ID,
			want:         profile,
		},
		{
			name:         "IPv6 fabric gets no profile",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			metadata:     &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true}},
			id:           rdma0ID,
		},
		{
			name:         "unnamed RDMA NIC with an OCA address gets the profile",
			ifName:       "ens3f0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			addresses:    []string{"10.224.13.19/12"},
			id:           cloudprovider.DeviceIdentifiers{Name: "ens3f0", PCIAddress: "0000:0c:00.0"},
			want:         profile,
		},
		{
			name:         "unnamed RDMA NIC is ignored on a node with rdmaN names",
			ifName:       "ens3f0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			addresses:    []string{"10.224.13.19/12"},
			namedNICs:    true,
			id:           cloudprovider.DeviceIdentifiers{Name: "ens3f0", PCIAddress: "0000:0c:00.0"},
		},
		{
			name:         "VCN interface outside the OCA network gets no profile",
			ifName:       "eth0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			addresses:    []string{"10.140.77.19/19"},
			id:           cloudprovider.DeviceIdentifiers{Name: "eth0", PCIAddress: "0000:0c:00.0"},
		},
		{
			name:         "InfiniBand RDMA NIC gets no profile",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_INFINIBAND,
			id:           rdma0ID,
		},
		{
			name:         "Ethernet interface without addresses gets no profile",
			ifName:       "eth0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           cloudprovider.DeviceIdentifiers{Name: "eth0", PCIAddress: "0000:0c:00.0"},
		},
		{
			name:         "unknown PCI address",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           cloudprovider.DeviceIdentifiers{Name: "rdma1", PCIAddress: "0000:0d:00.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			if tt.ifName != "" {
				fakeInterface(t, tt.ifName, tt.pciAddress, tt.hardwareType)
			}
			if tt.addresses != nil {
				fakeInterfaceAddresses(t, tt.ifName, tt.addresses...)
			}
			instance := newOKEInstance(tt.metadata, nil)
			instance.addressFallback = !tt.namedNICs

			if diff := cmp.Diff(tt.want, instance.GetDeviceConfig(tt.id)); diff != "" {
				t.Errorf("GetDeviceConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	ipv4Fabric := func() *okeMetadata {
		return &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false}, PrimaryVNIC: testVNIC()}
	}
	profileConfig := func(iface apis.InterfaceConfig) *apis.NetworkConfig {
		return &apis.NetworkConfig{Profile: okeRDMAProfile, Interface: iface}
	}
	rdma0Config := testRDMA0Config
	rdma0AddressOnly := &apis.NetworkConfig{Interface: rdma0Config.Interface}
	knownClassic := &ocaRDMAConfig{
		parentIPv4Range: netip.MustParsePrefix(okeRDMAParentIPv4CIDR),
		ipv4AddressMode: ocaIPv4AddressModeClassic,
	}
	rdma15Config := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			Type:        apis.InterfaceTypeIPVLAN,
			Addresses:   []string{"10.223.237.19/15"},
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
		},
		Routes: []apis.RouteConfig{{Destination: "10.222.0.0/15", Source: "10.223.237.19", Scope: unix.RT_SCOPE_LINK, Table: 115}},
		Rules:  []apis.RuleConfig{{Priority: apis.SourceRoutingRulePriority, Source: "10.223.237.19/32", Table: 115}},
	}

	tests := []struct {
		name         string
		config       *apis.NetworkConfig
		metadata     *okeMetadata
		ocaConfig    *ocaRDMAConfig
		ifName       string
		hardwareType int
		// addresses is nil for an interface whose addresses cannot be read.
		addresses   []string
		arpIgnore   string
		arpAnnounce string
		extra       []fakeRDMANic
		want        *apis.NetworkConfig
		wantErr     string
	}{
		{
			name:      "first RDMA NIC",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0Config,
		},
		{
			name:      "last RDMA NIC",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma15",
			addresses: []string{"10.225.237.19/12"},
			want:      rdma15Config,
		},
		{
			name:      "explicit IPVLAN type",
			config:    profileConfig(apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0Config,
		},
		{
			name:     "explicit passthrough is rejected",
			config:   profileConfig(apis.InterfaceConfig{Type: apis.InterfaceTypePassthrough}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "does not move an RDMA NIC into a pod",
		},
		{
			name:     "passthrough with DHCP is rejected as passthrough",
			config:   profileConfig(apis.InterfaceConfig{Type: apis.InterfaceTypePassthrough, Addressing: apis.AddressingModeDHCP}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "does not move an RDMA NIC into a pod",
		},
		{
			name:     "unknown profile with explicit passthrough is rejected",
			config:   &apis.NetworkConfig{Profile: "oke-rdmma", Interface: apis.InterfaceConfig{Type: apis.InterfaceTypePassthrough}},
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  `unsupported OKE profile "oke-rdmma"`,
		},
		{
			name:        "claim ARP values replace missing parent values",
			config:      profileConfig(apis.InterfaceConfig{ARPIgnore: ptr.To[int32](2), ARPAnnounce: ptr.To[int32](1)}),
			metadata:    ipv4Fabric(),
			ifName:      "rdma0",
			addresses:   []string{"10.224.13.19/12"},
			arpIgnore:   "-",
			arpAnnounce: "-",
			want: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Type:        apis.InterfaceTypeIPVLAN,
					Addresses:   []string{"10.222.13.19/15"},
					ARPIgnore:   ptr.To[int32](2),
					ARPAnnounce: ptr.To[int32](1),
				},
				Routes: rdma0Config.Routes,
				Rules:  rdma0Config.Rules,
			},
		},
		{
			name:        "claim arpIgnore does not cover a missing parent arp_announce",
			config:      profileConfig(apis.InterfaceConfig{ARPIgnore: ptr.To[int32](2)}),
			metadata:    ipv4Fabric(),
			ifName:      "rdma0",
			addresses:   []string{"10.224.13.19/12"},
			arpIgnore:   "-",
			arpAnnounce: "-",
			wantErr:     "could not read interface.arpAnnounce from host interface rdma0; set interface.arpAnnounce in the claim",
		},
		{
			name:      "other RDMA NIC without an address is skipped",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra:     []fakeRDMANic{{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{}}},
			want:      rdma0Config,
		},
		{
			name:      "static addressing without addresses",
			config:    profileConfig(apis.InterfaceConfig{Addressing: apis.AddressingModeStatic}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0Config,
		},
		{
			name:      "dhcp false is a static request",
			config:    profileConfig(apis.InterfaceConfig{DHCP: ptr.To(false)}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0Config,
		},
		{
			name:     "dhcp addressing is rejected",
			config:   profileConfig(apis.InterfaceConfig{Addressing: apis.AddressingModeDHCP}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "does not support DHCP",
		},
		{
			name:     "unnumbered addressing is rejected",
			config:   profileConfig(apis.InterfaceConfig{Addressing: apis.AddressingModeUnnumbered}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "does not support unnumbered",
		},
		{
			name:     "user addresses are rejected",
			config:   profileConfig(apis.InterfaceConfig{Addresses: []string{"10.222.13.19/15"}}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "assigns the child address",
		},
		{
			name:      "user routes suppress the provider routing",
			config:    &apis.NetworkConfig{Profile: okeRDMAProfile, Routes: []apis.RouteConfig{{Destination: "10.0.0.0/8", Gateway: "10.224.0.1"}}},
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0AddressOnly,
		},
		{
			name:      "user rules suppress the provider routing",
			config:    &apis.NetworkConfig{Profile: okeRDMAProfile, Rules: []apis.RuleConfig{{Priority: 100, Source: "10.222.13.19/32", Table: 200}}},
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0AddressOnly,
		},
		{
			name:      "user VRF suppresses the provider routing",
			config:    profileConfig(apis.InterfaceConfig{VRF: &apis.VRFConfig{Name: "RDMA NIC"}}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0AddressOnly,
		},
		{
			name:     "unknown profile",
			config:   &apis.NetworkConfig{Profile: "gce-managed"},
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  `unsupported OKE profile "gce-managed"`,
		},
		{
			name:    "missing fabric data",
			config:  profileConfig(apis.InterfaceConfig{}),
			ifName:  "rdma0",
			wantErr: "fabric data is not available yet",
		},
		{
			name:     "IPv6 fabric",
			config:   profileConfig(apis.InterfaceConfig{}),
			metadata: &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true}, PrimaryVNIC: testVNIC()},
			ifName:   "rdma0",
			wantErr:  "IPv6 RDMA fabric",
		},
		{
			name:     "missing primary VNIC",
			config:   profileConfig(apis.InterfaceConfig{}),
			metadata: &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false}},
			ifName:   "rdma0",
			wantErr:  "primary VNIC metadata is not available yet",
		},
		{
			name:      "topology address mode",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ocaConfig: &ocaRDMAConfig{parentIPv4Range: netip.MustParsePrefix(okeRDMAParentIPv4CIDR), ipv4AddressMode: ocaIPv4AddressModeTopology},
			ifName:    "rdma0",
			wantErr:   `address mode "topology"`,
		},
		{
			name:      "host-serial address mode",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ocaConfig: &ocaRDMAConfig{parentIPv4Range: netip.MustParsePrefix(okeRDMAParentIPv4CIDR), ipv4AddressMode: ocaIPv4AddressModeSerial},
			ifName:    "rdma0",
			wantErr:   `address mode "host-serial"`,
		},
		{
			name:         "native InfiniBand parent",
			config:       profileConfig(apis.InterfaceConfig{}),
			metadata:     ipv4Fabric(),
			ifName:       "rdma0",
			hardwareType: unix.ARPHRD_INFINIBAND,
			wantErr:      "requires an Ethernet parent",
		},
		{
			name:      "RDMA NIC name and address disagree",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma1",
			addresses: []string{"10.224.13.19/12"},
			wantErr:   "interface rdma1 has address 10.224.13.19, but the derived OCA address is 10.224.45.19",
		},
		{
			name:      "address not assigned yet",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{},
			wantErr:   "does not have the expected OCA address 10.224.13.19 yet",
		},
		{
			name:      "link-local address only",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"169.254.10.10/16"},
			wantErr:   "does not have the expected OCA address 10.224.13.19 yet",
		},
		{
			name:     "address read error",
			config:   profileConfig(apis.InterfaceConfig{}),
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "could not read addresses for rdma0",
		},
		{
			name:      "RDMA NIC outside the child range",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma16",
			addresses: []string{"10.226.13.19/12"},
			wantErr:   "outside the child range 10.222.0.0/15",
		},
		{
			name:      "missing arp_ignore",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			arpIgnore: "-",
			wantErr:   "could not read interface.arpIgnore from host interface rdma0; set interface.arpIgnore in the claim",
		},
		{
			name:        "missing arp_announce",
			config:      profileConfig(apis.InterfaceConfig{}),
			metadata:    ipv4Fabric(),
			ifName:      "rdma0",
			addresses:   []string{"10.224.13.19/12"},
			arpAnnounce: "-",
			wantErr:     "could not read interface.arpAnnounce from host interface rdma0; set interface.arpAnnounce in the claim",
		},
		{
			name:        "missing ARP policy",
			config:      profileConfig(apis.InterfaceConfig{}),
			metadata:    ipv4Fabric(),
			ifName:      "rdma0",
			addresses:   []string{"10.224.13.19/12"},
			arpIgnore:   "-",
			arpAnnounce: "-",
			wantErr:     "could not read interface.arpIgnore and interface.arpAnnounce from host interface rdma0",
		},
		{
			name:      "unsupported arp_ignore value",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			arpIgnore: "5",
			wantErr:   "arp_ignore has unsupported value 5",
		},
		{
			name:      "non-default OCA network",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ocaConfig: &ocaRDMAConfig{parentIPv4Range: netip.MustParsePrefix("192.168.0.0/16"), ipv4AddressMode: ocaIPv4AddressModeClassic},
			ifName:    "rdma0",
			addresses: []string{"192.168.13.19/16"},
			want:      rdma0Config,
		},
		{
			name:      "unnamed RDMA NIC resolved by address inversion",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "ens3f0",
			addresses: []string{"10.224.13.19/12"},
			want:      rdma0Config,
		},
		{
			name:      "unnamed last RDMA NIC resolved by address inversion",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "ens3f1",
			addresses: []string{"10.225.237.19/12"},
			want:      rdma15Config,
		},
		{
			name:      "unnamed RDMA NIC without an OCA address yet",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "ens3f0",
			addresses: []string{"10.140.77.19/19"},
			wantErr:   "interface ens3f0 has no address in the OCA RDMA network 10.224.0.0/12 yet",
		},
		{
			name:      "unnamed RDMA NIC with an address of another host",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "ens3f0",
			addresses: []string{"10.224.13.20/12"},
			wantErr:   "address 10.224.13.20 does not match the primary VNIC position 3347 in a /19 subnet",
		},
		{
			name:      "unknown address mode accepts a consistent host layout",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra: []fakeRDMANic{
				{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.45.19/12"}},
				{ifName: "ens3f1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.225.237.19/12"}},
				{ifName: "eth0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.140.77.19/19"}},
				{ifName: "ib0", hardwareType: unix.ARPHRD_INFINIBAND},
			},
			want: rdma0Config,
		},
		{
			name:      "unknown address mode rejects another RDMA NIC with a wrong address",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra:     []fakeRDMANic{{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.20/12"}}},
			wantErr:   "could not validate the OCA classic IPv4 layout: interface rdma1 has address 10.224.13.20, but the derived OCA address is 10.224.45.19",
		},
		{
			name:      "unknown address mode rejects an unnamed RDMA NIC with a wrong remainder",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra:     []fakeRDMANic{{ifName: "ens3f1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.20/12"}}},
			wantErr:   "could not validate the OCA classic IPv4 layout: interface ens3f1: address 10.224.13.20 does not match",
		},
		{
			name:      "unknown address mode accepts another RDMA NIC outside the child range",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra:     []fakeRDMANic{{ifName: "rdma16", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.226.13.19/12"}}},
			want:      rdma0Config,
		},
		{
			name:      "known classic mode checks the claimed RDMA NIC only",
			config:    profileConfig(apis.InterfaceConfig{}),
			metadata:  ipv4Fabric(),
			ocaConfig: knownClassic,
			ifName:    "rdma0",
			addresses: []string{"10.224.13.19/12"},
			extra:     []fakeRDMANic{{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.20/12"}}},
			want:      rdma0Config,
		},
		{
			name:     "nil configuration",
			metadata: ipv4Fabric(),
			ifName:   "rdma0",
			wantErr:  "configuration is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			hardwareType := tt.hardwareType
			if hardwareType == 0 {
				hardwareType = unix.ARPHRD_ETHER
			}
			fakeInterface(t, tt.ifName, "0000:0c:00.0", hardwareType)
			if tt.addresses != nil {
				fakeInterfaceAddresses(t, tt.ifName, tt.addresses...)
			}
			fakeARP(t, tt.ifName, arpValue(tt.arpIgnore, "1"), arpValue(tt.arpAnnounce, "2"))
			for _, extra := range tt.extra {
				fakeInterface(t, extra.ifName, "", extra.hardwareType)
				if extra.addresses != nil {
					fakeInterfaceAddresses(t, extra.ifName, extra.addresses...)
				}
			}

			instance := newOKEInstance(tt.metadata, nil)
			if tt.ocaConfig != nil {
				instance.ocaConfig = *tt.ocaConfig
			}
			id := cloudprovider.DeviceIdentifiers{Name: tt.ifName, PCIAddress: "0000:0c:00.0"}
			got, err := instance.GetProfileConfig(id, nil, tt.config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetProfileConfig() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetProfileConfig() returned error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetProfileConfig() mismatch (-want +got):\n%s", diff)
			}
			if err := instance.ReleaseProfileConfig(id, "claim", got); err != nil {
				t.Errorf("ReleaseProfileConfig() returned error: %v", err)
			}
		})
	}
}

func TestGetProfileConfigValidatesLayoutOnce(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma0", "0000:0c:00.0", unix.ARPHRD_ETHER)
	fakeInterfaceAddresses(t, "rdma0", "10.224.13.19/12")
	fakeARP(t, "rdma0", "1", "2")
	fakeInterface(t, "rdma1", "", unix.ARPHRD_ETHER)
	fakeInterfaceAddresses(t, "rdma1", "10.224.13.20/12")
	instance := newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{}, PrimaryVNIC: testVNIC()}, nil)
	id := cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"}
	config := &apis.NetworkConfig{Profile: okeRDMAProfile}

	// The wrong rdma1 address fails the whole-host check on every call.
	for range 2 {
		if _, err := instance.GetProfileConfig(id, nil, config); err == nil || !strings.Contains(err.Error(), "could not validate the OCA classic IPv4 layout") {
			t.Fatalf("GetProfileConfig() error = %v, want a layout error", err)
		}
	}
	if instance.layoutValidated.Load() {
		t.Fatal("layoutValidated is set after a failed check")
	}

	// A consistent host passes once, and later drift on another RDMA NIC is not rechecked.
	fakeInterfaceAddresses(t, "rdma1", "10.224.45.19/12")
	if _, err := instance.GetProfileConfig(id, nil, config); err != nil {
		t.Fatalf("GetProfileConfig() returned error: %v", err)
	}
	if !instance.layoutValidated.Load() {
		t.Fatal("layoutValidated is not set after a passed check")
	}
	fakeInterfaceAddresses(t, "rdma1", "10.224.13.20/12")
	if _, err := instance.GetProfileConfig(id, nil, config); err != nil {
		t.Fatalf("GetProfileConfig() returned error after the check passed: %v", err)
	}

	// A known address mode never runs the check.
	instance = newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{}, PrimaryVNIC: testVNIC()}, nil)
	instance.ocaConfig.ipv4AddressMode = ocaIPv4AddressModeClassic
	if _, err := instance.GetProfileConfig(id, nil, config); err != nil {
		t.Fatalf("GetProfileConfig() returned error with a known address mode: %v", err)
	}
	if instance.layoutValidated.Load() {
		t.Fatal("layoutValidated is set although the address mode is known")
	}
}

func TestRDMAExclusiveNetnsMode(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "missing parameter", want: false},
		{name: "shared Y", value: "Y", want: false},
		{name: "shared 1", value: "1", want: false},
		{name: "exclusive N", value: "N", want: true},
		{name: "exclusive 0", value: "0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			if tt.value != "" {
				fakeNetnsMode(t, tt.value)
			}
			if got := rdmaExclusiveNetnsMode(); got != tt.want {
				t.Errorf("rdmaExclusiveNetnsMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasRDMANic(t *testing.T) {
	t.Run("named RDMA NIC", func(t *testing.T) {
		fakeSysfs(t)
		instance := newOKEInstance(nil, nil)
		if hasNamedRDMANic() || instance.hasRDMANic() {
			t.Fatal("an RDMA NIC was found without a sysfs directory")
		}
		fakeInterface(t, "eth0", "", unix.ARPHRD_ETHER)
		fakeInterfaceAddresses(t, "eth0", "10.140.77.19/19")
		if hasNamedRDMANic() || instance.hasRDMANic() {
			t.Fatal("an RDMA NIC was found with only the VCN interface")
		}
		fakeInterface(t, "rdma3", "", unix.ARPHRD_INFINIBAND)
		if hasNamedRDMANic() || instance.hasRDMANic() {
			t.Fatal("an RDMA NIC was found with only native InfiniBand")
		}
		fakeInterface(t, "rdma4", "", unix.ARPHRD_ETHER)
		if !hasNamedRDMANic() || !instance.hasRDMANic() {
			t.Fatal("no RDMA NIC was found with an Ethernet rdmaN interface")
		}
	})

	t.Run("unnamed RDMA NIC with an OCA address", func(t *testing.T) {
		fakeSysfs(t)
		instance := newOKEInstance(nil, nil)
		fakeInterface(t, "ens3f0", "", unix.ARPHRD_ETHER)
		fakeInterfaceAddresses(t, "ens3f0")
		if hasNamedRDMANic() || instance.hasRDMANic() {
			t.Fatal("an RDMA NIC was found for an Ethernet interface without addresses")
		}
		fakeInterfaceAddresses(t, "ens3f0", "10.224.13.19/12")
		if hasNamedRDMANic() {
			t.Fatal("hasNamedRDMANic() = true for an unnamed RDMA NIC")
		}
		if !instance.hasRDMANic() {
			t.Fatal("hasRDMANic() = false for an Ethernet interface in the OCA network")
		}
		// A node with rdmaN names does not probe addresses.
		instance.addressFallback = false
		if instance.hasRDMANic() {
			t.Fatal("hasRDMANic() = true for an unnamed RDMA NIC without the address fallback")
		}
	})
}

func TestRDMANicIndex(t *testing.T) {
	tests := []struct {
		ifName    string
		wantIndex int
		wantOK    bool
	}{
		{ifName: "rdma0", wantIndex: 0, wantOK: true},
		{ifName: "rdma15", wantIndex: 15, wantOK: true},
		{ifName: "rdma", wantOK: false},
		{ifName: "rdmax", wantOK: false},
		{ifName: "rdma1x", wantOK: false},
		{ifName: "rdma-1", wantOK: false},
		{ifName: "rdma99999999999999999999", wantOK: false},
		{ifName: "eth0", wantOK: false},
		{ifName: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.ifName, func(t *testing.T) {
			gotIndex, gotOK := rdmaNicIndex(tt.ifName)
			if gotIndex != tt.wantIndex || gotOK != tt.wantOK {
				t.Errorf("rdmaNicIndex(%q) = %d, %v, want %d, %v", tt.ifName, gotIndex, gotOK, tt.wantIndex, tt.wantOK)
			}
		})
	}
}

func TestQueryIMDS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opc/v2/instance/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer Oracle" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		if _, err := w.Write([]byte(`{"shape":"BM.GPU.B4.8"}`)); err != nil {
			t.Errorf("Write() returned error: %v", err)
		}
	}))
	defer server.Close()

	var got imdsInstanceMetadata
	if err := queryIMDS(context.Background(), server.Client(), server.URL+"/opc/v2/instance/", &got); err != nil {
		t.Fatalf("queryIMDS() returned error: %v", err)
	}
	if got.Shape != "BM.GPU.B4.8" {
		t.Errorf("queryIMDS() = %#v, want shape BM.GPU.B4.8", got)
	}
}

func TestQueryIMDSRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	var got imdsInstanceMetadata
	if err := queryIMDS(context.Background(), server.Client(), server.URL, &got); err == nil {
		t.Fatal("queryIMDS() returned no error")
	}
}

func TestRDMAFabricDataFabric(t *testing.T) {
	tests := []struct {
		name string
		data *imdsRDMAFabricData
		want *rdmaFabric
	}{
		{
			name: "absent object",
			data: nil,
			want: nil,
		},
		{
			name: "IPv4 with zero planes",
			data: &imdsRDMAFabricData{IPv6: ptr.To(false), Planes: ptr.To[int64](0)},
			want: &rdmaFabric{IPv6: false, Planes: 0},
		},
		{
			name: "missing IPv6",
			data: &imdsRDMAFabricData{Planes: ptr.To[int64](8)},
			want: nil,
		},
		{
			name: "missing planes",
			data: &imdsRDMAFabricData{IPv6: ptr.To(true)},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.data.fabric()); diff != "" {
				t.Errorf("fabric() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchOKEMetadata(t *testing.T) {
	const shape = `{"shape":"BM.GPU.B4.8"}`
	tests := []struct {
		name             string
		hostResponse     string
		hostStatus       int
		instanceResponse string
		instanceStatus   int
		want             *okeMetadata
		wantErr          bool
	}{
		{
			name:             "embedded fabric data is used",
			hostResponse:     `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false,"planes":0}}`,
			instanceResponse: shape,
			want:             &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8", RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0}},
		},
		{
			name:             "partial fabric data is ignored",
			hostResponse:     `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true}}`,
			instanceResponse: shape,
			want:             &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8"},
		},
		{
			name:             "topology without fabric data",
			hostResponse:     `{"networkBlockId":"network-1","rackId":"rack-1"}`,
			instanceResponse: shape,
			want:             &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8"},
		},
		{
			name:             "host down keeps the shape and returns the host error",
			hostStatus:       http.StatusServiceUnavailable,
			instanceResponse: shape,
			want:             &okeMetadata{Shape: "BM.GPU.B4.8"},
			wantErr:          true,
		},
		{
			// The type mismatch fills networkBlockId before the decoder reports
			// the error. None of the host data may be used.
			name:             "host decode error discards the host data",
			hostResponse:     `{"networkBlockId":"network-1","rackId":5}`,
			instanceResponse: shape,
			want:             &okeMetadata{Shape: "BM.GPU.B4.8"},
			wantErr:          true,
		},
		{
			name:           "instance down keeps the host data",
			hostResponse:   `{"networkBlockId":"network-1","rackId":"rack-1"}`,
			instanceStatus: http.StatusNotFound,
			want:           &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1"},
		},
		{
			name:           "both down returns an error",
			hostStatus:     http.StatusServiceUnavailable,
			instanceStatus: http.StatusServiceUnavailable,
			want:           nil,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respond := func(w http.ResponseWriter, status int, body string) {
				if status != 0 {
					http.Error(w, "not ready", status)
					return
				}
				_, _ = w.Write([]byte(body))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer Oracle" {
					http.Error(w, "missing authorization", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/host/":
					respond(w, tt.hostStatus, tt.hostResponse)
				case "/instance/":
					respond(w, tt.instanceStatus, tt.instanceResponse)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			got, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("fetchOKEMetadata() error = %v, want error %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchOKEMetadataHostTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			// Hold the request until the client gives up.
			<-r.Context().Done()
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.B4.8"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// The timeout applies to each request, so the instance read still happens.
	client := server.Client()
	client.Timeout = 50 * time.Millisecond
	got, err := fetchOKEMetadata(context.Background(), client, server.URL, false)
	if err == nil {
		t.Fatal("fetchOKEMetadata() returned no error for a hanging host endpoint")
	}
	want := &okeMetadata{Shape: "BM.GPU.B4.8"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestFetchOKEMetadataFullTopology(t *testing.T) {
	const (
		islandOCID  = "ocid1.hpcisland.oc1.test-region-1.aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		networkOCID = "ocid1.networkblock.oc1.test-region-1.bbbbbbbb2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		localOCID   = "ocid1.localblock.oc1.test-region-1.cccccccc2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		fabricOCID  = "ocid1.computegpumemoryfabric.oc1.test-region-1.dddddddd2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = fmt.Fprintf(w, `{"networkBlockId":"top-level-network","rackId":"rack-1","rdmaTopologyData":{"customerHPCIslandId":%q,"customerNetworkBlock":%q,"customerLocalBlock":%q,"customerGpuMemoryFabric":%q,"customerHostId":"ocid1.host.oc1..host"},"rdmaFabricData":{"ipv6":true,"planes":2}}`, islandOCID, networkOCID, localOCID, fabricOCID)
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	metadata, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, false)
	if err != nil {
		t.Fatalf("fetchOKEMetadata() returned error: %v", err)
	}

	suffix := func(ocid string) *string {
		return ptr.To(ocid[strings.LastIndex(ocid, ".")+1:])
	}
	// The topology block overrides the top-level networkBlockId.
	want := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		AttrOKEHPCIslandId:      {StringValue: suffix(islandOCID)},
		AttrOKENetworkBlockId:   {StringValue: suffix(networkOCID)},
		AttrOKELocalBlockId:     {StringValue: suffix(localOCID)},
		AttrOKERackId:           {StringValue: ptr.To("rack-1")},
		AttrOKEGpuMemoryFabric:  {StringValue: suffix(fabricOCID)},
		AttrOKEShape:            {StringValue: ptr.To("BM.GPU.GB300.4")},
		AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
		AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](2)},
	}
	got := newOKEInstance(metadata, nil).GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetDeviceAttributes() mismatch (-want +got):\n%s", diff)
	}
}

func TestMetadataFromIMDS(t *testing.T) {
	const (
		networkOCID = "ocid1.networkblock.oc1.test-region-1.bbbbbbbb2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		localOCID   = "ocid1.localblock.oc1.test-region-1.cccccccc2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
	)
	tests := []struct {
		name     string
		host     *imdsHostMetadata
		instance *imdsInstanceMetadata
		want     *okeMetadata
	}{
		{
			name:     "shape only without host data",
			instance: &imdsInstanceMetadata{Shape: "BM.GPU.B4.8"},
			want:     &okeMetadata{Shape: "BM.GPU.B4.8"},
		},
		{
			name: "malformed OCID leaves only that field empty",
			host: &imdsHostMetadata{
				NetworkBlockId: "top-level-network",
				RackId:         "rack-1",
				RDMATopologyData: &imdsHostRDMATopologyData{
					CustomerHPCIslandId:  "not-an-ocid",
					CustomerNetworkBlock: networkOCID,
					CustomerLocalBlock:   localOCID,
				},
				RDMAFabricData: &imdsRDMAFabricData{IPv6: ptr.To(true), Planes: ptr.To[int64](2)},
			},
			instance: &imdsInstanceMetadata{Shape: "BM.GPU.GB300.4"},
			want: &okeMetadata{
				NetworkBlockId: "bbbbbbbb2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
				LocalBlockId:   "cccccccc2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
				RackId:         "rack-1",
				Shape:          "BM.GPU.GB300.4",
				RDMAFabric:     &rdmaFabric{IPv6: true, Planes: 2},
			},
		},
		{
			name: "malformed network block does not fall back to the top-level value",
			host: &imdsHostMetadata{
				NetworkBlockId:   "top-level-network",
				RackId:           "rack-1",
				RDMATopologyData: &imdsHostRDMATopologyData{CustomerNetworkBlock: "not-an-ocid"},
			},
			want: &okeMetadata{RackId: "rack-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, metadataFromIMDS(tt.host, tt.instance)); diff != "" {
				t.Errorf("metadataFromIMDS() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// captureLogs routes klog output at verbosity 2 to a buffer for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	var flags flag.FlagSet
	klog.InitFlags(&flags)
	for name, value := range map[string]string{"v": "2", "logtostderr": "false"} {
		if err := flags.Set(name, value); err != nil {
			t.Fatalf("could not set klog flag %s: %v", name, err)
		}
	}
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	return &buf
}

func TestMetadataFromIMDSLogsIncompleteFabricData(t *testing.T) {
	logs := captureLogs(t)
	got := metadataFromIMDS(&imdsHostMetadata{RackId: "rack-1", RDMAFabricData: &imdsRDMAFabricData{}}, nil)
	klog.Flush()
	if got.RDMAFabric != nil {
		t.Fatalf("metadataFromIMDS() stored fabric %+v from an empty object", *got.RDMAFabric)
	}
	if want := "ipv6 present=false, planes present=false"; !strings.Contains(logs.String(), want) {
		t.Errorf("metadataFromIMDS() logged %q, want it to contain %q", logs.String(), want)
	}
}

func TestMergeMetadata(t *testing.T) {
	// Fresh pointers for every row so a merge cannot mutate shared input.
	ipv4 := func() *rdmaFabric { return &rdmaFabric{IPv6: false, Planes: 0} }
	ipv6 := func() *rdmaFabric { return &rdmaFabric{IPv6: true, Planes: 8} }
	tests := []struct {
		name    string
		current *okeMetadata
		next    *okeMetadata
		want    *okeMetadata
	}{
		{
			name:    "first snapshot is stored as read",
			current: nil,
			next:    &okeMetadata{RackId: "rack-1"},
			want:    &okeMetadata{RackId: "rack-1"},
		},
		{
			name:    "omitted fields keep the last non-empty value",
			current: &okeMetadata{HPCIslandId: "island-1", NetworkBlockId: "network-1", LocalBlockId: "local-1", GpuMemoryFabric: "fabric-1", Shape: "BM.GPU.GB200.4"},
			next:    &okeMetadata{RackId: "rack-1"},
			want:    &okeMetadata{HPCIslandId: "island-1", NetworkBlockId: "network-1", LocalBlockId: "local-1", RackId: "rack-1", GpuMemoryFabric: "fabric-1", Shape: "BM.GPU.GB200.4"},
		},
		{
			name:    "omitted rack id keeps the last value",
			current: &okeMetadata{RackId: "rack-1"},
			next:    &okeMetadata{Shape: "BM.GPU.GB200.4"},
			want:    &okeMetadata{RackId: "rack-1", Shape: "BM.GPU.GB200.4"},
		},
		{
			name:    "changed values are accepted",
			current: &okeMetadata{HPCIslandId: "island-1", NetworkBlockId: "network-1", LocalBlockId: "local-1", RackId: "rack-1", GpuMemoryFabric: "fabric-1", Shape: "BM.GPU.GB200.4"},
			next:    &okeMetadata{HPCIslandId: "island-2", NetworkBlockId: "network-2", LocalBlockId: "local-2", RackId: "rack-2", GpuMemoryFabric: "fabric-2", Shape: "BM.GPU.GB300.4"},
			want:    &okeMetadata{HPCIslandId: "island-2", NetworkBlockId: "network-2", LocalBlockId: "local-2", RackId: "rack-2", GpuMemoryFabric: "fabric-2", Shape: "BM.GPU.GB300.4"},
		},
		{
			name:    "first fabric data is stored",
			current: &okeMetadata{RackId: "rack-1"},
			next:    &okeMetadata{RackId: "rack-1", RDMAFabric: ipv4()},
			want:    &okeMetadata{RackId: "rack-1", RDMAFabric: ipv4()},
		},
		{
			name:    "changed fabric data is accepted",
			current: &okeMetadata{RDMAFabric: ipv4()},
			next:    &okeMetadata{RDMAFabric: ipv6()},
			want:    &okeMetadata{RDMAFabric: ipv6()},
		},
		{
			name:    "fabric data with false and zero replaces the last value",
			current: &okeMetadata{RDMAFabric: ipv6()},
			next:    &okeMetadata{RDMAFabric: ipv4()},
			want:    &okeMetadata{RDMAFabric: ipv4()},
		},
		{
			name:    "identical fabric data is kept",
			current: &okeMetadata{RDMAFabric: ipv4()},
			next:    &okeMetadata{RDMAFabric: ipv4()},
			want:    &okeMetadata{RDMAFabric: ipv4()},
		},
		{
			name:    "omitted fabric data keeps the last value",
			current: &okeMetadata{RDMAFabric: ipv4()},
			next:    &okeMetadata{RackId: "rack-1"},
			want:    &okeMetadata{RackId: "rack-1", RDMAFabric: ipv4()},
		},
		{
			name:    "omitted primary VNIC keeps the last value",
			current: &okeMetadata{PrimaryVNIC: testVNIC()},
			next:    &okeMetadata{RackId: "rack-1"},
			want:    &okeMetadata{RackId: "rack-1", PrimaryVNIC: testVNIC()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, mergeMetadata(tt.current, tt.next), compareVNIC); diff != "" {
				t.Errorf("mergeMetadata() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshMetadataStoresPartialRead(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		NetworkBlockId: "network-1",
		RackId:         "rack-1",
		RDMAFabric:     &rdmaFabric{IPv6: false, Planes: 0},
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{Shape: "BM.GPU.B4.8"}, errors.New("host endpoint is down")
	})

	if err := instance.refreshMetadata(context.Background()); err == nil {
		t.Fatal("refreshMetadata() returned no error for a partial read")
	}
	// The shape is stored, and the host fields stay.
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RackId:         "rack-1",
		Shape:          "BM.GPU.B4.8",
		RDMAFabric:     &rdmaFabric{IPv6: false, Planes: 0},
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshMetadataKeepsValidOCIDOnMalformedRefresh(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{LocalBlockId: "local-1", RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		host := &imdsHostMetadata{
			RackId:           "rack-2",
			RDMATopologyData: &imdsHostRDMATopologyData{CustomerLocalBlock: "not-an-ocid"},
		}
		return metadataFromIMDS(host, nil), nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{LocalBlockId: "local-1", RackId: "rack-2"}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshMetadataKeepsLastKnownDataOnFailure(t *testing.T) {
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RDMAFabric:     &rdmaFabric{IPv6: false, Planes: 0},
	}
	instance := newOKEInstance(want, func(context.Context) (*okeMetadata, error) {
		return nil, errors.New("IMDS is not ready")
	})

	if err := instance.refreshMetadata(context.Background()); err == nil {
		t.Fatal("refreshMetadata() returned no error")
	}
	if instance.metadata.Load() != want {
		t.Fatal("refreshMetadata() replaced the last known metadata after a failure")
	}
}

func TestRefreshLoopPicksUpLateFabricData(t *testing.T) {
	attempts := make(chan int, 2)
	attempt := 0
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		attempt++
		select {
		case attempts <- attempt:
		default:
		}
		if attempt == 1 {
			return &okeMetadata{NetworkBlockId: "network-1"}, nil
		}
		return &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8}}, nil
	})
	instance.refreshInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		select {
		case gotAttempt := <-attempts:
			if gotAttempt != wantAttempt {
				t.Fatalf("refreshLoop() attempt = %d, want %d", gotAttempt, wantAttempt)
			}
		case <-time.After(time.Second):
			t.Fatalf("refreshLoop() did not make attempt %d", wantAttempt)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := instance.metadata.Load(); got != nil && got.RDMAFabric != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := instance.metadata.Load()
	if got == nil || got.NetworkBlockId != "network-1" || got.RDMAFabric == nil || !got.RDMAFabric.IPv6 || got.RDMAFabric.Planes != 8 {
		t.Fatalf("refreshLoop() stored %#v, want network-1 with IPv6 and 8 planes", got)
	}
}

func TestRefreshLoopStopsOnContextCancel(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{RackId: "rack-1"}, nil
	})
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		instance.refreshLoop(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refreshLoop() did not stop after the context was cancelled")
	}
}

func TestRefreshLoopRefreshesPeriodicallyAfterFailure(t *testing.T) {
	attempts := make(chan int, 3)
	attempt := 0
	instance := newOKEInstance(&okeMetadata{RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		attempt++
		select {
		case attempts <- attempt:
		default:
		}
		if attempt == 1 {
			return nil, errors.New("IMDS is not ready")
		}
		return &okeMetadata{RackId: "rack-1"}, nil
	})
	instance.refreshInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	// A failed refresh keeps the last snapshot and does not stop the loop.
	for wantAttempt := 1; wantAttempt <= 3; wantAttempt++ {
		select {
		case gotAttempt := <-attempts:
			if gotAttempt != wantAttempt {
				t.Fatalf("refreshLoop() attempt = %d, want %d", gotAttempt, wantAttempt)
			}
		case <-time.After(time.Second):
			t.Fatalf("refreshLoop() did not make attempt %d", wantAttempt)
		}
	}
	if got := instance.metadata.Load(); got == nil || got.RackId != "rack-1" {
		t.Fatalf("refreshLoop() stored %#v, want rack-1", got)
	}
}

func TestGetDeviceAttributesDuringRefresh(t *testing.T) {
	var fetches atomic.Int64
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		n := fetches.Add(1)
		// The rack and shape change together on every fetch.
		return &okeMetadata{
			RackId:     fmt.Sprintf("rack-%d", n),
			Shape:      fmt.Sprintf("shape-%d", n),
			RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8},
		}, nil
	})
	instance.refreshInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	// waitForSnapshot returns once the stored snapshot differs from previous.
	waitForSnapshot := func(previous *okeMetadata) *okeMetadata {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if current := instance.metadata.Load(); current != previous {
				return current
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("refreshLoop() did not store a new snapshot")
		return nil
	}
	waitForSnapshot(nil)

	// Read while the loop keeps storing new snapshots. Every read must see a
	// snapshot, and every value in a read must come from the same snapshot.
	stop := make(chan struct{})
	observed := make([]atomic.Int64, 4)
	var wg sync.WaitGroup
	for i := range observed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				attributes := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
				rack, ok := attributes[AttrOKERackId]
				if !ok {
					t.Errorf("GetDeviceAttributes() returned no rack after the first snapshot")
					return
				}
				shape := attributes[AttrOKEShape]
				if want := strings.Replace(*rack.StringValue, "rack-", "shape-", 1); *shape.StringValue != want {
					t.Errorf("GetDeviceAttributes() mixed snapshots: rack %q with shape %q", *rack.StringValue, *shape.StringValue)
					return
				}
				observed[i].Add(1)
			}
		}()
	}
	// Wait until every reader has observed a snapshot, then keep them active
	// across at least one more stored snapshot.
	allObserved := func() bool {
		for i := range observed {
			if observed[i].Load() == 0 {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(time.Second)
	for !allObserved() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitForSnapshot(instance.metadata.Load())
	close(stop)
	wg.Wait()
	for i := range observed {
		if observed[i].Load() == 0 {
			t.Errorf("reader %d observed no snapshot", i)
		}
	}
}

func TestStartReturnsAfterFirstHostRead(t *testing.T) {
	tests := []struct {
		name         string
		hostResponse string
		want         *okeMetadata
	}{
		{
			name:         "with fabric data",
			hostResponse: `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":0}}`,
			want:         &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.GB300.4", RDMAFabric: &rdmaFabric{IPv6: true, Planes: 0}},
		},
		{
			name:         "without fabric data",
			hostResponse: `{"networkBlockId":"network-1","rackId":"rack-1"}`,
			want:         &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.GB300.4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hostRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/host/":
					hostRequests.Add(1)
					_, _ = w.Write([]byte(tt.hostResponse))
				case "/instance/":
					_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			fakeSysfs(t)
			instance := newOKEInstance(nil, nil)
			instance.initialRetryInterval = time.Millisecond
			instance.initialWait = 2 * time.Second
			instance.refreshInterval = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startedAt := time.Now()
			got, err := instance.start(ctx, server.Client(), server.URL)
			if err != nil {
				t.Fatalf("start() returned error: %v", err)
			}
			if got != instance {
				t.Fatal("start() did not return the instance")
			}
			// The first successful host read ends the startup poll, with or
			// without fabric data.
			if elapsed := time.Since(startedAt); elapsed >= instance.initialWait {
				t.Fatalf("start() waited %s for metadata", elapsed)
			}
			if got := hostRequests.Load(); got != 1 {
				t.Fatalf("start() made %d host requests, want 1", got)
			}
			if diff := cmp.Diff(tt.want, instance.metadata.Load()); diff != "" {
				t.Errorf("start() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStartReturnsErrorWhenContextIsCancelled(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	fakeSysfs(t)
	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := instance.start(ctx, server.Client(), server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("start() returned error %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("start() returned %v with an error, want nil", got)
	}
}

func TestStartKeepsPollingWhileIMDSIsDown(t *testing.T) {
	var hostRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/host/" {
			hostRequests.Add(1)
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	fakeSysfs(t)
	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 200 * time.Millisecond
	// An hourly background interval keeps the request count to the startup polls.
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if instance.metadata.Load() != nil {
		t.Fatal("start() stored metadata while IMDS was unavailable")
	}
	if got := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"}); len(got) != 0 {
		t.Fatalf("GetDeviceAttributes() = %v without metadata, want none", got)
	}
	// A failing endpoint must keep start() polling for the whole window.
	if got := hostRequests.Load(); got < 2 {
		t.Fatalf("start() made %d host requests before giving up, want at least 2", got)
	}
}

func TestStartKeepsShapeWhileHostIsDown(t *testing.T) {
	var hostRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			hostRequests.Add(1)
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fakeSysfs(t)
	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 200 * time.Millisecond
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	// The shape alone does not end the startup poll.
	if elapsed := time.Since(startedAt); elapsed < instance.initialWait {
		t.Fatalf("start() returned after %s with the host endpoint down", elapsed)
	}
	if got := hostRequests.Load(); got < 2 {
		t.Fatalf("start() made %d host requests before giving up, want at least 2", got)
	}
	want := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		AttrOKEShape: {StringValue: ptr.To("BM.GPU.GB300.4")},
	}
	got := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetDeviceAttributes() mismatch (-want +got):\n%s", diff)
	}
}

func TestStartPicksUpHostAfterShape(t *testing.T) {
	var hostRequests atomic.Int64
	var shapeSeenBeforeHost atomic.Bool
	fakeSysfs(t)
	instance := newOKEInstance(nil, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			if hostRequests.Add(1) < 3 {
				// The earlier attempts stored the shape without host data.
				if m := instance.metadata.Load(); m != nil && m.Shape != "" && m.RackId == "" {
					shapeSeenBeforeHost.Store(true)
				}
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":0}}`))
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 2 * time.Second
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= instance.initialWait {
		t.Fatalf("start() waited %s for metadata", elapsed)
	}
	if !shapeSeenBeforeHost.Load() {
		t.Error("start() did not store the shape while the host endpoint was down")
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.GB300.4", RDMAFabric: &rdmaFabric{IPv6: true, Planes: 0}}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("start() mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshLoopRecoversFromOutage(t *testing.T) {
	var available atomic.Bool
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		if !available.Load() {
			return nil, errors.New("IMDS is not ready")
		}
		return &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "VM.Standard.E5.Flex"}, nil
	})
	instance.refreshInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	// Failed refreshes leave the snapshot empty, and the loop keeps trying.
	time.Sleep(10 * time.Millisecond)
	if instance.metadata.Load() != nil {
		t.Fatal("refreshLoop() stored metadata from a failing fetch")
	}

	available.Store(true)
	deadline := time.Now().Add(time.Second)
	for instance.metadata.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "VM.Standard.E5.Flex"}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("metadata after recovery mismatch (-want +got):\n%s", diff)
	}
}

func TestParsePrimaryVNIC(t *testing.T) {
	tests := []struct {
		name    string
		vnics   []imdsVNICMetadata
		want    *primaryVNIC
		wantErr bool
	}{
		{
			name: "primary VNIC is the first entry",
			vnics: []imdsVNICMetadata{
				{PrivateIP: "10.140.77.19", SubnetCidrBlock: "10.140.64.0/19"},
				{PrivateIP: "10.140.151.224", SubnetCidrBlock: "10.140.128.0/17"},
			},
			want: testVNIC(),
		},
		{
			name:  "subnet is masked",
			vnics: []imdsVNICMetadata{{PrivateIP: "10.140.77.19", SubnetCidrBlock: "10.140.77.0/19"}},
			want:  testVNIC(),
		},
		{name: "empty response", wantErr: true},
		{name: "primary VNIC without an address", vnics: []imdsVNICMetadata{{SubnetCidrBlock: "10.140.64.0/19"}}},
		{name: "IPv6 primary VNIC", vnics: []imdsVNICMetadata{{PrivateIP: "2603:c020::19", SubnetCidrBlock: "2603:c020::/64"}}},
		{name: "IPv6 subnet", vnics: []imdsVNICMetadata{{PrivateIP: "10.140.77.19", SubnetCidrBlock: "2603:c020::/64"}}},
		{name: "invalid address", vnics: []imdsVNICMetadata{{PrivateIP: "10.140.77", SubnetCidrBlock: "10.140.64.0/19"}}, wantErr: true},
		{name: "missing subnet", vnics: []imdsVNICMetadata{{PrivateIP: "10.140.77.19"}}, wantErr: true},
		{name: "invalid subnet", vnics: []imdsVNICMetadata{{PrivateIP: "10.140.77.19", SubnetCidrBlock: "10.140.64.0"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePrimaryVNIC(tt.vnics)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePrimaryVNIC() error = %v, want error %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, compareVNIC); diff != "" {
				t.Errorf("parsePrimaryVNIC() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchOKEMetadataReadsPrimaryVNIC(t *testing.T) {
	const (
		hostResponse  = `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false,"planes":0}}`
		vnicsResponse = `[{"macAddr":"b8:3f:d2:6d:9d:f0","nicIndex":0,"privateIp":"10.140.77.19","subnetCidrBlock":"10.140.64.0/19","vlanTag":0},{"macAddr":"02:00:17:01:e2:81","nicIndex":0,"privateIp":"10.140.151.224","subnetCidrBlock":"10.140.128.0/17","vlanTag":3772}]`
	)
	hostSnapshot := func(vnic *primaryVNIC) *okeMetadata {
		return &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8", RDMAFabric: &rdmaFabric{}, PrimaryVNIC: vnic}
	}
	tests := []struct {
		name           string
		needVNIC       bool
		hostStatus     int
		instanceStatus int
		vnicsResponse  string
		vnicsStatus    int
		want           *okeMetadata
		wantErr        bool
		wantRequested  bool
	}{
		{
			name:          "known primary VNIC is not queried again",
			vnicsResponse: vnicsResponse,
			want:          hostSnapshot(nil),
		},
		{
			name:          "primary VNIC is read",
			needVNIC:      true,
			vnicsResponse: vnicsResponse,
			want:          hostSnapshot(testVNIC()),
			wantRequested: true,
		},
		{
			name:          "missing vnics endpoint keeps the host data and returns an error",
			needVNIC:      true,
			vnicsStatus:   http.StatusNotFound,
			want:          hostSnapshot(nil),
			wantErr:       true,
			wantRequested: true,
		},
		{
			name:          "empty vnics response is an error",
			needVNIC:      true,
			vnicsResponse: `[]`,
			want:          hostSnapshot(nil),
			wantErr:       true,
			wantRequested: true,
		},
		{
			name:          "primary VNIC without an address is an error",
			needVNIC:      true,
			vnicsResponse: `[{"subnetCidrBlock":"10.140.64.0/19"}]`,
			want:          hostSnapshot(nil),
			wantErr:       true,
			wantRequested: true,
		},
		{
			name:          "IPv6 primary VNIC is an error",
			needVNIC:      true,
			vnicsResponse: `[{"privateIp":"2603:c020::19","subnetCidrBlock":"2603:c020::/64"}]`,
			want:          hostSnapshot(nil),
			wantErr:       true,
			wantRequested: true,
		},
		{
			name:           "host and instance down keep the primary VNIC",
			needVNIC:       true,
			hostStatus:     http.StatusServiceUnavailable,
			instanceStatus: http.StatusServiceUnavailable,
			vnicsResponse:  vnicsResponse,
			want:           &okeMetadata{PrimaryVNIC: testVNIC()},
			wantErr:        true,
			wantRequested:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respond := func(w http.ResponseWriter, status int, body string) {
				if status != 0 {
					http.Error(w, "not ready", status)
					return
				}
				_, _ = w.Write([]byte(body))
			}
			var vnicsRequested atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/host/":
					respond(w, tt.hostStatus, hostResponse)
				case "/instance/":
					respond(w, tt.instanceStatus, `{"shape":"BM.GPU.B4.8"}`)
				case "/vnics/":
					vnicsRequested.Store(true)
					respond(w, tt.vnicsStatus, tt.vnicsResponse)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			got, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, tt.needVNIC)
			if (err != nil) != tt.wantErr {
				t.Fatalf("fetchOKEMetadata() error = %v, want error %v", err, tt.wantErr)
			}
			if vnicsRequested.Load() != tt.wantRequested {
				t.Errorf("vnics endpoint requested = %v, want %v", vnicsRequested.Load(), tt.wantRequested)
			}
			if diff := cmp.Diff(tt.want, got, compareVNIC); diff != "" {
				t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseOCARDMANetwork(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{name: "deployed OKE file", data: `{"rdma-network":[{"default-settings":{"rdma_network":"10.224.0.0/12","single_subnet":true},"subnet-settings":{"netmask":"255.240.0.0"}}]}`, want: "10.224.0.0/12"},
		{name: "OCA embedded default", data: `{"rdma-network":[{"default-settings":{"rdma_network":"192.168.0.0/16"}}]}`, want: "192.168.0.0/16"},
		{name: "first entry wins", data: `{"rdma-network":[{"default-settings":{"rdma_network":"10.224.0.0/12"}},{"default-settings":{"rdma_network":"192.168.0.0/16"}}]}`, want: "10.224.0.0/12"},
		{name: "malformed JSON", data: `{"rdma-network":`, wantErr: true},
		{name: "empty list", data: `{"rdma-network":[]}`, wantErr: true},
		{name: "missing field", data: `{"rdma-network":[{"default-settings":{}}]}`, wantErr: true},
		{name: "invalid prefix", data: `{"rdma-network":[{"default-settings":{"rdma_network":"10.224.0.0"}}]}`, wantErr: true},
		{name: "IPv6 prefix", data: `{"rdma-network":[{"default-settings":{"rdma_network":"fd00::/64"}}]}`, wantErr: true},
		{name: "unmasked prefix", data: `{"rdma-network":[{"default-settings":{"rdma_network":"10.224.1.0/12"}}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOCARDMANetwork([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOCARDMANetwork() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.String() != tt.want {
				t.Errorf("parseOCARDMANetwork() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseOCARDMAFeatures(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{name: "classic", data: `{"host_serial_based_rdma_ips":false,"ipv4_topology_based_rdma_ips":false}`, want: ocaIPv4AddressModeClassic},
		{name: "empty object is classic", data: `{}`, want: ocaIPv4AddressModeClassic},
		{name: "host-serial", data: `{"host_serial_based_rdma_ips":true}`, want: ocaIPv4AddressModeSerial},
		{name: "topology", data: `{"ipv4_topology_based_rdma_ips":true}`, want: ocaIPv4AddressModeTopology},
		{name: "host-serial wins over topology", data: `{"host_serial_based_rdma_ips":true,"ipv4_topology_based_rdma_ips":true}`, want: ocaIPv4AddressModeSerial},
		{name: "malformed JSON", data: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOCARDMAFeatures([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOCARDMAFeatures() error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseOCARDMAFeatures() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadOCARDMAConfig(t *testing.T) {
	defaults := defaultOCARDMAConfig()
	t.Run("files are not mounted", func(t *testing.T) {
		fakeSysfs(t)
		if got := loadOCARDMAConfig(ocaConfigDir); got != defaults {
			t.Errorf("loadOCARDMAConfig() = %+v, want %+v", got, defaults)
		}
	})
	t.Run("mounted files override the defaults", func(t *testing.T) {
		fakeSysfs(t)
		fakeOCAFile(t, ocaRDMANetworkFileName, `{"rdma-network":[{"default-settings":{"rdma_network":"192.168.0.0/16"}}]}`)
		fakeOCAFile(t, ocaRDMAFeaturesFileName, `{"ipv4_topology_based_rdma_ips":true}`)
		want := ocaRDMAConfig{parentIPv4Range: netip.MustParsePrefix("192.168.0.0/16"), ipv4AddressMode: ocaIPv4AddressModeTopology}
		if got := loadOCARDMAConfig(ocaConfigDir); got != want {
			t.Errorf("loadOCARDMAConfig() = %+v, want %+v", got, want)
		}
	})
	t.Run("invalid files keep the defaults", func(t *testing.T) {
		fakeSysfs(t)
		fakeOCAFile(t, ocaRDMANetworkFileName, `{"rdma-network":[]}`)
		fakeOCAFile(t, ocaRDMAFeaturesFileName, `{`)
		if got := loadOCARDMAConfig(ocaConfigDir); got != defaults {
			t.Errorf("loadOCARDMAConfig() = %+v, want %+v", got, defaults)
		}
	})
	t.Run("unreadable files keep the defaults", func(t *testing.T) {
		fakeSysfs(t)
		for _, name := range []string{ocaRDMANetworkFileName, ocaRDMAFeaturesFileName} {
			if err := os.MkdirAll(filepath.Join(ocaConfigDir, name), 0o755); err != nil {
				t.Fatalf("MkdirAll() returned error: %v", err)
			}
		}
		if got := loadOCARDMAConfig(ocaConfigDir); got != defaults {
			t.Errorf("loadOCARDMAConfig() = %+v, want %+v", got, defaults)
		}
	})
}

func TestDeriveRDMAIPv4(t *testing.T) {
	defaultRange := netip.MustParsePrefix(okeRDMAParentIPv4CIDR)
	vnicIn := func(subnet string) *primaryVNIC {
		return &primaryVNIC{IPv4: netip.MustParseAddr("10.140.77.19"), Subnet: netip.MustParsePrefix(subnet)}
	}
	tests := []struct {
		name        string
		vnic        *primaryVNIC
		nicIndex    int
		parentRange netip.Prefix
		wantParent  string
		wantChild   string
		wantErr     string
	}{
		{name: "RDMA NIC index 0 on a /19 subnet", vnic: testVNIC(), nicIndex: 0, parentRange: defaultRange, wantParent: "10.224.13.19", wantChild: "10.222.13.19"},
		{name: "RDMA NIC index 1 on a /19 subnet", vnic: testVNIC(), nicIndex: 1, parentRange: defaultRange, wantParent: "10.224.45.19", wantChild: "10.222.45.19"},
		{name: "RDMA NIC index 8 on a /19 subnet", vnic: testVNIC(), nicIndex: 8, parentRange: defaultRange, wantParent: "10.225.13.19", wantChild: "10.223.13.19"},
		{name: "RDMA NIC index 15 on a /19 subnet", vnic: testVNIC(), nicIndex: 15, parentRange: defaultRange, wantParent: "10.225.237.19", wantChild: "10.223.237.19"},
		{name: "RDMA NIC index 16 on a /19 subnet", vnic: testVNIC(), nicIndex: 16, parentRange: defaultRange, wantErr: "RDMA NIC index 16 with a /19 primary VNIC subnet is outside the child range 10.222.0.0/15"},
		{name: "RDMA NIC index 1 on a /20 subnet", vnic: vnicIn("10.140.64.0/20"), nicIndex: 1, parentRange: defaultRange, wantParent: "10.224.29.19", wantChild: "10.222.29.19"},
		{name: "RDMA NIC index 7 on a /18 subnet", vnic: vnicIn("10.140.64.0/18"), nicIndex: 7, parentRange: defaultRange, wantParent: "10.225.205.19", wantChild: "10.223.205.19"},
		{name: "RDMA NIC index 8 on a /18 subnet", vnic: vnicIn("10.140.64.0/18"), nicIndex: 8, parentRange: defaultRange, wantErr: "RDMA NIC index 8 with a /18 primary VNIC subnet is outside the child range 10.222.0.0/15"},
		{name: "RDMA NIC index 0 in a /16 OCA network", vnic: testVNIC(), nicIndex: 0, parentRange: netip.MustParsePrefix("192.168.0.0/16"), wantParent: "192.168.13.19", wantChild: "10.222.13.19"},
		{name: "RDMA NIC index 8 in a /16 OCA network", vnic: testVNIC(), nicIndex: 8, parentRange: netip.MustParsePrefix("192.168.0.0/16"), wantErr: "RDMA NIC index 8 with a /19 primary VNIC subnet is outside the OCA RDMA network 192.168.0.0/16"},
		{name: "OCA network overlaps the child range", vnic: testVNIC(), parentRange: netip.MustParsePrefix("10.222.0.0/16"), wantErr: "overlaps the Dranet child range"},
		{name: "unmasked OCA network", vnic: testVNIC(), parentRange: netip.MustParsePrefix("10.224.1.0/12"), wantErr: "is not a masked IPv4 prefix"},
		{name: "VNIC subnet inside the OCA network", vnic: &primaryVNIC{IPv4: netip.MustParseAddr("10.224.5.5"), Subnet: netip.MustParsePrefix("10.224.0.0/19")}, parentRange: defaultRange, wantErr: "overlaps the OCA RDMA network"},
		{name: "VNIC subnet inside the child range", vnic: &primaryVNIC{IPv4: netip.MustParseAddr("10.222.5.5"), Subnet: netip.MustParsePrefix("10.222.0.0/19")}, parentRange: defaultRange, wantErr: "overlaps the Dranet child range"},
		{name: "VNIC outside its subnet", vnic: &primaryVNIC{IPv4: netip.MustParseAddr("10.140.200.19"), Subnet: netip.MustParsePrefix("10.140.64.0/19")}, parentRange: defaultRange, wantErr: "is outside its subnet"},
		{name: "missing VNIC", parentRange: defaultRange, wantErr: "not available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, child, err := deriveRDMAIPv4(tt.vnic, tt.nicIndex, tt.parentRange)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("deriveRDMAIPv4() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("deriveRDMAIPv4() returned error: %v", err)
			}
			if parent.String() != tt.wantParent || child.String() != tt.wantChild {
				t.Errorf("deriveRDMAIPv4() = %s, %s, want %s, %s", parent, child, tt.wantParent, tt.wantChild)
			}
		})
	}
}

func TestRDMANicIndexFromAddress(t *testing.T) {
	defaultRange := netip.MustParsePrefix(okeRDMAParentIPv4CIDR)
	tests := []struct {
		name    string
		vnic    *primaryVNIC
		address string
		want    int
		wantErr string
	}{
		{name: "RDMA NIC index 0", vnic: testVNIC(), address: "10.224.13.19", want: 0},
		{name: "RDMA NIC index 1", vnic: testVNIC(), address: "10.224.45.19", want: 1},
		{name: "RDMA NIC index 15", vnic: testVNIC(), address: "10.225.237.19", want: 15},
		{name: "address of another host", vnic: testVNIC(), address: "10.224.13.20", wantErr: "does not match the primary VNIC position 3347 in a /19 subnet"},
		{name: "address outside the OCA network", vnic: testVNIC(), address: "10.140.77.19", wantErr: "is outside the OCA RDMA network"},
		{name: "missing VNIC", address: "10.224.13.19", wantErr: "not available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rdmaNicIndexFromAddress(tt.vnic, netip.MustParseAddr(tt.address), defaultRange)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("rdmaNicIndexFromAddress() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rdmaNicIndexFromAddress() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("rdmaNicIndexFromAddress() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateClassicHostLayout(t *testing.T) {
	tests := []struct {
		name       string
		interfaces []fakeRDMANic
		namedNICs  bool
		wantErr    string
	}{
		{name: "no RDMA NICs", wantErr: "no Ethernet RDMA NIC holds an OCA address yet"},
		{
			name: "named RDMA NIC without an address is skipped",
			interfaces: []fakeRDMANic{
				{ifName: "rdma0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}},
				{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{}},
			},
		},
		{
			name:       "unnamed RDMA NICs are ignored on a node with rdmaN names",
			interfaces: []fakeRDMANic{{ifName: "ens3f0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}}},
			namedNICs:  true,
			wantErr:    "no Ethernet RDMA NIC holds an OCA address yet",
		},
		{
			name: "consistent named RDMA NICs",
			interfaces: []fakeRDMANic{
				{ifName: "rdma0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}},
				{ifName: "rdma15", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.225.237.19/12"}},
				{ifName: "eth0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.140.77.19/19"}},
				{ifName: "ib0", hardwareType: unix.ARPHRD_INFINIBAND},
			},
		},
		{
			name: "consistent unnamed RDMA NICs",
			interfaces: []fakeRDMANic{
				{ifName: "ens3f0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}},
				{ifName: "ens3f1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.45.19/12"}},
			},
		},
		{
			name:       "named RDMA NIC with a wrong address",
			interfaces: []fakeRDMANic{{ifName: "rdma1", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}}},
			wantErr:    "interface rdma1 has address 10.224.13.19, but the derived OCA address is 10.224.45.19",
		},
		{
			name:       "only RDMA NIC without an address",
			interfaces: []fakeRDMANic{{ifName: "rdma0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{}}},
			wantErr:    "no Ethernet RDMA NIC holds an OCA address yet",
		},
		{
			name:       "address read error",
			interfaces: []fakeRDMANic{{ifName: "rdma0", hardwareType: unix.ARPHRD_ETHER}},
			wantErr:    "could not read addresses for rdma0",
		},
		{
			name:       "unnamed RDMA NIC with a wrong remainder",
			interfaces: []fakeRDMANic{{ifName: "ens3f0", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.20/12"}}},
			wantErr:    "interface ens3f0: address 10.224.13.20 does not match",
		},
		{
			name:       "named RDMA NIC outside the child range is still checked",
			interfaces: []fakeRDMANic{{ifName: "rdma16", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.226.13.19/12"}}},
		},
		{
			name:       "named RDMA NIC outside the child range with a wrong address",
			interfaces: []fakeRDMANic{{ifName: "rdma16", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.226.13.20/12"}}},
			wantErr:    "interface rdma16 has address 10.226.13.20, but the derived OCA address is 10.226.13.19",
		},
		{
			name:       "named RDMA NIC outside the OCA network",
			interfaces: []fakeRDMANic{{ifName: "rdma128", hardwareType: unix.ARPHRD_ETHER, addresses: []string{"10.224.13.19/12"}}},
			wantErr:    "could not derive the OCA address for rdma128: RDMA NIC index 128 with a /19 primary VNIC subnet is outside the OCA RDMA network 10.224.0.0/12",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			if err := os.MkdirAll(sysClassNet, 0o755); err != nil {
				t.Fatalf("MkdirAll() returned error: %v", err)
			}
			for _, extra := range tt.interfaces {
				fakeInterface(t, extra.ifName, "", extra.hardwareType)
				if extra.addresses != nil {
					fakeInterfaceAddresses(t, extra.ifName, extra.addresses...)
				}
			}
			instance := newOKEInstance(nil, nil)
			instance.addressFallback = !tt.namedNICs
			err := instance.validateClassicHostLayout(testVNIC())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateClassicHostLayout() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateClassicHostLayout() returned error: %v", err)
			}
		})
	}
}

// rdmaNodeServer serves the IMDS responses of the B4.8 test node and counts
// the requests per endpoint. vnics answers the vnics requests in order; the
// last entry repeats.
func rdmaNodeServer(t *testing.T, requests map[string]*atomic.Int64, vnics ...func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count, ok := requests[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		n := count.Add(1)
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false,"planes":0}}`))
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.B4.8"}`))
		case "/vnics/":
			vnics[min(int(n), len(vnics))-1](w)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func serveVNIC(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`[{"privateIp":"10.140.77.19","subnetCidrBlock":"10.140.64.0/19"}]`))
}

func newRDMANodeRequests() map[string]*atomic.Int64 {
	return map[string]*atomic.Int64{"/host/": {}, "/instance/": {}, "/vnics/": {}}
}

func TestStartPollsUntilPrimaryVNICIsKnown(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma0", "0000:0c:00.0", unix.ARPHRD_ETHER)
	requests := newRDMANodeRequests()
	server := rdmaNodeServer(t, requests,
		func(w http.ResponseWriter) { http.Error(w, "not ready", http.StatusServiceUnavailable) },
		// A 200 without a usable address is retried too.
		func(w http.ResponseWriter) { _, _ = w.Write([]byte(`[{"subnetCidrBlock":"10.140.64.0/19"}]`)) },
		serveVNIC,
	)

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 2 * time.Second
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= instance.initialWait {
		t.Fatalf("start() waited %s for metadata", elapsed)
	}
	if got := requests["/vnics/"].Load(); got != 3 {
		t.Fatalf("start() made %d vnics requests, want 3", got)
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8", RDMAFabric: &rdmaFabric{}, PrimaryVNIC: testVNIC()}
	if diff := cmp.Diff(want, instance.metadata.Load(), compareVNIC); diff != "" {
		t.Errorf("start() mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshReadsVNICOnlyUntilKnown(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma0", "0000:0c:00.0", unix.ARPHRD_ETHER)
	requests := newRDMANodeRequests()
	server := rdmaNodeServer(t, requests, serveVNIC)

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = time.Second
	instance.refreshInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	// Let the background loop refresh several times.
	deadline := time.Now().Add(time.Second)
	for requests["/host/"].Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := requests["/host/"].Load(); got < 4 {
		t.Fatalf("refreshLoop() made %d host requests, want at least 4", got)
	}
	if got := requests["/vnics/"].Load(); got != 1 {
		t.Fatalf("vnics endpoint requested %d times, want 1", got)
	}
}

func TestRefreshPicksUpLateRDMANic(t *testing.T) {
	fakeSysfs(t)
	requests := newRDMANodeRequests()
	server := rdmaNodeServer(t, requests, serveVNIC)

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = time.Second
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if got := requests["/vnics/"].Load(); got != 0 {
		t.Fatalf("start() made %d vnics requests on a node without RDMA NICs", got)
	}
	id := cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"}
	config := &apis.NetworkConfig{Profile: okeRDMAProfile}
	if _, err := instance.GetProfileConfig(id, nil, config); err == nil || !strings.Contains(err.Error(), "primary VNIC") {
		t.Fatalf("GetProfileConfig() error = %v, want a pending primary VNIC error", err)
	}

	// The RDMA NIC appears after startup with its OCA address and ARP policy.
	fakeInterface(t, "rdma0", "0000:0c:00.0", unix.ARPHRD_ETHER)
	fakeInterfaceAddresses(t, "rdma0", "10.224.13.19/12")
	fakeARP(t, "rdma0", "1", "2")
	if err := instance.refreshMetadata(ctx); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	if got := requests["/vnics/"].Load(); got != 1 {
		t.Fatalf("refreshMetadata() made %d vnics requests, want 1", got)
	}
	got, err := instance.GetProfileConfig(id, nil, config)
	if err != nil {
		t.Fatalf("GetProfileConfig() returned error: %v", err)
	}
	if diff := cmp.Diff(testRDMA0Config, got); diff != "" {
		t.Errorf("GetProfileConfig() mismatch (-want +got):\n%s", diff)
	}
}

// start probes addresses only on a node without rdmaN names.
func TestStartSetsAddressFallback(t *testing.T) {
	tests := []struct {
		name        string
		namedNIC    bool
		wantProfile bool
	}{
		{name: "node without rdmaN names", wantProfile: true},
		{name: "node with rdmaN names", namedNIC: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			fakeInterface(t, "ens3f0", "0000:0c:00.0", unix.ARPHRD_ETHER)
			fakeInterfaceAddresses(t, "ens3f0", "10.224.13.19/12")
			if tt.namedNIC {
				fakeInterface(t, "rdma1", "0000:0c:00.1", unix.ARPHRD_ETHER)
			}
			server := rdmaNodeServer(t, newRDMANodeRequests(), serveVNIC)

			instance := newOKEInstance(nil, nil)
			// Start from the opposite value, so the result comes from start.
			instance.addressFallback = !tt.wantProfile
			instance.initialRetryInterval = time.Millisecond
			instance.initialWait = time.Second
			instance.refreshInterval = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
				t.Fatalf("start() returned error: %v", err)
			}
			got := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "ens3f0", PCIAddress: "0000:0c:00.0"})
			if (got != nil) != tt.wantProfile {
				t.Errorf("GetDeviceConfig() = %v, want a profile: %t", got, tt.wantProfile)
			}
		})
	}
}

func TestStartWarnsInExclusiveNetnsMode(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma0", "0000:0c:00.0", unix.ARPHRD_ETHER)
	fakeNetnsMode(t, "N")
	logs := captureLogs(t)
	server := rdmaNodeServer(t, newRDMANodeRequests(), serveVNIC)

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = time.Second
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	klog.Flush()
	if want := "exclusive network namespace mode"; !strings.Contains(logs.String(), want) {
		t.Errorf("start() logged %q, want it to contain %q", logs.String(), want)
	}
}
