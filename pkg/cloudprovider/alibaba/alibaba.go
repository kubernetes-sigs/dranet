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
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	AlibabaAttrPrefix = "alibaba.dra.net"

	AttrInstanceType = AlibabaAttrPrefix + "/" + "instanceType"

	// Alibaba Cloud ECS Instance Metadata Service endpoint.
	imdsEndpoint = "http://100.100.100.200/latest"
	// IMDSv2 token endpoint and header.
	imdsTokenPath = "/api/token"
	imdsTokenTTL  = "21600"
)

var _ cloudprovider.CloudInstance = (*AlibabaInstance)(nil)

// AlibabaInstance holds Alibaba Cloud instance metadata relevant to network device configuration.
type AlibabaInstance struct {
	InstanceType string
	IsHPN        bool
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

	isHPN := detectHPN(instanceType)
	klog.Infof("Alibaba Cloud instance: type=%q hpn=%v", instanceType, isHPN)

	return &AlibabaInstance{
		InstanceType: instanceType,
		IsHPN:        isHPN,
	}, nil
}

// GetDeviceAttributes returns Alibaba Cloud-specific attributes for a device.
func (a *AlibabaInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	if a.InstanceType != "" {
		attributes[AttrInstanceType] = resourceapi.DeviceAttribute{StringValue: &a.InstanceType}
	}
	return attributes
}

// GetDeviceConfig returns a NetworkConfig that signals IPvlan mode for
// HPN bond devices with global IPv6. Non-HPN, non-bond, or bonds
// without global IPv6 return nil.
func (a *AlibabaInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	if !a.IsHPN || !isHPNBondDevice(id.Name) {
		return nil
	}
	if !hasGlobalIPv6Func(id.Name) {
		klog.V(4).Infof("HPN bond device %s has no global IPv6, skipping IPvlan config", id.Name)
		return nil
	}
	return &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			IPVlan: &apis.IPVlanConfig{
				Mode: "l2",
				Flag: "bridge",
				Addressing: &apis.IPVlanAddressConfig{
					Type: apis.IPVlanAddrParentIPv6PrefixPodIPv4,
				},
				CopyRoutesFromParent:    ptr.To(true),
				CopyNeighborsFromParent: ptr.To(true),
			},
		},
	}
}

// isHPNBondDevice returns true if the device name indicates it is an HPN
// bond master interface (e.g. bond0, bond1). Bond slaves (reth*) and other
// interfaces are not HPN devices.
func isHPNBondDevice(name string) bool {
	return strings.HasPrefix(name, "bond")
}

// hasGlobalIPv6Func is the function used to check if an interface has global
// IPv6. It can be overridden in tests.
var hasGlobalIPv6Func = hasGlobalIPv6

// hasGlobalIPv6 checks whether the named interface has at least one
// non-link-local IPv6 address configured.
func hasGlobalIPv6(ifName string) bool {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return false
	}
	addrs, err := netlink.AddrList(link, unix.AF_INET6)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if !addr.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// detectHPN determines whether this instance is a HPN machine.
// Only called when we already know we're on Alibaba Cloud (via IMDS
// reachability or --cloud-provider-hint=ALIBABA).
func detectHPN(instanceType string) bool {
	lower := strings.ToLower(instanceType)
	if strings.Contains(lower, "hpn") || strings.Contains(lower, "efg") {
		return true
	}
	// On confirmed Alibaba Cloud instances where IMDS didn't return instance type
	// (e.g. bare-metal), check for RDMA infiniband devices as HPN indicator.
	if instanceType == "" {
		if entries, err := os.ReadDir("/sys/class/infiniband"); err == nil && len(entries) > 0 {
			klog.V(2).Infof("Alibaba Cloud instance with %d infiniband devices, assuming HPN", len(entries))
			return true
		}
	}
	return false
}

// fetchIMDSToken obtains a session token for IMDSv2.
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

// queryIMDS fetches a single metadata value from Alibaba Cloud IMDS (v2 with token).
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
