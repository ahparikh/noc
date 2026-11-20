package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// TestMain lets the test binary stand in for the noc executable: background
// servers re-exec os.Executable(), which is this binary.
func TestMain(m *testing.M) {
	if os.Getenv("NOC_CLI_TEST_AS_NOC") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// isolate points every noc path at a short temp directory and keeps provider
// binaries away from the real ones.
func isolate(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "noc-cli-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("NOC_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("NOC_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("NOC_SOCKET", filepath.Join(dir, "run", "noc.sock"))
	t.Setenv("NOC_DATABASE", filepath.Join(dir, "state", "noc.db"))
	t.Setenv("NOC_CLAUDE_BIN", "/usr/bin/false")
	t.Setenv("NOC_CODEX_BIN", "/usr/bin/false")
	t.Setenv("NOC_CODEX_SOCKET", filepath.Join(dir, "no-codex.sock"))
	t.Setenv("NOC_AGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("NOC_INTERNAL_SERVER_READY_FD", "")
	os.Unsetenv("NOC_INTERNAL_SERVER_READY_FD")
	t.Setenv("ZELLIJ", "")
	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("ZELLIJ_PANE_ID", "")
	t.Setenv("CLAUDE_CODE_REMOTE", "")
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWithInput(t, "", args...)
}

func runCLIWithInput(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO(context.Background(), args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// startServer runs a foreground server in-process and waits until it answers.
func startServer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- RunWithIO(ctx, []string{"server", "--discovery=false"}, strings.NewReader(""), io.Discard, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	waitFor(t, "server to accept requests", func() bool {
		code, _, _ := runCLI(t, "server", "status", "--json")
		return code == 0
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ingest(t *testing.T, provider, payload string) {
	t.Helper()
	code, stdout, stderr := runCLIWithInput(t, payload, "hook", "ingest", "--provider", provider)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("hook ingest = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func listAgents(t *testing.T, extra ...string) apiv1.AgentList {
	t.Helper()
	code, stdout, stderr := runCLI(t, append([]string{"ls", "--json"}, extra...)...)
	if code != 0 {
		t.Fatalf("ls --json = %d: %s", code, stderr)
	}
	var list apiv1.AgentList
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("decode ls: %v\n%s", err, stdout)
	}
	return list
}

func TestRunHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, stdout, stderr := runCLI(t, arg)
		if code != 0 || stderr != "" {
			t.Fatalf("%s = %d stderr=%q", arg, code, stderr)
		}
		if !strings.HasPrefix(stdout, "noc manages the local coding-agent registry.\n\nUsage:\n  noc\n") {
			t.Fatalf("%s usage = %q", arg, stdout)
		}
		for _, want := range []string{
			"noc server status [--json]", "noc server [--bg] [--socket path] [--database path]",
			"noc server restart", "noc server stop", "noc events [--json]", "noc doctor [--json]",
			"noc watch [--json] [--all]", "noc setup [--check|--uninstall] [claude|codex]",
			"noc hook ingest --provider claude|codex", "Run noc setup after installing the\nbinary",
		} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("usage missing %q:\n%s", want, stdout)
			}
		}
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := runCLI(t, "version")
	if code != 0 || stdout != "dev\n" {
		t.Fatalf("version = %d %q", code, stdout)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, "bogus")
	if code != 2 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.HasPrefix(stderr, "noc: unknown command \"bogus\"\n\nnoc manages the local") {
		t.Fatalf("stderr = %q", stderr)
	}
	if code, _, _ := runCLI(t, "--json"); code != 2 {
		t.Fatalf("--json should not route to ls, code=%d", code)
	}
}

func TestSingleBinaryServerAndDefaultClient(t *testing.T) {
	isolate(t)
	startServer(t)
	code, stdout, stderr := runCLI(t)
	if code != 0 || stdout != "No live agents.\n" || stderr != "" {
		t.Fatalf("noc = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestServerDownShowsConnectionHint(t *testing.T) {
	isolate(t)
	code, stdout, stderr := runCLI(t, "ls")
	if code != 1 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if !strings.HasPrefix(stderr, "noc: connect to noc server: ") || !strings.HasSuffix(stderr, "\nIs the server running? Start it with: noc server\n") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSetupRejectsConflictingActions(t *testing.T) {
	code, _, stderr := runCLI(t, "setup", "--check", "--uninstall")
	if code != 2 || !strings.Contains(stderr, "noc setup: --check and --uninstall are mutually exclusive") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "setup", "gemini"); code != 2 || !strings.Contains(stderr, `noc setup: unsupported provider "gemini"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "setup", "claude", "codex"); code != 2 || !strings.Contains(stderr, "usage: noc setup [--check|--uninstall] [claude|codex]") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestDoctorRejectsInvalidTimeout(t *testing.T) {
	code, _, stderr := runCLI(t, "doctor", "--timeout=0s")
	if code != 2 || !strings.Contains(stderr, "noc doctor: timeout must be positive") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "doctor", "extra"); code != 2 || !strings.Contains(stderr, "noc doctor: unexpected positional arguments") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestValidationPaths(t *testing.T) {
	isolate(t)
	cases := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"ls", "--bucket", "bogus"}, 2, `noc ls: unknown bucket "bogus" (want blocked, working, review, or quiet)`},
		{[]string{"ls", "extra"}, 2, "noc ls: unexpected positional arguments"},
		{[]string{"events", "--limit", "0"}, 2, "noc events: limit must be between 1 and 200"},
		{[]string{"events", "--limit", "201"}, 2, "noc events: limit must be between 1 and 200"},
		{[]string{"events", "--provider", "x"}, 2, "noc events: provider must be claude or codex"},
		{[]string{"events", "a", "b"}, 2, "usage: noc events [--json] [--provider claude|codex] [--limit count] [agent-id]"},
		{[]string{"show"}, 2, "usage: noc show [--json] <agent-id>"},
		{[]string{"rename", "id", "   "}, 2, "noc rename: name cannot be empty; use --clear to remove it"},
		{[]string{"rename", "id"}, 2, "usage: noc rename [--clear] <agent-id> [name]"},
		{[]string{"rename", "--clear", "id", "extra"}, 2, "usage: noc rename [--clear] <agent-id> [name]"},
		{[]string{"forget"}, 2, "usage: noc forget <agent-id> [<agent-id>...]"},
		{[]string{"attach", "id", "--force"}, 2, "usage: noc attach [--force] <agent-id>"},
		{[]string{"stop"}, 2, "usage: noc stop <agent-id>"},
		{[]string{"respawn"}, 2, "usage: noc respawn <agent-id>"},
		{[]string{"watch", "x"}, 2, "noc watch: unexpected positional arguments"},
		{[]string{"hook"}, 2, "usage: noc hook ingest --provider claude|codex"},
		{[]string{"hook", "other"}, 2, "usage: noc hook ingest --provider claude|codex"},
		{[]string{"server", "bogus"}, 2, `noc server: unknown command "bogus"`},
		{[]string{"server", "run", "stop"}, 2, `noc server: unknown command "stop"`},
		{[]string{"server", "--discovery=false", "--bg", "status"}, 2, "noc server: unexpected positional arguments"},
		{[]string{"server", "--discovery=false", "--log-format=xml"}, 2, `noc server: invalid log format "xml"`},
		{[]string{"server", "--discovery-interval=0"}, 2, "noc server: discovery interval must be positive"},
		{[]string{"server", "--stale-after=0"}, 2, "noc server: stale-after must be positive"},
		{[]string{"server", "restart", "--discovery=false", "--bg"}, 2, "flag provided but not defined: -bg"},
		{[]string{"server", "restart", "--discovery=false", "--timeout=0"}, 2, "noc server restart: timeout must be positive"},
		{[]string{"server", "stop", "--timeout=0"}, 2, "noc server stop: timeout must be positive"},
		{[]string{"server", "status", "x"}, 2, "noc server status: unexpected positional arguments"},
		{[]string{"ls", "-h"}, 0, "Usage of noc ls:"},
		{[]string{"ls", "--nope"}, 2, "flag provided but not defined: -nope"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCLI(t, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) || stdout != "" {
			t.Errorf("%v = %d stdout=%q stderr=%q, want %d containing %q", tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
}

func TestHookIngestSpoolsSilentlyWhileServerIsDown(t *testing.T) {
	isolate(t)
	sid := "11111111-2222-4333-8444-555555555555"
	ingest(t, "claude", `{"session_id":"`+sid+`","hook_event_name":"SessionStart","cwd":"/tmp","source":"startup"}`)
	ingest(t, "claude", `{not json`)
	ingest(t, "gemini", `{}`)
	code, stdout, stderr := runCLI(t, "hook", "ingest", "--provider", "claude", "stray")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("positional hook = %d %q %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLI(t, "hook", "ingest", "-h")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("-h hook = %d %q %q", code, stdout, stderr)
	}

	// The spooled observation is projected once a server starts.
	startServer(t)
	waitFor(t, "spooled hook to be projected", func() bool {
		for _, agent := range listAgents(t).Agents {
			if agent.NativeSessionID == sid {
				return true
			}
		}
		return false
	})
}

func TestRunningServerProcessesHookAndExposesPreview(t *testing.T) {
	isolate(t)
	t.Setenv("ZELLIJ_SESSION_NAME", "work")
	t.Setenv("ZELLIJ_PANE_ID", "7")
	startServer(t)
	cwd := t.TempDir()
	sid := "11111111-2222-4333-8444-555555555555"
	ingest(t, "claude", `{"session_id":"`+sid+`","hook_event_name":"SessionStart","cwd":"`+cwd+`","source":"startup"}`)
	ingest(t, "claude", `{"session_id":"`+sid+`","hook_event_name":"UserPromptSubmit","cwd":"`+cwd+`","prompt":"Please   fix\n\nthe parser"}`)

	var agent apiv1.Agent
	waitFor(t, "working hook agent", func() bool {
		for _, candidate := range listAgents(t).Agents {
			if candidate.NativeSessionID == sid && candidate.Bucket == apiv1.BucketWorking {
				agent = candidate
				return true
			}
		}
		return false
	})
	if agent.Origin != "hook" || agent.Provider != apiv1.ProviderClaude {
		t.Fatalf("agent = %+v", agent)
	}
	if len(agent.FrontendBindings) != 1 || agent.FrontendBindings[0].Kind != "zellij" || agent.FrontendBindings[0].Session != "work" {
		t.Fatalf("bindings = %+v", agent.FrontendBindings)
	}

	code, stdout, stderr := runCLI(t, "show", "--json", agent.ID)
	if code != 0 {
		t.Fatalf("show = %d %s", code, stderr)
	}
	var shown apiv1.Agent
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil || shown.InitialRequest == nil || shown.InitialRequest.Text != "Please fix the parser" {
		t.Fatalf("show = %v %+v", err, shown.InitialRequest)
	}
	code, stdout, _ = runCLI(t, "show", agent.ID)
	if code != 0 || !strings.Contains(stdout, "Initial request:") || !strings.Contains(stdout, "Frontend:") || !strings.Contains(stdout, "zellij:work/7 (") {
		t.Fatalf("human show = %d\n%s", code, stdout)
	}

	code, stdout, stderr = runCLI(t, "events", "--json", agent.ID)
	if code != 0 {
		t.Fatalf("events = %d %s", code, stderr)
	}
	var events apiv1.AgentEventList
	if err := json.Unmarshal([]byte(stdout), &events); err != nil || len(events.Events) < 2 {
		t.Fatalf("events = %v %+v", err, events)
	}
	if strings.Contains(stdout, "fix") {
		t.Fatalf("events leak prompt text: %s", stdout)
	}
	if code, stdout, _ := runCLI(t, "events"); code != 0 || !strings.HasPrefix(stdout, "TIME") {
		t.Fatalf("human events = %d %q", code, stdout)
	}

	code, stdout, stderr = runCLI(t, "rename", agent.ID, "Hook", "ingestion")
	want := `Named ` + agent.ID + ` "Hook ingestion" in noc. Claude cannot be renamed from outside, so the session keeps its own name until you rename it there.` + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("rename = %d %q %q", code, stdout, stderr)
	}
	if got := listAgents(t).Agents[0].UserName; got != "Hook ingestion" {
		t.Fatalf("user_name = %q", got)
	}
	code, stdout, _ = runCLI(t, "rename", "--clear", agent.ID)
	if code != 0 || stdout != "Cleared the name for "+agent.ID+".\n" {
		t.Fatalf("clear = %d %q", code, stdout)
	}
	if code, _, stderr := runCLI(t, "rename", agent.ID, strings.Repeat("n", 121)); code != 1 || !strings.Contains(stderr, "noc: noc server: agent name must be at most 120 characters") {
		t.Fatalf("long name = %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "show", "nope"); code != 1 || stderr != "noc: noc server: agent was not found\n" {
		t.Fatalf("show nope = %d %q", code, stderr)
	}

	// Stage 2: a Claude agent is not attachable without a live roster.
	if code, _, stderr := runCLI(t, "stop", agent.ID); code != 1 || !strings.Contains(stderr, "only background Claude sessions can be stopped") {
		t.Fatalf("stop = %d %q", code, stderr)
	}

	code, stdout, stderr = runCLI(t, "forget", agent.ID, "nope")
	if code != 1 || stdout != "forgot "+agent.ID+"\n" || stderr != "noc: noc server: agent was not found\n" {
		t.Fatalf("forget = %d %q %q", code, stdout, stderr)
	}
}

func TestAttachCodexRunsResume(t *testing.T) {
	dir := isolate(t)
	t.Setenv("NOC_CODEX_BIN", "/usr/bin/true")
	startServer(t)
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	ingest(t, "codex", `{"session_id":"thread-0001","hook_event_name":"UserPromptSubmit","cwd":"`+cwd+`","prompt":"hi"}`)
	var id string
	waitFor(t, "codex agent", func() bool {
		for _, agent := range listAgents(t).Agents {
			if agent.NativeSessionID == "thread-0001" {
				id = agent.ID
				return true
			}
		}
		return false
	})
	if code, _, stderr := runCLI(t, "attach", id); code != 0 {
		t.Fatalf("attach = %d %q", code, stderr)
	}

	// A fake Codex proves cwd, NOC_AGENT_ID, argv and exit-status propagation.
	logPath := filepath.Join(dir, "fake.log")
	fake := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\necho \"$PWD :: $NOC_AGENT_ID :: $*\" >> " + logPath + "\nexit 7\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_CODEX_BIN", fake)
	if code, _, _ := runCLI(t, "attach", id); code != 7 {
		t.Fatalf("attach exit = %d, want 7", code)
	}
	logged, _ := os.ReadFile(logPath)
	resolved, _ := filepath.EvalSymlinks(cwd)
	if !strings.HasSuffix(strings.TrimSpace(string(logged)), ":: "+id+" :: resume thread-0001") || !strings.Contains(string(logged), resolved) {
		t.Fatalf("fake log = %q", logged)
	}

	// Missing workspace and missing executable are refusals, not crashes.
	t.Setenv("NOC_CODEX_BIN", filepath.Join(dir, "absent"))
	if code, _, stderr := runCLI(t, "attach", id); code != 1 || !strings.Contains(stderr, `noc: find codex executable `) {
		t.Fatalf("absent bin = %d %q", code, stderr)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_CODEX_BIN", fake)
	if code, _, stderr := runCLI(t, "attach", id); code != 1 || !strings.Contains(stderr, `noc: open agent workspace `) {
		t.Fatalf("missing workspace = %d %q", code, stderr)
	}
}

func TestBackgroundStartRejectsExistingServerWithStopGuidance(t *testing.T) {
	isolate(t)
	startServer(t)
	for _, args := range [][]string{{"server", "--bg", "--discovery=false"}, {"server", "--discovery=false"}} {
		code, stdout, stderr := runCLI(t, args...)
		want := "noc: noc server is already running\nStop the previous instance by running: noc server stop\n"
		if code != 1 || stdout != "" || stderr != want {
			t.Fatalf("%v = %d %q %q", args, code, stdout, stderr)
		}
	}
}

func TestRenderEventsShowsSafeTimeline(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	renderEvents(&out, []apiv1.AgentEvent{
		{ID: "e1", AgentID: "a1", Provider: apiv1.ProviderClaude, EventType: "turn.stopped", Source: apiv1.ObservationSourceHook,
			OccurredAt: &at, ReceivedAt: at.Add(time.Hour), NativeEvent: "Stop", PreviousLiveState: apiv1.LiveStateLive, Reason: "done"},
		{ID: "e2", Provider: apiv1.ProviderCodex, EventType: "hook.rejected", Source: apiv1.ObservationSourceHook,
			ReceivedAt: at, LiveState: apiv1.LiveStateNotLive, Error: "bad"},
		{ID: "e3", Provider: apiv1.ProviderCodex, EventType: "x", Source: apiv1.ObservationSourceSnapshot, ReceivedAt: at},
	})
	text := out.String()
	for _, want := range []string{
		"TIME", "PROVIDER", "DETAIL",
		at.Local().Format("2006-01-02 15:04:05"),
		"native=Stop; live -> -; reason=done",
		"hook.rejected - - -> not_live; error=bad",
	} {
		if !strings.Contains(strings.Join(strings.Fields(text), " "), strings.Join(strings.Fields(want), " ")) {
			t.Fatalf("events missing %q:\n%s", want, text)
		}
	}
	if !strings.HasSuffix(strings.TrimRight(text, "\n"), "-") {
		t.Fatalf("event without detail should end with '-':\n%s", text)
	}
	out.Reset()
	renderEvents(&out, nil)
	if out.String() != "No events found.\n" {
		t.Fatalf("empty = %q", out.String())
	}
}

func testHealth() apiv1.Health {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	startup := at.Add(-time.Minute)
	return apiv1.Health{
		SchemaVersion: 1, Status: "ok", Version: "dev", HostID: "host-1", StartedAt: at,
		Providers: []apiv1.ProviderHealth{
			{Provider: apiv1.ProviderClaude, Status: "available", Version: "2.1", DiscoverySource: "claude agents",
				StartupStatus: "available", StartupObservedAt: &startup, StartupDetail: "ready", ObservedAt: at, Detail: "3 sessions"},
			{Provider: apiv1.ProviderCodex, Status: "degraded", ObservedAt: at, Detail: "no daemon"},
		},
	}
}

func TestRenderServerStatusIncludesStartupAndLatestDiscovery(t *testing.T) {
	agents := []apiv1.Agent{
		{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateLive, Origin: "hook"},
		{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateLive, Origin: "reconcile"},
		{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateLive},
		{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateNotLive, Origin: "hook"},
	}
	var out bytes.Buffer
	renderServerStatus(&out, testHealth(), agents[:1], &serverPIDRecord{PID: 42, Mode: "background"})
	text := out.String()
	for _, want := range []string{
		"noc server ok (dev)\n", "  host: host-1\n", "  process: pid 42 (background)\n",
		"\nclaude: available\n", "  version: 2.1\n", "  source: claude agents\n",
		"  registry: 1 live agent (origin: hook 1)\n",
		"  startup: available at " + testHealth().Providers[0].StartupObservedAt.Local().Format(time.RFC3339) + " — ready\n",
		"  latest: available at " + testHealth().StartedAt.Local().Format(time.RFC3339) + " — 3 sessions\n",
		"\ncodex: degraded\n", "  registry: 0 live agents\n",
		"  hint: start or inspect the shared daemon with `codex app-server daemon start` or `codex app-server daemon version`\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status missing %q:\n%s", want, text)
		}
	}
	out.Reset()
	renderServerStatus(&out, testHealth(), agents, nil)
	if !strings.Contains(out.String(), "registry: 3 live agents (origin: hook 1, snapshot 1, unknown 1)") || strings.Contains(out.String(), "process:") {
		t.Fatalf("status = %s", out.String())
	}
}

func TestRenderServerStatusDoesNotSuggestDaemonWhenDiscoveryIsDisabled(t *testing.T) {
	health := testHealth()
	health.Providers = []apiv1.ProviderHealth{
		{Provider: apiv1.ProviderClaude, Status: "disabled", ObservedAt: time.Now(), Detail: "provider discovery disabled"},
		{Provider: apiv1.ProviderCodex, Status: "disabled", ObservedAt: time.Now(), Detail: "provider discovery disabled"},
	}
	var out bytes.Buffer
	renderServerStatus(&out, health, nil, nil)
	if strings.Contains(out.String(), "hint:") || !strings.Contains(out.String(), "codex: disabled") {
		t.Fatalf("status = %s", out.String())
	}
}

func TestRenderAgentChangesShowsMembershipAndState(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 5, 0, time.UTC)
	old := apiv1.Agent{ID: "b", Provider: apiv1.ProviderClaude, DisplayState: apiv1.DisplayStateWorking, Workspace: &apiv1.Workspace{Path: "/w"}}
	updated := old
	updated.DisplayState = apiv1.DisplayStateReady
	updated.UserName = "Named"
	previous := apiv1.AgentList{Agents: []apiv1.Agent{old, {ID: "c", Provider: apiv1.ProviderCodex, DisplayState: apiv1.DisplayStateIdle}}}
	current := apiv1.AgentList{GeneratedAt: at, Agents: []apiv1.Agent{updated, {ID: "a", Provider: apiv1.ProviderCodex, DisplayState: apiv1.DisplayStateWorking}}}
	var out bytes.Buffer
	renderAgentChanges(&out, previous, current)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	stamp := at.Local().Format("15:04:05")
	for i, want := range []string{
		stamp + " ADDED a codex working - -",
		stamp + " REMOVED c codex idle - -",
		stamp + " UPDATED b claude working -> ready /w - -> Named",
	} {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != want {
			t.Fatalf("line %d = %q, want %q", i, got, want)
		}
	}
	out.Reset()
	renderAgentChanges(&out, current, current)
	if out.Len() != 0 {
		t.Fatalf("no-change output = %q", out.String())
	}
}

func TestWatchAgentsEqualIgnoresReconciliationTimestamps(t *testing.T) {
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	build := func(at time.Time) apiv1.Agent {
		return apiv1.Agent{
			ID: "a", LastSeenAt: at, SnapshotConfirmedAt: &at, ModelObservedAt: &at, ReasoningObservedAt: &at, SummaryUpdatedAt: &at,
			InitialRequest:   &apiv1.TextPreview{Text: "t", ObservedAt: at},
			Workspace:        &apiv1.Workspace{Path: "/w", ObservedAt: at},
			FrontendBindings: []apiv1.FrontendBinding{{Kind: "zellij", ObservedAt: at}},
		}
	}
	a, b := build(t1), build(t2)
	if !watchAgentsEqual(a, b) {
		t.Fatal("timestamp-only differences should be equal")
	}
	if a.Workspace.ObservedAt != t1 || a.FrontendBindings[0].ObservedAt != t1 {
		t.Fatal("projection mutated its input")
	}
	b.Summary = "changed"
	if watchAgentsEqual(a, b) {
		t.Fatal("summary change should be a difference")
	}
	c := build(t2)
	c.LastActivityAt = &t2
	if watchAgentsEqual(a, c) {
		t.Fatal("last_activity_at change should be a difference")
	}
}

func TestRenderAgentIncludesDetailPreviewMetadata(t *testing.T) {
	at := time.Now()
	agent := apiv1.Agent{
		ID: "id-1", Provider: apiv1.ProviderCodex, NativeSessionID: "thread", HostID: "h", DisplayState: apiv1.DisplayStateReady,
		RuntimeState: apiv1.RuntimeStateRunning, ActivityState: apiv1.ActivityStateIdle, UserName: "mine", Name: "theirs",
		Model: "gpt", ReasoningEffort: "high",
		InitialRequest: &apiv1.TextPreview{Text: "do it", Source: "hook", ObservedAt: at},
		Summary:        "done", SummarySource: "hook_last",
		Workspace:        &apiv1.Workspace{Path: "/w", RootPath: "/r", Kind: "git", Branch: "main"},
		FrontendBindings: []apiv1.FrontendBinding{{Kind: "zellij", Session: "s", PaneID: "3", State: "observed"}, {Kind: "tmux", State: "observed"}},
	}
	var out bytes.Buffer
	renderAgent(&out, agent)
	text := out.String()
	for _, want := range []string{
		"ID:", "Native session:", "Host:", "State:", "Runtime:", "Activity:", "User name:", "Provider name:", "Model:",
		"Reasoning effort:", "Initial request:", "Initial request source:", "Current summary:", "Summary source:",
		"Directory:", "Workspace root:", "Workspace kind:", "Branch:", "zellij:s/3 (observed)", "tmux (observed)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("show missing %q:\n%s", want, text)
		}
	}
	out.Reset()
	renderAgent(&out, apiv1.Agent{ID: "x"})
	if strings.Contains(out.String(), "Model:") || strings.Contains(out.String(), "Directory:") {
		t.Fatalf("minimal show = %s", out.String())
	}
}

func TestListFiltersAndOrdersByBucket(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { v := base.Add(time.Duration(m) * time.Minute); return &v }
	agents := []apiv1.Agent{
		{ID: "quiet-new", Bucket: apiv1.BucketQuiet, LastActivityAt: at(9)},
		{ID: "review-old", Bucket: apiv1.BucketReview, LastActivityAt: at(1)},
		{ID: "blocked", Bucket: apiv1.BucketBlocked, LastActivityAt: at(0)},
		{ID: "review-new", Bucket: apiv1.BucketReview, LastActivityAt: at(5)},
		{ID: "working", Bucket: apiv1.BucketWorking, EndedAt: at(2)},
		{ID: "odd", Bucket: "weird", LastActivityAt: at(99)},
	}
	sortByBucket(agents)
	var order []string
	for _, a := range agents {
		order = append(order, a.ID)
	}
	if got := strings.Join(order, ","); got != "blocked,working,review-new,review-old,quiet-new,odd" {
		t.Fatalf("order = %s", got)
	}
	selected, err := parseBuckets("review, Blocked,,")
	if err != nil {
		t.Fatal(err)
	}
	kept := filterBuckets(agents, selected)
	if len(kept) != 3 || kept[0].ID != "blocked" {
		t.Fatalf("kept = %+v", kept)
	}
	if _, err := parseBuckets("bogus"); err == nil || err.Error() != `unknown bucket "bogus" (want blocked, working, review, or quiet)` {
		t.Fatalf("err = %v", err)
	}
	for _, empty := range []string{"", ",", " "} {
		if sel, err := parseBuckets(empty); err != nil || len(sel) != 0 {
			t.Fatalf("%q = %v %v", empty, sel, err)
		}
	}
}

func TestRecencyIgnoresWhenNocLastLooked(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	last := base.Add(time.Hour)
	created := base.Add(-time.Hour)
	ended := base.Add(-30 * time.Minute)
	activity := base.Add(-10 * time.Minute)
	agent := apiv1.Agent{FirstSeenAt: base.Add(-2 * time.Hour), LastSeenAt: last, CreatedAt: &created}
	if got := agentRecency(agent); !got.Equal(created) {
		t.Fatalf("created fallback = %v", got)
	}
	agent.EndedAt = &ended
	if got := agentRecency(agent); !got.Equal(ended) {
		t.Fatalf("ended = %v", got)
	}
	agent.LastActivityAt = &activity
	if got := agentRecency(agent); !got.Equal(activity) {
		t.Fatalf("activity = %v", got)
	}
	if got := agentRecency(apiv1.Agent{FirstSeenAt: base, LastSeenAt: last}); !got.Equal(base) {
		t.Fatalf("first seen = %v", got)
	}
}

func TestServerLaunchFlagsAndBackgroundArgs(t *testing.T) {
	opts := serverOptions{Socket: "/s", Database: "/d", PIDFile: "/p", LogFormat: "json", Discovery: false, DiscoveryInterval: 5 * time.Second, StaleAfter: 30 * time.Second}
	got := strings.Join(backgroundServerArgs(opts), " ")
	want := "server --socket=/s --database=/d --pid-file=/p --log-format=json --discovery=false --discovery-interval=5s --stale-after=30s"
	if got != want {
		t.Fatalf("args = %q", got)
	}
	env := replaceEnvironmentValue([]string{"A=1", "X=old", "X=older"}, "X", "3")
	if strings.Join(env, ",") != "A=1,X=3" {
		t.Fatalf("env = %v", env)
	}
}

func TestAttachClaudeDecisionTable(t *testing.T) {
	dir := isolate(t)
	startServer(t)
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "11111111-2222-4333-8444-555555555555"
	ingest(t, "claude", `{"session_id":"`+sid+`","hook_event_name":"SessionStart","cwd":"`+work+`","source":"startup"}`)
	var id string
	waitFor(t, "claude agent", func() bool {
		for _, agent := range listAgents(t).Agents {
			if agent.NativeSessionID == sid {
				id = agent.ID
				return true
			}
		}
		return false
	})

	fake := filepath.Join(dir, "fake-claude")
	script := `#!/bin/sh
echo "$PWD :: NOC_AGENT_ID=$NOC_AGENT_ID :: $*" >> "$FAKE_LOG"
case "$1" in
  agents) [ -n "$FAKE_ROSTER_FAIL" ] && exit 1; cat "$FAKE_ROSTER" ;;
  --bg) cp "$FAKE_ROSTER_AFTER" "$FAKE_ROSTER"; echo restored ;;
  attach) echo "attached $2"; exit "${FAKE_ATTACH_EXIT:-0}" ;;
  stop) echo "stopped $2" ;;
  respawn) echo "respawned $2" ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "fake.log")
	roster := filepath.Join(dir, "roster.json")
	after := filepath.Join(dir, "roster-after.json")
	t.Setenv("NOC_CLAUDE_BIN", fake)
	t.Setenv("FAKE_LOG", logPath)
	t.Setenv("FAKE_ROSTER", roster)
	t.Setenv("FAKE_ROSTER_AFTER", after)
	t.Setenv("FAKE_ROSTER_FAIL", "")
	t.Setenv("FAKE_ATTACH_EXIT", "")
	setRoster := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "$SID", sid)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	readLog := func() string {
		data, _ := os.ReadFile(logPath)
		_ = os.Remove(logPath)
		return string(data)
	}
	resolvedWork, _ := filepath.EvalSymlinks(work)

	// Row 10: unreadable roster and a live session with no attach ID.
	t.Setenv("FAKE_ROSTER_FAIL", "1")
	code, _, stderr := runCLI(t, "attach", id)
	if code != 1 || !strings.HasPrefix(stderr, "noc: refusing to attach: this Claude session is live and has no background attach ID") ||
		!strings.HasSuffix(stderr, "\nFocus the terminal that already has it, or re-run with --force to start a second process anyway.\n") {
		t.Fatalf("row 10 = %d %q", code, stderr)
	}
	// Row 11: --force resumes in the foreground.
	readLog()
	if code, _, _ := runCLI(t, "attach", "--force", id); code != 0 {
		t.Fatalf("row 11 = %d", code)
	}
	if got := readLog(); !strings.Contains(got, "--resume "+sid) {
		t.Fatalf("row 11 log = %q", got)
	}
	t.Setenv("FAKE_ROSTER_FAIL", "")

	// Row 6: running in a terminal.
	setRoster(roster, `[{"id":"","sessionId":"$SID","kind":"interactive","state":"working","pid":4242}]`)
	code, _, stderr = runCLI(t, "attach", id)
	if code != 1 || stderr != "noc: refusing to attach: this Claude session is already running in a terminal (pid 4242); focus that terminal instead.\nRe-run with --force to open it here anyway.\n" {
		t.Fatalf("row 6 = %d %q", code, stderr)
	}

	// Row 7.
	readLog()
	setRoster(roster, `[{"id":"ab12cd34","sessionId":"$SID","kind":"background","state":"done","pid":null}]`)
	code, stdout, _ := runCLI(t, "attach", id)
	if code != 0 || stdout != "attached ab12cd34\n" {
		t.Fatalf("row 7 = %d %q", code, stdout)
	}
	if got := readLog(); !strings.Contains(got, resolvedWork+" :: NOC_AGENT_ID="+id+" :: attach ab12cd34") {
		t.Fatalf("row 7 log = %q", got)
	}

	// Row 8: restore, re-read, attach.
	setRoster(roster, `[]`)
	setRoster(after, `[{"id":"ef56ab78","sessionId":"$SID","kind":"background","state":"working","pid":999}]`)
	readLog()
	code, stdout, _ = runCLI(t, "attach", id)
	if code != 0 || stdout != "attached ef56ab78\n" {
		t.Fatalf("row 8 = %d %q", code, stdout)
	}
	log := readLog()
	if !strings.Contains(log, "--bg --resume "+sid) || !strings.Contains(log, "attach ef56ab78") || strings.Index(log, "--bg") > strings.Index(log, "attach ef56") {
		t.Fatalf("row 8 log = %q", log)
	}

	// Row 8 failure: no job after restore.
	setRoster(roster, `[]`)
	setRoster(after, `[]`)
	code, _, stderr = runCLI(t, "attach", id)
	if code != 1 || stderr != "noc: Claude resumed the session but lists no background job for it\n" {
		t.Fatalf("row 8 failure = %d %q", code, stderr)
	}

	// Exit status propagates.
	setRoster(roster, `[{"id":"ab12cd34","sessionId":"$SID","kind":"background","state":"done","pid":null}]`)
	t.Setenv("FAKE_ATTACH_EXIT", "7")
	if code, _, _ := runCLI(t, "attach", id); code != 7 {
		t.Fatalf("exit propagation = %d", code)
	}

	// Stop: a hook-only session is not a background job.
	code, _, stderr = runCLI(t, "stop", id)
	if code != 1 || stderr != "noc: only background Claude sessions can be stopped; end this one from its terminal with /exit\n" {
		t.Fatalf("stop = %d %q", code, stderr)
	}
	// Respawn: the same rule.
	code, _, stderr = runCLI(t, "respawn", id)
	if code != 1 || stderr != "noc: only background Claude sessions can be respawned; restart this one from its terminal\n" {
		t.Fatalf("respawn = %d %q", code, stderr)
	}
}
