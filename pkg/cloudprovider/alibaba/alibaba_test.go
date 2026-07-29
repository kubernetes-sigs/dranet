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
	orig := isLACPBond
	t.Cleanup(func() { isLACPBond = orig })

	tests := []struct {
		name     string
		isBond   bool
		id       cloudprovider.DeviceIdentifiers
		wantNil  bool
		wantType apis.SubInterfaceType
	}{
		{
			name:     "eRDMA device, not a bond -> nil config",
			isBond:   false,
			id:       cloudprovider.DeviceIdentifiers{PCIAddress: testPCIAddress},
			wantNil:  true,
			wantType: "",
		},
		{
			name:     "LACP bond -> IPVlan subinterface config (transparent to user)",
			isBond:   true,
			id:       cloudprovider.DeviceIdentifiers{Name: "bond0", PCIAddress: testPCIAddress},
			wantNil:  false,
			wantType: apis.SubInterfaceTypeIPVlan,
		},
		{
			name:     "regular NIC, not a bond -> nil config",
			isBond:   false,
			id:       cloudprovider.DeviceIdentifiers{Name: "eth0"},
			wantNil:  true,
			wantType: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isLACPBond = func(string) bool { return tt.isBond }
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
