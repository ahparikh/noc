package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

type fakeClient struct {
	agents []provider.NativeAgent
	err    error
}

func (f *fakeClient) DiscoverLive(context.Context) ([]provider.NativeAgent, error) {
	return f.agents, f.err
}

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testAdapter(r provider.CommandRunner, c DiscoveryClient) *Adapter {
	return NewWithOptions(Options{
		Runner:     r,
		Executable: "codex",
		SocketPath: "/tmp/unused.sock",
		Client:     c,
		Now:        func() time.Time { return fixedNow },
	})
}

func TestProbe(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{
		"codex --version":         "codex-cli 0.154.0\n",
		"codex app-server --help": "Run the app server",
	}}
	got := testAdapter(r, &fakeClient{}).Probe(context.Background())
	if !got.Available || got.Version != "codex-cli 0.154.0" || got.Diagnostic != "" || got.Provider != apiv1.ProviderCodex {
		t.Fatalf("probe = %+v", got)
	}
	if !got.Features["discover_live"] || !got.Features["direct_unix_socket"] || len(got.Features) != 2 {
		t.Fatalf("features = %v", got.Features)
	}
	if !got.ObservedAt.Equal(fixedNow) {
		t.Fatalf("observed = %v", got.ObservedAt)
	}
	if strings.Join(r.calls, "|") != "codex --version|codex app-server --help" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestProbeRequiresAppServerSupport(t *testing.T) {
	r := &fakeRunner{
		outputs: map[string]string{"codex --version": "codex-cli 0.1.0\n"},
		errs:    map[string]error{"codex app-server --help": errors.New("no such subcommand")},
	}
	got := testAdapter(r, &fakeClient{}).Probe(context.Background())
	want := "Codex live discovery unavailable; installed CLI does not expose app-server: no such subcommand"
	if got.Available || got.Version != "codex-cli 0.1.0" || got.Diagnostic != want || len(got.Features) != 0 {
		t.Fatalf("probe = %+v", got)
	}
}

func TestProbeUnavailable(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"codex --version": errors.New("missing")}}
	got := testAdapter(r, &fakeClient{}).Probe(context.Background())
	if got.Available || got.Diagnostic != "missing" || got.Version != "" || len(got.Features) != 0 {
		t.Fatalf("probe = %+v", got)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestNewUsesExecutableEnvironment(t *testing.T) {
	t.Setenv("NOC_CODEX_BIN", " /opt/codex ")
	if got := New().executable; got != "/opt/codex" {
		t.Fatalf("executable = %q", got)
	}
	t.Setenv("NOC_CODEX_BIN", "")
	a := New()
	if a.executable != "codex" || a.timeout != 5*time.Second || a.Provider() != apiv1.ProviderCodex {
		t.Fatalf("defaults = %q %v", a.executable, a.timeout)
	}
}

func TestExplicitExecutableOverridesEnvironment(t *testing.T) {
	t.Setenv("NOC_CODEX_BIN", "/env/codex")
	if got := NewWithOptions(Options{Executable: " /explicit/codex "}).executable; got != "/explicit/codex" {
		t.Fatalf("executable = %q", got)
	}
}

func TestNewUsesSocketEnvironment(t *testing.T) {
	t.Setenv("NOC_CODEX_SOCKET", " /tmp/x/../env.sock ")
	t.Setenv("CODEX_HOME", "/ignored")
	a := New()
	if a.socketPath != "/tmp/env.sock" {
		t.Fatalf("socketPath = %q", a.socketPath)
	}
	client, ok := a.client.(*socketClient)
	if !ok || client.path != "/tmp/env.sock" || client.fallback != nil {
		t.Fatalf("client = %#v", a.client)
	}
}

func TestExplicitSocketOverridesEnvironment(t *testing.T) {
	t.Setenv("NOC_CODEX_SOCKET", "/tmp/env.sock")
	a := NewWithOptions(Options{SocketPath: " /tmp/opt/./a.sock "})
	client := a.client.(*socketClient)
	if a.socketPath != "/tmp/opt/a.sock" || client.path != "/tmp/opt/a.sock" || client.fallback != nil {
		t.Fatalf("socket = %q client = %#v", a.socketPath, client)
	}
}

func TestNewDerivesSocketFromCodexHomeWithProxyFallback(t *testing.T) {
	t.Setenv("NOC_CODEX_SOCKET", "")
	t.Setenv("CODEX_HOME", " /tmp/codex-home/ ")
	a := NewWithOptions(Options{Executable: "/bin/codex-x"})
	want := filepath.Join("/tmp/codex-home", "app-server-control", "app-server-control.sock")
	client := a.client.(*socketClient)
	if client.path != want || a.socketPath != want {
		t.Fatalf("path = %q", client.path)
	}
	proxy, ok := client.fallback.(*proxyClient)
	if !ok || proxy.executable != "/bin/codex-x" || proxy.timeout != 5*time.Second {
		t.Fatalf("fallback = %#v", client.fallback)
	}

	t.Setenv("CODEX_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got := New().client.(*socketClient).path
	if got != filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock") {
		t.Fatalf("default path = %q", got)
	}
}

func TestDiscoverLiveDelegatesToClient(t *testing.T) {
	rows := []provider.NativeAgent{{NativeSessionID: "t1"}, {NativeSessionID: "t2"}}
	got, err := testAdapter(&fakeRunner{}, &fakeClient{agents: rows}).DiscoverLive(context.Background())
	if err != nil || len(got) != 2 || got[0].NativeSessionID != "t1" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDiscoverLiveWrapsClientFailure(t *testing.T) {
	_, err := testAdapter(&fakeRunner{}, &fakeClient{err: errors.New("daemon down")}).DiscoverLive(context.Background())
	if err == nil || err.Error() != "query Codex app-server: daemon down" {
		t.Fatalf("err = %v", err)
	}
}

func evidenceAgent(source, raw string) provider.NativeAgent {
	return provider.NativeAgent{NativeSessionID: "t", Name: "n", SourceKind: source, CWD: "/a/./b/", Raw: []byte(raw)}
}

func TestNormalizeStateMappings(t *testing.T) {
	a := testAdapter(&fakeRunner{}, &fakeClient{})
	cases := []struct {
		name     string
		raw      string
		runtime  apiv1.RuntimeState
		activity apiv1.ActivityState
		attn     apiv1.AttentionType
		detail   string
	}{
		{"active", `{"status":"active"}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking, apiv1.AttentionNone, ""},
		{"approval priority", `{"status":"active","active_flags":["waitingOnUserInput","waitingOnApproval"]}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting, apiv1.AttentionApprovalRequired, "Codex is waiting on approval"},
		{"input", `{"status":"active","active_flags":["waitingOnUserInput"]}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting, apiv1.AttentionInputRequired, "Codex is waiting on user input"},
		{"other flag", `{"status":"active","active_flags":["WAITINGONAPPROVAL"]}`, apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking, apiv1.AttentionNone, ""},
		{"idle", `{"status":"idle","active_flags":["waitingOnApproval"]}`, apiv1.RuntimeStateLoaded, apiv1.ActivityStateIdle, apiv1.AttentionNone, ""},
		{"error", `{"status":"systemError"}`, apiv1.RuntimeStateFailed, apiv1.ActivityStateUnknown, apiv1.AttentionError, "Codex app-server reported a system error"},
		{"unknown", `{"status":"mystery"}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateUnknown, apiv1.AttentionUnknown, ""},
		{"empty", `{}`, apiv1.RuntimeStateUnknown, apiv1.ActivityStateUnknown, apiv1.AttentionUnknown, ""},
	}
	for _, tc := range cases {
		n, err := a.Normalize(evidenceAgent("cli", tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n.RuntimeState != tc.runtime || n.ActivityState != tc.activity || n.Attention.Type != tc.attn || n.Attention.Detail != tc.detail {
			t.Errorf("%s = %s/%s/%s/%q", tc.name, n.RuntimeState, n.ActivityState, n.Attention.Type, n.Attention.Detail)
		}
		if n.LiveState != apiv1.LiveStateLive || n.NativeShortID != "" || n.Summary != "" || n.ModelSource != "codex_app_server_thread" || n.ReasoningSource != "codex_app_server_thread" {
			t.Errorf("%s = %+v", tc.name, n)
		}
		if !n.Capabilities["attach"].Supported || !n.Capabilities["resume"].Supported {
			t.Errorf("%s capabilities = %+v", tc.name, n.Capabilities)
		}
		if n.Workspace == nil || n.Workspace.Path != "/a/b" || !n.Workspace.ObservedAt.Equal(fixedNow) || n.Workspace.Owner != "unknown" {
			t.Errorf("%s workspace = %+v", tc.name, n.Workspace)
		}
	}

	n, err := a.Normalize(evidenceAgent("appServer",
		`{"status":"active","active_flags":["waitingOnApproval"],"created_at":1789400000,"updated_at":1789400010,"model":"m","reasoning_effort":"high","preview":"hello","preview_truncated":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if n.CreatedAt == nil || n.CreatedAt.Format(time.RFC3339) != "2026-09-14T15:33:20Z" ||
		n.LastActivityAt == nil || n.LastActivityAt.Format(time.RFC3339) != "2026-09-14T15:33:30Z" {
		t.Fatalf("times = %v %v", n.CreatedAt, n.LastActivityAt)
	}
	if n.Model != "m" || n.ReasoningEffort != "high" || n.Name != "n" || n.SourceKind != "appServer" {
		t.Fatalf("n = %+v", n)
	}
	if n.InitialRequest == nil || n.InitialRequest.Text != "hello" || n.InitialRequest.Source != "codex_app_server_preview" ||
		!n.InitialRequest.Truncated || !n.InitialRequest.ObservedAt.Equal(fixedNow) {
		t.Fatalf("initial = %+v", n.InitialRequest)
	}
	if got := string(n.NativeState); got != `{"status":"active","active_flags":["waitingOnApproval"]}` {
		t.Fatalf("native_state = %s", got)
	}

	empty, _ := a.Normalize(evidenceAgent("cli", `{"status":"idle"}`))
	if empty.InitialRequest != nil || empty.CreatedAt != nil || empty.LastActivityAt != nil || string(empty.NativeState) != `{"status":"idle"}` {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestNormalizeRejectsNonLiveOrInternalThreads(t *testing.T) {
	a := testAdapter(&fakeRunner{}, &fakeClient{})
	cases := []struct {
		agent provider.NativeAgent
		want  string
	}{
		{provider.NativeAgent{NativeSessionID: "  ", SourceKind: "cli", Raw: []byte(`{}`)}, "Codex thread has no stable thread ID"},
		{provider.NativeAgent{NativeSessionID: "t", SourceKind: "exec", Raw: []byte(`{}`)}, `unsupported Codex thread source "exec"`},
		{provider.NativeAgent{NativeSessionID: "t", SourceKind: "cli", Raw: []byte(`nope`)}, "decode sanitized Codex state: "},
		{provider.NativeAgent{NativeSessionID: "t", SourceKind: "cli", Raw: []byte(`{"status":"notLoaded"}`)}, "Codex thread is not loaded"},
	}
	for _, tc := range cases {
		_, err := a.Normalize(tc.agent)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("err = %v, want prefix %q", err, tc.want)
		}
	}
}

func TestBoundedText(t *testing.T) {
	if got, trunc := boundedText("  first\n\n  request\t ", 100); got != "first request" || trunc {
		t.Fatalf("got %q %v", got, trunc)
	}
	if got, trunc := boundedText(strings.Repeat("é", 5), 5); got != strings.Repeat("é", 5) || trunc {
		t.Fatalf("got %q %v", got, trunc)
	}
	if got, trunc := boundedText(strings.Repeat("é", 6), 5); got != strings.Repeat("é", 5) || !trunc {
		t.Fatalf("got %q %v", got, trunc)
	}
}
