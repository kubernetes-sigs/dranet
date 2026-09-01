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
	"fmt"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	AttrPrefix = "coreweave.dra.net"

	AttrFabricFlavor  = AttrPrefix + "/fabricFlavor"
	AttrFabric        = AttrPrefix + "/fabric"
	AttrSuperpod      = AttrPrefix + "/superpod"
	AttrLeafgroup     = AttrPrefix + "/leafgroup"
	AttrLeafgroupName = AttrPrefix + "/leafgroupName"
	AttrRack          = AttrPrefix + "/rack"
	AttrSpeedCurrent  = AttrPrefix + "/speedCurrent"
	AttrSpeedExpected = AttrPrefix + "/speedExpected"
	AttrInstanceType  = AttrPrefix + "/instanceType"
	AttrLeafSwitch    = AttrPrefix + "/leafSwitch"

	LabelCKSCluster    = "cks.coreweave.com/cluster"
	LabelFabricFlavor  = "backend.coreweave.cloud/flavor"
	LabelFabric        = "backend.coreweave.cloud/fabric"
	LabelSuperpod      = "backend.coreweave.cloud/superpod"
	LabelLeafgroup     = "backend.coreweave.cloud/leafgroup"
	LabelLeafgroupName = "backend.coreweave.cloud/leafgroup-name"
	LabelRack          = "node.coreweave.cloud/rack"
	LabelSpeedCurrent  = "backend.coreweave.cloud/speed.current"
	LabelSpeedExpected = "backend.coreweave.cloud/speed.expected"
	LabelInstanceType  = "node.kubernetes.io/instance-type"

	neighborLabelPrefix = "backend.coreweave.cloud/neighbors.expected."
	neighborLabelSuffix = ".device"
	defaultRDMASysfs    = "/sys/class/infiniband"
)

var nodeAttributeLabels = map[resourceapi.QualifiedName]string{
	AttrFabricFlavor:  LabelFabricFlavor,
	AttrFabric:        LabelFabric,
	AttrSuperpod:      LabelSuperpod,
	AttrLeafgroup:     LabelLeafgroup,
	AttrLeafgroupName: LabelLeafgroupName,
	AttrRack:          LabelRack,
	AttrSpeedCurrent:  LabelSpeedCurrent,
	AttrSpeedExpected: LabelSpeedExpected,
	AttrInstanceType:  LabelInstanceType,
}

var _ cloudprovider.CloudInstance = (*Instance)(nil)

// Instance contains the CKS node metadata used to enrich network devices.
// CKS publishes this information as Kubernetes Node labels instead of through
// an instance metadata service.
type Instance struct {
	labels          map[string]string
	rdmaDeviceByPCI map[string]string
}

// OnCKS reports whether nodeName belongs to a CoreWeave Kubernetes Service
// cluster. The CKS cluster label is present on both compute and GPU nodes.
func OnCKS(ctx context.Context, nodes corev1client.NodeInterface, nodeName string) bool {
	if nodes == nil || nodeName == "" {
		return false
	}
	node, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.V(4).Infof("could not get Node %q for CKS detection: %v", nodeName, err)
		return false
	}
	return node.Labels[LabelCKSCluster] != ""
}

// GetInstance reads the local CKS Node labels and discovers the PCI-to-RDMA
// device mapping used by CKS per-interface topology labels.
func GetInstance(ctx context.Context, nodes corev1client.NodeInterface, nodeName string) (cloudprovider.CloudInstance, error) {
	if nodes == nil {
		return nil, fmt.Errorf("kubernetes Node client is required for CKS")
	}
	if nodeName == "" {
		return nil, fmt.Errorf("node name is required for CKS")
	}

	node, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get CKS Node %q: %w", nodeName, err)
	}
	if node.Labels[LabelCKSCluster] == "" {
		return nil, fmt.Errorf("node %q does not have the %q CKS label", nodeName, LabelCKSCluster)
	}

	labels := make(map[string]string, len(node.Labels))
	for key, value := range node.Labels {
		labels[key] = value
	}

	instance := &Instance{
		labels:          labels,
		rdmaDeviceByPCI: discoverRDMADevicesByPCI(defaultRDMASysfs),
	}
	klog.Infof("CoreWeave CKS node %s: fabric flavor=%q fabric=%q superpod=%q leafgroup=%q",
		nodeName, labels[LabelFabricFlavor], labels[LabelFabric], labels[LabelSuperpod], labels[LabelLeafgroup])
	return instance, nil
}

// GetDeviceAttributes returns CKS fabric topology attributes for a device.
func (i *Instance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	for attribute, label := range nodeAttributeLabels {
		addStringAttribute(attributes, attribute, i.labels[label])
	}

	rdmaDevice := i.rdmaDeviceByPCI[normalizePCIAddress(id.PCIAddress)]
	if rdmaDevice == "" && strings.HasPrefix(id.Name, "ibp") {
		rdmaDevice = id.Name
	}
	if rdmaDevice != "" {
		label := neighborLabelPrefix + rdmaDevice + neighborLabelSuffix
		addStringAttribute(attributes, AttrLeafSwitch, i.labels[label])
	}

	return attributes
}

// GetDeviceConfig returns nil because CKS topology labels do not define an IP,
// subnet, gateway, or other per-claim network allocation contract.
func (i *Instance) GetDeviceConfig(cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	return nil
}

func addStringAttribute(attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, name resourceapi.QualifiedName, value string) {
	if value == "" {
		return
	}
	if len(value) > resourceapi.DeviceAttributeMaxValueLength {
		klog.Warningf("ignoring CKS attribute %s: value exceeds %d bytes", name, resourceapi.DeviceAttributeMaxValueLength)
		return
	}
	attributes[name] = resourceapi.DeviceAttribute{StringValue: ptr.To(value)}
}

func discoverRDMADevicesByPCI(sysfsRoot string) map[string]string {
	devices := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(sysfsRoot, "*", "device"))
	if err != nil {
		klog.V(4).Infof("could not enumerate RDMA devices in %s: %v", sysfsRoot, err)
		return devices
	}
	for _, path := range paths {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			klog.V(4).Infof("could not resolve RDMA device path %s: %v", path, err)
			continue
		}
		pciAddress := normalizePCIAddress(filepath.Base(target))
		if pciAddress == "" {
			continue
		}
		rdmaDevice := filepath.Base(filepath.Dir(path))
		if _, exists := devices[pciAddress]; !exists {
			devices[pciAddress] = rdmaDevice
		}
	}
	return devices
}

func normalizePCIAddress(address string) string {
	return strings.ToLower(strings.TrimPrefix(address, "0000:"))
}
