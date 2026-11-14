// Package server composes storage, provider adapters, reconcilers, the hook
// drainer, retention, and the local API into the noc server process.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"noc.local/noc/internal/buildinfo"
	"noc.local/noc/internal/hooks"
	"noc.local/noc/internal/localapi"
	"noc.local/noc/internal/provider"
	"noc.local/noc/internal/provider/claude"
	"noc.local/noc/internal/provider/codex"
	"noc.local/noc/internal/reconcile"
	sqlitestore "noc.local/noc/internal/storage/sqlite"
	"noc.local/noc/internal/workspace"
	apiv1 "noc.local/noc/pkg/api/v1"
)

const (
	DefaultDiscoveryInterval = 5 * time.Second
	DefaultStaleAfter        = 30 * time.Second
	DefaultHookDrainInterval = 2 * time.Second
	DefaultRetireAfter       = 14 * 24 * time.Hour
	DefaultRetentionInterval = time.Hour

	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 30 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Config configures Run. Zero values select the defaults.
type Config struct {
	SocketPath        string
	DatabasePath      string
	Logger            *slog.Logger
	Ready             func()
	DiscoveryInterval time.Duration
	StaleAfter        time.Duration
	DisableDiscovery  bool
	// RetireAfter 0 selects the default; negative disables the retention loop.
	RetireAfter       time.Duration
	RetentionInterval time.Duration
	Adapters          []provider.Adapter
}

// Run serves the noc API until ctx is cancelled or the HTTP server fails.
func Run(ctx context.Context, cfg Config) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	store, err := sqlitestore.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("could not close SQLite database", "error", err)
		}
	}()

	hostID, err := store.HostID(ctx)
	if err != nil {
		return err
	}

	discoveryInterval := cfg.DiscoveryInterval
	if discoveryInterval <= 0 {
		discoveryInterval = DefaultDiscoveryInterval
	}
	staleAfter := cfg.StaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	retireAfter := cfg.RetireAfter
	if retireAfter == 0 {
		retireAfter = DefaultRetireAfter
	}
	retentionInterval := cfg.RetentionInterval
	if retentionInterval <= 0 {
		retentionInterval = DefaultRetentionInterval
	}

	startedAt := time.Now().UTC()

	adapters := cfg.Adapters
	if adapters == nil {
		adapters = []provider.Adapter{claude.New(), codex.New()}
	}

	probes := make([]provider.ProbeResult, 0, len(adapters))
	for _, adapter := range adapters {
		if cfg.DisableDiscovery {
			probes = append(probes, provider.ProbeResult{
				Provider:   adapter.Provider(),
				Available:  false,
				Diagnostic: disabledDiagnostic,
				ObservedAt: startedAt,
			})
			continue
		}
		probes = append(probes, adapter.Probe(ctx))
	}
	health := newProviderHealthTracker(probes)

	inspector := workspace.NewInspector()
	processor, err := hooks.NewProcessor(hooks.Config{HostID: hostID, Store: store, WorkspaceInspector: inspector})
	if err != nil {
		return err
	}

	var workers []discoveryWorker
	if !cfg.DisableDiscovery {
		for i, adapter := range adapters {
			probe := probes[i]
			if !probe.Available || !probe.Features[featureDiscoverLive] {
				continue
			}
			rec, err := reconcile.New(reconcile.Config{
				HostID:             hostID,
				Store:              store,
				Adapter:            adapter,
				StaleAfter:         staleAfter,
				WorkspaceInspector: inspector,
				RecordObservation:  store.RecordObservation,
			})
			if err != nil {
				return err
			}
			w := discoveryWorker{adapter: adapter, reconciler: rec}
			workers = append(workers, w)
			reconcileOnce(ctx, w, health, logger)
		}
	}

	listener, cleanup, err := localapi.Listen(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			logger.Warn("could not clean up server socket", "error", err)
		}
	}()

	healthFunc := func() apiv1.Health {
		return apiv1.Health{
			SchemaVersion: apiv1.SchemaVersion,
			Status:        "ok",
			Version:       buildinfo.Version,
			Commit:        buildinfo.Commit,
			HostID:        hostID,
			StartedAt:     startedAt,
			Providers:     health.snapshot(),
		}
	}
	hookWake := make(chan struct{}, 1)
	wakeHooks := func() {
		select {
		case hookWake <- struct{}{}:
		default:
		}
	}
	handler := localapi.NewHandler(store, healthFunc, wakeHooks)
	renamers := map[apiv1.Provider]provider.Renamer{}
	for _, adapter := range adapters {
		if renamer, ok := adapter.(provider.Renamer); ok {
			renamers[adapter.Provider()] = renamer
		}
	}
	handler.SetRenamers(renamers)

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- httpServer.Serve(listener) }()

	if cfg.Ready != nil {
		cfg.Ready()
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	var wg sync.WaitGroup
	start := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}

	start(func() { runHookDrainLoop(workerCtx, processor, hookWake, DefaultHookDrainInterval, logger) })
	wakeHooks()
	for _, w := range workers {
		w := w
		start(func() { runDiscoveryLoop(workerCtx, w, discoveryInterval, health, logger) })
	}
	if retireAfter > 0 {
		start(func() { runRetentionLoop(workerCtx, store, retireAfter, retentionInterval, logger) })
	}

	logger.Info("noc server listening", "socket", cfg.SocketPath, "database", cfg.DatabasePath,
		"host_id", hostID, "version", buildinfo.String())

	var result error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		result = httpServer.Shutdown(shutdownCtx)
		cancel()
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	cancelWorkers()
	wg.Wait()
	return result
}

func drainHooks(ctx context.Context, processor *hooks.Processor, logger *slog.Logger) {
	report, err := processor.Drain(ctx, hooks.DefaultDrainLimit)
	if err != nil {
		logger.Warn("hook observation drain failed", "pending", report.Pending,
			"processed", report.Processed, "failed", report.Failed, "error", err)
		return
	}
	if report.Processed > 0 {
		logger.Debug("hook observations processed", "count", report.Processed)
	}
}

func runHookDrainLoop(ctx context.Context, processor *hooks.Processor, wake <-chan struct{}, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			drainHooks(ctx, processor, logger)
		case <-ticker.C:
			drainHooks(ctx, processor, logger)
		}
	}
}
