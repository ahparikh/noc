# 04 — Server, reconciliation, and storage

This section specifies the long-lived `noc server` composition (startup
ordering, workers, timers, shutdown, provider health), the snapshot
reconciler (natural-key identity, UUID allocation, field-by-field merge of
provider snapshots with existing and hook-derived data, the
`live → candidate_stale → not_live` liveness machine, the negative-only
process signal for hook-only rows, frontend-binding clearing, and the semantic
events it journals), the retention sweep, the SQLite store (driver, pragmas,
permissions, every table, index and migration, JSON storage, and the semantics
of list/get/upsert/rename/team/forget/prune), and read-only Git workspace
inspection. The HTTP handlers, hook normalization/projection, provider
adapters, and CLI lifecycle (PID file, `--bg`, `stop`, `restart`) are specified in
[02-api.md](02-api.md), [06-hooks.md](06-hooks.md), [05-providers.md](05-providers.md)
and [03-cli.md](03-cli.md), and appear here only where this code calls them.

Source files covered:

- `go/internal/server/run.go`, `go/internal/server/discovery.go`, `go/internal/server/health.go`
- `go/internal/server/run_test.go`, `go/internal/server/health_test.go`, `go/internal/server/retention_test.go`
- `go/internal/reconcile/reconciler.go`, `go/internal/reconcile/reconciler_test.go`, `go/internal/reconcile/reconciler_bindings_test.go`
- `go/internal/storage/sqlite/store.go`, `go/internal/storage/sqlite/migrations.go`, `go/internal/storage/sqlite/store_test.go`, `go/internal/storage/sqlite/teams_test.go`
- `go/internal/storage/sqlite/hooks.go` (schema and timestamp conventions of the `observations`/`hook_outbox` tables only)
- `go/internal/workspace/inspect.go`, `go/internal/workspace/inspect_test.go`

Neighbors read for context (not specified here): `go/internal/provider/provider.go`,
`go/internal/registry/registry.go`, `go/internal/identity/uuid.go`,
`go/internal/proclineage/proclineage.go`, `go/internal/localapi/socket.go`,
`go/internal/config/paths.go`, `go/pkg/api/v1/types.go`,
`go/internal/cli/run.go`, `go/cmd/noc/main.go`,
`go/internal/hooks/processor.go`.

---

## Constants

| Name | Value | Where | Overridable |
|---|---|---|---|
| `DefaultDiscoveryInterval` | `5 * time.Second` | `go/internal/server/run.go` | `Config.DiscoveryInterval`; CLI `--discovery-interval` |
| `DefaultStaleAfter` | `30 * time.Second` | `go/internal/server/run.go` | `Config.StaleAfter`; CLI `--stale-after` |
| `DefaultHookDrainInterval` | `2 * time.Second` | `go/internal/server/run.go` | No |
| `DefaultRetireAfter` | `14 * 24 * time.Hour` | `go/internal/server/run.go` | `Config.RetireAfter` only (no CLI flag) |
| `DefaultRetentionInterval` | `time.Hour` | `go/internal/server/run.go` | `Config.RetentionInterval` only (no CLI flag) |
| HTTP `ReadHeaderTimeout` | `5 * time.Second` | `go/internal/server/run.go` | No |
| HTTP `IdleTimeout` | `30 * time.Second` | `go/internal/server/run.go` | No |
| Graceful shutdown timeout | `5 * time.Second` | `go/internal/server/run.go` | No |
| `hooks.DefaultDrainLimit` | `128` observations per drain | `go/internal/hooks/processor.go` | No |
| reconciler `defaultStaleAfter` | `30 * time.Second` | `go/internal/reconcile/reconciler.go` | `reconcile.Config.StaleAfter` |
| SQLite `busy_timeout` | `5000` ms | `go/internal/storage/sqlite/store.go` | No |
| `MaxHookAttempts` | `5` | `go/internal/storage/sqlite/hooks.go` | No |
| outbox/observation timestamp layout | `2006-01-02T15:04:05.000000000Z07:00` | `go/internal/storage/sqlite/hooks.go` | No |
| workspace `inspectTimeout` | `750 * time.Millisecond` per Git command | `go/internal/workspace/inspect.go` | No |
| workspace `cacheTTL` | `15 * time.Second` | `go/internal/workspace/inspect.go` | No |
| socket stale-probe dial timeout | `200 * time.Millisecond` | `go/internal/localapi/socket.go` | No |

---

## Server composition (`SRV`)

### Startup sequence

```
 cli runServerForeground           server.Run                         store / adapters / localapi
 ----------------------           ----------                         ---------------------------
 acquire PID file  ─────────────▶ (1) sqlite.Open(DatabasePath) ───▶ mkdir 0700, open, ping,
                                                                      PRAGMAs, migrate, chmod 0600
                                  (2) store.HostID()  ──────────────▶ SELECT/INSERT metadata.host_id
                                  (3) normalise intervals
                                  (4) startedAt = now UTC
                                  (5) adapters = Config.Adapters or [claude, codex]
                                  (6) for each adapter (sequential):
                                        DisableDiscovery? synthetic probe
                                        else adapter.Probe(ctx) ────▶ `<bin> --version`, capability cmd
                                  (7) healthTracker(probes)
                                  (8) workspace.NewInspector()  (one shared instance)
                                  (9) hooks.NewProcessor(hostID, store, inspector)
                                 (10) for each eligible adapter (sequential, adapter order):
                                        reconcile.New(...)
                                        reconcileOnce()  ───────────▶ DiscoverLive, upserts, aging pass,
                                                                      events; health.update(startup)
                                 (11) localapi.Listen(SocketPath) ──▶ refuse live socket / non-socket,
                                                                      remove stale socket, listen, chmod 0600
                                 (12) build health closure, hookWake chan(1), handler, renamers, spawn support,
                                      http.Server{ReadHeaderTimeout 5s, IdleTimeout 30s}
                                 (13) go server.Serve(listener)
 readiness signal ◀───────────── (14) Config.Ready()
                                 (15) go hook-drain loop; wakeHooks()  (immediate outbox drain)
                                 (16) go discovery loop per worker (first tick after one interval)
                                 (17) go retention loop if retention > 0 (immediate sweep)
                                 (18) log INFO "noc server listening"
                                 (19) block until ctx.Done() or Serve error
```

#### SRV-001 Interval and retention defaults
- **Behavior:** `Run` MUST normalise configuration before any worker starts: `DiscoveryInterval <= 0` → `5s`; `StaleAfter <= 0` → `30s`; `RetireAfter == 0` → `14 * 24h`; `RetireAfter < 0` MUST disable the retention loop entirely; `RetentionInterval <= 0` → `1h`. The hook drain interval MUST be the constant `2s` regardless of configuration.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-023, D-028, D-041
- **Verified by:** UNTESTED

#### SRV-002 Production launch never overrides retention
- **Behavior:** `noc server` (foreground and `--bg` child) MUST call `server.Run` with only `SocketPath`, `DatabasePath`, `Logger`, `Ready`, `DiscoveryInterval`, `StaleAfter`, and `DisableDiscovery = !--discovery`; `RetireAfter`, `RetentionInterval`, and `Adapters` are left zero/nil, so the effective production retention is 14 days swept hourly and the adapters are the default pair. `--discovery-interval` and `--stale-after` MUST be positive when discovery is enabled (validation error `discovery interval must be positive` / `stale-after must be positive`, exit 2).
- **Source:** `go/internal/cli/run.go` (runServerForeground; serverLaunchFlags.validate)
- **Decision:** D-023, D-041
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (interval and stale-after validation); the fields passed to `server.Run` UNTESTED

#### SRV-003 Storage first; failure aborts
- **Behavior:** The first side effect of `Run` MUST be `sqlitestore.Open(ctx, DatabasePath)`. Any error MUST be returned from `Run` before probing, discovery, or socket creation. The store MUST be closed by a deferred call when `Run` returns; a close error is logged at WARN `could not close SQLite database` and does not change the return value.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-022
- **Verified by:** `go/internal/server/run_test.go` TestRunRefusesLiveSocketAndStorageFailureAborts (storage failure aborts)

#### SRV-004 Host identity established before anything uses it
- **Behavior:** Immediately after opening storage, `Run` MUST obtain `host_id` via `store.HostID(ctx)` (DB-012); an error MUST abort `Run`. The same `hostID` is passed to the hook processor, every reconciler, and the health payload.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-016
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### SRV-005 started_at
- **Behavior:** `started_at` MUST be captured once as `time.Now().UTC()` after storage and host identity are ready and before any probe runs; it is reported unchanged by every health response and is also the `ObservedAt` of synthetic "disabled" probes.
- **Source:** `go/internal/server/run.go` (Run)
- **Verified by:** MANUAL: run the isolated server (Verification procedure M2) and confirm `started_at` in `noc server status --json` is earlier than every provider `observed_at`.

#### SRV-006 Default adapter set and order
- **Behavior:** When `Config.Adapters == nil`, the adapter list MUST be `[claude.New(), codex.New()]` in that order. This order is the probe order, the startup-discovery order, and the order of `providers` in health.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-023, D-024
- **Verified by:** MANUAL: Verification procedure M2 — `providers[0].provider == "claude"`, `providers[1].provider == "codex"`.

#### SRV-007 Sequential probing; disabled discovery never probes
- **Behavior:** Probes MUST run sequentially in adapter order on the caller's goroutine. When `DisableDiscovery` is true, `adapter.Probe` MUST NOT be called; instead a synthetic `ProbeResult{Provider: adapter.Provider(), Available: false, Diagnostic: "provider discovery disabled", ObservedAt: startedAt}` is used. A slow probe therefore delays socket creation for all providers (each provider's probe has its own command timeouts, see [05-providers.md](05-providers.md)).
- **Source:** `go/internal/server/run.go` (Run)
- **Verified by:** `go/internal/server/run_test.go` TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers; MANUAL: Verification procedure M1 — both providers report `status: "disabled"` and `detail: "provider discovery disabled"`.

#### SRV-008 Shared workspace inspector and hook processor
- **Behavior:** Exactly one `workspace.Inspector` MUST be created and shared by the hook processor and all reconcilers (so the 15 s cache is shared). `hooks.NewProcessor` MUST be constructed with `HostID`, `Store`, and that inspector; a construction error aborts `Run`.
- **Source:** `go/internal/server/run.go` (Run)
- **Verified by:** UNTESTED

#### SRV-009 Discovery worker eligibility (never retried)
- **Behavior:** A discovery worker (reconciler + periodic loop) MUST be created only when discovery is enabled AND the adapter's startup probe has `Available == true` AND `Features["discover_live"] == true`. An adapter that fails its startup probe gets no worker for the lifetime of the process: it is never re-probed, its rows are never aged by snapshot absence, and its hook-only rows are never retired by the process signal (REC-024). Recovery requires a server restart. A `reconcile.New` error aborts `Run`.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunAvailableWithoutDiscoverLiveHasNoWorker (no `discover_live`, no worker); that a failed probe is never retried UNTESTED

#### SRV-010 Synchronous startup discovery before the listener
- **Behavior:** For each eligible adapter, in adapter order, `Run` MUST execute one `reconcileOnce` (SRV-021) synchronously before `localapi.Listen`. A failed startup reconciliation MUST NOT abort startup; it records `degraded` health. Consequently the first client to connect sees the startup snapshot already reconciled (including any aging of rows left from a previous run). The reconciler's `RecordObservation` MUST be `store.RecordObservation`.
- **Source:** `go/internal/server/run.go` (Run); `go/internal/server/discovery.go` (reconcileOnce)
- **Decision:** D-007, D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery

#### SRV-011 Listener creation and socket ownership
- **Behavior:** After startup discovery, `Run` MUST call `localapi.Listen(SocketPath)`, which: ensures the parent directory (created `0700` if missing, never chmodded if pre-existing); if the path exists and is not a socket returns `refusing to replace non-socket path %q`; if it is a socket that accepts a connection within 200 ms returns `localapi.ErrServerRunning` ("noc server is already running"); otherwise removes the stale socket; listens on `unix`; chmods the socket `0600`. Any error MUST abort `Run` (storage is then closed by its defer). The returned cleanup (remove socket, ignoring not-exist) MUST run in a defer when `Run` returns; a cleanup error is logged at WARN `could not clean up server socket`. Because startup discovery precedes this check, a second `Run` on the same database writes reconciliation results before failing with `ErrServerRunning` (the CLI's PID file normally prevents reaching this point).
- **Source:** `go/internal/server/run.go` (Run), `go/internal/localapi/socket.go` (Listen)
- **Decision:** D-019, D-026
- **Verified by:** `go/internal/server/run_test.go` TestRunRefusesLiveSocketAndStorageFailureAborts; `go/internal/localapi/server_test.go` TestListenSocketMode, TestListenRefusesNonSocketPath, TestListenReplacesStaleSocket, TestListenDetectsLiveServer, TestListenCreatesPrivateParent

#### SRV-012 HTTP server configuration
- **Behavior:** The HTTP server MUST use `localapi.NewHandler(store, health, wakeHooks)` with `ReadHeaderTimeout = 5s`, `IdleTimeout = 30s`, and no read/write timeout (watch streams are long-lived). `Serve(listener)` MUST run in its own goroutine, reporting its terminal error on a buffered channel of size 1.
- **Source:** `go/internal/server/run.go` (Run; readHeaderTimeout, idleTimeout)
- **Decision:** D-019
- **Verified by:** UNTESTED

#### SRV-013 Renamer registration
- **Behavior:** Every adapter that implements `provider.Renamer` MUST be registered with `handler.SetRenamers`, keyed by `adapter.Provider()`, independent of probe result or `DisableDiscovery`. (With discovery disabled, a rename of a Codex agent can still reach the Codex app-server.)
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-042
- **Verified by:** `go/internal/server/run_test.go` TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers; `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt (handler)

#### SRV-032 Spawn registration support
- **Behavior:** After the renamers, `Run` MUST call `handler.SetSpawnSupport(hostID, inspector)` with the host identity of SRV-004 and the shared workspace inspector of SRV-008, independent of probe results and of `DisableDiscovery`. This is what lets `POST /v1/agents` register a spawned agent ([02-api.md](02-api.md) HTTP-036). The server itself MUST NOT start a provider session: the registration only records one that the caller started.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-076
- **Verified by:** `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground (a server run with `--discovery=false` registers spawns and describes the directory)

#### SRV-014 Readiness
- **Behavior:** `Config.Ready` (if non-nil) MUST be invoked exactly once, after the listener is bound and the `Serve` goroutine is launched, and before background workers start. The CLI uses it to signal the `--bg` parent (D-026).
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (indirect)

#### SRV-015 Startup log line
- **Behavior:** After all workers are launched, `Run` MUST log INFO `noc server listening` with attributes `socket`, `database`, `host_id`, `version` (`buildinfo.String()`).
- **Source:** `go/internal/server/run.go` (Run)
- **Verified by:** MANUAL: Verification procedure M2 — the log file contains exactly one `msg="noc server listening"` line, after any startup discovery output.

### Workers

| Worker | Goroutines | Trigger | Period | Context | Body |
|---|---|---|---|---|---|
| HTTP `Serve` | 1 | connections | — | none (stopped by `Shutdown`) | localapi handler |
| Hook drain | 1 | immediate wake at start; `hookWake` channel; ticker | 2 s | `workerContext` | `processor.Drain(ctx, 128)` |
| Discovery | 1 per eligible adapter | ticker only (startup run already done) | `DiscoveryInterval` (default 5 s) | `workerContext` | `reconcileOnce` |
| Retention | 0 or 1 | immediate, then ticker | `RetentionInterval` (default 1 h) | `workerContext` | `PruneRetired(now - RetireAfter)` |

#### SRV-016 Hook wake coalescing
- **Behavior:** `hookWake` MUST be a channel of capacity 1 written non-blockingly (`select { case ch <- struct{}{}: default: }`), so any number of wakes between drains coalesce into one pending wake. The same `wakeHooks` function is given to the HTTP handler (for `POST /v1/hooks/wake`) and is called once by `Run` immediately after starting the drain loop so the outbox accumulated while the server was down is drained without waiting for the first tick.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-028
- **Verified by:** UNTESTED

#### SRV-017 Hook drain loop
- **Behavior:** The drain loop MUST select on `ctx.Done()` (return), a wake, or a 2 s ticker tick; both wake and tick call `drainHooks`, which calls `processor.Drain(ctx, hooks.DefaultDrainLimit=128)`. On error it MUST log WARN `hook observation drain failed` with `pending`, `processed`, `failed`, `error` and continue looping; on success with `Processed > 0` it logs DEBUG `hook observations processed` `count`. A drain never terminates the server.
- **Source:** `go/internal/server/run.go` (drainHooks, runHookDrainLoop)
- **Decision:** D-028
- **Verified by:** UNTESTED

#### SRV-018 Discovery loop
- **Behavior:** Each discovery loop MUST use a `time.Ticker` of `DiscoveryInterval`; it MUST NOT run immediately on start (startup discovery already ran, SRV-010). Each tick calls `reconcileOnce(ctx, worker, health, logger)`; reconciliations of one adapter are strictly serial (a slow pass delays the next; ticks missed while busy are dropped by `time.Ticker`, at most one buffered). Loops for different adapters run concurrently with each other, with the hook drain loop, and with HTTP handlers.
- **Source:** `go/internal/server/discovery.go` (runDiscoveryLoop)
- **Decision:** D-023
- **Verified by:** `go/internal/server/run_test.go` TestRunDiscoveryLoopPollsAfterInterval; serial execution and dropped ticks UNTESTED

#### SRV-019 Retention loop
- **Behavior:** When effective retention > 0, the retention loop MUST call `PruneRetired(ctx, time.Now().UTC().Add(-retireAfter))` once immediately and then on every tick of `RetentionInterval`, until `ctx` is cancelled. On error it MUST log WARN `registry retention sweep failed` and continue; when `pruned > 0` it logs INFO `pruned retired agents` with `count` and `retire_after`. The prune predicate is DB-022.
- **Source:** `go/internal/server/discovery.go` (runRetentionLoop); `go/internal/server/run.go` (Run)
- **Decision:** D-041, D-047
- **Verified by:** `go/internal/server/retention_test.go` TestRetentionLoopSweepsImmediatelyAndOnTick, TestRetentionLoopContinuesAfterError

#### SRV-020 Disabled discovery still runs drain and retention
- **Behavior:** With `DisableDiscovery`, `Run` MUST still open storage, serve the API, run the hook drain loop, and run the retention loop; it MUST create no reconcilers. Consequently every existing row MUST stay in its stored `live_state` indefinitely: snapshot-confirmed rows left `live` (or `candidate_stale`) by a previous discovery-enabled run are never aged to `not_live`, and hook-only rows are never retired by the process signal (REC-024). The only remaining paths that change liveness are hook observations drained from the outbox (e.g. `SessionEnd` makes a row `not_live`, see [06-hooks.md](06-hooks.md)), `forget`, and retention (which only removes rows that are already `not_live`). Health shows `disabled` for every adapter, so it does not reveal that `live` rows may be stale.
- **Source:** `go/internal/server/run.go` (Run); `go/internal/reconcile/reconciler.go` (aging exists only in RunOnce)
- **Verified by:** `go/internal/server/run_test.go` TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers (no probe, no discovery); MANUAL: Verification procedure M1 (no rows), and M5 (rows from a discovery-enabled run stay `live` under `--discovery=false`).

### Per-poll reconciliation wrapper

```
 ticker ─▶ reconcileOnce
            │ report, err := reconciler.RunOnce(ctx)
            │   ├─ adapter.DiscoverLive(ctx)            (error → return; nothing written)
            │   ├─ now := Now().UTC()
            │   ├─ for each native entry:
            │   │     Normalize → skip? → GetByNativeIdentity → Inspector.Inspect(cwd)
            │   │     → materialize(merge) → store.Upsert → RecordObservation (if transition)
            │   ├─ store.List(IncludeNotLive)  filtered to (hostID, provider)
            │   └─ for each row: hook-only → PID check unless already not_live
            │                    | present → skip | not_live → skip
            │                    | candidate_stale & ≥ staleAfter → not_live | else → candidate_stale
            │ observedAt := time.Now().UTC()
            ├─ err  → health.update(provider, "degraded", err.Error()); WARN "provider discovery failed"
            └─ ok   → health.update(provider, "available", "discovery active; observed N live sessions[; skipped K incompatible sessions]")
                      INFO "provider registry reconciled" iff CandidateStale>0 || MarkedNotLive>0
```

#### SRV-021 reconcileOnce health and logging
- **Behavior:** After `RunOnce` returns, `observedAt` MUST be `time.Now().UTC()`. On error: `health.update(provider, "degraded", err.Error(), observedAt)` and WARN `provider discovery failed` (`provider`, `error`). On success: detail MUST be exactly `discovery active; observed %d live sessions` with `report.Observed`, followed by `; skipped %d incompatible sessions` only when `report.Skipped > 0`; status `available`. INFO `provider registry reconciled` (`provider`, `observed`, `candidate_stale`, `marked_not_live`) MUST be logged only when `CandidateStale > 0 || MarkedNotLive > 0`.
- **Source:** `go/internal/server/discovery.go` (reconcileOnce)
- **Decision:** D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery

#### SRV-022 Observed count semantics
- **Behavior:** `Observed` (and thus the "observed N live sessions" detail) MUST equal the number of native entries returned by `DiscoverLive`, including entries later skipped as incompatible or duplicate.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerSkipsDuplicatesAndIncompatibleRows

### Shutdown

#### SRV-023 Shutdown ordering and return value
- **Behavior:** `Run` MUST block on `select { <-ctx.Done(); <-serverErrors }`. On context cancellation it MUST call `server.Shutdown` with a fresh 5 s timeout context and return that call's error (if any). If `Serve` returns first, any error other than `http.ErrServerClosed` MUST be returned. In both cases it then MUST cancel `workerContext` and wait for all worker goroutines (`sync.WaitGroup`) before returning. Deferred cleanup then runs in LIFO order: socket removal first, then `store.Close()`. Because `workerContext` derives from `ctx`, in-flight provider commands and drains observe cancellation as soon as `ctx` is cancelled, before HTTP shutdown completes.
- **Source:** `go/internal/server/run.go` (Run)
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery (returns nil within 3 s of cancellation)

#### SRV-024 Long-lived watch streams delay shutdown
- **Behavior:** `http.Server.Shutdown` waits for active requests; an open `GET /v1/agents/watch` stream only ends on its own request context, which `Shutdown` does not cancel. With one or more open watch streams, `Run` MUST therefore take the full 5 s and return `context deadline exceeded` (the socket file is still removed by the defer). Without open streams `Run` returns `nil` promptly.
- **Source:** `go/internal/server/run.go` (Run), `go/internal/localapi/server.go` (handleAgentWatch)
- **Verified by:** MANUAL: start the isolated server (M1), run `"$T/bin/noc" watch --json >/dev/null &`, then `kill -TERM <server pid>`; expect ~5 s until exit, stderr containing `context deadline exceeded`, and `$T/run/noc.sock` absent afterwards.

#### SRV-025 Signals
- **Behavior:** The process MUST map `SIGINT` and `SIGTERM` to cancellation of the root context via `signal.NotifyContext`; `Run` receives that context. Subsequent signals during shutdown are absorbed by the same notifier (no forced exit). The CLI releases the PID file (deferred in `runServerForeground`) only after `Run` returns.
- **Source:** `go/cmd/noc/main.go` (main), `go/internal/cli/run.go` (runServerForeground)
- **Decision:** D-026
- **Verified by:** MANUAL: Verification procedure M2 step 7 — after `kill -TERM`, both `noc.sock` and `noc.pid` are gone from `$T/run`.

### Provider health

| Field (`apiv1.ProviderHealth`) | Initial value (construction) | After `update()` |
|---|---|---|
| `provider` | `probe.Provider` | unchanged |
| `status` | derived from probe (SRV-027) | `available` / `degraded` |
| `version` | `probe.Version` | unchanged (never refreshed) |
| `discovery_source` | `claude agents --json` / `Codex app-server loaded-thread snapshot` / `provider adapter` | unchanged |
| `detail` | `probe.Diagnostic` | success detail or error text |
| `observed_at` | `probe.ObservedAt` | `observedAt.UTC()` |
| `startup_status`, `startup_detail`, `startup_observed_at` | set from probe iff adapter is not (`Available` ∧ `discover_live`) | set by the first `update()` only if not yet recorded; immutable afterwards |

#### SRV-026 Health payload
- **Behavior:** `GET /v1/health` MUST return `apiv1.Health{schema_version: 1, status: "ok", version: buildinfo.Version, commit: buildinfo.Commit, host_id, started_at, providers}` where `status` is the constant `"ok"` (server reachability, not provider health) and `providers` is the tracker snapshot in adapter order.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery

#### SRV-027 Initial status mapping
- **Behavior:** For each probe, initial `status` MUST be chosen in this precedence: `Diagnostic == "provider discovery disabled"` → `disabled`; `Diagnostic == "provider adapter not implemented"` (`provider.ErrNotImplemented`) → `not_implemented`; `Available` → `available`; otherwise `unavailable`. Matching is exact string equality on the diagnostic.
- **Source:** `go/internal/server/health.go` (newProviderHealthTracker, initialStatus)
- **Decision:** D-032
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthInitialStatusMapping, TestProviderHealthUsesUnavailableProbeAsStartupResult

#### SRV-028 Discovery source strings
- **Behavior:** `discovery_source` MUST be `claude agents --json` for `claude`, `Codex app-server loaded-thread snapshot` for `codex`, and `provider adapter` for any other provider.
- **Source:** `go/internal/server/health.go` (providerDiscoverySource)
- **Decision:** D-023, D-024, D-032
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthPreservesFirstDiscoveryResult

#### SRV-029 Startup result is recorded once and immutable
- **Behavior:** If a probe is not both `Available` and `Features["discover_live"]`, the startup fields MUST be set at construction from the probe (`startup_status` = initial status, `startup_detail` = diagnostic, `startup_observed_at` = `probe.ObservedAt.UTC()`) and marked recorded. Otherwise the first `update()` call (the synchronous startup reconciliation) MUST record them. Once recorded, `update()` MUST change only `status`, `detail`, and `observed_at`.
- **Source:** `go/internal/server/health.go` (newProviderHealthTracker, recordStartup, update)
- **Decision:** D-032
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthPreservesFirstDiscoveryResult, TestProviderHealthUsesUnavailableProbeAsStartupResult

#### SRV-030 Available-without-discover_live edge
- **Behavior:** A probe with `Available == true` but no `discover_live` feature MUST yield `status: "available"`, `detail` = probe diagnostic (usually empty), identical startup fields, and no worker; its health never changes afterwards.
- **Source:** `go/internal/server/health.go` (newProviderHealthTracker), `go/internal/server/run.go` (Run)
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthAvailableWithoutDiscoverLive; `go/internal/server/run_test.go` TestRunAvailableWithoutDiscoverLiveHasNoWorker

#### SRV-031 Tracker concurrency
- **Behavior:** The tracker MUST guard state with a `sync.RWMutex`: `update` takes the write lock; `snapshot` takes the read lock and returns a fresh slice of value copies in construction order. Updates for a provider not present at construction create a value but do not add it to `order`, so it is never reported.
- **Source:** `go/internal/server/health.go` (update, snapshot)
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthSnapshotOrderAndUnknownProvider; locking UNTESTED

---

## Reconciliation (`REC`)

### Identity

#### REC-001 Reconciler construction
- **Behavior:** `reconcile.New` MUST reject an empty `HostID` (`reconciler host ID is required`), nil `Store` (`reconciler store is required`), and nil `Adapter` (`reconciler adapter is required`). Defaults: `StaleAfter <= 0` → `30s`; `Now == nil` → `time.Now`; `NewID == nil` → `identity.NewUUID`; `WorkspaceInspector == nil` → a private `workspace.NewInspector()`; `IsRunning == nil` → `proclineage.IsRunning`. `RecordObservation` MAY be nil, in which case no events are recorded and no error results.
- **Source:** `go/internal/reconcile/reconciler.go` (New)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestNewValidates; TestReconcilerNilRecorderAndOtherProviderRowsUntouched (nil recorder)

#### REC-002 Natural key
- **Behavior:** Every registry row MUST be identified by the natural key `(host_id, provider, native_session_id)`; the reconciler resolves an existing row with `store.GetByNativeIdentity(ctx, reconciler.hostID, normalized.Provider, normalized.NativeSessionID)`. A store error other than `registry.ErrNotFound` MUST abort the pass, returning the partial report and the error wrapped as `look up <provider> agent: <err>`. Provider short IDs and names are never part of the key.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce, lookup)
- **Decision:** D-016, D-023
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

#### REC-003 UUID allocation
- **Behavior:** When no row exists, the reconciler MUST allocate the noc agent ID by calling `NewID` (default `identity.NewUUID`: 16 bytes from `crypto/rand`, version nibble `4`, RFC 4122 variant, lowercase hex `8-4-4-4-12`), and set `first_seen_at = now`. An allocation error MUST abort the pass with `allocate noc agent ID: <err>`. An existing row MUST keep its `id` and `first_seen_at` forever, including across `not_live` and revival.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize), `go/internal/identity/uuid.go` (NewUUID)
- **Decision:** D-016
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

### Snapshot pass

#### REC-004 Discovery failure changes nothing
- **Behavior:** If `adapter.DiscoverLive` returns an error, `RunOnce` MUST return `Report{}` and `discover <provider> agents: <err>` without reading or writing any row: no upserts, no aging, no hook-only process checks, no events.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-007
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestDiscoveryFailureDoesNotAgeAgents

#### REC-005 One timestamp per pass
- **Behavior:** `now` MUST be read once per pass as `Now().UTC()` after `DiscoverLive` returns; every timestamp written in the pass (`last_seen_at`, `snapshot_confirmed_at`, `ended_at`, `*_observed_at`, attention `since`, event times) uses that single value.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

#### REC-006 Skip rules
- **Behavior:** For each native entry, in order, the reconciler MUST skip (increment `Skipped`, write nothing) when: `Normalize` errors; `NativeSessionID == ""`; `normalized.Provider != adapter.Provider()`; or the `NativeSessionID` was already successfully upserted earlier in this pass (first occurrence wins). `Upserted` counts successful upserts.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-023
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerSkipsDuplicatesAndIncompatibleRows

#### REC-007 Skipped entries count as absent
- **Behavior:** A skipped entry MUST NOT be added to the pass's `seen` set, so an existing row whose native entry fails normalization is aged exactly as if the provider had omitted it (REC-020..REC-022).
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerSkippedEntriesAgeTheirRows

#### REC-008 Workspace enrichment
- **Behavior:** When `normalized.Workspace != nil` and `Workspace.Path != ""`, the reconciler MUST replace it with `Inspector.Inspect(ctx, path, owner, now)` where `owner` is the normalized owner or `"unknown"` if empty. (If the inspector returns nil for a whitespace-only path, the normalized workspace becomes nil and the existing workspace is preserved by the merge.)
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-027
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

#### REC-009 Upsert then event; errors abort
- **Behavior:** For each accepted entry the reconciler MUST compute the transition (REC-019) from the pre-upsert row, then `store.Upsert`, then (if a transition applies) record the event. An upsert error (wrapped `upsert <provider> agent: <err>`) or event-recording error MUST abort the pass and be returned (already-written rows stay written; a row upserted but whose event failed will not re-emit that event on a later pass).
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-033
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerFailuresAbortPass

### Merge rules

`existing` is the row read by natural key (zero value when absent; every
"existing" fallback below applies only when a row exists). `N` is the
normalized snapshot entry after workspace enrichment. Hook-derived values reach
the merge only through `existing`.

| Field | Result written by the snapshot |
|---|---|
| `schema_version` | `1` (also forced by the store) |
| `id` | `existing.id`, else new UUID (REC-003) |
| `host_id` | reconciler host ID |
| `provider` | `N.Provider` (equals the adapter's provider) |
| `native_session_id` | `N.NativeSessionID` |
| `native_short_id` | `N.NativeShortID` if non-empty, else `existing` |
| `name` | `N.Name` if non-empty; else `existing.name` unless it is an ID placeholder (REC-011), else `""` |
| `user_name` | not written by the reconciler; store column rule DB-016 |
| `team`, `lead`, `parent_id` | not written by the reconciler; store column rule DB-027 |
| `source_kind` | `N.SourceKind` if non-empty, else `existing` |
| `origin` | `existing.origin` if non-empty (`hook`, `spawn`), else `"reconcile"` |
| `live_state` | always `live` (`N.LiveState` is ignored) |
| `runtime_state` | `N.RuntimeState` (always overwrites, even empty) |
| `activity_state` | `N.ActivityState` (always overwrites, even empty) |
| `attention` | REC-013 (always from `N`; hook attention is overwritten) |
| `display_state`, `bucket` | recomputed by the store on write |
| `model`, `model_source`, `model_observed_at` | if `N.Model != ""`: `N.Model`, `N.ModelSource` or `"provider_snapshot"`, `now`; else all three from `existing` |
| `reasoning_effort`, `reasoning_source`, `reasoning_observed_at` | same pattern as model |
| `initial_request` | if `N.InitialRequest != nil`: copy, `source` defaulted to `"provider_snapshot"`, `observed_at` defaulted to `now` if zero; else copy of `existing` |
| `summary` | `N.Summary` if non-empty, else `existing` |
| `summary_source`, `summary_updated_at`, `summary_truncated` | if `N.Summary != ""`: `"provider_snapshot"`, `now`, `false`; else from `existing` |
| `created_at` | `N.CreatedAt` if non-nil, else `existing` |
| `first_seen_at` | `existing`, else `now` |
| `last_seen_at` | `now` |
| `last_activity_at` | `now` if `N.ActivityState == working`; else `N.LastActivityAt` if non-nil; else `existing` |
| `snapshot_confirmed_at` | `now` |
| `ended_at` | `nil` |
| `capabilities` | `N.Capabilities` if non-nil, else `existing` |
| `native_state` | `N.NativeState` (always overwrites, may become empty) |
| `workspace` | copy of `N.Workspace` with `observed_at = now` if non-nil, else copy of `existing` |
| `frontend_bindings` | copy of `existing` bindings, or `nil` for Claude `background` (REC-016) |
| `process_id` | always `0` (not carried; REC-017) |

#### REC-010 Snapshot materialization
- **Behavior:** The reconciler MUST build the stored agent exactly per the merge table above. Fields the snapshot leaves empty MUST fall back to the existing row only where the table says so; `runtime_state`, `activity_state`, `attention`, `native_state`, `live_state`, `last_seen_at`, `snapshot_confirmed_at`, and `ended_at` are always taken from the snapshot pass.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize)
- **Decision:** D-007, D-027
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage; `go/internal/reconcile/reconciler_test.go` TestReconcilerUsesProviderMetadataOverHookFallback

#### REC-011 Claude short-ID placeholder name is not carried forward
- **Behavior:** When `N.Name == ""` and a row exists, `existing.name` MUST be carried forward only if it is not an ID placeholder; a placeholder is a name whose `strings.TrimSpace` is non-empty and equals `existing.native_short_id` or `existing.native_session_id`. A placeholder therefore becomes `""` on the next snapshot, so an ID that was stored as a name is never treated as a provider name.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize, isIDPlaceholderName)
- **Decision:** D-042
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerPlaceholderNameNotCarriedForward

#### REC-012 user_name overlay interaction
- **Behavior:** The reconciler MUST NOT write `user_name`. The store's upsert clears it only when the materialized `name` is non-empty and differs from the previously stored snapshot `name` (DB-016). Because the merge carries the existing name when the snapshot has none, a nameless snapshot never clears the overlay; a new provider name does.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize), `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestProviderRenameReplacesUserName; `go/internal/registry/registry_test.go` TestMemoryProviderRenameReplacesUserName

#### REC-030 Team, lead and parent are not the reconciler's
- **Behavior:** The reconciler MUST NOT set an agent's `team`, `lead` or `parent_id`: the materialized agent carries none, and the store ignores them on an existing row and keeps what it has (DB-027). A row first created by the reconciler therefore has no team and no parent. The aging pass upserts rows it read from the store; the store ignores the overlay values those rows carry, so aging changes none of them either.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize, RunOnce), `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot (an upsert of the whole agent changes none of them). Not tested through the reconciler itself.

#### REC-013 Attention merge
- **Behavior:** `attention` MUST start as `N.Attention`; an empty type becomes `none`. For a non-`none` type, `since` MUST be preserved from `existing.attention.since` when a row exists with the same `type`, the same `detail`, and a non-nil `since`; otherwise `since = now`. For `none`, `since` is whatever `N` supplied (normally nil). `native_request_id` comes from `N`.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize, mergeAttention)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerAttentionSinceAndActivity

#### REC-014 Activity timestamp
- **Behavior:** `last_activity_at` MUST be `now` when `N.ActivityState == "working"`; otherwise `N.LastActivityAt` if non-nil; otherwise the existing value.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerAttentionSinceAndActivity; MANUAL: Verification procedure M2 — a `working` roster entry has `last_activity_at == last_seen_at`.

#### REC-015 Provenance of model, reasoning, initial request, summary
- **Behavior:** Provider-supplied model/reasoning/initial-request/summary MUST replace hook-derived values (with the provenance defaults in the merge table); when the provider supplies none, the hook-derived value and its original source/time MUST be preserved unchanged.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize)
- **Decision:** D-027
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerUsesProviderMetadataOverHookFallback; `go/internal/reconcile/reconciler_test.go` TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage

#### REC-016 Frontend bindings cleared for background Claude
- **Behavior:** `frontend_bindings` MUST be a copy of the existing bindings unless `apiv1.HooksSeeItsTerminal({Provider, SourceKind})` is false — i.e. provider `claude` and merged `source_kind == "background"` — in which case it MUST be `nil`. Interactive Claude and all Codex rows keep hook bindings.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize), `go/pkg/api/v1/types.go` (HooksSeeItsTerminal)
- **Decision:** D-050
- **Verified by:** `go/internal/reconcile/reconciler_bindings_test.go` TestBackgroundClaudeSessionsDropHookPaneBindings; `go/internal/reconcile/reconciler_test.go` TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage

#### REC-017 Process ID is dropped by snapshot upserts
- **Behavior:** The materialized agent MUST NOT carry `process_id`; every snapshot upsert stores `process_id = 0` (omitted from JSON). The value recorded by hooks is restored only by a later hook observation. This is harmless for the liveness signal because a snapshot-confirmed row is no longer hook-only (REC-023).
- **Source:** `go/internal/reconcile/reconciler.go` (materialize; no `ProcessID` in the literal)
- **Decision:** D-040
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerDropsProcessID

#### REC-018 Origin preserved
- **Behavior:** `origin` MUST be preserved from the existing row (`hook`, or `spawn` for a row a spawn registration created, [02-api.md](02-api.md) HTTP-036); only a row first created by the reconciler gets `reconcile`.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage

### Liveness state machine

A row is **hook-only** when `snapshot_confirmed_at == nil && origin != "reconcile"`
(`hookOnly` in `reconciler.go`); every other row is **snapshot-covered**. A row
that a spawn registration created (`origin` `spawn`) is hook-only by this
definition until a snapshot includes its session. The aging pass
reads `store.List(IncludeNotLive: true)` after all upserts and considers only rows
with this reconciler's `host_id` and provider.

Presence (first loop, any row present in the snapshot):

| Pre-state of row | Next | Event type | Reason |
|---|---|---|---|
| none (new) | `live` | `agent.discovered` | `observed_in_provider_snapshot` |
| `live`, `snapshot_confirmed_at == nil` | `live` | `agent.snapshot_confirmed` | `confirmed_by_provider_snapshot` |
| `live`, confirmed | `live` | — | — |
| `candidate_stale`, `not_live`, `unknown`, or `""` | `live`, `ended_at = nil` | `agent.revived` | `reappeared_in_provider_snapshot` |

Absence (aging loop, snapshot-covered rows not in `seen`):

| Pre-state | Condition | Next | Side effects | Event / reason |
|---|---|---|---|---|
| `not_live` | — | `not_live` | none | — |
| `candidate_stale` | `now - last_seen_at >= staleAfter` | `not_live` | `ended_at = now`; `runtime_state` unchanged | `agent.not_live` / `missing_from_snapshot_past_stale_grace` |
| `candidate_stale` | `now - last_seen_at < staleAfter` | `candidate_stale` | none | — |
| `live`, `unknown`, `""` | — | `candidate_stale` | `last_seen_at` unchanged | `agent.candidate_stale` / `missing_from_provider_snapshot` |

Hook-only rows (aging loop, regardless of presence in `seen`, which cannot contain them post-upsert):

| Condition | Next | Side effects | Event / reason |
|---|---|---|---|
| not yet `not_live`, `process_id > 0`, and `!IsRunning(process_id)` | `not_live` | `runtime_state = stopped`, `ended_at = now` | `agent.not_live` / `hook_session_process_exited` (once) |
| otherwise (already `not_live`, `process_id == 0`, or process answers) | unchanged | none | — |

Discovery error: all rows unchanged, no events (REC-004).

#### REC-019 Presence transitions
- **Behavior:** For every accepted snapshot entry the event MUST be chosen from the pre-upsert row as in the presence table; `previous_live_state` is the pre-upsert `live_state` (empty for a new row).
- **Source:** `go/internal/reconcile/reconciler.go` (presenceTransition)
- **Decision:** D-033
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness (discovered, revived from `not_live`), TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage (`agent.snapshot_confirmed`)

#### REC-020 Live to candidate_stale
- **Behavior:** A snapshot-covered row of this host/provider that is absent from a successful snapshot and whose state is anything other than `candidate_stale` or `not_live` MUST become `candidate_stale` with `last_seen_at` unchanged, emit `agent.candidate_stale` (`missing_from_provider_snapshot`), and increment `CandidateStale`. A single absent snapshot MUST NOT make a row `not_live`.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-023
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

#### REC-021 candidate_stale to not_live
- **Behavior:** A `candidate_stale` row still absent from a successful snapshot MUST become `not_live` iff `now - last_seen_at >= staleAfter`, setting `ended_at = now`, leaving `runtime_state`, `activity_state`, and `attention` as last observed, emitting `agent.not_live` (`missing_from_snapshot_past_stale_grace`), and incrementing `MarkedNotLive`. Below the threshold nothing changes.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-023
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

#### REC-022 Stale timing
- **Behavior:** With last sighting at `T`, poll interval `P`, grace `G`: the row becomes `candidate_stale` on the first successful pass after `T` that omits it, and `not_live` on the first later successful pass at or after `T + G`; it can never go from `live` to `not_live` in one pass (minimum two absent passes). With defaults (`P = 5s`, `G = 30s`) this is typically 30–35 s after the last sighting. Failed passes (REC-004) neither advance nor reset this, but `last_seen_at` keeps aging, so the first successful pass after a long outage marks `candidate_stale` and the next marks `not_live`.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-023
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness; MANUAL: Verification procedure M2 steps 4–5

#### REC-023 Hook-only rows are not aged by absence
- **Behavior:** A hook-only row MUST NOT be moved to `candidate_stale` or `not_live` because it is missing from a snapshot (a snapshot source may cover only part of a provider's sessions, e.g. Codex's shared daemon or Claude's background-only roster). Once a snapshot includes it, `snapshot_confirmed_at` is set and it ages as snapshot-covered from then on, even if its `origin` stays `hook`.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-028, D-040
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerRetiresHookOnlyAgentWhoseProcessExited; `go/internal/reconcile/reconciler_test.go` TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage

#### REC-024 Negative-only process signal for hook-only rows
- **Behavior:** On every successful pass, for each hook-only row of this host/provider that is not already `not_live`, has `process_id > 0`, and for which `IsRunning(process_id)` is false, the reconciler MUST set `live_state = not_live`, `runtime_state = stopped`, `ended_at = now`, upsert, emit `agent.not_live` with reason `hook_session_process_exited` and the row's previous live state, and increment `MarkedNotLive`. A running process or `process_id == 0` MUST leave the row untouched. `proclineage.IsRunning(pid)` is `false` for `pid <= 1` and for `kill(pid, 0) == ESRCH`, and `true` for success, `EPERM`, or any other error (fail-open). A stored `process_id` of `1` would therefore be treated as exited; the hook path never records one, because the ancestor walk stops at a parent `<= 1`.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce, hookOnly), `go/internal/proclineage/proclineage.go` (IsRunning, FindAncestor)
- **Decision:** D-040
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerRetiresHookOnlyAgentWhoseProcessExited; `go/internal/proclineage/proclineage_test.go` TestIsRunningDetectsAnExitedProcess

#### REC-025 Already-ended hook-only rows are left alone
- **Behavior:** The process check in REC-024 MUST skip a hook-only row whose `live_state` is already `not_live` (e.g. ended by a `SessionEnd` hook, or retired by an earlier pass), whatever its `process_id`: the row is not upserted, `ended_at` keeps its value, no `agent.not_live` event is journaled, and `MarkedNotLive` is not incremented. A dead process is therefore reported exactly once, and later passes neither rewrite the row nor log `provider registry reconciled` for it.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-033, D-040
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerDoesNotRemarkEndedHookOnlyRows

#### REC-026 Aging scope
- **Behavior:** The aging loop MUST skip rows with a different `host_id` or provider, and rows present in this pass's `seen` set. It runs only when every accepted entry was upserted without error.
- **Source:** `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-016
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerNilRecorderAndOtherProviderRowsUntouched; `go/internal/server/run_test.go` TestRunServesInitialDiscovery (two providers coexist)

#### REC-027 Derived display state of ended rows
- **Behavior:** Because the store recomputes `display_state` from attention → activity → live state (`apiv1.DeriveDisplayState`) and the aging path leaves activity/attention untouched, a row that went `not_live` while `working` MUST be stored with `display_state: "working"` and `bucket: "quiet"` (bucket requires a live process, D-048). Likewise a `candidate_stale` row keeps an activity-derived display state when activity is `working`/`waiting` or attention is set.
- **Source:** `go/pkg/api/v1/types.go` (DeriveDisplayState), `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-046, D-048
- **Verified by:** MANUAL: Verification procedure M2 step 5 — after the fake session ages out, `noc ls --all --json` shows `live_state: "not_live"`, `display_state: "working"`, `bucket: "quiet"`.

### Semantic events

#### REC-028 Snapshot event record
- **Behavior:** Each transition MUST be journaled (when `RecordObservation` is set) as `apiv1.Observation{schema_version: 1, event_id: "snapshot-" + lowercase hex SHA-256 of host_id NUL provider NUL native_session_id NUL event_type NUL previous_live_state NUL eventAt.UTC().Format(RFC3339Nano), provider, host_id, native_session_id, event_type, source: "snapshot", occurred_at: eventAt, received_at: eventAt, data: {reason, previous_live_state, live_state}}`, passed with `processedAt = eventAt`. A recording error MUST be wrapped as `record <event_type> transition: <err>`. Stored `payload_json` is `json.Marshal(data)`, which always includes `"native_event":""`. Routine passes with no transition MUST NOT record anything.
- **Source:** `go/internal/reconcile/reconciler.go` (recordTransition), `go/internal/storage/sqlite/hooks.go` (RecordObservation)
- **Decision:** D-033
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness (types, source, transitions); event ID format MANUAL: M2 step 6 — every snapshot event id matches `^snapshot-[0-9a-f]{64}$`.

#### REC-029 Event idempotency
- **Behavior:** Recording MUST use `INSERT ... ON CONFLICT(id) DO NOTHING`, so replaying an identical transition with the same timestamp is a no-op.
- **Source:** `go/internal/storage/sqlite/hooks.go` (RecordObservation)
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestHookOutboxIsDurableAndIdempotent (outbox and journal: recording the same event twice leaves one row)

| Event type | Constant | Emitted when |
|---|---|---|
| `agent.discovered` | `eventAgentDiscovered` | new natural key in snapshot |
| `agent.snapshot_confirmed` | `eventAgentSnapshotConfirmed` | live hook-only row first appears in snapshot |
| `agent.revived` | `eventAgentRevived` | non-live row reappears |
| `agent.candidate_stale` | `eventAgentCandidateStale` | first absent pass |
| `agent.not_live` | `eventAgentNotLive` | grace expired, or hook-only process exited |

---

## SQLite storage (`DB`)

### Opening

#### DB-001 Driver
- **Behavior:** The store MUST use `modernc.org/sqlite` (pure Go, no CGO) registered as driver name `"sqlite"` through `database/sql`; `go.mod` pins `v1.58.0`.
- **Source:** `go/internal/storage/sqlite/store.go` (driver import, Open), `go/go.mod`
- **Decision:** D-022
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### DB-002 Path and directory
- **Behavior:** `Open` MUST reject an empty path with `SQLite path is required`. It MUST ensure the parent directory with `config.EnsurePrivateDir`: an existing directory is used as is (never chmodded); an existing non-directory fails `private directory path %q is not a directory`; a missing one is created with `MkdirAll(0700)` then `Chmod(0700)`. Default path: `$NOC_DATABASE`, else `<state dir>/noc.db`, where state dir is `$NOC_STATE_DIR`, else `$XDG_STATE_HOME/noc`, else `~/Library/Application Support/noc` (darwin) or `~/.local/state/noc`.
- **Source:** `go/internal/storage/sqlite/store.go` (Open), `go/internal/config/paths.go` (EnsurePrivateDir, ResolvePaths)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestOpenRequiresPath; `go/internal/config/paths_test.go` TestEnsurePrivateDirDoesNotChmodExistingDirectory, TestEnsurePrivateDirSecuresNewDirectory

#### DB-003 Single connection and ping
- **Behavior:** `Open` MUST set `SetMaxOpenConns(1)` (all in-process statements serialize on one connection) and `PingContext`; ping failure closes the handle and returns `connect to SQLite database: <err>`.
- **Source:** `go/internal/storage/sqlite/store.go` (Open)
- **Verified by:** UNTESTED

#### DB-004 Pragmas
- **Behavior:** `Open` MUST execute, in order, `PRAGMA foreign_keys = ON`, `PRAGMA journal_mode = WAL`, `PRAGMA busy_timeout = 5000`; any failure closes the handle and returns `configure SQLite database: <err>`. `journal_mode=WAL` persists in the file; the other two are per-connection.
- **Source:** `go/internal/storage/sqlite/store.go` (Open)
- **Decision:** D-022
- **Verified by:** MANUAL: while an isolated server is running, `sqlite3 -readonly "$T/state/noc.db" "PRAGMA journal_mode;"` prints `wal`, and `noc.db-wal`/`noc.db-shm` exist. Immutable-URI inspection of a stopped server reports `delete`, which reflects immutable mode, not the file setting.

#### DB-005 Migrate then secure
- **Behavior:** After pragmas, `Open` MUST run `migrate` (DB-006) and then `os.Chmod(path, 0600)`; failure closes and returns `secure SQLite database %q: <err>`. Only the main database file is chmodded. The WAL sidecars `<db>-wal` and `<db>-shm` are created by SQLite with the process umask and MUST be expected at `0644` under the usual `022` umask (`noc.db` `0600`, `noc.db-wal` and `noc.db-shm` `0644`, a noc-created state directory `0700`). They are protected only by the containing directory, which is `0700` when noc created it (DB-002) and is left as is if it already existed (e.g. `NOC_DATABASE` in a shared directory), in which case the WAL, which holds recent row contents, is readable by other users. On a clean `Close` SQLite checkpoints and deletes both sidecars, so they exist only while a connection is open.
- **Source:** `go/internal/storage/sqlite/store.go` (Open)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (main file `0600` only; sidecar modes UNTESTED); MANUAL: Verification procedure M1 step 5

#### DB-006 Migration runner
- **Behavior:** `migrate` MUST first run `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);`, read `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`, and for each migration with a greater version, in ascending order, run its SQL and `INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))` inside one transaction per migration. Errors: `create schema migration table`, `read schema version`, `begin migration %d`, `apply migration %d`, `record migration %d`, `commit migration %d` (each `: <err>`), rolling back where applicable. A database whose recorded version exceeds the newest known migration MUST be accepted silently (no downgrade guard). Reopening an up-to-date database applies nothing.
- **Source:** `go/internal/storage/sqlite/migrations.go` (migrate)
- **Decision:** D-022
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestMigrationsAreIdempotentAndTolerateNewerDatabases, TestStorePersistsHostAndAgents (reopen)

#### DB-007 Close
- **Behavior:** `Close` MUST close the handle, wrapping failures as `close SQLite database: <err>`.
- **Source:** `go/internal/storage/sqlite/store.go` (Close)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

### Schema

#### DB-008 Migration 1 (initial schema)
- **Behavior:** Version 1 MUST execute exactly:
  ```sql
  CREATE TABLE metadata (
      key TEXT PRIMARY KEY,
      value TEXT NOT NULL,
      updated_at TEXT NOT NULL
  );

  CREATE TABLE agents (
      id TEXT PRIMARY KEY,
      host_id TEXT NOT NULL,
      provider TEXT NOT NULL,
      native_session_id TEXT NOT NULL,
      live_state TEXT NOT NULL,
      display_state TEXT NOT NULL,
      last_seen_at TEXT NOT NULL,
      snapshot_json BLOB NOT NULL,
      UNIQUE (host_id, provider, native_session_id)
  );

  CREATE INDEX agents_live_display_idx
      ON agents (live_state, display_state, id);

  CREATE TABLE observations (
      id TEXT PRIMARY KEY,
      schema_version INTEGER NOT NULL,
      provider TEXT NOT NULL,
      host_id TEXT NOT NULL,
      native_session_id TEXT,
      event_type TEXT NOT NULL,
      source TEXT NOT NULL,
      occurred_at TEXT,
      received_at TEXT NOT NULL,
      processed_at TEXT,
      payload_json BLOB NOT NULL,
      error TEXT
  );

  CREATE INDEX observations_session_received_idx
      ON observations (provider, native_session_id, received_at);

  CREATE TABLE hook_outbox (
      id TEXT PRIMARY KEY,
      received_at TEXT NOT NULL,
      envelope_json BLOB NOT NULL,
      attempts INTEGER NOT NULL DEFAULT 0,
      processed_at TEXT,
      last_error TEXT
  );

  CREATE INDEX hook_outbox_pending_idx
      ON hook_outbox (processed_at, received_at);
  ```
- **Source:** `go/internal/storage/sqlite/migrations.go` (migrations)
- **Decision:** D-016, D-022, D-028
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### DB-009 Migration 2
- **Behavior:** Version 2 MUST execute `CREATE INDEX observations_received_idx ON observations (received_at DESC, id DESC);` (supports newest-first event queries, D-033).
- **Source:** `go/internal/storage/sqlite/migrations.go` (migrations)
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads

#### DB-010 Migration 3
- **Behavior:** Version 3 MUST execute `ALTER TABLE agents ADD COLUMN user_name TEXT NOT NULL DEFAULT '';`.
- **Source:** `go/internal/storage/sqlite/migrations.go` (migrations)
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### DB-026 Migration 4
- **Behavior:** Version 4 MUST execute, in one transaction:
  ```sql
  ALTER TABLE agents ADD COLUMN team TEXT NOT NULL DEFAULT '';
  ALTER TABLE agents ADD COLUMN team_lead INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE agents ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
  CREATE INDEX agents_team_idx ON agents (team);
  ```
  Rows that existed before the migration are in no team, lead nothing and have no parent.
- **Source:** `go/internal/storage/sqlite/migrations.go` (migrations)
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot; `go/internal/storage/sqlite/store_test.go` TestMigrationsAreIdempotentAndTolerateNewerDatabases (four migrations recorded). Migrating a database that already holds rows is UNTESTED.

Effective schema after all migrations:

| Table | Column | Type / constraint | Written by |
|---|---|---|---|
| `schema_migrations` | `version` | `INTEGER PRIMARY KEY` | migrate |
| | `applied_at` | `TEXT NOT NULL`, `YYYY-MM-DDTHH:MM:SS.sssZ` | migrate |
| `metadata` | `key` | `TEXT PRIMARY KEY` (only `host_id` today) | HostID |
| | `value` | `TEXT NOT NULL` | HostID |
| | `updated_at` | `TEXT NOT NULL`, RFC3339Nano UTC | HostID |
| `agents` | `id` | `TEXT PRIMARY KEY` (noc UUID) | Upsert |
| | `host_id`, `provider`, `native_session_id` | `TEXT NOT NULL`, `UNIQUE(host_id, provider, native_session_id)` | Upsert |
| | `live_state` | `TEXT NOT NULL` (`live`/`candidate_stale`/`not_live`/…) | Upsert |
| | `display_state` | `TEXT NOT NULL` (derived) | Upsert |
| | `last_seen_at` | `TEXT NOT NULL`, Go `time.RFC3339Nano` UTC (variable fraction digits) | Upsert |
| | `snapshot_json` | `BLOB NOT NULL`, JSON of `apiv1.Agent` without `user_name`, `team`, `lead`, `parent_id` | Upsert |
| | `user_name` | `TEXT NOT NULL DEFAULT ''` | SetUserName; cleared by Upsert |
| | `team` | `TEXT NOT NULL DEFAULT ''` (`''` = no team) | Upsert on insert only; SetTeam |
| | `team_lead` | `INTEGER NOT NULL DEFAULT 0` (`1` = the team's lead) | SetLead; cleared by SetTeam |
| | `parent_id` | `TEXT NOT NULL DEFAULT ''` (a noc agent id; not a foreign key) | Upsert on insert only; SetParent |
| `observations` | `id` | `TEXT PRIMARY KEY` (event ID) | RecordObservation |
| | `schema_version` | `INTEGER NOT NULL` | RecordObservation |
| | `provider`, `host_id` | `TEXT NOT NULL` | RecordObservation |
| | `native_session_id` | `TEXT` (NULL when empty) | RecordObservation |
| | `event_type`, `source` | `TEXT NOT NULL` | RecordObservation |
| | `occurred_at` | `TEXT` (NULL when absent), fixed 9-digit layout | RecordObservation |
| | `received_at` | `TEXT NOT NULL`, fixed 9-digit layout | RecordObservation |
| | `processed_at` | `TEXT`, fixed 9-digit layout | RecordObservation |
| | `payload_json` | `BLOB NOT NULL`, JSON of `apiv1.ObservationData` | RecordObservation |
| | `error` | `TEXT` (NULL when empty) | RecordObservation |
| `hook_outbox` | `id` | `TEXT PRIMARY KEY` (event ID) | EnqueueObservation |
| | `received_at` | `TEXT NOT NULL`, fixed 9-digit layout | EnqueueObservation |
| | `envelope_json` | `BLOB NOT NULL`, JSON of full `apiv1.Observation` | EnqueueObservation |
| | `attempts` | `INTEGER NOT NULL DEFAULT 0`, max 5 | Mark* |
| | `processed_at` | `TEXT` | MarkObservationProcessed |
| | `last_error` | `TEXT`, ≤ 1024 bytes | MarkObservationFailed |

Indexes: `agents_live_display_idx`, `agents_team_idx`, `observations_session_received_idx`,
`observations_received_idx`, `hook_outbox_pending_idx`, plus SQLite's implicit
`sqlite_autoindex_agents_1` (PK), `sqlite_autoindex_agents_2` (natural-key
UNIQUE), `sqlite_autoindex_hook_outbox_1`, `sqlite_autoindex_metadata_1`,
`sqlite_autoindex_observations_1`, and the rowid PK of `schema_migrations`.
`foreign_keys` is enabled but no table declares a foreign key.

#### DB-011 JSON blob storage
- **Behavior:** JSON columns MUST be written as Go `[]byte` (`json.Marshal` output), so SQLite stores them with type `blob`; SQL JSON functions (`json_extract`) operate on them directly. `snapshot_json` MUST be the full `apiv1.Agent` JSON (field names per its `json` tags, `omitempty` fields absent, timestamps as RFC3339Nano) with `schema_version = 1`, derived `display_state` and `bucket`, and `user_name`, `team`, `lead` and `parent_id` removed.
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert)
- **Verified by:** MANUAL: Verification procedure M3 — `SELECT typeof(snapshot_json), json_extract(snapshot_json,'$.user_name') FROM agents;` prints `blob|` (NULL).

### Host identity

#### DB-012 HostID
- **Behavior:** `HostID` MUST return `metadata.value` for key `host_id` if present. Otherwise it MUST generate a UUIDv4, execute `INSERT INTO metadata (key, value, updated_at) VALUES ('host_id', ?, ?) ON CONFLICT(key) DO NOTHING` with `updated_at = time.Now().UTC().Format(RFC3339Nano)`, and re-read the stored value (so a concurrent writer's value wins). Errors: `read host identity`, `generate host identity`, `persist host identity`, `confirm host identity`. The identity MUST survive reopen.
- **Source:** `go/internal/storage/sqlite/store.go` (HostID)
- **Decision:** D-016
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

### Agent rows

#### DB-013 Upsert validation and derived fields
- **Behavior:** `Upsert` MUST reject an agent missing `id`, `host_id`, `provider`, or `native_session_id` with `agent ID, host ID, provider, and native session ID are required`. Before encoding it MUST set `schema_version = 1`, `display_state = DeriveDisplayState(agent)`, `bucket = DeriveBucket(agent)`, and `user_name = ""`; it MUST also take `team` and `parent_id` aside for the insert (DB-027) and blank `team`, `lead` and `parent_id` in the value it encodes. Encoding failure → `encode agent: <err>`.
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-046
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### DB-014 Upsert statement
- **Behavior:** `Upsert` MUST execute exactly (parameters: id, host_id, provider, native_session_id, live_state, display_state, `last_seen_at.UTC().Format(RFC3339Nano)`, snapshot bytes, team, parent_id, name, name):
  ```sql
  INSERT INTO agents (
      id, host_id, provider, native_session_id, live_state,
      display_state, last_seen_at, snapshot_json, team, parent_id
  ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  ON CONFLICT(id) DO UPDATE SET
      host_id = excluded.host_id,
      provider = excluded.provider,
      native_session_id = excluded.native_session_id,
      live_state = excluded.live_state,
      display_state = excluded.display_state,
      last_seen_at = excluded.last_seen_at,
      user_name = CASE
          WHEN ? <> '' AND ? <> COALESCE(json_extract(agents.snapshot_json, '$.name'), '')
          THEN ''
          ELSE agents.user_name
      END,
      snapshot_json = excluded.snapshot_json
  ```
  The conflict target is `id` only: inserting a different `id` for an existing natural key MUST fail with `upsert agent: ... UNIQUE constraint failed: agents.host_id, agents.provider, agents.native_session_id`. A new row gets `user_name = ''` and `team_lead = 0`. Each upsert is a single autocommit statement.
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-016
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestUpsertNaturalKeyConflictWithDifferentIDFails

#### DB-015 Memory/SQLite parity for rename
- **Behavior:** The memory registry MUST apply the same rule: keep `UserName` unless `ProviderRenamed(existing.Name, agent.Name)` (`current != "" && current != previous`).
- **Source:** `go/internal/registry/registry.go` (Memory.Upsert, ProviderRenamed)
- **Decision:** D-042
- **Verified by:** `go/internal/registry/registry_test.go` TestMemoryProviderRenameReplacesUserName

#### DB-016 user_name clearing rule
- **Behavior:** On update, `user_name` MUST be set to `''` iff the incoming `name` is non-empty AND differs (exact, untrimmed comparison) from the `name` in the previously stored `snapshot_json` (absent → `''`); otherwise the stored `user_name` is kept. A nameless observation or an identical name never clears it.
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-042
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestProviderRenameReplacesUserName, TestStorePersistsHostAndAgents

#### DB-017 List
- **Behavior:** `List` MUST execute `SELECT snapshot_json, user_name, team, team_lead, parent_id FROM agents [WHERE live_state <> 'not_live'] ORDER BY display_state, id`, the `WHERE` clause present unless `IncludeNotLive`. Ordering is byte-wise on the `display_state` string then `id`. An empty result MUST be a non-nil empty slice. Errors: `list agents`, `iterate agents`, or a decode error.
- **Source:** `go/internal/storage/sqlite/store.go` (List)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestListFiltersAndOrdersAlphabetically

#### DB-018 Get and GetByNativeIdentity
- **Behavior:** Both select the same five columns as `List` (DB-017). `Get` MUST select by `id = ?`; `GetByNativeIdentity` by `host_id = ? AND provider = ? AND native_session_id = ?`. `sql.ErrNoRows` MUST map to `registry.ErrNotFound`; other errors are returned as is.
- **Source:** `go/internal/storage/sqlite/store.go` (Get, GetByNativeIdentity, getWhere)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestForgetDeletesOnlyTheNamedAgent

#### DB-019 Row decoding
- **Behavior:** Decoding MUST unmarshal `snapshot_json` into `apiv1.Agent` (`decode stored agent: <err>` on failure), set `UserName`, `Team` and `ParentID` from their columns, set `Lead` to true only when `team_lead` is set and `team` is not empty, and recompute `Bucket = DeriveBucket(agent)` (so a row always carries the bucket the current rules give it, whatever was stored). `DisplayState` is NOT recomputed on read. `process_id` MUST round-trip.
- **Source:** `go/internal/storage/sqlite/store.go` (scanAgent)
- **Decision:** D-046
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestForgetDeletesOnlyTheNamedAgent (process ID round trip); `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot (team, lead, parent). A set `team_lead` beside an empty `team` is UNTESTED (no method produces it).

#### DB-020 SetUserName
- **Behavior:** `SetUserName(id, name)` MUST execute `UPDATE agents SET user_name = ? WHERE id = ?` with the value as given (no trimming here; empty clears), return `registry.ErrNotFound` when no row was updated, and otherwise return `Get(id)`. Errors: `set agent user name`, `confirm agent user name`.
- **Source:** `go/internal/storage/sqlite/store.go` (SetUserName)
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### DB-027 team, lead and parent on Upsert
- **Behavior:** The `INSERT` of DB-014 MUST write the incoming agent's `team` and `parent_id` to their columns, and leave `team_lead` at its default `0`, whatever `lead` the incoming agent carries. The `ON CONFLICT` update MUST NOT name `team`, `team_lead` or `parent_id`, so an existing row keeps all three and the incoming values are ignored. The memory registry MUST apply the same rule: a new agent keeps its incoming team and parent with `Lead = false`; an existing one takes all three from the stored agent. ([02-api.md](02-api.md) REG-021 states the consequence for callers.)
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert), `go/internal/registry/registry.go` (Memory.Upsert)
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot; `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead, TestSpawnRegistrationRules (memory)

#### DB-028 SetTeam, SetLead, SetParent
- **Behavior:**
  - `SetTeam(id, team)` MUST execute `UPDATE agents SET team_lead = CASE WHEN team = ? THEN team_lead ELSE 0 END, team = ? WHERE id = ?` (parameters team, team, id): the lead mark is kept only when the team does not change. Errors wrap as `set agent team: <err>`.
  - `SetLead(id)` MUST, in one transaction, read `SELECT team FROM agents WHERE id = ?` (no row → `registry.ErrNotFound`; empty team → `registry.ErrNoTeam`, nothing written), execute `UPDATE agents SET team_lead = (id = ?) WHERE team = ?` (parameters id, team), and commit. Every other agent of the team loses the mark in the same statement. Errors wrap as `set team lead: <err>`.
  - `SetParent(id, parentID)` MUST execute `UPDATE agents SET parent_id = ? WHERE id = ?`. Errors wrap as `set agent parent: <err>`.

  `SetTeam` and `SetParent` return `registry.ErrNotFound` when no row was updated. All three return `Get(id)` on success. Values are stored as given; validating a team name is the HTTP handler's job.
- **Source:** `go/internal/storage/sqlite/store.go` (SetTeam, SetLead, SetParent, updateOne)
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot

#### DB-021 Forget
- **Behavior:** `Forget(id)` MUST execute `DELETE FROM agents WHERE id = ?` and return `registry.ErrNotFound` when no row was deleted (including a second forget). Errors: `forget agent`, `confirm forgotten agent`. Observations for the session are not deleted. A live session is recreated (with a new UUID) by its next hook or snapshot.
- **Source:** `go/internal/storage/sqlite/store.go` (Forget)
- **Decision:** D-040
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestForgetDeletesOnlyTheNamedAgent

#### DB-022 PruneRetired
- **Behavior:** `PruneRetired(endedBefore)` MUST execute (parameters `'not_live'`, `endedBefore.UTC().Format(RFC3339Nano)`):
  ```sql
  DELETE FROM agents
  WHERE live_state = ?
    AND (user_name IS NULL OR TRIM(user_name) = '')
    AND (
      last_seen_at < ?
      OR (
        TRIM(COALESCE(json_extract(snapshot_json, '$.name'), '')) = ''
        AND TRIM(COALESCE(json_extract(snapshot_json, '$.initial_request.text'), '')) = ''
      )
    )
  ```
  and return the number of rows deleted. Thus: live and `candidate_stale` rows are never pruned; any row with a non-blank `user_name` is never pruned; an ended row with no provider name and no initial-request text ("never engaged", D-047) is pruned at any age; any other ended row is pruned when `last_seen_at` (last sighting, not `last_activity_at`) is older than the cutoff. Comparison is lexical on RFC3339Nano strings. Errors wrap as `prune retired agents: <err>`.
- **Source:** `go/internal/storage/sqlite/store.go` (PruneRetired)
- **Decision:** D-041, D-047
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestPruneRetiredMatchesTheMemoryPredicate

#### DB-023 Prune predicate parity
- **Behavior:** `registry.retired` (memory store) MUST implement the same predicate: `live_state == not_live` ∧ blank `UserName` ∧ (`NeverEngaged` ∨ `LastSeenAt < endedBefore`). Known divergence: Go uses `strings.TrimSpace` (all Unicode whitespace) while SQL `TRIM` strips only spaces, so a name consisting only of tabs/newlines is "blank" in memory but not in SQLite.
- **Source:** `go/internal/registry/registry.go` (retired), `go/pkg/api/v1/types.go` (NeverEngaged)
- **Decision:** D-041, D-047
- **Verified by:** `go/internal/registry/registry_test.go` TestPruneRetiredKeepsLiveAndNamedRows; `go/internal/storage/sqlite/store_test.go` TestPruneRetiredMatchesTheMemoryPredicate; the whitespace divergence UNTESTED

#### DB-024 Journal and outbox are never pruned
- **Behavior:** No code path deletes from `observations` or `hook_outbox`; processed outbox rows and all journaled events are retained for the life of the database file. (Their write semantics are in `hooks.go`: outbox `ON CONFLICT(id) DO NOTHING`, pending = `processed_at IS NULL AND attempts < 5` ordered by `received_at, id`; see [06-hooks.md](06-hooks.md).)
- **Source:** `go/internal/storage/sqlite/store.go` (only `DELETE` statements are on `agents`), `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-028
- **Verified by:** UNTESTED

#### DB-025 Concurrency
- **Behavior:** In-process access MUST be serialized through the single pooled connection; no statement nests another query while iterating rows. The only explicit transactions are the migrations and `SetLead` (DB-028): each upsert, delete, and other update is atomic on its own, but reconciler and hook-processor read-modify-write sequences are not, so a hook observation committed between a reconciler's read and its upsert can be overwritten by that upsert. Cross-process writers (each `noc hook ingest` opens its own `Store`) coordinate through WAL and the 5 s busy timeout.
- **Source:** `go/internal/storage/sqlite/store.go` (Open), `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-022, D-028
- **Verified by:** UNTESTED

---

## Workspace inspection (`WS`)

#### WS-001 Inspector construction
- **Behavior:** `NewInspector` MUST return an inspector that runs the `git` found on `PATH` (binary name `"git"`) with an empty cache map guarded by a `sync.Mutex`.
- **Source:** `go/internal/workspace/inspect.go` (NewInspector)
- **Decision:** D-027
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree

#### WS-002 Blank path
- **Behavior:** `Inspect` MUST return `nil` (no workspace, no Git call, no cache entry) when `strings.TrimSpace(path) == ""`.
- **Source:** `go/internal/workspace/inspect.go` (Inspect)
- **Verified by:** UNTESTED

#### WS-003 Cache key and hit
- **Behavior:** The cache key MUST be `filepath.Abs(filepath.Clean(path))` (or the cleaned path if `Abs` fails). A hit requires an entry whose `checkedAt` (wall clock at store time) is less than 15 s ago; it MUST return a copy of the cached workspace with `owner` replaced by the caller's owner and `observed_at` by the caller's `observedAt.UTC()`. Expired entries are ignored and overwritten on the next store. Failed inspections (directory fallbacks) are cached too. The cache has no size bound and no eviction.
- **Source:** `go/internal/workspace/inspect.go` (Inspect, cacheKey, cached, remember)
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorCacheHitOverridesOwnerAndObservedAt; expiry and unbounded growth UNTESTED

#### WS-004 Default result
- **Behavior:** On a miss the result MUST start as `{path: filepath.Clean(path) (not absolutized), kind: "directory", owner, observed_at: observedAt.UTC()}` with all Git fields empty.
- **Source:** `go/internal/workspace/inspect.go` (Inspect)
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorFallsBackToDirectory

#### WS-005 Rev-parse command
- **Behavior:** The inspector MUST run, with a 750 ms timeout derived from the caller's context, exactly `git -C <path> rev-parse --path-format=absolute --show-toplevel --git-common-dir --git-dir HEAD`, capturing stdout (stderr captured and discarded). A non-zero exit, timeout, or missing binary MUST yield the cached directory default. The output MUST be split into trimmed non-empty lines; anything other than exactly 4 lines yields the directory default.
- **Source:** `go/internal/workspace/inspect.go` (Inspect, run, nonEmptyLines)
- **Decision:** D-027
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree

#### WS-006 Git fields
- **Behavior:** From the 4 lines: `root_path = Clean(line0)`, `common_git_dir = Clean(line1)`, git dir = `Clean(line2)` (not stored), `head_sha = line3` (full object name).
- **Source:** `go/internal/workspace/inspect.go` (Inspect)
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree

#### WS-007 Classification
- **Behavior:** `kind` MUST be `main_checkout` when `samePath(common_git_dir, git_dir)` and `worktree` otherwise. `samePath` makes both absolute, resolves symlinks where possible (`EvalSymlinks`; on error the unresolved absolute path is used), and compares cleaned paths; if either `Abs` fails it compares the raw strings.

  | Situation | rev-parse result | `kind` |
  |---|---|---|
  | blank path | not run | (nil workspace) |
  | not a Git directory, missing dir, no `git`, timeout | non-zero / killed | `directory` |
  | repo with no commits (`HEAD` unresolvable) | non-zero | `directory` |
  | bare repo or inside `.git` (no work tree) | non-zero | `directory` |
  | Git older than 2.31 (no `--path-format`) | non-zero | `directory` |
  | primary work tree (common dir == git dir) | 4 lines | `main_checkout` |
  | linked worktree (`git worktree add`) | 4 lines | `worktree` |
  | `unknown` | — | never produced |
- **Source:** `go/internal/workspace/inspect.go` (Inspect, samePath; KindDirectory, KindMainCheckout, KindWorktree)
- **Decision:** D-027
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree, TestInspectorFallsBackToDirectory
- **Consumer note:** `kind` is a plain string in the API (`go/pkg/api/v1/types.go`); the three values are the constants `workspace.KindDirectory`, `workspace.KindMainCheckout` and `workspace.KindWorktree`. A linked worktree is always reported as `worktree`.

#### WS-008 Branch
- **Behavior:** After a successful classification the inspector MUST run, with its own 750 ms timeout, `git -C <path> symbolic-ref --short -q HEAD`; on success `branch = TrimSpace(stdout)`, on any failure (detached HEAD, timeout) `branch` stays empty and the result is still a Git classification. Worst-case inspection time is therefore ~1.5 s.
- **Source:** `go/internal/workspace/inspect.go` (Inspect)
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree

#### WS-009 Never fails, never mutates
- **Behavior:** `Inspect` MUST NOT return an error or panic for any Git failure; it only runs the two read-only commands above and MUST NOT create, remove, or switch worktrees/branches. It MUST NOT set `id` or `repository_id`.
- **Source:** `go/internal/workspace/inspect.go` (Inspector, Inspect)
- **Decision:** D-004, D-027
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorFallsBackToDirectory

#### WS-010 Concurrency
- **Behavior:** Cache reads and writes MUST be mutex-protected; Git commands run outside the lock, so concurrent misses for the same path may each run Git (last write wins). One inspector is shared by the hook processor and all reconcilers (SRV-008).
- **Source:** `go/internal/workspace/inspect.go` (cached, remember)
- **Verified by:** UNTESTED

#### WS-011 Reconciler overrides observed_at
- **Behavior:** After inspection the reconciler MUST set the stored workspace's `observed_at` to the pass `now`, regardless of a cache hit.
- **Source:** `go/internal/reconcile/reconciler.go` (materialize)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness

---

## 04 · Design choices (local)

- **Startup discovery before the socket.** An available snapshot adapter performs one synchronous reconciliation before `Listen`, so the first API response is already reconciled and the health tracker has a startup result. Cost: probes and startup discovery delay readiness, and a second instance writes before detecting the first (SRV-011).
- **No immediate tick in discovery loops; immediate sweep in retention.** Startup already reconciled; retention sweeps on start so that a short-lived server still bounds the registry.
- **Retention is slow on purpose.** Nothing depends on pruning being prompt, and the 14-day window lets a conversation closed by accident stay recoverable for more than a working week.
- **`user_name` in its own column.** Provider reconciliation replaces the snapshot and so cannot erase a user's label; the one exception is a provider rename (DB-016).
- **Team, lead and parent in their own columns too.** The same reasoning, without the exception: nothing a provider reports changes them (DB-027). `team_lead` is a flag on the member's row, not a pointer from a team record, because a team has no record of its own.
- **Bucket recomputed on read, display state not.** A row that is never upserted again still gets the bucket the current rules give it (DB-019).
- **Placeholder names not carried forward.** A stored name that only repeats a native ID is not a provider name; carrying it would make a later real name look like a rename (REC-011).
- **Workspace failure is a plain directory.** Workspace metadata must never make agent discovery fail; failures are cached for the same 15 s.
- **Process signal is negative-only and fails open.** `IsRunning` returns true on `EPERM` and unknown errors, because claiming death on an unknown error would retire a live agent. It is applied once per row: a row that is already `not_live` is not touched again (REC-025).
- **Event IDs are content-derived.** SHA-256 of identity, transition and timestamp makes snapshot events idempotent under `ON CONFLICT DO NOTHING`.
- **Single SQLite connection.** `SetMaxOpenConns(1)` avoids in-process lock contention and keeps per-connection pragmas effective; WAL plus `busy_timeout` handles the separate hook-ingest processes.
- **Health `status` is always `ok`.** Server reachability is separated from provider health (D-032), which lives per provider.

## 04 · Verification procedures

### Automated

```sh
cd go
go test -count=1 ./internal/server/... ./internal/reconcile/... ./internal/storage/... ./internal/workspace/...
go test -count=1 ./internal/registry/... ./internal/config/...          # parity and path helpers
go test -list . ./internal/server/ ./internal/reconcile/ ./internal/storage/sqlite/ ./internal/workspace/
```

Expected: `ok` for every package. `go test -list` prints the test functions
named in this section's `Verified by` lines. The workspace test skips if `git`
is not installed. The Unix socket path must stay under ~104 bytes on macOS,
which is why `go/internal/server/run_test.go` uses `/tmp`.

### Manual — isolated environment (shared setup)

Never point these at real paths. Every variable below overrides
`config.ResolvePaths` (`go/internal/config/paths.go`) or the provider
executables, so nothing touches the user's registry, `~/.claude`, `~/.codex`,
or real provider runtimes. Do not run `noc setup`.

```sh
T=$(mktemp -d /tmp/noc-spec-XXXX)          # short path: socket length limit
mkdir -p "$T/bin" "$T/ws"
(cd go && go build -o "$T/bin/noc" ./cmd/noc)

cat > "$T/bin/fake-claude" <<'EOF'
#!/bin/sh
case "$1 $2" in
  "--version "*)  echo "fake-claude 0.0.0"; exit 0 ;;
  "agents --help") exit 0 ;;
  "agents --json") cat "$FAKE_CLAUDE_ROSTER" 2>/dev/null || echo '[]'; exit 0 ;;
esac
exit 1
EOF
printf '#!/bin/sh\nexit 1\n' > "$T/bin/fake-codex"     # Codex probe fails → unavailable
chmod +x "$T/bin/fake-claude" "$T/bin/fake-codex"

export NOC_STATE_DIR="$T/state" NOC_RUNTIME_DIR="$T/run" \
       NOC_SOCKET="$T/run/noc.sock" NOC_DATABASE="$T/state/noc.db" \
       NOC_CLAUDE_BIN="$T/bin/fake-claude" NOC_CODEX_BIN="$T/bin/fake-codex" \
       NOC_CODEX_SOCKET="$T/run/codex.sock" FAKE_CLAUDE_ROSTER="$T/roster.json"
```

Clean up afterwards with `trash "$T"` (or remove the directory yourself).

Rules for every procedure below:

- Pass `--discovery=false` or rely on the fake `NOC_CLAUDE_BIN`/`NOC_CODEX_BIN`
  exports on **every** server start. Do not use `noc server restart` without
  re-passing every flag: a restart without flags comes back with discovery
  enabled even if the original server had `--discovery=false` (LIFE-020 in
  [03-cli.md](03-cli.md)). Prefer
  `kill -TERM` and a fresh `noc server ...` invocation in these checks.
- Inspect SQLite read-only. While the server is **running**, use
  `sqlite3 -readonly "$T/state/noc.db" "<sql>"`. Once the server is
  **stopped**, the clean close has checkpointed and deleted `noc.db-wal` and
  `noc.db-shm`, and `sqlite3 -readonly` fails with
  `unable to open database file (14)`. Use the immutable URI form instead, which
  writes nothing: `sqlite3 "file:$T/state/noc.db?immutable=1" "<sql>"`. In
  that mode `PRAGMA journal_mode;` reports `delete`, not `wal`. After an unclean
  stop (`kill -9`) a leftover WAL may hold rows that immutable mode does not
  read.
- Write SQL only against the real DDL (DB-008..DB-010, DB-026). `agents` has
  only `id`, `host_id`, `provider`, `native_session_id`, `live_state`,
  `display_state`, `last_seen_at`, `snapshot_json`, `user_name`, `team`,
  `team_lead`, and `parent_id`. Every other agent field
  (`origin`, `name`, `ended_at`, `process_id`, …) is read with
  `json_extract(snapshot_json, '$.<field>')`.

### M1 — discovery disabled

1. `"$T/bin/noc" server --discovery=false > "$T/server.log" 2>&1 & echo $! > "$T/server.pidbg"`
2. Wait for the socket: `until [ -S "$T/run/noc.sock" ]; do sleep 0.1; done`
3. `"$T/bin/noc" server status --json` → `status: "ok"`, a UUID `host_id`, and two providers (`claude`, `codex` in that order) each with `status`, `startup_status` = `disabled`, `detail`/`startup_detail` = `provider discovery disabled`, `observed_at` = `started_at` (SRV-005..007, SRV-027).
4. `"$T/bin/noc" ls --json` → `"agents": []`.
5. `ls -l "$T/run" "$T/state"` → `noc.sock` is `srw-------`, `noc.pid` `-rw-------`, `noc.db` `-rw-------`, `noc.db-wal` and `noc.db-shm` `-rw-r--r--` (umask `022`), directories `drwx------` (DB-002, DB-005, SRV-011).
6. `kill -TERM $(cat "$T/server.pidbg")`; within ~1 s `$T/run` is empty, and `$T/state` holds only `noc.db` because the sidecars were removed on close (SRV-023, SRV-025, DB-005).

### M2 — fake Claude roster: discovery, staleness, revival, rename

1. Write one background session (synthetic IDs only):
   ```sh
   cat > "$T/roster.json" <<EOF
   [{"id":"fake01","sessionId":"00000000-0000-4000-8000-000000000001","kind":"background",
     "cwd":"$T/ws","name":"spec-check","state":"working","pid":4242}]
   EOF
   ```
2. `"$T/bin/noc" server --discovery-interval 1s --stale-after 3s > "$T/server.log" 2>&1 & echo $! > "$T/server.pidbg"`; wait for the socket.
3. `"$T/bin/noc" server status --json` → `claude`: `status`/`startup_status` `available`, `version` `fake-claude 0.0.0`, `discovery_source` `claude agents --json`, `detail` `discovery active; observed 1 live sessions`; `codex`: `unavailable` with a detail naming the fake binary's exit status. `"$T/bin/noc" ls --json` → one agent: `origin: "reconcile"`, `live_state: "live"`, `runtime_state: "running"`, `activity_state: "working"`, `display_state: "working"`, `bucket: "working"`, `last_activity_at == last_seen_at == snapshot_confirmed_at == first_seen_at`, `workspace.kind: "directory"`, `workspace.owner: "unknown"` (REC-003, REC-010, REC-014, WS-004). `grep -c 'noc server listening' "$T/server.log"` → `1`.
4. `echo '[]' > "$T/roster.json"; sleep 1.5; "$T/bin/noc" ls --all --json` → `live_state: "candidate_stale"` (REC-020); log contains `provider registry reconciled ... candidate_stale=1`.
5. `sleep 4; "$T/bin/noc" ls --all --json` → `live_state: "not_live"`, `ended_at` set, `display_state: "working"`, `bucket: "quiet"` (REC-021, REC-027); `"$T/bin/noc" ls --json` (no `--all`) → empty.
6. Restore the roster from step 1 with `"state":"blocked"`; `sleep 1.5`. `ls --json` shows the same `id`, `live_state: "live"`, no `ended_at`, `display_state: "ready"`. `"$T/bin/noc" events --json` lists, in order, `agent.discovered` (`reason: observed_in_provider_snapshot`), `agent.candidate_stale` (`previous_live_state: live`), `agent.not_live` (`missing_from_snapshot_past_stale_grace`), `agent.revived` (`previous_live_state: not_live`), all `source: "snapshot"` with ids matching `^snapshot-[0-9a-f]{64}$` (REC-019, REC-028).
7. Rename overlay: `ID=$(sqlite3 -readonly "$T/state/noc.db" "SELECT id FROM agents")`; `"$T/bin/noc" rename "$ID" "my label"`; then `sqlite3 -readonly "$T/state/noc.db" "SELECT user_name, json_extract(snapshot_json,'\$.name') FROM agents"` → `my label|spec-check` after the next poll (nameless/same-name snapshots keep it). Change `"spec-check"` to `"renamed-in-session"` in the roster, `sleep 1.5`, re-run → `|renamed-in-session` (DB-016, REC-012).
8. `kill -TERM $(cat "$T/server.pidbg")`; `$T/run` becomes empty.

### M3 — read-only SQLite inspection

With the server stopped (after M2 step 8), use the immutable URI. While a server
is running, replace the first line with `sqlite3 -readonly "$T/state/noc.db" "`
(WAL readers do not block the writer):

```sh
sqlite3 "file:$T/state/noc.db?immutable=1" "
  PRAGMA journal_mode;
  SELECT version, applied_at FROM schema_migrations ORDER BY version;
  SELECT key FROM metadata;
  SELECT name FROM sqlite_master WHERE type='index' ORDER BY name;
  SELECT id, live_state, display_state, last_seen_at, quote(user_name),
         typeof(snapshot_json), json_extract(snapshot_json,'\$.user_name')
    FROM agents;
  SELECT event_type, source, json(payload_json) FROM observations ORDER BY received_at;
  SELECT sql FROM sqlite_master WHERE name IN ('agents','observations','hook_outbox','metadata');"
```

Expected: `delete` with the immutable URI (`wal` with `-readonly` against a running server); versions `1|…`, `2|…`, `3|…`, `4|…` with `YYYY-MM-DDTHH:MM:SS.sssZ`;
`host_id`; the ten index names listed under DB-026; `snapshot_json` type `blob`
and `$.user_name` NULL; observation payloads such as
`{"native_event":"","reason":"observed_in_provider_snapshot","live_state":"live"}`;
table DDL matching DB-008 with `user_name TEXT NOT NULL DEFAULT ''`,
`team TEXT NOT NULL DEFAULT ''`, `team_lead INTEGER NOT NULL DEFAULT 0` and
`parent_id TEXT NOT NULL DEFAULT ''` appended to `agents`. Never open the database read-write while verifying.

### M5 — discovery disabled freezes liveness (SRV-020)

1. Write the step-1 roster from M2. Start `"$T/bin/noc" server --discovery-interval 1s --stale-after 3s > "$T/s1.log" 2>&1 &`, wait for the socket, then `kill -TERM` it.
2. `echo '[]' > "$T/roster.json"`, which would age the session if discovery ran.
3. Start a fresh server with `--discovery=false` (not `server restart`): `"$T/bin/noc" server --discovery=false > "$T/s2.log" 2>&1 &`. Wait for the socket, then wait longer than the grace (`sleep 6`).
4. `"$T/bin/noc" ls --json` → the agent is still `live_state: "live"` (`display_state: "working"`). `server status --json` shows both providers `disabled`.
5. `kill -TERM` the server, then `sqlite3 "file:$T/state/noc.db?immutable=1" "SELECT live_state, json_extract(snapshot_json,'\$.origin') FROM agents; SELECT count(*) FROM observations;"` → `live|reconcile` and `1`. Only the original `agent.discovered` event exists; no `candidate_stale` or `not_live` was recorded.

### M4 — workspace classification (no server needed)

Repeat M2 with `"cwd"` pointing at a scratch repository created under `$T`
(`git init "$T/repo"`, commit one file, `git -C "$T/repo" worktree add -b linked "$T/wt"`):
the agent's `workspace.kind` is `main_checkout` for `$T/repo` (with `root_path`,
`common_git_dir`, `head_sha`, `branch`), `worktree` for `$T/wt` (`branch:
"linked"`), and `directory` for `$T/ws`. A freshly `git init`-ed repository with no
commits stays `directory` (WS-007). Changes are visible within one poll plus the
15 s cache TTL.

## 04 · Coverage gaps

- No test checks the workspace `kind` strings Go emits (WS-007) against a
  frontend's parser; each side tests its own half.
- No automated test of `Run` shutdown cleanup (socket removal), of the 5 s
  shutdown with open watch streams (SRV-024), or of the hook drain loop and wake
  coalescing (SRV-016, SRV-017). Interval and retention defaults (SRV-001) are
  untested.
- No test that an adapter failing its startup probe is never retried (SRV-009).
- Reconciler: `candidate_stale → live` revival and the event ID format
  (REC-028) have no automated test.
- Storage: no test for pragmas (DB-004), migration from an older schema version
  (DB-006), including migration 4 over existing rows (DB-026), WAL side-file
  permissions (DB-005), or SQL `TRIM` vs Go `TrimSpace` parity (DB-023).
- Teams: no reconciler-level test that a snapshot pass or the aging pass
  leaves `team`, `lead` and `parent_id` alone (REC-030); the storage test
  covers the upsert rule they rely on.
- `last_seen_at` is stored with variable fractional digits (RFC3339Nano), so
  the lexical `last_seen_at < ?` comparison in `PruneRetired` can misorder two
  instants within the same second (e.g. `…:35Z` sorts after `…:35.5Z`); the
  outbox/observation tables use a fixed 9-digit layout instead. Untested.
- Concurrency: reconciler/hook-processor read-modify-write races (DB-025) are
  not covered; a lost update can drop hook-set summary, bindings, or process ID.
- Workspace: cache expiry, blank-path nil, timeouts, and repositories without
  commits are untested (WS-002, WS-003, WS-007).
- WAL sidecar permissions (`0644`, DB-005) and the frozen liveness under
  `--discovery=false` (SRV-020) have only manual checks (M1 step 5, M5).
- Growth of `observations` and `hook_outbox` (DB-024) is neither measured nor
  limited by any test.

## Known limitations

- A row that a spawn registration created and that no hook or snapshot ever
  confirms is never retired. It is hook-only with `process_id == 0`, so
  neither absence from a snapshot (REC-023) nor the process signal (REC-024)
  applies, and retention prunes only `not_live` rows (DB-022). It stays, as a
  quiet agent with `runtime_state` `starting`, until `noc forget`
  ([02-api.md](02-api.md) HTTP-036; `go/internal/reconcile/reconciler.go`
  RunOnce).
- Being in a team does not protect a row from retention, and `parent_id` is
  not a foreign key: when a parent is forgotten or pruned, its children keep
  an id that names no agent, and a team whose last member is pruned ceases to
  exist (DB-022, DB-026; `go/internal/storage/sqlite/store.go` PruneRetired).
- A provider whose startup probe fails gets no discovery worker and is never
  probed again; it recovers only when the server is restarted. Failures of
  `DiscoverLive` after a successful probe do recover on a later poll (SRV-009;
  `go/internal/server/run.go` Run).
- With `--discovery=false` there are no reconcilers, so rows keep their stored
  `live_state` indefinitely and health reports only `disabled` (SRV-020;
  `go/internal/server/run.go` Run).
- Probes and startup discovery run sequentially before the socket exists, so
  one slow provider delays readiness for everything (SRV-007, SRV-010;
  `go/internal/server/run.go` Run).
- A second server on the same database reconciles once, and writes the result,
  before it is refused at the socket; the CLI's PID record normally prevents
  reaching this point (SRV-011; `go/internal/server/run.go` Run).
- An open `watch` stream is not cancelled by shutdown: the server takes the
  full 5 s and returns `context deadline exceeded` (SRV-024;
  `go/internal/server/run.go` Run).
- There is no ordering or compare-and-set between a provider poll and a hook
  update. The reconciler overwrites runtime state, activity and attention
  unconditionally, and its read-modify-write is not atomic, so a hook
  observation committed in between can be lost until the next hook (REC-010,
  DB-025; `go/internal/reconcile/reconciler.go` RunOnce,
  `go/internal/storage/sqlite/store.go` Upsert).
- A failed provider query leaves every row unchanged; the `unknown` live state
  is never assigned (REC-004; `go/internal/reconcile/reconciler.go` RunOnce).
- Identity is not retained once a row is gone. Retention and `forget` delete
  the only mapping from a native session to its noc ID, so a session that
  comes back afterwards gets a new UUID (DB-021, DB-022;
  `go/internal/storage/sqlite/store.go` Forget, PruneRetired).
- Retention age is measured from `last_seen_at` (the last sighting), not from
  `last_activity_at` or `ended_at` (DB-022; `go/internal/storage/sqlite/store.go`
  PruneRetired, `go/internal/registry/registry.go` retired).
- The prune comparison is lexical on RFC3339Nano strings with variable
  fraction digits, so two instants within the same second can misorder
  (DB-022; `go/internal/storage/sqlite/store.go` PruneRetired).
- The SQLite prune uses SQL `TRIM` (spaces only) where the memory registry uses
  `strings.TrimSpace` (all whitespace), so a name made only of tabs or
  newlines counts as blank in memory but not in SQLite (DB-023;
  `go/internal/storage/sqlite/store.go` PruneRetired,
  `go/internal/registry/registry.go` retired).
- Nothing ever deletes from `observations` or `hook_outbox`; both grow for the
  life of the database file (DB-024; `go/internal/storage/sqlite/hooks.go`).
- Only `noc.db` is made `0600`. The `-wal` and `-shm` side files follow the
  umask and are private only when noc created the parent directory `0700`
  (DB-005; `go/internal/storage/sqlite/store.go` Open).
- A database written by a newer schema version is opened without complaint
  (DB-006; `go/internal/storage/sqlite/migrations.go` migrate).
- `IsRunning` reports any `pid <= 1` as not running (REC-024;
  `go/internal/proclineage/proclineage.go` IsRunning).
- The workspace cache has no size bound or eviction (WS-003;
  `go/internal/workspace/inspect.go` remember).
- Agent IDs are random UUIDv4 and therefore not sortable by creation time
  (REC-003; `go/internal/identity/uuid.go` NewUUID).
