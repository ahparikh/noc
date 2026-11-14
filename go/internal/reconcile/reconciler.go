// Package reconcile turns provider snapshots into registry rows and liveness
// transitions.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"noc.local/noc/internal/identity"
	"noc.local/noc/internal/proclineage"
	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/registry"
	"noc.local/noc/internal/workspace"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const defaultStaleAfter = 30 * time.Second

const (
	eventAgentDiscovered        = "agent.discovered"
	eventAgentSnapshotConfirmed = "agent.snapshot_confirmed"
	eventAgentRevived           = "agent.revived"
	eventAgentCandidateStale    = "agent.candidate_stale"
	eventAgentNotLive           = "agent.not_live"
)

const (
	reasonObserved       = "observed_in_provider_snapshot"
	reasonConfirmed      = "confirmed_by_provider_snapshot"
	reasonReappeared     = "reappeared_in_provider_snapshot"
	reasonMissing        = "missing_from_provider_snapshot"
	reasonStaleGrace     = "missing_from_snapshot_past_stale_grace"
	reasonProcessExited  = "hook_session_process_exited"
	originReconcile      = "reconcile"
	defaultSnapshotModel = "provider_snapshot"
)

// Config configures a Reconciler.
type Config struct {
	HostID             string
	Store              registry.Store
	Adapter            provider.Adapter
	StaleAfter         time.Duration
	Now                func() time.Time
	NewID              func() (string, error)
	WorkspaceInspector *workspace.Inspector
	// RecordObservation journals semantic transitions; nil disables events.
	RecordObservation func(ctx context.Context, obs apiv1.Observation, processedAt time.Time) error
	// IsRunning reports whether a process exists; defaults to proclineage.IsRunning.
	IsRunning func(pid int) bool
}

// Report counts one reconciliation pass.
type Report struct {
	Observed, Upserted, Skipped, CandidateStale, MarkedNotLive int
}

// Reconciler reconciles one provider's snapshots into the registry.
type Reconciler struct {
	hostID     string
	store      registry.Store
	adapter    provider.Adapter
	staleAfter time.Duration
	now        func() time.Time
	newID      func() (string, error)
	inspector  *workspace.Inspector
	record     func(ctx context.Context, obs apiv1.Observation, processedAt time.Time) error
	isRunning  func(pid int) bool
}

// New validates cfg and applies defaults.
func New(cfg Config) (*Reconciler, error) {
	if cfg.HostID == "" {
		return nil, errors.New("reconciler host ID is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("reconciler store is required")
	}
	if cfg.Adapter == nil {
		return nil, errors.New("reconciler adapter is required")
	}
	r := &Reconciler{
		hostID:     cfg.HostID,
		store:      cfg.Store,
		adapter:    cfg.Adapter,
		staleAfter: cfg.StaleAfter,
		now:        cfg.Now,
		newID:      cfg.NewID,
		inspector:  cfg.WorkspaceInspector,
		record:     cfg.RecordObservation,
		isRunning:  cfg.IsRunning,
	}
	if r.staleAfter <= 0 {
		r.staleAfter = defaultStaleAfter
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.newID == nil {
		r.newID = identity.NewUUID
	}
	if r.inspector == nil {
		r.inspector = workspace.NewInspector()
	}
	if r.isRunning == nil {
		r.isRunning = proclineage.IsRunning
	}
	return r, nil
}

// RunOnce performs one snapshot pass followed by the aging pass.
func (r *Reconciler) RunOnce(ctx context.Context) (Report, error) {
	providerID := r.adapter.Provider()
	native, err := r.adapter.DiscoverLive(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("discover %s agents: %w", providerID, err)
	}
	now := r.now().UTC()
	report := Report{Observed: len(native)}
	seen := map[string]struct{}{}

	for _, entry := range native {
		normalized, err := r.adapter.Normalize(entry)
		if err != nil || normalized.NativeSessionID == "" || normalized.Provider != providerID {
			report.Skipped++
			continue
		}
		if _, dup := seen[normalized.NativeSessionID]; dup {
			report.Skipped++
			continue
		}
		existing, found, err := r.lookup(ctx, normalized)
		if err != nil {
			return report, err
		}
		if normalized.Workspace != nil && normalized.Workspace.Path != "" {
			owner := normalized.Workspace.Owner
			if owner == "" {
				owner = "unknown"
			}
			normalized.Workspace = r.inspector.Inspect(ctx, normalized.Workspace.Path, owner, now)
		}
		agent, err := r.materialize(normalized, existing, found, now)
		if err != nil {
			return report, err
		}
		eventType, reason := presenceTransition(existing, found)
		if err := r.store.Upsert(ctx, agent); err != nil {
			return report, fmt.Errorf("upsert %s agent: %w", providerID, err)
		}
		if eventType != "" {
			if err := r.recordTransition(ctx, agent, eventType, reason, existing.LiveState, now); err != nil {
				return report, err
			}
		}
		seen[normalized.NativeSessionID] = struct{}{}
		report.Upserted++
	}

	rows, err := r.store.List(ctx, registry.ListOptions{IncludeNotLive: true})
	if err != nil {
		return report, fmt.Errorf("list agents: %w", err)
	}
	for _, row := range rows {
		if row.HostID != r.hostID || row.Provider != providerID {
			continue
		}
		if hookOnly(row) {
			// Negative-only process signal; rows already not_live stay untouched.
			if row.LiveState == apiv1.LiveStateNotLive || row.ProcessID <= 0 || r.isRunning(row.ProcessID) {
				continue
			}
			previous := row.LiveState
			row.LiveState = apiv1.LiveStateNotLive
			row.RuntimeState = apiv1.RuntimeStateStopped
			row.EndedAt = &now
			if err := r.store.Upsert(ctx, row); err != nil {
				return report, fmt.Errorf("upsert %s agent: %w", providerID, err)
			}
			if err := r.recordTransition(ctx, row, eventAgentNotLive, reasonProcessExited, previous, now); err != nil {
				return report, err
			}
			report.MarkedNotLive++
			continue
		}
		if _, ok := seen[row.NativeSessionID]; ok {
			continue
		}
		switch row.LiveState {
		case apiv1.LiveStateNotLive:
			continue
		case apiv1.LiveStateCandidateStale:
			if now.Sub(row.LastSeenAt) < r.staleAfter {
				continue
			}
			row.LiveState = apiv1.LiveStateNotLive
			row.EndedAt = &now
			if err := r.store.Upsert(ctx, row); err != nil {
				return report, fmt.Errorf("upsert %s agent: %w", providerID, err)
			}
			if err := r.recordTransition(ctx, row, eventAgentNotLive, reasonStaleGrace, apiv1.LiveStateCandidateStale, now); err != nil {
				return report, err
			}
			report.MarkedNotLive++
		default:
			previous := row.LiveState
			row.LiveState = apiv1.LiveStateCandidateStale
			if err := r.store.Upsert(ctx, row); err != nil {
				return report, fmt.Errorf("upsert %s agent: %w", providerID, err)
			}
			if err := r.recordTransition(ctx, row, eventAgentCandidateStale, reasonMissing, previous, now); err != nil {
				return report, err
			}
			report.CandidateStale++
		}
	}
	return report, nil
}

func (r *Reconciler) lookup(ctx context.Context, n provider.NormalizedAgent) (apiv1.Agent, bool, error) {
	existing, err := r.store.GetByNativeIdentity(ctx, r.hostID, n.Provider, n.NativeSessionID)
	if errors.Is(err, registry.ErrNotFound) {
		return apiv1.Agent{}, false, nil
	}
	if err != nil {
		return apiv1.Agent{}, false, fmt.Errorf("look up %s agent: %w", n.Provider, err)
	}
	return existing, true, nil
}

// hookOnly reports rows no snapshot has ever confirmed and the reconciler did not create.
func hookOnly(a apiv1.Agent) bool {
	return a.SnapshotConfirmedAt == nil && a.Origin != originReconcile
}

func presenceTransition(existing apiv1.Agent, found bool) (eventType, reason string) {
	if !found {
		return eventAgentDiscovered, reasonObserved
	}
	if existing.LiveState == apiv1.LiveStateLive {
		if existing.SnapshotConfirmedAt == nil {
			return eventAgentSnapshotConfirmed, reasonConfirmed
		}
		return "", ""
	}
	return eventAgentRevived, reasonReappeared
}

func (r *Reconciler) recordTransition(ctx context.Context, agent apiv1.Agent, eventType, reason string, previous apiv1.LiveState, eventAt time.Time) error {
	if r.record == nil {
		return nil
	}
	eventAt = eventAt.UTC()
	sum := sha256.Sum256([]byte(strings.Join([]string{
		agent.HostID, string(agent.Provider), agent.NativeSessionID,
		eventType, string(previous), eventAt.Format(time.RFC3339Nano),
	}, "\x00")))
	occurred := eventAt
	obs := apiv1.Observation{
		SchemaVersion:   apiv1.SchemaVersion,
		EventID:         "snapshot-" + hex.EncodeToString(sum[:]),
		Provider:        agent.Provider,
		HostID:          agent.HostID,
		NativeSessionID: agent.NativeSessionID,
		EventType:       eventType,
		Source:          apiv1.ObservationSourceSnapshot,
		OccurredAt:      &occurred,
		ReceivedAt:      eventAt,
		Data: apiv1.ObservationData{
			Reason:            reason,
			PreviousLiveState: previous,
			LiveState:         agent.LiveState,
		},
	}
	if err := r.record(ctx, obs, eventAt); err != nil {
		return fmt.Errorf("record %s transition: %w", eventType, err)
	}
	return nil
}

func (r *Reconciler) materialize(n provider.NormalizedAgent, existing apiv1.Agent, found bool, now time.Time) (apiv1.Agent, error) {
	id := existing.ID
	firstSeen := existing.FirstSeenAt
	if !found {
		var err error
		if id, err = r.newID(); err != nil {
			return apiv1.Agent{}, fmt.Errorf("allocate noc agent ID: %w", err)
		}
		firstSeen = now
	}
	a := apiv1.Agent{
		SchemaVersion:       apiv1.SchemaVersion,
		ID:                  id,
		HostID:              r.hostID,
		Provider:            n.Provider,
		NativeSessionID:     n.NativeSessionID,
		NativeShortID:       firstNonEmpty(n.NativeShortID, existing.NativeShortID),
		SourceKind:          firstNonEmpty(n.SourceKind, existing.SourceKind),
		Origin:              firstNonEmpty(existing.Origin, originReconcile),
		LiveState:           apiv1.LiveStateLive,
		RuntimeState:        n.RuntimeState,
		ActivityState:       n.ActivityState,
		FirstSeenAt:         firstSeen,
		LastSeenAt:          now,
		SnapshotConfirmedAt: &now,
		NativeState:         n.NativeState,
	}

	switch {
	case n.Name != "":
		a.Name = n.Name
	case found && !isIDPlaceholderName(existing):
		a.Name = existing.Name
	}

	a.Model, a.ModelSource, a.ModelObservedAt = existing.Model, existing.ModelSource, existing.ModelObservedAt
	if n.Model != "" {
		t := now
		a.Model, a.ModelSource, a.ModelObservedAt = n.Model, firstNonEmpty(n.ModelSource, defaultSnapshotModel), &t
	}
	a.ReasoningEffort, a.ReasoningSource, a.ReasoningObservedAt = existing.ReasoningEffort, existing.ReasoningSource, existing.ReasoningObservedAt
	if n.ReasoningEffort != "" {
		t := now
		a.ReasoningEffort, a.ReasoningSource, a.ReasoningObservedAt = n.ReasoningEffort, firstNonEmpty(n.ReasoningSource, defaultSnapshotModel), &t
	}

	if n.InitialRequest != nil {
		p := *n.InitialRequest
		if p.Source == "" {
			p.Source = defaultSnapshotModel
		}
		if p.ObservedAt.IsZero() {
			p.ObservedAt = now
		}
		a.InitialRequest = &p
	} else if existing.InitialRequest != nil {
		p := *existing.InitialRequest
		a.InitialRequest = &p
	}

	if n.Summary != "" {
		t := now
		a.Summary, a.SummarySource, a.SummaryUpdatedAt, a.SummaryTruncated = n.Summary, defaultSnapshotModel, &t, false
	} else {
		a.Summary, a.SummarySource, a.SummaryUpdatedAt, a.SummaryTruncated = existing.Summary, existing.SummarySource, existing.SummaryUpdatedAt, existing.SummaryTruncated
	}

	a.CreatedAt = existing.CreatedAt
	if n.CreatedAt != nil {
		a.CreatedAt = n.CreatedAt
	}

	switch {
	case n.ActivityState == apiv1.ActivityStateWorking:
		t := now
		a.LastActivityAt = &t
	case n.LastActivityAt != nil:
		a.LastActivityAt = n.LastActivityAt
	default:
		a.LastActivityAt = existing.LastActivityAt
	}

	a.Capabilities = existing.Capabilities
	if n.Capabilities != nil {
		a.Capabilities = n.Capabilities
	}

	if n.Workspace != nil {
		ws := *n.Workspace
		ws.ObservedAt = now
		a.Workspace = &ws
	} else if existing.Workspace != nil {
		ws := *existing.Workspace
		a.Workspace = &ws
	}

	a.Attention = mergeAttention(n.Attention, existing, found, now)

	if apiv1.HooksSeeItsTerminal(a) && len(existing.FrontendBindings) > 0 {
		a.FrontendBindings = append([]apiv1.FrontendBinding(nil), existing.FrontendBindings...)
	}
	return a, nil
}

func mergeAttention(att apiv1.Attention, existing apiv1.Agent, found bool, now time.Time) apiv1.Attention {
	if att.Type == "" {
		att.Type = apiv1.AttentionNone
	}
	if att.Type != apiv1.AttentionNone {
		since := now
		if found && existing.Attention.Type == att.Type && existing.Attention.Detail == att.Detail && existing.Attention.Since != nil {
			since = *existing.Attention.Since
		}
		att.Since = &since
	}
	return att
}

// isIDPlaceholderName reports a stored name that merely repeats a native ID.
func isIDPlaceholderName(a apiv1.Agent) bool {
	name := strings.TrimSpace(a.Name)
	return name != "" && (name == a.NativeShortID || name == a.NativeSessionID)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
