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
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestSubinterface_IPVlan(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	// Phase 1: Initialize the network namespaces.
	// Save the host network namespace to restore it at the end of the test.
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	// Create a dedicated target container network namespace.
	rndString := make([]byte, 4)
	_, err = rand.Read(rndString)
	if err != nil {
		t.Fatalf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer testNS.Close()

	// Switch back to the original namespace.
	netns.Set(origns)

	// Open a netlink handle inside the container namespace to manage the loopback interface.
	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()

	loLink, err := nhNs.LinkByName("lo")
	if err != nil {
		t.Fatalf("Failed to get loopback interface: %v", err)
	}
	if err := nhNs.LinkSetUp(loLink); err != nil {
		t.Fatalf("Failed to set up loopback interface: %v", err)
	}

	// Phase 2: Create the host parent interface.
	// Define a parent dummy host interface with a custom MAC and MTU.
	// We use custom attributes to ensure the child IPVlan subinterface correctly inherits them.
	ifaceName := "testdummy-0"
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	la.HardwareAddr = net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	la.MTU = 1400

	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}

	// Ensure the parent link is cleaned up at teardown.
	t.Cleanup(func() {
		link, err := nlwrap.LinkByName(ifaceName)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	})

	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to set up dummy link %s on host: %v", ifaceName, err)
	}

	// Phase 3: Call nsCreateSubinterface.
	// Create the IPVlan subinterface in the target container
	// namespace and assert driver-reported device data.
	config := apis.InterfaceConfig{
		Name:      "dranet0",
		Addresses: []string{"2001:db8::3/128"},
		SubInterface: &apis.SubInterfaceConfig{
			Type: apis.SubInterfaceTypeIPVlan,
		},
	}

	deviceData, err := nsCreateSubinterface(ifaceName, path.Join("/run/netns", nsName), config)
	if err != nil {
		t.Fatalf("fail to create subinterface: %v", err)
	}

	if deviceData.InterfaceName != config.Name {
		t.Errorf("Expected reported InterfaceName %q, got %q", config.Name, deviceData.InterfaceName)
	}
	if deviceData.HardwareAddress != "00:11:22:33:44:55" {
		t.Errorf("Expected reported HardwareAddress %q, got %q", "00:11:22:33:44:55", deviceData.HardwareAddress)
	}
	if len(deviceData.IPs) != 1 || deviceData.IPs[0] != config.Addresses[0] {
		t.Errorf("Expected reported IPs %v, got %v", config.Addresses, deviceData.IPs)
	}

	// Verify that the parent interface still exists in the host namespace (was not moved).
	_, err = nlwrap.LinkByName(ifaceName)
	if err != nil {
		t.Errorf("expected parent interface %s to still exist in the host namespace: %v", ifaceName, err)
	}

	// Phase 4: Inspect the state of the interface in the container namespace.
	// Switch thread into the container namespace to query the interface directly.
	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}
		defer netns.Set(origns)

		cmd := exec.Command("ip", "-d", "link", "show", config.Name)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("failed to show link properties: %v", err)
		}
		outputStr := string(output)

		// Assert that the created subinterface is indeed of type ipvlan with mode l2 and flag bridge.
		if !strings.Contains(outputStr, "ipvlan") {
			t.Errorf("expected link type to be 'ipvlan', link status output:\n%s", outputStr)
		}
		if !strings.Contains(outputStr, "mode l2") {
			t.Errorf("expected ipvlan mode to be 'l2', link status output:\n%s", outputStr)
		}
		if !strings.Contains(outputStr, "bridge") {
			t.Errorf("expected ipvlan flag to be 'bridge', link status output:\n%s", outputStr)
		}

		// Assert that the child subinterface inherited the parent's MTU configuration (1400).
		if !strings.Contains(outputStr, "mtu 1400") {
			t.Errorf("expected MTU 1400 to be inherited, link status output:\n%s", outputStr)
		}

		// Assert that the child subinterface inherited the parent's MAC configuration.
		if !strings.Contains(outputStr, "link/ether 00:11:22:33:44:55") {
			t.Errorf("expected Hardware MAC 00:11:22:33:44:55 to be inherited, link status output:\n%s", outputStr)
		}

		// Assert that the assigned IP address is correctly configured.
		cmd = exec.Command("ip", "addr", "show", config.Name)
		output, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("failed to show interface addresses: %v", err)
		}
		outputStr = string(output)

		for _, addr := range config.Addresses {
			if !strings.Contains(outputStr, addr) {
				t.Errorf("expected address %s not found in ip addr show:\n%s", addr, outputStr)
			}
		}
	}()

	// Phase 5: Call nsDeleteSubinterface for teardown.
	// Verify that the subinterface is successfully deleted inside the test namespace.
	err = nsDeleteSubinterface(path.Join("/run/netns", nsName), config.Name)
	if err != nil {
		t.Fatalf("fail to delete subinterface: %v", err)
	}

	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}
		defer netns.Set(origns)

		cmd := exec.Command("ip", "link", "show", config.Name)
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("expected subinterface %s to be deleted, but it still exists: %s", config.Name, string(output))
		}
	}()
}
