// Package workspace classifies a host-local directory as a plain directory, a
// main Git checkout, or a linked worktree. It is read-only and never fails.
package workspace

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	inspectTimeout = 750 * time.Millisecond
	cacheTTL       = 15 * time.Second

	KindDirectory    = "directory"
	KindMainCheckout = "main_checkout"
	KindWorktree     = "worktree"
)

type cacheEntry struct {
	workspace apiv1.Workspace
	checkedAt time.Time
}

// Inspector inspects directories with the git found on PATH and caches results
// briefly. One inspector may be shared by many goroutines.
type Inspector struct {
	git   string
	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewInspector returns an inspector that runs the git found on PATH.
func NewInspector() *Inspector {
	return &Inspector{git: "git", cache: map[string]cacheEntry{}}
}

// Inspect describes path. It returns nil for a blank path and never errors:
// any Git failure yields a directory workspace.
func (i *Inspector) Inspect(ctx context.Context, path, owner string, observedAt time.Time) *apiv1.Workspace {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	key := cacheKey(path)
	if ws, ok := i.cached(key, owner, observedAt); ok {
		return &ws
	}
	ws := apiv1.Workspace{
		Path:       filepath.Clean(path),
		Kind:       KindDirectory,
		Owner:      owner,
		ObservedAt: observedAt.UTC(),
	}
	out, err := i.run(ctx, "-C", path, "rev-parse", "--path-format=absolute",
		"--show-toplevel", "--git-common-dir", "--git-dir", "HEAD")
	if err == nil {
		if lines := nonEmptyLines(out); len(lines) == 4 {
			ws.RootPath = filepath.Clean(lines[0])
			ws.CommonGitDir = filepath.Clean(lines[1])
			gitDir := filepath.Clean(lines[2])
			ws.HeadSHA = lines[3]
			if samePath(ws.CommonGitDir, gitDir) {
				ws.Kind = KindMainCheckout
			} else {
				ws.Kind = KindWorktree
			}
			if branch, berr := i.run(ctx, "-C", path, "symbolic-ref", "--short", "-q", "HEAD"); berr == nil {
				ws.Branch = strings.TrimSpace(branch)
			}
		}
	}
	i.remember(key, ws)
	return &ws
}

func (i *Inspector) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, i.git, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func cacheKey(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func (i *Inspector) cached(key, owner string, observedAt time.Time) (apiv1.Workspace, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, ok := i.cache[key]
	if !ok || time.Since(entry.checkedAt) >= cacheTTL {
		return apiv1.Workspace{}, false
	}
	ws := entry.workspace
	ws.Owner = owner
	ws.ObservedAt = observedAt.UTC()
	return ws, true
}

func (i *Inspector) remember(key string, ws apiv1.Workspace) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cache[key] = cacheEntry{workspace: ws, checkedAt: time.Now()}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if resolved, err := filepath.EvalSymlinks(absA); err == nil {
		absA = resolved
	}
	if resolved, err := filepath.EvalSymlinks(absB); err == nil {
		absB = resolved
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}
