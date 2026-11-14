# 02 — Public API contract

Scope: the versioned public JSON data model shared by `noc server`, the
`noc` CLI, and independent frontends (`go/pkg/api/v1`); the pure derivation
functions that live in that package (display state, bucket, process presence,
hook-terminal trust, never-engaged); the Unix-socket HTTP/JSON local API
(server routes, status codes, headers, error bodies, NDJSON watch stream,
event projection, socket lifecycle); the Go client used by the CLI (dialing,
error classification, caller timeouts); the registry interfaces and their two
implementations (in-memory `registry.Memory` and the SQLite store that backs
the real server) as far as they define API-visible semantics (list filter and
ordering, user-name overlay, rename, teams and their leads, the registration
of a spawned agent, forget, prune, event listing); and UUID generation. How rows are *produced* (hook normalization, snapshot
reconciliation, staleness timing) is specified in [06-hooks.md](06-hooks.md)
and [04-server.md](04-server.md); this section only cites those writers where
they fix the value of a public field.

Source files covered:

- `go/pkg/api/v1/types.go`, `go/pkg/api/v1/types_test.go`
- `go/internal/localapi/server.go`, `go/internal/localapi/client.go`,
  `go/internal/localapi/teams.go`, `go/internal/localapi/socket.go`,
  `go/internal/localapi/server_test.go`, `go/internal/localapi/rename_test.go`,
  `go/internal/localapi/teams_test.go`
- `go/internal/registry/registry.go`, `go/internal/registry/registry_test.go`
- `go/internal/identity/uuid.go`, `go/internal/identity/uuid_test.go`
- Neighbouring code read for call-site semantics only:
  `go/internal/storage/sqlite/store.go`, `go/internal/storage/sqlite/hooks.go`,
  `go/internal/storage/sqlite/migrations.go`, `go/internal/server/run.go`,
  `go/internal/server/health.go`, `go/internal/server/discovery.go`,
  `go/internal/config/paths.go`, `go/internal/cli/run.go`,
  `go/internal/cli/server_lifecycle.go`, `go/internal/reconcile/reconciler.go`,
  `go/internal/hooks/normalize.go`, `go/internal/hooks/processor.go`,
  `go/internal/provider/claude/adapter.go`, `go/internal/provider/codex/adapter.go`,
  `go/internal/workspace/inspect.go`, `go/internal/buildinfo/buildinfo.go`.

Conventions used below:

- "Wire name" is the JSON key. Go types are from `go/pkg/api/v1` unless
  qualified. "omitempty" means Go's `encoding/json` `omitempty` tag: strings
  `""`, ints `0`, bools `false`, nil pointers, nil/empty maps and slices, and
  zero-length `json.RawMessage` are omitted. A non-pointer `time.Time` is a
  struct and is **never** omitted; its zero value encodes as
  `"0001-01-01T00:00:00Z"`.
- All `time.Time` values encode with Go's default `MarshalJSON`
  (RFC 3339 with nanoseconds, trailing zeros trimmed, e.g.
  `"2026-10-01T12:00:00.5Z"`). Server-generated timestamps are UTC.
- Enum types are Go `string` types. The JSON decoder does not validate them:
  any string decodes. The enum tables below list the only values the code
  defines.

---

## 1. Schema version

#### API-001 One schema version constant
- **Behavior:** `SchemaVersion` MUST equal the integer `1`. Every top-level
  public object the server emits — `AgentList`, `Agent`, `AgentEventList`,
  `AgentEvent`, `Health`, `Error` — MUST carry `"schema_version": 1`. Writers
  force it: registry `Upsert` overwrites `Agent.SchemaVersion` with
  `SchemaVersion` before storing (memory and SQLite); list, watch, and event
  handlers set it on the envelope; `writeError` sets it on every error;
  `EnqueueObservation` forces it on every `Observation`. There is no version
  negotiation, no `Accept`/version header, and no second version; the URL
  prefix `/v1/` is the only other version marker.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/registry/registry.go`
  (Memory.Upsert); `go/internal/storage/sqlite/store.go` (Store.Upsert);
  `go/internal/localapi/server.go`;
  `go/internal/storage/sqlite/hooks.go` (EnqueueObservation);
  `go/internal/server/run.go` (health func)
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (every error body),
  TestWatchAgentsStreamsSemanticSnapshots (watch frame); `go/internal/storage/sqlite/store_test.go`
  TestStorePersistsHostAndAgents (stored agent); `go/internal/storage/sqlite/hooks_test.go`
  TestListEventsFiltersOrdersAndRedactsPayloads (event). `Health` and the
  `AgentEventList` envelope UNTESTED; MANUAL: §Verification procedures step 3
  — every body contains `"schema_version":1`.

#### API-002 Observation schema mismatch is rejected by the processor
- **Behavior:** The hook processor MUST reject (fail, not apply) any outbox
  `Observation` whose `schema_version` differs from `SchemaVersion`, with error
  text `unsupported observation schema version <n>`. (Specified fully in
  [06-hooks.md](06-hooks.md); listed here because it is the only
  consumer-side version check in the codebase.)
- **Source:** `go/internal/hooks/processor.go` (process)
- **Verified by:** `go/internal/hooks/processor_test.go` TestProcessorValidationAndRejectedObservations

---

## 2. Enumerations

#### API-003 `Provider`
- **Behavior:** MUST be exactly one of the strings below. The HTTP events
  filter (HTTP-021) and the hook processor accept only these two.

  | Go constant | Wire value |
  |---|---|
  | `ProviderClaude` | `claude` |
  | `ProviderCodex` | `codex` |
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend, TestNormalizeStopProducesProvenancedSummary (golden JSON carries `"provider":"claude"` / `"codex"`); MANUAL: §Verification step 4.

#### API-004 `LiveState`
- **Behavior:**

  | Go constant | Wire value | Meaning (set by reconcile/hooks) |
  |---|---|---|
  | `LiveStateLive` | `live` | Current provider/hook evidence includes the session. |
  | `LiveStateCandidateStale` | `candidate_stale` | Missing from evidence, within the grace window. |
  | `LiveStateNotLive` | `not_live` | Retired; hidden from default list/watch. |
  | `LiveStateUnknown` | `unknown` | Defined; no writer sets it. |

  Only `not_live` is treated specially by the registry list filter (REG-004).
  `candidate_stale` and `unknown` rows ARE returned by the default list.
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** UNTESTED for wire strings.

#### API-005 `RuntimeState`
- **Behavior:**

  | Go constant | Wire value | Counts as a process (`HasProcess`) | Eligible for `ready` |
  |---|---|---|---|
  | `RuntimeStateStarting` | `starting` | yes | **no** |
  | `RuntimeStateRunning` | `running` | yes | yes |
  | `RuntimeStateLoaded` | `loaded` | yes | yes |
  | `RuntimeStateResumable` | `resumable` | no | no |
  | `RuntimeStateStopped` | `stopped` | no | no |
  | `RuntimeStateFailed` | `failed` | no | no |
  | `RuntimeStateUnknown` | `unknown` | no | no |
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** UNTESTED for wire strings.

#### API-006 `ActivityState`
- **Behavior:**

  | Go constant | Wire value |
  |---|---|
  | `ActivityStateWorking` | `working` |
  | `ActivityStateWaiting` | `waiting` |
  | `ActivityStateIdle` | `idle` |
  | `ActivityStateUnknown` | `unknown` |
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** UNTESTED for wire strings.

#### API-007 `AttentionType`
- **Behavior:**

  | Go constant | Wire value | Maps to display state |
  |---|---|---|
  | `AttentionNone` | `none` | (falls through) |
  | `AttentionInputRequired` | `input_required` | `needs_input` |
  | `AttentionApprovalRequired` | `approval_required` | `needs_approval` |
  | `AttentionAuthenticationRequired` | `authentication_required` | `needs_authentication` |
  | `AttentionError` | `error` | `error` |
  | `AttentionUnknown` | `unknown` | (falls through — NOT treated as attention) |
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** UNTESTED for wire strings.

#### API-008 `DisplayState`
- **Behavior:** Server-derived (API-026); clients MUST NOT need to recompute it.

  | Go constant | Wire value |
  |---|---|
  | `DisplayStateNeedsApproval` | `needs_approval` |
  | `DisplayStateNeedsInput` | `needs_input` |
  | `DisplayStateNeedsAuthentication` | `needs_authentication` |
  | `DisplayStateError` | `error` |
  | `DisplayStateWorking` | `working` |
  | `DisplayStateWaiting` | `waiting` |
  | `DisplayStateReady` | `ready` — finished its turn, process still running; "your move", not a question |
  | `DisplayStateIdle` | `idle` — live but nothing running (e.g. exited-but-resumable background job) |
  | `DisplayStateStale` | `stale` |
  | `DisplayStateNotLive` | `not_live` |
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-044
- **Verified by:** UNTESTED for wire strings.

#### API-009 `Bucket`, display order, and rank
- **Behavior:** `Buckets` MUST list `[blocked, working, review, quiet]` in that
  order (most urgent first). `Bucket.Rank()` MUST return the index in
  `Buckets` (blocked 0, working 1, review 2, quiet 3) and `len(Buckets)` (= 4)
  for any other string, including `""`.

  | Go constant | Wire value | Rank | Meaning |
  |---|---|---|---|
  | `BucketBlocked` | `blocked` | 0 | asked for approval/input/auth, or failed |
  | `BucketWorking` | `working` | 1 | mid-turn |
  | `BucketReview` | `review` | 2 | has a process, finished its turn |
  | `BucketQuiet` | `quiet` | 3 | no process |
  | anything else | — | 4 | unknown, sorts last |
- **Source:** `go/pkg/api/v1/types.go` (Bucket, Buckets, Bucket.Rank)
- **Decision:** D-045, D-046
- **Verified by:** `go/pkg/api/v1/types_test.go` TestDeriveBucketGroupsByWhatTheUserIsNeededFor (ranks 0–3; `""` and an unknown string rank 4)

#### API-010 `ProviderNameSync` and its headers
- **Behavior:** Header names MUST be exactly `Noc-Provider-Name-Sync`
  (`ProviderNameSyncHeader`) and `Noc-Provider-Name-Sync-Error`
  (`ProviderNameSyncErrorHeader`). Values of the first:

  | Go constant | Wire value | Meaning |
  |---|---|---|
  | `ProviderNameSynced` | `synced` | provider session now carries the name |
  | `ProviderNameUnsupported` | `unsupported` | provider cannot be renamed from outside; noc-only |
  | `ProviderNameFailed` | `failed` | provider supports rename but the attempt failed; name kept in noc |
  | `ProviderNameNotAttempted` | `not_attempted` | nothing to push (clearing a name) |
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt

#### API-011 `ObservationSource`
- **Behavior:** MUST be one of:

  | Go constant | Wire value | Written by |
  |---|---|---|
  | `ObservationSourceHook` | `hook` | hook ingestion |
  | `ObservationSourceSnapshot` | `snapshot` | provider snapshot reconciliation |
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/reconcile/reconciler.go`
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend (golden JSON `"source":"hook"`); `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness (snapshot transitions). Wire string `snapshot` UNTESTED.

---

## 3. Public object types

### 3.1 Agent and its components

#### API-012 `Attention` object
- **Behavior:** Always present on an `Agent` (non-pointer, no omitempty).

  | Wire name | Go type | omitempty | Meaning / writer |
  |---|---|---|---|
  | `type` | `AttentionType` | no | Always emitted. Reconciliation normalizes `""` to `none` (`reconciler.go`). `registry.Memory` does not normalize, so a test-built agent may emit `"type":""`. |
  | `since` | `*time.Time` | yes | When this attention began. Reconciliation keeps the previous `since` if type and detail are unchanged, else sets "now"; nil when type is `none` (`reconciler.go`). |
  | `detail` | `string` | yes | Short redacted explanation. |
  | `native_request_id` | `string` | yes | Provider request id. No current writer sets it. |
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/reconcile/reconciler.go` (mergeAttention)
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerAttentionSinceAndActivity (`since` rules). JSON shape UNTESTED.

#### API-013 `Capability` object
- **Behavior:** `{"supported": bool (always emitted), "reason": string (omitempty)}`.
  Current keys written by adapters into `Agent.capabilities`: `attach` and
  `resume`. Claude: `attach.supported` is `true` iff the native short id is
  non-empty, otherwise `false` with reason
  `interactive session has no background attach ID`; `resume.supported` is
  `true`. Codex: both `true`.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/provider/claude/adapter.go`;
  `go/internal/provider/codex/adapter.go`
- **Verified by:** `go/internal/provider/claude/adapter_test.go` TestNormalizeStateMappings, TestNormalizeFallbackAndErrors (Claude `attach`/`resume` and the reason text). Codex capabilities: see [05-providers.md](05-providers.md).

#### API-014 `TextPreview` object
- **Behavior:** Bounded, sourced excerpt — never a transcript.

  | Wire name | Go type | omitempty | Meaning |
  |---|---|---|---|
  | `text` | `string` | no | Compacted text (bounds: 2,048 runes for initial requests, 1,200 for updates — enforced by writers, not the type). |
  | `source` | `string` | no | Provenance (API-018). |
  | `truncated` | `bool` | yes | `true` iff `text` is an excerpt. |
  | `observed_at` | `time.Time` | no | When observed (zero time if a writer left it unset). |
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-027
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend, TestNormalizeStopProducesProvenancedSummary (golden JSON of both previews; bounds).

#### API-015 `FrontendBinding` object
- **Behavior:** Presence evidence inherited by a hook; a navigation hint, not
  proof of a connected client.

  | Wire name | Go type | omitempty | Values written today |
  |---|---|---|---|
  | `kind` | `string` | no | `zellij`, `claude_remote_control` |
  | `session` | `string` | yes | Zellij session name (`ZELLIJ_SESSION_NAME`), bounded |
  | `pane_id` | `string` | yes | Zellij pane id (`ZELLIJ_PANE_ID`), bounded |
  | `state` | `string` | no | always `observed` |
  | `observed_at` | `time.Time` | no | hook receive time |

  A `claude_remote_control` binding is emitted only for provider `claude` when
  `CLAUDE_CODE_BRIDGE_SESSION_ID` is non-empty, and carries no session/pane.
  A Claude agent whose `source_kind` is `background` MUST carry no bindings
  (API-029).
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/hooks/normalize.go`
- **Decision:** D-027, D-036, D-050
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend (both kinds, `observed`); `go/internal/reconcile/reconciler_bindings_test.go` TestBackgroundClaudeSessionsDropHookPaneBindings and `go/internal/hooks/processor_test.go` TestProcessorSuppressesBindingsForBackgroundClaude (background rule).

#### API-016 `Workspace` object
- **Behavior:**

  | Wire name | Go type | omitempty | Meaning / values |
  |---|---|---|---|
  | `id` | `string` | yes | Reserved; not set by current writers. |
  | `path` | `string` | no | Provider-reported working directory. |
  | `root_path` | `string` | yes | Git checkout/worktree root (read-only inspection). |
  | `kind` | `string` | yes | `directory` (not a Git checkout, or Git inspection failed), `main_checkout`, `worktree` (a linked Git worktree). |
  | `repository_id` | `string` | yes | Reserved; not set by current writers. |
  | `common_git_dir` | `string` | yes | Shared git dir for related worktrees. |
  | `branch` | `string` | yes | Observed branch. |
  | `head_sha` | `string` | yes | Observed HEAD. |
  | `owner` | `string` | yes | `unknown` from adapters/hooks today. |
  | `observed_at` | `time.Time` | no | Refreshed by every reconciliation; freshness-only for watch (HTTP-027). |
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/workspace/inspect.go`;
  `go/internal/reconcile/reconciler.go`
- **Verified by:** `go/internal/workspace/inspect_test.go` TestInspectorClassifiesMainCheckoutAndLinkedWorktree, TestInspectorFallsBackToDirectory (the three `kind` values). JSON shape UNTESTED.

#### API-017 `Agent` object — every field
- **Behavior:** The `Agent` JSON object MUST have exactly these fields, in this
  Go declaration order (which is also the JSON emission order):

  | # | Wire name | Go type | omitempty | Meaning | Who sets it |
  |---|---|---|---|---|---|
  | 1 | `schema_version` | `int` | no | Always `1`. | Registry `Upsert` (API-001) |
  | 2 | `id` | `string` | no | Stable noc agent identity, a v4 UUID (API-032). | Allocated by the hook processor, the reconciler, or a spawn registration (HTTP-036) on first sight of a natural key |
  | 3 | `host_id` | `string` | no | Persistent host identity (API-033). | Writers |
  | 4 | `provider` | `Provider` | no | `claude`/`codex`. | Writers |
  | 5 | `native_session_id` | `string` | no | Provider conversation/thread id. Natural key with host+provider. | Writers |
  | 6 | `native_short_id` | `string` | yes | Provider alias used for attach (Claude short id). | Reconcile (keeps previous if new empty) |
  | 7 | `user_name` | `string` | yes | User-assigned label set via the API; preferred by frontends. Never in the stored snapshot; overlaid from its own column (REG-010). | `PUT /v1/agents/{id}/name` only; cleared by provider rename (REG-011) |
  | 8 | `team` | `string` | yes | The team the agent is in (API-034). Never in the stored snapshot; overlaid from its own column (REG-021). | `PUT /v1/agents/{id}/team`, `POST /v1/agents` |
  | 9 | `lead` | `bool` | yes | `true` only for the lead of the agent's team; at most one agent per team. Overlaid like `team`. | `PUT /v1/agents/{id}/team` with `lead`, and the rule in HTTP-037 |
  | 10 | `parent_id` | `string` | yes | The `id` of the agent that spawned this one. Independent of teams. Overlaid like `team`. | `POST /v1/agents` only |
  | 11 | `name` | `string` | yes | Provider-owned display name; not identity. Empty for a Claude session whose roster name is only its short id. | Reconcile/hooks |
  | 12 | `source_kind` | `string` | yes | Provider session kind, e.g. Claude `background`/`interactive`, Codex `cli`. Free-form string. | Reconcile/hooks |
  | 13 | `origin` | `string` | yes | `hook` (row created by a hook), `reconcile` (created by a snapshot) or `spawn` (created by a spawn registration, HTTP-036); preserved once set. | `hooks/processor.go`, `reconciler.go`, `localapi/teams.go` |
  | 14 | `live_state` | `LiveState` | no | API-004. | Reconcile/hooks |
  | 15 | `runtime_state` | `RuntimeState` | no | API-005. May be `""` if a writer left it unset. | Reconcile/hooks |
  | 16 | `activity_state` | `ActivityState` | no | API-006. | Reconcile/hooks |
  | 17 | `attention` | `Attention` | no | API-012. | Reconcile/hooks |
  | 18 | `display_state` | `DisplayState` | no | API-026; recomputed on every `Upsert`. | Registry `Upsert` |
  | 19 | `bucket` | `Bucket` | yes | API-028; recomputed on every `Upsert` and on every SQLite read. | Registry |
  | 20 | `model` | `string` | yes | Active model when exposed. | Reconcile/hooks |
  | 21 | `model_source` | `string` | yes | Provenance of `model` (API-018). | Reconcile/hooks |
  | 22 | `model_observed_at` | `*time.Time` | yes | When `model` was last observed. Freshness-only for watch. | Reconcile/hooks |
  | 23 | `reasoning_effort` | `string` | yes | Configured reasoning effort when exposed (Codex). | Reconcile |
  | 24 | `reasoning_source` | `string` | yes | Provenance. | Reconcile |
  | 25 | `reasoning_observed_at` | `*time.Time` | yes | Freshness-only for watch. | Reconcile |
  | 26 | `initial_request` | `*TextPreview` | yes | Bounded first-request preview. | Reconcile/hooks |
  | 27 | `summary` | `string` | yes | Bounded current preview (latest assistant excerpt today). | Reconcile/hooks |
  | 28 | `summary_source` | `string` | yes | Provenance. | Reconcile/hooks |
  | 29 | `summary_updated_at` | `*time.Time` | yes | Freshness-only for watch. | Reconcile/hooks |
  | 30 | `summary_truncated` | `bool` | yes | Whether `summary` is an excerpt. | Reconcile/hooks |
  | 31 | `created_at` | `*time.Time` | yes | Provider creation time. | Reconcile |
  | 32 | `first_seen_at` | `time.Time` | no | First noc observation. | Writers |
  | 33 | `last_seen_at` | `time.Time` | no | Latest evidence the session exists; also the retention clock (REG-015). Freshness-only for watch. | Writers |
  | 34 | `last_activity_at` | `*time.Time` | yes | Latest observed work. Reconcile sets it to "now" on every poll while `activity_state` is `working`. | Reconcile/hooks |
  | 35 | `snapshot_confirmed_at` | `*time.Time` | yes | Latest time a provider snapshot positively included the session; nil for hook-only rows. Freshness-only for watch. | Reconcile |
  | 36 | `ended_at` | `*time.Time` | yes | When confirmed no longer live. Reconcile clears it (nil) whenever the session is seen live. | Reconcile/hooks |
  | 37 | `capabilities` | `map[string]Capability` | yes | API-013. | Reconcile (keeps previous if adapter supplies nil) |
  | 38 | `native_state` | `json.RawMessage` | yes | Bounded provider payload for diagnostics; opaque to clients. | Adapters |
  | 39 | `workspace` | `*Workspace` | yes | API-016. | Reconcile/hooks |
  | 40 | `frontend_bindings` | `[]FrontendBinding` | yes | API-015. | Hooks (reconcile preserves/clears) |
  | 41 | `process_id` | `int` | yes | Last provider process observed by a hook. Used only as a negative liveness signal for rows no roster covers. | Hooks |
- **Source:** `go/pkg/api/v1/types.go`; writer citations inline;
  `go/internal/reconcile/reconciler.go` (materialize)
- **Decision:** D-016, D-027, D-035, D-040, D-042, D-046, D-071
- **Verified by:** UNTESTED as a whole-object contract (no golden JSON test);
  MANUAL: §Verification step 4. `team`, `lead` and `parent_id`:
  `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead, TestSpawnRegistrationRules.

#### API-018 Provenance (`*_source`) values
- **Behavior:** The provenance string fields MUST take these values from
  current writers (free-form type; clients MUST tolerate others):

  | Field | Value | Written when |
  |---|---|---|
  | `model_source` | `hook` | model reported by a hook (`hooks/processor.go`) |
  | `model_source`, `reasoning_source` | `codex_app_server_thread` | Codex app-server metadata (`provider/codex/adapter.go`) |
  | `model_source`, `reasoning_source`, `summary_source`, `initial_request.source` | `provider_snapshot` | a snapshot supplied the value without its own source (`reconciler.go`) |
  | `initial_request.source` | `codex_app_server_preview` | Codex thread `preview` (`provider/codex/adapter.go`) |
  | `initial_request.source` | `hook_first_observed_user_prompt` | first prompt seen by a hook (`hooks/normalize.go`) |
  | `initial_request.source` | `spawn` | the prompt a spawn registration carried (`localapi/teams.go`, HTTP-036) |
  | `summary_source` | `hook_last_assistant_message` | stop-hook assistant excerpt (`hooks/normalize.go`) |

  When a snapshot omits a value, reconcile MUST keep the previous value AND its
  previous source/timestamp unchanged (`reconciler.go`).
- **Source:** as cited
- **Decision:** D-027
- **Verified by:** `go/internal/reconcile/reconciler_test.go` TestReconcilerUsesProviderMetadataOverHookFallback (`provider_snapshot`), TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage (previous value and source kept); `go/internal/provider/codex/adapter_test.go` TestNormalizeStateMappings (`codex_app_server_*`); `go/internal/hooks/normalize_test.go` (hook sources in golden JSON).

#### API-019 Annotated example `Agent`
- **Behavior:** A conforming `Agent` (comments are annotations, not JSON):

```jsonc
{
  "schema_version": 1,                                   // always 1
  "id": "56f3a29a-73c8-4066-a4e4-96256f04a173",          // noc v4 UUID, stable
  "host_id": "c88452e1-1b7a-467d-8410-588aabaff572",     // persisted per database
  "provider": "codex",
  "native_session_id": "0199aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee",
  "native_short_id": "a1b2c3",                           // omitted when empty
  "user_name": "Cache work",                             // overlay; omitted when unset
  "team": "cache",                                       // overlay; omitted when in no team
  "lead": true,                                          // overlay; omitted unless the team's lead
  "parent_id": "0d5c1f6e-1b1f-4c55-9d53-0c7a4b3f2a10",   // overlay; omitted when not spawned by an agent
  "name": "cache refactor",                              // provider name; omitted when empty
  "source_kind": "cli",
  "origin": "reconcile",                                 // "hook" | "reconcile" | "spawn"
  "live_state": "live",
  "runtime_state": "loaded",
  "activity_state": "idle",
  "attention": { "type": "none" },                       // always present
  "display_state": "ready",                              // derived (API-026)
  "bucket": "review",                                    // derived (API-028)
  "model": "example-model",
  "model_source": "codex_app_server_thread",
  "model_observed_at": "2026-10-01T12:00:05Z",           // freshness-only
  "reasoning_effort": "high",
  "reasoning_source": "codex_app_server_thread",
  "reasoning_observed_at": "2026-10-01T12:00:05Z",
  "initial_request": {
    "text": "Example request",
    "source": "codex_app_server_preview",
    "observed_at": "2026-10-01T12:00:05Z"                // "truncated" omitted when false
  },
  "summary": "Example latest update",
  "summary_source": "hook_last_assistant_message",
  "summary_updated_at": "2026-10-01T12:00:04Z",
  "created_at": "2026-10-01T11:00:00Z",
  "first_seen_at": "2026-10-01T11:00:01Z",               // never omitted
  "last_seen_at": "2026-10-01T12:00:05Z",                // never omitted; freshness-only
  "last_activity_at": "2026-10-01T12:00:00Z",
  "snapshot_confirmed_at": "2026-10-01T12:00:05Z",
  "capabilities": {
    "attach": { "supported": true },
    "resume": { "supported": true }
  },
  "native_state": { "status": "idle" },                  // opaque provider payload
  "workspace": {
    "path": "/path/to/repo",
    "root_path": "/path/to/repo",
    "kind": "main_checkout",
    "common_git_dir": "/path/to/repo/.git",
    "branch": "main",
    "head_sha": "0123456789abcdef0123456789abcdef01234567",
    "owner": "unknown",
    "observed_at": "2026-10-01T12:00:05Z"
  },
  "frontend_bindings": [
    { "kind": "zellij", "session": "example", "pane_id": "3",
      "state": "observed", "observed_at": "2026-10-01T11:00:01Z" }
  ],
  "process_id": 4242
}
```
- **Source:** `go/pkg/api/v1/types.go`
- **Verified by:** MANUAL: §Verification step 4 (shape of a hook-created row).

### 3.2 Envelopes and other public types

#### API-020 `AgentList`
- **Behavior:** `{"schema_version": 1, "generated_at": <UTC time.Time>, "agents": [Agent...]}`.
  No field is omitempty. The server MUST encode an empty registry as
  `"agents": []`, never `null` (handlers replace a nil slice with an empty
  one). `generated_at` is `time.Now().UTC()` at encode time. The same type is
  the response of `GET /v1/agents` and each line of the watch stream.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/localapi/server.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (non-nil empty agents)

  Example:
  ```json
  {"schema_version":1,"generated_at":"2026-10-01T12:00:06.123456Z","agents":[]}
  ```

#### API-021 `AgentEvent` and `AgentEventList`
- **Behavior:** `AgentEvent` is a transcript-free projection of one stored
  observation:

  | Wire name | Go type | omitempty | Meaning |
  |---|---|---|---|
  | `schema_version` | `int` | no | Schema version stored with the observation row. |
  | `id` | `string` | no | Observation id: `hook-<32 hex>` for hooks, `snapshot-<64 hex>` for snapshot transitions (REG-020). |
  | `agent_id` | `string` | yes | noc agent id resolved **at query time** by joining on the natural key; omitted if no current row matches (REG-018). |
  | `provider` | `Provider` | no | |
  | `event_type` | `string` | no | Normalized event name (REG-020). |
  | `source` | `ObservationSource` | no | `hook` / `snapshot`. |
  | `occurred_at` | `*time.Time` | yes | Provider/event time if known. |
  | `received_at` | `time.Time` | no | Local ingestion time. |
  | `processed_at` | `*time.Time` | yes | When the registry applied it. |
  | `native_event` | `string` | yes | Provider-native hook event name, e.g. `UserPromptSubmit`, `SessionStart`. |
  | `previous_live_state` | `LiveState` | yes | Live state before the observation applied (present only if a row existed). |
  | `live_state` | `LiveState` | yes | Live state after it applied. |
  | `reason` | `string` | yes | Transition reason, e.g. `observed_in_provider_snapshot`, `missing_from_provider_snapshot`. |
  | `error` | `string` | yes | Bounded processing error recorded with the observation. |

  It MUST NOT include `native_session_id`, `host_id`, any preview text,
  `cwd`, model, attention, frontend bindings, process id, or any other
  `ObservationData` field.

  `AgentEventList` is `{"schema_version":1, "generated_at": UTC, "agent_id": string (omitempty), "provider": Provider (omitempty), "events": [AgentEvent...]}`;
  `agent_id`/`provider` echo the request filters; `events` is `[]` (never
  `null`) when empty.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/storage/sqlite/hooks.go`;
  `go/internal/localapi/server.go`
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads (prompt text not leaked);
  `go/internal/localapi/server_test.go` TestClientAndServerListAgentEvents (echoed filters)

  Annotated example:
  ```jsonc
  {
    "schema_version": 1,
    "generated_at": "2026-10-01T12:00:10Z",
    "agent_id": "56f3a29a-73c8-4066-a4e4-96256f04a173",  // echo; omitted for /v1/events
    "events": [                                          // oldest first
      {
        "schema_version": 1,
        "id": "hook-5f28682f7af6c042c76dbd39fcc8edeb",
        "agent_id": "56f3a29a-73c8-4066-a4e4-96256f04a173",
        "provider": "claude",
        "event_type": "turn.prompted",
        "source": "hook",
        "received_at": "2026-10-01T12:00:00.561768Z",
        "processed_at": "2026-10-01T12:00:00.624228Z",
        "native_event": "UserPromptSubmit",
        "live_state": "live"                             // no previous_live_state: row was new
      },
      {
        "schema_version": 1,
        "id": "snapshot-<64 hex>",
        "agent_id": "56f3a29a-73c8-4066-a4e4-96256f04a173",
        "provider": "claude",
        "event_type": "agent.candidate_stale",
        "source": "snapshot",
        "occurred_at": "2026-10-01T12:05:00Z",
        "received_at": "2026-10-01T12:05:00Z",
        "processed_at": "2026-10-01T12:05:00Z",
        "previous_live_state": "live",
        "live_state": "candidate_stale",
        "reason": "missing_from_provider_snapshot"
      }
    ]
  }
  ```

#### API-022 `ProviderHealth` and `Health`
- **Behavior:**

  `Health`:

  | Wire name | Go type | omitempty | Production value |
  |---|---|---|---|
  | `schema_version` | `int` | no | `1` |
  | `status` | `string` | no | always `ok` while the server answers |
  | `version` | `string` | no | `buildinfo.Version` (default `dev`) |
  | `commit` | `string` | yes | `buildinfo.Commit` (default `none`, which is non-empty and therefore emitted) |
  | `host_id` | `string` | no | persisted host id |
  | `started_at` | `time.Time` | no | server start time |
  | `providers` | `[]ProviderHealth` | no | one entry per adapter, in adapter order (`claude`, then `codex`); `[]` if none |

  `ProviderHealth`:

  | Wire name | Go type | omitempty | Meaning |
  |---|---|---|---|
  | `provider` | `Provider` | no | |
  | `status` | `string` | no | latest status: `available`, `unavailable`, `disabled`, `not_implemented`, `degraded` |
  | `version` | `string` | yes | provider version from the probe |
  | `discovery_source` | `string` | yes | `claude agents --json` (claude), `Codex app-server loaded-thread snapshot` (codex), `provider adapter` (other) |
  | `detail` | `string` | yes | latest detail; on success `discovery active; observed <n> live sessions` plus `; skipped <m> incompatible sessions` when m>0; on failure the error text; when disabled `provider discovery disabled` |
  | `observed_at` | `time.Time` | no | latest result time (UTC) |
  | `startup_status` | `string` | yes | first result for this process; immutable once recorded |
  | `startup_detail` | `string` | yes | first detail |
  | `startup_observed_at` | `*time.Time` | yes | first result time |

  Initial status from the probe: `disabled` if the probe diagnostic is exactly
  `provider discovery disabled`; else `not_implemented` if it equals
  `provider.ErrNotImplemented`'s text; else `available` if the probe is
  available; else `unavailable`. If the probe is unavailable or lacks the
  `discover_live` feature, the probe result is recorded as the startup result
  immediately; otherwise the first `update` (first reconcile: `available` or
  `degraded`) becomes the startup result.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/server/run.go`;
  `go/internal/server/health.go`; `go/internal/server/discovery.go`;
  `go/internal/buildinfo/buildinfo.go`
- **Decision:** D-032
- **Verified by:** `go/internal/server/run_test.go` TestRunServesInitialDiscovery (available/startup fields), TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers (`disabled`); `go/internal/server/health_test.go` TestProviderHealthInitialStatusMapping, TestProviderHealthPreservesFirstDiscoveryResult, TestProviderHealthUsesUnavailableProbeAsStartupResult, TestProviderHealthSnapshotOrderAndUnknownProvider.

  Annotated example (`--discovery=false`):
  ```jsonc
  {
    "schema_version": 1,
    "status": "ok",
    "version": "dev",
    "commit": "none",                       // emitted: "none" is non-empty
    "host_id": "c88452e1-1b7a-467d-8410-588aabaff572",
    "started_at": "2026-10-01T12:00:00.637823Z",
    "providers": [
      {
        "provider": "claude",
        "status": "disabled",
        "discovery_source": "claude agents --json",
        "detail": "provider discovery disabled",
        "observed_at": "2026-10-01T12:00:00.637823Z",
        "startup_status": "disabled",
        "startup_detail": "provider discovery disabled",
        "startup_observed_at": "2026-10-01T12:00:00.637823Z"
      },
      { "provider": "codex", "status": "disabled",
        "discovery_source": "Codex app-server loaded-thread snapshot", "...": "..." }
    ]
  }
  ```

#### API-023 `Error`
- **Behavior:** Every JSON error body MUST be
  `{"schema_version":1,"code":"<machine code>","message":"<human text>"}`, no
  field omitempty. The catalog of codes/messages is HTTP-010.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/localapi/server.go` (writeError)
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP

#### API-024 `AgentNameUpdate`
- **Behavior:** Request body for `PUT /v1/agents/{id}/name`: `{"name": string}`
  (no omitempty). An empty — or, after server trimming, whitespace-only —
  name clears the `user_name` override; it never touches the provider-reported
  `name`.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/localapi/server.go`
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (`"  "` clears)

#### API-025 `Observation` and `ObservationData` (persisted, not served)
- **Behavior:** These types define the hook-outbox / observation-log envelope.
  They are public Go types but no HTTP route returns them. Fields:

  `Observation`: `schema_version` int; `event_id` string; `provider`;
  `host_id` string; `native_session_id` string (omitempty); `event_type`
  string; `source` ObservationSource; `occurred_at` *time (omitempty);
  `received_at` time; `data` ObservationData; `error` string (omitempty).

  `ObservationData` (allow-list; all omitempty except `native_event`):
  `native_event`, `native_event_id`, `name`, `source_kind`, `cwd`, `model`,
  `initial_request` (*TextPreview), `summary` (*TextPreview),
  `attention_type`, `attention_detail`, `permission_mode`, `reason`,
  `previous_live_state`, `live_state`, `frontend_bindings`, `process_id`.
  Unknown provider fields and transcript paths MUST never be placed in it.
  `previous_live_state`/`live_state` are filled by the processor/reconciler
  after applying, not by the hook.
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/hooks/processor.go`
- **Decision:** D-028
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeClaudePromptIsBoundedAndCapturesFrontend, TestNormalizeNotificationGolden (golden envelopes); see [06-hooks.md](06-hooks.md).

#### API-034 Team names
- **Behavior:** A team is a name that agents carry in `team`; it has no object
  of its own and exists while at least one agent carries its name. Two agents
  are in the same team exactly when their `team` strings are equal
  (case-sensitive). `ValidTeamName(name)` MUST be true iff the name is 1 to
  `MaxTeamNameLength` = 48 bytes, every byte is one of `A-Za-z0-9._-`, the
  first byte is neither `-` nor `.`, and the name is not `none`. `NoTeam` =
  `"none"` is the reserved word that means "in no team" wherever a team name
  is accepted (the `--team` filters, `AgentSpawn.team`); the comparison is
  exact, so `None` is a valid team name.
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestValidTeamNames

#### API-035 `AgentTeamUpdate`
- **Behavior:** Request body for `PUT /v1/agents/{id}/team`:
  `{"team": string, "lead": bool}`; `lead` is omitempty. An empty `team`
  takes the agent out of its team. `lead: true` marks the agent as the lead
  of the team named, which it must already be in (HTTP-035).
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/localapi/teams.go`
- **Decision:** D-071
- **Verified by:** `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead, TestTeamAndSpawnErrorCatalog

#### API-036 `AgentSpawn`
- **Behavior:** Request body for `POST /v1/agents`, the registration of a
  provider session that the caller started. The server never starts one.

  | Wire name | Go type | omitempty | Meaning |
  |---|---|---|---|
  | `provider` | `Provider` | no | `claude` or `codex`. |
  | `native_session_id` | `string` | no | The provider's session id. |
  | `team` | `string` | yes | A team name, `none` for no team, or empty to take the parent's team. |
  | `parent_id` | `string` | yes | The `id` of the agent that spawned it. |
  | `name` | `string` | yes | A name for the agent. |
  | `directory` | `string` | yes | The directory it was started in. |
  | `prompt` | `string` | yes | The prompt it was started with. |
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/localapi/teams.go`
- **Decision:** D-076
- **Verified by:** `go/internal/localapi/teams_test.go` TestSpawnRegistrationRules, TestTeamAndSpawnErrorCatalog

#### API-037 `Team` and `TeamList`
- **Behavior:** `TeamList` is what `noc teams --json` writes
  ([03-cli.md](03-cli.md) CLI-044); no HTTP route returns it:
  `{"schema_version":1,"generated_at":<time>,"teams":[Team...]}`, no field
  omitempty. `Team` is a summary computed from the agents that carry the
  team's name:

  | Wire name | Go type | omitempty | Meaning |
  |---|---|---|---|
  | `team` | `string` | no | The team's name; `""` for the row of agents in no team. |
  | `lead_id` | `string` | yes | The `id` of the team's lead. |
  | `lead_name` | `string` | yes | The lead's `user_name`, else its `name`. |
  | `agents` | `int` | no | How many agents carry the name. |
  | `blocked`, `working`, `review`, `quiet` | `int` | no | How many of them are in each bucket; any other bucket value counts as `quiet`. |
- **Source:** `go/pkg/api/v1/types.go`; `go/internal/cli/teams.go`
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestSummarizeAndRenderTeams, TestTeamCommandsListsAndFilters (`teams --json`)

---

## 4. Derivation functions

#### API-026 `DeriveDisplayState` precedence
- **Behavior:** `DeriveDisplayState(agent)` MUST evaluate, top to bottom, and
  return the first match:

  | # | Condition | Result |
  |---|---|---|
  | 1 | `attention.type == approval_required` | `needs_approval` |
  | 2 | `attention.type == input_required` | `needs_input` |
  | 3 | `attention.type == authentication_required` | `needs_authentication` |
  | 4 | `attention.type == error` | `error` |
  | 5 | `activity_state == working` | `working` |
  | 6 | `activity_state == waiting` | `waiting` |
  | 7 | `activity_state == idle` AND `live_state == live` AND `runtime_state ∈ {running, loaded}` | `ready` |
  | 8 | `live_state == live` | `idle` |
  | 9 | `live_state == candidate_stale` | `stale` |
  | 10 | otherwise (`not_live`, `unknown`, `""`, any other) | `not_live` |

  Consequences a tester MUST be able to observe:
  - Attention `none`, `unknown`, `""` and unrecognized strings never produce a
    `needs_*` state.
  - Rows 1–6 ignore `live_state` and `runtime_state`: a `not_live` row whose
    last activity was `working` displays `working`; a dead row with pending
    approval displays `needs_approval`.
  - `idle` activity with `runtime_state` `starting`, `resumable`, `stopped`,
    `failed`, `unknown`, or `""` on a live row displays `idle`, not `ready`.
  - `unknown` activity on a live row displays `idle`.
  - `live_state == unknown` displays `not_live`.
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-044
- **Verified by:** `go/pkg/api/v1/types_test.go` TestDeriveDisplayState (every row, the attention `unknown` fall-through, `starting`+`idle`, and `live_state` `unknown`/empty)

#### API-027 `HasProcess`
- **Behavior:** MUST return `true` iff `live_state == live` AND
  `runtime_state ∈ {running, loaded, starting}`; `false` otherwise.

  | live_state \ runtime_state | running | loaded | starting | resumable/stopped/failed/unknown/"" |
  |---|---|---|---|---|
  | `live` | true | true | true | false |
  | `candidate_stale`, `not_live`, `unknown`, `""` | false | false | false | false |
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-048
- **Verified by:** `go/pkg/api/v1/types_test.go` TestHasProcess

#### API-028 `DeriveBucket`
- **Behavior:** MUST read the agent's **stored** `display_state` field (it does
  not recompute it) and return:

  | `HasProcess(agent)` | `display_state` | Bucket |
  |---|---|---|
  | false | any | `quiet` |
  | true | `needs_approval`, `needs_input`, `needs_authentication`, `error` | `blocked` |
  | true | `working`, `waiting` | `working` |
  | true | `ready` | `review` |
  | true | `idle`, `stale`, `not_live`, `""`, any other | `quiet` |

  Derived consequence: a live `starting` agent with `idle` activity has display
  `idle` (API-026 row 8) and therefore bucket `quiet`, although `HasProcess` is
  true; a live `starting` agent with `working` activity is bucket `working`.
- **Source:** `go/pkg/api/v1/types.go`
- **Decision:** D-046, D-048
- **Verified by:** `go/pkg/api/v1/types_test.go` TestDeriveBucketGroupsByWhatTheUserIsNeededFor (all ten display states with a running process; `starting` with `working` and with `idle`; three no-process cases)

#### API-029 `HooksSeeItsTerminal`
- **Behavior:** MUST return `false` iff `provider == "claude"` AND
  `source_kind == "background"` (exact, case-sensitive); `true` for every
  other combination (including Codex with any source kind, and Claude with
  empty source kind). Writers MUST NOT attach hook frontend bindings to an
  agent for which it returns false, and reconciliation clears existing ones.
- **Source:** `go/pkg/api/v1/types.go`; call sites `go/internal/hooks/processor.go`,
  `go/internal/reconcile/reconciler.go`
- **Decision:** D-050
- **Verified by:** `go/pkg/api/v1/types_test.go` TestHooksSeeItsTerminalAndNeverEngaged; `go/internal/reconcile/reconciler_bindings_test.go` TestBackgroundClaudeSessionsDropHookPaneBindings; `go/internal/hooks/processor_test.go` TestProcessorSuppressesBindingsForBackgroundClaude

#### API-030 `NeverEngaged`
- **Behavior:** MUST return `true` iff all of:
  `strings.TrimSpace(user_name) == ""`, `strings.TrimSpace(name) == ""`, and
  (`initial_request == nil` OR `strings.TrimSpace(initial_request.text) == ""`).
  `summary` is NOT considered. Used to drop ended sessions (hook processor
  forget on end; retention prune at any age, REG-015).
- **Source:** `go/pkg/api/v1/types.go`; call sites `go/internal/hooks/processor.go`,
  `go/internal/registry/registry.go`
- **Decision:** D-047
- **Verified by:** `go/pkg/api/v1/types_test.go` TestHooksSeeItsTerminalAndNeverEngaged (whitespace-only fields, `summary` ignored, each engaging field); `go/internal/registry/registry_test.go` TestPruneRetiredKeepsLiveAndNamedRows (`recent-never-engaged` pruned)

#### API-031 Derived fields are always written together
- **Behavior:** Every registry `Upsert` MUST set, in this order:
  `schema_version = 1`, `display_state = DeriveDisplayState(agent)`, then
  `bucket = DeriveBucket(agent)` (which therefore sees the fresh display
  state). Any caller-supplied `display_state`/`bucket` is discarded. The
  SQLite store additionally recomputes `bucket` from the stored
  `display_state` on every read (`scanAgent`), so a stored row always carries
  a bucket consistent with the current `DeriveBucket`; it does NOT recompute
  `display_state` on read.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-046
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (`schema_version` 1, `display_state` and `bucket` `working` after `Upsert`). Read-time bucket recomputation UNTESTED.

---

## 5. Identity generation

#### API-032 `identity.NewUUID` format
- **Behavior:** MUST read 16 bytes from `crypto/rand`; set byte 6 to
  `(b6 & 0x0f) | 0x40` (version 4) and byte 8 to `(b8 & 0x3f) | 0x80`
  (RFC 4122 variant); encode lowercase hex as
  `xxxxxxxx-xxxx-4xxx-Yxxx-xxxxxxxxxxxx` (8-4-4-4-12, 36 chars) where `Y ∈
  {8,9,a,b}`. On a `crypto/rand` read error it MUST return `("", err)`. IDs
  are random, NOT time-sortable. Used for agent ids
  (`hooks/processor.go`, `reconcile/reconciler.go`) and the host id.
- **Source:** `go/internal/identity/uuid.go`
- **Verified by:** `go/internal/identity/uuid_test.go` TestNewUUIDIsVersion4
  (format `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
  no duplicates in 50 draws). The `crypto/rand` error path is UNTESTED.

#### API-033 Host identity persistence
- **Behavior:** `Store.HostID` MUST return the `metadata` row `host_id` if
  present; otherwise generate a `NewUUID`, insert it with
  `ON CONFLICT(key) DO NOTHING`, and re-read, so concurrent first callers
  converge on one value. The host id is per database file: a new
  `NOC_DATABASE`/`NOC_STATE_DIR` yields a new host id.
- **Source:** `go/internal/storage/sqlite/store.go`
- **Decision:** D-016
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (stable across reopen)

---

## 6. Local API transport and socket

### 6.1 Socket lifecycle

#### HTTP-001 Transport
- **Behavior:** The API MUST be served as HTTP/1.1 over a Unix-domain stream
  socket only; there is no TCP listener. The socket path defaults to
  `<runtime dir>/noc.sock` and is overridden by `NOC_SOCKET` or a
  `--socket` flag ([03-cli.md](03-cli.md)). Clients send requests to the placeholder
  URL host `noc` (e.g. `http://noc/v1/health`); the server ignores Host.
- **Source:** `go/internal/localapi/socket.go`; `go/internal/localapi/client.go`;
  `go/internal/config/paths.go`
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket

#### HTTP-002 Parent directory creation
- **Behavior:** `Listen` MUST first call `config.EnsurePrivateDir(dir(socket))`:
  if the directory exists it MUST be left untouched (permissions are never
  changed on a directory noc did not create) but MUST be a directory, else
  error `private directory path "<p>" is not a directory`; if missing it MUST
  be created with `MkdirAll(..., 0700)` and then `chmod 0700` (errors
  `create private directory "<p>": <err>` and
  `secure private directory "<p>": <err>`). Other stat errors:
  `inspect private directory "<p>": <err>`.
- **Source:** `go/internal/localapi/socket.go` (Listen); `go/internal/config/paths.go` (EnsurePrivateDir)
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestListenCreatesPrivateParent; `go/internal/config/paths_test.go` TestEnsurePrivateDirDoesNotChmodExistingDirectory, TestEnsurePrivateDirSecuresNewDirectory. Error texts UNTESTED.

#### HTTP-003 Existing path handling (single instance, stale socket)
- **Behavior:** If `lstat(socket)` succeeds:
  1. If it is not a socket, `Listen` MUST fail with
     `refusing to replace non-socket path "<path>"` and MUST NOT delete it.
  2. Else it MUST attempt `net.DialTimeout("unix", path, 200ms)`. On success
     it closes the probe and returns `ErrServerRunning`
     (`noc server is already running`).
  3. On dial failure it MUST remove the stale socket (error:
     `remove stale socket "<path>": <err>`) and continue.
  If `lstat` fails with anything other than not-exist:
  `inspect socket "<path>": <err>`.
- **Source:** `go/internal/localapi/socket.go`
- **Decision:** D-026
- **Verified by:** `go/internal/localapi/server_test.go` TestListenRefusesNonSocketPath, TestListenDetectsLiveServer, TestListenReplacesStaleSocket; `go/internal/server/run_test.go` TestRunRefusesLiveSocketAndStorageFailureAborts

#### HTTP-004 Socket file mode
- **Behavior:** After `net.Listen` succeeds the socket file MUST be chmod
  `0600`. If chmod fails the listener MUST be closed, the socket removed, and
  `secure socket "<path>": <err>` returned. Listen failure:
  `listen on "<path>": <err>`.
- **Source:** `go/internal/localapi/socket.go`
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestListenSocketMode. The chmod-failure path is UNTESTED.

#### HTTP-005 Socket cleanup
- **Behavior:** `Listen` MUST return a cleanup function that removes the socket
  path, treating not-exist as success; other errors:
  `remove socket "<path>": <err>`. The server defers it after serving stops.
- **Source:** `go/internal/localapi/socket.go`; `go/internal/server/run.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestListenSocketMode (socket removed; a second cleanup succeeds)

#### HTTP-006 Server HTTP settings and shutdown
- **Behavior:** The production `http.Server` MUST use `ReadHeaderTimeout` 5 s
  and `IdleTimeout` 30 s, and no read/write timeout (so watch streams can stay
  open indefinitely). On context cancellation (SIGINT/SIGTERM) the server
  calls `Shutdown` with a 5 s deadline. `Shutdown` does not cancel in-flight
  request contexts, so an open watch stream keeps the server alive until that
  deadline; the server then exits with error `context deadline exceeded`
  (printed by the CLI as `noc: context deadline exceeded`). Without open
  streams shutdown is immediate.
- **Source:** `go/internal/server/run.go`
- **Verified by:** UNTESTED; MANUAL: §Verification step 10 (exit after ≈5 s with that message).

### 6.2 Routing, methods, and error shape

#### HTTP-007 Route table
- **Behavior:** `NewHandler` MUST register exactly these `http.ServeMux`
  patterns (method-less, Go 1.25 mux; module declares `go 1.25.0`):

  | Pattern | Handler | Methods accepted |
  |---|---|---|
  | `/v1/health` | handleHealth | GET |
  | `/v1/agents` | handleAgents | GET, POST |
  | `/v1/agents/watch` | handleAgentWatch | GET |
  | `/v1/agents/` (subtree) | handleAgent | `/v1/agents/{id}`: GET, DELETE; `/v1/agents/{id}/events`: GET; `/v1/agents/{id}/name`: PUT; `/v1/agents/{id}/team`: PUT |
  | `/v1/events` | handleEvents | GET |
  | `/v1/hooks/wake` | handleHookWake | POST |

  Any other path (e.g. `/v1/nope`, `/`) MUST fall through to the mux's
  default 404, which is **not** the JSON error shape: status 404,
  `Content-Type: text/plain; charset=utf-8`, `X-Content-Type-Options: nosniff`,
  body `404 page not found\n`. `/v1/agents/watch/` (trailing slash) is routed
  to the subtree handler and treated as agent id `watch`.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-019, D-031, D-033, D-071, D-076
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (every route; `/v1/nope` text 404; `/v1/agents/watch/` treated as an agent id); `go/internal/localapi/teams_test.go` TestTeamAndSpawnErrorCatalog (the team route and `POST /v1/agents`)

#### HTTP-008 Optional capabilities by interface assertion
- **Behavior:** `NewHandler(registry.Reader, HealthFunc, ...HookWakeFunc)` MUST
  enable rename, forget, events, and teams only if the reader also implements
  `registry.Namer`, `registry.Forgetter`, `registry.EventReader`,
  `registry.TeamStore` respectively; otherwise those routes answer 501
  (HTTP-010). Registering a spawn (`POST /v1/agents`) additionally needs the
  reader to implement `registry.Store` and a non-empty host id from
  `SetSpawnSupport(hostID, WorkspaceInspector)`; without all three it answers
  501. The inspector is optional (HTTP-036). Only the first variadic
  `HookWakeFunc` is used; none → `/v1/hooks/wake` answers 503. Renamers are
  set separately via `SetRenamers(map[Provider]provider.Renamer)`; never
  calling it is equivalent to an empty map. The production server passes the
  SQLite store (every one of these interfaces), a renamer for every adapter
  that implements `provider.Renamer` (Codex today), and
  `SetSpawnSupport(<host id>, <the workspace inspector>)`.
- **Source:** `go/internal/localapi/server.go`; `go/internal/localapi/teams.go`;
  `go/internal/server/run.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestUnavailableCapabilities (501 and 503 answers); `go/internal/localapi/teams_test.go` TestSpawnIsUnavailableWithoutAHost (spawn 501 without a host id); `go/internal/server/run_test.go` TestRunWithDisabledDiscoveryNeverProbesAndKeepsRenamers (production renamers); `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground (the production server registers spawns). The 501 for a reader without `TeamStore` is UNTESTED.

#### HTTP-009 Method enforcement
- **Behavior:** When a handler requires a method and the request uses another,
  it MUST respond 405 with header `Allow: <the single required method>` and
  JSON error `method_not_allowed` / `method not allowed`. `/v1/agents`
  accepts GET and POST, and answers any other method with
  `Allow: GET, POST`. For
  `/v1/agents/{id}` DELETE is dispatched before the GET check, so any other
  method gets `Allow: GET` (DELETE is accepted but not advertised). HEAD is
  not special-cased and gets 405 on every route.
- **Source:** `go/internal/localapi/server.go` (requireMethod, handleAgents, handleAgent)
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (405 and `Allow` on every route, including HEAD and `DELETE /v1/agents`); `go/internal/localapi/teams_test.go` TestTeamAndSpawnErrorCatalog (`GET /v1/agents/{id}/team`)

#### HTTP-010 Error catalog
- **Behavior:** All handler errors MUST be written by `writeError` as JSON
  (`Content-Type: application/json`, API-023 shape) with exactly these
  status/code/message triples:

  | Status | `code` | `message` | Raised by |
  |---|---|---|---|
  | 400 | `invalid_agent_id` | `an agent ID is required` | `/v1/agents/` with empty suffix, or more than 2 path segments after `/v1/agents/` (any method) |
  | 400 | `invalid_agent_name` | `request body must contain a name` | name body fails to decode (empty, malformed, unknown field, wrong type, >4096 bytes) |
  | 400 | `invalid_agent_name` | `agent name must be at most 120 characters` | trimmed name > 120 runes |
  | 400 | `invalid_provider` | `provider must be claude or codex` | events `provider` not empty/`claude`/`codex`; spawn `provider` not `claude`/`codex` |
  | 400 | `invalid_team` | `request body must contain a team` | team body fails to decode (empty, malformed, unknown field, wrong type, >4096 bytes) |
  | 400 | `invalid_team` | `a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none` | a team name that is not valid (API-034), on the team route or in a spawn |
  | 400 | `invalid_team` | `a lead needs a team` | team body with `lead: true` and an empty `team` |
  | 400 | `invalid_spawn` | `request body must describe a spawned agent` | spawn body fails to decode (empty, malformed, unknown field, wrong type, >64 KiB) |
  | 400 | `invalid_spawn` | `a native session ID is required` | spawn `native_session_id` blank after trimming, or over 512 runes |
  | 400 | `invalid_parent` | `the parent agent was not found` | spawn `parent_id` names no agent |
  | 400 | `invalid_limit` | `limit must be between 1 and 200` | events `limit` not an integer in [1,200] |
  | 404 | `agent_not_found` | `agent was not found` | get/delete/name/team/agent-events with unknown id |
  | 404 | `endpoint_not_found` | `endpoint was not found` | `/v1/agents/{id}/<anything other than events, name or team>` (any method) |
  | 405 | `method_not_allowed` | `method not allowed` | HTTP-009 |
  | 409 | `not_in_team` | `<id> is not in team <team>` | team body with `lead: true` for an agent that is not in that team |
  | 500 | `registry_error` | `could not list agents` | list or initial watch list failed |
  | 500 | `registry_error` | `could not read agent` | `Get` failed (agent route, agent-events existence check, a spawn's parent); `SetTeam` or `SetLead` failed on the team route |
  | 500 | `registry_error` | `could not register the spawned agent` | a spawn's lookup, insert or overlay update failed |
  | 500 | `registry_error` | `could not set the team's lead` | a spawn registered, but making its parent the lead failed (HTTP-037) |
  | 500 | `registry_error` | `could not forget agent` | `Forget` failed |
  | 500 | `registry_error` | `could not name agent` | `SetUserName` failed |
  | 500 | `registry_error` | `could not list events` | `ListEvents` failed |
  | 500 | `streaming_unavailable` | `response streaming is unavailable` | writer is not an `http.Flusher` |
  | 501 | `agent_forget_unavailable` | `forgetting agents is unavailable` | no Forgetter |
  | 501 | `agent_naming_unavailable` | `agent naming is unavailable` | no Namer |
  | 501 | `agent_teams_unavailable` | `agent teams are unavailable` | no TeamStore (team route) |
  | 501 | `agent_spawn_unavailable` | `registering spawned agents is unavailable` | no Store, no TeamStore, or no host id (HTTP-008) |
  | 501 | `events_unavailable` | `event queries are unavailable` | no EventReader |
  | 503 | `hook_ingestion_unavailable` | `hook ingestion is unavailable` | no HookWakeFunc |

  Internal error details MUST NOT be included in the message.
- **Source:** `go/internal/localapi/server.go`; `go/internal/localapi/teams.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (every 400, 404, and 405 row of the routes it had before teams), TestUnavailableCapabilities (501 and 503 rows); `go/internal/localapi/teams_test.go` TestTeamAndSpawnErrorCatalog (every `invalid_team`, `invalid_spawn`, `invalid_parent` and `not_in_team` row), TestSpawnIsUnavailableWithoutAHost (`agent_spawn_unavailable`). The 500 rows (`registry_error`, `streaming_unavailable`) and `agent_teams_unavailable` are UNTESTED.

#### HTTP-011 Success encoding
- **Behavior:** JSON success responses MUST be written with
  `Content-Type: application/json`, the status, then `json.NewEncoder(w).Encode(v)`
  — compact JSON followed by a single `\n`. Encode errors are ignored. No
  `Cache-Control`, ETag, or CORS headers are set (except on watch, HTTP-024).
- **Source:** `go/internal/localapi/server.go` (writeJSON)
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (body ends `}\n`), TestErrorCatalogOverRawHTTP (`Content-Type`)

### 6.3 Routes

#### HTTP-012 `GET /v1/health`
- **Behavior:** MUST return 200 with the `Health` produced by the injected
  `HealthFunc` (API-022), computed per request (provider entries reflect the
  latest discovery). Non-GET → 405 `Allow: GET`.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-032
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket; `go/internal/server/run_test.go` TestRunServesInitialDiscovery

#### HTTP-013 `GET /v1/agents`
- **Behavior:** (`POST` on this path is HTTP-036; any other method is 405
  `Allow: GET, POST`.) Query `all`: exactly the string `true` sets
  `IncludeNotLive`; any other value (`1`, `TRUE`, `yes`, empty, absent)
  means false. MUST return 200 `AgentList` with the registry's `List` result
  in registry order (REG-005), `[]` when empty. List error → 500
  `registry_error`/`could not list agents`. No pagination, no limit, no other
  filters (bucket and team filtering are CLI-side).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (empty list), TestAllQueryIsExactlyTrue (`all` absent, `1`, `TRUE`, `true`)

#### HTTP-014 `/v1/agents/{id}...` path parsing
- **Behavior:** The handler MUST compute
  `suffix = Trim(TrimPrefix(URL.Path, "/v1/agents/"), "/")` and split on `/`.
  `suffix == ""` or more than 2 parts → 400 `invalid_agent_id`. Leading and
  trailing slashes are ignored (`/v1/agents/abc/` ≡ `/v1/agents/abc`). Part 1
  is the agent id, used verbatim (no validation of UUID shape). With 2 parts,
  part 2 MUST be `events`, `name` or `team`, else 404 `endpoint_not_found`. Path is
  the decoded `URL.Path`; the Go client escapes ids with `url.PathEscape`.
- **Source:** `go/internal/localapi/server.go` (handleAgent)
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (empty suffix, three segments, unknown second segment)

#### HTTP-015 `GET /v1/agents/{id}`
- **Behavior:** MUST return 200 with the `Agent` from `registry.Get(id)`
  (including the `user_name`, `team`, `lead` and `parent_id` overlays and the
  read-time bucket). `ErrNotFound` → 404
  `agent_not_found`; other errors → 500 `registry_error`/`could not read agent`.
  Methods other than GET/DELETE → 405 `Allow: GET`.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-019
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (`GetAgent` returns the `user_name` overlay), TestErrorCatalogOverRawHTTP (404, 405)

#### HTTP-016 `DELETE /v1/agents/{id}` (forget)
- **Behavior:** MUST call `Forgetter.Forget(id)`. Success → 204 No Content
  with no body and no Content-Type. `ErrNotFound` → 404 `agent_not_found`
  (forgetting an already-absent row MUST fail, so a mistyped id is not
  mistaken for a cleanup). No Forgetter → 501 `agent_forget_unavailable`.
  Other errors → 500 `registry_error`/`could not forget agent`. Only the
  registry row is removed; the provider conversation and the observation
  journal are untouched (REG-014).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-040
- **Verified by:** `go/internal/localapi/server_test.go` TestForgetAgentRemovesTheRowOverTheSocket, TestErrorCatalogOverRawHTTP (204 with no body), TestUnavailableCapabilities (501)

#### HTTP-017 `PUT /v1/agents/{id}/name` — request validation
- **Behavior:** Order of checks MUST be: method PUT (else 405 `Allow: PUT`) →
  Namer present (else 501) → body read through `http.MaxBytesReader(4096)`
  and decoded by a `json.Decoder` with `DisallowUnknownFields` into
  `AgentNameUpdate` (any decode error → 400 `invalid_agent_name`/
  `request body must contain a name`) → `name = strings.TrimSpace(update.name)`
  → if `utf8.RuneCountInString(name) > 120` → 400
  `agent name must be at most 120 characters` → `SetUserName(id, name)`.
  Notes a tester MUST be able to observe:
  - `{}` and `null` decode successfully to an empty name and therefore
    **clear** the label (despite the "must contain a name" message).
  - Only the first JSON value is read; trailing bytes after it are ignored.
  - Exactly 120 runes is accepted; interior whitespace and control characters
    are preserved; the length check runs after trimming.
  - `Content-Type` is not checked.
  - Unknown agent id → 404 `agent_not_found` (detected by `SetUserName`, so
    validation errors win over not-found).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (set, whitespace clear), TestErrorCatalogOverRawHTTP (unknown field, empty body, 121 runes, body over 4096 bytes, exactly 120 multi-byte runes, `null` clears, validation before not-found); `go/internal/localapi/rename_test.go` TestRenameEmptyObjectBodyClears (`{"name":""}`). Trailing bytes after the first JSON value UNTESTED.

#### HTTP-018 `PUT /v1/agents/{id}/name` — response
- **Behavior:** On success MUST respond 200 with the `Agent` returned by
  `SetUserName` (the stored row with the new `user_name`, read before the
  provider push), header `Noc-Provider-Name-Sync: <ProviderNameSync>`
  always present, and header `Noc-Provider-Name-Sync-Error: <bounded reason>`
  present only when the sync value is `failed`. A provider failure MUST NOT
  change the status code or lose the stored name.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt

#### HTTP-019 Provider name push rules
- **Behavior:** After the name is stored, `pushNameToProvider` MUST decide:

  | Condition (first match) | Sync value | Error header |
  |---|---|---|
  | trimmed name is `""` | `not_attempted` (provider not called) | no |
  | no renamer registered for `agent.provider` | `unsupported` | no |
  | renamer returns an error matching `provider.ErrNotImplemented` | `unsupported` | no |
  | renamer returns any other error (incl. timeout) | `failed` | yes |
  | renamer returns nil | `synced` | no |

  The renamer is called as `SetName(ctx, agent.native_session_id, name)` with
  a context derived from the request context and bounded by
  `providerRenameTimeout` = 5 s. Claude has no renamer (always
  `unsupported`); Codex's adapter implements it.
- **Source:** `go/internal/localapi/server.go`; `go/internal/server/run.go`
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt (all four values, including a wrapped `ErrNotImplemented`; the renamer receives the native session id and the trimmed name). The 5 s timeout is UNTESTED.

#### HTTP-020 Sync error header bounding
- **Behavior:** The error header value MUST be `strings.Join(strings.Fields(err.Error()), " ")`
  (all whitespace runs, including newlines, collapsed to one space; leading
  and trailing removed); if the result exceeds 300 runes it MUST be cut to the
  first 300 runes followed by `…` (U+2026).
- **Source:** `go/internal/localapi/server.go` (boundedSyncError)
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt (whitespace collapsed; 400-rune tail cut to 300 runes plus `…`)

#### HTTP-035 `PUT /v1/agents/{id}/team`
- **Behavior:** Order of checks MUST be: method PUT (else 405 `Allow: PUT`) →
  TeamStore present (else 501 `agent_teams_unavailable`) → body read through
  `http.MaxBytesReader(4096)` and decoded with `DisallowUnknownFields` into
  `AgentTeamUpdate` (any decode error → 400 `invalid_team`/
  `request body must contain a team`) → a non-empty `team` that is not a
  valid name (API-034) → 400 `invalid_team` with the name rule. Then:

  | Body | Action | Result |
  |---|---|---|
  | `lead` false, `team` a name | `SetTeam(id, team)` | 200, the agent in that team |
  | `lead` false, `team` empty | `SetTeam(id, "")` | 200, the agent in no team |
  | `lead` true, `team` empty | none | 400 `invalid_team`/`a lead needs a team` |
  | `lead` true, `team` a name | `Get(id)`; if its `team` differs → 409 `not_in_team`/`<id> is not in team <team>`; else `SetLead(id)` | 200, the agent with `lead: true` |

  Unknown agent id → 404 `agent_not_found`, after the validation errors. Any
  other registry error → 500 `registry_error`/`could not read agent`. The
  name is not trimmed: `{}` and `null` decode to an empty team and take the
  agent out of its team. The effects of `SetTeam` and `SetLead` on the lead
  are REG-022. Success responds 200 with the updated `Agent`.
- **Source:** `go/internal/localapi/teams.go` (handleAgentTeam)
- **Decision:** D-071
- **Verified by:** `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead (set, move, clear, lead, lead outside the team), TestTeamAndSpawnErrorCatalog (405, every 400, 409, 404 with and without `lead`). The 501 and 500 answers are UNTESTED.

#### HTTP-036 `POST /v1/agents` — registering a spawned agent
- **Behavior:** The route records a provider session the caller started, so
  that the agent has its team and parent. It MUST NOT start, signal or
  contact a provider. Order of checks: Store, TeamStore and host id present
  (else 501 `agent_spawn_unavailable`) → body read through
  `http.MaxBytesReader(65536)` and decoded with `DisallowUnknownFields` into
  `AgentSpawn` (any decode error → 400 `invalid_spawn`/
  `request body must describe a spawned agent`) → `provider` is `claude` or
  `codex` (else 400 `invalid_provider`) → trimmed `native_session_id` is 1 to
  512 runes (else 400 `invalid_spawn`/`a native session ID is required`) →
  `team` is empty, `none`, or a valid name (else 400 `invalid_team`) →
  trimmed `name` is at most 120 runes (else 400 `invalid_agent_name`) → a
  non-empty `parent_id` names an existing agent (else 400 `invalid_parent`).

  The agent's team is then worked out (HTTP-037) and the row is found or
  made, by `GetByNativeIdentity(<host id>, provider, <session id>)`:

  - **A row exists** (a hook or a snapshot saw the session first): its team is
    set with `SetTeam` and its parent with `SetParent`. Nothing else on the
    row changes, so its `name`, `origin`, previews and workspace stay as the
    hook or snapshot wrote them, and the `name`, `directory` and `prompt` of
    the request are not used.
  - **No row exists:** a new agent is stored with a new `id` (API-032), the
    host id, `provider`, the session id, `name` = the trimmed request name,
    `origin` = `spawn`, `live_state` = `live`, `runtime_state` = `starting`,
    `activity_state` = `unknown`, `attention` = `{"type":"none"}`,
    `created_at` = `first_seen_at` = `last_seen_at` = now (UTC), and the team
    and parent. A non-blank `prompt` becomes `initial_request` with `source`
    = `spawn`, cut to its first 2048 runes with `truncated` set when longer.
    A non-blank `directory` becomes `workspace`: the inspector's description
    with owner `unknown` when an inspector is set and returns one, else
    `{"path": <directory>, "owner": "unknown", "observed_at": now}`. Derived
    fields follow API-031, so the row reads `display_state` `idle`, `bucket`
    `quiet` until a hook or a snapshot reports the session.

  Success MUST respond **201** with the `Agent` as stored, in both cases.
  A registry failure → 500 `registry_error`/
  `could not register the spawned agent`. A later hook or snapshot for the
  same session finds the row by its natural key (REG-007) and keeps its `id`,
  `origin`, team, lead and parent (REG-021).
- **Source:** `go/internal/localapi/teams.go` (handleSpawn, registerSpawn);
  `go/internal/server/run.go`
- **Decision:** D-076
- **Verified by:** `go/internal/localapi/teams_test.go` TestSpawnRegistrationRules (new row fields, the row a hook made first, a long prompt, no directory), TestTeamAndSpawnErrorCatalog (every 400), TestSpawnIsUnavailableWithoutAHost (501); `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground (the production server, with the workspace inspector). The 201 status and the 500 answers are UNTESTED.

#### HTTP-037 Team and lead of a spawned agent
- **Behavior:** With `parent` the agent named by `parent_id` (none when it is
  empty), the spawned agent's team MUST be:

  | `team` in the request | Team of the spawned agent |
  |---|---|
  | a name | that team |
  | `none` | no team |
  | empty | the parent's team; no team when there is no parent or it is in none |

  After the row is stored, exactly one rule can change another agent: when
  the request **names** a team, there is a parent, the parent is in **no**
  team, and no agent (live or not) is that team's lead, the parent MUST be
  put in the team and made its lead. In every other case no other agent
  changes: a parent that is already in a team stays there and is not made
  lead, a team that has a lead keeps it, and an inherited team never changes
  its lead. The spawned agent itself is never a lead. A failure of this step
  → 500 `registry_error`/`could not set the team's lead`; the spawned agent
  is already stored. The check and the two writes are not one transaction.
- **Source:** `go/internal/localapi/teams.go` (handleSpawn, teamHasLead)
- **Decision:** D-071
- **Verified by:** `go/internal/localapi/teams_test.go` TestSpawnRegistrationRules (named, inherited, `none`, nothing to inherit, the parent made lead, a led team, a parent in another team); `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground. The 500 answer is UNTESTED.

#### HTTP-021 `GET /v1/events` and `GET /v1/agents/{id}/events`
- **Behavior:** Order of checks MUST be: path/method (GET) → EventReader
  present (else 501 `events_unavailable`) → for the per-agent route,
  `registry.Get(id)` (`ErrNotFound` → 404 `agent_not_found`; other → 500
  `could not read agent`) → `provider` query (empty, `claude`, `codex`; else
  400 `invalid_provider`) → `limit` query (absent/empty → 50; otherwise
  `strconv.Atoi` must succeed and 1 ≤ n ≤ 200, else 400 `invalid_limit`) →
  `ListEvents({AgentID, Provider, Limit})` (error → 500 `could not list events`)
  → 200 `AgentEventList` (API-021) with `agent_id` = path id (or omitted) and
  `provider` = filter (or omitted). The per-agent route still applies the
  `provider` filter, so a mismatched provider yields `events: []`. Constants:
  `registry.DefaultEventLimit = 50`, `registry.MaxEventLimit = 200`.
- **Source:** `go/internal/localapi/server.go`; `go/internal/registry/registry.go`
- **Decision:** D-033
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerListAgentEvents (filters echoed, default limit, missing agent 404), TestErrorCatalogOverRawHTTP (`limit` 0/201/`abc`, bad `provider`, existence checked before `provider`), TestUnavailableCapabilities (501)

#### HTTP-022 `POST /v1/hooks/wake`
- **Behavior:** POST only (else 405 `Allow: POST`). No HookWakeFunc → 503
  `hook_ingestion_unavailable`. Otherwise MUST invoke the wake function
  synchronously and respond 202 Accepted with an empty body and no
  Content-Type. The request body is ignored: the event itself is read from the
  SQLite outbox, never trusted from HTTP. In production the wake function is a
  non-blocking send on a channel of capacity 1, so repeated wakes coalesce
  and never block the handler.
- **Source:** `go/internal/localapi/server.go`; `go/internal/server/run.go`
- **Decision:** D-028
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket (one call → one wake), TestErrorCatalogOverRawHTTP (202, empty body), TestUnavailableCapabilities (503)

### 6.4 Watch stream

#### HTTP-023 Watch preconditions
- **Behavior:** `GET /v1/agents/watch` (else 405 `Allow: GET`). If the
  response writer is not an `http.Flusher` → 500 `streaming_unavailable`.
  `all=true` (exact) includes `not_live` rows, as HTTP-013. The initial `List`
  runs **before** any header is written; if it fails the response is a normal
  JSON 500 `registry_error`/`could not list agents`.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestErrorCatalogOverRawHTTP (405). `streaming_unavailable` and the initial-list 500 are UNTESTED.

#### HTTP-024 Watch headers and initial frame
- **Behavior:** On success the server MUST write status 200 with
  `Content-Type: application/x-ndjson` and `Cache-Control: no-cache`
  (chunked transfer), then immediately one complete `AgentList` frame for the
  initial list, flushed. The initial frame is sent even when the registry is
  empty (`"agents":[]`).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (initial empty frame), TestWatchHeaders (status, both headers, first line)

#### HTTP-025 Watch frame format
- **Behavior:** Each frame MUST be one `AgentList` JSON object encoded by
  `json.Encoder` (compact, terminated by exactly one `\n`), flushed after
  each write. Every frame is a complete, independently sufficient snapshot
  (no deltas, no event vocabulary); `generated_at` is fresh per frame;
  `schema_version` is 1. Frames carry current values of all fields, including
  freshness-only timestamps, even though those did not trigger the frame.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots

#### HTTP-026 Watch poll interval
- **Behavior:** After the initial frame the handler MUST poll `List` with a
  `time.Ticker` at `watchInterval` (default `defaultWatchInterval` = 500 ms;
  values ≤ 0 fall back to 500 ms). The first re-poll happens one interval
  after the initial frame. Each watch connection polls independently (N
  watchers ⇒ N `List` calls per interval). There is no push path from writes
  to watchers; maximum change latency ≈ one interval.
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (interval overridden to 10 ms through `SetWatchInterval`); the 500 ms default is UNTESTED.

#### HTTP-027 Watch change detection (semantic fingerprint)
- **Behavior:** A frame MUST be emitted iff
  `watchFingerprint(current) != previousFingerprint`, where the fingerprint is
  SHA-256 of `json.Marshal` of a copy of the agent slice (same order) in which
  exactly these fields are zeroed:

  | Zeroed (freshness-only; changes alone never emit) |
  |---|
  | `last_seen_at` (set to zero time) |
  | `snapshot_confirmed_at` (nil) |
  | `model_observed_at` (nil) |
  | `reasoning_observed_at` (nil) |
  | `summary_updated_at` (nil) |
  | `initial_request.observed_at` (zero; rest of preview kept) |
  | `workspace.observed_at` (zero; rest of workspace kept) |
  | every `frontend_bindings[i].observed_at` (zero) |

  Every other difference IS semantic and MUST emit a frame, including:
  membership (row added/removed, e.g. a row becoming `not_live` in the default
  view), list order, `user_name`, `team`, `lead`, `parent_id`, `name`, any
  state/attention field,
  `attention.since`, `display_state`, `bucket`, `first_seen_at`,
  `last_activity_at`, `created_at`, `ended_at`, `capabilities`,
  `native_state` bytes, `process_id`, preview text/source/truncation,
  workspace path/branch/HEAD, binding kind/session/pane/state. The original
  agents are not mutated (bindings slice is copied before zeroing).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestFingerprintIgnoresFreshnessOnlyFields (every zeroed field; input not mutated; `process_id` is semantic), TestWatchAgentsStreamsSemanticSnapshots (add, rename, freshness-only change emits no frame within 50 ms, removal)

#### HTTP-028 Watch fingerprint advancement
- **Behavior:** `previousFingerprint` MUST be updated only after a frame was
  written successfully. Unchanged polls write nothing (no heartbeat/keepalive
  frames are ever sent).
- **Source:** `go/internal/localapi/server.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (no frame while nothing semantic changes). Advancement only after a successful write is UNTESTED.

#### HTTP-029 Watch termination
- **Behavior:** The stream MUST end (handler returns, response completes)
  when: the request context is done (client disconnected/cancelled); a
  subsequent `List` fails (silently — no error frame, no status change, since
  headers were already sent); or encoding/writing a frame fails. The server
  never ends a healthy stream on its own except via shutdown (HTTP-006).
- **Source:** `go/internal/localapi/server.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (client cancellation), TestWatchEndsOnListError (silent end on a later `List` error)

### 6.5 Go client

#### HTTP-030 Client construction
- **Behavior:** `NewClient(socketPath)` MUST build an `http.Client` whose
  transport dials `unix` at `socketPath` for every connection, ignoring the
  URL network/address. No client- or transport-level timeouts are set; all
  deadlines come from the caller's context. `Close()` closes idle
  keep-alive connections.
- **Source:** `go/internal/localapi/client.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket

#### HTTP-031 Client method → request mapping
- **Behavior:**

  | Method | Request | Returns |
  |---|---|---|
  | `Health(ctx)` | `GET /v1/health` | `Health` |
  | `ListAgents(ctx, includeNotLive)` | `GET /v1/agents` (+`?all=true` iff true) | `AgentList` |
  | `GetAgent(ctx, id)` | `GET /v1/agents/<PathEscape(id)>` | `Agent` |
  | `RenameAgent(ctx, id, name)` | `PUT /v1/agents/<PathEscape(id)>/name`, `Content-Type: application/json`, body `{"name":"<name>"}` (name sent untrimmed) | `AgentNameResult{Agent, ProviderSync (header), ProviderSyncError (header)}` |
  | `SetAgentName(ctx, id, name)` | same as `RenameAgent` | `Agent` only (headers discarded) |
  | `SetAgentTeam(ctx, id, team)` | `PUT /v1/agents/<PathEscape(id)>/team`, `Content-Type: application/json`, body `{"team":"<team>"}` (empty takes the agent out of its team) | `Agent` |
  | `SetTeamLead(ctx, id, team)` | same route, body `{"team":"<team>","lead":true}` | `Agent` |
  | `RegisterSpawn(ctx, AgentSpawn)` | `POST /v1/agents`, `Content-Type: application/json`, body the `AgentSpawn` | `Agent` |
  | `ForgetAgent(ctx, id)` | `DELETE /v1/agents/<PathEscape(id)>` | error only; body not decoded |
  | `ListEvents(ctx, agentID, provider, limit)` | `GET /v1/events`, or `/v1/agents/<PathEscape(agentID)>/events` when `agentID != ""`; query from `url.Values` (keys sorted: `limit`, `provider`); `provider` only if non-empty; `limit` only if > 0 (so ≤0 ⇒ server default 50) | `AgentEventList` |
  | `WatchAgents(ctx, includeNotLive, handle)` | `GET /v1/agents/watch` (+`?all=true`) | streams; see HTTP-034 |
  | `WakeHooks(ctx)` | `POST /v1/hooks/wake`, no body | error only |
- **Source:** `go/internal/localapi/client.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket, TestClientAndServerListAgentEvents, TestForgetAgentRemovesTheRowOverTheSocket, TestWatchAgentsStreamsSemanticSnapshots; `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt; `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead, TestSpawnRegistrationRules

#### HTTP-032 Client error mapping
- **Behavior:** For all non-watch JSON calls (`doJSON`):

  | Condition | Returned error text | `errors.Is(err, ErrConnection)` |
  |---|---|---|
  | `http.Client.Do` fails (dial refused/missing socket, context deadline, cancellation) | `connect to noc server: <cause>` (cause formatted with `%v`, NOT unwrappable) | true |
  | status outside [200,300) and body decodes as `Error` with non-empty `message` | `noc server: <message>` (the `code` is discarded) | false |
  | status outside [200,300), body not such JSON (e.g. mux text 404) | `noc server: unexpected HTTP status <Status>` e.g. `... 404 Not Found` | false |
  | 2xx but body fails to decode into the target | `decode noc server response: <err>` | false |
  | request construction fails | `create request: <err>` | false |
  | `RenameAgent` JSON encode fails | `encode agent name: <err>` | false |
  | `SetAgentTeam`/`SetTeamLead` JSON encode fails | `encode agent team: <err>` | false |
  | `RegisterSpawn` JSON encode fails | `encode spawned agent: <err>` | false |

  `WakeHooks` maps transport errors the same way but never decodes error
  bodies: any non-2xx → `noc server: unexpected HTTP status <Status>`.
  Because timeouts surface as transport errors, a request that times out is
  classified as a connection error, and `errors.Is(err, context.DeadlineExceeded)`
  is false for non-watch calls. The CLI prints connection errors as
  `noc: <err>` followed by `Is the server running? Start it with: noc server`
  and exits 1.
- **Source:** `go/internal/localapi/client.go`; `go/internal/cli/run.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestClientConnectionErrors (missing socket for a JSON call, wake, and watch; cause not unwrappable), TestClientUnexpectedStatus (text 404, wake non-2xx, decode failure), TestClientAndServerListAgentEvents (server message surfaced, not a connection error); `go/internal/cli/run_test.go` TestServerDownShowsConnectionHint. A deadline expiry specifically is UNTESTED.

#### HTTP-033 Caller timeouts (CLI)
- **Behavior:** The client has no defaults; current callers MUST bound requests
  as follows:

  | Caller | Timeout | Source |
  |---|---|---|
  | `ls`, `show`, `attach` lookup, `forget`, `stop` lookup, `events`, `server status` style requests | `requestTimeout` = 3 s | `go/internal/cli/run.go` |
  | `team` (one 3 s budget for the whole command), `teams`, and each of `spawn`'s two server phases (finding the caller, registering) | `requestTimeout` = 3 s | `go/internal/cli/teams.go` |
  | `rename` (covers the server's 5 s provider push) | `renameRequestTimeout` = 10 s | `go/internal/cli/run.go` |
  | hook ingest context (SQLite open, host ID, enqueue, wake) / wake notify | `hookIngestTimeout` = 900 ms / `hookWakeTimeout` = 150 ms; reading stdin is not interrupted by it, so it is not an overall bound (HK-005) | `go/internal/cli/run.go` |
  | server liveness probe | `serverProbeTimeout` = 300 ms | `go/internal/cli/server_lifecycle.go` |
  | `watch` | none (until interrupted) | `go/internal/cli/run.go` |

  `noc hook ingest` MUST ignore a wake failure (exit 0).
- **Source:** as cited
- **Verified by:** constants UNTESTED; `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown (wake failure ignored). See [03-cli.md](03-cli.md).

#### HTTP-034 `WatchAgents` client semantics
- **Behavior:**
  1. Transport failure → `connect to noc server: <cause>` wrapped with `%w`
     on both sides, so `errors.Is(err, ErrConnection)` AND
     `errors.Is(err, context.Canceled)` hold for a pre-cancelled context.
  2. Non-2xx → same message rules as HTTP-032 (`noc server: <message>` or
     `unexpected HTTP status`).
  3. Frames are decoded sequentially with one `json.Decoder`; `handle` is
     called once per frame, in order, synchronously (a slow handler applies
     back-pressure). A nil `handle` discards frames. A non-nil error from
     `handle` stops the watch and is returned as-is.
  4. On a decode error: if `ctx.Err() != nil` return `ctx.Err()` unwrapped
     (e.g. `context.Canceled`, NOT a connection error); else if `io.EOF`
     return `connect to noc server: agent watch stream closed`
     (`ErrConnection`); else `decode agent watch stream: <err>`.
  5. There is no automatic reconnect; callers decide.
- **Source:** `go/internal/localapi/client.go`
- **Decision:** D-031
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsPreservesCanceledContext (case 1), TestWatchAgentsStreamsSemanticSnapshots (case 4 cancellation), TestWatchAgentsEOFAndHandlerError (EOF text, handler error returned as-is). A nil `handle` and a mid-stream decode error are UNTESTED.

---

## 7. Registry

#### REG-001 Not-found sentinel
- **Behavior:** `registry.ErrNotFound` (`agent not found`) MUST be returned
  (directly, matchable with `errors.Is`) by `Get`, `GetByNativeIdentity`,
  `SetUserName`, `SetTeam`, `SetLead`, `SetParent`, and `Forget` when no row
  matches, by both implementations.
  The HTTP layer maps it to 404 `agent_not_found`.
- **Source:** `go/internal/registry/registry.go`;
  `go/internal/storage/sqlite/store.go`
- **Verified by:** `go/internal/localapi/server_test.go` TestForgetAgentRemovesTheRowOverTheSocket; `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestForgetDeletesOnlyTheNamedAgent; `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot (the three team methods); `go/internal/registry/registry_test.go` TestMemoryProviderRenameReplacesUserName

#### REG-002 Interfaces
- **Behavior:** The registry contract MUST consist of:

  | Interface | Methods |
  |---|---|
  | `Reader` | `List(ctx, ListOptions) ([]Agent, error)`; `Get(ctx, id) (Agent, error)` |
  | `Namer` | `SetUserName(ctx, id, name) (Agent, error)` |
  | `TeamStore` | `SetTeam(ctx, id, team) (Agent, error)`; `SetLead(ctx, id) (Agent, error)`; `SetParent(ctx, id, parentID) (Agent, error)` |
  | `Forgetter` | `Forget(ctx, id) error` |
  | `Pruner` | `PruneRetired(ctx, endedBefore time.Time) (int, error)` |
  | `EventReader` | `ListEvents(ctx, EventListOptions) ([]AgentEvent, error)` |
  | `Store` | `Reader` + `GetByNativeIdentity(ctx, hostID, provider, nativeSessionID) (Agent, error)` + `Upsert(ctx, Agent) error` |
  | `ObservationStore` | `EnqueueObservation`, `PendingObservations`, `MarkObservationProcessed`, `MarkObservationFailed`, `RecordObservation` (the hook outbox and observation journal; specified in [06-hooks.md](06-hooks.md)) |

  `ListOptions{IncludeNotLive bool}`;
  `EventListOptions{AgentID string, Provider Provider, Limit int}`.
  `registry.Memory` implements Store, Namer, TeamStore, Forgetter, Pruner
  (not EventReader or ObservationStore). The SQLite `Store` implements all
  eight.
  Both sets are compile-time asserted. The production server uses SQLite
  only; Memory is used by tests.
- **Source:** `go/internal/registry/registry.go`;
  `go/internal/storage/sqlite/store.go`; `go/internal/storage/sqlite/hooks.go`
- **Verified by:** compile-time assertions.

#### REG-003 Memory store concurrency
- **Behavior:** `registry.Memory` MUST guard its map with a `sync.RWMutex`
  (reads under RLock; Upsert/SetUserName/Prune/Forget under Lock) and return
  copies of `Agent` values (pointer fields inside are shared).
- **Source:** `go/internal/registry/registry.go`
- **Verified by:** UNTESTED (no race test).

#### REG-004 List filter
- **Behavior:** `List` with `IncludeNotLive == false` MUST exclude exactly the
  rows whose `live_state == "not_live"` and include all others (`live`,
  `candidate_stale`, `unknown`, empty). With `true` it MUST return all rows.
  No other filter exists.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-017, D-041
- **Verified by:** `go/internal/localapi/server_test.go` TestAllQueryIsExactlyTrue, TestWatchAgentsStreamsSemanticSnapshots (a row turned `not_live` leaves the default watch); `go/internal/storage/sqlite/store_test.go` TestListFiltersAndOrdersAlphabetically (`IncludeNotLive` true and false)

#### REG-005 List ordering
- **Behavior:** `List` MUST order by `display_state` ascending (byte-wise
  string order), then `id` ascending. This is alphabetical, NOT bucket order:

  `error` < `idle` < `needs_approval` < `needs_authentication` < `needs_input`
  < `not_live` < `ready` < `stale` < `waiting` < `working`.

  SQLite uses `ORDER BY display_state, id` (BINARY collation); Memory uses
  `sort.Slice` with the same keys. Frontends/CLI that want bucket order MUST
  re-sort (the CLI `ls` sorts by `Bucket.Rank()`, then most recent activity first).
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`;
  `go/internal/cli/run.go` (sortByBucket)
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestListFiltersAndOrdersAlphabetically (`idle` before `working`), TestPruneRetiredMatchesTheMemoryPredicate (SQLite and Memory agree on order); `go/internal/cli/run_test.go` TestListFiltersAndOrdersByBucket (CLI re-sort)

#### REG-006 List and Get return values
- **Behavior:** `List` MUST return a non-nil (possibly empty) slice on success.
  SQLite `List`/`Get` decode `snapshot_json`, overlay `user_name` from its
  column, and recompute `bucket` (API-031). A row whose snapshot fails to
  decode makes the whole `List` fail (`decode stored agent: <err>`).
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestListFiltersAndOrdersAlphabetically (non-nil empty list). The decode-failure path is UNTESTED.

#### REG-007 Natural-key lookup
- **Behavior:** `GetByNativeIdentity(hostID, provider, nativeSessionID)` MUST
  return the unique row with that triple (SQLite enforces
  `UNIQUE(host_id, provider, native_session_id)`), else `ErrNotFound`. Writers
  use it to reuse the existing noc `id`; a first sighting allocates a new
  `NewUUID`.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`;
  `go/internal/storage/sqlite/migrations.go`
- **Decision:** D-016
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents, TestUpsertNaturalKeyConflictWithDifferentIDFails

#### REG-008 Upsert required fields
- **Behavior:** SQLite `Upsert` MUST reject an agent missing any of `id`,
  `host_id`, `provider`, `native_session_id` with
  `agent ID, host ID, provider, and native session ID are required`.
  `registry.Memory.Upsert` performs no validation.
- **Source:** `go/internal/storage/sqlite/store.go`; `go/internal/registry/registry.go`
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (error text)

#### REG-009 Upsert replaces the whole snapshot
- **Behavior:** `Upsert` MUST replace the stored agent with the given value
  (after API-031 derivations) keyed by `id`; it does not merge fields. Merging
  with the previous row is the writer's job (reconcile/hooks). SQLite also
  updates the indexed columns `host_id`, `provider`, `native_session_id`,
  `live_state`, `display_state`, `last_seen_at` (RFC 3339 Nano UTC). The
  overlays are the exceptions: `user_name` (REG-010, REG-011) and `team`,
  `lead`, `parent_id` (REG-021).
- **Source:** `go/internal/storage/sqlite/store.go`; `go/internal/registry/registry.go`
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents

#### REG-010 `user_name` is an overlay Upsert cannot set
- **Behavior:** The `user_name` carried by the agent passed to `Upsert` MUST be
  ignored. SQLite stores the snapshot with `user_name` blanked and keeps the
  label in a separate `user_name` column (`TEXT NOT NULL DEFAULT ''`); Memory
  copies the existing value. A brand-new row always starts with
  `user_name = ""`. Only `SetUserName` writes a non-empty label.
- **Source:** `go/internal/storage/sqlite/store.go`; `go/internal/registry/registry.go`;
  `go/internal/storage/sqlite/migrations.go`
- **Decision:** D-035
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (incoming `user_name` ignored; label survives refresh and reopen); `go/internal/registry/registry_test.go` TestMemoryProviderRenameReplacesUserName

#### REG-011 Provider rename clears the label (most recent wins)
- **Behavior:** On `Upsert` of an existing row, `user_name` MUST be cleared iff
  `ProviderRenamed(previousStoredName, incoming.name)`, i.e. the incoming
  provider `name` is non-empty AND differs from the previously stored
  provider `name`; otherwise it MUST be kept. An observation without a name,
  or with the same name, never erases a label. SQLite evaluates the same
  predicate in SQL against `json_extract(snapshot_json, '$.name')` (NULL → `''`).
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-042 (supersedes D-035 precedence)
- **Verified by:** `go/internal/registry/registry_test.go` TestMemoryProviderRenameReplacesUserName;
  `go/internal/storage/sqlite/store_test.go` TestProviderRenameReplacesUserName

#### REG-012 `SetUserName`
- **Behavior:** MUST store `name` verbatim (no trimming or length check at this
  layer — the HTTP handler does both) and return the updated agent; `""`
  clears. Unknown id → `ErrNotFound`. It MUST NOT modify `name`,
  `display_state`, or any other field, and MUST NOT call the provider.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (stored verbatim, untrimmed; unknown id); `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (rename emits a watch frame)

#### REG-013 After a rename is pushed to Codex
- **Behavior:** (Derived consequence of REG-011 + HTTP-019, stated for
  testers.) After a `synced` Codex rename, the next snapshot carries the new
  provider `name`, which differs from the old stored `name`, so `user_name` is
  cleared and the label now lives in `name`. The visible label is unchanged.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-042
- **Verified by:** UNTESTED end-to-end.

#### REG-021 `team`, `lead` and `parent_id` are overlays
- **Behavior:** Like `user_name`, the three are kept outside the stored
  snapshot. SQLite blanks them in the snapshot JSON and keeps them in the
  columns `team`, `team_lead` and `parent_id`
  ([04-server.md](04-server.md) DB-026, DB-027); every read overlays them, with
  `lead` reported `true` only when `team_lead` is set **and** `team` is not
  empty. `Upsert` treats them as follows, in both implementations:

  | Row | `team`, `parent_id` | `lead` |
  |---|---|---|
  | new (no row with that `id`) | taken from the incoming agent | always `false`, whatever the incoming agent carries |
  | existing | the stored values are kept; the incoming ones are ignored | the stored value is kept |

  So the snapshots that hooks and reconciliation write can neither set nor
  erase a team, a lead or a parent, and a spawn registration (HTTP-036) can
  give a new row its team and parent in one write. A provider rename does not
  affect them. They survive a server restart.
- **Source:** `go/internal/storage/sqlite/store.go` (Upsert, scanAgent);
  `go/internal/registry/registry.go` (Memory.Upsert)
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot (new row, snapshot JSON without them, upsert of an existing row, restart); `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead (Memory: an upsert keeps the team). Memory's new-row rule is covered only through TestSpawnRegistrationRules.

#### REG-022 `SetTeam`, `SetLead`, `SetParent`
- **Behavior:** All three return the updated agent, return `ErrNotFound` for
  an unknown id, store their argument verbatim (name validation is the HTTP
  handler's), and MUST NOT change any other field or call a provider.

  - `SetTeam(id, team)` puts the agent in `team`, moving it out of any other;
    `""` takes it out. An agent whose team **changes** stops being a lead;
    setting the team it is already in keeps its lead mark.
  - `SetLead(id)` marks the agent as the lead of the team it is in and clears
    the mark on every other agent of that team, so a team has at most one
    lead; SQLite does both in one transaction. An agent in no team →
    `registry.ErrNoTeam` (`agent is not in a team`), and nothing changes.
  - `SetParent(id, parentID)` records the agent that spawned this one; `""`
    clears it. The parent id is not checked against the registry.

  A team needs no lead, and removing the lead from a team (by moving or
  clearing it) leaves the team without one.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-071
- **Verified by:** `go/internal/storage/sqlite/teams_test.go` TestTeamLeadAndParentAreStoredBesideTheSnapshot (every rule, `ErrNoTeam`, `ErrNotFound`, one lead in the list after a restart); `go/internal/localapi/teams_test.go` TestTeamMembershipAndLead (Memory, through the API). Memory's `SetParent` is covered only through TestSpawnRegistrationRules.

#### REG-014 Forget
- **Behavior:** `Forget(id)` MUST delete only the row with that `id` and
  return `ErrNotFound` if none was deleted. It MUST NOT touch the provider,
  other rows, or the observation journal. Because the natural-key mapping is
  deleted with the row, a still-live session that reappears via hook or
  snapshot is allocated a **new** noc `id` and loses its `user_name`, its
  team, its lead mark and its parent. Other agents that name the forgotten
  one in `parent_id` keep that value.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-040, D-041
- **Verified by:** `go/internal/storage/sqlite/store_test.go` TestForgetDeletesOnlyTheNamedAgent;
  `go/internal/localapi/server_test.go` TestForgetAgentRemovesTheRowOverTheSocket. New-id-on-rediscovery UNTESTED.

#### REG-015 PruneRetired predicate
- **Behavior:** `PruneRetired(endedBefore)` MUST delete every row for which
  `retired` is true and return the number deleted:

  | live_state | trimmed user_name | NeverEngaged | last_seen_at < endedBefore | Pruned |
  |---|---|---|---|---|
  | ≠ `not_live` | any | any | any | no |
  | `not_live` | non-empty | any | any | no |
  | `not_live` | empty | true | any | **yes** |
  | `not_live` | empty | false | true | **yes** |
  | `not_live` | empty | false | false | no |

  Age is measured from `last_seen_at`, not from retirement. SQLite implements
  the same predicate in SQL (NeverEngaged as trimmed `$.name` empty AND
  trimmed `$.initial_request.text` empty; user_name already checked). Being
  in a team, leading one, or having a parent does not protect a row.
- **Source:** `go/internal/registry/registry.go`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-041, D-047
- **Verified by:** `go/internal/registry/registry_test.go` TestPruneRetiredKeepsLiveAndNamedRows;
  `go/internal/storage/sqlite/store_test.go` TestPruneRetiredMatchesTheMemoryPredicate

#### REG-016 Retention schedule (server)
- **Behavior:** The server MUST call `PruneRetired(now − 14 days)` once when
  its loops start and then every hour (`DefaultRetireAfter` = 336 h,
  `DefaultRetentionInterval` = 1 h); a negative configured retention disables
  it. A failed sweep is logged and the loop continues.
- **Source:** `go/internal/server/run.go`; `go/internal/server/discovery.go` (runRetentionLoop)
- **Decision:** D-041
- **Verified by:** `go/internal/server/retention_test.go` TestRetentionLoopSweepsImmediatelyAndOnTick, TestRetentionLoopContinuesAfterError. Default values UNTESTED. See [04-server.md](04-server.md).

#### REG-017 ListEvents window and order
- **Behavior:** SQLite `ListEvents` MUST clamp `Limit` (≤ 0 → 50; > 200 →
  200), select the newest `Limit` rows by `ORDER BY received_at DESC, id DESC`,
  then reverse them, so the result is chronological (oldest first; ties broken
  by `id` ascending). Timestamps are stored in the fixed-width layout
  `2006-01-02T15:04:05.000000000Z07:00` (UTC), so string order equals time
  order.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads (order, limit 1 → newest), TestListEventsClampsLimit (0 and negative → 50, 1000 → 200)

#### REG-018 ListEvents filters and agent resolution
- **Behavior:** Observations MUST be LEFT JOINed to `agents` on
  `(host_id, provider, native_session_id)`; `agent_id` is the current row's
  id or `""`. `AgentID` filter → `a.id = ?`; `Provider` filter →
  `o.provider = ?`; both combine with AND. Consequences: observations without
  a native session (e.g. `hook.rejected` for a payload with no session) or for
  forgotten/never-created rows appear only in the global list with no
  `agent_id`; after a forget-and-rediscover, older observations attach to the
  new agent id.
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads (agent, provider, and combined filters; sessionless and unmatched observations carry no `agent_id`)

#### REG-019 Event payload projection
- **Behavior:** From the stored `payload_json` (an `ObservationData`) only
  `native_event`, `previous_live_state`, `live_state`, `reason` MUST be copied
  to the `AgentEvent`; `error` comes from the row's `error` column. A payload
  that fails to decode fails the whole query (`decode agent event data: <err>`).
- **Source:** `go/internal/storage/sqlite/hooks.go`
- **Decision:** D-033
- **Verified by:** `go/internal/storage/sqlite/hooks_test.go` TestListEventsFiltersOrdersAndRedactsPayloads (projected fields present; prompt text absent from encoded events)

#### REG-020 What gets journaled (event vocabulary)
- **Behavior:** (Writers are specified elsewhere; listed so event consumers
  know the vocabulary.) Every processed hook observation is recorded, with
  `id = "hook-" + 32 hex chars` and `event_type` one of `session.started`,
  `session.ended`, `turn.prompted`, `turn.stopped`, `turn.failed`,
  `workspace.changed`, `model.changed`, `attention.approval_required`,
  `notification.received`, `hook.observed`, `hook.rejected`. Snapshot
  reconciliation records only transitions, with
  `id = "snapshot-" + 64 hex chars` (SHA-256 of host, provider, native id,
  event type, previous state, time), `source = snapshot`, and `event_type`
  one of `agent.discovered`, `agent.snapshot_confirmed`, `agent.revived`,
  `agent.candidate_stale`, `agent.not_live`. Routine successful polls record
  nothing, and a row that is already `not_live` is never re-marked, so each
  retirement is journaled once.
- **Source:** `go/internal/hooks/normalize.go`; `go/internal/reconcile/reconciler.go`
  (RunOnce, recordTransition); `go/internal/hooks/processor.go`
- **Decision:** D-033
- **Verified by:** `go/internal/hooks/normalize_test.go` TestNormalizeEventMappings; `go/internal/reconcile/reconciler_test.go` TestReconcilerMaintainsIdentityAndLiveness, TestReconcilerDoesNotRemarkEndedHookOnlyRows. See [06-hooks.md](06-hooks.md) and [04-server.md](04-server.md).

---

## 02 · Design choices (local)

Implementation choices not recorded in [`docs/DECISIONS.md`](../DECISIONS.md):

1. **Change detection by fingerprinting full snapshots, per connection.** The
   watch handler hashes a projection of the whole list rather than tracking
   row versions. Simple and stateless for the store, at the cost of one `List`
   + JSON marshal + SHA-256 per watcher per 500 ms. Fields judged
   "freshness-only" are exactly those reconcile refreshes on every poll
   (`last_seen_at`, `snapshot_confirmed_at`, `*_observed_at`,
   `summary_updated_at`). `last_activity_at` is deliberately *not* zeroed, so
   a `working` agent emits a frame on every discovery poll (reconcile sets it
   to "now" while working, `reconciler.go`).
2. **Errors before headers only.** The watch performs its first `List` before
   committing to a 200 so a broken registry is reported as JSON; later failures
   just end the stream, and clients treat EOF as a connection loss.
3. **Optional capabilities via interface assertion** (`server.go`)
   rather than a richer handler config, so tests can pass a bare
   `registry.Memory` and get 501 for events.
4. **Name is recorded before the provider push** (`server.go`,
   `handleAgentName`), so a slow provider only delays the reply; the 5 s
   provider bound sits inside the CLI's 10 s rename timeout.
5. **Header-based sync status** keeps the rename response body identical to
   `GET /v1/agents/{id}` (`types.go`, `ProviderNameSync`); the error header is whitespace-collapsed and capped at 300 runes
   so arbitrary provider error text cannot break HTTP framing.
6. **Socket single-instance probe with a 200 ms dial**; stale sockets are
   removed only after a failed dial, and non-socket files are never removed.
   Existing parent directories (e.g. `/tmp`) are never chmodded
   (`paths.go`, `EnsurePrivateDir`).
7. **User name in its own column** so snapshot replacement cannot erase it
   (`store.go`, `Upsert`); Memory mirrors the rule through the shared
   `registry.ProviderRenamed` predicate.
8. **Bucket recomputed on read** (`store.go`, `scanAgent`), so the served
   bucket always follows the current `DeriveBucket`; `display_state` is not,
   so a change to `DeriveDisplayState` only takes effect for rows on their
   next `Upsert`.
9. **Event `agent_id` resolved at read time** via join instead of storing it on
   the observation, so events recorded before a row existed still attach.
10. **Events list newest-window-then-reverse** (`hooks.go`, `ListEvents`), so
    a CLI or frontend can read the window as a timeline.
11. **Random v4 UUIDs** from `crypto/rand` with no external dependency. Ids
    are not time-sortable; nothing orders by id except as a tie-break.
12. **Client classifies every transport failure, including timeouts, as a
    connection error** so the CLI can print one "Is the server running?" hint.
13. **Team, lead and parent in their own columns**, for the reason the user
    name has one (item 7): snapshot replacement cannot erase them. Unlike the
    user name, a new row may take its team and parent from the agent passed
    to `Upsert`, so a spawn registration is one insert (`store.go`, `Upsert`).
14. **A spawn registration finds or makes the row** (`teams.go`,
    `registerSpawn`). The caller starts the provider first and registers
    second, so a hook or a snapshot may have created the row already; the
    registration then only adds the team and parent, and the agent keeps one
    `id` either way.
15. **Teams are summarised by the CLI, not the server.** A team is only a
    name on agents, so `noc teams` counts the rows of `GET /v1/agents?all=true`
    and there is no team route to keep in step.

## 02 · Verification procedures

### Automated

From the repository root:

```sh
cd go && go test -count=1 ./pkg/... ./internal/localapi/... ./internal/registry/...
# Related store/server behaviour cited in this section:
cd go && go test -count=1 ./internal/storage/sqlite/... ./internal/server/... ./internal/reconcile/...
# List the tests that verify this section:
cd go && go test -list . ./pkg/api/v1 ./internal/localapi ./internal/registry
```

Expected: all `ok`. Test names:

- `go/pkg/api/v1`: `TestDeriveDisplayState`,
  `TestDeriveBucketGroupsByWhatTheUserIsNeededFor`, `TestHasProcess`,
  `TestHooksSeeItsTerminalAndNeverEngaged`.
- `go/internal/localapi`: `TestClientAndServerOverUnixSocket`,
  `TestWatchAgentsStreamsSemanticSnapshots`,
  `TestWatchAgentsPreservesCanceledContext`, `TestWatchAgentsEOFAndHandlerError`,
  `TestWatchEndsOnListError`, `TestClientAndServerListAgentEvents`,
  `TestForgetAgentRemovesTheRowOverTheSocket`, `TestErrorCatalogOverRawHTTP`,
  `TestAllQueryIsExactlyTrue`, `TestUnavailableCapabilities`,
  `TestClientConnectionErrors`, `TestClientUnexpectedStatus`, `TestWatchHeaders`,
  `TestFingerprintIgnoresFreshnessOnlyFields`, `TestListenSocketMode`,
  `TestListenRefusesNonSocketPath`, `TestListenReplacesStaleSocket`,
  `TestListenDetectsLiveServer`, `TestListenCreatesPrivateParent`,
  `TestRenamePushesNameToProvidersThatSupportIt`,
  `TestRenameEmptyObjectBodyClears`, `TestTeamMembershipAndLead`,
  `TestTeamAndSpawnErrorCatalog`, `TestSpawnRegistrationRules`,
  `TestSpawnIsUnavailableWithoutAHost`.
- `go/internal/registry`: `TestPruneRetiredKeepsLiveAndNamedRows`,
  `TestMemoryProviderRenameReplacesUserName`.
- `go/internal/identity` (run with `go test ./internal/identity/...`):
  `TestNewUUIDIsVersion4`.
- `go/internal/storage/sqlite` (teams): `TestTeamLeadAndParentAreStoredBesideTheSnapshot`.
- `go/internal/cli` (run with `go test ./internal/cli/...`; team names and
  the `TeamList` document): `TestValidTeamNames`, `TestSummarizeAndRenderTeams`.

### Manual (isolated; never touches the real registry)

The automated tests above cover most of these steps; the manual pass
exercises the built binary end to end. The same ground is covered by
`docs/spec/verify/isolated-smoke.sh` (see
[09-verification.md](09-verification.md)). All steps use a throwaway
directory and the path overrides from `go/internal/config/paths.go` (`NOC_STATE_DIR`, `NOC_RUNTIME_DIR`; unset
`NOC_SOCKET`/`NOC_DATABASE` so they derive from those). The server runs
with `--discovery=false`, so no provider command is executed and the registry
starts empty. Run in **bash** (the variable-splitting below is not zsh-safe).
Unset Zellij/Claude environment so hook ingestion records no bindings.

1. Setup:
   ```bash
   T=$(mktemp -d /tmp/noc-spec-XXXX)
   (cd go && go build -o "$T/noc" ./cmd/noc)
   unset NOC_SOCKET NOC_DATABASE ZELLIJ_SESSION_NAME ZELLIJ_PANE_ID CLAUDE_CODE_BRIDGE_SESSION_ID
   export NOC_STATE_DIR="$T/state" NOC_RUNTIME_DIR="$T/run"
   "$T/noc" server --discovery=false >"$T/server.log" 2>&1 & echo $! >"$T/pid"; sleep 1
   S="$T/run/noc.sock"
   ```
2. Permissions (HTTP-002, HTTP-004):
   `stat -f '%Sp %N' "$T/run" "$S"` (macOS; Linux: `stat -c '%A %n'`) →
   `drwx------ …/run` and `srw------- …/run/noc.sock`.
3. Health and empty list (API-001, API-020, API-022, HTTP-011, HTTP-012):
   `curl -s --unix-socket "$S" http://m/v1/health` → one line JSON with
   `"schema_version":1,"status":"ok","version":"dev","commit":"none"`, a UUID
   `host_id`, and two providers each `"status":"disabled"`,
   `"detail":"provider discovery disabled"`, `startup_status` `disabled`.
   `curl -si --unix-socket "$S" http://m/v1/agents` → `HTTP/1.1 200 OK`,
   `Content-Type: application/json`, body
   `{"schema_version":1,"generated_at":"…Z","agents":[]}`.
4. Seed one agent through the isolated hook path, then read it (API-017,
   API-032):
   ```bash
   echo '{"hook_event_name":"UserPromptSubmit","session_id":"11111111-2222-4333-8444-555555555555","cwd":"/tmp","prompt":"Example request"}' \
     | "$T/noc" hook ingest --provider claude
   sleep 3   # hook drain runs every 2 s; the wake usually makes it immediate
   curl -s --unix-socket "$S" http://m/v1/agents
   ```
   Expected one agent with `"provider":"claude"`, `"origin":"hook"`,
   `"live_state":"live"`, `"runtime_state":"running"`,
   `"activity_state":"working"`, `"attention":{"type":"none"}`,
   `"display_state":"working"`, `"bucket":"working"`,
   `initial_request.source` `hook_first_observed_user_prompt`,
   `workspace` `{"path":"/tmp","kind":"directory","owner":"unknown",…}`; `id`
   matching the v4 regex in API-032. (`process_id` appears if the shell has a
   `claude`/`codex` ancestor; that is expected.) Save
   `ID=$(curl -s --unix-socket "$S" http://m/v1/agents | python3 -c 'import json,sys;print(json.load(sys.stdin)["agents"][0]["id"])')`.
5. Error catalog (HTTP-007, HTTP-009, HTTP-010, HTTP-014). Each `curl -si`:
   | Request | Expected status, headers, body `code` |
   |---|---|
   | `-X DELETE http://m/v1/agents` | 405, `Allow: GET, POST`, `method_not_allowed` |
   | `-X POST http://m/v1/agents` (no body) | 400 `invalid_spawn` |
   | `-X POST http://m/v1/agents/$ID` | 405, `Allow: GET` |
   | `http://m/v1/agents/` | 400 `invalid_agent_id` |
   | `http://m/v1/agents/a/b/c` | 400 `invalid_agent_id` |
   | `http://m/v1/agents/a/bogus` | 404 `endpoint_not_found` |
   | `http://m/v1/agents/missing` | 404 `agent_not_found` |
   | `-X DELETE http://m/v1/agents/missing` | 404 `agent_not_found` |
   | `http://m/v1/nope` | 404 `text/plain`, body `404 page not found` |
   | `-X GET http://m/v1/agents/$ID/name` | 405 `Allow: PUT` |
   | `-X GET http://m/v1/hooks/wake` | 405 `Allow: POST` |
   | `-X POST http://m/v1/hooks/wake` | 202, empty body |
6. Rename (HTTP-017–HTTP-019):
   | `curl -si -X PUT -d …  http://m/v1/agents/$ID/name` | Expected |
   |---|---|
   | `'{"name":"x","extra":1}'` | 400 `request body must contain a name` |
   | `''` | 400 same |
   | a 121-character name | 400 `agent name must be at most 120 characters` |
   | `'{"name":"  Example label  "}'` | 200, `Noc-Provider-Name-Sync: unsupported`, body `"user_name":"Example label"` |
   | `'{}'` | 200, `Noc-Provider-Name-Sync: not_attempted`, `user_name` absent |
   | same body to `/v1/agents/missing/name` | 404 `agent_not_found` |
   Teams and spawn registration (HTTP-035–HTTP-037, REG-021, REG-022),
   run before step 9 forgets the agent:
   | Request | Expected |
   |---|---|
   | `-X PUT -d '{"team":"auth","lead":true}' http://m/v1/agents/$ID/team` | 409 `not_in_team`, message `<ID> is not in team auth` |
   | `-X PUT -d '{"team":"none"}' http://m/v1/agents/$ID/team` | 400 `invalid_team` |
   | `-X PUT -d '{"team":"auth"}' http://m/v1/agents/$ID/team` | 200, body `"team":"auth"`, no `lead` |
   | `-X PUT -d '{"team":"auth","lead":true}' http://m/v1/agents/$ID/team` | 200, body `"team":"auth","lead":true` |
   | `-X PUT -d '{"team":""}' http://m/v1/agents/$ID/team` | 200, neither `team` nor `lead` in the body |
   | `-X POST -d '{"provider":"claude","native_session_id":"s-1","team":"auth","parent_id":"'$ID'","prompt":"Example"}' http://m/v1/agents` | 201; body `"origin":"spawn"`, `"runtime_state":"starting"`, `"team":"auth"`, `"parent_id":"<ID>"`, `initial_request.source` `spawn`; then `GET /v1/agents/$ID` shows `"team":"auth","lead":true` |
   | `-X POST -d '{"provider":"claude","native_session_id":"s-2","parent_id":"gone"}' http://m/v1/agents` | 400 `invalid_parent` |
   Forget the spawned row afterwards (`-X DELETE http://m/v1/agents/<its id>`)
   so that step 9 leaves an empty registry.
7. Events (HTTP-021, API-021): `curl -s --unix-socket "$S" http://m/v1/events`
   → one event `{"id":"hook-<32 hex>","agent_id":"$ID","event_type":"turn.prompted","source":"hook","native_event":"UserPromptSubmit","live_state":"live",…}`
   and no prompt text anywhere in the body. `?limit=0`, `?limit=201`,
   `?limit=abc` → 400 `invalid_limit` (`limit must be between 1 and 200`);
   `?provider=other` → 400 `invalid_provider`;
   `/v1/agents/missing/events?provider=other` → 404 `agent_not_found` (existence
   checked first).
8. Watch (HTTP-024–HTTP-028):
   ```bash
   curl -s -N --max-time 2.5 -D - --unix-socket "$S" http://m/v1/agents/watch &
   sleep 1; curl -s -o /dev/null -X PUT -d '{"name":"Watch label"}' --unix-socket "$S" "http://m/v1/agents/$ID/name"; wait
   ```
   Expected headers `Content-Type: application/x-ndjson`,
   `Cache-Control: no-cache`, `Transfer-Encoding: chunked`; exactly two lines:
   the initial snapshot and, within ~0.5 s of the rename, a second snapshot
   with `"user_name":"Watch label"`. With no change, no further lines.
9. Forget and `all` (HTTP-016, REG-004):
   `curl -si -X DELETE --unix-socket "$S" "http://m/v1/agents/$ID"` → `204 No Content`;
   then `curl -s --unix-socket "$S" 'http://m/v1/agents?all=true'` → `"agents":[]`.
10. Shutdown with an open watcher (HTTP-005, HTTP-006):
    ```bash
    curl -s -N --unix-socket "$S" http://m/v1/agents/watch >/dev/null & C=$!; sleep 0.5
    time (kill -TERM "$(cat "$T/pid")"; while kill -0 "$(cat "$T/pid")" 2>/dev/null; do sleep 0.1; done)
    tail -1 "$T/server.log"; kill $C; ls "$S"
    ```
    Expected ≈5 s, last log line `noc: context deadline exceeded`, and
    `ls` reports the socket no longer exists. (Without the watcher the exit is
    immediate.)
11. Single instance and stale socket (HTTP-003): restart the server as in
    step 1; run `"$T/noc" server --discovery=false` again → stderr
    `noc: noc server is already running`. Stop it (`kill -TERM`), then
    create an orphan socket
    `python3 -c "import socket,sys;socket.socket(socket.AF_UNIX).bind(sys.argv[1])" "$S"`
    and start the server: it replaces the stale socket and `/v1/health`
    answers. Stop it, `touch "$S"` (regular file), start again → stderr
    `noc: refusing to replace non-socket path "<S>"`.
12. Cleanup: stop any server, then remove the directory `$T`.

## 02 · Coverage gaps

Behaviors with no automated test:

- Wire string values of most enums (API-004–API-008) and golden JSON for
  `Agent`, `Health`, and `AgentEvent`; the tests compare Go constants.
- `schema_version` on `Health` and on the `AgentEventList` envelope (API-001).
- The `crypto/rand` failure path of `identity.NewUUID` (API-032).
- Socket error texts other than the non-socket refusal and the live-server
  sentinel; the chmod-failure cleanup (HTTP-002–HTTP-004).
- Shutdown waiting 5 s on open watch streams (HTTP-006).
- Every 500 row of the error catalog: `registry_error` on list, read, forget,
  name, and events failures, and `streaming_unavailable` (HTTP-010, HTTP-023).
- Name body: trailing bytes after the first JSON value (HTTP-017); the 5 s
  provider rename timeout (HTTP-019).
- Watch: the default 500 ms interval (HTTP-026) and fingerprint advancement
  only after a successful write (HTTP-028).
- Client: a deadline expiry classified as a connection error, the
  `create request` and `encode agent name` errors (HTTP-032); a nil watch
  handler and a mid-stream decode error (HTTP-034); the caller timeout
  constants (HTTP-033).
- Teams and spawn registration: `agent_teams_unavailable` (HTTP-008); the
  500 answers of the team route and of `POST /v1/agents`, and the 201 status
  (the Go client accepts any 2xx) (HTTP-035–HTTP-037); the `encode agent
  team` and `encode spawned agent` client errors (HTTP-032); golden JSON for
  `AgentTeamUpdate`, `AgentSpawn`, `Team` and `TeamList`; a team change
  emitting a watch frame (HTTP-027); `registry.Memory`'s team rules other
  than through the API tests (REG-021, REG-022).
- Registry: `registry.Memory` under concurrent use (REG-003), a stored
  snapshot that fails to decode (REG-006), read-time bucket recomputation
  (API-031), a new id after forget and rediscovery (REG-014), and the
  end-to-end Codex rename followed by the provider-name snapshot (REG-013).

## Known limitations

- **405 on `/v1/agents/{id}` does not advertise DELETE.** The handler accepts
  GET and DELETE, but any other method is answered with `Allow: GET`.
  Code: `go/internal/localapi/server.go` (handleAgent, requireMethod).
- **An open watch stream delays shutdown by 5 s and makes it exit with an
  error.** `http.Server.Shutdown` does not cancel in-flight requests, so the
  server waits out `shutdownTimeout` and returns
  `context deadline exceeded`. Code: `go/internal/server/run.go` (Run).
- **A `working` agent produces a watch frame on every discovery poll.**
  `last_activity_at` is not a freshness-only field, and reconciliation sets
  it to the poll time while `activity_state` is `working`; byte changes in
  `native_state` emit frames the same way. Code:
  `go/internal/localapi/server.go` (watchFingerprint);
  `go/internal/reconcile/reconciler.go` (materialize).
- **A watch stream that fails after its first frame ends without an error
  frame.** Headers are already sent, so a later `List` failure just closes
  the stream; the client reports it as a lost connection. Code:
  `go/internal/localapi/server.go` (handleAgentWatch).
- **A name body with no `name` key clears the label.** `{}` and `null`
  decode to an empty name and succeed, although the 400 message for an
  undecodable body reads `request body must contain a name`. Code:
  `go/internal/localapi/server.go` (handleAgentName).
- **Client timeouts are reported as connection errors.** A non-watch request
  that exceeds its deadline returns `connect to noc server: ...` and the CLI
  prints the "Is the server running?" hint even when the server is up but
  slow. Code: `go/internal/localapi/client.go` (doJSON).
- **`native_state` is not bounded by the API layer.** The field is a raw
  `json.RawMessage`; its size is whatever the adapter produced. Code:
  `go/pkg/api/v1/types.go` (Agent); bounds are adapter-side
  ([05-providers.md](05-providers.md)).
- **The registry list is ordered alphabetically by `display_state`, not by
  urgency.** Clients that want bucket order must re-sort. Code:
  `go/internal/storage/sqlite/store.go` (List).
- **`display_state` is not recomputed on read.** A stored row keeps the
  display state derived at its last `Upsert`. Code:
  `go/internal/storage/sqlite/store.go` (scanAgent).
- **A spawn registration is trusted.** `POST /v1/agents` records what the
  caller says: it does not ask the provider whether the session exists. A
  row made this way (`origin` `spawn`, `runtime_state` `starting`) that no
  hook or snapshot ever confirms is never aged out, because reconciliation
  leaves unconfirmed rows without a process id alone
  ([04-server.md](04-server.md)); it stays, shown as quiet, until
  `noc forget`. Code: `go/internal/localapi/teams.go` (registerSpawn).
- **Marking a lead and the spawn lead rule are not atomic.** The team route
  reads the agent, checks its team, and then calls `SetLead`; the spawn
  route lists agents to see whether the team has a lead and then writes
  twice. A concurrent team change between the steps can make an agent the
  lead of a team it has just been moved into, or give a team a lead that the
  check said it lacked. `SetLead` itself keeps one lead per team. Code:
  `go/internal/localapi/teams.go` (handleAgentTeam, handleSpawn).
- **A registration for a row that already exists replaces its team and
  parent and nothing else.** An empty resolved team takes the existing row
  out of its team, an empty `parent_id` clears its parent, and the request's
  `name`, `directory` and `prompt` are dropped. Code:
  `go/internal/localapi/teams.go` (registerSpawn).
- **A team body with no `team` key takes the agent out of its team.** `{}`
  and `null` decode to an empty team and succeed, as with the name route.
  Code: `go/internal/localapi/teams.go` (handleAgentTeam).
- **Events re-attach after forget.** `agent_id` on an event is resolved at
  query time by natural key, so after a forget and rediscovery the older
  observations appear under the new agent id. Code:
  `go/internal/storage/sqlite/hooks.go` (ListEvents).
