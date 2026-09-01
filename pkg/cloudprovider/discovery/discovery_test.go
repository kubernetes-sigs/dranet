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

package discovery

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/dranet/pkg/cloudprovider/coreweave"
)

func TestDiscoverCloudProviderDetectsCKS(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "cks-node",
		Labels: map[string]string{
			coreweave.LabelCKSCluster: "use15",
		},
	}}
	dependencies := Dependencies{
		NodeClient: fake.NewSimpleClientset(node).CoreV1().Nodes(),
		NodeName:   node.Name,
	}

	if got := DiscoverCloudProviderWithDependencies(context.Background(), "", dependencies); got != CloudProviderHintCKS {
		t.Fatalf("DiscoverCloudProviderWithDependencies() = %q, want %q", got, CloudProviderHintCKS)
	}
}

func TestGetInstancePropertiesCKS(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "cks-node",
		Labels: map[string]string{
			coreweave.LabelCKSCluster:   "use15",
			coreweave.LabelFabricFlavor: "infiniband",
			coreweave.LabelFabric:       "US-EAST-15A-FAB66",
		},
	}}
	dependencies := Dependencies{
		NodeClient: fake.NewSimpleClientset(node).CoreV1().Nodes(),
		NodeName:   node.Name,
	}

	instance, err := GetInstancePropertiesWithDependencies(context.Background(), CloudProviderHintCKS, "", dependencies)
	if err != nil {
		t.Fatalf("GetInstancePropertiesWithDependencies() error = %v", err)
	}
	if _, ok := instance.(*coreweave.Instance); !ok {
		t.Fatalf("GetInstancePropertiesWithDependencies() = %T, want *coreweave.Instance", instance)
	}
}

func TestGetInstancePropertiesCKSRequiresDependencies(t *testing.T) {
	if _, err := GetInstanceProperties(context.Background(), CloudProviderHintCKS, ""); err == nil {
		t.Fatal("GetInstanceProperties() error = nil, want missing Kubernetes dependency error")
	}
}
