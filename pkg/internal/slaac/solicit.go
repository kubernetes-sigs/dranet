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

package slaac

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"time"
)

const (
	// maxSolicitations is how many Router Solicitations are sent before the
	// host gives up soliciting and only listens (MAX_RTR_SOLICITATIONS,
	// RFC 4861 section 10).
	maxSolicitations = 3
	// solicitInterval is the wait before the first retransmission; it doubles
	// after each one (RFC 7559 section 2). RFC 4861 uses a fixed 4s
	// (RTR_SOLICITATION_INTERVAL), too slow for a bounded one-shot client:
	// with 1s, solicitations go out at 0s, 1s and 3s.
	solicitInterval = time.Second
	// maxReportedRouters bounds the routers a SolicitError lists, which a link
	// with many senders of unusable advertisements would otherwise make long.
	maxReportedRouters = 8
)

// Solicit sends Router Solicitations on ifi and returns the first Router
// Advertisement that accept approves, with the address of the router that
// sent it. A nil accept takes the first valid advertisement.
//
// src is the source of the solicitations: the interface's link-local address
// if it has one, or the unspecified address, in which case the routers answer
// by multicast and no source link-layer address option is sent (RFC 4861
// section 4.1). The first solicitation goes out immediately (the random delay
// of RFC 4861 section 6.3.7 is for hosts booting together, not for one
// interface being prepared); it is retransmitted maxSolicitations times at
// most, after which Solicit keeps listening for unsolicited advertisements
// until ctx ends. The error then wraps the context's error and says how many
// advertisements were seen and from which routers, so a link where routers
// answer but none fits the request can be told from a silent one.
func Solicit(ctx context.Context, ifi *net.Interface, src netip.Addr, accept func(ra *RouterAdvertisement, router netip.Addr) bool) (*RouterAdvertisement, netip.Addr, error) {
	c, err := Listen(ifi, TypeRouterAdvertisement)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	defer c.Close()
	// A cancelled context must interrupt a blocked read, not only a deadline.
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
	defer stop()

	rs := &RouterSolicitation{}
	if !src.IsUnspecified() {
		rs.SourceLinkLayerAddress = ifi.HardwareAddr
	}

	var (
		sent     int
		next     = time.Now()
		interval = solicitInterval
		rejected int
		routers  []netip.Addr
	)
	for {
		if now := time.Now(); sent < maxSolicitations && !now.Before(next) {
			if err := c.WriteTo(rs, src, AllRouters, nil); err != nil {
				return nil, netip.Addr{}, err
			}
			sent++
			next = now.Add(interval)
			interval *= 2
		}
		// Wake up for the next retransmission, or never on our own once
		// they are exhausted: the context then ends the wait.
		var deadline time.Time
		if sent < maxSolicitations {
			deadline = next
		}
		if err := c.SetReadDeadline(deadline); err != nil {
			return nil, netip.Addr{}, err
		}
		// The AfterFunc may have fired before the deadline above replaced
		// its wake-up; a context that has ended by now must not be waited on.
		if ctx.Err() != nil {
			return nil, netip.Addr{}, &SolicitError{Interface: ifi.Name, Sent: sent, Rejected: rejected, Routers: routers, Err: ctx.Err()}
		}
		m, router, err := c.ReadFrom()
		if err != nil {
			if ctx.Err() != nil {
				return nil, netip.Addr{}, &SolicitError{Interface: ifi.Name, Sent: sent, Rejected: rejected, Routers: routers, Err: ctx.Err()}
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return nil, netip.Addr{}, err
		}
		ra, ok := m.(*RouterAdvertisement)
		if !ok {
			continue // the filter only lets advertisements through
		}
		if accept == nil || accept(ra, router) {
			return ra, router, nil
		}
		rejected++
		if len(routers) < maxReportedRouters && !slices.Contains(routers, router) {
			routers = append(routers, router)
		}
	}
}

// SolicitError reports that Solicit ended without an acceptable Router
// Advertisement.
type SolicitError struct {
	// Interface is the interface solicited on.
	Interface string
	// Sent is the number of solicitations sent.
	Sent int
	// Rejected is the number of advertisements accept turned down, and
	// Routers lists the routers that sent them, each once, up to
	// maxReportedRouters of them.
	Rejected int
	Routers  []netip.Addr
	// Err is the context error that ended the wait.
	Err error
}

func (e *SolicitError) Error() string {
	if e.Rejected == 0 {
		return fmt.Sprintf("no router advertisement received on %s after %d solicitation(s): %v", e.Interface, e.Sent, e.Err)
	}
	return fmt.Sprintf("no acceptable router advertisement on %s after %d solicitation(s): %d received from %v did not fit: %v", e.Interface, e.Sent, e.Rejected, e.Routers, e.Err)
}

func (e *SolicitError) Unwrap() error { return e.Err }
