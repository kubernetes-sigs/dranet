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

package node

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
)

var testBackoff = wait.Backoff{
	Duration: 5 * time.Millisecond,
	Factor:   2,
	Steps:    4,
	Cap:      25 * time.Millisecond,
}

func TestIPFamilies(t *testing.T) {
	tests := []struct {
		name      string
		addresses []v1.NodeAddress
		want      sets.Set[v1.IPFamily]
	}{
		{
			name: "no addresses",
			want: sets.New[v1.IPFamily](),
		},
		{
			name: "ipv4 only",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: "node1"},
			},
			want: sets.New(v1.IPv4Protocol),
		},
		{
			name: "ipv6 only",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fd00::1"},
			},
			want: sets.New(v1.IPv6Protocol),
		},
		{
			name: "dual stack",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fd00::1"},
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			},
			want: sets.New(v1.IPv4Protocol, v1.IPv6Protocol),
		},
		{
			name: "external ip of the other family makes the node dual stack",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeExternalIP, Address: "2001:db8::1"},
			},
			want: sets.New(v1.IPv4Protocol, v1.IPv6Protocol),
		},
		{
			name: "external ip only",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeExternalIP, Address: "2001:db8::1"},
				{Type: v1.NodeHostName, Address: "node1"},
			},
			want: sets.New(v1.IPv6Protocol),
		},
		{
			name: "hostname and other address types are ignored",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: "10.0.0.1"},
				{Type: v1.NodeInternalDNS, Address: "fd00::1"},
			},
			want: sets.New[v1.IPFamily](),
		},
		{
			name: "invalid addresses are ignored",
			addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "not-an-ip"},
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			},
			want: sets.New(v1.IPv4Protocol),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{}
			node.Status.Addresses = tt.addresses
			if diff := cmp.Diff(tt.want, IPFamilies(node)); diff != "" {
				t.Errorf("IPFamilies() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	if got := IPFamilies(nil); got.Len() != 0 {
		t.Errorf("IPFamilies(nil) = %v, want empty", got)
	}
}

func TestDetectIPFamilies(t *testing.T) {
	nodeName := "node1"

	tests := []struct {
		name  string
		nodes []*v1.Node
		want  sets.Set[v1.IPFamily]
	}{
		{
			name: "ipv4 node",
			nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}}},
			}},
			want: sets.New(v1.IPv4Protocol),
		},
		{
			name: "ipv6 node",
			nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "fd00::1"}}},
			}},
			want: sets.New(v1.IPv6Protocol),
		},
		{
			name: "dual-stack node",
			nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
					{Type: v1.NodeInternalIP, Address: "fd00::1"},
					{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				}},
			}},
			want: sets.New(v1.IPv4Protocol, v1.IPv6Protocol),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.nodes))
			for _, n := range tt.nodes {
				objs = append(objs, n)
			}
			client := fake.NewSimpleClientset(objs...)
			got := DetectIPFamilies(context.Background(), client.CoreV1().Nodes(), nodeName)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("DetectIPFamilies() mismatch (-want +got):\n%s", diff)
			}
			if actions := client.Actions(); len(actions) != 1 {
				t.Errorf("DetectIPFamilies() made %d API calls, want 1", len(actions))
			}
		})
	}
}

func TestDetectIPFamiliesContextCancelled(t *testing.T) {
	nodeName := "node1"

	tests := []struct {
		name  string
		nodes []*v1.Node
	}{
		{
			name:  "node does not exist returns empty on cancel",
			nodes: nil,
		},
		{
			name: "node without addresses returns empty on cancel",
			nodes: []*v1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.nodes))
			for _, n := range tt.nodes {
				objs = append(objs, n)
			}
			client := fake.NewSimpleClientset(objs...)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()

			got := detectIPFamilies(ctx, client.CoreV1().Nodes(), nodeName, testBackoff)
			if got.Len() != 0 {
				t.Errorf("detectIPFamilies() = %v, want empty", got)
			}
			if len(client.Actions()) < 2 {
				t.Errorf("detectIPFamilies() made %d API calls before cancel, want retries", len(client.Actions()))
			}
		})
	}
}

func TestDetectIPFamiliesWaitsForAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	nodeName := "node1"
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	client := fake.NewSimpleClientset(node)

	// Publish the addresses after several backoff intervals, as the cloud
	// controller manager does once it initializes the node.
	go func() {
		time.Sleep(60 * time.Millisecond)
		updated := node.DeepCopy()
		updated.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "fd00::1"}}
		if _, err := client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			t.Errorf("failed to update node status: %v", err)
		}
	}()

	got := detectIPFamilies(ctx, client.CoreV1().Nodes(), nodeName, testBackoff)
	if diff := cmp.Diff(sets.New(v1.IPv6Protocol), got); diff != "" {
		t.Errorf("detectIPFamilies() mismatch (-want +got):\n%s", diff)
	}
}

func TestDetectIPFamiliesWaitsForNodeCreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	nodeName := "node1"
	client := fake.NewSimpleClientset()

	go func() {
		time.Sleep(40 * time.Millisecond)
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}}},
		}
		if _, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Errorf("failed to create node: %v", err)
		}
	}()

	got := detectIPFamilies(ctx, client.CoreV1().Nodes(), nodeName, testBackoff)
	if diff := cmp.Diff(sets.New(v1.IPv4Protocol), got); diff != "" {
		t.Errorf("detectIPFamilies() mismatch (-want +got):\n%s", diff)
	}
}
