package attach

import (
	"reflect"
	"testing"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestStopPlanStopsOnlyLiveClaudeBackgroundSessions(t *testing.T) {
	claude := func(live apiv1.LiveState, source, short string) apiv1.Agent {
		return apiv1.Agent{Provider: apiv1.ProviderClaude, LiveState: live, SourceKind: source, NativeShortID: short}
	}
	errTests := []struct {
		name   string
		agent  apiv1.Agent
		bin    string
		wanted string
	}{
		{"codex", apiv1.Agent{Provider: apiv1.ProviderCodex, LiveState: apiv1.LiveStateLive}, "claude", "stopping Codex agents is not supported yet"},
		{"other", apiv1.Agent{Provider: "gemini"}, "claude", `provider "gemini" does not support stopping agents`},
		{"not live", claude(apiv1.LiveStateNotLive, "background", "abc"), "claude", "agent is not running"},
		{"not live non-background", claude(apiv1.LiveStateNotLive, "interactive", ""), "claude", "agent is not running"},
		{"live interactive", claude(apiv1.LiveStateLive, "interactive", "abc"), "claude", "only background Claude sessions can be stopped; end this one from its terminal with /exit"},
		{"stale empty source", claude(apiv1.LiveStateCandidateStale, "", "abc"), "claude", "only background Claude sessions can be stopped; end this one from its terminal with /exit"},
		{"unknown", claude(apiv1.LiveStateUnknown, "interactive", ""), "claude", "only background Claude sessions can be stopped; end this one from its terminal with /exit"},
		{"empty live state", claude("", "interactive", ""), "claude", "only background Claude sessions can be stopped; end this one from its terminal with /exit"},
		{"blank short id", claude(apiv1.LiveStateLive, "background", "  "), "claude", "Claude session has no background session ID to stop"},
		{"no binary", claude(apiv1.LiveStateLive, "background", "abc"), "  ", "Claude executable is not configured"},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StopPlan(tt.agent, tt.bin)
			if err == nil || err.Error() != tt.wanted {
				t.Fatalf("err = %v, want %q", err, tt.wanted)
			}
		})
	}

	for _, live := range []apiv1.LiveState{apiv1.LiveStateLive, apiv1.LiveStateCandidateStale, apiv1.LiveStateUnknown, ""} {
		t.Run("stops "+string(live), func(t *testing.T) {
			agent := claude(live, "background", " abc ")
			agent.Workspace = &apiv1.Workspace{Path: "/ignored"}
			got, err := StopPlan(agent, " claude-bin ")
			if err != nil {
				t.Fatal(err)
			}
			want := Plan{Program: "claude-bin", Arguments: []string{"stop", "abc"}, Mode: ModeStop}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("plan = %#v, want %#v", got, want)
			}
		})
	}
}
