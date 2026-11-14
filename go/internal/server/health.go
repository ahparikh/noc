package server

import (
	"sync"
	"time"

	"noc.local/noc/internal/provider"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	disabledDiagnostic       = "provider discovery disabled"
	statusAvailable          = "available"
	statusUnavailable        = "unavailable"
	statusDegraded           = "degraded"
	statusDisabled           = "disabled"
	statusNotImplemented     = "not_implemented"
	featureDiscoverLive      = "discover_live"
	defaultDiscoverySource   = "provider adapter"
	claudeDiscoverySource    = "claude agents --json"
	codexDiscoverySource     = "Codex app-server loaded-thread snapshot"
	notImplementedDiagnostic = "provider adapter not implemented"
)

// providerHealthTracker holds per-provider health. The startup fields are
// recorded once and never change afterwards.
type providerHealthTracker struct {
	mu        sync.RWMutex
	order     []apiv1.Provider
	providers map[apiv1.Provider]*trackedProvider
}

type trackedProvider struct {
	health          apiv1.ProviderHealth
	startupRecorded bool
}

func newProviderHealthTracker(probes []provider.ProbeResult) *providerHealthTracker {
	t := &providerHealthTracker{providers: map[apiv1.Provider]*trackedProvider{}}
	for _, probe := range probes {
		status := initialStatus(probe)
		tp := &trackedProvider{health: apiv1.ProviderHealth{
			Provider:        probe.Provider,
			Status:          status,
			Version:         probe.Version,
			DiscoverySource: providerDiscoverySource(probe.Provider),
			Detail:          probe.Diagnostic,
			ObservedAt:      probe.ObservedAt.UTC(),
		}}
		if !(probe.Available && probe.Features[featureDiscoverLive]) {
			tp.recordStartup(status, probe.Diagnostic, probe.ObservedAt)
		}
		if _, exists := t.providers[probe.Provider]; !exists {
			t.order = append(t.order, probe.Provider)
		}
		t.providers[probe.Provider] = tp
	}
	return t
}

func (tp *trackedProvider) recordStartup(status, detail string, observedAt time.Time) {
	at := observedAt.UTC()
	tp.health.StartupStatus = status
	tp.health.StartupDetail = detail
	tp.health.StartupObservedAt = &at
	tp.startupRecorded = true
}

func initialStatus(probe provider.ProbeResult) string {
	switch {
	case probe.Diagnostic == disabledDiagnostic:
		return statusDisabled
	case probe.Diagnostic == notImplementedDiagnostic:
		return statusNotImplemented
	case probe.Available:
		return statusAvailable
	default:
		return statusUnavailable
	}
}

// update records a discovery result. Only status, detail, and observed_at change
// once the startup result has been recorded.
func (t *providerHealthTracker) update(p apiv1.Provider, status, detail string, observedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tp, ok := t.providers[p]
	if !ok {
		tp = &trackedProvider{health: apiv1.ProviderHealth{Provider: p, DiscoverySource: providerDiscoverySource(p)}}
		t.providers[p] = tp
	}
	if !tp.startupRecorded {
		tp.recordStartup(status, detail, observedAt)
	}
	tp.health.Status = status
	tp.health.Detail = detail
	tp.health.ObservedAt = observedAt.UTC()
}

// snapshot returns value copies in construction order.
func (t *providerHealthTracker) snapshot() []apiv1.ProviderHealth {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]apiv1.ProviderHealth, 0, len(t.order))
	for _, p := range t.order {
		h := t.providers[p].health
		if h.StartupObservedAt != nil {
			at := *h.StartupObservedAt
			h.StartupObservedAt = &at
		}
		out = append(out, h)
	}
	return out
}

func providerDiscoverySource(p apiv1.Provider) string {
	switch p {
	case apiv1.ProviderClaude:
		return claudeDiscoverySource
	case apiv1.ProviderCodex:
		return codexDiscoverySource
	default:
		return defaultDiscoverySource
	}
}
