// Package codex adapts the Codex shared app-server to the provider adapter
// contract. Discovery reads loaded-thread metadata over the daemon's Unix
// control socket (WebSocket + JSON-RPC), with a `codex app-server proxy`
// fallback when the inferred socket cannot be dialed.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	defaultOperationTimeout = 5 * time.Second
	defaultExecutable       = "codex"

	executableEnv = "NOC_CODEX_BIN"
	socketEnv     = "NOC_CODEX_SOCKET"

	maxModelRunes     = 256
	maxReasoningRunes = 64
	maxPreviewRunes   = 2048

	threadSourceLabel  = "codex_app_server_thread"
	previewSourceLabel = "codex_app_server_preview"

	statusActive      = "active"
	statusIdle        = "idle"
	statusSystemError = "systemError"
	statusNotLoaded   = "notLoaded"

	flagWaitingOnApproval  = "waitingOnApproval"
	flagWaitingOnUserInput = "waitingOnUserInput"
)

// acceptedSources are the top-level thread sources the adapter reports.
var acceptedSources = []string{"cli", "vscode", "appServer"}

var (
	_ provider.Adapter = (*Adapter)(nil)
	_ provider.Renamer = (*Adapter)(nil)
)

// DiscoveryClient returns one snapshot of live Codex threads.
type DiscoveryClient interface {
	DiscoverLive(ctx context.Context) ([]provider.NativeAgent, error)
}

// ThreadNamer is an optional capability of a DiscoveryClient: pushing a thread
// name to Codex.
type ThreadNamer interface {
	SetThreadName(ctx context.Context, threadID, name string) error
}

// Options configures an Adapter. Zero values select production defaults.
type Options struct {
	Runner           provider.CommandRunner
	Executable       string
	SocketPath       string
	OperationTimeout time.Duration
	Now              func() time.Time
	Client           DiscoveryClient
}

// Adapter discovers live Codex threads and pushes thread names.
type Adapter struct {
	runner     provider.CommandRunner
	executable string
	socketPath string
	timeout    time.Duration
	now        func() time.Time
	client     DiscoveryClient
}

// New returns an adapter configured from the environment.
func New() *Adapter { return NewWithOptions(Options{}) }

// NewWithOptions returns an adapter with o applied over the defaults.
func NewWithOptions(o Options) *Adapter {
	a := &Adapter{
		runner:     o.Runner,
		executable: resolveExecutable(o.Executable),
		timeout:    o.OperationTimeout,
		now:        o.Now,
		client:     o.Client,
	}
	if a.runner == nil {
		a.runner = provider.NewExecCommandRunner()
	}
	if a.timeout <= 0 {
		a.timeout = defaultOperationTimeout
	}
	if a.now == nil {
		a.now = time.Now
	}
	path, explicit := resolveSocketPath(o.SocketPath)
	a.socketPath = path
	if a.client == nil {
		// Only an inferred socket may fall back to the proxy; an explicit one
		// must fail visibly.
		var fallback DiscoveryClient
		if !explicit {
			fallback = newProxyClient(a.executable, a.timeout)
		}
		a.client = newSocketClient(path, a.timeout, fallback)
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
func (a *Adapter) Provider() apiv1.Provider { return apiv1.ProviderCodex }

func (a *Adapter) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return a.runner.Run(ctx, a.executable, args...)
}

// Probe checks the CLI version and that the app-server subcommand exists. It
// never touches the control socket or starts a daemon.
func (a *Adapter) Probe(ctx context.Context) provider.ProbeResult {
	observed := a.now().UTC()
	out, err := a.run(ctx, "--version")
	if err != nil {
		return provider.ProbeResult{Provider: apiv1.ProviderCodex, Diagnostic: err.Error(), ObservedAt: observed}
	}
	version := strings.TrimSpace(string(out))
	if _, err := a.run(ctx, "app-server", "--help"); err != nil {
		return provider.ProbeResult{
			Provider:   apiv1.ProviderCodex,
			Version:    version,
			Diagnostic: "Codex live discovery unavailable; installed CLI does not expose app-server: " + err.Error(),
			ObservedAt: observed,
		}
	}
	return provider.ProbeResult{
		Provider:  apiv1.ProviderCodex,
		Available: true,
		Version:   version,
		// direct_unix_socket claims client capability only; reachability is
		// measured by each snapshot.
		Features:   map[string]bool{"discover_live": true, "direct_unix_socket": true},
		ObservedAt: observed,
	}
}

// DiscoverLive delegates to the configured client.
func (a *Adapter) DiscoverLive(ctx context.Context) ([]provider.NativeAgent, error) {
	agents, err := a.client.DiscoverLive(ctx)
	if err != nil {
		return nil, fmt.Errorf("query Codex app-server: %w", err)
	}
	return agents, nil
}

// SetName pushes a thread name to Codex. The caller validates name.
func (a *Adapter) SetName(ctx context.Context, nativeSessionID, name string) error {
	namer, ok := a.client.(ThreadNamer)
	if !ok {
		return provider.ErrNotImplemented
	}
	if err := namer.SetThreadName(ctx, nativeSessionID, name); err != nil {
		return fmt.Errorf("rename Codex thread: %w", err)
	}
	return nil
}

// evidence is the sanitized thread state carried in NativeAgent.Raw.
type evidence struct {
	Status           string   `json:"status"`
	ActiveFlags      []string `json:"active_flags,omitempty"`
	CreatedAt        int64    `json:"created_at,omitempty"`
	UpdatedAt        int64    `json:"updated_at,omitempty"`
	Model            string   `json:"model,omitempty"`
	ReasoningEffort  string   `json:"reasoning_effort,omitempty"`
	Preview          string   `json:"preview,omitempty"`
	PreviewTruncated bool     `json:"preview_truncated,omitempty"`
}

// nativeState is the narrower persisted shape.
type nativeState struct {
	Status      string   `json:"status"`
	ActiveFlags []string `json:"active_flags,omitempty"`
}

// Normalize maps one native row to provider-neutral evidence.
func (a *Adapter) Normalize(agent provider.NativeAgent) (provider.NormalizedAgent, error) {
	threadID := strings.TrimSpace(agent.NativeSessionID)
	if threadID == "" {
		return provider.NormalizedAgent{}, fmt.Errorf("Codex thread has no stable thread ID")
	}
	if !slices.Contains(acceptedSources, agent.SourceKind) {
		return provider.NormalizedAgent{}, fmt.Errorf("unsupported Codex thread source %q", agent.SourceKind)
	}
	var ev evidence
	if err := json.Unmarshal(agent.Raw, &ev); err != nil {
		return provider.NormalizedAgent{}, fmt.Errorf("decode sanitized Codex state: %w", err)
	}
	if ev.Status == statusNotLoaded {
		return provider.NormalizedAgent{}, fmt.Errorf("Codex thread is not loaded")
	}

	now := a.now().UTC()
	runtime, activity, attention := normalizeStatus(ev)

	var created, updated *time.Time
	if ev.CreatedAt > 0 {
		t := time.Unix(ev.CreatedAt, 0).UTC()
		created = &t
	}
	if ev.UpdatedAt > 0 {
		t := time.Unix(ev.UpdatedAt, 0).UTC()
		updated = &t
	}
	var workspace *apiv1.Workspace
	if agent.CWD != "" {
		workspace = &apiv1.Workspace{
			Path:       filepath.Clean(agent.CWD),
			Kind:       "directory",
			Owner:      "unknown",
			ObservedAt: now,
		}
	}
	var preview *apiv1.TextPreview
	if ev.Preview != "" {
		preview = &apiv1.TextPreview{
			Text:       ev.Preview,
			Source:     previewSourceLabel,
			Truncated:  ev.PreviewTruncated,
			ObservedAt: now,
		}
	}
	state, err := json.Marshal(nativeState{Status: ev.Status, ActiveFlags: ev.ActiveFlags})
	if err != nil {
		return provider.NormalizedAgent{}, fmt.Errorf("encode normalized Codex state: %w", err)
	}
	return provider.NormalizedAgent{
		Provider:        apiv1.ProviderCodex,
		NativeSessionID: threadID,
		Name:            agent.Name,
		SourceKind:      agent.SourceKind,
		LiveState:       apiv1.LiveStateLive,
		RuntimeState:    runtime,
		ActivityState:   activity,
		Attention:       attention,
		Model:           ev.Model,
		// Sources are labeled even when the value is empty; the registry
		// ignores empty values.
		ModelSource:     threadSourceLabel,
		ReasoningEffort: ev.ReasoningEffort,
		ReasoningSource: threadSourceLabel,
		InitialRequest:  preview,
		CreatedAt:       created,
		LastActivityAt:  updated,
		Capabilities: map[string]apiv1.Capability{
			"attach": {Supported: true},
			"resume": {Supported: true},
		},
		NativeState: state,
		Workspace:   workspace,
	}, nil
}

func normalizeStatus(ev evidence) (apiv1.RuntimeState, apiv1.ActivityState, apiv1.Attention) {
	switch ev.Status {
	case statusActive:
		// Approval beats user input when both flags are set.
		switch {
		case slices.Contains(ev.ActiveFlags, flagWaitingOnApproval):
			return apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting,
				apiv1.Attention{Type: apiv1.AttentionApprovalRequired, Detail: "Codex is waiting on approval"}
		case slices.Contains(ev.ActiveFlags, flagWaitingOnUserInput):
			return apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting,
				apiv1.Attention{Type: apiv1.AttentionInputRequired, Detail: "Codex is waiting on user input"}
		}
		return apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking, apiv1.Attention{Type: apiv1.AttentionNone}
	case statusIdle:
		return apiv1.RuntimeStateLoaded, apiv1.ActivityStateIdle, apiv1.Attention{Type: apiv1.AttentionNone}
	case statusSystemError:
		return apiv1.RuntimeStateFailed, apiv1.ActivityStateUnknown,
			apiv1.Attention{Type: apiv1.AttentionError, Detail: "Codex app-server reported a system error"}
	case statusNotLoaded:
		return apiv1.RuntimeStateResumable, apiv1.ActivityStateUnknown, apiv1.Attention{Type: apiv1.AttentionNone}
	}
	return apiv1.RuntimeStateUnknown, apiv1.ActivityStateUnknown, apiv1.Attention{Type: apiv1.AttentionUnknown}
}

// boundedText compacts whitespace (split on any Unicode whitespace, rejoin
// with single spaces) and then cuts to the first limit runes. truncated
// reports whether the compacted text exceeded limit.
func boundedText(value string, limit int) (text string, truncated bool) {
	text = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:limit]), true
}
