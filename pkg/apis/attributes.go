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

package apis

const (
	// TODO: Reconsider the domain being used when project becomes owned by some
	// SIG. The issue with "dra.net" is that http://dra.net is an actual
	// domain that is totally unrelated to this project and it can be a source
	// of confusion and problems.
	AttrPrefix = "dra.net"

	// TODO: Re-evaluate whether all attributes are needed.

	// AttrInterfaceName is the kernel network interface name. Absent for
	// IB-only devices, which have no netdev.
	AttrInterfaceName = AttrPrefix + "/" + "ifName"
	// AttrPCIAddress is the PCI address in BDF notation, e.g. "0000:3f:00.0".
	// Absent for any interface whose sysfs path contains no PCI component.
	AttrPCIAddress = AttrPrefix + "/" + "pciAddress"
	// AttrMac is the interface's MAC address.
	AttrMac = AttrPrefix + "/" + "mac"
	// AttrPCIVendor is the PCI vendor name reported by ghw, or "unknown" when
	// the vendor ID is not found in its PCI ID database. Truncated to the DRA
	// attribute value limit.
	AttrPCIVendor = AttrPrefix + "/" + "pciVendor"
	// AttrPCIDevice is the PCI device/product name reported by ghw, or "unknown"
	// when the device ID is not found in its PCI ID database. Truncated to the
	// DRA attribute value limit.
	AttrPCIDevice = AttrPrefix + "/" + "pciDevice"
	// AttrPCISubsystem is the 4-hex-digit subsystem device ID such as 0016,
	// with no subsystem vendor component.
	AttrPCISubsystem = AttrPrefix + "/" + "pciSubsystem"
	// AttrNUMANode is the NUMA node of the PCI device. Kept alongside the
	// standardized resource.kubernetes.io/numaNode for compatibility.
	AttrNUMANode = AttrPrefix + "/" + "numaNode"
	// AttrMTU is the interface MTU in bytes.
	AttrMTU = AttrPrefix + "/" + "mtu"
	// AttrEncapsulation is the link-layer encapsulation type (e.g. "ether",
	// "infiniband"). Ethernet with RoCE reports "ether", not "infiniband";
	// use AttrRDMA to detect RDMA capability, not this attribute.
	AttrEncapsulation = AttrPrefix + "/" + "encapsulation"
	// AttrAlias is the administrator-set interface alias, may be empty.
	AttrAlias = AttrPrefix + "/" + "alias"
	// AttrState is the interface operational state, e.g. "up", "down".
	AttrState = AttrPrefix + "/" + "state"
	// AttrType is the netlink link type, e.g. "device", "bond", "veth".
	AttrType = AttrPrefix + "/" + "type"
	// AttrIPv4 is a comma-separated, lexicographically sorted list of the
	// interface's global unicast IPv4 addresses in CIDR notation (e.g.
	// "10.0.0.5/24"). Trailing entries are dropped so the value stays within
	// the DRA attribute size limit. Absent if none.
	AttrIPv4 = AttrPrefix + "/" + "ipv4"
	// AttrIPv6 is the IPv6 equivalent of AttrIPv4.
	AttrIPv6 = AttrPrefix + "/" + "ipv6"
	// AttrTCFilterNames is a comma-separated list of detected ingress/egress
	// tc BPF filter names. Absent if no such filters are detected.
	AttrTCFilterNames = AttrPrefix + "/" + "tcFilterNames"
	// AttrTCXProgramNames is a comma-separated list of attached tcx
	// program names. Absent if none are attached.
	AttrTCXProgramNames = AttrPrefix + "/" + "tcxProgramNames"
	// AttrEBPF is true if an ingress/egress tc BPF filter or TCX program
	// is detected on the interface.
	AttrEBPF = AttrPrefix + "/" + "ebpf"
	// AttrSRIOV is true if the PF supports SR-IOV (sriov_totalvfs > 0),
	// regardless of whether any VFs are currently provisioned.
	AttrSRIOV = AttrPrefix + "/" + "sriov"
	// AttrSRIOVVfs is the number of VFs currently provisioned. Set only
	// when AttrSRIOV is true.
	AttrSRIOVVfs = AttrPrefix + "/" + "sriovVfs"
	// AttrIsSriovVf is true if the interface is an SR-IOV VF. Unlike most
	// booleans here, set only when true; absence means false.
	AttrIsSriovVf = AttrPrefix + "/" + "isSriovVf"
	// AttrVirtual is true for virtual/software interfaces (bond, veth,
	// tunnels, ...) not backed by a PCI device. Such interfaces do not
	// inherit the PCI/NUMA/RDMA attributes of their physical members.
	AttrVirtual = AttrPrefix + "/" + "virtual"
	// AttrRDMA indicates whether an RDMA link is associated with the device's
	// network interface or PCI address. Detection is per device, so an SR-IOV VF
	// is true only when its own RDMA link is visible during host discovery.
	// RoCE-capable Ethernet interfaces use this attribute with encapsulation
	// "ether".
	AttrRDMA = AttrPrefix + "/" + "rdma"
	// AttrRDMADevice is the RDMA link name, e.g. "mlx5_0". Set only when
	// AttrRDMA is true.
	AttrRDMADevice = AttrPrefix + "/" + "rdmaDevice"
)
