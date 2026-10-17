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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// requestTimeout bounds the time a connected hook has to send its request.
// The hook writes it as soon as it connects.
const requestTimeout = 5 * time.Second

// Handler runs the provider hook for the container the hook process reports.
// It returns nil when the container may start. The context carries the
// deadline the daemon gives the provider.
type Handler func(ctx context.Context, containerID string) error

// Server is the daemon's end of the hook channel: a UNIX socket that answers
// one request per connection.
type Server struct {
	listener net.Listener
	timeout  time.Duration
	handler  Handler

	wg sync.WaitGroup
}

// Listen creates the socket at path, replacing a stale one left by a
// previous daemon, and restricts it to root: the hooks run as the container
// runtime, and nothing else should be able to trigger a provider.
// timeout is the runtime's hook timeout; the handler gets ProviderTimeout of
// it.
func Listen(path string, timeout time.Duration, handler Handler) (*Server, error) {
	if handler == nil {
		return nil, fmt.Errorf("hook handler is nil")
	}
	if err := ValidateTimeout(timeout); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create the hook socket directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove the stale hook socket %s: %w", path, err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on the hook socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("restrict the hook socket %s: %w", path, err)
	}
	return &Server{listener: listener, timeout: timeout, handler: handler}, nil
}

// Addr returns the socket path.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve answers hooks until ctx is done or the listener fails, then waits
// for the requests in flight. Each request runs in its own goroutine: Pods
// start concurrently and a provider may take its whole deadline.
func (s *Server) Serve(ctx context.Context) error {
	defer s.wg.Wait()
	stop := context.AfterFunc(ctx, func() { s.listener.Close() })
	defer stop()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on the hook socket: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(ctx, conn)
		}()
	}
}

// Close stops accepting hooks. Serve returns and the requests in flight
// finish.
func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Handle returns before the runtime's timeout: the provider's deadline
	// leaves a margin for the answer to be written and read.
	if err := conn.SetDeadline(time.Now().Add(s.timeout)); err != nil {
		return
	}
	containerID, err := s.readRequest(conn)
	if err != nil {
		_, _ = io.WriteString(conn, encodeResponse(err))
		return
	}
	hookCtx, cancel := context.WithTimeout(ctx, ProviderTimeout(s.timeout))
	defer cancel()
	_, _ = io.WriteString(conn, encodeResponse(s.handler(hookCtx, containerID)))
}

func (s *Server) readRequest(conn net.Conn) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
		return "", err
	}
	line, err := readLine(bufio.NewReaderSize(conn, MaxLineLength))
	if err != nil {
		return "", fmt.Errorf("read the hook request: %w", err)
	}
	return decodeRequest(line)
}
