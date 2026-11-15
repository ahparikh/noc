# Architecture

`noc` is a provider-neutral registry and control plane for durable
coding-agent conversations. It discovers the Claude Code and Codex sessions
running on a machine, including ones never started through it, normalizes
them into one model, and serves that model over a versioned local API and a
CLI with a stable JSON contract.

It answers, for any frontend:

- Which top-level agents exist, and which need a human?
- What provider, host, model, directory, repository, worktree, and branch is
  each one using?
- What was it asked, and what did it last say?
- How do I get a terminal on it?

This document describes the design. Exact wire constants, flags, messages,
and timings are specified in [`spec/`](spec/README.md); the reasons behind
individual choices are in [`DECISIONS.md`](DECISIONS.md).

## The core boundary

`noc` is a control plane, not an agent runtime.

| `noc` owns | `noc` does not own |
|---|---|
| Stable agent and host identity | Running Claude Code, Codex, or any other provider |
| Discovery of sessions, however they were started | Terminal emulation, PTYs, multiplexing |
| Provider adapters and normalized lifecycle state | Provider-native subagents |
| The current registry projection and bounded metadata | Git branches, worktrees, commits, rebases, pushes, pull requests |
| Read-only workspace and Git observation | Planning or executing coding work |
| Routing attach, rename, and stop to the provider's own commands | Panes, tabs, editors, selection, or any frontend layout |
| The versioned API, event stream, and CLI | Human approval |

Six rules follow from this and are not negotiable:

1. **Control plane, not runtime.** The server never hosts a conversation,
   keeps a PTY, or spawns a provider session.
2. **Fail open toward providers.** No `noc` failure may block or alter an
   agent. `noc hook ingest` always exits 0 and prints nothing.
3. **Supported interfaces only.** State comes from provider commands,
   protocols, and hooks. Terminal output and transcripts are never parsed.
4. **Frontend independence.** The Go application contains no frontend logic;
   frontends consume the CLI JSON contract or the local API.
5. **Secure by default.** The socket, database, and every file `noc` creates
   are accessible only to the current user.
6. **Never start or install a provider runtime silently.**

## What an agent is

An **agent** is one durable, top-level provider conversation that a human may
want to inspect, attach to, resume, or stop.

It is not a process, a turn, a prompt, an issue, a pull request, a branch, or
a worktree. A conversation can move from exploration to implementation,
change directories, be backgrounded, or have its worker process restarted
without changing its `noc` identity.

Provider-native subagents and other internal workers stay hidden behind their
parent. Cloud-executed sessions (Claude Code on the web, Codex Cloud) and
one-shot jobs such as `codex exec` are not agents. A session that ends
without ever receiving a prompt or a name is not kept either: nobody engaged
it.

`noc` tracks sessions that are live now. It does not import provider history.
Rows for ended sessions are retained for a bounded window so that resuming a
conversation finds the same identity (see [Retention](#retention)).

## Architecture

```text
            Claude Code                        Codex
   `claude agents --json`, hooks     shared app-server socket, hooks
                 |                                |
           Claude adapter                    Codex adapter
                  \                              /
                   +------ reconciliation ------+       noc hook ingest
                                |                              |
                                +------- SQLite registry ------+
                                    (agents, journal, outbox)
                                              |
                               HTTP/JSON on a user-only Unix socket
                                    /                  \
                               noc CLI          other API clients
                                  |
                          frontends (zellij-noc)
```

### One executable

`noc` is a single Go binary with several process modes: the long-lived
`noc server`, the client commands (`ls`, `show`, `watch`, `events`, `rename`,
`attach`, `stop`, `forget`), the provider hook entry point
(`noc hook ingest`), and `setup` and `doctor`. Server and clients are
separate OS processes connected by the local API. One artifact means hooks,
client, and server cannot drift apart in version. The command reference is
[`spec/03-cli.md`](spec/03-cli.md).

### Server

`noc server` opens the database, establishes the host identity, probes each
provider, reconciles once, and then serves the API while three loops run:
provider discovery, hook-outbox draining, and retention.

It runs in the foreground by default so a terminal or service manager can
supervise it. `noc server --bg` starts the same binary detached, waits for a
readiness signal sent after the listener is up, and reports the PID and log
path. Both modes take the same user-only PID record (`noc.pid`, beside the
socket), so a second instance is rejected. `noc server stop` sends `SIGTERM`
and waits for the socket and PID record to disappear. `noc server restart`
replaces a background server and refuses to detach a foreground one. There
is no service-manager integration; a `launchd` or systemd user unit should
supervise the foreground mode. Details: [`spec/04-server.md`](spec/04-server.md).

Stopping or restarting the server never affects a provider session.

### Provider adapters

Each provider implements one small contract:

```go
type Adapter interface {
    Provider() apiv1.Provider
    Probe(ctx context.Context) ProbeResult
    DiscoverLive(ctx context.Context) ([]NativeAgent, error)
    Normalize(agent NativeAgent) (NormalizedAgent, error)
}
```

`Probe` checks the installed CLI's capabilities, `DiscoverLive` returns one
snapshot of live, local, top-level conversations, and `Normalize` maps a
native row to provider-neutral state. An optional `Renamer` capability pushes
a name to the provider. Adapters own command and protocol invocation,
source filtering, state mapping, and redaction: only an allow-list of fields
ever leaves an adapter. They do not own persistence, identity, or
presentation.

Discovery is snapshot-based. The Claude adapter runs `claude agents --json`.
The Codex adapter connects to the shared app-server daemon's Unix control
socket and reads metadata for loaded threads only. Both are described in
[`PROVIDERS.md`](PROVIDERS.md).

### Hook ingestion

Claude and Codex have different hook configuration formats, but both invoke
the same command:

```text
noc hook ingest --provider claude|codex
```

It reads a bounded JSON payload from stdin, keeps an allow-list of fields and
environment values, writes one normalized observation to an outbox table in
SQLite, and makes a best-effort attempt to wake the server. It works when the
server is down, produces no output, runs under a sub-second deadline, makes
no network request, and returns success whatever happens.

The server drains the outbox when woken and on a short fallback interval,
appends each observation to the journal, and updates the agent projection. An
event that keeps failing is abandoned after a fixed number of attempts.

`noc setup` installs the hook handlers with structured edits to the
providers' own configuration files, and removes exactly what it installed.
See [`PROVIDERS.md`](PROVIDERS.md) and [`spec/06-hooks.md`](spec/06-hooks.md).

### Storage

SQLite is the node-local store and private implementation state; no client
reads it. It holds:

- the persistent host identity;
- one row per agent, keyed by `noc` ID and unique on
  `(host_id, provider, native_session_id)`, with the user-assigned name kept
  apart from the provider snapshot;
- the observation journal behind `noc events`;
- the hook outbox.

Observations are small and bounded individually. The journal and processed
outbox rows are not pruned.

### Local API

The server exposes HTTP/JSON on a Unix-domain socket with mode `0600`; there
is no TCP listener. Every top-level response carries a schema version.

| Operation | Purpose |
|---|---|
| Health | Server identity plus per-provider discovery status |
| List agents, get one agent | The current projection; non-live rows on request |
| Watch | Newline-delimited JSON: one complete agent list immediately, then another whenever membership or semantic state changes |
| Events | A bounded, transcript-free window of the observation journal |
| Rename, forget | Set or clear the user name; delete one registry row |
| Hook wake | Internal signal that the outbox has new work; carries no event data |

Each watch frame is a complete snapshot, so a client rebuilds its view from
any single frame and reconnects without replay. Routes, schemas, limits, and
error codes are in [`spec/02-api.md`](spec/02-api.md).

Provider health keeps two results per provider: the first discovery outcome
after server start, which never changes, and the latest one. A client can
therefore tell "Codex's daemon was absent at startup" from "Codex recovered
on a later poll", and neither is a server failure.

## Identity

Every database has a generated, persisted `host_id`. Host names are labels,
not identity.

The natural key of a conversation is

```text
(host_id, provider, native_session_id)
```

The first observation of a key, from either a snapshot or a hook, allocates a
random UUID as the agent's `noc` ID. Every later observation, and every
server restart, resolves through the natural key to the same ID. Provider
short IDs and names are aliases: they may be absent, reused, or ambiguous and
never reconcile identity.

`host_id` is part of the key although only one host exists today, so that
identities stay valid when remote nodes are added.

## Discovery lifecycle

Correctness combines two inputs. **Hooks** give low-latency lifecycle events
and are the only source for sessions a provider's roster does not list.
**Snapshots** give current truth and repair missed, duplicated, or reordered
hooks.

### Server start

1. Open storage and establish `host_id`.
2. Probe each adapter. A provider whose CLI is missing or lacks the discovery
   command is reported unavailable and gets no discovery worker for the life
   of this server process; the other provider is unaffected.
3. Run one reconciliation pass per available provider.
4. Open the listener and signal readiness.
5. Drain hook events spooled while the server was down, then keep draining.
6. Reconcile each provider on a fixed interval (five seconds by default).

### A session appears

A `SessionStart` hook creates the agent at once. The next snapshot that
includes the session confirms it and fills in what the hook did not carry. A
session started before hooks were installed is found by the snapshot alone.

### State changes

Hooks update the row as turns start, stop, fail, or ask for permission.
Snapshots overwrite runtime, activity, and attention with the provider's
current view. A hook observation older than the row's latest evidence can add
missing metadata but cannot regress state.

### A session leaves the live view

The rule depends on how the row is known:

- **Snapshot-covered rows** (confirmed by a snapshot at least once). Missing
  from one successful snapshot moves the row from `live` to
  `candidate_stale`. A later successful snapshot that still omits it, at
  least the stale grace period (30 seconds by default) after it was last
  seen, moves it to `not_live`. A failed provider query never ages anything.
- **Hook-only rows** (never confirmed by a snapshot). Snapshots do not age
  them, because the snapshot source may simply not cover that session. They
  end on an explicit `SessionEnd` hook, or when the provider process the
  hooks ran under has exited. That process check is negative-only: a dead
  process ends the row, a live one proves nothing.

A `not_live` row is hidden from the default list and revived, with the same
ID, by a later hook or snapshot.

### Retention

A retention sweep runs at server start and hourly. It deletes `not_live` rows
that have no user-assigned name and either were last seen more than 14 days
ago or were never engaged (no prompt observed, no name). A user-named row is
kept indefinitely, because the name exists nowhere else. Live rows are never
touched. `noc forget` deletes a row immediately; if the session is still
running it reappears on its next observation.

## Observation authority

Evidence is trusted in this order:

1. A supported provider API or machine-readable current-state command.
2. A provider lifecycle hook.
3. Conservative inference from earlier observations.

The process table is consulted in exactly one place, the negative-only check
above, and never to discover or identify an agent. Fields whose value can
disagree between sources (model, reasoning effort, previews, workspace,
frontend bindings) record where and when they were observed.

## State

A single status cannot represent the useful combinations, so independent
facts stay independent.

| Dimension | Values | Meaning |
|---|---|---|
| Live state | `live`, `candidate_stale`, `not_live`, `unknown` | Whether current evidence includes the session |
| Runtime state | `starting`, `running`, `loaded`, `resumable`, `stopped`, `failed`, `unknown` | What the provider's runtime holds for it |
| Activity state | `working`, `waiting`, `idle`, `unknown` | What the conversation is doing |
| Attention | `none`, `input_required`, `approval_required`, `authentication_required`, `error`, `unknown` | Why a human is needed, with a short detail and a since-time |

Provider terms are never translated into completion: Claude's `done`, Codex's
`notLoaded`, an idle process, or a finished turn say nothing about whether
the work is finished. Objective state is a separate, future signal
([`ROADMAP.md`](ROADMAP.md)).

### Display state

The server derives one display state so that clients do not reimplement
precedence:

```text
attention approval_required        -> needs_approval
attention input_required           -> needs_input
attention authentication_required  -> needs_authentication
attention error                    -> error
activity working                   -> working
activity waiting                   -> waiting
activity idle, live, runtime running or loaded -> ready
live                               -> idle
candidate_stale                    -> stale
otherwise                          -> not_live
```

`ready` means the agent finished its turn and its process or loaded thread is
still there: "your move", not a question. `idle` is live with nothing
running, such as a background job that exited but can be resumed.

### Bucket

Every agent also carries a `bucket`, the coarse grouping shared by the CLI
and all frontends.

| Bucket | Display states | Meaning |
|---|---|---|
| `blocked` | `needs_approval`, `needs_input`, `needs_authentication`, `error` | Asked for something, or failed |
| `working` | `working`, `waiting` | Mid-turn |
| `review` | `ready` | Finished its turn; waits for the user's next move |
| `quiet` | everything else | No process: a resumable job or an ended session |

The first three require a live process: the row is `live` and its runtime
state is `running`, `loaded`, or `starting`. Without one an agent is `quiet`
whatever state it stopped in; its display state still records a question left
pending. `noc ls` orders by bucket, then by most recent activity, and
`--bucket` filters. How long a quiet agent stays visible is each frontend's
choice.

## Data model

The public types live in the Go package `pkg/api/v1`, the only package meant
for other Go programs. Field-level definitions are in
[`spec/02-api.md`](spec/02-api.md).

### Agent

| Group | Fields | Notes |
|---|---|---|
| Identity | `id`, `host_id`, `provider`, `native_session_id`, `native_short_id` | The short ID is a provider alias used by attach and stop |
| Names | `user_name`, `name` | User-assigned and provider-reported; see [Names](#names) |
| Team | `team`, `lead`, `parent_id` | The team the agent is in, whether it leads it, and the agent that spawned it; see [Teams](#teams) |
| Provenance | `source_kind`, `origin` | Provider's session kind (`interactive`, `background`, `cli`, `vscode`, `appServer`); whether a hook or a snapshot first created the row |
| State | `live_state`, `runtime_state`, `activity_state`, `attention`, `display_state`, `bucket` | As above |
| Model | `model`, `reasoning_effort`, each with source and observation time | Present when a supported surface exposes them |
| Previews | `initial_request`, `summary`, with source, time, and truncation flag | Bounded excerpts, never a transcript |
| Times | `created_at`, `first_seen_at`, `last_seen_at`, `last_activity_at`, `snapshot_confirmed_at`, `ended_at` | `snapshot_confirmed_at` is what separates snapshot-covered rows from hook-only ones |
| Capabilities | `attach`, `resume`, each `{supported, reason}` | Clients render an unsupported operation as unavailable instead of inferring from the provider name |
| Native state | `native_state` | A small allow-listed object for diagnostics, such as Claude's `state` / `status` / `waiting_for` or Codex's `status` and active flags |
| Workspace | `workspace` | One current workspace; see below |
| Frontend bindings | `frontend_bindings` | Navigation hints; see [Frontend separation](#frontend-separation) |
| Process | `process_id` | The provider process the hooks ran under, used only for the negative liveness check |

### Previews

`initial_request` is the provider's own thread preview when one is available
(Codex), and otherwise the first prompt `noc` happened to observe through a
hook. The two carry different source labels, because a hook installed after a
session began cannot know the true first prompt. `summary` is the latest
assistant message supplied by a stop hook: an excerpt, not an agent-authored
summary, and labeled as such so a frontend can present it honestly. Both are
whitespace-compacted and length-capped.

### Workspace

The workspace is the directory the provider or hook reports, enriched by a
bounded, cached, read-only Git query: checkout root, common Git directory,
branch, and `HEAD`. Its `kind` is `directory`, `main_checkout`, or
`worktree` (a linked worktree). `noc` never creates, switches, or removes
anything in Git. An agent has one current workspace today.

### Observations and events

An observation is one normalized fact: a hook event or a liveness transition
found by reconciliation (discovered, confirmed by snapshot, revived,
candidate-stale, not-live). Routine successful polls record nothing. The
public event projection carries identity, provider, event type, source,
timing, the live-state transition, and a reason. It deliberately omits
prompt and assistant previews, native session IDs, and provider payloads, so
`noc events` explains registry behavior without becoming a transcript
browser.

## Names

An agent has two names. `name` is what the provider reports; `user_name` is a
label set with `noc rename` and stored apart from the provider snapshot, so
reconciliation cannot erase it. Frontends prefer `user_name`, then `name`,
then the provider's short ID.

Naming is two-way and the most recent rename wins. `noc rename` stores the
label and pushes it to the provider where a supported interface exists
(Codex); the response reports whether that push happened. When the provider
later reports a name different from the one it reported before, the session
was renamed from inside and `user_name` is cleared. Claude reports a
session's ID as its name until the session is named; that placeholder is
treated as no name.

## Teams

Agents that work together carry the same team name, and one of them may be
its lead. A team has no record of its own: it exists while an agent carries
its name. `noc team set`, `noc team clear` and `noc team lead` change it,
`noc teams` summarises it, and `--team` filters `noc ls`, `noc search` and
`noc watch`. Like `user_name`, the team, the lead mark and the parent are
stored beside the provider's snapshot, so hooks and reconciliation cannot
erase them.

`noc spawn` starts a background Claude agent and records its team and the
agent that spawned it. The CLI runs the provider; the server only records
the result. An agent spawned from inside a team joins that team, and an
agent in no team that names a team for its first worker becomes that team's
lead. Details are in [`spec/03-cli.md`](spec/03-cli.md).

## Attach and stop

`noc attach <agent>` hands the current terminal to the provider's own
command for that conversation, run in the agent's workspace. `noc` decides
which command; the caller decides where the terminal lives.

- **Codex:** `codex resume <thread>`. Several terminals on one thread are
  views onto the same thread in the shared daemon.
- **Claude:** `noc` asks Claude for its full job list at attach time. A
  background job Claude still knows is attached with `claude attach`. A
  conversation Claude no longer lists is first restored with
  `claude --bg --resume`, then attached, so the session belongs to Claude's
  supervisor and survives the terminal. A conversation running
  interactively in another terminal is refused, because a second process
  would silently diverge; `--force` overrides.

`noc stop <agent>` runs `claude stop` for a Claude background session, which
ends the process and keeps the conversation. Interactive Claude sessions and
Codex threads are refused. Both commands run in the CLI process, not the
server; the registry learns the outcome through reconciliation. The
reasoning is in [`DECISIONS.md`](DECISIONS.md) (D-038, D-043, D-051, D-054).

## Frontend separation

The dependency points one way: a frontend understands `noc`; `noc` knows no
frontend. The Go application has no frontend-specific command, layout,
renderer, or transport, and nothing a frontend needs requires reading SQLite,
provider files, or terminal output.

Frontend-owned state stays in the frontend: panes, tabs, terminals, editors,
selection, visibility, and layout. Closing or breaking a frontend changes no
agent's lifecycle.

The registry does carry **frontend bindings**: facts a hook inherited from
its environment, such as the Zellij session and pane it ran in, or the
presence of Claude Remote Control. A binding is a navigation hint in state
`observed` or `disconnected`, not proof that a client is connected. A
frontend validates a binding immediately before using it and falls back to
`noc attach` when it is stale; a stale binding is a navigation failure, never
an agent-state change. Hooks for Claude background sessions run inside
Claude's daemon and see the daemon's environment, so those sessions carry no
bindings at all.

Whether an agent is local or remote is relative to the viewer and is not
stored: a client compares the agent's `host_id` with the serving node's.

The first frontend is `zellij-noc`, a Zellij plugin that shows the registry
as a sidebar beside the user's own Zellij session. It reads only
`noc ls --json --all`, `noc search --json` and `noc show --json`, and acts
only through
`noc attach`, `noc rename`, `noc stop`, `noc team set` and `noc team clear`.
See [`ZELLIJ.md`](ZELLIJ.md).

## Security and privacy

- The server runs as the current user, never as a privileged daemon.
- The socket, database, PID record, and log are mode `0600`; directories
  `noc` creates are `0700`. A directory that already exists is used as is.
- The API is reachable only through the Unix socket.
- Hook payloads are bounded in size, decoded against a fixed allow-list, and
  every retained string is length-capped. Unknown fields and transcript paths
  are dropped before anything is written.
- No transcript, arbitrary turn, credential, or provider messaging token is
  stored. The only conversation text retained is the two bounded previews.
- Provider commands are run as literal argument vectors, never through a
  shell, with bounded output and a deadline.
- `noc setup` edits provider configuration structurally, preserves everything
  it does not own, and is reversible.

## Failure behavior

| Failure | Behavior |
|---|---|
| Server not running | Hooks spool to the outbox and exit 0; agents are unaffected. The server drains the backlog when it next starts. CLI commands report that the server is not running. |
| Provider CLI missing at server start | That provider is reported unavailable until the server is restarted; the other provider is discovered normally. |
| Provider query fails or times out | Provider health becomes degraded, nothing is aged, and the next poll retries. |
| Codex daemon not running | Codex discovery is degraded and retried every poll; `noc` does not start the daemon. Sessions with hooks are still tracked. |
| One incompatible row in a snapshot | The row is skipped and counted in provider health; other rows are processed. |
| Malformed hook payload | Recorded as a rejected observation with a bounded reason; no agent is created. |
| Hook event that cannot be applied | Retried on later drains up to the attempt limit, then left with its last error. |
| Duplicate hook delivery | Deterministic event IDs collapse it; no duplicate agent. |
| Out-of-order hook | An older observation cannot regress newer state. |
| Database cannot be opened | The server refuses to start; hook ingestion exits 0 without recording. Provider sessions are untouched. |
| Frontend closed or crashed | No effect on the registry or on any agent. |

## Repository layout

```text
go/                      Go module; builds the single `noc` executable
  cmd/noc                entry point
  pkg/api/v1             public JSON types and state derivation
  internal/cli           command dispatch, presentation, server lifecycle
  internal/server        server composition, discovery and retention loops
  internal/localapi      Unix-socket HTTP server and client
  internal/provider      adapter contract, command runner, claude/, codex/
  internal/reconcile     snapshot reconciliation and liveness transitions
  internal/hooks         hook normalization and outbox projection
  internal/storage       SQLite store and migrations
  internal/...           setup, hook configuration, doctor, attach planning,
                         workspace inspection, process lineage
zellij/                  Rust/WASM plugin `zellij-noc`
docs/                    these documents and the behavioral specification
```

The Rust plugin shares no code with the Go module; the JSON contract is the
only interface between them. Building, testing, and releasing are covered in
[`DEVELOPMENT.md`](DEVELOPMENT.md).
