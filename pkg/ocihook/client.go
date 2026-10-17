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

package ocihook

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// ExitSuccess and the other exit codes of the hook process. The runtime
	// fails the container on any non-zero code and reports stderr.
	ExitSuccess = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Args returns the arguments, after the executable, that make the DRANET
// binary run as the hook for containerID.
func Args(socket, containerID string, timeout time.Duration) []string {
	return []string{
		Subcommand,
		"--socket=" + socket,
		"--container-id=" + containerID,
		"--timeout=" + ClientTimeout(timeout).String(),
	}
}

// Run is the hook process: the DRANET executable started by the container
// runtime with Args. It is a semaphore for the daemon and must stay small
// and fast: it parses its arguments, drains the OCI state the runtime writes
// on stdin, signals the daemon over the socket, and waits for the answer.
// It returns the exit code and writes failures to stderr, which the runtime
// reports as the container's start error.
func Run(args []string, stdin io.Reader, stderr io.Writer) int {
	flags := flag.NewFlagSet(Subcommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", DefaultSocketPath, "UNIX socket of the DRANET daemon")
	containerID := flags.String("container-id", "", "ID of the container being created")
	timeout := flags.Duration("timeout", ClientTimeout(DefaultTimeout), "time to wait for the daemon")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "dranet %s: unexpected arguments %q\n", Subcommand, flags.Args())
		return ExitUsage
	}

	// The runtime writes the container state on stdin. It is not needed, but
	// it is drained so the runtime never blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, stdin) }()

	if err := Signal(*socket, *containerID, *timeout); err != nil {
		fmt.Fprintf(stderr, "dranet %s: %v\n", Subcommand, err)
		return ExitFailure
	}
	return ExitSuccess
}

// Signal tells the daemon at socket that containerID is paused in the
// runtime, and waits at most timeout for the daemon to run the provider hook.
// A nil error means the container may start.
func Signal(socket, containerID string, timeout time.Duration) error {
	request, err := encodeRequest(containerID)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return fmt.Errorf("the DRANET daemon is not reachable at %s: %w", socket, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set the socket deadline: %w", err)
	}
	if _, err := io.WriteString(conn, request); err != nil {
		return fmt.Errorf("send the request to the DRANET daemon: %w", err)
	}
	line, err := readLine(bufio.NewReaderSize(conn, MaxLineLength))
	if err != nil {
		if isTimeout(err) {
			return fmt.Errorf("the DRANET daemon did not answer for container %s within %v", containerID, timeout)
		}
		return fmt.Errorf("read the answer of the DRANET daemon for container %s: %w", containerID, err)
	}
	if err := decodeResponse(line); err != nil {
		return fmt.Errorf("container %s: %w", containerID, err)
	}
	return nil
}

func isTimeout(err error) bool {
	timeout, ok := err.(interface{ Timeout() bool })
	return ok && timeout.Timeout()
}
