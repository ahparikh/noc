package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestInspectorClassifiesMainCheckoutAndLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "f")
	git(t, repo, "commit", "-q", "-m", "init")
	linked := filepath.Join(root, "linked")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", linked)

	in := NewInspector()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	main := in.Inspect(context.Background(), repo, "owner", at)
	if main == nil || main.Kind != KindMainCheckout || main.Branch != "main" || len(main.HeadSHA) < 40 || main.RootPath != repo {
		t.Fatalf("main checkout = %+v", main)
	}
	if main.ID != "" || main.RepositoryID != "" {
		t.Fatalf("id fields must be empty: %+v", main)
	}
	wt := in.Inspect(context.Background(), linked, "owner", at)
	if wt == nil || wt.Kind != KindWorktree || wt.Branch != "feature" || wt.RootPath != linked {
		t.Fatalf("worktree = %+v", wt)
	}
	if wt.CommonGitDir != main.CommonGitDir {
		t.Fatalf("common dirs differ: %q vs %q", wt.CommonGitDir, main.CommonGitDir)
	}
}

func TestInspectorFallsBackToDirectory(t *testing.T) {
	in := NewInspector()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := in.Inspect(context.Background(), "   ", "o", at); got != nil {
		t.Fatalf("blank path = %+v", got)
	}
	if len(in.cache) != 0 {
		t.Fatal("blank path must not cache")
	}
	dir := t.TempDir() + "/sub/../plain"
	got := in.Inspect(context.Background(), dir, "o", at)
	if got == nil || got.Kind != KindDirectory || got.Path != filepath.Clean(dir) || got.RootPath != "" || got.HeadSHA != "" || got.Owner != "o" {
		t.Fatalf("directory = %+v", got)
	}
	missing := in.Inspect(context.Background(), "/definitely/not/here", "o", at)
	if missing == nil || missing.Kind != KindDirectory {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestInspectorCacheHitOverridesOwnerAndObservedAt(t *testing.T) {
	in := NewInspector()
	dir := t.TempDir()
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := t1.Add(5 * time.Second)
	first := in.Inspect(context.Background(), dir, "one", t1)
	// Break git: a cache hit must not need it.
	in.git = "/nonexistent/git"
	second := in.Inspect(context.Background(), dir, "two", t2)
	if second.Owner != "two" || !second.ObservedAt.Equal(t2) || second.Path != first.Path || first.Owner != "one" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	// Expired entries are ignored.
	key := cacheKey(dir)
	entry := in.cache[key]
	entry.checkedAt = time.Now().Add(-cacheTTL - time.Second)
	in.cache[key] = entry
	third := in.Inspect(context.Background(), dir, "three", t2)
	if third.Owner != "three" || third.Kind != KindDirectory {
		t.Fatalf("third = %+v", third)
	}
	if time.Since(in.cache[key].checkedAt) > time.Second {
		t.Fatal("expired entry not overwritten")
	}
}
