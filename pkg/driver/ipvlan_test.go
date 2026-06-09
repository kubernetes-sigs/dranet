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
	"net"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/vishvananda/netlink"
)

func TestDeriveIPv6ForPod(t *testing.T) {
	tests := []struct {
		name      string
		prefix    [12]byte
		prefixLen int
		podIPv4   net.IP
		wantIP    string
		wantErr   bool
	}{
		{
			name: "standard derivation",
			prefix: [12]byte{
				0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
			prefixLen: 64,
			podIPv4:   net.ParseIP("10.0.1.5"),
			wantIP:    "fd00::a00:105",
			wantErr:   false,
		},
		{
			name: "real-world HPN address",
			prefix: [12]byte{
				0x20, 0x01, 0x0d, 0xb8, 0x00, 0x01, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
			prefixLen: 64,
			podIPv4:   net.ParseIP("172.16.0.100"),
			wantIP:    "2001:db8:1::ac10:64",
			wantErr:   false,
		},
		{
			name: "pod IP 192.168.1.1",
			prefix: [12]byte{
				0xfe, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
			prefixLen: 96,
			podIPv4:   net.ParseIP("192.168.1.1"),
			wantIP:    "fe80::c0a8:101",
			wantErr:   false,
		},
		{
			name: "nil IPv4",
			prefix: [12]byte{
				0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
			prefixLen: 64,
			podIPv4:   nil,
			wantErr:   true,
		},
		{
			name: "IPv6 passed as podIPv4",
			prefix: [12]byte{
				0xfd, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
			},
			prefixLen: 64,
			podIPv4:   net.ParseIP("::1"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, ipNet, err := deriveIPv6ForPod(tt.prefix, tt.prefixLen, tt.podIPv4)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ip.String() != tt.wantIP {
				t.Errorf("got IP %s, want %s", ip.String(), tt.wantIP)
			}
			prefixLen, _ := ipNet.Mask.Size()
			if prefixLen != tt.prefixLen {
				t.Errorf("got prefix length %d, want %d", prefixLen, tt.prefixLen)
			}
		})
	}
}

func TestExtractPodIPv4(t *testing.T) {
	tests := []struct {
		name   string
		ips    []string
		wantIP string
	}{
		{
			name:   "single IPv4",
			ips:    []string{"10.0.1.5"},
			wantIP: "10.0.1.5",
		},
		{
			name:   "IPv6 then IPv4",
			ips:    []string{"fd00::1", "10.0.1.5"},
			wantIP: "10.0.1.5",
		},
		{
			name:   "only IPv6",
			ips:    []string{"fd00::1"},
			wantIP: "",
		},
		{
			name:   "empty",
			ips:    nil,
			wantIP: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &api.PodSandbox{Ips: tt.ips}
			result := extractPodIPv4(pod)
			if tt.wantIP == "" {
				if result != nil {
					t.Errorf("expected nil, got %s", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil IP, got nil")
			}
			if result.String() != tt.wantIP {
				t.Errorf("got %s, want %s", result, tt.wantIP)
			}
		})
	}
}

func TestIpvlanTempName(t *testing.T) {
	tests := []struct {
		name       string
		parentName string
		want       string
	}{
		{"typical", "bond0", "bond0_iv"},
		{"exact 12 chars parent", "123456789012", "123456789012_iv"},
		{"13 chars parent truncated", "1234567890123", "123456789012_iv"},
		{"long parent", "very-long-parent-name", "very-long-pa_iv"},
		{"short", "eth0", "eth0_iv"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ipvlanTempName(tt.parentName)
			if got != tt.want {
				t.Errorf("ipvlanTempName(%q) = %q, want %q", tt.parentName, got, tt.want)
			}
			if len(got) > 15 {
				t.Errorf("temp name %q exceeds IFNAMSIZ (len=%d)", got, len(got))
			}
		})
	}
}

func TestParseIPVlanKernelMode(t *testing.T) {
	tests := []struct {
		input   string
		want    netlink.IPVlanMode
		wantErr bool
	}{
		{"", netlink.IPVLAN_MODE_L2, false},
		{"l2", netlink.IPVLAN_MODE_L2, false},
		{"l3", netlink.IPVLAN_MODE_L3, false},
		{"l3s", netlink.IPVLAN_MODE_L3S, false},
		{"ipv6", 0, true},
		{"invalid", 0, true},
	}
	for _, tt := range tests {
		t.Run("mode_"+tt.input, func(t *testing.T) {
			got, err := parseIPVlanKernelMode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseIPVlanKernelMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseIPVlanKernelMode(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseIPVlanKernelFlag(t *testing.T) {
	tests := []struct {
		input   string
		want    netlink.IPVlanFlag
		wantErr bool
	}{
		{"", netlink.IPVLAN_FLAG_BRIDGE, false},
		{"bridge", netlink.IPVLAN_FLAG_BRIDGE, false},
		{"private", netlink.IPVLAN_FLAG_PRIVATE, false},
		{"vepa", netlink.IPVLAN_FLAG_VEPA, false},
		{"invalid", 0, true},
	}
	for _, tt := range tests {
		t.Run("flag_"+tt.input, func(t *testing.T) {
			got, err := parseIPVlanKernelFlag(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseIPVlanKernelFlag(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseIPVlanKernelFlag(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
