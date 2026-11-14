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

func TestResolveExecutable(t *testing.T) {
	_, exe := fixture(t)
	if got, err := resolveExecutable(exe); err != nil || got != exe {
		t.Fatalf("%q %v", got, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveExecutable(link); err != nil || got != exe {
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
