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
	"math"
	"math/big"
	"math/rand"
	"net/netip"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// based on existing Kindnet IP allocator:
// https://github.com/kubernetes-sigs/kindnet/blob/main/cmd/cni-kindnet/netconf.go

type LocalIPAM struct {
	podConfigStore *PodConfigStore
}

func newLocalIPAM(store *PodConfigStore) *LocalIPAM {
	return &LocalIPAM{podConfigStore: store}
}

// Allocate allocates a free IP address from the given CIDR prefix.
func (ipam *LocalIPAM) Allocate(podUID types.UID, cidr netip.Prefix) (netip.Addr, error) {
	allocatedIPs := ipam.podConfigStore.GetAllocatedIPs()
	bounds, err := calculateBounds(cidr)
	if err != nil {
		return netip.Addr{}, err
	}

	var allocatedIP netip.Addr
	iterator := iPIterator(bounds)
	for {
		ip := iterator()
		if !ip.IsValid() {
			break
		}
		// Check if IP is already allocated
		if _, exists := allocatedIPs[ip]; exists {
			continue
		}
		allocatedIP = ip
		klog.V(2).Infof("Successfully allocated IP %s for pod %s from CIDR %s", allocatedIP.String(), podUID, cidr.String())
		return allocatedIP, nil
	}
	return netip.Addr{}, fmt.Errorf("no more free IP addresses in range %s", cidr.String())
}

// bounds defines computed static boundary bounds for IP allocation
type bounds struct {
	ipFirst netip.Addr
	ipLast  netip.Addr
	offset  uint64
}

func calculateBounds(cidr netip.Prefix) (bounds, error) {
	var size uint64
	hostsBits := cidr.Addr().BitLen() - cidr.Bits()
	if hostsBits >= 64 {
		size = math.MaxInt64
	} else {
		size = uint64(1) << uint(hostsBits)
	}
	size-- // skip network address

	reserved := 6
	if size <= 64 {
		reserved = 2
	} else if size <= 128 {
		reserved = 4
	}

	ipFirst := cidr.Masked().Addr().Next()
	ipFirst, err := addOffsetAddress(ipFirst, uint64(reserved))
	if err != nil {
		return bounds{}, err
	}

	ipLast, err := broadcastAddress(cidr)
	if err != nil {
		return bounds{}, err
	}

	if size <= uint64(reserved) {
		return bounds{}, fmt.Errorf("no available addresses")
	}
	rangeSize := size - uint64(reserved)

	var offset uint64
	if rangeSize >= math.MaxInt64 {
		offset = rand.Uint64()
	} else {
		offset = uint64(rand.Int63n(int64(rangeSize)))
	}

	return bounds{
		ipFirst: ipFirst,
		ipLast:  ipLast,
		offset:  offset,
	}, nil
}

// iPIterator allows to iterate over all the IP addresses
// in a range defined by the start and last address in bounds.
func iPIterator(b bounds) func() netip.Addr {
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

func addOffsetAddress(address netip.Addr, offset uint64) (netip.Addr, error) {
	addressBytes := address.AsSlice()
	addressBig := big.NewInt(0).SetBytes(addressBytes)
	r := big.NewInt(0).Add(addressBig, big.NewInt(int64(offset))).Bytes()
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

