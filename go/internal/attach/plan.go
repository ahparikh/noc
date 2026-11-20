// Package attach plans how the CLI hands the terminal to a provider session.
// It holds pure planners only; lookup, execution and exit codes live in the CLI.
package attach

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// Mode is the only key the CLI branches on after planning.
type Mode string

const (
	ModeAttach           Mode = "attach"
	ModeResume           Mode = "resume"
	ModeBackgroundResume Mode = "background-resume"
	ModeStop             Mode = "stop"
	ModeRespawn          Mode = "respawn"
)

// Plan describes one provider command to run.
type Plan struct {
	Program   string
	Arguments []string
	Directory string
	Mode      Mode
}

// Binaries names the provider executables a planner may use.
type Binaries struct {
	Claude string
	Codex  string
}

// DefaultBinaries are looked up on PATH.
var DefaultBinaries = Binaries{Claude: "claude", Codex: "codex"}

var errNoNativeSession = errors.New("agent has no native session ID")

// PlanForAgent builds the registry-based plan for an agent.
func PlanForAgent(agent apiv1.Agent, bins Binaries) (Plan, error) {
	sessionID := strings.TrimSpace(agent.NativeSessionID)
	if sessionID == "" {
		return Plan{}, errNoNativeSession
	}
	directory := workspaceDirectory(agent)

	switch agent.Provider {
	case apiv1.ProviderClaude:
		program := strings.TrimSpace(bins.Claude)
		if program == "" {
			return Plan{}, errors.New("Claude executable is not configured")
		}
		shortID := strings.TrimSpace(agent.NativeShortID)
		if capabilityState(agent, "attach") == capSupported && shortID != "" && agent.LiveState == apiv1.LiveStateLive {
			return Plan{Program: program, Arguments: []string{"attach", shortID}, Directory: directory, Mode: ModeAttach}, nil
		}
		if err := resumeDenied(agent); err != nil {
			return Plan{}, err
		}
		return Plan{Program: program, Arguments: []string{"--resume", sessionID}, Directory: directory, Mode: ModeResume}, nil
	case apiv1.ProviderCodex:
		if err := resumeDenied(agent); err != nil {
			return Plan{}, err
		}
		program := strings.TrimSpace(bins.Codex)
		if program == "" {
			return Plan{}, errors.New("Codex executable is not configured")
		}
		return Plan{Program: program, Arguments: []string{"resume", sessionID}, Directory: directory, Mode: ModeResume}, nil
	default:
		return Plan{}, fmt.Errorf("provider %q does not support terminal attachment", string(agent.Provider))
	}
}

// ConcurrentUseRisk reports whether running plan would start a second
// independent process on a live Claude conversation. The string is the
// refusal suffix naming the Zellij sessions that appear to hold it.
func ConcurrentUseRisk(agent apiv1.Agent, plan Plan) (bool, string) {
	if agent.Provider != apiv1.ProviderClaude || plan.Mode != ModeResume || agent.LiveState != apiv1.LiveStateLive {
		return false, ""
	}
	sessions := zellijSessions(agent)
	switch len(sessions) {
	case 0:
		return true, ""
	case 1:
		return true, fmt.Sprintf("; it appears to be open in Zellij session %q", sessions[0])
	default:
		return true, "; it appears to be open in Zellij sessions " + strings.Join(sessions, ", ")
	}
}

func zellijSessions(agent apiv1.Agent) []string {
	var sessions []string
	seen := map[string]bool{}
	for _, binding := range agent.FrontendBindings {
		if binding.Kind != "zellij" {
			continue
		}
		session := strings.TrimSpace(binding.Session)
		if session == "" || seen[session] {
			continue
		}
		seen[session] = true
		sessions = append(sessions, session)
	}
	return sessions
}

func workspaceDirectory(agent apiv1.Agent) string {
	if agent.Workspace == nil {
		return ""
	}
	return strings.TrimSpace(agent.Workspace.Path)
}

type capState int

const (
	capMissing capState = iota
	capSupported
	capDenied
)

func capabilityState(agent apiv1.Agent, name string) capState {
	capability, ok := agent.Capabilities[name]
	if !ok {
		return capMissing
	}
	if capability.Supported {
		return capSupported
	}
	return capDenied
}

func resumeDenied(agent apiv1.Agent) error {
	if capabilityState(agent, "resume") != capDenied {
		return nil
	}
	if reason := strings.TrimSpace(agent.Capabilities["resume"].Reason); reason != "" {
		return fmt.Errorf("agent cannot resume: %s", reason)
	}
	return errors.New("agent cannot resume")
}
