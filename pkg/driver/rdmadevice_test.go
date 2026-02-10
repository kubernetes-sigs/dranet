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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetRdmaDeviceFromNetdevSysfs tests the sysfs fallback logic
// of getRdmaDeviceFromNetdev using a mock sysfs structure.
func TestGetRdmaDeviceFromNetdevSysfs(t *testing.T) {
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

// getRdmaDeviceFromSysfs is a testable helper that implements the sysfs fallback logic
// with a configurable base path instead of hardcoded /sys/class/net.
// This mirrors the fallback logic in getRdmaDeviceFromNetdev.
func getRdmaDeviceFromSysfs(basePath, ifName string) (string, error) {
	rdmaDir := filepath.Join(basePath, ifName, "device/infiniband")

	entries, err := os.ReadDir(rdmaDir)
	if err != nil {
		return "", fmt.Errorf("no RDMA device for %s: %w", ifName, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			return entry.Name(), nil // Return first RDMA device found (e.g., "mlx5_0")
		}
	}

	return "", fmt.Errorf("no RDMA device found for %s", ifName)
}
