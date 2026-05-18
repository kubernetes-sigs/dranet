// This file implements a stateful IPAM allocator backed by bbolt.
// The core allocation logic is adapted from the kindnet project:
// https://github.com/kubernetes-sigs/kindnet/blob/main/cmd/cni-kindnet/netconf.go
//
// This file also introduces GetIPFromRange to provide a simplified "one-key" allocation
// interface for the DRANET driver.

package driver

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"net/netip"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ipconfBucketName = []byte("ip_allocations")
)

// Allocator manages IP address allocation from a given CIDR range,
// persisting state in a bbolt database.
type Allocator struct {
	cidr     netip.Prefix
	ipFirst  netip.Addr
	ipLast   netip.Addr
	size     uint64
	reserved int
	db       *bolt.DB
}

// NewAllocator creates a new Allocator for the given CIDR and DB path.
func NewAllocator(cidr netip.Prefix, dbPath string) (*Allocator, error) {
	var size uint64
	hostsBits := cidr.Addr().BitLen() - cidr.Bits()
	if hostsBits >= 64 {
		size = math.MaxInt64
	} else {
		size = uint64(1) << uint(hostsBits)
	}
	size = size - 1 // skip network address

	reserved := 6
	if size <= 64 {
		reserved = 2
	} else if size <= 128 {
		reserved = 4
	}

	ipFirst := cidr.Masked().Addr().Next()
	ipFirst, err := addOffsetAddress(ipFirst, uint64(reserved))
	if err != nil {
		return nil, err
	}

	ipLast, err := broadcastAddress(cidr)
	if err != nil {
		return nil, err
	}

	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open ipconf db: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(ipconfBucketName)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize ipconf db bucket: %w", err)
	}

	return &Allocator{
		cidr:     cidr,
		size:     size,
		reserved: reserved,
		ipFirst:  ipFirst,
		ipLast:   ipLast,
		db:       db,
	}, nil
}

// Close closes the underlying database.
func (a *Allocator) Close() error {
	return a.db.Close()
}

func (a *Allocator) ipExists(tx *bolt.Tx, ip netip.Addr) bool {
	bucket := tx.Bucket(ipconfBucketName)
	if bucket == nil {
		return false
	}
	return bucket.Get([]byte(ip.String())) != nil
}

func (a *Allocator) ipInsert(tx *bolt.Tx, ip netip.Addr, id string) error {
	bucket := tx.Bucket(ipconfBucketName)
	if bucket == nil {
		return fmt.Errorf("bucket not found")
	}
	return bucket.Put([]byte(ip.String()), []byte(id))
}

// Allocate finds and allocates a free IP address for the given ID.
func (a *Allocator) Allocate(id string) (netip.Addr, error) {
	rangeSize := a.size - uint64(a.reserved)
	var offset uint64
	switch {
	case rangeSize >= math.MaxInt64:
		offset = rand.Uint64()
	case rangeSize == 0:
		return netip.Addr{}, fmt.Errorf("not available addresses")
	default:
		offset = uint64(rand.Int63n(int64(rangeSize)))
	}

	var allocatedIP netip.Addr
	err := a.db.Update(func(tx *bolt.Tx) error {
		iterator := ipIterator(a.ipFirst, a.ipLast, offset)
		for {
			ip := iterator()
			if !ip.IsValid() {
				break
			}
			if a.ipExists(tx, ip) {
				continue
			}
			err := a.ipInsert(tx, ip, id)
			if err != nil {
				return err
			}
			allocatedIP = ip
			return nil
		}
		return fmt.Errorf("allocator full")
	})

	if err != nil {
		return netip.Addr{}, err
	}
	return allocatedIP, nil
}

// ipIterator allows to iterate over all the IP addresses
// in a range defined by the start and last address.
func ipIterator(first netip.Addr, last netip.Addr, offset uint64) func() netip.Addr {
	modulo := func(addr netip.Addr) netip.Addr {
		if addr.Compare(last) == 1 {
			return first
		}
		return addr
	}
	next := func(addr netip.Addr) netip.Addr {
		return modulo(addr.Next())
	}
	start, err := addOffsetAddress(first, offset)
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

// GetIPFromRange is a wrapper function that achieves "one-key" IP allocation from a specified range.
func GetIPFromRange(cidrStr string, id string, dbPath string) (netip.Addr, error) {
	cidr, err := netip.ParsePrefix(cidrStr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid cidr %s: %w", cidrStr, err)
	}

	alloc, err := NewAllocator(cidr, dbPath)
	if err != nil {
		return netip.Addr{}, err
	}
	defer alloc.Close()

	return alloc.Allocate(id)
}
