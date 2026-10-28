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
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// TestSmoke covers the basic lifecycle: a link on the node is published, a
// pod claims it, the link moves into the pod with its address, and after the
// pod is gone the link is back on the node and the slice reflects that.
func TestSmoke(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	const cidr = "192.168.77.10/24"
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
								Expression: fmt.Sprintf(`has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName == %q`, link),
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
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke"},
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
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err = pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("pod %s did not reach Running: %v", pod.Name, err)
	}

	onHost, _, err := hostLink(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if onHost {
		t.Errorf("link %s is still on the node while pod %s runs", link, pod.Name)
	}

	var claimName string
	for _, s := range pod.Status.ResourceClaimStatuses {
		if s.Name == "nic" && s.ResourceClaimName != nil {
			claimName = *s.ResourceClaimName
		}
	}
	if claimName == "" {
		t.Fatalf("pod %s has no generated claim: %v", pod.Name, pod.Status.ResourceClaimStatuses)
	}
	claims := tc.client.ResourceV1().ResourceClaims(e.ns)
	claim, err := claims.Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status.Allocation == nil {
		t.Fatalf("claim %s is not allocated", claim.Name)
	}
	var ips []string
	for _, d := range claim.Status.Devices {
		if d.NetworkData != nil {
			ips = append(ips, d.NetworkData.IPs...)
		}
	}
	if !slices.Contains(ips, cidr) {
		t.Errorf("claim %s networkData.ips = %v, want it to contain %s", claim.Name, ips, cidr)
	}

	// A virtual link is not visible on the node while a pod has it, so the
	// driver must stop publishing it; otherwise a second pod could be
	// scheduled onto a device the node cannot hand out.
	err = wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		_, ok, err := deviceByIfName(ctx, link)
		return !ok, err
	})
	if err != nil {
		t.Errorf("device for %s is still published while pod %s has the link: %v", link, pod.Name, err)
	}

	if err := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("pod %s was not removed: %v", pod.Name, err)
	}
	var addrs []string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		var ok bool
		ok, addrs, err = hostLink(ctx, link)
		return ok, err
	})
	if err != nil {
		t.Fatalf("link %s did not return to the node: %v", link, err)
	}
	t.Logf("link %s is back on the node with addresses %v", link, addrs)

	// The claim is owned by the pod, so it must go away with it.
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := claims.Get(ctx, claim.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Errorf("claim %s was not deleted with its pod: %v", claim.Name, err)
	}

	// The kernel drops the address when the link changes namespace, so the
	// republished device must not carry one.
	var dev resourceapi.Device
	err = wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		var ok bool
		dev, ok, err = deviceByIfName(ctx, link)
		return ok, err
	})
	if err != nil {
		t.Fatalf("device for %s was not published again after the pod was removed: %v", link, err)
	}
	if ip, ok := dev.Attributes["dra.net/ipv4"]; ok {
		t.Errorf("device %s advertises dra.net/ipv4=%v after the link returned without an address", dev.Name, *ip.StringValue)
	}
}
