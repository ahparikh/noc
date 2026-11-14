package localapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "noc-api-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "noc.sock")
}

func serve(t *testing.T, handler http.Handler) (string, *Client) {
	t.Helper()
	path := shortSocketPath(t)
	listener, cleanup, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	client := NewClient(path)
	t.Cleanup(func() {
		client.Close()
		_ = server.Close()
		<-done
		_ = cleanup()
	})
	return path, client
}

func testHealth() apiv1.Health {
	return apiv1.Health{SchemaVersion: 1, Status: "ok", Version: "test", HostID: "host-1",
		StartedAt: time.Unix(1, 0).UTC(), Providers: []apiv1.ProviderHealth{}}
}

func newAgent(id string) apiv1.Agent {
	now := time.Now().UTC()
	return apiv1.Agent{
		SchemaVersion: 1, ID: id, HostID: "host-1", Provider: apiv1.ProviderCodex,
		NativeSessionID: "thread-" + id, LiveState: apiv1.LiveStateLive,
		RuntimeState: apiv1.RuntimeStateRunning, ActivityState: apiv1.ActivityStateIdle,
		Attention: apiv1.Attention{Type: apiv1.AttentionNone}, FirstSeenAt: now, LastSeenAt: now,
	}
}

// eventMemory adds an EventReader to the in-memory registry.
type eventMemory struct {
	*registry.Memory
	mu     sync.Mutex
	events []apiv1.AgentEvent
	last   registry.EventListOptions
}

func (m *eventMemory) ListEvents(_ context.Context, opts registry.EventListOptions) ([]apiv1.AgentEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = opts
	return m.events, nil
}

// bareReader implements only registry.Reader.
type bareReader struct{ m *registry.Memory }

func (b bareReader) List(ctx context.Context, o registry.ListOptions) ([]apiv1.Agent, error) {
	return b.m.List(ctx, o)
}
func (b bareReader) Get(ctx context.Context, id string) (apiv1.Agent, error) {
	return b.m.Get(ctx, id)
}
