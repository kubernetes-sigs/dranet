package main

import (
	"testing"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

func ifname(name string) string {
	return cniIfname(webhook.ProfileRequest{
		Device: cloudprovider.DeviceIdentifiers{Name: name},
	})
}

func TestCniIfname(t *testing.T) {
	tests := []struct {
		name    string
		devName string
		want    string // exact expected ifname ("" => assert invariants only)
	}{
		{
			name:    "short valid name passes through verbatim",
			devName: "eth0",
			want:    "eth0",
		},
		{
			name:    "15-char name is at the limit and passes through",
			devName: "abcdefghijklmno", // exactly 15
			want:    "abcdefghijklmno",
		},
		{
			name:    "PCI-derived 16-char name keeps the readable BDF (prefix stripped)",
			devName: "pci-0000-27-00-2", // 16 chars, over IFNAMSIZ
			want:    "0000-27-00-2",     // 12 chars, valid, still reads as PCI BDF
		},
		{
			name:    "longest PCI address still fits after stripping",
			devName: "pci-ffff-ff-1f-7",
			want:    "ffff-ff-1f-7",
		},
		{
			name:    "non-PCI over-length name falls back to deterministic hash",
			devName: "net-aaaaaaaaaaaaaaaaaaaa", // base32-style, no "pci-" prefix
			want:    "",                          // opaque hash; checked via invariants
		},
		{
			// Standard Linux PCI addresses never reach this, but a hypothetical
			// over-length PCI name (still >15 after stripping "pci-") must stay
			// correct by degrading to the hash rather than emitting a long name.
			name:    "over-length PCI name still degrades to hash, not an invalid name",
			devName: "pci-00000000-27-00-2", // >15 even after "pci-" is stripped
			want:    "",                      // opaque hash; checked via invariants
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ifname(tt.devName)

			if !isValidCNIIfname(got) {
				t.Fatalf("cniIfname(%q) = %q, which is not a valid CNI ifname", tt.devName, got)
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("cniIfname(%q) = %q, want %q", tt.devName, got, tt.want)
			}
			if tt.want == "" && got == tt.devName {
				t.Errorf("cniIfname(%q) returned the (invalid) name verbatim instead of deriving", tt.devName)
			}
		})
	}
}

// TestCniIfnameDeterministic is the property that actually protects against IP
// leaks: whereabouts matches release on (containerID, ifname), so the same
// device identifier MUST always yield the same CNI_IFNAME (ADD == DEL).
func TestCniIfnameDeterministic(t *testing.T) {
	const dev = "pci-0000-27-00-2"
	first := ifname(dev)
	for i := 0; i < 100; i++ {
		if got := ifname(dev); got != first {
			t.Fatalf("cniIfname(%q) not deterministic: %q != %q", dev, got, first)
		}
	}
}

// TestCniIfnameDistinct ensures different devices in the same claim (same
// containerID, distinct deviceName) do not collide on the derived ifname,
// which would otherwise alias their whereabouts reservations.
func TestCniIfnameDistinct(t *testing.T) {
	names := []string{
		"pci-0000-27-00-2",
		"pci-0000-27-00-3",
		"pci-0000-27-00-4",
		"pci-0000-3b-00-0",
	}
	seen := map[string]string{}
	for _, n := range names {
		got := ifname(n)
		if prev, dup := seen[got]; dup {
			t.Errorf("ifname collision: %q and %q both derive to %q", prev, n, got)
		}
		seen[got] = n
	}
}

func TestIsValidCNIIfname(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"eth0", true},
		{"abcdefghijklmno", true},  // 15 chars
		{"abcdefghijklmnop", false}, // 16 chars
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"a:b", false},
		{"a b", false},
		{"pci-0000-27-00-2", false}, // 16 chars
		{"dra1a2b3c4d", true},
	}
	for _, tt := range tests {
		if got := isValidCNIIfname(tt.in); got != tt.want {
			t.Errorf("isValidCNIIfname(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
