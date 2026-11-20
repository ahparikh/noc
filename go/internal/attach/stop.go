package attach

import (
	"errors"
	"fmt"
	"strings"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// lifecycle is one provider command that acts on a live background Claude
// session by its short ID.
type lifecycle struct {
	command string // the `claude` subcommand
	mode    Mode
	gerund  string // "stopping"
	verb    string // "stop"
	refusal string // what to say about a Claude session that is not a background one
}

var (
	stopLifecycle = lifecycle{
		command: "stop", mode: ModeStop, gerund: "stopping", verb: "stop",
		refusal: "only background Claude sessions can be stopped; end this one from its terminal with /exit",
	}
	respawnLifecycle = lifecycle{
		command: "respawn", mode: ModeRespawn, gerund: "respawning", verb: "respawn",
		refusal: "only background Claude sessions can be respawned; restart this one from its terminal",
	}
)

// StopPlan plans `claude stop` for a live background Claude session.
func StopPlan(agent apiv1.Agent, claudeBin string) (Plan, error) {
	return stopLifecycle.plan(agent, claudeBin)
}

// RespawnPlan plans `claude respawn` for a live background Claude session:
// the provider restarts the session's process on the Claude binary that is
// installed now and keeps the conversation.
func RespawnPlan(agent apiv1.Agent, claudeBin string) (Plan, error) {
	return respawnLifecycle.plan(agent, claudeBin)
}

func (l lifecycle) plan(agent apiv1.Agent, claudeBin string) (Plan, error) {
	switch agent.Provider {
	case apiv1.ProviderClaude:
	case apiv1.ProviderCodex:
		return Plan{}, fmt.Errorf("%s Codex agents is not supported yet", l.gerund)
	default:
		return Plan{}, fmt.Errorf("provider %q does not support %s agents", string(agent.Provider), l.gerund)
	}
	if agent.LiveState == apiv1.LiveStateNotLive {
		return Plan{}, errors.New("agent is not running")
	}
	if agent.SourceKind != "background" {
		return Plan{}, errors.New(l.refusal)
	}
	shortID := strings.TrimSpace(agent.NativeShortID)
	if shortID == "" {
		return Plan{}, fmt.Errorf("Claude session has no background session ID to %s", l.verb)
	}
	program := strings.TrimSpace(claudeBin)
	if program == "" {
		return Plan{}, errors.New("Claude executable is not configured")
	}
	return Plan{Program: program, Arguments: []string{l.command, shortID}, Mode: l.mode}, nil
}
