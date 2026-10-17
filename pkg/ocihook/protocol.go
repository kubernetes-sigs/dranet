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

// Package ocihook implements the OCI runtime hook DRANET injects into the
// containers of the Pods it serves, and the channel between that hook and
// the DRANET daemon.
//
// NRI callbacks run inside the container runtime's request timeout (two
// seconds by default), too short for a provider to configure a Pod's network
// namespace. An OCI createRuntime hook has its own timeout and runs after the
// namespaces exist and before the container's process starts. DRANET does not
// install a separate binary on the node: the hook is the DRANET executable
// itself (dranet oci-hook), reached through /proc/<pid>/exe, and it acts as a
// semaphore. It connects to the daemon over a UNIX socket, says which
// container is paused, and blocks until the daemon has run the provider hook.
// All state and logic stay in the daemon.
//
// The wire protocol is one line each way:
//
//	hook   -> daemon: "READY <container-id>\n"
//	daemon -> hook:   "OK\n" | "FAIL <reason>\n"
//
// A FAIL makes the hook exit non-zero with the reason on stderr, which the
// runtime reports as the container's start error.
package ocihook

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// Subcommand is the first argument that makes the DRANET executable run
	// as the hook instead of the daemon.
	Subcommand = "oci-hook"

	// DefaultSocketPath is where the daemon listens for its hooks. It is under
	// the directory the deployments share with the host, where the runtime
	// executes the hook.
	DefaultSocketPath = "/var/run/dranet/hook.sock"

	// DefaultTimeout is the time the runtime gives the hook before it kills
	// it and fails the container. It bounds the provider's work.
	DefaultTimeout = 30 * time.Second

	// MaxTimeout is the maximum hook timeout allowed, keeping the OCI hook
	// well within the kubelet's runtime request timeout.
	MaxTimeout = 60 * time.Second

	// ProviderTimeoutMargin is kept between the provider's deadline and the
	// hook's timeout, so a slow provider is reported as a FAIL with a reason
	// instead of being killed by the runtime without one.
	ProviderTimeoutMargin = 5 * time.Second

	// ClientTimeoutMargin is kept between the hook's own deadline and the
	// runtime's, so a daemon that never answers is reported by the hook
	// instead of the runtime's generic timeout message.
	ClientTimeoutMargin = 2 * time.Second

	// MaxLineLength bounds a protocol line. Container IDs are 64 hex
	// characters and reasons are short messages.
	MaxLineLength = 4096

	verbReady = "READY"
	verbOK    = "OK"
	verbFail  = "FAIL"
)

// ProviderTimeout is the deadline the daemon gives the provider for a hook
// whose runtime timeout is timeout.
func ProviderTimeout(timeout time.Duration) time.Duration {
	return timeout - ProviderTimeoutMargin
}

// ClientTimeout is the deadline the hook process gives itself for a hook
// whose runtime timeout is timeout.
func ClientTimeout(timeout time.Duration) time.Duration {
	return timeout - ClientTimeoutMargin
}

// ValidateTimeout checks that a hook timeout leaves room for the margins and
// does not exceed MaxTimeout.
func ValidateTimeout(timeout time.Duration) error {
	minimum := ProviderTimeoutMargin + ClientTimeoutMargin
	if timeout <= minimum || timeout > MaxTimeout {
		return fmt.Errorf("hook timeout %v must be over %v and at most %v", timeout, minimum, MaxTimeout)
	}
	return nil
}

// readLine reads one protocol line, without its terminator, rejecting lines
// over MaxLineLength.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return "", fmt.Errorf("unterminated line %q", truncate(line))
		}
		return "", err
	}
	if len(line) > MaxLineLength {
		return "", fmt.Errorf("line over %d bytes", MaxLineLength)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// encodeRequest formats the hook's request for containerID.
func encodeRequest(containerID string) (string, error) {
	if err := validateContainerID(containerID); err != nil {
		return "", err
	}
	return verbReady + " " + containerID + "\n", nil
}

// decodeRequest parses a request line and returns the container ID.
func decodeRequest(line string) (string, error) {
	verb, containerID, _ := strings.Cut(line, " ")
	if verb != verbReady {
		return "", fmt.Errorf("unexpected request %q", truncate(line))
	}
	if err := validateContainerID(containerID); err != nil {
		return "", err
	}
	return containerID, nil
}

// encodeResponse formats the daemon's answer: OK, or FAIL with the error.
func encodeResponse(err error) string {
	if err == nil {
		return verbOK + "\n"
	}
	// A multi-line reason would be read as several protocol lines.
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if reason == "" {
		reason = "unknown error"
	}
	if len(reason) > MaxLineLength-len(verbFail)-2 {
		reason = reason[:MaxLineLength-len(verbFail)-2]
	}
	return verbFail + " " + reason + "\n"
}

// decodeResponse parses the daemon's answer: nil for OK, an error with the
// reason for FAIL.
func decodeResponse(line string) error {
	verb, reason, _ := strings.Cut(line, " ")
	switch verb {
	case verbOK:
		return nil
	case verbFail:
		if reason == "" {
			reason = "unknown error"
		}
		return fmt.Errorf("%s", reason)
	default:
		return fmt.Errorf("unexpected response %q", truncate(line))
	}
}

// validateContainerID accepts the IDs runtimes use: non-empty, printable,
// without whitespace, and short.
func validateContainerID(id string) error {
	if id == "" {
		return fmt.Errorf("empty container ID")
	}
	if len(id) > 256 {
		return fmt.Errorf("container ID over 256 bytes")
	}
	for _, r := range id {
		if r <= ' ' || r > '~' {
			return fmt.Errorf("container ID %q contains an invalid character", truncate(id))
		}
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
