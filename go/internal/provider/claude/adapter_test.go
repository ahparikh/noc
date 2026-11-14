package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

type fakeRunner struct {
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	out, ok := f.outputs[key]
	if !ok {
		return nil, errors.New("unexpected command: " + key)
	}
	return []byte(out), nil
}

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newTestAdapter(r provider.CommandRunner) *Adapter {
	return NewWithOptions(Options{Runner: r, Executable: "claude", Now: func() time.Time { return fixedNow }})
}

func TestProbe(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{
		"claude --version":     "2.1.270 (Claude Code)\n",
		"claude agents --help": "Usage",
	}}
	got := newTestAdapter(r).Probe(context.Background())
	if !got.Available || got.Version != "2.1.270 (Claude Code)" || got.Diagnostic != "" {
		t.Fatalf("probe = %+v", got)
	}
	if got.Provider != apiv1.ProviderClaude || !got.Features["discover_live"] || len(got.Features) != 1 {
		t.Fatalf("features = %+v", got)
	}
	if !got.ObservedAt.Equal(fixedNow) {
		t.Fatalf("observed = %v", got.ObservedAt)
	}
	if strings.Join(r.calls, "|") != "claude --version|claude agents --help" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestProbeUnavailable(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"claude --version": errors.New("not found")}}
	got := newTestAdapter(r).Probe(context.Background())
	if got.Available || got.Diagnostic != "not found" || got.Version != "" || len(got.Features) != 0 {
		t.Fatalf("probe = %+v", got)
	}
	if len(r.calls) != 1 {
		t.Fatalf("step 2 must not run: %v", r.calls)
	}
}

func TestProbeRequiresAgentRosterCapability(t *testing.T) {
	r := &fakeRunner{
		outputs: map[string]string{"claude --version": " 2.0.0 \n"},
		errs:    map[string]error{"claude agents --help": errors.New("unknown command")},
	}
	got := newTestAdapter(r).Probe(context.Background())
	if got.Available || got.Version != "2.0.0" || len(got.Features) != 0 {
		t.Fatalf("probe = %+v", got)
	}
	if got.Diagnostic != "Claude live discovery unavailable: unknown command" {
		t.Fatalf("diagnostic = %q", got.Diagnostic)
	}
}

func TestNewUsesExecutableEnvironment(t *testing.T) {
	t.Setenv("NOC_CLAUDE_BIN", "  /opt/claude  ")
	if got := New().executable; got != "/opt/claude" {
		t.Fatalf("executable = %q", got)
	}
	t.Setenv("NOC_CLAUDE_BIN", "   ")
	if got := New().executable; got != "claude" {
		t.Fatalf("executable = %q", got)
	}
}

func TestExplicitExecutableOverridesEnvironment(t *testing.T) {
	t.Setenv("NOC_CLAUDE_BIN", "/env/claude")
	a := NewWithOptions(Options{Executable: " /explicit/claude "})
	if a.executable != "/explicit/claude" {
		t.Fatalf("executable = %q", a.executable)
	}
	if a.timeout != 5*time.Second || a.Provider() != apiv1.ProviderClaude {
		t.Fatalf("defaults = %v %v", a.timeout, a.Provider())
	}
}

func discoverFixture(t *testing.T) (*Adapter, []provider.NativeAgent) {
	t.Helper()
	data, err := os.ReadFile("testdata/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{outputs: map[string]string{"claude agents --json": string(data)}}
	a := newTestAdapter(r)
	agents, err := a.DiscoverLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0] != "claude agents --json" {
		t.Fatalf("calls = %v", r.calls)
	}
	return a, agents
}

func TestDiscoverLiveFiltersAndSanitizes(t *testing.T) {
	_, agents := discoverFixture(t)
	if len(agents) != 7 {
		t.Fatalf("accepted %d rows, want 7", len(agents))
	}
	first := agents[0]
	if first.NativeShortID != "11111111" || first.CWD != "/workspace/service-a" || first.SourceKind != "background" {
		t.Fatalf("first = %+v", first)
	}
	if first.Name != "working-agent" {
		t.Fatalf("name = %q", first.Name)
	}
	for _, a := range agents {
		raw := string(a.Raw)
		for _, banned := range []string{`"prompt"`, "transcript", "1201", "pid", "must never"} {
			if strings.Contains(raw, banned) {
				t.Fatalf("evidence %s leaks %q", raw, banned)
			}
		}
	}
	if got := string(agents[1].Raw); got != `{"state":"blocked","status":"waiting","waiting_for":"permission prompt","started_at":1760000001000,"process_alive":true}` {
		t.Fatalf("raw = %s", got)
	}
	last := agents[6]
	if last.NativeShortID != "" || last.SourceKind != "interactive" || last.NativeSessionID != "77777777-7777-4777-8777-777777777777" {
		t.Fatalf("last = %+v", last)
	}
	for _, a := range agents {
		if strings.Contains(a.Name, "cloud") || strings.Contains(a.Name, "no-session") {
			t.Fatalf("dropped row leaked: %+v", a)
		}
	}
}

func TestDiscoverLiveTopLevelDecoding(t *testing.T) {
	cases := []struct {
		out     string
		wantErr string
		want    int
	}{
		{out: "null", want: 0},
		{out: "[]", want: 0},
		{out: "", wantErr: "decode Claude agent roster: unexpected end of JSON input"},
		{out: `{"a":1}`, wantErr: "decode Claude agent roster: "},
	}
	for _, tc := range cases {
		r := &fakeRunner{outputs: map[string]string{"claude agents --json": tc.out}}
		got, err := newTestAdapter(r).DiscoverLive(context.Background())
		if tc.wantErr != "" {
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("%q: err = %v", tc.out, err)
			}
			continue
		}
		if err != nil || len(got) != tc.want {
			t.Fatalf("%q: got %v, %v", tc.out, got, err)
		}
	}
	boom := errors.New("boom")
	r := &fakeRunner{errs: map[string]error{"claude agents --json": boom}}
	if _, err := newTestAdapter(r).DiscoverLive(context.Background()); err != boom {
		t.Fatalf("runner error must be unwrapped, got %v", err)
	}
}

func TestNormalizeStateMappings(t *testing.T) {
	a, agents := discoverFixture(t)
	type want struct {
		runtime  apiv1.RuntimeState
		activity apiv1.ActivityState
		attn     apiv1.AttentionType
		detail   string
		attach   bool
	}
	wants := []want{
		{apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking, apiv1.AttentionNone, "", true},
		{apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting, apiv1.AttentionApprovalRequired, "permission prompt", true},
		{apiv1.RuntimeStateResumable, apiv1.ActivityStateWaiting, apiv1.AttentionInputRequired, "input needed", true},
		{apiv1.RuntimeStateResumable, apiv1.ActivityStateIdle, apiv1.AttentionNone, "", true},
		{apiv1.RuntimeStateFailed, apiv1.ActivityStateUnknown, apiv1.AttentionError, "Claude session failed", true},
		{apiv1.RuntimeStateStopped, apiv1.ActivityStateIdle, apiv1.AttentionNone, "", true},
		{apiv1.RuntimeStateRunning, apiv1.ActivityStateIdle, apiv1.AttentionNone, "", false},
	}
	for i, w := range wants {
		n, err := a.Normalize(agents[i])
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if n.RuntimeState != w.runtime || n.ActivityState != w.activity || n.Attention.Type != w.attn || n.Attention.Detail != w.detail {
			t.Errorf("row %d (%s) = %s/%s/%s/%q", i, n.Name, n.RuntimeState, n.ActivityState, n.Attention.Type, n.Attention.Detail)
		}
		if n.Capabilities["attach"].Supported != w.attach || !n.Capabilities["resume"].Supported {
			t.Errorf("row %d capabilities = %+v", i, n.Capabilities)
		}
		if n.LiveState != apiv1.LiveStateLive || n.Provider != apiv1.ProviderClaude || n.LastActivityAt != nil {
			t.Errorf("row %d = %+v", i, n)
		}
		if n.Workspace == nil || !n.Workspace.ObservedAt.Equal(fixedNow) || n.Workspace.Kind != "directory" || n.Workspace.Owner != "unknown" {
			t.Errorf("row %d workspace = %+v", i, n.Workspace)
		}
	}
	n, _ := a.Normalize(agents[1])
	if n.CreatedAt == nil || n.CreatedAt.Format(time.RFC3339) != "2025-10-09T08:53:21Z" {
		t.Fatalf("created = %v", n.CreatedAt)
	}
	if got := string(n.NativeState); got != `{"state":"blocked","status":"waiting","waiting_for":"permission prompt","process_alive":true}` {
		t.Fatalf("native_state = %s", got)
	}
	last, _ := a.Normalize(agents[6])
	if last.Capabilities["attach"].Reason != "interactive session has no background attach ID" {
		t.Fatalf("reason = %q", last.Capabilities["attach"].Reason)
	}
}

func TestNormalizeFallbackAndErrors(t *testing.T) {
	a := newTestAdapter(&fakeRunner{})
	mk := func(ev string) provider.NativeAgent {
		return provider.NativeAgent{NativeSessionID: "s", SourceKind: "background", Raw: json.RawMessage(ev)}
	}
	cases := []struct {
		ev       string
		runtime  apiv1.RuntimeState
		activity apiv1.ActivityState
		attn     apiv1.AttentionType
		detail   string
	}{
		{`{"status":"busy","process_alive":true}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking, apiv1.AttentionNone, ""},
		{`{"status":"waiting","waiting_for":"dialog open","process_alive":true}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting, apiv1.AttentionInputRequired, "dialog open"},
		{`{"status":"waiting","waiting_for":"sandbox request"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateWaiting, apiv1.AttentionApprovalRequired, "sandbox request"},
		{`{"status":"waiting","waiting_for":"weird"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateWaiting, apiv1.AttentionUnknown, "weird"},
		{`{"status":"waiting"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateWaiting, apiv1.AttentionNone, ""},
		{`{"state":"blocked","waiting_for":"worker request"}`, apiv1.RuntimeStateResumable, apiv1.ActivityStateWaiting, apiv1.AttentionInputRequired, "worker request"},
		{`{"state":"blocked","waiting_for":"weird"}`, apiv1.RuntimeStateResumable, apiv1.ActivityStateWaiting, apiv1.AttentionInputRequired, "weird"},
		{`{"state":"mystery","status":"idle"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateIdle, apiv1.AttentionNone, ""},
		{`{"status":"zzz"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateUnknown, apiv1.AttentionNone, ""},
	}
	for _, tc := range cases {
		n, err := a.Normalize(mk(tc.ev))
		if err != nil {
			t.Fatal(err)
		}
		if n.RuntimeState != tc.runtime || n.ActivityState != tc.activity || n.Attention.Type != tc.attn || n.Attention.Detail != tc.detail {
			t.Errorf("%s = %s/%s/%s/%q", tc.ev, n.RuntimeState, n.ActivityState, n.Attention.Type, n.Attention.Detail)
		}
	}
	if _, err := a.Normalize(provider.NativeAgent{NativeSessionID: " ", SourceKind: "background"}); err == nil || err.Error() != "Claude session has no stable session ID" {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Normalize(provider.NativeAgent{NativeSessionID: "s", SourceKind: "cloud"}); err == nil || err.Error() != `unsupported Claude session kind "cloud"` {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Normalize(mk(`nope`)); err == nil || !strings.HasPrefix(err.Error(), "decode sanitized Claude state: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionNameIgnoresIDPlaceholders(t *testing.T) {
	cases := []struct{ name, id, session, want string }{
		{"  real name ", "1111", "aaaa", "real name"},
		{"1111", "1111", "aaaa", ""},
		{" aaaa ", "1111", " aaaa", ""},
		{"", "1111", "aaaa", ""},
		{"   ", "1111", "aaaa", ""},
	}
	for _, tc := range cases {
		if got := sessionName(tc.name, tc.id, tc.session); got != tc.want {
			t.Errorf("sessionName(%q,%q,%q) = %q, want %q", tc.name, tc.id, tc.session, got, tc.want)
		}
	}
}

func TestAdapterHasNoRenamer(t *testing.T) {
	var a any = New()
	if _, ok := a.(provider.Renamer); ok {
		t.Fatal("Claude adapter must not implement Renamer")
	}
}
