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

	return interfaces
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
