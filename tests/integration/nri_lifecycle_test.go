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
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// httpGetNode fetches url inside the kind node container and returns the HTTP
// status code and response body without failing on non-2xx responses.
func httpGetNode(ctx context.Context, url string) (int, string, error) {
	const marker = "\n__HTTP_STATUS__:"
	out, err := tc.nodeExec(ctx, "curl", "-sS", "-w", marker+"%{http_code}", url)
	if err != nil {
		return 0, "", err
	}
	idx := strings.LastIndex(out, marker)
	if idx < 0 {
		return 0, "", fmt.Errorf("missing status marker in curl output: %q", out)
	}
	body := out[:idx]
	codeStr := strings.TrimSpace(out[idx+len(marker):])
	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return 0, "", fmt.Errorf("parse HTTP status %q: %w", codeStr, err)
	}
	return code, body, nil
}

func dranetMainPID(ctx context.Context) (string, error) {
	out, err := tc.nodeExec(ctx, "systemctl", "show", "-p", "MainPID", "--value", dranetUnit)
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(out)
	if pid == "" || pid == "0" {
		return "", fmt.Errorf("dranet MainPID=%q", pid)
	}
	return pid, nil
}

func dranetMetric(ctx context.Context, metric string) (int, error) {
	out, err := tc.nodeExec(ctx, "curl", "-fsS", "http://127.0.0.1:9177/metrics")
	if err != nil {
		return 0, fmt.Errorf("scrape dranet metrics: %w", err)
	}
	return parseMetricInt(out, metric)
}

// TestNRILifecycleAndContainerdRestart verifies /healthz (dra + nri checks,
// verbose output, and ?exclude=nri), PrepareResourceClaims gating during an
// NRI outage, and automatic NRI reconnection without crashing dranet across
// containerd restarts.
func TestNRILifecycleAndContainerdRestart(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx

	// Ensure containerd is always running when the test exits even on failure.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = tc.nodeExec(cleanupCtx, "systemctl", "start", "containerd")
	})

	// 1. Verify /healthz endpoints report healthy DRA and NRI.
	code, body, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if code != 200 || body != "ok" {
		t.Fatalf("GET /healthz = (%d, %q), want (200, \"ok\")", code, body)
	}
	code, body, err = httpGetNode(ctx, "http://127.0.0.1:9177/healthz?verbose")
	if err != nil {
		t.Fatalf("GET /healthz?verbose: %v", err)
	}
	if code != 200 {
		t.Fatalf("GET /healthz?verbose status = %d, want 200:\n%s", code, body)
	}
	for _, want := range []string{"[+]dra ok", "[+]nri ok", "healthz check passed"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /healthz?verbose body = %q, missing %q", body, want)
		}
	}
	for _, sub := range []string{"/healthz/dra", "/healthz/nri"} {
		c, b, err := httpGetNode(ctx, "http://127.0.0.1:9177"+sub)
		if err != nil {
			t.Fatalf("GET %s: %v", sub, err)
		}
		if c != 200 || b != "ok" {
			t.Errorf("GET %s = (%d, %q), want (200, \"ok\")", sub, c, b)
		}
	}

	pidBefore, err := dranetMainPID(ctx)
	if err != nil {
		t.Fatalf("dranetMainPID: %v", err)
	}
	disconnectsBefore, err := dranetMetric(ctx, "dranet_driver_nri_plugin_disconnects_total")
	if err != nil {
		t.Fatalf("read disconnects metric: %v", err)
	}
	reconnectsBefore, err := dranetMetric(ctx, "dranet_driver_nri_plugin_reconnect_attempts_total")
	if err != nil {
		t.Fatalf("read reconnects metric: %v", err)
	}

	// 2. Create two dummy links and start pod-before holding link1.
	const (
		cidr1 = "192.168.80.10/24"
		cidr2 = "192.168.80.20/24"
	)
	link1 := e.addDummyLink(cidr1)
	link2 := e.addDummyLink(cidr2)

	for _, link := range []string{link1, link2} {
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
	}

	createTemplate := func(name, ifName string) {
		t.Helper()
		tmpl := &resourceapi.ResourceClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: resourceapi.ResourceClaimTemplateSpec{
				Spec: resourceapi.ResourceClaimSpec{
					Devices: resourceapi.DeviceClaim{
						Requests: []resourceapi.DeviceRequest{{
							Name: "nic",
							Exactly: &resourceapi.ExactDeviceRequest{
								DeviceClassName: driverName,
								Selectors: []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{
									Expression: fmt.Sprintf(`has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName == %q`, ifName),
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
	}
	createTemplate("nic1", link1)
	createTemplate("nic2", link2)

	pods := tc.client.CoreV1().Pods(e.ns)
	makePod := func(name, tmpl string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1.PodSpec{
				TerminationGracePeriodSeconds: ptr.To[int64](5),
				ResourceClaims:                []v1.PodResourceClaim{{Name: "nic", ResourceClaimTemplateName: ptr.To(tmpl)}},
				Containers: []v1.Container{{
					Name:      "pause",
					Image:     tc.pauseImage,
					Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "nic"}}},
				}},
			},
		}
	}

	if _, err := pods.Create(ctx, makePod("pod-before", "nic1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		p, err := pods.Get(ctx, "pod-before", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("pod-before did not reach Running: %v", err)
	}
	if onHost, _, err := hostLink(ctx, link1); err != nil || onHost {
		t.Fatalf("link1 onHost=%v (err=%v) while pod-before runs, want false", onHost, err)
	}

	// 3. Stop containerd briefly to observe /healthz transitions and schedule
	// pod-during-outage while NRI is disconnected. Keep the outage short (<3s)
	// so it stays well within dranet's exponential backoff window (1+2+4+8+16=31s).
	if _, err := tc.nodeExec(ctx, "systemctl", "stop", "containerd"); err != nil {
		t.Fatalf("stop containerd: %v", err)
	}

	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		c, b, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz")
		if err != nil {
			return false, err
		}
		return c == 503 && strings.Contains(b, "[+]dra ok") && strings.Contains(b, "[-]nri failed:"), nil
	})
	if err != nil {
		t.Fatalf("/healthz did not report NRI failure with DRA healthy while containerd was stopped: %v", err)
	}

	if c, _, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz/dra"); err != nil || c != 200 {
		t.Errorf("GET /healthz/dra during NRI outage = (%d, %v), want 200", c, err)
	}
	if c, _, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz/nri"); err != nil || c != 503 {
		t.Errorf("GET /healthz/nri during NRI outage = (%d, %v), want 503", c, err)
	}
	if c, b, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz?exclude=nri&verbose"); err != nil || c != 200 || !strings.Contains(b, "[+]nri excluded: ok") {
		t.Errorf("GET /healthz?exclude=nri&verbose during NRI outage = (%d, %q, %v), want 200 with nri excluded", c, b, err)
	}

	// Create pod-after while NRI is disconnected; kubelet must not start it
	// without its network interface, and must converge once containerd returns.
	if _, err := pods.Create(ctx, makePod("pod-after", "nic2"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// 4. Start containerd and verify dranet reconnects in-place without restarting.
	if _, err := tc.nodeExec(ctx, "systemctl", "start", "containerd"); err != nil {
		t.Fatalf("start containerd: %v", err)
	}

	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		c, b, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz")
		if err != nil {
			return false, nil
		}
		return c == 200 && b == "ok", nil
	})
	if err != nil {
		t.Fatalf("/healthz did not recover to 200 ok after starting containerd: %v", err)
	}

	pidAfterFirst, err := dranetMainPID(ctx)
	if err != nil {
		t.Fatalf("dranetMainPID after first restart: %v", err)
	}
	if pidAfterFirst != pidBefore {
		t.Errorf("dranet restarted across containerd restart: PID changed from %s to %s", pidBefore, pidAfterFirst)
	}

	disconnectsAfter, err := dranetMetric(ctx, "dranet_driver_nri_plugin_disconnects_total")
	if err != nil {
		t.Fatalf("read disconnects metric after recovery: %v", err)
	}
	if disconnectsAfter <= disconnectsBefore {
		t.Errorf("disconnects metric = %d, want > %d", disconnectsAfter, disconnectsBefore)
	}
	reconnectsAfter, err := dranetMetric(ctx, "dranet_driver_nri_plugin_reconnect_attempts_total")
	if err != nil {
		t.Fatalf("read reconnects metric after recovery: %v", err)
	}
	if reconnectsAfter <= reconnectsBefore {
		t.Errorf("reconnects metric = %d, want > %d", reconnectsAfter, reconnectsBefore)
	}

	// 5. Verify pod-before kept link1 + cidr1 across the containerd restart, and
	// pod-after reaches Running with link2 moved into its namespace and its claim
	// status populated (allow 2*podTimeout for kubelet CRI/PLEG recovery backoff).
	podBefore, err := pods.Get(ctx, "pod-before", metav1.GetOptions{})
	if err != nil || podBefore.Status.Phase != v1.PodRunning {
		t.Fatalf("pod-before after containerd restart = (%v, %v), want Running", podBefore, err)
	}
	if onHost, _, err := hostLink(ctx, link1); err != nil || onHost {
		t.Errorf("link1 onHost=%v (err=%v) after containerd restart, want false (still in pod-before)", onHost, err)
	}

	var podAfter *v1.Pod
	err = wait.PollUntilContextTimeout(ctx, pollInterval, 2*podTimeout, true, func(ctx context.Context) (bool, error) {
		var err error
		podAfter, err = pods.Get(ctx, "pod-after", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return podAfter.Status.Phase == v1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("pod-after did not reach Running after containerd recovery: %v", err)
	}
	if onHost, _, err := hostLink(ctx, link2); err != nil || onHost {
		t.Errorf("link2 onHost=%v (err=%v) while pod-after runs, want false", onHost, err)
	}

	var claim2Name string
	for _, s := range podAfter.Status.ResourceClaimStatuses {
		if s.Name == "nic" && s.ResourceClaimName != nil {
			claim2Name = *s.ResourceClaimName
		}
	}
	if claim2Name == "" {
		t.Fatalf("pod-after has no generated claim: %v", podAfter.Status.ResourceClaimStatuses)
	}
	var claim2IPs []string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		claim, err := tc.client.ResourceV1().ResourceClaims(e.ns).Get(ctx, claim2Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		claim2IPs = nil
		for _, d := range claim.Status.Devices {
			if d.NetworkData != nil {
				claim2IPs = append(claim2IPs, d.NetworkData.IPs...)
			}
		}
		return slices.Contains(claim2IPs, cidr2), nil
	})
	if err != nil {
		t.Errorf("claim %s networkData.ips = %v, want %s: %v", claim2Name, claim2IPs, cidr2, err)
	}

	// 6. Delete pod-after (created on the reconnected NRI session) and verify
	// StopPodSandbox detaches link2 back to the host namespace. Also delete
	// pod-before and verify its claim is unprepared and removed (note: upstream
	// containerd internal/nri syncPlugin restores container state but omits pod
	// sandbox state, so containerd skips NRI StopPodSandbox for pre-restart
	// sandboxes and kernel netns teardown deletes virtual dummy link1 whereas
	// hardware NICs return to init_net).
	if err := pods.Delete(ctx, "pod-after", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, "pod-after", metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("pod-after was not removed: %v", err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		ok, _, err := hostLink(ctx, link2)
		return ok, err
	})
	if err != nil {
		t.Fatalf("link2 (%s) did not return to the node after pod-after deletion: %v", link2, err)
	}

	var claim1Name string
	for _, s := range podBefore.Status.ResourceClaimStatuses {
		if s.Name == "nic" && s.ResourceClaimName != nil {
			claim1Name = *s.ResourceClaimName
		}
	}
	if err := pods.Delete(ctx, "pod-before", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, "pod-before", metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("pod-before was not removed: %v", err)
	}
	if claim1Name != "" {
		err = wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := tc.client.ResourceV1().ResourceClaims(e.ns).Get(ctx, claim1Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
		if err != nil {
			t.Errorf("claim %s was not deleted with pod-before: %v", claim1Name, err)
		}
	}

	// 7. Restart containerd a second time to verify the consecutive failure
	// streak reset and dranet stays alive with the same PID.
	if _, err := tc.nodeExec(ctx, "systemctl", "restart", "containerd"); err != nil {
		t.Fatalf("systemctl restart containerd: %v", err)
	}
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		c, b, err := httpGetNode(ctx, "http://127.0.0.1:9177/healthz")
		if err != nil {
			return false, nil
		}
		return c == 200 && b == "ok", nil
	})
	if err != nil {
		t.Fatalf("/healthz did not recover after second containerd restart: %v", err)
	}
	pidAfterSecond, err := dranetMainPID(ctx)
	if err != nil {
		t.Fatalf("dranetMainPID after second restart: %v", err)
	}
	if pidAfterSecond != pidBefore {
		t.Errorf("dranet restarted on second containerd restart: PID changed from %s to %s", pidBefore, pidAfterSecond)
	}
}
