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
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

var (
	testClientMAC = net.HardwareAddr{0x92, 0x0e, 0x50, 0x89, 0x7a, 0x97}
	testClientIP  = net.IP{172, 17, 11, 66}
	testServerID  = net.IP{172, 17, 11, 1}
)

func testACK(t *testing.T, modifiers ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	ack, err := dhcpv4.New(append([]dhcpv4.Modifier{
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithHwAddr(testClientMAC),
		dhcpv4.WithYourIP(testClientIP),
	}, modifiers...)...)
	if err != nil {
		t.Fatalf("build ACK: %v", err)
	}
	return ack
}

// The record must carry exactly what the server keyed the lease by, and the
// address the release is unicast to.
func TestNewDHCPLeaseRecord(t *testing.T) {
	ack := testACK(t, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(testServerID)))

	got := newDHCPLeaseRecord(ack)
	want := &DHCPLeaseRecord{
		ClientIP:  "172.17.11.66",
		ClientMAC: "92:0e:50:89:7a:97",
		ServerID:  "172.17.11.1",
	}
	if got == nil || *got != *want {
		t.Errorf("newDHCPLeaseRecord = %+v, want %+v", got, want)
	}
}

// An ACK without a server identifier gives a release nowhere to go, so no
// record is kept and unprepare has nothing to do.
func TestNewDHCPLeaseRecordWithoutServerIdentifier(t *testing.T) {
	if got := newDHCPLeaseRecord(testACK(t)); got != nil {
		t.Errorf("newDHCPLeaseRecord = %+v, want nil", got)
	}
}

// The release must keep the address and the hardware address of the lease:
// the server keys the lease by them, so a release that loses either frees
// nothing. It must also be addressed to the server that granted it.
func TestNewDHCPRelease(t *testing.T) {
	rec := newDHCPLeaseRecord(testACK(t, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(testServerID))))

	release, err := newDHCPRelease(rec)
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	if got := release.MessageType(); got != dhcpv4.MessageTypeRelease {
		t.Errorf("MessageType = %v, want RELEASE", got)
	}
	if !release.ClientIPAddr.Equal(testClientIP) {
		t.Errorf("ClientIPAddr = %v, want %v", release.ClientIPAddr, testClientIP)
	}
	if release.ClientHWAddr.String() != testClientMAC.String() {
		t.Errorf("ClientHWAddr = %v, want %v", release.ClientHWAddr, testClientMAC)
	}
	if !release.ServerIdentifier().Equal(testServerID) {
		t.Errorf("ServerIdentifier = %v, want %v", release.ServerIdentifier(), testServerID)
	}
	if release.IsBroadcast() {
		t.Error("release must be unicast")
	}
}

// The record is checkpointed as strings, so a corrupt one must surface as an
// error rather than a malformed packet.
func TestNewDHCPReleaseRejectsInvalidRecord(t *testing.T) {
	valid := DHCPLeaseRecord{ClientIP: "172.17.11.66", ClientMAC: "92:0e:50:89:7a:97", ServerID: "172.17.11.1"}
	for name, mutate := range map[string]func(*DHCPLeaseRecord){
		"client IP":         func(r *DHCPLeaseRecord) { r.ClientIP = "not-an-ip" },
		"client MAC":        func(r *DHCPLeaseRecord) { r.ClientMAC = "zz" },
		"server identifier": func(r *DHCPLeaseRecord) { r.ServerID = "0.0.0.0" },
	} {
		rec := valid
		mutate(&rec)
		if _, err := newDHCPRelease(&rec); err == nil {
			t.Errorf("%s: invalid record should error", name)
		}
	}
}

// A claim that took no lease must not make unprepare open a socket on an
// interface that may no longer exist.
func TestReleaseDHCPWithoutLease(t *testing.T) {
	if err := releaseDHCP(context.Background(), "does-not-exist", nil); err != nil {
		t.Errorf("nil lease should be a no-op, got %v", err)
	}
}
