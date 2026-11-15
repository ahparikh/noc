# 05 — Provider adapters

This section specifies the provider adapter contract, the bounded command
runner shared by adapters, and the two discovery adapters: Claude Code
(`claude agents --json` roster snapshots) and Codex (the shared app-server
daemon's Unix control socket, spoken as WebSocket + JSON-RPC, with a
`codex app-server proxy` compatibility fallback). It covers executable and
socket resolution, probing, discovery argv and protocol exchanges, field
decoding and sanitizing, row acceptance and rejection, state normalization,
preview/metadata bounding, error text, timeouts, and the Codex
`thread/name/set` rename push. Consumers of the contract (server worker
gating, provider health, reconciliation, rename HTTP handler) are referenced
only where they define behavior observable at the adapter boundary; their full
behavior belongs to other sections.

Source files covered:

- `go/internal/provider/provider.go`
- `go/internal/provider/command.go`, `go/internal/provider/command_test.go`
- `go/internal/provider/claude/adapter.go`, `adapter_test.go`, `testdata/agents.json`
- `go/internal/provider/codex/adapter.go`, `adapter_test.go`
- `go/internal/provider/codex/socket.go`, `socket_test.go`
- `go/internal/provider/codex/websocket.go`, `websocket_test.go`
- `go/internal/provider/codex/proxy.go`, `proxy_test.go`
- `go/internal/provider/codex/testdata/discovery.jsonl`

Neighbors read for boundary behavior: `go/internal/server/run.go`,
`go/internal/server/health.go`, `go/internal/server/discovery.go`,
`go/internal/reconcile/reconciler.go`, `go/internal/localapi/server.go`,
`go/pkg/api/v1/types.go`.

Conventions: "trimmed" means Go `strings.TrimSpace` (Unicode whitespace at
both ends). "Clean" means Go `filepath.Clean`. "Runes" means Unicode code
points. Error strings are given exactly; `<err>` marks a wrapped error's text
and `%q` marks Go double-quoted formatting.

---

## 1. Adapter contract

### 1.1 Interfaces and types

#### PRV-001 Adapter interface
- **Behavior:** Every provider adapter MUST implement exactly four methods: `Provider() apiv1.Provider` (returns the constant provider enum, `"claude"` or `"codex"`); `Probe(ctx) ProbeResult` (never returns an error; failures are encoded in the result); `DiscoverLive(ctx) ([]NativeAgent, error)` (one snapshot of live, local, top-level conversations, already filtered and sanitized); `Normalize(NativeAgent) (NormalizedAgent, error)` (pure mapping of one native row; MUST NOT perform I/O other than reading the adapter's clock). Both concrete adapters assert conformance at compile time.
- **Source:** `go/internal/provider/provider.go` (Adapter); `go/internal/provider/claude/adapter.go`; `go/internal/provider/codex/adapter.go`
- **Decision:** D-023, D-024
- **Verified by:** compile-time assertions `var _ provider.Adapter = (*Adapter)(nil)` in both adapter files.

#### PRV-002 ProbeResult
- **Behavior:** `ProbeResult` MUST carry `Provider`, `Available bool`, `Version string` (provider-reported, trimmed), `Features map[string]bool`, `Diagnostic string` (human-readable failure reason; empty on success), `ObservedAt time.Time` (UTC, captured *before* the probe commands run). Feature keys currently emitted: `discover_live` (both adapters) and `direct_unix_socket` (Codex only). On failure `Features` MUST be nil/empty. A consumer MUST treat a provider as discoverable only when `Available && Features["discover_live"]`.
- **Source:** `go/internal/provider/provider.go` (ProbeResult); `go/internal/server/run.go`
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestProbe; `go/internal/provider/codex/adapter_test.go` TestProbe

#### PRV-003 NativeAgent
- **Behavior:** `NativeAgent` is the adapter-internal, already-sanitized row returned by `DiscoverLive`: `NativeSessionID` (durable provider identity, trimmed, non-empty), `NativeShortID` (Claude attach alias; empty for Codex), `Name` (provider name with placeholders removed; may be empty), `SourceKind` (provider-native kind string), `CWD` (trimmed; Claude also Cleans it), `Raw json.RawMessage` (an adapter-defined, allow-listed evidence object — never the provider payload). `Raw` MUST NOT contain prompts, transcripts, transcript paths, PIDs, or unknown provider fields.
- **Source:** `go/internal/provider/provider.go` (NativeAgent)
- **Decision:** D-023, D-024, D-027
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes; `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes

#### PRV-004 NormalizedAgent
- **Behavior:** `NormalizedAgent` MUST contain provider evidence with provider-specific enums removed: `Provider`, `NativeSessionID`, `NativeShortID`, `Name`, `SourceKind`, `LiveState`, `RuntimeState`, `ActivityState`, `Attention{Type,Detail}`, `Model`, `ModelSource`, `ReasoningEffort`, `ReasoningSource`, `InitialRequest *TextPreview`, `Summary`, `CreatedAt *time.Time`, `LastActivityAt *time.Time`, `Capabilities map[string]Capability`, `NativeState json.RawMessage`, `Workspace *Workspace`. It MUST NOT contain a noc agent ID or host ID (those belong to reconciliation/registry). Enumerations used: RuntimeState `starting|running|loaded|resumable|stopped|failed|unknown`; ActivityState `working|waiting|idle|unknown`; AttentionType `none|input_required|approval_required|authentication_required|error|unknown`; LiveState `live` (the only value adapters emit).
- **Source:** `go/internal/provider/provider.go` (NormalizedAgent); `go/pkg/api/v1/types.go`
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings; `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings

#### PRV-005 Optional Renamer and ErrNotImplemented
- **Behavior:** An adapter MAY implement `Renamer{ SetName(ctx, nativeSessionID, name string) error }`. Only the Codex adapter implements it; the Claude adapter MUST NOT (Claude names are a noc overlay). `ErrNotImplemented` MUST have the exact text `provider adapter not implemented`. An adapter that implements `Renamer` but cannot rename in its current configuration MUST return an error satisfying `errors.Is(err, ErrNotImplemented)`.
- **Source:** `go/internal/provider/provider.go` (Renamer, ErrNotImplemented)
- **Decision:** D-042
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestAdapterWithoutRenamingClientReportsNotImplemented

#### PRV-006 Watcher contract is declared but unused
- **Behavior:** `Watcher{ Watch(ctx, ObservationSink) error }`, `ObservationSink{ Observe(ctx, Observation) error }`, and `Observation{Provider, NativeSessionID, EventType, OccurredAt, Agent NormalizedAgent}` exist as an optional streaming capability. No adapter implements `Watcher`; discovery is snapshot-only. A consumer MUST NOT require `Watcher` of a snapshot adapter.
- **Source:** `go/internal/provider/provider.go` (Watcher, ObservationSink, Observation)
- **Decision:** D-007
- **Verified by:** UNTESTED

### 1.2 Contract consumers (boundary behavior)

#### PRV-007 Probing happens once, at server start; failed probes are never retried
- **Behavior:** The server MUST construct `claude.New()` then `codex.New()` (in that order) unless adapters are injected, probe each sequentially at startup, and create a discovery worker only for adapters whose probe has `Available && Features["discover_live"]`. Adapters MUST NOT be re-probed later in the process lifetime: a provider whose probe failed at startup (missing executable, probe timeout, missing `agents`/`app-server` subcommand) stays undiscovered until the server restarts. A provider whose probe succeeded but whose snapshots fail (e.g. Codex daemon absent) keeps its worker and retries every poll. When discovery is disabled, the server MUST synthesize `ProbeResult{Available:false, Diagnostic:"provider discovery disabled"}` instead of probing.
- **Source:** `go/internal/server/run.go` (Run)
- **Decision:** D-024, D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery (available path only); never-reprobe behavior UNTESTED

#### PRV-008 Probe-to-health mapping and discovery source strings
- **Behavior:** Initial health status per provider MUST be: `disabled` when `Diagnostic == "provider discovery disabled"`; else `not_implemented` when `Diagnostic == "provider adapter not implemented"`; else `available` when `Available`; else `unavailable`. `Detail` = `Diagnostic`; `Version` = probe version. `DiscoverySource` MUST be exactly `claude agents --json` for Claude and `Codex app-server loaded-thread snapshot` for Codex (`provider adapter` otherwise). For an unavailable/disabled probe the probe is also the immutable startup result.
- **Source:** `go/internal/server/health.go` (initialStatus, providerDiscoverySource)
- **Decision:** D-032
- **Verified by:** `go/internal/server/health_test.go` TestProviderHealthPreservesFirstDiscoveryResult; `go/internal/server/health_test.go` TestProviderHealthUsesUnavailableProbeAsStartupResult

#### PRV-009 Snapshot outcome reporting
- **Behavior:** After each snapshot, health MUST be updated to `degraded` with detail `discover <provider> agents: <DiscoverLive error>` when `DiscoverLive` fails, else `available` with detail `discovery active; observed <N> live sessions` plus `; skipped <M> incompatible sessions` when M > 0. `N` is the number of rows `DiscoverLive` returned (before `Normalize`), so it counts rows later skipped. A failed snapshot MUST NOT age any agent (no rows are marked stale).
- **Source:** `go/internal/server/discovery.go` (reconcileOnce); `go/internal/reconcile/reconciler.go` (RunOnce)
- **Decision:** D-032
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestDiscoveryFailureDoesNotAgeAgents; `go/internal/server/run_test.go` TestRunServesInitialDiscovery (`available` detail text); `degraded` and `skipped` detail text UNTESTED

#### PRV-010 Rows the reconciler skips
- **Behavior:** For every row returned by `DiscoverLive`, the reconciler MUST call `Normalize` and count the row as skipped (not an error) if `Normalize` errors, if the normalized `NativeSessionID` is empty, if the normalized `Provider` differs from `adapter.Provider()`, or if the same `NativeSessionID` was already accepted in this snapshot (first occurrence wins).
- **Source:** `go/internal/reconcile/reconciler.go`
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerSkipsDuplicatesAndIncompatibleRows

#### PRV-011 Rename push uses Renamer with a 5-second budget
- **Behavior:** The server MUST register every adapter that type-asserts to `Renamer`, keyed by provider. On a rename with a non-empty name, the API handler MUST call `SetName(ctx with 5 s timeout, agent.NativeSessionID, name)` and report `Noc-Provider-Name-Sync`: `not_attempted` for an empty (clearing) name; `unsupported` if no renamer is registered for the provider (Claude) or `SetName` returns `ErrNotImplemented`; `failed` (with an error header bounded to 300 runes after whitespace compaction, `…` appended when cut) on any other error; `synced` on success. The noc-side name is kept in every case.
- **Source:** `go/internal/server/run.go` (Run); `go/internal/localapi/server.go` (pushNameToProvider, boundedSyncError)
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt

---

## 2. Bounded command runner

`CommandRunner.Run(ctx, name, args...) ([]byte, error)`; production
implementation `ExecCommandRunner`, created by `NewExecCommandRunner()`.

| Limit | Value | Source |
|---|---|---|
| Captured stdout | 8 MiB (`8 << 20` = 8,388,608 bytes) | `command.go` |
| Captured stderr | 64 KiB (`64 << 10` = 65,536 bytes) | `command.go` |
| Command timeout | none in the runner; callers supply a context deadline | `command.go` |
| Wait delay after the context ends or the child exits (`waitDelay`) | 2 s | `command.go` |

#### PRV-012 argv execution without a shell
- **Behavior:** `Run` MUST execute `name` with `args` as a literal argv via `exec.CommandContext` — no shell, no word splitting, no globbing. `name` without a slash MUST be resolved through the server's `PATH`. The child MUST inherit noc's full environment and working directory unchanged (no env filtering or additions), and MUST get no stdin (null device). When the context ends, the child MUST be killed (SIGKILL, Go's `CommandContext` default). The command MUST set `cmd.WaitDelay` to 2 s (PRV-015).
- **Source:** `go/internal/provider/command.go` (Run)
- **Verified by:** `go/internal/provider/command_test.go` TestExecCommandRunnerClassifiesErrors (argv execution, `PATH` lookup failure, kill on deadline); environment, working directory, and stdin inheritance UNTESTED

#### PRV-013 Output caps and defaults
- **Behavior:** If `StdoutLimit <= 0` it MUST default to 8 MiB; if `StderrLimit <= 0`, to 64 KiB. Stdout and stderr MUST each be captured into a capped buffer that keeps the first `limit` bytes, silently discards the rest, records that truncation happened, and always reports the full write length to the writer (so the child never sees a short write or `EPIPE` because of the cap). A capped buffer with limit `<= 0` MUST fail writes with `buffer limit must be positive` (unreachable through `Run` because of the defaults).
- **Source:** `go/internal/provider/command.go` (Run, cappedBuffer.Write)
- **Verified by:** `go/internal/provider/command_test.go` TestCappedBuffer (buffer semantics); defaults UNTESTED

#### PRV-014 Error classification order
- **Behavior:** After the child exits, `Run` MUST classify in this order and return `nil` output on any error:
  1. If `ctx.Err() != nil` (deadline or cancellation, even if the child succeeded): `run provider command %q: <ctx err>` wrapping `context.DeadlineExceeded` / `context.Canceled` (e.g. `run provider command "claude": context deadline exceeded`).
  2. Else if start/exit failed and trimmed stderr (plus ` (truncated)` if stderr overflowed) is non-empty: `run provider command %q: <err>: <detail>` (e.g. `run provider command "/path/claude": exit status 3: no such subcommand`).
  3. Else if start/exit failed: `run provider command %q: <err>` (e.g. `run provider command "missing": exec: "missing": executable file not found in $PATH`).
  4. Else if stdout overflowed: `provider command %q exceeded the %d-byte output limit` (note: no `run ` prefix; `%d` is the effective stdout limit). Truncated stdout MUST NOT be returned as success.
  5. Else return a copy of the captured stdout.
  A wait-delay expiry without a context error (PRV-015) is an exit failure and falls under rule 2 or 3 with `<err>` = `exec: WaitDelay expired before I/O complete`.
  The runner does not otherwise classify errors (no retry, no not-found vs. permission distinction); callers surface `err.Error()` verbatim.
- **Source:** `go/internal/provider/command.go` (Run)
- **Verified by:** `go/internal/provider/command_test.go` TestExecCommandRunnerClassifiesErrors (success, rule 1 on deadline, rule 2, rule 3 for a missing executable, rule 4); stderr ` (truncated)` suffix and context error taking precedence over a successful exit UNTESTED

#### PRV-015 Inherited output pipes cannot outlive the timeout
- **Behavior:** The runner captures output through Go-managed pipes, so a process other than the direct child (a wrapper script's grandchild, a shim's helper) can keep stdout or stderr open after the child is gone. `Run` MUST set `cmd.WaitDelay = 2 s` (`waitDelay`) so this cannot block it indefinitely:
  - When the context ends, the direct child is killed. If the output pipes are still open 2 s later, they are closed and `Run` returns. `Run` therefore returns no later than the caller's deadline plus 2 s, with the context error (PRV-014 rule 1).
  - When the direct child exits on its own while another process still holds a pipe, `Run` waits at most 2 s after the exit and then returns `run provider command %q: exec: WaitDelay expired before I/O complete` (with the stderr detail appended when there is any). Output captured so far is discarded.
  The grandchild itself is not killed; only the pipes are closed.
- **Source:** `go/internal/provider/command.go` (waitDelay, Run)
- **Verified by:** `go/internal/provider/command_test.go` TestExecCommandRunnerClassifiesErrors (a 100 ms deadline on `sh -c "sleep 5"` returns `context.DeadlineExceeded` in under 4 s); the child-exits-first case UNTESTED; MANUAL: §8.2 M-C3

---

## 3. Executable, socket, and environment resolution

| Variable | Read by | Effect | Source |
|---|---|---|---|
| `NOC_CLAUDE_BIN` | Claude adapter (also CLI attach/stop, out of scope here) | Claude executable when no explicit option; trimmed; empty/whitespace ignored | `claude/adapter.go` |
| `NOC_CODEX_BIN` | Codex adapter (probe and proxy fallback) | Codex executable when no explicit option; trimmed | `codex/adapter.go` |
| `NOC_CODEX_SOCKET` | Codex adapter | Explicit control socket; trimmed then Cleaned; **disables proxy fallback** | `codex/adapter.go`; `codex/socket.go` |
| `CODEX_HOME` | Codex adapter | Base for the inferred socket; trimmed; default `$HOME/.codex` | `codex/adapter.go`; `codex/socket.go` |
| `HOME` (via `os.UserHomeDir`) | Codex adapter | Default `CODEX_HOME` parent; if unresolvable the socket path is empty | `codex/socket.go` |
| `PATH` | runner and proxy `exec` | Resolves bare `claude` / `codex` | `command.go`; `codex/proxy.go` |
| `NOC_CODEX_INTEGRATION` | Codex test only | `=1` enables `TestInstalledSharedDaemonDiscovery` | `codex/socket_test.go` |

All resolution happens once, at adapter construction (`New`/`NewWithOptions`);
changing the environment later has no effect on a running server.

#### PRV-016 Executable resolution order
- **Behavior:** Each adapter MUST resolve its executable as: (1) the explicit in-process option `Options.Executable`, trimmed, if non-empty; else (2) the provider env var (`NOC_CLAUDE_BIN` / `NOC_CODEX_BIN`), trimmed, if non-empty; else (3) the bare name `claude` / `codex` looked up on `PATH` at execution time. No existence or executability check happens at construction. A relative path containing `/` is relative to the server's working directory.
- **Source:** `go/internal/provider/claude/adapter.go` (resolveExecutable); `go/internal/provider/codex/adapter.go` (resolveExecutable)
- **Decision:** D-025
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNewUsesExecutableEnvironment; `go/internal/provider/claude/adapter_test.go` TestExplicitExecutableOverridesEnvironment; `go/internal/provider/codex/adapter_test.go` TestNewUsesExecutableEnvironment; `go/internal/provider/codex/adapter_test.go` TestExplicitExecutableOverridesEnvironment

---

## 4. Claude adapter

| Constant | Value | Source |
|---|---|---|
| Default command timeout (per command) | 5 s | `claude/adapter.go` |
| Default executable | `claude` | `claude/adapter.go` |
| Probe argv | `<exe> --version`, then `<exe> agents --help` | `claude/adapter.go` |
| Discovery argv | `<exe> agents --json` | `claude/adapter.go` |
| Feature keys on success | `discover_live: true` | `claude/adapter.go` |

### 4.1 Construction and probe

#### CLA-001 Construction defaults
- **Behavior:** `New()` MUST equal `NewWithOptions(Options{})`. Defaults: `Runner` → `provider.NewExecCommandRunner()`; executable per PRV-016; `CommandTimeout <= 0` → 5 s; `Now == nil` → `time.Now`. `Provider()` MUST return `claude`.
- **Source:** `go/internal/provider/claude/adapter.go` (New, NewWithOptions, Provider)
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNewUsesExecutableEnvironment (executable only); timeout default UNTESTED

#### CLA-002 Probe step 1: version
- **Behavior:** `Probe` MUST capture `ObservedAt = now().UTC()` first, then run `<exe> --version` under its own `CommandTimeout` context derived from the caller's context. On error it MUST return `{Provider: claude, Available: false, Diagnostic: err.Error(), ObservedAt}` with empty `Version` and no features, without running step 2.
- **Source:** `go/internal/provider/claude/adapter.go` (Probe)
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestProbeUnavailable

#### CLA-003 Probe step 2: roster capability
- **Behavior:** After a successful version command, `Probe` MUST run `<exe> agents --help` under a fresh `CommandTimeout` context (the two deadlines total 2 × timeout = 10 s by default; with the runner's wait delay, PRV-015, the probe returns within 14 s). Its output is ignored; only success matters. On error it MUST return `Available: false`, `Version: trimmed(version stdout)`, `Diagnostic: "Claude live discovery unavailable: <err>"`.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestProbeRequiresAgentRosterCapability

#### CLA-004 Probe success
- **Behavior:** On success `Probe` MUST return `Available: true`, `Version` = the entire trimmed `--version` stdout (e.g. `2.1.270 (Claude Code)`; no parsing, no version comparison), `Features: {"discover_live": true}`, empty `Diagnostic`.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestProbe

### 4.2 Discovery

#### CLA-005 Discovery command
- **Behavior:** `DiscoverLive` MUST run exactly `<exe> agents --json` (never `--all`) under one `CommandTimeout` context. A runner error MUST be returned unwrapped (so the reconciler's health detail reads e.g. `discover claude agents: run provider command "claude": context deadline exceeded`).
- **Source:** `go/internal/provider/claude/adapter.go` (DiscoverLive)
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (argv via fake runner key)

#### CLA-006 Top-level document must be a JSON array
- **Behavior:** Stdout MUST decode as a JSON array of arbitrary values; otherwise return `decode Claude agent roster: <json err>` (this includes empty stdout: `unexpected end of JSON input`). A literal `null` decodes to an empty roster with no error. `[]` yields zero agents and no error.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Verified by:** UNTESTED

#### CLA-007 Per-row decoding is independent; incompatible rows are dropped silently
- **Behavior:** Each array element MUST be decoded separately into the roster schema (CLA-008). If decoding fails (element not an object, or any decoded field has the wrong JSON type — e.g. `"kind": 42`, `"pid": "123"`, a fractional `startedAt`), that row MUST be skipped without error, log, or counter, and the remaining rows processed. Go `encoding/json` semantics apply: key matching is case-insensitive and the last duplicate key wins.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (fixture row with `"kind": 42`)

#### CLA-008 Decoded roster fields; everything else is discarded
- **Behavior:** Only these keys MUST be decoded; all others (e.g. `prompt`, `transcript`, any future field) MUST be discarded before anything is retained:

  | Roster key | Go type | Use |
  |---|---|---|
  | `id` | string | short attach alias → `NativeShortID` (trimmed); also placeholder check |
  | `cwd` | string | workspace path (trimmed, Cleaned) |
  | `kind` | string | acceptance filter → `SourceKind` |
  | `startedAt` | int64 (epoch ms) | evidence `started_at` → `CreatedAt` |
  | `sessionId` | string | durable identity → `NativeSessionID` (trimmed) |
  | `name` | string | `Name` after placeholder removal |
  | `state` | string | evidence `state` (untrimmed) |
  | `pid` | *int | reduced to boolean `process_alive = pid != null && pid > 0`; the PID value is discarded |
  | `status` | string | evidence `status` (untrimmed) |
  | `waitingFor` | string | evidence `waiting_for` (untrimmed) |
- **Source:** `go/internal/provider/claude/adapter.go` (rosterEntry)
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes

#### CLA-009 Kind acceptance
- **Behavior:** A row MUST be accepted only if `kind` is exactly (case-sensitive, untrimmed) `interactive` or `background`. Every other value — `cloud`, empty/missing, any other or future kind — MUST be dropped. This is the only cloud/remote exclusion in the adapter.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-011, D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (fixture `cloud` row)

#### CLA-010 Full session ID required
- **Behavior:** A row whose trimmed `sessionId` is empty or missing MUST be dropped (it is retried naturally by the next snapshot). The short `id` MUST NOT be used as identity.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (fixture `no-session-id` row)

#### CLA-011 Sanitized evidence (`NativeAgent.Raw`)
- **Behavior:** `Raw` MUST be the JSON encoding of exactly, in this key order: `state` (omitted if empty), `status` (omitted if empty), `waiting_for` (omitted if empty), `started_at` (omitted if 0), `process_alive` (always present, boolean). A marshal failure MUST abort the snapshot with `encode sanitized Claude state: <err>` (unreachable in practice).
- **Source:** `go/internal/provider/claude/adapter.go` (evidence, DiscoverLive)
- **Decision:** D-023
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (absence of `prompt`/`transcript`)

#### CLA-012 Short ID, cwd, kind, ordering
- **Behavior:** `NativeShortID` MUST be the trimmed `id` (empty for interactive rows without one). `CWD` MUST be the trimmed `cwd`, Cleaned when non-empty (relative paths stay relative; `/a/./b/` → `/a/b`). `SourceKind` MUST be the accepted `kind`. Output order MUST equal roster order; the adapter MUST NOT de-duplicate (the reconciler does, PRV-010).
- **Source:** `go/internal/provider/claude/adapter.go`
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestDiscoverLiveFiltersAndSanitizes (first row short ID and cwd)

#### CLA-013 Name placeholder handling
- **Behavior:** `Name` MUST be the trimmed roster `name`, except that it MUST be empty when the trimmed name equals the trimmed `id` or the trimmed `sessionId` (Claude reports the short id as the name of an unnamed session). A missing name yields empty.
- **Source:** `go/internal/provider/claude/adapter.go` (sessionName)
- **Decision:** D-042
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestSessionNameIgnoresIDPlaceholders

### 4.3 Normalization

#### CLA-014 Normalize input validation
- **Behavior:** `Normalize` MUST reject, in order: trimmed `NativeSessionID` empty → `Claude session has no stable session ID`; `SourceKind` not `interactive`/`background` → `unsupported Claude session kind %q`; `Raw` not decodable as the evidence object → `decode sanitized Claude state: <err>`.
- **Source:** `go/internal/provider/claude/adapter.go` (Normalize)
- **Verified by:** UNTESTED

#### CLA-015 Runtime state
- **Behavior:** Runtime MUST be computed in this precedence:

  | Condition (evaluated top to bottom) | RuntimeState |
  |---|---|
  | `state == "failed"` | `failed` |
  | `state == "stopped"` | `stopped` |
  | `process_alive` | `running` |
  | `state ∈ {working, blocked, done}` | `resumable` |
  | anything else (empty or unknown `state`, no process) | `unknown` |

  `status` and `waitingFor` never affect runtime.
- **Source:** `go/internal/provider/claude/adapter.go` (normalizeRuntime)
- **Decision:** D-044, D-048
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings (all rows except `unknown`; `unknown` UNTESTED)

#### CLA-016 Activity and attention
- **Behavior:** Activity/attention MUST be computed from `state` first, falling back to `status` only when `state` is empty or unrecognized. `wf` = trimmed `waitingFor`.

  | `state` | `status` | `wf` | Activity | Attention type | Detail |
  |---|---|---|---|---|---|
  | `working` | any | any | `working` | `none` | — |
  | `blocked` | any | empty | `idle` | `none` | — |
  | `blocked` | any | non-empty | `waiting` | per CLA-017 (blocked=true) | per CLA-017 |
  | `done` | any | any | `idle` | `none` | — |
  | `stopped` | any | any | `idle` | `none` | — |
  | `failed` | any | any | `unknown` | `error` | `Claude session failed` |
  | other/empty | `busy` | any | `working` | `none` | — |
  | other/empty | `waiting` | any | `waiting` | per CLA-017 (blocked=false) | per CLA-017 |
  | other/empty | `idle` | any | `idle` | `none` | — |
  | other/empty | other/empty | any | `unknown` | `none` | — |

  `Attention.Since` and `NativeRequestID` are never set.
- **Source:** `go/internal/provider/claude/adapter.go` (normalizeActivity)
- **Decision:** D-044
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings (working, blocked±wf, done, failed, status idle); `stopped`, `busy`, `waiting`, unknown-status rows UNTESTED

#### CLA-017 waitingFor mapping
- **Behavior:** `detail` = trimmed `waitingFor` (exact, case-sensitive match):

  | `detail` | blocked=true (from `state=blocked`) | blocked=false (from `status=waiting`) |
  |---|---|---|
  | `permission prompt`, `sandbox request` | `approval_required`, detail | `approval_required`, detail |
  | `input needed`, `worker request`, `dialog open` | `input_required`, detail | `input_required`, detail |
  | empty | `input_required`, no detail (unreachable: CLA-016 routes empty to idle) | `none`, no detail |
  | any other string | `input_required`, detail | `unknown`, detail |

  The adapter MUST NOT emit `authentication_required`.
- **Source:** `go/internal/provider/claude/adapter.go` (normalizeWaitingFor)
- **Decision:** D-044
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings (`permission prompt`, `input needed`, unknown reason with blocked); others UNTESTED

#### CLA-018 Timestamps
- **Behavior:** `CreatedAt` MUST be `time.UnixMilli(started_at).UTC()` when `started_at > 0`, else nil. `LastActivityAt` MUST be nil (the roster has no activity time).
- **Source:** `go/internal/provider/claude/adapter.go`
- **Verified by:** UNTESTED (value; see the §7.1 example)

#### CLA-019 Workspace
- **Behavior:** When `CWD` is non-empty, `Workspace` MUST be `{Path: CWD (as given, not re-Cleaned), Kind: "directory", Owner: "unknown", ObservedAt: now().UTC()}`; otherwise nil. Git enrichment happens later in the reconciler.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-027
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings (ObservedAt)

#### CLA-020 Persisted native_state
- **Behavior:** `NativeState` MUST be the JSON object, in key order, `state` (omitempty), `status` (omitempty), `waiting_for` (omitempty), `process_alive` (always). `started_at` MUST NOT be included. Marshal failure → `encode normalized Claude state: <err>`.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Decision:** D-023
- **Verified by:** UNTESTED

#### CLA-021 Capabilities and remaining fields
- **Behavior:** `Capabilities` MUST be `{"attach": {supported: NativeShortID != ""}, "resume": {supported: true}}`; when attach is unsupported its `reason` MUST be `interactive session has no background attach ID`. `LiveState` MUST be `live`. `Model`, `ModelSource`, `ReasoningEffort`, `ReasoningSource`, `Summary` MUST be empty and `InitialRequest` nil (the roster exposes none; hooks supply them). `Name`, `NativeShortID`, `SourceKind` pass through.
- **Source:** `go/internal/provider/claude/adapter.go`
- **Verified by:** UNTESTED

#### CLA-022 No provider-side rename
- **Behavior:** The Claude adapter MUST NOT implement `Renamer`; a noc rename of a Claude agent reports `unsupported` and stays a registry overlay.
- **Source:** `go/internal/provider/claude/adapter.go` (no `SetName`); `go/internal/server/run.go`
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt (`claude unsupported` case)

---

## 5. Codex adapter

| Constant | Value | Source |
|---|---|---|
| Default operation timeout | 5 s | `codex/adapter.go` |
| Default executable | `codex` | `codex/adapter.go` |
| Max model runes | 256 | `codex/adapter.go` |
| Max reasoning-effort runes | 64 | `codex/adapter.go` |
| Max preview runes | 2,048 | `codex/adapter.go` |
| Model/reasoning source label | `codex_app_server_thread` | `codex/adapter.go` |
| Preview source label | `codex_app_server_preview` | `codex/adapter.go` |
| Accepted sources | `cli`, `vscode`, `appServer` | `codex/adapter.go` |
| Control socket | `<CODEX_HOME>/app-server-control/app-server-control.sock` | `codex/socket.go` |
| Max WebSocket frame / reassembled message (`maxWebSocketBytes`) | 16 MiB (`16 << 20` = 16,777,216 bytes) | `codex/websocket.go` |
| Proxy stderr cap | 64 KiB | `codex/proxy.go` |
| Proxy exit grace | 1 s | `codex/proxy.go` |
| `thread/loaded/list` page size | `limit: 1000` | `codex/proxy.go` |

### 5.1 Construction, resolution, probe

#### CDX-001 Construction defaults
- **Behavior:** `New()` MUST equal `NewWithOptions(Options{})`. Defaults: `Runner` → `NewExecCommandRunner()` (used only by `Probe`); executable per PRV-016; `OperationTimeout <= 0` → 5 s; `Now` → `time.Now`. `Provider()` MUST return `codex`.
- **Source:** `go/internal/provider/codex/adapter.go` (New, NewWithOptions, Provider)
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNewUsesExecutableEnvironment (executable only)

#### CDX-002 Socket path resolution
- **Behavior:** The control socket MUST resolve as: (1) `Options.SocketPath`, trimmed, non-empty → Cleaned, **explicit**; (2) `NOC_CODEX_SOCKET`, trimmed, non-empty → Cleaned, **explicit**; (3) **inferred**: `codexHome` = trimmed `CODEX_HOME`, or `os.UserHomeDir()/.codex` if empty; result `Clean(codexHome)/app-server-control/app-server-control.sock`. If `CODEX_HOME` is empty and the home directory cannot be resolved, the path MUST be empty and inferred (not explicit). The socket's existence is not checked at construction.
- **Source:** `go/internal/provider/codex/socket.go` (resolveSocketPath)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNewUsesSocketEnvironment; `go/internal/provider/codex/adapter_test.go` TestExplicitSocketOverridesEnvironment; `go/internal/provider/codex/adapter_test.go` TestNewDerivesSocketFromCodexHomeWithProxyFallback

#### CDX-003 Client wiring and fallback eligibility
- **Behavior:** If `Options.Client` is supplied it MUST be used as-is (no socket client, no fallback; the resolved socket path is stored but unused). Otherwise the client MUST be a direct socket client for the resolved path whose fallback is a proxy client (`<exe> app-server proxy`, same operation timeout) **only when the path was inferred**; an explicit path MUST have no fallback.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNewUsesSocketEnvironment; `go/internal/provider/codex/adapter_test.go` TestNewDerivesSocketFromCodexHomeWithProxyFallback

#### CDX-004 Probe step 1: version
- **Behavior:** `Probe` MUST capture `ObservedAt = now().UTC()`, then run `<exe> --version` under its own operation-timeout context. On error return `{Available: false, Diagnostic: err.Error(), ObservedAt}` with empty version.
- **Source:** `go/internal/provider/codex/adapter.go` (Probe)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestProbeUnavailable

#### CDX-005 Probe step 2: app-server capability
- **Behavior:** Then run `<exe> app-server --help` under a fresh operation-timeout context (output ignored). On error return `Available: false`, `Version` = trimmed version stdout, `Diagnostic: "Codex live discovery unavailable; installed CLI does not expose app-server: <err>"`. `Probe` MUST NOT touch the control socket, start a daemon, or run `codex app-server daemon version`.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Decision:** D-024, D-029
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestProbeRequiresAppServerSupport

#### CDX-006 Probe success
- **Behavior:** On success return `Available: true`, `Version` = trimmed `--version` stdout (e.g. `codex-cli 0.154.0`), `Features: {"discover_live": true, "direct_unix_socket": true}`. `direct_unix_socket` is a static claim of client capability; it MUST NOT be read as proof that the socket is reachable.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestProbe

#### CDX-007 DiscoverLive delegates and wraps
- **Behavior:** `DiscoverLive` MUST delegate to the configured client and wrap any error as `query Codex app-server: <err>`; on success it returns the client's rows unchanged.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestDiscoverLiveDelegatesToClient; `go/internal/provider/codex/adapter_test.go` TestDiscoverLiveWrapsClientFailure

### 5.2 Direct socket client

#### CDX-008 One deadline per snapshot, shared with the fallback
- **Behavior:** Each `DiscoverLive` MUST create one context with the operation timeout (5 s) covering dial, WebSocket upgrade, every RPC, and — if used — the proxy fallback (the fallback receives the same, partly consumed, context). Total snapshot time is therefore bounded by 5 s plus at most the 1 s proxy exit grace (CDX-017).
- **Source:** `go/internal/provider/codex/socket.go` (DiscoverLive)
- **Decision:** D-024
- **Verified by:** UNTESTED

#### CDX-009 "Not connected" is only an empty path or a failed dial
- **Behavior:** The client MUST report "not connected" (fallback-eligible) only when (a) the trimmed socket path is empty → error `Codex app-server socket path is unavailable`, or (b) `dial("unix", path)` fails → `dial Codex app-server socket %q: <err>` (e.g. `... "/x/codex.sock": dial unix /x/codex.sock: connect: no such file or directory`). Any failure after a successful dial — WebSocket upgrade, protocol, RPC error, timeout — MUST be reported as "connected" and MUST NOT fall back.
- **Source:** `go/internal/provider/codex/socket.go` (withConnection)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientFallsBackOnlyWhenDialFails; `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientDoesNotHideProtocolFailureWithFallback

#### CDX-010 Cancellation by closing the connection
- **Behavior:** After dialing, the client MUST register `context.AfterFunc(ctx, close connection)` so that a deadline or cancellation unblocks any in-progress read/write by closing the socket; the connection MUST be closed when the operation returns. No per-read socket deadlines are used.
- **Source:** `go/internal/provider/codex/socket.go` (withConnection)
- **Verified by:** UNTESTED

#### CDX-011 Upgrade and operation error text
- **Behavior:** An upgrade failure MUST be wrapped `open Codex app-server WebSocket on %q: <err>`; when the context is already done, `<err>` is the context error (e.g. `context deadline exceeded`) rather than the I/O error caused by closing the socket. A discovery failure on an open connection MUST be wrapped `discover through Codex app-server socket %q: <err>`.
- **Source:** `go/internal/provider/codex/socket.go` (withConnection, DiscoverLive)
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientDoesNotHideProtocolFailureWithFallback (upgrade text contains `WebSocket upgrade returned 200 OK`)

#### CDX-012 Fallback outcome
- **Behavior:** When not connected: if there is no fallback, return the direct error. Otherwise call `fallback.DiscoverLive(sameCtx)`; on success return its rows (the direct error is discarded without logging); on failure return `<direct err>; Codex proxy fallback failed: <fallback err>`.
- **Source:** `go/internal/provider/codex/socket.go` (DiscoverLive)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientFallsBackOnlyWhenDialFails; `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientReportsBothConnectionFailures

#### CDX-013 No WebSocket close handshake on completion
- **Behavior:** After the operation completes the client MUST simply close the Unix socket; it does not send a WebSocket Close frame. Servers MUST tolerate an abrupt EOF.
- **Source:** `go/internal/provider/codex/socket.go` (withConnection)
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientUsesDirectUnixSocket (fixture returns after the last response; implicit)

### 5.3 Proxy fallback client

#### CDX-014 Proxy process
- **Behavior:** The proxy client MUST start `<exe> app-server proxy` (argv exactly `app-server`, `proxy`) via `exec.CommandContext` under `WithTimeout(ctx, operationTimeout)`, with stdin and stdout as pipes and stderr captured into a 64 KiB bounded buffer (` (truncated)` appended to its string when exceeded). The child inherits noc's environment and cwd. Pipe errors → `open Codex proxy stdin: <err>` / `open Codex proxy stdout: <err>`; start failure → `start Codex app-server proxy: <err>`. The proxy stream MUST be treated as the same WebSocket byte stream as the socket (upgrade then frames), not JSONL; upgrade failure → `open Codex app-server WebSocket: <err>`.
- **Source:** `go/internal/provider/codex/proxy.go` (withConnection, boundedBuffer)
- **Decision:** D-024
- **Verified by:** UNTESTED (only replaced by a fake in socket tests)

#### CDX-015 One process per operation
- **Behavior:** Each fallback discovery or rename MUST spawn one short-lived proxy process and wait for it; there is no pooling or persistent proxy.
- **Source:** `go/internal/provider/codex/proxy.go` (proxyClient, withConnection)
- **Decision:** D-024
- **Verified by:** UNTESTED

#### CDX-016 Proxy error precedence
- **Behavior:** After the operation, the client MUST close stdin and wait for exit (CDX-017). If the operation (or upgrade) failed, return that error, suffixed `: <trimmed stderr>` when stderr is non-empty. Else if the process exited with an error, return `Codex app-server proxy exited: <wait err>` with the same stderr suffix. Else success.
- **Source:** `go/internal/provider/codex/proxy.go` (withConnection, proxyError)
- **Verified by:** UNTESTED

#### CDX-017 Proxy exit grace
- **Behavior:** After stdin is closed the client MUST wait up to 1 s (`proxyExitGrace`, not bounded by the context) for the process to exit; if it has not, it MUST kill it, wait for it, and treat the exit as success (no error).
- **Source:** `go/internal/provider/codex/proxy.go` (waitForProxy)
- **Verified by:** UNTESTED

### 5.4 WebSocket client

#### CDX-018 Upgrade request bytes
- **Behavior:** If the context is already done, fail with the context error before writing. Otherwise generate 16 bytes from `crypto/rand` (failure → `generate WebSocket handshake key: <err>`), base64-encode (standard alphabet, padded) as the key, and write exactly, then flush:
  ```
  GET / HTTP/1.1\r\n
  Host: localhost\r\n
  Upgrade: websocket\r\n
  Connection: Upgrade\r\n
  Sec-WebSocket-Key: <key>\r\n
  Sec-WebSocket-Version: 13\r\n
  \r\n
  ```
  No `Origin`, `Sec-WebSocket-Protocol`, `Sec-WebSocket-Extensions`, or auth headers are sent. Write/flush errors → `write WebSocket upgrade request: <err>` / `flush WebSocket upgrade request: <err>`.
- **Source:** `go/internal/provider/codex/websocket.go` (newWebSocketMessageTransport)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/websocket_test.go` TestWebSocketMessageTransportHandshakeMaskingAndFragmentation (path `/`, Connection/Upgrade tokens, key present, version 13)

#### CDX-019 Upgrade response validation
- **Behavior:** The response MUST be parsed as HTTP/1.x (`read WebSocket upgrade response: <err>` on failure) and checked in order: status MUST be 101 else `WebSocket upgrade returned <status line text>` (e.g. `WebSocket upgrade returned 200 OK`); the first `Connection` header MUST contain the token `upgrade` and the first `Upgrade` header the token `websocket` (comma-separated, trimmed, case-insensitive) else `WebSocket upgrade response omitted required headers`; `Sec-WebSocket-Accept` MUST equal `base64(SHA-1(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))` exactly, else `WebSocket upgrade response had an invalid accept value`; a non-empty `Sec-WebSocket-Extensions` MUST fail with `WebSocket server selected an extension noc did not request`. `Sec-WebSocket-Protocol` is not checked. The same buffered reader MUST be kept for frames so bytes following the response are not lost.
- **Source:** `go/internal/provider/codex/websocket.go` (newWebSocketMessageTransport, webSocketAccept, headerContainsToken)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/websocket_test.go` TestWebSocketMessageTransportRejectsInvalidUpgrade (status); header/accept/extension rejections UNTESTED

#### CDX-020 Incoming frame header rules
- **Behavior:** For each server frame, in order: any RSV bit set (`first & 0x70`) → `received a WebSocket frame with unsupported reserved bits`; a masked server frame → `received WebSocket frame with masked=true, want false`; payload length decoding: 0–125 literal; 126 → 16-bit big-endian, MUST be ≥ 126 else `received a non-canonical WebSocket frame length`; 127 → 64-bit big-endian, MUST be ≥ 65,536 with the top bit clear else `received an invalid WebSocket frame length`; length > 16,777,216 → `WebSocket frame exceeds 16777216 bytes`; control opcode (≥ 0x8) that is non-final or longer than 125 bytes → `received an invalid WebSocket control frame`. Short reads return the I/O error (e.g. `EOF`, `unexpected EOF`).
- **Source:** `go/internal/provider/codex/websocket.go` (readWebSocketFrame)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/websocket_test.go` TestWebSocketFrameLengths (valid encodings 0/125/126/65535/65536 round-trip); rejection paths UNTESTED

#### CDX-021 Opcode dispatch on read
- **Behavior:** `ReadMessage` MUST loop over frames, checking the context before each frame read:

  | Opcode | Behavior |
  |---|---|
  | `0x1` text, final, no open fragment | validate UTF-8 (`received a WebSocket text frame with invalid UTF-8`), return payload |
  | `0x1` text, non-final, no open fragment | open fragment, append (size-checked) |
  | `0x1` text while a fragment is open | `received a new WebSocket data frame during a fragmented message` |
  | `0x0` continuation, no open fragment | `received an unexpected WebSocket continuation frame` |
  | `0x0` continuation | append (size-checked); if final, validate UTF-8 of the whole message (`received a fragmented WebSocket text message with invalid UTF-8`) and return it |
  | `0x9` ping | reply immediately with a pong carrying the identical payload (`write WebSocket pong: <err>` on failure); continue (open fragment preserved) |
  | `0xA` pong | ignore |
  | `0x8` close | echo a close frame with the identical payload (best effort, error ignored); return `io.EOF` |
  | `0x2` binary | `received an unsupported WebSocket binary message` |
  | other | `received unsupported WebSocket opcode %#x` (e.g. `0x3`) |
- **Source:** `go/internal/provider/codex/websocket.go` (ReadMessage)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/websocket_test.go` TestWebSocketMessageTransportHandshakeMaskingAndFragmentation (ping→pong, text+continuation reassembly); other rows UNTESTED

#### CDX-022 Reassembled message size bound
- **Behavior:** A fragmented message's total payload MUST NOT exceed 16,777,216 bytes; the fragment that would exceed it fails with `WebSocket message exceeds 16777216 bytes`.
- **Source:** `go/internal/provider/codex/websocket.go` (appendWebSocketFragment)
- **Verified by:** UNTESTED

#### CDX-023 Outgoing frames
- **Behavior:** `WriteMessage` MUST refuse invalid UTF-8 (`refusing to write a WebSocket text frame with invalid UTF-8`) and otherwise send the whole message as one final text frame (the client never fragments). Every client frame (text, pong, close echo) MUST be masked with a fresh 4-byte `crypto/rand` mask (`generate WebSocket mask: <err>` on failure), use the minimal length encoding (≤125 inline; ≤65,535 → 126 + 16-bit; else 127 + 64-bit), be written under a mutex, and be flushed immediately. Payloads > 16,777,216 bytes → `WebSocket frame exceeds 16777216 bytes`; an invalid control frame → `refusing to write an invalid WebSocket control frame`.
- **Source:** `go/internal/provider/codex/websocket.go` (WriteMessage, writeFrame, writeWebSocketFrame)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/websocket_test.go` TestWebSocketMessageTransportHandshakeMaskingAndFragmentation (masked text and pong); `go/internal/provider/codex/websocket_test.go` TestWebSocketFrameLengths

### 5.5 JSON-RPC layer

#### CDX-024 Message envelope and IDs
- **Behavior:** Each JSON-RPC message MUST be one WebSocket text message. Requests MUST be `{"id": <int>, "method": "<m>", "params": <obj>}` and notifications `{"method": "<m>", "params": <obj>}`; no `"jsonrpc": "2.0"` member is sent. Request IDs MUST be integers starting at 1 per connection and incrementing by 1 per request (notifications do not consume IDs). Encode errors → `encode Codex app-server message: <err>`; transport write errors → `flush Codex app-server message: <err>`. Requests are strictly sequential (one outstanding call).
- **Source:** `go/internal/provider/codex/proxy.go` (rpcRequest, rpcNotification, call, notify, write)
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (method sequence); `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientUsesDirectUnixSocket

#### CDX-025 Reading responses
- **Behavior:** After writing a request, `call` MUST read messages until the matching response:
  - read error → the context error itself if the context is done (e.g. bare `context deadline exceeded`), else `read Codex app-server message: <err>` (e.g. `... EOF` after a server close);
  - undecodable JSON → `decode Codex app-server message: <err>` (fatal);
  - a message with a non-empty `method` **and** a present `id` (including `"id": null`) is a server-initiated request: reply `{"id": <same raw id>, "error": {"code": -32601, "message": "noc does not implement server-initiated requests"}}` and keep reading;
  - a message whose `id` does not decode to the expected integer (notifications, other IDs, string IDs, `null`, `1.0`) is ignored;
  - matching `id` with non-null `error` → `Codex app-server error <code>: <message>` (typed `rpcCallError`); an `error` member that is not an object of that shape → `decode <method> error: <err>`;
  - matching `id` otherwise → success; if a result target was given and `result` is present, decode it (`decode <method> response: <err>` on failure). A missing `result`, or `null`, is success with zero values.
- **Source:** `go/internal/provider/codex/proxy.go` (call, rejectServerRequest, matchesRPCID)
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (server request rejected with -32601); `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionRejectsMalformedProtocol; `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionReturnsNonLivenessRPCError

#### CDX-026 initialize / initialized
- **Behavior:** Every connection MUST begin with request `initialize` with params exactly:
  ```json
  {
    "clientInfo": {"name": "noc", "title": "noc agent registry", "version": "<buildinfo.Version>"},
    "capabilities": {
      "experimentalApi": false,
      "optOutNotificationMethods": [
        "item/agentMessage/delta",
        "item/commandExecution/outputDelta",
        "item/reasoning/summaryTextDelta",
        "item/reasoning/textDelta"
      ]
    }
  }
  ```
  (`buildinfo.Version` defaults to `dev`). The result is ignored. Failure → `initialize Codex app-server connection: <err>`. Then the notification `initialized` with params `{}`; failure → `acknowledge Codex app-server initialization: <err>`.
- **Source:** `go/internal/provider/codex/proxy.go` (initializeConnection)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (order `initialize`, `initialized`); param contents UNTESTED (MANUAL: §8.2 M-X3)

### 5.6 Discovery exchange

```
noc (socket client)                                 Codex shared app-server
  |-- connect(unix, <CODEX_HOME>/app-server-control/app-server-control.sock)
  |-- GET / HTTP/1.1  Upgrade: websocket  Sec-WebSocket-Key: K ------------>|
  |<----------------- HTTP/1.1 101  Sec-WebSocket-Accept: A(K) -------------|
  |== masked text ==> {"id":1,"method":"initialize","params":{clientInfo,capabilities}}
  |<== text ======== {"id":1,"result":{...ignored...}}
  |== masked text ==> {"method":"initialized","params":{}}
  |== masked text ==> {"id":2,"method":"thread/loaded/list","params":{"limit":1000}}
  |<== text ======== {"id":"s-1","method":"<server request>",...}      (any time)
  |== masked text ==> {"id":"s-1","error":{"code":-32601,"message":"noc does not implement server-initiated requests"}}
  |<== ping "p" ====                                                    (any time)
  |== masked pong "p" ==>
  |<== text ======== {"id":2,"result":{"data":["T1","T2"],"nextCursor":"C"}}
  |== masked text ==> {"id":3,"method":"thread/loaded/list","params":{"limit":1000,"cursor":"C"}}
  |<== text ======== {"id":3,"result":{"data":["T3"],"nextCursor":null}}
  |== masked text ==> {"id":4,"method":"thread/read","params":{"threadId":"T1","includeTurns":false}}
  |<== text ======== {"id":4,"result":{"thread":{...}}}
  |   ... one thread/read per unique loaded ID, sequentially ...
  |== masked text ==> {"id":6,"method":"thread/read","params":{"threadId":"T3","includeTurns":false}}
  |<== text ======== {"id":6,"error":{"code":-32602,"message":"thread is no longer loaded"}}  (skipped)
  |-- close(socket)   (no WebSocket Close frame)
  All of the above shares one 5 s deadline (CDX-008).
```

#### CDX-027 Discovery sequence
- **Behavior:** `discoverFromConnection` MUST: initialize (CDX-026); fetch all loaded thread IDs (CDX-028); then for each ID, in list order, trim it, skip empty IDs, skip IDs already seen in this snapshot, and call `thread/read` with params exactly `{"threadId": <id>, "includeTurns": false}` (CDX-029/030). It MUST NOT call `thread/list`, `thread/start`, `thread/resume`, any subscribe method, or any mutating method.
- **Source:** `go/internal/provider/codex/proxy.go` (discoverFromConnection)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (8 reads for 9 IDs with one duplicate; `includeTurns` false; forbidden methods absent)

#### CDX-028 Loaded-thread pagination
- **Behavior:** `thread/loaded/list` MUST be called with `{"limit": 1000}` and, on later pages, `"cursor": <nextCursor>`. `data` (array of strings) is appended in order. Paging stops when `nextCursor` is absent, `null`, or `""`. A `nextCursor` already returned earlier in this snapshot MUST fail with `Codex repeated a loaded-thread pagination cursor`. Any call error → `list loaded Codex threads: <err>`. There is no page-count cap other than the snapshot deadline.
- **Source:** `go/internal/provider/codex/proxy.go` (listLoadedThreads)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionPaginatesLoadedThreads; repeated-cursor error UNTESTED

#### CDX-029 Unload race tolerance
- **Behavior:** If `thread/read` returns a JSON-RPC error whose lower-cased `message` contains `not loaded`, `no longer loaded`, or `thread not found`, that thread MUST be skipped and the snapshot continues. The error `code` is not consulted.
- **Source:** `go/internal/provider/codex/proxy.go` (discoverFromConnection, isThreadNoLongerLoadedError)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionSkipsThreadReadRPCError

#### CDX-030 Any other thread/read failure aborts the snapshot
- **Behavior:** Any other `thread/read` error (other RPC error, transport, decode, timeout) MUST abort the whole snapshot with `read loaded Codex thread metadata: <err>` and return no rows.
- **Source:** `go/internal/provider/codex/proxy.go` (discoverFromConnection)
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionReturnsNonLivenessRPCError

#### CDX-031 Decoded thread fields; everything else discarded
- **Behavior:** From `result.thread` only these keys MUST be decoded; all others (`path`, `turns`, unknown fields) MUST be discarded:

  | Key | Type | Use |
  |---|---|---|
  | `id` | string | trimmed → `NativeSessionID` (the response's ID, not the requested one) |
  | `name` | string or null | null → `""`; else trimmed → `Name` (no placeholder logic) |
  | `cwd` | string | trimmed → `CWD` (Cleaned later in `Normalize`) |
  | `source` | raw JSON | acceptance (CDX-032) → `SourceKind` |
  | `status.type` | string | evidence `status` |
  | `status.activeFlags` | []string | evidence `active_flags` (copied) |
  | `model` | string | bounded to 256 runes → evidence `model` |
  | `reasoningEffort` | string | bounded to 64 runes → evidence `reasoning_effort` |
  | `preview` | string | bounded to 2,048 runes → evidence `preview`, `preview_truncated` |
  | `createdAt` / `updatedAt` | int64 epoch seconds | evidence `created_at` / `updated_at` |

  A `thread` that fails to decode (wrong types) makes `thread/read` fail and aborts the snapshot (CDX-030: `decode thread/read response: ...`).
- **Source:** `go/internal/provider/codex/proxy.go` (threadRecord, nativeAgentFromThread)
- **Decision:** D-024, D-027
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (no `transcript`/`/private/` in evidence)

#### CDX-032 Source and liveness acceptance
- **Behavior:** A thread MUST be dropped (silently, not an error) when its trimmed `id` is empty, when `status.type == "notLoaded"`, or when `source` is not a JSON string exactly equal to one of `cli`, `vscode`, `appServer`:

  | `source` value | Result |
  |---|---|
  | `"cli"`, `"vscode"`, `"appServer"` | accepted; `SourceKind` = that string |
  | `"exec"` or any other string | dropped |
  | object, e.g. `{"subAgent":"review"}`, `{"custom":"hosted"}` | dropped |
  | missing / null / number | dropped |
- **Source:** `go/internal/provider/codex/proxy.go` (nativeAgentFromThread, topLevelSourceKind)
- **Decision:** D-011, D-024
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes (exec, subAgent, custom, notLoaded dropped)

#### CDX-033 Text bounding
- **Behavior:** `model`, `reasoningEffort`, and `preview` MUST be compacted first — split on any Unicode whitespace and re-joined with single spaces (so leading/trailing whitespace and newlines disappear) — and then cut to the first N runes if longer. `preview_truncated` MUST be true only if the compacted preview exceeded 2,048 runes. Truncation of model/reasoning is silent (no flag).
- **Source:** `go/internal/provider/codex/adapter.go` (boundedText); `go/internal/provider/codex/proxy.go` (nativeAgentFromThread)
- **Decision:** D-027
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestNativeAgentFromThreadBoundsPreviewMetadata (rune limits with multi-byte characters); whitespace compaction UNTESTED (MANUAL: §8.2 M-X3)

#### CDX-034 Sanitized evidence (`NativeAgent.Raw`)
- **Behavior:** `Raw` MUST be the JSON object, in key order: `status` (always), `active_flags` (omitted if empty), `created_at`, `updated_at`, `model`, `reasoning_effort`, `preview`, `preview_truncated` (each omitted when zero/empty/false). Marshal failure → `encode sanitized Codex state: <err>`.
- **Source:** `go/internal/provider/codex/adapter.go` (evidence); `go/internal/provider/codex/proxy.go` (nativeAgentFromThread)
- **Verified by:** `go/internal/provider/codex/proxy_test.go` TestDiscoverFromConnectionFiltersAndSanitizes

### 5.7 Normalization

#### CDX-035 Normalize input validation
- **Behavior:** `Normalize` MUST reject, in order: trimmed `NativeSessionID` empty → `Codex thread has no stable thread ID`; `SourceKind` not in `{cli, vscode, appServer}` → `unsupported Codex thread source %q`; undecodable `Raw` → `decode sanitized Codex state: <err>`; evidence `status == "notLoaded"` → `Codex thread is not loaded`.
- **Source:** `go/internal/provider/codex/adapter.go` (Normalize)
- **Decision:** D-024
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeRejectsNonLiveOrInternalThreads

#### CDX-036 Status normalization
- **Behavior:**

  | `status` | `active_flags` | Runtime | Activity | Attention | Detail |
  |---|---|---|---|---|---|
  | `active` | contains `waitingOnApproval` (takes priority) | `running` | `waiting` | `approval_required` | `Codex is waiting on approval` |
  | `active` | contains `waitingOnUserInput` | `running` | `waiting` | `input_required` | `Codex is waiting on user input` |
  | `active` | neither | `running` | `working` | `none` | — |
  | `idle` | any | `loaded` | `idle` | `none` | — |
  | `systemError` | any | `failed` | `unknown` | `error` | `Codex app-server reported a system error` |
  | `notLoaded` | any | `resumable` | `unknown` | `none` | — (unreachable via `Normalize`, CDX-035) |
  | other/empty | any | `unknown` | `unknown` | `unknown` | — |

  Flags are matched exactly (case-sensitive); other flags are ignored.
- **Source:** `go/internal/provider/codex/adapter.go` (normalizeStatus)
- **Decision:** D-024, D-044
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings (active, approval priority, input, idle, systemError); unknown-status row UNTESTED

#### CDX-037 Timestamps
- **Behavior:** `CreatedAt` = `time.Unix(created_at, 0).UTC()` when > 0, else nil; `LastActivityAt` = `time.Unix(updated_at, 0).UTC()` when > 0, else nil. Units are seconds (unlike Claude's milliseconds).
- **Source:** `go/internal/provider/codex/adapter.go`
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings (non-nil only)

#### CDX-038 Workspace
- **Behavior:** When `CWD` is non-empty, `Workspace` MUST be `{Path: Clean(CWD), Kind: "directory", Owner: "unknown", ObservedAt: now().UTC()}`; else nil.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings (ObservedAt)

#### CDX-039 Initial request preview
- **Behavior:** When evidence `preview` is non-empty, `InitialRequest` MUST be `{Text: preview, Source: "codex_app_server_preview", Truncated: preview_truncated, ObservedAt: now().UTC()}`; else nil. `Summary` MUST be empty.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Decision:** D-027
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings

#### CDX-040 Model and reasoning effort
- **Behavior:** `Model` and `ReasoningEffort` MUST be the bounded evidence values. `ModelSource` and `ReasoningSource` MUST be `codex_app_server_thread` unconditionally — including when the value is empty.
- **Source:** `go/internal/provider/codex/adapter.go`
- **Decision:** D-027
- **Verified by:** `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings (non-empty case only)

#### CDX-041 Persisted native_state, capabilities, other fields
- **Behavior:** `NativeState` MUST be `{"status": <status>, "active_flags": [...]}` with `active_flags` omitted when empty; timestamps, model, and preview MUST NOT appear in it (marshal failure → `encode normalized Codex state: <err>`). `Capabilities` MUST be `{"attach": {supported: true}, "resume": {supported: true}}`. `NativeShortID` MUST be empty. `LiveState` MUST be `live`. `Name`, `SourceKind` pass through.
- **Source:** `go/internal/provider/codex/adapter.go` (Normalize, nativeState)
- **Verified by:** UNTESTED (MANUAL: §8.2 M-X3)

### 5.8 Rename push (`thread/name/set`)

#### CDX-042 SetName
- **Behavior:** `Adapter.SetName(ctx, nativeSessionID, name)` MUST return `ErrNotImplemented` (unwrapped) if the configured client does not implement thread naming (e.g. an injected discovery-only client); otherwise call it and wrap failures as `rename Codex thread: <err>`. The adapter performs no validation or trimming of `name` (the API handler validates).
- **Source:** `go/internal/provider/codex/adapter.go` (SetName)
- **Decision:** D-042
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestAdapterWithoutRenamingClientReportsNotImplemented; `go/internal/provider/codex/socket_test.go` TestSocketClientRenamesThreadThroughAppServer

#### CDX-043 Rename exchange
- **Behavior:** On a fresh connection, the client MUST run `initialize` + `initialized` (CDX-026) and then request `thread/name/set` with params exactly `{"threadId": <nativeSessionID>, "name": <name>}`; the result is ignored. Failure → `set Codex thread name: <err>`. One connection per rename.
- **Source:** `go/internal/provider/codex/proxy.go` (setThreadNameOnConnection)
- **Decision:** D-042
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketClientRenamesThreadThroughAppServer

#### CDX-044 Rename transport, timeout, and fallback
- **Behavior:** The socket client MUST bound a rename with its own operation-timeout context (5 s; the API handler also applies 5 s). Fallback rules match discovery: only when not connected (empty path or dial failure) and only if the fallback implements naming, the proxy client renames over a fresh `codex app-server proxy` process; a combined failure reads `<direct err>; Codex proxy fallback failed: <fallback err>`. Errors on a connected socket are returned without the `discover through ...` wrapping. With no fallback (explicit socket) the dial error is returned directly.
- **Source:** `go/internal/provider/codex/socket.go` (SetThreadName); `go/internal/provider/codex/proxy.go` (proxyClient.SetThreadName)
- **Decision:** D-024, D-042
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketClientRenamesThreadThroughAppServer (direct path); fallback rename UNTESTED

### 5.9 Health text produced by Codex failures

#### CDX-045 Degraded detail strings
- **Behavior:** Combined with PRV-009 and CDX-007/009/012, a Codex snapshot failure MUST surface as health `degraded` with these exact shapes:

  | Situation | Detail |
  |---|---|
  | Explicit socket missing | `discover codex agents: query Codex app-server: dial Codex app-server socket "<path>": dial unix <path>: connect: no such file or directory` |
  | Inferred socket missing, proxy fails | `discover codex agents: query Codex app-server: dial Codex app-server socket "<path>": <dial err>; Codex proxy fallback failed: <proxy err>[: <stderr>]` |
  | Socket accepts, server not WebSocket | `discover codex agents: query Codex app-server: open Codex app-server WebSocket on "<path>": WebSocket upgrade returned <status>` |
  | Snapshot exceeds 5 s | `discover codex agents: query Codex app-server: discover through Codex app-server socket "<path>": [<wrapping>: ]context deadline exceeded` |
  | Unsupported request | `discover codex agents: query Codex app-server: discover through Codex app-server socket "<path>": read loaded Codex thread metadata: Codex app-server error <code>: <message>` |

  The worker keeps polling; a daemon started later is discovered on the next successful poll.
- **Source:** `go/internal/server/discovery.go`; `go/internal/reconcile/reconciler.go`; `go/internal/provider/codex/adapter.go`; `go/internal/provider/codex/socket.go`
- **Decision:** D-024, D-032
- **Verified by:** `go/internal/provider/codex/socket_test.go` TestSocketDiscoveryClientReportsBothConnectionFailures (client-level fragments); full health string MANUAL: §8.2 M-X1

---

## 6. Timeouts and limits summary

| Operation | Budget | Notes | Source |
|---|---|---|---|
| Claude `--version` | 5 s | own context; `Run` may take up to 2 s longer (PRV-015) | `claude/adapter.go` |
| Claude `agents --help` | 5 s | own context; probe worst case 14 s (2 × (5 s + 2 s)) | `claude/adapter.go` |
| Claude `agents --json` | 5 s | returns within 7 s even if a grandchild holds stdout (PRV-015) | `claude/adapter.go` |
| Codex `--version` | 5 s | own context; `Run` may take up to 2 s longer (PRV-015) | `codex/adapter.go` |
| Codex `app-server --help` | 5 s | own context; probe worst case 14 s | `codex/adapter.go` |
| Codex snapshot (dial + upgrade + all RPC + fallback) | 5 s total | one shared context | `codex/socket.go` |
| Proxy process | min(5 s, remaining parent) + 1 s exit grace | grace not context-bound | `codex/proxy.go` |
| Codex rename | 5 s (client) ∧ 5 s (API handler) | | `codex/socket.go`; `localapi/server.go` |
| Server startup probes | sequential, Claude then Codex | up to 28 s worst case before first discovery | `server/run.go` |
| stdout / stderr caps | 8 MiB / 64 KiB | runner | `command.go` |
| Runner wait delay | 2 s | after the context ends or the child exits | `command.go` |
| WebSocket frame / message | 16 MiB | both directions | `codex/websocket.go` |
| Proxy stderr | 64 KiB | | `codex/proxy.go` |
| Preview / model / reasoning | 2,048 / 256 / 64 runes | after whitespace compaction | `codex/adapter.go` |

---

## 7. Annotated examples

### 7.1 Claude (from `claude/testdata/agents.json`)

Input rows (excerpt; IDs are synthetic):

```jsonc
[
  { "id": "22222222", "cwd": "/workspace/service-b", "kind": "background",
    "startedAt": 1760000001000, "sessionId": "22222222-2222-4222-8222-222222222222",
    "name": "approval-agent", "state": "blocked", "pid": 1202,
    "status": "waiting", "waitingFor": "permission prompt" },          // accepted
  { "cwd": "/workspace/service-g", "kind": "interactive",
    "startedAt": 1760000006000, "sessionId": "77777777-7777-4777-8777-777777777777",
    "name": "interactive-agent", "pid": 1207, "status": "idle" },     // accepted, no short id
  { "cwd": "/workspace/remote", "kind": "cloud", "sessionId": "8888..." },   // dropped: kind
  { "id": "99999999", "kind": "background", "name": "no-session-id",
    "state": "working" },                                              // dropped: no sessionId
  { "cwd": "/workspace/incompatible", "kind": 42, "sessionId": "aaaa..." },  // dropped: decode
  { "id": "11111111", "...": "...", "prompt": "must never be retained" }     // accepted; prompt discarded
]
```

Fixture result: 7 of 10 rows accepted, in order.

`NativeAgent.Raw` and normalized output for the approval row:

```jsonc
// Raw (sanitized evidence)
{"state":"blocked","status":"waiting","waiting_for":"permission prompt","started_at":1760000001000,"process_alive":true}
// Normalized (selected fields)
{
  "provider": "claude", "native_session_id": "22222222-2222-4222-8222-222222222222",
  "native_short_id": "22222222", "name": "approval-agent", "source_kind": "background",
  "live_state": "live", "runtime_state": "running", "activity_state": "waiting",
  "attention": {"type": "approval_required", "detail": "permission prompt"},
  "created_at": "2025-10-09T08:53:21Z",
  "capabilities": {"attach": {"supported": true}, "resume": {"supported": true}},
  "native_state": {"state":"blocked","status":"waiting","waiting_for":"permission prompt","process_alive":true},
  "workspace": {"path": "/workspace/service-b", "kind": "directory", "owner": "unknown"}
}
```

All seven fixture rows, normalized:

| Row | state / status / waitingFor / pid | Runtime | Activity | Attention (detail) | attach |
|---|---|---|---|---|---|
| working-agent | working / busy / – / 1201 | running | working | none | yes |
| approval-agent | blocked / waiting / permission prompt / 1202 | running | waiting | approval_required (permission prompt) | yes |
| input-agent | blocked / – / input needed / – | resumable | waiting | input_required (input needed) | yes |
| done-agent | done / – / – / – | resumable | idle | none | yes |
| failed-agent | failed / – / – / – | failed | unknown | error (Claude session failed) | yes |
| stopped-agent | stopped / – / – / – | stopped | idle | none | yes |
| interactive-agent | – / idle / – / 1207 | running | idle | none | no: `interactive session has no background attach ID` |

### 7.2 Codex (from `codex/testdata/discovery.jsonl`)

The fixture is the server side of one snapshot. Line 2 is a server-initiated
request (`{"id":"server-1","method":"account/chatgptAuthTokens/refresh"}`)
that arrives before the `thread/loaded/list` response and is rejected with
-32601. The loaded list contains 9 IDs, one duplicate, so exactly 8
`thread/read` calls are made.

| Thread | source | status | Result |
|---|---|---|---|
| cli-active | `"cli"` | active, no flags | accepted |
| vscode-idle | `"vscode"` | idle | accepted |
| app-approval | `"appServer"` | active, `waitingOnApproval` | accepted |
| exec-live | `"exec"` | active | dropped (source) |
| sub-live | `{"subAgent":"review"}` | active | dropped (structured source) |
| race | `"cli"` | notLoaded | dropped (race) |
| error-live | `"cli"`, `name: null` | systemError | accepted, name `""` |
| custom-live | `{"custom":"hosted"}` | active | dropped (structured source) |

`cli-active` thread/read result (note `path`, which is discarded):

```json
{"id":"cli-active","name":"CLI task","cwd":"/workspace/cli","source":"cli",
 "status":{"type":"active","activeFlags":[]},"model":"gpt-6-astra","reasoningEffort":"high",
 "createdAt":1789400000,"updatedAt":1789400010,"preview":"Build a live agent registry",
 "path":"/private/transcript.jsonl"}
```

Sanitized `Raw`, then normalized output:

```jsonc
{"status":"active","created_at":1789400000,"updated_at":1789400010,"model":"gpt-6-astra","reasoning_effort":"high","preview":"Build a live agent registry"}
{
  "provider": "codex", "native_session_id": "cli-active", "name": "CLI task", "source_kind": "cli",
  "live_state": "live", "runtime_state": "running", "activity_state": "working",
  "attention": {"type": "none"},
  "model": "gpt-6-astra", "model_source": "codex_app_server_thread",
  "reasoning_effort": "high", "reasoning_source": "codex_app_server_thread",
  "initial_request": {"text": "Build a live agent registry", "source": "codex_app_server_preview"},
  "created_at": "2026-09-14T15:33:20Z", "last_activity_at": "2026-09-14T15:33:30Z",
  "capabilities": {"attach": {"supported": true}, "resume": {"supported": true}},
  "native_state": {"status": "active"},
  "workspace": {"path": "/workspace/cli", "kind": "directory", "owner": "unknown"}
}
```

A preview of `"  first\n\n  request  "` is retained as `"first request"`
with `truncated` false (CDX-033).

---

## 05 · Design choices (local)

Implementation choices not recorded in `docs/DECISIONS.md`, with their
rationale:

1. **Per-row tolerance in the Claude roster.** A row that fails to decode is
   dropped rather than failing the snapshot, so one incompatible row cannot
   hide all other sessions; the provider's next snapshot gets another chance
   to report it (`claude/adapter.go`). The cost is that the drops are
   invisible: no counter or log.
2. **Two evidence shapes per adapter.** `NativeAgent.Raw` is the full
   sanitized evidence (Claude: plus `started_at`; Codex: plus timestamps,
   model, preview). `NormalizedAgent.NativeState`, which gets persisted, is a
   narrower re-encoding holding only native *state* fields. Timestamps and
   metadata go into first-class normalized fields instead.
3. **A blocked turn with no reason is idle.** Claude's `blocked` with no
   `waitingFor` is a session that finished its turn and waits for the next
   message (`claude/adapter.go`). D-044 records the product decision;
   the code also maps an *unknown* non-empty `waitingFor` under `blocked` to
   `input_required` with the raw detail, so a new Claude reason still counts
   as needing the user.
4. **Approval beats user input.** When Codex sets both flags, approval is
   reported (`codex/adapter.go`).
5. **The unload race is detected by matching message text.** `thread/read`
   errors are classified by lower-cased substrings rather than by error code,
   because one thread unloading between list and read does not invalidate
   the rest of the loaded snapshot (`codex/proxy.go`).
6. **Server-initiated requests get an explicit rejection.** They are answered
   with -32601 instead of being ignored, so the daemon does not wait on noc
   (`codex/proxy.go`). Notifications are dropped, and four high-volume
   delta notifications are opted out of at `initialize`.
7. **The WebSocket client is hand-written and minimal.** It supports no
   extensions, no subprotocols, and no client fragmentation. It requires
   unmasked server frames, validates UTF-8, and caps frames and messages at
   16 MiB. The proxy does not translate the WebSocket stream into JSONL
   (`codex/websocket.go`), so one transport serves both the socket and the
   proxy.
8. **A JSONL transport is kept for tests.** `newJSONLineMessageTransport` /
   `jsonLineMessageTransport` (`codex/proxy.go`) has no production
   caller. Protocol tests use it to replay `discovery.jsonl` line by line.
9. **Cancellation works by closing the connection.** `context.AfterFunc`
   closes the socket (`codex/socket.go`) instead of setting read
   deadlines, so one mechanism covers dial, handshake, and every RPC.
10. **The fallback shares the snapshot deadline.** The proxy runs inside the
    remaining socket-discovery budget, which keeps a poll's worst case near
    5 s.
11. **The proxy exit grace is not an error.** A proxy that does not exit
    within 1 s of stdin closing is killed and the operation still succeeds,
    because the RPC result has already been obtained (`codex/proxy.go`).
12. **`direct_unix_socket` is a static feature flag.** It advertises client
    capability only. Reachability is measured by each snapshot (D-024).
13. **Model and reasoning sources are always labeled.** Codex sets
    `ModelSource` and `ReasoningSource` even when the values are empty. The
    registry is responsible for ignoring empty values.
14. **Probe time is captured first.** `ObservedAt` is taken before the probe
    commands run, so it marks the start of the probe.
15. **The runner closes inherited pipes instead of waiting for them.** A
    2 s `WaitDelay` bounds a poll when a wrapper's grandchild keeps stdout
    open; the grandchild is left running (`command.go`, PRV-015).

---

## 05 · Verification procedures

### 8.1 Automated tests

Run from the repository root:

```sh
cd go && go test -count=1 ./internal/provider/...
# expected:
# ok   noc.local/noc/internal/provider
# ok   noc.local/noc/internal/provider/claude
# ok   noc.local/noc/internal/provider/codex
cd go && go test -list . ./internal/provider/...   # enumerates the test functions
cd go && go test -count=1 -run 'TestSocket|TestWebSocket|TestDiscoverFromConnection' -v ./internal/provider/codex
```

All tests use fake runners, in-memory pipes, `net.Pipe`, or private Unix
sockets under `/tmp/noc-codex-*`. They never execute `claude` or `codex`,
with one exception, below.

**Opt-in integration test.** `TestInstalledSharedDaemonDiscovery`
(`codex/socket_test.go`) is skipped unless `NOC_CODEX_INTEGRATION=1`:

```sh
cd go && NOC_CODEX_INTEGRATION=1 go test -count=1 -run TestInstalledSharedDaemonDiscovery -v ./internal/provider/codex
```

What it does: it builds `codex.New()` from the real environment
(`NOC_CODEX_BIN`, `NOC_CODEX_SOCKET`, `CODEX_HOME`, `PATH`). It runs the
real `codex --version` and `codex app-server --help`, asserts the client is
the direct socket client, and performs one `DiscoverLive` with a 15 s outer
context. It logs the count of loaded top-level threads.

What it touches: it reads `<CODEX_HOME>/app-server-control/app-server-control.sock`.
It sends `initialize`, `initialized`, `thread/loaded/list`, and
`thread/read{includeTurns:false}`.

What it does not do: it does not create, resume, subscribe to, rename, or
mutate threads, and it does not start the daemon. Two caveats:

- If the inferred socket cannot be dialed, it **will spawn
  `codex app-server proxy`** as the fallback. Set `NOC_CODEX_SOCKET` to
  forbid that.
- `initialize` registers a client connection with the daemon for the
  duration of the test.

It requires a real Codex installation and a running shared daemon. It is not
part of `make check`.

### 8.2 Manual checks with fake providers (no real provider is invoked)

Every step below uses fake executables and fake sockets in a scratch
directory. No step reads or writes `~/.claude`, `~/.codex`, or `~/.config`.

Setup:

```sh
export SPEC=$(mktemp -d /tmp/noc-spec.XXXXXX)
(cd go && go build -o "$SPEC/noc" ./cmd/noc)
cp go/internal/provider/claude/testdata/agents.json "$SPEC/agents.json"
cat > "$SPEC/fake-claude" <<EOF
#!/bin/sh
echo "\$*" >> "$SPEC/claude-calls.log"
case "\$*" in
  "--version") echo "9.9.9 (Fake Claude Code)" ;;
  "agents --help") echo "Usage: claude agents [options]" ;;
  "agents --json") cat "$SPEC/agents.json" ;;
  *) echo "fake-claude: unexpected argv: \$*" >&2; exit 64 ;;
esac
EOF
cat > "$SPEC/fake-codex" <<EOF
#!/bin/sh
echo "\$*" >> "$SPEC/codex-calls.log"
case "\$*" in
  "--version") echo "codex-cli 9.9.9" ;;
  "app-server --help") echo "Run the app server or related tooling" ;;
  "app-server proxy") echo "fake proxy refused" >&2; exit 2 ;;
  *) echo "fake-codex: unexpected argv: \$*" >&2; exit 64 ;;
esac
EOF
chmod +x "$SPEC/fake-claude" "$SPEC/fake-codex"
export NOC_STATE_DIR="$SPEC/state" NOC_RUNTIME_DIR="$SPEC/run"
export NOC_CLAUDE_BIN="$SPEC/fake-claude" NOC_CODEX_BIN="$SPEC/fake-codex"
```

**M-C1: Claude discovery from a fake roster.**
1. Run `NOC_CODEX_SOCKET="$SPEC/none.sock" "$SPEC/noc" server` in its own
   pane, with the environment above.
2. Run `"$SPEC/noc" server status`. Expected: Claude `available`, version
   `9.9.9 (Fake Claude Code)`, source `claude agents --json`, detail
   `discovery active; observed 7 live sessions`.
3. Run `"$SPEC/noc" ls --json`. Expected: seven `claude` agents matching the
   table in §7.1. For example, `approval-agent` has
   `runtime_state: running`, `activity_state: waiting`, and
   `attention.type: approval_required`. `interactive-agent` has
   `capabilities.attach.supported: false`. No `native_state` contains
   `prompt`, `transcript`, `pid`, or `started_at`.
4. Check `$SPEC/claude-calls.log`. It contains exactly `--version`,
   `agents --help`, and then one `agents --json` per poll (every 5 s). It
   never contains `--all`.

**M-C2: Claude placeholder name.** In `$SPEC/agents.json`, set
`working-agent`'s `"name"` to `"11111111"` (its short `id`). Within one poll,
`noc ls --json` shows that agent with no provider `name` (CLA-013).

**M-C3: Runner error strings and the bounded timeout (PRV-014/015).**
1. Write a fake that prints to stderr and exits 3:
   `printf '#!/bin/sh\necho "no such subcommand" >&2\nexit 3\n' > "$SPEC/fail" && chmod +x "$SPEC/fail"`.
2. Start a server with `NOC_CLAUDE_BIN="$SPEC/fail"`. Claude health is
   `unavailable` with detail
   `run provider command "<SPEC>/fail": exit status 3: no such subcommand`.
3. Start a server with `NOC_CLAUDE_BIN=definitely-not-claude`. The detail
   is
   `run provider command "definitely-not-claude": exec: "definitely-not-claude": executable file not found in $PATH`.
4. Write a hanging fake whose grandchild holds stdout:
   `printf '#!/bin/sh\nsleep 10\n' > "$SPEC/slow" && chmod +x "$SPEC/slow"`.
   Start a server with `NOC_CLAUDE_BIN="$SPEC/slow"`. The Claude probe
   returns after about 7 s (the 5 s timeout plus the 2 s wait delay), not
   10 s, and the detail is
   `run provider command "<SPEC>/slow": context deadline exceeded`.

**M-X1: Codex explicit socket missing, no fallback.** In the M-C1 server
(`NOC_CODEX_SOCKET=$SPEC/none.sock`), `noc server status` shows Codex
`degraded` for both the startup and the latest result. The probe passed, so
the synchronous first snapshot is recorded as the startup result. The detail
is
`discover codex agents: query Codex app-server: dial Codex app-server socket "<SPEC>/none.sock": dial unix <SPEC>/none.sock: connect: no such file or directory`.
`$SPEC/codex-calls.log` contains `--version` and `app-server --help` and
**never** `app-server proxy`.

**M-X2: Codex inferred socket missing, fallback attempted.** Restart the
server with `NOC_CODEX_SOCKET` unset and `CODEX_HOME="$SPEC/codex-home"`
(the directory need not exist). On every poll, `codex-calls.log` gains one
`app-server proxy` line. The Codex detail reads
`... dial Codex app-server socket "<SPEC>/codex-home/app-server-control/app-server-control.sock": dial unix ...: connect: no such file or directory; Codex proxy fallback failed: open Codex app-server WebSocket: read WebSocket upgrade response: EOF: fake proxy refused`.
The exact middle text may differ with pipe timing. The required parts are
the `; Codex proxy fallback failed:` join and the `fake proxy refused`
stderr suffix.

**M-X3: Codex discovery and rename through a fake socket.** Save the script
below as `$SPEC/fake_codex_socket.py`.

```python
"""Fake Codex app-server control socket for noc manual checks.
Usage: python3 -I fake_codex_socket.py /path/to/codex.sock"""
import base64, hashlib, json, os, socket, struct, sys

GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
THREADS = {
    "fake-cli": {"id": "fake-cli", "name": "Fake CLI task", "cwd": "/tmp", "source": "cli",
                 "status": {"type": "active", "activeFlags": ["waitingOnApproval"]},
                 "model": "fake-model", "reasoningEffort": "high",
                 "preview": "  first\n\n  request  ", "createdAt": 1789400000,
                 "updatedAt": 1789400010, "path": "/private/transcript.jsonl"},
    "fake-idle": {"id": "fake-idle", "name": None, "cwd": "/tmp/./idle/", "source": "vscode",
                  "status": {"type": "idle"}, "createdAt": 1789400100, "updatedAt": 1789400200},
    "fake-exec": {"id": "fake-exec", "name": "excluded", "cwd": "/tmp", "source": "exec",
                  "status": {"type": "active", "activeFlags": []}},
    "fake-sub": {"id": "fake-sub", "name": "excluded", "cwd": "/tmp",
                 "source": {"subAgent": "review"}, "status": {"type": "idle"}},
}
GONE = "fake-gone"  # listed as loaded, then reported as no longer loaded

def recv_exact(conn, size):
    data = b""
    while len(data) < size:
        chunk = conn.recv(size - len(data))
        if not chunk:
            raise EOFError
        data += chunk
    return data

def read_frame(conn):
    head = recv_exact(conn, 2)
    opcode, length = head[0] & 0x0F, head[1] & 0x7F
    if length == 126:
        length = struct.unpack(">H", recv_exact(conn, 2))[0]
    elif length == 127:
        length = struct.unpack(">Q", recv_exact(conn, 8))[0]
    mask = recv_exact(conn, 4) if head[1] & 0x80 else b"\0\0\0\0"
    payload = recv_exact(conn, length)
    return opcode, bytes(b ^ mask[i % 4] for i, b in enumerate(payload))

def send_text(conn, message):
    data = json.dumps(message).encode()
    if len(data) < 126:
        head = bytes([0x81, len(data)])
    elif len(data) < 65536:
        head = bytes([0x81, 126]) + struct.pack(">H", len(data))
    else:
        head = bytes([0x81, 127]) + struct.pack(">Q", len(data))
    conn.sendall(head + data)

def handshake(conn):
    request = b""
    while b"\r\n\r\n" not in request:
        chunk = conn.recv(4096)
        if not chunk:
            raise EOFError
        request += chunk
    key = b""
    for line in request.split(b"\r\n"):
        if line.lower().startswith(b"sec-websocket-key:"):
            key = line.split(b":", 1)[1].strip()
    accept = base64.b64encode(hashlib.sha1(key + GUID).digest()).decode()
    conn.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                  "Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n" % accept).encode())

def respond(message):
    method, params = message.get("method"), message.get("params") or {}
    if method == "initialize":
        return {"result": {"userAgent": "fake-codex"}}
    if method == "thread/loaded/list":
        return {"result": {"data": list(THREADS) + [GONE], "nextCursor": None}}
    if method == "thread/read":
        thread = THREADS.get(params.get("threadId"))
        if thread is None:
            return {"error": {"code": -32602, "message": "thread is no longer loaded"}}
        return {"result": {"thread": thread}}
    if method == "thread/name/set":
        THREADS[params["threadId"]]["name"] = params["name"]
        return {"result": {}}
    return {"error": {"code": -32601, "message": "fake does not implement " + str(method)}}

def main():
    path = sys.argv[1]
    if os.path.exists(path):
        os.unlink(path)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    server.listen()
    print("fake codex socket listening on", path, file=sys.stderr, flush=True)
    while True:
        conn, _ = server.accept()
        try:
            handshake(conn)
            while True:
                opcode, payload = read_frame(conn)
                if opcode == 0x8:
                    break
                message = json.loads(payload)
                print("client:", json.dumps(message), file=sys.stderr, flush=True)
                if "id" in message and "method" in message:
                    reply = respond(message)
                    reply["id"] = message["id"]
                    send_text(conn, reply)
        except EOFError:
            pass
        finally:
            conn.close()

if __name__ == "__main__":
    main()
```

1. In one pane, run `python3 -I "$SPEC/fake_codex_socket.py" "$SPEC/codex.sock"`.
2. In another, run `NOC_CODEX_SOCKET="$SPEC/codex.sock" "$SPEC/noc" server`.
3. The fake's stderr shows the following per poll, in order:
   - `initialize`, with `clientInfo` `{"name":"noc","title":"noc agent registry","version":...}` and the four `optOutNotificationMethods`;
   - `initialized` with `{}`;
   - `thread/loaded/list` with `{"limit": 1000}`;
   - five `thread/read` requests with `"includeTurns": false`, IDs 3–7.

   No other methods appear.
4. Run `"$SPEC/noc" ls --json`. Codex shows exactly two agents:
   - `fake-cli`: `runtime_state: running`, `activity_state: waiting`,
     attention `approval_required` / `Codex is waiting on approval`,
     `model: fake-model`, `initial_request.text: "first request"`,
     `native_state: {"status":"active","active_flags":["waitingOnApproval"]}`,
     `workspace.path: /tmp`.
   - `fake-idle`: `runtime_state: loaded`, `activity_state: idle`, no name,
     `workspace.path: /tmp/idle`.

   `fake-exec`, `fake-sub`, and `fake-gone` are absent. Health detail:
   `discovery active; observed 2 live sessions`.
5. Run `"$SPEC/noc" rename <fake-idle agent id> Renamed`. The fake logs
   `initialize`, `initialized`, then
   `thread/name/set {"name": "Renamed", "threadId": "fake-idle"}`. The CLI
   prints `Named <id> "Renamed"; the codex session was renamed too.` (header
   `Noc-Provider-Name-Sync: synced`). The next poll reports the provider
   name `Renamed`.
6. Stop the fake. The next poll shows Codex `degraded` with a `dial ...`
   detail. Restart the fake, and the poll after that returns to `available`
   with no server restart.

Cleanup: stop the server and the fake, then delete the scratch directory
`$SPEC`.

### 8.3 Checks against real providers (read-only, opt-in)

These checks need real installations and are **read-only**. Run them only
deliberately.

- **R-1: real Claude roster shape.** Run `claude agents --json | head -c 2000`.
  Confirm it is a JSON array, and that rows have `kind`, `sessionId`, and
  `state`/`status`/`waitingFor` with the documented value sets. This only
  reads the roster.
- **R-2: real Codex daemon.** Run the opt-in integration test in §8.1 with
  `NOC_CODEX_SOCKET` set explicitly, so the proxy fallback is impossible.
  It only reads loaded-thread metadata.
- Do **not** run `noc rename` against a real Codex agent as a check. It
  calls `thread/name/set` and changes provider state.

---

## 05 · Coverage gaps

1. **The command runner is partly tested.** The default caps, the stderr
   ` (truncated)` suffix, the context error taking precedence over a
   successful exit, and environment/cwd/stdin inheritance (PRV-012–PRV-014)
   have no tests.
2. **The wait delay is tested only on the deadline path (PRV-015).** No test
   uses a grandchild that outlives the direct child, and none covers the
   `WaitDelay expired` error after a normal exit.
3. **Never-reprobe behavior is untested (PRV-007).** No test asserts that a
   provider whose startup probe failed gets no worker.
4. **Silent exclusions are not asserted as silent.** Tests check that
   excluded Claude rows and Codex threads are absent, not that health and
   the skipped count are unaffected.
5. **WebSocket rejection paths are untested.** Missing tests: RSV bits,
   masked server frames, non-canonical lengths, oversize frames and
   messages, invalid control frames, binary frames, unknown opcodes, invalid
   UTF-8, continuation without a start, a data frame during a fragment,
   close echo, invalid accept value, missing upgrade headers, and an
   unrequested extension.
6. **The proxy client is untested.** Process spawn, stderr bounding, exit
   grace and kill, error precedence, and fallback rename have no tests.
7. **The shared fallback deadline and AfterFunc cancellation are untested**
   (CDX-008, CDX-010).
8. **Normalization branches are untested:** Claude `status=busy`,
   `status=waiting` (with and without `waitingFor`), unknown status,
   `stopped` activity, the runtime `unknown` branch, the `sandbox request`,
   `worker request`, and `dialog open` mappings, the `native_state` shape,
   capabilities, and the `CreatedAt` value. Codex: unknown status,
   `native_state` shape, empty-model sources, and the timestamp values.
9. **Claude edge decoding is untested:** top-level `null` or a non-array, and
   empty stdout.
10. **Codex protocol edges:**
    - An error response with `"id": null` is ignored, so the call waits for
      the deadline.
    - A notification carrying `"id": null` is answered with a -32601 error.
    - The repeated-cursor error is untested.
    - The response thread `id` is trusted over the requested ID.
11. **The `initialize` parameters are not asserted by any test.** They were
    confirmed manually only.
12. **No Close frame on completion (CDX-013).** A strict server may log an
    abnormal closure for every poll.
13. **The opt-in integration test can spawn the proxy** when the inferred
    socket is missing (§8.1).

---

## 05 · Provider version assumptions

Baselines (see `docs/PROVIDERS.md`): **Claude Code 2.1.270** and
**Codex CLI 0.154.0**. These are development baselines, not enforced
minimums. Neither adapter
compares versions.

| # | Assumed provider behavior | Relied on by | Observed against |
|---|---|---|---|
| 1 | `claude --version` exits 0 and prints a single version line | CLA-002 | 2.1.270 (`adapter_test.go` fixture text `2.1.270 (Claude Code)`) |
| 2 | `claude agents --help` exits 0 if and only if the roster command exists | CLA-003 | 2.1.270 |
| 3 | `claude agents --json` prints a JSON array of live sessions only (completed history needs `--all`) | CLA-005/006 | 2.1.270 |
| 4 | Roster keys `id`, `cwd`, `kind`, `startedAt` (epoch **ms**), `sessionId`, `name`, `state`, `pid`, `status`, `waitingFor` have those JSON types | CLA-008 | 2.1.270 |
| 5 | `kind` ∈ `interactive`, `background` for local sessions; cloud and remote rows use another kind | CLA-009 | 2.1.270 |
| 6 | `state` ∈ working/blocked/done/failed/stopped; `status` ∈ busy/waiting/idle; `waitingFor` ∈ permission prompt/input needed/sandbox request/worker request/dialog open | CLA-015–017 | 2.1.270 (`docs/PROVIDERS.md`) |
| 7 | `blocked` with no `waitingFor` means "turn finished", not a question | CLA-016 | D-044 |
| 8 | Unnamed sessions report the short id (or session id) as `name` | CLA-013 | D-042 |
| 9 | Interactive rows may lack `id`, and `sessionId` can be briefly absent for new jobs | CLA-010/012/021 | 2.1.270 |
| 10 | Claude has no external rename interface | CLA-022 | D-042 |
| 11 | `codex --version` prints e.g. `codex-cli 0.154.0`; `codex app-server --help` exits 0 when the app-server exists | CDX-004/005 | 0.154.0 |
| 12 | The shared daemon listens at `${CODEX_HOME:-~/.codex}/app-server-control/app-server-control.sock` | CDX-002 | 0.154.0 |
| 13 | The socket and `codex app-server proxy` carry an HTTP/1.1 WebSocket upgrade on path `/`, then RFC 6455 text frames. The server sends unmasked frames and negotiates no extensions. | CDX-018–023 | 0.154.0 |
| 14 | JSON-RPC without a `jsonrpc` member; integer IDs accepted; `initialize` then `initialized` required; `clientInfo` and `capabilities.optOutNotificationMethods` accepted; `experimentalApi:false` gives the stable surface | CDX-024–026 | 0.154.0 schema (generated, not vendored; `docs/PROVIDERS.md`) |
| 15 | The server may send its own requests (e.g. `account/chatgptAuthTokens/refresh`) during a call and tolerates a -32601 reply | CDX-025 | 0.154.0 fixture (`discovery.jsonl`) |
| 16 | `thread/loaded/list {limit, cursor?}` → `{data: [string], nextCursor: string\|null}` | CDX-028 | 0.154.0 |
| 17 | `thread/read {threadId, includeTurns:false}` → `{thread: {id, name\|null, cwd, source, status{type, activeFlags?}, model, reasoningEffort, preview, createdAt, updatedAt (epoch **s**), ...}}` | CDX-031 | 0.154.0 |
| 18 | Top-level sources are JSON strings `cli`/`vscode`/`appServer`; `exec` is a string; subagent and custom sources are objects | CDX-032 | 0.154.0 (`docs/PROVIDERS.md`) |
| 19 | Status types active/idle/systemError/notLoaded; flags `waitingOnApproval`, `waitingOnUserInput` | CDX-036 | 0.154.0 |
| 20 | A thread that unloads between list and read yields an error whose message contains "not loaded", "no longer loaded", or "thread not found", or a `notLoaded` status | CDX-029/032 | 0.154.0 (assumed message text; the fixture uses `thread is no longer loaded`) |
| 21 | `preview` is usually the first user message | CDX-039 | Codex documentation (D-027) |
| 22 | `thread/name/set {threadId, name}` renames a thread the way Codex's own rename does | CDX-043 | D-042 |
| 23 | Closing the socket without a WebSocket Close frame is harmless to the daemon | CDX-013 | 0.154.0 |
| 24 | `claude --bg <prompt>` starts a background session, returns at once, and prints the job's ID; it chooses the session ID itself and ignores `--session-id`. `--permission-mode auto` and `--name <name>` are accepted with it. It exits 1 with a message in a directory Claude does not trust. | [03-cli.md](03-cli.md) CLI-046 | 2.1.325 (by hand; not in any test) |
| 25 | A job started that way is listed by `claude agents --json --all` as kind `background`, with `id` equal to the printed job ID and the full `sessionId` | [03-cli.md](03-cli.md) CLI-046 | 2.1.325 (by hand) |
| 26 | Claude sets `CLAUDE_CODE_SESSION_ID` to the session's ID in the commands a session runs. A background session does not inherit the environment of the command that started it, so `NOC_AGENT_ID` does not reach it. | [03-cli.md](03-cli.md) CLI-043 | 2.1.325 (by hand) |

---

## Known limitations

- **A provider whose startup probe fails is never re-probed.** If `claude` or
  `codex` is missing, slow, or lacks the `agents` / `app-server` subcommand
  when the server starts, no discovery worker is created for it and its
  health stays `unavailable` until the server restarts (PRV-007;
  `go/internal/server/run.go`, Run).
- **Startup probes delay readiness.** Probes run sequentially before the
  socket is served; four hanging probe commands can take up to 28 s
  (`go/internal/server/run.go`, Run; `go/internal/provider/command.go`).
- **Exclusions are silent.** Claude rows dropped for kind, missing session
  ID, or a decode failure, and Codex threads dropped for source or
  `notLoaded`, are neither counted nor logged; health stays `available`.
  Only `Normalize` failures count as `skipped`
  (`go/internal/provider/claude/adapter.go`, DiscoverLive;
  `go/internal/provider/codex/proxy.go`, nativeAgentFromThread).
- **The proxy fallback sets no wait delay.** `codex app-server proxy` is
  killed at the deadline or after the 1 s exit grace, but if another process
  holds its stderr open, the wait for it does not return until that process
  exits (`go/internal/provider/codex/proxy.go`, withConnection, waitForProxy).
- **A grandchild that outlives a timed-out provider command is not killed.**
  The runner closes its pipes after 2 s and returns; the process keeps
  running (`go/internal/provider/command.go`, Run).
- **An error response with `"id": null` stalls the call.** It matches no
  request ID and is ignored, so the call waits for the snapshot deadline
  (`go/internal/provider/codex/proxy.go`, call).
- **No WebSocket Close frame is sent.** Each poll ends with an abrupt socket
  close, which a strict server may log as an abnormal closure (CDX-013;
  `go/internal/provider/codex/socket.go`, withConnection).
