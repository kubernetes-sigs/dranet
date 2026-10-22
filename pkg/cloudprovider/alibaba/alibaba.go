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
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/inventory"
	"sigs.k8s.io/dranet/pkg/ipam"
)

const (
	AlibabaAttrPrefix = "alibaba.dra.net"

	AttrInstanceType = AlibabaAttrPrefix + "/" + "instanceType"
	AttrERDMA        = AlibabaAttrPrefix + "/" + "erdma"

	imdsEndpoint  = "http://100.100.100.200/latest"
	imdsTokenPath = "/api/token"
	imdsTokenTTL  = "21600"
)

var (
	_ cloudprovider.CloudInstance   = (*AlibabaInstance)(nil)
	_ cloudprovider.ProfileProvider = (*AlibabaInstance)(nil)
)

type AlibabaInstance struct {
	InstanceType      string
	ERDMAPCIAddresses sets.Set[string]

	// localIPAM is seeded with addresses restored from the driver checkpoint.
	localIPAM *ipam.LocalIPAM
}

const alibabaEfloSubinterfaceProfile = "alibaba-eflo-subinterface"

// efloGatewayMAC is the virtual gateway MAC used by the eFLO RDMA fabric.
// Keep it scoped to this profile; it is not a general cloud gateway default.
const efloGatewayMAC = "00:00:5e:00:01:01"

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
func GetInstance(ctx context.Context, opts ...Option) (cloudprovider.CloudInstance, error) {
	instanceType, err := queryIMDS(ctx, "/meta-data/instance/instance-type")
	if err != nil {
		klog.Infof("could not get Alibaba instance type: %v", err)
	}

	erdmaPCIAddresses := detectERDMAPCIAddresses()
	klog.Infof("Alibaba Cloud instance: type=%q erdma=%v", instanceType, erdmaPCIAddresses.UnsortedList())

	instance := &AlibabaInstance{
		InstanceType:      instanceType,
		ERDMAPCIAddresses: erdmaPCIAddresses,
		localIPAM:         ipam.NewLocalIPAM(nil),
	}
	for _, opt := range opts {
		opt(instance)
	}
	return instance, nil
}

type Option func(*AlibabaInstance)

func WithReservedAddresses(addrs []string) Option {
	return func(instance *AlibabaInstance) {
		instance.localIPAM = ipam.NewLocalIPAM(addrs)
	}
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
	// Moving an LACP bond into a pod netns would break link aggregation. Only
	// eFLO RDMA bonds are served through a subinterface, which requires the
	// IPv6 address the block is derived from; an IPv4-only bond is not an eFLO
	// device and keeps the default handling.
	if id.Name == "" || !inventory.IsLACPBond(id.Name) || !hasEfloIPv6(id.Name) {
		return nil
	}

	return &apis.NetworkConfig{
		Profile: alibabaEfloSubinterfaceProfile,
		Interface: apis.InterfaceConfig{
			Type:        apis.InterfaceTypeIPVLAN,
			AddrGenMode: ptr.To[int32](1),
		},
	}
}

// GetProfileConfig allocates an eFLO subinterface address.
func (a *AlibabaInstance) GetProfileConfig(id cloudprovider.DeviceIdentifiers, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	if config == nil || !config.Interface.IsSubinterface() {
		return nil, nil
	}
	if a.localIPAM == nil {
		return nil, fmt.Errorf("alibaba profile IPAM is not initialized for subinterface device %q", id.Name)
	}

	prefix, parentRoutes, err := getNICIPv6Config(id.Name)
	if err != nil {
		return nil, fmt.Errorf("determining eflo RDMA IPv6 config for device %q: %w", id.Name, err)
	}

	if len(config.Interface.Addresses) > 0 {
		if err := a.localIPAM.Reserve(config.Interface.Addresses); err != nil {
			return nil, fmt.Errorf("reserving static subinterface addresses for device %q: %w", id.Name, err)
		}
		return efloSourceRoutingConfig(id.Name, config, nil, parentRoutes), nil
	}

	ranges, err := a.subinterfaceRanges(id.Name, prefix)
	if err != nil {
		return nil, err
	}
	addrs, err := a.localIPAM.Allocate(ranges)
	if err != nil {
		return nil, fmt.Errorf("allocating subinterface addresses for device %q: %w", id.Name, err)
	}
	return efloSourceRoutingConfig(id.Name, config, addrs, parentRoutes), nil
}

// efloSourceRoutingConfig returns the allocated addresses plus PBR for them:
// the parent's gateway IPv6 routes moved to a per-device table and one source rule
// per address. If the merged config already defines routes, rules or a VRF,
// routing is user-owned and nothing is synthesized.
func efloSourceRoutingConfig(ifName string, config *apis.NetworkConfig, allocated []string, parentRoutes []apis.RouteConfig) *apis.NetworkConfig {
	profile := &apis.NetworkConfig{Interface: apis.InterfaceConfig{Addresses: allocated}}

	addrs := allocated
	if len(addrs) == 0 {
		addrs = config.Interface.Addresses
	}
	if len(config.Routes) > 0 || len(config.Rules) > 0 || config.Interface.VRF != nil {
		if len(allocated) == 0 {
			return nil
		}
		return profile
	}

	tableID := apis.TableIDForName(ifName)
	gateways := sets.New[string]()
	for i := range parentRoutes {
		route := parentRoutes[i]
		route.Table = tableID
		profile.Routes = append(profile.Routes, route)
		// Without a link-local address, RDMA cannot dynamically resolve even
		// the gateway on a cold neighbor cache. Install its fabric MAC directly.
		if route.Gateway != "" && !gateways.Has(route.Gateway) {
			profile.Neighbors = append(profile.Neighbors, apis.NeighborConfig{
				Destination: route.Gateway, HardwareAddr: efloGatewayMAC,
			})
			gateways.Insert(route.Gateway)
		}
	}
	for _, addrStr := range addrs {
		prefix, err := netip.ParsePrefix(addrStr)
		if err != nil {
			continue
		}
		// Host prefix so rules for sibling subinterfaces never overlap.
		profile.Rules = append(profile.Rules, apis.RuleConfig{
			Source:   netip.PrefixFrom(prefix.Addr(), prefix.Addr().BitLen()).String(),
			Table:    tableID,
			Priority: apis.SourceRoutingRulePriority,
		})
	}
	if len(allocated) == 0 && len(profile.Routes) == 0 && len(profile.Rules) == 0 {
		return nil
	}
	return profile
}

// ReleaseProfileConfig releases subinterface addresses.
func (a *AlibabaInstance) ReleaseProfileConfig(id cloudprovider.DeviceIdentifiers, claimUID types.UID, config *apis.NetworkConfig) error {
	if config == nil || !config.Interface.IsSubinterface() || a.localIPAM == nil {
		return nil
	}
	for _, addr := range config.Interface.Addresses {
		a.localIPAM.Release(addr)
	}
	return nil
}

func (a *AlibabaInstance) subinterfaceRanges(bondName string, prefix *net.IPNet) ([]ipam.IPRange, error) {
	cidr, err := efloRDMASubinterfaceRange(prefix)
	if err != nil {
		return nil, fmt.Errorf("deriving eflo RDMA subinterface range for bond %s: %w", bondName, err)
	}
	start, end, err := cloudprovider.IPRangeFromCIDR(cidr, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("deriving allocation range from %q: %w", cidr, err)
	}
	return []ipam.IPRange{{Start: start, End: end}}, nil
}

// hasEfloIPv6 reports whether ifName carries the IPv6 address the eFLO
// subinterface block is derived from. Link-local addresses are never usable
// for this, and neither is an IPv4-only interface.
func hasEfloIPv6(ifName string) bool {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return false
	}
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if !addr.IP.IsGlobalUnicast() {
			continue
		}
		if ones, bits := addr.Mask.Size(); bits == 128 && ones <= 124 {
			return true
		}
	}
	return false
}

// eFLO reserves 0000:000f:0000:0c00/124 within each RDMA NIC's IPv6 network.
var efloRDMABlockSuffix = [8]byte{0x00, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0c, 0x00}

func efloRDMASubinterfaceRange(prefix *net.IPNet) (string, error) {
	ones, bits := prefix.Mask.Size()
	if bits != 128 {
		return "", fmt.Errorf("address %s is not IPv6", prefix)
	}
	if ones > 124 {
		return "", fmt.Errorf("expected prefix length <= /124 for eFLO range, got /%d for %s", ones, prefix)
	}

	pattern := make(net.IP, net.IPv6len)
	copy(pattern[8:], efloRDMABlockSuffix[:])

	rangeIP := make(net.IP, 16)
	prefixIP := prefix.IP.To16()
	if prefixIP == nil {
		return "", fmt.Errorf("invalid IPv6 prefix %s", prefix)
	}
	copy(rangeIP, prefixIP)
	for i := ones; i < 124; i++ {
		if getIPBit(pattern, i) {
			setIPBit(rangeIP, i, true)
		} else {
			setIPBit(rangeIP, i, false)
		}
	}

	return (&net.IPNet{IP: rangeIP, Mask: net.CIDRMask(124, 128)}).String(), nil
}

func getIPBit(ip net.IP, i int) bool {
	byteOffset := i / 8
	bitOffset := uint(7 - i%8)
	return ip[byteOffset]&(1<<bitOffset) != 0
}

func setIPBit(ip net.IP, i int, set bool) {
	byteOffset := i / 8
	bitOffset := uint(7 - i%8)
	if set {
		ip[byteOffset] |= 1 << bitOffset
	} else {
		ip[byteOffset] &^= 1 << bitOffset
	}
}

// getNICIPv6Config returns the global IPv6 prefix (<= /124) of ifName together
// with its IPv6 routes, which seed the eFLO subinterface source-routing config.
func getNICIPv6Config(ifName string) (*net.IPNet, []apis.RouteConfig, error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, nil, fmt.Errorf("could not find interface %s: %w", ifName, err)
	}
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, nil, fmt.Errorf("could not list IPv6 addresses for %s: %w", ifName, err)
	}
	var parentIP net.IP
	var prefix *net.IPNet
	for _, addr := range addrs {
		if !addr.IP.IsGlobalUnicast() {
			continue
		}
		ones, bits := addr.Mask.Size()
		if bits != 128 || ones > 124 {
			continue
		}
		parentIP = addr.IP
		prefix = &net.IPNet{IP: addr.IP.Mask(addr.Mask), Mask: addr.Mask}
		break
	}
	if parentIP == nil {
		return nil, nil, fmt.Errorf("no global IPv6 address with prefix length <= /124 found on %s", ifName)
	}
	filter := &netlink.Route{LinkIndex: link.Attrs().Index}
	routes, err := nlwrap.RouteListFiltered(netlink.FAMILY_V6, filter, netlink.RT_FILTER_OIF)
	if err != nil {
		return nil, nil, err
	}
	result, err := efloRouteConfigs(ifName, routes)
	if err != nil {
		return nil, nil, err
	}
	if len(result) == 0 {
		return nil, nil, fmt.Errorf("no IPv6 routes found on %s", ifName)
	}
	return prefix, result, nil
}

func efloRouteConfigs(ifName string, routes []netlink.Route) ([]apis.RouteConfig, error) {
	result := make([]apis.RouteConfig, 0, len(routes)+1)
	gateways := sets.New[string]()
	for _, route := range routes {
		if route.Gw == nil {
			// Children have /128 addresses and must reach peers through the
			// gateway. Copying the parent's connected prefix would send peer
			// traffic directly and require dynamic NDP, which RDMA cannot
			// initiate without a usable link-local address on the child.
			continue
		}
		gateway := route.Gw.String()
		if !gateways.Has(gateway) {
			gatewayAddr, err := netip.ParseAddr(gateway)
			if err != nil {
				return nil, fmt.Errorf("invalid IPv6 gateway %q on %s: %w", gateway, ifName, err)
			}
			result = append(result, apis.RouteConfig{
				Destination: netip.PrefixFrom(gatewayAddr, 128).String(),
				Scope:       unix.RT_SCOPE_LINK,
			})
			gateways.Insert(gateway)
		}
		destination := "::/0"
		if route.Dst != nil {
			destination = route.Dst.String()
		}
		result = append(result, apis.RouteConfig{
			Destination: destination,
			Gateway:     gateway,
		})
	}
	return result, nil
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
