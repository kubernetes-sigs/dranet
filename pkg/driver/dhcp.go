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
	"errors"
	"fmt"
	"net"
	"time"

	"sigs.k8s.io/dranet/pkg/apis"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
	"sigs.k8s.io/dranet/internal/nlwrap"
)

// getDHCP obtains an address for ifName over DHCP and returns it together with
// the lease record. The caller keeps the record so the lease can be released
// when the claim goes away: the client here is one-shot, so without a
// DHCPRELEASE the server holds the address for the whole valid-lifetime even
// though nothing uses it.
func getDHCP(ctx context.Context, ifName string) (ip string, routes []apis.RouteConfig, lease *DHCPLeaseRecord, err error) {
	link, err := nlwrap.LinkByName(ifName)
	if err != nil {
		return "", nil, nil, err
	}
	if link.Attrs().OperState != netlink.OperUp {
		if err := netlink.LinkSetUp(link); err != nil {
			return "", nil, nil, fmt.Errorf("failed to set interface %s up: %w", ifName, err)
		}
	}
	dhclient, err := nclient4.New(ifName)
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to create DHCP client on interface %s: %w", ifName, err)
	}
	defer dhclient.Close()

	result, err := dhclient.Request(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to obtain DHCP lease on interface %s: %w", ifName, err)
	}
	ack := result.ACK
	if ack == nil {
		return "", nil, nil, fmt.Errorf("no DHCPACK in the lease obtained on interface %s", ifName)
	}
	ip = (&net.IPNet{
		IP:   ack.YourIPAddr,
		Mask: ack.SubnetMask(),
	}).String()

	// only support opt 121 (ignore 33)
	for _, route := range ack.ClasslessStaticRoute() {
		routeCfg := apis.RouteConfig{
			Destination: route.Dest.String(),
			Gateway:     route.Router.String(),
		}
		routes = append(routes, routeCfg)
	}
	return ip, routes, newDHCPLeaseRecord(ack), nil
}

// newDHCPLeaseRecord extracts from the ACK what a later DHCPRELEASE needs.
// RELEASE is unicast to the server identifier, so an ACK without one leaves
// nothing to release to and yields no record; the lease then expires on its
// own, as it did before releases were sent at all.
func newDHCPLeaseRecord(ack *dhcpv4.DHCPv4) *DHCPLeaseRecord {
	serverID := ack.ServerIdentifier()
	if serverID == nil || serverID.IsUnspecified() {
		return nil
	}
	return &DHCPLeaseRecord{
		ClientIP:  ack.YourIPAddr.String(),
		ClientMAC: ack.ClientHWAddr.String(),
		ServerID:  serverID.String(),
	}
}

// dhcpReleaseTimeout bounds the release attempt. RELEASE is not answered, so
// this is how long the send is allowed to take, not a wait for a reply.
const dhcpReleaseTimeout = 2 * time.Second

// releaseDHCP sends a DHCPRELEASE for lease, so the server frees the address
// immediately instead of holding it for the rest of the valid-lifetime.
// Callers treat a failure as non-fatal: the lease then expires on its own,
// which is the behaviour without this call at all.
//
// The release goes out on ifName, the host interface the lease was taken on.
// The kernel returns a netdev to the host namespace when the pod's namespace
// goes away, so by unprepare time the interface is normally back. If it is
// not, there is nothing to send from and the caller logs it.
func releaseDHCP(ctx context.Context, ifName string, lease *DHCPLeaseRecord) error {
	if lease == nil {
		return nil
	}
	release, err := newDHCPRelease(lease)
	if err != nil {
		return fmt.Errorf("failed to build DHCPRELEASE for interface %s: %w", ifName, err)
	}
	dhclient, err := nclient4.New(ifName)
	if err != nil {
		return fmt.Errorf("failed to create DHCP client on interface %s: %w", ifName, err)
	}
	defer dhclient.Close()

	// The server does not answer a RELEASE, so the read that follows the send
	// always ends with the context running out. That is the success path here,
	// not a failure, for both a deadline and an explicit cancel.
	server := &net.UDPAddr{IP: release.ServerIdentifier(), Port: nclient4.ServerPort}
	_, err = dhclient.SendAndRead(ctx, server, release, nil)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("failed to send DHCPRELEASE on interface %s: %w", ifName, err)
	}
	return nil
}

// newDHCPRelease builds the DHCPRELEASE message for lease. The record is
// checkpointed as strings, so each field is parsed and checked here rather
// than trusted.
func newDHCPRelease(lease *DHCPLeaseRecord) (*dhcpv4.DHCPv4, error) {
	clientIP := net.ParseIP(lease.ClientIP)
	if clientIP == nil || clientIP.IsUnspecified() {
		return nil, fmt.Errorf("invalid client IP %q", lease.ClientIP)
	}
	hwAddr, err := net.ParseMAC(lease.ClientMAC)
	if err != nil {
		return nil, fmt.Errorf("invalid client MAC %q: %w", lease.ClientMAC, err)
	}
	serverID := net.ParseIP(lease.ServerID)
	if serverID == nil || serverID.IsUnspecified() {
		return nil, fmt.Errorf("invalid server identifier %q", lease.ServerID)
	}
	return dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRelease),
		dhcpv4.WithClientIP(clientIP),
		dhcpv4.WithHwAddr(hwAddr),
		dhcpv4.WithBroadcast(false),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverID)),
	)
}
