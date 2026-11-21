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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/dranet/pkg/driver"
)

func TestHealthzAggregateAndIndividualChecks(t *testing.T) {
	var draErr, nriErr error

	mux := http.NewServeMux()
	registerHealthz(mux,
		healthChecker{name: "dra", check: func() error { return draErr }},
		healthChecker{name: "nri", check: func() error { return nriErr }},
	)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// Both healthy -> 200 ok, verbose includes per-check lines.
	if rec := get("/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz = (%d, %q), want (200, \"ok\")", rec.Code, rec.Body.String())
	}
	rec := get("/healthz?verbose")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz?verbose status = %d, want 200", rec.Code)
	}
	for _, want := range []string{"[+]dra ok", "[+]nri ok", "healthz check passed"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /healthz?verbose body = %q, missing %q", rec.Body.String(), want)
		}
	}
	if rec := get("/healthz/dra"); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz/dra = (%d, %q), want (200, \"ok\")", rec.Code, rec.Body.String())
	}
	if rec := get("/healthz/nri?verbose"); rec.Code != http.StatusOK || rec.Body.String() != "[+]nri ok\n" {
		t.Fatalf("GET /healthz/nri?verbose = (%d, %q), want (200, \"[+]nri ok\\n\")", rec.Code, rec.Body.String())
	}

	// Only NRI unhealthy -> /healthz fails (logical AND), /healthz/dra passes, /healthz/nri fails.
	nriErr = errors.New("NRI plugin is not connected: connection closed")
	rec = get("/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz while NRI down = %d, want 503", rec.Code)
	}
	for _, want := range []string{"[+]dra ok", "[-]nri failed: NRI plugin is not connected", "healthz check failed"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /healthz body = %q, missing %q", rec.Body.String(), want)
		}
	}
	if rec := get("/healthz/dra"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz/dra while NRI down = %d, want 200", rec.Code)
	}
	if rec := get("/healthz/nri"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "[-]nri failed:") {
		t.Fatalf("GET /healthz/nri while NRI down = (%d, %q), want 503 with failure", rec.Code, rec.Body.String())
	}

	// Excluding the failing check allows /healthz to pass.
	rec = get("/healthz?exclude=nri&verbose")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz?exclude=nri&verbose = %d, want 200", rec.Code)
	}
	for _, want := range []string{"[+]dra ok", "[+]nri excluded: ok", "healthz check passed"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /healthz?exclude=nri&verbose body = %q, missing %q", rec.Body.String(), want)
		}
	}

	// Only DRA unhealthy -> /healthz fails (logical AND).
	nriErr = nil
	draErr = errors.New("DRA plugin is not registered with kubelet")
	rec = get("/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz while DRA down = %d, want 503", rec.Code)
	}
	for _, want := range []string{"[-]dra failed:", "[+]nri ok", "healthz check failed"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /healthz body = %q, missing %q", rec.Body.String(), want)
		}
	}
	if rec := get("/healthz/nri"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz/nri while DRA down = %d, want 200", rec.Code)
	}
}

func TestHealthzBeforeDriverStarts(t *testing.T) {
	store, err := driver.NewPodConfigStore("")
	if err != nil {
		t.Fatalf("NewPodConfigStore: %v", err)
	}
	defer store.Close()

	dranet := driver.New(driverName, nil, "test-node", store)
	mux := http.NewServeMux()
	registerHealthz(mux,
		healthChecker{name: "dra", check: dranet.CheckDRA},
		healthChecker{name: "nri", check: dranet.CheckNRI},
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz before driver start = %d, want 503", rec.Code)
	}
	for _, want := range []string{"[-]dra failed: DRA plugin is not started", "[-]nri failed: NRI plugin is not connected"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET /healthz body = %q, missing %q", rec.Body.String(), want)
		}
	}
}
