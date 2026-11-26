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

var defaultBackoff = wait.Backoff{
	Duration: time.Second,
	Factor:   2,
	Jitter:   0.2,
	Steps:    6,
	Cap:      30 * time.Second,
}

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
// derived from the node addresses published by the kubelet or cloud controller
// manager. Those addresses may not be present yet when DRANET starts (for
// example, while the node has the uninitialized taint), so DetectIPFamilies
// polls with exponential backoff until at least one usable address is present
// or ctx is cancelled. If ctx is cancelled first, it returns an empty set.
func DetectIPFamilies(ctx context.Context, nodeClient corev1client.NodeInterface, nodeName string) sets.Set[v1.IPFamily] {
	return detectIPFamilies(ctx, nodeClient, nodeName, defaultBackoff)
}

func detectIPFamilies(ctx context.Context, nodeClient corev1client.NodeInterface, nodeName string, backoff wait.Backoff) sets.Set[v1.IPFamily] {
	families := sets.New[v1.IPFamily]()
	loggedWaiting := false

	err := backoff.DelayFunc().Until(ctx, true, true, func(ctx context.Context) (bool, error) {
		node, err := nodeClient.Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			if !loggedWaiting {
				loggedWaiting = true
				klog.Infof("Waiting for node %q to determine IP families (excluding default-gateway uplinks of both IP families meanwhile): %v", nodeName, err)
			} else {
				klog.V(2).Infof("Waiting for node %q to exist: %v", nodeName, err)
			}
			return false, nil
		}
		families = IPFamilies(node)
		if families.Len() > 0 {
			return true, nil
		}
		if !loggedWaiting {
			loggedWaiting = true
			klog.Infof("Node %q has no IP addresses yet; excluding default-gateway uplinks of both IP families until addresses are published", nodeName)
		}
		return false, nil
	})
	if err != nil {
		klog.Warningf("Stopped waiting for IP families of node %q: %v", nodeName, err)
		return sets.New[v1.IPFamily]()
	}
	klog.Infof("Detected node %q IP families: %v", nodeName, sets.List(families))
	return families
}
