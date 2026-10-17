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

package ocihook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtocolRoundTrip(t *testing.T) {
	request, err := encodeRequest("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if request != "READY 0123456789abcdef\n" {
		t.Fatalf("request = %q", request)
	}
	id, err := decodeRequest(strings.TrimSuffix(request, "\n"))
	if err != nil || id != "0123456789abcdef" {
		t.Fatalf("decodeRequest = %q, %v", id, err)
	}

	if got := encodeResponse(nil); got != "OK\n" {
		t.Fatalf("OK response = %q", got)
	}
	if err := decodeResponse("OK"); err != nil {
		t.Fatalf("decodeResponse(OK) = %v", err)
	}
	// Reasons are flattened to one line so the hook reads one answer.
	got := encodeResponse(errors.New("webhook returned HTTP 500:\n  boom\n"))
	if got != "FAIL webhook returned HTTP 500: boom\n" {
		t.Fatalf("FAIL response = %q", got)
	}
	if err := decodeResponse(strings.TrimSuffix(got, "\n")); err == nil || err.Error() != "webhook returned HTTP 500: boom" {
		t.Fatalf("decodeResponse(FAIL) = %v", err)
	}
	if err := decodeResponse("FAIL"); err == nil || err.Error() != "unknown error" {
		t.Fatalf("decodeResponse(FAIL without reason) = %v", err)
	}
	if got := encodeResponse(errors.New(" \n ")); got != "FAIL unknown error\n" {
		t.Fatalf("blank reason = %q", got)
	}
}

func TestProtocolRejectsMalformedInput(t *testing.T) {
	for _, id := range []string{"", "with space", "tab\tin", "nul\x00", strings.Repeat("a", 257), "ünïcode"} {
		if _, err := encodeRequest(id); err == nil {
			t.Errorf("encodeRequest(%q) accepted an invalid container ID", id)
		}
	}
	for _, line := range []string{"", "READY", "READY ", "HELLO abc", "OK abc"} {
		if _, err := decodeRequest(line); err == nil {
			t.Errorf("decodeRequest(%q) accepted an invalid request", line)
		}
	}
	for _, line := range []string{"", "READY abc", "ok", "DONE"} {
		if err := decodeResponse(line); err == nil {
			t.Errorf("decodeResponse(%q) accepted an invalid response", line)
		}
	}
	long := encodeResponse(errors.New(strings.Repeat("x", 2*MaxLineLength)))
	if len(long) > MaxLineLength {
		t.Fatalf("response of %d bytes is over the line limit", len(long))
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxLineLength+1) + "\n"))); err == nil {
		t.Fatal("readLine accepted a line over the limit")
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("OK"))); err == nil {
		t.Fatal("readLine accepted an unterminated line")
	}
}

func TestTimeouts(t *testing.T) {
	if err := ValidateTimeout(DefaultTimeout); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTimeout(MaxTimeout); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTimeout(ProviderTimeoutMargin + ClientTimeoutMargin); err == nil {
		t.Fatal("a timeout that leaves no room for the hook was accepted")
	}
	if err := ValidateTimeout(MaxTimeout + time.Second); err == nil {
		t.Fatal("a timeout over MaxTimeout was accepted")
	}
	if ProviderTimeout(DefaultTimeout) >= ClientTimeout(DefaultTimeout) || ClientTimeout(DefaultTimeout) >= DefaultTimeout {
		t.Fatalf("the provider (%v) must give up before the hook (%v), and the hook before the runtime (%v)",
			ProviderTimeout(DefaultTimeout), ClientTimeout(DefaultTimeout), DefaultTimeout)
	}
}

// startServer serves handler on a socket in a temporary directory until the
// test ends.
func startServer(t *testing.T, timeout time.Duration, handler Handler) *Server {
	t.Helper()
	server, err := Listen(socketPath(t), timeout, handler)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return server
}

// socketPath keeps the path short: UNIX socket paths are limited to 108
// bytes and test temporary directories are long.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ocihook")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "hook.sock")
}

func TestHookSignalsTheDaemonAndWaitsForItsAnswer(t *testing.T) {
	var handled atomic.Int32
	server := startServer(t, DefaultTimeout, func(ctx context.Context, containerID string) error {
		handled.Add(1)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > ProviderTimeout(DefaultTimeout) {
			t.Errorf("the provider deadline %v is not bounded by the hook timeout", deadline)
		}
		switch containerID {
		case "ok":
			return nil
		case "slow":
			time.Sleep(100 * time.Millisecond)
			return nil
		default:
			return fmt.Errorf("provider rejected %s", containerID)
		}
	})

	if err := Signal(server.Addr(), "ok", time.Second); err != nil {
		t.Fatalf("Signal(ok) = %v", err)
	}
	err := Signal(server.Addr(), "bad", time.Second)
	if err == nil || !strings.Contains(err.Error(), "provider rejected bad") {
		t.Fatalf("Signal(bad) = %v, want the provider's reason", err)
	}
	if got := handled.Load(); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}

	// Concurrent hooks are served concurrently: a slow one must not delay
	// the others by its whole duration.
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Signal(server.Addr(), "slow", 5*time.Second); err != nil {
				t.Errorf("concurrent Signal = %v", err)
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("10 concurrent 100ms hooks took %v: they ran sequentially", elapsed)
	}
}

func TestHookFailsWhenTheDaemonIsDown(t *testing.T) {
	err := Signal(filepath.Join(t.TempDir(), "missing.sock"), "abc", time.Second)
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("Signal without a daemon = %v", err)
	}
}

func TestHookFailsWhenTheDaemonDoesNotAnswerInTime(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	server := startServer(t, DefaultTimeout, func(ctx context.Context, _ string) error {
		<-release
		return nil
	})
	start := time.Now()
	err := Signal(server.Addr(), "abc", 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("Signal with a stuck daemon = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the hook waited %v past its deadline", elapsed)
	}
}

func TestServerBoundsTheProviderBeforeTheRuntimeKillsTheHook(t *testing.T) {
	// The smallest valid timeout keeps the test fast.
	timeout := ProviderTimeoutMargin + ClientTimeoutMargin + 100*time.Millisecond
	server := startServer(t, timeout, func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return fmt.Errorf("provider gave up: %w", ctx.Err())
	})
	start := time.Now()
	err := Signal(server.Addr(), "abc", ClientTimeout(timeout))
	if err == nil || !strings.Contains(err.Error(), "provider gave up") {
		t.Fatalf("Signal = %v, want the provider's timeout reason delivered before the hook's own deadline", err)
	}
	if elapsed := time.Since(start); elapsed >= ClientTimeout(timeout) {
		t.Fatalf("the answer took %v, not before the hook deadline %v", elapsed, ClientTimeout(timeout))
	}
}

func TestServerRejectsMalformedRequests(t *testing.T) {
	server := startServer(t, DefaultTimeout, func(context.Context, string) error {
		t.Error("the handler ran for a malformed request")
		return nil
	})
	for _, request := range []string{"HELLO abc\n", "READY\n", "READY two words\n"} {
		conn, err := net.Dial("unix", server.Addr())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		line, err := readLine(bufio.NewReader(conn))
		conn.Close()
		if err != nil || !strings.HasPrefix(line, "FAIL ") {
			t.Fatalf("request %q got %q, %v", request, line, err)
		}
	}
}

func TestListenReplacesStaleSocketAndRestrictsIt(t *testing.T) {
	path := socketPath(t)
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.Close()
	if err := os.WriteFile(path, nil, 0o644); err != nil { // a leftover file, not even a socket
		t.Fatal(err)
	}
	server, err := Listen(path, DefaultTimeout, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Type() != os.ModeSocket {
		t.Fatalf("%s is not a socket: %v", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket permissions = %o, want 600", perm)
	}
	if _, err := Listen(path, time.Second, nil); err == nil {
		t.Fatal("Listen accepted a nil handler")
	}
	if _, err := Listen(path, time.Second, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("Listen accepted a timeout with no room for the hook")
	}
}

func TestServeStopsWithTheContextAndWaitsForRequestsInFlight(t *testing.T) {
	release := make(chan struct{})
	server, err := Listen(socketPath(t), DefaultTimeout, func(context.Context, string) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	hook := make(chan error, 1)
	go func() { hook <- Signal(server.Addr(), "abc", 5*time.Second) }()
	time.Sleep(50 * time.Millisecond) // let the hook reach the handler
	cancel()
	select {
	case <-served:
		t.Fatal("Serve returned with a request in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if err := <-hook; err != nil {
		t.Fatalf("the hook in flight failed: %v", err)
	}
}

func TestRun(t *testing.T) {
	server := startServer(t, DefaultTimeout, func(_ context.Context, containerID string) error {
		if containerID == "bad" {
			return errors.New("no device for you")
		}
		return nil
	})
	stdin := strings.NewReader(`{"ociVersion":"1.2.0","id":"abc","status":"creating","pid":42,"bundle":"/run/containerd/bundle"}`)
	var stderr bytes.Buffer
	if code := Run(Args(server.Addr(), "abc", DefaultTimeout)[1:], stdin, &stderr); code != ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("Run = %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := Run(Args(server.Addr(), "bad", DefaultTimeout)[1:], strings.NewReader(""), &stderr); code != ExitFailure || !strings.Contains(stderr.String(), "no device for you") {
		t.Fatalf("Run(bad) = %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := Run([]string{"--container-id=abc", "extra"}, strings.NewReader(""), &stderr); code != ExitUsage {
		t.Fatalf("Run with extra arguments = %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := Run([]string{"--unknown"}, strings.NewReader(""), &stderr); code != ExitUsage {
		t.Fatalf("Run with an unknown flag = %d, stderr %q", code, stderr.String())
	}
	args := Args("/var/run/dranet/hook.sock", "abc", DefaultTimeout)
	want := []string{Subcommand, "--socket=/var/run/dranet/hook.sock", "--container-id=abc", "--timeout=28s"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("Args = %q, want %q", args, want)
	}
}

func TestPodFromMountInfo(t *testing.T) {
	const kubeletRootOnRootFS = `
1976 1955 0:81 / / rw,relatime master:683 - overlay overlay rw,lowerdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/35/fs,upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/36/fs,workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/36/work
1977 1976 0:84 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1981 1976 0:68 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
2040 1976 254:1 /var/lib/kubelet/pods/0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69/containers/dranet/8f9e0d1c /dev/termination-log rw,relatime - ext4 /dev/vda1 rw,discard,errors=remount-ro
2041 1976 0:93 / /var/run/secrets/kubernetes.io/serviceaccount ro,relatime - tmpfs tmpfs rw,size=1048576k
2042 1976 254:1 /var/lib/kubelet/plugins /var/lib/kubelet/plugins rw,relatime - ext4 /dev/vda1 rw,discard,errors=remount-ro
2043 1976 0:24 /dranet /var/run/dranet rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,size=1638400k,mode=755
`
	const kubeletRootOnItsOwnDisk = `
2040 1976 259:2 /pods/0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69/volumes/kubernetes.io~projected/kube-api-access-x7k2p /var/run/secrets/kubernetes.io/serviceaccount ro,relatime - ext4 /dev/nvme0n1p2 rw
2041 1976 259:2 /pods/0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69/containers/dranet/8f9e0d1c /dev/termination-log rw,relatime - ext4 /dev/nvme0n1p2 rw
`
	testCases := []struct {
		name          string
		mountInfo     string
		podUID        string
		containerName string
		wantErr       bool
	}{
		{name: "kubelet root dir on the root filesystem", mountInfo: kubeletRootOnRootFS, podUID: "0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69", containerName: "dranet"},
		{name: "kubelet root dir on its own disk", mountInfo: kubeletRootOnItsOwnDisk, podUID: "0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69", containerName: "dranet"},
		{name: "no termination log", mountInfo: strings.Split(kubeletRootOnItsOwnDisk, "\n")[1], podUID: "0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69"},
		{name: "escaped path", mountInfo: `2040 1976 259:2 /pods/0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69/containers/my\040app/8f9e0d1c /dev/termination-log rw - ext4 /dev/nvme0n1p2 rw`, podUID: "0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69", containerName: "my app"},
		{name: "not in a pod", mountInfo: strings.Join(strings.Split(kubeletRootOnRootFS, "\n")[:4], "\n")},
		{name: "empty", mountInfo: ""},
		{name: "mounts from two pods", mountInfo: kubeletRootOnItsOwnDisk + `2050 1976 259:2 /pods/ffffffff-9a2e-4f3c-8b8e-1f2d3c4b5a69/volumes/kubernetes.io~empty-dir/x /x rw - ext4 /dev/nvme0n1p2 rw`, wantErr: true},
		{name: "mounts from two containers", mountInfo: kubeletRootOnItsOwnDisk + `2050 1976 259:2 /pods/0d7a3c6e-9a2e-4f3c-8b8e-1f2d3c4b5a69/containers/other/aa /x rw - ext4 /dev/nvme0n1p2 rw`, wantErr: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podUID, containerName, err := podFromMountInfo(strings.NewReader(tc.mountInfo))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if podUID != tc.podUID || containerName != tc.containerName {
				t.Fatalf("got pod %q container %q, want pod %q container %q", podUID, containerName, tc.podUID, tc.containerName)
			}
		})
	}
}

func TestSelfIdentity(t *testing.T) {
	id, err := SelfIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if id.PID != os.Getpid() || len(id.Args) == 0 {
		t.Fatalf("identity %+v does not describe this process", id)
	}
	if HostExecutable(id.PID) != fmt.Sprintf("/proc/%d/exe", os.Getpid()) {
		t.Fatalf("HostExecutable = %s", HostExecutable(id.PID))
	}
	// The test binary runs wherever go test runs it: in the initial PID
	// namespace its PID is a host PID and /proc/<pid>/exe is this binary.
	if id.InitPIDNamespace {
		exe, err := os.Readlink(HostExecutable(id.PID))
		if err != nil {
			t.Fatal(err)
		}
		if self, _ := os.Executable(); exe != self {
			t.Fatalf("/proc/<pid>/exe = %s, want %s", exe, self)
		}
	}
	if _, err := inInitPIDNamespace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("inInitPIDNamespace accepted a missing namespace file")
	}
}
