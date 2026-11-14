// Package provider defines the adapter contract every provider implements and
// the bounded command runner adapters share.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// ErrNotImplemented is returned by adapters for capabilities they lack.
var ErrNotImplemented = errors.New("provider adapter not implemented")

// ProbeResult reports whether a provider's CLI can serve discovery.
type ProbeResult struct {
	Provider   apiv1.Provider
	Available  bool
	Version    string
	Features   map[string]bool
	Diagnostic string
	ObservedAt time.Time
}

// NativeAgent is the adapter-internal, already-sanitized row returned by
// DiscoverLive. Raw is an adapter-defined, allow-listed evidence object and
// never the provider payload.
type NativeAgent struct {
	NativeSessionID string
	NativeShortID   string
	Name            string
	SourceKind      string
	CWD             string
	Raw             json.RawMessage
}

// NormalizedAgent is provider evidence with provider-specific enums removed.
// It carries no noc agent ID or host ID; those belong to reconciliation.
type NormalizedAgent struct {
	Provider        apiv1.Provider
	NativeSessionID string
	NativeShortID   string
	Name            string
	SourceKind      string
	LiveState       apiv1.LiveState
	RuntimeState    apiv1.RuntimeState
	ActivityState   apiv1.ActivityState
	Attention       apiv1.Attention
	Model           string
	ModelSource     string
	ReasoningEffort string
	ReasoningSource string
	InitialRequest  *apiv1.TextPreview
	Summary         string
	CreatedAt       *time.Time
	LastActivityAt  *time.Time
	Capabilities    map[string]apiv1.Capability
	NativeState     json.RawMessage
	Workspace       *apiv1.Workspace
}

// Observation is one streamed provider fact (optional capability, unused).
type Observation struct {
	Provider        apiv1.Provider
	NativeSessionID string
	EventType       string
	OccurredAt      time.Time
	Agent           NormalizedAgent
}

// Adapter is implemented by every provider.
type Adapter interface {
	Provider() apiv1.Provider
	// Probe never returns an error; failures are encoded in the result.
	Probe(ctx context.Context) ProbeResult
	// DiscoverLive returns one snapshot of live, local, top-level conversations.
	DiscoverLive(ctx context.Context) ([]NativeAgent, error)
	// Normalize maps one native row; it performs no I/O other than reading a clock.
	Normalize(agent NativeAgent) (NormalizedAgent, error)
}

// Renamer is an optional adapter capability: pushing a name to the provider.
type Renamer interface {
	SetName(ctx context.Context, nativeSessionID, name string) error
}

// ObservationSink receives streamed observations.
type ObservationSink interface {
	Observe(ctx context.Context, obs Observation) error
}

// Watcher is an optional streaming capability. No adapter implements it today;
// discovery is snapshot-only.
type Watcher interface {
	Watch(ctx context.Context, sink ObservationSink) error
}

// CommandRunner runs a provider query command and returns its stdout.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}
