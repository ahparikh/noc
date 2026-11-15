package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noc.local/noc/internal/skill"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// isolateSkills points both providers' configuration at empty directories.
func isolateSkills(t *testing.T) (claude, codex string) {
	t.Helper()
	dir := isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	return filepath.Join(dir, "claude", "skills", "noc", "SKILL.md"), filepath.Join(dir, "codex", "skills", "noc", "SKILL.md")
}

func TestSetupSkillInstallsChecksAndRemoves(t *testing.T) {
	claude, codex := isolateSkills(t)

	// Nothing installed: the check says so and fails.
	code, stdout, _ := runCLI(t, "setup", "skill", "--check")
	if code != 1 || stdout != "noc setup skill check\n  claude: not installed; run noc setup skill\n  codex: not installed; run noc setup skill\n" {
		t.Fatalf("check before = %d %q", code, stdout)
	}

	code, stdout, stderr := runCLI(t, "setup", "skill")
	if code != 0 || stderr != "" || stdout != "noc setup skill install\n  claude: installed in "+claude+"\n  codex: installed in "+codex+"\n" {
		t.Fatalf("install = %d %q %q", code, stdout, stderr)
	}
	for _, path := range []string{claude, codex} {
		written, err := os.ReadFile(path)
		if err != nil || string(written) != string(skill.Document()) {
			t.Fatalf("%s: %v", path, err)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", path, info.Mode().Perm())
		}
	}
	// Again changes nothing, and the word may follow the flags.
	if code, stdout, _ := runCLI(t, "setup", "skill", "claude"); code != 0 || stdout != "noc setup skill install\n  claude: already installed in "+claude+"\n" {
		t.Fatalf("second install = %d %q", code, stdout)
	}
	if code, stdout, _ := runCLI(t, "setup", "--check", "skill"); code != 0 || stdout != "noc setup skill check\n  claude: installed\n  codex: installed\n" {
		t.Fatalf("check after = %d %q", code, stdout)
	}

	// An older noc's skill is replaced; someone else's is left alone.
	old := "---\nname: noc\n---\n<!-- noc-skill: an earlier version -->\nold text\n"
	if err := os.WriteFile(claude, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	theirs := "---\nname: noc\n---\nmy own notes\n"
	if err := os.WriteFile(codex, []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCLI(t, "setup", "skill", "claude", "--check", "--json")
	var report skillReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || code != 1 || report.SchemaVersion != 1 || report.Action != "check" ||
		len(report.Results) != 1 || report.Results[0].Provider != apiv1.ProviderClaude || report.Results[0].Status != skill.StatusOutdated || report.Results[0].Path != claude {
		t.Fatalf("check --json = %d %v %+v", code, err, report)
	}
	code, stdout, _ = runCLI(t, "setup", "skill", "--check")
	if code != 1 || !strings.Contains(stdout, "  claude: outdated; run noc setup skill to update "+claude) || !strings.Contains(stdout, "  codex: "+codex+" was not installed by noc") {
		t.Fatalf("check mixed = %d %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "setup", "skill")
	if code != 1 || !strings.Contains(stdout, "  claude: installed in "+claude) || !strings.Contains(stdout, "  codex: error — a skill named noc that noc did not install is already there; it was not modified ("+codex+")") {
		t.Fatalf("install mixed = %d %q", code, stdout)
	}
	if kept, _ := os.ReadFile(codex); string(kept) != theirs {
		t.Fatalf("someone else's skill was changed: %q", kept)
	}

	// Uninstall removes only what noc installed, and its empty directory.
	code, stdout, _ = runCLI(t, "setup", "skill", "--uninstall")
	if code != 1 || !strings.Contains(stdout, "  claude: removed "+claude) || !strings.Contains(stdout, "  codex: error — ") {
		t.Fatalf("uninstall = %d %q", code, stdout)
	}
	if _, err := os.Stat(filepath.Dir(claude)); !os.IsNotExist(err) {
		t.Fatalf("the empty skill directory should be gone: %v", err)
	}
	if _, err := os.Stat(codex); err != nil {
		t.Fatalf("someone else's skill was removed: %v", err)
	}
	if code, stdout, _ := runCLI(t, "setup", "skill", "--uninstall", "claude"); code != 0 || stdout != "noc setup skill uninstall\n  claude: no noc skill found at "+claude+"\n" {
		t.Fatalf("second uninstall = %d %q", code, stdout)
	}

	for _, args := range [][]string{
		{"setup", "skill", "--check", "--uninstall"}, {"setup", "skill", "gemini"}, {"setup", "skill", "claude", "codex"},
	} {
		if code, _, stderr := runCLI(t, args...); code != 2 || stderr == "" {
			t.Errorf("%v = %d %q", args, code, stderr)
		}
	}
	// Plain setup is still about hooks.
	if code, stdout, _ := runCLI(t, "setup", "--check"); !strings.HasPrefix(stdout, "noc setup check using ") {
		t.Fatalf("setup --check = %d %q", code, stdout)
	}
}

// The skill is only useful if what it tells agents to run exists.
func TestSkillNamesOnlyCommandsThatExist(t *testing.T) {
	text := string(skill.Document())
	if !strings.HasPrefix(text, "---\nname: noc\ndescription: ") || !strings.Contains(text, "<!-- noc-skill:") {
		t.Fatalf("frontmatter or marker missing:\n%.200s", text)
	}
	for _, command := range []string{
		"noc whoami --json", "noc ls --json --all --team", "noc teams", "noc search --json", "noc show --json",
		"noc spawn --team", "noc team set", "noc team clear", "noc team lead", "noc rename",
	} {
		if !strings.Contains(text, command) {
			t.Errorf("the skill does not mention %q", command)
		}
		if !strings.Contains(usageText, strings.Join(strings.Fields(command)[:2], " ")) {
			t.Errorf("%q is not a command in the usage text", command)
		}
	}
}

func TestWhoami(t *testing.T) {
	dir := isolate(t)
	outsideAnAgent(t)
	startServer(t)
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	id := codexAgents(t, work, "thread-me")["thread-me"]

	if code, stdout, stderr := runCLI(t, "whoami"); code != 1 || stdout != "" || stderr != "noc whoami: this is not running inside an agent that noc knows\n" {
		t.Fatalf("outside = %d %q %q", code, stdout, stderr)
	}
	t.Setenv("NOC_AGENT_ID", id)
	if code, _, _ := runCLI(t, "team", "set", "auth"); code != 0 {
		t.Fatal("team set")
	}
	code, stdout, _ := runCLI(t, "whoami", "--json")
	var agent apiv1.Agent
	if err := json.Unmarshal([]byte(stdout), &agent); err != nil || code != 0 || agent.ID != id || agent.Team != "auth" {
		t.Fatalf("whoami --json = %d %v %+v", code, err, agent)
	}
	if code, stdout, _ := runCLI(t, "whoami"); code != 0 || !strings.Contains(stdout, id) || !strings.Contains(stdout, "auth") {
		t.Fatalf("whoami = %d %q", code, stdout)
	}
	t.Setenv("NOC_AGENT_ID", "gone")
	if code, _, stderr := runCLI(t, "whoami"); code != 1 || !strings.Contains(stderr, "agent was not found") {
		t.Fatalf("stale = %d %q", code, stderr)
	}
	if code, _, _ := runCLI(t, "whoami", "extra"); code != 2 {
		t.Fatalf("extra argument = %d", code)
	}
}
