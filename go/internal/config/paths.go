// Package config resolves noc's state, runtime, socket, and database paths.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	socketFileName   = "noc.sock"
	databaseFileName = "noc.db"
)

// Paths holds every resolved filesystem location noc uses.
type Paths struct {
	StateDir     string
	RuntimeDir   string
	SocketPath   string
	DatabasePath string
}

// ResolvePaths resolves the paths from the environment and platform defaults.
//
//	State dir:   NOC_STATE_DIR → $XDG_STATE_HOME/noc → ~/Library/Application Support/noc (darwin) → ~/.local/state/noc
//	Runtime dir: NOC_RUNTIME_DIR → $XDG_RUNTIME_DIR/noc → os.UserCacheDir()/noc
//	Socket:      NOC_SOCKET → <runtime>/noc.sock
//	Database:    NOC_DATABASE → <state>/noc.db
func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}

	stateDir := os.Getenv("NOC_STATE_DIR")
	if stateDir == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			stateDir = filepath.Join(xdg, "noc")
		} else if runtime.GOOS == "darwin" {
			stateDir = filepath.Join(home, "Library", "Application Support", "noc")
		} else {
			stateDir = filepath.Join(home, ".local", "state", "noc")
		}
	}

	runtimeDir := os.Getenv("NOC_RUNTIME_DIR")
	if runtimeDir == "" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			runtimeDir = filepath.Join(xdg, "noc")
		} else {
			cache, err := os.UserCacheDir()
			if err != nil {
				return Paths{}, fmt.Errorf("resolve user cache directory: %w", err)
			}
			runtimeDir = filepath.Join(cache, "noc")
		}
	}

	socketPath := os.Getenv("NOC_SOCKET")
	if socketPath == "" {
		socketPath = filepath.Join(runtimeDir, socketFileName)
	}
	databasePath := os.Getenv("NOC_DATABASE")
	if databasePath == "" {
		databasePath = filepath.Join(stateDir, databaseFileName)
	}

	return Paths{
		StateDir:     filepath.Clean(stateDir),
		RuntimeDir:   filepath.Clean(runtimeDir),
		SocketPath:   filepath.Clean(socketPath),
		DatabasePath: filepath.Clean(databasePath),
	}, nil
}

// EnsurePrivateDir creates path as a user-only directory when it is missing.
// An existing directory is left untouched: the caller can intentionally place
// a socket in an existing directory such as /tmp, and noc never changes
// permissions on a directory it did not create.
func EnsurePrivateDir(path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("private directory path %q is not a directory", path)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create private directory %q: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure private directory %q: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("inspect private directory %q: %w", path, err)
	}
}
