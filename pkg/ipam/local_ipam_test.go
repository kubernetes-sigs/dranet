package ipam

import (
	"net/netip"
	"sync"
	"testing"
)

func TestAllocateForRange(t *testing.T) {
	t.Run("IPv6 range returns a /128 host address", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		start := netip.MustParseAddr("2001:db8::1")
		end := netip.MustParseAddr("2001:db8::5")

		ip, err := ipam.allocateForRange(start, end)
		if err != nil {
			t.Fatalf("allocateForRange() unexpected error = %v", err)
		}
		parsedIP, err := netip.ParsePrefix(ip)
		if err != nil {
			t.Fatalf("failed to parse returned IP %q: %v", ip, err)
		}
		if parsedIP.Bits() != 128 {
			t.Errorf("allocateForRange() returned IP %s, want a /128 host address", ip)
		}
		if parsedIP.Addr().Compare(start) < 0 || parsedIP.Addr().Compare(end) > 0 {
			t.Errorf("allocateForRange() returned IP %s outside range [%s, %s]", ip, start, end)
		}
	})

	t.Run("IPv4 range returns a /32 host address", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		start := netip.MustParseAddr("10.0.0.5")
		end := netip.MustParseAddr("10.0.0.10")

		ip, err := ipam.allocateForRange(start, end)
		if err != nil {
			t.Fatalf("allocateForRange() unexpected error = %v", err)
		}
		parsedIP, err := netip.ParsePrefix(ip)
		if err != nil {
			t.Fatalf("failed to parse returned IP %q: %v", ip, err)
		}
		if parsedIP.Bits() != 32 {
			t.Errorf("allocateForRange() returned IP %s, want a /32 host address", ip)
		}
		if parsedIP.Addr().Compare(start) < 0 || parsedIP.Addr().Compare(end) > 0 {
			t.Errorf("allocateForRange() returned IP %s outside range [%s, %s]", ip, start, end)
		}
	})

	t.Run("collision avoidance - skips occupied IP", func(t *testing.T) {
		// Usable range 2001:db8::1 - 2001:db8::2; occupy ::1 so ::2 is the only free IP.
		ipam := NewLocalIPAM([]string{"2001:db8::1/128"})
		ip, err := ipam.allocateForRange(netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2"))
		if err != nil {
			t.Fatalf("allocateForRange() unexpected error = %v", err)
		}
		if ip != "2001:db8::2/128" {
			t.Errorf("allocateForRange() = %s, want 2001:db8::2/128 (the only free usable IP)", ip)
		}
	})

	t.Run("single-address range (start == end), then exhausted", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		addr := netip.MustParseAddr("10.0.0.5")

		ip, err := ipam.allocateForRange(addr, addr)
		if err != nil {
			t.Fatalf("allocateForRange() unexpected error = %v", err)
		}
		if ip != "10.0.0.5/32" {
			t.Errorf("allocateForRange() = %s, want 10.0.0.5/32", ip)
		}
		// The single address is now taken; a second allocation must fail.
		if _, err := ipam.allocateForRange(addr, addr); err == nil {
			t.Fatal("second allocateForRange() expected error, but got nil")
		}
	})

	t.Run("invalid range: start after end", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		_, err := ipam.allocateForRange(netip.MustParseAddr("10.0.0.10"), netip.MustParseAddr("10.0.0.5"))
		if err == nil {
			t.Fatal("allocateForRange() expected error for start > end, got nil")
		}
	})
}

func TestAllocate(t *testing.T) {
	t.Run("single range yields one host IP", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.6")},
		}
		ips, err := ipam.Allocate(ranges)
		if err != nil {
			t.Fatalf("Allocate() unexpected error = %v", err)
		}
		if len(ips) != 1 {
			t.Fatalf("Allocate() returned %d addresses, want 1: %v", len(ips), ips)
		}
		within := netip.MustParsePrefix("10.0.0.0/29")
		got := netip.MustParsePrefix(ips[0])
		if got.Bits() != 32 || !within.Contains(got.Addr()) {
			t.Errorf("Allocate() = %v, want a /32 within 10.0.0.0/29", ips)
		}
	})

	t.Run("dual stack: one IP per family", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.6")},
			{Start: netip.MustParseAddr("2001:db8::1"), End: netip.MustParseAddr("2001:db8::fe")},
		}
		ips, err := ipam.Allocate(ranges)
		if err != nil {
			t.Fatalf("Allocate() unexpected error = %v", err)
		}
		if len(ips) != 2 {
			t.Fatalf("Allocate() returned %d addresses, want 2: %v", len(ips), ips)
		}
		var haveV4, haveV6 bool
		for _, ip := range ips {
			p := netip.MustParsePrefix(ip)
			if p.Addr().Is6() {
				haveV6 = true
			} else {
				haveV4 = true
			}
		}
		if !haveV4 || !haveV6 {
			t.Errorf("Allocate() = %v, want one IPv4 and one IPv6 address", ips)
		}
	})

	t.Run("rejects two ranges of the same family", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.6")},
			{Start: netip.MustParseAddr("10.0.1.1"), End: netip.MustParseAddr("10.0.1.6")},
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error for two ranges of the same family, got nil")
		}
		// The first range's address must have been rolled back, leaving nothing allocated.
		ips, err := ipam.Allocate([]IPRange{{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.1")}})
		if err != nil || len(ips) != 1 || ips[0] != "10.0.0.1/32" {
			t.Errorf("re-Allocate after rejected dup-family = (%v, %v), want [10.0.0.1/32], nil (proving rollback)", ips, err)
		}
	})

	t.Run("range exhausted yields error", func(t *testing.T) {
		// Occupy the only address in a single-address range.
		ipam := NewLocalIPAM([]string{"10.0.0.5/32"})
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.5"), End: netip.MustParseAddr("10.0.0.5")},
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error for exhausted range, got nil")
		}
	})

	t.Run("invalid range rejected", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.10"), End: netip.MustParseAddr("10.0.0.5")}, // start > end
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error for invalid range, got nil")
		}
	})

	t.Run("no leak: addresses released when a later range fails", func(t *testing.T) {
		// IPv4 succeeds, then the IPv6 range is exhausted -> the whole call fails.
		ipam := NewLocalIPAM([]string{"2001:db8::1/128"}) // occupy the only IPv6 address
		ranges := []IPRange{
			{Start: netip.MustParseAddr("10.0.0.5"), End: netip.MustParseAddr("10.0.0.5")},       // succeeds
			{Start: netip.MustParseAddr("2001:db8::1"), End: netip.MustParseAddr("2001:db8::1")}, // exhausted -> error
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error, got nil")
		}
		// The previously allocated 10.0.0.5 must have been released.
		ips, err := ipam.Allocate([]IPRange{{Start: netip.MustParseAddr("10.0.0.5"), End: netip.MustParseAddr("10.0.0.5")}})
		if err != nil {
			t.Fatalf("Allocate() after cleanup unexpected error = %v", err)
		}
		if len(ips) != 1 || ips[0] != "10.0.0.5/32" {
			t.Errorf("Allocate() = %v, want [10.0.0.5/32] (proving prior alloc was released)", ips)
		}
	})
}

// TestAllocateConcurrent is a stress test for the concurrency-safety of Allocate.
// The allocator's whole purpose is to never hand the same address to two callers,
// so these tests exercise that under simultaneous access. Run with -race to also
// catch data races on the shared state.
func TestAllocateConcurrent(t *testing.T) {
	t.Run("two pods contending for the same address: one succeeds, the other fails", func(t *testing.T) {
		// Seed every address in [10.0.0.1, 10.0.0.5] except 10.0.0.3, so both pods
		// must converge on 10.0.0.3.
		initial := []string{
			"10.0.0.1/32", "10.0.0.2/32", "10.0.0.4/32", "10.0.0.5/32",
		}
		ipam := NewLocalIPAM(initial)
		ranges := []IPRange{{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.5")}}

		var wg sync.WaitGroup
		results := make([][]string, 2)
		errs := make([]error, 2)

		// Line both goroutines up on a barrier so they race to allocate together.
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = ipam.Allocate(ranges)
			}(i)
		}
		close(start)
		wg.Wait()

		// Exactly one pod gets 10.0.0.3; the other must fail because it was the only
		// free address and it gets skipped once taken.
		succeeded, failed := 0, 0
		for i := 0; i < 2; i++ {
			if errs[i] == nil {
				succeeded++
				if len(results[i]) != 1 || results[i][0] != "10.0.0.3/32" {
					t.Errorf("pod %d: successful Allocate() = %v, want [10.0.0.3/32]", i, results[i])
				}
			} else {
				failed++
			}
		}
		if succeeded != 1 || failed != 1 {
			t.Errorf("got %d successful and %d failed allocations, want exactly 1 each (double-allocation or a lost skip)", succeeded, failed)
		}
	})

	t.Run("50 concurrent allocations on a 10-address range: only 10 succeeds, exhausting the range", func(t *testing.T) {
		// A range with exactly 10 allocatable addresses ([.1, .10]).
		ranges := []IPRange{{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.10")}}
		const capacity = 10
		const workers = 50

		ipam := NewLocalIPAM(nil)

		var wg sync.WaitGroup
		var mu sync.Mutex
		counts := make(map[string]int) // allocated IP -> number of times handed out
		successes := 0

		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ips, err := ipam.Allocate(ranges)
				if err != nil {
					return // expected once the range is exhausted
				}
				mu.Lock()
				defer mu.Unlock()
				successes++
				for _, ip := range ips {
					counts[ip]++
				}
			}()
		}
		close(start)
		wg.Wait()

		if successes != capacity {
			t.Errorf("got %d successful allocations, want %d", successes, capacity)
		}
		if len(counts) != capacity {
			t.Errorf("got %d unique IPs, want %d", len(counts), capacity)
		}
		for ip, n := range counts {
			if n != 1 {
				t.Errorf("IP %q was handed out %d times, want exactly 1 (double-allocation under concurrency)", ip, n)
			}
		}
	})
}

func TestAddOffsetAddress(t *testing.T) {
	tests := []struct {
		name    string
		addr    netip.Addr
		offset  uint64
		want    string
		wantErr bool
	}{
		{
			name:   "IPv4 add offset",
			addr:   netip.MustParseAddr("192.168.1.1"),
			offset: 10,
			want:   "192.168.1.11",
		},
		{
			name:    "IPv4 add offset overflow",
			addr:    netip.MustParseAddr("255.255.255.255"),
			offset:  1,
			wantErr: true,
		},
		{
			name:    "invalid zero address",
			addr:    netip.Addr{},
			offset:  5,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := addOffsetAddress(tt.addr, tt.offset)
			if (err != nil) != tt.wantErr {
				t.Fatalf("addOffsetAddress() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.String() != tt.want {
				t.Errorf("addOffsetAddress() = %s, want %s", got.String(), tt.want)
			}
		})
	}
}

func TestIPIterator(t *testing.T) {
	t.Run("iteration with offset", func(t *testing.T) {
		b := bounds{
			ipFirst: netip.MustParseAddr("192.168.1.1"),
			ipLast:  netip.MustParseAddr("192.168.1.3"),
			offset:  1,
		}
		it := ipIterator(b)
		var ips []string
		for {
			ip := it()
			if !ip.IsValid() {
				break
			}
			ips = append(ips, ip.String())
		}
		expected := []string{"192.168.1.2", "192.168.1.3", "192.168.1.1"}
		if len(ips) != len(expected) {
			t.Fatalf("expected %d IPs, got %d: %v", len(expected), len(ips), ips)
		}
		for i, v := range expected {
			if ips[i] != v {
				t.Errorf("at index %d: expected %s, got %s", i, v, ips[i])
			}
		}
	})

	t.Run("iteration with offset larger than range wrapping to ipFirst", func(t *testing.T) {
		b := bounds{
			ipFirst: netip.MustParseAddr("192.168.1.1"),
			ipLast:  netip.MustParseAddr("192.168.1.3"),
			offset:  10, // rangeSize is 2. offset 10 > 2. modulo(start) will wrap to ipFirst.
		}
		it := ipIterator(b)
		var ips []string
		for {
			ip := it()
			if !ip.IsValid() {
				break
			}
			ips = append(ips, ip.String())
		}
		expected := []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"}
		if len(ips) != len(expected) {
			t.Fatalf("expected %d IPs, got %d: %v", len(expected), len(ips), ips)
		}
		for i, v := range expected {
			if ips[i] != v {
				t.Errorf("at index %d: expected %s, got %s", i, v, ips[i])
			}
		}
	})

	t.Run("iteration error path with invalid bounds", func(t *testing.T) {
		b := bounds{
			ipFirst: netip.Addr{},
			offset:  5,
		}
		it := ipIterator(b)
		ip := it()
		if ip.IsValid() {
			t.Errorf("expected invalid IP, got %s", ip)
		}
	})
}

func TestRelease(t *testing.T) {
	addr := netip.MustParseAddr("192.168.1.10")
	initialIPs := []string{
		"192.168.1.10/24",
	}
	ipam := NewLocalIPAM(initialIPs)

	ipam.Release("192.168.1.10/24")

	if _, exists := ipam.allocatedIPs[addr]; exists {
		t.Errorf("Expected address %s to be released from in-memory map", addr)
	}
}

func TestReserve(t *testing.T) {
	t.Run("records addresses so Allocate avoids them", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		if err := ipam.Reserve([]string{"10.0.0.5/32"}); err != nil {
			t.Fatalf("Reserve() unexpected error = %v", err)
		}
		// 10.0.0.5 is now the only address in the range and it is reserved.
		if _, err := ipam.allocateForRange(netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("10.0.0.5")); err == nil {
			t.Fatalf("allocateForRange() = nil error, want exhaustion error for reserved address")
		}
	})

	t.Run("already in-use address returns error", func(t *testing.T) {
		ipam := NewLocalIPAM([]string{"10.0.0.5/32"})
		if err := ipam.Reserve([]string{"10.0.0.5/32"}); err == nil {
			t.Fatalf("Reserve() = nil error, want already-in-use error")
		}
	})

	t.Run("invalid address returns error", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		if err := ipam.Reserve([]string{"not-an-ip"}); err == nil {
			t.Fatalf("Reserve() = nil error, want parse error")
		}
	})

	t.Run("atomic: nothing recorded when one address is invalid", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		if err := ipam.Reserve([]string{"10.0.0.5/32", "not-an-ip"}); err == nil {
			t.Fatalf("Reserve() = nil error, want error")
		}
		// The valid address must be free (atomic rollback), so a re-reserve succeeds.
		if err := ipam.Reserve([]string{"10.0.0.5/32"}); err != nil {
			t.Fatalf("Reserve() after failed atomic call error = %v, want nil", err)
		}
	})
}
