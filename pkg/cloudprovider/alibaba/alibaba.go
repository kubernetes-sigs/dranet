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

package alibaba

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/inventory"
)

const (
	AlibabaAttrPrefix = "alibaba.dra.net"

	AttrInstanceType = AlibabaAttrPrefix + "/" + "instanceType"
	AttrERDMA        = AlibabaAttrPrefix + "/" + "erdma"

	imdsEndpoint  = "http://100.100.100.200/latest"
	imdsTokenPath = "/api/token"
	imdsTokenTTL  = "21600"
)

var _ cloudprovider.CloudInstance = (*AlibabaInstance)(nil)

type AlibabaInstance struct {
	InstanceType     string
	ERDMAPCIAddresses sets.Set[string]
}

// OnAlibaba returns true if running on an Alibaba Cloud ECS instance.
func OnAlibaba(ctx context.Context) bool {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wait.PollUntilContextCancel(pollCtx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		token, err := fetchIMDSToken(ctx)
		if err != nil {
			return false, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+"/meta-data/instance-id", nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("X-aliyun-ecs-metadata-token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}) == nil
}

// GetInstance retrieves Alibaba Cloud instance metadata via IMDS.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	instanceType, err := queryIMDS(ctx, "/meta-data/instance/instance-type")
	if err != nil {
		klog.Infof("could not get Alibaba instance type: %v", err)
	}

	erdmaPCIAddresses := detectERDMAPCIAddresses()
	klog.Infof("Alibaba Cloud instance: type=%q erdma=%v", instanceType, erdmaPCIAddresses.UnsortedList())

	return &AlibabaInstance{
		InstanceType:     instanceType,
		ERDMAPCIAddresses: erdmaPCIAddresses,
	}, nil
}

func (a *AlibabaInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	if a.InstanceType != "" {
		attributes[AttrInstanceType] = resourceapi.DeviceAttribute{StringValue: &a.InstanceType}
	}
	if id.PCIAddress != "" && a.ERDMAPCIAddresses.Has(id.PCIAddress) {
		v := true
		attributes[AttrERDMA] = resourceapi.DeviceAttribute{BoolValue: &v}
	}
	return attributes
}

func (a *AlibabaInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	// LACP-bonded interfaces (common on HPN GPU nodes, e.g. H20 with dual-port
	// CX7 NICs bonded at the infrastructure level) cannot be moved into a pod
	// network namespace without breaking link aggregation. When such a bond is
	// claimed, transparently request an IPVlan subinterface instead of moving
	// the bond, so claiming a bond feels identical to claiming a regular NIC
	// (issue #239). This is a provider-internal decision: the user's
	// ResourceClaim YAML never mentions the subInterface field.
	if id.Name != "" && isLACPBond(id.Name) {
		return &apis.NetworkConfig{
			SubInterface: &apis.SubInterfaceConfig{
				Type: apis.SubInterfaceTypeIPVlan,
			},
		}
	}
	return nil
}

// isLACPBond reports whether the named interface is a bond in 802.3ad (LACP)
// mode. It is a package var so tests can override it without a real bond.
var isLACPBond = inventory.IsLACPBond

// detectERDMAPCIAddresses returns the PCI addresses of eRDMA devices found in
// /sys/class/infiniband/ by following the device symlink of each erdma_* entry.
var detectERDMAPCIAddresses = func() sets.Set[string] {
	addrs := sets.New[string]()
	entries, err := os.ReadDir("/sys/class/infiniband")
	if err != nil {
		return addrs
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "erdma") {
			continue
		}
		deviceLink := filepath.Join("/sys/class/infiniband", entry.Name(), "device")
		target, err := os.Readlink(deviceLink)
		if err != nil {
			klog.V(4).Infof("could not read device symlink for %s: %v", entry.Name(), err)
			continue
		}
		addrs.Insert(filepath.Base(target))
	}
	return addrs
}

func fetchIMDSToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsEndpoint+imdsTokenPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aliyun-ecs-metadata-token-ttl-seconds", imdsTokenTTL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IMDS token request returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func queryIMDS(ctx context.Context, path string) (string, error) {
	var result string
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		token, err := fetchIMDSToken(ctx)
		if err != nil {
			klog.V(4).Infof("IMDS token fetch failed: %v", err)
			return false, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+path, nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("X-aliyun-ecs-metadata-token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			klog.V(4).Infof("IMDS request to %s failed: %v", path, err)
			return false, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, nil
		}
		result = strings.TrimSpace(string(body))
		return true, nil
	})
	return result, err
}
