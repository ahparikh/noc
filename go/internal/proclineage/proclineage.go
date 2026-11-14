// Package proclineage finds the provider process a hook was spawned under and
// answers a negative-only liveness question about it. Every failure is open.
package proclineage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxAncestry     = 12
	snapshotTimeout = 2 * time.Second
)

// Table is a process-table snapshot.
type Table struct {
	Parent map[int]int
	Name   map[int]string
}

// ProviderProcessID returns the PID of the nearest ancestor of this process
// whose command name is one of names, or 0 when none is found.
func ProviderProcessID(names ...string) int {
	if len(names) == 0 {
		return 0
	}
	return FindAncestor(snapshot(), os.Getpid(), names...)
}

// FindAncestor walks up to maxAncestry parents of pid in table and returns the
// nearest ancestor (never pid itself) whose command name is in names.
func FindAncestor(table Table, pid int, names ...string) int {
	wanted := map[string]bool{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		return 0
	}
	current := pid
	for step := 0; step < maxAncestry; step++ {
		parent, ok := table.Parent[current]
		if !ok || parent <= 1 {
			return 0
		}
		if wanted[table.Name[parent]] {
			return parent
		}
		current = parent
	}
	return 0
}

// IsRunning reports whether pid may still exist. It is negative-only: false
// means the process is gone, true means it is or might be alive.
func IsRunning(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

func snapshot() Table {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-eo", "pid=,ppid=,comm=")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return Table{}
	}
	return ParseTable(out.String())
}

// ParseTable parses `ps -eo pid=,ppid=,comm=` output.
func ParseTable(s string) Table {
	table := Table{Parent: map[int]int{}, Name: map[int]string{}}
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		table.Parent[pid] = ppid
		table.Name[pid] = commandName(fields[2])
	}
	return table
}

func commandName(command string) string {
	command = strings.TrimSpace(command)
	if i := strings.IndexByte(command, ' '); i >= 0 {
		command = command[:i]
	}
	if command == "" {
		return ""
	}
	return filepath.Base(command)
}
