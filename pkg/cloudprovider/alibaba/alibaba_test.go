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

package alibaba

import (
	"fmt"
	"net"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const testPCIAddress = "0000:00:0b.0"

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name          string
		instance      AlibabaInstance
		id            cloudprovider.DeviceIdentifiers
		wantInstType  string
		wantERDMA     bool
	}{
		{
			name: "GPU instance with eRDMA, matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.gn8is-2x.8xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.gn8is-2x.8xlarge",
			wantERDMA:    true,
		},
		{
			name: "GPU instance with eRDMA, non-matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.gn8is-2x.8xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: "0000:00:0c.0"},
			wantInstType: "ecs.gn8is-2x.8xlarge",
			wantERDMA:    false,
		},
		{
			name: "regular ECS instance without eRDMA",
			instance: AlibabaInstance{
				InstanceType:      "ecs.g6.xlarge",
				ERDMAPCIAddresses: sets.New[string](),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.g6.xlarge",
			wantERDMA:    false,
		},
		{
			name: "bare metal with eRDMA, matching PCI address",
			instance: AlibabaInstance{
				InstanceType:      "ecs.ebmgn8is.32xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			},
			id:           cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantInstType: "ecs.ebmgn8is.32xlarge",
			wantERDMA:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := tt.instance.GetDeviceAttributes(tt.id)
			if tt.wantInstType != "" {
				instAttr, ok := attrs[AttrInstanceType]
				if !ok {
					t.Fatal("missing instanceType attribute")
				}
				if instAttr.StringValue == nil || *instAttr.StringValue != tt.wantInstType {
					t.Errorf("instanceType = %v, want %s", instAttr.StringValue, tt.wantInstType)
				}
			}
			erdmaAttr, ok := attrs[AttrERDMA]
			if tt.wantERDMA {
				if !ok {
					t.Fatal("missing erdma attribute")
				}
				if erdmaAttr.BoolValue == nil || !*erdmaAttr.BoolValue {
					t.Error("expected erdma=true")
				}
			} else {
				if ok {
					t.Errorf("unexpected erdma attribute: %v", erdmaAttr)
				}
			}
		})
	}
}

func TestGetDeviceConfig(t *testing.T) {
	origIsBond := isLACPBond
	origPrefix := getNICIPv6Prefix
	t.Cleanup(func() {
		isLACPBond = origIsBond
		getNICIPv6Prefix = origPrefix
	})

	_, testPrefix, err := net.ParseCIDR("2001:db8:1234:5678::/64")
	if err != nil {
		t.Fatalf("failed to parse test prefix: %v", err)
	}

	tests := []struct {
		name        string
		isBond      bool
		prefix      *net.IPNet
		prefixErr   error
		id          cloudprovider.DeviceIdentifiers
		wantNil     bool
		wantType    apis.SubInterfaceType
		wantIPRange string
	}{
		{
			name:    "eRDMA device, not a bond -> nil config",
			isBond:  false,
			id:      cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantNil: true,
		},
		{
			name:        "LACP bond -> IPVlan subinterface with eflo RDMA IP block (transparent to user)",
			isBond:      true,
			prefix:      testPrefix,
			id:          cloudprovider.DeviceIdentifiers{Name: "bond0", PCIAddress: testPCIAddress},
			wantNil:     false,
			wantType:    apis.SubInterfaceTypeIPVlan,
			wantIPRange: "2001:db8:1234:5678:0:f:0:c00/124",
		},
		{
			name:    "regular NIC, not a bond -> nil config",
			isBond:  false,
			id:      cloudprovider.DeviceIdentifiers{Name: "eth0"},
			wantNil: true,
		},
		{
			name:      "LACP bond but no eflo RDMA IPv6 prefix found -> nil config",
			isBond:    true,
			prefixErr: fmt.Errorf("no global /64 IPv6 address found"),
			id:        cloudprovider.DeviceIdentifiers{Name: "bond0"},
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isLACPBond = func(string) bool { return tt.isBond }
			getNICIPv6Prefix = func(string) (*net.IPNet, error) { return tt.prefix, tt.prefixErr }
			instance := &AlibabaInstance{
				InstanceType:      "ecs.gn8is-2x.8xlarge",
				ERDMAPCIAddresses: sets.New[string](testPCIAddress),
			}
			config := instance.GetDeviceConfig(tt.id)
			if tt.wantNil {
				if config != nil {
					t.Errorf("expected nil config, got %v", config)
				}
				return
			}
			if config == nil || config.SubInterface == nil {
				t.Fatalf("expected non-nil SubInterface config, got %v", config)
			}
			if config.SubInterface.Type != tt.wantType {
				t.Errorf("SubInterface.Type = %q, want %q", config.SubInterface.Type, tt.wantType)
			}
			if len(config.SubInterface.IPRanges) != 1 || config.SubInterface.IPRanges[0].CIDR != tt.wantIPRange {
				t.Errorf("SubInterface.IPRanges = %v, want [{CIDR: %q}]", config.SubInterface.IPRanges, tt.wantIPRange)
			}
			// The parent's own address must never appear in the subinterface
			// config: the derived range is disjoint from the NIC's live host
			// address, so there is nothing to strip from or restore to the host.
			if len(config.SubInterface.Addresses) != 0 {
				t.Errorf("expected no static Addresses (range is IPAM-allocated), got %v", config.SubInterface.Addresses)
			}
		})
	}
}

func TestEfloRDMASubinterfaceRange(t *testing.T) {
	tests := []struct {
		name    string
		cidr    string
		want    string
		wantErr bool
	}{
		{
			name: "typical /64 prefix",
			cidr: "2001:db8:1234:5678::/64",
			want: "2001:db8:1234:5678:0:f:0:c00/124",
		},
		{
			name: "prefix with non-zero host bits gets masked away",
			cidr: "2001:db8:1234:5678::1/64",
			want: "2001:db8:1234:5678:0:f:0:c00/124",
		},
		{
			name:    "IPv4 rejected",
			cidr:    "10.0.0.0/24",
			wantErr: true,
		},
		{
			name:    "non-/64 prefix rejected",
			cidr:    "2001:db8:1234:5678::/80",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, prefix, err := net.ParseCIDR(tt.cidr)
			if err != nil {
				t.Fatalf("failed to parse test CIDR: %v", err)
			}
			got, err := efloRDMASubinterfaceRange(prefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("efloRDMASubinterfaceRange() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("efloRDMASubinterfaceRange() = %q, want %q", got, tt.want)
			}
		})
	}
}


func TestDetectERDMAPCIAddresses(t *testing.T) {
	orig := detectERDMAPCIAddresses
	t.Cleanup(func() { detectERDMAPCIAddresses = orig })

	detectERDMAPCIAddresses = func() sets.Set[string] {
		return sets.New[string](testPCIAddress)
	}
	got := detectERDMAPCIAddresses()
	if !got.Has(testPCIAddress) {
		t.Errorf("expected %s in result, got %v", testPCIAddress, got)
	}

	detectERDMAPCIAddresses = func() sets.Set[string] {
		return sets.New[string]()
	}
	got = detectERDMAPCIAddresses()
	if got.Len() != 0 {
		t.Errorf("expected empty set, got %v", got)
	}
}
