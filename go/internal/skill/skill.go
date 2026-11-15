// Package skill installs, checks and removes the noc skill: one document,
// the same for every provider, that teaches an agent how to use the registry.
package skill

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "noc.local/noc/pkg/api/v1"
)

//go:embed SKILL.md
var document []byte

// marker is in every skill file noc installs. A file without it is someone
// else's and is never written or removed.
const marker = "<!-- noc-skill:"

// Document returns the skill as it is installed.
func Document() []byte { return append([]byte(nil), document...) }

// Status is what a provider's skill file is.
type Status string

const (
	// StatusCurrent: the file is this binary's skill.
	StatusCurrent Status = "current"
	// StatusOutdated: noc installed the file, and it differs from this binary's.
	StatusOutdated Status = "outdated"
	// StatusMissing: there is no file.
	StatusMissing Status = "missing"
	// StatusForeign: there is a file that noc did not install.
	StatusForeign Status = "foreign"
)

// ErrForeign is returned when the skill file was not installed by noc.
var ErrForeign = errors.New("a skill named noc that noc did not install is already there; it was not modified")

// Paths locates each provider's skill file.
type Paths struct {
	Claude string
	Codex  string
}

// For returns the provider's skill file.
func (p Paths) For(provider apiv1.Provider) string {
	if provider == apiv1.ProviderCodex {
		return p.Codex
	}
	return p.Claude
}

// ResolvePaths computes the skill files from the environment:
// $CLAUDE_CONFIG_DIR or ~/.claude, and $CODEX_HOME or ~/.codex, each with
// skills/noc/SKILL.md.
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
	file := filepath.Join("skills", "noc", "SKILL.md")
	return Paths{
		Claude: filepath.Clean(filepath.Join(claudeDir, file)),
		Codex:  filepath.Clean(filepath.Join(codexDir, file)),
	}, nil
}

// Inspect reports what the file at path is.
func Inspect(path string) (Status, error) {
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StatusMissing, nil
	case err != nil:
		return "", fmt.Errorf("read skill %q: %w", path, err)
	case bytes.Equal(existing, document):
		return StatusCurrent, nil
	case bytes.Contains(existing, []byte(marker)):
		return StatusOutdated, nil
	default:
		return StatusForeign, nil
	}
}

// Install writes the skill to path, replacing an older one that noc
// installed, and reports whether the file changed. A file noc did not
// install is left alone with ErrForeign.
func Install(path string) (bool, error) {
	status, err := Inspect(path)
	if err != nil {
		return false, err
	}
	switch status {
	case StatusCurrent:
		return false, nil
	case StatusForeign:
		return false, ErrForeign
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create skill directory: %w", err)
	}
	// Written beside the file and renamed, so a reader never sees half of it.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".SKILL.md.*")
	if err != nil {
		return false, fmt.Errorf("write skill: %w", err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(document); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("write skill: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("write skill: %w", err)
	}
	if err := os.Chmod(temporary.Name(), 0o600); err != nil {
		return false, fmt.Errorf("write skill: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return false, fmt.Errorf("write skill: %w", err)
	}
	return true, nil
}

// Uninstall removes a skill file that noc installed, and its directory when
// that leaves it empty, and reports whether a file was removed. A file noc
// did not install is left alone with ErrForeign.
func Uninstall(path string) (bool, error) {
	status, err := Inspect(path)
	if err != nil {
		return false, err
	}
	switch status {
	case StatusMissing:
		return false, nil
	case StatusForeign:
		return false, ErrForeign
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("remove skill %q: %w", path, err)
	}
	// Fails, harmlessly, when the directory holds anything else.
	_ = os.Remove(filepath.Dir(path))
	return true, nil
}
