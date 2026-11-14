package doctor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/hookconfig"
	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

type env struct {
	root, exe string
	opts      Options
}

func okHealth(context.Context, string) (apiv1.Health, error) {
	return apiv1.Health{
		Status: "ok", Version: "1.2.3", HostID: "host-1",
		Providers: []apiv1.ProviderHealth{
			{Provider: apiv1.ProviderClaude, Status: "available", Version: "c1"},
			{Provider: apiv1.ProviderCodex, Status: "available", Version: "x1"},
		},
	}, nil
}

func okProviders() map[apiv1.Provider]ProviderCheck {
	mk := func(p apiv1.Provider) ProviderCheck {
		return ProviderCheck{
			Probe: func(context.Context) provider.ProbeResult {
				return provider.ProbeResult{Provider: p, Available: true, Version: "v9"}
			},
			DiscoverLive: func(context.Context) ([]provider.NativeAgent, error) {
				return []provider.NativeAgent{{}, {}}, nil
			},
		}
	}
	return map[apiv1.Provider]ProviderCheck{apiv1.ProviderClaude: mk(apiv1.ProviderClaude), apiv1.ProviderCodex: mk(apiv1.ProviderCodex)}
}

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "bin", "noc")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return env{root: root, exe: exe, opts: Options{
		SocketPath:      filepath.Join(root, "run", "noc.sock"),
		DatabasePath:    filepath.Join(root, "state", "noc.db"),
		Executable:      exe,
		ClaudeConfigDir: filepath.Join(root, "claude"),
		CodexHome:       filepath.Join(root, "codex"),
		Health:          okHealth,
		Providers:       okProviders(),
		Now:             func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}}
}

func (e env) install(t *testing.T) {
	t.Helper()
	paths := hookconfig.Paths{
		ClaudeSettings: filepath.Join(e.opts.ClaudeConfigDir, "settings.json"),
		CodexHooks:     filepath.Join(e.opts.CodexHome, "hooks.json"),
		CodexConfig:    filepath.Join(e.opts.CodexHome, "config.toml"),
	}
	for _, p := range []apiv1.Provider{apiv1.ProviderClaude, apiv1.ProviderCodex} {
		if _, err := hookconfig.Install(p, e.exe, paths); err != nil {
			t.Fatal(err)
		}
	}
}

func find(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q missing in %+v", id, r.Checks)
	return Check{}
}

func ids(r Report) []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, c.ID)
	}
	return out
}

func TestRunHealthyInstallation(t *testing.T) {
	e := newEnv(t)
	e.install(t)
	r, err := Run(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"state", "runtime", "server", "server.provider.claude", "server.provider.codex",
		"provider.claude.cli", "provider.claude.discovery", "provider.codex.cli", "provider.codex.discovery",
		"hooks.claude", "hooks.codex"}
	if got := strings.Join(ids(r), ","); got != strings.Join(want, ",") {
		t.Fatalf("ids = %s", got)
	}
	for _, c := range r.Checks {
		if c.Status != StatusPass {
			t.Errorf("%s = %s (%s) %s", c.ID, c.Status, c.Summary, c.Detail)
		}
	}
	if r.Status != StatusPass || r.HasFailures() || r.SchemaVersion != 1 {
		t.Fatalf("report = %+v", r)
	}
	if !strings.Contains(find(t, r, "state").Detail, "nearest existing parent "+e.root) || !strings.HasSuffix(find(t, r, "state").Detail, "can create it") {
		t.Errorf("state detail = %q", find(t, r, "state").Detail)
	}
	if d := find(t, r, "provider.claude.discovery").Detail; d != "observed 2 live top-level sessions" {
		t.Errorf("discovery detail = %q", d)
	}
	for _, d := range []string{"state", "run"} {
		if _, err := os.Stat(filepath.Join(e.root, d)); !os.IsNotExist(err) {
			t.Errorf("doctor created %s", d)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(e.root, ".noc-doctor-*")); len(m) != 0 {
		t.Errorf("probe leftovers %v", m)
	}
}

func TestRunDegradedInstallation(t *testing.T) {
	e := newEnv(t)
	e.opts.Health = func(context.Context, string) (apiv1.Health, error) { return apiv1.Health{}, errors.New("boom") }
	r, err := Run(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasFailures() || r.Status != StatusFail {
		t.Fatalf("status = %s", r.Status)
	}
	s := find(t, r, "server")
	if s.Status != StatusFail || s.Summary != "noc server is not reachable" || s.Remediation != "Start it with: noc server --bg" ||
		s.Detail != "socket "+e.opts.SocketPath+": boom" {
		t.Errorf("server = %+v", s)
	}
	for _, c := range r.Checks {
		if strings.HasPrefix(c.ID, "server.provider.") {
			t.Errorf("unexpected %s", c.ID)
		}
	}
	for _, p := range []string{"claude", "codex"} {
		h := find(t, r, "hooks."+p)
		if h.Status != StatusWarning || h.Remediation != "Run `noc setup "+p+"`; expected executable: "+e.exe {
			t.Errorf("hooks.%s = %+v", p, h)
		}
		if !strings.Contains(h.Detail, "hook files: none") || !strings.Contains(h.Detail, "missing events: ") {
			t.Errorf("detail = %q", h.Detail)
		}
	}
	if !strings.Contains(find(t, r, "hooks.codex").Detail, "configuration check only; Codex trust approval is not externally verifiable") {
		t.Error("codex detail lacks trust note")
	}
}

func TestWarningOnlyDoesNotFail(t *testing.T) {
	e := newEnv(t)
	r, _ := Run(context.Background(), e.opts)
	if r.Status != StatusWarning || r.HasFailures() {
		t.Fatalf("status = %s", r.Status)
	}
}

func TestProviderDiscoveryFailureIsWarning(t *testing.T) {
	e := newEnv(t)
	e.install(t)
	e.opts.Providers[apiv1.ProviderCodex] = ProviderCheck{
		Probe: okProviders()[apiv1.ProviderCodex].Probe,
		DiscoverLive: func(context.Context) ([]provider.NativeAgent, error) {
			return nil, errors.New("no daemon")
		},
	}
	r, _ := Run(context.Background(), e.opts)
	c := find(t, r, "provider.codex.discovery")
	if c.Status != StatusWarning || c.Detail != "no daemon" || c.Remediation != "Start Codex's shared daemon with: codex app-server daemon start" {
		t.Errorf("%+v", c)
	}
	if r.Status != StatusWarning || r.HasFailures() {
		t.Errorf("status = %s", r.Status)
	}
}

func TestProviderCLIUnavailableAndMissingFuncs(t *testing.T) {
	e := newEnv(t)
	e.opts.Providers = map[apiv1.Provider]ProviderCheck{
		apiv1.ProviderClaude: {
			Probe: func(context.Context) provider.ProbeResult {
				return provider.ProbeResult{Diagnostic: "not found"}
			},
			DiscoverLive: func(context.Context) ([]provider.NativeAgent, error) { t.Error("discovery called"); return nil, nil },
		},
	}
	r, _ := Run(context.Background(), e.opts)
	c := find(t, r, "provider.claude.cli")
	if c.Status != StatusWarning || c.Detail != "not found" || !strings.Contains(c.Remediation, "claude agents --json") {
		t.Errorf("%+v", c)
	}
	for _, c := range r.Checks {
		if c.ID == "provider.claude.discovery" {
			t.Error("discovery check present")
		}
	}
	m := find(t, r, "provider.codex")
	if m.Status != StatusWarning || m.Summary != "codex diagnostic is unavailable" {
		t.Errorf("%+v", m)
	}
}

func TestSkipProviderChecks(t *testing.T) {
	e := newEnv(t)
	e.opts.Providers = nil
	e.opts.SkipProviderChecks = true
	r, _ := Run(context.Background(), e.opts)
	for _, c := range r.Checks {
		if strings.HasPrefix(c.ID, "provider.") {
			t.Errorf("unexpected %s", c.ID)
		}
	}
}

func TestCodexTOMLCoverageAndDisabledFeature(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.opts.CodexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("[features]\nhooks = false\n\n")
	for _, ev := range hookconfig.RequiredEvents(apiv1.ProviderCodex) {
		b.WriteString("[[hooks." + ev + "]]\n[[hooks." + ev + ".hooks]]\ntype = \"command\"\ncommand = \"'" + e.exe + "' hook ingest --provider codex\"\n\n")
	}
	if err := os.WriteFile(filepath.Join(e.opts.CodexHome, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := Run(context.Background(), e.opts)
	c := find(t, r, "hooks.codex")
	if c.Status != StatusWarning || !strings.Contains(c.Detail, "Codex hooks are disabled in [features]") {
		t.Errorf("%+v", c)
	}
	if strings.Contains(c.Detail, "missing events") {
		t.Errorf("TOML coverage not seen: %s", c.Detail)
	}

	// Enabling features restores a pass.
	b2 := strings.Replace(b.String(), "hooks = false", "hooks = true", 1)
	_ = os.WriteFile(filepath.Join(e.opts.CodexHome, "config.toml"), []byte(b2), 0o600)
	r, _ = Run(context.Background(), e.opts)
	if c := find(t, r, "hooks.codex"); c.Status != StatusPass {
		t.Errorf("%+v", c)
	}
}

func TestHookCoverageRequiresCurrentExecutable(t *testing.T) {
	e := newEnv(t)
	old := e
	old.exe = "/old/bin/noc"
	old.install(t)
	r, _ := Run(context.Background(), e.opts)
	for _, p := range []string{"claude", "codex"} {
		c := find(t, r, "hooks."+p)
		if c.Status != StatusWarning || !strings.Contains(c.Detail, "events not bound to this executable: ") || strings.Contains(c.Detail, "missing events") {
			t.Errorf("hooks.%s = %+v", p, c)
		}
	}
}

func TestStateFailures(t *testing.T) {
	e := newEnv(t)
	e.opts.DatabasePath = ""
	r, _ := Run(context.Background(), e.opts)
	if c := find(t, r, "state"); c.Status != StatusFail || c.Summary != "state storage path is empty" {
		t.Errorf("%+v", c)
	}

	e = newEnv(t)
	e.opts.DatabasePath = filepath.Join(e.root, "dbdir")
	_ = os.Mkdir(e.opts.DatabasePath, 0o755)
	r, _ = Run(context.Background(), e.opts)
	if c := find(t, r, "state"); c.Status != StatusFail || c.Summary != "state storage is not a regular file" || c.Detail != e.opts.DatabasePath {
		t.Errorf("%+v", c)
	}

	e = newEnv(t)
	blocker := filepath.Join(e.root, "blocker")
	_ = os.WriteFile(blocker, nil, 0o600)
	e.opts.DatabasePath = filepath.Join(blocker, "noc.db")
	r, _ = Run(context.Background(), e.opts)
	c := find(t, r, "state")
	if c.Status != StatusFail || c.Summary != "state storage is not writable" || c.Remediation != "Choose a writable path with NOC_STATE_DIR or NOC_DATABASE." {
		t.Errorf("%+v", c)
	}
}

func TestStateExistingDatabase(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.opts.DatabasePath), 0o755)
	_ = os.WriteFile(e.opts.DatabasePath, nil, 0o600)
	r, _ := Run(context.Background(), e.opts)
	c := find(t, r, "state")
	if c.Status != StatusPass || c.Detail != e.opts.DatabasePath+"; directory exists" {
		t.Errorf("%+v", c)
	}
}

func TestRuntimeFailures(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(filepath.Dir(e.opts.SocketPath), 0o755)
	_ = os.WriteFile(e.opts.SocketPath, nil, 0o600)
	r, _ := Run(context.Background(), e.opts)
	c := find(t, r, "runtime")
	if c.Status != StatusFail || c.Summary != "runtime socket path is occupied by a non-socket file" || c.Remediation != "Move the file or select a different path with NOC_SOCKET." {
		t.Errorf("%+v", c)
	}

	e = newEnv(t)
	e.opts.SocketPath = ""
	r, _ = Run(context.Background(), e.opts)
	if c := find(t, r, "runtime"); c.Summary != "runtime socket path is empty" {
		t.Errorf("%+v", c)
	}

	// A real socket passes.
	e = newEnv(t)
	dir, err := os.MkdirTemp("/tmp", "nd")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(dir)
	e.opts.SocketPath = filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", e.opts.SocketPath)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	r, _ = Run(context.Background(), e.opts)
	if c := find(t, r, "runtime"); c.Status != StatusPass {
		t.Errorf("%+v", c)
	}
}

func TestServerNonOKAndProviderWarning(t *testing.T) {
	e := newEnv(t)
	e.opts.Health = func(context.Context, string) (apiv1.Health, error) {
		return apiv1.Health{Status: "degraded", Version: "v", HostID: "h", Providers: []apiv1.ProviderHealth{
			{Provider: apiv1.ProviderClaude, Status: "unavailable", Version: "1", Detail: "no cli"},
		}}, nil
	}
	r, _ := Run(context.Background(), e.opts)
	s := find(t, r, "server")
	if s.Status != StatusFail || s.Summary != "noc server reports degraded" || s.Detail != "v on "+e.opts.SocketPath+"; host h" {
		t.Errorf("%+v", s)
	}
	p := find(t, r, "server.provider.claude")
	if p.Status != StatusWarning || p.Summary != "server sees claude as unavailable" || p.Detail != "1; no cli" {
		t.Errorf("%+v", p)
	}
	if !r.HasFailures() {
		t.Error("expected failure")
	}
}
