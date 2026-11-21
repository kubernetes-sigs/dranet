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

// Package integration runs dranet against a single node kind cluster.
//
// The cluster gives us a real apiserver, scheduler, resourceclaim controller,
// kubelet and containerd with NRI; dranet runs as a host process inside the
// node container, so there is no image build, kind load or DaemonSet rollout
// in the loop. Tests talk to the cluster with client-go and to the node with
// docker exec. See README.md for the environment variables.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// tc is the shared cluster for the whole package. Tests must not run in
// parallel: they share the node's network namespace.
var tc *cluster

// artifactsDir receives logs and object dumps of failed tests.
var artifactsDir string

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	artifactsDir = filepath.Join(envOr("ARTIFACTS", filepath.Join("..", "..", "_artifacts", "integration")), time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "artifacts of failed tests go to %s\n", artifactsDir)
	workdir, err := os.MkdirTemp("", "dranet-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(workdir)

	// Build first: a compile error must not cost a cluster creation.
	bin, err := buildDranet(ctx, workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	tc, err = setupCluster(ctx, workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer tc.teardown(context.Background())

	if err := tc.installDranet(ctx, bin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := ensureDeviceClass(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// ensureDeviceClass creates the dra.net DeviceClass used by every test.
func ensureDeviceClass(ctx context.Context) error {
	class := &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: driverName},
		Spec: resourceapi.DeviceClassSpec{
			Selectors: []resourceapi.DeviceSelector{{
				CEL: &resourceapi.CELDeviceSelector{Expression: `device.driver == "` + driverName + `"`},
			}},
		},
	}
	_, err := tc.client.ResourceV1().DeviceClasses().Create(ctx, class, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
