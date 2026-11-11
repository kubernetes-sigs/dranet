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
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// slowPodDevices is how many links the slow pod claims. Moving a link into
// a pod namespace costs the kernel tens of milliseconds (an RCU grace
// period), so 96 of them keep RunPodSandbox well past containerd's default
// 2 s plugin_request_timeout. A claim holds at most 32 devices and a slice
// at most 128, hence 3 claims of 32.
const slowPodDevices = 96

// TestNRITimeout covers the NRI failure policy from #401. dranet registers
// with FailRequest and KeepConnected, so when RunPodSandbox exceeds the
// runtime's plugin_request_timeout the pod must fail to start instead of
// starting without its network interfaces, and dranet must stay registered
// so later pods are served.
//
// The policy needs containerd with NRI v0.12.4 or newer. On an older
// runtime the request proceeds without dranet, the pod starts with no
// interfaces and the plugin is dropped; this test fails there.
func TestNRITimeout(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx
	prefix := e.addDummyLinks(slowPodDevices)

	err := wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		slices, err := tc.nodeSlices(ctx)
		if err != nil {
			return false, err
		}
		n := 0
		for _, s := range slices {
			for _, d := range s.Spec.Devices {
				if attr, ok := d.Attributes["dra.net/ifName"]; ok && attr.StringValue != nil && strings.HasPrefix(*attr.StringValue, prefix) {
					n++
				}
			}
		}
		return n == slowPodDevices, nil
	})
	if err != nil {
		t.Fatalf("%d devices with prefix %s were not published: %v", slowPodDevices, prefix, err)
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
							Count:           resourceapi.AllocationResultsMaxSize,
							Selectors: []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{
								Expression: fmt.Sprintf(`has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName.startsWith(%q)`, prefix),
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

	disconnectsBefore, err := nriDisconnects(ctx)
	if err != nil {
		t.Fatal(err)
	}

	pods := tc.client.CoreV1().Pods(e.ns)
	slow := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "slow"},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To[int64](5),
			Containers: []v1.Container{{
				Name:  "pause",
				Image: tc.pauseImage,
			}},
		},
	}
	for i := 0; i < slowPodDevices/resourceapi.AllocationResultsMaxSize; i++ {
		name := fmt.Sprintf("nic%d", i)
		slow.Spec.ResourceClaims = append(slow.Spec.ResourceClaims, v1.PodResourceClaim{Name: name, ResourceClaimTemplateName: ptr.To("nic")})
		slow.Spec.Containers[0].Resources.Claims = append(slow.Spec.Containers[0].Resources.Claims, v1.ResourceClaim{Name: name})
	}
	if _, err := pods.Create(ctx, slow, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Either the runtime fails the sandbox and the kubelet records why, or
	// the runtime proceeds without dranet and the pod starts. The second is
	// the bug the failure policy prevents.
	var sandboxErr string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := pods.Get(ctx, slow.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase == v1.PodRunning {
			return false, fmt.Errorf("pod %s started although RunPodSandbox exceeded the NRI request timeout: the runtime proceeded without dranet", pod.Name)
		}
		events, err := tc.client.CoreV1().Events(e.ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + slow.Name + ",reason=FailedCreatePodSandBox",
		})
		if err != nil {
			return false, err
		}
		for _, ev := range events.Items {
			if strings.Contains(ev.Message, driverName) {
				sandboxErr = ev.Message
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("waiting for the sandbox of pod %s to fail on the NRI timeout: %v", slow.Name, err)
	}
	t.Logf("sandbox failed as expected: %s", sandboxErr)

	disconnectsAfter, err := nriDisconnects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if disconnectsAfter != disconnectsBefore {
		t.Errorf("the runtime dropped the plugin on the timeout: disconnects went from %d to %d, want KeepConnected to hold", disconnectsBefore, disconnectsAfter)
	}

	// dranet must still serve pods after the timeout.
	link := e.addDummyLink("192.168.79.10/24")
	err = wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		_, ok, err := deviceByIfName(ctx, link)
		return ok, err
	})
	if err != nil {
		t.Fatalf("device for %s was not published: %v", link, err)
	}
	one := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "one"},
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
	if _, err := tc.client.ResourceV1().ResourceClaimTemplates(e.ns).Create(ctx, one, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	after := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "after"},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To[int64](5),
			ResourceClaims:                []v1.PodResourceClaim{{Name: "nic", ResourceClaimTemplateName: ptr.To("one")}},
			Containers: []v1.Container{{
				Name:      "pause",
				Image:     tc.pauseImage,
				Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "nic"}}},
			}},
		},
	}
	if _, err := pods.Create(ctx, after, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := pods.Get(ctx, after.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("pod %s did not reach Running after the timeout of pod %s: %v", after.Name, slow.Name, err)
	}
	onHost, _, err := hostLink(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if onHost {
		t.Errorf("link %s is still on the node while pod %s runs", link, after.Name)
	}
}

// nriDisconnects scrapes dranet's metrics inside the node and returns how
// many times the runtime closed the NRI connection.
func nriDisconnects(ctx context.Context) (int, error) {
	const metric = "dranet_driver_nri_plugin_disconnects_total"
	out, err := tc.nodeExec(ctx, "bash", "-c",
		`exec 3<>/dev/tcp/127.0.0.1/9177; printf 'GET /metrics HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3; cat <&3`)
	if err != nil {
		return 0, fmt.Errorf("scrape dranet metrics: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if name, value, ok := strings.Cut(sc.Text(), " "); ok && name == metric {
			n, err := strconv.ParseFloat(value, 64)
			return int(n), err
		}
	}
	return 0, fmt.Errorf("metric %s not found", metric)
}
