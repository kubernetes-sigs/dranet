package driver

import (
	"net/netip"
	"testing"

	"sigs.k8s.io/dranet/pkg/apis"
)

func TestAllocate(t *testing.T) {
	t.Run("successful allocation (IPv6)", func(t *testing.T) {
		store := mustNewPodConfigStore()
		ipam := newLocalIPAM(store)
		cidr := netip.MustParsePrefix("2001:db8::/64")

		ip, err := ipam.Allocate("pod-1", cidr)
		if err != nil {
			t.Fatalf("Allocate() unexpected error = %v", err)
		}
		if !cidr.Contains(ip) {
			t.Errorf("Allocate() returned IP %s which is not in prefix %s", ip, cidr)
		}
	})

	t.Run("collision avoidance - skips occupied IP (IPv6)", func(t *testing.T) {
		store := mustNewPodConfigStore()
		ipam := newLocalIPAM(store)
		cidr := netip.MustParsePrefix("2001:db8::/126")

		// IPv6 /126 has size = 3 (after network address skip).
		// reserved = 2 (IPs 2001:db8::1 and 2001:db8::2 are skipped).
		// rangeSize = 3 - 2 = 1.
		// The only available address is 2001:db8::3. We occupy it before allocation.

		err := store.SetDeviceConfig("pod-occupied", "eth0", DeviceConfig{
			NetworkInterfaceConfigInPod: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{"2001:db8::3"},
				},
			},
		})
		if err != nil {
			t.Fatalf("SetDeviceConfig() unexpected error = %v", err)
		}

		// Try to allocate. Since the only available IP is occupied, this should fail.
		_, err = ipam.Allocate("pod-new", cidr)
		if err == nil {
			t.Fatal("Allocate() expected error due to no free IPs, but got nil")
		}
	})

	t.Run("out of IPs error (IPv4)", func(t *testing.T) {
		store := mustNewPodConfigStore()
		ipam := newLocalIPAM(store)
		cidr := netip.MustParsePrefix("192.168.1.0/30") // only 192.168.1.3 is free

		// First allocation should succeed and return 192.168.1.3
		ip, err := ipam.Allocate("pod-1", cidr)
		if err != nil {
			t.Fatalf("First Allocate() unexpected error = %v", err)
		}
		if ip.String() != "192.168.1.3" {
			t.Errorf("Expected first IP to be 192.168.1.3, got %s", ip)
		}

		// Mock the IP registering as allocated
		err = store.SetDeviceConfig("pod-1", "eth0", DeviceConfig{
			NetworkInterfaceConfigInPod: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					Addresses: []string{ip.String()},
				},
			},
		})
		if err != nil {
			t.Fatalf("SetDeviceConfig() unexpected error = %v", err)
		}

		// Second allocation should fail
		_, err = ipam.Allocate("pod-2", cidr)
		if err == nil {
			t.Fatal("Second Allocate() expected error, but got nil")
		}
	})
}


func TestCalculateBounds(t *testing.T) {
	tests := []struct {
		name    string
		cidr    netip.Prefix
		wantErr bool
	}{
		{
			name:    "valid IPv6 /64 CIDR",
			cidr:    netip.MustParsePrefix("2001:db8::/64"),
			wantErr: false,
		},
		{
			name:    "valid IPv6 /120 CIDR (medium size)",
			cidr:    netip.MustParsePrefix("2001:db8::/120"),
			wantErr: false,
		},
		{
			name:    "valid IPv4 /30 CIDR",
			cidr:    netip.MustParsePrefix("192.168.1.0/30"),
			wantErr: false,
		},
		{
			name:    "too small IPv4 /31 CIDR",
			cidr:    netip.MustParsePrefix("192.168.1.0/31"),
			wantErr: true,
		},
		{
			name:    "too small IPv6 /127 CIDR",
			cidr:    netip.MustParsePrefix("2001:db8::/127"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := calculateBounds(tt.cidr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("calculateBounds() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if !got.ipFirst.IsValid() || !got.ipLast.IsValid() {
					t.Errorf("calculateBounds() returned invalid bounds: %+v", got)
				}
				if got.ipFirst.Compare(got.ipLast) == 1 {
					t.Errorf("calculateBounds() returned ipFirst %s > ipLast %s", got.ipFirst, got.ipLast)
				}
			}
		})
	}
}

func TestBroadcastAddress(t *testing.T) {
	tests := []struct {
		name    string
		subnet  netip.Prefix
		want    string
		wantErr bool
	}{
		{
			name:   "IPv4 /24 CIDR",
			subnet: netip.MustParsePrefix("192.168.1.0/24"),
			want:   "192.168.1.255",
		},
		{
			name:   "IPv4 /22 CIDR (non-byte aligned)",
			subnet: netip.MustParsePrefix("192.168.0.0/22"),
			want:   "192.168.3.255",
		},
		{
			name:   "IPv6 /64 CIDR",
			subnet: netip.MustParsePrefix("2001:db8::/64"),
			want:   "2001:db8::ffff:ffff:ffff:ffff",
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
			name:   "IPv4 add offset with carry",
			addr:   netip.MustParseAddr("0.0.0.1"),
			offset: 256,
			want:   "0.0.1.1",
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
	t.Run("normal iteration without offset", func(t *testing.T) {
		b := bounds{
			ipFirst: netip.MustParseAddr("192.168.1.1"),
			ipLast:  netip.MustParseAddr("192.168.1.3"),
			offset:  0,
		}
		it := iPIterator(b)
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

	t.Run("iteration with offset", func(t *testing.T) {
		b := bounds{
			ipFirst: netip.MustParseAddr("192.168.1.1"),
			ipLast:  netip.MustParseAddr("192.168.1.3"),
			offset:  1,
		}
		it := iPIterator(b)
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
		it := iPIterator(b)
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
		it := iPIterator(b)
		ip := it()
		if ip.IsValid() {
			t.Errorf("expected invalid IP, got %s", ip)
		}
	})
}
