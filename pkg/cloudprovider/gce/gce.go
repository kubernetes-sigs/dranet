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

package gce

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/ipam"
)

// GPUDirectSupport represents the type of GPUDirect support for a given machine type.
type GPUDirectSupport string

const (
	GPUDirectTCPX  GPUDirectSupport = "GPUDirect-TCPX"
	GPUDirectTCPXO GPUDirectSupport = "GPUDirect-TCPXO"
	GPUDirectRDMA  GPUDirectSupport = "GPUDirect-RDMA"
)

const (
	GCEAttrPrefix = "gce.dra.net"

	AttrGCEBlock                = GCEAttrPrefix + "/" + "block"
	AttrGCESubBlock             = GCEAttrPrefix + "/" + "subBlock"
	AttrGCEHost                 = GCEAttrPrefix + "/" + "host"
	AttrGCENetworkName          = GCEAttrPrefix + "/" + "networkName"
	AttrGCENetworkProjectNumber = GCEAttrPrefix + "/" + "networkProjectNumber"
	AttrGCEIPAliases            = GCEAttrPrefix + "/" + "ipAliases"
	AttrGCEMachineType          = GCEAttrPrefix + "/" + "machineType"
)

var (
	// https://cloud.google.com/compute/docs/accelerator-optimized-machines#network-protocol
	// machine types have a one to one mapping to a network protocol in google cloud
	NetworkProtocolMap = map[string]GPUDirectSupport{
		"a3-highgpu-1g":  GPUDirectTCPX,  // 8 GPU 4 accelerator NICs
		"a3-highgpu-2g":  GPUDirectTCPX,  // "
		"a3-highgpu-4g":  GPUDirectTCPX,  // "
		"a3-highgpu-8g":  GPUDirectTCPX,  // "
		"a3-edgegpu-8g":  GPUDirectTCPX,  // "
		"a3-megagpu-8g":  GPUDirectTCPXO, // 8 GPUs 8 NICs
		"a3-ultragpu-8g": GPUDirectRDMA,  // 8 GPUs 8 NICs
		"a4-highgpu-8g":  GPUDirectRDMA,  // 8 GPUs 8 NICs
	}
	// Network Technology
	// GPUDirect-TCPX: one VPCs for GPU NICs, one subnet per VPC 8244MTU
	// GPUDirect-TCPXO: one VPCs for GPU NICs, one subnet per VPC 8244MTU
	// GPUDirect-RDMA: one HPC VPC, one subnet per NIC, 8896MTU
)

// gceNetworkInterface matches the structure expected from GCE metadata.
type gceNetworkInterface struct {
	IPv4        string   `json:"ip,omitempty"`
	IPv6        []string `json:"ipv6,omitempty"`
	Mac         string   `json:"mac,omitempty"`
	MTU         int      `json:"mtu,omitempty"`
	Network     string   `json:"network,omitempty"`
	IPAliases   []string `json:"ipAliases,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	GatewayIPv6 string   `json:"gatewayIpv6,omitempty"`
}

var _ cloudprovider.CloudInstance = (*GCEInstance)(nil)
var _ cloudprovider.ProfileProvider = (*GCEInstance)(nil)

// GCEInstance holds the GCE specific instance data.
type GCEInstance struct {
	Name                string
	Type                string
	AcceleratorProtocol string
	Interfaces          []gceNetworkInterface
	Topology            string
	// localIPAM is the node-local IP allocator used to assign addresses to subinterfaces.
	localIPAM *ipam.LocalIPAM
}

// GetDeviceAttributes fetches all attributes related to the provided device,
// identified by it's MAC.
func (g *GCEInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	attributes[AttrGCEMachineType] = resourceapi.DeviceAttribute{StringValue: &g.Type}

	if g.Topology != "" {
		topologyParts := strings.SplitN(strings.TrimPrefix(g.Topology, "/"), "/", 3)
		// topology may not be always available
		if len(topologyParts) == 3 {
			attributes[AttrGCEBlock] = resourceapi.DeviceAttribute{StringValue: &topologyParts[0]}
			attributes[AttrGCESubBlock] = resourceapi.DeviceAttribute{StringValue: &topologyParts[1]}
			attributes[AttrGCEHost] = resourceapi.DeviceAttribute{StringValue: &topologyParts[2]}
		} else {
			klog.Warningf("Error parsing host topology %q; it may be unsupported for the VM", g.Topology)
		}
	}

	// Determine properties specific to the device identified by this mac
	if id.MAC == "" {
		return attributes
	}

	interfaceForMacFound := false
	var interfaceForMac gceNetworkInterface
	for _, cloudInterface := range g.Interfaces {
		if cloudInterface.Mac == id.MAC {
			interfaceForMacFound = true
			interfaceForMac = cloudInterface
			break
		}
	}
	if interfaceForMacFound {
		if len(interfaceForMac.IPAliases) > 0 {
			ipAliases := strings.Join(interfaceForMac.IPAliases, ",")
			attributes[AttrGCEIPAliases] = resourceapi.DeviceAttribute{StringValue: &ipAliases}
		}

		var projectNumber int64
		var name string
		// Use custom parsing because the network path is
		// different from the format expected by k8s-cloud-provider
		_, err := fmt.Sscanf(interfaceForMac.Network, "projects/%d/networks/%s", &projectNumber, &name)
		if err != nil {
			klog.Warningf("Error parsing network %q : %v", interfaceForMac.Network, err)
			return nil
		}
		attributes[AttrGCENetworkName] = resourceapi.DeviceAttribute{StringValue: &name}
		attributes[AttrGCENetworkProjectNumber] = resourceapi.DeviceAttribute{IntValue: &projectNumber}
	} else {
		klog.V(4).Infof("No cloud metadata found for device with mac %q; it is possible this device has no associated cloud provider metadata", id.MAC)
	}

	return attributes
}

// ManagedProfile is the profile GCE advertises on every device it recognizes,
// so claim-scoped resolution (GetProfileConfig) always runs on GCE without the
// user having to set interface.profile. Resolution is a no-op unless the claim
// requests something GCE manages dynamically (e.g. an IPVLAN subinterface,
// which gets IPAM addresses plus its policy based routing).
const ManagedProfile = "gce-managed"

// GetDeviceConfig fetches any infrastructure-specific network configuration
// required by the device. Returning nil means no specific config is needed.
func (g *GCEInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	return &apis.NetworkConfig{Profile: ManagedProfile}
}

func (g *GCEInstance) GetProfileConfig(id cloudprovider.DeviceIdentifiers, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	if id.MAC == "" {
		return nil, nil
	}

	interfaceForMacFound := false
	var interfaceForMac gceNetworkInterface
	for _, cloudInterface := range g.Interfaces {
		if cloudInterface.Mac == id.MAC {
			interfaceForMacFound = true
			interfaceForMac = cloudInterface
			break
		}
	}
	if !interfaceForMacFound {
		klog.V(4).Infof("No cloud metadata found for device with mac %q; it is possible this device has no associated cloud provider metadata", id.MAC)
		return nil, nil
	}

	if config == nil || !config.Interface.IsSubinterface() {
		return nil, nil
	}
	if g.localIPAM == nil {
		return nil, fmt.Errorf("GCE profile IPAM is not initialized for subinterface device %q", id.MAC)
	}

	// Record static sub-interface IP addresses in IPAM as in-use.
	if len(config.Interface.Addresses) > 0 {
		if err := g.localIPAM.Reserve(config.Interface.Addresses); err != nil {
			return nil, fmt.Errorf("reserving static subinterface addresses for device %q: %w", id.MAC, err)
		}
		return sourceRoutingConfig(id.Name, interfaceForMac, config, nil), nil
	}

	// Otherwise allocate node-local IPs from the interface's ranges.
	ranges, err := g.subinterfaceRanges(id.Name, interfaceForMac)
	if err != nil {
		return nil, err
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("no subinterface IP ranges found for device %q (mac %q)", id.Name, id.MAC)
	}
	addrs, err := g.localIPAM.Allocate(ranges)
	if err != nil {
		return nil, fmt.Errorf("allocating subinterface addresses for device %q: %w", id.MAC, err)
	}
	return sourceRoutingConfig(id.Name, interfaceForMac, config, addrs), nil
}

// sourceRoutingConfig builds the profile config for a subinterface: the newly
// allocated addresses (if any) plus the policy based routing (PBR) needed for
// them to egress via the parent NIC's gateway: a per-device routing table
// holding an on-link gateway route and a default route, and one source rule per
// address. When the merged config already carries routes, rules or a VRF, the
// user/provider owns routing and nothing is synthesized.
func sourceRoutingConfig(ifName string, iface gceNetworkInterface, config *apis.NetworkConfig, allocated []string) *apis.NetworkConfig {
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

	// Gateways from the VM metadata, keyed by netip.Addr.Is6().
	gateways := map[bool]netip.Addr{}
	if gw, err := netip.ParseAddr(iface.Gateway); err == nil && gw.Is4() {
		gateways[false] = gw
	}
	if gw := getIPv6GatewayFromNeighbors(ifName); gw.IsValid() {
		gateways[true] = gw
	} else if gw, err := netip.ParseAddr(iface.GatewayIPv6); err == nil && gw.Is6() {
		gateways[true] = gw
	}

	tableID := apis.TableIDForName(iface.Mac)
	routesAdded := map[bool]bool{}
	for _, addrStr := range addrs {
		prefix, err := netip.ParsePrefix(addrStr)
		if err != nil {
			continue
		}
		is6 := prefix.Addr().Is6()
		gw, ok := gateways[is6]
		if !ok {
			continue
		}
		if !routesAdded[is6] {
			defaultDst := "0.0.0.0/0"
			if is6 {
				defaultDst = "::/0"
			}
			profile.Routes = append(profile.Routes,
				// On-link route so the gateway is reachable from a host-prefix address.
				apis.RouteConfig{Destination: netip.PrefixFrom(gw, gw.BitLen()).String(), Scope: unix.RT_SCOPE_LINK, Table: tableID},
				apis.RouteConfig{Destination: defaultDst, Gateway: gw.String(), Table: tableID})
			routesAdded[is6] = true
		}
		// Host prefix (/32 or /128) so rules for sibling subinterfaces never overlap.
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

func (g *GCEInstance) ReleaseProfileConfig(id cloudprovider.DeviceIdentifiers, claimUID types.UID, config *apis.NetworkConfig) error {
	if config == nil || !config.Interface.IsSubinterface() || g.localIPAM == nil {
		return nil
	}
	// Release the node-local IP leases that GetProfileConfig allocated.
	for _, addr := range config.Interface.Addresses {
		g.localIPAM.Release(addr)
	}
	return nil
}

// GetInstance retrieves GCE instance properties by querying the metadata server.
func GetInstance(ctx context.Context, opts ...Option) (cloudprovider.CloudInstance, error) {
	var instance *GCEInstance
	// metadata server can not be available during startup
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (done bool, err error) {
		instanceName, err := metadata.InstanceNameWithContext(ctx)
		if err != nil {
			klog.Infof("could not get instance name on GCE ... retrying: %v", err)
			return false, nil
		}

		instanceType, err := metadata.GetWithContext(ctx, "instance/machine-type")
		if err != nil {
			klog.Infof("could not get instance type on VM %s GCE ... retrying: %v", instanceName, err)
			return false, nil
		}
		// Metadata server returns instanceType in the format
		// "projects/{PROJECT_NUMBER}/machineTypes/{MACHINE_TYPE}". We only care
		// about the specific name.
		instanceType = path.Base(instanceType)

		//  curl "http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/?recursive=true" -H "Metadata-Flavor: Google"
		// [{"accessConfigs":[{"externalIp":"35.225.164.134","type":"ONE_TO_ONE_NAT"}],"dnsServers":["169.254.169.254"],"forwardedIps":[],"gateway":"10.128.0.1","ip":"10.128.0.70","ipAliases":["10.24.3.0/24"],"mac":"42:01:0a:80:00:46","mtu":1460,"network":"projects/628944397724/networks/default","subnetmask":"255.255.240.0","targetInstanceIps":[]},{"accessConfigs":[{"externalIp":"","type":"ONE_TO_ONE_NAT"}],"dnsServers":["169.254.169.254"],"forwardedIps":[],"gateway":"192.168.1.1","ip":"192.168.1.2","ipAliases":[],"mac":"42:01:c0:a8:01:02","mtu":8244,"network":"projects/628944397724/networks/aojea-dra-net-1","subnetmask":"255.255.255.0","targetInstanceIps":[]},{"accessConfigs":[{"externalIp":"","type":"ONE_TO_ONE_NAT"}],"dnsServers":["169.254.169.254"],"forwardedIps":[],"gateway":"192.168.2.1","ip":"192.168.2.2","ipAliases":[],"mac":"42:01:c0:a8:02:02","mtu":8244,"network":"projects/628944397724/networks/aojea-dra-net-2","subnetmask":"255.255.255.0","targetInstanceIps":[]},{"accessConfigs":[{"externalIp":"","type":"ONE_TO_ONE_NAT"}],"dnsServers":["169.254.169.254"],"forwardedIps":[],"gateway":"192.168.3.1","ip":"192.168.3.2","ipAliases":[],"mac":"42:01:c0:a8:03:02","mtu":8244,"network":"projects/628944397724/networks/aojea-dra-net-3","subnetmask":"255.255.255.0","targetInstanceIps":[]},{"accessConfigs":[{"externalIp":"","type":"ONE_TO_ONE_NAT"}],"dnsServers":["169.254.169.254"],"forwardedIps":[],"gateway":"192.168.4.1","ip":"192.168.4.2","ipAliases":[],"mac":"42:01:c0:a8:04:02","mtu":8244,"network":"projects/628944397724/networks/aojea-dra-net-4","subnetmask":"255.255.255.0","targetInstanceIps":[]}]
		gceInterfacesRaw, err := metadata.GetWithContext(ctx, "instance/network-interfaces/?recursive=true&alt=json")
		if err != nil {
			klog.Infof("could not get network interfaces on GCE ... retrying: %v", err)
			return false, nil
		}
		protocol := NetworkProtocolMap[instanceType]
		instance = &GCEInstance{
			Name:                instanceName,
			Type:                instanceType,
			AcceleratorProtocol: string(protocol),
			localIPAM:           ipam.NewLocalIPAM(nil),
		}
		for _, opt := range opts {
			opt(instance)
		}
		if err = json.Unmarshal([]byte(gceInterfacesRaw), &instance.Interfaces); err != nil {
			klog.Infof("could not get network interfaces on GCE ... retrying: %v", err)
			return false, nil
		}
		// Physical location of VM is not always available. We don't fail if
		// it's not available.
		//
		// Ref. https://cloud.google.com/compute/docs/instances/use-compact-placement-policies#verify-vm-location
		gceTopologyAttributes, err := metadata.GetWithContext(ctx, "instance/attributes/physical_host")
		if err != nil {
			klog.Warningf("Failed to retrieve physical host for GCE VM %q, this maybe normal since not all VMs and VM types have this populated: %v", instanceName, err)
		} else {
			instance.Topology = gceTopologyAttributes
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// Option configures a GCEInstance at construction time; see GetInstance.
type Option func(*GCEInstance)

// WithReservedAddresses seeds the instance's node-local IPAM with addresses
// already in use on the node, so it doesn't hand out ones that are taken.
func WithReservedAddresses(addrs []string) Option {
	return func(g *GCEInstance) {
		g.localIPAM = ipam.NewLocalIPAM(addrs)
	}
}

// subinterfaceRanges derives the node-local IP allocation ranges for the given GCE network interface.
func (g *GCEInstance) subinterfaceRanges(ifName string, iface gceNetworkInterface) ([]ipam.IPRange, error) {
	var ranges []ipam.IPRange
	// IPv6: derive the subinterface range from the host interface's global IPv6 prefix.
	prefix, err := getNICIPv6Prefix(ifName)
	if err != nil {
		return nil, fmt.Errorf("determining IPv6 prefix for device %q: %w", ifName, err)
	}
	if prefix != nil {
		cidr, err := getIPv6Range(prefix.String())
		if err != nil {
			return nil, fmt.Errorf("calculating IPv6 range for base IP %q: %w", prefix.String(), err)
		}
		start, end, err := cloudprovider.IPRangeFromCIDR(cidr, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("deriving IPv6 allocation range from %q: %w", cidr, err)
		}
		ranges = append(ranges, ipam.IPRange{Start: start, End: end})
	}
	// IPv4: from the first alias range.
	if len(iface.IPAliases) > 0 {
		alias := iface.IPAliases[0]
		start, end, err := cloudprovider.IPRangeFromCIDR(alias, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("deriving IPv4 allocation range from alias %q: %w", alias, err)
		}
		ranges = append(ranges, ipam.IPRange{Start: start, End: end})
	}
	return ranges, nil
}

// getNICIPv6Prefix queries the host interface ifName via netlink and returns
// the global IPv6 prefix of ifName.
func getNICIPv6Prefix(ifName string) (*net.IPNet, error) {
	if ifName == "" {
		return nil, nil
	}
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
		if ones, bits := addr.Mask.Size(); bits != 128 || ones/8 >= 14 {
			continue
		}
		return &net.IPNet{IP: addr.IP.Mask(addr.Mask), Mask: addr.Mask}, nil
	}
	return nil, nil
}

// getIPv6GatewayFromNeighbors inspects ifName for a permanent, link-local IPv6
// neighbor entry (e.g. "fe80::1 lladdr 02:32:00:00:00:00 PERMANENT" on A5X
// dcnN_lbvf) and returns its IP as the IPv6 gateway.
func getIPv6GatewayFromNeighbors(ifName string) netip.Addr {
	if ifName == "" {
		return netip.Addr{}
	}
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return netip.Addr{}
	}
	neighs, err := nlwrap.NeighList(link.Attrs().Index, netlink.FAMILY_V6)
	if err != nil {
		return netip.Addr{}
	}
	for _, neigh := range neighs {
		if neigh.State&netlink.NUD_PERMANENT == 0 || neigh.IP == nil || len(neigh.HardwareAddr) == 0 {
			continue
		}
		if !neigh.IP.IsLinkLocalUnicast() {
			continue
		}
		gw, err := netip.ParseAddr(neigh.IP.String())
		if err != nil || !gw.Is6() {
			continue
		}
		return gw
	}
	return netip.Addr{}
}

// getIPv6Range calculates the subinterface IPv6 range by appending 0xC0DE marker to the base range.
func getIPv6Range(baseIPStr string) (string, error) {
	const workerMarker = 0xC0DE

	_, ipnet, err := net.ParseCIDR(baseIPStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse CIDR %q: %v", baseIPStr, err)
	}
	prefixLen, _ := ipnet.Mask.Size()
	baseIP := ipnet.IP

	if baseIP.To4() != nil {
		return "", fmt.Errorf("IP %q is an IPv4 address, expected IPv6", baseIPStr)
	}
	ip16 := baseIP.To16()
	if ip16 == nil {
		return "", fmt.Errorf("IP %q is not a valid IPv6 address", baseIPStr)
	}

	workerIP := make(net.IP, 16)
	numBaseBytes := prefixLen / 8

	if numBaseBytes >= 14 {
		return "", fmt.Errorf("prefix length %d is too large to append %x", prefixLen, workerMarker)
	}

	copy(workerIP[0:numBaseBytes], ip16[0:numBaseBytes])
	workerIP[numBaseBytes] = byte(workerMarker >> 8)
	workerIP[numBaseBytes+1] = byte(workerMarker & 0xFF)
	newPrefixLen := (numBaseBytes + 2) * 8

	return workerIP.String() + "/" + strconv.Itoa(newPrefixLen), nil
}
