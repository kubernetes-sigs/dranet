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
	"errors"
	"fmt"
	"net"
	"path"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func Test_applyRoutingConfig(t *testing.T) {
	userns.Run(t, test_applyRoutingConfig_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func test_applyRoutingConfig_Namespaced(t *testing.T) {
	// NewNamed moves the calling thread into the new namespace, so pin the
	// goroutine and put the thread back where we found it afterwards.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("fail to generate random name: %v", err)
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
	if err := netns.Set(origns); err != nil {
		t.Fatalf("failed to restore original network namespace: %v", err)
	}

	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()

	// A /32 address keeps the gateway off-link, so the universe route installs
	// only after the link route to it: that makes the scope ordering observable.
	ifaceName := "dummy0"
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	la.Namespace = netlink.NsFd(int(testNS))
	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := nhNs.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}
	if err := nhNs.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to set up dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}
	addr, err := netlink.ParseAddr("10.10.0.1/32")
	if err != nil {
		t.Fatalf("failed to parse address: %v", err)
	}
	if err := nhNs.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to %s: %v", ifaceName, err)
	}

	// Universe route first: without applyRoutingConfig sorting by scope, its
	// gateway is unreachable when it is added and the route fails to install.
	routes := []apis.RouteConfig{
		{Destination: "192.168.99.0/24", Gateway: "10.10.0.254", Scope: 0},
		{Destination: "10.10.0.254/32", Scope: 253},
	}
	if err := applyRoutingConfig(containerNsPath, ifaceName, routes, 0); err != nil {
		t.Fatalf("applyRoutingConfig failed: %v", err)
	}

	// applyRoutingConfig sorts a copy, so the shared input keeps its order.
	if routes[0].Destination != "192.168.99.0/24" || routes[1].Destination != "10.10.0.254/32" {
		t.Errorf("applyRoutingConfig reordered its input slice: %+v", routes)
	}

	nsLink, err := nhNs.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("Failed to get %s after applying routes: %v", ifaceName, err)
	}
	got, err := nhNs.RouteList(nsLink, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("failed to list routes: %v", err)
	}
	for _, want := range routes {
		found := false
		for _, r := range got {
			if r.Dst == nil || r.Dst.String() != want.Destination {
				continue
			}
			found = true
			gotGw := ""
			if r.Gw != nil {
				gotGw = r.Gw.String()
			}
			if gotGw != want.Gateway {
				t.Errorf("route %s gateway = %q, want %q", want.Destination, gotGw, want.Gateway)
			}
			if uint8(r.Scope) != want.Scope {
				t.Errorf("route %s scope = %d, want %d", want.Destination, r.Scope, want.Scope)
			}
			break
		}
		if !found {
			t.Errorf("route %s not found in namespace", want.Destination)
		}
	}
}

func Test_validateVRFLink(t *testing.T) {
	tests := []struct {
		name    string
		link    netlink.Link
		wantErr []string
	}{
		{
			name: "vrf with matching table",
			link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue"}, Table: 100},
		},
		{
			name:    "vrf with different table",
			link:    &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue"}, Table: 200},
			wantErr: []string{"vrf-blue", "100", "200"},
		},
		{
			name:    "non-vrf link",
			link:    &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue"}},
			wantErr: []string{"vrf-blue", "not a vrf", "dummy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVRFLink(tt.link, "vrf-blue", 100)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("validateVRFLink() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateVRFLink() succeeded, want error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validateVRFLink() error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func Test_applyVRFConfig(t *testing.T) {
	userns.Run(t, test_applyVRFConfig_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// newVRFTestNamespace creates a named network namespace containing a dummy0
// interface and returns its path together with a netlink handle into it.
func newVRFTestNamespace(t *testing.T) (string, nlwrap.Handle) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	if _, err := rand.Read(rndString); err != nil {
		t.Fatalf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	t.Cleanup(func() { netns.DeleteNamed(nsName) })
	t.Cleanup(func() { testNS.Close() })

	if err := netns.Set(origns); err != nil {
		t.Fatalf("failed to restore original network namespace: %v", err)
	}

	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	t.Cleanup(nhNs.Close)

	if err := nhNs.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}); err != nil {
		t.Fatalf("Failed to add dummy0 in ns %s: %v", nsName, err)
	}
	return path.Join("/run/netns", nsName), nhNs
}

// vrfSupported probes, in its own namespace, whether VRF devices can be
// created. Only EOPNOTSUPP, returned when the kernel has no vrf link type,
// counts as unsupported; any other failure fails the test.
func vrfSupported(t *testing.T) bool {
	t.Helper()
	_, nhNs := newVRFTestNamespace(t)
	err := nhNs.LinkAdd(&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-probe"}, Table: 1000})
	if errors.Is(err, syscall.EOPNOTSUPP) {
		return false
	}
	if err != nil {
		t.Fatalf("failed to probe vrf support: %v", err)
	}
	return true
}

func test_applyVRFConfig_Namespaced(t *testing.T) {
	const (
		ifName  = "dummy0"
		vrfName = "vrf-blue"
	)
	table := 100
	haveVRF := vrfSupported(t)

	tests := []struct {
		name string
		// existing is created in the namespace before applyVRFConfig runs.
		existing netlink.Link
		needsVRF bool
		wantErr  []string
	}{
		{
			name:     "creates missing vrf",
			needsVRF: true,
		},
		{
			name:     "reuses vrf with matching table",
			existing: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: vrfName}, Table: 100},
			needsVRF: true,
		},
		{
			name:     "rejects vrf with different table",
			existing: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: vrfName}, Table: 200},
			needsVRF: true,
			wantErr:  []string{vrfName, "100", "200"},
		},
		{
			name:     "rejects non-vrf device with vrf name",
			existing: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: vrfName}},
			wantErr:  []string{vrfName, "not a vrf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsVRF && !haveVRF {
				t.Skip("kernel does not support vrf devices (EOPNOTSUPP)")
			}
			nsPath, nhNs := newVRFTestNamespace(t)
			if tt.existing != nil {
				if err := nhNs.LinkAdd(tt.existing); err != nil {
					t.Fatalf("failed to add existing link %s: %v", vrfName, err)
				}
			}

			got, err := applyVRFConfig(nsPath, ifName, &apis.VRFConfig{Name: vrfName, Table: &table})
			if len(tt.wantErr) == 0 && err != nil {
				t.Fatalf("applyVRFConfig() unexpected error: %v", err)
			}
			if len(tt.wantErr) > 0 && err == nil {
				t.Fatalf("applyVRFConfig() succeeded with table %d, want error", got)
			}

			nsLink, lerr := nhNs.LinkByName(ifName)
			if lerr != nil {
				t.Fatalf("failed to get %s: %v", ifName, lerr)
			}
			vrfLink, lerr := nhNs.LinkByName(vrfName)
			if lerr != nil {
				t.Fatalf("failed to get %s: %v", vrfName, lerr)
			}

			if len(tt.wantErr) > 0 {
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("applyVRFConfig() error = %q, want it to mention %q", err, want)
					}
				}
				// The existing device must be left alone and the interface
				// must not be moved into it.
				if nsLink.Attrs().MasterIndex != 0 {
					t.Errorf("%s was attached to master index %d, want no master", ifName, nsLink.Attrs().MasterIndex)
				}
				if vrfLink.Attrs().Flags&net.FlagUp != 0 {
					t.Errorf("%s was brought up on error", vrfName)
				}
				return
			}

			if got != table {
				t.Errorf("applyVRFConfig() = %d, want %d", got, table)
			}
			vrf, ok := vrfLink.(*netlink.Vrf)
			if !ok {
				t.Fatalf("%s is %s, want vrf", vrfName, vrfLink.Type())
			}
			if vrf.Table != uint32(table) {
				t.Errorf("%s table = %d, want %d", vrfName, vrf.Table, table)
			}
			if vrfLink.Attrs().Flags&net.FlagUp == 0 {
				t.Errorf("%s is not up", vrfName)
			}
			if nsLink.Attrs().MasterIndex != vrfLink.Attrs().Index {
				t.Errorf("%s master index = %d, want %d (%s)", ifName, nsLink.Attrs().MasterIndex, vrfLink.Attrs().Index, vrfName)
			}
		})
	}
}
