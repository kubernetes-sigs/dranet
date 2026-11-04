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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	driverName = "dra.net"

	// defaultNodeImage must match kind.yaml so both suites test the same
	// Kubernetes version.
	defaultNodeImage = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"

	// dranetUnit is the transient systemd unit that runs dranet inside the
	// node container. systemd gives us start/stop/restart and journal logs
	// without an image build, a kind load, or a DaemonSet rollout.
	dranetUnit = "dranet"
	dranetPath = "/usr/local/bin/dranet"
)

// kindConfig is a single node cluster. With no workers kind removes the
// control-plane taint, so pods schedule on it. The scheduler and kubelet run
// at -v=5 like kind.yaml, so the journal shows DRA allocation decisions.
const kindConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    scheduler:
        extraArgs:
          v: "5"
          vmodule: "allocator=6,dynamicresources=6"
    controllerManager:
        extraArgs:
          v: "5"
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        v: "5"
`

// cluster is a single node kind cluster with dranet running as a host
// process inside the node container.
type cluster struct {
	name       string
	node       string
	client     kubernetes.Interface
	kubeconfig string
	pauseImage string
	dranetArgs []string
	// keep is true when the cluster must survive the test run, either
	// because the user asked for it or because the cluster existed before.
	keep bool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// setupCluster creates or reuses the kind cluster and returns a client for it.
func setupCluster(ctx context.Context, workdir string) (*cluster, error) {
	c := &cluster{
		name: envOr("DRANET_KIND_CLUSTER", "dranet-integration"),
		keep: os.Getenv("DRANET_KIND_REUSE") != "",
	}
	c.node = c.name + "-control-plane"
	if extra := os.Getenv("DRANET_ARGS"); extra != "" {
		c.dranetArgs = strings.Fields(extra)
	}

	out, err := run(ctx, "kind", "get", "clusters")
	if err != nil {
		return nil, err
	}
	exists := false
	for _, line := range strings.Fields(out) {
		if line == c.name {
			exists = true
		}
	}
	c.kubeconfig = filepath.Join(workdir, "kubeconfig")
	if exists {
		fmt.Fprintf(os.Stderr, "reusing kind cluster %q; it is kept after the run\n", c.name)
		c.keep = true
	} else {
		cfg := filepath.Join(workdir, "kind.yaml")
		if err := os.WriteFile(cfg, []byte(kindConfig), 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "creating kind cluster %q\n", c.name)
		if _, err := run(ctx, "kind", "create", "cluster",
			"--name", c.name,
			"--image", envOr("DRANET_KIND_IMAGE", defaultNodeImage),
			"--config", cfg,
			"--wait", "90s",
			"--kubeconfig", c.kubeconfig,
		); err != nil {
			return nil, err
		}
	}

	kc, err := run(ctx, "kind", "get", "kubeconfig", "--name", c.name)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.kubeconfig, []byte(kc), 0o600); err != nil {
		return nil, err
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kc))
	if err != nil {
		return nil, err
	}
	c.client, err = kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}

	if c.pauseImage, err = c.findPauseImage(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cluster) teardown(ctx context.Context) {
	if c.keep {
		fmt.Fprintf(os.Stderr, "keeping kind cluster %q (KUBECONFIG=%s)\n", c.name, c.kubeconfig)
		return
	}
	if out, err := run(ctx, "kind", "delete", "cluster", "--name", c.name); err != nil {
		fmt.Fprintln(os.Stderr, out)
	}
}

// nodeExec runs a command inside the node container. The node is the
// "host" from dranet's point of view, so this is where links are created
// and inspected.
func (c *cluster) nodeExec(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "docker", append([]string{"exec", c.node}, args...)...)
}

// findPauseImage returns the pause image already present on the node, so
// test pods start without pulling anything.
func (c *cluster) findPauseImage(ctx context.Context) (string, error) {
	out, err := c.nodeExec(ctx, "crictl", "images", "-o", "json")
	if err != nil {
		return "", err
	}
	var images struct {
		Images []struct {
			RepoTags []string `json:"repoTags"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &images); err != nil {
		return "", fmt.Errorf("parse crictl images: %w", err)
	}
	for _, img := range images.Images {
		for _, tag := range img.RepoTags {
			if strings.Contains(tag, "/pause:") {
				return tag, nil
			}
		}
	}
	return "", fmt.Errorf("no pause image on node %s", c.node)
}

// buildDranet compiles a static dranet binary on the host. DRANET_BINARY
// skips the build and uses a prebuilt file.
func buildDranet(ctx context.Context, workdir string) (string, error) {
	if p := os.Getenv("DRANET_BINARY"); p != "" {
		return p, nil
	}
	bin := filepath.Join(workdir, "dranet")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "sigs.k8s.io/dranet/cmd/dranet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build dranet: %w", err)
	}
	return bin, nil
}

// installDranet copies the binary into the node and (re)starts the unit.
func (c *cluster) installDranet(ctx context.Context, bin string) error {
	c.stopDranet(ctx)
	if _, err := run(ctx, "docker", "cp", bin, c.node+":"+dranetPath); err != nil {
		return err
	}
	return c.startDranet(ctx)
}

// startDranet runs dranet as a transient systemd service inside the node,
// with the same flags as install.yaml plus the ones needed outside a pod.
// It returns once the driver has published a ResourceSlice for the node.
func (c *cluster) startDranet(ctx context.Context) error {
	args := []string{
		"systemd-run", "--unit", dranetUnit, "--property", "Restart=no",
		dranetPath,
		"--v=4",
		"--kubeconfig=/etc/kubernetes/admin.conf",
		"--hostname-override=" + c.node,
		"--cloud-provider-hint=NONE",
	}
	args = append(args, c.dranetArgs...)
	if _, err := c.nodeExec(ctx, args...); err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		if _, err := c.nodeExec(ctx, "systemctl", "is-active", "--quiet", dranetUnit); err != nil {
			logs, _ := c.dranetLogs(ctx, "")
			return false, fmt.Errorf("dranet unit is not active:\n%s", logs)
		}
		slices, err := c.nodeSlices(ctx)
		if err != nil {
			return false, nil
		}
		return len(slices) > 0, nil
	})
}

func (c *cluster) stopDranet(ctx context.Context) {
	_, _ = c.nodeExec(ctx, "systemctl", "stop", dranetUnit)
	_, _ = c.nodeExec(ctx, "systemctl", "reset-failed", dranetUnit)
}

// restartDranet stops the running driver and starts it again. The
// checkpoint database and the kubelet registration survive, like a
// DaemonSet pod restart does.
func (c *cluster) restartDranet(ctx context.Context) error {
	c.stopDranet(ctx)
	return c.startDranet(ctx)
}

// dranetLogs returns the journal of the dranet unit. since uses journalctl
// syntax, for example "-2min" or an RFC3339 timestamp; empty means all.
func (c *cluster) dranetLogs(ctx context.Context, since string) (string, error) {
	args := []string{"journalctl", "--no-pager", "-o", "short-precise", "-u", dranetUnit}
	if since != "" {
		args = append(args, "--since", since)
	}
	return c.nodeExec(ctx, args...)
}

// nodeSlices lists the ResourceSlices dranet published for the node.
func (c *cluster) nodeSlices(ctx context.Context) ([]resourceapi.ResourceSlice, error) {
	slices, err := c.client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		FieldSelector: "spec.driver=" + driverName + ",spec.nodeName=" + c.node,
	})
	if err != nil {
		return nil, err
	}
	return slices.Items, nil
}
