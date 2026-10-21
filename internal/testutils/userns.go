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

package testutils

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var (
	isSupported bool
	checkOnce   sync.Once
)

// IsSupported checks if unprivileged user namespaces are enabled on the host system.
// It caches the result after the first check.
func IsSupported() bool {
	checkOnce.Do(func() {
		cmd := exec.Command("sleep", "1")

		// Attempt to map ourselves to root inside the userns.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}

		if err := cmd.Start(); err != nil {
			return // User namespaces are likely restricted or not supported
		}

		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}()

		isSupported = true
	})

	return isSupported
}

// Run executes the given test function inside a user namespace where the
// current user is mapped to root. This provides capabilities to create network
// namespaces and netfilter rules without running as actual root on the host.
//
// extraCloneflags can be used to request additional namespace types
// (e.g., syscall.CLONE_NEWNET).
//
// Requesting syscall.CLONE_NEWNS in extraCloneflags provides a private
// tmpfs on /run so that new network namespaces can be mounted for the test.
func Run(t *testing.T, f func(t *testing.T), extraCloneflags ...uintptr) {
	const subprocessEnvKey = "GO_USERNS_SUBPROCESS_KEY"

	cloneflags := uintptr(syscall.CLONE_NEWUSER)
	for _, flag := range extraCloneflags {
		cloneflags |= flag
	}

	// 1. If we are already inside the subprocess, run the actual test logic.
	if testIDString, ok := os.LookupEnv(subprocessEnvKey); ok && testIDString == "1" {
		// With a private mount namespace, replace /run with a fresh tmpfs so
		// the test can bind-mount network namespaces without touching the host.
		if cloneflags&syscall.CLONE_NEWNS != 0 {
			if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
				t.Fatalf("failed to mount a private tmpfs on /run in the subprocess: %v", err)
			}
		}
		t.Run("subprocess", f)
		return
	}

	// 2. If we are on the host, verify support before attempting to spawn.
	if !IsSupported() {
		t.Skip("Unprivileged user namespaces are not supported on this system")
	}

	// 3. Prepare the command to re-execute the current test binary.
	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{os.Args[0], "-test.run=" + t.Name() + "$", "-test.v=true"}

	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.testlogfile=") {
			cmd.Args = append(cmd.Args, arg)
		}
	}

	cmd.Env = append(os.Environ(), subprocessEnvKey+"=1")
	// Include sbin in PATH, as some networking commands are not found otherwise.
	cmd.Env = append(cmd.Env, "PATH=/usr/local/sbin:/usr/sbin:/sbin:"+os.Getenv("PATH"))
	cmd.Stdin = os.Stdin

	// 4. Map ourselves to root inside the new user namespace.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  cloneflags,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	// 5. Execute and capture output.
	out, err := cmd.CombinedOutput()

	// Prepending a newline makes the nested test output much easier to read
	// in the standard `go test` log format.
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("Subprocess execution failed: %v", err)
	}
}

// RunBenchmark executes the given benchmark function inside a user namespace
// where the current user is mapped to root.
func RunBenchmark(b *testing.B, f func(b *testing.B), extraCloneflags ...uintptr) {
	const subprocessEnvKey = "GO_USERNS_SUBPROCESS_KEY"

	cloneflags := uintptr(syscall.CLONE_NEWUSER)
	for _, flag := range extraCloneflags {
		cloneflags |= flag
	}

	if testIDString, ok := os.LookupEnv(subprocessEnvKey); ok && testIDString == "1" {
		if cloneflags&syscall.CLONE_NEWNS != 0 {
			if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
				b.Fatalf("failed to mount a private tmpfs on /run in the subprocess: %v", err)
			}
		}
		f(b)
		return
	}

	if !IsSupported() {
		b.Skip("Unprivileged user namespaces are not supported on this system")
	}

	cmd := exec.Command(os.Args[0])
	cmd.Args = []string{os.Args[0], "-test.run=^$", "-test.bench=^" + b.Name() + "$"}
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-test.benchtime=") || strings.HasPrefix(arg, "-test.benchmem") || strings.HasPrefix(arg, "-test.count=") {
			cmd.Args = append(cmd.Args, arg)
		}
	}

	cmd.Env = append(os.Environ(), subprocessEnvKey+"=1")
	cmd.Env = append(cmd.Env, "PATH=/usr/local/sbin:/usr/sbin:/sbin:"+os.Getenv("PATH"))
	cmd.Stdin = os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  cloneflags,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	out, err := cmd.CombinedOutput()
	os.Stdout.Write(out)
	if err != nil {
		b.Fatalf("Subprocess benchmark execution failed: %v\n%s", err, out)
	}
	b.SkipNow()
}
