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
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/vishvananda/netlink"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/internal/nlwrap"
	userns "sigs.k8s.io/dranet/internal/testutils"
	"sigs.k8s.io/dranet/pkg/apis"
)

// claimDevices waits for the claim status to list want devices and returns
// them by name.
func claimDevices(t *testing.T, client *fake.Clientset, claim types.NamespacedName, want int) map[string]resourcev1.AllocatedDeviceStatus {
	t.Helper()
	got := map[string]resourcev1.AllocatedDeviceStatus{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := client.ResourceV1().ResourceClaims(claim.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
		if err == nil && len(c.Status.Devices) >= want {
			for _, d := range c.Status.Devices {
				got[d.Device] = d
			}
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("claim %s did not list %d devices in time (got %v)", claim, want, got)
	return nil
}

func hasCondition(d resourcev1.AllocatedDeviceStatus, condType string, status metav1.ConditionStatus) bool {
	for _, c := range d.Conditions {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}

// Each claim status apply replaces the devices the previous apply of the same
// field manager listed, so failures are reported once per claim.
func TestReportSLAACNotReadyGroupsByClaim(t *testing.T) {
	a := types.NamespacedName{Namespace: "default", Name: "claim-a"}
	b := types.NamespacedName{Namespace: "default", Name: "claim-b"}
	client := fake.NewClientset(
		&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: a.Name}},
		&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: b.Name}},
	)
	np := &NetworkDriver{
		kubeClient:    client,
		driverName:    "test.driver",
		nodeName:      "node",
		eventRecorder: record.NewFakeRecorder(10),
	}
	failure := func(claim types.NamespacedName, device string) slaacFailure {
		return slaacFailure{claim: claim, notReady: &slaacNotReadyError{deviceName: device, ifName: device, err: errors.New("no address"), outcome: slaacDeleted}}
	}
	pod := &api.PodSandbox{Name: "pod", Namespace: "default", Uid: "pod-uid"}
	np.reportSLAACNotReady(klog.Background(), pod, []slaacFailure{failure(a, "dev0"), failure(a, "dev1"), failure(b, "dev2")})

	gotA := claimDevices(t, client, a, 2)
	for _, dev := range []string{"dev0", "dev1"} {
		if !hasCondition(gotA[dev], "SLAACReady", metav1.ConditionFalse) {
			t.Errorf("claim %s device %s has no SLAACReady=False condition: %+v", a, dev, gotA[dev])
		}
	}
	if gotB := claimDevices(t, client, b, 1); !hasCondition(gotB["dev2"], "SLAACReady", metav1.ConditionFalse) {
		t.Errorf("claim %s device dev2 has no SLAACReady=False condition", b)
	}
}

func TestRunPodSandboxSLAACPastDeadline(t *testing.T) {
	userns.Run(t, testRunPodSandboxSLAACPastDeadline_Namespaced, syscall.CLONE_NEWNET, syscall.CLONE_NEWNS)
}

// Once the runtime request deadline has passed the runtime starts the Pod
// regardless, so a SLAAC failure is recorded rather than ending the request:
// every device is still attached, the children that got an address get their
// routes, and the claim lists all devices.
func testRunPodSandboxSLAACPastDeadline_Namespaced(t *testing.T) {
	containerNsPath, containerNs, _ := testNetns(t)
	disableIPv6ByDefault(t, containerNs)

	// rail0 has a router, rail1 has none.
	router := addRail(t, "rail0", "rtr0")
	addRail(t, "rail1", "rtr1")
	_, prefix, err := net.ParseCIDR("2001:db8:10::/64")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go advertisePrefix(t, router.Attrs().Index, *prefix, stop)
	addHostDummy(t, "slp0")
	addHostDummy(t, "stat0")
	for _, name := range []string{"slp0", "stat0"} {
		link, err := nlwrap.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}

	claim := types.NamespacedName{Namespace: "default", Name: "claim-past"}
	device := func(host string, pod apis.InterfaceConfig, routes []apis.RouteConfig) DeviceConfig {
		cfg := apis.NetworkConfig{Interface: pod, Routes: routes}
		cfg.Default()
		return DeviceConfig{
			Claim:                        claim,
			NetworkInterfaceConfigInHost: apis.NetworkConfig{Interface: apis.InterfaceConfig{Name: host}},
			NetworkInterfaceConfigInPod:  cfg,
		}
	}
	ipvlanSLAAC := func(name string) apis.InterfaceConfig {
		return apis.InterfaceConfig{Name: name, Type: apis.InterfaceTypeIPVLAN, Addressing: apis.AddressingModeSLAAC}
	}
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"dev-rail0": device("rail0", ipvlanSLAAC("rdma0"), []apis.RouteConfig{{Destination: "2001:db8:99::/64", Gateway: "2001:db8:10::1"}}),
		"dev-rail1": device("rail1", ipvlanSLAAC("rdma1"), nil),
		"dev-slp":   device("slp0", apis.InterfaceConfig{Name: "slp0", Addressing: apis.AddressingModeSLAAC}, nil),
		"dev-stat":  device("stat0", apis.InterfaceConfig{Name: "stat0", Addresses: []string{"192.0.2.20/24"}}, nil),
	}}

	client := fake.NewClientset(&resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Namespace, Name: claim.Name}})
	recorder := record.NewFakeRecorder(20)
	np := &NetworkDriver{
		kubeClient:           client,
		driverName:           "test.driver",
		nodeName:             "node",
		eventRecorder:        recorder,
		podConfigStore:       mustNewPodConfigStore(),
		rdmaSharedMode:       true,
		slaacReadyTimeout:    400 * time.Millisecond,
		slaacRollbackReserve: DefaultSLAACRollbackReserve,
	}
	pod := &api.PodSandbox{
		Name: "pod", Namespace: "default", Uid: "pod-uid-past",
		Linux: &api.LinuxPodSandbox{Namespaces: []*api.LinuxNamespace{{Type: "network", Path: containerNsPath}}},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if err := np.runPodSandbox(ctx, pod, podConfig); err != nil {
		t.Fatalf("runPodSandbox() past the deadline returned %v, want nil: the runtime starts the Pod regardless", err)
	}

	nhNs, err := nlwrap.NewHandleAt(containerNs)
	if err != nil {
		t.Fatal(err)
	}
	defer nhNs.Close()
	for _, name := range []string{"rdma0", "rdma1", "slp0", "stat0"} {
		if _, err := nhNs.LinkByName(name); err != nil {
			t.Errorf("interface %s is not in the Pod: %v", name, err)
		}
	}
	rdma0, err := nhNs.LinkByName("rdma0")
	if err != nil {
		t.Fatal(err)
	}
	routes, err := nhNs.RouteList(rdma0, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	routed := false
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == "2001:db8:99::/64" {
			routed = true
		}
	}
	if !routed {
		t.Errorf("the child that got an address has no configured route: %v", routes)
	}

	var leftEvents int
	for len(recorder.Events) > 0 {
		e := <-recorder.Events
		if strings.Contains(e, "NetworkDeviceNotReady") {
			if !strings.Contains(e, "left in the Pod") {
				t.Errorf("event %q does not say the interface was left in the Pod", e)
			}
			leftEvents++
		}
	}
	if leftEvents != 2 {
		t.Errorf("got %d NetworkDeviceNotReady events, want 2 (rdma1 and slp0)", leftEvents)
	}

	got := claimDevices(t, client, claim, 4)
	for _, dev := range []string{"dev-rail1", "dev-slp"} {
		if !hasCondition(got[dev], "SLAACReady", metav1.ConditionFalse) {
			t.Errorf("device %s has no SLAACReady=False condition: %+v", dev, got[dev].Conditions)
		}
	}
	for _, dev := range []string{"dev-rail0", "dev-stat"} {
		if !hasCondition(got[dev], "Ready", metav1.ConditionTrue) {
			t.Errorf("device %s is not Ready: %+v", dev, got[dev].Conditions)
		}
	}
}
