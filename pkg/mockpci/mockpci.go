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
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/internal/nlwrap"
)

var (
	defaultMockSysfsRoot = "/var/run/dranet/sysfs"
	stateFileName        = "state.json"
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
func GenerateModalias(vendorID, deviceID, class string) (string, error) {
	if vendorID == "" {
		vendorID = "0x15b3"
	}
	if deviceID == "" {
		deviceID = "0x101b"
	}
	if class == "" {
		class = "0x020000"
	}
	vInt, err := strconv.ParseUint(strings.TrimPrefix(vendorID, "0x"), 16, 64)
	if err != nil {
		return "", fmt.Errorf("invalid hex vendorID %q: %w", vendorID, err)
	}
	dInt, err := strconv.ParseUint(strings.TrimPrefix(deviceID, "0x"), 16, 64)
	if err != nil {
		return "", fmt.Errorf("invalid hex deviceID %q: %w", deviceID, err)
	}
	cInt, err := strconv.ParseUint(strings.TrimPrefix(class, "0x"), 16, 64)
	if err != nil {
		return "", fmt.Errorf("invalid hex class %q: %w", class, err)
	}

	baseClass := (cInt >> 16) & 0xff
	subClass := (cInt >> 8) & 0xff
	progIface := cInt & 0xff

	return fmt.Sprintf("pci:v%08Xd%08Xsv%08Xsd00000001bc%02Xsc%02Xi%02X\n", vInt, dInt, vInt, baseClass, subClass, progIface), nil
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

// PopulateMockPCIDir creates the mock sysfs tree structure for a PCIe device inside a shadow sysfs root.
func PopulateMockPCIDir(cfg DeviceConfig, rootDir string) error {
	ApplyDefaults(&cfg)

	sysRoot := filepath.Join(rootDir, "sys")
	busPCIDevices := filepath.Join(sysRoot, "bus", "pci", "devices")
	busPCIDrivers := filepath.Join(sysRoot, "bus", "pci", "drivers")
	classNet := filepath.Join(sysRoot, "class", "net")
	devicesDir := filepath.Join(sysRoot, "devices", "pci0000:00")
	mockPCIDir := filepath.Join(devicesDir, cfg.PCIAddress)
	mockNetDir := filepath.Join(mockPCIDir, "net", cfg.Name)

	if err := os.MkdirAll(mockNetDir, 0755); err != nil {
		return fmt.Errorf("failed creating mock directory %s: %w", mockNetDir, err)
	}
	if err := os.MkdirAll(busPCIDevices, 0755); err != nil {
		return fmt.Errorf("failed creating %s: %w", busPCIDevices, err)
	}
	if err := os.MkdirAll(classNet, 0755); err != nil {
		return fmt.Errorf("failed creating %s: %w", classNet, err)
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

	modalias, err := GenerateModalias(cfg.VendorID, cfg.DeviceID, cfg.Class)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mockPCIDir, "modalias"), []byte(modalias), 0644); err != nil {
		return err
	}

	// Driver symlink
	driverDir := filepath.Join(busPCIDrivers, cfg.Driver)
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
		if err := ensureSymlink(filepath.Join(devicesDir, cfg.PhysFn), filepath.Join(mockPCIDir, "physfn")); err != nil {
			return err
		}
	}

	// Net device link inside PCI directory: mockPCIDir/net/<dev>/device -> mockPCIDir
	if err := ensureSymlink(mockPCIDir, filepath.Join(mockNetDir, "device")); err != nil {
		return err
	}

	// Symlink in bus/pci/devices/<PCIAddress> -> mockPCIDir
	if err := ensureSymlink(mockPCIDir, filepath.Join(busPCIDevices, cfg.PCIAddress)); err != nil {
		return err
	}

	// Symlink in class/net/<Name> -> mockNetDir
	if err := ensureSymlink(mockNetDir, filepath.Join(classNet, cfg.Name)); err != nil {
		return err
	}

	// Mirror existing real host PCI devices into shadow bus/pci/devices if present
	if realEntries, err := os.ReadDir("/sys/bus/pci/devices"); err == nil {
		for _, entry := range realEntries {
			if entry.Name() != cfg.PCIAddress {
				target := filepath.Join("/sys/bus/pci/devices", entry.Name())
				link := filepath.Join(busPCIDevices, entry.Name())
				if err := ensureSymlink(target, link); err != nil {
					return fmt.Errorf("failed mirroring host PCI device %s: %w", entry.Name(), err)
				}
			}
		}
	}

	return nil
}

// State stores active mocked PCI devices.
type State struct {
	Devices map[string]DeviceConfig `json:"devices"`
}

func loadState() (*State, error) {
	statePath := filepath.Join(defaultMockSysfsRoot, stateFileName)
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
	if err := os.MkdirAll(defaultMockSysfsRoot, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(defaultMockSysfsRoot, stateFileName), data, 0644)
}

// Create sets up an in-kernel dummy network interface and registers it in the shadow sysfs hierarchy.
func Create(cfg DeviceConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("device name is required")
	}
	if cfg.PCIAddress == "" {
		return fmt.Errorf("pciAddress is required")
	}
	ApplyDefaults(&cfg)

	// 1. Create in-kernel dummy network interface via Netlink
	link, err := nlwrap.LinkByName(cfg.Name)
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
				dummy.HardwareAddr = hwAddr
			}
		}
		if err := netlink.LinkAdd(dummy); err != nil {
			return fmt.Errorf("failed to add dummy interface %s: %w", cfg.Name, err)
		}
		link, err = nlwrap.LinkByName(cfg.Name)
		if err != nil {
			return fmt.Errorf("failed to get newly created interface %s: %w", cfg.Name, err)
		}
	}

	// 2. Attach Soft-RoCE (rdma_rxe) if requested
	if cfg.RDMADevice != "" {
		// modprobe may fail if running without CAP_SYS_MODULE, but the module may already be loaded in the kernel.
		if out, err := exec.Command("modprobe", "rdma_rxe").CombinedOutput(); err != nil {
			klog.V(4).Infof("modprobe rdma_rxe notice: %s (%v)", strings.TrimSpace(string(out)), err)
		}
		if out, err := exec.Command("rdma", "link", "add", cfg.RDMADevice, "type", "rxe", "netdev", cfg.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("failed adding rdma link %s: %s: %w", cfg.RDMADevice, strings.TrimSpace(string(out)), err)
		}
	}

	// 3. Populate shadow sysfs directory
	if err := PopulateMockPCIDir(cfg, defaultMockSysfsRoot); err != nil {
		return err
	}

	// 4. Trigger Netlink notification so dranet immediately scans the interface
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
			if out, err := exec.Command("rdma", "link", "del", targetCfg.RDMADevice).CombinedOutput(); err != nil {
				return fmt.Errorf("failed deleting rdma link %s: %s: %w", targetCfg.RDMADevice, strings.TrimSpace(string(out)), err)
			}
		}

		// Delete dummy interface
		if link, err := nlwrap.LinkByName(targetCfg.Name); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				return fmt.Errorf("failed deleting link %s: %w", targetCfg.Name, err)
			}
		}

		// Remove shadow sysfs links and device directory
		sysRoot := filepath.Join(defaultMockSysfsRoot, "sys")
		busPCILink := filepath.Join(sysRoot, "bus", "pci", "devices", targetCfg.PCIAddress)
		classNetLink := filepath.Join(sysRoot, "class", "net", targetCfg.Name)
		deviceDir := filepath.Join(sysRoot, "devices", "pci0000:00", targetCfg.PCIAddress)

		if err := os.Remove(busPCILink); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing %s: %w", busPCILink, err)
		}
		if err := os.Remove(classNetLink); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing %s: %w", classNetLink, err)
		}
		if err := os.RemoveAll(deviceDir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed removing %s: %w", deviceDir, err)
		}
	}

	return saveState(state)
}

// Cleanup purges all mocked devices and removes the shadow sysfs hierarchy.
func Cleanup() error {
	var errs []error
	state, err := loadState()
	if err == nil {
		for _, dev := range state.Devices {
			if dev.RDMADevice != "" {
				if out, err := exec.Command("rdma", "link", "del", dev.RDMADevice).CombinedOutput(); err != nil {
					errs = append(errs, fmt.Errorf("failed deleting rdma link %s: %s: %w", dev.RDMADevice, strings.TrimSpace(string(out)), err))
				}
			}
			if link, err := nlwrap.LinkByName(dev.Name); err == nil {
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
					if out, err := exec.Command("rdma", "link", "del", rdmaDev).CombinedOutput(); err != nil {
						errs = append(errs, fmt.Errorf("failed deleting rdma link %s: %s: %w", rdmaDev, strings.TrimSpace(string(out)), err))
					}
				}
			}
		}
	}

	if err := os.RemoveAll(defaultMockSysfsRoot); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed removing %s: %w", defaultMockSysfsRoot, err))
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
