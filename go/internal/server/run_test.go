package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/provider"
	sqlitestore "noc.local/noc/internal/storage/sqlite"
	apiv1 "noc.local/noc/pkg/api/v1"
)

type fakeAdapter struct {
	name     apiv1.Provider
	probe    provider.ProbeResult
	sessions []string
	probes   atomic.Int32
	finds    atomic.Int32
}

func newAdapter(name apiv1.Provider, available, discoverLive bool, sessions ...string) *fakeAdapter {
	f := &fakeAdapter{name: name, sessions: sessions}
	f.probe = provider.ProbeResult{Provider: name, Available: available, Version: "1.2.3", ObservedAt: time.Now().UTC()}
	if discoverLive {
		f.probe.Features = map[string]bool{"discover_live": true}
	}
	return f
}

func (f *fakeAdapter) Provider() apiv1.Provider { return f.name }
func (f *fakeAdapter) Probe(context.Context) provider.ProbeResult {
	f.probes.Add(1)
	return f.probe
}
func (f *fakeAdapter) DiscoverLive(context.Context) ([]provider.NativeAgent, error) {
	f.finds.Add(1)
	var out []provider.NativeAgent
	for _, s := range f.sessions {
		out = append(out, provider.NativeAgent{NativeSessionID: s, Name: "name-" + s})
	}
	return out, nil
}
func (f *fakeAdapter) Normalize(a provider.NativeAgent) (provider.NormalizedAgent, error) {
	return provider.NormalizedAgent{
		Provider: f.name, NativeSessionID: a.NativeSessionID, Name: a.Name, SourceKind: "interactive",
		RuntimeState: apiv1.RuntimeStateRunning, ActivityState: apiv1.ActivityStateIdle,
	}, nil
}

type renamerAdapter struct {
	*fakeAdapter
	names atomic.Value
}

func (r *renamerAdapter) SetName(_ context.Context, id, name string) error {
	r.names.Store(id + "=" + name)
	return nil
}

type running struct {
	client *localapi.Client
	cancel context.CancelFunc
	done   chan error
	db     string
	socket string
}

func start(t *testing.T, cfg Config) *running {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "noc-srv-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(dir, "run", "noc.sock")
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(dir, "state", "noc.db")
	}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ready := make(chan struct{})
	cfg.Ready = func() { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server not ready")
	}
	r := &running{client: localapi.NewClient(cfg.SocketPath), cancel: cancel, done: done, db: cfg.DatabasePath, socket: cfg.SocketPath}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.client.Close()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
		r.done <- nil // allow repeated stop
	case <-time.After(3 * time.Second):
		t.Error("Run did not return within 3s")
	}
}

func TestRunServesInitialDiscovery(t *testing.T) {
	claudeAd := newAdapter(apiv1.ProviderClaude, true, true, "c-1")
	codexAd := newAdapter(apiv1.ProviderCodex, true, true, "x-1", "x-2")
	r := start(t, Config{Adapters: []provider.Adapter{claudeAd, codexAd}, DiscoveryInterval: time.Hour})
	ctx := context.Background()

	health, err := r.client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.HostID == "" || health.StartedAt.IsZero() || len(health.Providers) != 2 {
		t.Fatalf("health = %+v", health)
	}
	c, x := health.Providers[0], health.Providers[1]
	if c.Provider != apiv1.ProviderClaude || x.Provider != apiv1.ProviderCodex {
		t.Fatalf("order = %v %v", c.Provider, x.Provider)
	}
	if c.Status != "available" || c.Detail != "discovery active; observed 1 live sessions" || c.StartupStatus != "available" ||
		c.StartupDetail != c.Detail || c.StartupObservedAt == nil || c.StartupObservedAt.Before(health.StartedAt) || c.Version != "1.2.3" {
		t.Fatalf("claude health = %+v", c)
	}
	if x.Detail != "discovery active; observed 2 live sessions" {
		t.Fatalf("codex health = %+v", x)
	}

	list, err := r.client.ListAgents(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Agents) != 3 {
		t.Fatalf("agents = %d", len(list.Agents))
	}
	byProvider := map[apiv1.Provider]int{}
	for _, a := range list.Agents {
		byProvider[a.Provider]++
		if a.LiveState != apiv1.LiveStateLive || a.HostID != health.HostID {
			t.Errorf("agent = %+v", a)
		}
	}
	if byProvider[apiv1.ProviderClaude] != 1 || byProvider[apiv1.ProviderCodex] != 2 {
		t.Fatalf("by provider = %v", byProvider)
	}
	if claudeAd.probes.Load() != 1 {
		t.Errorf("probes = %d", claudeAd.probes.Load())
	}
}

func TestRunDiscoveryLoopPollsAfterInterval(t *testing.T) {
	ad := newAdapter(apiv1.ProviderClaude, true, true, "c-1")
	start(t, Config{Adapters: []provider.Adapter{ad}, DiscoveryInterval: 30 * time.Millisecond})
	deadline := time.Now().Add(3 * time.Second)
	for ad.finds.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ad.finds.Load() < 3 {
		t.Fatalf("discoveries = %d", ad.finds.Load())
	}
}

func TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "noc-srv-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dbPath := filepath.Join(dir, "state", "noc.db")

	ctx := context.Background()
	seed, err := sqlitestore.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hostID, _ := seed.HostID(ctx)
	now := time.Now().UTC()
	if err := seed.Upsert(ctx, apiv1.Agent{
		ID: "agent-x", HostID: hostID, Provider: apiv1.ProviderCodex, NativeSessionID: "thread-x", Origin: "hook",
		LiveState: apiv1.LiveStateLive, RuntimeState: apiv1.RuntimeStateRunning, ActivityState: apiv1.ActivityStateIdle,
		Attention: apiv1.Attention{Type: apiv1.AttentionNone}, FirstSeenAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	ad := &renamerAdapter{fakeAdapter: newAdapter(apiv1.ProviderCodex, true, true, "never")}
	r := start(t, Config{DatabasePath: dbPath, Adapters: []provider.Adapter{ad}, DisableDiscovery: true, RetireAfter: -1})

	health, err := r.client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := health.Providers[0]
	if h.Status != "disabled" || h.Detail != "provider discovery disabled" || h.StartupStatus != "disabled" ||
		h.StartupObservedAt == nil || !h.StartupObservedAt.Equal(health.StartedAt) || !h.ObservedAt.Equal(health.StartedAt) {
		t.Fatalf("health = %+v", h)
	}
	if ad.probes.Load() != 0 || ad.finds.Load() != 0 {
		t.Fatalf("probes=%d finds=%d", ad.probes.Load(), ad.finds.Load())
	}

	res, err := r.client.RenameAgent(ctx, "agent-x", "Cache work")
	if err != nil || res.ProviderSync != apiv1.ProviderNameSynced {
		t.Fatalf("rename = %+v %v", res, err)
	}
	if got, _ := ad.names.Load().(string); got != "thread-x=Cache work" {
		t.Fatalf("renamer called with %q", got)
	}
}

func TestRunAvailableWithoutDiscoverLiveHasNoWorker(t *testing.T) {
	ad := newAdapter(apiv1.ProviderCodex, true, false, "x-1")
	ad.probe.Diagnostic = ""
	r := start(t, Config{Adapters: []provider.Adapter{ad}, DiscoveryInterval: 20 * time.Millisecond})
	time.Sleep(100 * time.Millisecond)
	health, err := r.client.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := health.Providers[0]
	if h.Status != "available" || h.Detail != "" || h.StartupStatus != "available" || ad.finds.Load() != 0 {
		t.Fatalf("health = %+v finds=%d", h, ad.finds.Load())
	}
	list, _ := r.client.ListAgents(context.Background(), true)
	if len(list.Agents) != 0 {
		t.Fatalf("agents = %+v", list.Agents)
	}
}

func TestRunRefusesLiveSocketAndStorageFailureAborts(t *testing.T) {
	r := start(t, Config{Adapters: []provider.Adapter{}})
	_ = r
	dir := filepath.Dir(r.db)
	err := Run(context.Background(), Config{SocketPath: r.socket, DatabasePath: filepath.Join(dir, "second.db"), Adapters: []provider.Adapter{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != localapi.ErrServerRunning {
		t.Fatalf("err = %v", err)
	}
	if err := Run(context.Background(), Config{SocketPath: r.socket}); err == nil {
		t.Fatal("expected error for missing database path")
	}
}
