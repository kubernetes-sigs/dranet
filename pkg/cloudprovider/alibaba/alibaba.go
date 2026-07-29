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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/internal/nlwrap"
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
	// LACP bonds can't be moved into a pod netns without breaking link
	// aggregation, so claiming one transparently gets an IPVlan subinterface
	// instead (issue #239). Everything else, including eRDMA, needs no
	// special config here and is moved into the pod netns as-is.
	if id.Name == "" || !isLACPBond(id.Name) {
		return nil
	}

	prefix, err := getNICIPv6Prefix(id.Name)
	if err != nil {
		klog.Warningf("could not determine eflo RDMA IPv6 prefix for bond %s: %v", id.Name, err)
		return nil
	}

	ipRange, err := efloRDMASubinterfaceRange(prefix)
	if err != nil {
		klog.Warningf("could not derive eflo RDMA subinterface range for bond %s: %v", id.Name, err)
		return nil
	}

	return &apis.NetworkConfig{
		SubInterface: &apis.SubInterfaceConfig{
			Type:     apis.SubInterfaceTypeIPVlan,
			IPRanges: []apis.IPRangeConfig{{CIDR: ipRange}},
		},
	}
}

// isLACPBond reports whether the named interface is a bond in 802.3ad (LACP)
// mode. It is a package var so tests can override it without a real bond.
var isLACPBond = inventory.IsLACPBond

// efloRDMABlockSuffix is the fixed 64-bit host-part offset eflo reserves, out
// of every RDMA NIC's own /64 prefix, for that NIC's pod-facing IPVlan
// subinterface addresses: 0000:000f:0000:0c00. Only the low 4 bits vary
// (0c00-0c0f), giving a 16-address /124 block. The prefix itself is left
// untouched, so this range is disjoint from whatever address the NIC already
// carries on the host -- no address is shared between host and pod, so no
// stripping/restoring of host addresses is needed.
var efloRDMABlockSuffix = [8]byte{0x00, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0c, 0x00}

// efloRDMASubinterfaceRange derives the eflo-reserved /124 IPv6 range for the
// pod-facing subinterface from the RDMA NIC's own /64 prefix, by replacing
// the host portion of the prefix with eflo's fixed RDMA block offset.
func efloRDMASubinterfaceRange(prefix *net.IPNet) (string, error) {
	ones, bits := prefix.Mask.Size()
	if bits != 128 {
		return "", fmt.Errorf("address %s is not IPv6", prefix)
	}
	if ones != 64 {
		return "", fmt.Errorf("expected a /64 prefix, got /%d for %s", ones, prefix)
	}

	rangeIP := make(net.IP, 16)
	copy(rangeIP[0:8], prefix.IP.To16()[0:8])
	copy(rangeIP[8:16], efloRDMABlockSuffix[:])

	return (&net.IPNet{IP: rangeIP, Mask: net.CIDRMask(124, 128)}).String(), nil
}

// getNICIPv6Prefix returns the /64 network prefix of the first global-scope
// IPv6 address configured on ifName in the host network namespace. It is a
// package var so tests can override it without a real interface.
var getNICIPv6Prefix = func(ifName string) (*net.IPNet, error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("could not find interface %s: %w", ifName, err)
	}
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, fmt.Errorf("could not list IPv6 addresses for %s: %w", ifName, err)
	}
	for _, addr := range addrs {
		if !addr.IP.IsGlobalUnicast() {
			continue
		}
		ones, bits := addr.IPNet.Mask.Size()
		if bits != 128 || ones != 64 {
			continue
		}
		return &net.IPNet{IP: addr.IP.Mask(addr.IPNet.Mask), Mask: addr.IPNet.Mask}, nil
	}
	return nil, fmt.Errorf("no global /64 IPv6 address found on %s", ifName)
}

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
