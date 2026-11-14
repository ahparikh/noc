package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/registry"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// outsideAnAgent makes the process-lineage lookup find nothing, so a test
// behaves the same whether or not it is itself run by a provider.
func outsideAnAgent(t *testing.T) {
	t.Helper()
	previous := providerProcessID
	providerProcessID = func() int { return 0 }
	t.Cleanup(func() { providerProcessID = previous })
}

// codexAgents ingests one hook per thread and returns the agents' IDs by thread.
func codexAgents(t *testing.T, cwd string, threads ...string) map[string]string {
	t.Helper()
	for _, thread := range threads {
		ingest(t, "codex", `{"session_id":"`+thread+`","hook_event_name":"UserPromptSubmit","cwd":"`+cwd+`","prompt":"hi"}`)
	}
	ids := map[string]string{}
	waitFor(t, "codex agents", func() bool {
		for _, agent := range listAgents(t).Agents {
			ids[agent.NativeSessionID] = agent.ID
		}
		return len(ids) == len(threads)
	})
	return ids
}

func showAgent(t *testing.T, id string) apiv1.Agent {
	t.Helper()
	code, stdout, stderr := runCLI(t, "show", "--json", id)
	if code != 0 {
		t.Fatalf("show %s = %d: %s", id, code, stderr)
	}
	var agent apiv1.Agent
	if err := json.Unmarshal([]byte(stdout), &agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

// sameDirectory compares two paths after resolving symbolic links, since a
// temporary directory is often reached through one.
func sameDirectory(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func TestValidTeamNames(t *testing.T) {
	for name, want := range map[string]bool{
		"auth": true, "auth-migration": true, "a.b_c-9": true, "A": true, strings.Repeat("x", 48): true,
		"": false, "none": false, "-auth": false, ".auth": false, "a b": false, "a/b": false,
		"é": false, strings.Repeat("x", 49): false, "None": true,
	} {
		if got := apiv1.ValidTeamName(name); got != want {
			t.Errorf("ValidTeamName(%q) = %v", name, got)
		}
	}
}

func TestTeamCommandsListsAndFilters(t *testing.T) {
	dir := isolate(t)
	outsideAnAgent(t)
	startServer(t)
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	ids := codexAgents(t, cwd, "thread-a", "thread-b", "thread-c")
	a, b, c := ids["thread-a"], ids["thread-b"], ids["thread-c"]

	// With no team anywhere the table is the one it always was.
	if _, stdout, _ := runCLI(t, "ls"); strings.Contains(stdout, "TEAM") {
		t.Fatalf("no TEAM column expected:\n%s", stdout)
	}

	code, stdout, stderr := runCLI(t, "team", "set", "auth", a, b)
	if code != 0 || stdout != a+" is in team auth\n"+b+" is in team auth\n" {
		t.Fatalf("team set = %d %q %q", code, stdout, stderr)
	}
	if code, _, stderr := runCLI(t, "team", "lead", "auth", c); code != 1 || stderr != "noc: "+c+" is not in team auth\n" {
		t.Fatalf("lead outside the team = %d %q", code, stderr)
	}
	if code, stdout, _ := runCLI(t, "team", "lead", "auth", a); code != 0 || stdout != a+" leads team auth\n" {
		t.Fatalf("team lead = %d %q", code, stdout)
	}

	_, stdout, _ = runCLI(t, "ls")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if fields := strings.Fields(lines[0]); len(fields) < 2 || fields[0] != "ID" || fields[1] != "TEAM" {
		t.Fatalf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		want := map[string]string{a: "auth*", b: "auth", c: "-"}[fields[0]]
		if fields[1] != want {
			t.Fatalf("TEAM cell for %s = %q, want %q", fields[0], fields[1], want)
		}
	}
	if got := listAgents(t, "--team", "auth").Agents; len(got) != 2 {
		t.Fatalf("--team auth kept %d", len(got))
	}
	if got := listAgents(t, "--team", "none").Agents; len(got) != 1 || got[0].ID != c {
		t.Fatalf("--team none kept %+v", got)
	}
	if got := listAgents(t, "--team", "nobody").Agents; len(got) != 0 {
		t.Fatalf("--team nobody kept %d", len(got))
	}
	if agent := showAgent(t, a); agent.Team != "auth" || !agent.Lead {
		t.Fatalf("show: %+v", agent)
	}
	if _, stdout, _ := runCLI(t, "show", a); !strings.Contains(stdout, "auth (lead)") {
		t.Fatalf("show text:\n%s", stdout)
	}

	// noc teams: every team by name, then the agents in none.
	_, stdout, _ = runCLI(t, "teams")
	rows := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(rows) != 3 || strings.Join(strings.Fields(rows[0]), " ") != "TEAM LEAD AGENTS BLOCKED WORKING REVIEW QUIET" ||
		!strings.HasPrefix(rows[1], "auth ") || !strings.HasPrefix(rows[2], "(none) ") {
		t.Fatalf("teams:\n%s", stdout)
	}
	code, stdout, _ = runCLI(t, "teams", "--json")
	var teams apiv1.TeamList
	if err := json.Unmarshal([]byte(stdout), &teams); code != 0 || err != nil {
		t.Fatalf("teams --json = %d %v", code, err)
	}
	if teams.SchemaVersion != 1 || teams.GeneratedAt.IsZero() || len(teams.Teams) != 2 ||
		teams.Teams[0].Name != "auth" || teams.Teams[0].LeadID != a || teams.Teams[0].Agents != 2 ||
		teams.Teams[1].Name != "" || teams.Teams[1].Agents != 1 || teams.Teams[1].LeadID != "" {
		t.Fatalf("teams --json: %+v", teams)
	}

	// Search: the name is text, team:<name> is exact, and --team filters.
	search := func(args ...string) int {
		t.Helper()
		code, stdout, stderr := runCLI(t, append([]string{"search", "--json"}, args...)...)
		var list apiv1.AgentList
		if err := json.Unmarshal([]byte(stdout), &list); code != 0 || err != nil {
			t.Fatalf("search %v = %d %v %s", args, code, err, stderr)
		}
		return len(list.Agents)
	}
	for query, want := range map[string]int{"auth": 2, "team:auth": 2, "team:aut": 0, "team:none": 1, "team:AUTH": 2} {
		if got := search(query); got != want {
			t.Errorf("search %q = %d, want %d", query, got, want)
		}
	}
	if got := search("--team", "auth", "thread-a"); got != 1 {
		t.Errorf("search --team auth thread-a = %d", got)
	}

	// With no ID the command acts on the agent it runs inside.
	if code, _, stderr := runCLI(t, "team", "set", "auth"); code != 2 || !strings.Contains(stderr, "not running inside an agent") {
		t.Fatalf("no caller = %d %q", code, stderr)
	}
	t.Setenv("NOC_AGENT_ID", c)
	if code, stdout, _ := runCLI(t, "team", "set", "billing"); code != 0 || stdout != c+" is in team billing\n" {
		t.Fatalf("set self = %d %q", code, stdout)
	}
	if code, stdout, _ := runCLI(t, "team", "lead", "billing"); code != 0 || stdout != c+" leads team billing\n" {
		t.Fatalf("lead self = %d %q", code, stdout)
	}
	if code, stdout, _ := runCLI(t, "team", "clear"); code != 0 || stdout != c+" is in no team\n" {
		t.Fatalf("clear self = %d %q", code, stdout)
	}
	if agent := showAgent(t, c); agent.Team != "" || agent.Lead {
		t.Fatalf("after clear: %+v", agent)
	}

	// Usage and validation stop before the server is asked.
	for _, args := range [][]string{
		{"team"}, {"team", "bogus"}, {"team", "set"}, {"team", "set", "none", a}, {"team", "set", "a b", a},
		{"team", "lead"}, {"team", "lead", "auth", a, b}, {"ls", "--team", "a b"},
		{"search", "--team", "-x"}, {"watch", "--team", "a/b"}, {"teams", "extra"},
	} {
		if code, _, stderr := runCLI(t, args...); code != 2 || stderr == "" {
			t.Errorf("%v = %d %q", args, code, stderr)
		}
	}
	if code, _, stderr := runCLI(t, "team", "set", "auth", "missing"); code != 1 || !strings.Contains(stderr, "agent was not found") {
		t.Fatalf("missing agent = %d %q", code, stderr)
	}
}

func TestSpawnStartsClaudeInTheBackground(t *testing.T) {
	dir := isolate(t)
	outsideAnAgent(t)
	startServer(t)
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(work)
	boss := codexAgents(t, work, "thread-boss")["thread-boss"]

	logPath := filepath.Join(dir, "claude.log")
	fake := filepath.Join(dir, "fake-claude")
	// The fake starts "sessions" job1, job2, ... and lists them as Claude
	// does: the session ID is Claude's choice, learned from its roster.
	rosterPath := filepath.Join(dir, "roster")
	script := `#!/bin/sh
if [ "$1" = agents ]; then
  printf '[{"id":"old1","sessionId":"session-old1","kind":"background"}'
  cat ` + rosterPath + ` 2>/dev/null
  echo ']'
  exit 0
fi
n=$(($(cat ` + rosterPath + `.count 2>/dev/null || echo 0) + 1))
echo $n > ` + rosterPath + `.count
printf ',{"id":"job%s","sessionId":"session-job%s","kind":"background"}' $n $n >> ` + rosterPath + `
echo "$(pwd -P) :: $*" >> ` + logPath + `
echo "Started background session job$n. Attach with: claude attach job$n"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_CLAUDE_BIN", fake)

	// From inside an agent that is in no team: the worker joins the named
	// team, records its parent, and the spawner becomes the lead.
	t.Setenv("NOC_AGENT_ID", boss)
	code, stdout, stderr := runCLI(t, "spawn", "--team", "auth", "--name", "worker one", "--dir", work,
		"fix", "the tests", "--", "--model", "opus", "--permission-mode", "plan")
	if code != 0 || stderr != "" {
		t.Fatalf("spawn = %d %q %q", code, stdout, stderr)
	}
	id := strings.TrimSpace(stdout)
	worker := showAgent(t, id)
	if worker.Team != "auth" || worker.Lead || worker.ParentID != boss || worker.Provider != apiv1.ProviderClaude ||
		worker.Name != "worker one" || worker.Origin != "spawn" || worker.Workspace == nil || !sameDirectory(worker.Workspace.Path, resolved) ||
		worker.InitialRequest == nil || worker.InitialRequest.Text != "fix the tests" {
		t.Fatalf("worker: %+v", worker)
	}
	if lead := showAgent(t, boss); lead.Team != "auth" || !lead.Lead {
		t.Fatalf("spawner: %+v", lead)
	}
	logged, _ := os.ReadFile(logPath)
	want := resolved + " :: --bg --permission-mode auto --name worker one fix the tests --model opus --permission-mode plan"
	if worker.NativeSessionID != "session-job1" || strings.TrimSpace(string(logged)) != want {
		t.Fatalf("claude ran as\n%s\nwant\n%s", logged, want)
	}

	// No --team: the caller's team. The directory defaults to the current one.
	t.Chdir(work)
	_, stdout, _ = runCLI(t, "spawn", "write the docs")
	second := showAgent(t, strings.TrimSpace(stdout))
	if second.Team != "auth" || second.ParentID != boss || second.Name != "" || !sameDirectory(second.Workspace.Path, resolved) {
		t.Fatalf("inherited: %+v", second)
	}
	_, stdout, _ = runCLI(t, "spawn", "--team", "none", "something else")
	if third := showAgent(t, strings.TrimSpace(stdout)); third.Team != "" || third.ParentID != boss {
		t.Fatalf("opted out: %+v", third)
	}

	// From outside any agent there is no parent and nobody is made lead.
	t.Setenv("NOC_AGENT_ID", "")
	_, stdout, _ = runCLI(t, "spawn", "--team", "billing", "count the money")
	if fourth := showAgent(t, strings.TrimSpace(stdout)); fourth.Team != "billing" || fourth.ParentID != "" || fourth.Lead {
		t.Fatalf("outside an agent: %+v", fourth)
	}
	// A caller the registry does not know is not a parent.
	t.Setenv("NOC_AGENT_ID", "gone")
	_, stdout, _ = runCLI(t, "spawn", "--team", "billing", "count it again")
	if fifth := showAgent(t, strings.TrimSpace(stdout)); fifth.ParentID != "" {
		t.Fatalf("unknown caller: %+v", fifth)
	}
	t.Setenv("NOC_AGENT_ID", "")

	// Inside a Claude session the caller is that session, whatever
	// NOC_AGENT_ID was inherited from further out.
	t.Setenv("NOC_AGENT_ID", boss)
	t.Setenv("CLAUDE_CODE_SESSION_ID", second.NativeSessionID)
	_, stdout, _ = runCLI(t, "spawn", "a grandchild")
	if grandchild := showAgent(t, strings.TrimSpace(stdout)); grandchild.ParentID != second.ID || grandchild.Team != "auth" {
		t.Fatalf("spawned by a worker: %+v", grandchild)
	}
	t.Setenv("NOC_AGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	// A session the hooks reported first is the same agent, not a second one.
	ingest(t, "claude", `{"session_id":"session-job7","hook_event_name":"SessionStart","cwd":"`+work+`"}`)
	waitFor(t, "the hook's agent", func() bool {
		for _, agent := range listAgents(t).Agents {
			if agent.NativeSessionID == "session-job7" {
				return true
			}
		}
		return false
	})
	_, stdout, _ = runCLI(t, "spawn", "--team", "docs", "reported early")
	if early := showAgent(t, strings.TrimSpace(stdout)); early.NativeSessionID != "session-job7" || early.Team != "docs" || early.Origin != "hook" {
		t.Fatalf("adopted: %+v", early)
	}

	// A provider that does not start leaves nothing behind.
	before := len(listAgents(t, "--all").Agents)
	failing := filepath.Join(dir, "failing-claude")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_CLAUDE_BIN", failing)
	code, stdout, stderr = runCLI(t, "spawn", "--team", "auth", "never starts")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "boom") || !strings.Contains(stderr, "noc: start Claude in the background: ") {
		t.Fatalf("failed start = %d %q %q", code, stdout, stderr)
	}
	if after := len(listAgents(t, "--all").Agents); after != before {
		t.Fatalf("a failed spawn left a row: %d -> %d", before, after)
	}

	// A session noc cannot find in Claude's roster is reported, not guessed.
	mute := filepath.Join(dir, "mute-claude")
	if err := os.WriteFile(mute, []byte("#!/bin/sh\n[ \"$1\" = agents ] && echo '[]' && exit 0\necho started nothing-listed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOC_CLAUDE_BIN", mute)
	previousWait := spawnRosterWait
	spawnRosterWait = 300 * time.Millisecond
	code, stdout, stderr = runCLI(t, "spawn", "--team", "auth", "unlisted")
	spawnRosterWait = previousWait
	if code != 1 || stdout != "" || !strings.Contains(stderr, "noc could not identify it") || !strings.Contains(stderr, "started nothing-listed") {
		t.Fatalf("unlisted = %d %q %q", code, stdout, stderr)
	}
	if after := len(listAgents(t, "--all").Agents); after != before {
		t.Fatalf("an unidentified spawn left a row: %d -> %d", before, after)
	}

	// Refusals.
	t.Setenv("NOC_CLAUDE_BIN", filepath.Join(dir, "absent"))
	if code, _, stderr := runCLI(t, "spawn", "x"); code != 1 || !strings.Contains(stderr, "noc: find claude executable ") {
		t.Fatalf("absent claude = %d %q", code, stderr)
	}
	t.Setenv("NOC_CLAUDE_BIN", fake)
	before = len(listAgents(t, "--all").Agents)
	if code, _, stderr := runCLI(t, "spawn", "--provider", "codex", "x"); code != 1 || stderr != "noc spawn: codex is not supported yet\n" {
		t.Fatalf("codex = %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "spawn", "--dir", filepath.Join(dir, "nowhere"), "x"); code != 1 || !strings.Contains(stderr, "noc: open directory ") {
		t.Fatalf("missing dir = %d %q", code, stderr)
	}
	for _, args := range [][]string{
		{"spawn"}, {"spawn", "--team", "auth"}, {"spawn", "--", "--model", "opus"},
		{"spawn", "--team", "a b", "x"}, {"spawn", "--provider", "other", "x"},
	} {
		if code, _, stderr := runCLI(t, args...); code != 2 || stderr == "" {
			t.Errorf("%v = %d %q", args, code, stderr)
		}
	}
	if after := len(listAgents(t, "--all").Agents); after != before {
		t.Fatalf("a refused spawn left a row: %d -> %d", before, after)
	}
}

func TestCallerIsFoundByItsProviderProcess(t *testing.T) {
	isolate(t)
	mem := registry.NewMemory()
	now := time.Now().UTC()
	for id, pid := range map[string]int{"a1": 4242, "a2": 0} {
		_ = mem.Upsert(context.Background(), apiv1.Agent{
			ID: id, HostID: "h", Provider: apiv1.ProviderClaude, NativeSessionID: "s-" + id,
			LiveState: apiv1.LiveStateLive, ProcessID: pid, FirstSeenAt: now, LastSeenAt: now,
		})
	}
	dir, err := os.MkdirTemp("/tmp", "noc-caller-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "noc.sock")
	listener, cleanup, err := localapi.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: localapi.NewHandler(mem, func() apiv1.Health { return apiv1.Health{} })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = cleanup() })
	client := localapi.NewClient(socket)
	defer client.Close()

	previous := providerProcessID
	t.Cleanup(func() { providerProcessID = previous })
	for _, c := range []struct {
		env     string
		session string
		pid     int
		want    string
	}{
		{"", "", 4242, "a1"},         // the provider process above this command
		{"", "", 999, ""},            // a provider process noc does not know
		{"", "", 0, ""},              // not under a provider at all
		{"a2", "", 4242, "a2"},       // NOC_AGENT_ID beats the process
		{" a2 ", "", 0, "a2"},        // and is trimmed
		{"a2", "s-a1", 0, "a1"},      // the Claude session beats NOC_AGENT_ID
		{"a2", "s-unknown", 0, "a2"}, // unless noc does not know that session
		{"", "s-unknown", 4242, "a1"},
	} {
		t.Setenv("NOC_AGENT_ID", c.env)
		t.Setenv("CLAUDE_CODE_SESSION_ID", c.session)
		providerProcessID = func() int { return c.pid }
		got, err := callerAgentID(context.Background(), client)
		if err != nil || got != c.want {
			t.Errorf("env=%q session=%q pid=%d: %q %v, want %q", c.env, c.session, c.pid, got, err, c.want)
		}
	}
}

func TestSummarizeAndRenderTeams(t *testing.T) {
	agents := []apiv1.Agent{
		{ID: "1", Team: "zeta", Bucket: apiv1.BucketWorking},
		{ID: "2", Bucket: apiv1.BucketReview},
		{ID: "3", Team: "alpha", Bucket: apiv1.BucketBlocked, Lead: true, UserName: "the boss"},
		{ID: "4", Team: "alpha", Bucket: apiv1.BucketQuiet},
		{ID: "5", Team: "alpha", Bucket: "odd"},
		{ID: "6", Team: "zeta", Bucket: apiv1.BucketReview, Lead: true},
		{ID: "7", Lead: true, Bucket: apiv1.BucketQuiet},
	}
	var out bytes.Buffer
	renderTeams(&out, summarizeTeams(agents))
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"TEAM LEAD AGENTS BLOCKED WORKING REVIEW QUIET",
		"alpha the boss 3 1 0 0 2",
		"zeta 6 2 0 1 1 0",
		"(none) - 2 0 0 1 1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("teams table:\n%s", out.String())
	}
	out.Reset()
	renderTeams(&out, summarizeTeams(nil))
	if out.String() != "No agents.\n" {
		t.Fatalf("empty = %q", out.String())
	}
	if teams := summarizeTeams(agents[:1]); len(teams) != 1 || teams[0].Name != "zeta" {
		t.Fatalf("no (none) row without teamless agents: %+v", teams)
	}
}
