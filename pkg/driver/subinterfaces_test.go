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
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/apimachinery/pkg/types"
	resourceapply "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/component-helpers/node/util/sysctl"
	sysctltesting "k8s.io/component-helpers/node/util/sysctl/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

const testParentMAC = "00:11:22:33:44:55"

// ipvlanTestEnv is a dedicated network namespace plus a dummy parent
// interface on the host. It needs root.
type ipvlanTestEnv struct {
	origns netns.NsHandle
	testNS netns.NsHandle
	nsPath string
	parent string
}

func newIPVlanTestEnv(t *testing.T, parentMTU int) *ipvlanTestEnv {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	t.Cleanup(func() { origns.Close() })

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	// NewNamed switches the calling thread into the new namespace, so keep the
	// goroutine on one thread until it is switched back.
	runtime.LockOSThread()
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	t.Cleanup(func() {
		testNS.Close()
		_ = netns.DeleteNamed(nsName)
	})
	setErr := netns.Set(origns)
	runtime.UnlockOSThread()
	if setErr != nil {
		t.Fatalf("failed to switch back to the host namespace: %v", setErr)
	}

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

	// A fixed MAC and MTU on the parent show what the child inherits.
	parent := fmt.Sprintf("tdummy-%x", rndString)
	la := netlink.NewLinkAttrs()
	la.Name = parent
	la.HardwareAddr, _ = net.ParseMAC(testParentMAC)
	la.MTU = parentMTU
	link := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s: %v", parent, err)
	}
	t.Cleanup(func() {
		if link, err := nlwrap.LinkByName(parent); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to set up dummy link %s on host: %v", parent, err)
	}

	return &ipvlanTestEnv{origns: origns, testNS: testNS, nsPath: path.Join("/run/netns", nsName), parent: parent}
}

// inNS runs fn with the current thread switched into the test namespace.
func (e *ipvlanTestEnv) inNS(t *testing.T, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(e.testNS); err != nil {
		t.Fatal(err)
	}
	defer netns.Set(e.origns)
	fn()
}

// linkNames returns the interface names present in the test namespace.
func (e *ipvlanTestEnv) linkNames(t *testing.T) []string {
	t.Helper()
	nhNs, err := nlwrap.NewHandleAt(e.testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()
	links, err := nhNs.LinkList()
	if err != nil {
		t.Fatalf("failed to list links in the test namespace: %v", err)
	}
	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.Attrs().Name)
	}
	return names
}

// linkAttrs reads the attributes of a link in the test namespace through netlink.
func (e *ipvlanTestEnv) linkAttrs(t *testing.T, name string) *netlink.LinkAttrs {
	t.Helper()
	nhNs, err := nlwrap.NewHandleAt(e.testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()
	link, err := nhNs.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to find %s in the test namespace: %v", name, err)
	}
	return link.Attrs()
}

func assertOnlyLoopback(t *testing.T, env *ipvlanTestEnv) {
	t.Helper()
	names := env.linkNames(t)
	if len(names) != 1 || names[0] != "lo" {
		t.Errorf("expected only lo in the test namespace after the failure, got %v", names)
	}
}

func TestSubinterface_IPVlan(t *testing.T) {
	env := newIPVlanTestEnv(t, 1400)

	config := apis.InterfaceConfig{
		Name:           "dranet0",
		Type:           apis.InterfaceTypeIPVLAN,
		Addresses:      []string{"2001:db8::3/128"},
		MTU:            ptr.To[int32](1300),
		GSOMaxSize:     ptr.To[int32](1024),
		GROMaxSize:     ptr.To[int32](1025),
		GSOIPv4MaxSize: ptr.To[int32](1026),
		GROIPv4MaxSize: ptr.To[int32](1027),
		ARPIgnore:      ptr.To[int32](1),
		ARPAnnounce:    ptr.To[int32](2),
	}

	deviceData, err := nsCreateSubinterface(env.parent, env.nsPath, config)
	if err != nil {
		t.Fatalf("fail to create subinterface: %v", err)
	}

	if deviceData.InterfaceName != config.Name {
		t.Errorf("Expected reported InterfaceName %q, got %q", config.Name, deviceData.InterfaceName)
	}
	if deviceData.HardwareAddress != testParentMAC {
		t.Errorf("Expected reported HardwareAddress %q, got %q", testParentMAC, deviceData.HardwareAddress)
	}
	if len(deviceData.IPs) != 1 || deviceData.IPs[0] != config.Addresses[0] {
		t.Errorf("Expected reported IPs %v, got %v", config.Addresses, deviceData.IPs)
	}

	// The parent interface stays in the host namespace.
	if _, err := nlwrap.LinkByName(env.parent); err != nil {
		t.Errorf("expected parent interface %s to still exist in the host namespace: %v", env.parent, err)
	}

	// Link settings are read back through netlink, which does not depend on the
	// iproute2 version like `ip -d link` does for the IPv4 offload sizes.
	attrs := env.linkAttrs(t, config.Name)
	if attrs.MTU != int(*config.MTU) {
		t.Errorf("mtu = %d, want %d", attrs.MTU, *config.MTU)
	}
	if attrs.GSOMaxSize != uint32(*config.GSOMaxSize) {
		t.Errorf("gso_max_size = %d, want %d", attrs.GSOMaxSize, *config.GSOMaxSize)
	}
	if attrs.GROMaxSize != uint32(*config.GROMaxSize) {
		t.Errorf("gro_max_size = %d, want %d", attrs.GROMaxSize, *config.GROMaxSize)
	}
	if attrs.GSOIPv4MaxSize != uint32(*config.GSOIPv4MaxSize) {
		t.Errorf("gso_ipv4_max_size = %d, want %d", attrs.GSOIPv4MaxSize, *config.GSOIPv4MaxSize)
	}
	if attrs.GROIPv4MaxSize != uint32(*config.GROIPv4MaxSize) {
		t.Errorf("gro_ipv4_max_size = %d, want %d", attrs.GROIPv4MaxSize, *config.GROIPv4MaxSize)
	}
	// The child always keeps the parent MAC.
	if attrs.HardwareAddr.String() != testParentMAC {
		t.Errorf("hardware address = %s, want the parent MAC %s", attrs.HardwareAddr, testParentMAC)
	}

	env.inNS(t, func() {
		output, err := exec.Command("ip", "-d", "link", "show", config.Name).CombinedOutput()
		if err != nil {
			t.Fatalf("failed to show link properties: %v", err)
		}
		outputStr := string(output)
		for _, want := range []string{"ipvlan", "mode l2", "bridge"} {
			if !strings.Contains(outputStr, want) {
				t.Errorf("expected %q in the link output:\n%s", want, outputStr)
			}
		}

		// lo is the control: it shares the namespace but has no config, so it
		// shows the namespace default the child would have kept.
		for _, tc := range []struct {
			setting string
			want    int
		}{
			{"arp_ignore", int(*config.ARPIgnore)},
			{"arp_announce", int(*config.ARPAnnounce)},
		} {
			got, err := sysctl.New().GetSysctl(fmt.Sprintf("net/ipv4/conf/%s/%s", config.Name, tc.setting))
			if err != nil {
				t.Fatalf("failed to read %s in the pod namespace: %v", tc.setting, err)
			}
			if got != tc.want {
				t.Errorf("%s = %d, want %d", tc.setting, got, tc.want)
			}
			baseline, err := sysctl.New().GetSysctl(fmt.Sprintf("net/ipv4/conf/lo/%s", tc.setting))
			if err != nil {
				t.Fatalf("failed to read baseline %s in the pod namespace: %v", tc.setting, err)
			}
			if baseline == tc.want {
				t.Errorf("%s baseline is already %d, the test cannot prove the config was applied", tc.setting, baseline)
			}
		}

		output, err = exec.Command("ip", "addr", "show", config.Name).CombinedOutput()
		if err != nil {
			t.Fatalf("failed to show interface addresses: %v", err)
		}
		outputStr = string(output)
		for _, addr := range config.Addresses {
			if !strings.Contains(outputStr, addr) {
				t.Errorf("expected address %s not found in ip addr show:\n%s", addr, outputStr)
			}
		}
	})

	if err := nsDeleteSubinterface(env.nsPath, config.Name); err != nil {
		t.Fatalf("fail to delete subinterface: %v", err)
	}
	assertOnlyLoopback(t, env)
}

func TestSubinterface_IPVlanMTU(t *testing.T) {
	tests := []struct {
		name string
		mtu  *int32
		want int
	}{
		{name: "omitted inherits the parent MTU", mtu: nil, want: 1400},
		{name: "equal to the parent MTU", mtu: ptr.To[int32](1400), want: 1400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newIPVlanTestEnv(t, 1400)
			config := apis.InterfaceConfig{
				Name:      "dranet0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"2001:db8::3/128"},
				MTU:       tc.mtu,
			}
			if _, err := nsCreateSubinterface(env.parent, env.nsPath, config); err != nil {
				t.Fatalf("fail to create subinterface: %v", err)
			}
			if got := env.linkAttrs(t, config.Name).MTU; got != tc.want {
				t.Errorf("mtu = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSubinterface_IPVlanRejectsMTUAboveParent(t *testing.T) {
	env := newIPVlanTestEnv(t, 1400)
	config := apis.InterfaceConfig{
		Name:      "dranet0",
		Type:      apis.InterfaceTypeIPVLAN,
		Addresses: []string{"2001:db8::3/128"},
		MTU:       ptr.To[int32](1500),
	}

	_, err := nsCreateSubinterface(env.parent, env.nsPath, config)
	if err == nil || !strings.Contains(err.Error(), "exceeds parent interface") {
		t.Fatalf("nsCreateSubinterface() error = %v, want a parent MTU error", err)
	}
	assertOnlyLoopback(t, env)
}

func TestSubinterface_IPVlanRollsBackOnSysctlFailure(t *testing.T) {
	env := newIPVlanTestEnv(t, 1400)
	config := apis.InterfaceConfig{
		Name:      "dranet0",
		Type:      apis.InterfaceTypeIPVLAN,
		Addresses: []string{"2001:db8::3/128"},
		ARPIgnore: ptr.To[int32](1),
	}

	original := sysctlProvider
	sysctlProvider = func() sysctl.Interface {
		return &failingSetSysctl{
			Fake:    sysctltesting.NewFake(),
			setting: fmt.Sprintf("net/ipv4/conf/%s/arp_ignore", config.Name),
		}
	}
	t.Cleanup(func() { sysctlProvider = original })

	_, err := nsCreateSubinterface(env.parent, env.nsPath, config)
	if err == nil || !strings.Contains(err.Error(), "arp_ignore") {
		t.Fatalf("nsCreateSubinterface() error = %v, want an arp_ignore apply error", err)
	}
	assertOnlyLoopback(t, env)
}

func TestSubinterface_IPVlanRollsBackOnNameCollision(t *testing.T) {
	env := newIPVlanTestEnv(t, 1400)
	config := apis.InterfaceConfig{
		Name:      "dranet0",
		Type:      apis.InterfaceTypeIPVLAN,
		Addresses: []string{"2001:db8::3/128"},
	}

	// Occupy the requested name inside the pod namespace so the rename fails.
	nhNs, err := nlwrap.NewHandleAt(env.testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()
	la := netlink.NewLinkAttrs()
	la.Name = config.Name
	if err := nhNs.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add colliding dummy link: %v", err)
	}

	_, err = nsCreateSubinterface(env.parent, env.nsPath, config)
	if err == nil || !strings.Contains(err.Error(), "failed to rename interface") {
		t.Fatalf("nsCreateSubinterface() error = %v, want a rename error", err)
	}
	// Only lo and the pre-existing dummy remain, and the dummy is untouched.
	names := env.linkNames(t)
	slices.Sort(names)
	if want := []string{config.Name, "lo"}; !slices.Equal(names, want) {
		t.Errorf("links after the rename failure = %v, want %v", names, want)
	}
	existing, err := nhNs.LinkByName(config.Name)
	if err != nil {
		t.Fatalf("pre-existing link %s is gone after the rename failure: %v", config.Name, err)
	}
	if existing.Type() != "dummy" {
		t.Errorf("pre-existing link %s type = %s, want dummy", config.Name, existing.Type())
	}
}

func TestCreateSubinterfaceInNS_RollsBackOnConfigureFailure(t *testing.T) {
	env := newIPVlanTestEnv(t, 1400)
	deviceCfg := DeviceConfig{
		Claim: types.NamespacedName{Namespace: "ns", Name: "claim1"},
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: env.parent},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{
				Name:      "dranet0",
				Type:      apis.InterfaceTypeIPVLAN,
				Addresses: []string{"192.0.2.10/24"},
			},
			// The gateway is not on link, so the route cannot be applied.
			Routes: []apis.RouteConfig{{Destination: "198.51.100.0/24", Gateway: "203.0.113.1"}},
		},
	}

	err := createSubinterfaceInNS(context.Background(), env.nsPath, "net-dev-0", deviceCfg, resourceapply.AllocatedDeviceStatus())
	if err == nil || !strings.Contains(err.Error(), "error configuring device net-dev-0 routes") {
		t.Fatalf("createSubinterfaceInNS() error = %v, want a routes configuration error", err)
	}
	assertOnlyLoopback(t, env)
}

func TestNsDeleteSubinterfaceMissingNamespace(t *testing.T) {
	// A nonexistent namespace path counts as cleaned up and returns nil.
	nsPath := path.Join(t.TempDir(), "netns-gone")
	if err := nsDeleteSubinterface(nsPath, "rdma15"); err != nil {
		t.Fatalf("nsDeleteSubinterface() returned error for a missing namespace: %v", err)
	}
}
