package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathsHonorsOverrides(t *testing.T) {
	t.Setenv("NOC_STATE_DIR", "/tmp/noc-state/")
	t.Setenv("NOC_RUNTIME_DIR", "/tmp/noc-run/./")
	t.Setenv("NOC_SOCKET", "")
	t.Setenv("NOC_DATABASE", "")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != "/tmp/noc-state" || p.RuntimeDir != "/tmp/noc-run" {
		t.Fatalf("dirs: %+v", p)
	}
	if p.SocketPath != "/tmp/noc-run/noc.sock" || p.DatabasePath != "/tmp/noc-state/noc.db" {
		t.Fatalf("derived: %+v", p)
	}
	t.Setenv("NOC_SOCKET", "/x/s.sock")
	t.Setenv("NOC_DATABASE", "/y/d.db")
	p, err = ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.SocketPath != "/x/s.sock" || p.DatabasePath != "/y/d.db" {
		t.Fatalf("explicit: %+v", p)
	}
}

func TestResolvePathsDefaults(t *testing.T) {
	t.Setenv("NOC_STATE_DIR", "")
	t.Setenv("NOC_RUNTIME_DIR", "")
	t.Setenv("NOC_SOCKET", "")
	t.Setenv("NOC_DATABASE", "")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	t.Setenv("XDG_RUNTIME_DIR", "/xdg/run")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != "/xdg/state/noc" || p.RuntimeDir != "/xdg/run/noc" {
		t.Fatalf("xdg: %+v", p)
	}
}

func TestEnsurePrivateDirDoesNotChmodExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory was chmodded to %v", info.Mode().Perm())
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if err := EnsurePrivateDir(file); err == nil {
		t.Fatal("expected an error for a non-directory")
	}
}

func TestEnsurePrivateDirSecuresNewDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("new directory mode %v", info.Mode().Perm())
	}
}
