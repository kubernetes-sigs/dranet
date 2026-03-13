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

package azure

import (
	"testing"

	"sigs.k8s.io/dranet/pkg/cloudprovider"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		instance *AzureInstance
		id       cloudprovider.DeviceIdentifiers
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "instance with placementGroupId and vmSize",
			instance: &AzureInstance{
				PlacementGroupID: "739e6cfb-2607-462e-9e2b-21d24b31f5ed",
				VMSize:           "Standard_ND128isr_GB300_v6",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzurePlacementGroupID: {StringValue: ptr.To("739e6cfb-2607-462e-9e2b-21d24b31f5ed")},
				AttrAzureVMSize:           {StringValue: ptr.To("Standard_ND128isr_GB300_v6")},
			},
		},
		{
			name: "instance with only vmSize, no placementGroupId",
			instance: &AzureInstance{
				PlacementGroupID: "",
				VMSize:           "Standard_D4s_v3",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzureVMSize: {StringValue: ptr.To("Standard_D4s_v3")},
			},
		},
		{
			name: "instance with no metadata",
			instance: &AzureInstance{
				PlacementGroupID: "",
				VMSize:           "",
			},
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		{
			name: "attributes are node-level, same for any device identifier",
			instance: &AzureInstance{
				PlacementGroupID: "ab1690bb-a478-4039-b89a-8a3f4264d4b4",
				VMSize:           "Standard_ND128isr_GB300_v6",
			},
			id: cloudprovider.DeviceIdentifiers{
				Name:       "0001-00-00-0",
				MAC:        "00:11:22:33:44:55",
				PCIAddress: "0001:00:00.0",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrAzurePlacementGroupID: {StringValue: ptr.To("ab1690bb-a478-4039-b89a-8a3f4264d4b4")},
				AttrAzureVMSize:           {StringValue: ptr.To("Standard_ND128isr_GB300_v6")},
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

func TestGetDeviceConfig(t *testing.T) {
	instance := &AzureInstance{
		PlacementGroupID: "test-group",
		VMSize:           "Standard_D4s_v3",
	}
	got := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "dev1"})
	if got != nil {
		t.Errorf("GetDeviceConfig() = %v, want nil", got)
	}
}
