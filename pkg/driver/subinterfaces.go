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
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func addMacVlan(containerNsPath string, devName string, mode netlink.MacvlanMode) error {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPath, devName, err)
	}
	defer containerNs.Close()

	parentLink, err := nlwrap.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("could not find parent interface %s : %w", devName, err)
	}

	macvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        "macvlan-" + devName,
			ParentIndex: parentLink.Attrs().Index,
			NetNsID:     int(containerNs),
		},
		Mode: mode,
	}
	if err := netlink.LinkAdd(macvlan); err != nil {
		// If a user creates a macvlan and ipvlan on same parent, only one slave iface can be active at a time.
		return fmt.Errorf("failed to create the %s macvlan interface: %v", macvlan.Name, err)
	}

	return nil
}

func addIPVlan(ifName string, parentLink netlink.Link, containerNs netns.NsHandle, config apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
	var mode netlink.IPVlanMode
	var flag netlink.IPVlanFlag
	if config.SubInterface != nil && config.SubInterface.IPVlanConfig != nil {
		switch config.SubInterface.IPVlanConfig.Mode {
		case "", "l2":
			mode = netlink.IPVLAN_MODE_L2
		case "l3":
			mode = netlink.IPVLAN_MODE_L3
		case "l3s":
			mode = netlink.IPVLAN_MODE_L3S
		default:
			return nil, fmt.Errorf("unsupported ipvlan mode: %s", config.SubInterface.IPVlanConfig.Mode)
		}

		switch config.SubInterface.IPVlanConfig.Flag {
		case "bridge":
			flag = netlink.IPVLAN_FLAG_BRIDGE
		case "vepa":
			flag = netlink.IPVLAN_FLAG_VEPA
		case "private":
			flag = netlink.IPVLAN_FLAG_PRIVATE
		default:
			return nil, fmt.Errorf("unsupported ipvlan flag: %s", config.SubInterface.IPVlanConfig.Flag)
		}
	} else {
		mode = netlink.IPVLAN_MODE_L2
		flag = netlink.IPVLAN_FLAG_BRIDGE
	}

	ipvlan := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        ifName,
			ParentIndex: parentLink.Attrs().Index,
			Namespace:   netlink.NsFd(int(containerNs)),
		},
		Mode: mode,
		Flag: flag,
	}

	if err := netlink.LinkAdd(ipvlan); err != nil {
		// If a user creates a macvlan and ipvlan on same parent, only one slave iface can be active at a time.
		return nil, fmt.Errorf("failed to create the %s ipvlan interface: %v", ipvlan.Name, err)
	}

	// Get handle in container namespace to configure IPs and bring the link UP.
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return nil, fmt.Errorf("failed to get netlink handle: %w", err)
	}
	defer nhNs.Close()

	nsLink, err := nhNs.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("link not found for interface %s: %w", ifName, err)
	}

	networkData := &resourceapi.NetworkDeviceData{
		InterfaceName:   nsLink.Attrs().Name,
		HardwareAddress: nsLink.Attrs().HardwareAddr.String(),
	}

	for _, address := range config.Addresses {
		ip, ipnet, err := net.ParseCIDR(address)
		if err != nil {
			klog.Infof("failed to parse address %s : %v", address, err)
			continue
		}
		err = nhNs.AddrAdd(nsLink, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}})
		if err != nil {
			return nil, fmt.Errorf("failed to set up address %s: %w", address, err)
		}
		networkData.IPs = append(networkData.IPs, address)
	}

	err = nhNs.LinkSetUp(nsLink)
	if err != nil {
		return nil, fmt.Errorf("failed to set up interface %s: %w", nsLink.Attrs().Name, err)
	}

	return networkData, nil
}

// nsCreateSubinterface creates a subinterface (currently supports IPVLAN) of hostIfName
// directly in the container network namespace and configures it with the specified addresses.
func nsCreateSubinterface(hostIfName string, containerNsPath string, interfaceConfig apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network namespace %s: %w", containerNsPath, err)
	}
	defer containerNs.Close()

	parentLink, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		return nil, fmt.Errorf("could not find parent interface %s on host: %w", hostIfName, err)
	}

	// Make sure the parent link is up on the host, otherwise subinterfaces cannot transmit traffic.
	if err := netlink.LinkSetUp(parentLink); err != nil {
		return nil, fmt.Errorf("failed to bring parent interface %s UP on host: %w", hostIfName, err)
	}

	// ifName should be the interface name configured in Pod, which has the subInterface
	// type prefix (e.g. "ipvlan-"). It falls back to use the name configured in host,
	// if the config in Pod doesn't specify the interface name.
	ifName := hostIfName
	if interfaceConfig.Name != "" {
		ifName = interfaceConfig.Name
	}

	var networkData *resourceapi.NetworkDeviceData
	// We only support creating IPVlan subinterface right now.
	if interfaceConfig.SubInterface.Type == apis.SubInterfaceTypeIPVlan {
		networkData, err = addIPVlan(ifName, parentLink, containerNs, interfaceConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create the %s ipvlan interface on namespace %s: %w", ifName, containerNsPath, err)
		}
	} else {
		return nil, fmt.Errorf("unsupported subinterface type: %v", interfaceConfig.SubInterface.Type)
	}

	return networkData, nil
}

// nsDeleteSubinterface deletes a subinterface inside the container namespace.
func nsDeleteSubinterface(containerNsPath string, devName string) error {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return fmt.Errorf("could not get container network namespace %s: %w", containerNsPath, err)
	}
	defer containerNs.Close()

	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	defer nhNs.Close()

	link, err := nhNs.LinkByName(devName)
	if err != nil {
		// If the link is already gone, return nil (idempotent cleanup).
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("link not found for interface %s on namespace %s: %w", devName, containerNsPath, err)
	}

	if err := nhNs.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete subinterface %s inside namespace %s: %w", devName, containerNsPath, err)
	}
	return nil
}
