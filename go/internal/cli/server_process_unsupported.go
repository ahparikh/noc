//go:build !(darwin || linux)

package cli

import (
	"errors"
	"os/exec"
)

var errUnsupportedPlatform = errors.New("background server mode is supported only on macOS and Linux")

func configureBackgroundCommand(*exec.Cmd) error { return errUnsupportedPlatform }

func processIsAlive(int) (bool, error) { return false, errUnsupportedPlatform }

func signalProcessTermination(int) error { return errUnsupportedPlatform }
