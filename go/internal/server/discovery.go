package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/reconcile"
	"noc.local/noc/internal/registry"
)

// discoveryWorker pairs an adapter with its reconciler.
type discoveryWorker struct {
	adapter    provider.Adapter
	reconciler *reconcile.Reconciler
}

// reconcileOnce runs one pass and publishes the outcome to health and logs.
func reconcileOnce(ctx context.Context, w discoveryWorker, health *providerHealthTracker, logger *slog.Logger) {
	name := w.adapter.Provider()
	report, err := w.reconciler.RunOnce(ctx)
	observedAt := time.Now().UTC()
	if err != nil {
		health.update(name, statusDegraded, err.Error(), observedAt)
		logger.Warn("provider discovery failed", "provider", name, "error", err)
		return
	}
	detail := fmt.Sprintf("discovery active; observed %d live sessions", report.Observed)
	if report.Skipped > 0 {
		detail += fmt.Sprintf("; skipped %d incompatible sessions", report.Skipped)
	}
	health.update(name, statusAvailable, detail, observedAt)
	if report.CandidateStale > 0 || report.MarkedNotLive > 0 {
		logger.Info("provider registry reconciled", "provider", name, "observed", report.Observed,
			"candidate_stale", report.CandidateStale, "marked_not_live", report.MarkedNotLive)
	}
}

// runDiscoveryLoop reconciles on every tick; it does not run immediately.
func runDiscoveryLoop(ctx context.Context, w discoveryWorker, interval time.Duration, health *providerHealthTracker, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce(ctx, w, health, logger)
		}
	}
}

// runRetentionLoop prunes retired rows immediately and then on every tick.
func runRetentionLoop(ctx context.Context, pruner registry.Pruner, retireAfter, interval time.Duration, logger *slog.Logger) {
	sweep := func() {
		pruned, err := pruner.PruneRetired(ctx, time.Now().UTC().Add(-retireAfter))
		if err != nil {
			logger.Warn("registry retention sweep failed", "error", err)
			return
		}
		if pruned > 0 {
			logger.Info("pruned retired agents", "count", pruned, "retire_after", retireAfter)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
