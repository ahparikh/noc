# 06 — Hooks, setup, and doctor

This section specifies how `noc` turns provider hook invocations into registry
state, and how it installs, checks, and diagnoses that integration. It covers
the `noc hook ingest` entrypoint (stdin bounds, deadlines, environment
allowlist, fail-open silence, wake request), the normalized observation
envelope and its per-provider event mapping, deterministic event IDs, the
SQLite hook outbox and observation journal, the server drain loop and the
projection of observations into agent records, provider-process lineage, the
structured editing of Claude `settings.json` and Codex `hooks.json` (with
read-only inspection of Codex `config.toml`), the `noc setup` CLI
(install, `--check`, `--uninstall`), and every `noc doctor` check.

Source files covered:

- `go/internal/hooks/normalize.go`, `go/internal/hooks/processor.go`
  (+ `normalize_test.go`, `processor_test.go`)
- `go/internal/storage/sqlite/hooks.go` (+ `hooks_test.go`); the relevant
  tables in `go/internal/storage/sqlite/migrations.go` and `Open`/`HostID`/
  `Upsert`/`Forget` in `go/internal/storage/sqlite/store.go`
- `go/internal/proclineage/proclineage.go` (+ test)
- `go/internal/hookconfig/hookconfig.go` (+ test)
- `go/internal/setup/setup.go` (+ test)
- `go/internal/doctor/doctor.go`, `go/internal/doctor/hooks.go` (+ test)
- Neighbors read for behavior at the boundary: `go/internal/cli/run.go`
  (`runHook`, `runHookIngest`, `runSetup`, `runDoctor`, renderers),
  `go/internal/server/run.go` (drain loop), `go/internal/localapi/server.go`
  and `client.go` (wake route), `go/internal/config/paths.go`,
  `go/internal/reconcile/reconciler.go` (consumers of process ID and binding
  suppression), `go/pkg/api/v1/types.go` (wire types).

---

## Hook ingest entrypoint (`noc hook ingest`)

#### HK-001 Command shape and dispatch
- **Behavior:** `noc hook ingest --provider claude|codex` MUST be the only hook entrypoint for both providers. `noc hook` with no subcommand, or a first argument other than `ingest`, MUST print `usage: noc hook ingest --provider claude|codex` to stderr and exit `2` (this is the only non-silent, non-zero path under `hook`). Accepted flags after `ingest`: `--provider <string>` (default empty), `--database <path>` (default: resolved `DatabasePath`), `--socket <path>` (default: resolved `SocketPath`).
- **Source:** `go/internal/cli/run.go` (RunWithIO, runHook, runHookIngest)
- **Decision:** D-008, D-028
- **Verified by:** `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown (ingest path); usage path UNTESTED

#### HK-002 Silent, always-zero exit for `ingest`
- **Behavior:** `runHookIngest` MUST NOT write to stdout or stderr under any condition (flag-set output is `io.Discard`; stdout is not passed in) and MUST return exit code `0` in every case, including: path resolution failure, flag parse error (including `-h`), any positional argument, unsupported/missing `--provider`, SQLite open failure, host-ID read failure, payload rejection, Claude cloud exclusion, enqueue failure, and wake failure. A panic anywhere in the ingest MUST be recovered and also yield exit code `0`.
- **Source:** `go/internal/cli/run.go` (runHookIngest)
- **Decision:** D-028
- **Verified by:** `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown (exit 0, empty stdout/stderr with server down); other failure branches UNTESTED

#### HK-003 Ingest pipeline order
- **Behavior:** The ingest MUST execute these steps in order, stopping with exit 0 at the first failure:
  1. `config.ResolvePaths()` (env: `HOME`, `NOC_STATE_DIR`, `XDG_STATE_HOME`, `NOC_RUNTIME_DIR`, `XDG_RUNTIME_DIR`, `NOC_SOCKET`, `NOC_DATABASE`; see `go/internal/config/paths.go`).
  2. Parse flags; reject any positional argument; require provider `claude` or `codex`.
  3. Create a context with a **900 ms** deadline (`hookIngestTimeout`) derived from the caller's context (the CLI's signal context, which has no deadline). The 900 ms clock starts **here**, before SQLite open, and keeps running through steps 4–7 even though steps 6–7 do not observe it.
  4. `sqlitestore.Open(ctx, databasePath)` — creates the parent directory `0700` if missing, opens with `MaxOpenConns=1`, applies `PRAGMA foreign_keys = ON`, `journal_mode = WAL`, `busy_timeout = 5000`, runs migrations, `chmod 0600` on the DB file.
  5. `store.HostID(ctx)` — reads `metadata.host_id`, or mints and persists a new UUID if absent (so a hook can create the host identity before any server ever ran).
  6. `receivedAt = time.Now().UTC()`.
  7. Evaluate `hooks.EnvironmentFromOS()` (which runs the process-table snapshot, PL-001, with its own 2 s timeout) and then `hooks.ReadObservation(stdin, …)` (an `io.ReadAll` with no deadline). Go evaluates call arguments first, so the `ps` snapshot runs after SQLite open and the host-ID read but **before** stdin is read. Neither is cancelled by the 900 ms context.
  8. If the error is `ErrCloudSession`: exit 0 with nothing written.
  9. Any other normalization error: replace the observation with `RejectedObservation(provider, hostID, receivedAt, err)` (HK-023).
  10. `store.EnqueueObservation(ctx, observation)` (OBX-002). If the 900 ms context has already expired during steps 4–7, this fails, and the observation is **dropped**: nothing is spooled and the exit code is still 0 (HK-042).
  11. Best-effort wake: new local API client on `--socket`; `POST http://noc/v1/hooks/wake` with a **150 ms** context derived from the ingest context; result ignored.
  12. Return 0.
- **Source:** `go/internal/cli/run.go` (runHookIngest); `go/internal/storage/sqlite/store.go` (Open, HostID)
- **Decision:** D-028
- **Verified by:** `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown; `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (end-to-end with wake)

#### HK-004 Stdin cap (256 KiB)
- **Behavior:** Stdin MUST be read through a limit of `MaxPayloadBytes + 1` bytes where `MaxPayloadBytes = 256 * 1024` (262144). If more than 262144 bytes are read, the read MUST fail with `ErrPayloadTooLarge` ("hook payload exceeds the size limit"); exactly 262144 bytes is accepted. A read error MUST be wrapped as `read hook payload: <err>`. In the CLI both outcomes become a `hook.rejected` observation (HK-023).
- **Source:** `go/internal/hooks/normalize.go` (MaxPayloadBytes, readBounded, ReadObservation)
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` TestReadObservationRejectsOversizedPayload (boundary of exactly 262144 bytes UNTESTED)

#### HK-005 Deadlines and their scope
- **Behavior:**
  - **What the context bounds.** The 900 ms context, created before SQLite
    open, is passed to SQLite open, the host-ID read, the enqueue, and the
    wake request; the wake is further capped at 150 ms.
  - **What it does not bound:**
    - the process-table snapshot, which has its own independent **2 s**
      timeout from `context.Background()` (PL-002);
    - the stdin read, a plain `io.ReadAll` with **no** time limit.
  - **The binary's own guarantees are only these:**
    - each SQLite call fails once the 900 ms context has expired;
    - `ps` is killed after 2 s;
    - wake is abandoned after 150 ms.
  - **No internal bound on total runtime.** The binary has no bound on its
    total wall time. It waits for stdin until the writer closes it or
    sends EOF.
  - **The 3 s figure is the provider's, not noc's.** The 3 s `timeout`
    that setup writes into provider configs (HC-004) is enforced by the
    provider, if at all.
  - **When the context expires before enqueue.** If `ps` plus the stdin
    read take longer than what is left of the 900 ms after SQLite open,
    the enqueue fails with the expired context. The observation is then
    silently dropped (HK-042).
  - **Normal case.** Providers write the payload and close stdin
    immediately, and `ps` finishes in milliseconds. Ingest then completes
    well within 900 ms.
- **Source:** `go/internal/cli/run.go` (hookIngestTimeout, hookWakeTimeout, runHookIngest); `go/internal/hooks/normalize.go` (readBounded); `go/internal/proclineage/proclineage.go` (snapshotTimeout, snapshot)
- **Decision:** D-028
- **Verified by:** UNTESTED. MANUAL: hold stdin open (`sleep 5 | noc hook ingest --provider claude` in an isolated state directory) and confirm the process is still running after 3 s.

#### HK-042 Late payload is silently dropped
- **Behavior:** Consequence of HK-003/HK-005:
  - A hook invocation whose stdin payload is complete only after the 900 ms
    context has expired MUST still exit 0 silently.
  - It writes **no** `hook_outbox` row, so the event is lost rather than
    spooled.
  - Reconciliation can repair a lost event only for sessions a provider
    snapshot covers. A lost `SessionStart` for a foreground Claude session
    (hook-only) is not repaired until a later hook for that session arrives.
- **Source:** `go/internal/cli/run.go` (runHookIngest)
- **Decision:** D-028
- **Verified by:** UNTESTED. MANUAL (isolated state directory): `( sleep 1.5; printf '%s' "$PAYLOAD" ) | noc hook ingest --provider claude`, then `sqlite3 "file:$NOC_STATE_DIR/noc.db?immutable=1" 'select count(*) from hook_outbox'`. Expected: exit 0 and an unchanged row count; with `sleep 0.3` the count grows by one.

#### HK-006 Environment allowlist
- **Behavior:** The only process-environment values the normalizer may read are exactly:

  | Variable | Captured as | Rule |
  |---|---|---|
  | `ZELLIJ_SESSION_NAME` | `Environment.ZellijSession` | raw value |
  | `ZELLIJ_PANE_ID` | `Environment.ZellijPaneID` | raw value |
  | `CLAUDE_CODE_REMOTE` | `Environment.ClaudeRemote` (bool) | `true` iff value is exactly `"true"` |
  | `CLAUDE_CODE_BRIDGE_SESSION_ID` | `Environment.ClaudeRemoteControl` (bool) | `true` iff non-empty; the value itself MUST NOT be stored |

  Plus `Environment.ProcessID` from process lineage (PL-001). No credential or other environment variable MUST be collected. (Path-resolution variables in HK-003 step 1 select files; they never enter the envelope.)
- **Source:** `go/internal/hooks/normalize.go` (Environment, EnvironmentFromOS)
- **Decision:** D-027, D-028
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (Zellij vars end-to-end); `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend (struct-level); `EnvironmentFromOS` parsing of `CLAUDE_CODE_REMOTE` values other than `"true"` UNTESTED

#### HK-007 Wake request and route
- **Behavior:** The client MUST send `POST /v1/hooks/wake` with no body to host `noc` over the Unix socket. The server handler MUST: respond `405` with `Allow: POST` and error code `method_not_allowed` for any other method; respond `503` with code `hook_ingestion_unavailable` / message `hook ingestion is unavailable` if no wake function is registered; otherwise invoke the wake function and respond `202 Accepted` with an empty body. The client treats any non-2xx as an error (ignored by ingest).
- **Source:** `go/internal/localapi/client.go` (WakeHooks); `go/internal/localapi/server.go` (handleHookWake, requireMethod)
- **Decision:** D-028
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (happy path, indirectly); 405/503 UNTESTED

---

## Normalization

### Envelope

#### HK-008 Normalized envelope schema (v1)
- **Behavior:** `Normalize` MUST emit an `apiv1.Observation` with `schema_version = 1`, `source = "hook"`, and these JSON fields:

  | Field | Type | Rule |
  |---|---|---|
  | `schema_version` | int | always `1` (`apiv1.SchemaVersion`) |
  | `event_id` | string | `hook-` + 32 lowercase hex chars (HK-024) |
  | `provider` | string | `claude` or `codex` |
  | `host_id` | string | from `metadata.host_id` |
  | `native_session_id` | string, omitempty | trimmed, ≤512 runes `session_id` |
  | `event_type` | string | HK-015 table |
  | `source` | string | `"hook"` |
  | `occurred_at` | RFC 3339, omitempty | **never set by hooks** |
  | `received_at` | RFC 3339 (UTC) | ingest wall clock |
  | `data` | object | `ObservationData`, below |
  | `error` | string, omitempty | only on `hook.rejected` |

  `data` (`ObservationData`) fields: `native_event` (raw `hook_event_name`), `native_event_id` (omitempty), `name`, `source_kind`, `cwd`, `model`, `initial_request` (TextPreview), `summary` (TextPreview), `attention_type`, `attention_detail`, `permission_mode`, `reason`, `previous_live_state`, `live_state`, `frontend_bindings`, `process_id` — all omitempty except `native_event`. `previous_live_state`/`live_state` are never set by `Normalize`; the processor sets them before journaling (HK-034). `attention_type` is always non-empty on accepted payloads (`none` at minimum) so it always appears.
  `TextPreview` = `{text, source, truncated (omitempty), observed_at}`. `FrontendBinding` = `{kind, session (omitempty), pane_id (omitempty), state, observed_at}`.
- **Source:** `go/internal/hooks/normalize.go` (Normalize); `go/pkg/api/v1/types.go`
- **Decision:** D-027, D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend (partial); full field-by-field shape UNTESTED

#### HK-009 Supported provider and host preconditions
- **Behavior:** `Normalize` MUST return `unsupported hook provider "<p>"` for any provider other than `claude`/`codex`, and `hook host ID is required` for an empty host ID. These checks MUST precede JSON parsing.
- **Source:** `go/internal/hooks/normalize.go`
- **Verified by:** UNTESTED

#### HK-010 Claude cloud/web exclusion
- **Behavior:** When provider is `claude` and `CLAUDE_CODE_REMOTE == "true"`, `Normalize` MUST return `ErrCloudSession` before parsing the payload. The CLI MUST then exit 0 without enqueuing anything (no rejection row). The check MUST NOT apply to `codex`. Claude Remote Control (`CLAUDE_CODE_BRIDGE_SESSION_ID`) is a local session and MUST NOT be excluded (it yields a binding instead, HK-021).
- **Source:** `go/internal/hooks/normalize.go`; `go/internal/cli/run.go`
- **Decision:** D-011, D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeExcludesClaudeCloud; CLI no-write branch: `MANUAL:` see Verification procedures step M3

#### HK-011 Payload parsing and required fields
- **Behavior:** The payload MUST be decoded with `encoding/json` into a fixed struct (HK-013). Consequences that MUST hold: (a) any decode error — invalid JSON, a top-level array/string, or **an allowlisted key with the wrong JSON type** (e.g. `"cwd": 5`) — rejects the whole payload with message `hook payload is not valid JSON`; (b) unknown keys are silently ignored; (c) key matching is Go's case-insensitive struct matching (e.g. `Session_ID` matches `session_id`); (d) JSON `null` values leave fields empty; (e) top-level `null` decodes without error. After decoding, `session_id` and `hook_event_name` MUST be trimmed and bounded to 512 runes; an empty `session_id` MUST fail with `hook payload has no session_id`, then an empty `hook_event_name` with `hook payload has no hook_event_name`.
- **Source:** `go/internal/hooks/normalize.go`
- **Verified by:** UNTESTED (wrong-type rejection confirmed MANUAL, step M2)

#### HK-012 Reception time
- **Behavior:** `received_at` MUST be the caller-supplied time converted to UTC. All preview `observed_at` and binding `observed_at` values in the envelope MUST equal `received_at`.
- **Source:** `go/internal/hooks/normalize.go`
- **Verified by:** UNTESTED

### Allowlisted input fields

#### HK-013 Input allowlist
- **Behavior:** Only these provider input keys MAY be read; every other key (including `transcript_path`, `source`, `stop_hook_active`, `tool_name`, `tool_input`, `agent_id`, and any unknown key) MUST be discarded before the outbox write:

  | Input key | Used for | Normalization |
  |---|---|---|
  | `session_id` | `native_session_id` | trim, ≤512 runes; required |
  | `hook_event_name` | `data.native_event`, mapping | trim, ≤512 runes; required |
  | `prompt_id`, `turn_id`, `tool_use_id` | `data.native_event_id` = first non-blank in that order | trim, ≤512 runes |
  | `cwd` | `data.cwd` | trim, ≤4096 runes |
  | `new_cwd` | `data.cwd` when `hook_event_name == "CwdChanged"` and non-empty | trim, ≤4096 runes |
  | `old_cwd` | parsed, never stored | — |
  | `model` | `data.model` | trim, ≤256 runes |
  | `to_model` | `data.model` when non-empty (overrides `model` for any event) | trim, ≤256 runes |
  | `prompt` | `data.initial_request` on `UserPromptSubmit` only | HK-018 |
  | `last_assistant_message` | `data.summary` on `Stop`/`StopFailure` only | HK-019 |
  | `session_title` | `data.name` | trim, ≤512 runes (not whitespace-compacted) |
  | `source_kind` | `data.source_kind` | trim, ≤512 runes |
  | `permission_mode` | `data.permission_mode` | trim, ≤512 runes |
  | `reason` | `data.reason` | whitespace-compact, ≤512 runes |
  | `notification_type` | attention classification only (HK-017); never stored | lower-cased substring match |
  | `message` | `data.attention_detail` (HK-020) | whitespace-compact, ≤512 runes |
  | `error` | `data.attention_detail` for `turn.failed` | whitespace-compact, ≤512 runes |

- **Source:** `go/internal/hooks/normalize.go` (providerInput)
- **Decision:** D-027, D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend (`transcript_path` and an unknown key absent from encoded output); per-field bounds UNTESTED

#### HK-014 Metadata bounding primitive
- **Behavior:** `bounded(value, n)` MUST trim leading/trailing whitespace, then if the result has more than `n` Unicode code points keep exactly the first `n` code points (no ellipsis, no marker). `compactText(value)` MUST split on any Unicode whitespace run (`strings.Fields`) and rejoin with single ASCII spaces (removing leading/trailing whitespace and newlines). Limits: `maxPathRunes = 4096`, `maxMetadataRunes = 512`, `maxModelRunes = 256`.
- **Source:** `go/internal/hooks/normalize.go` (firstNonEmpty)
- **Verified by:** UNTESTED (direct)

### Event mapping

#### HK-015 Native event → normalized event table
- **Behavior:** `normalizeEvent` MUST map the (trimmed) `hook_event_name` exactly (case-sensitive) as below, independent of provider; the "installed for" columns reflect HC-002. Projection effects are specified in HK-038…HK-041.

  | `hook_event_name` | Installed for Claude | Installed for Codex | `event_type` | `attention_type` | Projection (runtime / activity / attention) |
  |---|---|---|---|---|---|
  | `SessionStart` | yes | yes | `session.started` | `none` | running / idle / cleared |
  | `SessionEnd` | yes | yes | `session.ended` | `none` | not_live, stopped / idle / cleared; `ended_at` set; bindings `disconnected` |
  | `UserPromptSubmit` | yes | yes | `turn.prompted` | `none` | running / working / cleared; `last_activity_at` |
  | `Stop` | yes | yes | `turn.stopped` | `none` | running / idle / cleared; `last_activity_at` |
  | `Interrupt` | no | yes | `turn.stopped` | `none` | same as `Stop` (no summary captured) |
  | `StopFailure` | yes | no | `turn.failed` | `error` | failed / idle / set `error`; `last_activity_at` |
  | `CwdChanged` | yes | no | `workspace.changed` | `none` | unchanged / unchanged / unchanged |
  | `PostModelSwitch` | yes | no | `model.changed` | `none` | unchanged / unchanged / unchanged |
  | `PermissionRequest` | yes | yes | `attention.approval_required` | `approval_required` | running / waiting / set |
  | `Notification` | yes | no | `notification.received` | per HK-017 | unchanged runtime; waiting if attention ≠ none; attention set if ≠ none |
  | anything else | — | — | `hook.observed` | `none` | unchanged / unchanged / unchanged |

  Every non-`session.ended` event that is applied as current also sets `live_state = live` and clears `ended_at` (HK-039), including `hook.observed`.
- **Source:** `go/internal/hooks/normalize.go` (normalizeEvent); `go/internal/hooks/processor.go` (applyState)
- **Decision:** D-029 (event sets), D-044
- **Verified by:** `go/internal/hooks/normalize_test.go` (UserPromptSubmit), `go/internal/hooks/normalize_test.go` (Stop), `go/internal/hooks/normalize_test.go` TestNormalizeCodexInterruptStopsCurrentTurn (Interrupt); SessionStart/SessionEnd via `go/internal/hooks/processor_test.go`; `StopFailure`, `CwdChanged`, `PostModelSwitch`, `PermissionRequest`, unknown-event mapping UNTESTED

#### HK-016 Workspace and model overrides
- **Behavior:** For `CwdChanged` with a non-empty `new_cwd`, `data.cwd` MUST be `new_cwd`; otherwise `cwd`. Whenever `to_model` is non-empty (any event), `data.model` MUST be `to_model`; otherwise `model`.
- **Source:** `go/internal/hooks/normalize.go`
- **Verified by:** UNTESTED (MANUAL step M2 shows both)

#### HK-017 Notification subtype classification
- **Behavior:** For `Notification`, `attention_type` MUST be derived from `strings.ToLower(notification_type)` with the first matching rule:
  1. contains `permission` or `approval` → `approval_required`
  2. contains `auth` and not `success` → `authentication_required`
  3. contains `input` or `elicitation` → `input_required`
  4. otherwise → `none`

  Therefore: `permission_prompt` → `approval_required`; `elicitation_dialog` → `input_required`; `auth_success` → `none`; `idle_prompt` → `none` (Claude's "still waiting at the prompt" nudge is not a question); empty/missing → `none`.
- **Source:** `go/internal/hooks/normalize.go` (notificationAttention)
- **Decision:** D-044
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNotificationAttentionIgnoresIdleNudges (authentication_required branch UNTESTED)

### Previews, detail, bindings

#### HK-018 First-observed user request preview (2048)
- **Behavior:** Only when `hook_event_name == "UserPromptSubmit"` and `prompt` is not blank, `data.initial_request` MUST be `{text, source: "hook_first_observed_user_prompt", truncated, observed_at: received_at}` where `text = bounded(compactText(prompt), 2048)` and `truncated = runeCount(compactText(prompt)) > 2048` (measured after compaction). No other event produces an initial request.
- **Source:** `go/internal/hooks/normalize.go` (textPreview)
- **Decision:** D-027
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend

#### HK-019 Latest assistant update preview (1200)
- **Behavior:** Only when `hook_event_name` is `Stop` or `StopFailure` (not `Interrupt`) and `last_assistant_message` is not blank, `data.summary` MUST be `{text: bounded(compactText(msg), 1200), source: "hook_last_assistant_message", truncated: runeCount(compacted) > 1200, observed_at: received_at}`.
- **Source:** `go/internal/hooks/normalize.go`
- **Decision:** D-027
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeStopProducesProvenancedSummary (compaction `"…parser.\n\nTests pass."` → `"…parser. Tests pass."`); 1200 truncation UNTESTED

#### HK-020 Attention detail
- **Behavior:** For `turn.failed`, `data.attention_detail` MUST be `bounded(compactText(firstNonEmpty(error, message)), 512)`. For `attention.approval_required` and `notification.received`, it MUST be `bounded(compactText(message), 512)`. For all other event types it MUST be empty.
- **Source:** `go/internal/hooks/normalize.go`
- **Verified by:** UNTESTED

#### HK-021 Frontend binding capture
- **Behavior:** `data.frontend_bindings` MUST contain, in order: (1) a `zellij` binding `{kind: "zellij", session: bounded(ZELLIJ_SESSION_NAME,512), pane_id: bounded(ZELLIJ_PANE_ID,512), state: "observed", observed_at: received_at}` if either Zellij variable is non-empty (either provider); (2) a `claude_remote_control` binding `{kind, state: "observed", observed_at}` (no session/pane) only when provider is `claude` and the bridge variable is non-empty. Empty list is omitted from JSON.
- **Source:** `go/internal/hooks/normalize.go` (frontendBindings)
- **Decision:** D-027, D-036
- **Verified by:** `go/internal/hooks/normalize_test.go` (two bindings); `go/internal/cli/run_test.go` (Zellij end-to-end)

#### HK-022 Process ID field
- **Behavior:** `data.process_id` MUST be `Environment.ProcessID` (PL-001), omitted when 0.
- **Source:** `go/internal/hooks/normalize.go`
- **Decision:** D-040
- **Verified by:** UNTESTED (unit); MANUAL step M1 shows a non-zero value when run under a provider

#### HK-023 Rejected observation
- **Behavior:** On any normalization error except `ErrCloudSession`, the CLI MUST enqueue `RejectedObservation`: `schema_version 1`, given provider and host, **no** `native_session_id`, `event_type = "hook.rejected"`, `source = "hook"`, `received_at` UTC, `data = {native_event: "unknown"}`, `error = bounded(compactText(err.Error()), 512)` (or `hook payload was rejected` for a nil error). Its `event_id` MUST be `"hook-" + hex(sha256(provider ‖ 0x00 ‖ host_id ‖ 0x00 ‖ message ‖ 0x00 ‖ decimal(received_at.UnixNano()))[:16])` so distinct malformed invocations are never deduplicated. Possible messages: `hook payload exceeds the size limit`, `read hook payload: …`, `hook payload is not valid JSON`, `hook payload has no session_id`, `hook payload has no hook_event_name`.
- **Source:** `go/internal/hooks/normalize.go` (RejectedObservation); `go/internal/cli/run.go`
- **Decision:** D-028
- **Verified by:** UNTESTED (MANUAL step M2)

### Deterministic event IDs

#### HK-024 Event ID derivation
- **Behavior:** For accepted payloads, `event_id` MUST be `"hook-" + lowercase hex of the first 16 bytes of SHA-256` over the `encoding/json` serialization of the struct `{provider, host_id, native_session_id, event_type, time_bucket (omitempty int64), data}` (field order as listed, JSON names as listed), where `data` is the full `ObservationData` with `initial_request.observed_at`, `summary.observed_at`, and every `frontend_bindings[i].observed_at` replaced by the zero time (`0001-01-01T00:00:00Z`). `time_bucket` MUST be included only when `data.native_event_id` is empty and MUST equal `received_at.Unix()` (whole UTC seconds).
- **Source:** `go/internal/hooks/normalize.go` (deterministicEventID)
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` (same `prompt_id` 1 s apart → same ID); `go/internal/hooks/normalize_test.go` TestLifecycleEventIDSeparatesLaterResume (no native ID: +100 ms same ID, +2 s different ID)

#### HK-025 Event ID consequences
- **Behavior:** Implementations MUST reproduce these consequences of HK-024: (a) the reception bucket is 1 s wide and aligned to second boundaries, so duplicates 100 ms apart that straddle a second boundary get different IDs; (b) with a native event ID there is no time component, so two deliveries of the same event type with identical allowlisted data in the same prompt/turn collapse permanently (e.g. a repeated identical `PermissionRequest` within one turn whose payload carries `prompt_id`/`turn_id`); (c) because `data` includes `process_id` and frontend bindings (session/pane, not time), the same provider event delivered under a different process or pane yields a different ID; (d) `prompt_id` takes precedence over `turn_id` over `tool_use_id`.
- **Source:** `go/internal/hooks/normalize.go`
- **Decision:** D-028
- **Verified by:** UNTESTED (b–d)

### Example payloads (sanitized)

Claude `UserPromptSubmit` stdin (run with Zellij variables unset):

```json
{"session_id":"11111111-2222-3333-4444-555555555555","transcript_path":"/tmp/x.jsonl","cwd":"/tmp","hook_event_name":"UserPromptSubmit","prompt_id":"p-1","permission_mode":"default","prompt":"  Fix   the\n\nflaky test  ","unknown_field":"drop me"}
```

Resulting outbox envelope (`host_id` and `process_id` sanitized):

```json
{"schema_version":1,"event_id":"hook-f8bc2ad3042289e9a6b615282f816831","provider":"claude","host_id":"HOST","native_session_id":"11111111-2222-3333-4444-555555555555","event_type":"turn.prompted","source":"hook","received_at":"2026-10-27T16:31:34.720315Z","data":{"native_event":"UserPromptSubmit","native_event_id":"p-1","cwd":"/tmp","initial_request":{"text":"Fix the flaky test","source":"hook_first_observed_user_prompt","observed_at":"2026-10-27T16:31:34.720315Z"},"attention_type":"none","permission_mode":"default","process_id":4242}}
```

Codex `Stop` stdin (with `ZELLIJ_SESSION_NAME=work`, `ZELLIJ_PANE_ID=4`):

```json
{"session_id":"thread-0001","turn_id":"turn-7","hook_event_name":"Stop","cwd":"/work/repo","model":"gpt-test","permission_mode":"default","transcript_path":null,"last_assistant_message":"Done.\n\n  All tests pass.","stop_hook_active":false}
```

```json
{"schema_version":1,"event_id":"hook-ea7cf4b2bebae8c97b1ebb231920d336","provider":"codex","host_id":"HOST","native_session_id":"thread-0001","event_type":"turn.stopped","source":"hook","received_at":"2026-10-27T16:33:59.993892Z","data":{"native_event":"Stop","native_event_id":"turn-7","cwd":"/work/repo","model":"gpt-test","summary":{"text":"Done. All tests pass.","source":"hook_last_assistant_message","observed_at":"2026-10-27T16:33:59.993892Z"},"attention_type":"none","permission_mode":"default","frontend_bindings":[{"kind":"zellij","session":"work","pane_id":"4","state":"observed","observed_at":"2026-10-27T16:33:59.993892Z"}],"process_id":4242}}
```

Claude `Notification` (with Zellij vars and `CLAUDE_CODE_BRIDGE_SESSION_ID` set):

```json
{"session_id":"sess-0001","hook_event_name":"Notification","cwd":"/work/repo","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash","transcript_path":"/x"}
```

```json
{"schema_version":1,"event_id":"hook-862c29f8eba0658e415b32a2fc4ef20f","provider":"claude","host_id":"HOST","native_session_id":"sess-0001","event_type":"notification.received","source":"hook","received_at":"2026-10-27T16:34:00.10318Z","data":{"native_event":"Notification","cwd":"/work/repo","attention_type":"approval_required","attention_detail":"Claude needs your permission to use Bash","frontend_bindings":[{"kind":"zellij","session":"work","pane_id":"4","state":"observed","observed_at":"2026-10-27T16:34:00.10318Z"},{"kind":"claude_remote_control","state":"observed","observed_at":"2026-10-27T16:34:00.10318Z"}],"process_id":4242}}
```

Other observed mappings (data excerpts): `CwdChanged` with `new_cwd:"/work/repo/sub"` → `"event_type":"workspace.changed","data":{"cwd":"/work/repo/sub",…}`; `PostModelSwitch` with `model:"old-model", to_model:"new-model"` → `"model.changed"`, `"model":"new-model"`; `StopFailure` with `error:"rate_limit", last_assistant_message:"partial"` → `"turn.failed"`, `"attention_type":"error","attention_detail":"rate_limit"`, summary `"partial"`; `SessionEnd` with `reason:"prompt_input_exit"` → `"session.ended"`, `"reason":"prompt_input_exit"`.

Malformed payload (`not json`, or `{"session_id":"s","hook_event_name":"Bogus","cwd":5}`):

```json
{"schema_version":1,"event_id":"hook-88c40d13f273582187f6dd2390ad4681","provider":"codex","host_id":"HOST","event_type":"hook.rejected","source":"hook","received_at":"2026-10-27T16:31:35.484141Z","data":{"native_event":"unknown"},"error":"hook payload is not valid JSON"}
```

---

## Outbox and observation storage

#### OBX-001 Tables
- **Behavior:** Migration v1 MUST create `hook_outbox(id TEXT PRIMARY KEY, received_at TEXT NOT NULL, envelope_json BLOB NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, processed_at TEXT, last_error TEXT)` with index `hook_outbox_pending_idx ON hook_outbox(processed_at, received_at)`, and `observations(id TEXT PRIMARY KEY, schema_version INTEGER NOT NULL, provider TEXT NOT NULL, host_id TEXT NOT NULL, native_session_id TEXT, event_type TEXT NOT NULL, source TEXT NOT NULL, occurred_at TEXT, received_at TEXT NOT NULL, processed_at TEXT, payload_json BLOB NOT NULL, error TEXT)` with index `observations_session_received_idx ON observations(provider, native_session_id, received_at)`; migration v2 adds `observations_received_idx ON observations(received_at DESC, id DESC)`. No code deletes from either table, including when the agent a row refers to is forgotten or pruned (see Known limitations).
- **Source:** `go/internal/storage/sqlite/migrations.go`
- **Decision:** D-022, D-028
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestHookOutboxIsDurableAndIdempotent (indirect)

#### OBX-002 Idempotent enqueue
- **Behavior:** `EnqueueObservation` MUST fail with `event ID, host ID, and provider are required` if any is empty; MUST force `schema_version = 1`; MUST insert `(id = event_id, received_at = formatTimestamp(received_at), envelope_json = json(observation))` with `ON CONFLICT(id) DO NOTHING`; and MUST return `true` iff a row was inserted (`false, nil` for a duplicate). Errors wrap as `encode hook observation:`, `enqueue hook observation:`, `read hook enqueue result:`.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-028
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestHookOutboxIsDurableAndIdempotent

#### OBX-003 Timestamp storage format
- **Behavior:** Every timestamp column written by this subsystem MUST be UTC formatted with layout `2006-01-02T15:04:05.000000000Z07:00` (always 9 fractional digits, `Z` suffix), so lexical order equals chronological order. (The JSON envelope itself uses Go's RFC 3339 with trailing zeros trimmed.)
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Verified by:** UNTESTED (direct)

#### OBX-004 Outbox row states
- **Behavior:** A row's state MUST be derived, not stored: **pending** = `processed_at IS NULL AND attempts < 5`; **processed** = `processed_at IS NOT NULL`; **quarantined** = `processed_at IS NULL AND attempts >= 5` (never selected again, retained with `last_error`). `MaxHookAttempts = 5`.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-028
- **Verified by:** pending→processed: `go/internal/storage/sqlite/hooks_test.go`; quarantine after 5 failures UNTESTED

#### OBX-005 Pending selection
- **Behavior:** `PendingObservations(limit)` MUST return `[]` (no query) for `limit <= 0`; otherwise select `envelope_json` of pending rows `ORDER BY received_at, id LIMIT limit` and JSON-decode each. A single undecodable row MUST fail the whole call (`decode pending hook observation: …`) without incrementing any attempt counter.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` (selection); decode-failure behavior UNTESTED

#### OBX-006 Mark processed
- **Behavior:** `MarkObservationProcessed(id, t)` MUST execute `UPDATE hook_outbox SET attempts = attempts + 1, processed_at = t, last_error = NULL WHERE id = ? AND processed_at IS NULL` and MUST return `sql.ErrNoRows` if no row changed.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` (happy path); `ErrNoRows` UNTESTED

#### OBX-007 Mark failed with bounded error text
- **Behavior:** `MarkObservationFailed(id, err)` MUST set `attempts = attempts + 1, last_error = message` for an unprocessed row, where `message = err.Error()` (or `unknown processing error` for nil) truncated to the first **1024 bytes** (byte-level; may split a UTF-8 sequence), and MUST return `sql.ErrNoRows` if no row changed.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-028
- **Verified by:** UNTESTED

#### OBX-008 Observation journal write
- **Behavior:** `RecordObservation(obs, processedAt)` MUST insert into `observations` with `id = event_id`, `schema_version`, `provider`, `host_id`, `native_session_id` (NULL when empty), `event_type`, `source`, `occurred_at` (NULL when nil), `received_at`, `processed_at`, `payload_json = json(obs.data)` (the allowlisted data, including any previews), `error` (NULL when empty), with `ON CONFLICT(id) DO NOTHING`.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-028, D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` (one row after record)

#### OBX-009 Event-list projection excludes previews
- **Behavior:** `ListEvents` MUST project each journal row to `AgentEvent` exposing only `native_event`, `previous_live_state`, `live_state`, `reason` from `payload_json` (plus row metadata and the agent ID from a LEFT JOIN on `(host_id, provider, native_session_id)`); preview text MUST NOT appear. Limit defaults to `registry.DefaultEventLimit` and is capped at `registry.MaxEventLimit`; the newest window is selected (`received_at DESC, id DESC`) and returned in chronological order.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads

#### OBX-010 File permissions
- **Behavior:** `Open` MUST create a missing DB parent directory with mode `0700` (an existing directory's mode is never changed) and MUST `chmod 0600` the DB file on every open. SQLite's `-wal`/`-shm` sidecars are created by SQLite under the process umask (observed `0644` with umask `022`) and are protected only by the parent directory.
- **Source:** `go/internal/storage/sqlite/store.go`; `go/internal/config/paths.go` (EnsurePrivateDir)
- **Verified by:** UNTESTED

---

## Drain loop and projection

#### HK-026 Server drain triggers
- **Behavior:** The server MUST run one drain goroutine that calls `Drain(ctx, 128)` (a) whenever the wake channel fires and (b) every `DefaultHookDrainInterval = 2 s` (ticker). The wake channel MUST have capacity 1 and a non-blocking send, so concurrent wakes coalesce. The server MUST send one wake immediately after the drain goroutine and HTTP listener start (draining any backlog spooled while it was down). A drain error MUST be logged at Warn with pending/processed/failed counts; successful processing logs at Debug.
- **Source:** `go/internal/server/run.go`
- **Decision:** D-028
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (end-to-end); fallback-tick behavior UNTESTED

#### HK-027 Drain batch semantics
- **Behavior:** `Drain(limit)` MUST use `DefaultDrainLimit = 128` when `limit <= 0`, MUST serialize concurrent drains with a mutex, and for each pending observation (OBX-005 order) MUST: take `processedAt = now().UTC()`; run `process`; on error call `MarkObservationFailed` (OBX-007) and count `Failed`; on success call `MarkObservationProcessed` (OBX-006), counting `Failed` if that errors, else `Processed`. One failure MUST NOT stop the batch. It MUST return `DrainReport{Pending, Processed, Failed}` and `errors.Join` of all errors (a mark-failed error is reported as `<err>; mark failed: <markErr>`). A backlog larger than 128 is drained 128 per trigger.
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/processor_test.go` TestProcessorBuildsAgentPreviewFromHookSequence (Processed counts); failure path UNTESTED

#### HK-028 Processor construction
- **Behavior:** `NewProcessor` MUST require a non-empty host ID (`hook processor host ID is required`) and a store (`hook processor store is required`); defaults: new `workspace.Inspector`, `time.Now`, `identity.NewUUID`.
- **Source:** `go/internal/hooks/processor.go`
- **Verified by:** UNTESTED

#### HK-029 Per-observation validation
- **Behavior:** `process` MUST fail (→ retry, eventually quarantine) with `unsupported observation schema version <n>` if `schema_version != 1`, and with `observation identity is invalid for this host` if `event_id` or `provider` is empty or `host_id` differs from the processor's host. A `hook.rejected` observation or one without `native_session_id` MUST only be journaled (OBX-008) and then succeed. An observation with a provider other than claude/codex MUST be journaled and then fail with `unsupported observation provider "<p>"`.
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-028
- **Verified by:** UNTESTED

#### HK-030 Lookup, apply, journal, persist
- **Behavior:** For an accepted observation, `process` MUST: look up the agent by `(host_id, provider, native_session_id)` (`registry.ErrNotFound` means new; other errors fail); compute the projected agent (HK-031…HK-041); set `data.previous_live_state` to the existing row's `live_state` (only if one existed) and `data.live_state` to the projected `live_state`; journal the observation (OBX-008); then either drop (HK-035) or `Upsert` the agent. Journal write and upsert are separate statements (no transaction); on retry the journal insert is a no-op.
- **Source:** `go/internal/hooks/processor.go`
- **Verified by:** `go/internal/hooks/processor_test.go`, `go/internal/hooks/processor_test.go`; journal `live_state` values: `go/internal/cli/run_test.go` (events output) — exact previous/live recording UNTESTED

#### HK-031 New agent record from a hook
- **Behavior:** When no agent exists, the processor MUST allocate a new UUID (`allocate noc agent ID: …` on failure) and start from `{schema_version 1, id, host_id, provider, native_session_id, origin: "hook", live_state: live, runtime_state: unknown, activity_state: unknown, attention: {type: none}, first_seen_at: eventAt, last_seen_at: eventAt}`, where `eventAt = received_at` (UTC), or `now()` if `received_at` is zero.
- **Source:** `go/internal/hooks/processor.go`
- **Verified by:** `go/internal/hooks/processor_test.go`

#### HK-032 Recency gate
- **Behavior:** `stateIsCurrent` MUST be `true` for a new agent, else `eventAt >= existing.last_seen_at`. Only current events change live/runtime/activity/attention (`applyState`). `last_seen_at` MUST advance only when `eventAt` is strictly later.
- **Source:** `go/internal/hooks/processor.go`
- **Verified by:** UNTESTED (out-of-order case)

#### HK-033 Field-by-field merge rules
- **Behavior:** Regardless of `stateIsCurrent` unless noted:

  | Agent field | Rule |
  |---|---|
  | `name` | set to `data.name` when non-empty AND (agent name empty OR event current). Via `Upsert`, a non-empty name that differs from the stored snapshot name clears `user_name` (provider rename from hooks wins, D-042) |
  | `source_kind` | set to `data.source_kind` only if currently empty |
  | `model`, `model_source`, `model_observed_at` | when `data.model` non-empty and (`model_observed_at` nil or `eventAt >= model_observed_at`): model, `"hook"`, eventAt |
  | `initial_request` | only if agent has none: clone of `data.initial_request` with `observed_at = eventAt` (first observed prompt is never replaced, including by a provider-native preview already present) |
  | `summary`, `summary_source`, `summary_truncated`, `summary_updated_at` | when `data.summary` present and (`summary_updated_at` nil or `eventAt >=` it): text, source, truncated, eventAt |
  | `workspace` | when `data.cwd` non-empty and (no workspace or `eventAt >= workspace.observed_at`): `workspaceInspector.Inspect(ctx, cwd, "unknown", eventAt)`; a nil result leaves the workspace unchanged |
  | `frontend_bindings` | merged (HK-036), then cleared to `nil` if `!HooksSeeItsTerminal(agent)` (HK-037) |
  | `process_id` | set when `data.process_id > 0` and `eventAt >= last_seen_at` (never regress to an older process) |
  | `last_seen_at` | max(existing, eventAt) |

- **Source:** `go/internal/hooks/processor.go`; `go/internal/storage/sqlite/store.go` (Upsert)
- **Decision:** D-027, D-040, D-042
- **Verified by:** `go/internal/hooks/processor_test.go` (initial request kept, summary/source updated, model, workspace kind `directory`, binding); name/user_name, model recency, process ID UNTESTED

#### HK-034 Journal live-state annotation
- **Behavior:** The journaled `payload_json.live_state` MUST be the agent's projected live state and `previous_live_state` the prior row's (absent for a new agent).
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-033
- **Verified by:** UNTESTED (unit)

#### HK-035 Drop an ended, never-engaged session
- **Behavior:** After journaling, if the projected agent is `not_live` and `NeverEngaged` (blank `user_name`, blank `name`, and no or blank `initial_request.text`): if it did not previously exist, MUST NOT create a row; if it existed and the store implements `registry.Forgetter`, MUST delete it (`ErrNotFound` ignored); if the store is not a Forgetter, fall through to `Upsert`. A session with a provider name (`session_title`) or a prompt is kept.
- **Source:** `go/internal/hooks/processor.go`; `go/pkg/api/v1/types.go` (NeverEngaged); `go/internal/storage/sqlite/store.go` (Forget)
- **Decision:** D-047
- **Verified by:** `go/internal/hooks/processor_test.go` TestProcessorDropsASessionThatEndsWithoutAConversation; prompted session kept: `go/internal/hooks/processor_test.go`

#### HK-036 Frontend binding merge
- **Behavior:** Each observed binding MUST get `observed_at = eventAt`; a binding with the same `(kind, session, pane_id)` as an existing one replaces it only if not older; otherwise it is appended. Existing bindings not re-observed are kept.
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-027
- **Verified by:** `go/internal/hooks/processor_test.go` (single binding retained after later events without Zellij env)

#### HK-037 Background-Claude binding suppression
- **Behavior:** If the agent is a Claude agent with `source_kind == "background"`, all frontend bindings MUST be cleared on every hook projection (hooks run in Claude's daemon and report the daemon's pane). The processor learns `source_kind` only from `data.source_kind` or from reconciliation; until then bindings are kept. Reconciliation applies the same predicate.
- **Source:** `go/internal/hooks/processor.go`; `go/pkg/api/v1/types.go` (HooksSeeItsTerminal); `go/internal/reconcile/reconciler.go`
- **Decision:** D-050
- **Verified by:** `go/internal/reconcile/reconciler_bindings_test.go` TestBackgroundClaudeSessionsDropHookPaneBindings (reconciler side); processor side UNTESTED

### State application (`applyState`, current events only)

#### HK-038 Session end
- **Behavior:** `session.ended` MUST set `live_state = not_live`, `runtime_state = stopped`, `activity_state = idle`, `attention = {type: none}`, `ended_at = eventAt`, and every frontend binding's `state = "disconnected"` with `observed_at = eventAt`, and MUST return without the live/attention logic below.
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/processor_test.go` TestProcessorEndsHookOnlyAgentOnSessionEnd

#### HK-039 Runtime/activity per event and revival
- **Behavior:** `session.started` → running/idle; `turn.prompted` → running/working + `last_activity_at`; `turn.stopped` → running/idle + `last_activity_at`; `turn.failed` → failed/idle + `last_activity_at`; `attention.approval_required` → running/waiting; `notification.received` → activity `waiting` only if attention ≠ none/empty (runtime unchanged); others unchanged. After any non-end event the agent MUST be `live` with `ended_at = nil` (an explicit start or any later hook revives an ended row).
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/processor_test.go` (working → idle); others UNTESTED

#### HK-040 Attention clearing
- **Behavior:** When the observation's attention is `none` (or empty), attention MUST be cleared to `{type: none}` only for `session.started`, `turn.prompted`, `turn.stopped`; for `workspace.changed`, `model.changed`, `hook.observed`, and attention-less notifications it MUST be left unchanged. Attention is never cleared by a timer.
- **Source:** `go/internal/hooks/processor.go`
- **Decision:** D-044
- **Verified by:** UNTESTED

#### HK-041 Attention setting
- **Behavior:** When the observation's attention is not `none`, the agent's attention MUST be replaced with `{type, since: eventAt, detail: data.attention_detail}` only if the type or detail differs from the current attention; an identical repeat MUST preserve the original `since`. `native_request_id` is never set by hooks.
- **Source:** `go/internal/hooks/processor.go`
- **Verified by:** UNTESTED

---

## Process lineage

#### PL-001 Provider ancestor resolution
- **Behavior:** `ProviderProcessID("claude", "codex")` MUST take one process-table snapshot and walk from `os.Getpid()` upward at most `maxAncestry = 12` steps: at each step take the parent; stop with "not found" if the current PID has no entry or the parent is `<= 1`; return the parent if its command name is in the wanted set (trimmed, non-empty names). The hook process itself is never matched; the nearest matching ancestor wins (so a `codex` under `claude` resolves to `codex`). The same name set is used for both providers. No names → not found.
- **Source:** `go/internal/proclineage/proclineage.go`; `go/internal/hooks/normalize.go`
- **Decision:** D-040
- **Verified by:** `go/internal/proclineage/proclineage_test.go` TestProviderProcessIDWalksToAnAncestor (negative cases only); positive match UNTESTED

#### PL-002 Process-table snapshot
- **Behavior:** The snapshot MUST run `ps -eo pid=,ppid=,comm=` once per call with a 2 s timeout (`snapshotTimeout`, from `context.Background()`; `ps` is killed when it fires), capturing stdout into an in-memory buffer with stderr discarded, parse each line's whitespace fields, skip lines with fewer than 3 fields or non-integer PID/PPID, and map `pid → ppid` and `pid → commandName(fields[2])`. Only the third whitespace field is used (a `comm` containing spaces is cut there). An exec error (including the timeout or a non-zero exit) or an empty table MUST yield "not found". The command sets no `WaitDelay`; `ps` starts no child processes, so nothing else can hold its stdout pipe open past the kill. There is no cache.
- **Source:** `go/internal/proclineage/proclineage.go`
- **Decision:** D-040
- **Verified by:** UNTESTED

#### PL-003 Command-name reduction
- **Behavior:** `commandName` MUST trim, cut at the first space, and return `filepath.Base` (`/opt/x/bin/codex` → `codex`, `claude bg-pty-host` → `claude`, empty → empty). A provider running under an interpreter (e.g. `comm` = `node`) does not match and yields no PID.
- **Source:** `go/internal/proclineage/proclineage.go`
- **Verified by:** `go/internal/proclineage/proclineage_test.go` TestCommandNameReducesToABareExecutable

#### PL-004 Fail-open
- **Behavior:** Any lineage failure MUST result in `ProcessID = 0` (field omitted) and MUST NOT cause the ingest to fail or emit output.
- **Source:** `go/internal/proclineage/proclineage.go`; `go/internal/hooks/normalize.go`
- **Decision:** D-040
- **Verified by:** `go/internal/proclineage/proclineage_test.go` (no panic, no match)

#### PL-005 Negative-only liveness
- **Behavior:** `IsRunning(pid)` MUST return `false` for `pid <= 1`; otherwise send signal 0: `nil` → true, `ESRCH` → false, any other error (incl. `EPERM`) → true. Reconciliation MUST retire a hook-only row (`snapshot_confirmed_at` nil and `origin != "reconcile"`) only when `process_id > 0` and `!IsRunning(process_id)`, setting `not_live`, `stopped`, `ended_at = now`, and journaling reason `hook_session_process_exited`.
- **Source:** `go/internal/proclineage/proclineage.go`; `go/internal/reconcile/reconciler.go`
- **Decision:** D-040
- **Verified by:** `go/internal/proclineage/proclineage_test.go` TestIsRunningDetectsAnExitedProcess; `go/internal/reconcile/reconciler_test.go` TestReconcilerRetiresHookOnlyAgentWhoseProcessExited

---

## Provider hook configuration (`hookconfig`)

#### HC-001 Configuration paths
- **Behavior:** `ResolvePaths` MUST use `CLAUDE_CONFIG_DIR` (trimmed) or `$HOME/.claude` → `<dir>/settings.json`; `CODEX_HOME` (trimmed) or `$HOME/.codex` → `<dir>/hooks.json` and `<dir>/config.toml` (each `filepath.Clean`ed). Home resolution failure → `resolve home directory: …`.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** UNTESTED (tests pass explicit paths)

#### HC-002 Required event lists
- **Behavior:** `RequiredEvents` MUST return a copy of: Claude — `SessionStart, SessionEnd, UserPromptSubmit, Stop, StopFailure, Notification, PermissionRequest, CwdChanged, PostModelSwitch` (9); Codex — `SessionStart, SessionEnd, UserPromptSubmit, Stop, PermissionRequest, Interrupt` (6); any other provider — empty. `WorktreeCreate`, subagent, task, tool, and compaction hooks MUST NOT be installed.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-029
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (uninstall removes `len(RequiredEvents(claude))` = 9 handlers)

#### HC-003 Managed command string
- **Behavior:** The installed command MUST be `shellQuote(canonicalExecutable) + " hook ingest --provider " + provider`, where `shellQuote` wraps in single quotes and replaces each `'` with `'"'"'`, and `canonicalExecutable` = `filepath.Clean(TrimSpace(path))` (empty/`.` → empty → `noc executable path is required`). Symlinks MUST NOT be resolved: the command names the path it was given, so a package manager's stable name (Homebrew's `bin/noc`) keeps working when an upgrade replaces the versioned file behind it. Whether a handler belongs to an executable (HC-015, HC-017) is still decided with symlinks resolved on both sides.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (indirect)

#### HC-004 Written JSON shape
- **Behavior:** For each required event, Install MUST append to `hooks.<Event>` (array) one new group `{"hooks":[{"type":"command","command":<cmd>,"timeout":3}]}` — no `matcher` key (applies to all matchers) — after any existing groups. The same shape is used for Claude `settings.json` and Codex `hooks.json`. Example Claude result for an executable at `/home/user/bin/noc` (keys are sorted; other events elided):

  ```json
  {
    "hooks": {
      "CwdChanged": [
        {
          "hooks": [
            {
              "command": "'/home/user/bin/noc' hook ingest --provider claude",
              "timeout": 3,
              "type": "command"
            }
          ]
        }
      ],
      "Notification": [ … same group … ],
      "PermissionRequest": [ … ],
      "PostModelSwitch": [ … ],
      "SessionEnd": [ … ],
      "SessionStart": [ … ],
      "Stop": [ … ],
      "StopFailure": [ … ],
      "UserPromptSubmit": [ … ]
    }
  }
  ```

  Codex `hooks.json` is identical in shape with keys `Interrupt, PermissionRequest, SessionEnd, SessionStart, Stop, UserPromptSubmit` and `--provider codex`.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-008, D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go`, `go/internal/hookconfig/hookconfig_test.go`; exact bytes MANUAL step M4

#### HC-005 Exact-argv ownership detection
- **Behavior:** A command string is noc-managed for provider P iff `splitShellWords` succeeds and the words are exactly `[<x>, "hook", "ingest", "--provider", P]` or `[<x>, "hook", "ingest", "--provider=P"]` where `filepath.Base(<x>) == "noc"` (any directory). Anything else — extra flags (`--database`), env prefixes, `echo noc …`, a different provider, basename ≠ `noc`, unbalanced quotes — is NOT managed and MUST be left untouched.
- **Source:** `go/internal/hookconfig/hookconfig.go` (MatchesCommand)
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` TestMatchesCommandRequiresExactManagedArgv

#### HC-006 Shell-word splitting
- **Behavior:** `splitShellWords` MUST: treat space, tab, CR, LF as separators outside quotes; take single-quoted text literally; in double quotes treat `\` as escaping the next character; outside quotes treat `\` as escaping the next character; count `''`/`""` as an (empty) word; perform no expansion or substitution; and fail (`ok=false`) on a trailing escape or unterminated quote.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (quoted path with spaces; unterminated quote)

#### HC-007 Remove managed handlers
- **Behavior:** `removeManagedHooks(root, P)` MUST iterate every key of a top-level `hooks` object (if absent or not an object, remove nothing); for array values, inspect each group: non-object groups or groups whose `hooks` is not an array are kept verbatim; within a group remove each handler that is an object with a string `command` that is managed for P (HC-005), counting removals; a group left with zero handlers is dropped (even if it had a `matcher`); a group with remaining handlers keeps all its other keys; an event left with zero groups is deleted. Non-array event values are left untouched. Returns the removal count.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (unrelated `matcher: "startup"` hook preserved)

#### HC-008 Install algorithm and idempotency
- **Behavior:** `Install(P, exe, paths)` MUST: canonicalize exe (HC-003); for Codex, refuse if `config.toml` already holds a managed hook (HC-014); pick `settings.json` (Claude) or `hooks.json` (Codex); follow a symlink (HC-009); read the JSON object (HC-010); remove all managed handlers for P (HC-007); ensure `hooks` is an object (missing/`null` → created; other type → `update <path>: top-level hooks value is not an object`); for each required event require the value to be absent/`null` or an array (`update <path> hook <Event>: hook event value is not an array`) and append the managed group; serialize (HC-011); write atomically only if `TrimSpace(original) != TrimSpace(updated)`. Return `Change{Provider, Path (resolved), Changed, Removed}`. Re-running with the same executable MUST be a byte-level no-op (`Changed=false`, `Removed` = handlers re-added). Re-running with a different executable MUST replace the old managed handlers.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` TestInstallIsIdempotentAndPreservesUnrelatedClaudeSettings; executable-replacement UNTESTED

#### HC-009 Symlink following
- **Behavior:** If the configuration path is a symlink (`Lstat`), the write target MUST be `filepath.EvalSymlinks(path)` so the link itself is preserved and its target updated; a dangling link MUST fail `resolve hook configuration symlink <path>: …`; a missing path is used as is. `Change.Path` reports the resolved target.
- **Source:** `go/internal/hookconfig/hookconfig.go` (writableConfigPath)
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` TestInstallPreservesConfigurationSymlink

#### HC-010 Reading and malformed-file refusal
- **Behavior:** `readJSONObject` MUST: treat a missing file as `{}` with mode `0600`; read at most `maxConfigBytes = 1 MiB` (larger → `read <path>: file exceeds 1048576 bytes`); keep the existing permission bits; treat whitespace-only content as `{}`; and fail with `parse <path>: …` for invalid JSON or a non-object top level, leaving the file untouched.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` TestInstallRejectsMalformedConfigurationWithoutReplacingIt

#### HC-011 Serialization
- **Behavior:** Output MUST be `json.MarshalIndent(root, "", "  ")` plus a trailing newline. Because the document round-trips through `map[string]any`, a rewrite: sorts all object keys alphabetically, re-indents with two spaces, encodes numbers via float64 (e.g. `1.0` → `1`, integers beyond 2^53 lose precision), and HTML-escapes `<`, `>`, `&` in strings (as `\u003c`, `\u003e`, `\u0026`). Semantics of unrelated settings are preserved; formatting is not.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** UNTESTED (key sorting observed MANUAL step M4)

#### HC-012 Atomic write
- **Behavior:** `atomicWrite` MUST `MkdirAll(dir, 0700)`, create a temp file `.noc-hooks-*` in the same directory, `chmod` it to the preserved mode (or `0600` for new files), write, `fsync`, close, and `rename` over the target; on any failure the temp file MUST be removed and the original left intact. The directory is not fsynced.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (rename into symlink target); failure cleanup UNTESTED

#### HC-013 Uninstall algorithm
- **Behavior:** `Uninstall(P, paths)` MUST resolve the same preferred path and symlink, return `Change{Path}` with no error when the file is missing, return without writing when zero managed handlers are found, else remove them (HC-007) and atomically write if changed. It removes managed handlers for any executable path (basename `noc`). An emptied hooks object remains as `"hooks": {}`. It MUST NOT modify Codex `config.toml`.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (Removed = 9, unrelated hook preserved)

#### HC-014 Codex inline-TOML conflict
- **Behavior:** Before writing for Codex, Install MUST inspect `config.toml`; if any managed Codex command is found there (HC-016), it MUST fail with `<config.toml> contains an existing noc hook; remove it before setup can manage <hooks.json>` and MUST NOT create or modify `hooks.json`.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-030
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` TestInstallCodexRefusesCompetingInlineManagedHook

#### HC-015 JSON inspection
- **Behavior:** For each JSON file inspected (Claude `settings.json`; Codex `hooks.json`): a missing file is silently skipped (not listed in `files`, no error); a read failure or oversize file appends the error text to `errors`; otherwise the path is appended to `files`; a parse failure appends `parse <path>: …`; if `hooks` is not an object nothing is counted. For every key of `hooks` (any event name, not only required ones), every string value under a key named `command` at any nesting depth is considered; if managed for P (HC-005) the event is marked *covered*, and if additionally `canonicalPath(words[0]) == canonicalExecutable` it is marked *exact*. `type`, `timeout`, and `matcher` are not checked.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-029, D-030
- **Verified by:** `go/internal/doctor/doctor_test.go` (complete coverage), `go/internal/doctor/doctor_test.go` (non-exact executable)

#### HC-016 Codex `config.toml` line scanner
- **Behavior:** The scanner MUST read at most 1 MiB (oversize → error) and, line by line: a line matching `^\s*\[\[hooks\.([A-Za-z0-9_]+)(?:\.hooks)?\]\]\s*(?:#.*)?$` sets the current event (and clears the current table); a line matching `^\s*\[([^][]+)]\s*(?:#.*)?$` sets the current table and clears the event; within table `features`, a line whose space-stripped form starts with `hooks=false` or `codex_hooks=false` sets `hooks_disabled`; within an event, a line matching `^\s*command\s*=\s*(.+?)\s*$` whose value is a single-quoted literal or a Go-unquotable double-quoted string, and which is managed for Codex, sets `managed_in_codex_config`, marks the event covered, and exact if the executable matches. Inline-table/array forms, multi-line strings, and trailing comments after the string are not recognized. An existing `config.toml` is always listed in `files` (even with no hooks).
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-029, D-030
- **Verified by:** `go/internal/doctor/doctor_test.go` TestCodexTOMLCoverageAndDisabledFeature; `go/internal/hookconfig/hookconfig_test.go` TestInstallCodexUsesHooksJSONAndDetectsDisabledFeature; `codex_hooks=false` spelling UNTESTED

#### HC-017 Inspection result and completeness
- **Behavior:** `Inspect` MUST return `{provider, paths, files, missing_events?, mismatched_executable_events?, errors?, hooks_disabled?, managed_in_codex_config?}` where `paths` = `[settings.json]` (Claude) or `[hooks.json, config.toml]` (Codex), `missing_events` = required events not covered, `mismatched_executable_events` = required events not *exact* (a superset of missing), both sorted. `files` has no `omitempty` and is `null` when no file exists. An unsupported provider yields error `unsupported provider "<p>"`. `Complete()` MUST be `errors` empty AND not `hooks_disabled` AND no missing AND no mismatched events.
- **Source:** `go/internal/hookconfig/hookconfig.go`
- **Decision:** D-029
- **Verified by:** `go/internal/hookconfig/hookconfig_test.go` (disabled → incomplete with no missing events); `go/internal/setup/setup_test.go` (missing)

---

## Setup CLI (`noc setup`)

This section is about provider hooks. `noc setup skill`, which installs the
noc skill and touches no hook, is specified in [03-cli.md](03-cli.md)
CLI-048..CLI-051; an argument `skill` hands the command to it before any of
the following applies.

#### HC-018 Arguments and exit codes
- **Behavior:** `noc setup [--check|--uninstall] [--json] [--executable PATH] [claude|codex]`. MUST exit `2` with stderr `noc setup: --check and --uninstall are mutually exclusive` if both are set; `2` with `usage: noc setup [--check|--uninstall] [claude|codex]` for more than one positional; `2` with `noc setup: unsupported provider "<x>"` for an unknown provider; `2` on flag parse errors; `0` on `-h`. A `setup.Run` error MUST print `noc: <err>` to stderr and exit `1`. Otherwise print the report (JSON with `--json`, 2-space indented, else human) to stdout and exit `1` if `HasFailures()` else `0`. Configuration paths always come from `hookconfig.ResolvePaths` (no path flags).
- **Source:** `go/internal/cli/run.go` (runSetup)
- **Decision:** D-030
- **Verified by:** `go/internal/cli/run_test.go` TestSetupRejectsConflictingActions; other branches UNTESTED

#### HC-019 Executable resolution
- **Behavior:** The registered executable MUST be `--executable` or, without it, the name the process was started by: `os.Args[0]`, looked up on `PATH` when it has no directory separator, made absolute, when that name resolves (symlinks followed) to the same file as `os.Executable()`; otherwise `os.Executable()`. Either is made absolute and its symlinks are kept. It MUST lead to a regular file with at least one execute bit; otherwise `resolve noc executable: …`, `inspect noc executable "<p>": …`, or `noc executable "<p>" is not an executable regular file`.
- **Source:** `go/internal/setup/setup.go`
- **Decision:** D-029, D-030
- **Verified by:** `go/internal/setup/setup_test.go` `TestResolveExecutable`, `TestHooksSurviveAnUpgradeBehindASymlink`

#### HC-020 Orchestration
- **Behavior:** `setup.Run` MUST default action to `install`, reject other actions (`unsupported setup action`) and providers (`unsupported setup provider`), default providers to `[claude, codex]` (no detection of installed providers), and for each provider in order: run Install or Uninstall (check does nothing), record `path/changed/removed` and any error string, then always run `Inspect` and set `complete`. For `uninstall`, if the inspection shows `managed_in_codex_config`, `error` MUST be set (overwriting any prior error) to `<config.toml> still contains a noc hook and was not modified`.
- **Source:** `go/internal/setup/setup.go`
- **Decision:** D-030
- **Verified by:** `go/internal/setup/setup_test.go` TestInstallCheckAndUninstall; uninstall TOML error UNTESTED

#### HC-021 Failure predicate
- **Behavior:** `HasFailures()` MUST be true iff any result has a non-empty `error`, or the action is not `uninstall` and any result is not `complete`. Consequently `install` exits 1 when Codex hooks are disabled in `[features]` even though `hooks.json` was written, and `--check` exits 1 for any missing/stale handler.
- **Source:** `go/internal/setup/setup.go`
- **Verified by:** `go/internal/setup/setup_test.go` TestCheckReportsMissingHooks; `go/internal/setup/setup_test.go`

#### HC-022 JSON report shape
- **Behavior:** `--json` MUST emit `{"schema_version":1,"action":"install|check|uninstall","executable_path":"<abs>","results":[{"provider", "path"?, "changed"?, "removed"?, "complete", "inspection":{…HC-017…}, "error"?}]}`.
- **Source:** `go/internal/setup/setup.go`
- **Verified by:** UNTESTED (MANUAL step M4)

#### HC-023 Human output
- **Behavior:** First line `noc setup <action> using <executable>`. Per provider, if `error` is set: `  <p>: error — <error>` and nothing else. Otherwise: install → `  <p>: installed in <path>` (changed) or `  <p>: already configured in <path>`, followed by inspection lines if incomplete; check → `  <p>: configured` or `  <p>: incomplete` + inspection lines; uninstall → `  <p>: removed <n> hook handlers from <path>` (changed) or `  <p>: no managed hooks found in <path>`. For Codex with action install (and no error) always add `    Review and trust the new hook definitions when Codex prompts you.` (even when unchanged). Inspection lines, each only when non-empty: `    errors: <a; b>`, `    hooks are disabled in Codex [features]`, `    missing events: <A, B>`, `    events using another executable: <A, B>` (printed even when it duplicates the missing list).
- **Source:** `go/internal/cli/run.go`
- **Decision:** D-029
- **Verified by:** UNTESTED (MANUAL step M4)

---

## Doctor (`noc doctor`)

#### DOC-001 Arguments and exit codes
- **Behavior:** `noc doctor [--json] [--socket PATH] [--database PATH] [--timeout DURATION]` (defaults: resolved socket and database, `15s`). Path resolution failure → `noc: <err>`, exit 1; flag error → 2; `-h` → 0; any positional → stderr `noc doctor: unexpected positional arguments`, exit 2; `--timeout <= 0` → stderr `noc doctor: timeout must be positive`, exit 2. The whole run is bounded by the timeout. Exit MUST be `1` iff overall status is `fail`; `warning` exits `0`.
- **Source:** `go/internal/cli/run.go` (runDoctor); `go/internal/doctor/doctor.go` (HasFailures)
- **Decision:** D-029
- **Verified by:** `go/internal/cli/run_test.go` TestDoctorRejectsInvalidTimeout; exit 1 on fail via `go/internal/doctor/doctor_test.go` (report level)

#### DOC-002 Report shape and ordering
- **Behavior:** JSON MUST be `{"schema_version":1,"status":"pass|warning|fail","generated_at":<UTC>,"checks":[{"id","status","summary","detail"?,"remediation"?}]}`. Checks MUST appear in this order: `state`, `runtime`, `server` (+ `server.provider.<p>` per provider reported by the server, only when reachable), provider checks for claude then codex (unless skipped), `hooks.claude`, `hooks.codex`. Overall status MUST be `fail` if any check fails, else `warning` if any warns, else `pass`.
- **Source:** `go/internal/doctor/doctor.go`
- **Decision:** D-029
- **Verified by:** `go/internal/doctor/doctor_test.go` TestRunHealthyInstallation (IDs and pass), `go/internal/doctor/doctor_test.go`

#### DOC-003 Human output
- **Behavior:** First line `noc doctor: <status>`, then a tabwriter (min width 0, tab 4, padding 2, spaces) table with one row `<STATUS IN UPPERCASE>\t<id>\t<summary>` per check, an extra row `\t\t<detail>` when detail is set, and `\t\tNext: <remediation>` when remediation is set. Status words render as `PASS`, `WARNING`, `FAIL`.
- **Source:** `go/internal/cli/run.go`
- **Verified by:** UNTESTED (MANUAL step M5)

#### DOC-004 Defaults
- **Behavior:** Doctor MUST canonicalize the executable (`os.Executable()` or given path; `Clean` + symlinks resolved); Claude dir = trimmed `CLAUDE_CONFIG_DIR` else `$HOME/.claude`; Codex home = trimmed `CODEX_HOME` else `$HOME/.codex`; default provider checks use the Claude and Codex adapters' `Probe`/`DiscoverLive`; health uses the local API client on the socket path. `SkipProviderChecks` exists for tests only (no CLI flag).
- **Source:** `go/internal/doctor/doctor.go`
- **Decision:** D-029
- **Verified by:** UNTESTED (tests inject all options)

#### DOC-005 Temporary-directory write probe
- **Behavior:** `probeWritableDirectory(dir)` MUST find the nearest existing ancestor (walking `filepath.Dir`; a non-directory → `"<p>" is not a directory`; a non-ENOENT stat error → `inspect "<p>": …`; reaching root → `no existing parent directory for "<p>"`), create `os.MkdirTemp(existing, ".noc-doctor-")`, and remove it immediately. Detail is `directory exists` when the target exists, else `nearest existing parent <existing> can create it`. Doctor MUST NOT create the state/runtime directories themselves or retain any file.
- **Source:** `go/internal/doctor/doctor.go`
- **Decision:** D-029
- **Verified by:** `go/internal/doctor/doctor_test.go` (pass with non-existent dirs); non-creation confirmed MANUAL step M5

#### DOC-006 Checks table
- **Behavior:** Each check MUST produce exactly these results:

  | ID | Condition | Status | Summary | Detail | Remediation |
  |---|---|---|---|---|---|
  | `state` | DB path blank | fail | `state storage path is empty` | — | — |
  | `state` | probe of DB parent fails | fail | `state storage is not writable` | `<db>: <err>` | `Choose a writable path with NOC_STATE_DIR or NOC_DATABASE.` |
  | `state` | DB exists, not regular | fail | `state storage is not a regular file` | `<db>` | — |
  | `state` | DB exists, `open(O_WRONLY)` fails | fail | `state storage database is not writable` | `<db>: <err>` | `Correct ownership or permissions on the noc database.` |
  | `state` | stat error other than not-exist | fail | `state storage database cannot be inspected` | `<db>: <err>` | — |
  | `state` | otherwise | pass | `state storage is writable` | `<db>; <probe detail>` | — |
  | `runtime` | socket path blank | fail | `runtime socket path is empty` | — | — |
  | `runtime` | probe of socket dir fails | fail | `runtime directory is not writable` | `<dir>: <err>` | `Choose a writable path with NOC_RUNTIME_DIR or NOC_SOCKET.` |
  | `runtime` | path exists (lstat) and is not a socket | fail | `runtime socket path is occupied by a non-socket file` | `<sock>` | `Move the file or select a different path with NOC_SOCKET.` |
  | `runtime` | lstat error other than not-exist | fail | `runtime socket cannot be inspected` | `<sock>: <err>` | — |
  | `runtime` | otherwise | pass | `runtime directory is writable` | `<sock>; <probe detail>` | — |
  | `server` | health request error | fail | `noc server is not reachable` | `socket <sock>: <err>` | `Start it with: noc server --bg` |
  | `server` | health `status == "ok"` / other | pass / fail | `noc server reports <status>` | `<version> on <sock>; host <host_id>` | — |
  | `server.provider.<p>` | provider status `available` / other | pass / warning | `server sees <p> as <status>` | `<version>[; <detail>]` | — |
  | `provider.<p>` | Probe or DiscoverLive func missing | warning | `<p> diagnostic is unavailable` | — | — |
  | `provider.<p>.cli` | probe not available | warning (and no discovery check) | `<p> CLI discovery capability is unavailable` | probe diagnostic | claude: ``Install or update Claude Code so `claude agents --json` is available.``; codex: ``Install or update Codex so `codex app-server` is available.`` |
  | `provider.<p>.cli` | probe available | pass | `<p> CLI supports live discovery` | probe version | — |
  | `provider.<p>.discovery` | DiscoverLive error | warning | `<p> live discovery failed` | error text | claude: same as cli; codex: `Start Codex's shared daemon with: codex app-server daemon start` |
  | `provider.<p>.discovery` | success | pass | `<p> live discovery succeeded` | `observed <n> live top-level sessions` | — |
  | `hooks.<p>` | `Inspection.Complete()` | pass | `<p> hooks cover the registry lifecycle` | DOC-007 | — |
  | `hooks.<p>` | not complete | warning | `<p> hooks are not fully configured` | DOC-007 | ``Run `noc setup <p>`; expected executable: <exe>`` |

- **Source:** `go/internal/doctor/doctor.go`; `go/internal/doctor/hooks.go`
- **Decision:** D-029
- **Verified by:** `go/internal/doctor/doctor_test.go` (all pass rows), `go/internal/doctor/doctor_test.go` (server unreachable; hooks warning), `go/internal/doctor/doctor_test.go` TestProviderDiscoveryFailureIsWarning; storage/socket failure rows, `server` non-ok, `server.provider` warning, `provider.<p>` missing-func row UNTESTED

#### DOC-007 Hook check detail and stale-executable detection
- **Behavior:** `hooks.claude` MUST inspect `<claudeDir>/settings.json`; `hooks.codex` MUST inspect `<codexHome>/hooks.json` and `<codexHome>/config.toml`, both against the running canonical executable. Detail MUST be the `; `-join of: `inspected <paths, comma-joined>`; `hook files: <files>` (or `none`); for Codex `configuration check only; Codex trust approval is not externally verifiable`; if errors `errors: <a; b>`; if disabled `Codex hooks are disabled in [features]`; if missing `missing events: <A, B>`; and only when nothing is missing but some events are not exact, `events not bound to this executable: <A, B>`. The last part is the stale-executable signal: handlers pointing at any other `noc` binary (e.g. an old dev build) keep coverage but fail exactness and downgrade the check to warning.
- **Source:** `go/internal/doctor/hooks.go`
- **Decision:** D-029, D-030
- **Verified by:** `go/internal/doctor/doctor_test.go` TestHookCoverageRequiresCurrentExecutable; `go/internal/doctor/doctor_test.go` (disabled); `go/internal/doctor/doctor_test.go` (missing)

#### DOC-008 Non-mutation
- **Behavior:** Doctor MUST NOT edit provider configuration, open the SQLite DB with SQLite (only a write-mode `open`/close of an existing file), create persistent files, or start provider runtimes; it may invoke the provider CLIs' probe/discovery commands.
- **Source:** `go/internal/doctor/doctor.go`
- **Decision:** D-029
- **Verified by:** MANUAL: step M5 (state and runtime dirs are not created)

---

## 06 · Design choices (local)

- **900 ms ingest deadline, 150 ms wake, 3 s handler timeout.** The ingest deadline sits well under the 3 s `timeout` written into provider configs, so a stalled SQLite or socket never lets the provider kill or flag the hook. The 900 ms bounds only the SQLite calls and the wake. `ps` (2 s) and the stdin read (unbounded) run inside the window without being cancelled by it, so the binary does not itself guarantee completion under 3 s, and a slow window drops the event (HK-005, HK-042). The wake gets a small slice because the 2 s drain tick covers a missed wake (`go/internal/cli/run.go`, `go/internal/hookconfig/hookconfig.go`).
- **Enqueue-then-wake, never call the server synchronously.** Ingest is fail-open and silent: provider hooks must not block or pollute an agent conversation when noc is unavailable or an input schema changes (`go/internal/cli/run.go`). Ingest even creates the DB and host ID itself.
- **Rejected payloads are journaled, not dropped.** A malformed payload becomes a `hook.rejected` row. Rejections have no provider correlation key, so the ID includes the reception time in nanoseconds and separate malformed invocations stay separately diagnosable (`go/internal/hooks/normalize.go`).
- **Whole-second reception bucket.** Lifecycle hooks do not always carry a provider event ID. A one-second reception bucket collapses immediate duplicate delivery without treating a later resume or a repeated cwd transition as the same event (`go/internal/hooks/normalize.go`).
- **Metadata caps** of 512 runes (identifiers, names, reasons, detail), 4096 (paths), 256 (model) are local choices beyond D-027's preview caps (`go/internal/hooks/normalize.go`).
- **Drain batch of 128 and serialized drains** (`go/internal/hooks/processor.go`); `last_error` capped at 1024 bytes (`go/internal/storage/sqlite/hooks.go`).
- **Process ID only moves forward.** An older event replayed from the outbox must not bring back a stale process ID, so the field is set only by an event at least as new as `last_seen_at` (`go/internal/hooks/processor.go`).
- **One `ps` snapshot per invocation.** The whole process table is read once rather than queried per process, so resolving a chain costs a single execution. Ancestry is capped at 12 steps; the provider typically spawns a shell which spawns the hook, so the answer is two or three levels up. `IsRunning` treats `EPERM` and other unknown errors as alive, because claiming death on an unknown error would retire a live agent (`go/internal/proclineage/proclineage.go`).
- **No matcher on managed groups; append after existing groups.** The handler observes every occurrence of the event; user groups keep their order ahead of noc's.
- **Inspection is permissive, removal is strict.** Inspection finds `command` strings at any depth under any event key; removal only touches `hooks[event][i].hooks[j].command` with an exact argv. Hook commands outside that exact argv shape are left untouched (`go/internal/hookconfig/hookconfig.go`).
- **Codex `config.toml` is read with a line scanner, never written**, to avoid a TOML dependency and to never rewrite a file noc does not own (`go/internal/hookconfig/hookconfig.go`).
- **Symlink following.** The atomic rename updates the link's target instead of replacing a symlink owned by a dotfile manager (`go/internal/hookconfig/hookconfig.go`).
- **Doctor's probe** creates and removes a private temporary directory. This verifies actual access (including ACLs) without retaining state or touching provider configuration (`go/internal/doctor/doctor.go`).

## 06 · Verification procedures

### Automated

```sh
cd go
go test -count=1 ./internal/hooks/... ./internal/storage/... ./internal/proclineage/... \
  ./internal/hookconfig/... ./internal/setup/... ./internal/doctor/...
# Boundary tests that live in neighbor packages:
go test -count=1 -run 'TestHookIngest|TestRunningServerProcessesHook|TestSetupRejects|TestDoctorRejects' ./internal/cli/
go test -count=1 -run 'TestReconcilerRetiresHookOnlyAgentWhoseProcessExited|TestBackgroundClaudeSessionsDropHookPaneBindings' ./internal/reconcile/
```

Expected: every package prints `ok`.

### Manual (isolated; never touches `~/.claude`, `~/.codex`, or the real state dir)

Common setup (zsh/bash):

```sh
T=$(mktemp -d /tmp/noc-spec06-XXXX)
(cd go && go build -o "$T/noc" ./cmd/noc)
cd "$T"
export NOC_DATABASE="$T/state/noc.db" NOC_SOCKET="$T/run/noc.sock"
# Read the DB without writing to it. The database is in WAL mode:
# - No -wal file (ingest-only or server stopped cleanly): use immutable=1.
#   `sqlite3 -readonly` fails here with "unable to open database file (14)"
#   because it cannot create the -shm file.
# - -wal/-shm present (server running): immutable=1 ignores the WAL and can
#   miss rows or tables ("no such table"); use `sqlite3 -readonly` instead.
peek() {
  if [ -e "$T/state/noc.db-wal" ]; then sqlite3 -readonly "$T/state/noc.db" "$@"
  else sqlite3 "file:$T/state/noc.db?immutable=1" "$@"; fi
}
# Check the real DDL before writing queries: peek '.schema hook_outbox' / '.schema observations'
```

- **M1 — Accepted payload is spooled silently with the server down.**
  ```sh
  printf '%s' '{"session_id":"11111111-2222-3333-4444-555555555555","hook_event_name":"UserPromptSubmit","prompt_id":"p-1","cwd":"/tmp","prompt":"  Fix   the\n\nflaky test  ","transcript_path":"/tmp/x.jsonl"}' \
    | env -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID -u CLAUDE_CODE_REMOTE -u CLAUDE_CODE_BRIDGE_SESSION_ID ./noc hook ingest --provider claude; echo "exit=$?"
  peek "select id, attempts, processed_at, last_error from hook_outbox; select envelope_json from hook_outbox;"
  ```
  Expect `exit=0`, no other output, one row `hook-<32 hex>|0||`, envelope with `"event_type":"turn.prompted"`, `"initial_request":{"text":"Fix the flaky test","source":"hook_first_observed_user_prompt",…}`, no `transcript_path`. `process_id` appears only when run under a `claude`/`codex` ancestor.
- **M2 — Rejection.** `printf 'not json' | ./noc hook ingest --provider codex; echo $?` → `0`; a new row with `"event_type":"hook.rejected","data":{"native_event":"unknown"},"error":"hook payload is not valid JSON"`. Repeat with `{"session_id":"s","hook_event_name":"X","cwd":5}` → same error (wrong-typed allowlisted key). Also try `CwdChanged` with `new_cwd` and `PostModelSwitch` with `to_model` and confirm `cwd`/`model` overrides.
- **M3 — Cloud exclusion.** `printf '{"session_id":"s","hook_event_name":"SessionStart"}' | CLAUDE_CODE_REMOTE=true ./noc hook ingest --provider claude` → exit 0 and `select count(*) from hook_outbox` unchanged. `./noc hook bogus` → stderr usage, exit 2.
- **M4 — Setup against a temp HOME only.**
  ```sh
  export HOME="$T/home" CLAUDE_CONFIG_DIR="$T/home/.claude" CODEX_HOME="$T/home/.codex"
  mkdir -p "$HOME"
  ./noc setup --check; echo "exit=$?"     # incomplete for both, exit=1
  ./noc setup; echo "exit=$?"             # "installed in …", Codex trust line, exit=0
  cat "$CLAUDE_CONFIG_DIR/settings.json"    # HC-004 shape, keys sorted, timeout 3
  ./noc setup; echo "exit=$?"             # "already configured in …", exit=0, file unchanged
  ./noc setup --check --json              # complete:true for both
  ./noc setup --uninstall; echo "exit=$?" # "removed 9 …" / "removed 6 …"; settings.json becomes {"hooks": {}}
  printf '[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = "command"\ncommand = %s\n' "'$T/noc hook ingest --provider codex'" > "$CODEX_HOME/config.toml"
  ./noc setup codex; echo "exit=$?"       # error — … contains an existing noc hook …, exit=1, hooks.json not rewritten
  ```
- **M5 — Doctor without touching real providers.** With the M4 environment plus `NOC_STATE_DIR="$T/state2" NOC_RUNTIME_DIR="$T/run2" NOC_CLAUDE_BIN=/usr/bin/false NOC_CODEX_BIN=/usr/bin/false NOC_CODEX_SOCKET="$T/none.sock"`, after `./noc setup`: `./noc doctor --timeout 5s; echo $?` → `noc doctor: fail`, `PASS state`/`PASS runtime` with "nearest existing parent … can create it", `FAIL server … Next: Start it with: noc server --bg`, `WARNING provider.claude.cli`/`provider.codex.cli`, `PASS hooks.claude`, `PASS hooks.codex` (with the trust caveat), exit 1; afterwards `ls "$T/state2" "$T/run2"` → both missing. Edit one handler's path in `settings.json` to `/old/bin/noc` → `hooks.claude` warns with `events not bound to this executable: …`.
- **M6 — Projection through a running server.** In the M5 environment, start `./noc server &`, ingest the M1 payload with `ZELLIJ_SESSION_NAME=demo ZELLIJ_PANE_ID=3`, then `./noc ls --json` within ~1 s shows one agent with `origin:"hook"`, `activity_state:"working"`, `display_state:"working"`, `initial_request.text`, and a `zellij` binding (`state:"observed"`); `./noc events` shows `turn.prompted … native=UserPromptSubmit; - -> live`; the outbox row has `attempts=1` and non-null `processed_at`. Stop the server with `kill %1`. Clean up the temp directory when done.

## 06 · Coverage gaps

- No test exercises: quarantine after 5 failed attempts (OBX-004/007), `last_error` truncation, `MarkObservationProcessed` returning `ErrNoRows`, or a poison (undecodable) outbox row, which blocks every subsequent drain because the decode error aborts `PendingObservations` before any attempt is counted (OBX-005).
- Event mappings for `StopFailure`, `CwdChanged`, `PostModelSwitch`, `PermissionRequest`, unknown events, and the `authentication_required` notification branch are untested; so are attention set/clear rules (HK-040/HK-041), out-of-order recency (HK-032), model recency, process-ID forward-only, and the `session_title` → name → `user_name` clear path.
- Processor-side background-Claude binding suppression (HK-037) is untested; only the reconciler predicate is tested.
- `RejectedObservation`, `HK-011` wrong-type rejection, field bounds (512/4096/256), 1200-rune summary truncation, and the exact 262144-byte boundary are untested.
- Positive process-lineage match is untested (only negatives); lineage fails for providers whose `ps` `comm` is an interpreter (e.g. `node`), and Linux truncates `comm` to 15 chars.
- No test asserts total ingest latency, a `ps` snapshot that hits its 2 s timeout, or a payload that arrives after the 900 ms deadline and is dropped (HK-005, HK-042).
- `hookconfig.ResolvePaths`, executable-replacement on reinstall, serialization side effects (HC-011), atomic-write failure cleanup, `codex_hooks=false`, and setup's human/JSON renderers are untested.
- Doctor storage/socket failure rows, non-ok server status, server provider warnings, `withDefaults`, and the human renderer are untested. If `HOME` and `CLAUDE_CONFIG_DIR` are both unavailable, doctor inspects a relative `settings.json` in the CWD.
- Wake route 405/503 branches and drain-loop tick fallback are untested.

## Known limitations

- **Hook ingest has no bound on total runtime.** The 900 ms deadline covers
  only the SQLite calls and the wake. The `ps` snapshot has its own 2 s
  timeout, and the stdin read has none, so the process waits for as long as
  the writer keeps stdin open (HK-005; `go/internal/cli/run.go`,
  runHookIngest; `go/internal/hooks/normalize.go`, readBounded).
- **A late payload is dropped without a trace.** If `ps` plus the stdin read
  outlast what remains of the 900 ms window, the enqueue fails on the
  expired context, nothing is spooled, and the exit code is still 0 (HK-042;
  `go/internal/cli/run.go`, runHookIngest).
- **Stored previews are never deleted.** Prompt previews and assistant
  excerpts in `hook_outbox.envelope_json` and `observations.payload_json`
  persist indefinitely. Forgetting or pruning an agent deletes only its
  `agents` row; both tables grow without bound (OBX-001;
  `go/internal/storage/sqlite/hooks.go`, `go/internal/storage/sqlite/store.go`).
- **An undecodable outbox row blocks draining.** `PendingObservations` fails
  on the first row whose envelope does not decode, before any attempt is
  counted, so the row is never quarantined and every later drain fails the
  same way (OBX-005; `go/internal/storage/sqlite/hooks.go`).
- **SQLite sidecar files follow the umask.** Only the database file is set
  to `0600`; the `-wal` and `-shm` files rely on the `0700` parent directory
  noc creates, and an existing parent directory's mode is not changed
  (OBX-010; `go/internal/storage/sqlite/store.go`, Open).
- **Process lineage misses interpreter-hosted providers.** A provider whose
  `ps` command name is an interpreter such as `node` yields no process ID,
  so a hook-only row for it is never retired by process exit (PL-003, PL-005;
  `go/internal/proclineage/proclineage.go`).
- **A config rewrite reformats the file.** Installing or uninstalling hooks
  sorts object keys, re-indents, HTML-escapes `<`, `>`, `&`, and passes
  numbers through float64 (HC-011; `go/internal/hookconfig/hookconfig.go`,
  marshalConfig).
