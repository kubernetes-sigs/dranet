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

package driver

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func parseIPVlanKernelMode(mode string) (netlink.IPVlanMode, error) {
	switch mode {
	case "", "l2":
		return netlink.IPVLAN_MODE_L2, nil
	case "l3":
		return netlink.IPVLAN_MODE_L3, nil
	case "l3s":
		return netlink.IPVLAN_MODE_L3S, nil
	default:
		return 0, fmt.Errorf("unsupported IPvlan mode %q", mode)
	}
}

func parseIPVlanKernelFlag(flag string) (netlink.IPVlanFlag, error) {
	switch flag {
	case "", "bridge":
		return netlink.IPVLAN_FLAG_BRIDGE, nil
	case "private":
		return netlink.IPVLAN_FLAG_PRIVATE, nil
	case "vepa":
		return netlink.IPVLAN_FLAG_VEPA, nil
	default:
		return 0, fmt.Errorf("unsupported IPvlan flag %q", flag)
	}
}

// ipvlanTempName returns a readable host-namespace name for an IPvlan slave
// during its transient existence before being moved to the pod netns.
// Concurrency is handled by ipvlanMu, so a simple suffix is safe.
// The result is capped at 15 characters (IFNAMSIZ-1).
func ipvlanTempName(parentName string) string {
	const suffix = "_iv"
	const maxLen = 15
	if len(parentName)+len(suffix) <= maxLen {
		return parentName + suffix
	}
	return parentName[:maxLen-len(suffix)] + suffix
}

// discoverRDMADevices finds RDMA device names associated with the parent
// interface (direct or via bond slaves).
func discoverRDMADevices(nlHandle nlwrap.Handle, parentIfName string, parentIndex int) []string {
	var rdmaDevices []string
	seen := map[string]bool{}

	rdmaDir := filepath.Join("/sys/class/net", parentIfName, "device/infiniband")
	if entries, err := os.ReadDir(rdmaDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !seen[e.Name()] {
				seen[e.Name()] = true
				rdmaDevices = append(rdmaDevices, e.Name())
			}
		}
	}

	links, err := nlHandle.LinkList()
	if err != nil {
		return rdmaDevices
	}
	for _, l := range links {
		if l.Attrs().MasterIndex != parentIndex {
			continue
		}
		rdmaDir := filepath.Join("/sys/class/net", l.Attrs().Name, "device/infiniband")
		entries, err := os.ReadDir(rdmaDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && !seen[e.Name()] {
				seen[e.Name()] = true
				rdmaDevices = append(rdmaDevices, e.Name())
			}
		}
	}
	return rdmaDevices
}

// computeIPVlanSlave builds the IPvlan configuration for a single parent
// interface based on the user/cloud IPvlan config.
// Called during PrepareResourceClaim.
func computeIPVlanSlave(nlHandle nlwrap.Handle, parentIfName string, ipvlanCfg *apis.IPVlanConfig) (*IPVlanSlaveConfig, error) {
	link, err := nlHandle.LinkByName(parentIfName)
	if err != nil {
		return nil, fmt.Errorf("interface %s not found: %w", parentIfName, err)
	}

	kernelMode, err := parseIPVlanKernelMode(ipvlanCfg.Mode)
	if err != nil {
		return nil, err
	}
	kernelFlag, err := parseIPVlanKernelFlag(ipvlanCfg.Flag)
	if err != nil {
		return nil, err
	}

	addressingType := apis.IPVlanAddrParentIPv6PrefixPodIPv4
	if ipvlanCfg.Addressing != nil && ipvlanCfg.Addressing.Type != "" {
		addressingType = ipvlanCfg.Addressing.Type
	}

	slave := &IPVlanSlaveConfig{
		ParentNetdev:   parentIfName,
		KernelMode:     uint16(kernelMode),
		KernelFlag:     uint16(kernelFlag),
		AddressingType: addressingType,
		TargetName:     parentIfName,
		TempName:       ipvlanTempName(parentIfName),
	}

	if addressingType == apis.IPVlanAddrParentIPv6PrefixPodIPv4 {
		addrs, err := nlHandle.AddrList(link, unix.AF_INET6)
		if err != nil {
			return nil, fmt.Errorf("failed to list IPv6 addresses for %s: %w", parentIfName, err)
		}

		var srcAddr *net.IPNet
		for _, addr := range addrs {
			if addr.IP.IsLinkLocalUnicast() {
				continue
			}
			srcAddr = addr.IPNet
			break
		}
		if srcAddr == nil {
			return nil, fmt.Errorf("no global IPv6 address found for %s", parentIfName)
		}

		ip := srcAddr.IP.To16()
		prefixLen, _ := srcAddr.Mask.Size()
		if prefixLen > 96 {
			return nil, fmt.Errorf("IPv6 prefix length %d too large for %s, no room for pod identity", prefixLen, parentIfName)
		}

		var prefix [12]byte
		copy(prefix[:], ip[:12])
		slave.IPv6Prefix = prefix
		slave.PrefixLen = prefixLen
	}

	copyRoutes := ipvlanCfg.CopyRoutesFromParent != nil && *ipvlanCfg.CopyRoutesFromParent
	copyNeighbors := ipvlanCfg.CopyNeighborsFromParent != nil && *ipvlanCfg.CopyNeighborsFromParent

	if copyRoutes || copyNeighbors {
		routes, err := nlHandle.RouteList(link, unix.AF_INET6)
		if err != nil {
			return nil, fmt.Errorf("failed to list IPv6 routes for %s: %w", parentIfName, err)
		}

		var srcIP net.IP
		if addressingType == apis.IPVlanAddrParentIPv6PrefixPodIPv4 {
			srcIP = make(net.IP, 16)
			copy(srcIP[:12], slave.IPv6Prefix[:])
		}

		for _, r := range routes {
			if srcIP != nil && r.Dst != nil && r.Dst.Contains(srcIP) && r.Gw == nil {
				continue
			}
			if copyRoutes {
				rc := apis.RouteConfig{}
				if r.Dst != nil {
					rc.Destination = r.Dst.String()
				}
				if r.Gw != nil {
					rc.Gateway = r.Gw.String()
				}
				slave.Routes = append(slave.Routes, rc)
			}
			if copyNeighbors && r.Gw != nil {
				mac, err := resolveNeighborMAC(nlHandle, link.Attrs().Index, r.Gw)
				if err != nil {
					return nil, fmt.Errorf("failed to resolve neighbor for gateway %s on %s: %w", r.Gw, parentIfName, err)
				}
				if mac != "" {
					slave.GatewayNeighbors = append(slave.GatewayNeighbors, apis.NeighborConfig{
						Destination:  r.Gw.String(),
						HardwareAddr: mac,
					})
				} else {
					klog.Warningf("IPvlan: copyNeighborsFromParent: gateway %s on %s has no resolved MAC, pod will rely on in-netns NDP", r.Gw, parentIfName)
				}
			}
		}
	}

	return slave, nil
}

func resolveNeighborMAC(nlHandle nlwrap.Handle, ifIndex int, ip net.IP) (string, error) {
	neighs, err := nlHandle.NeighList(ifIndex, unix.AF_INET6)
	if err != nil {
		return "", fmt.Errorf("failed to list neighbors for ifindex %d: %w", ifIndex, err)
	}
	for _, n := range neighs {
		if n.IP.Equal(ip) && n.HardwareAddr != nil {
			return n.HardwareAddr.String(), nil
		}
	}
	return "", nil
}

// ipvlanMu serializes ipvlan slave creation/move across concurrent
// RunPodSandbox calls to avoid TOCTOU races on the temporary host-namespace
// interface name.
var ipvlanMu sync.Mutex

func attachIPVlanSlaves(pod *api.PodSandbox, nsPath string, slaves []IPVlanSlaveConfig) (*resourceapi.NetworkDeviceData, error) {
	ipvlanMu.Lock()
	defer ipvlanMu.Unlock()

	var podIPv4 net.IP
	for _, slave := range slaves {
		addrType := slave.AddressingType
		if addrType == "" {
			addrType = apis.IPVlanAddrParentIPv6PrefixPodIPv4
		}
		if addrType == apis.IPVlanAddrParentIPv6PrefixPodIPv4 {
			podIPv4 = extractPodIPv4(pod)
			if podIPv4 == nil {
				var err error
				podIPv4, err = readPodIPv4FromNetns(nsPath)
				if err != nil || podIPv4 == nil {
					return nil, fmt.Errorf("cannot determine pod IPv4 for IPvlan IPv6 derivation")
				}
			}
			break
		}
	}

	containerNs, err := netns.GetFromPath(nsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open pod netns %s: %w", nsPath, err)
	}
	defer containerNs.Close()

	var networkData *resourceapi.NetworkDeviceData
	for _, slave := range slaves {
		if err := createIPVlanSlave(slave, podIPv4, containerNs, nsPath); err != nil {
			return nil, fmt.Errorf("failed to create IPvlan slave for %s: %w", slave.ParentNetdev, err)
		}
		// Collect network data from the first slave for status reporting.
		if networkData == nil {
			networkData = collectIPVlanNetworkData(containerNs, slave)
		}
	}
	return networkData, nil
}

func collectIPVlanNetworkData(containerNs netns.NsHandle, slave IPVlanSlaveConfig) *resourceapi.NetworkDeviceData {
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return nil
	}
	defer nhNs.Close()

	link, err := nhNs.LinkByName(slave.TargetName)
	if err != nil {
		return nil
	}

	nd := &resourceapi.NetworkDeviceData{
		InterfaceName:   link.Attrs().Name,
		HardwareAddress: link.Attrs().HardwareAddr.String(),
	}

	addrs, err := nhNs.AddrList(link, unix.AF_UNSPEC)
	if err != nil {
		return nd
	}
	for _, addr := range addrs {
		if addr.IP.IsLinkLocalUnicast() {
			continue
		}
		nd.IPs = append(nd.IPs, addr.IPNet.String())
	}
	return nd
}

func createIPVlanSlave(slave IPVlanSlaveConfig, podIPv4 net.IP, containerNs netns.NsHandle, nsPath string) error {
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return fmt.Errorf("failed to create netlink handle in pod netns: %w", err)
	}
	defer nhNs.Close()

	// Idempotency: if the target already exists from a prior attempt, reuse it.
	// Verify it's actually an IPvlan to avoid confusing a CNI-created interface
	// (e.g., eth0) with our slave.
	if existingLink, err := nhNs.LinkByName(slave.TargetName); err == nil {
		if _, ok := existingLink.(*netlink.IPVlan); !ok {
			return fmt.Errorf("interface %s already exists in pod netns but is not an IPvlan (type: %s), name collision with CNI or other driver",
				slave.TargetName, existingLink.Type())
		}
		klog.V(4).Infof("IPvlan: target %s already exists in pod netns, reusing", slave.TargetName)
		return configureIPVlanSlave(nhNs, existingLink, slave, podIPv4, nsPath)
	}

	parentLink, err := nlwrap.LinkByName(slave.ParentNetdev)
	if err != nil {
		return fmt.Errorf("parent interface %s not found: %w", slave.ParentNetdev, err)
	}

	// Clean up orphan from a prior attempt that created the slave but
	// failed before moving it to the pod netns.
	if prevLink, err := nlwrap.LinkByName(slave.TempName); err == nil {
		klog.V(4).Infof("IPvlan: removing orphan temp slave %s", slave.TempName)
		if err := netlink.LinkDel(prevLink); err != nil {
			return fmt.Errorf("failed to remove orphan IPvlan slave %s: %w", slave.TempName, err)
		}
	}

	kernelMode := netlink.IPVlanMode(slave.KernelMode)
	kernelFlag := netlink.IPVlanFlag(slave.KernelFlag)
	if kernelMode == 0 {
		kernelMode = netlink.IPVLAN_MODE_L2
	}
	if kernelFlag == 0 {
		kernelFlag = netlink.IPVLAN_FLAG_BRIDGE
	}

	ipvlan := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        slave.TempName,
			ParentIndex: parentLink.Attrs().Index,
		},
		Mode: kernelMode,
		Flag: kernelFlag,
	}
	if err := netlink.LinkAdd(ipvlan); err != nil {
		return fmt.Errorf("failed to create IPvlan: %w", err)
	}

	if err := netlink.LinkSetNsFd(ipvlan, int(containerNs)); err != nil {
		if delErr := netlink.LinkDel(ipvlan); delErr != nil {
			klog.Warningf("failed to cleanup IPvlan %s after NsFd error: %v", slave.TempName, delErr)
		}
		return fmt.Errorf("failed to move IPvlan to pod netns: %w", err)
	}

	podLink, err := nhNs.LinkByName(slave.TempName)
	if err != nil {
		return fmt.Errorf("IPvlan slave %s not found in pod netns: %w", slave.TempName, err)
	}

	if err := nhNs.LinkSetName(podLink, slave.TargetName); err != nil {
		_ = nhNs.LinkDel(podLink)
		return fmt.Errorf("failed to rename %s to %s: %w", slave.TempName, slave.TargetName, err)
	}

	podLink, err = nhNs.LinkByName(slave.TargetName)
	if err != nil {
		return fmt.Errorf("failed to get renamed link %s: %w", slave.TargetName, err)
	}

	return configureIPVlanSlave(nhNs, podLink, slave, podIPv4, nsPath)
}

func configureIPVlanSlave(nhNs nlwrap.Handle, podLink netlink.Link, slave IPVlanSlaveConfig, podIPv4 net.IP, nsPath string) error {

	addrType := slave.AddressingType
	if addrType == "" {
		addrType = apis.IPVlanAddrParentIPv6PrefixPodIPv4
	}

	switch addrType {
	case apis.IPVlanAddrNone:
		// No addresses; just bring up.
	case apis.IPVlanAddrStatic:
		setAddrGenMode(nsPath, slave.TargetName)
		for _, addrStr := range slave.StaticAddresses {
			ip, ipnet, err := net.ParseCIDR(addrStr)
			if err != nil {
				return fmt.Errorf("invalid static address %s: %w", addrStr, err)
			}
			if err := nhNs.AddrAdd(podLink, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}}); err != nil && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("failed to add static address %s: %w", addrStr, err)
			}
		}
	case apis.IPVlanAddrParentIPv6PrefixPodIPv4:
		setAddrGenMode(nsPath, slave.TargetName)
		derivedIP, derivedNet, err := deriveIPv6ForPod(slave.IPv6Prefix, slave.PrefixLen, podIPv4)
		if err != nil {
			return fmt.Errorf("failed to derive IPv6: %w", err)
		}
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: derivedIP, Mask: derivedNet.Mask}}
		if err := nhNs.AddrAdd(podLink, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add IPv6 address %s: %w", derivedIP, err)
		}
	}

	if err := nhNs.LinkSetUp(podLink); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", slave.TargetName, err)
	}

	// Routes from parent (populated by copyRoutesFromParent) — apply for all addressing types.
	if len(slave.Routes) > 0 {
		routeMetric := 1024 + podLink.Attrs().Index
		for _, rc := range slave.Routes {
			route := netlink.Route{
				LinkIndex: podLink.Attrs().Index,
				Priority:  routeMetric,
			}
			if rc.Destination != "" {
				_, dst, err := net.ParseCIDR(rc.Destination)
				if err != nil {
					return fmt.Errorf("invalid route destination %s: %w", rc.Destination, err)
				}
				route.Dst = dst
			}
			if rc.Gateway != "" {
				route.Gw = net.ParseIP(rc.Gateway)
				if err := addGatewayConnectedRoute(nhNs, podLink.Attrs().Index, route.Gw); err != nil {
					return fmt.Errorf("failed to add gateway connected route for %s: %w", rc.Gateway, err)
				}
			}
			if err := nhNs.RouteReplace(&route); err != nil {
				return fmt.Errorf("failed to add route %v: %w", rc, err)
			}
		}
	}

	// Gateway neighbors from parent (populated by copyNeighborsFromParent).
	if len(slave.GatewayNeighbors) > 0 {
		for _, nc := range slave.GatewayNeighbors {
			gwIP := net.ParseIP(nc.Destination)
			if gwIP == nil {
				return fmt.Errorf("invalid gateway neighbor IP %q", nc.Destination)
			}
			family := unix.AF_INET6
			if gwIP.To4() != nil {
				family = unix.AF_INET
			}
			var hwAddr net.HardwareAddr
			if nc.HardwareAddr != "" {
				var err error
				hwAddr, err = net.ParseMAC(nc.HardwareAddr)
				if err != nil {
					return fmt.Errorf("invalid neighbor MAC %s for %s: %w", nc.HardwareAddr, nc.Destination, err)
				}
			}
			if hwAddr == nil {
				hwAddr = resolveNeighborByND(nhNs, podLink.Attrs().Index, gwIP)
			}
			if hwAddr == nil {
				return fmt.Errorf("failed to resolve MAC for gateway %s on %s", nc.Destination, slave.TargetName)
			}
			neigh := &netlink.Neigh{
				LinkIndex:    podLink.Attrs().Index,
				IP:           gwIP,
				HardwareAddr: hwAddr,
				State:        netlink.NUD_PERMANENT,
				Family:       family,
			}
			if err := nhNs.NeighSet(neigh); err != nil {
				return fmt.Errorf("failed to set permanent neighbor %s: %w", nc.Destination, err)
			}
		}
	}

	// User-configured routes and neighbors — apply for all addressing types.
	// Sort link-scope routes before universe-scope so gateway reachability is
	// established before routes that depend on it (same logic as applyRoutingConfig).
	slices.SortFunc(slave.ConfiguredRoutes, func(a, b apis.RouteConfig) int {
		return int(b.Scope) - int(a.Scope)
	})
	for _, rc := range slave.ConfiguredRoutes {
		route := netlink.Route{
			LinkIndex: podLink.Attrs().Index,
			Scope:     netlink.Scope(rc.Scope),
			Table:     rc.Table,
		}
		if rc.Destination != "" {
			_, dst, err := net.ParseCIDR(rc.Destination)
			if err != nil {
				return fmt.Errorf("invalid configured route destination %s: %w", rc.Destination, err)
			}
			route.Dst = dst
		}
		if rc.Gateway != "" {
			route.Gw = net.ParseIP(rc.Gateway)
			if err := addGatewayConnectedRoute(nhNs, podLink.Attrs().Index, route.Gw); err != nil {
				return fmt.Errorf("failed to add gateway connected route for %s: %w", rc.Gateway, err)
			}
		}
		if rc.Source != "" {
			route.Src = net.ParseIP(rc.Source)
		}
		if err := nhNs.RouteReplace(&route); err != nil {
			return fmt.Errorf("failed to add configured route %v: %w", rc, err)
		}
	}
	for _, nc := range slave.ConfiguredNeighbors {
		ip := net.ParseIP(nc.Destination)
		if ip == nil {
			return fmt.Errorf("invalid configured neighbor IP %q", nc.Destination)
		}
		hwAddr, err := net.ParseMAC(nc.HardwareAddr)
		if err != nil {
			return fmt.Errorf("invalid configured neighbor MAC %s for %s: %w", nc.HardwareAddr, nc.Destination, err)
		}
		family := unix.AF_INET6
		if ip.To4() != nil {
			family = unix.AF_INET
		}
		neigh := &netlink.Neigh{
			LinkIndex:    podLink.Attrs().Index,
			IP:           ip,
			HardwareAddr: hwAddr,
			State:        netlink.NUD_PERMANENT,
			Family:       family,
		}
		if err := nhNs.NeighSet(neigh); err != nil {
			return fmt.Errorf("failed to set configured neighbor %s: %w", nc.Destination, err)
		}
	}

	return nil
}

func addGatewayConnectedRoute(nhNs nlwrap.Handle, linkIndex int, gw net.IP) error {
	bits := 128
	if gw.To4() != nil {
		bits = 32
	}
	gwRoute := netlink.Route{
		LinkIndex: linkIndex,
		Dst:       &net.IPNet{IP: gw, Mask: net.CIDRMask(bits, bits)},
		Scope:     netlink.SCOPE_LINK,
	}
	return nhNs.RouteReplace(&gwRoute)
}

func resolveNeighborByND(nhNs nlwrap.Handle, ifIndex int, ip net.IP) net.HardwareAddr {
	probe := &netlink.Neigh{
		LinkIndex: ifIndex,
		IP:        ip,
		State:     netlink.NUD_INCOMPLETE,
		Family:    unix.AF_INET6,
	}
	_ = nhNs.NeighSet(probe)

	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		neighs, err := nhNs.NeighList(ifIndex, unix.AF_INET6)
		if err != nil {
			continue
		}
		for _, n := range neighs {
			if n.IP.Equal(ip) && len(n.HardwareAddr) > 0 &&
				n.State&(netlink.NUD_REACHABLE|netlink.NUD_STALE|netlink.NUD_PERMANENT) != 0 {
				klog.V(4).Infof("ND resolved gateway %s → %s", ip, n.HardwareAddr)
				return n.HardwareAddr
			}
		}
	}
	return nil
}

func deriveIPv6ForPod(prefix [12]byte, prefixLen int, podIPv4 net.IP) (net.IP, *net.IPNet, error) {
	v4 := podIPv4.To4()
	if v4 == nil {
		return nil, nil, fmt.Errorf("invalid IPv4 address: %v", podIPv4)
	}
	ip := make(net.IP, 16)
	copy(ip[:12], prefix[:])
	copy(ip[12:], v4)
	mask := net.CIDRMask(prefixLen, 128)
	return ip, &net.IPNet{IP: ip, Mask: mask}, nil
}

func extractPodIPv4(pod *api.PodSandbox) net.IP {
	for _, ip := range pod.GetIps() {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		if v4 := parsed.To4(); v4 != nil {
			return v4
		}
	}
	return nil
}

func readPodIPv4FromNetns(nsPath string) (net.IP, error) {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return nil, err
	}
	defer ns.Close()

	nhNs, err := nlwrap.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer nhNs.Close()

	links, err := nhNs.LinkList()
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.Attrs().Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := nhNs.AddrList(link, unix.AF_INET)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if v4 := addr.IP.To4(); v4 != nil {
				return v4, nil
			}
		}
	}
	return nil, nil
}

func setAddrGenMode(nsPath, ifName string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	path := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/addr_gen_mode", ifName)
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		klog.Warningf("failed to open netns for addr_gen_mode: %v", err)
		return
	}
	defer ns.Close()

	origNs, err := netns.Get()
	if err != nil {
		klog.Warningf("failed to get current netns: %v", err)
		return
	}
	defer origNs.Close()
	defer netns.Set(origNs) //nolint:errcheck

	if err := netns.Set(ns); err != nil {
		klog.Warningf("failed to enter pod netns: %v", err)
		return
	}

	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		klog.V(4).Infof("failed to set addr_gen_mode for %s: %v", ifName, err)
	}
}
