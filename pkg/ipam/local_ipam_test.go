package ipam

import (
	"net/netip"
	"sync"
	"testing"

	"sigs.k8s.io/dranet/pkg/apis"
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

func TestResolveRange(t *testing.T) {
	tests := []struct {
		name      string
		cfg       apis.IPRangeConfig
		wantErr   bool
		wantStart string
		wantEnd   string
	}{
		{
			name:      "CIDR only (IPv4 /24)",
			cfg:       apis.IPRangeConfig{CIDR: "192.168.1.0/24"},
			wantStart: "192.168.1.1",
			wantEnd:   "192.168.1.254",
		},
		{
			name:      "CIDR only (IPv6 /64)",
			cfg:       apis.IPRangeConfig{CIDR: "2001:db8::/64"},
			wantStart: "2001:db8::1",
			wantEnd:   "2001:db8::ffff:ffff:ffff:fffe",
		},
		{
			name:      "explicit start/end only",
			cfg:       apis.IPRangeConfig{StartIP: "10.0.0.5", EndIP: "10.0.0.10"},
			wantStart: "10.0.0.5",
			wantEnd:   "10.0.0.10",
		},
		{
			name:      "all three: start/end take priority and are within CIDR",
			cfg:       apis.IPRangeConfig{CIDR: "10.0.0.0/24", StartIP: "10.0.0.5", EndIP: "10.0.0.10"},
			wantStart: "10.0.0.5",
			wantEnd:   "10.0.0.10",
		},
		{
			name:    "CIDR too small to have allocatable addresses (/31)",
			cfg:     apis.IPRangeConfig{CIDR: "192.168.1.0/31"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := resolveRange(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveRange() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if start.String() != tt.wantStart {
				t.Errorf("resolveRange() start = %s, want %s", start, tt.wantStart)
			}
			if end.String() != tt.wantEnd {
				t.Errorf("resolveRange() end = %s, want %s", end, tt.wantEnd)
			}
		})
	}
}

func TestAllocate(t *testing.T) {
	t.Run("single family: one IP even with multiple ranges", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []apis.IPRangeConfig{
			{CIDR: "10.0.0.0/29"},
			{CIDR: "10.0.1.0/29"},
		}
		ips, err := ipam.Allocate(ranges)
		if err != nil {
			t.Fatalf("Allocate() unexpected error = %v", err)
		}
		if len(ips) != 1 {
			t.Fatalf("Allocate() returned %d addresses, want 1: %v", len(ips), ips)
		}
		// The address must come from the first range; the second is skipped.
		first := netip.MustParsePrefix("10.0.0.0/29")
		got := netip.MustParsePrefix(ips[0])
		if got.Bits() != 32 || !first.Contains(got.Addr()) {
			t.Errorf("Allocate() = %v, want a /32 within 10.0.0.0/29", ips)
		}
	})

	t.Run("dual stack: one IP per family", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []apis.IPRangeConfig{
			{CIDR: "10.0.0.0/29"},
			{CIDR: "2001:db8::/120"},
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

	t.Run("fallback to next range of same family when exhausted", func(t *testing.T) {
		ipam := NewLocalIPAM([]string{"10.0.0.5/32"}) // occupy the only address of rangeA
		ranges := []apis.IPRangeConfig{
			{StartIP: "10.0.0.5", EndIP: "10.0.0.5"},
			{StartIP: "10.0.0.6", EndIP: "10.0.0.6"},
		}
		ips, err := ipam.Allocate(ranges)
		if err != nil {
			t.Fatalf("Allocate() unexpected error = %v", err)
		}
		if len(ips) != 1 || ips[0] != "10.0.0.6/32" {
			t.Errorf("Allocate() = %v, want [10.0.0.6/32]", ips)
		}
	})

	t.Run("error when all ranges of a family are exhausted", func(t *testing.T) {
		ipam := NewLocalIPAM([]string{"10.0.0.5/32"})
		ranges := []apis.IPRangeConfig{
			{StartIP: "10.0.0.5", EndIP: "10.0.0.5"},
			{StartIP: "10.0.0.5", EndIP: "10.0.0.5"},
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error for exhausted family, got nil")
		}
	})

	t.Run("no leak: addresses released when a later range fails", func(t *testing.T) {
		ipam := NewLocalIPAM(nil)
		ranges := []apis.IPRangeConfig{
			{StartIP: "10.0.0.5", EndIP: "10.0.0.5"}, // succeeds first
			{CIDR: "not-a-cidr"},                     // then hard error
		}
		if _, err := ipam.Allocate(ranges); err == nil {
			t.Fatal("Allocate() expected error, got nil")
		}
		// The previously allocated 10.0.0.5 must have been released, so a fresh
		// allocation of the same address now succeeds.
		ips, err := ipam.Allocate([]apis.IPRangeConfig{{StartIP: "10.0.0.5", EndIP: "10.0.0.5"}})
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
		ranges := []apis.IPRangeConfig{{StartIP: "10.0.0.1", EndIP: "10.0.0.5"}}

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
		ranges := []apis.IPRangeConfig{{StartIP: "10.0.0.1", EndIP: "10.0.0.10"}}
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

func TestBroadcastAddress(t *testing.T) {
	tests := []struct {
		name    string
		subnet  netip.Prefix
		want    string
		wantErr bool
	}{
		{
			name:   "IPv6 /64 CIDR",
			subnet: netip.MustParsePrefix("2001:db8::/64"),
			want:   "2001:db8::ffff:ffff:ffff:ffff",
		},
		{
			name:   "IPv4 /22 CIDR",
			subnet: netip.MustParsePrefix("192.168.0.0/22"),
			want:   "192.168.3.255",
		},
		{
			name:    "invalid zero prefix",
			subnet:  netip.Prefix{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := broadcastAddress(tt.subnet)
			if (err != nil) != tt.wantErr {
				t.Fatalf("broadcastAddress() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.String() != tt.want {
				t.Errorf("broadcastAddress() = %s, want %s", got.String(), tt.want)
			}
		})
	}
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
