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

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name         string
		instance     AlibabaInstance
		wantInstType string
	}{
		{
			name: "HPN instance",
			instance: AlibabaInstance{
				InstanceType: "ecs.ebmhpn7.320xlarge",
				IsHPN:        true,
			},
			wantInstType: "ecs.ebmhpn7.320xlarge",
		},
		{
			name: "regular ECS instance",
			instance: AlibabaInstance{
				InstanceType: "ecs.g7.xlarge",
				IsHPN:        false,
			},
			wantInstType: "ecs.g7.xlarge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := tt.instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "bond0"})
			if tt.wantInstType != "" {
				instAttr, ok := attrs[AttrInstanceType]
				if !ok {
					t.Fatal("missing instanceType attribute")
				}
				if instAttr.StringValue == nil || *instAttr.StringValue != tt.wantInstType {
					t.Errorf("instanceType = %v, want %s", instAttr.StringValue, tt.wantInstType)
				}
			}
		})
	}
}

func TestGetDeviceConfig(t *testing.T) {
	// Override IPv6 check for unit tests (no real interfaces available).
	origFunc := hasGlobalIPv6Func
	hasGlobalIPv6Func = func(ifName string) bool { return true }
	t.Cleanup(func() { hasGlobalIPv6Func = origFunc })

	t.Run("HPN returns IPVlan config for bond device", func(t *testing.T) {
		instance := &AlibabaInstance{IsHPN: true}
		config := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "bond0"})
		if config == nil {
			t.Fatal("expected non-nil config for HPN bond device")
		}
		if config.Interface.IPVlan == nil {
			t.Fatal("expected IPVlan config")
		}
		if config.Interface.IPVlan.Mode != "l2" {
			t.Errorf("IPVlan mode = %q, want %q", config.Interface.IPVlan.Mode, "l2")
		}
		if config.Interface.IPVlan.Flag != "bridge" {
			t.Errorf("IPVlan flag = %q, want %q", config.Interface.IPVlan.Flag, "bridge")
		}
		if config.Interface.IPVlan.Addressing == nil {
			t.Fatal("expected Addressing config")
		}
		if config.Interface.IPVlan.Addressing.Type != apis.IPVlanAddrParentIPv6PrefixPodIPv4 {
			t.Errorf("Addressing.Type = %q, want %q", config.Interface.IPVlan.Addressing.Type, apis.IPVlanAddrParentIPv6PrefixPodIPv4)
		}
		if config.Interface.IPVlan.CopyRoutesFromParent == nil || !*config.Interface.IPVlan.CopyRoutesFromParent {
			t.Error("expected CopyRoutesFromParent=true")
		}
		if config.Interface.IPVlan.CopyNeighborsFromParent == nil || !*config.Interface.IPVlan.CopyNeighborsFromParent {
			t.Error("expected CopyNeighborsFromParent=true")
		}
	})

	t.Run("HPN returns nil for non-bond device", func(t *testing.T) {
		instance := &AlibabaInstance{IsHPN: true}
		config := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "reth0"})
		if config != nil {
			t.Errorf("expected nil config for bond slave, got %v", config)
		}
	})

	t.Run("non-HPN returns nil", func(t *testing.T) {
		instance := &AlibabaInstance{IsHPN: false}
		config := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "bond0"})
		if config != nil {
			t.Errorf("expected nil config for non-HPN, got %v", config)
		}
	})
}

func TestDetectHPNHPN(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		want         bool
	}{
		{"hpn in name", "ecs.ebmhpn7.320xlarge", true},
		{"hpn in name", "ecs.hpn2.xlarge", true},
		{"HPN uppercase", "ecs.ebmHPN.large", true},
		{"regular instance without infiniband", "ecs.g7.xlarge", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectHPN(tt.instanceType)
			// For types without "hpn", the result depends on
			// whether /sys/class/infiniband exists on the test host.
			// We only assert for positive matches from the instance type.
			if tt.want && !got {
				t.Errorf("detectHPN(%q) = false, want true", tt.instanceType)
			}
		})
	}
}
