// Package registry defines the agent registry contract and an in-memory
// implementation used by tests.
package registry

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	apiv1 "noc.local/noc/pkg/api/v1"
)

// ErrNotFound is returned when no row matches.
var ErrNotFound = errors.New("agent not found")

const (
	// DefaultEventLimit is the number of events returned when none is requested.
	DefaultEventLimit = 50
	// MaxEventLimit caps an event query.
	MaxEventLimit = 200
)

// ListOptions filters List.
type ListOptions struct {
	IncludeNotLive bool
}

// EventListOptions filters ListEvents.
type EventListOptions struct {
	AgentID  string
	Provider apiv1.Provider
	Limit    int
}

// Reader reads agents.
type Reader interface {
	List(ctx context.Context, opts ListOptions) ([]apiv1.Agent, error)
	Get(ctx context.Context, id string) (apiv1.Agent, error)
}

// Namer sets the user-assigned label.
type Namer interface {
	SetUserName(ctx context.Context, id, name string) (apiv1.Agent, error)
}

// Forgetter deletes a registry row.
type Forgetter interface {
	Forget(ctx context.Context, id string) error
}

// Pruner removes retired rows.
type Pruner interface {
	PruneRetired(ctx context.Context, endedBefore time.Time) (int, error)
}

// EventReader lists journaled observations as events.
type EventReader interface {
	ListEvents(ctx context.Context, opts EventListOptions) ([]apiv1.AgentEvent, error)
}

// Store is the writer contract used by the hook processor and the reconciler.
type Store interface {
	Reader
	GetByNativeIdentity(ctx context.Context, hostID string, provider apiv1.Provider, nativeSessionID string) (apiv1.Agent, error)
	Upsert(ctx context.Context, agent apiv1.Agent) error
}

// ObservationStore is the hook outbox and observation journal.
type ObservationStore interface {
	// EnqueueObservation spools an observation; false means it was a duplicate.
	EnqueueObservation(ctx context.Context, obs apiv1.Observation) (bool, error)
	// PendingObservations returns unprocessed, non-quarantined observations in order.
	PendingObservations(ctx context.Context, limit int) ([]apiv1.Observation, error)
	MarkObservationProcessed(ctx context.Context, id string, processedAt time.Time) error
	MarkObservationFailed(ctx context.Context, id string, err error) error
	// RecordObservation appends to the observation journal (idempotent by event ID).
	RecordObservation(ctx context.Context, obs apiv1.Observation, processedAt time.Time) error
}

// ProviderRenamed reports whether an incoming provider name is a rename: a
// non-empty name that differs from the previously stored provider name. The
// memory registry and the SQLite store keep the two in step.
func ProviderRenamed(previous, current string) bool {
	return current != "" && current != previous
}

// Memory is an in-memory registry used by tests.
type Memory struct {
	mu     sync.RWMutex
	agents map[string]apiv1.Agent
}

// NewMemory returns an empty in-memory registry.
func NewMemory() *Memory {
	return &Memory{agents: map[string]apiv1.Agent{}}
}

func (m *Memory) List(_ context.Context, opts ListOptions) ([]apiv1.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]apiv1.Agent, 0, len(m.agents))
	for _, agent := range m.agents {
		if !opts.IncludeNotLive && agent.LiveState == apiv1.LiveStateNotLive {
			continue
		}
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayState != out[j].DisplayState {
			return out[i].DisplayState < out[j].DisplayState
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Memory) Get(_ context.Context, id string) (apiv1.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[id]
	if !ok {
		return apiv1.Agent{}, ErrNotFound
	}
	return agent, nil
}

func (m *Memory) GetByNativeIdentity(_ context.Context, hostID string, provider apiv1.Provider, nativeSessionID string) (apiv1.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, agent := range m.agents {
		if agent.HostID == hostID && agent.Provider == provider && agent.NativeSessionID == nativeSessionID {
			return agent, nil
		}
	}
	return apiv1.Agent{}, ErrNotFound
}

// Upsert replaces the stored agent, recomputing derived fields. The user name
// is an overlay: it is kept from the existing row unless the provider renamed
// the session.
func (m *Memory) Upsert(_ context.Context, agent apiv1.Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent.SchemaVersion = apiv1.SchemaVersion
	agent.DisplayState = apiv1.DeriveDisplayState(agent)
	agent.Bucket = apiv1.DeriveBucket(agent)
	agent.UserName = ""
	if existing, ok := m.agents[agent.ID]; ok {
		if !ProviderRenamed(existing.Name, agent.Name) {
			agent.UserName = existing.UserName
		}
	}
	m.agents[agent.ID] = agent
	return nil
}

func (m *Memory) SetUserName(_ context.Context, id, name string) (apiv1.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[id]
	if !ok {
		return apiv1.Agent{}, ErrNotFound
	}
	agent.UserName = name
	m.agents[id] = agent
	return agent, nil
}

func (m *Memory) Forget(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[id]; !ok {
		return ErrNotFound
	}
	delete(m.agents, id)
	return nil
}

// PruneRetired deletes every retired row and returns how many were deleted.
func (m *Memory) PruneRetired(_ context.Context, endedBefore time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pruned := 0
	for id, agent := range m.agents {
		if retired(agent, endedBefore) {
			delete(m.agents, id)
			pruned++
		}
	}
	return pruned, nil
}

// retired mirrors the SQLite prune predicate: ended, unnamed by the user, and
// either never engaged or last seen before the cutoff. Age is measured from
// the last sighting, not from retirement.
func retired(agent apiv1.Agent, endedBefore time.Time) bool {
	if agent.LiveState != apiv1.LiveStateNotLive {
		return false
	}
	if strings.TrimSpace(agent.UserName) != "" {
		return false
	}
	if apiv1.NeverEngaged(agent) {
		return true
	}
	return agent.LastSeenAt.Before(endedBefore)
}

var (
	_ Store     = (*Memory)(nil)
	_ Namer     = (*Memory)(nil)
	_ Forgetter = (*Memory)(nil)
	_ Pruner    = (*Memory)(nil)
)
