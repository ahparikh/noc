package codex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noc.local/noc/internal/provider"
)

// controlSocketRelativePath is the shared daemon's control socket under
// CODEX_HOME.
var controlSocketRelativePath = filepath.Join("app-server-control", "app-server-control.sock")

// resolveSocketPath returns the control socket and whether it was explicit.
// An explicit socket (option, then NOC_CODEX_SOCKET) disables proxy fallback.
// If CODEX_HOME is unset and the home directory is unresolvable, the inferred
// path is empty.
func resolveSocketPath(explicit string) (path string, isExplicit bool) {
	if v := strings.TrimSpace(explicit); v != "" {
		return filepath.Clean(v), true
	}
	if v := strings.TrimSpace(os.Getenv(socketEnv)); v != "" {
		return filepath.Clean(v), true
	}
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil || userHome == "" {
			return "", false
		}
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(filepath.Clean(home), controlSocketRelativePath), false
}

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// socketClient talks to the shared daemon directly over its Unix socket. It
// falls back (when configured) only if the daemon was never reached: an empty
// path or a failed dial. Failures after a successful dial are never hidden.
type socketClient struct {
	path     string
	timeout  time.Duration
	dial     dialFunc
	fallback DiscoveryClient
}

var (
	_ DiscoveryClient = (*socketClient)(nil)
	_ ThreadNamer     = (*socketClient)(nil)
)

func newSocketClient(path string, timeout time.Duration, fallback DiscoveryClient) *socketClient {
	return &socketClient{
		path:     path,
		timeout:  timeout,
		dial:     (&net.Dialer{}).DialContext,
		fallback: fallback,
	}
}

// DiscoverLive runs one snapshot under a single deadline shared with any
// fallback.
func (c *socketClient) DiscoverLive(ctx context.Context) ([]provider.NativeAgent, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var agents []provider.NativeAgent
	connected, err := c.withConnection(ctx,
		func(err error) error {
			return fmt.Errorf("discover through Codex app-server socket %q: %w", c.path, err)
		},
		func(ctx context.Context, conn *rpcConnection) error {
			var err error
			agents, err = discoverFromConnection(ctx, conn)
			return err
		})
	if err == nil {
		return agents, nil
	}
	if connected || c.fallback == nil {
		return nil, err
	}
	agents, fallbackErr := c.fallback.DiscoverLive(ctx)
	if fallbackErr != nil {
		return nil, fmt.Errorf("%w; Codex proxy fallback failed: %v", err, fallbackErr)
	}
	return agents, nil
}

// SetThreadName pushes a thread name over a fresh connection.
func (c *socketClient) SetThreadName(ctx context.Context, threadID, name string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	connected, err := c.withConnection(ctx,
		func(err error) error { return err },
		func(ctx context.Context, conn *rpcConnection) error {
			return setThreadNameOnConnection(ctx, conn, threadID, name)
		})
	if err == nil {
		return nil
	}
	namer, ok := c.fallback.(ThreadNamer)
	if connected || !ok {
		return err
	}
	if fallbackErr := namer.SetThreadName(ctx, threadID, name); fallbackErr != nil {
		return fmt.Errorf("%w; Codex proxy fallback failed: %v", err, fallbackErr)
	}
	return nil
}

// withConnection dials, upgrades, and runs fn. connected is false only when
// the daemon was not reached (empty path or failed dial). wrap decorates errors
// from fn, not from dialing or the upgrade.
func (c *socketClient) withConnection(
	ctx context.Context,
	wrap func(error) error,
	fn func(context.Context, *rpcConnection) error,
) (connected bool, err error) {
	if strings.TrimSpace(c.path) == "" {
		return false, errors.New("Codex app-server socket path is unavailable")
	}
	conn, err := c.dial(ctx, "unix", c.path)
	if err != nil {
		return false, fmt.Errorf("dial Codex app-server socket %q: %w", c.path, err)
	}
	defer conn.Close()
	// Closing the socket unblocks any in-progress read or write when the
	// deadline fires; one mechanism covers the upgrade and every RPC.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	transport, err := newWebSocketMessageTransport(ctx, conn)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return true, fmt.Errorf("open Codex app-server WebSocket on %q: %w", c.path, err)
	}
	if err := fn(ctx, newRPCConnection(transport)); err != nil {
		return true, wrap(err)
	}
	// No WebSocket Close frame: the deferred close ends the connection.
	return true, nil
}
