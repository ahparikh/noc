package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"noc.local/noc/internal/registry"
)

func TestTeamLeadAndParentAreStoredBesideTheSnapshot(t *testing.T) {
	ctx := context.Background()
	s, path := openStore(t)

	// A new row takes its team and parent from the agent, and is never a lead.
	spawned := testAgent("w", "s-w")
	spawned.Team, spawned.Lead, spawned.ParentID = "auth", true, "boss"
	if err := s.Upsert(ctx, spawned); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "w")
	if got.Team != "auth" || got.Lead || got.ParentID != "boss" {
		t.Fatalf("new row: %+v", got)
	}
	var snapshot string
	_ = s.db.QueryRow(`SELECT snapshot_json FROM agents WHERE id = 'w'`).Scan(&snapshot)
	if strings.Contains(snapshot, "auth") || strings.Contains(snapshot, "boss") {
		t.Fatalf("team and parent belong in their columns, not the snapshot: %s", snapshot)
	}

	for _, id := range []string{"a", "b"} {
		if err := s.Upsert(ctx, testAgent(id, "s-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetLead(ctx, "a"); !errors.Is(err, registry.ErrNoTeam) {
		t.Fatalf("lead without a team: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if agent, err := s.SetTeam(ctx, id, "auth"); err != nil || agent.Team != "auth" {
			t.Fatalf("SetTeam %s: %+v %v", id, agent, err)
		}
	}
	if agent, err := s.SetLead(ctx, "a"); err != nil || !agent.Lead {
		t.Fatalf("SetLead a: %+v %v", agent, err)
	}
	if agent, err := s.SetLead(ctx, "b"); err != nil || !agent.Lead {
		t.Fatalf("SetLead b: %+v %v", agent, err)
	}
	if a, _ := s.Get(ctx, "a"); a.Lead {
		t.Fatalf("one lead per team: %+v", a)
	}

	// An upsert of the whole agent, as hooks and reconciliation do, changes
	// none of it, whatever the incoming agent carries.
	incoming := testAgent("b", "s-b")
	incoming.Team, incoming.ParentID = "other", "someone"
	if err := s.Upsert(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Get(ctx, "b"); b.Team != "auth" || !b.Lead || b.ParentID != "" {
		t.Fatalf("upsert changed the overlay: %+v", b)
	}

	// Same team keeps the lead; another team or none drops it.
	if b, _ := s.SetTeam(ctx, "b", "auth"); !b.Lead {
		t.Fatalf("same team: %+v", b)
	}
	if b, _ := s.SetTeam(ctx, "b", "billing"); b.Lead || b.Team != "billing" {
		t.Fatalf("moved: %+v", b)
	}
	if _, err := s.SetLead(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.SetTeam(ctx, "b", ""); b.Lead || b.Team != "" {
		t.Fatalf("cleared: %+v", b)
	}
	if a, err := s.SetParent(ctx, "a", "w"); err != nil || a.ParentID != "w" {
		t.Fatalf("SetParent: %+v %v", a, err)
	}
	for _, call := range []func() error{
		func() error { _, err := s.SetTeam(ctx, "missing", "auth"); return err },
		func() error { _, err := s.SetLead(ctx, "missing"); return err },
		func() error { _, err := s.SetParent(ctx, "missing", "a"); return err },
	} {
		if err := call(); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("missing agent: %v", err)
		}
	}

	// It all survives a restart.
	if _, err := s.SetLead(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	a, _ := again.Get(ctx, "a")
	w, _ := again.Get(ctx, "w")
	if a.Team != "auth" || !a.Lead || a.ParentID != "w" || w.Team != "auth" || w.ParentID != "boss" {
		t.Fatalf("after restart: a=%+v w=%+v", a, w)
	}
	list, _ := again.List(ctx, registry.ListOptions{IncludeNotLive: true})
	leads := 0
	for _, agent := range list {
		if agent.Lead {
			leads++
		}
	}
	if leads != 1 {
		t.Fatalf("leads in list = %d", leads)
	}
}
