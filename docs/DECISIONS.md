# Design decisions

Each entry states a decision that is in force, why it was made, and what
follows from it. The identifiers are stable: the
[specification](spec/README.md) cites them, so numbers are never reused and
gaps are listed under [Retired identifiers](#retired-identifiers).

The design these decisions add up to is described in
[`ARCHITECTURE.md`](ARCHITECTURE.md) and [`PROVIDERS.md`](PROVIDERS.md).
Decisions marked *planned* constrain work that is not built yet; see
[`ROADMAP.md`](ROADMAP.md).

## Product boundary

### D-002: control plane, not runtime

`noc` uses the native Claude Code and Codex runtimes and supervisors. It is
not an agent executor, a PTY host, or a terminal multiplexer.

**Why:** the providers already run, supervise, and resume conversations, and
their terminal experience is the one users want. A wrapper runtime would have
to chase every provider change and would exclude sessions started without it.
**Consequence:** every capability is built on a supported provider interface,
and anything a provider does not expose is not offered.

### D-003: the registry unit follows human engagement

One row is one durable, top-level provider conversation a human may engage
independently. Provider-native subagents stay behind their parent.

**Why:** subagents are numerous and short-lived; listing them would bury the
conversations a person actually needs to find.
**Consequence:** a row is not a process, turn, issue, pull request, or
worktree, and keeps its identity as those change.

### D-004: the coding workflow stays agent-owned

Agents and providers own worktrees, branches, commits, rebases, pull
requests, and review. `noc` observes those artifacts read-only.

**Why:** the agent knows what is unpushed and what the work needs; a registry
making Git decisions would be both dangerous and wrong.
**Consequence:** `noc` runs only read-only Git queries, and no feature may
create, switch, or delete anything in a repository.

### D-005: sessions are dynamic, with no launch profiles

A session may move from exploration to implementation or become a
coordinator of others. `noc` tracks current state rather than assigning a
permanent role at creation.

**Why:** real conversations change purpose, and a profile chosen at launch
would be wrong as often as right.
**Consequence:** nothing in the model depends on how or why a session began.

### D-009: sessions started outside `noc` are discovered

A live top-level session is registered whether it was started with the
provider's CLI, an IDE, or anything else supported.

**Why:** a registry that only knows its own launches is incomplete on day
one and forces a wrapper on every workflow.
**Consequence:** discovery cannot rely on having been present at session
start; see D-007.

### D-010: live, local sessions first

The registry covers sessions that are live now on the local machine.

**Why:** that is the smallest scope that is useful every day, and it can be
made correct before history and remote hosts add their own problems.
**Consequence:** remote hosts are future work that extends the same node
model (D-012, D-016).

### D-011: cloud sessions are excluded

Claude Code on the web and Codex Cloud sessions are not registered. A
session executing locally stays local even when a provider's remote-control
frontend can reach it.

**Why:** a cloud session has no local runtime to observe, attach to, or stop
through the interfaces `noc` uses.
**Consequence:** hook ingestion ignores cloud-executed Claude sessions, and
the Codex adapter accepts only local top-level sources.

### D-017: no provider history import

Discovery reads only what providers report as live. Ended rows already in
the registry are retained under D-041; nothing is imported from provider
history.

**Why:** history is large, mostly irrelevant to "what is running", and its
retention and privacy rules deserve their own design.
**Consequence:** the Claude roster is read without its history flag, and
Codex threads are read only for IDs the daemon reports as loaded.

## Structure and packaging

### D-014: frontends are independent, and Zellij is the first

Every frontend consumes the same stable contract. The registry is not
designed around any one of them.

**Why:** terminal, editor, and web users should all be served by one
registry, and a frontend should be replaceable without touching it.
**Consequence:** see D-034 and D-036 for the rules this imposes.

### D-015: Go for the application, Rust for the Zellij plugin

The server, CLI, adapters, setup, and hook ingestion are Go. The Zellij
plugin is Rust compiled to WebAssembly.

**Why:** Go gives one static binary with fast startup, which matters for a
hook that runs on every lifecycle event; Zellij plugins are Rust/WASM.
**Consequence:** the two share no code, only the JSON contract.

### D-018: one repository, separate language roots

The Go module lives under `go/` and the plugin under `zellij/`, each with its
own manifest and Makefile. The Go module path is `noc.local/noc`; only
`pkg/api/v1` is intended for other Go programs, and everything else is under
`internal/`.

**Why:** one repository keeps the contract and its consumers in step, while
separate roots keep either side buildable alone.
**Consequence:** the plugin never links to or mirrors Go implementation
packages.

### D-019: HTTP/JSON over a user-only Unix socket

The local API is HTTP/JSON on a Unix-domain socket with mode `0600`. Every
top-level response carries a schema version.

**Why:** it is easy to inspect and to consume from any language, needs no
port, and file permissions provide the access control.
**Consequence:** there is no TCP listener, and clients that see an unknown
schema version must fail clearly rather than guess.

### D-020: the Zellij plugin API is pinned to the host version

The plugin pins `zellij-tile` to an exact version (`=0.46.0`), with the Rust
toolchain pinned beside it, and targets `wasm32-wasip1`.

**Why:** plugin and host speak a protocol whose compatibility matters more
than picking up semver-compatible updates.
**Consequence:** the Zellij host and the plugin API are upgraded together.

### D-021: one executable named `noc`

`noc` is the only distributed Go executable. `noc server` is the long-lived
mode; the client commands, setup, diagnostics, and hook ingestion are other
modes of the same binary. Server and clients remain separate processes
connected by the local API.

**Why:** hooks, client, and server on one artifact cannot drift apart in
version, and users, hooks, and service managers all invoke one path.
**Consequence:** new process roles are added as subcommands, not binaries.

### D-022: SQLite is the durable local store

SQLite holds the host identity, agent rows, the observation journal, and the
hook outbox, through `modernc.org/sqlite` and ordered in-process migrations.
The server opens the database before it serves clients.

**Why:** a pure-Go driver needs no C toolchain and keeps the single static
binary; SQLite gives durable, concurrent access from the server and from
short-lived hook processes.
**Consequence:** the database is private state. No client or frontend reads
it.

### D-025: provider executables are explicit configuration

Adapters resolve a provider executable from an explicit in-process option,
then `NOC_CLAUDE_BIN` / `NOC_CODEX_BIN`, then the command name on `PATH`.

**Why:** tests need injection, and a service manager's environment often
lacks the interactive `PATH`.
**Consequence:** no path is hard-coded.

### D-026: the server is foreground-first and single-instance

`noc server` runs in the foreground. `noc server --bg` starts the same binary
detached, waits for a readiness signal sent once the listener is active, and
reports the PID and log path. Both modes acquire the same user-only PID
record beside the socket; a live record or a reachable API rejects a second
instance. `server stop` checks that the record belongs to the requested
socket, sends `SIGTERM`, and waits for cleanup. `server restart` replaces a
background server, keeping the flags given to the restart command, and
refuses to detach a foreground one. A stale PID record is removed only once
the operating system reports the process gone.

**Why:** foreground mode is what terminals and service managers supervise
best; the background mode is a convenience over the same code.
**Consequence:** this is manual lifecycle, not service installation. A
service unit should run the foreground mode and must not become a second
server implementation.

## Identity and discovery

### D-007: hooks plus reconciliation

Hooks provide low-latency observations. Provider snapshots provide current
truth and recovery. Terminal and transcript parsing are not part of the
design.

**Why:** hooks alone can be missed, duplicated, or disabled; snapshots alone
are slow and blind to sessions a roster does not list.
**Consequence:** every state has two possible writers, so observations are
idempotent and ordering-aware.

### D-008: one hook implementation, separate hook configuration

Claude and Codex each get a thin configuration fragment in their own format.
Both call `noc hook ingest` and produce one normalized observation schema.

**Why:** the providers' hook formats differ, but the work to do is the same.
**Consequence:** provider differences are confined to configuration and to
the normalizer's event mapping.

### D-016: identity is host-aware from the start

Every agent key includes a generated, persisted `host_id`. Agent IDs are
random UUIDs allocated on first observation and resolved thereafter through
`(host_id, provider, native_session_id)`.

**Why:** adding the host to identity later would mean rewriting every key.
**Consequence:** identities stay valid when remote nodes are added, and
"local or remote" is derived by comparing host IDs, not stored.

### D-023: Claude discovery uses the supported roster and the full session ID

The Claude adapter runs `claude agents --json` without the history flag,
accepts only `interactive` and `background` entries, and keys on the full
`sessionId`. The short job ID is an attach alias. An entry without a session
ID waits for a later snapshot. The adapter probes the roster command's
existence rather than a version range. Only documented state fields are
decoded, and the persisted native state holds no prompt, transcript, or
process ID.

A session missing from a successful snapshot becomes `candidate_stale`; it
becomes `not_live` on a later successful snapshot at least the stale grace
period after it was last seen. The poll interval (five seconds) and grace
period (30 seconds) are server flags.

**Why:** the roster is the provider's own machine-readable state; a
process-derived identity would change when the supervisor restarts a worker.
**Consequence:** a single missed or failed poll never removes a session.

### D-024: Codex discovery uses the shared daemon and the loaded-thread boundary

Each snapshot connects to the shared app-server daemon's Unix control socket,
upgrades to WebSocket, completes the `initialize` / `initialized` handshake,
calls `thread/loaded/list`, and reads each loaded thread with
`thread/read` and `includeTurns=false`. Accepted sources are exactly `cli`,
`vscode`, and `appServer`; `notLoaded` results are dropped. Only allow-listed
thread metadata crosses the adapter boundary.

The socket defaults to a path under `${CODEX_HOME:-~/.codex}`. An explicit
socket (option or `NOC_CODEX_SOCKET`) disables fallback. Only when the
inferred default cannot be dialed does the adapter retry through
`codex app-server proxy`; a failure after a successful dial never falls
back. Reachability is measured by the snapshot itself, not by the CLI's
daemon-version command, which inspects the process table and can be denied
in a sandbox. `noc` never installs or starts the daemon; a failed connection
degrades Codex health and the worker keeps retrying.

**Why:** reading only loaded threads keeps cost proportional to the live
set, never enumerates history, and never creates, resumes, subscribes to, or
mutates a thread. The narrow fallback prevents a wrong setting from silently
selecting another daemon.
**Consequence:** Codex sessions outside the shared daemon are covered by
hooks (D-007, D-009), and a daemon started later is discovered without
restarting `noc`.

### D-028: hook ingestion writes a normalized outbox

`noc hook ingest --provider claude|codex` reads at most 256 KiB of JSON,
drops unknown fields and transcript paths, writes one normalized observation
to an outbox table, and makes a best-effort attempt to wake the server. It
is silent, bounded in time, and returns success when normalization, storage,
or notification fails.

The server drains the outbox on wake and every two seconds. Provider event
or turn IDs form deterministic event IDs; an event without one is
de-duplicated within a one-second reception bucket, which collapses
immediate duplicate delivery without mistaking a later resume for the
original start. An event that fails processing is retried up to five times
and keeps a short diagnostic.

An explicit `SessionEnd` removes a hook-only row from the live view; a later
start or snapshot revives it. A snapshot never ages a row it has not
confirmed.

**Why:** a hook runs inside the provider's lifecycle, so it must be fast,
must work with the server down, and must never fail the provider.
**Consequence:** the wake request carries no event data; the event is read
from the database, so nothing is trusted from the HTTP request.

### D-040: process liveness is a negative-only signal for hook-only rows

Hooks record the provider process they ran under, found by walking the
process tree from the hook to its nearest `claude` or `codex` ancestor.
Reconciliation checks it for rows no snapshot has confirmed: an exited
process ends the row; a process that still answers proves nothing, because
the ID may have been reused. Resolution fails open: where the process table
cannot be read, nothing is recorded and nothing changes.

`noc forget <agent-id>...` removes a registry row and nothing else. A
session that is still running reappears on its next observation.

**Why:** a hook-only row has no roster to be reconciled against, so without
this a session whose process died without a `SessionEnd` would look live
forever.
**Consequence:** the process table is never a discovery mechanism. Rosters
and hooks decide which agents exist; this only stops a row outliving its
process.

### D-041: ended sessions are retained for a bounded window, named ones indefinitely

Rows for ended sessions are hidden from the default view and deleted once
they were last seen more than 14 days ago; a sweep runs at server start and
hourly. A row the user named is kept regardless of age. Live rows are never
touched.

**Why:** resuming an ended conversation yields the same agent ID, so the row
stays useful for a while; a user-assigned name exists nowhere else, so
deleting it would discard the user's work. Deleting on exit was rejected for
the same reasons, and because a stopped background session is still
attachable.
**Consequence:** `noc forget` is the way to remove a row immediately,
including one retention would keep.

### D-047: a session nobody spoke to is not a row once it ends

An ended session with no observed prompt and no name, from the user or the
provider, is dropped: by the hook processor when its end arrives, and by the
retention sweep at any age.

**Why:** tools and reattachment start provider processes that end within
moments without a prompt. They are not human-engaged conversations (D-003)
and would otherwise make up much of the registry.
**Consequence:** a name keeps a row, and a live session that is merely quiet
so far is unaffected.

## Data and privacy

### D-027: previews are bounded, sourced observations

An agent carries the model and reasoning effort when a supported surface
exposes them, a bounded initial-request preview, and a bounded latest-update
excerpt. Each records its source and observation time, and previews record
truncation. Initial requests are capped at 2,048 characters and latest
updates at 1,200, after whitespace compaction.

Codex thread metadata is preferred for model, reasoning effort, and initial
request, since it needs no turns; its `preview` is labeled
`codex_app_server_preview` because Codex documents it as usually, not
always, the first user message. Otherwise the first prompt a hook observed is
kept and labeled as first-observed. The hook's last assistant message is
labeled `hook_last_assistant_message`: an excerpt, not a summary.

Workspace metadata comes from bounded, cached, read-only Git inspection:
reported directory, checkout root, main checkout versus linked worktree,
branch, `HEAD`, and common Git directory.

Hooks also keep allow-listed Zellij session and pane context and the
presence of Claude Remote Control as `observed` frontend bindings.

**Why:** a person choosing which agent to open needs a little context, and
honest labels stop a frontend presenting an excerpt as something it is not.
**Consequence:** transcripts, transcript paths, arbitrary hook fields, and
credentials are never stored.

### D-031: watch streams complete snapshots as NDJSON

The watch route emits a complete, versioned agent list immediately and
another whenever membership or semantic state changes; freshness-only
timestamps do not produce a frame. The server detects changes by comparing
the registry every 500 ms. The human `noc watch` derives added, updated, and
removed lines; `--json` passes the stream through; `--all` includes retained
non-live rows.

**Why:** complete snapshots make reconnection and view replacement trivial
and avoid committing the contract to a fine-grained event vocabulary.
**Consequence:** clients never need SQLite or a replay protocol.

### D-032: provider health keeps startup and latest discovery evidence

Health distinguishes server reachability from provider discovery. Each
provider reports its discovery source, version, the first discovery result
after server start (fixed for the life of the process), and the latest
result. Successful detail includes the number of sessions the snapshot
observed. `noc server status` also reports live registry rows by origin,
since a hook-created agent can be live while the snapshot returns none.

**Why:** "the Codex daemon was absent when the server started and appeared
later" should be visible, not mistaken for a server fault.
**Consequence:** `noc` reports and retries provider failures; it never
starts a provider runtime to fix them.

### D-033: event diagnostics expose a safe projection

The event routes and `noc events` read the observation journal through the
server. Queries default to the 50 most recent events, are capped at 200, and
return the window in chronological order. The projection includes identity,
provider, event type and source, timing, the live-state transition, and a
reason. It excludes previews, native session IDs, and provider payloads.
Reconciliation records only semantic transitions (discovered, confirmed,
revived, candidate-stale, not-live), not routine polls.

**Why:** the journal should explain why the registry did something without
becoming a transcript or telemetry browser.
**Consequence:** nothing sensitive is reachable through diagnostics.

## Setup and diagnostics

### D-029: diagnostics are read-only and capability-based

`noc doctor` reports resolved paths, server reachability, the server's view
of provider health, direct provider CLI and snapshot capability, Codex
daemon connectivity, and hook coverage. It may create and remove a private
temporary directory to test write access. It changes no configuration,
starts nothing, and repairs nothing. Core failures exit 1; incomplete
provider integration is a warning.

Hook coverage requires every selected lifecycle event to invoke the `noc`
executable that is running the check, which detects handlers left pointing
at another copy. For Codex, doctor verifies configuration only and says so:
Codex's trust decision for hooks is interactive and cannot be queried.

The hook event sets are asymmetric on purpose: each provider gets only the
low-volume lifecycle events it documents (listed in
[`PROVIDERS.md`](PROVIDERS.md)). Claude's `WorktreeCreate` is never
installed, because configuring it replaces Claude's own worktree creation
(D-004). Tool, compaction, and subagent hooks are left out because they add
volume without identifying another top-level conversation.

**Why:** a diagnostic that mutates state cannot be run safely when something
is already wrong.
**Consequence:** fixing a finding is always an explicit, separate command.

### D-030: setup owns exact hook handlers, not provider configuration files

`noc setup` makes structured JSON edits to Claude's `settings.json` and
Codex's `hooks.json`, binding each handler to the absolute path of the `noc`
executable running it. Unrelated settings, matchers, and handlers are
preserved. A handler is owned only when its command is a `noc` executable
invoking exactly `hook ingest --provider <provider>`; reinstalling replaces
those and uninstalling removes only those. Writes are atomic, and a
configuration file that is a symlink is updated at its target. A malformed
file is an error and is left untouched. An existing `noc` hook in Codex's
`config.toml` is a conflict: setup refuses to add a second representation.

**Why:** recognizing handlers by their exact shape makes install, upgrade,
and uninstall reversible without a custom marker format.
**Consequence:** setup never installs or starts Claude, Codex, or the `noc`
server, and cannot assert Codex's trust decision.

## Frontends

### D-034: Zellij is an independent downstream frontend

All Zellij rendering, layout, permissions, key handling, and pane navigation
live in the `zellij-noc` plugin. The Go application has no Zellij command,
does not launch Zellij, and does not depend on a frontend for its lifecycle.
The plugin reads the versioned CLI JSON (`noc ls --json --all`,
`noc show --json`) and acts through `noc attach`, `noc rename`,
`noc stop`, `noc team set` and `noc team clear`.

Frontend bindings are generic registry facts. The plugin interprets a
binding of kind `zellij` and validates it before use; a stale or absent
binding is a UI outcome, not a lifecycle transition, and the fallback is
`noc attach`. The Go command decides which provider command attaches; the
plugin decides where the terminal opens.

**Why:** a one-way dependency keeps the registry usable from any client and
lets the frontend change shape freely.
**Consequence:** a need discovered by one frontend is added to the backend
only when it is generic. The plugin's own design is in
[`ZELLIJ.md`](ZELLIJ.md).

### D-035: names are an overlay

The provider's `name` is observed metadata. A separate durable `user_name`
is written through the local API, stored apart from the provider snapshot,
and preferred by frontends.

**Why:** a provider title is not user-owned, and a provider refresh must not
erase a label the user chose.
**Consequence:** reconciliation cannot overwrite a user name. Which name
wins when both change is decided by D-042.

### D-036: frontend environment state stays in each frontend

`noc` owns agent, host, lifecycle, attention, capability, preview, and
workspace observations. It may carry generic, ephemeral frontend bindings.
It does not own any frontend's windows, panes, tabs, terminals, editors,
selection, visibility, or layout, and no frontend gets an endpoint or
command named after it.

**Why:** frontends differ in everything except what they need to know about
agents.
**Consequence:** closing or breaking a frontend changes no agent's
lifecycle; a stale binding is a navigation failure; and frontends evolve
independently with different interaction models.

### D-055: the Zellij frontend is a nested pair

The plugin runs as a sidebar in a host session (`noc-host`) beside one guest
pane that holds the user's own session (`noc`), attached nested. That is the
only deployment. The host session sees every key first, binds three
(`Alt m` keyboard to the sidebar and back, `Alt o` hide or show the sidebar,
`Alt d` detach) and passes everything else to the inner session unchanged.
The sidebar is selectable only while it has the keyboard, so the inner
session's pane navigation never lands on it. The plugin opens, focuses and
follows agents in the inner session over the Zellij CLI.

**Why:** a sidebar that belongs to the whole session, rather than to each
tab, needs no per-tab layout repair and leaves the user's session exactly as
they configured it. A plugin can act only on its own session, so the inner
session is reached with `zellij --session <name> action …`.
**Consequence:** there is one code path to keep working. The plugin types a
nested-session "focus gained" frame into the guest pane, which depends on
Zellij's nested-session protocol.

### D-056: Zellij configuration is static and lives with the user

Nothing is generated, and nothing writes to the user's Zellij
configuration unless they run the installer (D-062). Everything is in two
static layouts. `noc-host.kdl` loads the plugin by its release URL with
all of its settings, carries the host session's options and keys, and holds
the guest pane's attach loop as an inline shell command. `noc-inner.kdl`
carries what the inner session has only inside the pair, including the two
key bindings of the theme relay. The guest pane's own client creates the
inner session. Nothing of noc's is in the user's `config.kdl`, which both
sessions read as it is. The files users copy are in `zellij/config/`.

**Why:** configuration the user can read and keep in their dotfiles is
easier to trust and to change than files rewritten on every start. A layout
file can override options and keybindings for the session it starts, and
can pass settings to a plugin it loads, so neither session needs anything
in `config.kdl`: setup is copying files, with no merge into a file the user
already maintains. Creating the inner session from the
guest pane leaves it with exactly one client.
**Consequence:** setup is a few copy steps, described in
[`ZELLIJ.md`](ZELLIJ.md). Settings that cannot work are reported in the
sidebar instead of failing silently. Go stays free of Zellij commands.

### D-057: one permission set, and no direct writes to panes

Every role of the plugin (panel, details window, theme relay) asks for the
same six permissions. Bytes for the inner session (the theme keys and the
focus frame) are typed by running `zellij action write --pane-id`, not with
the plugin API's write-to-pane call.

**Why:** Zellij records one grant per plugin location, as the set the latest
instance asked for, so roles that ask for different sets overwrite each
other's grant and stall on a prompt nobody can see. Running commands is
already granted, so writing through the CLI needs no further permission.
**Consequence:** the user answers one prompt per plugin location. Nothing
in the repository writes to Zellij's permission cache.

### D-058: releases are the only way into daily use

A pushed `vX.Y.Z` tag publishes a GitHub release with static `noc` binaries
(macOS and Linux, arm64 and amd64), `zellij-noc.wasm`, and checksums.
The binary is installed with Homebrew, from a formula the release writes
into this repository (which is its own tap), or with `install.sh` where
there is no Homebrew. Zellij loads the plugin from the release
URL in the host layout, and that URL names the version.

**Why:** machines that use noc should not need a toolchain. Zellij downloads
a plugin URL once and then uses its cached copy, so a "latest" URL would
never upgrade; a versioned URL makes the upgrade an explicit one-line change.
**Consequence:** upgrading the plugin changes its location, so Zellij asks
for its permissions again. A fixed `file:` location avoids that at the cost
of a manual download.

### D-059: development never shares state with daily use

A checkout is tried through the dev pair (`make dev`): its own session names
(`noc-dev`, `noc-dev-host`), its own Zellij config directory rendered from
`zellij/config/`, the locally built plugin and binary, and its own noc state
and socket. There is no target that copies a local build over the installed
binary.

**Why:** the registry and the sidebar are in constant use; a half-finished
change must not be able to break them.
**Consequence:** a change reaches daily use only through a release. The dev
pair renders the shipped configuration files, so those are what gets tested.

### D-060: the sidebar says when a newer plugin release exists

A few times a day the panel asks GitHub where the repository's "latest
release" link redirects, and shows `↑` on its status line when that tag
is newer than its own version. It runs `curl` for this, compares only
`X.Y.Z` versions, never downloads or replaces anything, and stays silent
when the check fails. `update_check "false"` turns it off.

**Why:** Zellij neither updates nor reports on a plugin loaded by URL, so
without this nothing on a machine says its plugin is behind. `curl` runs
under the command permission the plugin already holds; Zellij's own web
request call would add a seventh permission and a new prompt for every
user. The redirect needs no API token and is not rate limited the way the
GitHub API is.
**Consequence:** the plugin contacts github.com (with the user's address and
nothing else) unless told not to. It compares against the plugin's own
version only, not the `noc` binary's. Upgrading stays the explicit URL
change of D-058.

### D-061: the inner session has its own layout file

The guest pane creates the inner session with the layout `noc-inner.kdl`.
That file holds the session's first tab and, beside it, any Zellij options
that should hold only while the session runs inside the pair. As shipped it
hides the session name and the tab bar's sessions box.

**Why:** the inner session otherwise reads only `config.kdl`, which every
other Zellij session reads too, so there was nowhere to say "only in the
pair". A layout file's options apply to the session it creates and to no
other, the same mechanism the host layout uses (D-056).
**Consequence:** one more file to copy. The options are read when the inner
session is created, so a change needs the pair restarted. The inner
session's first tab comes from this file, not from `default_layout`.

### D-062: an installer the user runs, and a dev mode on their own config

`zellij/install-config.sh`, run straight from GitHub or as
`make zellij-install-config`, installs the layouts and the theme into the
user's Zellij configuration directory, and is run again to upgrade: it
fetches the latest release's files, carries the user's plugin settings into
the new host layout, and leaves an inner layout the user has edited alone
apart from its plugin location. It does not touch `config.kdl`, and a file
it replaces is kept as `.bak`. The dev
pair can render from that installed configuration (`make dev-installed`)
instead of the checkout's files (`make dev`).

**Why:** copying files by hand on every machine is the step most likely to
be skipped or done wrong, and one command that needs no checkout makes a
new machine quick. A plugin change should be
tried against the configuration it will really run with, and a
configuration change against a clean one.
The files are not part of the Homebrew package: they are set up once and
then belong to the user's own configuration, which they keep under version
control themselves, while the binary is what gets upgraded.
Upgrading by hand means changing a version in three places, which is the
kind of step that gets done halfway; one command that is the same for
setup and upgrade does not.
**Consequence:** the installer is the one thing in the repository that
writes to the user's configuration, and only when they run it. It takes
its files from the latest release, not from `main`, so a change to the
shipped layouts reaches users with the next release. It compares the
user's settings with what their release shipped, so that a changed default
arrives and a choice of the user's, including a default turned off, stays.
Edits to the host layout other than the plugin's settings and the session
names are not carried over (they are in the `.bak`).

### D-063: the sidebar has its own colours

The sidebar and the details window use a fixed palette, the one herdr ships
as its default (Catppuccin Mocha with a blue accent): blue for the header
and the connected line, red for Blocked, yellow for Working, teal for
Review, grey for Quiet. `colors "zellij"` makes them follow the Zellij theme
instead, with the groups on the terminal's own palette.

**Why:** colours taken from the Zellij theme varied from theme to theme and
leaned green; the groups are easier to read at a glance in the same colours
everywhere.
**Consequence:** the built-in palette assumes a dark background. On a light
terminal, set `colors "zellij"`.

### D-064: an agent's tab is named after the agent, and stays so

The panel renames an inner tab to its agent's current name when the tab
holds exactly one agent and its name is either the one the panel last gave
or saw there, or one Zellij made up (`Tab #3`). A tab the user named is
never renamed.

**Why:** an agent's name changes (a rename in noc, or the provider's own
title for the conversation), and a tab bar that still shows the old name
points at the wrong thing. Limiting it to names nobody chose keeps the
user's own tab names safe.
**Consequence:** starting an agent by hand in an unnamed tab names that tab
after it. Renaming a tab by hand takes it out of the panel's hands until it
is given a default-style name again.

### D-065: search is a noc command, and the sidebar only asks it

`noc search` matches words against an agent's names, ids, workspace,
branch, request and summary, and bounds its age with `--older-than` and
`--newer-than` (or `older:14d` and `newer:3d` inside the text). Its JSON is
the document `noc ls --json` writes. The sidebar's `/` passes what is typed
to `noc search` and shows the result as a flat list that includes agents
too old for the normal view.

**Why:** finding conversations left idle for weeks is a question about the
registry, useful from a shell and from any frontend, so the matching lives
in one place. Bounds written inside the text let a single typed line carry
the whole query. A half-typed bound is treated as a word so that typing
never produces an error.
**Consequence:** the sidebar runs `noc search` on each change of the query,
and the frontend contract gains that command. Age means time since the
agent's last activity, never when noc last looked at it. The pipe transport
has no search.

### D-074: the sidebar names the panes of the inner session

With `pane_titles "name"`, the shipped setting, the panel renames every
terminal pane of the inner session to its machine and what runs in it
(`ohm-2 · claude`, `ohm-2 · zsh`). `hide` blanks the title instead, and
`show` leaves titles alone.

**Why:** a tab already carries its agent's name and the shell prompt the
directory, so the usual title (`user@host:dir`, or the agent's name again)
repeats what is on screen. Zellij cannot draw a frame without a title: a
blank one leaves a gap in the border, so the space may as well say
something short that the rest of the screen does not, and the machine will
matter once agents run on several.
**Consequence:** a title a program sets afterwards is not shown. The panel
keeps the title it saw first, for recognising agents by it. The command
shown is the one the pane was started with, not what is running in a
shell at the moment, which Zellij does not report.

### D-075: one status line, with the words behind a click

The sidebar's status is a single line under the list: the agent count on
the left, and at the right edge a dot (green, yellow or red) with an `↑`
when a newer release exists. A click on the line, or `i`, opens a small box
that says it in words: the connection, any problem, and the versions.

**Why:** the state of the connection is almost always "fine", and a line
of text saying so every day is space the list could use. A colour is read
at a glance; the words matter only when the colour is not green.
**Consequence:** a problem is no longer spelled out until the box is
opened. A prompt or a notice still takes the whole line while it lasts, so
the count is hidden while a search is being typed.

## Names, state, and grouping

### D-042: names are two-way and the most recent rename wins

`noc rename` stores `user_name` and pushes the name to the provider where a
supported interface exists. Codex is renamed through `thread/name/set`.
Claude cannot rename a running session from outside, so there the name stays
a `noc` overlay. The response reports the outcome in the
`Noc-Provider-Name-Sync` header (`synced`, `unsupported`, `failed`,
`not_attempted`); a provider failure never loses the `noc` name.

A provider-reported name that differs from the one previously reported
clears `user_name`, because a rename inside the session is more recent. An
observation carrying no name, or the same name, changes nothing.

Claude's roster reports a session's ID as its name until it is named. That
placeholder is treated as no name, and frontends show the short ID for an
unnamed agent rather than inventing a name from its first prompt.

**Why:** naming an agent in `noc` and naming it inside its session are the
same act, so neither side should permanently override the other.
**Consequence:** a Claude agent named in `noc` keeps that name in the
registry while the session itself does not see it. Clearing a name is local
only, since providers have no notion of "no name".

### D-044: only real questions need the user; finished turns are "ready"

Attention is reported only when the provider says something is being asked:
an approval, an input request, authentication, or an error. Claude's
`blocked` state without a waiting reason, and its idle-prompt notification,
carry no attention: both mean the session finished its turn.

The display state `ready` covers a live agent that finished its turn with
its process running (Claude) or its thread loaded (Codex). A finished turn
with no process, such as a background job that exited but can be resumed, is
`idle`.

**Why:** when every parked session claims to need input, the signal is
useless. A reasonless `blocked` is not a request in the provider's own
model, so the adapter should not report one.
**Consequence:** the rule is objective and time-independent; nothing ages
out of `ready` on a timer.

### D-046: buckets are registry state

Every agent carries a `bucket` (`blocked`, `working`, `review`, or `quiet`)
derived from its display state and written with it. `noc ls` orders and
filters by bucket, and frontends read it rather than deriving their own.
How long a quiet agent stays in view is a frontend choice.

**Why:** these four groups are how a person thinks about their agents, so
the CLI and every frontend should agree on them.
**Consequence:** grouping changes are made once, in the API package.

### D-048: only agents with a live process are blocked, working, or review

`blocked`, `working`, and `review` require a live process: the row is live
and its runtime state is running, loaded, or starting. Without one an agent
is `quiet` whatever state it stopped in. Its display state still records a
question left pending.

**Why:** an ended row keeps the state it last had, so deriving the bucket
from display state alone showed dead sessions as needing attention, and
opening one failed.
**Consequence:** a quiet agent is reached by resuming it (D-051, D-054), and
a question pending in a dead process is not restored by the provider.

### D-049: the panel folds Quiet into time sections

The Zellij panel shows Quiet as collapsed, non-overlapping sections by age
(under 24 hours, under 48 hours, under 7 days), each with a count. Agents
quiet for longer are left out of the panel; `noc ls --all` lists them.
Opening a quiet agent resumes it through `noc attach`.

**Why:** a flat list of everything that ever went quiet only grows and hides
the conversations worth returning to.
**Consequence:** this is presentation in the plugin (D-046); the registry
keeps the rows under D-041.

### D-070: the sidebar groups by status or by team

`group_by` and the `v` key choose how the sidebar groups its rows: by
status (the default) or by team. Grouping by machine and by provider is
planned with remote nodes (spec/10-remote-nodes.md).

**Why:** the same list answers different questions: what needs me, and who
is working on what together.
**Consequence:** grouping is a view over the same rows; colours and
symbols keep meaning the agent's state in every grouping.

### D-071: a team is a name agents share, with a lead

Agents that work together carry the same team name, and one of them may be
marked its lead. A team has no record of its own: it exists while any agent
carries its name. The name is stored beside the agent like `user_name`, so
hooks and reconciliation cannot erase it. An agent spawned from inside a
team joins it, and an agent in no team that spawns the first member of a
team with no lead joins that team and leads it. How an agent was started
(its parent) is recorded separately and never changes.

**Why:** a shared name needs no coordination, so a team can later span
machines without a registry of teams. Inheritance means an orchestrator
gets a team by naming it once. "Workspace" already means an agent's
directory, so the set needed its own word.
**Consequence:** renaming a team means setting the new name on each agent.
A team disappears when its last agent is forgotten or pruned.

### D-076: `noc spawn` runs the provider from the CLI, and reads its session back

`noc spawn` starts a background Claude session with `claude --bg`, in the
CLI process and with the caller's directory. The server never starts a
provider: it only records the session's team, parent and name
(`POST /v1/agents`). Claude chooses the session ID, so `noc spawn` finds it
in Claude's roster by the job ID that `claude --bg` printed, and registers
the agent after the session exists. The session starts with
`--permission-mode auto`; everything after `--` is passed to Claude
unchanged and can override it. Codex is not supported yet.

The calling agent is the one whose Claude session ID is in
`CLAUDE_CODE_SESSION_ID`, then `NOC_AGENT_ID`, then the agent whose provider
process is an ancestor of the command.

**Why:** the control-plane rule (D-021) holds only if the server starts
nothing, and a spawn should run as the caller, where the caller is.
`claude --bg` ignores `--session-id`, so the ID cannot be chosen up front.
A background session does not inherit the caller's environment, so
`NOC_AGENT_ID` cannot identify a spawned agent; Claude's own session
variable can. Auto mode keeps a background agent from stopping at its first
permission prompt, and it was chosen for that.
**Consequence:** a spawn has no quota, idempotency key or worktree of its
own. If the roster does not list the session, `noc spawn` reports it and
the session runs without a team or parent.

### D-006: a shared skill teaches agents how to use `noc`

`noc setup skill` installs one `noc` skill, the same document for Claude
and Codex, into each provider's personal skills directory. It teaches an
agent how to learn which agent it is (`noc whoami`), which team it is on
and who else is in it, how to find another agent by what it is working on,
and how to start a worker. It says plainly that `noc` carries no messages:
an agent finds whom to address through `noc` and reaches them by its
provider's own means. The skill grows as messaging and other features
arrive.

The file carries a marker. `noc setup skill` replaces a file that has it
and never writes or removes one that does not.

**Why:** agent behavior is better shaped by shared guidance than by static
session profiles (D-005). Finding the right agent is what a registry can
offer before it can deliver anything, and delivery across machines needs
its own design. The skill is a separate step from `noc setup`, so that
adding hooks never changes what an agent is told.
**Consequence:** the skill is advisory. It never replaces reconciliation or
provider permission boundaries. An upgrade of `noc` needs
`noc setup skill` run again to refresh the text, and
`noc setup skill --check` says when it is out of date.

## Attach and stop

### D-038: attach refuses only what would fork a conversation

A second connection behaves differently per provider and path:

| Path | Second connection |
|---|---|
| `claude attach <short-id>` on a background session | Both terminals are live views of one process |
| `claude --resume <id>` on a live foreground session | A second independent process starts without warning, and the two diverge |
| `codex resume <id>` on a live thread | Both terminals are views of the same thread in the shared daemon |

Only the second is unsafe, and the provider does not guard it. `noc attach`
therefore refuses to open a Claude conversation that is running
interactively in another terminal, and names the process or the Zellij
session that holds it. `--force` overrides. Nothing else is blocked: attaching twice to a
background Claude session or a Codex thread is a supported way to watch an
agent from two places.

**Why:** two processes silently diverging on one conversation loses work.
**Consequence:** the guard is specific to one provider and mode, not a
general lock; `noc` does not own or reserve provider sessions.

### D-043: stopping an agent is a provider command

`noc stop <agent-id>` runs the provider's own stop command. A Claude
background session is stopped with `claude stop <short-id>`, which ends the
process and keeps the conversation. A Claude session running in a terminal
is refused with a pointer to `/exit`. Codex is refused: its nearest
supported operation archives a thread rather than stopping a running
session. Stop runs in the CLI process, and the registry learns of it through
reconciliation.

**Why:** deleting a conversation cannot be undone, so the reversible
operation is the one exposed; and `noc` does not signal processes it did not
start.
**Consequence:** a stopped agent stays in the registry as a quiet,
resumable row.

### D-050: background Claude sessions carry no hook pane bindings

Hooks for a Claude background session record no frontend binding, and
reconciliation clears any the row already had.

**Why:** Claude runs background sessions inside its daemon, where hooks see
the daemon's environment. The pane they report is the one the daemon was
first started from, so every background session would point at the same,
wrong pane.
**Consequence:** a frontend must locate or open the terminal for a
background session by its own means, normally `noc attach`.

### D-051: attach asks Claude directly, so background sessions stay background

`noc attach` reads Claude's full job list (`claude agents --json --all`) at
attach time:

- a conversation already running interactively in a terminal is refused
  (D-038);
- a background job Claude still knows, running or stopped, is attached;
- a conversation Claude no longer lists is resumed under D-054.

If Claude cannot be asked, the plan falls back to what the registry knows:
attach when the row is live and has a job ID, otherwise resume, subject to
D-038.

**Why:** Claude's supervisor, not the terminal, owns background sessions,
and it stops an idle, unattached session's process while keeping the job.
The discovery roster lists running sessions only, so such a job looks ended
to the registry although `claude attach` restarts it in place.
**Consequence:** the registry's view is advisory for attach; the provider
is asked at the moment it matters.

### D-054: forgotten Claude conversations resume in the background

When Claude no longer lists a job for a conversation, `noc attach` runs
`claude --bg --resume <session-id>` in the agent's workspace, finds the
restored job in Claude's job list, and attaches to it.

**Why:** a foreground `--resume` creates a process owned by the terminal
that ran it, which dies with that terminal. Resuming in the background keeps
the same session ID under Claude's supervisor.
**Consequence:** an agent opened through `noc attach` survives its terminal
or multiplexer closing.

## Planned

These are settled constraints on work that is not built yet.

### D-012: the first hub is also a node *(planned)*

When federation is added, one machine runs `noc server` as both its own
local node and the consolidated hub; remote nodes are added to it
incrementally.

**Why:** a single-machine setup must keep working unchanged, and the hub
needs no deployment of its own.
**Consequence:** each node stays authoritative for the providers on its
host.

### D-066: the hub pulls from nodes over SSH; frontends see one registry *(planned)*

The local `noc server` reads each remote `noc server` through
`ssh <alias> noc api stdio` and serves their agents with its own. A remote
node runs nothing extra. There is still one host session and one inner
session in Zellij; a remote agent opens as a tab like a local one.

**Why:** merging, reachability and routing belong in `noc`, where the CLI,
search and every frontend get them once. A second inner session per
machine, or a sidebar that talks to several servers, would put federation
in the plugin and split the list the sidebar exists to unify. SSH brings
authentication and reachability with no listening port.
**Consequence:** nothing remote may slow or break the local node. Commands
on a remote agent are carried out by its owning node.

### D-067: configured hosts and session hosts *(planned)*

Hosts in `nodes.toml` are connected at every start. Hosts added with
`noc nodes add` last until the server stops, unless `--save` writes them to
the file.

**Why:** a machine used every day should not need adding each morning, and
a machine tried once should not linger. The file is TOML because it is
edited by hand, wants comments, and will gain per-host fields.
**Consequence:** after a restart only configured hosts are looked for. The
binary gains one pure-Go TOML module, its first dependency besides SQLite.

### D-068: `noc` reports link state; the frontend decides what to do with it *(planned)*

When a host is unreachable, `noc` keeps serving the agents it last read
from it, unchanged, and reports the host's state beside them. It never
hides or alters them. The sidebar draws such agents greyed out by default
and can hide them instead.

**Why:** "this machine is down" and "this agent is idle" are different
facts, and whether to show stale rows is a presentation choice.
**Consequence:** a frontend must read the `nodes` array to tell fresh
agents from stale ones.

### D-069: remote attach assumes background agents *(planned)*

Attaching to a remote agent is `ssh -t <alias> noc attach <id>`, and is
specified for agents the provider runs in the background, which survive
the connection that views them. `noc spawn` creates such agents (D-076).

**Why:** `noc` is not a runtime and does not keep terminals alive; a
background agent needs none.
**Consequence:** a remote agent that lives in someone's terminal is not
given a new home by attaching to it.

### D-072: one room per team, one room on show *(planned)*

In Zellij each team would have its own inner session, a room, and the host
session's guest pane would show one room at a time. Agents in no team would
use the default room, which is today's inner session. This is not designed
in detail yet; see spec/11-teams.md.

**Why:** each piece of work gets its own tabs and layout, and moving
between pieces of work is one step instead of a search through tabs. The
sidebar still lists every team, because what needs attention anywhere is
its purpose.
**Consequence:** this would replace "one inner session" (D-055) with "one
room on show".

### D-073: a tab is one agent, and its panes are ways into it *(planned)*

An agent has one tab. The other panes in that tab, such as a shell in its
worktree, belong to that agent. An agent's tab is found wherever it is
open, it is never moved between rooms, and it outlives the agent.

**Why:** a tab bar of agents is a readable picture of a team, and the shell
beside an agent is the one you want when you look at it. Zellij cannot move
a tab between sessions, and a tab may hold work worth keeping after the
agent is gone.
**Consequence:** changing an agent's team changes where it is listed, not
where it is open. Two agents side by side in one tab is still allowed, and
such a tab belongs to neither.

### D-013: completion is separate and reversible *(planned)*

An agent's objective may be marked complete through `noc` and later
reopened. A provider being idle, done, unloaded, or between turns never
implies completion.

**Why:** "the turn ended" and "the work is finished" are different facts,
and only the second belongs to the person or agent who owns the objective.
**Consequence:** no provider state is ever mapped to completion, and
objective state must outlive liveness.

### D-039: spawned agents default to background, worktree, and auto mode *(planned)*

When spawning is added: sessions run in the background where the provider
supports it; anything that edits gets an isolated worktree, created by the
provider or the agent (D-004); Claude sessions start in auto mode. A spawn
request may override any of these, and agents discovered from outside are
never rewritten to match.

**Why:** background sessions attach as mirrored views instead of forking
(D-038) and survive their terminal; worktrees stop concurrent agents
colliding on one index.
**Consequence:** the registry must keep distinguishing a main checkout from
a linked worktree so the policy is observable.

## Open questions

Not decided, and not blocking current work:

- User-facing history: import, search, and retention beyond D-041.
- Remote nodes: how long a host that never returns is kept, and clock
  differences between machines (see spec/10-remote-nodes.md).
- Whether the first hub stays the hub or an always-on host takes the role.
- Provider-neutral semantics for moving an existing session into a newly
  created worktree.
- Spawn quotas and authorization, and spawning Codex agents.
- How Zellij shows a team: a session per team or something else
  (spec/11-teams.md).
- Mailbox retention, acknowledgement, and offline delivery.
- A process protocol for third-party provider adapters.
- Multi-user access control.

## Retired identifiers

| Identifier | Status |
|---|---|
| D-001 | Folded into D-021 |
| D-045 | Folded into D-046 |
| D-037, D-052, D-053 | Folded into D-055 and D-056 |
