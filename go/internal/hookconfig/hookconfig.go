// Package hookconfig installs, removes, and inspects the noc hook handlers in
// Claude Code and Codex configuration files.
package hookconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	maxConfigBytes = 1 << 20
	hookTimeout    = 3
	managedBase    = "noc"
)

var (
	claudeEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "PermissionRequest", "CwdChanged", "PostModelSwitch"}
	codexEvents  = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "PermissionRequest", "Interrupt"}

	tomlEventPattern   = regexp.MustCompile(`^\s*\[\[hooks\.([A-Za-z0-9_]+)(?:\.hooks)?\]\]\s*(?:#.*)?$`)
	tomlTablePattern   = regexp.MustCompile(`^\s*\[([^][]+)]\s*(?:#.*)?$`)
	tomlCommandPattern = regexp.MustCompile(`^\s*command\s*=\s*(.+?)\s*$`)
)

// Paths locates the provider configuration files.
type Paths struct {
	ClaudeSettings string
	CodexHooks     string
	CodexConfig    string
}

// ResolvePaths computes configuration paths from the environment.
func ResolvePaths() (Paths, error) {
	claudeDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	codexDir := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if claudeDir == "" || codexDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve home directory: %w", err)
		}
		if claudeDir == "" {
			claudeDir = filepath.Join(home, ".claude")
		}
		if codexDir == "" {
			codexDir = filepath.Join(home, ".codex")
		}
	}
	return Paths{
		ClaudeSettings: filepath.Clean(filepath.Join(claudeDir, "settings.json")),
		CodexHooks:     filepath.Clean(filepath.Join(codexDir, "hooks.json")),
		CodexConfig:    filepath.Clean(filepath.Join(codexDir, "config.toml")),
	}, nil
}

// RequiredEvents returns a copy of the events noc registers for a provider.
func RequiredEvents(p apiv1.Provider) []string {
	switch p {
	case apiv1.ProviderClaude:
		return append([]string(nil), claudeEvents...)
	case apiv1.ProviderCodex:
		return append([]string(nil), codexEvents...)
	}
	return []string{}
}

// Change describes the outcome of Install or Uninstall.
type Change struct {
	Provider apiv1.Provider `json:"provider"`
	Path     string         `json:"path"`
	Changed  bool           `json:"changed"`
	Removed  int            `json:"removed"`
}

// Inspection reports how completely hooks are registered.
type Inspection struct {
	Provider                   apiv1.Provider `json:"provider"`
	Paths                      []string       `json:"paths"`
	Files                      []string       `json:"files"`
	MissingEvents              []string       `json:"missing_events,omitempty"`
	MismatchedExecutableEvents []string       `json:"mismatched_executable_events,omitempty"`
	Errors                     []string       `json:"errors,omitempty"`
	HooksDisabled              bool           `json:"hooks_disabled,omitempty"`
	ManagedInCodexConfig       bool           `json:"managed_in_codex_config,omitempty"`
}

// Complete reports whether every required hook is registered for the executable.
func (i Inspection) Complete() bool {
	return len(i.Errors) == 0 && !i.HooksDisabled && len(i.MissingEvents) == 0 && len(i.MismatchedExecutableEvents) == 0
}

// Install registers the managed hooks for a provider.
func Install(p apiv1.Provider, executable string, paths Paths) (Change, error) {
	// Written as given: a symlink the caller chose outlives the file it
	// points at today.
	exe := cleanPath(executable)
	if exe == "" {
		return Change{}, errors.New("noc executable path is required")
	}
	events := RequiredEvents(p)
	if len(events) == 0 {
		return Change{}, fmt.Errorf("unsupported provider %q", p)
	}
	path := preferredPath(p, paths)
	if p == apiv1.ProviderCodex {
		scan, err := scanCodexConfig(paths.CodexConfig, exe)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Change{}, err
		}
		if scan.managed {
			return Change{}, fmt.Errorf("%s contains an existing noc hook; remove it before setup can manage %s", paths.CodexConfig, paths.CodexHooks)
		}
	}
	target, err := writableConfigPath(path)
	if err != nil {
		return Change{}, err
	}
	root, mode, original, err := readJSONObject(target)
	if err != nil {
		return Change{}, err
	}
	removed := removeManagedHooks(root, p)
	var hooks map[string]any
	switch v := root["hooks"].(type) {
	case nil:
		hooks = map[string]any{}
		root["hooks"] = hooks
	case map[string]any:
		hooks = v
	default:
		return Change{}, fmt.Errorf("update %s: top-level hooks value is not an object", target)
	}
	command := shellQuote(exe) + " hook ingest --provider " + string(p)
	for _, event := range events {
		var groups []any
		switch v := hooks[event].(type) {
		case nil:
		case []any:
			groups = v
		default:
			return Change{}, fmt.Errorf("update %s hook %s: hook event value is not an array", target, event)
		}
		hooks[event] = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": hookTimeout}},
		})
	}
	updated, err := marshalConfig(root)
	if err != nil {
		return Change{}, fmt.Errorf("update %s: %w", target, err)
	}
	change := Change{Provider: p, Path: target, Removed: removed}
	if strings.TrimSpace(original) != strings.TrimSpace(string(updated)) {
		if err := atomicWrite(target, updated, mode); err != nil {
			return Change{}, err
		}
		change.Changed = true
	}
	return change, nil
}

// Uninstall removes managed hooks for a provider. Codex config.toml is never modified.
func Uninstall(p apiv1.Provider, paths Paths) (Change, error) {
	if len(RequiredEvents(p)) == 0 {
		return Change{}, fmt.Errorf("unsupported provider %q", p)
	}
	target, err := writableConfigPath(preferredPath(p, paths))
	if err != nil {
		return Change{}, err
	}
	change := Change{Provider: p, Path: target}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return change, nil
	}
	root, mode, original, err := readJSONObject(target)
	if err != nil {
		return Change{}, err
	}
	change.Removed = removeManagedHooks(root, p)
	if change.Removed == 0 {
		return change, nil
	}
	updated, err := marshalConfig(root)
	if err != nil {
		return Change{}, fmt.Errorf("update %s: %w", target, err)
	}
	if strings.TrimSpace(original) != strings.TrimSpace(string(updated)) {
		if err := atomicWrite(target, updated, mode); err != nil {
			return Change{}, err
		}
		change.Changed = true
	}
	return change, nil
}

// Inspect reports hook coverage for a provider against an executable.
func Inspect(p apiv1.Provider, executable string, paths Paths) (Inspection, error) {
	insp := Inspection{Provider: p}
	required := RequiredEvents(p)
	if len(required) == 0 {
		insp.Paths = []string{}
		insp.Errors = []string{fmt.Sprintf("unsupported provider %q", p)}
		return insp, nil
	}
	exe := canonicalPath(executable)
	covered := map[string]bool{}
	exact := map[string]bool{}

	inspectJSON := func(path string) {
		data, err := readLimited(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			insp.Errors = append(insp.Errors, err.Error())
			return
		}
		insp.Files = append(insp.Files, path)
		var root map[string]any
		if err := json.Unmarshal(data, &root); err != nil {
			insp.Errors = append(insp.Errors, fmt.Sprintf("parse %s: %v", path, err))
			return
		}
		hooks, _ := root["hooks"].(map[string]any)
		for event, value := range hooks {
			walkCommands(value, func(command string) {
				words, ok := splitShellWords(command)
				if !ok || !managedWords(words, p) {
					return
				}
				covered[event] = true
				if exe != "" && canonicalPath(words[0]) == exe {
					exact[event] = true
				}
			})
		}
	}

	switch p {
	case apiv1.ProviderClaude:
		insp.Paths = []string{paths.ClaudeSettings}
		inspectJSON(paths.ClaudeSettings)
	case apiv1.ProviderCodex:
		insp.Paths = []string{paths.CodexHooks, paths.CodexConfig}
		inspectJSON(paths.CodexHooks)
		scan, err := scanCodexConfig(paths.CodexConfig, exe)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			insp.Errors = append(insp.Errors, err.Error())
		default:
			insp.Files = append(insp.Files, paths.CodexConfig)
			insp.HooksDisabled = scan.disabled
			insp.ManagedInCodexConfig = scan.managed
			for e := range scan.covered {
				covered[e] = true
			}
			for e := range scan.exact {
				exact[e] = true
			}
		}
	}
	for _, event := range required {
		if !covered[event] {
			insp.MissingEvents = append(insp.MissingEvents, event)
		}
		if !exact[event] {
			insp.MismatchedExecutableEvents = append(insp.MismatchedExecutableEvents, event)
		}
	}
	sort.Strings(insp.MissingEvents)
	sort.Strings(insp.MismatchedExecutableEvents)
	return insp, nil
}

// MatchesCommand reports whether command is exactly the noc-managed hook argv for p.
func MatchesCommand(command string, p apiv1.Provider) bool {
	words, ok := splitShellWords(command)
	return ok && managedWords(words, p)
}

func managedWords(words []string, p apiv1.Provider) bool {
	if len(words) == 0 || filepath.Base(words[0]) != managedBase {
		return false
	}
	switch len(words) {
	case 5:
		return words[1] == "hook" && words[2] == "ingest" && words[3] == "--provider" && words[4] == string(p)
	case 4:
		return words[1] == "hook" && words[2] == "ingest" && words[3] == "--provider="+string(p)
	}
	return false
}

func walkCommands(v any, fn func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if s, ok := child.(string); ok && k == "command" {
				fn(s)
				continue
			}
			walkCommands(child, fn)
		}
	case []any:
		for _, child := range t {
			walkCommands(child, fn)
		}
	}
}

type codexScan struct {
	disabled bool
	managed  bool
	covered  map[string]bool
	exact    map[string]bool
}

func scanCodexConfig(path, exe string) (codexScan, error) {
	scan := codexScan{covered: map[string]bool{}, exact: map[string]bool{}}
	data, err := readLimited(path)
	if err != nil {
		return scan, err
	}
	var table, event string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if m := tomlEventPattern.FindStringSubmatch(line); m != nil {
			event, table = m[1], ""
			continue
		}
		if m := tomlTablePattern.FindStringSubmatch(line); m != nil {
			table, event = strings.TrimSpace(m[1]), ""
			continue
		}
		if table == "features" {
			compact := strings.Join(strings.Fields(line), "")
			if strings.HasPrefix(compact, "hooks=false") || strings.HasPrefix(compact, "codex_hooks=false") {
				scan.disabled = true
			}
			continue
		}
		if event == "" {
			continue
		}
		m := tomlCommandPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		command, ok := tomlString(m[1])
		if !ok {
			continue
		}
		words, ok := splitShellWords(command)
		if !ok || !managedWords(words, apiv1.ProviderCodex) {
			continue
		}
		scan.managed = true
		scan.covered[event] = true
		if exe != "" && canonicalPath(words[0]) == exe {
			scan.exact[event] = true
		}
	}
	return scan, nil
}

func tomlString(v string) (string, bool) {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1], true
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		s, err := strconv.Unquote(v)
		return s, err == nil
	}
	return "", false
}

func splitShellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case ' ', '\t', '\r', '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\\':
			if i+1 >= len(rs) {
				return nil, false
			}
			i++
			cur.WriteRune(rs[i])
			inWord = true
		case '\'':
			inWord = true
			i++
			for ; i < len(rs) && rs[i] != '\''; i++ {
				cur.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, false
			}
		case '"':
			inWord = true
			i++
			for ; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' {
					i++
					if i >= len(rs) {
						return nil, false
					}
				}
				cur.WriteRune(rs[i])
			}
			if i >= len(rs) {
				return nil, false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

func removeManagedHooks(root map[string]any, p apiv1.Provider) int {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return 0
	}
	total := 0
	for event, value := range hooks {
		groups, ok := value.([]any)
		if !ok {
			continue
		}
		eventRemoved := 0
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			remaining := make([]any, 0, len(handlers))
			groupRemoved := 0
			for _, h := range handlers {
				if hm, ok := h.(map[string]any); ok {
					if cmd, ok := hm["command"].(string); ok && MatchesCommand(cmd, p) {
						groupRemoved++
						continue
					}
				}
				remaining = append(remaining, h)
			}
			if groupRemoved == 0 {
				kept = append(kept, g)
				continue
			}
			eventRemoved += groupRemoved
			if len(remaining) == 0 {
				continue
			}
			group["hooks"] = remaining
			kept = append(kept, group)
		}
		if eventRemoved == 0 {
			continue
		}
		total += eventRemoved
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return total
}

func preferredPath(p apiv1.Provider, paths Paths) string {
	if p == apiv1.ProviderCodex {
		return paths.CodexHooks
	}
	return paths.ClaudeSettings
}

func writableConfigPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve hook configuration symlink %s: %w", path, err)
	}
	return resolved, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("read %s: file exceeds %d bytes", path, maxConfigBytes)
	}
	return data, nil
}

func readJSONObject(path string) (map[string]any, os.FileMode, string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, 0o600, "", nil
	}
	if err != nil {
		return nil, 0, "", fmt.Errorf("inspect %s: %w", path, err)
	}
	data, err := readLimited(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, 0o600, "", nil
		}
		return nil, 0, "", err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, info.Mode().Perm(), string(data), nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, 0, "", fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		return nil, 0, "", fmt.Errorf("parse %s: top-level value is not an object", path)
	}
	return root, info.Mode().Perm(), string(data), nil
}

func marshalConfig(root map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".noc-hooks-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// cleanPath trims and cleans a path; empty and "." are no path.
func cleanPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	return path
}

// canonicalPath is cleanPath with symlinks resolved when possible, for
// telling whether two paths are the same executable.
func canonicalPath(path string) string {
	path = cleanPath(path)
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
