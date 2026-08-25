/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package coreweave

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

func TestOnCKS(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cks-node",
			Labels: map[string]string{LabelCKSCluster: "test-cluster"},
		},
	}
	nodes := fake.NewSimpleClientset(node).CoreV1().Nodes()

	if !OnCKS(context.Background(), nodes, node.Name) {
		t.Fatal("OnCKS() = false, want true")
	}
	if OnCKS(context.Background(), nodes, "missing-node") {
		t.Fatal("OnCKS() = true for a missing Node")
	}
	if OnCKS(context.Background(), nil, node.Name) {
		t.Fatal("OnCKS() = true with a nil Node client")
	}
}

func TestGetInstanceRejectsNonCKSNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other-node"}}
	nodes := fake.NewSimpleClientset(node).CoreV1().Nodes()

	if _, err := GetInstance(context.Background(), nodes, node.Name); err == nil {
		t.Fatal("GetInstance() error = nil, want missing CKS label error")
	}
}

func TestGetDeviceAttributes(t *testing.T) {
	labels := map[string]string{
		LabelCKSCluster:    "use15",
		LabelFabricFlavor:  "infiniband",
		LabelFabric:        "US-EAST-15A-FAB66",
		LabelSuperpod:      "2",
		LabelLeafgroup:     "856886992745",
		LabelLeafgroupName: "90.1-DH1",
		LabelRack:          "82",
		LabelSpeedCurrent:  "3200G",
		LabelSpeedExpected: "3200G",
		LabelInstanceType:  "b200-8x",
		neighborLabelPrefix + "ibp0" + neighborLabelSuffix: "L90.1.3",
	}
	instance := &Instance{
		labels:          labels,
		rdmaDeviceByPCI: map[string]string{"18:00.0": "ibp0"},
	}

	attributes := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{PCIAddress: "0000:18:00.0"})
	want := map[resourceapi.QualifiedName]string{
		AttrFabricFlavor:  "infiniband",
		AttrFabric:        "US-EAST-15A-FAB66",
		AttrSuperpod:      "2",
		AttrLeafgroup:     "856886992745",
		AttrLeafgroupName: "90.1-DH1",
		AttrRack:          "82",
		AttrSpeedCurrent:  "3200G",
		AttrSpeedExpected: "3200G",
		AttrInstanceType:  "b200-8x",
		AttrLeafSwitch:    "L90.1.3",
	}
	if len(attributes) != len(want) {
		t.Fatalf("GetDeviceAttributes() returned %d attributes, want %d: %#v", len(attributes), len(want), attributes)
	}
	for name, wantValue := range want {
		attribute, ok := attributes[name]
		if !ok || attribute.StringValue == nil || *attribute.StringValue != wantValue {
			t.Errorf("attribute %s = %#v, want %q", name, attribute, wantValue)
		}
	}
}

func TestGetDeviceAttributesSkipsUnavailableValues(t *testing.T) {
	instance := &Instance{labels: map[string]string{
		LabelFabric: strings.Repeat("x", resourceapi.DeviceAttributeMaxValueLength+1),
	}}

	attributes := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{})
	if len(attributes) != 0 {
		t.Fatalf("GetDeviceAttributes() = %#v, want no attributes", attributes)
	}
}

func TestGetDeviceAttributesUsesRDMABasedName(t *testing.T) {
	instance := &Instance{labels: map[string]string{
		neighborLabelPrefix + "ibp7" + neighborLabelSuffix: "L90.1.5",
	}}

	attributes := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "ibp7"})
	if got := attributes[AttrLeafSwitch].StringValue; got == nil || *got != "L90.1.5" {
		t.Fatalf("leaf switch attribute = %#v, want L90.1.5", got)
	}
}

func TestDiscoverRDMADevicesByPCI(t *testing.T) {
	root := t.TempDir()
	pciDevice := filepath.Join(root, "devices", "pci0000:00", "0000:18:00.0")
	if err := os.MkdirAll(pciDevice, 0o755); err != nil {
		t.Fatal(err)
	}
	rdmaDevice := filepath.Join(root, "class", "infiniband", "ibp0")
	if err := os.MkdirAll(rdmaDevice, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDevice, filepath.Join(rdmaDevice, "device")); err != nil {
		t.Fatal(err)
	}

	devices := discoverRDMADevicesByPCI(filepath.Join(root, "class", "infiniband"))
	if got := devices["18:00.0"]; got != "ibp0" {
		t.Fatalf("discoverRDMADevicesByPCI() = %#v, want 18:00.0 -> ibp0", devices)
	}
}

func TestGetDeviceConfig(t *testing.T) {
	instance := &Instance{}
	if config := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{}); config != nil {
		t.Fatalf("GetDeviceConfig() = %#v, want nil", config)
	}
}
