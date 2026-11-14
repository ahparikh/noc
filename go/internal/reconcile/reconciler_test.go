package reconcile

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

type fakeAdapter struct {
	provider  apiv1.Provider
	entries   []provider.NativeAgent
	discover  error
	normalize map[string]provider.NormalizedAgent
	badNorm   map[string]bool
}

func newFake() *fakeAdapter {
	return &fakeAdapter{provider: apiv1.ProviderClaude, normalize: map[string]provider.NormalizedAgent{}, badNorm: map[string]bool{}}
}

func (f *fakeAdapter) Provider() apiv1.Provider                   { return f.provider }
func (f *fakeAdapter) Probe(context.Context) provider.ProbeResult { return provider.ProbeResult{} }
func (f *fakeAdapter) DiscoverLive(context.Context) ([]provider.NativeAgent, error) {
	return f.entries, f.discover
}
func (f *fakeAdapter) Normalize(a provider.NativeAgent) (provider.NormalizedAgent, error) {
	if f.badNorm[a.NativeSessionID] {
		return provider.NormalizedAgent{}, errors.New("bad row")
	}
	n, ok := f.normalize[a.NativeSessionID]
	if !ok {
		n = provider.NormalizedAgent{Provider: f.provider, NativeSessionID: a.NativeSessionID}
	}
	return n, nil
}

// set makes the snapshot contain exactly the given normalized agents.
func (f *fakeAdapter) set(ns ...provider.NormalizedAgent) {
	f.entries = nil
	for _, n := range ns {
		f.entries = append(f.entries, provider.NativeAgent{NativeSessionID: n.NativeSessionID})
		f.normalize[n.NativeSessionID] = n
	}
}

type journal struct {
	mu   sync.Mutex
	obs  []apiv1.Observation
	fail error
}

func (j *journal) record(_ context.Context, o apiv1.Observation, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail != nil {
		return j.fail
	}
	if !o.ReceivedAt.Equal(at) {
		panic("processedAt must equal event time")
	}
	j.obs = append(j.obs, o)
	return nil
}

func (j *journal) types() []string {
	var out []string
	for _, o := range j.obs {
		out = append(out, o.EventType)
	}
	return out
}

type harness struct {
	t     *testing.T
	store *registry.Memory
	ad    *fakeAdapter
	j     *journal
	clock time.Time
	ids   int
	r     *Reconciler
	alive func(int) bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, store: registry.NewMemory(), ad: newFake(), j: &journal{}, clock: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	h.alive = func(int) bool { return true }
	r, err := New(Config{
		HostID: "host-1", Store: h.store, Adapter: h.ad, StaleAfter: 30 * time.Second,
		Now:               func() time.Time { return h.clock },
		NewID:             func() (string, error) { h.ids++; return "agent-" + string(rune('0'+h.ids)), nil },
		RecordObservation: h.j.record,
		IsRunning:         func(pid int) bool { return h.alive(pid) },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	return h
}

func (h *harness) run() Report {
	h.t.Helper()
	rep, err := h.r.RunOnce(context.Background())
	if err != nil {
		h.t.Fatalf("RunOnce: %v", err)
	}
	return rep
}

func (h *harness) get(id string) apiv1.Agent {
	h.t.Helper()
	a, err := h.store.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) byNative(native string) apiv1.Agent {
	h.t.Helper()
	a, err := h.store.GetByNativeIdentity(context.Background(), "host-1", apiv1.ProviderClaude, native)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func norm(id string) provider.NormalizedAgent {
	return provider.NormalizedAgent{Provider: apiv1.ProviderClaude, NativeSessionID: id}
}

func TestNewValidates(t *testing.T) {
	m := registry.NewMemory()
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{Store: m, Adapter: newFake()}, "reconciler host ID is required"},
		{Config{HostID: "h", Adapter: newFake()}, "reconciler store is required"},
		{Config{HostID: "h", Store: m}, "reconciler adapter is required"},
	}
	for _, c := range cases {
		if _, err := New(c.cfg); err == nil || err.Error() != c.want {
			t.Errorf("New(%+v) err = %v, want %q", c.cfg, err, c.want)
		}
	}
	r, err := New(Config{HostID: "h", Store: m, Adapter: newFake()})
	if err != nil || r.staleAfter != 30*time.Second || r.now == nil || r.newID == nil || r.inspector == nil || r.isRunning == nil {
		t.Fatalf("defaults not applied: %+v %v", r, err)
	}
}

func TestReconcilerMaintainsIdentityAndLiveness(t *testing.T) {
	h := newHarness(t)
	start := h.clock
	h.ad.set(norm("s1"))

	rep := h.run()
	if rep != (Report{Observed: 1, Upserted: 1}) {
		t.Fatalf("report = %+v", rep)
	}
	a := h.byNative("s1")
	if a.ID != "agent-1" || a.LiveState != apiv1.LiveStateLive || a.Origin != "reconcile" ||
		!a.FirstSeenAt.Equal(start) || !a.LastSeenAt.Equal(start) || a.SnapshotConfirmedAt == nil || !a.SnapshotConfirmedAt.Equal(start) {
		t.Fatalf("discovered = %+v", a)
	}

	// Steady state records nothing.
	h.clock = start.Add(5 * time.Second)
	h.run()
	if got := h.j.types(); len(got) != 1 {
		t.Fatalf("steady events = %v", got)
	}

	// First absent pass: candidate_stale, last_seen unchanged.
	h.ad.set()
	h.clock = start.Add(10 * time.Second)
	rep = h.run()
	if rep.CandidateStale != 1 || rep.MarkedNotLive != 0 {
		t.Fatalf("report = %+v", rep)
	}
	a = h.get("agent-1")
	if a.LiveState != apiv1.LiveStateCandidateStale || !a.LastSeenAt.Equal(start.Add(5*time.Second)) {
		t.Fatalf("stale = %+v", a)
	}

	// Below grace: unchanged.
	h.clock = start.Add(30 * time.Second)
	rep = h.run()
	if rep.CandidateStale != 0 || rep.MarkedNotLive != 0 || h.get("agent-1").LiveState != apiv1.LiveStateCandidateStale {
		t.Fatalf("below grace changed: %+v", rep)
	}

	// At grace: not_live with ended_at.
	h.clock = start.Add(35 * time.Second)
	rep = h.run()
	a = h.get("agent-1")
	if rep.MarkedNotLive != 1 || a.LiveState != apiv1.LiveStateNotLive || a.EndedAt == nil || !a.EndedAt.Equal(h.clock) {
		t.Fatalf("not_live: %+v %+v", rep, a)
	}

	// Further absent passes do nothing.
	h.clock = start.Add(40 * time.Second)
	if rep = h.run(); rep.CandidateStale+rep.MarkedNotLive != 0 {
		t.Fatalf("report = %+v", rep)
	}

	// Revival keeps id and first_seen_at, clears ended_at.
	h.ad.set(norm("s1"))
	h.clock = start.Add(45 * time.Second)
	h.run()
	a = h.get("agent-1")
	if a.LiveState != apiv1.LiveStateLive || a.EndedAt != nil || !a.FirstSeenAt.Equal(start) || !a.LastSeenAt.Equal(h.clock) {
		t.Fatalf("revived = %+v", a)
	}

	want := []string{eventAgentDiscovered, eventAgentCandidateStale, eventAgentNotLive, eventAgentRevived}
	got := h.j.types()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	wantPrev := []apiv1.LiveState{"", apiv1.LiveStateLive, apiv1.LiveStateCandidateStale, apiv1.LiveStateNotLive}
	wantReason := []string{reasonObserved, reasonMissing, reasonStaleGrace, reasonReappeared}
	idRE := regexp.MustCompile(`^snapshot-[0-9a-f]{64}$`)
	for i, o := range h.j.obs {
		if o.Source != apiv1.ObservationSourceSnapshot || o.SchemaVersion != 1 || o.Provider != apiv1.ProviderClaude || o.HostID != "host-1" || o.NativeSessionID != "s1" {
			t.Errorf("event %d envelope = %+v", i, o)
		}
		if !idRE.MatchString(o.EventID) {
			t.Errorf("event %d id = %q", i, o.EventID)
		}
		if o.Data.PreviousLiveState != wantPrev[i] || o.Data.Reason != wantReason[i] || o.Data.LiveState == "" {
			t.Errorf("event %d data = %+v", i, o.Data)
		}
		if o.OccurredAt == nil || !o.OccurredAt.Equal(o.ReceivedAt) {
			t.Errorf("event %d times = %v %v", i, o.OccurredAt, o.ReceivedAt)
		}
	}
}

func TestDiscoveryFailureDoesNotAgeAgents(t *testing.T) {
	h := newHarness(t)
	h.ad.set(norm("s1"))
	h.run()
	before := h.get("agent-1")
	events := len(h.j.obs)

	h.ad.discover = errors.New("boom")
	h.clock = h.clock.Add(time.Hour)
	rep, err := h.r.RunOnce(context.Background())
	if rep != (Report{}) || err == nil || err.Error() != "discover claude agents: boom" {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	after := h.get("agent-1")
	if after.LiveState != before.LiveState || !after.LastSeenAt.Equal(before.LastSeenAt) || len(h.j.obs) != events {
		t.Fatalf("state changed: %+v events %d", after, len(h.j.obs))
	}
}

func TestReconcilerSkipsDuplicatesAndIncompatibleRows(t *testing.T) {
	h := newHarness(t)
	h.ad.entries = []provider.NativeAgent{
		{NativeSessionID: "ok"}, {NativeSessionID: "ok"}, {NativeSessionID: "bad"},
		{NativeSessionID: ""}, {NativeSessionID: "other"},
	}
	h.ad.badNorm["bad"] = true
	h.ad.normalize["other"] = provider.NormalizedAgent{Provider: apiv1.ProviderCodex, NativeSessionID: "other"}
	h.ad.normalize[""] = provider.NormalizedAgent{Provider: apiv1.ProviderClaude}
	rep := h.run()
	if rep != (Report{Observed: 5, Upserted: 1, Skipped: 4}) {
		t.Fatalf("report = %+v", rep)
	}
	list, _ := h.store.List(context.Background(), registry.ListOptions{IncludeNotLive: true})
	if len(list) != 1 {
		t.Fatalf("rows = %d", len(list))
	}
}

func TestReconcilerSkippedEntriesAgeTheirRows(t *testing.T) {
	h := newHarness(t)
	h.ad.set(norm("s1"))
	h.run()
	h.ad.badNorm["s1"] = true
	h.clock = h.clock.Add(5 * time.Second)
	rep := h.run()
	if rep.Skipped != 1 || rep.CandidateStale != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func hookRow(id, native string, pid int, live apiv1.LiveState) apiv1.Agent {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return apiv1.Agent{
		ID: id, HostID: "host-1", Provider: apiv1.ProviderClaude, NativeSessionID: native, Origin: "hook",
		LiveState: live, RuntimeState: apiv1.RuntimeStateRunning, FirstSeenAt: t0, LastSeenAt: t0, ProcessID: pid,
	}
}

func TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage(t *testing.T) {
	h := newHarness(t)
	obs := h.clock.Add(-time.Hour)
	row := hookRow("hook-1", "s1", 4242, apiv1.LiveStateLive)
	row.Name = "from hook"
	row.InitialRequest = &apiv1.TextPreview{Text: "hello", Source: "hook", ObservedAt: obs}
	row.FrontendBindings = []apiv1.FrontendBinding{{Kind: "zellij", Session: "s", PaneID: "1", State: "bound", ObservedAt: obs}}
	row.Model, row.ModelSource, row.ModelObservedAt = "m", "hook", &obs
	_ = h.store.Upsert(context.Background(), row)

	h.ad.set(norm("s1"))
	rep := h.run()
	if rep.Upserted != 1 || rep.MarkedNotLive != 0 {
		t.Fatalf("report = %+v", rep)
	}
	a := h.get("hook-1")
	if a.Origin != "hook" || a.SnapshotConfirmedAt == nil || a.InitialRequest == nil || a.InitialRequest.Text != "hello" || a.InitialRequest.Source != "hook" ||
		a.Name != "from hook" || len(a.FrontendBindings) != 1 || a.Model != "m" || a.ModelSource != "hook" || !a.ModelObservedAt.Equal(obs) {
		t.Fatalf("merged = %+v", a)
	}
	if got := h.j.types(); len(got) != 1 || got[0] != eventAgentSnapshotConfirmed {
		t.Fatalf("events = %v", got)
	}
	// Now snapshot-covered: absence ages it.
	h.ad.set()
	h.clock = h.clock.Add(5 * time.Second)
	if rep := h.run(); rep.CandidateStale != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestReconcilerUsesProviderMetadataOverHookFallback(t *testing.T) {
	h := newHarness(t)
	old := h.clock.Add(-time.Hour)
	row := hookRow("hook-1", "s1", 0, apiv1.LiveStateLive)
	row.Model, row.ModelSource, row.ModelObservedAt = "hook-model", "hook", &old
	row.ReasoningEffort, row.ReasoningSource, row.ReasoningObservedAt = "low", "hook", &old
	row.InitialRequest = &apiv1.TextPreview{Text: "hook text", Source: "hook", ObservedAt: old}
	row.Summary, row.SummarySource, row.SummaryUpdatedAt, row.SummaryTruncated = "hook sum", "hook", &old, true
	_ = h.store.Upsert(context.Background(), row)

	n := norm("s1")
	n.Model, n.ReasoningEffort = "snap-model", "high"
	n.InitialRequest = &apiv1.TextPreview{Text: "snap text"}
	n.Summary = "snap sum"
	h.ad.set(n)
	h.run()
	a := h.get("hook-1")
	now := h.clock
	if a.Model != "snap-model" || a.ModelSource != "provider_snapshot" || !a.ModelObservedAt.Equal(now) ||
		a.ReasoningEffort != "high" || a.ReasoningSource != "provider_snapshot" || !a.ReasoningObservedAt.Equal(now) ||
		a.InitialRequest.Text != "snap text" || a.InitialRequest.Source != "provider_snapshot" || !a.InitialRequest.ObservedAt.Equal(now) ||
		a.Summary != "snap sum" || a.SummarySource != "provider_snapshot" || !a.SummaryUpdatedAt.Equal(now) || a.SummaryTruncated {
		t.Fatalf("agent = %+v", a)
	}
}

func TestReconcilerRetiresHookOnlyAgentWhoseProcessExited(t *testing.T) {
	h := newHarness(t)
	_ = h.store.Upsert(context.Background(), hookRow("hook-1", "gone", 777, apiv1.LiveStateLive))
	_ = h.store.Upsert(context.Background(), hookRow("hook-2", "alive", 888, apiv1.LiveStateLive))
	_ = h.store.Upsert(context.Background(), hookRow("hook-3", "nopid", 0, apiv1.LiveStateLive))
	h.alive = func(pid int) bool { return pid != 777 }

	rep := h.run()
	if rep.MarkedNotLive != 1 {
		t.Fatalf("report = %+v", rep)
	}
	a := h.get("hook-1")
	if a.LiveState != apiv1.LiveStateNotLive || a.RuntimeState != apiv1.RuntimeStateStopped || a.EndedAt == nil || !a.EndedAt.Equal(h.clock) {
		t.Fatalf("retired = %+v", a)
	}
	for _, id := range []string{"hook-2", "hook-3"} {
		if h.get(id).LiveState != apiv1.LiveStateLive {
			t.Errorf("%s changed", id)
		}
	}
	if len(h.j.obs) != 1 || h.j.obs[0].EventType != eventAgentNotLive || h.j.obs[0].Data.Reason != reasonProcessExited || h.j.obs[0].Data.PreviousLiveState != apiv1.LiveStateLive {
		t.Fatalf("events = %+v", h.j.obs)
	}
}

// Rows already not_live must not be re-retired every pass.
func TestReconcilerDoesNotRemarkEndedHookOnlyRows(t *testing.T) {
	h := newHarness(t)
	ended := h.clock.Add(-time.Minute)
	row := hookRow("hook-1", "done", 777, apiv1.LiveStateNotLive)
	row.EndedAt = &ended
	_ = h.store.Upsert(context.Background(), row)
	h.alive = func(int) bool { return false }

	for i := 0; i < 3; i++ {
		h.clock = h.clock.Add(5 * time.Second)
		if rep := h.run(); rep.MarkedNotLive != 0 {
			t.Fatalf("pass %d report = %+v", i, rep)
		}
	}
	if len(h.j.obs) != 0 || !h.get("hook-1").EndedAt.Equal(ended) {
		t.Fatalf("row re-marked: events=%v ended=%v", h.j.types(), h.get("hook-1").EndedAt)
	}
}

func TestReconcilerPlaceholderNameNotCarriedForward(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"abc123", " s1 "} {
		row := hookRow("hook-1", "s1", 0, apiv1.LiveStateLive)
		row.NativeShortID, row.Name = "abc123", name
		_ = h.store.Upsert(context.Background(), row)
		h.ad.set(norm("s1"))
		h.run()
		if got := h.get("hook-1").Name; got != "" {
			t.Errorf("placeholder %q carried as %q", name, got)
		}
	}
	row := hookRow("hook-1", "s1", 0, apiv1.LiveStateLive)
	row.Name = "real name"
	_ = h.store.Upsert(context.Background(), row)
	h.run()
	if got := h.get("hook-1").Name; got != "real name" {
		t.Errorf("name = %q", got)
	}
	n := norm("s1")
	n.Name = "new"
	h.ad.set(n)
	h.run()
	if got := h.get("hook-1").Name; got != "new" {
		t.Errorf("name = %q", got)
	}
}

func TestReconcilerAttentionSinceAndActivity(t *testing.T) {
	h := newHarness(t)
	start := h.clock
	n := norm("s1")
	n.Attention = apiv1.Attention{Type: apiv1.AttentionApprovalRequired, Detail: "rm"}
	n.ActivityState = apiv1.ActivityStateWaiting
	lastAct := start.Add(-time.Minute)
	n.LastActivityAt = &lastAct
	h.ad.set(n)
	h.run()
	a := h.byNative("s1")
	if a.Attention.Since == nil || !a.Attention.Since.Equal(start) || a.LastActivityAt == nil || !a.LastActivityAt.Equal(lastAct) {
		t.Fatalf("first = %+v", a)
	}

	h.clock = start.Add(5 * time.Second)
	h.run()
	if a = h.byNative("s1"); !a.Attention.Since.Equal(start) {
		t.Fatalf("since not preserved: %v", a.Attention.Since)
	}

	n.Attention.Detail = "other"
	h.ad.set(n)
	h.clock = start.Add(10 * time.Second)
	h.run()
	if a = h.byNative("s1"); !a.Attention.Since.Equal(h.clock) {
		t.Fatalf("since not reset on detail change: %v", a.Attention.Since)
	}

	n.Attention = apiv1.Attention{}
	n.ActivityState = apiv1.ActivityStateWorking
	n.LastActivityAt = nil
	h.ad.set(n)
	h.clock = start.Add(15 * time.Second)
	h.run()
	a = h.byNative("s1")
	if a.Attention.Type != apiv1.AttentionNone || a.Attention.Since != nil || a.LastActivityAt == nil || !a.LastActivityAt.Equal(h.clock) {
		t.Fatalf("working = %+v", a)
	}

	n.ActivityState = apiv1.ActivityStateIdle
	h.ad.set(n)
	h.clock = start.Add(20 * time.Second)
	h.run()
	if a = h.byNative("s1"); !a.LastActivityAt.Equal(start.Add(15 * time.Second)) {
		t.Fatalf("existing activity not kept: %v", a.LastActivityAt)
	}
}

func TestReconcilerDropsProcessID(t *testing.T) {
	h := newHarness(t)
	_ = h.store.Upsert(context.Background(), hookRow("hook-1", "s1", 4242, apiv1.LiveStateLive))
	h.ad.set(norm("s1"))
	h.run()
	if pid := h.get("hook-1").ProcessID; pid != 0 {
		t.Fatalf("process_id = %d", pid)
	}
}

func TestReconcilerFailuresAbortPass(t *testing.T) {
	h := newHarness(t)
	h.ad.set(norm("s1"))
	h.j.fail = errors.New("journal down")
	_, err := h.r.RunOnce(context.Background())
	if err == nil || err.Error() != "record agent.discovered transition: journal down" {
		t.Fatalf("err = %v", err)
	}
	h2 := newHarness(t)
	h2.ad.set(norm("s1"))
	h2.r.newID = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := h2.r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "allocate noc agent ID: no entropy") {
		t.Fatalf("err = %v", err)
	}
}

func TestReconcilerNilRecorderAndOtherProviderRowsUntouched(t *testing.T) {
	h := newHarness(t)
	h.r.record = nil
	other := hookRow("codex-1", "c1", 0, apiv1.LiveStateLive)
	other.Provider = apiv1.ProviderCodex
	other.SnapshotConfirmedAt = &h.clock
	_ = h.store.Upsert(context.Background(), other)
	foreign := hookRow("foreign", "f1", 0, apiv1.LiveStateLive)
	foreign.HostID = "other-host"
	foreign.SnapshotConfirmedAt = &h.clock
	_ = h.store.Upsert(context.Background(), foreign)
	h.ad.set(norm("s1"))
	if rep := h.run(); rep.CandidateStale != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if h.get("codex-1").LiveState != apiv1.LiveStateLive || h.get("foreign").LiveState != apiv1.LiveStateLive {
		t.Fatal("foreign rows aged")
	}
}
