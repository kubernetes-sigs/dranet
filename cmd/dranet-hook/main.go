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

// dranet-hook is the OCI createRuntime hook that dranet adds to the
// containers of a Pod whose network devices are still being attached. The
// container runtime runs it on the host when it starts the container, after
// the container's namespaces exist and before its process runs; it blocks
// until the dranet daemon reports the Pod's devices attached, and exits
// non-zero with the reason when the attach failed, so the container does not
// start without its devices.
//
// Usage:
//
//	dranet-hook wait [--pod-uid UID] [--socket PATH]
//	dranet-hook install DIR
//
// wait reads the Pod UID and the socket from the environment the daemon sets
// on the hook (DRANET_POD_UID, DRANET_SOCKET; see package apis/hook); the
// flags override it for manual use. install copies this binary to DIR; the
// DaemonSet runs it in an init container because the image has no shell.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"sigs.k8s.io/dranet/pkg/apis/hook"
)

const binaryName = "dranet-hook"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "wait":
		err = runWait(os.Args[2:], os.Stdin)
	case "install":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		err = install(os.Args[2])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binaryName, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s wait [--pod-uid UID] [--socket PATH]\n       %s install DIR\n", binaryName, binaryName)
}

// runWait blocks until the daemon reports the Pod's devices attached. The
// runtime enforces the hook timeout by killing the process, so there is no
// client-side timeout. SIGINT and SIGTERM cancel the request, so the daemon
// sees the hook leave and the hook exits non-zero with the reason.
func runWait(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	podUID := fs.String("pod-uid", os.Getenv(hook.EnvPodUID), "UID of the Pod whose network devices to wait for (default $"+hook.EnvPodUID+")")
	socket := fs.String("socket", os.Getenv(hook.EnvSocket), "unix socket of the dranet daemon (default $"+hook.EnvSocket+", then "+hook.SocketPath+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *podUID == "" {
		return fmt.Errorf("the Pod UID is required: set --pod-uid or %s", hook.EnvPodUID)
	}
	if *socket == "" {
		*socket = hook.SocketPath
	}
	// The runtime writes the container state to stdin; drain it so it never blocks.
	go io.Copy(io.Discard, stdin) //nolint:errcheck
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return wait(ctx, *socket, *podUID)
}

// wait returns nil when the daemon reports all devices attached, or an error
// carrying the daemon's reason, or the interruption when ctx ends first.
func wait(ctx context.Context, socket, podUID string) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dranet"+hook.WaitPath+"?pod="+podUID, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted while waiting for the network devices of pod %s, refusing to start the container without them", podUID)
		}
		return fmt.Errorf("the dranet daemon is not reachable at %s, refusing to start the container without its network devices: %w", socket, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var status hook.WaitResponse
	if json.Unmarshal(body, &status) == nil && status.Error != "" {
		return fmt.Errorf("network devices not attached (%d/%d): %s", status.Attached, status.Total, status.Error)
	}
	return fmt.Errorf("network devices not attached: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// install copies the running binary to dir/dranet-hook atomically.
func install(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+binaryName+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dst := filepath.Join(dir, binaryName)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	fmt.Printf("installed %s\n", dst)
	return nil
}
