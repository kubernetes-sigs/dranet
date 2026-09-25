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
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
	sysctltesting "k8s.io/component-helpers/node/util/sysctl/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func Test_nhNetdev(t *testing.T) {
	userns.Run(t, test_nhNetdev_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func test_nhNetdev_Namespaced(t *testing.T) {
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	_, err = rand.Read(rndString)
	if err != nil {
		t.Errorf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	containerNsPath := path.Join("/run/netns", nsName)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer testNS.Close()

	// Switch back to the original namespace
	netns.Set(origns)

	// Create a dummy interface in the test namespace
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

	ifaceName := "testdummy-0"
	// Create a veth pair
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}

	t.Cleanup(func() {
		link, err := nlwrap.LinkByName(ifaceName)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to add veth link %s in ns %s: %v", ifaceName, nsName, err)
	}
	// The MTU stays at or above the IPv6 minimum of 1280. Below it the kernel
	// creates no IPv6 settings for the interface, so accept_ra cannot be applied.
	config := apis.InterfaceConfig{
		Name:           "dranet0",
		Addresses:      []string{"192.168.7.7/32"},
		MTU:            ptr.To[int32](1300),
		HardwareAddr:   ptr.To("00:11:22:33:44:55"),
		GSOMaxSize:     ptr.To[int32](1024),
		GROMaxSize:     ptr.To[int32](1025),
		GSOIPv4MaxSize: ptr.To[int32](1026),
		GROIPv4MaxSize: ptr.To[int32](1027),
		ARPIgnore:      ptr.To[int32](3),
		ARPAnnounce:    ptr.To[int32](1),
		AcceptRA:       ptr.To[int32](2),
	}

	deviceData, err := nsAttachNetdev(ifaceName, containerNsPath, config)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}

	// check against  ip lin
	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ip", "-d", "link", "show", config.Name)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr := string(output)

		if !strings.Contains(outputStr, fmt.Sprintf("mtu %d", *config.MTU)) {
			t.Errorf("mtu not changed %s", outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize)) {
			t.Errorf("GSOMaxSize not changed wanted %s got %s", fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize), outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gro_max_size %d", *config.GROMaxSize)) {
			t.Errorf("GROMaxSize not changed %s", outputStr)
		}
		// require iproute 6.3.0+
		// TODO: validate the ip version to check it
		// https://github.com/iproute2/iproute2/commit/1dafe448c7a2f2be5dfddd8da250980708a48c41
		/*
			if !strings.Contains(outputStr, fmt.Sprintf("gso_ipv4_max_size %d", *config.GSOIPv4MaxSize)) {
				t.Errorf("GSOIPv4MaxSize not changed %s", outputStr)
			}
			if !strings.Contains(outputStr, fmt.Sprintf("gro_ipv4_max_size %d", *config.GROIPv4MaxSize)) {
				t.Errorf("GROIPv4MaxSize not changed %s", outputStr)
			}
		*/
		if !strings.Contains(outputStr, fmt.Sprintf("link/ether %s", *config.HardwareAddr)) {
			t.Errorf("HardwareAddr not changed %s", outputStr)
		}
		if *config.HardwareAddr != deviceData.HardwareAddress {
			t.Errorf("HardwareAddr not reported")
		}

		// lo is the control: it shares the namespace but has no config, so it
		// shows the namespace default the moved interface would have kept.
		for _, tc := range []struct {
			family  string
			setting string
			want    int
		}{
			{"ipv4", "arp_ignore", int(*config.ARPIgnore)},
			{"ipv4", "arp_announce", int(*config.ARPAnnounce)},
			{"ipv6", "accept_ra", int(*config.AcceptRA)},
		} {
			got, err := sysctl.New().GetSysctl(fmt.Sprintf("net/%s/conf/%s/%s", tc.family, config.Name, tc.setting))
			if err != nil {
				t.Fatalf("failed to read %s in pod namespace: %v", tc.setting, err)
			}
			if got != tc.want {
				t.Errorf("%s = %d, want %d", tc.setting, got, tc.want)
			}
			baseline, err := sysctl.New().GetSysctl(fmt.Sprintf("net/%s/conf/lo/%s", tc.family, tc.setting))
			if err != nil {
				t.Fatalf("failed to read baseline %s in pod namespace: %v", tc.setting, err)
			}
			if baseline == tc.want {
				t.Errorf("%s baseline is already %d, the test cannot prove the config was applied", tc.setting, baseline)
			}
		}

		cmd = exec.Command("ip", "addr", "show", config.Name)
		output, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr = string(output)
		// TODO check reported state
		for _, addr := range config.Addresses {
			if !strings.Contains(outputStr, addr) {
				t.Errorf("address %s not found", addr)
			}
		}

		// Switch back to the original namespace
		err = netns.Set(origns)
		if err != nil {
			t.Fatal(err)
		}
	}()

	err = nsDetachNetdev(containerNsPath, config.Name, ifaceName)
	if err != nil {
		t.Fatalf("failed to detach netdev from namespace: %v", err)
	}

	// Delete the namespace path during the ARP failure to prove rollback uses
	// the open namespace handle and returns the device to the host.
	var deleteNamedErr error
	original := sysctlProvider
	sysctlProvider = func() sysctl.Interface {
		return &failingSetSysctl{
			Fake:    sysctltesting.NewFake(),
			setting: fmt.Sprintf("net/ipv4/conf/%s/arp_ignore", config.Name),
			onFailure: func() {
				deleteNamedErr = netns.DeleteNamed(nsName)
			},
		}
	}
	t.Cleanup(func() { sysctlProvider = original })

	if _, err := nsAttachNetdev(ifaceName, containerNsPath, config); err == nil ||
		!strings.Contains(err.Error(), "arp_ignore") {
		t.Fatalf("nsAttachNetdev() error = %v, want an arp_ignore apply error", err)
	}
	if deleteNamedErr != nil {
		t.Fatalf("failed to remove network namespace path during ARP failure: %v", deleteNamedErr)
	}
	if _, statErr := os.Stat(containerNsPath); statErr == nil {
		t.Fatal("network namespace path still exists after ARP failure")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("failed to check network namespace path after ARP failure: %v", statErr)
	}
	returnedDev, err := nlwrap.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("network device was not returned to the host after the ARP apply error: %v", err)
	}
	if returnedDev.Attrs().Flags&net.FlagUp == 0 {
		t.Error("network device was not brought up after the ARP apply error")
	}
}

// A claim with acceptRA and no mtu on a host device below the IPv6 minimum MTU
// is rejected before the device is set down or moved.
func Test_nsAttachNetdevRejectsAcceptRABelowIPv6MTU(t *testing.T) {
	userns.Run(t, test_nsAttachNetdevRejectsAcceptRABelowIPv6MTU_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func test_nsAttachNetdevRejectsAcceptRABelowIPv6MTU_Namespaced(t *testing.T) {
	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	containerNsPath := path.Join("/run/netns", nsName)
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()
	runtime.LockOSThread()
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	setErr := netns.Set(origns)
	runtime.UnlockOSThread()
	if setErr != nil {
		t.Fatalf("failed to switch back to the host namespace: %v", setErr)
	}
	t.Cleanup(func() {
		testNS.Close()
		_ = netns.DeleteNamed(nsName)
	})

	name := fmt.Sprintf("tdummy-%x", rndString)
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.MTU = apis.MinIPv6MTU - 1
	link := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s: %v", name, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to set up dummy link %s: %v", name, err)
	}
	addr, err := netlink.ParseAddr("192.0.2.1/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("Failed to add address to %s: %v", name, err)
	}
	before, err := nlwrap.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	// Link events show every change to the device, including a down that a
	// later up would hide from the final state.
	events, done := subscribeLinkEvents(t, before.Attrs().Index)

	config := apis.InterfaceConfig{Name: "dranet0", AcceptRA: ptr.To[int32](0)}
	_, err = nsAttachNetdev(name, containerNsPath, config)
	if err == nil || !strings.Contains(err.Error(), "acceptRA requires an MTU of at least 1280") {
		t.Fatalf("nsAttachNetdev() error = %v, want an MTU error", err)
	}
	// Events of the call arrive asynchronously; give them a moment before stopping.
	// The setup above may still deliver its own up event, so the check is that
	// the device never reported itself down.
	time.Sleep(200 * time.Millisecond)
	close(done)
	for _, event := range <-events {
		if !strings.Contains(event, "flags=up") {
			t.Errorf("the host device was set down during the rejection: %s", event)
		}
	}

	// The device is still in the host namespace with its state and address.
	after, err := nlwrap.LinkByName(name)
	if err != nil {
		t.Fatalf("host device is gone after the rejection: %v", err)
	}
	if after.Attrs().Index != before.Attrs().Index || after.Attrs().Flags != before.Attrs().Flags || after.Attrs().OperState != before.Attrs().OperState {
		t.Errorf("host device changed: before index=%d flags=%v operstate=%v, after index=%d flags=%v operstate=%v", before.Attrs().Index, before.Attrs().Flags, before.Attrs().OperState, after.Attrs().Index, after.Attrs().Flags, after.Attrs().OperState)
	}
	addrs, err := netlink.AddrList(after, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].IPNet.String() != "192.0.2.1/24" {
		t.Errorf("host addresses after the rejection = %v, want [192.0.2.1/24]", addrs)
	}
}

// subscribeLinkEvents collects the netlink link events for one interface index
// in the host namespace. Closing done stops the collection; the result channel
// then receives the events seen up to that point.
func subscribeLinkEvents(t *testing.T, index int) (<-chan []string, chan struct{}) {
	t.Helper()
	updates := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	if err := netlink.LinkSubscribe(updates, done); err != nil {
		t.Fatalf("LinkSubscribe() error: %v", err)
	}
	result := make(chan []string, 1)
	go func() {
		var got []string
		for {
			select {
			case update := <-updates:
				if update.Link != nil && update.Link.Attrs().Index == index {
					got = append(got, fmt.Sprintf("type=%d flags=%v", update.Header.Type, update.Link.Attrs().Flags))
				}
			case <-done:
				result <- got
				return
			}
		}
	}()
	return result, done
}

func Test_nsDetachNetdevFromNSUsesOpenNamespace(t *testing.T) {
	userns.Run(t, test_nsDetachNetdevFromNSUsesOpenNamespace_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func test_nsDetachNetdevFromNSUsesOpenNamespace_Namespaced(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	originalNs, err := netns.Get()
	if err != nil {
		t.Fatalf("failed to get current network namespace: %v", err)
	}
	defer originalNs.Close()

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("failed to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	targetNs, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("failed to create network namespace: %v", err)
	}
	defer targetNs.Close()
	defer func() { _ = netns.DeleteNamed(nsName) }()

	if err := netns.Set(originalNs); err != nil {
		t.Fatalf("failed to restore original network namespace: %v", err)
	}

	ifaceName := fmt.Sprintf("td%x", rndString)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ifaceName}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("failed to add dummy link: %v", err)
	}
	t.Cleanup(func() {
		link, err := nlwrap.LinkByName(ifaceName)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to bring dummy link up: %v", err)
	}

	containerNsPath := path.Join("/run/netns", nsName)
	if _, err := nsAttachNetdev(ifaceName, containerNsPath, apis.InterfaceConfig{Name: "dranet0"}); err != nil {
		t.Fatalf("failed to attach dummy link: %v", err)
	}
	if err := netns.DeleteNamed(nsName); err != nil {
		t.Fatalf("failed to remove network namespace path: %v", err)
	}
	if err := nsDetachNetdevFromNS(targetNs, containerNsPath, "dranet0", ifaceName); err != nil {
		t.Fatalf("failed to detach with open namespace handle: %v", err)
	}

	returnedDev, err := nlwrap.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("network device was not returned to the host: %v", err)
	}
	if returnedDev.Attrs().Flags&net.FlagUp == 0 {
		t.Error("network device was not brought up after detach")
	}
}
