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

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/dranet/pkg/apis/hook"
)

// serveWait serves hook.WaitPath on a unix socket with handler for the test.
func serveWait(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "hook.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(hook.WaitPath, handler)
	server := &http.Server{Handler: mux}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestWait(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		socket := serveWait(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("pod"); got != "pod-1" {
				t.Errorf("pod query = %q, want pod-1", got)
			}
			json.NewEncoder(w).Encode(hook.WaitResponse{PodUID: "pod-1", Attached: 2, Total: 2}) //nolint:errcheck
		})
		if err := wait(context.Background(), socket, "pod-1"); err != nil {
			t.Fatalf("wait() error = %v", err)
		}
	})

	t.Run("failed with the daemon's reason", func(t *testing.T) {
		socket := serveWait(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(hook.WaitResponse{PodUID: "pod-1", Attached: 1, Total: 2, Error: "link not found"}) //nolint:errcheck
		})
		err := wait(context.Background(), socket, "pod-1")
		if err == nil || !strings.Contains(err.Error(), "(1/2): link not found") {
			t.Fatalf("wait() error = %v, want the daemon's reason", err)
		}
	})

	t.Run("interrupted while the daemon is still attaching", func(t *testing.T) {
		clientGone := make(chan struct{})
		socket := serveWait(t, func(w http.ResponseWriter, r *http.Request) {
			// The daemon waits until the attach ends or the hook leaves.
			<-r.Context().Done()
			close(clientGone)
		})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		err := wait(ctx, socket, "pod-1")
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("wait() error = %v, want the interruption", err)
		}
		select {
		case <-clientGone:
		case <-time.After(5 * time.Second):
			t.Fatal("the daemon did not see the hook leave")
		}
	})

	t.Run("daemon not reachable", func(t *testing.T) {
		err := wait(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "pod-1")
		if err == nil || !strings.Contains(err.Error(), "not reachable") {
			t.Fatalf("wait() error = %v, want not reachable", err)
		}
	})
}

func TestRunWaitRequiresPodUID(t *testing.T) {
	t.Setenv(hook.EnvPodUID, "")
	if err := runWait(nil, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), hook.EnvPodUID) {
		t.Fatalf("runWait() error = %v, want the missing Pod UID", err)
	}
}
