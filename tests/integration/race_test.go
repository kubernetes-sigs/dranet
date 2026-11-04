//go:build integration

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

package integration

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// ipRestoreDelay stands in for the host agent (DHCP, guest agent) that puts
// the address back on a link after the kernel wiped it on the namespace
// move. On the affected nodes this takes 1-2 seconds.
const ipRestoreDelay = 2 * time.Second

// inventorySettle lets the inventory rate limiter refill (burst 5, one
// token per 2 s by default). On a quiet node the bucket is full, so the scan
// triggered by the link return runs at once, inside the window between
// StopPodSandbox and unprepare. With the bucket drained by the setup
// activity the scan is delayed past unprepare and the window never opens.
const inventorySettle = 12 * time.Second

// rescheduleBound is how long after the address restore pod 2 may take to
// be scheduled. An address change must reach the slice through an event and
// the rate limiter (2 s), not through the max poll interval (1 min).
const rescheduleBound = 10 * time.Second

// TestFastReschedule is race.md scenario 1. Pod 1 holds a link; pod 2 waits
// for the same link with the CEL mitigation that demands dra.net/ipv4. When
// pod 1 is deleted the link returns to the node without its address. The
// scheduler must not place pod 2 until the address is back and published,
// and pod 2's claim must end up with that address.
//
// The scenario runs with PersistentResourceSliceAttributes on and off: the
// snapshot merge changes what the slice advertises in the window between
// StopPodSandbox and unprepare. That window is about as long as one
// inventory scan on a kind node, so with the gate on the stale dra.net/ipv4
// path is hit on some runs and not on others; the slow reschedule after the
// address restore is hit on every run.
func TestFastReschedule(t *testing.T) {
	for _, gate := range []string{"false", "true"} {
		t.Run("PersistentResourceSliceAttributes="+gate, func(t *testing.T) {
			withDranetArgs(t, "--feature-gates=PersistentResourceSliceAttributes="+gate)
			testFastReschedule(t)
		})
	}
}

func testFastReschedule(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	const cidr = "192.168.78.10/24"
	link := e.addDummyLink(cidr)

	err := wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		dev, ok, err := deviceByIfName(ctx, link)
		if err != nil {
			return false, err
		}
		_, hasIP := dev.Attributes["dra.net/ipv4"]
		return ok && hasIP, nil
	})
	if err != nil {
		t.Fatalf("device for %s was not published with dra.net/ipv4: %v", link, err)
	}

	tmpl := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "nic"},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{
					Requests: []resourceapi.DeviceRequest{{
						Name: "nic",
						Exactly: &resourceapi.ExactDeviceRequest{
							DeviceClassName: driverName,
							Selectors: []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{
								// The selector includes the mitigation given to the customer in race.md.
								Expression: fmt.Sprintf(`has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName == %q`+
									` && has(device.attributes["dra.net"].ipv4) && device.attributes["dra.net"].ipv4 != ""`, link),
							}}},
						},
					}},
				},
			},
		},
	}
	if _, err := tc.client.ResourceV1().ResourceClaimTemplates(e.ns).Create(ctx, tmpl, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	pods := tc.client.CoreV1().Pods(e.ns)
	createPod := func(name string) {
		t.Helper()
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1.PodSpec{
				TerminationGracePeriodSeconds: ptr.To[int64](5),
				ResourceClaims:                []v1.PodResourceClaim{{Name: "nic", ResourceClaimTemplateName: ptr.To("nic")}},
				Containers: []v1.Container{{
					Name:      "pause",
					Image:     tc.pauseImage,
					Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "nic"}}},
				}},
			},
		}
		if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	waitRunning := func(name string, timeout time.Duration) *v1.Pod {
		t.Helper()
		var pod *v1.Pod
		err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
			var err error
			pod, err = pods.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return pod.Status.Phase == v1.PodRunning, nil
		})
		if err != nil {
			t.Fatalf("pod %s did not reach Running: %v", name, err)
		}
		return pod
	}
	claimIPs := func(pod *v1.Pod) []string {
		t.Helper()
		var ips []string
		for _, s := range pod.Status.ResourceClaimStatuses {
			if s.ResourceClaimName == nil {
				continue
			}
			claim, err := tc.client.ResourceV1().ResourceClaims(e.ns).Get(ctx, *s.ResourceClaimName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range claim.Status.Devices {
				if d.NetworkData != nil {
					ips = append(ips, d.NetworkData.IPs...)
				}
			}
		}
		return ips
	}

	createPod("pod1")
	pod1 := waitRunning("pod1", podTimeout)
	if got := claimIPs(pod1); !slices.Contains(got, cidr) {
		t.Fatalf("pod1 claim networkData.ips = %v, want %s", got, cidr)
	}

	// Pod 2 asks for the same link and must stay pending while pod 1 has it.
	createPod("pod2")
	scheduled := watchScheduled(t, ctx, e.ns, "pod2")
	time.Sleep(inventorySettle)
	if when, ok := scheduled(); ok {
		t.Fatalf("pod2 was scheduled at %s while pod1 holds the link", when.Format(time.RFC3339Nano))
	}

	// Delete pod 1. The link comes back to the node with no address; then the
	// "host agent" restores it after ipRestoreDelay. During that window the
	// slice must not advertise dra.net/ipv4 for the link.
	if err := pods.Delete(ctx, "pod1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var addrs []string
	err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, podTimeout, true, func(ctx context.Context) (bool, error) {
		var ok bool
		ok, addrs, err = hostLink(ctx, link)
		return ok, err
	})
	if err != nil {
		t.Fatalf("link %s did not return to the node: %v", link, err)
	}
	returned := time.Now()
	if len(addrs) != 0 {
		t.Fatalf("link %s returned to the node with addresses %v; the kernel is expected to drop them", link, addrs)
	}
	time.Sleep(ipRestoreDelay)
	// If the link is not on the node any more, pod 2 already took it: that
	// is the bug, reported below with the scheduling time and the claim.
	onHost, _, err := hostLink(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if onHost {
		if out, err := tc.nodeExec(ctx, "ip", "addr", "add", cidr, "dev", link); err != nil {
			t.Fatalf("restore address: %s", out)
		}
	} else {
		t.Errorf("link %s left the node before the address was restored: pod2 took it without an address", link)
	}
	restored := time.Now()
	t.Logf("link %s returned at %s, address restored at %s", link, returned.Format(time.RFC3339Nano), restored.Format(time.RFC3339Nano))

	pod2 := waitRunning("pod2", 2*sliceTimeout)
	when, ok := scheduled()
	if !ok {
		t.Fatalf("pod2 is Running but the watch never saw a node assigned")
	}
	switch {
	case when.Before(returned):
		t.Errorf("pod2 was scheduled at %s, before the link returned to the node at %s: the slice advertised a device the node did not have",
			when.Format(time.RFC3339Nano), returned.Format(time.RFC3339Nano))
	case when.Before(restored):
		t.Errorf("pod2 was scheduled at %s, before the address was restored at %s: the slice advertised a stale dra.net/ipv4",
			when.Format(time.RFC3339Nano), restored.Format(time.RFC3339Nano))
	case when.Sub(restored) > rescheduleBound:
		t.Errorf("pod2 was scheduled %s after the address was restored, want at most %s: the address change did not trigger a rescan",
			when.Sub(restored).Round(time.Millisecond), rescheduleBound)
	default:
		t.Logf("pod2 scheduled %s after the address was restored", when.Sub(restored).Round(time.Millisecond))
	}
	if got := claimIPs(pod2); !slices.Contains(got, cidr) {
		t.Errorf("pod2 claim networkData.ips = %v, want it to contain %s", got, cidr)
	}
}

// watchScheduled starts a watch on the pod and returns a function that
// reports when, on the test's clock, the pod first had a node assigned.
// The PodScheduled condition only has second precision, which is too coarse
// for the windows measured here.
func watchScheduled(t *testing.T, ctx context.Context, ns, name string) func() (time.Time, bool) {
	t.Helper()
	w, err := tc.client.CoreV1().Pods(ns).Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		t.Fatalf("watch pod %s: %v", name, err)
	}
	t.Cleanup(w.Stop)
	var (
		mu sync.Mutex
		at time.Time
		ok bool
	)
	go func() {
		for ev := range w.ResultChan() {
			pod, isPod := ev.Object.(*v1.Pod)
			if !isPod || pod.Spec.NodeName == "" {
				continue
			}
			mu.Lock()
			at, ok = time.Now(), true
			mu.Unlock()
			w.Stop()
			return
		}
	}()
	return func() (time.Time, bool) {
		mu.Lock()
		defer mu.Unlock()
		return at, ok
	}
}
