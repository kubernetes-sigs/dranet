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

package gce

import (
	"testing"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ipam"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		mac      string
		instance *GCEInstance
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "instance with no interfaces",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type:       "machine-type-a",
				Interfaces: []gceNetworkInterface{},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "MAC not found in instance interfaces, no topology",
			mac:  "00:11:22:33:44:FF", // MAC that won't be found
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "MAC not found in instance interfaces, has topology",
			mac:  "00:11:22:33:44:FF", // MAC that won't be found
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
				},
				Topology: "/block/subblock/host",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCEBlock:       {StringValue: ptr.To("block")},
				AttrGCESubBlock:    {StringValue: ptr.To("subblock")},
				AttrGCEHost:        {StringValue: ptr.To("host")},
				AttrGCEMachineType: {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, valid network",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
					{Mac: "AA:BB:CC:DD:EE:FF", Network: "projects/67890/networks/other-network"},
				},
				Topology: "/block/subblock/host",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEBlock:                {StringValue: ptr.To("block")},
				AttrGCESubBlock:             {StringValue: ptr.To("subblock")},
				AttrGCEHost:                 {StringValue: ptr.To("host")},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, invalid network string for GCE parsing",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "invalid-gce-network-string"},
				},
			},
			want: nil, // GetDeviceAttributes returns nil for invalid network string
		},
		{
			name: "GCE provider, MAC found, valid network, invalid topology",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{Mac: "00:11:22:33:44:55", Network: "projects/12345/networks/test-network"},
					{Mac: "AA:BB:CC:DD:EE:FF", Network: "projects/67890/networks/other-network"},
				},
				Topology: "/block/subblock",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
		{
			name: "GCE provider, MAC found, with IP aliases",
			mac:  "00:11:22:33:44:55",
			instance: &GCEInstance{
				Type: "machine-type-a",
				Interfaces: []gceNetworkInterface{
					{
						Mac:       "00:11:22:33:44:55",
						Network:   "projects/12345/networks/test-network",
						IPAliases: []string{"10.0.0.1/24", "10.0.0.2/24"},
					},
				},
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrGCENetworkName:          {StringValue: ptr.To("test-network")},
				AttrGCENetworkProjectNumber: {IntValue: ptr.To(int64(12345))},
				AttrGCEIPAliases:            {StringValue: ptr.To("10.0.0.1/24,10.0.0.2/24")},
				AttrGCEMachineType:          {StringValue: ptr.To("machine-type-a")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{MAC: tt.mac})
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	const mac = "00:11:22:33:44:55"
	dualStackIface := gceNetworkInterface{
		Mac:       mac,
		IPAliases: []string{"10.24.3.0/24"},
		IPv6:      []string{"2001:db8:1234:5678::/64"},
	}
	bareIface := gceNetworkInterface{Mac: mac}
	ipvlanConfig := func() *apis.NetworkConfig {
		return &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVlan}}
	}

	tests := []struct {
		name      string
		mac       string
		iface     gceNetworkInterface
		config    *apis.NetworkConfig
		wantAddrs int
		wantErr   bool
		nilIPAM   bool
	}{
		{
			name:      "dual-stack subinterface allocates one address per family",
			mac:       mac,
			iface:     dualStackIface,
			config:    ipvlanConfig(),
			wantAddrs: 2,
		},
		{
			name:   "empty MAC returns nil",
			mac:    "",
			iface:  dualStackIface,
			config: ipvlanConfig(),
		},
		{
			name:   "MAC not found returns nil",
			mac:    "aa:bb:cc:dd:ee:ff",
			iface:  dualStackIface,
			config: ipvlanConfig(),
		},
		{
			name:   "non-subinterface config returns nil",
			mac:    mac,
			iface:  dualStackIface,
			config: &apis.NetworkConfig{},
		},
		{
			name:    "subinterface without cloud ranges returns error",
			mac:     mac,
			iface:   bareIface,
			config:  ipvlanConfig(),
			wantErr: true,
		},
		{
			name:    "subinterface without IPAM returns error",
			mac:     mac,
			iface:   dualStackIface,
			config:  ipvlanConfig(),
			nilIPAM: true,
			wantErr: true,
		},
		{
			name:      "static addresses are reserved not allocated",
			mac:       mac,
			iface:     dualStackIface,
			config:    &apis.NetworkConfig{Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVlan, Addresses: []string{"10.24.3.5/32"}}},
			wantAddrs: 0,
		},
		{
			name:    "invalid IPv6 metadata returns error",
			mac:     mac,
			iface:   gceNetworkInterface{Mac: mac, IPv6: []string{"not-an-ip"}},
			config:  ipvlanConfig(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &GCEInstance{Interfaces: []gceNetworkInterface{tt.iface}}
			if !tt.nilIPAM {
				instance.localIPAM = ipam.NewLocalIPAM(nil)
			}

			got, err := instance.GetProfileConfig(cloudprovider.DeviceIdentifiers{MAC: tt.mac}, types.UID("claim-uid"), tt.config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetProfileConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantAddrs == 0 {
				if got != nil {
					t.Fatalf("GetProfileConfig() = %#v, want nil", got)
				}
				return
			}
			if got == nil || len(got.Interface.Addresses) != tt.wantAddrs {
				t.Fatalf("GetProfileConfig() addresses = %v, want %d", got, tt.wantAddrs)
			}
		})
	}
}

func TestGetIPv6Range(t *testing.T) {
	tests := []struct {
		name      string
		baseIPStr string
		want      string
		wantErr   bool
	}{
		{
			name:      "valid CIDR prefix /64",
			baseIPStr: "2001:db8:1234:5678::/64",
			want:      "2001:db8:1234:5678:c0de::/80",
			wantErr:   false,
		},
		{
			name:      "too large CIDR prefix /112",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2345::/112",
			wantErr:   true,
		},
		{
			name:      "plain IP, no CIDR prefix",
			baseIPStr: "2001:db8:1234:5678:abcd:ef01:2345:6789",
			wantErr:   true,
		},
		{
			name:      "invalid IP format",
			baseIPStr: "invalid-ip",
			wantErr:   true,
		},
		{
			name:      "IPv4 CIDR input",
			baseIPStr: "192.168.1.0/24",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getIPv6Range(tt.baseIPStr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getIPv6Range() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("getIPv6Range() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubinterfaceRanges(t *testing.T) {
	tests := []struct {
		name    string
		iface   gceNetworkInterface
		want    [][2]string // {start, end} per range, in order.
		wantErr bool
	}{
		{
			name:  "IPv4 alias only",
			iface: gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}},
			want:  [][2]string{{"10.24.3.1", "10.24.3.254"}},
		},
		{
			name:  "IPv6 only",
			iface: gceNetworkInterface{IPv6: []string{"2001:db8:1234:5678::/64"}},
			want:  [][2]string{{"2001:db8:1234:5678:c0de::1", "2001:db8:1234:5678:c0de:ffff:ffff:fffe"}},
		},
		{
			name:  "dual stack orders IPv6 before IPv4",
			iface: gceNetworkInterface{IPAliases: []string{"10.24.3.0/24"}, IPv6: []string{"2001:db8:1234:5678::/64"}},
			want: [][2]string{
				{"2001:db8:1234:5678:c0de::1", "2001:db8:1234:5678:c0de:ffff:ffff:fffe"},
				{"10.24.3.1", "10.24.3.254"},
			},
		},
		{
			name:  "no IPv6 or aliases yields no ranges",
			iface: gceNetworkInterface{Mac: "00:11:22:33:44:55"},
			want:  nil,
		},
		{
			name:    "invalid IPv6 base returns error",
			iface:   gceNetworkInterface{IPv6: []string{"not-an-ip"}},
			wantErr: true,
		},
		{
			name:    "invalid IPv4 alias returns error",
			iface:   gceNetworkInterface{IPAliases: []string{"bad-cidr"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GCEInstance{}
			got, err := g.subinterfaceRanges(tt.iface)
			if (err != nil) != tt.wantErr {
				t.Fatalf("subinterfaceRanges() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("subinterfaceRanges() got %d ranges, want %d (%v)", len(got), len(tt.want), got)
			}
			for i, r := range got {
				if r.Start.String() != tt.want[i][0] || r.End.String() != tt.want[i][1] {
					t.Errorf("range[%d] = [%s, %s], want [%s, %s]", i, r.Start, r.End, tt.want[i][0], tt.want[i][1])
				}
			}
		})
	}
}
