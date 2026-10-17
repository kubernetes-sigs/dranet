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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
	"sigs.k8s.io/dranet/pkg/features"
	"sigs.k8s.io/dranet/pkg/ocihook"
)

// nativeRuntimeProvider is a cloud provider that is also a runtime provider.
type nativeRuntimeProvider struct{}

func (nativeRuntimeProvider) GetDeviceAttributes(cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	return nil
}
func (nativeRuntimeProvider) GetDeviceConfig(cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	return nil
}
func (nativeRuntimeProvider) ConfigurePod(context.Context, *cloudprovider.ConfigurePodRequest) error {
	return nil
}

// profileOnlyProvider is not a runtime provider.
type profileOnlyProvider struct{}

func (profileOnlyProvider) GetProfileConfig(cloudprovider.DeviceIdentifiers, *resourceapi.ResourceClaim, *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	return nil, nil
}
func (profileOnlyProvider) ReleaseProfileConfig(cloudprovider.DeviceIdentifiers, types.UID, *apis.NetworkConfig) error {
	return nil
}

func newWebhookWithCapabilities(t *testing.T, caps webhook.Capabilities) *webhook.WebhookProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == webhook.PathHealth {
			json.NewEncoder(w).Encode(caps)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	provider, err := webhook.NewWebhookProvider(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestRuntimeProviders(t *testing.T) {
	declaring := newWebhookWithCapabilities(t, webhook.Capabilities{CloudProvider: true, ProfileProvider: true, RuntimeProvider: true})
	silent := newWebhookWithCapabilities(t, webhook.Capabilities{ProfileProvider: true})
	native := nativeRuntimeProvider{}

	// Disabled by default behind the RuntimeProvider alpha feature gate.
	if got := runtimeProviders(native, declaring); len(got) != 0 {
		t.Fatalf("runtimeProviders with feature gate disabled = %v, want nil", got)
	}

	featuregatetesting.SetFeatureGateDuringTest(t, features.DefaultFeatureGate, features.RuntimeProvider, true)

	testCases := []struct {
		name      string
		providers []any
		want      []cloudprovider.RuntimeProvider
	}{
		{name: "no providers", providers: []any{nil, nil}},
		{name: "providers without a hook", providers: []any{profileOnlyProvider{}, nil}},
		{name: "webhook that declares the hook", providers: []any{nil, declaring}, want: []cloudprovider.RuntimeProvider{declaring}},
		{name: "webhook that does not declare the hook", providers: []any{nil, silent}},
		{name: "native provider with a hook", providers: []any{native, profileOnlyProvider{}}, want: []cloudprovider.RuntimeProvider{native}},
		{name: "both cloud and profile providers are called in order", providers: []any{native, declaring}, want: []cloudprovider.RuntimeProvider{native, declaring}},
		{name: "same provider instance for cloud and profile is deduplicated", providers: []any{declaring, declaring}, want: []cloudprovider.RuntimeProvider{declaring}},
		{name: "a silent webhook does not hide the cloud provider's hook", providers: []any{native, silent}, want: []cloudprovider.RuntimeProvider{native}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := runtimeProviders(tc.providers...)
			if len(got) != len(tc.want) {
				t.Fatalf("runtimeProviders = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("runtimeProviders[%d] = %T(%v), want %T(%v)", i, got[i], got[i], tc.want[i], tc.want[i])
				}
			}
		})
	}
}

// TestHookSubcommand runs the dranet binary the way the container runtime
// does, as the OCI hook of a container, against a daemon-side socket: the
// subcommand must be dispatched before any daemon flag or component, and its
// exit code and stderr are what the runtime reports.
func TestHookSubcommand(t *testing.T) {
	dir, err := os.MkdirTemp("", "dranet-hook")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	binary := filepath.Join(dir, "dranet")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dranet: %v\n%s", err, out)
	}

	socket := filepath.Join(dir, "hook.sock")
	server, err := ocihook.Listen(socket, ocihook.DefaultTimeout, func(_ context.Context, containerID string) error {
		if containerID == "rejected" {
			return errors.New("the provider said no")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()

	run := func(args ...string) (int, string, time.Duration) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Stdin = strings.NewReader(`{"ociVersion":"1.2.0","id":"ctr","status":"creating","pid":1,"bundle":"/b"}`)
		cmd.Env = []string{} // the runtime passes no environment to hooks
		var stderr strings.Builder
		cmd.Stderr = &stderr
		start := time.Now()
		err := cmd.Run()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return 0, stderr.String(), time.Since(start)
		case errors.As(err, &exitErr):
			return exitErr.ExitCode(), stderr.String(), time.Since(start)
		default:
			t.Fatalf("run %v: %v", args, err)
			return -1, "", 0
		}
	}

	code, stderr, elapsed := run(ocihook.Args(socket, "accepted", ocihook.DefaultTimeout)...)
	// Package initializers of the daemon's dependencies run before main and
	// may log to stderr (the AWS Neuron lookup warns on non-EFA instances);
	// the runtime ignores the stderr of a successful hook. The hook itself
	// must report nothing.
	if code != ocihook.ExitSuccess || strings.Contains(stderr, "dranet "+ocihook.Subcommand) {
		t.Fatalf("accepted hook: exit %d, stderr %q", code, stderr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the hook took %v: the daemon's initialization must not run on the hook path", elapsed)
	}
	if code, stderr, _ := run(ocihook.Args(socket, "rejected", ocihook.DefaultTimeout)...); code != ocihook.ExitFailure || !strings.Contains(stderr, "the provider said no") {
		t.Fatalf("rejected hook: exit %d, stderr %q", code, stderr)
	}
	if code, stderr, _ := run(ocihook.Args(filepath.Join(dir, "no-daemon.sock"), "orphan", ocihook.DefaultTimeout)...); code != ocihook.ExitFailure || !strings.Contains(stderr, "not reachable") {
		t.Fatalf("hook without a daemon: exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := run(ocihook.Subcommand, "--bogus"); code != ocihook.ExitUsage {
		t.Fatalf("hook with a bad flag: exit %d", code)
	}
}
