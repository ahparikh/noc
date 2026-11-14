package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noc.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func testAgent(id, session string) apiv1.Agent {
	return apiv1.Agent{
		ID:              id,
		HostID:          "host-1",
		Provider:        apiv1.ProviderClaude,
		NativeSessionID: session,
		LiveState:       apiv1.LiveStateLive,
		RuntimeState:    apiv1.RuntimeStateRunning,
		ActivityState:   apiv1.ActivityStateWorking,
		Name:            "provider name",
		FirstSeenAt:     baseTime,
		LastSeenAt:      baseTime,
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil || err.Error() != "SQLite path is required" {
		t.Fatalf("err = %v", err)
	}
}

func TestStorePersistsHostAndAgents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "noc.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	host, err := s.HostID(ctx)
	if err != nil || host == "" {
		t.Fatalf("HostID = %q, %v", host, err)
	}
	if again, _ := s.HostID(ctx); again != host {
		t.Fatalf("host changed within one open: %q vs %q", again, host)
	}

	if err := s.Upsert(ctx, apiv1.Agent{ID: "a"}); err == nil ||
		err.Error() != "agent ID, host ID, provider, and native session ID are required" {
		t.Fatalf("validation err = %v", err)
	}

	agent := testAgent("agent-1", "native-1")
	agent.UserName = "ignored on upsert"
	if err := s.Upsert(ctx, agent); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserName != "" {
		t.Fatalf("upsert must ignore user_name, got %q", got.UserName)
	}
	if got.SchemaVersion != 1 || got.DisplayState != apiv1.DisplayStateWorking || got.Bucket != apiv1.BucketWorking {
		t.Fatalf("derived fields wrong: %+v", got)
	}
	if !got.LastSeenAt.Equal(baseTime) {
		t.Fatalf("last_seen_at = %v", got.LastSeenAt)
	}

	named, err := s.SetUserName(ctx, "agent-1", "  my label ")
	if err != nil || named.UserName != "  my label " {
		t.Fatalf("SetUserName = %+v, %v", named, err)
	}

	// Refresh with an unchanged and with an absent provider name keeps the label.
	refresh := agent
	refresh.LastSeenAt = baseTime.Add(time.Minute)
	if err := s.Upsert(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	refresh.Name = ""
	if err := s.Upsert(ctx, refresh); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "agent-1")
	if got.UserName != "  my label " {
		t.Fatalf("user_name lost on refresh: %q", got.UserName)
	}

	by, err := s.GetByNativeIdentity(ctx, "host-1", apiv1.ProviderClaude, "native-1")
	if err != nil || by.ID != "agent-1" {
		t.Fatalf("GetByNativeIdentity = %+v, %v", by, err)
	}
	if _, err := s.GetByNativeIdentity(ctx, "host-1", apiv1.ProviderCodex, "native-1"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.SetUserName(ctx, "missing", "x"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	empty, err := s.List(ctx, registry.ListOptions{})
	if err != nil || len(empty) != 1 {
		t.Fatalf("List = %v, %v", empty, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v", info.Mode().Perm())
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if again, _ := reopened.HostID(ctx); again != host {
		t.Fatalf("host id changed across reopen: %q vs %q", again, host)
	}
	got, err = reopened.Get(ctx, "agent-1")
	if err != nil || got.UserName != "  my label " {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
}

func TestListFiltersAndOrdersAlphabetically(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	idle := testAgent("b", "s-b")
	idle.ActivityState = apiv1.ActivityStateIdle
	idle.RuntimeState = apiv1.RuntimeStateStopped
	working := testAgent("a", "s-a")
	gone := testAgent("c", "s-c")
	gone.LiveState = apiv1.LiveStateNotLive
	for _, a := range []apiv1.Agent{working, idle, gone} {
		if err := s.Upsert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	live, _ := s.List(ctx, registry.ListOptions{})
	if len(live) != 2 || live[0].ID != "b" || live[1].ID != "a" { // idle < working
		t.Fatalf("live list = %+v", live)
	}
	all, _ := s.List(ctx, registry.ListOptions{IncludeNotLive: true})
	if len(all) != 3 {
		t.Fatalf("all = %d", len(all))
	}
	none, err := openEmptyList(t)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty list must be non-nil: %v %v", none, err)
	}
}

func openEmptyList(t *testing.T) ([]apiv1.Agent, error) {
	s, _ := openStore(t)
	return s.List(context.Background(), registry.ListOptions{})
}

func TestForgetDeletesOnlyTheNamedAgent(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	a, b := testAgent("a", "s-a"), testAgent("b", "s-b")
	a.ProcessID = 4242
	for _, x := range []apiv1.Agent{a, b} {
		if err := s.Upsert(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, "a")
	if err != nil || got.ProcessID != 4242 {
		t.Fatalf("process_id round trip: %+v, %v", got, err)
	}
	if err := s.Forget(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get after forget = %v", err)
	}
	if _, err := s.Get(ctx, "b"); err != nil {
		t.Fatalf("other agent removed: %v", err)
	}
	if err := s.Forget(ctx, "a"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("second forget = %v", err)
	}
}

func TestPruneRetiredMatchesTheMemoryPredicate(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	mem := registry.NewMemory()
	cutoff := baseTime
	old, recent := cutoff.Add(-48*time.Hour), cutoff.Add(time.Hour)

	mk := func(id string, mutate func(*apiv1.Agent)) apiv1.Agent {
		a := testAgent(id, "s-"+id)
		a.LiveState = apiv1.LiveStateNotLive
		a.RuntimeState = apiv1.RuntimeStateStopped
		a.ActivityState = apiv1.ActivityStateIdle
		mutate(&a)
		return a
	}
	engaged := func(a *apiv1.Agent) {
		a.Name = ""
		a.InitialRequest = &apiv1.TextPreview{Text: "do work", Source: "hook", ObservedAt: baseTime}
	}
	fixtures := []apiv1.Agent{
		mk("live", func(a *apiv1.Agent) { a.LiveState = apiv1.LiveStateLive; a.LastSeenAt = old }),
		mk("stale", func(a *apiv1.Agent) { a.LiveState = apiv1.LiveStateCandidateStale; a.LastSeenAt = old }),
		mk("never-engaged-recent", func(a *apiv1.Agent) { a.Name = ""; a.LastSeenAt = recent }),
		mk("engaged-old", func(a *apiv1.Agent) { engaged(a); a.LastSeenAt = old }),
		mk("engaged-recent", func(a *apiv1.Agent) { engaged(a); a.LastSeenAt = recent }),
		mk("named-old", func(a *apiv1.Agent) { a.Name = "kept by provider"; a.LastSeenAt = old }),
		mk("labelled-old", func(a *apiv1.Agent) { a.Name = ""; a.LastSeenAt = old }),
	}
	for _, f := range fixtures {
		if err := s.Upsert(ctx, f); err != nil {
			t.Fatal(err)
		}
		if err := mem.Upsert(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, store := range []interface {
		SetUserName(context.Context, string, string) (apiv1.Agent, error)
	}{s, mem} {
		if _, err := store.SetUserName(ctx, "labelled-old", "mine"); err != nil {
			t.Fatal(err)
		}
	}

	nSQL, err := s.PruneRetired(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	nMem, _ := mem.PruneRetired(ctx, cutoff)
	if nSQL != nMem || nSQL != 3 { // never-engaged-recent, engaged-old, named-old
		// named-old has a provider name, so only last_seen_at < cutoff applies: pruned.
		t.Fatalf("pruned sqlite=%d memory=%d", nSQL, nMem)
	}
	sqlAgents, _ := s.List(ctx, registry.ListOptions{IncludeNotLive: true})
	memAgents, _ := mem.List(ctx, registry.ListOptions{IncludeNotLive: true})
	if len(sqlAgents) != len(memAgents) {
		t.Fatalf("remaining differ: %d vs %d", len(sqlAgents), len(memAgents))
	}
	for i := range sqlAgents {
		if sqlAgents[i].ID != memAgents[i].ID {
			t.Fatalf("remaining[%d]: %s vs %s", i, sqlAgents[i].ID, memAgents[i].ID)
		}
	}
}

func TestProviderRenameReplacesUserName(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	agent := testAgent("a", "s-a")
	if err := s.Upsert(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserName(ctx, "a", "label"); err != nil {
		t.Fatal(err)
	}
	renamed := agent
	renamed.Name = "new provider name"
	if err := s.Upsert(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "a")
	if got.UserName != "" || got.Name != "new provider name" {
		t.Fatalf("rename must clear user_name: %+v", got)
	}

	// A new label on a row with no stored provider name is cleared by a first name.
	bare := testAgent("b", "s-b")
	bare.Name = ""
	_ = s.Upsert(ctx, bare)
	_, _ = s.SetUserName(ctx, "b", "label")
	bare.Name = "first name"
	_ = s.Upsert(ctx, bare)
	got, _ = s.Get(ctx, "b")
	if got.UserName != "" {
		t.Fatalf("first provider name should clear label, got %q", got.UserName)
	}
}

func TestUpsertNaturalKeyConflictWithDifferentIDFails(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.Upsert(ctx, testAgent("a", "same-session")); err != nil {
		t.Fatal(err)
	}
	err := s.Upsert(ctx, testAgent("other-id", "same-session"))
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: agents.host_id, agents.provider, agents.native_session_id") ||
		!strings.HasPrefix(err.Error(), "upsert agent: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrationsAreIdempotentAndTolerateNewerDatabases(t *testing.T) {
	ctx := context.Background()
	s, path := openStore(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (99, 'x')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("newer database must be accepted: %v", err)
	}
	defer again.Close()
	var n int
	_ = again.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if n != 5 {
		t.Fatalf("migrations rows = %d", n)
	}
}
