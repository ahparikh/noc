package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// RosterTimeout bounds one `claude agents` read.
const RosterTimeout = 5 * time.Second

// ErrRunningInTerminal marks a session that is already open in a terminal.
var ErrRunningInTerminal = errors.New("this Claude session is already running in a terminal")

// ClaudeSession is one entry of `claude agents --json --all`.
type ClaudeSession struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	PID       *int   `json:"pid"`
}

func (s ClaudeSession) running() bool {
	return s.PID != nil && *s.PID > 0
}

// ReadClaudeRoster lists Claude's sessions. Only stdout is read.
func ReadClaudeRoster(ctx context.Context, claudeBin string) ([]ClaudeSession, error) {
	ctx, cancel := context.WithTimeout(ctx, RosterTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, strings.TrimSpace(claudeBin), "agents", "--json", "--all")
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w: %v", ctxErr, err)
		}
		return nil, fmt.Errorf("list Claude sessions: %w", err)
	}
	var roster []ClaudeSession
	if err := json.Unmarshal(stdout.Bytes(), &roster); err != nil {
		return nil, fmt.Errorf("decode Claude sessions: %w", err)
	}
	return roster, nil
}

// PlanClaudeWithRoster plans an attach using what Claude itself lists.
func PlanClaudeWithRoster(agent apiv1.Agent, roster []ClaudeSession, claudeBin string, force bool) (Plan, error) {
	sessionID := strings.TrimSpace(agent.NativeSessionID)
	if sessionID == "" {
		return Plan{}, errNoNativeSession
	}
	program := strings.TrimSpace(claudeBin)
	if program == "" {
		return Plan{}, errors.New("Claude executable is not configured")
	}
	directory := workspaceDirectory(agent)

	job := ""
	for _, entry := range roster {
		if strings.TrimSpace(entry.SessionID) != sessionID {
			continue
		}
		switch entry.Kind {
		case "interactive":
			if entry.running() && !force {
				return Plan{}, fmt.Errorf("%w (pid %d); focus that terminal instead", ErrRunningInTerminal, *entry.PID)
			}
		case "background":
			if id := strings.TrimSpace(entry.ID); id != "" {
				job = id
			}
		}
	}
	if job != "" {
		return Plan{Program: program, Arguments: []string{"attach", job}, Directory: directory, Mode: ModeAttach}, nil
	}
	return Plan{Program: program, Arguments: []string{"--bg", "--resume", sessionID}, Directory: directory, Mode: ModeBackgroundResume}, nil
}

// BackgroundJobFor returns the trimmed id of the first background entry for
// the session, or "" when there is none or its id is blank.
func BackgroundJobFor(roster []ClaudeSession, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	for _, entry := range roster {
		if strings.TrimSpace(entry.SessionID) == sessionID && entry.Kind == "background" {
			return strings.TrimSpace(entry.ID)
		}
	}
	return ""
}
