// Package doctor produces a read-only diagnostic report about a noc
// installation. It never edits provider configuration, never opens the SQLite
// database, and never retains files it creates for probing.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/provider/claude"
	"noc.local/noc/internal/provider/codex"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// Status is a check or overall result.
type Status string

const (
	StatusPass    Status = "pass"
	StatusWarning Status = "warning"
	StatusFail    Status = "fail"
)

// Check is one diagnostic row.
type Check struct {
	ID          string `json:"id"`
	Status      Status `json:"status"`
	Summary     string `json:"summary"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is the full doctor output.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Status        Status    `json:"status"`
	GeneratedAt   time.Time `json:"generated_at"`
	Checks        []Check   `json:"checks"`
}

// HasFailures reports whether the overall status is fail.
func (r Report) HasFailures() bool { return r.Status == StatusFail }

// ProviderCheck supplies the provider diagnostics.
type ProviderCheck struct {
	Probe        func(ctx context.Context) provider.ProbeResult
	DiscoverLive func(ctx context.Context) ([]provider.NativeAgent, error)
}

// Options configures Run. Zero values select production defaults.
type Options struct {
	SocketPath         string
	DatabasePath       string
	Executable         string
	ClaudeConfigDir    string
	CodexHome          string
	Health             func(ctx context.Context, socket string) (apiv1.Health, error)
	Providers          map[apiv1.Provider]ProviderCheck
	SkipProviderChecks bool
	Now                func() time.Time
}

var providerOrder = []apiv1.Provider{apiv1.ProviderClaude, apiv1.ProviderCodex}

// Run executes every check and returns the report.
func Run(ctx context.Context, opts Options) (Report, error) {
	opts, err := withDefaults(opts)
	if err != nil {
		return Report{}, err
	}
	var checks []Check
	checks = append(checks, stateCheck(opts.DatabasePath), runtimeCheck(opts.SocketPath))
	checks = append(checks, serverChecks(ctx, opts)...)
	if !opts.SkipProviderChecks {
		for _, p := range providerOrder {
			checks = append(checks, providerChecks(ctx, p, opts.Providers[p])...)
		}
	}
	for _, p := range providerOrder {
		checks = append(checks, hookCheck(p, opts))
	}
	return Report{
		SchemaVersion: 1,
		Status:        overall(checks),
		GeneratedAt:   opts.Now().UTC(),
		Checks:        checks,
	}, nil
}

func overall(checks []Check) Status {
	status := StatusPass
	for _, c := range checks {
		switch c.Status {
		case StatusFail:
			return StatusFail
		case StatusWarning:
			status = StatusWarning
		}
	}
	return status
}

func withDefaults(opts Options) (Options, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	exe := strings.TrimSpace(opts.Executable)
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return opts, fmt.Errorf("resolve noc executable: %w", err)
		}
	}
	exe = filepath.Clean(exe)
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	opts.Executable = exe

	if strings.TrimSpace(opts.ClaudeConfigDir) == "" || strings.TrimSpace(opts.CodexHome) == "" {
		claudeDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
		codexDir := strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if claudeDir == "" || codexDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return opts, fmt.Errorf("resolve home directory: %w", err)
			}
			if claudeDir == "" {
				claudeDir = filepath.Join(home, ".claude")
			}
			if codexDir == "" {
				codexDir = filepath.Join(home, ".codex")
			}
		}
		if strings.TrimSpace(opts.ClaudeConfigDir) == "" {
			opts.ClaudeConfigDir = claudeDir
		}
		if strings.TrimSpace(opts.CodexHome) == "" {
			opts.CodexHome = codexDir
		}
	}
	if opts.Health == nil {
		opts.Health = func(ctx context.Context, socket string) (apiv1.Health, error) {
			c := localapi.NewClient(socket)
			defer c.Close()
			return c.Health(ctx)
		}
	}
	if opts.Providers == nil && !opts.SkipProviderChecks {
		cl, cx := claude.New(), codex.New()
		opts.Providers = map[apiv1.Provider]ProviderCheck{
			apiv1.ProviderClaude: {Probe: cl.Probe, DiscoverLive: cl.DiscoverLive},
			apiv1.ProviderCodex:  {Probe: cx.Probe, DiscoverLive: cx.DiscoverLive},
		}
	}
	return opts, nil
}

func filepathJoin(dir, name string) string { return filepath.Clean(filepath.Join(dir, name)) }

func stateCheck(db string) Check {
	c := Check{ID: "state"}
	if strings.TrimSpace(db) == "" {
		return fail(c, "state storage path is empty", "", "")
	}
	detail, err := probeWritableDirectory(filepath.Dir(db))
	if err != nil {
		return fail(c, "state storage is not writable", db+": "+err.Error(),
			"Choose a writable path with NOC_STATE_DIR or NOC_DATABASE.")
	}
	info, err := os.Stat(db)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return fail(c, "state storage is not a regular file", db, "")
		}
		f, err := os.OpenFile(db, os.O_WRONLY, 0)
		if err != nil {
			return fail(c, "state storage database is not writable", db+": "+err.Error(),
				"Correct ownership or permissions on the noc database.")
		}
		_ = f.Close()
	case !errors.Is(err, os.ErrNotExist):
		return fail(c, "state storage database cannot be inspected", db+": "+err.Error(), "")
	}
	c.Status = StatusPass
	c.Summary = "state storage is writable"
	c.Detail = db + "; " + detail
	return c
}

func runtimeCheck(sock string) Check {
	c := Check{ID: "runtime"}
	if strings.TrimSpace(sock) == "" {
		return fail(c, "runtime socket path is empty", "", "")
	}
	dir := filepath.Dir(sock)
	detail, err := probeWritableDirectory(dir)
	if err != nil {
		return fail(c, "runtime directory is not writable", dir+": "+err.Error(),
			"Choose a writable path with NOC_RUNTIME_DIR or NOC_SOCKET.")
	}
	info, err := os.Lstat(sock)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSocket == 0 {
			return fail(c, "runtime socket path is occupied by a non-socket file", sock,
				"Move the file or select a different path with NOC_SOCKET.")
		}
	case !errors.Is(err, os.ErrNotExist):
		return fail(c, "runtime socket cannot be inspected", sock+": "+err.Error(), "")
	}
	c.Status = StatusPass
	c.Summary = "runtime directory is writable"
	c.Detail = sock + "; " + detail
	return c
}

func fail(c Check, summary, detail, remediation string) Check {
	c.Status = StatusFail
	c.Summary = summary
	c.Detail = detail
	c.Remediation = remediation
	return c
}

func serverChecks(ctx context.Context, opts Options) []Check {
	h, err := opts.Health(ctx, opts.SocketPath)
	if err != nil {
		return []Check{{
			ID:          "server",
			Status:      StatusFail,
			Summary:     "noc server is not reachable",
			Detail:      fmt.Sprintf("socket %s: %v", opts.SocketPath, err),
			Remediation: "Start it with: noc server --bg",
		}}
	}
	server := Check{
		ID:      "server",
		Status:  StatusPass,
		Summary: fmt.Sprintf("noc server reports %s", h.Status),
		Detail:  fmt.Sprintf("%s on %s; host %s", h.Version, opts.SocketPath, h.HostID),
	}
	if h.Status != "ok" {
		server.Status = StatusFail
	}
	checks := []Check{server}
	for _, ph := range h.Providers {
		c := Check{
			ID:      "server.provider." + string(ph.Provider),
			Status:  StatusPass,
			Summary: fmt.Sprintf("server sees %s as %s", ph.Provider, ph.Status),
			Detail:  ph.Version,
		}
		if ph.Detail != "" {
			c.Detail += "; " + ph.Detail
		}
		if ph.Status != "available" {
			c.Status = StatusWarning
		}
		checks = append(checks, c)
	}
	return checks
}

func providerChecks(ctx context.Context, p apiv1.Provider, pc ProviderCheck) []Check {
	if pc.Probe == nil || pc.DiscoverLive == nil {
		return []Check{{
			ID:      "provider." + string(p),
			Status:  StatusWarning,
			Summary: fmt.Sprintf("%s diagnostic is unavailable", p),
		}}
	}
	var installHint, discoveryHint string
	switch p {
	case apiv1.ProviderClaude:
		installHint = "Install or update Claude Code so `claude agents --json` is available."
		discoveryHint = installHint
	case apiv1.ProviderCodex:
		installHint = "Install or update Codex so `codex app-server` is available."
		discoveryHint = "Start Codex's shared daemon with: codex app-server daemon start"
	}
	probe := pc.Probe(ctx)
	if !probe.Available {
		return []Check{{
			ID:          fmt.Sprintf("provider.%s.cli", p),
			Status:      StatusWarning,
			Summary:     fmt.Sprintf("%s CLI discovery capability is unavailable", p),
			Detail:      probe.Diagnostic,
			Remediation: installHint,
		}}
	}
	cli := Check{
		ID:      fmt.Sprintf("provider.%s.cli", p),
		Status:  StatusPass,
		Summary: fmt.Sprintf("%s CLI supports live discovery", p),
		Detail:  probe.Version,
	}
	disc := Check{ID: fmt.Sprintf("provider.%s.discovery", p)}
	agents, err := pc.DiscoverLive(ctx)
	if err != nil {
		disc.Status = StatusWarning
		disc.Summary = fmt.Sprintf("%s live discovery failed", p)
		disc.Detail = err.Error()
		disc.Remediation = discoveryHint
	} else {
		disc.Status = StatusPass
		disc.Summary = fmt.Sprintf("%s live discovery succeeded", p)
		disc.Detail = fmt.Sprintf("observed %d live top-level sessions", len(agents))
	}
	return []Check{cli, disc}
}

// probeWritableDirectory verifies dir (or its nearest existing ancestor) accepts
// new entries, without leaving anything behind.
func probeWritableDirectory(dir string) (string, error) {
	existing := dir
	for {
		info, err := os.Stat(existing)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%q is not a directory", existing)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOENT) {
			return "", fmt.Errorf("inspect %q: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing parent directory for %q", dir)
		}
		existing = parent
	}
	tmp, err := os.MkdirTemp(existing, ".noc-doctor-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(tmp); err != nil {
		return "", err
	}
	if existing == dir {
		return "directory exists", nil
	}
	return fmt.Sprintf("nearest existing parent %s can create it", existing), nil
}
