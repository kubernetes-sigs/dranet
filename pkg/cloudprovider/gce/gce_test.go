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

	"github.com/google/dranet/pkg/cloudprovider"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
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
