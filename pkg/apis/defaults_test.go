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
package apis

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

func TestInterfaceConfigDefault(t *testing.T) {
	tests := []struct {
		name string
		cfg  InterfaceConfig
		want InterfaceConfig
	}{
		{
			name: "no addressing leaves the config untouched",
			cfg:  InterfaceConfig{Name: "eth0"},
			want: InterfaceConfig{Name: "eth0"},
		},
		{
			name: "deprecated dhcp field folds into addressing",
			cfg:  InterfaceConfig{Name: "eth0", DHCP: ptr.To(true)},
			want: InterfaceConfig{Name: "eth0", DHCP: ptr.To(true), Addressing: AddressingModeDHCP},
		},
		{
			name: "static addressing does not get the IPv6 sysctls",
			cfg:  InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic},
			want: InterfaceConfig{Name: "eth0", Addressing: AddressingModeStatic},
		},
		{
			name: "SLAAC fills in the sysctls autoconfiguration needs",
			cfg:  InterfaceConfig{Name: "eth0", Addressing: AddressingModeSLAAC},
			want: InterfaceConfig{
				Name:                       "eth0",
				Addressing:                 AddressingModeSLAAC,
				AcceptRA:                   ptr.To[int32](2),
				DADTransmits:               ptr.To[int32](0),
				RouterSolicitationDelay:    ptr.To[int32](0),
				RouterSolicitationInterval: ptr.To[int32](1),
				DisableIPv6:                ptr.To(false),
			},
		},
		{
			// A new hardware address means a new address on the link, which the
			// host never verified, so duplicate address detection stays at the
			// kernel default unless asked for explicitly.
			name: "SLAAC with a new hardware address keeps duplicate address detection",
			cfg:  InterfaceConfig{Name: "eth0", Addressing: AddressingModeSLAAC, HardwareAddr: ptr.To("02:00:00:00:00:01")},
			want: InterfaceConfig{
				Name:                       "eth0",
				Addressing:                 AddressingModeSLAAC,
				HardwareAddr:               ptr.To("02:00:00:00:00:01"),
				AcceptRA:                   ptr.To[int32](2),
				RouterSolicitationDelay:    ptr.To[int32](0),
				RouterSolicitationInterval: ptr.To[int32](1),
				DisableIPv6:                ptr.To(false),
			},
		},
		{
			name: "SLAAC does not overwrite explicit values",
			cfg: InterfaceConfig{
				Name:                       "eth0",
				Addressing:                 AddressingModeSLAAC,
				AcceptRA:                   ptr.To[int32](1),
				DADTransmits:               ptr.To[int32](2),
				RouterSolicitationDelay:    ptr.To[int32](1),
				RouterSolicitationInterval: ptr.To[int32](2),
				DisableIPv6:                ptr.To(false),
				AddrGenMode:                ptr.To[int32](3),
			},
			want: InterfaceConfig{
				Name:                       "eth0",
				Addressing:                 AddressingModeSLAAC,
				AcceptRA:                   ptr.To[int32](1),
				DADTransmits:               ptr.To[int32](2),
				RouterSolicitationDelay:    ptr.To[int32](1),
				RouterSolicitationInterval: ptr.To[int32](2),
				DisableIPv6:                ptr.To(false),
				AddrGenMode:                ptr.To[int32](3),
			},
		},
		{
			// A child shares its parent's hardware address, so it gets a random
			// interface identifier, and with a random identifier duplicate
			// address detection is skipped.
			name: "SLAAC on an IPVLAN child generates a random identifier and skips duplicate address detection",
			cfg:  InterfaceConfig{Name: "rdma0", Type: InterfaceTypeIPVLAN, Addressing: AddressingModeSLAAC},
			want: InterfaceConfig{
				Name:                       "rdma0",
				Type:                       InterfaceTypeIPVLAN,
				Addressing:                 AddressingModeSLAAC,
				AcceptRA:                   ptr.To[int32](2),
				DADTransmits:               ptr.To[int32](0),
				RouterSolicitationDelay:    ptr.To[int32](0),
				RouterSolicitationInterval: ptr.To[int32](1),
				DisableIPv6:                ptr.To(false),
				AddrGenMode:                ptr.To[int32](3),
			},
		},
		{
			name: "SLAAC on an IPVLAN child keeps an explicit dadTransmits",
			cfg:  InterfaceConfig{Name: "rdma0", Type: InterfaceTypeIPVLAN, Addressing: AddressingModeSLAAC, DADTransmits: ptr.To[int32](1)},
			want: InterfaceConfig{
				Name:                       "rdma0",
				Type:                       InterfaceTypeIPVLAN,
				Addressing:                 AddressingModeSLAAC,
				AcceptRA:                   ptr.To[int32](2),
				DADTransmits:               ptr.To[int32](1),
				RouterSolicitationDelay:    ptr.To[int32](0),
				RouterSolicitationInterval: ptr.To[int32](1),
				DisableIPv6:                ptr.To(false),
				AddrGenMode:                ptr.To[int32](3),
			},
		},
		{
			name: "static addressing does not touch disableIPv6 or addrGenMode",
			cfg:  InterfaceConfig{Name: "rdma0", Type: InterfaceTypeIPVLAN, Addresses: []string{"10.0.0.1/24"}},
			want: InterfaceConfig{Name: "rdma0", Type: InterfaceTypeIPVLAN, Addresses: []string{"10.0.0.1/24"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg
			got.Default()
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Default() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
