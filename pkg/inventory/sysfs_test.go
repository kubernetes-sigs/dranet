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

package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParsePCIAddress(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    *pciAddress
		wantErr bool
	}{
		{
			name:  "valid with domain",
			input: "0000:00:04.0",
			want: &pciAddress{
				domain:   "0000",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:  "valid without domain",
			input: "00:04.0",
			want: &pciAddress{
				domain:   "",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:    "invalid format",
			input:   "not-a-pci-address",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "embedded in string",
			input:   "pci-0000:8c:00.0-device",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePCIAddress(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("pciAddressFromString() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(pciAddress{})); diff != "" {
				t.Errorf("pciAddressFromString() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPCIAddressFromPath(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    *pciAddress
		wantErr bool
	}{
		{
			name:  "simple path",
			input: "/sys/devices/pci0000:00/0000:00:04.0/virtio1/net/eth0",
			want: &pciAddress{
				domain:   "0000",
				bus:      "00",
				device:   "04",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:  "hierarchical path",
			input: "/sys/devices/pci0000:8c/0000:8c:00.0/0000:8d:00.0/0000:8e:02.0/0000:91:00.0/net/eth3",
			want: &pciAddress{
				domain:   "0000",
				bus:      "91",
				device:   "00",
				function: "0",
			},
			wantErr: false,
		},
		{
			name:    "no pci address in path",
			input:   "/sys/devices/virtual/net/lo",
			wantErr: true,
		},
		{
			name:    "empty path",
			input:   "",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pciAddressFromPath(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("pciAddressFromPath() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(pciAddress{})); diff != "" {
				t.Errorf("pciAddressFromPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestIsSriovVf(t *testing.T) {
	syspath := t.TempDir()

	createDeviceDir := func(t *testing.T, ifName string) string {
		t.Helper()
		deviceDir := filepath.Join(syspath, ifName, "device")
		if err := os.MkdirAll(deviceDir, 0o755); err != nil {
			t.Fatalf("failed to create device directory for %q: %v", ifName, err)
		}
		return deviceDir
	}

	t.Run("missing physfn", func(t *testing.T) {
		createDeviceDir(t, "eth0")
		want := false
		got := isSriovVf("eth0", syspath)
		if want != got {
			t.Errorf("isSriovVf() mismatch (-want +got):\n- %t\n+ %t", want, got)
		}
	})

	t.Run("physfn symlink exists", func(t *testing.T) {
		deviceDir := createDeviceDir(t, "eth1")
		pfDir := filepath.Join(syspath, "pf0")
		if err := os.MkdirAll(pfDir, 0o755); err != nil {
			t.Fatalf("failed to create pf directory: %v", err)
		}
		if err := os.Symlink(pfDir, filepath.Join(deviceDir, "physfn")); err != nil {
			t.Fatalf("failed to create physfn symlink: %v", err)
		}
		want := true
		got := isSriovVf("eth1", syspath)
		if want != got {
			t.Errorf("isSriovVf() mismatch (-want +got):\n- %t\n+ %t", want, got)
		}
	})

	t.Run("physfn exists but is not symlink", func(t *testing.T) {
		deviceDir := createDeviceDir(t, "eth2")
		if err := os.WriteFile(filepath.Join(deviceDir, "physfn"), []byte("not-a-symlink"), 0o644); err != nil {
			t.Fatalf("failed to create physfn file: %v", err)
		}
		want := false
		got := isSriovVf("eth2", syspath)
		if want != got {
			t.Errorf("isSriovVf() mismatch (-want +got):\n- %t\n+ %t", want, got)
		}
	})
}

// TestIsLACPBond_NonBond asserts that a non-existent / non-bond interface
// is reported as not an LACP bond (the sysfs path is absent). The positive
// case (a real 802.3ad bond) is covered by e2e on bond nodes.
func TestIsLACPBond_NonBond(t *testing.T) {
	if IsLACPBond("dranet-definitely-not-a-bond-9999") {
		t.Errorf("expected non-bond interface to report false")
	}
}

func TestGetPFInterfaceNameFromSysfs(t *testing.T) {
	testCases := []struct {
		name        string
		vfName      string
		setupFunc   func(t *testing.T, baseDir string)
		want        string
		wantErr     bool
		errContains string
	}{
		{
			name:   "VF with single PF interface",
			vfName: "eth1",
			setupFunc: func(t *testing.T, baseDir string) {
				t.Helper()
				pfNetDir := filepath.Join(baseDir, "eth1", "device", "physfn", "net", "eth0")
				if err := os.MkdirAll(pfNetDir, 0o755); err != nil {
					t.Fatalf("failed to create PF net directory: %v", err)
				}
			},
			want:    "eth0",
			wantErr: false,
		},
		{
			name:   "not a VF - physfn directory missing",
			vfName: "eth2",
			setupFunc: func(t *testing.T, baseDir string) {
				t.Helper()
				deviceDir := filepath.Join(baseDir, "eth2", "device")
				if err := os.MkdirAll(deviceDir, 0o755); err != nil {
					t.Fatalf("failed to create device directory: %v", err)
				}
			},
			wantErr:     true,
			errContains: "failed to read PF net directory for VF eth2",
		},
		{
			name:   "physfn net directory exists but is empty",
			vfName: "eth3",
			setupFunc: func(t *testing.T, baseDir string) {
				t.Helper()
				pfNetDir := filepath.Join(baseDir, "eth3", "device", "physfn", "net")
				if err := os.MkdirAll(pfNetDir, 0o755); err != nil {
					t.Fatalf("failed to create PF net directory: %v", err)
				}
			},
			wantErr:     true,
			errContains: "no PF interface found for VF eth3",
		},
		{
			name:   "interface does not exist",
			vfName: "nonexistent",
			setupFunc: func(t *testing.T, baseDir string) {
				// Do not create anything
			},
			wantErr:     true,
			errContains: "failed to read PF net directory for VF nonexistent",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tc.setupFunc(t, tmpDir)

			got, err := getPFInterfaceNameFromSysfs(tmpDir, tc.vfName)

			if tc.wantErr {
				if err == nil {
					t.Errorf("getPFInterfaceNameFromSysfs() expected error, got nil")
					return
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("getPFInterfaceNameFromSysfs() error = %v, want error containing %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("getPFInterfaceNameFromSysfs() unexpected error: %v", err)
				return
			}
			if got != tc.want {
				t.Errorf("getPFInterfaceNameFromSysfs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetRdmaDeviceFromSysfs tests the getRdmaDeviceFromSysfs function
func TestGetRdmaDeviceFromSysfs(t *testing.T) {
	testCases := []struct {
		name        string
		ifName      string
		setupFunc   func(t *testing.T, baseDir string)
		want        string
		wantErr     bool
		errContains string
	}{
		{
			name:   "valid RDMA device found",
			ifName: "eth0",
			setupFunc: func(t *testing.T, baseDir string) {
				// Create mock sysfs structure: /sys/class/net/eth0/device/infiniband/mlx5_0
				rdmaDir := filepath.Join(baseDir, "eth0", "device", "infiniband", "mlx5_0")
				if err := os.MkdirAll(rdmaDir, 0755); err != nil {
					t.Fatalf("failed to create mock sysfs dir: %v", err)
				}
			},
			want:    "mlx5_0",
			wantErr: false,
		},
		{
			name:   "multiple RDMA devices returns first",
			ifName: "eth1",
			setupFunc: func(t *testing.T, baseDir string) {
				// Create mock sysfs structure with multiple RDMA devices
				for _, rdmaDev := range []string{"mlx5_0", "mlx5_1"} {
					rdmaDir := filepath.Join(baseDir, "eth1", "device", "infiniband", rdmaDev)
					if err := os.MkdirAll(rdmaDir, 0755); err != nil {
						t.Fatalf("failed to create mock sysfs dir: %v", err)
					}
				}
			},
			want:    "", // Returns first found, but order is not guaranteed
			wantErr: false,
		},
		{
			name:   "no RDMA device - infiniband dir missing",
			ifName: "eth2",
			setupFunc: func(t *testing.T, baseDir string) {
				// Create mock sysfs structure without infiniband dir
				deviceDir := filepath.Join(baseDir, "eth2", "device")
				if err := os.MkdirAll(deviceDir, 0755); err != nil {
					t.Fatalf("failed to create mock sysfs dir: %v", err)
				}
			},
			want:        "",
			wantErr:     true,
			errContains: "no RDMA device for eth2",
		},
		{
			name:   "no RDMA device - empty infiniband dir",
			ifName: "eth3",
			setupFunc: func(t *testing.T, baseDir string) {
				// Create mock sysfs structure with empty infiniband dir
				rdmaDir := filepath.Join(baseDir, "eth3", "device", "infiniband")
				if err := os.MkdirAll(rdmaDir, 0755); err != nil {
					t.Fatalf("failed to create mock sysfs dir: %v", err)
				}
			},
			want:        "",
			wantErr:     true,
			errContains: "no RDMA device found for eth3",
		},
		{
			name:   "interface does not exist",
			ifName: "nonexistent",
			setupFunc: func(t *testing.T, baseDir string) {
				// Don't create anything
			},
			want:        "",
			wantErr:     true,
			errContains: "no RDMA device for nonexistent",
		},
		{
			name:   "only files in infiniband dir, no directories",
			ifName: "eth4",
			setupFunc: func(t *testing.T, baseDir string) {
				// Create mock sysfs structure with only files (no directories)
				rdmaDir := filepath.Join(baseDir, "eth4", "device", "infiniband")
				if err := os.MkdirAll(rdmaDir, 0755); err != nil {
					t.Fatalf("failed to create mock sysfs dir: %v", err)
				}
				// Create a file instead of directory
				filePath := filepath.Join(rdmaDir, "somefile")
				if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
					t.Fatalf("failed to create mock file: %v", err)
				}
			},
			want:        "",
			wantErr:     true,
			errContains: "no RDMA device found for eth4",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create temporary directory to mock /sys/class/net
			tmpDir := t.TempDir()

			// Setup mock sysfs structure
			tc.setupFunc(t, tmpDir)

			// Call the sysfs fallback helper with the temp dir
			got, err := getRdmaDeviceFromSysfs(tmpDir, tc.ifName)

			// Check error conditions
			if tc.wantErr {
				if err == nil {
					t.Errorf("getRdmaDeviceFromSysfs() expected error, got nil")
					return
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("getRdmaDeviceFromSysfs() error = %v, want error containing %q", err, tc.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("getRdmaDeviceFromSysfs() unexpected error: %v", err)
				return
			}

			// For the "multiple RDMA devices" case, just check we got something valid
			if tc.name == "multiple RDMA devices returns first" {
				if got != "mlx5_0" && got != "mlx5_1" {
					t.Errorf("getRdmaDeviceFromSysfs() = %v, want mlx5_0 or mlx5_1", got)
				}
				return
			}

			if got != tc.want {
				t.Errorf("getRdmaDeviceFromSysfs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPCIAddressForRDMADevice(t *testing.T) {
	t.Run("resolves PCI address from device symlink", func(t *testing.T) {
		dir := t.TempDir()
		// Mimic: /sys/class/infiniband/erdma_0/device ->
		//        /sys/devices/pci0000:16/0000:16:01.0/0000:1b:00.0
		pciDir := filepath.Join(dir, "devices", "pci0000:16", "0000:16:01.0", "0000:1b:00.0")
		if err := os.MkdirAll(pciDir, 0o755); err != nil {
			t.Fatal(err)
		}
		rdmaDir := filepath.Join(dir, "erdma_0")
		if err := os.MkdirAll(rdmaDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(pciDir, filepath.Join(rdmaDir, "device")); err != nil {
			t.Fatal(err)
		}

		addr, err := pciAddressForRDMADevice(dir, "erdma_0")
		if err != nil {
			t.Fatalf("pciAddressForRDMADevice() unexpected error: %v", err)
		}
		if addr.String() != "0000:1b:00.0" {
			t.Errorf("pciAddressForRDMADevice() = %q, want %q", addr.String(), "0000:1b:00.0")
		}
	})

	t.Run("missing device symlink returns error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "erdma_0"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := pciAddressForRDMADevice(dir, "erdma_0")
		if err == nil {
			t.Error("pciAddressForRDMADevice() expected error, got nil")
		}
	})

	t.Run("non-existent RDMA device returns error", func(t *testing.T) {
		dir := t.TempDir()
		_, err := pciAddressForRDMADevice(dir, "nonexistent")
		if err == nil {
			t.Error("pciAddressForRDMADevice() expected error, got nil")
		}
	})
}
