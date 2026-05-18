package driver

import (
	"net/netip"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestAllocateIPsForPods(t *testing.T) {
	tests := []struct {
		name       string
		ipRange    string
		podUIDs    []types.UID
		deviceName string
		wantErr    bool
		wantCount  int // Expected number of allocated IPs
	}{
		{
			name:       "Valid IPv6 allocation for single pod",
			ipRange:    "2001:db8::/64",
			podUIDs:    []types.UID{"pod-1"},
			deviceName: "device-1",
			wantErr:    false,
			wantCount:  1,
		},
		{
			name:       "Valid IPv4 allocation for multiple pods",
			ipRange:    "192.168.1.0/24",
			podUIDs:    []types.UID{"pod-2", "pod-3"},
			deviceName: "device-2",
			wantErr:    false,
			wantCount:  2,
		},
		{
			name:       "Empty IP range",
			ipRange:    "",
			podUIDs:    []types.UID{"pod-4"},
			deviceName: "device-3",
			wantErr:    true,
			wantCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize the driver with a unique temporary DB to ensure test isolation.
			np := &NetworkDriver{
				dbPath: filepath.Join(t.TempDir(), "ipconf_test.db"),
			}

			// Execute the IP allocation logic for the given pods.
			podIPs, errors := np.allocateIPsForPods(tt.ipRange, tt.podUIDs, tt.deviceName)

			// Verify that the error status matches the expectation.
			if (len(errors) > 0) != tt.wantErr {
				t.Errorf("allocateIPsForPods() errors = %v, wantErr %v", errors, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify that the number of allocated IPs matches the expected count.
				if len(podIPs) != tt.wantCount {
					t.Errorf("Expected %d allocated IPs, got %d", tt.wantCount, len(podIPs))
				}

				// Parse the CIDR prefix to validate allocated IPs against it.
				cidr, _ := netip.ParsePrefix(tt.ipRange)
				seenIPs := make(map[string]bool)

				// Iterate through each pod to validate its allocated IP.
				for _, uid := range tt.podUIDs {
					ipStr, ok := podIPs[uid]
					if !ok {
						t.Errorf("Expected IP for pod %s, but not found", uid)
						continue
					}

					addr, err := netip.ParseAddr(ipStr)
					if err != nil {
						t.Errorf("Failed to parse allocated IP %s: %v", ipStr, err)
						continue
					}

					// Verify that the allocated IP falls within the expected range.
					if !cidr.Contains(addr) {
						t.Errorf("Allocated IP %s is not in range %s", ipStr, tt.ipRange)
					}

					// Verify isolation: different pods should get different IPs
					if seenIPs[ipStr] {
						t.Errorf("Duplicate IP allocated: %s", ipStr)
					}
					seenIPs[ipStr] = true
				}
			}
		})
	}
}
