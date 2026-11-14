package attach

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// StopPlan plans `claude stop` for a live background Claude session.
func StopPlan(agent apiv1.Agent, claudeBin string) (Plan, error) {
	switch agent.Provider {
	case apiv1.ProviderClaude:
	case apiv1.ProviderCodex:
		return Plan{}, errors.New("stopping Codex agents is not supported yet")
	default:
		return Plan{}, fmt.Errorf("provider %q does not support stopping agents", string(agent.Provider))
	}
	if agent.LiveState == apiv1.LiveStateNotLive {
		return Plan{}, errors.New("agent is not running")
	}
	if agent.SourceKind != "background" {
		return Plan{}, errors.New("only background Claude sessions can be stopped; end this one from its terminal with /exit")
	}
	shortID := strings.TrimSpace(agent.NativeShortID)
	if shortID == "" {
		return Plan{}, errors.New("Claude session has no background session ID to stop")
	}
	program := strings.TrimSpace(claudeBin)
	if program == "" {
		return Plan{}, errors.New("Claude executable is not configured")
	}
	return Plan{Program: program, Arguments: []string{"stop", shortID}, Mode: ModeStop}, nil
}
