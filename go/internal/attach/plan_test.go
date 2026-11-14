package attach

import (
	"reflect"
	"testing"

	apiv1 "noc.local/noc/pkg/api/v1"
)

func TestPlanForAgentUsesNativeProviderEntryPoints(t *testing.T) {
	bins := Binaries{Claude: " claude-bin ", Codex: " codex-bin "}
	attachable := map[string]apiv1.Capability{"attach": {Supported: true}}
	tests := []struct {
		name  string
		agent apiv1.Agent
		want  Plan
	}{
		{
			name: "claude live attach",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderClaude, NativeSessionID: "sess", NativeShortID: " short ",
				LiveState: apiv1.LiveStateLive, Capabilities: attachable,
				Workspace: &apiv1.Workspace{Path: " /work "},
			},
			want: Plan{Program: "claude-bin", Arguments: []string{"attach", "short"}, Directory: "/work", Mode: ModeAttach},
		},
		{
			name: "claude attach needs live state",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderClaude, NativeSessionID: " sess ", NativeShortID: "short",
				LiveState: apiv1.LiveStateNotLive, Capabilities: attachable,
			},
			want: Plan{Program: "claude-bin", Arguments: []string{"--resume", "sess"}, Mode: ModeResume},
		},
		{
			name: "claude attach needs short id",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderClaude, NativeSessionID: "sess", NativeShortID: "  ",
				LiveState: apiv1.LiveStateLive, Capabilities: attachable,
			},
			want: Plan{Program: "claude-bin", Arguments: []string{"--resume", "sess"}, Mode: ModeResume},
		},
		{
			name: "claude attach needs capability",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderClaude, NativeSessionID: "sess", NativeShortID: "short",
				LiveState: apiv1.LiveStateLive,
			},
			want: Plan{Program: "claude-bin", Arguments: []string{"--resume", "sess"}, Mode: ModeResume},
		},
		{
			name: "claude attach beats denied resume",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderClaude, NativeSessionID: "sess", NativeShortID: "short",
				LiveState: apiv1.LiveStateLive,
				Capabilities: map[string]apiv1.Capability{
					"attach": {Supported: true}, "resume": {Supported: false, Reason: "no"},
				},
			},
			want: Plan{Program: "claude-bin", Arguments: []string{"attach", "short"}, Mode: ModeAttach},
		},
		{
			name: "codex resume",
			agent: apiv1.Agent{
				Provider: apiv1.ProviderCodex, NativeSessionID: " thread ",
				Workspace:    &apiv1.Workspace{Path: "  "},
				Capabilities: map[string]apiv1.Capability{"resume": {Supported: true}},
			},
			want: Plan{Program: "codex-bin", Arguments: []string{"resume", "thread"}, Mode: ModeResume},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PlanForAgent(tt.agent, bins)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("plan = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPlanForAgentRejectsMissingIdentityAndDeniedResume(t *testing.T) {
	bins := DefaultBinaries
	denied := map[string]apiv1.Capability{"resume": {Supported: false, Reason: " too old "}}
	deniedNoReason := map[string]apiv1.Capability{"resume": {Supported: false}}
	tests := []struct {
		name  string
		agent apiv1.Agent
		bins  Binaries
		want  string
	}{
		{"blank session", apiv1.Agent{Provider: apiv1.ProviderClaude, NativeSessionID: "  "}, bins, "agent has no native session ID"},
		{"blank session beats provider", apiv1.Agent{Provider: "other"}, bins, "agent has no native session ID"},
		{"claude no binary", apiv1.Agent{Provider: apiv1.ProviderClaude, NativeSessionID: "s"}, Binaries{Claude: " "}, "Claude executable is not configured"},
		{"claude denied", apiv1.Agent{Provider: apiv1.ProviderClaude, NativeSessionID: "s", Capabilities: denied}, bins, "agent cannot resume: too old"},
		{"claude denied no reason", apiv1.Agent{Provider: apiv1.ProviderClaude, NativeSessionID: "s", Capabilities: deniedNoReason}, bins, "agent cannot resume"},
		{"codex denied before binary", apiv1.Agent{Provider: apiv1.ProviderCodex, NativeSessionID: "s", Capabilities: denied}, Binaries{}, "agent cannot resume: too old"},
		{"codex no binary", apiv1.Agent{Provider: apiv1.ProviderCodex, NativeSessionID: "s"}, Binaries{Codex: ""}, "Codex executable is not configured"},
		{"other provider", apiv1.Agent{Provider: "gemini", NativeSessionID: "s"}, bins, `provider "gemini" does not support terminal attachment`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanForAgent(tt.agent, tt.bins)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConcurrentUseRiskOnlyBlocksLiveClaudeResume(t *testing.T) {
	zellij := func(sessions ...string) []apiv1.FrontendBinding {
		var out []apiv1.FrontendBinding
		for _, s := range sessions {
			out = append(out, apiv1.FrontendBinding{Kind: "zellij", Session: s})
		}
		return out
	}
	live := apiv1.Agent{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateLive}
	resume := Plan{Mode: ModeResume}

	tests := []struct {
		name       string
		agent      apiv1.Agent
		plan       Plan
		wantRisk   bool
		wantSuffix string
	}{
		{"live claude resume", live, resume, true, ""},
		{"attach mode", live, Plan{Mode: ModeAttach}, false, ""},
		{"background resume mode", live, Plan{Mode: ModeBackgroundResume}, false, ""},
		{"not live", apiv1.Agent{Provider: apiv1.ProviderClaude, LiveState: apiv1.LiveStateCandidateStale}, resume, false, ""},
		{"codex", apiv1.Agent{Provider: apiv1.ProviderCodex, LiveState: apiv1.LiveStateLive}, resume, false, ""},
		{"one session", withBindings(live, zellij("main")), resume, true, `; it appears to be open in Zellij session "main"`},
		{"several sessions", withBindings(live, zellij("a", " b ", "a", "")), resume, true, "; it appears to be open in Zellij sessions a, b"},
		{"ignores other kinds and blanks", withBindings(live, append(zellij(" "), apiv1.FrontendBinding{Kind: "tmux", Session: "x"})), resume, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			risk, suffix := ConcurrentUseRisk(tt.agent, tt.plan)
			if risk != tt.wantRisk || suffix != tt.wantSuffix {
				t.Fatalf("got (%v, %q), want (%v, %q)", risk, suffix, tt.wantRisk, tt.wantSuffix)
			}
		})
	}
}

func withBindings(agent apiv1.Agent, bindings []apiv1.FrontendBinding) apiv1.Agent {
	agent.FrontendBindings = bindings
	return agent
}
