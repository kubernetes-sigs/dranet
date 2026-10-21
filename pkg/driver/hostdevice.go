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

	"sigs.k8s.io/dranet/pkg/apis"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/dranet/internal/nlwrap"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// podNetnsHandle holds open netlink handles for the host and pod network
// namespaces so RunPodSandbox can attach and configure all devices for a pod
// without repeatedly opening namespaces, switching threads with setns, or
// creating netlink sockets per device. Handles are initialized lazily on first
// use.
type podNetnsHandle struct {
	nsPath      string
	containerNs netns.NsHandle
	hostHandle  nlwrap.Handle
	hostSocket  *nl.NetlinkSocket
	podHandle   nlwrap.Handle
}

func newPodNetnsHandle(nsPath string) *podNetnsHandle {
	return &podNetnsHandle{
		nsPath:      nsPath,
		containerNs: netns.None(),
	}
}

func (h *podNetnsHandle) ensureHost() (nlwrap.Handle, *nl.NetlinkSocket, error) {
	if h.hostHandle.Handle == nil {
		nh, err := nlwrap.NewHandle()
		if err != nil {
			return nlwrap.Handle{}, nil, fmt.Errorf("could not get host netlink handle: %w", err)
		}
		h.hostHandle = nh
	}
	if h.hostSocket == nil {
		s, err := nl.GetNetlinkSocketAt(netns.None(), netns.None(), unix.NETLINK_ROUTE)
		if err != nil {
			return nlwrap.Handle{}, nil, fmt.Errorf("could not get network namespace handle: %w", err)
		}
		h.hostSocket = s
	}
	return h.hostHandle, h.hostSocket, nil
}

func (h *podNetnsHandle) ensurePod() (netns.NsHandle, nlwrap.Handle, error) {
	if !h.containerNs.IsOpen() {
		ns, err := netns.GetFromPath(h.nsPath)
		if err != nil {
			return netns.None(), nlwrap.Handle{}, fmt.Errorf("failed to get container network namespace %s: %w", h.nsPath, err)
		}
		h.containerNs = ns
	}
	if h.podHandle.Handle == nil {
		nhNs, err := nlwrap.NewHandleAt(h.containerNs)
		if err != nil {
			return netns.None(), nlwrap.Handle{}, fmt.Errorf("failed to get netlink handle in container namespace %s: %w", h.nsPath, err)
		}
		h.podHandle = nhNs
	}
	return h.containerNs, h.podHandle, nil
}

func (h *podNetnsHandle) Close() {
	if h.podHandle.Handle != nil {
		h.podHandle.Close()
		h.podHandle = nlwrap.Handle{}
	}
	if h.hostSocket != nil {
		h.hostSocket.Close()
		h.hostSocket = nil
	}
	if h.hostHandle.Handle != nil {
		h.hostHandle.Close()
		h.hostHandle = nlwrap.Handle{}
	}
	if h.containerNs.IsOpen() {
		_ = h.containerNs.Close()
		h.containerNs = netns.None()
	}
}

// attachNetdev is nsAttachNetdevWithHandle behind a variable so tests can
// simulate slow device attachment.
var attachNetdev = nsAttachNetdevWithHandle

func nsAttachNetdev(hostIfName string, containerNsPAth string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
	h := newPodNetnsHandle(containerNsPAth)
	defer h.Close()
	data, _, err := nsAttachNetdevWithHandle(h, hostIfName, interfaceConfig)
	return data, err
}

func nsAttachNetdevWithHandle(h *podNetnsHandle, hostIfName string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, netlink.Link, error) {
	containerNsPAth := h.nsPath
	hostHandle, s, err := h.ensureHost()
	if err != nil {
		return nil, nil, err
	}

	hostDev, err := hostHandle.LinkByName(hostIfName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get link for interface %s: %w", hostIfName, err)
	}

	// The kernel creates no IPv6 settings below this MTU, so accept_ra cannot be
	// set. Reject it before the link is touched so the host device stays usable.
	if interfaceConfig.AcceptRA != nil && interfaceConfig.MTU == nil && hostDev.Attrs().MTU < apis.MinIPv6MTU {
		return nil, nil, fmt.Errorf("acceptRA requires an MTU of at least %d, but %s has MTU %d and the claim sets no mtu", apis.MinIPv6MTU, hostIfName, hostDev.Attrs().MTU)
	}

	// Devices can be renamed only when down. Skip the netlink call if
	// PrepareResourceClaims already set the interface down.
	if hostDev.Attrs().Flags&net.FlagUp != 0 {
		if err = hostHandle.LinkSetDown(hostDev); err != nil {
			return nil, nil, fmt.Errorf("failed to set %q down: %w", hostIfName, err)
		}
	}

	containerNs, nhNs, err := h.ensurePod()
	if err != nil {
		return nil, nil, err
	}

	attrs := hostDev.Attrs()

	// copy from netlink.LinkModify(dev) using only the parts needed
	flags := unix.NLM_F_REQUEST | unix.NLM_F_ACK
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, flags)
	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(attrs.Index)
	req.AddData(msg)

	ifName := attrs.Name
	if interfaceConfig.Name != "" {
		ifName = interfaceConfig.Name
	}
	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(ifName))
	req.AddData(nameData)

	// Configuration values
	if interfaceConfig.MTU != nil {
		ifMtu := uint32(*interfaceConfig.MTU)
		mtu := nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(ifMtu))
		req.AddData(mtu)
	}

	if interfaceConfig.HardwareAddr != nil {
		if hardwareAddr, err := net.ParseMAC(*interfaceConfig.HardwareAddr); err == nil {
			hwaddr := nl.NewRtAttr(unix.IFLA_ADDRESS, []byte(hardwareAddr))
			req.AddData(hwaddr)
		}
	}

	if interfaceConfig.GSOMaxSize != nil {
		gsoMaxSize := uint32(*interfaceConfig.GSOMaxSize)
		gsoAttr := nl.NewRtAttr(unix.IFLA_GSO_MAX_SIZE, nl.Uint32Attr(gsoMaxSize))
		req.AddData(gsoAttr)
	}

	if interfaceConfig.GROMaxSize != nil {
		groMaxSize := uint32(*interfaceConfig.GROMaxSize)
		groAttr := nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, nl.Uint32Attr(groMaxSize))
		req.AddData(groAttr)
	}

	if interfaceConfig.GSOIPv4MaxSize != nil {
		gsoMaxSize := uint32(*interfaceConfig.GSOIPv4MaxSize)
		gsoV4Attr := nl.NewRtAttr(unix.IFLA_GSO_IPV4_MAX_SIZE, nl.Uint32Attr(gsoMaxSize))
		req.AddData(gsoV4Attr)
	}

	if interfaceConfig.GROIPv4MaxSize != nil {
		groMaxSize := uint32(*interfaceConfig.GROIPv4MaxSize)
		groV4Attr := nl.NewRtAttr(unix.IFLA_GRO_IPV4_MAX_SIZE, nl.Uint32Attr(groMaxSize))
		req.AddData(groV4Attr)
	}

	val := nl.Uint32Attr(uint32(containerNs))
	attr := nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
	req.AddData(attr)

	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, nil, fmt.Errorf("failed to move interface %s to container namespace %s: %w", hostIfName, containerNsPAth, err)
	}

	nsLink, err := nhNs.LinkByName(ifName)
	if err != nil {
		return nil, nil, fmt.Errorf("link not found for interface %s on namespace %s: %w", ifName, containerNsPAth, err)
	}

	// Apply before the link comes up so it never answers ARP or accepts router
	// advertisements with the wrong policy.
	if err := applyInterfaceSysctlConfig(containerNs, ifName, interfaceConfig); err != nil {
		rollbackErr := nsDetachNetdevFromNS(containerNs, containerNsPAth, ifName, hostIfName)
		return nil, nil, fmt.Errorf("failed to apply sysctl configuration to interface %s in namespace %s: %w", ifName, containerNsPAth, errors.Join(err, rollbackErr))
	}

	networkData := &resourceapi.NetworkDeviceData{
		InterfaceName:   nsLink.Attrs().Name,
		HardwareAddress: string(nsLink.Attrs().HardwareAddr.String()),
	}

	for _, address := range interfaceConfig.Addresses {
		ip, ipnet, err := net.ParseCIDR(address)
		if err != nil {
			klog.Infof("failed to parse address %s : %v", address, err)
			continue // this should not happen since it has been already validated
		}
		err = nhNs.AddrAdd(nsLink, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to set up address %s on namespace %s: %w", address, containerNsPAth, err)
		}
		networkData.IPs = append(networkData.IPs, address)
	}

	err = nhNs.LinkSetUp(nsLink)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set up interface %s on namespace %s: %w", nsLink.Attrs().Name, containerNsPAth, err)
	}

	return networkData, nsLink, nil
}

func nsDetachNetdev(containerNsPAth string, devName string, outName string) error {
	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPAth, devName, err)
	}
	defer containerNs.Close()
	return nsDetachNetdevFromNS(containerNs, containerNsPAth, devName, outName)
}

func nsDetachNetdevFromNS(containerNs netns.NsHandle, containerNsPath string, devName string, outName string) error {
	// to avoid golang problem with goroutines we create the socket in the
	// namespace and use it directly
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer nhNs.Close()

	nsLink, err := nhNs.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("link not found for interface %s on namespace %s: %w", devName, containerNsPath, err)
	}

	// set the device down to avoid network conflicts
	// when it is restored to the original namespace
	err = nhNs.LinkSetDown(nsLink)
	if err != nil {
		return fmt.Errorf("failed to set %q down: %w", devName, err)
	}

	attrs := nsLink.Attrs()
	// restore the original name if it was renamed
	if nsLink.Attrs().Alias != "" {
		attrs.Name = nsLink.Attrs().Alias
	}

	rootNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get root network namespace: %w", err)
	}
	defer rootNs.Close()

	s, err := nl.GetNetlinkSocketAt(containerNs, rootNs, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer s.Close()
	// copy from netlink.LinkModify(dev) using only the parts needed
	flags := unix.NLM_F_REQUEST | unix.NLM_F_ACK
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, flags)
	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(attrs.Index)
	req.AddData(msg)

	ifName := attrs.Name
	if outName != "" {
		ifName = outName
	}
	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(ifName))
	req.AddData(nameData)

	val := nl.Uint32Attr(uint32(rootNs))
	attr := nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
	req.AddData(attr)

	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return fmt.Errorf("failed to move interface %s to root namespace: %w", devName, err)
	}

	// Set up the interface in case host network workloads depend on it
	hostDev, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to get link for interface %s: %w", ifName, err)
	}

	if err = netlink.LinkSetUp(hostDev); err != nil {
		return fmt.Errorf("failed to set %q up: %w", ifName, err)
	}
	return nil
}
