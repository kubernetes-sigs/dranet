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
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestReadinessBudget(t *testing.T) {
	const (
		max     = 1500 * time.Millisecond
		reserve = 500 * time.Millisecond
	)

	tests := []struct {
		name         string
		deadline     time.Duration // 0 means no deadline
		want         time.Duration
		wantExceeded bool
	}{
		{
			name: "no deadline falls back to the maximum",
			want: max,
		},
		{
			name:     "a deadline beyond the maximum does not extend the wait",
			deadline: 10 * time.Second,
			want:     max,
		},
		{
			name:     "a tight deadline shortens the wait by the reserve",
			deadline: 900 * time.Millisecond,
			want:     400 * time.Millisecond,
		},
		{
			// Time to roll back, not to wait as well: a zero budget makes the
			// caller roll back at once and stay inside the request deadline.
			name:     "a deadline inside the reserve leaves no budget",
			deadline: 300 * time.Millisecond,
			want:     0,
		},
		{
			// Nothing is left to protect with a rollback once the runtime has
			// stopped waiting for the request, so the caller gets the full
			// budget and a heads-up instead of being told to give up.
			name:         "an expired deadline is treated as exceeded",
			deadline:     -time.Second,
			want:         max,
			wantExceeded: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.deadline != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(tt.deadline))
				defer cancel()
			}

			got, exceeded := readinessBudget(ctx, max, reserve)
			if exceeded != tt.wantExceeded {
				t.Errorf("readinessBudget() deadlineExceeded = %v, want %v", exceeded, tt.wantExceeded)
			}
			// time.Until loses a little to the clock between the two calls.
			if diff := tt.want - got; diff < 0 || diff > 50*time.Millisecond {
				t.Errorf("readinessBudget() = %v, want approximately %v", got, tt.want)
			}
		})
	}
}

func TestAutoconfiguredAddresses(t *testing.T) {
	addr := func(cidr string, flags int) netlink.Addr {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("bad test address %s: %v", cidr, err)
		}
		return netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask}, Flags: flags}
	}

	tests := []struct {
		name      string
		addresses []netlink.Addr
		want      []string
	}{
		{
			name: "no addresses",
		},
		{
			name:      "an autoconfigured address is ready",
			addresses: []netlink.Addr{addr("2001:db8::1/64", 0)},
			want:      []string{"2001:db8::1/64"},
		},
		{
			name:      "a link-local address is not enough",
			addresses: []netlink.Addr{addr("fe80::1/64", 0)},
		},
		{
			name:      "a tentative address is still running duplicate address detection",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_TENTATIVE)},
		},
		{
			name:      "an address that failed duplicate address detection is unusable",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_DADFAILED)},
		},
		{
			name:      "a permanent address did not come from a router advertisement",
			addresses: []netlink.Addr{addr("2001:db8::1/64", unix.IFA_F_PERMANENT)},
		},
		{
			name: "the ready addresses are picked out of a mixed list",
			addresses: []netlink.Addr{
				addr("fe80::1/64", unix.IFA_F_PERMANENT),
				addr("2001:db8::1/64", unix.IFA_F_TENTATIVE),
				addr("fdcd:8200:cde5:20b7::708/64", unix.IFA_F_MANAGETEMPADDR),
			},
			want: []string{"fdcd:8200:cde5:20b7::708/64"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, autoconfiguredAddresses(tt.addresses)); diff != "" {
				t.Errorf("autoconfiguredAddresses() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDescribeAddresses(t *testing.T) {
	if got := describeAddresses(nil); got != "none" {
		t.Errorf("describeAddresses(nil) = %q, want %q", got, "none")
	}
	addresses := []netlink.Addr{
		{IPNet: &net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}, Flags: unix.IFA_F_PERMANENT},
		{IPNet: &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)}, Flags: unix.IFA_F_TENTATIVE},
	}
	want := "fe80::1(permanent) 2001:db8::1(tentative)"
	if got := describeAddresses(addresses); got != want {
		t.Errorf("describeAddresses() = %q, want %q", got, want)
	}
}

// testNetns creates a named network namespace with a dummy interface up inside
// it, standing in for a Pod namespace, and returns the namespace path, a handle
// to it and the interface name. Everything is removed when the test ends.
func testNetns(t *testing.T) (string, netns.NsHandle, string) {
	t.Helper()

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("failed to get the current namespace: %v", err)
	}
	t.Cleanup(func() { origns.Close() })

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("failed to generate a random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	// NewNamed unshares the calling OS thread and leaves it in the new
	// namespace. Pin the goroutine to that thread until the original namespace
	// is restored, so the restore lands on the thread that was moved.
	runtime.LockOSThread()
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("failed to create a network namespace: %v", err)
	}
	t.Cleanup(func() {
		testNS.Close()
		netns.DeleteNamed(nsName)
	})
	if err := netns.Set(origns); err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("failed to restore the original namespace: %v", err)
	}
	runtime.UnlockOSThread()

	ifName := "slaac0"
	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	t.Cleanup(nhNs.Close)

	la := netlink.NewLinkAttrs()
	la.Name = ifName
	if err := nhNs.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", ifName, err)
	}
	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifName, err)
	}
	if err := nhNs.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set up link %s: %v", ifName, err)
	}
	return path.Join("/run/netns", nsName), testNS, ifName
}

// addHostDummy creates a dummy interface on the host side of the test and
// removes it again at the end, wherever it has ended up.
func addHostDummy(t *testing.T, name string) {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = name
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link %s: %v", name, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
}

func TestWaitForSLAAC(t *testing.T) {
	userns.Run(t, testWaitForSLAAC_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAAC_Namespaced(t *testing.T) {
	_, testNS, ifName := testNetns(t)

	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifName, err)
	}

	// A router advertisement arrives while the wait is already running. Lifetimes
	// are what make the kernel treat the address as dynamic rather than
	// permanent, the same as an autoconfigured one.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = nhNs.AddrAdd(link, &netlink.Addr{
			IPNet:       &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
			ValidLft:    3600,
			PreferedLft: 1800,
			Flags:       unix.IFA_F_NODAD,
		})
	}()

	addresses, err := waitForSLAAC(context.Background(), testNS, ifName, time.Second)
	if err != nil {
		t.Fatalf("waitForSLAAC() error: %v", err)
	}
	if diff := cmp.Diff([]string{"2001:db8::1/64"}, addresses); diff != "" {
		t.Errorf("waitForSLAAC() addresses mismatch (-want +got):\n%s", diff)
	}
}

func TestWaitForSLAACTimesOut(t *testing.T) {
	userns.Run(t, testWaitForSLAACTimesOut_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAACTimesOut_Namespaced(t *testing.T) {
	_, testNS, ifName := testNetns(t)

	start := time.Now()
	_, err := waitForSLAAC(context.Background(), testNS, ifName, 150*time.Millisecond)
	if err == nil {
		t.Fatal("waitForSLAAC() succeeded on an interface with no router, want an error")
	}
	if !strings.Contains(err.Error(), ifName) {
		t.Errorf("waitForSLAAC() error %q does not name the interface %s", err, ifName)
	}
	// The wait must honour its budget rather than block the runtime request.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitForSLAAC() took %v, want it to give up near its 150ms budget", elapsed)
	}
}

func TestWaitForSLAACUnknownInterface(t *testing.T) {
	userns.Run(t, testWaitForSLAACUnknownInterface_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testWaitForSLAACUnknownInterface_Namespaced(t *testing.T) {
	_, testNS, _ := testNetns(t)

	if _, err := waitForSLAAC(context.Background(), testNS, "nosuchif", time.Second); err == nil {
		t.Fatal("waitForSLAAC() succeeded for an interface that does not exist, want an error")
	}
}

// TestAwaitSLAACReadyRollsBack drives the full path: an interface is moved into
// a Pod namespace, autoconfiguration never completes because nothing answers on
// the link, and the interface has to come back to the host.
func TestAwaitSLAACReadyRollsBack(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyRollsBack_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyRollsBack_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost0"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod0"
	config := apis.InterfaceConfig{
		Name:                    ifNameInNs,
		Addressing:              apis.AddressingModeSLAAC,
		AcceptRA:                ptr.To[int32](2),
		DADTransmits:            ptr.To[int32](0),
		RouterSolicitationDelay: ptr.To[int32](0),
	}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	start := time.Now()
	_, err := awaitSLAACReady(context.Background(), containerNsPath, ifNameInNs, hostIfName, 150*time.Millisecond, DefaultSLAACRollbackReserve)
	if err == nil {
		t.Fatal("awaitSLAACReady() succeeded with no router on the link, want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("awaitSLAACReady() error %q does not wrap context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("awaitSLAACReady() took %v, want it to give up near its 150ms budget", elapsed)
	}

	// The rollback must leave the interface on the host, under its original name
	// and up, so the next attempt at the sandbox starts from the same state.
	link, err := nlwrap.LinkByName(hostIfName)
	if err != nil {
		t.Fatalf("interface %s was not rolled back to the host: %v", hostIfName, err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Errorf("interface %s was rolled back to the host but left down", hostIfName)
	}
}

// TestAwaitSLAACReadyPastDeadline checks that a request whose deadline has
// already passed still finishes the wait, instead of giving up on an interface
// in a Pod the runtime is going to start anyway.
func TestAwaitSLAACReadyPastDeadline(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyPastDeadline_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyPastDeadline_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost1"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod1"
	config := apis.InterfaceConfig{Name: ifNameInNs, Addressing: apis.AddressingModeSLAAC}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	containerNs, err := netns.GetFromPath(containerNsPath)
	if err != nil {
		t.Fatalf("failed to open the container namespace: %v", err)
	}
	defer containerNs.Close()
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatalf("failed to open a netlink handle: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifNameInNs)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", ifNameInNs, err)
	}

	// The address turns up after the deadline has already passed.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = nhNs.AddrAdd(link, &netlink.Addr{
			IPNet:       &net.IPNet{IP: net.ParseIP("2001:db8::2"), Mask: net.CIDRMask(64, 128)},
			ValidLft:    3600,
			PreferedLft: 1800,
			Flags:       unix.IFA_F_NODAD,
		})
	}()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	addresses, err := awaitSLAACReady(ctx, containerNsPath, ifNameInNs, hostIfName, time.Second, DefaultSLAACRollbackReserve)
	if err != nil {
		t.Fatalf("awaitSLAACReady() gave up after the deadline passed: %v", err)
	}
	if diff := cmp.Diff([]string{"2001:db8::2/64"}, addresses); diff != "" {
		t.Errorf("awaitSLAACReady() addresses mismatch (-want +got):\n%s", diff)
	}
	// The interface must stay in the Pod namespace, not be rolled back.
	if _, err := nlwrap.LinkByName(hostIfName); err == nil {
		t.Errorf("interface %s was rolled back to the host even though autoconfiguration succeeded", hostIfName)
	}
}

// TestAwaitSLAACReadyWithinReserveRollsBack checks the middle case: the request
// is still live but has no more than the rollback reserve left, so there is no
// time to wait. The check runs once, and the interface goes straight back to
// the host, inside the deadline.
func TestAwaitSLAACReadyWithinReserveRollsBack(t *testing.T) {
	userns.Run(t, testAwaitSLAACReadyWithinReserveRollsBack_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAwaitSLAACReadyWithinReserveRollsBack_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	hostIfName := "slaachost2"
	addHostDummy(t, hostIfName)

	ifNameInNs := "slaacpod2"
	config := apis.InterfaceConfig{Name: ifNameInNs, Addressing: apis.AddressingModeSLAAC}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	// Less than the reserve remains, so the wait must not start.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	defer cancel()

	start := time.Now()
	_, err := awaitSLAACReady(ctx, containerNsPath, ifNameInNs, hostIfName, time.Second, DefaultSLAACRollbackReserve)
	if err == nil {
		t.Fatal("awaitSLAACReady() succeeded with no time to wait, want an error")
	}
	// A single check and a rollback of a dummy interface: nowhere near the 1s
	// wait it must not have started.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("awaitSLAACReady() took %v, want it to roll back without waiting", elapsed)
	}
	if _, err := nlwrap.LinkByName(hostIfName); err != nil {
		t.Fatalf("interface %s was not rolled back to the host: %v", hostIfName, err)
	}
}

// TestSLAACFromRouterAdvertisement drives the real kernel path rather than
// adding the address by hand: one end of a veth pair moves into the Pod
// namespace with the settings SLAAC defaults to, a router advertisement
// arrives on its link from the other end, and the wait returns the address the
// kernel derived from the advertised prefix.
func TestSLAACFromRouterAdvertisement(t *testing.T) {
	userns.Run(t, testSLAACFromRouterAdvertisement_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testSLAACFromRouterAdvertisement_Namespaced(t *testing.T) {
	containerNsPath, _, _ := testNetns(t)

	// One end plays the rail NIC and moves into the Pod; the other stays here
	// and plays the router.
	hostIfName, routerIfName := "slaacnic0", "slaacrtr0"
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostIfName}, PeerName: routerIfName}); err != nil {
		t.Fatalf("failed to add veth pair %s/%s: %v", hostIfName, routerIfName, err)
	}
	t.Cleanup(func() {
		// Deleting either end removes the pair, wherever the other end is.
		if link, err := nlwrap.LinkByName(routerIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	router, err := nlwrap.LinkByName(routerIfName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", routerIfName, err)
	}
	// The router needs a link-local source address that is not tentative to
	// send from. Skipping its duplicate address detection is a shortcut; if
	// it is not allowed here, the sender below retries until detection ends.
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+routerIfName+"/dad_transmits", []byte("0"), 0o644)
	if err := netlink.LinkSetUp(router); err != nil {
		t.Fatalf("failed to set up link %s: %v", routerIfName, err)
	}

	ifNameInNs := "slaacpod3"
	config := apis.InterfaceConfig{
		Name:                    ifNameInNs,
		Addressing:              apis.AddressingModeSLAAC,
		AcceptRA:                ptr.To[int32](2),
		DADTransmits:            ptr.To[int32](0),
		RouterSolicitationDelay: ptr.To[int32](0),
	}
	if _, err := nsAttachNetdev(hostIfName, containerNsPath, config); err != nil {
		t.Fatalf("nsAttachNetdev() error: %v", err)
	}

	_, prefix, err := net.ParseCIDR("2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go advertisePrefix(t, router.Attrs().Index, *prefix, stop)

	addresses, err := awaitSLAACReady(context.Background(), containerNsPath, ifNameInNs, hostIfName, 3*time.Second, DefaultSLAACRollbackReserve)
	if err != nil {
		t.Fatalf("awaitSLAACReady() error: %v", err)
	}
	if len(addresses) != 1 || !strings.HasPrefix(addresses[0], "2001:db8:1:") || !strings.HasSuffix(addresses[0], "/64") {
		t.Errorf("awaitSLAACReady() = %v, want one address in 2001:db8:1::/64", addresses)
	}
}

// advertisePrefix sends an unsolicited Router Advertisement for prefix out of
// the interface with index ifIndex every 50ms until stop is closed. It reports
// failures through t.Log only: the test that uses it fails on the wait instead,
// with the diagnostic the wait produces.
func advertisePrefix(t *testing.T, ifIndex int, prefix net.IPNet, stop <-chan struct{}) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		t.Logf("failed to open a raw ICMPv6 socket: %v", err)
		return
	}
	defer unix.Close(fd)
	// Neighbor discovery messages are only valid with a hop limit of 255.
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_HOPS, 255); err != nil {
		t.Logf("failed to set the multicast hop limit: %v", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, ifIndex); err != nil {
		t.Logf("failed to bind multicast sends to interface %d: %v", ifIndex, err)
	}
	allNodes := &unix.SockaddrInet6{ZoneId: uint32(ifIndex)}
	copy(allNodes.Addr[:], net.ParseIP("ff02::1").To16())

	packet := routerAdvertisement(prefix)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// EADDRNOTAVAIL until the router's link-local address is usable;
			// the next tick tries again.
			_ = unix.Sendto(fd, packet, 0, allNodes)
		}
	}
}

// routerAdvertisement builds an ICMPv6 Router Advertisement (RFC 4861 section
// 4.2) carrying one Prefix Information option (section 4.6.2) with the
// autonomous flag set, which is what makes a host derive an address from it.
// The kernel fills in the checksum for raw ICMPv6 sockets.
func routerAdvertisement(prefix net.IPNet) []byte {
	ra := make([]byte, 16+32)
	ra[0] = 134                               // Router Advertisement
	ra[4] = 64                                // current hop limit
	binary.BigEndian.PutUint16(ra[6:8], 1800) // router lifetime, seconds

	option := ra[16:]
	option[0] = 3 // Prefix Information
	option[1] = 4 // length, in units of 8 octets
	ones, _ := prefix.Mask.Size()
	option[2] = byte(ones)
	option[3] = 0xc0                               // on-link, autonomous
	binary.BigEndian.PutUint32(option[4:8], 3600)  // valid lifetime
	binary.BigEndian.PutUint32(option[8:12], 1800) // preferred lifetime
	copy(option[16:32], prefix.IP.To16())
	return ra
}

func TestAttachBudget(t *testing.T) {
	const floor = 500 * time.Millisecond
	tests := []struct {
		name     string
		deadline time.Duration // 0 means no deadline
		longest  time.Duration
		wantErr  bool
	}{
		{name: "no deadline never blocks", longest: 2 * time.Second},
		{name: "first interface only needs the floor", deadline: 600 * time.Millisecond},
		{name: "first interface with less than the floor left is not moved", deadline: 400 * time.Millisecond, wantErr: true},
		{name: "a move and a rollback of the same size must both fit", deadline: time.Second, longest: 600 * time.Millisecond, wantErr: true},
		{name: "enough time for a move and a rollback", deadline: 2 * time.Second, longest: 600 * time.Millisecond},
		{name: "a passed deadline is let through, the runtime is not waiting", deadline: -time.Second, longest: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.deadline != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(tt.deadline))
				defer cancel()
			}
			budget := &attachBudget{}
			budget.record(tt.longest)
			err := budget.check(ctx, "eth0", floor)
			if (err != nil) != tt.wantErr {
				t.Errorf("check() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestAttachNetdevToNSRoutesAfterSLAAC drives attachNetdevToNS itself, with a
// router on the other end of a veth pair: a route through a gateway in the
// advertised prefix can only be installed once the prefix is on the link, so
// the routes have to go in after the wait. It also covers the budget gate and
// the status the hook records.
func TestAttachNetdevToNSRoutesAfterSLAAC(t *testing.T) {
	userns.Run(t, testAttachNetdevToNSRoutesAfterSLAAC_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testAttachNetdevToNSRoutesAfterSLAAC_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)

	hostIfName, routerIfName := "slaacnic1", "slaacrtr1"
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostIfName}, PeerName: routerIfName}); err != nil {
		t.Fatalf("failed to add veth pair %s/%s: %v", hostIfName, routerIfName, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(routerIfName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	router, err := nlwrap.LinkByName(routerIfName)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+routerIfName+"/dad_transmits", []byte("0"), 0o644)
	if err := netlink.LinkSetUp(router); err != nil {
		t.Fatalf("failed to set up link %s: %v", routerIfName, err)
	}
	_, prefix, err := net.ParseCIDR("2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go advertisePrefix(t, router.Attrs().Index, *prefix, stop)

	ifNameInNs := "slaacpod7"
	config := DeviceConfig{
		NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: hostIfName}},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:                    ifNameInNs,
				Addressing:              apis.AddressingModeSLAAC,
				AcceptRA:                ptr.To[int32](2),
				DADTransmits:            ptr.To[int32](0),
				RouterSolicitationDelay: ptr.To[int32](0),
			},
			// Through a gateway in the advertised prefix, which is unreachable
			// until the advertisement has put the prefix on the link.
			Routes: []apis.RouteConfig{{Destination: "2001:db8:99::/64", Gateway: "2001:db8:1::1"}},
		},
	}
	status := resourceapply.AllocatedDeviceStatus()
	budget := &attachBudget{}
	if err := attachNetdevToNS(context.Background(), containerNsPath, "dev7", config, status, 3*time.Second, DefaultSLAACRollbackReserve, budget); err != nil {
		t.Fatalf("attachNetdevToNS() error: %v", err)
	}
	if budget.longest == 0 {
		t.Error("attachNetdevToNS() did not record the move in the budget")
	}

	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatal(err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(ifNameInNs)
	if err != nil {
		t.Fatalf("interface %s is not in the Pod namespace: %v", ifNameInNs, err)
	}
	routes, err := nhNs.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_OIF)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, route := range routes {
		if route.Dst != nil && route.Dst.String() == "2001:db8:99::/64" {
			found = true
		}
	}
	if !found {
		t.Errorf("route 2001:db8:99::/64 via the advertised prefix was not installed; routes: %v", routes)
	}

	if status.NetworkData == nil || len(status.NetworkData.IPs) != 1 || !strings.HasPrefix(status.NetworkData.IPs[0], "2001:db8:1:") {
		t.Errorf("status network data = %+v, want one address in 2001:db8:1::/64", status.NetworkData)
	}
	var conditions []string
	for _, c := range status.Conditions {
		conditions = append(conditions, *c.Type)
	}
	sort.Strings(conditions)
	if diff := cmp.Diff([]string{"NetworkReady", "Ready", "SLAACReady"}, conditions); diff != "" {
		t.Errorf("status conditions mismatch (-want +got):\n%s", diff)
	}
}

// disableIPv6ByDefault makes interfaces created in or moved into the namespace
// start with IPv6 off, as some CNI plugins leave a Pod namespace on IPv4-only
// clusters.
func disableIPv6ByDefault(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("failed to get the current namespace: %v", err)
	}
	defer origns.Close()
	if err := netns.Set(ns); err != nil {
		t.Fatalf("failed to join the test namespace: %v", err)
	}
	writeErr := os.WriteFile("/proc/sys/net/ipv6/conf/default/disable_ipv6", []byte("1"), 0o644)
	if err := netns.Set(origns); err != nil {
		t.Fatalf("failed to restore the original namespace: %v", err)
	}
	if writeErr != nil {
		t.Fatalf("failed to disable IPv6 by default in the test namespace: %v", writeErr)
	}
}

// addRail creates a veth pair: the first end plays an RDMA NIC that stays on
// the host as the parent of a subinterface, the second plays its router.
func addRail(t *testing.T, nicName, routerName string) netlink.Link {
	t.Helper()
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: nicName}, PeerName: routerName}); err != nil {
		t.Fatalf("failed to add veth pair %s/%s: %v", nicName, routerName, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(routerName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+routerName+"/dad_transmits", []byte("0"), 0o644)
	router, err := nlwrap.LinkByName(routerName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", routerName, err)
	}
	if err := netlink.LinkSetUp(router); err != nil {
		t.Fatalf("failed to set up link %s: %v", routerName, err)
	}
	// A veth end has carrier, and with it a link-local address, only once its
	// peer is up too. The router can only advertise from a usable link-local
	// address; wait for it, so a test's wait budget is not spent on the router
	// coming up.
	nic, err := nlwrap.LinkByName(nicName)
	if err != nil {
		t.Fatalf("failed to get link %s: %v", nicName, err)
	}
	if err := netlink.LinkSetUp(nic); err != nil {
		t.Fatalf("failed to set up link %s: %v", nicName, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		addrs, err := nlwrap.AddrList(router, netlink.FAMILY_V6)
		if err != nil {
			t.Fatalf("failed to list addresses of %s: %v", routerName, err)
		}
		for _, a := range addrs {
			if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				return router
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("router %s has no usable link-local address: %v", routerName, addrs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// slaacSubinterfaceConfig is the configuration of an IPVLAN child with SLAAC
// addressing, defaulted the way the merged claim configuration is.
func slaacSubinterfaceConfig(parent, name string) DeviceConfig {
	config := DeviceConfig{
		NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: parent}},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{Interface: apis.InterfaceConfig{
			Name:       name,
			Type:       apis.InterfaceTypeIPVLAN,
			Addressing: apis.AddressingModeSLAAC,
		}},
	}
	config.NetworkInterfaceConfigInPod.Default()
	return config
}

func TestSubinterfacesSLAACFromRouterAdvertisements(t *testing.T) {
	userns.Run(t, testSubinterfacesSLAACFromRouterAdvertisements_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// Two IPVLAN children on two rails, in a namespace with IPv6 off by default,
// are waited for together and each gets an address from its own rail.
func testSubinterfacesSLAACFromRouterAdvertisements_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)
	disableIPv6ByDefault(t, containerNs)

	prefixes := []string{"2001:db8:10::/64", "2001:db8:11::/64"}
	stop := make(chan struct{})
	defer close(stop)
	var pending []*pendingSubinterface
	for i, cidr := range prefixes {
		nic, rtr := fmt.Sprintf("rail%d", i), fmt.Sprintf("rtr%d", i)
		router := addRail(t, nic, rtr)
		_, prefix, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		go advertisePrefix(t, router.Attrs().Index, *prefix, stop)

		status := resourceapply.AllocatedDeviceStatus()
		p, err := createSubinterfaceInNS(context.Background(), containerNsPath, nic, slaacSubinterfaceConfig(nic, fmt.Sprintf("rdma%d", i)), status)
		if err != nil {
			t.Fatalf("createSubinterfaceInNS(%s) error: %v", nic, err)
		}
		if p == nil {
			t.Fatalf("createSubinterfaceInNS(%s) returned no pending subinterface for addressing SLAAC", nic)
		}
		if len(status.Conditions) != 0 {
			t.Errorf("status of %s before the wait has %d conditions, want none", nic, len(status.Conditions))
		}
		pending = append(pending, p)
	}

	// The shipped defaults: they have to get the addresses inside
	// --slaac-ready-timeout, not just eventually.
	results, pastDeadline, err := awaitSubinterfacesSLAAC(context.Background(), containerNsPath, pending, DefaultSLAACReadyTimeout, DefaultSLAACRollbackReserve)
	if err != nil || pastDeadline {
		t.Fatalf("awaitSubinterfacesSLAAC() = pastDeadline %v, error %v", pastDeadline, err)
	}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("subinterface %d: %v", i, r.err)
		}
		want := strings.TrimSuffix(prefixes[i], ":/64")
		if len(r.addresses) != 1 || !strings.HasPrefix(r.addresses[0], want) {
			t.Errorf("subinterface %d addresses = %v, want one in %s", i, r.addresses, prefixes[i])
		}
		if err := finishSubinterfaceInNS(context.Background(), containerNsPath, pending[i], r.addresses); err != nil {
			t.Fatalf("finishSubinterfaceInNS(%d) error: %v", i, err)
		}
		types := map[string]bool{}
		for _, c := range pending[i].status.Conditions {
			types[*c.Type] = true
		}
		if !types["SLAACReady"] || !types["Ready"] || !types["NetworkReady"] {
			t.Errorf("subinterface %d conditions = %v, want SLAACReady, NetworkReady and Ready", i, types)
		}
		if pending[i].status.NetworkData == nil || len(pending[i].status.NetworkData.IPs) != 1 {
			t.Errorf("subinterface %d network data = %+v, want the autoconfigured address", i, pending[i].status.NetworkData)
		}
	}
}

func TestSubinterfacesSLAACTimeoutDeletesChildren(t *testing.T) {
	userns.Run(t, testSubinterfacesSLAACTimeoutDeletesChildren_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// Without router advertisements the wait fails and every child of the request
// is deleted, so the kubelet's retry starts from a clean namespace.
func testSubinterfacesSLAACTimeoutDeletesChildren_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)
	var pending []*pendingSubinterface
	for i := range 2 {
		nic := fmt.Sprintf("rail%d", i)
		addRail(t, nic, fmt.Sprintf("rtr%d", i))
		p, err := createSubinterfaceInNS(context.Background(), containerNsPath, nic, slaacSubinterfaceConfig(nic, fmt.Sprintf("rdma%d", i)), resourceapply.AllocatedDeviceStatus())
		if err != nil || p == nil {
			t.Fatalf("createSubinterfaceInNS(%s) = %v, %v; want a pending subinterface", nic, p, err)
		}
		pending = append(pending, p)
	}

	results, pastDeadline, err := awaitSubinterfacesSLAAC(context.Background(), containerNsPath, pending, 300*time.Millisecond, 100*time.Millisecond)
	if err != nil || pastDeadline {
		t.Fatalf("awaitSubinterfacesSLAAC() = pastDeadline %v, deletion error %v", pastDeadline, err)
	}
	for i, r := range results {
		if r.err == nil {
			t.Errorf("subinterface %d: want a timeout error without router advertisements", i)
		}
	}
	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatal(err)
	}
	defer nhNs.Close()
	for _, p := range pending {
		if _, err := nhNs.LinkByName(p.networkData.InterfaceName); err == nil {
			t.Errorf("subinterface %s still exists after the failed wait", p.networkData.InterfaceName)
		}
	}
}
