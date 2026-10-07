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
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestPrepareSLAACAddressInheritance(t *testing.T) {
	userns.Run(t, testPrepareSLAACAddressInheritance_Namespaced, syscall.CLONE_NEWNET)
}

// Under SLAAC a passthrough interface inherits the host's IPv4 addresses and
// none of its IPv6 ones; an IPVLAN child inherits nothing, because the parent
// keeps its addresses on the host.
func testPrepareSLAACAddressInheritance_Namespaced(t *testing.T) {
	la := netlink.NewLinkAttrs()
	la.Name = "slaacp0"
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("failed to add dummy link: %v", err)
	}
	link, err := netlink.LinkByName(la.Name)
	if err != nil {
		t.Fatalf("failed to get link: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to set link up: %v", err)
	}
	for _, cidr := range []string{"192.0.2.10/24", "2001:db8:5::10/64"} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatal(err)
		}
		addr.Flags = unix.IFA_F_NODAD
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("failed to add address %s: %v", cidr, err)
		}
	}

	for _, tc := range []struct {
		name          string
		cloud         apis.InterfaceConfig
		wantAddresses []string
	}{
		{
			name:          "passthrough inherits only IPv4",
			cloud:         apis.InterfaceConfig{Addressing: apis.AddressingModeSLAAC},
			wantAddresses: []string{"192.0.2.10/24"},
		},
		{
			name:  "IPVLAN child inherits nothing",
			cloud: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN, Addressing: apis.AddressingModeSLAAC},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeDB := newFakeInventoryDB()
			fakeDB.IsIBOnlyDeviceFunc = func(string) bool { return false }
			fakeDB.GetNetInterfaceNameFunc = func(string) (string, error) { return la.Name, nil }
			fakeDB.GetDeviceFunc = func(deviceName string) (resourcev1.Device, bool) {
				return resourcev1.Device{Name: deviceName}, true
			}
			fakeDB.GetDeviceConfigFunc = func(string) (*apis.NetworkConfig, bool) {
				return &apis.NetworkConfig{Interface: tc.cloud}, true
			}
			np := &NetworkDriver{
				netdb:          fakeDB,
				driverName:     "test.driver",
				podConfigStore: mustNewPodConfigStore(),
				eventRecorder:  record.NewFakeRecorder(100),
			}
			podUID := types.UID("pod-uid-slaac-inherit")
			claim := &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-slaac-inherit", Namespace: "default", Name: "claim-slaac-inherit"},
				Status: resourcev1.ResourceClaimStatus{
					ReservedFor: []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: "test-pod", UID: podUID}},
					Allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
						Results: []resourcev1.DeviceRequestAllocationResult{{Driver: "test.driver", Device: "net-dev-0", Request: "req-0"}},
					}},
				},
			}
			if result := np.prepareResourceClaim(t.Context(), claim); result.Err != nil {
				t.Fatalf("prepareResourceClaim() error: %v", result.Err)
			}
			podCfg, ok := np.podConfigStore.GetPodConfig(podUID)
			if !ok {
				t.Fatal("no pod config stored")
			}
			got := podCfg.DeviceConfigs["net-dev-0"].NetworkInterfaceConfigInPod.Interface.Addresses
			if diff := cmp.Diff(tc.wantAddresses, got); diff != "" {
				t.Errorf("inherited addresses mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Profile providers, webhooks among them, get the merged configuration with
// the defaults for the interface type known so far, while the configuration
// the user wrote stays undefaulted until the profile has been merged.
func TestProfileProviderGetsDefaultedConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		user  string
		check func(t *testing.T, got *apis.NetworkConfig)
	}{
		{
			name: "deprecated dhcp field is folded into addressing",
			user: `{"interface":{"dhcp":true}}`,
			check: func(t *testing.T, got *apis.NetworkConfig) {
				if got.Interface.Addressing != apis.AddressingModeDHCP {
					t.Errorf("profile provider got addressing %q, want %q", got.Interface.Addressing, apis.AddressingModeDHCP)
				}
			},
		},
		{
			name: "SLAAC gets the IPVLAN defaults of the provider's type",
			user: `{"interface":{"addressing":"SLAAC"}}`,
			check: func(t *testing.T, got *apis.NetworkConfig) {
				iface := got.Interface
				if iface.AddrGenMode == nil || *iface.AddrGenMode != 3 || iface.DisableIPv6 == nil || *iface.DisableIPv6 {
					t.Errorf("profile provider got addrGenMode %v and disableIPv6 %v, want 3 and false", iface.AddrGenMode, iface.DisableIPv6)
				}
			},
		},
		{
			name: "VRF table is derived from the name",
			user: `{"interface":{"vrf":{"name":"blue"}}}`,
			check: func(t *testing.T, got *apis.NetworkConfig) {
				if got.Interface.VRF == nil || got.Interface.VRF.Table == nil {
					t.Errorf("profile provider got VRF %+v, want the derived table", got.Interface.VRF)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userConf, err := apis.UnmarshalConfig(&runtime.RawExtension{Raw: []byte(tc.user)})
			if err != nil {
				t.Fatal(err)
			}
			var got *apis.NetworkConfig
			leaked := false
			fakeDB := newFakeInventoryDB()
			fakeDB.GetDeviceConfigFunc = func(string) (*apis.NetworkConfig, bool) {
				return &apis.NetworkConfig{Profile: "test-profile", Interface: apis.InterfaceConfig{Type: apis.InterfaceTypeIPVLAN}}, true
			}
			fakeDB.GetProfileConfigFunc = func(_ string, _ *resourcev1.ResourceClaim, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
				got = config
				// The provider's copy is defaulted; the user's is not, yet.
				leaked = userConf.Interface.VRF != nil && userConf.Interface.VRF.Table != nil
				return &apis.NetworkConfig{}, nil
			}
			np := &NetworkDriver{netdb: fakeDB}
			claim := &resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: "claim-uid-profile"}}
			// The merged configuration may still be invalid for other reasons
			// (DHCP on IPVLAN, for one); only what the provider saw matters here.
			_, _ = np.getDeviceNetworkConfig("net-dev-0", claim, userConf)
			if got == nil {
				t.Fatal("profile provider was not called")
			}
			tc.check(t, got)
			if leaked {
				t.Error("defaulting the profile input wrote through to the user configuration")
			}
		})
	}
}
