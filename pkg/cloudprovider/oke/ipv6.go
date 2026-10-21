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

package oke

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

// On an IPv6 RDMA fabric the rail routers advertise one /64 for each RDMA NIC
// of each host, and the host autoconfigures its RDMA NIC from it. The OKE
// profile gives the pod an IPvlan child with a static address in that /64 and
// the default route of its parent, both read from the host. The child ignores
// router advertisements, so nothing autoconfigures in the pod, and several
// RDMA NICs in one pod each keep their own default route.

const (
	// rdmaVFRailPrefix names the SR-IOV VF of each rail on a multi-planar
	// fabric, rdma_vf_rail<N>, the endpoint a pod claims there. Its PF function
	// also carries the plane netdevs, rdma_p<P>_rail<N>, and the VF's
	// representor or the PF itself under the rdma<N> name.
	rdmaVFRailPrefix = "rdma_vf_rail"
	// okeMaxIPv6RDMANics bounds the RDMA NIC index on an IPv6 fabric, as
	// okeMaxIPv4RDMANics does on an IPv4 one: rdma<N> uses the tables 100 to
	// 115 of the IPv4 profile, rdma_vf_rail<N> the next 16, 116 to 131, so a
	// pod with both kinds gets one table per RDMA NIC.
	okeMaxIPv6RDMANics   = 16
	okeRailVFTableBase   = okeRDMASBRTableBase + okeMaxIPv6RDMANics
	unusableIPv6AddrFlag = unix.IFA_F_DEPRECATED | unix.IFA_F_TENTATIVE | unix.IFA_F_DADFAILED
	// rdmaIPv6PrefixBits is the length of the prefix the rail routers advertise
	// for autoconfiguration.
	rdmaIPv6PrefixBits = 64
)

// ipv6Router is one IPv6 default route of a host interface.
type ipv6Router struct {
	Gateway netip.Addr
	Metric  int
}

// ipv6HostAddr is one IPv6 address of a host interface with its IFA_F flags.
type ipv6HostAddr struct {
	Prefix netip.Prefix
	Flags  int
}

// hostIPv6Addresses returns the IPv6 addresses of ifName with their flags,
// which net.Interface.Addrs does not report. Tests replace it.
var hostIPv6Addresses = func(ifName string) ([]ipv6HostAddr, error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("could not find interface %s: %w", ifName, err)
	}
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, fmt.Errorf("could not list the IPv6 addresses of %s: %w", ifName, err)
	}
	var result []ipv6HostAddr
	for _, addr := range addrs {
		if addr.IPNet == nil {
			continue
		}
		ip, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			continue
		}
		ones, _ := addr.Mask.Size()
		result = append(result, ipv6HostAddr{Prefix: netip.PrefixFrom(ip.Unmap(), ones), Flags: addr.Flags})
	}
	return result, nil
}

// hostIPv6DefaultRouters returns the IPv6 default routes through ifName in
// every table. Router advertisements put them in the table of the interface's
// VRF when it has one. Tests replace it.
var hostIPv6DefaultRouters = func(ifName string) ([]ipv6Router, error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("could not find interface %s: %w", ifName, err)
	}
	// No RT_FILTER_OIF: a multipath route has no interface of its own, only
	// its nexthops do, and the filter would drop it.
	filter := &netlink.Route{Table: unix.RT_TABLE_UNSPEC}
	routes, err := nlwrap.RouteListFiltered(netlink.FAMILY_V6, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("could not list the IPv6 routes of %s: %w", ifName, err)
	}
	routers, unreadable := ipv6DefaultRouters(routes, link.Attrs().Index)
	if len(routers) == 0 && unreadable > 0 {
		return nil, fmt.Errorf("interface %s has no IPv6 default route the OKE profile can read; %d default route(s) on the host have neither a gateway nor an interface, as a route through a nexthop object has, which the profile does not resolve", ifName, unreadable)
	}
	return routers, nil
}

// ipv6DefaultRouters returns the gateways of the unicast default routes that
// leave through linkIndex, directly or as one nexthop of a multipath route, and
// how many unicast default routes have neither a gateway, an interface nor
// nexthops. A route through a nexthop object (RTA_NH_ID, as FRR installs) looks
// like that here, because netlink does not report the object, so it cannot be
// attributed to any interface.
func ipv6DefaultRouters(routes []netlink.Route, linkIndex int) ([]ipv6Router, int) {
	var routers []ipv6Router
	unreadable := 0
	add := func(gw net.IP, metric int) {
		if gateway, ok := netip.AddrFromSlice(gw); ok && gw != nil {
			routers = append(routers, ipv6Router{Gateway: gateway.Unmap(), Metric: metric})
		}
	}
	for _, route := range routes {
		if route.Type != unix.RTN_UNICAST {
			continue
		}
		if route.Dst != nil {
			if ones, _ := route.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		if len(route.MultiPath) == 0 {
			switch {
			case route.LinkIndex == linkIndex:
				add(route.Gw, route.Priority)
			case route.LinkIndex == 0 && route.Gw == nil:
				unreadable++
			}
			continue
		}
		for _, nexthop := range route.MultiPath {
			if nexthop != nil && nexthop.LinkIndex == linkIndex {
				add(nexthop.Gw, route.Priority)
			}
		}
	}
	return routers, unreadable
}

// railVFIndex returns the rail index encoded in an rdma_vf_rail<N> name.
func railVFIndex(ifName string) (int, bool) {
	digits, ok := strings.CutPrefix(ifName, rdmaVFRailPrefix)
	if !ok {
		return 0, false
	}
	index, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int(index), true
}

// isEthernetRailVF reports whether ifName is an Ethernet rdma_vf_rail<N> VF.
// It does not depend on the fabric metadata: GetDeviceConfig runs when the
// inventory is scanned, possibly before IMDS answers, and a VF without the
// profile would move into the pod.
func isEthernetRailVF(ifName string) bool {
	if _, ok := railVFIndex(ifName); !ok {
		return false
	}
	hardwareType, err := interfaceHardwareType(ifName)
	return err == nil && hardwareType == unix.ARPHRD_ETHER
}

// ipv6ProfileConfig resolves the OKE profile on an IPv6 RDMA fabric for the
// Ethernet parent ifName. It returns an error, which Kubernetes retries, until
// the parent has its autoconfigured address and default route.
//
// The address and the router are read once, when the claim is prepared, and
// the child ignores router advertisements: after the fabric renumbers the
// prefix or the rail router changes, the child keeps the old ones until the pod
// is recreated.
//
// The pod namespace must have IPv6 enabled: a CNI that disables it there, as
// the OCI VCN-Native CNI does on an IPv4-only cluster, makes the address fail.
func (o *OKEInstance) ipv6ProfileConfig(ifName string, claim *resourceapi.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	index, tableBase := 0, okeRDMASBRTableBase
	if i, ok := rdmaNicIndex(ifName); ok {
		index = i
	} else if i, ok := railVFIndex(ifName); ok {
		index, tableBase = i, okeRailVFTableBase
	} else {
		return nil, fmt.Errorf("the OKE profile on an IPv6 fabric needs an rdma<N> or %s<N> parent, not %s", rdmaVFRailPrefix, ifName)
	}
	// An index above the largest shape would leave the profile's tables.
	if index >= okeMaxIPv6RDMANics {
		return nil, fmt.Errorf("RDMA NIC index %d of %s is above the largest supported index %d", index, ifName, okeMaxIPv6RDMANics-1)
	}
	if claim == nil || claim.UID == "" {
		return nil, fmt.Errorf("the OKE profile on an IPv6 fabric needs the claim UID to derive the address of the %s child", ifName)
	}
	// A claim value would win over the profile's and autoconfigure a second
	// address, GID and default route on the child.
	if ra := config.Interface.AcceptRA; ra != nil && *ra != 0 {
		return nil, fmt.Errorf("the OKE profile on an IPv6 fabric configures the %s child itself and needs acceptRA 0; remove interface.acceptRA", ifName)
	}
	// Joining a VRF cycles the interface, and the kernel removes its IPv6
	// addresses on the way down unless keep_addr_on_down is set, which the
	// driver does not do yet (kubernetes-sigs/dranet#390); a VRF device created
	// in a namespace with IPv6 disabled also keeps it off. Drop this check once
	// the driver keeps the addresses. The profile's source routing gives each
	// rail its own table without a VRF.
	if config.Interface.VRF != nil {
		return nil, fmt.Errorf("the OKE profile on an IPv6 fabric routes the %s child with a table of its own and does not support interface.vrf, whose IPv6 addresses would not survive joining it", ifName)
	}

	prefix, parentAddrs, err := parentIPv6Prefix(ifName)
	if err != nil {
		return nil, err
	}
	router, err := parentIPv6Router(ifName)
	if err != nil {
		return nil, err
	}
	child := childIPv6Address(prefix, claim.UID, ifName, parentAddrs)

	result := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			Type:      apis.InterfaceTypeIPVLAN,
			Addresses: []string{netip.PrefixFrom(child, prefix.Bits()).String()},
			// A pod namespace the CNI leaves at the kernel defaults accepts the
			// rail router's advertisements, and the child would autoconfigure a
			// second address, GID and default route next to these.
			AcceptRA: ptr.To[int32](0),
		},
	}
	// Routing is user-owned when the claim carries routes or rules.
	if len(config.Routes) > 0 || len(config.Rules) > 0 {
		return result, nil
	}
	// Source routing, as on an IPv4 fabric: every RDMA NIC's default route is
	// ::/0, so one table per RDMA NIC keeps them apart, and the rule sends the
	// child's traffic, including the RoCE address resolution of its GID, to the
	// table of its own rail.
	table := tableBase + index
	result.Routes = []apis.RouteConfig{
		// The kernel ignores the scope of an IPv6 route, but the driver's
		// validation needs a gateway on a universe route and applies link
		// routes first.
		{Destination: prefix.String(), Scope: unix.RT_SCOPE_LINK, Table: table},
		{Destination: "::/0", Gateway: router.String(), Table: table},
	}
	result.Rules = []apis.RuleConfig{{
		Priority: apis.SourceRoutingRulePriority,
		Source:   netip.PrefixFrom(child, child.BitLen()).String(),
		Table:    table,
	}}
	return result, nil
}

// parentIPv6Prefix returns the autoconfigured /64 of ifName and the parent's
// global addresses, all of which the child's address avoids. Only a usable
// address names the prefix: a deprecated one, as the old prefix keeps for its
// valid lifetime after the fabric renumbers, a tentative one, or one that
// failed duplicate address detection does not. More than one usable prefix is
// an error: the profile cannot tell which one the fabric routes for this host.
func parentIPv6Prefix(ifName string) (netip.Prefix, []netip.Addr, error) {
	addresses, err := hostIPv6Addresses(ifName)
	if err != nil {
		return netip.Prefix{}, nil, fmt.Errorf("could not read the IPv6 addresses of %s: %w", ifName, err)
	}
	var found []netip.Prefix
	var avoid []netip.Addr
	for _, address := range addresses {
		addr := address.Prefix.Addr()
		// hostIPv6Addresses unmaps, so an IPv4-mapped address is IPv4 here.
		if !addr.Is6() || !addr.IsGlobalUnicast() {
			continue
		}
		avoid = append(avoid, addr)
		if address.Prefix.Bits() != rdmaIPv6PrefixBits || address.Flags&unusableIPv6AddrFlag != 0 {
			continue
		}
		if masked := address.Prefix.Masked(); !slices.Contains(found, masked) {
			found = append(found, masked)
		}
	}
	switch len(found) {
	case 0:
		return netip.Prefix{}, nil, fmt.Errorf("interface %s has no usable global IPv6 address in a /%d yet; the rail router's advertisement assigns it", ifName, rdmaIPv6PrefixBits)
	case 1:
		return found[0], avoid, nil
	default:
		return netip.Prefix{}, nil, fmt.Errorf("interface %s has usable global IPv6 addresses in %d prefixes %v; the OKE profile needs exactly one", ifName, len(found), found)
	}
}

// parentIPv6Router returns the router of the IPv6 default route through ifName,
// the lowest metric first when there are several, then the lowest address: the
// route's router preference (RFC 4191) is not read.
func parentIPv6Router(ifName string) (netip.Addr, error) {
	routers, err := hostIPv6DefaultRouters(ifName)
	if err != nil {
		return netip.Addr{}, err
	}
	routers = slices.DeleteFunc(routers, func(r ipv6Router) bool { return !r.Gateway.Is6() })
	if len(routers) == 0 {
		return netip.Addr{}, fmt.Errorf("interface %s has no IPv6 default route yet; the rail router's advertisement installs it", ifName)
	}
	slices.SortFunc(routers, func(a, b ipv6Router) int {
		return cmp.Or(cmp.Compare(a.Metric, b.Metric), a.Gateway.Compare(b.Gateway))
	})
	if len(routers) > 1 {
		klog.V(2).Infof("Interface %s has %d IPv6 default routes; the OKE profile uses %s", ifName, len(routers), routers[0].Gateway)
	}
	return routers[0].Gateway, nil
}

// childIPv6Address derives the child's address in prefix from the claim UID and
// the parent name, so a repeated prepare of a claim gives the same address and
// the next claim on the parent a new one, which does not clash with a child of
// the previous claim that has not been torn down yet. The interface identifier
// is a SHA-256 hash; one that is reserved (RFC 5453) or taken by the parent is
// hashed again with a counter.
func childIPv6Address(prefix netip.Prefix, claimUID types.UID, ifName string, avoid []netip.Addr) netip.Addr {
	network := prefix.Masked().Addr().As16()
	for counter := uint64(0); ; counter++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", claimUID, ifName, counter))
		iid := binary.BigEndian.Uint64(sum[:8])
		if reservedInterfaceID(iid) {
			continue
		}
		bytes := network
		binary.BigEndian.PutUint64(bytes[8:], iid)
		addr := netip.AddrFrom16(bytes)
		if slices.Contains(avoid, addr) {
			continue
		}
		return addr
	}
}

// reservedInterfaceID reports the interface identifiers RFC 5453 reserves: the
// subnet-router anycast identifier, the IANA Ethernet block
// 0200:5EFF:FE00:0000 to 0200:5EFF:FEFF:FFFF, and the subnet anycast
// identifiers FDFF:FFFF:FFFF:FF80 to FDFF:FFFF:FFFF:FFFF.
func reservedInterfaceID(iid uint64) bool {
	return iid == 0 ||
		iid>>24 == 0x02005efffe ||
		iid&^0x7f == 0xfdffffffffffff80
}
