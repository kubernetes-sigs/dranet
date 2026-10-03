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
	"net"
	"net/http"
	"testing"
	"time"
)

func healthzMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// TestStartMetricsServer_HeldPortFailsThenReleasedPortServes: while another
// listener holds the address, startup returns an error at once; after the
// port is released, startup succeeds and /healthz answers.
func TestStartMetricsServer_HeldPortFailsThenReleasedPortServes(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := held.Addr().String()
	start := time.Now()
	if ln, err := startMetricsServer(addr, healthzMux(), func(error) {}); err == nil {
		ln.Close()
		t.Fatalf("startup succeeded on %s while another listener holds it", addr)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("startup took %s to fail on a held port; it must fail at once", elapsed)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := startMetricsServer(addr, healthzMux(), func(error) {})
	if err != nil {
		t.Fatalf("startup failed after the port was released: %v", err)
	}
	defer ln.Close()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestStartMetricsServer_ServeErrorReachesTheCallback: when http.Serve
// returns, its error is handed to onServeError rather than dropped.
func TestStartMetricsServer_ServeErrorReachesTheCallback(t *testing.T) {
	got := make(chan error, 1)
	ln, err := startMetricsServer("127.0.0.1:0", healthzMux(), func(err error) { got <- err })
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	select {
	case err := <-got:
		if err == nil {
			t.Error("onServeError received a nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onServeError was not called after the listener closed")
	}
}
