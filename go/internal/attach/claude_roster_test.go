package attach

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func intPtr(v int) *int { return &v }

func TestPlanClaudeWithRosterKeepsBackgroundSessionsInTheBackground(t *testing.T) {
	agent := apiv1.Agent{
		Provider: apiv1.ProviderClaude, NativeSessionID: " sess ",
		Workspace: &apiv1.Workspace{Path: " /work "},
	}
	t.Run("interactive running refuses", func(t *testing.T) {
		roster := []ClaudeSession{{SessionID: "sess", Kind: "interactive", PID: intPtr(42)}}
		_, err := PlanClaudeWithRoster(agent, roster, "claude", false)
		if !errors.Is(err, ErrRunningInTerminal) {
			t.Fatalf("err = %v, want ErrRunningInTerminal", err)
		}
		want := "this Claude session is already running in a terminal (pid 42); focus that terminal instead"
		if err.Error() != want {
			t.Fatalf("err = %q, want %q", err.Error(), want)
		}
	})
	t.Run("interactive not running is ignored", func(t *testing.T) {
		for _, pid := range []*int{nil, intPtr(0), intPtr(-1)} {
			roster := []ClaudeSession{{SessionID: "sess", Kind: "interactive", PID: pid}}
			plan, err := PlanClaudeWithRoster(agent, roster, "claude", false)
			if err != nil || plan.Mode != ModeBackgroundResume {
				t.Fatalf("plan = %#v, err = %v", plan, err)
			}
		}
	})
	t.Run("force ignores interactive", func(t *testing.T) {
		roster := []ClaudeSession{
			{SessionID: "sess", Kind: "interactive", PID: intPtr(42)},
			{ID: " job-1 ", SessionID: " sess ", Kind: "background"},
		}
		got, err := PlanClaudeWithRoster(agent, roster, " claude ", true)
		if err != nil {
			t.Fatal(err)
		}
		want := Plan{Program: "claude", Arguments: []string{"attach", "job-1"}, Directory: "/work", Mode: ModeAttach}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("plan = %#v, want %#v", got, want)
		}
	})
	t.Run("last background wins", func(t *testing.T) {
		roster := []ClaudeSession{
			{ID: "first", SessionID: "sess", Kind: "background", State: "running"},
			{ID: "other", SessionID: "different", Kind: "background"},
			{ID: "last", SessionID: "sess", Kind: "background", State: "stopped"},
			{ID: "  ", SessionID: "sess", Kind: "background"},
		}
		got, err := PlanClaudeWithRoster(agent, roster, "claude", false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Arguments, []string{"attach", "last"}) || got.Mode != ModeAttach {
			t.Fatalf("plan = %#v", got)
		}
	})
	t.Run("running interactive refuses even with background entry", func(t *testing.T) {
		roster := []ClaudeSession{
			{ID: "job", SessionID: "sess", Kind: "background"},
			{SessionID: "sess", Kind: "interactive", PID: intPtr(7)},
		}
		if _, err := PlanClaudeWithRoster(agent, roster, "claude", false); !errors.Is(err, ErrRunningInTerminal) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no entry restores in the background", func(t *testing.T) {
		roster := []ClaudeSession{{ID: "x", SessionID: "other", Kind: "background"}}
		got, err := PlanClaudeWithRoster(agent, roster, "claude", false)
		if err != nil {
			t.Fatal(err)
		}
		want := Plan{Program: "claude", Arguments: []string{"--bg", "--resume", "sess"}, Directory: "/work", Mode: ModeBackgroundResume}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("plan = %#v, want %#v", got, want)
		}
	})
	t.Run("blank session", func(t *testing.T) {
		_, err := PlanClaudeWithRoster(apiv1.Agent{NativeSessionID: " "}, nil, "claude", false)
		if err == nil || err.Error() != "agent has no native session ID" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBackgroundJobForFindsTheRestoredJob(t *testing.T) {
	roster := []ClaudeSession{
		{ID: "i", SessionID: "sess", Kind: "interactive"},
		{ID: " first ", SessionID: " sess ", Kind: "background"},
		{ID: "second", SessionID: "sess", Kind: "background"},
		{ID: "z", SessionID: "other", Kind: "background"},
	}
	if got := BackgroundJobFor(roster, " sess "); got != "first" {
		t.Fatalf("job = %q, want first", got)
	}
	if got := BackgroundJobFor(roster, "missing"); got != "" {
		t.Fatalf("job = %q, want empty", got)
	}
	blank := []ClaudeSession{{ID: " ", SessionID: "sess", Kind: "background"}, {ID: "later", SessionID: "sess", Kind: "background"}}
	if got := BackgroundJobFor(blank, "sess"); got != "" {
		t.Fatalf("blank-id first entry should yield empty, got %q", got)
	}
}

func writeFake(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadClaudeRoster(t *testing.T) {
	t.Run("decodes entries and ignores unknown fields and stderr", func(t *testing.T) {
		bin := writeFake(t, `[ "$*" = "agents --json --all" ] || exit 9
echo noise >&2
cat <<'JSON'
[{"id":"j1","sessionId":"s1","kind":"background","state":"running","pid":12,"extra":true},
 {"id":"","sessionId":"s2","kind":"interactive","state":"idle","pid":null}]
JSON`)
		roster, err := ReadClaudeRoster(context.Background(), bin)
		if err != nil {
			t.Fatal(err)
		}
		if len(roster) != 2 || roster[0].ID != "j1" || roster[0].PID == nil || *roster[0].PID != 12 ||
			roster[1].SessionID != "s2" || roster[1].PID != nil {
			t.Fatalf("roster = %#v", roster)
		}
	})
	t.Run("null is empty", func(t *testing.T) {
		roster, err := ReadClaudeRoster(context.Background(), writeFake(t, "echo null"))
		if err != nil || len(roster) != 0 {
			t.Fatalf("roster = %#v, err = %v", roster, err)
		}
	})
	t.Run("exit status", func(t *testing.T) {
		_, err := ReadClaudeRoster(context.Background(), writeFake(t, "exit 1"))
		if err == nil || !strings.HasPrefix(err.Error(), "list Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		_, err := ReadClaudeRoster(context.Background(), filepath.Join(t.TempDir(), "nope"))
		if err == nil || !strings.HasPrefix(err.Error(), "list Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		_, err := ReadClaudeRoster(context.Background(), writeFake(t, "echo not-json"))
		if err == nil || !strings.HasPrefix(err.Error(), "decode Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not an array", func(t *testing.T) {
		_, err := ReadClaudeRoster(context.Background(), writeFake(t, `echo '{"id":"x"}'`))
		if err == nil || !strings.HasPrefix(err.Error(), "decode Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits for the real roster timeout")
		}
		start := time.Now()
		_, err := ReadClaudeRoster(context.Background(), writeFake(t, "exec sleep 30"))
		if err == nil || !strings.HasPrefix(err.Error(), "list Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(start); elapsed > RosterTimeout+3*time.Second {
			t.Fatalf("took %v, want about %v", elapsed, RosterTimeout)
		}
	})
	t.Run("cancelled context errors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ReadClaudeRoster(ctx, writeFake(t, "echo '[]'"))
		if err == nil || !strings.HasPrefix(err.Error(), "list Claude sessions: ") {
			t.Fatalf("err = %v", err)
		}
	})
}
