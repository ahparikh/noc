// Package claude adapts `claude agents --json` roster snapshots to the
// provider adapter contract.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	defaultCommandTimeout = 5 * time.Second
	defaultExecutable     = "claude"
	executableEnv         = "NOC_CLAUDE_BIN"

	kindInteractive = "interactive"
	kindBackground  = "background"
)

var _ provider.Adapter = (*Adapter)(nil)

// Options configures an Adapter. Zero values select production defaults.
type Options struct {
	Runner         provider.CommandRunner
	Executable     string
	CommandTimeout time.Duration
	Now            func() time.Time
}

// Adapter discovers live Claude Code sessions from the agent roster.
type Adapter struct {
	runner     provider.CommandRunner
	executable string
	timeout    time.Duration
	now        func() time.Time
}

// New returns an adapter configured from the environment.
func New() *Adapter { return NewWithOptions(Options{}) }

// NewWithOptions returns an adapter with o applied over the defaults.
func NewWithOptions(o Options) *Adapter {
	a := &Adapter{
		runner:     o.Runner,
		executable: resolveExecutable(o.Executable),
		timeout:    o.CommandTimeout,
		now:        o.Now,
	}
	if a.runner == nil {
		a.runner = provider.NewExecCommandRunner()
	}
	if a.timeout <= 0 {
		a.timeout = defaultCommandTimeout
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a
}

func resolveExecutable(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(executableEnv)); v != "" {
		return v
	}
	return defaultExecutable
}

// Provider returns the constant provider enum.
func (a *Adapter) Provider() apiv1.Provider { return apiv1.ProviderClaude }

func (a *Adapter) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return a.runner.Run(ctx, a.executable, args...)
}

// Probe checks the CLI version and that the agent roster command exists.
func (a *Adapter) Probe(ctx context.Context) provider.ProbeResult {
	observed := a.now().UTC()
	out, err := a.run(ctx, "--version")
	if err != nil {
		return provider.ProbeResult{Provider: apiv1.ProviderClaude, Diagnostic: err.Error(), ObservedAt: observed}
	}
	version := strings.TrimSpace(string(out))
	if _, err := a.run(ctx, "agents", "--help"); err != nil {
		return provider.ProbeResult{
			Provider:   apiv1.ProviderClaude,
			Version:    version,
			Diagnostic: "Claude live discovery unavailable: " + err.Error(),
			ObservedAt: observed,
		}
	}
	return provider.ProbeResult{
		Provider:   apiv1.ProviderClaude,
		Available:  true,
		Version:    version,
		Features:   map[string]bool{"discover_live": true},
		ObservedAt: observed,
	}
}

// rosterEntry holds the only roster keys the adapter decodes.
type rosterEntry struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	Kind       string `json:"kind"`
	StartedAt  int64  `json:"startedAt"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	State      string `json:"state"`
	PID        *int   `json:"pid"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
}

// evidence is the sanitized state carried in NativeAgent.Raw.
type evidence struct {
	State        string `json:"state,omitempty"`
	Status       string `json:"status,omitempty"`
	WaitingFor   string `json:"waiting_for,omitempty"`
	StartedAt    int64  `json:"started_at,omitempty"`
	ProcessAlive bool   `json:"process_alive"`
}

// nativeState is the narrower persisted shape (no start time).
type nativeState struct {
	State        string `json:"state,omitempty"`
	Status       string `json:"status,omitempty"`
	WaitingFor   string `json:"waiting_for,omitempty"`
	ProcessAlive bool   `json:"process_alive"`
}

// DiscoverLive returns one snapshot of live, local Claude sessions.
func (a *Adapter) DiscoverLive(ctx context.Context) ([]provider.NativeAgent, error) {
	out, err := a.run(ctx, "agents", "--json")
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode Claude agent roster: %w", err)
	}
	agents := make([]provider.NativeAgent, 0, len(rows))
	for _, row := range rows {
		// One incompatible row must not hide all other sessions. The
		// provider's next snapshot gets another chance to report it.
		var e rosterEntry
		if err := json.Unmarshal(row, &e); err != nil {
			continue
		}
		if e.Kind != kindInteractive && e.Kind != kindBackground {
			continue
		}
		sessionID := strings.TrimSpace(e.SessionID)
		if sessionID == "" {
			continue
		}
		raw, err := json.Marshal(evidence{
			State:        e.State,
			Status:       e.Status,
			WaitingFor:   e.WaitingFor,
			StartedAt:    e.StartedAt,
			ProcessAlive: e.PID != nil && *e.PID > 0,
		})
		if err != nil {
			return nil, fmt.Errorf("encode sanitized Claude state: %w", err)
		}
		cwd := strings.TrimSpace(e.CWD)
		if cwd != "" {
			cwd = filepath.Clean(cwd)
		}
		agents = append(agents, provider.NativeAgent{
			NativeSessionID: sessionID,
			NativeShortID:   strings.TrimSpace(e.ID),
			Name:            sessionName(e.Name, e.ID, e.SessionID),
			SourceKind:      e.Kind,
			CWD:             cwd,
			Raw:             raw,
		})
	}
	return agents, nil
}

// sessionName drops the placeholder Claude reports for unnamed sessions: the
// short or full session ID.
func sessionName(name, id, sessionID string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == strings.TrimSpace(id) || name == strings.TrimSpace(sessionID) {
		return ""
	}
	return name
}

// Normalize maps one native row to provider-neutral evidence.
func (a *Adapter) Normalize(agent provider.NativeAgent) (provider.NormalizedAgent, error) {
	sessionID := strings.TrimSpace(agent.NativeSessionID)
	if sessionID == "" {
		return provider.NormalizedAgent{}, fmt.Errorf("Claude session has no stable session ID")
	}
	if agent.SourceKind != kindInteractive && agent.SourceKind != kindBackground {
		return provider.NormalizedAgent{}, fmt.Errorf("unsupported Claude session kind %q", agent.SourceKind)
	}
	var ev evidence
	if err := json.Unmarshal(agent.Raw, &ev); err != nil {
		return provider.NormalizedAgent{}, fmt.Errorf("decode sanitized Claude state: %w", err)
	}

	activity, attention := normalizeActivity(ev)
	var created *time.Time
	if ev.StartedAt > 0 {
		t := time.UnixMilli(ev.StartedAt).UTC()
		created = &t
	}
	var workspace *apiv1.Workspace
	if agent.CWD != "" {
		workspace = &apiv1.Workspace{
			Path:       agent.CWD,
			Kind:       "directory",
			Owner:      "unknown",
			ObservedAt: a.now().UTC(),
		}
	}
	state, err := json.Marshal(nativeState{
		State:        ev.State,
		Status:       ev.Status,
		WaitingFor:   ev.WaitingFor,
		ProcessAlive: ev.ProcessAlive,
	})
	if err != nil {
		return provider.NormalizedAgent{}, fmt.Errorf("encode normalized Claude state: %w", err)
	}
	attach := apiv1.Capability{Supported: agent.NativeShortID != ""}
	if !attach.Supported {
		attach.Reason = "interactive session has no background attach ID"
	}
	return provider.NormalizedAgent{
		Provider:        apiv1.ProviderClaude,
		NativeSessionID: sessionID,
		NativeShortID:   agent.NativeShortID,
		Name:            agent.Name,
		SourceKind:      agent.SourceKind,
		LiveState:       apiv1.LiveStateLive,
		RuntimeState:    normalizeRuntime(ev),
		ActivityState:   activity,
		Attention:       attention,
		CreatedAt:       created,
		Capabilities: map[string]apiv1.Capability{
			"attach": attach,
			"resume": {Supported: true},
		},
		NativeState: state,
		Workspace:   workspace,
	}, nil
}

func normalizeRuntime(ev evidence) apiv1.RuntimeState {
	switch {
	case ev.State == "failed":
		return apiv1.RuntimeStateFailed
	case ev.State == "stopped":
		return apiv1.RuntimeStateStopped
	case ev.ProcessAlive:
		return apiv1.RuntimeStateRunning
	}
	switch ev.State {
	case "working", "blocked", "done":
		return apiv1.RuntimeStateResumable
	}
	return apiv1.RuntimeStateUnknown
}

func normalizeActivity(ev evidence) (apiv1.ActivityState, apiv1.Attention) {
	none := apiv1.Attention{Type: apiv1.AttentionNone}
	wf := strings.TrimSpace(ev.WaitingFor)
	switch ev.State {
	case "working":
		return apiv1.ActivityStateWorking, none
	case "blocked":
		// A blocked turn with no reason finished its turn and waits for the
		// next message.
		if wf == "" {
			return apiv1.ActivityStateIdle, none
		}
		return apiv1.ActivityStateWaiting, normalizeWaitingFor(wf, true)
	case "done", "stopped":
		return apiv1.ActivityStateIdle, none
	case "failed":
		return apiv1.ActivityStateUnknown, apiv1.Attention{Type: apiv1.AttentionError, Detail: "Claude session failed"}
	}
	switch ev.Status {
	case "busy":
		return apiv1.ActivityStateWorking, none
	case "waiting":
		return apiv1.ActivityStateWaiting, normalizeWaitingFor(wf, false)
	case "idle":
		return apiv1.ActivityStateIdle, none
	}
	return apiv1.ActivityStateUnknown, none
}

func normalizeWaitingFor(detail string, blocked bool) apiv1.Attention {
	switch detail {
	case "permission prompt", "sandbox request":
		return apiv1.Attention{Type: apiv1.AttentionApprovalRequired, Detail: detail}
	case "input needed", "worker request", "dialog open":
		return apiv1.Attention{Type: apiv1.AttentionInputRequired, Detail: detail}
	case "":
		if blocked {
			return apiv1.Attention{Type: apiv1.AttentionInputRequired}
		}
		return apiv1.Attention{Type: apiv1.AttentionNone}
	}
	if blocked {
		return apiv1.Attention{Type: apiv1.AttentionInputRequired, Detail: detail}
	}
	return apiv1.Attention{Type: apiv1.AttentionUnknown, Detail: detail}
}
