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
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
	sysctltesting "k8s.io/component-helpers/node/util/sysctl/testing"
	"k8s.io/utils/ptr"

	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// failingSetSysctl fails writes to a single setting and delegates the rest.
type failingSetSysctl struct {
	*sysctltesting.Fake
	setting   string
	onFailure func()
}

func (f *failingSetSysctl) SetSysctl(setting string, value int) error {
	if setting == f.setting {
		if f.onFailure != nil {
			f.onFailure()
		}
		return errors.New("test set failure")
	}
	return f.Fake.SetSysctl(setting, value)
}

func TestHasInterfaceSysctlConfig(t *testing.T) {
	tests := []struct {
		name            string
		interfaceConfig apis.InterfaceConfig
		want            bool
	}{
		{
			name:            "empty",
			interfaceConfig: apis.InterfaceConfig{Name: "eth0"},
			want:            false,
		},
		{
			name:            "arp ignore only",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1)},
			want:            true,
		},
		{
			name:            "arp announce only",
			interfaceConfig: apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](2)},
			want:            true,
		},
		{
			name:            "accept ra only",
			interfaceConfig: apis.InterfaceConfig{AcceptRA: ptr.To[int32](2)},
			want:            true,
		},
		{
			name:            "accept ra zero is still requested",
			interfaceConfig: apis.InterfaceConfig{AcceptRA: ptr.To[int32](0)},
			want:            true,
		},
		{
			name:            "dad transmits only",
			interfaceConfig: apis.InterfaceConfig{DADTransmits: ptr.To[int32](0)},
			want:            true,
		},
		{
			name:            "router solicitation delay only",
			interfaceConfig: apis.InterfaceConfig{RouterSolicitationDelay: ptr.To[int32](0)},
			want:            true,
		},
		{
			name:            "disable ipv6 false is still requested",
			interfaceConfig: apis.InterfaceConfig{DisableIPv6: ptr.To(false)},
			want:            true,
		},
		{
			name:            "addr gen mode only",
			interfaceConfig: apis.InterfaceConfig{AddrGenMode: ptr.To[int32](3)},
			want:            true,
		},
		{
			name:            "zero values are still requested",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](0), ARPAnnounce: ptr.To[int32](0)},
			want:            true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasInterfaceSysctlConfig(tt.interfaceConfig); got != tt.want {
				t.Errorf("hasInterfaceSysctlConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyInterfaceSysctlsWithSysctl(t *testing.T) {
	tests := []struct {
		name            string
		interfaceConfig apis.InterfaceConfig
		want            map[string]int
	}{
		{
			name: "all settings",
			interfaceConfig: apis.InterfaceConfig{
				ARPIgnore:               ptr.To[int32](1),
				ARPAnnounce:             ptr.To[int32](2),
				AcceptRA:                ptr.To[int32](0),
				DADTransmits:            ptr.To[int32](0),
				RouterSolicitationDelay: ptr.To[int32](0),
			},
			want: map[string]int{
				"net/ipv4/conf/rdma0/arp_ignore":                1,
				"net/ipv4/conf/rdma0/arp_announce":              2,
				"net/ipv6/conf/rdma0/accept_ra":                 0,
				"net/ipv6/conf/rdma0/dad_transmits":             0,
				"net/ipv6/conf/rdma0/router_solicitation_delay": 0,
			},
		},
		{
			name: "the settings SLAAC defaults to",
			interfaceConfig: apis.InterfaceConfig{
				AcceptRA:                ptr.To[int32](2),
				DADTransmits:            ptr.To[int32](0),
				RouterSolicitationDelay: ptr.To[int32](0),
			},
			want: map[string]int{
				"net/ipv6/conf/rdma0/accept_ra":                 2,
				"net/ipv6/conf/rdma0/dad_transmits":             0,
				"net/ipv6/conf/rdma0/router_solicitation_delay": 0,
			},
		},
		{
			name: "the settings SLAAC defaults to on an IPVLAN child",
			interfaceConfig: apis.InterfaceConfig{
				AcceptRA:                ptr.To[int32](2),
				RouterSolicitationDelay: ptr.To[int32](0),
				DisableIPv6:             ptr.To(false),
				AddrGenMode:             ptr.To[int32](3),
			},
			want: map[string]int{
				"net/ipv6/conf/rdma0/accept_ra":                 2,
				"net/ipv6/conf/rdma0/router_solicitation_delay": 0,
				"net/ipv6/conf/rdma0/disable_ipv6":              0,
				"net/ipv6/conf/rdma0/addr_gen_mode":             3,
			},
		},
		{
			name:            "disable ipv6 true is written as 1",
			interfaceConfig: apis.InterfaceConfig{DisableIPv6: ptr.To(true)},
			want:            map[string]int{"net/ipv6/conf/rdma0/disable_ipv6": 1},
		},
		{
			name:            "only accept_ra",
			interfaceConfig: apis.InterfaceConfig{AcceptRA: ptr.To[int32](1)},
			want:            map[string]int{"net/ipv6/conf/rdma0/accept_ra": 1},
		},
		{
			name:            "only accept_ra zero is written",
			interfaceConfig: apis.InterfaceConfig{AcceptRA: ptr.To[int32](0)},
			want:            map[string]int{"net/ipv6/conf/rdma0/accept_ra": 0},
		},
		{
			name:            "only arp_ignore",
			interfaceConfig: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1)},
			want:            map[string]int{"net/ipv4/conf/rdma0/arp_ignore": 1},
		},
		{
			name:            "explicit zero is written",
			interfaceConfig: apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](0)},
			want:            map[string]int{"net/ipv4/conf/rdma0/arp_announce": 0},
		},
		{
			name:            "nothing requested",
			interfaceConfig: apis.InterfaceConfig{Name: "rdma0"},
			want:            map[string]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysctls := sysctltesting.NewFake()
			if err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", tt.interfaceConfig); err != nil {
				t.Fatalf("applyInterfaceSysctlsWithSysctl() error: %v", err)
			}
			if diff := cmp.Diff(tt.want, sysctls.Settings); diff != "" {
				t.Errorf("applyInterfaceSysctlsWithSysctl() settings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyInterfaceSysctlsWithSysctlReturnsSetErrors(t *testing.T) {
	sysctls := &failingSetSysctl{
		Fake:    sysctltesting.NewFake(),
		setting: "net/ipv4/conf/rdma0/arp_ignore",
	}
	interfaceConfig := apis.InterfaceConfig{
		ARPIgnore:   ptr.To[int32](1),
		ARPAnnounce: ptr.To[int32](2),
	}

	err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", interfaceConfig)
	if err == nil || !strings.Contains(err.Error(), sysctls.setting) {
		t.Fatalf("applyInterfaceSysctlsWithSysctl() error = %v, want error naming %s", err, sysctls.setting)
	}
	// A failed setting must not stop the remaining ones from being applied.
	if len(sysctls.Settings) != 1 {
		t.Errorf("applyInterfaceSysctlsWithSysctl() applied %d settings, want 1", len(sysctls.Settings))
	}
}

func TestApplyInterfaceSysctlsWithSysctlReturnsIPv6SetErrors(t *testing.T) {
	sysctls := &failingSetSysctl{
		Fake:    sysctltesting.NewFake(),
		setting: "net/ipv6/conf/rdma0/accept_ra",
	}
	interfaceConfig := apis.InterfaceConfig{
		ARPIgnore: ptr.To[int32](1),
		AcceptRA:  ptr.To[int32](2),
	}

	err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", interfaceConfig)
	if err == nil || !strings.Contains(err.Error(), sysctls.setting) {
		t.Fatalf("applyInterfaceSysctlsWithSysctl() error = %v, want error naming %s", err, sysctls.setting)
	}
	// The IPv4 setting before the failing IPv6 one is still applied.
	if len(sysctls.Settings) != 1 {
		t.Errorf("applyInterfaceSysctlsWithSysctl() applied %d settings, want 1", len(sysctls.Settings))
	}
}

// erroringSetSysctl fails writes to a single setting with a chosen error.
type erroringSetSysctl struct {
	*sysctltesting.Fake
	setting string
	err     error
}

func (e *erroringSetSysctl) SetSysctl(setting string, value int) error {
	if setting == e.setting {
		return e.err
	}
	return e.Fake.SetSysctl(setting, value)
}

// A missing accept_ra sysctl means the interface has no IPv6 settings. Zero is
// then already satisfied; any other value is an error that names the cause.
func TestApplyInterfaceSysctlsWithSysctlAcceptRANotExist(t *testing.T) {
	const setting = "net/ipv6/conf/rdma0/accept_ra"
	tests := []struct {
		name     string
		acceptRA int32
		err      error
		wantErr  string
	}{
		{name: "zero with a missing sysctl is satisfied", acceptRA: 0, err: os.ErrNotExist},
		{name: "one with a missing sysctl fails", acceptRA: 1, err: os.ErrNotExist, wantErr: "IPv6 is not enabled on the interface"},
		{name: "two with a missing sysctl fails", acceptRA: 2, err: os.ErrNotExist, wantErr: "IPv6 is not enabled on the interface"},
		{name: "zero with another error still fails", acceptRA: 0, err: errors.New("test set failure"), wantErr: "test set failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysctls := &erroringSetSysctl{Fake: sysctltesting.NewFake(), setting: setting, err: tt.err}
			config := apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1), AcceptRA: ptr.To(tt.acceptRA)}
			err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", config)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("applyInterfaceSysctlsWithSysctl() error = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), setting) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("applyInterfaceSysctlsWithSysctl() error = %v, want one naming %s and containing %q", err, setting, tt.wantErr)
			}
			// The IPv4 setting is applied in every case.
			if got := sysctls.Settings["net/ipv4/conf/rdma0/arp_ignore"]; got != 1 {
				t.Errorf("arp_ignore = %d, want 1", got)
			}
		})
	}
}

func TestApplyInterfaceSysctlConfigNoConfigDoesNotEnterNamespace(t *testing.T) {
	if err := applyInterfaceSysctlConfig(netns.None(), "rdma0", apis.InterfaceConfig{Name: "rdma0"}); err != nil {
		t.Fatalf("applyInterfaceSysctlConfig() error: %v", err)
	}
}

func TestApplyInterfaceSysctlConfigUsesOpenNamespace(t *testing.T) {
	userns.Run(t, testApplyInterfaceSysctlConfigUsesOpenNamespace_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

func testApplyInterfaceSysctlConfigUsesOpenNamespace_Namespaced(t *testing.T) {
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
	nsName := fmt.Sprintf("sysctl-%x", rndString)
	targetNs, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("failed to create network namespace: %v", err)
	}
	defer targetNs.Close()
	defer func() { _ = netns.DeleteNamed(nsName) }()

	if err := netns.Set(originalNs); err != nil {
		t.Fatalf("failed to restore original network namespace: %v", err)
	}
	if err := netns.DeleteNamed(nsName); err != nil {
		t.Fatalf("failed to remove network namespace path: %v", err)
	}

	config := apis.InterfaceConfig{ARPIgnore: ptr.To[int32](1)}
	if err := applyInterfaceSysctlConfig(targetNs, "lo", config); err != nil {
		t.Fatalf("applyInterfaceSysctlConfig() with an open namespace handle failed: %v", err)
	}

	if err := netns.Set(targetNs); err != nil {
		t.Fatalf("failed to enter target network namespace: %v", err)
	}
	got, readErr := sysctl.New().GetSysctl("net/ipv4/conf/lo/arp_ignore")
	restoreErr := netns.Set(originalNs)
	if readErr != nil {
		t.Fatalf("failed to read arp_ignore: %v", readErr)
	}
	if restoreErr != nil {
		t.Fatalf("failed to restore original network namespace: %v", restoreErr)
	}
	if got != 1 {
		t.Errorf("arp_ignore = %d, want 1", got)
	}
}

// orderRecordingSysctl records the order in which settings are written.
type orderRecordingSysctl struct {
	*sysctltesting.Fake
	order []string
}

func (o *orderRecordingSysctl) SetSysctl(setting string, value int) error {
	o.order = append(o.order, setting)
	return o.Fake.SetSysctl(setting, value)
}

// Enabling IPv6 starts address configuration with whatever the other settings
// are at that moment, so the address generation mode comes first and
// disable_ipv6 last.
func TestApplyInterfaceSysctlsWithSysctlEnablesIPv6Last(t *testing.T) {
	sysctls := &orderRecordingSysctl{Fake: sysctltesting.NewFake()}
	interfaceConfig := apis.InterfaceConfig{
		ARPIgnore:               ptr.To[int32](1),
		AcceptRA:                ptr.To[int32](2),
		DADTransmits:            ptr.To[int32](0),
		RouterSolicitationDelay: ptr.To[int32](0),
		DisableIPv6:             ptr.To(false),
		AddrGenMode:             ptr.To[int32](3),
	}
	if err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", interfaceConfig); err != nil {
		t.Fatalf("applyInterfaceSysctlsWithSysctl() error: %v", err)
	}
	want := []string{
		"net/ipv4/conf/rdma0/arp_ignore",
		"net/ipv6/conf/rdma0/addr_gen_mode",
		"net/ipv6/conf/rdma0/dad_transmits",
		"net/ipv6/conf/rdma0/router_solicitation_delay",
		"net/ipv6/conf/rdma0/accept_ra",
		"net/ipv6/conf/rdma0/disable_ipv6",
	}
	if diff := cmp.Diff(want, sysctls.order); diff != "" {
		t.Errorf("applyInterfaceSysctlsWithSysctl() order mismatch (-want +got):\n%s", diff)
	}
}

func TestApplyInterfaceSysctlsWithSysctlDisableIPv6WithoutIPv6(t *testing.T) {
	for _, tt := range []struct {
		name    string
		disable bool
		wantErr bool
	}{
		{name: "disabling is already satisfied", disable: true, wantErr: false},
		{name: "enabling fails", disable: false, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sysctls := &erroringSetSysctl{
				Fake:    sysctltesting.NewFake(),
				setting: "net/ipv6/conf/rdma0/disable_ipv6",
				err:     fmt.Errorf("open: %w", os.ErrNotExist),
			}
			err := applyInterfaceSysctlsWithSysctl(sysctls, "rdma0", apis.InterfaceConfig{DisableIPv6: ptr.To(tt.disable)})
			if (err != nil) != tt.wantErr {
				t.Fatalf("applyInterfaceSysctlsWithSysctl() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "IPv6 is not available") {
				t.Errorf("applyInterfaceSysctlsWithSysctl() error = %v, want it to say IPv6 is not available", err)
			}
		})
	}
}
