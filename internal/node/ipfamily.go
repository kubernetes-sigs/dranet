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

// Package node derives information about the cluster from the Node object
// of the host DRANET runs on.
package node

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// IPFamilies returns the IP families of the node's host IPs. It follows the
// same heuristic that kube-proxy uses to detect the cluster IP family
// (k8s.io/kubernetes/pkg/util/node.GetNodeHostIPs): the node IPs are the
// first InternalIP or ExternalIP address plus the first address of the other
// family, if any, so the families are those of all the InternalIP and
// ExternalIP addresses. An empty set means the node has no usable addresses
// yet, so the family can not be determined.
func IPFamilies(node *v1.Node) sets.Set[v1.IPFamily] {
	families := sets.New[v1.IPFamily]()
	if node == nil {
		return families
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type != v1.NodeInternalIP && addr.Type != v1.NodeExternalIP {
			continue
		}
		switch netutils.IPFamilyOfString(addr.Address) {
		case netutils.IPv4:
			families.Insert(v1.IPv4Protocol)
		case netutils.IPv6:
			families.Insert(v1.IPv6Protocol)
		}
	}
	return families
}

// DetectIPFamilies returns the IP families of the cluster on this node,
// derived from the node addresses the kubelet publishes. The kubelet may
// not have published the addresses yet when DRANET starts, so the node is
// polled every pollInterval until they show up or timeout expires. On
// timeout it returns an empty set.
func DetectIPFamilies(ctx context.Context, nodeClient corev1client.NodeInterface, nodeName string, pollInterval, timeout time.Duration) sets.Set[v1.IPFamily] {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	families := sets.New[v1.IPFamily]()
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		node, err := nodeClient.Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.V(2).Infof("Waiting for node %q to exist: %v", nodeName, err)
			return false, nil
		}
		families = IPFamilies(node)
		return families.Len() > 0, nil
	})
	if err != nil {
		klog.Warningf("Could not determine the IP families of node %q: %v", nodeName, err)
		return sets.New[v1.IPFamily]()
	}
	klog.Infof("Detected node %q IP families: %v", nodeName, sets.List(families))
	return families
}
