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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

// ipvlanModeToNetlink translates the API IPVLAN mode to its netlink constant.
var ipvlanModeToNetlink = map[apis.IPVlanMode]netlink.IPVlanMode{
	apis.IPVlanModeL2: netlink.IPVLAN_MODE_L2,
}

// ipvlanFlagToNetlink translates the API IPVLAN flag to its netlink constant.
var ipvlanFlagToNetlink = map[apis.IPVlanFlag]netlink.IPVlanFlag{
	apis.IPVlanFlagBridge: netlink.IPVLAN_FLAG_BRIDGE,
}

// addIPVlan creates an IPVLAN child of parentLink inside containerNs, applies
// the interface settings, and deletes the child again if any later step fails.
func addIPVlan(ifName string, parentLink netlink.Link, containerNs netns.NsHandle, config apis.InterfaceConfig) (networkData *resourceapi.NetworkDeviceData, err error) {
	mode := netlink.IPVLAN_MODE_L2
	flag := netlink.IPVLAN_FLAG_BRIDGE
	if config.IPVlan != nil {
		var ok bool
		if mode, ok = ipvlanModeToNetlink[config.IPVlan.Mode]; !ok {
			return nil, fmt.Errorf("unsupported ipvlan mode: %s", config.IPVlan.Mode)
		}
		if flag, ok = ipvlanFlagToNetlink[config.IPVlan.Flag]; !ok {
			return nil, fmt.Errorf("unsupported ipvlan flag: %s", config.IPVlan.Flag)
		}
	}

	// Older kernels reject creating a link directly in another netns if the name
	// collides in the host netns, so use a random temp name and rename it later.
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("failed to generate temporary interface name: %w", err)
	}
	tempName := hex.EncodeToString(b)

	// Open the handle before creating the link so a handle failure cannot leak the child.
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		return nil, fmt.Errorf("failed to get netlink handle: %w", err)
	}
	defer nhNs.Close()

	ipvlan := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        tempName,
			ParentIndex: parentLink.Attrs().Index,
			Namespace:   netlink.NsFd(int(containerNs)),
		},
		Mode: mode,
		Flag: flag,
	}
	// The kernel applies the MTU on creation. Without it the child inherits the parent MTU.
	if config.MTU != nil {
		ipvlan.MTU = int(*config.MTU)
	}

	if err := netlink.LinkAdd(ipvlan); err != nil {
		// If a user creates a macvlan and ipvlan on same parent, only one slave iface can be active at a time.
		return nil, fmt.Errorf("failed to create the %s ipvlan interface: %v", ifName, err)
	}

	// Delete the child on any later failure so it never outlives a failed setup.
	// Until the lookup below succeeds the delete resolves the temp name, after it the index.
	rollbackLink := netlink.Link(ipvlan)
	defer func() {
		if err == nil {
			return
		}
		if delErr := nhNs.LinkDel(rollbackLink); delErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to delete the %s ipvlan interface after a setup failure: %w", ifName, delErr))
		}
	}()

	nsLink, err := nhNs.LinkByName(tempName)
	if err != nil {
		return nil, fmt.Errorf("link not found for interface %s: %w", tempName, err)
	}
	rollbackLink = nsLink

	// Rename back to ifName now that the link is created in the pod netns.
	if err := nhNs.LinkSetName(nsLink, ifName); err != nil {
		return nil, fmt.Errorf("failed to rename interface %s to %s: %w", tempName, ifName, err)
	}

	if err := applyOffloadSizes(nhNs, nsLink, ifName, config); err != nil {
		return nil, err
	}

	// Apply before the link comes up so it never answers ARP with the wrong policy.
	if err := applyInterfaceARPConfig(containerNs, ifName, config); err != nil {
		return nil, fmt.Errorf("failed to apply ARP configuration to interface %s: %w", ifName, err)
	}

	networkData = &resourceapi.NetworkDeviceData{
		InterfaceName:   ifName,
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

	if err := nhNs.LinkSetUp(nsLink); err != nil {
		return nil, fmt.Errorf("failed to set up interface %s: %w", ifName, err)
	}

	return networkData, nil
}

// applyOffloadSizes sets the requested GSO and GRO limits on the child. The kernel
// rejects a GSO size above the inherited TSO maximum and a GRO size above GRO_MAX_SIZE.
func applyOffloadSizes(nhNs nlwrap.Handle, link netlink.Link, ifName string, config apis.InterfaceConfig) error {
	if config.GSOMaxSize != nil {
		if err := nhNs.LinkSetGSOMaxSize(link, int(*config.GSOMaxSize)); err != nil {
			return fmt.Errorf("failed to set gso_max_size %d on interface %s: %w", *config.GSOMaxSize, ifName, err)
		}
	}
	if config.GROMaxSize != nil {
		if err := nhNs.LinkSetGROMaxSize(link, int(*config.GROMaxSize)); err != nil {
			return fmt.Errorf("failed to set gro_max_size %d on interface %s: %w", *config.GROMaxSize, ifName, err)
		}
	}
	if config.GSOIPv4MaxSize != nil {
		if err := nhNs.LinkSetGSOIPv4MaxSize(link, int(*config.GSOIPv4MaxSize)); err != nil {
			return fmt.Errorf("failed to set gso_ipv4_max_size %d on interface %s: %w", *config.GSOIPv4MaxSize, ifName, err)
		}
	}
	if config.GROIPv4MaxSize != nil {
		if err := nhNs.LinkSetGROIPv4MaxSize(link, int(*config.GROIPv4MaxSize)); err != nil {
			return fmt.Errorf("failed to set gro_ipv4_max_size %d on interface %s: %w", *config.GROIPv4MaxSize, ifName, err)
		}
	}
	return nil
}

// nsCreateSubinterface creates a subinterface (currently supports IPVLAN) of hostIfName
// directly in the container network namespace and configures it with the specified addresses.
func nsCreateSubinterface(hostIfName string, containerNsPath string, config apis.InterfaceConfig) (*resourceapi.NetworkDeviceData, error) {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get container network namespace %s: %w", containerNsPath, err)
	}
	defer containerNs.Close()

	parentLink, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		return nil, fmt.Errorf("could not find parent interface %s on host: %w", hostIfName, err)
	}

	// The kernel does not check a child MTU against its parent, so check it here.
	if config.MTU != nil && int(*config.MTU) > parentLink.Attrs().MTU {
		return nil, fmt.Errorf("requested MTU %d for %s subinterface %s exceeds parent interface %s MTU %d", *config.MTU, config.Type, config.Name, hostIfName, parentLink.Attrs().MTU)
	}

	// Make sure the parent link is up on the host, otherwise subinterfaces cannot transmit traffic.
	if err := netlink.LinkSetUp(parentLink); err != nil {
		return nil, fmt.Errorf("failed to bring parent interface %s UP on host: %w", hostIfName, err)
	}

	var networkData *resourceapi.NetworkDeviceData
	// We only support creating IPVLAN subinterface right now.
	if config.Type == apis.InterfaceTypeIPVLAN {
		networkData, err = addIPVlan(config.Name, parentLink, containerNs, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create the %s ipvlan interface on namespace %s: %w", config.Name, containerNsPath, err)
		}
	} else {
		return nil, fmt.Errorf("unsupported interface type: %v", config.Type)
	}

	return networkData, nil
}

// nsDeleteSubinterface deletes a subinterface inside the container namespace.
// A namespace path that no longer exists counts as cleaned up. The kernel
// deletes any IPvlan child still in the namespace when the namespace is
// destroyed.
func nsDeleteSubinterface(containerNsPath string, devName string) error {
	containerNs, err := netns.GetFromPath(containerNsPath)
	if errors.Is(err, os.ErrNotExist) {
		klog.V(2).Infof("Network namespace path %s is gone; relying on the kernel to delete subinterface %s when the namespace is destroyed", containerNsPath, devName)
		return nil
	}
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
