package proclineage

import (
	"os/exec"
	"testing"
)

func TestProviderProcessIDWalksToAnAncestor(t *testing.T) {
	if got := ProviderProcessID(); got != 0 {
		t.Fatalf("no names = %d", got)
	}
	if got := ProviderProcessID("definitely-not-a-real-process-name"); got != 0 {
		t.Fatalf("unmatched = %d", got)
	}
	if got := ProviderProcessID("  "); got != 0 {
		t.Fatalf("blank = %d", got)
	}
}

func TestFindAncestorNearestMatchWins(t *testing.T) {
	table := ParseTable(`
 500     1 launchd
 600   500 claude
 700   600 codex
 800   700 zsh
 900   800 noc
 bad   1 x
 901
`)
	if got := FindAncestor(table, 900, "claude", "codex"); got != 700 {
		t.Fatalf("nearest = %d, want 700", got)
	}
	if got := FindAncestor(table, 900, "claude"); got != 600 {
		t.Fatalf("claude = %d, want 600", got)
	}
	if got := FindAncestor(table, 900, "node"); got != 0 {
		t.Fatalf("node = %d", got)
	}
	// The process itself is never matched.
	if got := FindAncestor(table, 600, "claude"); got != 0 {
		t.Fatalf("self = %d", got)
	}
	// Unknown pid and parent <= 1 stop the walk.
	if got := FindAncestor(table, 12345, "claude"); got != 0 {
		t.Fatalf("unknown pid = %d", got)
	}
	if got := FindAncestor(table, 500, "launchd"); got != 0 {
		t.Fatalf("parent<=1 matched: %d", got)
	}
}

func TestFindAncestorLimitsDepth(t *testing.T) {
	table := Table{Parent: map[int]int{}, Name: map[int]string{}}
	for pid := 100; pid < 120; pid++ {
		table.Parent[pid] = pid + 1
		table.Name[pid] = "sh"
	}
	table.Name[113] = "claude" // 13th ancestor of 100
	if got := FindAncestor(table, 100, "claude"); got != 0 {
		t.Fatalf("beyond depth = %d", got)
	}
	table.Name[113] = "sh"
	table.Name[112] = "claude" // 12th ancestor
	if got := FindAncestor(table, 100, "claude"); got != 112 {
		t.Fatalf("at depth = %d", got)
	}
}

func TestCommandNameReducesToABareExecutable(t *testing.T) {
	cases := map[string]string{
		"/opt/x/bin/codex":   "codex",
		"claude bg-pty-host": "claude",
		"  claude  ":         "claude",
		"":                   "",
		"node":               "node",
	}
	for in, want := range cases {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsRunningDetectsAnExitedProcess(t *testing.T) {
	for _, pid := range []int{-5, 0, 1} {
		if IsRunning(pid) {
			t.Errorf("IsRunning(%d) = true", pid)
		}
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if !IsRunning(pid) {
		t.Fatal("running process reported gone")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if IsRunning(pid) {
		t.Fatal("exited process reported running")
	}
}
