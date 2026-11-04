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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	pollInterval = 200 * time.Millisecond
	podTimeout   = 90 * time.Second
	// sliceTimeout covers the inventory max poll interval, which is the
	// slowest path for a change to reach the ResourceSlice.
	sliceTimeout = 90 * time.Second
)

// env is the per-test state: a namespace for the Kubernetes objects and the
// links the test created on the node. Its cleanup removes the pods first, so
// the links are back on the node when they are deleted.
type env struct {
	t     *testing.T
	ctx   context.Context
	ns    string
	start time.Time
	links []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	e := &env{t: t, ctx: ctx, start: time.Now()}

	name := strings.ToLower(strings.NewReplacer("/", "-", "_", "-", "=", "-").Replace(t.Name()))
	if len(name) > 40 {
		name = name[:40]
	}
	ns, err := tc.client.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "it-" + strings.Trim(name, "-") + "-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	e.ns = ns.Name

	t.Cleanup(func() {
		defer cancel()
		if t.Failed() {
			e.dump()
		}
		e.cleanup()
	})
	return e
}

func (e *env) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pods := tc.client.CoreV1().Pods(e.ns)
	_ = pods.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	err := wait.PollUntilContextTimeout(ctx, pollInterval, podTimeout, true, func(ctx context.Context) (bool, error) {
		list, err := pods.List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		return len(list.Items) == 0, nil
	})
	if err != nil {
		e.t.Logf("pods in %s did not go away: %v", e.ns, err)
	}
	// Links that were inside a destroyed pod namespace are already gone, so
	// a missing link is not an error here.
	if len(e.links) > 0 {
		script := "for l in " + strings.Join(e.links, " ") + "; do ip link del $l 2>&1 | grep -v 'Cannot find device'; done; true"
		if out, err := tc.nodeExec(ctx, "sh", "-c", script); err != nil || strings.TrimSpace(out) != "" {
			e.t.Logf("delete links: %v %s", err, out)
		}
	}
	if err := tc.client.CoreV1().Namespaces().Delete(ctx, e.ns, metav1.DeleteOptions{}); err != nil {
		e.t.Logf("delete namespace %s: %v", e.ns, err)
	}
}

// dump writes the dranet journal since the test started and the objects of
// the test to the artifacts directory.
func (e *env) dump() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base := filepath.Join(artifactsDir, strings.ReplaceAll(e.t.Name(), "/", "_"))

	logs, _ := tc.dranetLogs(ctx, fmt.Sprintf("@%d", e.start.Unix()))
	_ = os.WriteFile(base+"-dranet.log", []byte(logs), 0o644)

	state := map[string]any{}
	if pods, err := tc.client.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{}); err == nil {
		state["pods"] = pods.Items
	}
	if events, err := tc.client.CoreV1().Events(e.ns).List(ctx, metav1.ListOptions{}); err == nil {
		state["events"] = events.Items
	}
	if claims, err := tc.client.ResourceV1().ResourceClaims(e.ns).List(ctx, metav1.ListOptions{}); err == nil {
		state["resourceclaims"] = claims.Items
	}
	if slices, err := tc.nodeSlices(ctx); err == nil {
		state["resourceslices"] = slices
	}
	if links, err := tc.nodeExec(ctx, "ip", "-j", "addr"); err == nil {
		state["nodeLinks"] = json.RawMessage(links)
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(base+"-state.json", data, 0o644)
	e.t.Logf("debug output in %s-{dranet.log,state.json}", base)
}

// addDummyLink creates a dummy link with an address on the node and returns
// its name. The name is unique per call, so state left behind by one test
// cannot leak into another.
func (e *env) addDummyLink(cidr string) string {
	e.t.Helper()
	name := fmt.Sprintf("it%x", rand.Uint32())
	for _, cmd := range [][]string{
		{"ip", "link", "add", name, "type", "dummy"},
		{"ip", "link", "set", "up", "dev", name},
		{"ip", "addr", "add", cidr, "dev", name},
	} {
		if out, err := tc.nodeExec(e.ctx, cmd...); err != nil {
			e.t.Fatalf("%s: %s", strings.Join(cmd, " "), out)
		}
	}
	e.links = append(e.links, name)
	return name
}

// addDummyLinks creates n dummy links without addresses in one node exec and
// returns their common name prefix.
func (e *env) addDummyLinks(n int) string {
	e.t.Helper()
	prefix := fmt.Sprintf("it%x", rand.Uint32())
	script := fmt.Sprintf("set -e; for i in $(seq 0 %d); do ip link add %s$i type dummy; ip link set up dev %s$i; done", n-1, prefix, prefix)
	if out, err := tc.nodeExec(e.ctx, "sh", "-c", script); err != nil {
		e.t.Fatalf("create %d dummy links: %s", n, out)
	}
	for i := 0; i < n; i++ {
		e.links = append(e.links, fmt.Sprintf("%s%d", prefix, i))
	}
	return prefix
}

// hostLink reports whether the link is in the node's network namespace and
// the IPv4 addresses it has there.
func hostLink(ctx context.Context, name string) (bool, []string, error) {
	out, err := tc.nodeExec(ctx, "ip", "-j", "-4", "addr", "show", "dev", name)
	if err != nil {
		if strings.Contains(out, "does not exist") {
			return false, nil, nil
		}
		return false, nil, err
	}
	var links []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return false, nil, fmt.Errorf("parse ip -j output: %w\n%s", err, out)
	}
	var addrs []string
	for _, l := range links {
		for _, a := range l.AddrInfo {
			addrs = append(addrs, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
		}
	}
	return true, addrs, nil
}

// deviceByIfName returns the published device whose dra.net/ifName is name.
func deviceByIfName(ctx context.Context, name string) (resourceapi.Device, bool, error) {
	slices, err := tc.nodeSlices(ctx)
	if err != nil {
		return resourceapi.Device{}, false, err
	}
	for _, s := range slices {
		for _, d := range s.Spec.Devices {
			if attr, ok := d.Attributes["dra.net/ifName"]; ok && attr.StringValue != nil && *attr.StringValue == name {
				return d, true, nil
			}
		}
	}
	return resourceapi.Device{}, false, nil
}

// withDranetArgs restarts dranet with extra flags for the rest of the test
// and restores the previous flags afterwards.
func withDranetArgs(t *testing.T, args ...string) {
	t.Helper()
	ctx := context.Background()
	prev := tc.dranetArgs
	tc.dranetArgs = append(append([]string{}, prev...), args...)
	if err := tc.restartDranet(ctx); err != nil {
		tc.dranetArgs = prev
		t.Fatalf("restart dranet with %v: %v", args, err)
	}
	t.Cleanup(func() {
		tc.dranetArgs = prev
		if err := tc.restartDranet(ctx); err != nil {
			t.Errorf("restore dranet flags: %v", err)
		}
	})
}
