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

package ipam

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"net/netip"
	"sync"

	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/apis"
)

// based on existing Kindnet IP allocator:
// https://github.com/kubernetes-sigs/kindnet/blob/main/cmd/cni-kindnet/netconf.go

// LocalIPAM is a node-local IP allocator. It hands out addresses from
// caller-provided ranges while avoiding collisions with addresses already in use
// on the node.
type LocalIPAM struct {
	mu sync.Mutex
	// allocatedIPs is the set of addresses currently in use on the node.
	allocatedIPs map[netip.Addr]struct{}
}

func NewLocalIPAM(initialIPs []string) *LocalIPAM {
	ips := make(map[netip.Addr]struct{})
	for _, ipStr := range initialIPs {
		if prefix, err := netip.ParsePrefix(ipStr); err == nil {
			ips[prefix.Addr()] = struct{}{}
		} else {
			klog.Warningf("Failed to parse IP to initialize %q: %v", ipStr, err)
		}
	}
	return &LocalIPAM{
		allocatedIPs: ips,
	}
}

// Allocate allocates at most one IP address per IP family from the given
// ranges. Ranges are considered in order: the first range that yields an address
// for a family wins, and subsequent ranges of that same family are skipped. If a
// range is valid but exhausted, the next range of the same family is tried.
//
// It returns the allocated addresses formatted as host CIDRs. If a family has at
// least one range but no address could be allocated for it, an error is returned.
func (ipam *LocalIPAM) Allocate(ranges []apis.IPRangeConfig) ([]string, error) {
	// family key: true for IPv6, false for IPv4.
	allocated := make(map[bool]string)
	seen := make(map[bool]bool)
	lastErr := make(map[bool]error)

	// assigned holds the addresses generated so far by this Allocate invocation
	var assigned []string
	// releaseAll frees the addresses this Allocate invocation already generated,
	// so a mid-way failure leaves no partially-allocated state in the IPAM.
	releaseAll := func() {
		for _, ip := range assigned {
			ipam.Release(ip)
		}
	}

	for _, r := range ranges {
		start, end, err := resolveRange(r)
		if err != nil {
			releaseAll()
			return nil, err
		}
		family := start.Is6()
		seen[family] = true
		if _, done := allocated[family]; done {
			continue
		}
		ip, err := ipam.allocateForRange(start, end)
		if err != nil {
			// Range is valid but exhausted.
			lastErr[family] = err
			continue
		}
		allocated[family] = ip
		assigned = append(assigned, ip)
	}

	// Every family that had at least one range must have produced an address.
	for family := range seen {
		if _, done := allocated[family]; !done {
			releaseAll()
			return nil, fmt.Errorf("no available IP address for %s in the configured ranges: %w", familyName(family), lastErr[family])
		}
	}

	return assigned, nil
}

// Release removes the specified IP address allocation from the IPAM state.
// ipStr must be in CIDR prefix form (e.g. "192.168.1.100/32").
func (ipam *LocalIPAM) Release(ipStr string) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	var ip netip.Addr
	if prefix, err := netip.ParsePrefix(ipStr); err == nil {
		ip = prefix.Addr()
	} else {
		klog.Warningf("Failed to parse IP to release %q: %v", ipStr, err)
		return
	}

	delete(ipam.allocatedIPs, ip)
}

// resolveRange converts an IPRangeConfig into the concrete inclusive [start, end]
// boundaries used for allocation.
//
// When both StartIP and EndIP are set, they are validated and used directly.
// Otherwise the boundaries are derived from CIDR, spanning every address
// except the network address and the broadcast address.
func resolveRange(cfg apis.IPRangeConfig) (start netip.Addr, end netip.Addr, err error) {
	if err := cfg.Validate(); err != nil {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("ipRange: %w", err)
	}

	// Mode 1: explicit start/end boundaries take priority.
	if cfg.StartIP != "" {
		return netip.MustParseAddr(cfg.StartIP), netip.MustParseAddr(cfg.EndIP), nil
	}

	// Mode 2: derive boundaries from CIDR.
	cidr := netip.MustParsePrefix(cfg.CIDR).Masked()
	broadcast, err := broadcastAddress(cidr)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	start, end = cidr.Addr().Next(), broadcast.Prev()
	if start.Compare(end) > 0 {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("ipRange: cidr %q has no allocatable addresses", cfg.CIDR)
	}
	return start, end, nil
}

// allocateForRange finds and returns a free IP address within the inclusive [start, end]
// range, avoiding conflicts with currently allocated IPs on the node.
func (ipam *LocalIPAM) allocateForRange(start, end netip.Addr) (string, error) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	b, err := randomizedBounds(start, end)
	if err != nil {
		return "", err
	}

	iterator := ipIterator(b)
	for {
		ip := iterator()
		if !ip.IsValid() {
			break
		}
		// Check if IP is already allocated
		if _, exists := ipam.allocatedIPs[ip]; exists {
			continue
		}
		ipam.allocatedIPs[ip] = struct{}{}
		prefix := netip.PrefixFrom(ip, ip.BitLen())
		klog.V(2).Infof("Successfully allocated IP %s from range [%s, %s]", prefix.String(), start, end)
		return prefix.String(), nil
	}
	return "", fmt.Errorf("no more free IP addresses in range [%s, %s]", start, end)
}

// familyName returns a human-readable name for an IP family.
func familyName(isV6 bool) string {
	if isV6 {
		return "IPv6"
	}
	return "IPv4"
}

// bounds defines the inclusive [ipFirst, ipLast] allocation range together with
// a random starting offset within it.
type bounds struct {
	ipFirst netip.Addr
	ipLast  netip.Addr
	offset  uint64
}

// randomizedBounds builds allocation bounds over the inclusive [first, last] range
// with a random starting offset to spread allocations across the range.
func randomizedBounds(first, last netip.Addr) (bounds, error) {
	rangeSize, err := addressRangeSize(first, last)
	if err != nil {
		return bounds{}, err
	}

	var offset uint64
	if rangeSize >= math.MaxInt64 {
		offset = rand.Uint64()
	} else {
		offset = uint64(rand.Int63n(int64(rangeSize)))
	}

	return bounds{
		ipFirst: first,
		ipLast:  last,
		offset:  offset,
	}, nil
}

// addressRangeSize returns the number of addresses in the inclusive [first, last]
// range, capped at math.MaxInt64 to avoid overflow for large IPv6 ranges.
func addressRangeSize(first, last netip.Addr) (uint64, error) {
	firstBig := big.NewInt(0).SetBytes(first.AsSlice())
	lastBig := big.NewInt(0).SetBytes(last.AsSlice())
	diff := big.NewInt(0).Sub(lastBig, firstBig)
	diff.Add(diff, big.NewInt(1)) // inclusive count
	if diff.Sign() <= 0 {
		return 0, fmt.Errorf("no available addresses in range [%s, %s]", first, last)
	}
	if diff.Cmp(big.NewInt(math.MaxInt64)) >= 0 {
		return math.MaxInt64, nil
	}
	return diff.Uint64(), nil
}

// ipIterator allows to iterate over all the IP addresses
// in a range defined by the start and last address in bounds.
func ipIterator(b bounds) func() netip.Addr {
	modulo := func(addr netip.Addr) netip.Addr {
		if addr.Compare(b.ipLast) == 1 {
			return b.ipFirst
		}
		return addr
	}
	next := func(addr netip.Addr) netip.Addr {
		return modulo(addr.Next())
	}
	start, err := addOffsetAddress(b.ipFirst, b.offset)
	if err != nil {
		return func() netip.Addr { return netip.Addr{} }
	}
	start = modulo(start)
	ip := start
	seen := false
	return func() netip.Addr {
		value := ip
		if value == start {
			if seen {
				return netip.Addr{}
			}
			seen = true
		}
		ip = next(ip)
		return value
	}
}

// broadcastAddress returns the broadcast address (last address) of the given CIDR subnet.
func broadcastAddress(subnet netip.Prefix) (netip.Addr, error) {
	base := subnet.Masked().Addr()
	bytes := base.AsSlice()
	n := 8*len(bytes) - subnet.Bits()
	for i := len(bytes) - 1; i >= 0 && n > 0; i-- {
		if n >= 8 {
			bytes[i] = 0xff
			n -= 8
		} else {
			mask := ^uint8(0) >> (8 - n)
			bytes[i] |= mask
			break
		}
	}
	addr, ok := netip.AddrFromSlice(bytes)
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid address %v", bytes)
	}
	return addr, nil
}

// addOffsetAddress returns the IP address obtained by adding a numeric offset to the given address.
func addOffsetAddress(address netip.Addr, offset uint64) (netip.Addr, error) {
	addressBytes := address.AsSlice()
	addressBig := big.NewInt(0).SetBytes(addressBytes)
	r := big.NewInt(0).Add(addressBig, new(big.Int).SetUint64(offset)).Bytes()
	lenDiff := len(addressBytes) - len(r)
	if lenDiff > 0 {
		r = append(make([]byte, lenDiff), r...)
	} else if lenDiff < 0 {
		return netip.Addr{}, fmt.Errorf("invalid address %v", r)
	}
	addr, ok := netip.AddrFromSlice(r)
	if !ok {
		return netip.Addr{}, fmt.Errorf("invalid address %v", r)
	}
	return addr, nil
}
