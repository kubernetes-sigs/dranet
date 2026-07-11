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

// IPRange is an inclusive [Start, End] IP allocation range supplied by a caller
// (typically a cloud provider). Both bounds must be valid addresses of the same
// IP family with Start <= End.
type IPRange struct {
	Start netip.Addr
	End   netip.Addr
}

// Allocate allocates one IP address for each range provided. It returns the
// allocated addresses formatted as host CIDRs (IPv4 first).
// At most one range per IP family may be supplied. If any range is invalid or
// cannot yield a free address, every address already allocated by this call is
// released and an error is returned.
func (ipam *LocalIPAM) Allocate(ranges []IPRange) ([]string, error) {
	var allocatedV4, allocatedV6 string
	releaseAll := func() {
		for _, ip := range []string{allocatedV4, allocatedV6} {
			if ip != "" {
				ipam.Release(ip)
			}
		}
	}

	for _, r := range ranges {
		start, end := r.Start, r.End
		if !start.IsValid() || !end.IsValid() || start.Is6() != end.Is6() || start.Compare(end) > 0 {
			releaseAll()
			return nil, fmt.Errorf("invalid ip range [%s, %s]", start, end)
		}

		familyName := "ipv4"
		if start.Is6() {
			familyName = "ipv6"
		}
		if (start.Is6() && allocatedV6 != "") || (start.Is4() && allocatedV4 != "") {
			releaseAll()
			return nil, fmt.Errorf("more than one %s range provided; at most one range per IP family is allowed", familyName)
		}

		ip, err := ipam.allocateForRange(start, end)
		if err != nil {
			releaseAll()
			return nil, fmt.Errorf("no available IP address for %s in range [%s, %s]: %w", familyName, start, end, err)
		}
		if start.Is6() {
			allocatedV6 = ip
		} else {
			allocatedV4 = ip
		}
	}

	var assigned []string
	if allocatedV4 != "" {
		assigned = append(assigned, allocatedV4)
	}
	if allocatedV6 != "" {
		assigned = append(assigned, allocatedV6)
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

// Reserve records the given addresses as in-use to avoid allocation conflict.
// Reserve is atomic: if any address is invalid or already in use, nothing is
// recorded and an error is returned.
func (ipam *LocalIPAM) Reserve(addrs []string) error {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	parsed := make([]netip.Addr, 0, len(addrs))
	for _, addrStr := range addrs {
		prefix, err := netip.ParsePrefix(addrStr)
		if err != nil {
			return fmt.Errorf("failed to parse address %q: %w", addrStr, err)
		}
		ip := prefix.Addr()
		if _, exists := ipam.allocatedIPs[ip]; exists {
			return fmt.Errorf("address %q is already in use", addrStr)
		}
		parsed = append(parsed, ip)
	}
	for _, ip := range parsed {
		ipam.allocatedIPs[ip] = struct{}{}
		klog.V(2).Infof("Successfully reserved in-use IP %s", ip)
	}
	return nil
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
