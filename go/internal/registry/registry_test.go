package registry

import (
	"context"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestPruneRetiredKeepsLiveAndNamedRows(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-15 * 24 * time.Hour)
	add := func(id string, a apiv1.Agent) {
		a.ID, a.HostID, a.Provider, a.NativeSessionID = id, "h", apiv1.ProviderClaude, id
		if err := m.Upsert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	add("live-old", apiv1.Agent{LiveState: apiv1.LiveStateLive, LastSeenAt: old})
	add("stale-old", apiv1.Agent{LiveState: apiv1.LiveStateCandidateStale, LastSeenAt: old})
	add("ended-old", apiv1.Agent{LiveState: apiv1.LiveStateNotLive, LastSeenAt: old, Name: "n"})
	add("ended-recent", apiv1.Agent{LiveState: apiv1.LiveStateNotLive, LastSeenAt: now, Name: "n"})
	add("recent-never-engaged", apiv1.Agent{LiveState: apiv1.LiveStateNotLive, LastSeenAt: now})
	add("named-old", apiv1.Agent{LiveState: apiv1.LiveStateNotLive, LastSeenAt: old})
	if _, err := m.SetUserName(ctx, "named-old", "keep me"); err != nil {
		t.Fatal(err)
	}
	pruned, err := m.PruneRetired(ctx, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned %d, want 2", pruned)
	}
	for _, id := range []string{"live-old", "stale-old", "ended-recent", "named-old"} {
		if _, err := m.Get(ctx, id); err != nil {
			t.Errorf("%s should survive: %v", id, err)
		}
	}
	for _, id := range []string{"ended-old", "recent-never-engaged"} {
		if _, err := m.Get(ctx, id); err == nil {
			t.Errorf("%s should be pruned", id)
		}
	}
}

func TestMemoryProviderRenameReplacesUserName(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	base := apiv1.Agent{ID: "a", HostID: "h", Provider: apiv1.ProviderCodex, NativeSessionID: "t", Name: "old", LiveState: apiv1.LiveStateLive}
	if err := m.Upsert(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetUserName(ctx, "a", "label"); err != nil {
		t.Fatal(err)
	}
	same := base
	same.UserName = "ignored by upsert"
	if err := m.Upsert(ctx, same); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(ctx, "a")
	if got.UserName != "label" {
		t.Fatalf("same name must keep the label, got %q", got.UserName)
	}
	nameless := base
	nameless.Name = ""
	m.Upsert(ctx, nameless)
	got, _ = m.Get(ctx, "a")
	if got.UserName != "label" {
		t.Fatalf("nameless observation must keep the label, got %q", got.UserName)
	}
	renamed := base
	renamed.Name = "new"
	m.Upsert(ctx, renamed)
	got, _ = m.Get(ctx, "a")
	if got.UserName != "" {
		t.Fatalf("provider rename must clear the label, got %q", got.UserName)
	}
	if err := m.Forget(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Forget(ctx, "a"); err != ErrNotFound {
		t.Fatalf("second forget: %v", err)
	}
}
