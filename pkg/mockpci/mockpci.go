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
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var (
	defaultMockPCIRoot = "/var/run/dranet/mock-pci"
	stateFileName      = "state.json"
	sysBusPCIDevices   = "/sys/bus/pci/devices"
	sysClassNet        = "/sys/class/net"
	savedPCIBusDir     = "/var/run/dranet/pci-bus-save"
	savedNetDir        = "/var/run/dranet/net-save"
)

// DeviceConfig represents a synthetic PCI network device configuration.
type DeviceConfig struct {
	Name          string `json:"name"`
	PCIAddress    string `json:"pciAddress"`
	VendorID      string `json:"vendorId"`
	DeviceID      string `json:"deviceId"`
	Class         string `json:"class"`
	Driver        string `json:"driver"`
	NUMANode      int    `json:"numaNode"`
	MAC           string `json:"mac,omitempty"`
	MTU           int    `json:"mtu,omitempty"`
	RDMADevice    string `json:"rdmaDevice,omitempty"`
	SRIOVTotalVFs int    `json:"sriovTotalVFs,omitempty"`
	SRIOVNumVFs   int    `json:"sriovNumVFs,omitempty"`
	PhysFn        string `json:"physFn,omitempty"`
}

// ApplyDefaults populates default values for missing configuration fields.
func ApplyDefaults(cfg *DeviceConfig) {
	if cfg.VendorID == "" {
		cfg.VendorID = "0x15b3"
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = "0x101b"
	}
	if cfg.Class == "" {
		cfg.Class = "0x020000"
	}
	if cfg.Driver == "" {
		cfg.Driver = "mlx5_core"
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
}

// GenerateModalias creates a standard Linux kernel modalias string for a PCI device.
func GenerateModalias(vendorID, deviceID, class string) string {
	if vendorID == "" {
		vendorID = "0x15b3"
	}
	if deviceID == "" {
		deviceID = "0x101b"
	}
	if class == "" {
		class = "0x020000"
	}
	vInt, _ := strconv.ParseUint(strings.TrimPrefix(vendorID, "0x"), 16, 64)
	dInt, _ := strconv.ParseUint(strings.TrimPrefix(deviceID, "0x"), 16, 64)
	cInt, _ := strconv.ParseUint(strings.TrimPrefix(class, "0x"), 16, 64)

	baseClass := (cInt >> 16) & 0xff
	subClass := (cInt >> 8) & 0xff
	progIface := cInt & 0xff

	return fmt.Sprintf("pci:v%08Xd%08Xsv%08Xsd00000001bc%02Xsc%02Xi%02X\n", vInt, dInt, vInt, baseClass, subClass, progIface)
}

func ensureSymlink(target, link string) error {
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed removing existing link %s: %w", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("failed creating symlink %s -> %s: %w", link, target, err)
	}
	return nil
}

// PopulateMockPCIDir creates the mock sysfs tree structure for a PCIe device.
func PopulateMockPCIDir(cfg DeviceConfig, rootDir string) error {
	ApplyDefaults(&cfg)
	mockPCIDir := filepath.Join(rootDir, cfg.PCIAddress)
	mockNetDir := filepath.Join(mockPCIDir, "net", cfg.Name)
	if err := os.MkdirAll(mockNetDir, 0755); err != nil {
		return fmt.Errorf("failed creating mock directory %s: %w", mockNetDir, err)
	}

	if err := os.WriteFile(filepath.Join(mockPCIDir, "vendor"), []byte(cfg.VendorID+"\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "device"), []byte(cfg.DeviceID+"\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "subsystem_vendor"), []byte(cfg.VendorID+"\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "subsystem_device"), []byte("0x0001\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "class"), []byte(cfg.Class+"\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "numa_node"), []byte(strconv.Itoa(cfg.NUMANode)+"\n"), 0644); err != nil {
		return err
	}

	modalias := GenerateModalias(cfg.VendorID, cfg.DeviceID, cfg.Class)
	if err := os.WriteFile(filepath.Join(mockPCIDir, "modalias"), []byte(modalias), 0644); err != nil {
		return err
	}

	// Driver symlink
	driverDir := filepath.Join(rootDir, "drivers", cfg.Driver)
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		return fmt.Errorf("failed creating driver directory %s: %w", driverDir, err)
	}
	if err := ensureSymlink(driverDir, filepath.Join(mockPCIDir, "driver")); err != nil {
		return err
	}

	// SR-IOV attributes if present
	if cfg.SRIOVTotalVFs > 0 {
		if err := os.WriteFile(filepath.Join(mockPCIDir, "sriov_totalvfs"), []byte(strconv.Itoa(cfg.SRIOVTotalVFs)+"\n"), 0644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(mockPCIDir, "sriov_numvfs"), []byte(strconv.Itoa(cfg.SRIOVNumVFs)+"\n"), 0644); err != nil {
			return err
		}
	}
	if cfg.PhysFn != "" {
		if err := ensureSymlink(filepath.Join(rootDir, cfg.PhysFn), filepath.Join(mockPCIDir, "physfn")); err != nil {
			return err
		}
	}

	// Net device link inside PCI directory
	if err := ensureSymlink(mockPCIDir, filepath.Join(mockNetDir, "device")); err != nil {
		return err
	}

	return nil
}

// State stores active mocked PCI devices.
type State struct {
	Devices map[string]DeviceConfig `json:"devices"`
}

func loadState() (*State, error) {
	statePath := filepath.Join(defaultMockPCIRoot, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Devices: make(map[string]DeviceConfig)}, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Devices == nil {
		state.Devices = make(map[string]DeviceConfig)
	}
	return &state, nil
}

func saveState(state *State) error {
	if err := os.MkdirAll(defaultMockPCIRoot, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(defaultMockPCIRoot, stateFileName), data, 0644)
}

func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

func ensureTmpfsMount(target, backupDir string) error {
	if isMountPoint(target) {
		return nil
	}

	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(target)
	if err == nil {
		for _, entry := range entries {
			src := filepath.Join(target, entry.Name())
			linkDst, err := filepath.EvalSymlinks(src)
			if err == nil {
				if err := ensureSymlink(linkDst, filepath.Join(backupDir, entry.Name())); err != nil {
					return fmt.Errorf("failed backing up symlink %s: %w", entry.Name(), err)
				}
			}
		}
	}

	if err := unix.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("failed mounting tmpfs over %s: %w", target, err)
	}

	backupEntries, err := os.ReadDir(backupDir)
	if err == nil {
		for _, entry := range backupEntries {
			dst, err := os.Readlink(filepath.Join(backupDir, entry.Name()))
			if err == nil {
				if err := ensureSymlink(dst, filepath.Join(target, entry.Name())); err != nil {
					return fmt.Errorf("failed restoring symlink %s: %w", entry.Name(), err)
				}
			}
		}
	}

	return nil
}

// Create sets up an in-kernel dummy network interface and links it into a mocked sysfs PCI hierarchy.
func Create(cfg DeviceConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("device name is required")
	}
	if cfg.PCIAddress == "" {
		return fmt.Errorf("pciAddress is required")
	}
	ApplyDefaults(&cfg)

	// 1. Create in-kernel dummy network interface via Netlink
	link, err := netlink.LinkByName(cfg.Name)
	if err != nil {
		dummy := &netlink.Dummy{
			LinkAttrs: netlink.LinkAttrs{
				Name: cfg.Name,
				MTU:  cfg.MTU,
			},
		}
		if cfg.MAC != "" {
			hwAddr, err := net.ParseMAC(cfg.MAC)
			if err == nil {
				dummy.LinkAttrs.HardwareAddr = hwAddr
			}
		}
		if err := netlink.LinkAdd(dummy); err != nil {
			return fmt.Errorf("failed to add dummy interface %s: %w", cfg.Name, err)
		}
		link, err = netlink.LinkByName(cfg.Name)
		if err != nil {
			return fmt.Errorf("failed to get newly created interface %s: %w", cfg.Name, err)
		}
	}

	// 2. Attach Soft-RoCE (rdma_rxe) if requested
	if cfg.RDMADevice != "" {
		_ = exec.Command("modprobe", "rdma_rxe").Run()
		if out, err := exec.Command("rdma", "link", "add", cfg.RDMADevice, "type", "rxe", "netdev", cfg.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("failed adding rdma link %s: %s: %w", cfg.RDMADevice, strings.TrimSpace(string(out)), err)
		}
	}

	// 3. Populate mock PCI directory
	if err := PopulateMockPCIDir(cfg, defaultMockPCIRoot); err != nil {
		return err
	}
	mockPCIDir := filepath.Join(defaultMockPCIRoot, cfg.PCIAddress)
	mockNetDir := filepath.Join(mockPCIDir, "net", cfg.Name)

	// 4. Mount tmpfs over /sys/bus/pci/devices and /sys/class/net
	if err := ensureTmpfsMount(sysBusPCIDevices, savedPCIBusDir); err != nil {
		return err
	}
	if err := ensureTmpfsMount(sysClassNet, savedNetDir); err != nil {
		return err
	}

	// 5. Link into sysfs
	if err := ensureSymlink(mockPCIDir, filepath.Join(sysBusPCIDevices, cfg.PCIAddress)); err != nil {
		return err
	}
	if err := ensureSymlink(mockNetDir, filepath.Join(sysClassNet, cfg.Name)); err != nil {
		return err
	}

	// 6. Trigger Netlink notification so dranet immediately scans the interface
	if err := netlink.LinkSetDown(link); err != nil {
		return fmt.Errorf("failed setting link %s down: %w", cfg.Name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed setting link %s up: %w", cfg.Name, err)
	}

	// Save state
	state, err := loadState()
	if err != nil {
		return fmt.Errorf("failed loading state: %w", err)
	}
	state.Devices[cfg.Name] = cfg
	return saveState(state)
}

// Delete removes a single mocked device.
func Delete(nameOrBDF string) error {
	state, err := loadState()
	if err != nil {
		return err
	}

	var targetCfg *DeviceConfig
	for name, dev := range state.Devices {
		if name == nameOrBDF || dev.PCIAddress == nameOrBDF {
			targetCfg = &dev
			delete(state.Devices, name)
			break
		}
	}

	if targetCfg != nil {
		// Remove RDMA link if attached
		if targetCfg.RDMADevice != "" {
			_ = exec.Command("rdma", "link", "del", targetCfg.RDMADevice).Run()
		}

		// Delete dummy interface
		if link, err := netlink.LinkByName(targetCfg.Name); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				return fmt.Errorf("failed deleting link %s: %w", targetCfg.Name, err)
			}
		}

		// Remove sysfs links
		if err := os.Remove(filepath.Join(sysBusPCIDevices, targetCfg.PCIAddress)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing %s from %s: %w", targetCfg.PCIAddress, sysBusPCIDevices, err)
		}
		if err := os.Remove(filepath.Join(sysClassNet, targetCfg.Name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing %s from %s: %w", targetCfg.Name, sysClassNet, err)
		}
		if err := os.RemoveAll(filepath.Join(defaultMockPCIRoot, targetCfg.PCIAddress)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing mock PCI directory %s: %w", targetCfg.PCIAddress, err)
		}
	}

	return saveState(state)
}

// Cleanup unmounts tmpfs layers and purges all mocked devices.
func Cleanup() error {
	var errs []error
	state, err := loadState()
	if err == nil {
		for _, dev := range state.Devices {
			if dev.RDMADevice != "" {
				_ = exec.Command("rdma", "link", "del", dev.RDMADevice).Run()
			}
			if link, err := netlink.LinkByName(dev.Name); err == nil {
				if err := netlink.LinkDel(link); err != nil {
					errs = append(errs, fmt.Errorf("failed deleting link %s: %w", dev.Name, err))
				}
			}
		}
	}

	// Clean up any other rdma links
	out, err := exec.Command("rdma", "link", "show").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				rdmaDev := strings.TrimSpace(parts[1])
				if rdmaDev != "" {
					_ = exec.Command("rdma", "link", "del", rdmaDev).Run()
				}
			}
		}
	}

	if isMountPoint(sysBusPCIDevices) {
		if err := unix.Unmount(sysBusPCIDevices, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Errorf("failed unmounting %s: %w", sysBusPCIDevices, err))
		}
	}
	if isMountPoint(sysClassNet) {
		if err := unix.Unmount(sysClassNet, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Errorf("failed unmounting %s: %w", sysClassNet, err))
		}
	}

	if err := os.RemoveAll(defaultMockPCIRoot); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed removing %s: %w", defaultMockPCIRoot, err))
	}
	if err := os.RemoveAll(savedPCIBusDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed removing %s: %w", savedPCIBusDir, err))
	}
	if err := os.RemoveAll(savedNetDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed removing %s: %w", savedNetDir, err))
	}

	if len(errs) > 0 {
		var errMsgs []string
		for _, e := range errs {
			errMsgs = append(errMsgs, e.Error())
		}
		return fmt.Errorf("cleanup encountered errors:\n%s", strings.Join(errMsgs, "\n"))
	}
	return nil
}

// List returns all registered mock devices.
func List() ([]DeviceConfig, error) {
	state, err := loadState()
	if err != nil {
		return nil, err
	}
	devices := make([]DeviceConfig, 0, len(state.Devices))
	for _, d := range state.Devices {
		devices = append(devices, d)
	}
	return devices, nil
}
