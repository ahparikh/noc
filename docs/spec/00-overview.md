# 00 — Product overview, scope, and invariants

This section states what `noc` is, what it deliberately is not, the
system-wide invariants every component must uphold, and how the pieces fit
together. Later sections specify each component at the level of exact
constants, strings, and tests; this one is the frame they hang from.

Related documents: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) (product
definition, architecture, data model), [`docs/PROVIDERS.md`](../PROVIDERS.md),
[`docs/ZELLIJ.md`](../ZELLIJ.md) (Zellij frontend guide),
[`docs/DEVELOPMENT.md`](../DEVELOPMENT.md) (build, test, release),
[`docs/DECISIONS.md`](../DECISIONS.md) (decision log; the `D-0NN` identifiers
cited throughout), and [`docs/ROADMAP.md`](../ROADMAP.md). The code lives
under `go/` and `zellij/` and is cited in sections 01–08.

## 1. Product definition

`noc` is a **provider-neutral registry and control plane for durable
coding-agent conversations**. It discovers live, local Claude Code and Codex
conversations — including ones never launched through `noc` — normalizes
their identity, lifecycle, attention, model, preview, and workspace into one
model, and exposes that model through a versioned local API and a stable CLI
JSON contract. Independent frontends (today: the Rust `zellij-noc` plugin;
planned: VS Code) consume that contract and own all of their own UI.

It answers, for a human or a frontend:

- Which top-level agents exist on this machine right now?
- Which need me (blocked), which are working, which finished a turn and are
  waiting for review, and which are quiet?
- What provider, model, reasoning effort, directory, checkout/worktree,
  branch, and HEAD is each using?
- What was it asked first, and what did it say last (bounded, with
  provenance)?
- How do I get a terminal on it (`noc attach`), rename it, or stop it?

### 1.1 The registry unit: an *agent*

An **agent** is one durable, top-level provider conversation that a human may
independently inspect, attach to, resume, rename, or stop (D-003).

| An agent IS | An agent IS NOT |
|---|---|
| A Claude Code session identified by its full `sessionId` | An OS process (processes come and go under one conversation) |
| A Codex thread identified by its thread ID | A single prompt, turn, or response |
| Interactive or background, from CLI, IDE, or app-server clients | A worktree, branch, PR, PR stack, Linear issue, or SDD feature |
| The same row across resume, backgrounding, worker restart, server restart | A provider-native subagent, agent-team teammate, or SDD worker |
| | A Codex `exec` one-shot job, or a Claude/Codex Cloud session |

Sessions are dynamic: one conversation may begin exploratory, move into a
worktree, become a controller, and later resume — its `noc` identity never
changes (D-005).

## 2. Scope

### 2.1 Implemented

| Area | Spec section |
|---|---|
| One Go executable `noc` with server, client, hook, setup, doctor modes | [03](03-cli.md) |
| Foreground/background server, PID ownership, stop/restart, status | [03](03-cli.md), [04](04-server.md) |
| Claude discovery via `claude agents --json` (5 s poll) | [05](05-providers.md) |
| Codex discovery via direct WebSocket to the shared app-server socket (5 s poll), proxy fallback | [05](05-providers.md) |
| Hook ingestion → SQLite outbox → projection (Claude + Codex) | [06](06-hooks.md) |
| Reconciliation: stable identity, `live` → `candidate_stale` → `not_live` | [04](04-server.md) |
| Retention: 14-day prune, named-row protection, unprompted-row drop | [04](04-server.md) |
| Read-only Git workspace classification | [04](04-server.md) |
| Versioned HTTP/JSON API over a user-only Unix socket, including the NDJSON watch stream | [02](02-api.md) |
| `ls`, `search`, `show`, `watch`, `events`, `rename`, `forget`, `attach`, `stop` | [03](03-cli.md) |
| Teams: a name agents share, with an optional lead; `noc team set\|clear\|lead`, `noc teams`, `--team` filters | [02](02-api.md), [03](03-cli.md) |
| `noc spawn`: starts a background Claude session from the CLI process and registers its team and parent | [03](03-cli.md), [02](02-api.md) |
| Buckets `blocked`/`working`/`review`/`quiet` as registry state | [02](02-api.md) |
| Two-way names (Codex `thread/name/set`; Claude overlay only) | [02](02-api.md), [05](05-providers.md) |
| `noc setup [--check\|--uninstall]`, `noc doctor` | [06](06-hooks.md) |
| The `noc` skill, one document for both providers, installed by `noc setup skill`; `noc whoami` | [03](03-cli.md) |
| `zellij-noc` plugin: a 38-column agent sidebar with a details view, attach, rename, and stop | [07](07-zellij-panel.md) |
| Zellij host session: the sidebar in an outer session (`noc-host`) beside one pane holding the user's own session (`noc`), attached nested; started from the static layout `noc-host.kdl` | [08](08-zellij-host.md) |
| Releases on GitHub: the `noc` binary installs with `install.sh`; the plugin loads by release URL | [01](01-build.md) |

### 2.2 In scope but not yet implemented

Planned work is tracked in [`docs/ROADMAP.md`](../ROADMAP.md):

| Milestone | Content |
|---|---|
| M2 (rest) | Persistent subscribed Codex connection; Claude context headroom via `noc statusline`; headroom registry field (spike notes in D-054) |
| M3 | Remote nodes over SSH stdio, Mac as hub, `noc nodes`, `noc ls --host`, remote attach/stop routing, reachability |
| M4 | Cross-agent, cross-provider mailbox, `noc send`, inbox, optional MCP |

`noc setup` installs provider hook handlers only. The agent skill is
installed separately, by `noc setup skill` (D-006).

### 2.3 Explicitly out of scope

History import/browsing; Claude Cloud / Codex Cloud sessions; provider
subagents; spawning Codex agents, and spawning from the server; objectives
(`noc objective`), interrupt, archive; Git/worktree/branch/PR management of
any kind; launchd/systemd service installation; multi-user access control;
third-party provider plugins. History, cloud, objectives, and Git/PR
management require an explicit scope change recorded in
[`docs/DECISIONS.md`](../DECISIONS.md). Spawning a Claude agent from the CLI
is in scope (D-076).

## 3. System invariants

These are product-level MUSTs. Each is enforced by one or more component
requirements in later sections; the `Verified by` field says how to check the
invariant holistically. Where the code falls short of an invariant, the
requirement says so and the gap is repeated under
[Known limitations](#known-limitations).

#### INV-001 Control plane, not runtime
- **Behavior:** `noc` MUST NOT host conversations, own PTYs, or act as a
  terminal multiplexer. `noc attach` MUST run the provider's own command
  as a child process wired to the caller's terminal (`exec.CommandContext`,
  not a noc-owned PTY); the server MUST NOT spawn provider conversation
  processes. `noc spawn` starts a provider session only from the CLI process
  the user or an agent ran, by the provider's own background command, and
  the server only records it ([03](03-cli.md) CLI-046). Discovery may run
  only bounded, read-only provider query commands (`claude agents --json`,
  `codex app-server proxy` fallback).
- **Decision:** D-002, D-076
- **Verified by:** Code review of `go/internal/attach`, `go/internal/cli` and
  `go/internal/server` (no PTY code exists, and no provider command is run
  by the server other than discovery). See [03](03-cli.md) ATT-*, CLI-046,
  [04](04-server.md) SRV-*.

#### INV-002 Fail-open toward providers
- **Behavior:** A missing, broken, or stopped `noc server`, a corrupt
  database, or a failing hook MUST NOT prevent Claude Code or Codex from
  starting or continuing. `noc hook ingest` MUST exit 0 and print nothing in
  every failure mode.

  Exit status and silence hold in every failure mode. Two bounds are weaker
  than the invariant suggests:
  - The binary has no internal bound on how long it waits for stdin. It
    relies on the provider's own handler timeout (HK-005).
  - The 900 ms ingest deadline starts before stdin is read, so a payload
    that completes after it is dropped, not spooled (HK-042).

  Neither blocks the provider, but the second can lose an observation;
  reconciliation (INV-004) repairs the registry on the next poll.
- **Decision:** D-028
- **Verified by:** [06](06-hooks.md) HK-* (daemon-down spooling, silent exit
  tests) and the isolated smoke test in
  [09-verification.md](09-verification.md) (silent exit, daemon-down,
  malformed input).

#### INV-003 Supported interfaces only
- **Behavior:** Discovery MUST use provider-supported machine-readable
  interfaces (`claude agents --json`, Codex app-server JSON-RPC, documented
  hooks). It MUST NOT parse terminal output, screen contents, transcripts, or
  provider-private state files as a primary interface. Process-table reads are
  permitted only as a negative liveness signal for hook-only rows (D-040) and
  Zellij pane titles only as frontend pane identity (D-050).
- **Decision:** D-007, D-040, D-050
- **Verified by:** [05](05-providers.md) CLA-*/CDX-*, [06](06-hooks.md) PL-*;
  the grep-based architectural audits in
  [09-verification.md](09-verification.md).

#### INV-004 Hooks plus reconciliation
- **Behavior:** Hooks MUST be treated as low-latency observations only.
  Startup and periodic (default 5 s) provider reconciliation MUST run and
  MUST repair missed, duplicated, or out-of-order hooks.

  Reconciliation runs only for providers whose startup probe passed. A
  provider whose probe failed gets no discovery worker and the probe is not
  retried; its rows are then maintained by hooks alone until the server is
  restarted.
- **Decision:** D-007
- **Verified by:** [04](04-server.md) REC-*, [06](06-hooks.md) HK-*.

#### INV-005 Top-level, human-engaged rows only
- **Behavior:** Only durable top-level conversations become rows. Provider
  subagents, exec jobs, custom/hosted sources, and cloud sessions MUST be
  excluded. An ended session with no observed prompt and no name MUST be
  dropped (D-047).
- **Decision:** D-003, D-011, D-024, D-047
- **Verified by:** [05](05-providers.md) CDX source filter tests,
  [06](06-hooks.md) Claude-web exclusion, [04](04-server.md) retention tests.

#### INV-006 Stable identity
- **Behavior:** The natural key is `(host_id, provider, native_session_id)`.
  The first observation allocates a `noc` agent ID (UUID); all later
  observations, server restarts, provider worker restarts, resume, and
  backgrounding MUST resolve to the same ID. `host_id` is generated once and
  persisted; hostnames are labels only.

  The ID is stable for as long as the row exists. Once retention or
  `noc forget` deletes the row, a later resume of the same provider session
  is a first observation again and gets a new ID.
- **Decision:** D-016
- **Verified by:** [04](04-server.md) REC-*/DB-* identity and reopen tests.

#### INV-007 No deletion from one missed poll
- **Behavior:** A snapshot-covered session absent from one successful poll
  MUST become `candidate_stale`, and only become `not_live` after a later
  successful snapshot at least the stale grace (default 30 s) after its last
  sighting. A failed provider query MUST NOT age any record.
- **Decision:** D-023
- **Verified by:** [04](04-server.md) REC-* transition tests.

#### INV-008 Raw vs normalized state are separate
- **Behavior:** Provider-native state MUST be persisted separately
  (`native_state`, allow-listed) from normalized dimensions (`live_state`,
  `runtime_state`, `activity_state`, `attention`), so provider enums never
  leak into the public contract except as diagnostic payload.
- **Decision:** D-023, D-024
- **Verified by:** [02](02-api.md) API-*, [05](05-providers.md) normalization
  tables.

#### INV-009 Privacy bounds
- **Behavior:** `noc` MUST NOT persist transcripts, transcript paths,
  arbitrary turns, credentials, or provider messaging tokens. It MAY persist
  only a bounded initial-request preview (≤ 2,048 Unicode chars) and latest
  update (≤ 1,200 chars), each with source, observation time, and truncation
  flag. Hook stdin MUST be capped at 256 KiB. Event APIs MUST omit previews,
  native session IDs, and raw payloads.

  The agent row holds exactly those two previews. The hook outbox and the
  observation journal, however, store the bounded preview carried by every
  prompt and stop hook, and neither table is ever pruned, so bounded excerpts
  of later turns stay in the database file. They are never served by any API.
- **Decision:** D-027, D-028, D-033
- **Verified by:** [06](06-hooks.md) HK-* bounding tests, [02](02-api.md)
  events projection tests.

#### INV-010 Secure by default
- **Behavior:** The server runs as the current user. The Unix socket MUST be
  mode `0600`; state/runtime directories, database, PID file, and log MUST be
  user-only. No TCP listener exists.

  The database file is chmodded `0600` when opened. Its SQLite WAL and SHM
  sidecar files are not chmodded; they are protected by the `0700` mode of
  the state directory, which holds only when `noc` created that directory.
- **Decision:** D-019
- **Verified by:** [02](02-api.md) HTTP-* socket tests, [03](03-cli.md) CFG-*,
  and the smoke test's permission checks in
  [09-verification.md](09-verification.md).

#### INV-011 No Git or workflow ownership
- **Behavior:** `noc` MUST only read Git state (bounded, cached, read-only
  commands). It MUST NOT create/remove worktrees, switch/rename branches,
  commit, rebase, push, or manage PRs, and MUST NOT install Claude's
  `WorktreeCreate` hook.
- **Decision:** D-004, D-029
- **Verified by:** [04](04-server.md) WS-*, [06](06-hooks.md) HC-* event-list
  tests.

#### INV-012 Frontend independence (one-way dependency)
- **Behavior:** The Go application MUST NOT contain Zellij/VS Code/web
  commands, layout, or transport. Frontends MUST consume only the public API
  or CLI JSON and MUST NOT read SQLite or provider files. Frontend pane/tab/
  visibility state MUST NOT enter canonical agent fields; frontend bindings
  are advisory hints only.
- **Decision:** D-014, D-034, D-036
- **Verified by:** the architectural audits in
  [09-verification.md](09-verification.md); [07](07-zellij-panel.md),
  [08](08-zellij-host.md).

#### INV-013 Provider runtimes are never started or installed silently
- **Behavior:** Discovery, setup, and doctor MUST NOT install or start Claude,
  Codex, or the Codex daemon. Explicit user commands `noc attach` and
  `noc stop` are the only paths that run provider lifecycle commands.
- **Decision:** D-024, D-029, D-030, D-043
- **Verified by:** [05](05-providers.md) CDX health/degraded tests,
  [06](06-hooks.md) DOC-*.

#### INV-014 Completion is not idleness
- **Behavior:** Provider `done`, idle, `notLoaded`, or a finished turn MUST
  NOT be presented as objective completion. (Objective state does not exist
  yet; D-013 reserves it.)
- **Decision:** D-013, D-044
- **Verified by:** [02](02-api.md) display-state truth table (API-026).

## 4. System context

```text
                    Claude Code CLI                        Codex CLI
          (interactive + background supervisor)   (TUIs + shared app-server daemon)
             |  roster: claude agents --json         |  Unix socket: WebSocket + JSON-RPC
             |  hooks → noc hook ingest              |  hooks → noc hook ingest
             v                                       v
   +-------------------------------- noc (one Go binary) -----------------------------+
   |  hook ingest ──► SQLite hook_outbox ──wake──►  server: outbox drain (wake + 2 s)  |
   |                                                 │                                 |
   |  server: Claude worker (5 s) ─┐                 ▼                                 |
   |  server: Codex worker  (5 s) ─┴──► reconcile ──► agents / observations (SQLite)   |
   |                                                 │       retention sweep (1 h)     |
   |                                     HTTP/JSON on user-only Unix socket            |
   |  CLI clients: ls show watch events rename forget server-status ◄───────┘          |
   |  CLI local actions: attach stop setup doctor (run in the CLI process)             |
   +-----------------------------------------------------------------------------------+
                         │  reads:  noc ls --json --all, noc show --json
                         │  runs:   noc attach | rename | stop
                         ▼
                  zellij-noc (Rust/WASM plugin)
        38-column sidebar in the outer Zellij session `noc-host`,
        beside one pane holding the user's own session `noc`, attached nested
```

### 4.1 Core flows (each specified in detail in the cited section)

1. **Startup ([03](03-cli.md), [04](04-server.md)):** acquire the PID record
   (CLI) → open SQLite and migrate → establish `host_id` → probe providers
   sequentially → run the first reconciliation per available provider
   (recorded as the immutable startup health) → listen on the socket (`0600`)
   → signal readiness → start loops (provider discovery every 5 s, outbox
   drain on wake + 2 s, retention sweep immediately then hourly; watch
   polling is per client connection at 500 ms).
2. **New session ([06](06-hooks.md) → [04](04-server.md)):** provider fires
   `SessionStart` → `noc hook ingest` normalizes, bounds, writes
   `hook_outbox`, best-effort `POST /v1/hooks/wake` → server drains and
   projects into an agent row (origin `hook`) → next snapshot confirms it and
   fills richer fields.
3. **Poll ([05](05-providers.md) → [04](04-server.md)):** each provider worker
   snapshots live sessions → reconcile upserts by natural key, merges fields,
   ages absentees, emits only semantic transition events.
4. **Client read ([02](02-api.md), [03](03-cli.md)):** CLI and frontends read
   `GET /v1/agents[...]`; `watch` streams complete snapshots on semantic
   change.
5. **Act ([03](03-cli.md)):** `attach`/`stop` plan provider argv in the CLI
   process (no server round-trip for execution); `rename` goes through the
   server, which writes `user_name` and pushes to Codex; `team` writes an
   agent's team and lead through the server.
6. **Spawn ([03](03-cli.md) → [02](02-api.md)):** `noc spawn` runs
   `claude --bg` in the CLI process, reads the new session's ID from Claude's
   roster, and registers it with the server, which gives the agent its team
   and parent (origin `spawn` when no hook or snapshot saw it first).
7. **Zellij ([07](07-zellij-panel.md), [08](08-zellij-host.md)):** the static
   layout `noc-host.kdl` starts the outer session `noc-host` with the sidebar
   plugin beside a pane that attaches the user's session `noc`. The plugin is
   declared once, with its settings, in the host layout
   (`plugin location="..." { ... }`), reads the registry only through
   `noc ls --json --all`, `noc search --json` and `noc show --json`, and
   acts only by running
   `noc attach`, `noc rename`, `noc stop`, `noc team set` and `noc team clear`.

### 4.2 Repository layout

```text
go/        Go module noc.local/noc → one executable `noc`
           (server, client, hooks, setup, doctor); public package go/pkg/api/v1
zellij/    Rust/WASM plugin `zellij-noc` and the static host layout `noc-host.kdl`
docs/      architecture, provider, Zellij, development, decision, and roadmap
           documents; this specification under docs/spec/
```

The toolchains, build targets, and release artifacts are specified in
[01-build.md](01-build.md).

## 5. State ownership

| State | Canonical owner | `noc` role |
|---|---|---|
| Conversation, runtime process, turns | Provider on owning host | Observe via adapters |
| Stable agent ID, normalized state, bucket, retention | `noc` | Own |
| `user_name` overlay | `noc` (and Codex via push) | Own; most recent rename wins (D-042) |
| cwd, checkout root, worktree, branch, HEAD | Git / agent | Observe read-only |
| Pane, tab, stack, visibility, selection, focus | Frontend | Never stored; bindings are hints |
| Objective completion | Future agent/human-set `noc` state | Reserved (D-013) |

## 6. Terminology

| Term | Meaning |
|---|---|
| Agent | Durable top-level provider conversation a human may engage (§1.1). |
| Native session ID | Provider's stable conversation/thread ID (Claude `sessionId`, Codex thread `id`). |
| Native short ID | Provider alias for attach (Claude background job `id`). Never identity. |
| Live | Currently exposed by the provider as active/loaded/running, or hook-observed and not yet contradicted. |
| Observation | One fact from a hook, provider API, or reconciliation, journaled in `observations`. |
| Reconciliation | Comparing provider live state to the registry and repairing it. |
| Origin | How a row was first created: `hook` or `reconcile`. |
| Snapshot-confirmed | A provider snapshot has positively included the row at least once (`snapshot_confirmed_at`). |
| Hook-only | Never snapshot-confirmed; liveness driven by hooks and process-liveness signal. |
| Display state | Server-derived single status for sorting/labels ([02](02-api.md)). |
| Bucket | `blocked` / `working` / `review` / `quiet` — the shared grouping (D-046). |
| Has process | Live and its runtime state is `running`, `loaded`, or `starting` (D-048; API-027). |
| Frontend binding | Advisory hint (e.g. Zellij session/pane) captured by hooks. |
| Sidebar | The `zellij-noc` plugin pane: a 38-column agent list with a details view ([07](07-zellij-panel.md)). |
| Host session | The outer Zellij session `noc-host`, which holds the sidebar and one pane for the inner session ([08](08-zellij-host.md)). |
| Inner session | The user's own Zellij session `noc`, attached nested inside the host session's pane. |
| Workspace | Host-local directory/checkout/worktree associated with an agent. Its `kind` is `directory`, `main_checkout`, or `worktree` (a linked Git worktree). |
| Node / Hub | Future: a `noc server` authoritative for one host / one presenting a merged registry. |

## 7. How to read the rest of this spec

The index, conventions, and requirement counts are in
[README.md](README.md).

| Section | Covers | Requirement prefixes |
|---|---|---|
| [01-build.md](01-build.md) | Repository, toolchains, build, install | BLD |
| [02-api.md](02-api.md) | Public data model, HTTP API, registry | API, HTTP, REG |
| [03-cli.md](03-cli.md) | CLI, server process lifecycle, paths, attach/stop/rename | CLI, LIFE, CFG, ATT |
| [04-server.md](04-server.md) | Server composition, reconciliation, SQLite, workspace | SRV, REC, DB, WS |
| [05-providers.md](05-providers.md) | Provider contract, Claude and Codex adapters | PRV, CLA, CDX |
| [06-hooks.md](06-hooks.md) | Hook ingestion, outbox, process lineage, setup, doctor | HK, OBX, PL, HC, DOC |
| [07-zellij-panel.md](07-zellij-panel.md) | Zellij sidebar: model, grouping, rendering, keys | ZM, ZG, ZR, ZK |
| [08-zellij-host.md](08-zellij-host.md) | Zellij host session: layout, plugin configuration, lifecycle, actions | ZH, ZX, ZE |
| [09-verification.md](09-verification.md) | Gates, isolated smoke test, change protocol | — |

Decisions are recorded in [`docs/DECISIONS.md`](../DECISIONS.md); every
`Decision:` field in this spec cites an identifier from that log.

## Known limitations

- **Late hook payloads are dropped (INV-002).** `noc hook ingest` starts its
  900 ms deadline before reading stdin and has no separate stdin bound; a
  payload that completes after the deadline is discarded rather than spooled.
  Code: `go/internal/cli/run.go` (hook ingest, `hookIngestTimeout`).
- **A failed startup probe is never retried (INV-004).** Discovery workers
  are created only for providers that probed available with `discover_live`
  at server start; a provider that was unavailable then is not reconciled
  until the server restarts. Code: `go/internal/server/run.go` (`Run`).
- **Identity does not survive row deletion (INV-006).** After retention or
  `noc forget` removes a row, the same provider session reappears under a new
  agent ID and without its `user_name`. Code:
  `go/internal/reconcile/reconciler.go` (`materialize`),
  `go/internal/hooks/processor.go`, `go/internal/storage/sqlite/store.go`
  (`Forget`, `PruneRetired`).
- **Per-prompt previews accumulate in the journal (INV-009).** `hook_outbox`
  and `observations` keep the bounded prompt and last-message previews of
  every hook and are never pruned. Code:
  `go/internal/storage/sqlite/hooks.go` (`EnqueueObservation`,
  `RecordObservation`); the only `DELETE` statements in the store target
  `agents`.
- **SQLite sidecar files rely on the directory mode (INV-010).** Only the
  main database file is chmodded `0600`; the `-wal` and `-shm` files are not.
  Code: `go/internal/storage/sqlite/store.go` (`Open`).
