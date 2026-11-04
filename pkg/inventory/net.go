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

package inventory

import (
	"math"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/dranet/internal/nlwrap"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// getDefaultGwInterfaces returns a set of interface names that are configured
// as active default gateways in the main routing table, respecting route metrics.
// It identifies defaults as routes where Dst is nil (kernel default) or where
// Dst is exactly 0.0.0.0/0 (IPv4) or ::/0 (IPv6).
func getDefaultGwInterfaces() sets.Set[string] {
	interfaces := make(sets.Set[string])

	filter := &netlink.Route{
		Table: unix.RT_TABLE_MAIN,
	}
	routes, err := nlwrap.RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		klog.Errorf("Failed to list routes: %v", err)
		return interfaces
	}

	minMetricV4 := math.MaxInt32
	minMetricV6 := math.MaxInt32

	v4Interfaces := make(sets.Set[string])
	v6Interfaces := make(sets.Set[string])

	for _, r := range routes {
		if r.Family != netlink.FAMILY_V4 && r.Family != netlink.FAMILY_V6 {
			continue
		}

		if r.Dst != nil {
			ones, bits := r.Dst.Mask.Size()
			if !r.Dst.IP.IsUnspecified() || ones != 0 || (bits != 32 && bits != 128) {
				continue
			}
		}

		metric := r.Priority

		// 1. Gather all relevant link indices for this route
		var linkIndices []int
		if len(r.MultiPath) > 0 {
			for _, nh := range r.MultiPath {
				linkIndices = append(linkIndices, nh.LinkIndex)
			}
		} else {
			linkIndices = append(linkIndices, r.LinkIndex)
		}

		// 2. Evaluate each link index against our metric trackers
		for _, linkIndex := range linkIndices {
			intfLink, err := netlink.LinkByIndex(linkIndex)
			if err != nil {
				klog.Infof("Failed to get interface link for index %d: %v", linkIndex, err)
				continue
			}
			name := intfLink.Attrs().Name

			if r.Family == netlink.FAMILY_V4 {
				if metric < minMetricV4 {
					minMetricV4 = metric
					v4Interfaces = make(sets.Set[string]) // Clear previous losers
					v4Interfaces.Insert(name)
				} else if metric == minMetricV4 {
					v4Interfaces.Insert(name) // ECMP tie: keep both
				}
			} else {
				if metric < minMetricV6 {
					minMetricV6 = metric
					v6Interfaces = make(sets.Set[string]) // Clear previous losers
					v6Interfaces.Insert(name)
				} else if metric == minMetricV6 {
					v6Interfaces.Insert(name) // ECMP tie: keep both
				}
			}
		}
	}

	// Merge the winning IPv4 and IPv6 interfaces into the final set
	for k := range v4Interfaces {
		interfaces.Insert(k)
	}
	for k := range v6Interfaces {
		interfaces.Insert(k)
	}

	// A host has one management uplink, or two for redundancy. A large winning
	// set means something other than a management uplink is advertising a
	// default route at the same metric, which on RDMA fabrics is common: every
	// rail NIC gets a default route from a Router Advertisement, ties on metric
	// 1024, and the whole fabric disappears from the inventory. Say so, because
	// the symptom is otherwise just missing devices.
	if len(v6Interfaces) > maxPlausibleUplinks {
		klog.Warningf("Detected %d interfaces as IPv6 default-gateway uplinks (%v), which is more than a host plausibly has. "+
			"If these are RDMA NICs receiving Router Advertisements, they are excluded from the inventory; "+
			"set --uplink-interfaces to name the real uplinks instead.", len(v6Interfaces), v6Interfaces.UnsortedList())
	}
	if len(v4Interfaces) > maxPlausibleUplinks {
		klog.Warningf("Detected %d interfaces as IPv4 default-gateway uplinks (%v), which is more than a host plausibly has. "+
			"Set --uplink-interfaces to name the real uplinks instead.", len(v4Interfaces), v4Interfaces.UnsortedList())
	}

	return interfaces
}

// maxPlausibleUplinks is the largest number of default-gateway uplinks a host
// is expected to have: one, or two for redundancy. Above that, the detection is
// almost certainly picking up a fabric rather than a management uplink.
const maxPlausibleUplinks = 2

// getExcludedUplinkInterfaces returns the set of interface names that must be
// excluded from the inventory: the active default-gateway uplinks plus every
// netdev that is a descendant of one of those uplinks.
//
// When uplinks is non-empty it names the uplinks explicitly and replaces the
// automatic default-gateway detection. That is needed on fabrics where the RDMA
// NICs themselves receive a default route from Router Advertisements: they are
// indistinguishable from a management uplink by routing alone, so detection
// would exclude the very devices DraNet exists to hand out. Names that do not
// exist on this host are ignored, so one list can carry the uplink name of
// every node shape in the cluster; detection runs instead only when none of
// the names exist. A child tied to a
// parent through MasterIndex (bond/team slave, bridge port, VF enslaved to
// its PF, ...) shares its forwarding state with that parent, so moving just
// the child into a pod netns strands it from the parent that owns that
// state; when the parent is the host's default-gw uplink this also degrades
// host connectivity. There is no scenario where relocating only the child of
// a default-gw uplink is correct, so the entire MasterIndex-linked subtree
// rooted at each uplink should be excluded.
func getExcludedUplinkInterfaces(uplinks sets.Set[string]) sets.Set[string] {
	links, err := nlwrap.LinkList()
	if err != nil {
		klog.Errorf("Failed to list links for uplink child exclusion: %v", err)
		if len(uplinks) > 0 {
			return uplinks.Clone()
		}
		return getDefaultGwInterfaces()
	}

	var excluded sets.Set[string]
	if len(uplinks) > 0 {
		// Only names that exist on this host count. The DaemonSet runs the
		// same flag on every node, so in a cluster with more than one node
		// shape the list carries the uplink name of each shape and every node
		// is missing some of them; that is expected and only worth noting.
		// When none of the names exist the flag is most likely a typo, and
		// honouring it would switch detection off with nothing in its place,
		// handing the real uplink to the first Pod that asks for it. Fall back
		// to detection in that case, so the mistake costs devices rather than
		// the node's connectivity, and say so.
		excluded = sets.New[string]()
		missing := sets.New[string]()
		names := sets.New[string]()
		for _, l := range links {
			names.Insert(l.Attrs().Name)
		}
		for name := range uplinks {
			if names.Has(name) {
				excluded.Insert(name)
			} else {
				missing.Insert(name)
			}
		}
		switch {
		case len(excluded) == 0:
			klog.Warningf("None of the interfaces named by --uplink-interfaces (%v) exist on this host; excluding the detected default-gateway uplinks instead so a typo cannot expose the real uplink", sets.List(missing))
			excluded = getDefaultGwInterfaces()
		case len(missing) > 0:
			klog.V(2).Infof("Ignoring the interfaces named by --uplink-interfaces that do not exist on this host: %v", sets.List(missing))
		}
	} else {
		excluded = getDefaultGwInterfaces()
	}

	// Build a parent-index -> children adjacency map in a single pass so we
	// can cull whole families in one mutating walk instead of re-scanning the
	// link list per level of nesting.
	childrenOf := make(map[int][]netlink.Link)
	var seeds []int
	for _, l := range links {
		attrs := l.Attrs()
		if attrs.MasterIndex != 0 {
			childrenOf[attrs.MasterIndex] = append(childrenOf[attrs.MasterIndex], l)
		}
		if excluded.Has(attrs.Name) {
			seeds = append(seeds, attrs.Index)
		}
	}

	// BFS from each excluded uplink through the adjacency map. Deleting the
	// entry after visiting guarantees each child is processed at most once
	// even if the hierarchy is deep (vf -> vf-child -> uplink).
	for i := 0; i < len(seeds); i++ {
		children, found := childrenOf[seeds[i]]
		if !found {
			continue
		}
		delete(childrenOf, seeds[i])
		for _, child := range children {
			attrs := child.Attrs()
			excluded.Insert(attrs.Name)
			seeds = append(seeds, attrs.Index)
		}
	}

	return excluded
}

func getTcFilters(link netlink.Link) ([]string, bool) {
	isTcEBPF := false
	filterNames := sets.Set[string]{}
	for _, parent := range []uint32{netlink.HANDLE_MIN_INGRESS, netlink.HANDLE_MIN_EGRESS} {
		filters, err := nlwrap.FilterList(link, parent)
		if err == nil {
			for _, f := range filters {
				if bpffFilter, ok := f.(*netlink.BpfFilter); ok {
					isTcEBPF = true
					filterNames.Insert(bpffFilter.Name)
				}
			}
		}
	}
	return filterNames.UnsortedList(), isTcEBPF
}

// see https://github.com/cilium/ebpf/issues/1117
func getTcxFilters(device netlink.Link) ([]string, bool) {
	isTcxEBPF := false
	programNames := sets.Set[string]{}
	for _, attach := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
		result, err := link.QueryPrograms(link.QueryOptions{
			Target: int(device.Attrs().Index),
			Attach: attach,
		})
		if err != nil || result == nil || len(result.Programs) == 0 {
			continue
		}

		isTcxEBPF = true
		for _, p := range result.Programs {
			prog, err := ebpf.NewProgramFromID(p.ID)
			if err != nil {
				continue
			}
			defer prog.Close()

			pi, err := prog.Info()
			if err != nil {
				continue
			}
			programNames.Insert(pi.Name)
		}
	}
	return programNames.UnsortedList(), isTcxEBPF
}

// IsLACPBond reports whether ifName is an 802.3ad (LACP) bond.
func IsLACPBond(ifName string) bool {
	if ifName == "" {
		return false
	}

	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return false
	}

	bond, ok := link.(*netlink.Bond)
	if !ok {
		// Not a bonding interface.
		return false
	}

	return bond.Mode == netlink.BOND_MODE_802_3AD
}
