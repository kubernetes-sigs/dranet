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
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
	"sigs.k8s.io/dranet/pkg/features"
)

// featureEnabled reports whether feature gate f is enabled when dranet is
// started with args, matching flag.StringVar("--feature-gates") in cmd/dranet.
func featureEnabled(args []string, f featuregate.Feature) (bool, error) {
	fg := features.DefaultFeatureGate.DeepCopy()
	var raw string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--feature-gates" || arg == "-feature-gates":
			if i+1 >= len(args) {
				return false, fmt.Errorf("flag %s needs an argument", arg)
			}
			raw = args[i+1]
			i++
		case strings.HasPrefix(arg, "--feature-gates="):
			raw = strings.TrimPrefix(arg, "--feature-gates=")
		case strings.HasPrefix(arg, "-feature-gates="):
			raw = strings.TrimPrefix(arg, "-feature-gates=")
		}
	}
	if raw != "" {
		if err := fg.Set(raw); err != nil {
			return false, fmt.Errorf("parse --feature-gates=%q: %w", raw, err)
		}
	}
	return fg.Enabled(f), nil
}

// checkNRIVersion inspects Go module buildinfo from a containerd binary and
// reports its github.com/containerd/nri version and whether it is >= minVersion.
func checkNRIVersion(info *buildinfo.BuildInfo, minVersion string) (string, bool, error) {
	want, err := version.ParseSemantic(minVersion)
	if err != nil {
		return "", false, fmt.Errorf("invalid minimum version %q: %w", minVersion, err)
	}
	if info == nil {
		return "", false, fmt.Errorf("missing Go buildinfo")
	}
	const nriModule = "github.com/containerd/nri"
	for _, dep := range info.Deps {
		if dep.Path != nriModule {
			continue
		}
		ver := dep.Version
		if dep.Replace != nil {
			if dep.Replace.Version == "" || dep.Replace.Version == "(devel)" {
				// Local directory replace in a custom build; assume it has the fix.
				return "(devel)", true, nil
			}
			ver = dep.Replace.Version
		}
		if ver == "(devel)" {
			return ver, true, nil
		}
		got, err := version.ParseSemantic(ver)
		if err != nil {
			return ver, false, fmt.Errorf("parse %s version %q: %w", nriModule, ver, err)
		}
		return ver, !got.LessThan(want), nil
	}
	return "", false, fmt.Errorf("module %s not found in binary dependencies", nriModule)
}

// parseMetricInt parses the Prometheus text exposition body and returns the
// integer value of a single unlabelled metric.
func parseMetricInt(body, metric string) (int, error) {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, ok := strings.Cut(line, " "); ok && name == metric {
			n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return 0, fmt.Errorf("parse metric %s=%q: %w", metric, value, err)
			}
			return int(n), nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("scan metrics: %w", err)
	}
	return 0, fmt.Errorf("metric %s not found", metric)
}

func TestFeatureEnabled(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    bool
		wantErr bool
	}{
		{
			name: "default off",
			args: nil,
			want: false,
		},
		{
			name: "enabled with equals",
			args: []string{"--feature-gates=PersistentResourceSliceAttributes=true"},
			want: true,
		},
		{
			name: "enabled with separate arg",
			args: []string{"--feature-gates", "PersistentResourceSliceAttributes=true"},
			want: true,
		},
		{
			name: "single dash flag",
			args: []string{"-feature-gates=PersistentResourceSliceAttributes=true"},
			want: true,
		},
		{
			name: "last flag wins like flag.StringVar",
			args: []string{
				"--feature-gates=PersistentResourceSliceAttributes=true",
				"--feature-gates=PersistentResourceSliceAttributes=false",
			},
			want: false,
		},
		{
			name: "stops at double dash",
			args: []string{"--", "--feature-gates=PersistentResourceSliceAttributes=true"},
			want: false,
		},
		{
			name:    "missing value",
			args:    []string{"--feature-gates"},
			wantErr: true,
		},
		{
			name:    "unknown feature gate",
			args:    []string{"--feature-gates=NoSuchGate=true"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := featureEnabled(tc.args, features.PersistentResourceSliceAttributes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("featureEnabled(%v) succeeded with %v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("featureEnabled(%v) unexpected error: %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("featureEnabled(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestCheckNRIVersion(t *testing.T) {
	const minVer = "v0.12.4"
	tests := []struct {
		name    string
		info    *buildinfo.BuildInfo
		wantVer string
		wantOK  bool
		wantErr bool
	}{
		{
			name: "older release v0.12.0",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{Path: "github.com/containerd/nri", Version: "v0.12.0"}},
			},
			wantVer: "v0.12.0",
			wantOK:  false,
		},
		{
			name: "pre-0.12.4 pseudo-version",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{Path: "github.com/containerd/nri", Version: "v0.12.4-0.20261020000000-abcdef123456"}},
			},
			wantVer: "v0.12.4-0.20261020000000-abcdef123456",
			wantOK:  false,
		},
		{
			name: "exact minimum v0.12.4",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{Path: "github.com/containerd/nri", Version: "v0.12.4"}},
			},
			wantVer: "v0.12.4",
			wantOK:  true,
		},
		{
			name: "newer release v0.12.5",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{Path: "github.com/containerd/nri", Version: "v0.12.5"}},
			},
			wantVer: "v0.12.5",
			wantOK:  true,
		},
		{
			name: "replaced with newer version",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{
					Path:    "github.com/containerd/nri",
					Version: "v0.12.0",
					Replace: &debug.Module{Path: "github.com/containerd/nri", Version: "v0.12.4"},
				}},
			},
			wantVer: "v0.12.4",
			wantOK:  true,
		},
		{
			name: "local directory replace",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{
					Path:    "github.com/containerd/nri",
					Version: "v0.12.0",
					Replace: &debug.Module{Path: "../nri", Version: ""},
				}},
			},
			wantVer: "(devel)",
			wantOK:  true,
		},
		{
			name: "missing dependency",
			info: &buildinfo.BuildInfo{
				Deps: []*debug.Module{{Path: "example.com/other", Version: "v1.0.0"}},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotVer, gotOK, err := checkNRIVersion(tc.info, minVer)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkNRIVersion() succeeded (%q, %v), want error", gotVer, gotOK)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkNRIVersion() unexpected error: %v", err)
			}
			if gotVer != tc.wantVer || gotOK != tc.wantOK {
				t.Errorf("checkNRIVersion() = (%q, %v), want (%q, %v)", gotVer, gotOK, tc.wantVer, tc.wantOK)
			}
		})
	}
}

func TestParseMetricInt(t *testing.T) {
	const metric = "dranet_driver_nri_plugin_disconnects_total"
	tests := []struct {
		name    string
		body    string
		want    int
		wantErr bool
	}{
		{
			name: "zero counter with comments",
			body: "# HELP dranet_driver_nri_plugin_disconnects_total Total disconnects.\n# TYPE dranet_driver_nri_plugin_disconnects_total counter\ndranet_driver_nri_plugin_disconnects_total 0\n",
			want: 0,
		},
		{
			name: "scientific notation value",
			body: "other_metric 1\ndranet_driver_nri_plugin_disconnects_total 3e+00\n",
			want: 3,
		},
		{
			name:    "metric absent",
			body:    "other_metric 1\n",
			wantErr: true,
		},
		{
			name:    "malformed float",
			body:    "dranet_driver_nri_plugin_disconnects_total nope\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMetricInt(tc.body, metric)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMetricInt() = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMetricInt() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseMetricInt() = %d, want %d", got, tc.want)
			}
		})
	}
}
