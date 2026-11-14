package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

func testObs(id string, at time.Time) apiv1.Observation {
	return apiv1.Observation{
		SchemaVersion:   7, // must be forced to 1
		EventID:         id,
		Provider:        apiv1.ProviderClaude,
		HostID:          "host-1",
		NativeSessionID: "native-1",
		EventType:       "turn.prompted",
		Source:          apiv1.ObservationSourceHook,
		ReceivedAt:      at,
		Data:            apiv1.ObservationData{NativeEvent: "UserPromptSubmit"},
	}
}

func TestHookOutboxIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := openStore(t)
	t0 := baseTime
	first, second := testObs("hook-1", t0), testObs("hook-2", t0.Add(time.Second))

	// Enqueue out of order; selection is by received_at.
	for _, o := range []apiv1.Observation{second, first} {
		ok, err := s.EnqueueObservation(ctx, o)
		if err != nil || !ok {
			t.Fatalf("enqueue %s = %v, %v", o.EventID, ok, err)
		}
	}
	if ok, err := s.EnqueueObservation(ctx, first); err != nil || ok {
		t.Fatalf("duplicate enqueue = %v, %v", ok, err)
	}
	if _, err := s.EnqueueObservation(ctx, apiv1.Observation{EventID: "x"}); err == nil ||
		err.Error() != "event ID, host ID, and provider are required" {
		t.Fatalf("validation = %v", err)
	}

	if got, err := s.PendingObservations(ctx, 0); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("limit 0 = %v, %v", got, err)
	}

	// Durable across reopen.
	_ = s.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.PendingObservations(ctx, 10)
	if err != nil || len(pending) != 2 || pending[0].EventID != "hook-1" || pending[1].EventID != "hook-2" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if pending[0].SchemaVersion != 1 {
		t.Fatalf("schema_version = %d", pending[0].SchemaVersion)
	}
	if one, _ := s.PendingObservations(ctx, 1); len(one) != 1 || one[0].EventID != "hook-1" {
		t.Fatalf("limit 1 = %+v", one)
	}

	var stored string
	_ = s.db.QueryRow(`SELECT received_at FROM hook_outbox WHERE id = 'hook-1'`).Scan(&stored)
	if stored != "2026-03-01T12:00:00.123456789Z" {
		t.Fatalf("stored timestamp = %q", stored)
	}

	if err := s.MarkObservationProcessed(ctx, "hook-1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkObservationProcessed(ctx, "hook-1", t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second processed = %v", err)
	}
	if err := s.MarkObservationProcessed(ctx, "nope", t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown processed = %v", err)
	}
	pending, _ = s.PendingObservations(ctx, 10)
	if len(pending) != 1 || pending[0].EventID != "hook-2" {
		t.Fatalf("after processed = %+v", pending)
	}
	var attempts int
	_ = s.db.QueryRow(`SELECT attempts FROM hook_outbox WHERE id = 'hook-1'`).Scan(&attempts)
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
	// Processed rows are retained and still dedupe.
	if ok, _ := s.EnqueueObservation(ctx, first); ok {
		t.Fatal("processed row must still dedupe")
	}

	if err := s.RecordObservation(ctx, first, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(ctx, first, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n)
	if n != 1 {
		t.Fatalf("observations rows = %d", n)
	}
}

func TestHookQuarantinesAfterFiveFailures(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if _, err := s.EnqueueObservation(ctx, testObs("hook-1", baseTime)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxHookAttempts; i++ {
		pending, _ := s.PendingObservations(ctx, 10)
		if len(pending) != 1 {
			t.Fatalf("attempt %d: pending = %d", i, len(pending))
		}
		if err := s.MarkObservationFailed(ctx, "hook-1", fmt.Errorf("boom %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ := s.PendingObservations(ctx, 10); len(pending) != 0 {
		t.Fatalf("quarantined row still pending: %+v", pending)
	}
	var attempts int
	var lastErr string
	_ = s.db.QueryRow(`SELECT attempts, last_error FROM hook_outbox WHERE id = 'hook-1'`).Scan(&attempts, &lastErr)
	if attempts != MaxHookAttempts || lastErr != "boom 5" {
		t.Fatalf("attempts=%d last_error=%q", attempts, lastErr)
	}
}

func TestMarkObservationFailedBoundsErrorText(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	_, _ = s.EnqueueObservation(ctx, testObs("hook-1", baseTime))
	if err := s.MarkObservationFailed(ctx, "hook-1", errors.New(strings.Repeat("x", 5000))); err != nil {
		t.Fatal(err)
	}
	var lastErr string
	_ = s.db.QueryRow(`SELECT last_error FROM hook_outbox WHERE id = 'hook-1'`).Scan(&lastErr)
	if len(lastErr) != 1024 {
		t.Fatalf("len = %d", len(lastErr))
	}
	if err := s.MarkObservationFailed(ctx, "hook-1", nil); err != nil {
		t.Fatal(err)
	}
	_ = s.db.QueryRow(`SELECT last_error FROM hook_outbox WHERE id = 'hook-1'`).Scan(&lastErr)
	if lastErr != "unknown processing error" {
		t.Fatalf("last_error = %q", lastErr)
	}
}

func TestListEventsFiltersOrdersAndRedactsPayloads(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	if err := s.Upsert(ctx, testAgent("agent-1", "native-1")); err != nil {
		t.Fatal(err)
	}
	const secret = "SECRET PROMPT TEXT"
	record := func(id string, at time.Time, mutate func(*apiv1.Observation)) {
		o := testObs(id, at)
		mutate(&o)
		if err := s.RecordObservation(ctx, o, at.Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	record("e1", baseTime, func(o *apiv1.Observation) {
		o.Data.InitialRequest = &apiv1.TextPreview{Text: secret, Source: "hook", ObservedAt: baseTime}
		o.Data.Summary = &apiv1.TextPreview{Text: secret, Source: "hook", ObservedAt: baseTime}
		o.Data.Reason = "because"
		o.Data.PreviousLiveState = apiv1.LiveStateUnknown
		o.Data.LiveState = apiv1.LiveStateLive
	})
	record("e2", baseTime.Add(time.Second), func(o *apiv1.Observation) {
		o.Error = "something failed"
		o.OccurredAt = &baseTime
	})
	record("e3", baseTime.Add(2*time.Second), func(o *apiv1.Observation) {
		o.Provider = apiv1.ProviderCodex
		o.NativeSessionID = "codex-native"
	})
	record("e4", baseTime.Add(3*time.Second), func(o *apiv1.Observation) { o.NativeSessionID = "" })

	all, err := s.ListEvents(ctx, registry.EventListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, e := range all {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "e1,e2,e3,e4" {
		t.Fatalf("order = %v", ids)
	}
	encoded, _ := json.Marshal(all)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("prompt text leaked: %s", encoded)
	}
	e1 := all[0]
	if e1.AgentID != "agent-1" || e1.NativeEvent != "UserPromptSubmit" || e1.Reason != "because" ||
		e1.PreviousLiveState != apiv1.LiveStateUnknown || e1.LiveState != apiv1.LiveStateLive ||
		e1.SchemaVersion != 1 || e1.ProcessedAt == nil || !e1.ReceivedAt.Equal(baseTime) {
		t.Fatalf("e1 = %+v", e1)
	}
	if all[1].Error != "something failed" || all[1].OccurredAt == nil || all[2].AgentID != "" || all[3].AgentID != "" {
		t.Fatalf("others = %+v", all[1:])
	}

	byAgent, _ := s.ListEvents(ctx, registry.EventListOptions{AgentID: "agent-1"})
	if len(byAgent) != 2 || byAgent[0].ID != "e1" || byAgent[1].ID != "e2" {
		t.Fatalf("by agent = %+v", byAgent)
	}
	byProvider, _ := s.ListEvents(ctx, registry.EventListOptions{Provider: apiv1.ProviderCodex})
	if len(byProvider) != 1 || byProvider[0].ID != "e3" {
		t.Fatalf("by provider = %+v", byProvider)
	}
	both, _ := s.ListEvents(ctx, registry.EventListOptions{AgentID: "agent-1", Provider: apiv1.ProviderCodex})
	if len(both) != 0 {
		t.Fatalf("both = %+v", both)
	}
	newest, _ := s.ListEvents(ctx, registry.EventListOptions{Limit: 1})
	if len(newest) != 1 || newest[0].ID != "e4" {
		t.Fatalf("limit 1 = %+v", newest)
	}
	last2, _ := s.ListEvents(ctx, registry.EventListOptions{Limit: 2})
	if len(last2) != 2 || last2[0].ID != "e3" || last2[1].ID != "e4" {
		t.Fatalf("limit 2 = %+v", last2)
	}
}

func TestListEventsClampsLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	for i := 0; i < 210; i++ {
		o := testObs(fmt.Sprintf("e%03d", i), baseTime.Add(time.Duration(i)*time.Second))
		if err := s.RecordObservation(ctx, o, baseTime); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ limit, want int }{{0, 50}, {-3, 50}, {5, 5}, {1000, 200}} {
		got, err := s.ListEvents(ctx, registry.EventListOptions{Limit: c.limit})
		if err != nil || len(got) != c.want {
			t.Fatalf("limit %d: got %d, %v; want %d", c.limit, len(got), err, c.want)
		}
		if got[len(got)-1].ID != "e209" {
			t.Fatalf("limit %d: newest = %s", c.limit, got[len(got)-1].ID)
		}
	}
}
