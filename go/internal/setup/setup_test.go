package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noc.local/noc/internal/hookconfig"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// fixture isolates all provider configuration under a temp directory.
func fixture(t *testing.T) (hookconfig.Paths, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	paths, err := hookconfig.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "bin", "noc")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	return paths, resolved
}

func TestInstallCheckAndUninstall(t *testing.T) {
	paths, exe := fixture(t)
	ctx := context.Background()

	rep, err := run(ctx, Options{Action: ActionInstall, Executable: exe}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SchemaVersion != 1 || rep.ExecutablePath != exe || len(rep.Results) != 2 || rep.HasFailures() {
		t.Fatalf("install: %+v", rep)
	}
	for _, r := range rep.Results {
		if !r.Changed || !r.Complete || r.Path == "" {
			t.Fatalf("install result: %+v", r)
		}
	}

	rep, err = run(ctx, Options{Action: ActionCheck, Executable: exe}, paths)
	if err != nil || rep.HasFailures() || rep.Results[0].Changed {
		t.Fatalf("check: %+v %v", rep, err)
	}

	rep, err = run(ctx, Options{Action: ActionUninstall, Executable: exe}, paths)
	if err != nil || rep.HasFailures() {
		t.Fatalf("uninstall: %+v %v", rep, err)
	}
	if rep.Results[0].Removed != 9 || rep.Results[1].Removed != 6 {
		t.Fatalf("removed: %+v", rep.Results)
	}
}

func TestCheckReportsMissingHooks(t *testing.T) {
	paths, exe := fixture(t)
	rep, err := run(context.Background(), Options{Action: ActionCheck, Executable: exe, Providers: []apiv1.Provider{apiv1.ProviderClaude}}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasFailures() || len(rep.Results) != 1 || rep.Results[0].Complete || len(rep.Results[0].Inspection.MissingEvents) != 9 {
		t.Fatalf("%+v", rep)
	}
}

func TestInstallFailsWhenCodexHooksDisabled(t *testing.T) {
	paths, exe := fixture(t)
	if err := os.MkdirAll(filepath.Dir(paths.CodexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CodexConfig, []byte("[features]\ncodex_hooks = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := run(context.Background(), Options{Providers: []apiv1.Provider{apiv1.ProviderCodex}, Executable: exe}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Action != ActionInstall || !rep.HasFailures() || rep.Results[0].Error != "" || !rep.Results[0].Changed {
		t.Fatalf("%+v", rep)
	}
}

func TestUninstallReportsTOMLRemnant(t *testing.T) {
	paths, exe := fixture(t)
	if err := os.MkdirAll(filepath.Dir(paths.CodexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "[[hooks.Stop]]\ncommand = '/x/noc hook ingest --provider codex'\n"
	if err := os.WriteFile(paths.CodexConfig, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := run(context.Background(), Options{Action: ActionUninstall, Providers: []apiv1.Provider{apiv1.ProviderCodex}, Executable: exe}, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := paths.CodexConfig + " still contains a noc hook and was not modified"
	if rep.Results[0].Error != want || !rep.HasFailures() {
		t.Fatalf("%+v", rep.Results[0])
	}
	if b, _ := os.ReadFile(paths.CodexConfig); string(b) != toml {
		t.Fatal("config.toml modified")
	}
}

func TestRunValidation(t *testing.T) {
	paths, exe := fixture(t)
	ctx := context.Background()
	if _, err := run(ctx, Options{Action: "bogus", Executable: exe}, paths); err == nil || !strings.Contains(err.Error(), "unsupported setup action") {
		t.Fatal(err)
	}
	if _, err := run(ctx, Options{Providers: []apiv1.Provider{"x"}, Executable: exe}, paths); err == nil || !strings.Contains(err.Error(), "unsupported setup provider") {
		t.Fatal(err)
	}
}

// The hooks must name the path the user runs, not the versioned file a
// package manager's symlink points at today.
func TestHooksSurviveAnUpgradeBehindASymlink(t *testing.T) {
	paths, _ := fixture(t)
	root := t.TempDir()
	version := func(v string) string {
		exe := filepath.Join(root, "Cellar", v, "bin", "noc")
		if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	old := version("0.1.0")
	stable := filepath.Join(root, "bin", "noc")
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, stable); err != nil {
		t.Fatal(err)
	}
	onPath := func(string) (string, error) { return stable, nil }

	// Started as `noc`, found on PATH as the symlink.
	if got := invokedName("noc", old, onPath); got != stable {
		t.Fatalf("bare name: %q", got)
	}
	if got := invokedName(stable, old, onPath); got != stable {
		t.Fatalf("path: %q", got)
	}
	// A name that leads somewhere else, or nowhere, is not trusted.
	other := version("9.9.9")
	if got := invokedName(other, old, onPath); got != old {
		t.Fatalf("other file: %q", got)
	}
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	if got := invokedName("noc", old, missing); got != old {
		t.Fatalf("not on PATH: %q", got)
	}

	ctx := context.Background()
	report, err := run(ctx, Options{Action: ActionInstall, Providers: []apiv1.Provider{apiv1.ProviderClaude}, Executable: stable}, paths)
	if err != nil || report.ExecutablePath != stable {
		t.Fatalf("install: %q %v", report.ExecutablePath, err)
	}
	data, err := os.ReadFile(paths.ClaudeSettings)
	if err != nil || !strings.Contains(string(data), stable) || strings.Contains(string(data), "Cellar") {
		t.Fatalf("settings name the versioned file: %s %v", data, err)
	}

	// The upgrade: a new version, the symlink moved, the old one removed.
	next := version("0.2.0")
	if err := os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(next, stable); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(old))); err != nil {
		t.Fatal(err)
	}
	check, err := run(ctx, Options{Action: ActionCheck, Providers: []apiv1.Provider{apiv1.ProviderClaude}, Executable: next}, paths)
	if err != nil || len(check.Results) != 1 || !check.Results[0].Complete {
		t.Fatalf("hooks did not survive the upgrade: %+v %v", check.Results, err)
	}
}

func TestResolveExecutable(t *testing.T) {
	_, exe := fixture(t)
	if got, err := resolveExecutable(exe); err != nil || got != exe {
		t.Fatalf("%q %v", got, err)
	}
	// A symlink is kept: it is the name that survives an upgrade.
	link := filepath.Join(t.TempDir(), "noc")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveExecutable(link); err != nil || got != link {
		t.Fatalf("symlink: %q %v", got, err)
	}
	if _, err := resolveExecutable(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.HasPrefix(err.Error(), "inspect noc executable ") {
		t.Fatal(err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(plain, nil, 0o644)
	if _, err := resolveExecutable(plain); err == nil || !strings.Contains(err.Error(), "is not an executable regular file") {
		t.Fatal(err)
	}
	if _, err := resolveExecutable(t.TempDir()); err == nil || !strings.Contains(err.Error(), "is not an executable regular file") {
		t.Fatal(err)
	}
}

func TestRunUsesEnvironmentPaths(t *testing.T) {
	paths, exe := fixture(t)
	rep, err := Run(context.Background(), Options{Executable: exe, Providers: []apiv1.Provider{apiv1.ProviderClaude}})
	if err != nil || rep.HasFailures() || rep.Results[0].Path != paths.ClaudeSettings {
		t.Fatalf("%+v %v", rep, err)
	}
}
