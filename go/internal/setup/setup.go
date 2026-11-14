// Package setup orchestrates installing, checking, and removing noc hooks.
package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"noc.local/noc/internal/hookconfig"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// Action is a setup operation.
type Action string

const (
	ActionInstall   Action = "install"
	ActionCheck     Action = "check"
	ActionUninstall Action = "uninstall"
)

// Options selects what Run does.
type Options struct {
	Action     Action
	Providers  []apiv1.Provider
	Executable string
}

// Result is the per-provider outcome.
type Result struct {
	Provider   apiv1.Provider        `json:"provider"`
	Path       string                `json:"path,omitempty"`
	Changed    bool                  `json:"changed,omitempty"`
	Removed    int                   `json:"removed,omitempty"`
	Complete   bool                  `json:"complete"`
	Inspection hookconfig.Inspection `json:"inspection"`
	Error      string                `json:"error,omitempty"`
}

// Report is the full setup outcome.
type Report struct {
	SchemaVersion  int      `json:"schema_version"`
	Action         Action   `json:"action"`
	ExecutablePath string   `json:"executable_path"`
	Results        []Result `json:"results"`
}

// HasFailures reports whether any result failed.
func (r Report) HasFailures() bool {
	for _, res := range r.Results {
		if res.Error != "" {
			return true
		}
		if r.Action != ActionUninstall && !res.Complete {
			return true
		}
	}
	return false
}

// Run performs the requested action using the environment's configuration paths.
func Run(ctx context.Context, opts Options) (Report, error) {
	paths, err := hookconfig.ResolvePaths()
	if err != nil {
		return Report{}, err
	}
	return run(ctx, opts, paths)
}

func run(ctx context.Context, opts Options, paths hookconfig.Paths) (Report, error) {
	action := opts.Action
	if action == "" {
		action = ActionInstall
	}
	switch action {
	case ActionInstall, ActionCheck, ActionUninstall:
	default:
		return Report{}, fmt.Errorf("unsupported setup action %q", action)
	}
	providers := opts.Providers
	if len(providers) == 0 {
		providers = []apiv1.Provider{apiv1.ProviderClaude, apiv1.ProviderCodex}
	}
	for _, p := range providers {
		if p != apiv1.ProviderClaude && p != apiv1.ProviderCodex {
			return Report{}, fmt.Errorf("unsupported setup provider %q", p)
		}
	}
	exe, err := resolveExecutable(opts.Executable)
	if err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: 1, Action: action, ExecutablePath: exe}
	for _, p := range providers {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		res := Result{Provider: p}
		var change hookconfig.Change
		var opErr error
		switch action {
		case ActionInstall:
			change, opErr = hookconfig.Install(p, exe, paths)
		case ActionUninstall:
			change, opErr = hookconfig.Uninstall(p, paths)
		}
		if opErr != nil {
			res.Error = opErr.Error()
		} else {
			res.Path, res.Changed, res.Removed = change.Path, change.Changed, change.Removed
		}
		insp, inspErr := hookconfig.Inspect(p, exe, paths)
		if inspErr != nil && res.Error == "" {
			res.Error = inspErr.Error()
		}
		res.Inspection = insp
		res.Complete = insp.Complete()
		if action == ActionUninstall && insp.ManagedInCodexConfig {
			res.Error = fmt.Sprintf("%s still contains a noc hook and was not modified", paths.CodexConfig)
		}
		report.Results = append(report.Results, res)
	}
	return report, nil
}

func resolveExecutable(path string) (string, error) {
	if path == "" {
		var err error
		if path, err = os.Executable(); err != nil {
			return "", fmt.Errorf("resolve noc executable: %w", err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve noc executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect noc executable %q: %w", abs, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("noc executable %q is not an executable regular file", abs)
	}
	return abs, nil
}
