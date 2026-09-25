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
				ARPIgnore:   ptr.To[int32](1),
				ARPAnnounce: ptr.To[int32](2),
				AcceptRA:    ptr.To[int32](0),
			},
			want: map[string]int{
				"net/ipv4/conf/rdma0/arp_ignore":   1,
				"net/ipv4/conf/rdma0/arp_announce": 2,
				"net/ipv6/conf/rdma0/accept_ra":    0,
			},
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
