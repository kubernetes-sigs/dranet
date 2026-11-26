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
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// TestMissingDeviceIsReported checks that DRANET reports a pod that runs
// without its claimed network device.
//
// The test kills DRANET and stops the sandbox of a running pod. A killed
// process does not remove its registration socket, so the kubelet keeps the
// driver as registered. The kubelet does not prepare the claim again, and it
// makes a new sandbox while the runtime does not have the DRANET NRI plugin.
// The new sandbox does not have the device. DRANET cannot prevent this, but
// when it starts again it must report the pod with an event and a metric.
func TestMissingDeviceIsReported(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx

	// waitHealthy returns when DRANET has its NRI and DRA plugins ready.
	waitHealthy := func(ctx context.Context) error {
		return wait.PollUntilContextTimeout(ctx, pollInterval, 60*time.Second, true, func(ctx context.Context) (bool, error) {
			code, _, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz")
			return err == nil && code == 200, nil
		})
	}

	// DRANET must run and be healthy when the test exits, also on failure,
	// because the next test starts with it.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := tc.nodeExec(cleanupCtx, "systemctl", "is-active", "--quiet", dranetUnit); err != nil {
			if err := tc.restartDranet(cleanupCtx); err != nil {
				t.Errorf("restart dranet: %v", err)
			}
		}
		if err := waitHealthy(cleanupCtx); err != nil {
			t.Errorf("dranet is not healthy at the end of the test: %v", err)
		}
	})

	link := e.addDummyLink("192.168.83.10/24")
	err := wait.PollUntilContextTimeout(ctx, pollInterval, sliceTimeout, true, func(ctx context.Context) (bool, error) {
		_, ok, err := deviceByIfName(ctx, link)
		return ok, err
	})
	if err != nil {
		t.Fatalf("device for %s was not published: %v", link, err)
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

	const podName = "missing-device"
	pods := tc.client.CoreV1().Pods(e.ns)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName},
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
		p, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("pod %s did not reach Running: %v", podName, err)
	}

	// readySandbox returns the id of the Ready sandbox of the pod, or "".
	readySandbox := func(ctx context.Context) string {
		out, _ := tc.nodeExec(ctx, "crictl", "pods", "--name", podName, "--namespace", e.ns, "--state", "Ready", "-q")
		return strings.TrimSpace(out)
	}
	oldSandbox := readySandbox(ctx)
	if oldSandbox == "" {
		t.Fatalf("pod %s has no Ready sandbox", podName)
	}

	// 1. DRANET stops without a graceful shutdown.
	if out, err := tc.nodeExec(ctx, "systemctl", "kill", "-s", "KILL", dranetUnit); err != nil {
		t.Fatalf("kill dranet: %s", out)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := tc.nodeExec(ctx, "systemctl", "is-active", "--quiet", dranetUnit)
		return err != nil, nil
	})
	if err != nil {
		t.Fatalf("dranet did not stop after SIGKILL: %v", err)
	}

	// 2. The sandbox of the pod stops. The kubelet makes a new one and starts
	// the container again, without the device. The default timeout of crictl
	// is 2s, which is too short for the stop of a sandbox on a slow machine.
	if out, err := tc.nodeExec(ctx, "crictl", "--timeout=30s", "stopp", oldSandbox); err != nil {
		t.Fatalf("stop sandbox %s: %s", oldSandbox, out)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		id := readySandbox(ctx)
		if id == "" || id == oldSandbox {
			return false, nil
		}
		out, _ := tc.nodeExec(ctx, "crictl", "ps", "--pod", id, "--state", "Running", "-q")
		return strings.TrimSpace(out) != "", nil
	})
	if err != nil {
		t.Fatalf("the kubelet did not make a new sandbox with a running container for pod %s while dranet was down: %v", podName, err)
	}

	// 3. DRANET starts again and reports the pod.
	if err := tc.restartDranet(ctx); err != nil {
		t.Fatalf("restart dranet: %v", err)
	}
	if err := waitHealthy(ctx); err != nil {
		t.Fatalf("dranet is not healthy after the restart: %v", err)
	}
	var messages []string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		events, err := tc.client.CoreV1().Events(e.ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + podName + ",reason=NetworkDeviceMissing",
		})
		if err != nil {
			return false, err
		}
		messages = nil
		for _, ev := range events.Items {
			messages = append(messages, ev.Type+": "+ev.Message)
		}
		return len(messages) > 0, nil
	})
	if err != nil {
		t.Fatalf("no NetworkDeviceMissing event for pod %s: %v", podName, err)
	}
	if !strings.HasPrefix(messages[0], v1.EventTypeWarning+": ") {
		t.Errorf("NetworkDeviceMissing event = %q, want a Warning event", messages[0])
	}
	t.Logf("event on pod %s: %s", podName, messages[0])
	found, err := dranetMetric(ctx, "dranet_driver_pods_missing_devices_total")
	if err != nil {
		t.Fatalf("read the metric: %v", err)
	}
	if found < 1 {
		t.Errorf("dranet_driver_pods_missing_devices_total = %d, want at least 1", found)
	}
}
