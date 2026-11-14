package hookconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		ClaudeSettings: filepath.Join(dir, "claude", "settings.json"),
		CodexHooks:     filepath.Join(dir, "codex", "hooks.json"),
		CodexConfig:    filepath.Join(dir, "codex", "config.toml"),
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallIsIdempotentAndPreservesUnrelatedClaudeSettings(t *testing.T) {
	paths := testPaths(t)
	writeFile(t, paths.ClaudeSettings, `{"model":"x","hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hi"}]}]}}`)
	exe := "/opt/bin/noc"

	c, err := Install(apiv1.ProviderClaude, exe, paths)
	if err != nil || !c.Changed || c.Removed != 0 {
		t.Fatalf("first install: %+v %v", c, err)
	}
	first := readFile(t, paths.ClaudeSettings)
	c, err = Install(apiv1.ProviderClaude, exe, paths)
	if err != nil || c.Changed || c.Removed != 9 {
		t.Fatalf("second install: %+v %v", c, err)
	}
	if readFile(t, paths.ClaudeSettings) != first {
		t.Fatal("second install changed bytes")
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(first), &root); err != nil {
		t.Fatal(err)
	}
	groups := root["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(groups) != 2 || groups[0].(map[string]any)["matcher"] != "startup" {
		t.Fatalf("unrelated group not preserved first: %v", groups)
	}
	if _, ok := groups[1].(map[string]any)["matcher"]; ok {
		t.Fatal("managed group must not have matcher")
	}
	if !strings.Contains(first, `"command": "'/opt/bin/noc' hook ingest --provider claude"`) || !strings.Contains(first, `"timeout": 3`) {
		t.Fatalf("unexpected shape: %s", first)
	}

	c, err = Uninstall(apiv1.ProviderClaude, paths)
	if err != nil || !c.Changed || c.Removed != 9 {
		t.Fatalf("uninstall: %+v %v", c, err)
	}
	after := readFile(t, paths.ClaudeSettings)
	if !strings.Contains(after, "echo hi") || !strings.Contains(after, "startup") || strings.Contains(after, "hook ingest") {
		t.Fatalf("uninstall result: %s", after)
	}
	root = nil
	_ = json.Unmarshal([]byte(after), &root)
	if len(root["hooks"].(map[string]any)) != 1 {
		t.Fatalf("only SessionStart should remain: %s", after)
	}
}

func TestInstallReplacesExecutableOnReinstall(t *testing.T) {
	paths := testPaths(t)
	if _, err := Install(apiv1.ProviderClaude, "/old/noc", paths); err != nil {
		t.Fatal(err)
	}
	c, err := Install(apiv1.ProviderClaude, "/new/noc", paths)
	if err != nil || !c.Changed || c.Removed != 9 {
		t.Fatalf("%+v %v", c, err)
	}
	got := readFile(t, paths.ClaudeSettings)
	if strings.Contains(got, "/old/noc") || strings.Count(got, "/new/noc") != 9 {
		t.Fatalf("not replaced: %s", got)
	}
}

func TestInstallCodexUsesHooksJSONAndDetectsDisabledFeature(t *testing.T) {
	for _, spelling := range []string{"hooks = false", "codex_hooks=false"} {
		paths := testPaths(t)
		writeFile(t, paths.CodexConfig, "[features]\n"+spelling+"\n")
		exe := "/opt/bin/noc"
		c, err := Install(apiv1.ProviderCodex, exe, paths)
		if err != nil || !c.Changed || c.Path != paths.CodexHooks {
			t.Fatalf("%+v %v", c, err)
		}
		if got := readFile(t, paths.CodexConfig); got != "[features]\n"+spelling+"\n" {
			t.Fatal("config.toml modified")
		}
		insp, err := Inspect(apiv1.ProviderCodex, exe, paths)
		if err != nil {
			t.Fatal(err)
		}
		if !insp.HooksDisabled || insp.Complete() || len(insp.MissingEvents) != 0 || len(insp.Files) != 2 {
			t.Fatalf("%s: %+v", spelling, insp)
		}
		if len(insp.Paths) != 2 || insp.Paths[0] != paths.CodexHooks {
			t.Fatalf("paths %v", insp.Paths)
		}
	}
}

func TestInspectCompleteAndMismatch(t *testing.T) {
	paths := testPaths(t)
	insp, _ := Inspect(apiv1.ProviderClaude, "/a/noc", paths)
	if insp.Files != nil || len(insp.MissingEvents) != 9 || insp.Complete() {
		t.Fatalf("%+v", insp)
	}
	if _, err := Install(apiv1.ProviderClaude, "/a/noc", paths); err != nil {
		t.Fatal(err)
	}
	insp, _ = Inspect(apiv1.ProviderClaude, "/a/noc", paths)
	if !insp.Complete() {
		t.Fatalf("%+v", insp)
	}
	insp, _ = Inspect(apiv1.ProviderClaude, "/b/noc", paths)
	if insp.Complete() || len(insp.MissingEvents) != 0 || len(insp.MismatchedExecutableEvents) != 9 {
		t.Fatalf("%+v", insp)
	}
	if bad, _ := Inspect("other", "/a/noc", paths); len(bad.Errors) != 1 || bad.Errors[0] != `unsupported provider "other"` {
		t.Fatalf("%+v", bad)
	}
}

func TestInstallCodexRefusesCompetingInlineManagedHook(t *testing.T) {
	paths := testPaths(t)
	writeFile(t, paths.CodexConfig, "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ncommand = '/x/noc hook ingest --provider codex'\n")
	_, err := Install(apiv1.ProviderCodex, "/opt/bin/noc", paths)
	want := paths.CodexConfig + " contains an existing noc hook; remove it before setup can manage " + paths.CodexHooks
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(paths.CodexHooks); !os.IsNotExist(statErr) {
		t.Fatal("hooks.json must not be created")
	}
	insp, _ := Inspect(apiv1.ProviderCodex, "/x/noc", paths)
	if !insp.ManagedInCodexConfig || insp.MissingEvents[0] == "Stop" {
		t.Fatalf("%+v", insp)
	}
	for _, e := range insp.MissingEvents {
		if e == "Stop" {
			t.Fatal("Stop should be covered by toml")
		}
	}
}

func TestInstallRejectsMalformedConfigurationWithoutReplacingIt(t *testing.T) {
	for name, content := range map[string]string{"invalid": "{nope", "array": "[1]", "null": "null"} {
		paths := testPaths(t)
		writeFile(t, paths.ClaudeSettings, content)
		_, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths)
		if err == nil || !strings.Contains(err.Error(), "parse "+paths.ClaudeSettings) {
			t.Fatalf("%s: %v", name, err)
		}
		if readFile(t, paths.ClaudeSettings) != content {
			t.Fatalf("%s: file replaced", name)
		}
	}
	paths := testPaths(t)
	writeFile(t, paths.ClaudeSettings, `{"hooks":[]}`)
	if _, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths); err == nil || !strings.Contains(err.Error(), "top-level hooks value is not an object") {
		t.Fatal(err)
	}
	writeFile(t, paths.ClaudeSettings, `{"hooks":{"Stop":{}}}`)
	if _, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths); err == nil || !strings.Contains(err.Error(), "hook Stop: hook event value is not an array") {
		t.Fatal(err)
	}
	writeFile(t, paths.ClaudeSettings, strings.Repeat(" ", maxConfigBytes+1))
	if _, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths); err == nil || !strings.Contains(err.Error(), "file exceeds 1048576 bytes") {
		t.Fatal(err)
	}
}

func TestInstallPreservesConfigurationSymlink(t *testing.T) {
	paths := testPaths(t)
	target := filepath.Join(t.TempDir(), "real.json")
	writeFile(t, target, "{}")
	if err := os.MkdirAll(filepath.Dir(paths.ClaudeSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.ClaudeSettings); err != nil {
		t.Fatal(err)
	}
	c, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(target); c.Path != resolved {
		t.Fatalf("path %q", c.Path)
	}
	if fi, _ := os.Lstat(paths.ClaudeSettings); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if !strings.Contains(readFile(t, target), "hook ingest") {
		t.Fatal("target not updated")
	}

	dangling := testPaths(t)
	_ = os.MkdirAll(filepath.Dir(dangling.ClaudeSettings), 0o700)
	_ = os.Symlink(filepath.Join(t.TempDir(), "missing"), dangling.ClaudeSettings)
	if _, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", dangling); err == nil || !strings.Contains(err.Error(), "resolve hook configuration symlink") {
		t.Fatal(err)
	}
}

func TestInstallRequiresExecutableAndPreservesMode(t *testing.T) {
	paths := testPaths(t)
	if _, err := Install(apiv1.ProviderClaude, " ", paths); err == nil || err.Error() != "noc executable path is required" {
		t.Fatal(err)
	}
	writeFile(t, paths.ClaudeSettings, "  \n")
	_ = os.Chmod(paths.ClaudeSettings, 0o640)
	if _, err := Install(apiv1.ProviderClaude, "/opt/bin/noc", paths); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(paths.ClaudeSettings); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestUninstallMissingFileAndCodexConfigUntouched(t *testing.T) {
	paths := testPaths(t)
	c, err := Uninstall(apiv1.ProviderCodex, paths)
	if err != nil || c.Changed || c.Removed != 0 || c.Path != paths.CodexHooks {
		t.Fatalf("%+v %v", c, err)
	}
	toml := "[[hooks.Stop]]\ncommand = \"/x/noc hook ingest --provider codex\"\n"
	writeFile(t, paths.CodexConfig, toml)
	if _, err := Uninstall(apiv1.ProviderCodex, paths); err != nil {
		t.Fatal(err)
	}
	if readFile(t, paths.CodexConfig) != toml {
		t.Fatal("config.toml modified")
	}
}

func TestResolvePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", " "+filepath.Join(home, "cx")+" ")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ClaudeSettings != filepath.Join(home, ".claude", "settings.json") || p.CodexHooks != filepath.Join(home, "cx", "hooks.json") || p.CodexConfig != filepath.Join(home, "cx", "config.toml") {
		t.Fatalf("%+v", p)
	}
}

func TestMatchesCommandRequiresExactManagedArgv(t *testing.T) {
	cases := []struct {
		cmd  string
		p    apiv1.Provider
		want bool
	}{
		{"'/opt/bin/noc' hook ingest --provider claude", apiv1.ProviderClaude, true},
		{"/opt/bin/noc hook ingest --provider claude", apiv1.ProviderClaude, true},
		{"'/path with space/noc' hook ingest --provider codex", apiv1.ProviderCodex, true},
		{"noc hook ingest --provider=codex", apiv1.ProviderCodex, true},
		{"noc hook ingest --provider codex", apiv1.ProviderClaude, false},
		{"'/opt/bin/noc hook ingest --provider claude", apiv1.ProviderClaude, false},
		{"echo noc hook ingest --provider claude", apiv1.ProviderClaude, false},
		{"noc hook ingest --provider claude --database x", apiv1.ProviderClaude, false},
		{"FOO=1 noc hook ingest --provider claude", apiv1.ProviderClaude, false},
		{"/opt/bin/other hook ingest --provider claude", apiv1.ProviderClaude, false},
		{"noc hook ingest --provider claude\\", apiv1.ProviderClaude, false},
		{"", apiv1.ProviderClaude, false},
	}
	for _, c := range cases {
		if got := MatchesCommand(c.cmd, c.p); got != c.want {
			t.Errorf("MatchesCommand(%q, %s) = %v, want %v", c.cmd, c.p, got, c.want)
		}
	}
}

func TestSplitShellWords(t *testing.T) {
	got, ok := splitShellWords(`a 'b c' "d\"e" f\ g '' x"y"`)
	want := []string{"a", "b c", `d"e`, "f g", "", "xy"}
	if !ok || len(got) != len(want) {
		t.Fatalf("%q %v", got, ok)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q", got)
		}
	}
	if _, ok := splitShellWords(`"abc`); ok {
		t.Fatal("unterminated")
	}
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	q := shellQuote("/a'b/noc")
	if q != `'/a'"'"'b/noc'` {
		t.Fatal(q)
	}
	words, ok := splitShellWords(q)
	if !ok || len(words) != 1 || words[0] != "/a'b/noc" {
		t.Fatal(words)
	}
}
