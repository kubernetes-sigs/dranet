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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func TestDeviceIdentifiersFromDevice(t *testing.T) {
	if got := DeviceIdentifiersFromDevice(nil); got != (DeviceIdentifiers{}) {
		t.Fatalf("nil device: %#v", got)
	}
	device := &resourceapi.Device{Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		AttrInterfaceName: {StringValue: ptr.To("eth1")},
		AttrMac:           {StringValue: ptr.To("aa:bb:cc:dd:ee:ff")},
		AttrPCIAddress:    {StringValue: ptr.To("0000:00:01.0")},
	}}
	want := DeviceIdentifiers{Name: "eth1", MAC: "aa:bb:cc:dd:ee:ff", PCIAddress: "0000:00:01.0"}
	if got := DeviceIdentifiersFromDevice(device); got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
