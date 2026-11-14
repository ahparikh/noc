package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// outboxStore adds an in-memory outbox and journal to registry.Memory.
type outboxStore struct {
	*registry.Memory
	mu        sync.Mutex
	pending   []apiv1.Observation
	processed map[string]time.Time
	failures  map[string][]string
	journal   map[string]apiv1.Observation
	order     []string
	failMark  map[string]bool
}

func newOutboxStore() *outboxStore {
	return &outboxStore{
		Memory:    registry.NewMemory(),
		processed: map[string]time.Time{},
		failures:  map[string][]string{},
		journal:   map[string]apiv1.Observation{},
		failMark:  map[string]bool{},
	}
}

const maxAttempts = 5

func (s *outboxStore) EnqueueObservation(_ context.Context, obs apiv1.Observation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.pending {
		if existing.EventID == obs.EventID {
			return false, nil
		}
	}
	s.pending = append(s.pending, obs)
	return true, nil
}

func (s *outboxStore) PendingObservations(_ context.Context, limit int) ([]apiv1.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []apiv1.Observation
	for _, obs := range s.pending {
		if _, done := s.processed[obs.EventID]; done || len(s.failures[obs.EventID]) >= maxAttempts {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, obs)
	}
	return out, nil
}

func (s *outboxStore) MarkObservationProcessed(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failMark[id] {
		return errors.New("mark processed boom")
	}
	s.processed[id] = at
	return nil
}

func (s *outboxStore) MarkObservationFailed(_ context.Context, id string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[id] = append(s.failures[id], err.Error())
	return nil
}

func (s *outboxStore) RecordObservation(_ context.Context, obs apiv1.Observation, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.journal[obs.EventID]; !ok {
		s.journal[obs.EventID] = obs
		s.order = append(s.order, obs.EventID)
	}
	return nil
}

var base = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type harness struct {
	t     *testing.T
	store *outboxStore
	proc  *Processor
	now   time.Time
	ids   int
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, store: newOutboxStore(), now: base.Add(time.Hour)}
	proc, err := NewProcessor(Config{
		HostID: "HOST",
		Store:  h.store,
		Now:    func() time.Time { return h.now },
		NewID: func() (string, error) {
			h.ids++
			return fmt.Sprintf("agent-%d", h.ids), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.proc = proc
	return h
}

// send normalizes a payload at base+offset and spools it.
func (h *harness) send(provider apiv1.Provider, offset time.Duration, payload string, env Environment) apiv1.Observation {
	h.t.Helper()
	obs, err := Normalize(provider, "HOST", []byte(payload), base.Add(offset), env)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.store.EnqueueObservation(context.Background(), obs); err != nil {
		h.t.Fatal(err)
	}
	return obs
}

func (h *harness) drain() DrainReport {
	h.t.Helper()
	report, err := h.proc.Drain(context.Background(), 0)
	if err != nil {
		h.t.Fatalf("Drain: %v", err)
	}
	return report
}

func (h *harness) agent(session string) apiv1.Agent {
	h.t.Helper()
	agent, err := h.store.GetByNativeIdentity(context.Background(), "HOST", apiv1.ProviderClaude, session)
	if err != nil {
		h.t.Fatalf("agent %s: %v", session, err)
	}
	return agent
}

func TestNewProcessorValidates(t *testing.T) {
	if _, err := NewProcessor(Config{Store: newOutboxStore()}); err == nil || err.Error() != "hook processor host ID is required" {
		t.Fatalf("host err = %v", err)
	}
	if _, err := NewProcessor(Config{HostID: "H"}); err == nil || err.Error() != "hook processor store is required" {
		t.Fatalf("store err = %v", err)
	}
	p, err := NewProcessor(Config{HostID: "H", Store: newOutboxStore()})
	if err != nil || p.inspector == nil || p.now == nil || p.newID == nil {
		t.Fatalf("defaults missing: %v", err)
	}
}

func TestProcessorBuildsAgentPreviewFromHookSequence(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	zellij := Environment{ZellijSession: "work", ZellijPaneID: "4", ProcessID: 4242}

	h.send(apiv1.ProviderClaude, 0, `{"session_id":"s1","hook_event_name":"SessionStart","cwd":"`+dir+`","model":"m1","session_title":"Parser work"}`, zellij)
	h.send(apiv1.ProviderClaude, 1*time.Second, `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt_id":"p1","prompt":"  Fix   the\n flaky test "}`, zellij)
	if r := h.drain(); r.Pending != 2 || r.Processed != 2 || r.Failed != 0 {
		t.Fatalf("report = %+v", r)
	}
	agent := h.agent("s1")
	if agent.ID != "agent-1" || agent.Origin != "hook" || agent.Name != "Parser work" ||
		agent.RuntimeState != apiv1.RuntimeStateRunning || agent.ActivityState != apiv1.ActivityStateWorking ||
		agent.LiveState != apiv1.LiveStateLive || agent.DisplayState != apiv1.DisplayStateWorking {
		t.Fatalf("working agent = %+v", agent)
	}
	if agent.InitialRequest == nil || agent.InitialRequest.Text != "Fix the flaky test" ||
		!agent.InitialRequest.ObservedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("initial request = %+v", agent.InitialRequest)
	}
	if agent.Model != "m1" || agent.ModelSource != "hook" || agent.ProcessID != 4242 {
		t.Fatalf("model/process = %q %q %d", agent.Model, agent.ModelSource, agent.ProcessID)
	}
	if agent.Workspace == nil || agent.Workspace.Kind != "directory" || agent.Workspace.Owner != "unknown" {
		t.Fatalf("workspace = %+v", agent.Workspace)
	}
	if !agent.FirstSeenAt.Equal(base) || !agent.LastSeenAt.Equal(base.Add(time.Second)) ||
		agent.LastActivityAt == nil || !agent.LastActivityAt.Equal(base.Add(time.Second)) {
		t.Fatalf("times = %v %v %v", agent.FirstSeenAt, agent.LastSeenAt, agent.LastActivityAt)
	}
	if len(agent.FrontendBindings) != 1 || agent.FrontendBindings[0].Session != "work" || agent.FrontendBindings[0].State != "observed" {
		t.Fatalf("bindings = %+v", agent.FrontendBindings)
	}

	// A later prompt does not replace the first; Stop captures a summary; no
	// Zellij env keeps the earlier binding.
	h.send(apiv1.ProviderClaude, 2*time.Second, `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt_id":"p2","prompt":"Something else"}`, Environment{})
	h.send(apiv1.ProviderClaude, 3*time.Second, `{"session_id":"s1","hook_event_name":"Stop","turn_id":"t1","last_assistant_message":"Done.\n\nAll good."}`, Environment{})
	h.drain()
	agent = h.agent("s1")
	if agent.InitialRequest.Text != "Fix the flaky test" {
		t.Fatalf("initial request replaced: %+v", agent.InitialRequest)
	}
	if agent.Summary != "Done. All good." || agent.SummarySource != "hook_last_assistant_message" ||
		agent.SummaryUpdatedAt == nil || !agent.SummaryUpdatedAt.Equal(base.Add(3*time.Second)) {
		t.Fatalf("summary = %q %q", agent.Summary, agent.SummarySource)
	}
	if agent.ActivityState != apiv1.ActivityStateIdle || agent.DisplayState != apiv1.DisplayStateReady {
		t.Fatalf("after stop: %s/%s", agent.ActivityState, agent.DisplayState)
	}
	if len(agent.FrontendBindings) != 1 || !agent.FrontendBindings[0].ObservedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("binding after later events = %+v", agent.FrontendBindings)
	}

	// Journal annotation: first event has no previous live state.
	var first apiv1.Observation
	for _, id := range h.store.order {
		if h.store.journal[id].Data.NativeEvent == "SessionStart" {
			first = h.store.journal[id]
		}
	}
	if first.Data.PreviousLiveState != "" || first.Data.LiveState != apiv1.LiveStateLive {
		t.Fatalf("first journal = %+v", first.Data)
	}
	last := h.store.journal[h.store.order[len(h.store.order)-1]]
	if last.Data.PreviousLiveState != apiv1.LiveStateLive || last.Data.LiveState != apiv1.LiveStateLive {
		t.Fatalf("last journal = %+v", last.Data)
	}
	if len(h.store.processed) != 4 {
		t.Fatalf("processed = %d", len(h.store.processed))
	}
}

func TestProcessorEndsHookOnlyAgentOnSessionEnd(t *testing.T) {
	h := newHarness(t)
	env := Environment{ZellijSession: "work", ZellijPaneID: "4"}
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`, env)
	h.send(apiv1.ProviderClaude, time.Second, `{"session_id":"s1","hook_event_name":"PermissionRequest","message":"ok?"}`, env)
	h.drain()
	if a := h.agent("s1"); a.Attention.Type != apiv1.AttentionApprovalRequired || a.ActivityState != apiv1.ActivityStateWaiting {
		t.Fatalf("approval agent = %+v", a)
	}
	h.send(apiv1.ProviderClaude, 2*time.Second, `{"session_id":"s1","hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`, Environment{})
	h.drain()
	agent := h.agent("s1")
	if agent.LiveState != apiv1.LiveStateNotLive || agent.RuntimeState != apiv1.RuntimeStateStopped ||
		agent.ActivityState != apiv1.ActivityStateIdle || agent.Attention.Type != apiv1.AttentionNone ||
		agent.EndedAt == nil || !agent.EndedAt.Equal(base.Add(2*time.Second)) || agent.DisplayState != apiv1.DisplayStateNotLive {
		t.Fatalf("ended agent = %+v", agent)
	}
	if len(agent.FrontendBindings) != 1 || agent.FrontendBindings[0].State != "disconnected" ||
		!agent.FrontendBindings[0].ObservedAt.Equal(base.Add(2*time.Second)) {
		t.Fatalf("bindings = %+v", agent.FrontendBindings)
	}
	// Any later hook revives it.
	h.send(apiv1.ProviderClaude, 3*time.Second, `{"session_id":"s1","hook_event_name":"Whatever"}`, Environment{})
	h.drain()
	agent = h.agent("s1")
	if agent.LiveState != apiv1.LiveStateLive || agent.EndedAt != nil {
		t.Fatalf("revived agent = %+v", agent)
	}
}

func TestProcessorDropsASessionThatEndsWithoutAConversation(t *testing.T) {
	h := newHarness(t)
	// Never existed: no row created, journal still written.
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"ghost","hook_event_name":"SessionEnd"}`, Environment{})
	h.drain()
	if _, err := h.store.GetByNativeIdentity(context.Background(), "HOST", apiv1.ProviderClaude, "ghost"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("ghost row exists: %v", err)
	}
	if len(h.store.journal) != 1 {
		t.Fatalf("journal = %d", len(h.store.journal))
	}

	// Existed (started, never engaged) then ended: deleted.
	h.send(apiv1.ProviderClaude, time.Second, `{"session_id":"quiet","hook_event_name":"SessionStart"}`, Environment{})
	h.drain()
	h.agent("quiet")
	h.send(apiv1.ProviderClaude, 2*time.Second, `{"session_id":"quiet","hook_event_name":"SessionEnd"}`, Environment{})
	h.drain()
	if _, err := h.store.GetByNativeIdentity(context.Background(), "HOST", apiv1.ProviderClaude, "quiet"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("quiet row survived: %v", err)
	}

	// A provider name keeps the row; a prompt keeps the row.
	h.send(apiv1.ProviderClaude, 3*time.Second, `{"session_id":"named","hook_event_name":"SessionStart"}`, Environment{})
	h.send(apiv1.ProviderClaude, 4*time.Second, `{"session_id":"named","hook_event_name":"SessionEnd","session_title":"Keep me"}`, Environment{})
	h.send(apiv1.ProviderClaude, 5*time.Second, `{"session_id":"prompted","hook_event_name":"UserPromptSubmit","prompt":"hi"}`, Environment{})
	h.send(apiv1.ProviderClaude, 6*time.Second, `{"session_id":"prompted","hook_event_name":"SessionEnd"}`, Environment{})
	h.drain()
	if a := h.agent("named"); a.LiveState != apiv1.LiveStateNotLive {
		t.Fatalf("named = %+v", a)
	}
	if a := h.agent("prompted"); a.LiveState != apiv1.LiveStateNotLive {
		t.Fatalf("prompted = %+v", a)
	}
}

func TestProcessorUpsertsEndedUnengagedAgentWithoutForgetter(t *testing.T) {
	store := newOutboxStore()
	wrapped := struct {
		registry.Store
		registry.ObservationStore
	}{store, store}
	proc, err := NewProcessor(Config{HostID: "HOST", Store: wrapped, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	for i, payload := range []string{
		`{"session_id":"s","hook_event_name":"SessionStart"}`,
		`{"session_id":"s","hook_event_name":"SessionEnd"}`,
	} {
		obs, _ := Normalize(apiv1.ProviderClaude, "HOST", []byte(payload), base.Add(time.Duration(i)*time.Second), Environment{})
		_, _ = store.EnqueueObservation(context.Background(), obs)
	}
	if _, err := proc.Drain(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	agent, err := store.GetByNativeIdentity(context.Background(), "HOST", apiv1.ProviderClaude, "s")
	if err != nil || agent.LiveState != apiv1.LiveStateNotLive {
		t.Fatalf("expected upserted not_live row, got %+v / %v", agent, err)
	}
}

func TestProcessorAppliesAttentionRules(t *testing.T) {
	h := newHarness(t)
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"go"}`, Environment{})
	h.send(apiv1.ProviderClaude, 1*time.Second, `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt","message":"allow Bash?"}`, Environment{})
	h.drain()
	a := h.agent("s")
	since := a.Attention.Since
	if a.Attention.Type != apiv1.AttentionApprovalRequired || a.Attention.Detail != "allow Bash?" || a.ActivityState != apiv1.ActivityStateWaiting ||
		a.RuntimeState != apiv1.RuntimeStateRunning || since == nil || !since.Equal(base.Add(time.Second)) || a.Attention.NativeRequestID != "" {
		t.Fatalf("attention = %+v activity=%s", a.Attention, a.ActivityState)
	}
	// Identical repeat preserves since.
	h.send(apiv1.ProviderClaude, 5*time.Second, `{"session_id":"s","hook_event_name":"Notification","notification_type":"permission_prompt","message":"allow Bash?"}`, Environment{})
	h.drain()
	if a = h.agent("s"); !a.Attention.Since.Equal(base.Add(time.Second)) {
		t.Fatalf("since moved: %v", a.Attention.Since)
	}
	// An idle nudge neither clears attention nor changes activity.
	h.send(apiv1.ProviderClaude, 6*time.Second, `{"session_id":"s","hook_event_name":"Notification","notification_type":"idle_prompt","message":"waiting"}`, Environment{})
	h.send(apiv1.ProviderClaude, 7*time.Second, `{"session_id":"s","hook_event_name":"CwdChanged","new_cwd":"/tmp"}`, Environment{})
	h.drain()
	if a = h.agent("s"); a.Attention.Type != apiv1.AttentionApprovalRequired || a.Workspace == nil || a.Workspace.Path != "/tmp" {
		t.Fatalf("after nudge: %+v", a)
	}
	// Stop clears it.
	h.send(apiv1.ProviderClaude, 8*time.Second, `{"session_id":"s","hook_event_name":"Stop"}`, Environment{})
	h.drain()
	if a = h.agent("s"); a.Attention.Type != apiv1.AttentionNone || a.Attention.Since != nil {
		t.Fatalf("after stop: %+v", a.Attention)
	}
	// StopFailure sets error and failed runtime.
	h.send(apiv1.ProviderClaude, 9*time.Second, `{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit","last_assistant_message":"partial"}`, Environment{})
	h.drain()
	a = h.agent("s")
	if a.Attention.Type != apiv1.AttentionError || a.Attention.Detail != "rate_limit" || a.RuntimeState != apiv1.RuntimeStateFailed ||
		a.ActivityState != apiv1.ActivityStateIdle || a.DisplayState != apiv1.DisplayStateError || a.Summary != "partial" {
		t.Fatalf("failure = %+v", a)
	}
	// Model switch leaves state alone but updates the model.
	h.send(apiv1.ProviderClaude, 10*time.Second, `{"session_id":"s","hook_event_name":"PostModelSwitch","model":"old","to_model":"new-model"}`, Environment{})
	h.drain()
	a = h.agent("s")
	if a.Model != "new-model" || a.Attention.Type != apiv1.AttentionError || a.RuntimeState != apiv1.RuntimeStateFailed {
		t.Fatalf("model switch = %+v", a)
	}
	// Auth notification.
	h.send(apiv1.ProviderClaude, 11*time.Second, `{"session_id":"s","hook_event_name":"Notification","notification_type":"auth_required"}`, Environment{})
	h.drain()
	if a = h.agent("s"); a.Attention.Type != apiv1.AttentionAuthenticationRequired || a.ActivityState != apiv1.ActivityStateWaiting {
		t.Fatalf("auth = %+v", a)
	}
}

func TestProcessorUnknownEventKeepsStateButRevives(t *testing.T) {
	h := newHarness(t)
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"x"}`, Environment{})
	h.send(apiv1.ProviderClaude, time.Second, `{"session_id":"s","hook_event_name":"PreCompact"}`, Environment{})
	h.drain()
	a := h.agent("s")
	if a.ActivityState != apiv1.ActivityStateWorking || a.LiveState != apiv1.LiveStateLive {
		t.Fatalf("agent = %+v", a)
	}
}

func TestProcessorOutOfOrderEventsDoNotRewindState(t *testing.T) {
	h := newHarness(t)
	h.send(apiv1.ProviderClaude, 10*time.Second, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"first","cwd":"/new","model":"m-new","session_title":"New title"}`, Environment{ProcessID: 100})
	h.drain()
	// Older stop arrives later.
	h.send(apiv1.ProviderClaude, 5*time.Second, `{"session_id":"s","hook_event_name":"Stop","turn_id":"old","cwd":"/old","model":"m-old","session_title":"Old title","last_assistant_message":"old summary"}`, Environment{ProcessID: 50})
	h.drain()
	a := h.agent("s")
	if a.ActivityState != apiv1.ActivityStateWorking {
		t.Fatalf("stale stop rewound activity: %s", a.ActivityState)
	}
	if !a.LastSeenAt.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("last_seen moved back: %v", a.LastSeenAt)
	}
	if a.Name != "New title" {
		t.Fatalf("stale event changed an existing name: %q", a.Name)
	}
	if a.ProcessID != 100 {
		t.Fatalf("process regressed: %d", a.ProcessID)
	}
	if a.Model != "m-new" || a.Workspace.Path != "/new" {
		t.Fatalf("stale model/workspace applied: %q %+v", a.Model, a.Workspace)
	}
	// Summary was absent, so even a stale one is accepted.
	if a.Summary != "old summary" {
		t.Fatalf("summary = %q", a.Summary)
	}
	// A stale name is accepted only while the name is empty.
	h2 := newHarness(t)
	h2.send(apiv1.ProviderClaude, 10*time.Second, `{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"p"}`, Environment{})
	h2.send(apiv1.ProviderClaude, 5*time.Second, `{"session_id":"s","hook_event_name":"Stop","session_title":"Late name"}`, Environment{})
	h2.drain()
	if a = h2.agent("s"); a.Name != "Late name" || a.ActivityState != apiv1.ActivityStateWorking {
		t.Fatalf("late name = %q activity=%s", a.Name, a.ActivityState)
	}
}

func TestProcessorSuppressesBindingsForBackgroundClaude(t *testing.T) {
	h := newHarness(t)
	env := Environment{ZellijSession: "daemon", ZellijPaneID: "1"}
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"fg","hook_event_name":"SessionStart"}`, env)
	h.send(apiv1.ProviderClaude, 0, `{"session_id":"bg","hook_event_name":"UserPromptSubmit","prompt":"x"}`, env)
	h.drain()
	if len(h.agent("bg").FrontendBindings) != 1 {
		t.Fatal("bindings are kept until source_kind is learned")
	}
	h.send(apiv1.ProviderClaude, time.Second, `{"session_id":"bg","hook_event_name":"Stop","source_kind":"background"}`, env)
	h.send(apiv1.ProviderClaude, 2*time.Second, `{"session_id":"bg","hook_event_name":"UserPromptSubmit","prompt":"y"}`, env)
	h.drain()
	bg := h.agent("bg")
	if bg.SourceKind != "background" || len(bg.FrontendBindings) != 0 {
		t.Fatalf("background bindings = %+v", bg.FrontendBindings)
	}
	if len(h.agent("fg").FrontendBindings) != 1 {
		t.Fatal("foreground binding lost")
	}
	// source_kind is only learned once.
	h.send(apiv1.ProviderClaude, 3*time.Second, `{"session_id":"bg","hook_event_name":"Stop","source_kind":"interactive"}`, env)
	h.drain()
	if h.agent("bg").SourceKind != "background" {
		t.Fatal("source_kind overwritten")
	}
	// Codex with the same source_kind keeps its bindings.
	h.send(apiv1.ProviderCodex, 0, `{"session_id":"cx","hook_event_name":"SessionStart","source_kind":"background"}`, env)
	h.drain()
	cx, _ := h.store.GetByNativeIdentity(context.Background(), "HOST", apiv1.ProviderCodex, "cx")
	if len(cx.FrontendBindings) != 1 {
		t.Fatalf("codex bindings = %+v", cx.FrontendBindings)
	}
}

func TestMergeBindingsReplacesOnlyWhenNotOlder(t *testing.T) {
	old := []apiv1.FrontendBinding{
		{Kind: "zellij", Session: "a", PaneID: "1", State: "observed", ObservedAt: base.Add(10 * time.Second)},
		{Kind: "zellij", Session: "a", PaneID: "2", State: "observed", ObservedAt: base},
	}
	observed := []apiv1.FrontendBinding{
		{Kind: "zellij", Session: "a", PaneID: "1", State: "observed"},
		{Kind: "zellij", Session: "a", PaneID: "2", State: "observed"},
		{Kind: "claude_remote_control", State: "observed"},
	}
	got := mergeBindings(old, observed, base.Add(5*time.Second))
	if len(got) != 3 || !got[0].ObservedAt.Equal(base.Add(10*time.Second)) || !got[1].ObservedAt.Equal(base.Add(5*time.Second)) ||
		got[2].Kind != "claude_remote_control" || !got[2].ObservedAt.Equal(base.Add(5*time.Second)) {
		t.Fatalf("merged = %+v", got)
	}
	if len(old) != 2 || !old[1].ObservedAt.Equal(base) {
		t.Fatal("input mutated")
	}
}

func TestProcessorValidationAndRejectedObservations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rejected := RejectedObservation(apiv1.ProviderClaude, "HOST", base, errors.New("hook payload is not valid JSON"))
	noSession := apiv1.Observation{SchemaVersion: 1, EventID: "hook-nosession", Provider: apiv1.ProviderClaude, HostID: "HOST", EventType: EventHookObserved, ReceivedAt: base}
	wrongSchema := apiv1.Observation{SchemaVersion: 2, EventID: "hook-schema", Provider: apiv1.ProviderClaude, HostID: "HOST", NativeSessionID: "s", EventType: EventHookObserved, ReceivedAt: base}
	wrongHost := apiv1.Observation{SchemaVersion: 1, EventID: "hook-host", Provider: apiv1.ProviderClaude, HostID: "OTHER", NativeSessionID: "s", EventType: EventHookObserved, ReceivedAt: base}
	noID := apiv1.Observation{SchemaVersion: 1, Provider: apiv1.ProviderClaude, HostID: "HOST", NativeSessionID: "s", EventType: EventHookObserved, ReceivedAt: base}
	badProvider := apiv1.Observation{SchemaVersion: 1, EventID: "hook-prov", Provider: "gemini", HostID: "HOST", NativeSessionID: "s", EventType: EventHookObserved, ReceivedAt: base}
	for _, obs := range []apiv1.Observation{rejected, noSession, wrongSchema, wrongHost, noID, badProvider} {
		if _, err := h.store.EnqueueObservation(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}
	report, err := h.proc.Drain(ctx, 0)
	if err == nil || report.Pending != 6 || report.Processed != 2 || report.Failed != 4 {
		t.Fatalf("report = %+v err=%v", report, err)
	}
	for id, want := range map[string]string{
		"hook-schema": "unsupported observation schema version 2",
		"hook-host":   "observation identity is invalid for this host",
		"":            "observation identity is invalid for this host",
		"hook-prov":   `unsupported observation provider "gemini"`,
	} {
		if got := h.store.failures[id]; len(got) != 1 || got[0] != want {
			t.Errorf("failure[%q] = %v, want %q", id, got, want)
		}
	}
	// Rejected, session-less, and unsupported-provider observations are journaled.
	for _, id := range []string{rejected.EventID, "hook-nosession", "hook-prov"} {
		if _, ok := h.store.journal[id]; !ok {
			t.Errorf("%s not journaled", id)
		}
	}
	if _, ok := h.store.journal["hook-schema"]; ok {
		t.Error("schema-mismatch observation journaled")
	}
	if agents, _ := h.store.List(ctx, registry.ListOptions{IncludeNotLive: true}); len(agents) != 0 {
		t.Fatalf("rejected observations created agents: %+v", agents)
	}
}

func TestDrainQuarantinesAfterRepeatedFailuresAndContinuesBatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bad := apiv1.Observation{SchemaVersion: 9, EventID: "hook-bad", Provider: apiv1.ProviderClaude, HostID: "HOST", NativeSessionID: "s", EventType: EventHookObserved, ReceivedAt: base}
	_, _ = h.store.EnqueueObservation(ctx, bad)
	h.send(apiv1.ProviderClaude, time.Second, `{"session_id":"good","hook_event_name":"UserPromptSubmit","prompt":"x"}`, Environment{})
	report, err := h.proc.Drain(ctx, 0)
	if err == nil || report.Pending != 2 || report.Processed != 1 || report.Failed != 1 {
		t.Fatalf("first drain = %+v / %v", report, err)
	}
	h.agent("good")
	for i := 0; i < maxAttempts-1; i++ {
		if _, err := h.proc.Drain(ctx, 0); err == nil {
			t.Fatalf("drain %d should still fail", i)
		}
	}
	report, err = h.proc.Drain(ctx, 0)
	if err != nil || report.Pending != 0 {
		t.Fatalf("quarantined observation must stop being selected: %+v / %v", report, err)
	}
	if len(h.store.failures["hook-bad"]) != maxAttempts {
		t.Fatalf("attempts = %d", len(h.store.failures["hook-bad"]))
	}
}

func TestDrainReportsMarkFailuresAndHonoursLimit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		h.send(apiv1.ProviderClaude, time.Duration(i)*time.Second, fmt.Sprintf(`{"session_id":"s%d","hook_event_name":"UserPromptSubmit","prompt":"x"}`, i), Environment{})
	}
	report, err := h.proc.Drain(ctx, 2)
	if err != nil || report.Pending != 2 || report.Processed != 2 {
		t.Fatalf("limited drain = %+v / %v", report, err)
	}
	report, err = h.proc.Drain(ctx, -1)
	if err != nil || report.Pending != 1 || report.Processed != 1 {
		t.Fatalf("default-limit drain = %+v / %v", report, err)
	}
	obs := h.send(apiv1.ProviderClaude, 10*time.Second, `{"session_id":"m","hook_event_name":"UserPromptSubmit","prompt":"x"}`, Environment{})
	h.store.failMark[obs.EventID] = true
	report, err = h.proc.Drain(ctx, 0)
	if err == nil || report.Failed != 1 || report.Processed != 0 {
		t.Fatalf("mark failure = %+v / %v", report, err)
	}
}

func TestNewAgentUsesNowWhenReceivedAtIsZero(t *testing.T) {
	h := newHarness(t)
	obs := apiv1.Observation{
		SchemaVersion: 1, EventID: "hook-zero", Provider: apiv1.ProviderClaude, HostID: "HOST", NativeSessionID: "z",
		EventType: EventTurnPrompted, Source: apiv1.ObservationSourceHook,
		Data: apiv1.ObservationData{NativeEvent: "UserPromptSubmit", AttentionType: apiv1.AttentionNone},
	}
	_, _ = h.store.EnqueueObservation(context.Background(), obs)
	h.drain()
	if a := h.agent("z"); !a.FirstSeenAt.Equal(h.now) {
		t.Fatalf("first seen = %v, want %v", a.FirstSeenAt, h.now)
	}
}

func TestNewAgentIDFailureFailsTheObservation(t *testing.T) {
	store := newOutboxStore()
	proc, _ := NewProcessor(Config{HostID: "HOST", Store: store, NewID: func() (string, error) { return "", errors.New("no entropy") }})
	obs, _ := Normalize(apiv1.ProviderClaude, "HOST", []byte(`{"session_id":"s","hook_event_name":"SessionStart"}`), base, Environment{})
	_, _ = store.EnqueueObservation(context.Background(), obs)
	report, err := proc.Drain(context.Background(), 0)
	if err == nil || report.Failed != 1 || !strings.Contains(err.Error(), "allocate noc agent ID: no entropy") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
