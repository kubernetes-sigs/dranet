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

package inventory

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/time/rate"
	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"

	userns "sigs.k8s.io/dranet/internal/testutils"
)

func TestRunRescansOnAddressChange(t *testing.T) {
	userns.Run(t, testRunRescansOnAddressChange, syscall.CLONE_NEWNET)
}

// testRunRescansOnAddressChange runs inside a fresh network namespace. It
// adds an address to an interface that already exists and checks that Run
// publishes the new ipv4 attribute well before the periodic poll would.
func testRunRescansOnAddressChange(t *testing.T) {
	const ifName = "veth0"
	la := netlink.NewLinkAttrs()
	la.Name = ifName
	link := &netlink.Veth{LinkAttrs: la, PeerName: "veth1"}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("failed to add veth pair %s: %v", ifName, err)
	}
	// The pair stays down: no IPv6 autoconfiguration or carrier events, so
	// the only netlink events after the initial scan are the address changes
	// below.

	db := New(
		WithRateLimiter(rate.NewLimiter(rate.Inf, 1)),
		// Far longer than the test timeout, so only a netlink event can
		// trigger the rescan.
		WithMaxPollInterval(time.Hour),
	)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- db.Run(ctx) }()
	updates := db.GetResources(ctx)
	defer func() {
		cancel()
		// Run may be blocked publishing a scan; drain until it closes the
		// channel on return.
		for range updates {
		}
		if err := <-runErr; err != context.Canceled {
			t.Errorf("Run() = %v, want %v", err, context.Canceled)
		}
	}()

	timeout := time.After(10 * time.Second)
	// waitFor returns once a published scan has the veth device and pred
	// accepts its ipv4 attribute, or fails the test on timeout.
	waitFor := func(what string, pred func(ipv4 *string) bool) {
		t.Helper()
		for {
			select {
			case devices, ok := <-updates:
				if !ok {
					t.Fatalf("Run stopped while waiting for %s", what)
				}
				if device := findDevice(devices, ifName); device != nil && pred(device.Attributes[apis.AttrIPv4].StringValue) {
					return
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	waitFor("the initial scan without an address", func(ipv4 *string) bool { return ipv4 == nil })

	addr, err := netlink.ParseAddr("192.0.2.10/24")
	if err != nil {
		t.Fatalf("failed to parse address: %v", err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("failed to add address to %s: %v", ifName, err)
	}
	waitFor("the scan with the added address", func(ipv4 *string) bool {
		return ipv4 != nil && *ipv4 == "192.0.2.10/24"
	})

	if err := netlink.AddrDel(link, addr); err != nil {
		t.Fatalf("failed to delete address from %s: %v", ifName, err)
	}
	waitFor("the scan after the address was removed", func(ipv4 *string) bool { return ipv4 == nil })
}

func findDevice(devices []resourceapi.Device, ifName string) *resourceapi.Device {
	for i := range devices {
		if name := devices[i].Attributes[apis.AttrInterfaceName].StringValue; name != nil && *name == ifName {
			return &devices[i]
		}
	}
	return nil
}
