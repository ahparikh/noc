//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// configureBackgroundCommand detaches the child into its own session so it
// survives the terminal and its SIGINT.
func configureBackgroundCommand(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

// processIsAlive probes pid with signal 0. EPERM means the process exists but
// belongs to another user, so it counts as alive.
func processIsAlive(pid int) (bool, error) {
	if pid <= 1 {
		return false, fmt.Errorf("invalid server process ID %d", pid)
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, fmt.Errorf("inspect server process %d: %w", pid, err)
	}
}

// signalProcessTermination asks pid to shut down gracefully.
func signalProcessTermination(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
