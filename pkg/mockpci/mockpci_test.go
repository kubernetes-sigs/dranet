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

package mockpci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	tests := []struct {
		name     string
		input    DeviceConfig
		expected DeviceConfig
	}{
		{
			name:  "all empty fields get defaults",
			input: DeviceConfig{Name: "test0", PCIAddress: "0000:00:10.0"},
			expected: DeviceConfig{
				Name:       "test0",
				PCIAddress: "0000:00:10.0",
				VendorID:   "0x15b3",
				DeviceID:   "0x101b",
				Class:      "0x020000",
				Driver:     "mlx5_core",
				MTU:        1500,
			},
		},
		{
			name: "custom fields preserved",
			input: DeviceConfig{
				Name:       "custom0",
				PCIAddress: "0000:01:00.0",
				VendorID:   "0x8086",
				DeviceID:   "0x1592",
				Class:      "0x020001",
				Driver:     "ice",
				MTU:        9000,
			},
			expected: DeviceConfig{
				Name:       "custom0",
				PCIAddress: "0000:01:00.0",
				VendorID:   "0x8086",
				DeviceID:   "0x1592",
				Class:      "0x020001",
				Driver:     "ice",
				MTU:        9000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input
			ApplyDefaults(&cfg)
			if cfg.VendorID != tt.expected.VendorID {
				t.Errorf("VendorID = %q, want %q", cfg.VendorID, tt.expected.VendorID)
			}
			if cfg.DeviceID != tt.expected.DeviceID {
				t.Errorf("DeviceID = %q, want %q", cfg.DeviceID, tt.expected.DeviceID)
			}
			if cfg.Class != tt.expected.Class {
				t.Errorf("Class = %q, want %q", cfg.Class, tt.expected.Class)
			}
			if cfg.Driver != tt.expected.Driver {
				t.Errorf("Driver = %q, want %q", cfg.Driver, tt.expected.Driver)
			}
			if cfg.MTU != tt.expected.MTU {
				t.Errorf("MTU = %d, want %d", cfg.MTU, tt.expected.MTU)
			}
		})
	}
}

func TestGenerateModalias(t *testing.T) {
	tests := []struct {
		name     string
		vendor   string
		device   string
		class    string
		expected string
	}{
		{
			name:     "default mellanox connectx",
			vendor:   "0x15b3",
			device:   "0x101b",
			class:    "0x020000",
			expected: "pci:v000015B3d0000101Bsv000015B3sd00000001bc02sc00i00\n",
		},
		{
			name:     "intel e810",
			vendor:   "0x8086",
			device:   "0x1592",
			class:    "0x020000",
			expected: "pci:v00008086d00001592sv00008086sd00000001bc02sc00i00\n",
		},
		{
			name:     "class with subclass and prog interface",
			vendor:   "0x10de",
			device:   "0x1eb8",
			class:    "0x030201",
			expected: "pci:v000010DEd00001EB8sv000010DEsd00000001bc03sc02i01\n",
		},
		{
			name:     "without 0x prefix",
			vendor:   "15b3",
			device:   "101b",
			class:    "020000",
			expected: "pci:v000015B3d0000101Bsv000015B3sd00000001bc02sc00i00\n",
		},
		{
			name:     "empty fields fallback to defaults",
			vendor:   "",
			device:   "",
			class:    "",
			expected: "pci:v000015B3d0000101Bsv000015B3sd00000001bc02sc00i00\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := GenerateModalias(tt.vendor, tt.device, tt.class)
			if actual != tt.expected {
				t.Errorf("GenerateModalias(%q, %q, %q) = %q, want %q", tt.vendor, tt.device, tt.class, actual, tt.expected)
			}
			// Kernel modalias without newline must be 53 chars
			trimmed := strings.TrimRight(actual, "\n")
			if len(trimmed) != 53 {
				t.Errorf("modalias length = %d, expected 53", len(trimmed))
			}
		})
	}
}

func TestPopulateMockPCIDir(t *testing.T) {
	tempDir := t.TempDir()

	cfg := DeviceConfig{
		Name:          "mlx0",
		PCIAddress:    "0000:00:10.0",
		VendorID:      "0x15b3",
		DeviceID:      "0x101b",
		Class:         "0x020000",
		Driver:        "mlx5_core",
		NUMANode:      1,
		SRIOVTotalVFs: 8,
		SRIOVNumVFs:   4,
		PhysFn:        "0000:00:08.0",
	}

	if err := PopulateMockPCIDir(cfg, tempDir); err != nil {
		t.Fatalf("PopulateMockPCIDir failed: %v", err)
	}

	mockPCIDir := filepath.Join(tempDir, cfg.PCIAddress)

	// Check files created
	checkFileContent := func(filename, expected string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(mockPCIDir, filename))
		if err != nil {
			t.Errorf("failed to read %s: %v", filename, err)
			return
		}
		if string(content) != expected {
			t.Errorf("%s content = %q, want %q", filename, string(content), expected)
		}
	}

	checkFileContent("vendor", "0x15b3\n")
	checkFileContent("device", "0x101b\n")
	checkFileContent("subsystem_vendor", "0x15b3\n")
	checkFileContent("subsystem_device", "0x0001\n")
	checkFileContent("class", "0x020000\n")
	checkFileContent("numa_node", "1\n")
	checkFileContent("sriov_totalvfs", "8\n")
	checkFileContent("sriov_numvfs", "4\n")
	checkFileContent("modalias", "pci:v000015B3d0000101Bsv000015B3sd00000001bc02sc00i00\n")

	// Check driver symlink
	driverLink, err := os.Readlink(filepath.Join(mockPCIDir, "driver"))
	if err != nil {
		t.Fatalf("failed to read driver symlink: %v", err)
	}
	expectedDriverDest := filepath.Join(tempDir, "drivers", "mlx5_core")
	if driverLink != expectedDriverDest {
		t.Errorf("driver link = %q, want %q", driverLink, expectedDriverDest)
	}

	// Check net device symlink
	netDevLink, err := os.Readlink(filepath.Join(mockPCIDir, "net", "mlx0", "device"))
	if err != nil {
		t.Fatalf("failed to read net device symlink: %v", err)
	}
	if netDevLink != mockPCIDir {
		t.Errorf("net device link = %q, want %q", netDevLink, mockPCIDir)
	}

	// Check physfn symlink
	physfnLink, err := os.Readlink(filepath.Join(mockPCIDir, "physfn"))
	if err != nil {
		t.Fatalf("failed to read physfn symlink: %v", err)
	}
	expectedPhysfn := filepath.Join(tempDir, "0000:00:08.0")
	if physfnLink != expectedPhysfn {
		t.Errorf("physfn link = %q, want %q", physfnLink, expectedPhysfn)
	}
}

func TestStateLoadSave(t *testing.T) {
	origRoot := defaultMockPCIRoot
	tempDir := t.TempDir()
	defaultMockPCIRoot = tempDir
	defer func() {
		defaultMockPCIRoot = origRoot
	}()

	// 1. Initial load from non-existent state file should return empty map
	state, err := loadState()
	if err != nil {
		t.Fatalf("loadState on non-existent file failed: %v", err)
	}
	if len(state.Devices) != 0 {
		t.Errorf("expected empty devices map, got %d items", len(state.Devices))
	}

	// 2. Save state
	testCfg := DeviceConfig{
		Name:       "mlx0",
		PCIAddress: "0000:00:10.0",
		VendorID:   "0x15b3",
		DeviceID:   "0x101b",
	}
	state.Devices[testCfg.Name] = testCfg
	if err := saveState(state); err != nil {
		t.Fatalf("saveState failed: %v", err)
	}

	// 3. Reload state
	reloaded, err := loadState()
	if err != nil {
		t.Fatalf("reloaded loadState failed: %v", err)
	}
	if len(reloaded.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(reloaded.Devices))
	}
	dev, ok := reloaded.Devices["mlx0"]
	if !ok {
		t.Fatalf("expected mlx0 in reloaded devices")
	}
	if dev.PCIAddress != "0000:00:10.0" || dev.VendorID != "0x15b3" {
		t.Errorf("device data mismatch: %+v", dev)
	}

	// 4. Test List()
	list, err := List()
	if err != nil {
		t.Fatalf("List() failed: %v", err)
	}
	if len(list) != 1 || list[0].Name != "mlx0" {
		t.Errorf("unexpected List() result: %+v", list)
	}

	// 5. Corrupt state file returns unmarshal error
	stateFile := filepath.Join(tempDir, stateFileName)
	if err := os.WriteFile(stateFile, []byte("invalid json content"), 0644); err != nil {
		t.Fatalf("failed to write corrupted state: %v", err)
	}
	if _, err := loadState(); err == nil {
		t.Errorf("expected error loading corrupted state file, got nil")
	}
}

func TestValidation(t *testing.T) {
	if err := Create(DeviceConfig{PCIAddress: "0000:00:10.0"}); err == nil || !strings.Contains(err.Error(), "device name is required") {
		t.Errorf("expected device name error, got: %v", err)
	}

	if err := Create(DeviceConfig{Name: "eth0"}); err == nil || !strings.Contains(err.Error(), "pciAddress is required") {
		t.Errorf("expected pciAddress error, got: %v", err)
	}
}

func TestEnsureSymlink(t *testing.T) {
	tempDir := t.TempDir()
	target1 := filepath.Join(tempDir, "target1")
	target2 := filepath.Join(tempDir, "target2")
	link := filepath.Join(tempDir, "symlink")

	if err := os.WriteFile(target1, []byte("one"), 0644); err != nil {
		t.Fatalf("failed to create target1: %v", err)
	}
	if err := os.WriteFile(target2, []byte("two"), 0644); err != nil {
		t.Fatalf("failed to create target2: %v", err)
	}

	// 1. Initial symlink creation
	if err := ensureSymlink(target1, link); err != nil {
		t.Fatalf("ensureSymlink(target1, link) failed: %v", err)
	}
	dest, err := os.Readlink(link)
	if err != nil || dest != target1 {
		t.Errorf("link points to %q, want %q", dest, target1)
	}

	// 2. Overwrite existing symlink cleanly
	if err := ensureSymlink(target2, link); err != nil {
		t.Fatalf("ensureSymlink(target2, link) overwrite failed: %v", err)
	}
	dest, err = os.Readlink(link)
	if err != nil || dest != target2 {
		t.Errorf("overwritten link points to %q, want %q", dest, target2)
	}
}
