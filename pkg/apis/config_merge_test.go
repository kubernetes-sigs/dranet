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

func TestMergeNetworkConfig(t *testing.T) {
	tests := []struct {
		name  string
		user  *NetworkConfig
		cloud *NetworkConfig
		want  *NetworkConfig
	}{
		{
			name: "nil cloud config",
			user: &NetworkConfig{
				Interface: InterfaceConfig{Name: "eth0"},
			},
			cloud: nil,
			want: &NetworkConfig{
				Interface: InterfaceConfig{Name: "eth0"},
			},
		},
		{
			name: "scalar overrides",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-user",
					MTU:  ptr.To[int32](1400),
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-cloud",
					MTU:  ptr.To[int32](1500),
					DHCP: ptr.To(true),
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0-user",
					MTU:  ptr.To[int32](1400),
					DHCP: ptr.To(true),
				},
			},
		},
		{
			name: "merge slices without duplicates",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "1.1.1.1/32", Gateway: "10.0.0.1"},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32"},
				},
				Routes: []RouteConfig{
					{Destination: "2.2.2.2/32", Gateway: "10.0.0.1"},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32", "1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "2.2.2.2/32", Gateway: "10.0.0.1"},
					{Destination: "1.1.1.1/32", Gateway: "10.0.0.1"},
				},
			},
		},
		{
			name: "conflict resolution (user wins)",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1"},
				},
				Ethtool: &EthtoolConfig{
					Features: map[string]bool{"tcp-segmentation-offload": false},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"1.1.1.1/32", "2.2.2.2/32"},
				},
				Routes: []RouteConfig{
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.254"}, // Conflicting dest
					{Destination: "10.0.0.0/8", Gateway: "10.0.0.254"},
				},
				Ethtool: &EthtoolConfig{
					Features: map[string]bool{"tcp-segmentation-offload": true, "rx-checksum": true},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Addresses: []string{"2.2.2.2/32", "1.1.1.1/32"},
				},
				Routes: []RouteConfig{
					{Destination: "10.0.0.0/8", Gateway: "10.0.0.254"},
					{Destination: "0.0.0.0/0", Gateway: "10.0.0.1"},
				},
				Ethtool: &EthtoolConfig{
					Features:     map[string]bool{"tcp-segmentation-offload": false, "rx-checksum": true},
					PrivateFlags: nil,
				},
			},
		},
		{
			name: "subinterface: user does not request, cloud config is discarded",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0",
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					SubInterface: &SubInterfaceConfig{
						IPRange: "10.24.3.0/24",
					},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0",
				},
			},
		},
		{
			name: "subinterface: user requests, cloud config is merged",
			user: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0",
					SubInterface: &SubInterfaceConfig{
						Type: "ipvlan",
					},
				},
			},
			cloud: &NetworkConfig{
				Interface: InterfaceConfig{
					SubInterface: &SubInterfaceConfig{
						IPRange: "10.24.3.0/24",
					},
				},
			},
			want: &NetworkConfig{
				Interface: InterfaceConfig{
					Name: "eth0",
					SubInterface: &SubInterfaceConfig{
						Type:    "ipvlan",
						IPRange: "10.24.3.0/24",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeNetworkConfig(tt.user, tt.cloud)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("MergeNetworkConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
