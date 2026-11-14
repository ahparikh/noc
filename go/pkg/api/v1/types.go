// Package apiv1 is the public, versioned JSON contract shared by the noc
// server, the noc CLI, and independent frontends. It is the only public Go
// package in the module.
package apiv1

import (
	"encoding/json"
	"strings"
	"time"
)

// SchemaVersion is carried by every top-level public object.
const SchemaVersion = 1

// Provider identifies a coding-agent provider.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// LiveState records whether current evidence includes the session.
type LiveState string

const (
	LiveStateLive           LiveState = "live"
	LiveStateCandidateStale LiveState = "candidate_stale"
	LiveStateNotLive        LiveState = "not_live"
	LiveStateUnknown        LiveState = "unknown"
)

// RuntimeState records the provider process state.
type RuntimeState string

const (
	RuntimeStateStarting  RuntimeState = "starting"
	RuntimeStateRunning   RuntimeState = "running"
	RuntimeStateLoaded    RuntimeState = "loaded"
	RuntimeStateResumable RuntimeState = "resumable"
	RuntimeStateStopped   RuntimeState = "stopped"
	RuntimeStateFailed    RuntimeState = "failed"
	RuntimeStateUnknown   RuntimeState = "unknown"
)

// ActivityState records what the conversation is doing.
type ActivityState string

const (
	ActivityStateWorking ActivityState = "working"
	ActivityStateWaiting ActivityState = "waiting"
	ActivityStateIdle    ActivityState = "idle"
	ActivityStateUnknown ActivityState = "unknown"
)

// AttentionType records why the agent needs the user.
type AttentionType string

const (
	AttentionNone                   AttentionType = "none"
	AttentionInputRequired          AttentionType = "input_required"
	AttentionApprovalRequired       AttentionType = "approval_required"
	AttentionAuthenticationRequired AttentionType = "authentication_required"
	AttentionError                  AttentionType = "error"
	AttentionUnknown                AttentionType = "unknown"
)

// DisplayState is the single server-derived status used for sorting and labels.
type DisplayState string

const (
	DisplayStateNeedsApproval       DisplayState = "needs_approval"
	DisplayStateNeedsInput          DisplayState = "needs_input"
	DisplayStateNeedsAuthentication DisplayState = "needs_authentication"
	DisplayStateError               DisplayState = "error"
	DisplayStateWorking             DisplayState = "working"
	DisplayStateWaiting             DisplayState = "waiting"
	// DisplayStateReady: finished its turn, process still running; "your move".
	DisplayStateReady DisplayState = "ready"
	// DisplayStateIdle: live but nothing running (e.g. exited-but-resumable job).
	DisplayStateIdle    DisplayState = "idle"
	DisplayStateStale   DisplayState = "stale"
	DisplayStateNotLive DisplayState = "not_live"
)

// Attention describes why an agent needs the user.
type Attention struct {
	Type            AttentionType `json:"type"`
	Since           *time.Time    `json:"since,omitempty"`
	Detail          string        `json:"detail,omitempty"`
	NativeRequestID string        `json:"native_request_id,omitempty"`
}

// Capability records whether an action is supported for an agent.
type Capability struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// TextPreview is a bounded, sourced excerpt; never a transcript.
type TextPreview struct {
	Text       string    `json:"text"`
	Source     string    `json:"source"`
	Truncated  bool      `json:"truncated,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// FrontendBinding is presence evidence inherited by a hook: a navigation
// hint, not proof of a connected client.
type FrontendBinding struct {
	Kind       string    `json:"kind"`
	Session    string    `json:"session,omitempty"`
	PaneID     string    `json:"pane_id,omitempty"`
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}

// Workspace is the host-local directory/checkout associated with an agent.
type Workspace struct {
	ID           string    `json:"id,omitempty"`
	Path         string    `json:"path"`
	RootPath     string    `json:"root_path,omitempty"`
	Kind         string    `json:"kind,omitempty"`
	RepositoryID string    `json:"repository_id,omitempty"`
	CommonGitDir string    `json:"common_git_dir,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	Owner        string    `json:"owner,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
}

// Agent is one durable, top-level provider conversation.
type Agent struct {
	SchemaVersion       int                   `json:"schema_version"`
	ID                  string                `json:"id"`
	HostID              string                `json:"host_id"`
	Provider            Provider              `json:"provider"`
	NativeSessionID     string                `json:"native_session_id"`
	NativeShortID       string                `json:"native_short_id,omitempty"`
	UserName            string                `json:"user_name,omitempty"`
	Team                string                `json:"team,omitempty"`
	Lead                bool                  `json:"lead,omitempty"`
	ParentID            string                `json:"parent_id,omitempty"`
	Name                string                `json:"name,omitempty"`
	SourceKind          string                `json:"source_kind,omitempty"`
	Origin              string                `json:"origin,omitempty"`
	LiveState           LiveState             `json:"live_state"`
	RuntimeState        RuntimeState          `json:"runtime_state"`
	ActivityState       ActivityState         `json:"activity_state"`
	Attention           Attention             `json:"attention"`
	DisplayState        DisplayState          `json:"display_state"`
	Bucket              Bucket                `json:"bucket,omitempty"`
	Model               string                `json:"model,omitempty"`
	ModelSource         string                `json:"model_source,omitempty"`
	ModelObservedAt     *time.Time            `json:"model_observed_at,omitempty"`
	ReasoningEffort     string                `json:"reasoning_effort,omitempty"`
	ReasoningSource     string                `json:"reasoning_source,omitempty"`
	ReasoningObservedAt *time.Time            `json:"reasoning_observed_at,omitempty"`
	InitialRequest      *TextPreview          `json:"initial_request,omitempty"`
	Summary             string                `json:"summary,omitempty"`
	SummarySource       string                `json:"summary_source,omitempty"`
	SummaryUpdatedAt    *time.Time            `json:"summary_updated_at,omitempty"`
	SummaryTruncated    bool                  `json:"summary_truncated,omitempty"`
	CreatedAt           *time.Time            `json:"created_at,omitempty"`
	FirstSeenAt         time.Time             `json:"first_seen_at"`
	LastSeenAt          time.Time             `json:"last_seen_at"`
	LastActivityAt      *time.Time            `json:"last_activity_at,omitempty"`
	SnapshotConfirmedAt *time.Time            `json:"snapshot_confirmed_at,omitempty"`
	EndedAt             *time.Time            `json:"ended_at,omitempty"`
	Capabilities        map[string]Capability `json:"capabilities,omitempty"`
	NativeState         json.RawMessage       `json:"native_state,omitempty"`
	Workspace           *Workspace            `json:"workspace,omitempty"`
	FrontendBindings    []FrontendBinding     `json:"frontend_bindings,omitempty"`
	ProcessID           int                   `json:"process_id,omitempty"`
}

// AgentNameUpdate is the request body for PUT /v1/agents/{id}/name.
// An empty name clears the override.
type AgentNameUpdate struct {
	Name string `json:"name"`
}

// NoTeam is the reserved word that means "in no team" wherever a team name
// is accepted. It is never a team's name.
const NoTeam = "none"

// MaxTeamNameLength bounds a team name.
const MaxTeamNameLength = 48

// ValidTeamName reports whether name can name a team: 1 to 48 characters
// from A-Za-z0-9._-, not starting with '-' or '.', and not "none".
func ValidTeamName(name string) bool {
	if name == "" || len(name) > MaxTeamNameLength || name == NoTeam {
		return false
	}
	if name[0] == '-' || name[0] == '.' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// AgentTeamUpdate is the request body for PUT /v1/agents/{id}/team. An empty
// team takes the agent out of its team. Lead marks the agent as the lead of
// the team named, which it must already be in.
type AgentTeamUpdate struct {
	Team string `json:"team"`
	Lead bool   `json:"lead,omitempty"`
}

// AgentSpawn is the request body for POST /v1/agents: the registration of a
// provider session the caller is about to start. The server never starts it.
type AgentSpawn struct {
	Provider        Provider `json:"provider"`
	NativeSessionID string   `json:"native_session_id"`
	// Team is a team name, "none" for no team, or empty to inherit the
	// parent's team.
	Team      string `json:"team,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Directory string `json:"directory,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

// Team is one row of `noc teams`: a summary computed from the agents that
// carry the team's name. The row for agents in no team has an empty name.
type Team struct {
	Name     string `json:"team"`
	LeadID   string `json:"lead_id,omitempty"`
	LeadName string `json:"lead_name,omitempty"`
	Agents   int    `json:"agents"`
	Blocked  int    `json:"blocked"`
	Working  int    `json:"working"`
	Review   int    `json:"review"`
	Quiet    int    `json:"quiet"`
}

// TeamList is what `noc teams --json` writes.
type TeamList struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Teams         []Team    `json:"teams"`
}

// ProviderNameSync reports whether a rename reached the provider.
// The body of a rename response stays the updated agent; the sync result
// travels in a header.
type ProviderNameSync string

const (
	ProviderNameSyncHeader      = "Noc-Provider-Name-Sync"
	ProviderNameSyncErrorHeader = "Noc-Provider-Name-Sync-Error"

	// ProviderNameSynced: the provider session now carries the name.
	ProviderNameSynced ProviderNameSync = "synced"
	// ProviderNameUnsupported: the provider cannot be renamed from outside.
	ProviderNameUnsupported ProviderNameSync = "unsupported"
	// ProviderNameFailed: the provider supports rename but the attempt failed.
	ProviderNameFailed ProviderNameSync = "failed"
	// ProviderNameNotAttempted: nothing to push (clearing a name).
	ProviderNameNotAttempted ProviderNameSync = "not_attempted"
)

// ObservationSource records where an observation came from.
type ObservationSource string

const (
	ObservationSourceHook     ObservationSource = "hook"
	ObservationSourceSnapshot ObservationSource = "snapshot"
)

// Observation is the hook-outbox / observation-log envelope. It is persisted,
// never served by any HTTP route.
type Observation struct {
	SchemaVersion   int               `json:"schema_version"`
	EventID         string            `json:"event_id"`
	Provider        Provider          `json:"provider"`
	HostID          string            `json:"host_id"`
	NativeSessionID string            `json:"native_session_id,omitempty"`
	EventType       string            `json:"event_type"`
	Source          ObservationSource `json:"source"`
	OccurredAt      *time.Time        `json:"occurred_at,omitempty"`
	ReceivedAt      time.Time         `json:"received_at"`
	Data            ObservationData   `json:"data"`
	Error           string            `json:"error,omitempty"`
}

// ObservationData is the allow-listed payload of an observation. Unknown
// provider fields and transcript paths are never placed in it.
type ObservationData struct {
	NativeEvent       string            `json:"native_event"`
	NativeEventID     string            `json:"native_event_id,omitempty"`
	Name              string            `json:"name,omitempty"`
	SourceKind        string            `json:"source_kind,omitempty"`
	CWD               string            `json:"cwd,omitempty"`
	Model             string            `json:"model,omitempty"`
	InitialRequest    *TextPreview      `json:"initial_request,omitempty"`
	Summary           *TextPreview      `json:"summary,omitempty"`
	AttentionType     AttentionType     `json:"attention_type,omitempty"`
	AttentionDetail   string            `json:"attention_detail,omitempty"`
	PermissionMode    string            `json:"permission_mode,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	PreviousLiveState LiveState         `json:"previous_live_state,omitempty"`
	LiveState         LiveState         `json:"live_state,omitempty"`
	FrontendBindings  []FrontendBinding `json:"frontend_bindings,omitempty"`
	ProcessID         int               `json:"process_id,omitempty"`
}

// AgentList is the response of GET /v1/agents and each watch frame.
type AgentList struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Agents        []Agent   `json:"agents"`
}

// AgentEvent is a transcript-free projection of one stored observation.
type AgentEvent struct {
	SchemaVersion     int               `json:"schema_version"`
	ID                string            `json:"id"`
	AgentID           string            `json:"agent_id,omitempty"`
	Provider          Provider          `json:"provider"`
	EventType         string            `json:"event_type"`
	Source            ObservationSource `json:"source"`
	OccurredAt        *time.Time        `json:"occurred_at,omitempty"`
	ReceivedAt        time.Time         `json:"received_at"`
	ProcessedAt       *time.Time        `json:"processed_at,omitempty"`
	NativeEvent       string            `json:"native_event,omitempty"`
	PreviousLiveState LiveState         `json:"previous_live_state,omitempty"`
	LiveState         LiveState         `json:"live_state,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	Error             string            `json:"error,omitempty"`
}

// AgentEventList is the response of the event routes.
type AgentEventList struct {
	SchemaVersion int          `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	AgentID       string       `json:"agent_id,omitempty"`
	Provider      Provider     `json:"provider,omitempty"`
	Events        []AgentEvent `json:"events"`
}

// ProviderHealth is one provider's startup and latest discovery status.
type ProviderHealth struct {
	Provider          Provider   `json:"provider"`
	Status            string     `json:"status"`
	Version           string     `json:"version,omitempty"`
	DiscoverySource   string     `json:"discovery_source,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	ObservedAt        time.Time  `json:"observed_at"`
	StartupStatus     string     `json:"startup_status,omitempty"`
	StartupDetail     string     `json:"startup_detail,omitempty"`
	StartupObservedAt *time.Time `json:"startup_observed_at,omitempty"`
}

// Health is the response of GET /v1/health.
type Health struct {
	SchemaVersion int              `json:"schema_version"`
	Status        string           `json:"status"`
	Version       string           `json:"version"`
	Commit        string           `json:"commit,omitempty"`
	HostID        string           `json:"host_id"`
	StartedAt     time.Time        `json:"started_at"`
	Providers     []ProviderHealth `json:"providers"`
}

// Error is every JSON error body.
type Error struct {
	SchemaVersion int    `json:"schema_version"`
	Code          string `json:"code"`
	Message       string `json:"message"`
}

// HooksSeeItsTerminal reports whether hook-captured frontend bindings describe
// the terminal the user engages this agent in. Background Claude sessions run
// their hooks in Claude's daemon, so their bindings describe the daemon.
func HooksSeeItsTerminal(agent Agent) bool {
	return !(agent.Provider == ProviderClaude && agent.SourceKind == "background")
}

// NeverEngaged reports whether nobody ever spoke to or named the agent.
func NeverEngaged(agent Agent) bool {
	if strings.TrimSpace(agent.UserName) != "" || strings.TrimSpace(agent.Name) != "" {
		return false
	}
	return agent.InitialRequest == nil || strings.TrimSpace(agent.InitialRequest.Text) == ""
}

// Bucket is the shared grouping of agents by what the user is needed for.
type Bucket string

const (
	// BucketBlocked: asked for approval/input/auth, or failed.
	BucketBlocked Bucket = "blocked"
	// BucketWorking: mid-turn.
	BucketWorking Bucket = "working"
	// BucketReview: has a process, finished its turn.
	BucketReview Bucket = "review"
	// BucketQuiet: no process.
	BucketQuiet Bucket = "quiet"
)

// Buckets lists every bucket in display order, most urgent first.
var Buckets = []Bucket{BucketBlocked, BucketWorking, BucketReview, BucketQuiet}

// HasProcess reports whether the agent currently has a live provider process.
func HasProcess(agent Agent) bool {
	if agent.LiveState != LiveStateLive {
		return false
	}
	switch agent.RuntimeState {
	case RuntimeStateRunning, RuntimeStateLoaded, RuntimeStateStarting:
		return true
	}
	return false
}

// DeriveBucket reads the agent's stored display state and process presence.
func DeriveBucket(agent Agent) Bucket {
	if !HasProcess(agent) {
		return BucketQuiet
	}
	switch agent.DisplayState {
	case DisplayStateNeedsApproval, DisplayStateNeedsInput, DisplayStateNeedsAuthentication, DisplayStateError:
		return BucketBlocked
	case DisplayStateWorking, DisplayStateWaiting:
		return BucketWorking
	case DisplayStateReady:
		return BucketReview
	}
	return BucketQuiet
}

// Rank returns the bucket's index in Buckets, or len(Buckets) for any other value.
func (b Bucket) Rank() int {
	for i, candidate := range Buckets {
		if candidate == b {
			return i
		}
	}
	return len(Buckets)
}

// DeriveDisplayState computes the single status shown for an agent.
func DeriveDisplayState(agent Agent) DisplayState {
	switch agent.Attention.Type {
	case AttentionApprovalRequired:
		return DisplayStateNeedsApproval
	case AttentionInputRequired:
		return DisplayStateNeedsInput
	case AttentionAuthenticationRequired:
		return DisplayStateNeedsAuthentication
	case AttentionError:
		return DisplayStateError
	}
	switch agent.ActivityState {
	case ActivityStateWorking:
		return DisplayStateWorking
	case ActivityStateWaiting:
		return DisplayStateWaiting
	case ActivityStateIdle:
		if agent.LiveState == LiveStateLive &&
			(agent.RuntimeState == RuntimeStateRunning || agent.RuntimeState == RuntimeStateLoaded) {
			return DisplayStateReady
		}
	}
	switch agent.LiveState {
	case LiveStateLive:
		return DisplayStateIdle
	case LiveStateCandidateStale:
		return DisplayStateStale
	}
	return DisplayStateNotLive
}
