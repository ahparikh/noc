# Provider integrations

`noc` observes Claude Code and Codex through their supported, machine-readable
interfaces. This document explains which interfaces are used, why, and how
native state maps onto the normalized model in
[`ARCHITECTURE.md`](ARCHITECTURE.md). Exact commands, field lists, limits,
and edge cases are specified in [`spec/05-providers.md`](spec/05-providers.md)
and [`spec/06-hooks.md`](spec/06-hooks.md).

## Adapter policy

Every provider integration follows the same rules.

- **Supported interfaces only.** A documented command, protocol, or hook.
  Never terminal rendering, transcripts, or private files.
- **Probe capabilities, not versions.** An adapter checks that the installed
  CLI offers the command it needs. There is no version allow-list.
- **Discover only live, local, top-level conversations.** No history import,
  no cloud sessions, no subagents.
- **Key on the native session ID.** Names and short IDs are aliases.
- **Allow-list at the boundary.** Only named fields are decoded; everything
  else is discarded before normalization. What is persisted as native state
  is a handful of state fields: no prompt, transcript, or arbitrary payload.
- **Prefer `unknown` to a wrong mapping.**
- **Fail independently.** One provider being absent or broken does not affect
  the other.
- **Never start or install a provider runtime.** A missing daemon is reported
  and retried, not launched.

Each provider is observed through two paths that feed the same agent rows:

| Path | Gives | Limits |
|---|---|---|
| Snapshot, every discovery interval | Current truth: runtime, activity, attention, names, and for Codex model and preview | Only sees what the provider's roster lists |
| Hooks, as events happen | Low latency; sessions the roster does not list; prompt and last-message previews; frontend bindings | Can be missed, duplicated, reordered, or disabled by the user or by policy |

The provider executables are resolved from `NOC_CLAUDE_BIN` and
`NOC_CODEX_BIN` when set, otherwise from `PATH`. A service manager whose
environment lacks the interactive `PATH` should set them.

## Claude Code

### Interfaces used

| Interface | Used for |
|---|---|
| `claude --version`, `claude agents --help` | Capability probe |
| `claude agents --json` | Discovery snapshot |
| `claude agents --json --all` | Attach planning only: which job, if any, Claude still holds for a conversation |
| Lifecycle hooks in `settings.json` | Low-latency events and interactive sessions |
| `claude attach <short-id>`, `claude --bg --resume <session-id>` | `noc attach` |
| `claude stop <short-id>` | `noc stop` |
| `claude --bg --permission-mode auto <prompt>`, then `claude agents --json --all` | `noc spawn`: start a background session, then find its session ID by the job ID Claude printed |
| `CLAUDE_CODE_SESSION_ID` in the environment of a session's commands | Which agent a `noc` command is run by |

References: [agent view and JSON roster](https://code.claude.com/docs/en/agent-view),
[hooks](https://code.claude.com/docs/en/hooks-guide),
[worktrees](https://code.claude.com/docs/en/worktrees).

### Snapshot

The roster is read without `--all`, so completed background history is not
imported. Each array entry is decoded on its own, so one incompatible entry
cannot hide the others. An entry is accepted when its `kind` is `interactive`
or `background` and it has a full `sessionId`; that ID is the durable
identity. The short job `id` is kept as the alias `claude attach` and
`claude stop` need. An entry without a session ID is skipped and gets another
chance on the next snapshot.

From each entry the adapter keeps the working directory, kind, start time,
name, `state`, `status`, `waitingFor`, and whether a process ID is present.
The process ID itself is not kept.

Claude reports a session's ID as its name until the session is named. That
placeholder is treated as no name.

### Snapshot mapping

Runtime:

| Evidence | Runtime |
|---|---|
| `state` is `failed` | `failed` |
| `state` is `stopped` | `stopped` |
| A process is present | `running` |
| No process; `state` is `working`, `blocked`, or `done` | `resumable` |
| Anything else | `unknown` |

Activity and attention:

| Evidence | Activity | Attention |
|---|---|---|
| `state` is `working` | working | none |
| `state` is `blocked`, `waitingFor` is `permission prompt` or `sandbox request` | waiting | approval required |
| `state` is `blocked`, `waitingFor` is `input needed`, `worker request`, `dialog open`, or any other non-empty value | waiting | input required |
| `state` is `blocked`, no `waitingFor` | idle | none |
| `state` is `done` or `stopped` | idle | none |
| `state` is `failed` | unknown | error |
| No recognized `state`: `status` `busy` / `waiting` / `idle` | working / waiting / idle | from `waitingFor` when waiting, else none |

Two points are easy to get wrong:

- **`blocked` without a `waitingFor` is not a question.** It is how Claude
  reports a session that finished its turn and waits for the next message.
  It maps to idle with no attention, which the registry shows as `ready`
  (bucket `review`) while the process is there and `idle` (bucket `quiet`)
  once it is not.
- **`done` is not completion.** It means a turn ended.

Capabilities: `resume` is always supported; `attach` is supported when the
entry has a short job ID, which interactive sessions do not.

### Hooks

`noc setup claude` installs handlers for these events in
`${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json`:

| Claude event | Effect on the agent |
|---|---|
| `SessionStart` | Create or revive the row; running, idle |
| `UserPromptSubmit` | Working; keep the first prompt observed as the initial-request preview |
| `Stop` | Idle; keep the last assistant message as the summary preview |
| `StopFailure` | Failed runtime; error attention with the reported detail |
| `PermissionRequest` | Waiting; approval-required attention |
| `Notification` | Attention from the notification type: permission or approval, authentication, input or elicitation. Other types, including the idle-prompt nudge, carry none |
| `CwdChanged` | Re-inspect the workspace at the new directory |
| `PostModelSwitch` | Record the active model |
| `SessionEnd` | Mark the row not live; drop it if it was never engaged |

Events not in this list are deliberately not installed:

- **`WorktreeCreate`** is never used for observation. Configuring that hook
  replaces Claude's own worktree creation, which would make `noc` a
  workspace manager. The directory from `SessionStart`, `CwdChanged`, and
  the roster, plus read-only Git inspection, gives the same view.
- **Tool, compaction, subagent, task, and teammate hooks** add volume
  without identifying another top-level conversation.

From the hook's environment `noc` keeps the Zellij session name and pane ID,
and the presence (not the value) of Claude's Remote Control session, as
frontend bindings. A hook running with `CLAUDE_CODE_REMOTE=true` is a
cloud-executed session and is ignored. The payload's transcript path is
never read or stored.

### Things to know

- **Interactive sessions depend on hooks.** The adapter accepts `interactive`
  roster entries, but the roster is centered on supervised background jobs
  and is not relied on to list every session running in a plain terminal.
  Such a session is tracked as a hook-only row: created by `SessionStart`,
  ended by `SessionEnd` or by its process exiting. One started before hooks
  were installed, and absent from the roster, is not seen until it is
  resumed.
- **Background sessions belong to Claude's supervisor, not a terminal.** The
  supervisor can stop an idle, unattached session's process and restart it on
  the next attach. Session identity is unaffected.
- **Hooks for background sessions run inside Claude's daemon** and inherit
  the daemon's environment, including whatever pane the daemon was first
  started from. Bindings from those hooks would point at the wrong pane, so
  background sessions carry none.
- **`claude --bg` chooses the session ID.** `--session-id` is ignored with
  `--bg`, so `noc spawn` learns the ID from the roster entry whose job ID
  Claude printed. A directory Claude does not trust makes `claude --bg` exit
  with a message, which `noc spawn` relays.
- **A background session does not inherit its starter's environment.**
  `NOC_AGENT_ID` therefore does not reach a spawned agent. Claude sets
  `CLAUDE_CODE_SESSION_ID` for the commands a session runs, and that is how
  `noc` tells which agent ran `noc spawn` or `noc team`.
- **A running session cannot be renamed from outside.** `noc rename` on a
  Claude agent stays a `noc`-side label, and the rename response says so.
- **Hooks can be disabled** by safe or bare modes or by organization policy.
  Sessions the roster lists are still discovered; the rest are not.

## Codex

### Interfaces used

| Interface | Used for |
|---|---|
| `codex --version`, `codex app-server --help` | Capability probe |
| Shared app-server daemon, Unix control socket | Discovery snapshot and rename |
| `codex app-server proxy` | Fallback transport when the default socket cannot be dialed |
| Lifecycle hooks in `hooks.json` | Low-latency events and sessions outside the shared daemon |
| `codex resume <thread-id>` | `noc attach` |

References: [app-server](https://learn.chatgpt.com/docs/app-server),
[app-server daemon](https://github.com/openai/codex/blob/main/codex-rs/app-server-daemon/README.md),
[hooks](https://learn.chatgpt.com/docs/hooks).

### Connection

Each snapshot opens a short-lived connection to the daemon's control socket,
performs a WebSocket upgrade, and exchanges JSON-RPC messages as text frames:
`initialize`, the `initialized` notification, then the discovery calls. Frame
and message sizes are bounded and one deadline covers the whole snapshot.

The socket path is `NOC_CODEX_SOCKET` when set, otherwise derived from
`${CODEX_HOME:-~/.codex}`. The fallback rule is narrow on purpose:

- If the **inferred** default socket cannot be dialed, the adapter retries
  through `codex app-server proxy`, which speaks the same protocol, in case
  the provider's layout has moved.
- An **explicit** socket never falls back, so a wrong setting fails visibly
  instead of silently selecting another daemon.
- A failure **after** a successful dial (handshake or protocol) never falls
  back.

Daemon reachability is measured by the snapshot itself, not by
`codex app-server daemon version`: that command inspects the process table,
which a sandbox may deny even when the socket is usable. When the daemon is
not running, Codex health is degraded and every later poll retries, so a
daemon started afterwards is picked up without restarting `noc`.

The user manages the daemon with Codex's own commands
(`codex app-server daemon start|stop|restart`). A Codex TUI joins the shared
loaded-thread set only if the daemon was running when the TUI started.

### Snapshot

```text
thread/loaded/list
thread/read { threadId, includeTurns: false }    for each loaded ID
```

`thread/loaded/list` is the live boundary. Reading only those IDs keeps the
cost proportional to the live set, never enumerates persisted history, and
never creates, resumes, subscribes to, or mutates a thread. Turns are not
loaded.

A thread is accepted when its source is exactly `cli`, `vscode`, or
`appServer` and its status is not `notLoaded`. That excludes `exec` jobs,
subagent and custom sources (which are structured objects, not strings),
cloud sessions, and a thread that unloaded between the two calls.

From each thread the adapter keeps the ID, name, source, working directory,
status type and active flags, creation and update times, current model,
reasoning effort, and the provider's `preview`.

Codex documents `preview` as usually the first user message. It is stored as
the initial request with the source label `codex_app_server_preview`,
distinct from a hook-observed first prompt, and is not treated as a summary.

### Snapshot mapping

| Evidence | Runtime | Activity | Attention |
|---|---|---|---|
| `active`, flag `waitingOnApproval` | running | waiting | approval required |
| `active`, flag `waitingOnUserInput` | running | waiting | input required |
| `active`, neither flag | running | working | none |
| `idle` | loaded | idle | none |
| `systemError` | failed | unknown | error |
| Any other status | unknown | unknown | unknown |

Approval wins when both flags are set. An idle loaded thread is `ready`
(bucket `review`): the thread is there and it is the user's move.

Capabilities: `attach` and `resume` are both supported.

### Hooks

`noc setup codex` installs handlers for these events in
`${CODEX_HOME:-~/.codex}/hooks.json`:

| Codex event | Effect on the agent |
|---|---|
| `SessionStart` | Create or revive the row; running, idle |
| `UserPromptSubmit` | Working; keep the first prompt observed |
| `Stop` | Idle; keep the last assistant message |
| `Interrupt` | Idle |
| `PermissionRequest` | Waiting; approval-required attention |
| `SessionEnd` | Mark the row not live; drop it if it was never engaged |

The event set is smaller than Claude's because Codex documents fewer
lifecycle events; `noc` does not invent parity. The app-server snapshot
supplies status, model, reasoning effort, and preview instead.

Two things are specific to Codex hooks:

- **Trust is separate from configuration.** Codex asks the user to review new
  or changed hook definitions interactively, and offers no supported way to
  query that decision. `noc setup` installs the definitions and tells the
  user to review them in Codex; `noc doctor` checks configuration coverage
  and says explicitly that trust is not verified.
- **`config.toml` is never edited.** If it already contains a `noc` hook,
  setup refuses to add a second representation to `hooks.json` and reports
  the conflict.

### Things to know

- **Not every Codex session is in the shared daemon.** A TUI started before
  the daemon, or a client with its own private app-server, is invisible to
  the snapshot. Hooks cover those sessions; they appear as hook-only rows.
- **Rename reaches the provider.** `noc rename` calls `thread/name/set`, so
  the name shows inside Codex too.
- **Stop is not offered.** The nearest supported operation archives a thread
  rather than stopping a running session, which is a different act.
- **Several terminals may resume one thread.** They are views onto the same
  thread in the daemon and stay in sync, so `noc attach` does not guard
  against it.
- **Source shapes should be rechecked on upgrade.** The accepted sources are
  strings today; a change in representation would silently exclude threads.

## Hook setup

`noc setup [claude|codex]` writes hook handlers, `--check` reports coverage
without changing anything, and `--uninstall` removes them. The rules:

- The handler command is the absolute, symlink-resolved path of the `noc`
  executable running setup (or `--executable`), followed by
  `hook ingest --provider <provider>`. Run setup again after moving the
  binary; `noc doctor` flags handlers that point at a different executable.
- Edits are structural JSON edits. Unrelated settings, matchers, and handlers
  are preserved.
- Only handlers whose command is a `noc` executable invoking exactly
  `hook ingest --provider <provider>` are treated as owned. Reinstalling
  replaces those; uninstalling removes those and nothing else.
- Writes go through a temporary file and an atomic rename. A configuration
  file that is a symlink is updated at its target, so dotfile managers keep
  working.
- A malformed or unexpectedly shaped file is an error and is left untouched.
- Setup never starts Claude, Codex, or the `noc` server.

## Cross-provider rules

- A snapshot replaces runtime, activity, and attention with the provider's
  current view. Attention is therefore cleared by newer provider evidence,
  never by a timer.
- A hook observation older than the row's latest evidence may fill in
  missing metadata but does not change state.
- A row no snapshot has ever confirmed is not aged by snapshots. It ends on
  `SessionEnd` or when its provider process has exited.
- A row a snapshot has confirmed leaves the live view in two steps, after
  the stale grace period, and only on successful snapshots.
- Model and reasoning effort from Codex's thread metadata and model from a
  hook each record their source; the most recent observation is shown.
- The initial-request preview from a provider's own metadata replaces a
  hook-observed first prompt. The hook value is only ever the first prompt
  `noc` saw.
- Names never reconcile identity; native session IDs do.

## Test strategy

- **Fixtures, not live providers.** Adapter and normalizer tests run from
  sanitized fixtures under each package's `testdata/` and from fake command
  runners. The default test suite never invokes an installed provider and
  never touches user-level provider configuration.
- **What fixtures cover.** Empty rosters; interactive and background
  sessions; working, idle, input-required, approval-required, failed, and
  stopped states; rows that must be excluded (subagents, exec jobs,
  unloaded threads, cloud sessions); unknown fields from a newer provider
  version; malformed rows and messages.
- **Transport tests.** The Codex WebSocket layer is tested separately:
  upgrade validation, client masking, control frames, fragmentation, and
  frame-length boundaries. The direct-socket client and the proxy fallback
  are tested against in-process fakes.
- **Hook tests.** Size limits, allow-listing, transcript-path exclusion,
  preview bounds and provenance, deterministic event IDs, duplicate
  delivery, out-of-order events, spooling while the server is down, and
  setup's preservation, idempotency, symlink handling, and uninstall.
- **Opt-in real-provider test.** `NOC_CODEX_INTEGRATION=1` runs a read-only
  snapshot against the local Codex daemon. It creates, resumes, and mutates
  nothing.
- **When a provider is upgraded.** Regenerate or re-capture the provider's
  schema or roster sample for the new version, update the fixture, and let
  the mapping tests show what changed.

Running the suites is covered in [`DEVELOPMENT.md`](DEVELOPMENT.md); the
end-to-end checks are in [`spec/09-verification.md`](spec/09-verification.md).
