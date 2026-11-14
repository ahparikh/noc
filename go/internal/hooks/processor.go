package hooks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"noc.local/noc/internal/identity"
	"noc.local/noc/internal/registry"
	"noc.local/noc/internal/workspace"
	apiv1 "noc.local/noc/pkg/api/v1"
)

// DefaultDrainLimit is the number of observations handled per drain.
const DefaultDrainLimit = 128

// Store is what the processor needs: the registry writer plus the outbox.
type Store interface {
	registry.Store
	registry.ObservationStore
}

// Config configures a Processor.
type Config struct {
	HostID             string
	Store              Store
	WorkspaceInspector *workspace.Inspector
	Now                func() time.Time
	NewID              func() (string, error)
}

// Processor drains the hook outbox into the agent registry.
type Processor struct {
	hostID    string
	store     Store
	inspector *workspace.Inspector
	now       func() time.Time
	newID     func() (string, error)
	mu        sync.Mutex
}

// DrainReport counts one drain pass.
type DrainReport struct {
	Pending, Processed, Failed int
}

// NewProcessor validates cfg and applies defaults.
func NewProcessor(cfg Config) (*Processor, error) {
	if cfg.HostID == "" {
		return nil, errors.New("hook processor host ID is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("hook processor store is required")
	}
	p := &Processor{
		hostID:    cfg.HostID,
		store:     cfg.Store,
		inspector: cfg.WorkspaceInspector,
		now:       cfg.Now,
		newID:     cfg.NewID,
	}
	if p.inspector == nil {
		p.inspector = workspace.NewInspector()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.newID == nil {
		p.newID = identity.NewUUID
	}
	return p, nil
}

// Drain processes up to limit pending observations. One failure does not stop
// the batch; all errors are joined.
func (p *Processor) Drain(ctx context.Context, limit int) (DrainReport, error) {
	if limit <= 0 {
		limit = DefaultDrainLimit
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var report DrainReport
	pending, err := p.store.PendingObservations(ctx, limit)
	if err != nil {
		return report, err
	}
	report.Pending = len(pending)
	var errs []error
	for _, obs := range pending {
		processedAt := p.now().UTC()
		if err := p.process(ctx, obs, processedAt); err != nil {
			if markErr := p.store.MarkObservationFailed(ctx, obs.EventID, err); markErr != nil {
				err = fmt.Errorf("%w; mark failed: %v", err, markErr)
			}
			errs = append(errs, err)
			report.Failed++
			continue
		}
		if err := p.store.MarkObservationProcessed(ctx, obs.EventID, processedAt); err != nil {
			errs = append(errs, err)
			report.Failed++
			continue
		}
		report.Processed++
	}
	return report, errors.Join(errs...)
}

func (p *Processor) process(ctx context.Context, obs apiv1.Observation, processedAt time.Time) error {
	if obs.SchemaVersion != apiv1.SchemaVersion {
		return fmt.Errorf("unsupported observation schema version %d", obs.SchemaVersion)
	}
	if obs.EventID == "" || obs.Provider == "" || obs.HostID != p.hostID {
		return errors.New("observation identity is invalid for this host")
	}
	if obs.EventType == EventHookRejected || obs.NativeSessionID == "" {
		return p.store.RecordObservation(ctx, obs, processedAt)
	}
	if obs.Provider != apiv1.ProviderClaude && obs.Provider != apiv1.ProviderCodex {
		if err := p.store.RecordObservation(ctx, obs, processedAt); err != nil {
			return err
		}
		return fmt.Errorf("unsupported observation provider %q", obs.Provider)
	}

	existing, err := p.store.GetByNativeIdentity(ctx, obs.HostID, obs.Provider, obs.NativeSessionID)
	existed := err == nil
	if err != nil && !errors.Is(err, registry.ErrNotFound) {
		return err
	}
	agent, err := p.project(ctx, obs, existing, existed)
	if err != nil {
		return err
	}
	if existed {
		obs.Data.PreviousLiveState = existing.LiveState
	}
	obs.Data.LiveState = agent.LiveState
	if err := p.store.RecordObservation(ctx, obs, processedAt); err != nil {
		return err
	}

	if agent.LiveState == apiv1.LiveStateNotLive && apiv1.NeverEngaged(agent) {
		if !existed {
			return nil
		}
		if forgetter, ok := p.store.(registry.Forgetter); ok {
			if err := forgetter.Forget(ctx, agent.ID); err != nil && !errors.Is(err, registry.ErrNotFound) {
				return err
			}
			return nil
		}
	}
	return p.store.Upsert(ctx, agent)
}

func (p *Processor) project(ctx context.Context, obs apiv1.Observation, existing apiv1.Agent, existed bool) (apiv1.Agent, error) {
	eventAt := obs.ReceivedAt.UTC()
	if obs.ReceivedAt.IsZero() {
		eventAt = p.now().UTC()
	}
	agent := existing
	if !existed {
		id, err := p.newID()
		if err != nil {
			return apiv1.Agent{}, fmt.Errorf("allocate noc agent ID: %w", err)
		}
		agent = apiv1.Agent{
			SchemaVersion:   apiv1.SchemaVersion,
			ID:              id,
			HostID:          obs.HostID,
			Provider:        obs.Provider,
			NativeSessionID: obs.NativeSessionID,
			Origin:          "hook",
			LiveState:       apiv1.LiveStateLive,
			RuntimeState:    apiv1.RuntimeStateUnknown,
			ActivityState:   apiv1.ActivityStateUnknown,
			Attention:       apiv1.Attention{Type: apiv1.AttentionNone},
			FirstSeenAt:     eventAt,
			LastSeenAt:      eventAt,
		}
	}
	current := !existed || !eventAt.Before(existing.LastSeenAt)
	data := obs.Data

	if data.Name != "" && (agent.Name == "" || current) {
		agent.Name = data.Name
	}
	if agent.SourceKind == "" && data.SourceKind != "" {
		agent.SourceKind = data.SourceKind
	}
	if data.Model != "" && (agent.ModelObservedAt == nil || !eventAt.Before(*agent.ModelObservedAt)) {
		at := eventAt
		agent.Model, agent.ModelSource, agent.ModelObservedAt = data.Model, "hook", &at
	}
	if agent.InitialRequest == nil && data.InitialRequest != nil {
		preview := *data.InitialRequest
		preview.ObservedAt = eventAt
		agent.InitialRequest = &preview
	}
	if data.Summary != nil && (agent.SummaryUpdatedAt == nil || !eventAt.Before(*agent.SummaryUpdatedAt)) {
		at := eventAt
		agent.Summary = data.Summary.Text
		agent.SummarySource = data.Summary.Source
		agent.SummaryTruncated = data.Summary.Truncated
		agent.SummaryUpdatedAt = &at
	}
	if data.CWD != "" && (agent.Workspace == nil || !eventAt.Before(agent.Workspace.ObservedAt)) {
		if ws := p.inspector.Inspect(ctx, data.CWD, "unknown", eventAt); ws != nil {
			agent.Workspace = ws
		}
	}
	agent.FrontendBindings = mergeBindings(agent.FrontendBindings, data.FrontendBindings, eventAt)
	if !apiv1.HooksSeeItsTerminal(agent) {
		agent.FrontendBindings = nil
	}
	if data.ProcessID > 0 && !eventAt.Before(agent.LastSeenAt) {
		agent.ProcessID = data.ProcessID
	}
	if eventAt.After(agent.LastSeenAt) {
		agent.LastSeenAt = eventAt
	}
	if current {
		applyState(&agent, obs, eventAt)
	}
	return agent, nil
}

func applyState(agent *apiv1.Agent, obs apiv1.Observation, eventAt time.Time) {
	at := eventAt
	if obs.EventType == EventSessionEnded {
		agent.LiveState = apiv1.LiveStateNotLive
		agent.RuntimeState = apiv1.RuntimeStateStopped
		agent.ActivityState = apiv1.ActivityStateIdle
		agent.Attention = apiv1.Attention{Type: apiv1.AttentionNone}
		agent.EndedAt = &at
		bindings := make([]apiv1.FrontendBinding, len(agent.FrontendBindings))
		copy(bindings, agent.FrontendBindings)
		for i := range bindings {
			bindings[i].State = "disconnected"
			bindings[i].ObservedAt = eventAt
		}
		if len(bindings) == 0 {
			bindings = nil
		}
		agent.FrontendBindings = bindings
		return
	}

	attention := obs.Data.AttentionType
	if attention == "" {
		attention = apiv1.AttentionNone
	}
	switch obs.EventType {
	case EventSessionStarted:
		agent.RuntimeState, agent.ActivityState = apiv1.RuntimeStateRunning, apiv1.ActivityStateIdle
	case EventTurnPrompted:
		agent.RuntimeState, agent.ActivityState = apiv1.RuntimeStateRunning, apiv1.ActivityStateWorking
		agent.LastActivityAt = &at
	case EventTurnStopped:
		agent.RuntimeState, agent.ActivityState = apiv1.RuntimeStateRunning, apiv1.ActivityStateIdle
		agent.LastActivityAt = &at
	case EventTurnFailed:
		agent.RuntimeState, agent.ActivityState = apiv1.RuntimeStateFailed, apiv1.ActivityStateIdle
		agent.LastActivityAt = &at
	case EventApprovalRequired:
		agent.RuntimeState, agent.ActivityState = apiv1.RuntimeStateRunning, apiv1.ActivityStateWaiting
	case EventNotification:
		if attention != apiv1.AttentionNone {
			agent.ActivityState = apiv1.ActivityStateWaiting
		}
	}
	agent.LiveState = apiv1.LiveStateLive
	agent.EndedAt = nil

	if attention == apiv1.AttentionNone {
		switch obs.EventType {
		case EventSessionStarted, EventTurnPrompted, EventTurnStopped:
			agent.Attention = apiv1.Attention{Type: apiv1.AttentionNone}
		}
		return
	}
	if agent.Attention.Type != attention || agent.Attention.Detail != obs.Data.AttentionDetail {
		agent.Attention = apiv1.Attention{Type: attention, Since: &at, Detail: obs.Data.AttentionDetail}
	}
}

func mergeBindings(existing, observed []apiv1.FrontendBinding, eventAt time.Time) []apiv1.FrontendBinding {
	if len(existing) == 0 && len(observed) == 0 {
		return existing
	}
	out := make([]apiv1.FrontendBinding, len(existing), len(existing)+len(observed))
	copy(out, existing)
	for _, binding := range observed {
		binding.ObservedAt = eventAt
		replaced := false
		for i := range out {
			if out[i].Kind == binding.Kind && out[i].Session == binding.Session && out[i].PaneID == binding.PaneID {
				if !binding.ObservedAt.Before(out[i].ObservedAt) {
					out[i] = binding
				}
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, binding)
		}
	}
	return out
}
