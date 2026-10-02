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

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/dranet/pkg/mockpci"
)

var (
	devCfg mockpci.DeviceConfig
)

var rootCmd = &cobra.Command{
	Use:   "mockpci",
	Short: "mockpci manages synthetic in-kernel and PCIe network devices for testing",
}

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add and configure a mocked PCIe network device",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := mockpci.Create(devCfg); err != nil {
			return fmt.Errorf("failed creating mock PCI device: %w", err)
		}
		fmt.Printf("Successfully created mock PCI device %s at %s\n", devCfg.Name, devCfg.PCIAddress)
		return nil
	},
}

var delCmd = &cobra.Command{
	Use:   "del [name or BDF]",
	Short: "Delete a mocked PCIe network device",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := mockpci.Delete(args[0]); err != nil {
			return fmt.Errorf("failed deleting mock PCI device %s: %w", args[0], err)
		}
		fmt.Printf("Successfully deleted mock PCI device %s\n", args[0])
		return nil
	},
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean up all mocked PCIe devices and unmount sysfs tmpfs layers",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := mockpci.Cleanup(); err != nil {
			return fmt.Errorf("failed during cleanup: %w", err)
		}
		fmt.Println("Successfully cleaned up all mock PCI devices and unmounted sysfs layers")
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all active mocked PCIe devices",
	RunE: func(cmd *cobra.Command, args []string) error {
		devs, err := mockpci.List()
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(devs, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	},
}

func init() {
	addCmd.Flags().StringVarP(&devCfg.Name, "name", "n", "", "Interface name (e.g. mlx0)")
	addCmd.Flags().StringVar(&devCfg.PCIAddress, "pci-address", "", "PCI BDF address (e.g. 0000:00:10.0)")
	addCmd.Flags().StringVarP(&devCfg.VendorID, "vendor", "v", "0x15b3", "PCI Vendor ID in hex")
	addCmd.Flags().StringVarP(&devCfg.DeviceID, "device", "d", "0x101b", "PCI Device ID in hex")
	addCmd.Flags().StringVar(&devCfg.Class, "class", "0x020000", "PCI Class in hex")
	addCmd.Flags().StringVar(&devCfg.Driver, "driver", "mlx5_core", "PCI kernel driver name")
	addCmd.Flags().IntVar(&devCfg.NUMANode, "numa-node", 0, "NUMA node ID")
	addCmd.Flags().StringVar(&devCfg.MAC, "mac", "", "Hardware MAC address")
	addCmd.Flags().IntVar(&devCfg.MTU, "mtu", 1500, "Interface MTU")
	addCmd.Flags().StringVar(&devCfg.RDMADevice, "rdma-device", "", "RDMA device name (e.g. mlx5_0 or rxe0)")
	addCmd.Flags().IntVar(&devCfg.SRIOVTotalVFs, "sriov-total-vfs", 0, "SR-IOV total VFs supported (for PF)")
	addCmd.Flags().IntVar(&devCfg.SRIOVNumVFs, "sriov-num-vfs", 0, "SR-IOV active VFs (for PF)")
	addCmd.Flags().StringVar(&devCfg.PhysFn, "physfn", "", "BDF of Physical Function (if this device is a VF)")

	if err := addCmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	if err := addCmd.MarkFlagRequired("pci-address"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(addCmd, delCmd, cleanupCmd, listCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
