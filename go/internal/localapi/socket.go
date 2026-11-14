// Package localapi serves and consumes the noc HTTP API over a Unix socket.
package localapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"noc.local/noc/internal/config"
)

// ErrServerRunning is returned by Listen when a live server owns the socket.
var ErrServerRunning = errors.New("noc server is already running")

const probeTimeout = 200 * time.Millisecond

// Listen creates the user-only Unix socket at socketPath. The returned
// cleanup function removes the socket file.
func Listen(socketPath string) (net.Listener, func() error, error) {
	if err := config.EnsurePrivateDir(filepath.Dir(socketPath)); err != nil {
		return nil, nil, err
	}

	info, err := os.Lstat(socketPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("refusing to replace non-socket path %q", socketPath)
		}
		if conn, dialErr := net.DialTimeout("unix", socketPath, probeTimeout); dialErr == nil {
			_ = conn.Close()
			return nil, nil, ErrServerRunning
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("remove stale socket %q: %w", socketPath, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, nil, fmt.Errorf("inspect socket %q: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %q: %w", socketPath, err)
	}
	cleanup := func() error {
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove socket %q: %w", socketPath, err)
		}
		return nil
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = cleanup()
		return nil, nil, fmt.Errorf("secure socket %q: %w", socketPath, err)
	}
	return listener, cleanup, nil
}
