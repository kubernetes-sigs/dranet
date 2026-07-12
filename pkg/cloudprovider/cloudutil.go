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

// This file holds generic helpers shared by the individual cloud provider
// implementations (gce, aws, azure, ...). Keep provider-specific logic in the
// respective provider package; only put broadly reusable utilities here.

package cloudprovider

import (
	"fmt"
	"math/big"
	"net/netip"
)

// IPRangeFromCIDR derives explicit [startIP, endIP] allocation boundaries from a
// CIDR block.
//
// The network (base) address and the broadcast (last) address are always
// excluded automatically. startReserved and endReserved specify how many
// additional addresses to reserve.
//
// It returns an error if the reserved counts are negative, the CIDR is invalid,
// or the block is too small to yield at least one allocatable address.
func IPRangeFromCIDR(cidr string, startReserved, endReserved int) (startIP, endIP string, err error) {
	if startReserved < 0 || endReserved < 0 {
		return "", "", fmt.Errorf("reserved counts must be non-negative (start=%d, end=%d)", startReserved, endReserved)
	}

	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", "", fmt.Errorf("invalid cidr %q: %w", cidr, err)
	}
	prefix = prefix.Masked()
	base := prefix.Addr()

	// size = number of addresses in the block = 2^hostBits.
	hostBits := base.BitLen() - prefix.Bits()
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))

	// Offset 0 is the network address and offset size-1 is the broadcast address;
	// both are always excluded. startReserved/endReserved reserve additional
	// addresses inward from each end.
	startOff := big.NewInt(int64(startReserved) + 1)
	endOff := new(big.Int).Sub(size, big.NewInt(int64(endReserved)+2))

	if startOff.Cmp(endOff) > 0 {
		return "", "", fmt.Errorf("cidr %q is too small to reserve %d start and %d end addresses (network and broadcast are also excluded)", cidr, startReserved, endReserved)
	}

	start, err := addOffset(base, startOff)
	if err != nil {
		return "", "", err
	}
	end, err := addOffset(base, endOff)
	if err != nil {
		return "", "", err
	}
	return start.String(), end.String(), nil
}

// addOffset returns addr + offset, staying within the address family of addr. It
// returns an error if the result overflows the address width.
func addOffset(addr netip.Addr, offset *big.Int) (netip.Addr, error) {
	sum := new(big.Int).Add(new(big.Int).SetBytes(addr.AsSlice()), offset)

	width := addr.BitLen() / 8 // 4 for IPv4, 16 for IPv6
	b := sum.Bytes()
	if len(b) > width {
		return netip.Addr{}, fmt.Errorf("address offset %s overflows the %d-bit space starting at %s", offset, addr.BitLen(), addr)
	}

	buf := make([]byte, width)
	copy(buf[width-len(b):], b)
	res, ok := netip.AddrFromSlice(buf)
	if !ok {
		return netip.Addr{}, fmt.Errorf("failed to construct address from bytes %v", buf)
	}
	return res, nil
}
