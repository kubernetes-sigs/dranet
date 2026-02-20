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
	"testing"

	"github.com/google/dranet/pkg/apis"
	"github.com/google/dranet/pkg/cloudprovider"
	"github.com/google/dranet/pkg/cloudprovider/gce"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

// mockCloudInstance implements cloudprovider.CloudInstance for testing
type mockCloudInstance struct {
	provider         cloudprovider.CloudProvider
	deviceAttributes map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
}

func (m *mockCloudInstance) GetProvider() cloudprovider.CloudProvider {
	return m.provider
}

func (m *mockCloudInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	// For testing, we primarily look up by MAC if present, similar to the GCE implementation for now
	if id.MAC != "" {
		return m.deviceAttributes[id.MAC]
	}
	// We could extend this to look up by Name or PCIAddress if tests require it
	return nil
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
			instance: &mockCloudInstance{provider: cloudprovider.CloudProviderGCE},
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
				provider: cloudprovider.CloudProviderGCE,
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
				provider: cloudprovider.CloudProviderGCE,
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
			name: "Device Name used (future proofing)",
			device: &resourceapi.Device{
				Name: "dev-pci-1", // PCI device without MAC
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					apis.AttrPCIAddress: {StringValue: ptr.To("0000:00:01.0")},
				},
			},
			instance: &mockCloudInstance{
				provider: cloudprovider.CloudProviderGCE,
				// Mock implementation currently only checks MAC, so this returns nil
				// This test case ensures we can pass non-MAC devices without crashing
				deviceAttributes: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getProviderAttributes(tt.device, tt.instance)
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("getProviderAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func Test_getLastSegmentAndTruncate(t *testing.T) {
	type args struct {
		s         string
		maxLength int
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "shorter than max",
			args: args{s: "/path/to/segment", maxLength: 20},
			want: "segment",
		},
		{
			name: "longer than max",
			args: args{s: "/path/to/verylongsegmentname", maxLength: 10},
			want: "verylongse",
		},
		{
			name: "equal to max",
			args: args{s: "/path/to/segmentname", maxLength: 12},
			want: "segmentname",
		},
		{
			name: "empty string",
			args: args{s: "", maxLength: 10},
			want: "",
		},
		{
			name: "no path separators",
			args: args{s: "justafilename", maxLength: 20},
			want: "justafilename",
		},
		{
			name: "no path separators, needs truncation",
			args: args{s: "justaverylongfilename", maxLength: 10},
			want: "justaveryl",
		},
		{
			name: "trailing slash",
			args: args{s: "/path/to/segment/", maxLength: 10},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getLastSegmentAndTruncate(tt.args.s, tt.args.maxLength); got != tt.want {
				t.Errorf("getLastSegmentAndTruncate() = %v, want %v", got, tt.want)
			}
		})
	}
}
