# 09 — Verification

How to establish that `noc` behaves as this specification says. The checks
are layered from cheapest to most realistic:

1. **Gates.** Compile, format, lint, and unit and integration tests (§1).
2. **Isolated smoke test.** The real binary against temporary state, with
   no provider involved (§2).
3. **Acceptance criteria and invariants.** Each mapped to its evidence
   (§3, §4).
4. **Zellij end-to-end test.** The plugin in a real, headless Zellij pair
   (§5).
5. **Real-provider checks.** Read-only, and run only when an operator
   decides to (§6).
6. **Architectural audits.** Grep-based checks of the boundaries (§7).

Every requirement in [00-overview.md](00-overview.md) through
[08-zellij-host.md](08-zellij-host.md) carries its own `Verified by` field.
This file is the umbrella: what to run, what each run proves, and how to
keep the specification and the code in step (§8).

Each section file ends with a "Known limitations" list: behavior that is
real, intended or not, and that a reader should know before relying on the
requirement it qualifies. There is no separate findings register. A newly
discovered gap goes into the list of the file that owns the requirement.

## 1. Gates

| ID | Command | What it proves | Run by CI |
|---|---|---|---|
| G-1 | `make check` | Go compiles; every Go test passes; `go vet` is clean; `gofmt -l` lists nothing | yes, Linux and macOS |
| G-2 | `make zellij-check` | Rust formatting; clippy with `-D warnings` on the WASM target and on host targets; every plugin unit test passes | yes, Linux |
| G-3 | `make build && docs/spec/verify/isolated-smoke.sh go/bin/noc` | Server lifecycle, hook ingestion and projection end to end (§2) | yes, Linux and macOS |
| G-4 | `make zellij` | The release plugin `zellij-noc.wasm` builds | yes, Linux |
| G-5 | `make zellij-e2e` | The host session, sidebar and inner session work together in a real Zellij (§5) | no (local) |
| G-6 | `cd go && go test -count=1 -race ./...` | No data race on tested paths | no (optional) |
| G-7 | `cd go && NOC_CODEX_INTEGRATION=1 go test ./internal/provider/codex -run TestInstalledSharedDaemonDiscovery -v` | Handshake with a real Codex daemon, read-only (§6) | no (operator) |

**A green build** is G-1 through G-4, which is what CI runs on every push
and pull request ([01-build.md](01-build.md) BLD-044). G-5 is also required
before merging any change under `zellij/`. G-7 and the procedures in §6 are
required before a change in provider behavior is called proven. Without
`NOC_CODEX_INTEGRATION=1`, `TestInstalledSharedDaemonDiscovery` skips, so
G-1 never touches a real provider.

Expected output: G-1 prints `ok` for every package except `cmd/noc`
(`[no test files]`); G-2 ends with `test result: ok`; G-3 ends with
`passed: 34  failed: 0`; G-5 ends with `nested: all checks passed`.

### 1.1 Test inventory

Counts are top-level test functions and are approximate: they move with
the code. Regenerate them with `cd go && go test ./... -list '^Test'` and
`cd zellij && cargo test -- --list`.

| Package or module | Tests | Covers |
|---|---|---|
| `go/pkg/api/v1` | 4 | display-state and bucket derivation tables |
| `go/internal/attach` | 7 | attach plans from the registry and the Claude roster, concurrent-use refusal, stop plan |
| `go/internal/buildinfo` | 1 | version string |
| `go/internal/cli` | 29 | single binary, `ls` ordering and filters, rendering, events, watch, status, background server lifecycle, restart, hook spooling while the server is down, attach and stop |
| `go/internal/config` | 4 | path resolution, overrides, private directory creation |
| `go/internal/doctor` | 12 | healthy and degraded reports, exit semantics, hook coverage |
| `go/internal/hookconfig` | 13 | idempotent install, exact-command ownership, symlinks, refusal of malformed files, Codex conflicts |
| `go/internal/hooks` | 29 | normalization bounds and allowlist, cloud exclusion, event IDs, projection into the registry |
| `go/internal/identity` | 1 | UUID format |
| `go/internal/localapi` | 21 | Unix-socket client and server, rename sync, events, forget, watch |
| `go/internal/proclineage` | 5 | ancestor walk, exited-process detection |
| `go/internal/provider` | 2 | bounded command runner |
| `go/internal/provider/claude` | 11 | probe, roster normalization, names, bindings |
| `go/internal/provider/codex` | 39 | probe, discovery filters, pagination, malformed protocol, socket and proxy rules, WebSocket framing, rename |
| `go/internal/reconcile` | 15 | identity and liveness transitions, failed discovery does not age rows, hook and snapshot merge, process-exit retirement, bindings |
| `go/internal/registry` | 2 | prune predicate, rename overlay |
| `go/internal/server` | 12 | initial discovery, health, retention loop |
| `go/internal/setup` | 7 | install, check, uninstall |
| `go/internal/storage/sqlite` | 13 | persistence, outbox durability and idempotency, prune, forget |
| `go/internal/workspace` | 3 | main checkout, linked worktree, directory fallback |
| `zellij/src/model.rs` | 14 | parsing, schema rejection, sorting, buckets, ages |
| `zellij/src/app.rs` | 20 | selection, hidden stopped agents, sections, navigation, configuration |
| `zellij/src/panel.rs` | 18 | key dispatch, modifiers, stop confirmation, rename mode, the keys of each section |
| `zellij/src/view.rs` | 24 | rendering bounds, spinner, sections, help, Unicode width, empty and error states, the three titled sections |
| `zellij/src/panes.rs` | 13 | pane recognition, attach commands, tab names, focus, pipe decoding |
| `zellij/src/guest.rs` | 14 | inner-session pane parsing and focus, relay theme names, keyboard signals, team sessions |
| `zellij/src/bar.rs` | 3 | the tab bar: what fits, where the tabs are, what a tab says |
| `zellij/src/details.rs` | 1 | closing keys of the details window |
| `zellij/e2e/nested.sh` | 55 checks | the pair end to end (§5) |

`go/cmd/noc` has no test; it is a few lines that call `cli.Run`.

A `Verified by` field states the evidence a requirement has. It is one of:
a named test, a gate, a check in the smoke or end-to-end script,
`MANUAL:` with the steps to run, or `UNTESTED`. A manual procedure is a
description of how to check, not a record that someone did.

## 2. Isolated smoke test

`docs/spec/verify/isolated-smoke.sh [path/to/noc]` (default `go/bin/noc`)
runs 34 self-asserting checks against the real binary. It needs `python3`.
It exits 0 when every check passes and 1 otherwise, and prints
`passed: N  failed: M`.

**Safety.**

- All state lives in a fresh temporary directory, passed through
  `NOC_STATE_DIR` and `NOC_RUNTIME_DIR`. The directory is left in place for
  inspection and its path is printed at the end.
- `NOC_CLAUDE_BIN`, `NOC_CODEX_BIN` and `NOC_CODEX_SOCKET` point at paths
  that do not exist.
- Every server start and every restart passes `--discovery=false`. A
  restart takes its configuration only from its own flags
  ([03-cli.md](03-cli.md) LIFE-020), so the flag is repeated there.

| Step | Asserts | Requirements exercised |
|---|---|---|
| `server --bg` | exit 0 and the socket exists; socket, PID file and database are `0600`; runtime and state directories are `0700` | LIFE, CFG-007, INV-010 |
| second `server --bg` | exit 1 | LIFE |
| `server status --json` | output parses | CLI, SRV |
| hook `SessionStart` (Claude) | exit 0 and silent; a live hook-origin row in the review bucket; model recorded with hook provenance; `show` by full ID | HK, OBX, INV-002 |
| hook `UserPromptSubmit` | bucket becomes working; the initial request is whitespace-compacted and labelled `hook_first_observed_user_prompt` | HK, D-027 |
| hook `Stop` with a 1,500-character message | bucket returns to review; the latest update is at most 1,200 characters | HK, D-027 |
| malformed hook | exit 0 and silent; journaled as `hook.rejected` | HK, OBX, INV-002 |
| `events --json` | never contains prompt text | INV-009, D-033 |
| `rename` | `user_name` is set | REG |
| `server restart --discovery=false` | exit 0; the PID changed; both providers report `disabled`; the agent keeps its ID and `user_name` | LIFE-020, INV-006 |
| `server stop` | socket and PID file are removed | LIFE |
| hook while the server is down | exit 0 and silent; `ls` exits 1 | INV-002, CLI |
| `server --bg` again | exit 0; the spooled hook is projected; ID and `user_name` survive the stop/start cycle | OBX, INV-006 |
| `SessionEnd` on a named row | hidden from `ls`; present in `ls --all` as `not_live` | HK, D-041 |
| `SessionStart` then `SessionEnd` with no prompt and no name | the row is gone even from `ls --all` | D-047 |

A failed restart fails its own checks; the script never falls back to stop
and start, which are checked as a separate cycle.

## 3. Acceptance criteria → evidence

The product is acceptable when each of these holds. "§6" refers to the
operator-initiated checks below.

| # | Criterion | Automated evidence | Real-provider evidence | Notes |
|---|---|---|---|---|
| AC-1 | Live Claude and Codex sessions that already exist appear after `noc server` starts | `TestRunServesInitialDiscovery`, `TestReconcilerMaintainsIdentityAndLiveness`, `TestDiscoverLiveFiltersAndSanitizes`, `TestSocketDiscoveryClientUsesDirectUnixSocket` | §6 R-1 | Automated against fakes |
| AC-2 | New top-level sessions appear without being launched through `noc` | `TestRunningServerProcessesHookAndExposesPreview`, `TestHookIngestSpoolsSilentlyWhileServerIsDown`, smoke §2 | §6 R-2 | Automated with synthetic hooks |
| AC-3 | Interactive and background sessions are distinguished | `TestNormalizeStateMappings` (Claude), `TestDiscoverLiveFiltersAndSanitizes` (`source_kind`) | §6 R-1 | |
| AC-4 | Working, idle, input, approval, failed and stale are represented when the evidence supports them | `TestNormalizeStateMappings` (Claude and Codex), `TestDeriveDisplayState`, `TestNotificationAttentionIgnoresIdleNudges`, `TestNormalizeCodexInterruptStopsCurrentTurn`, `TestReconcilerMaintainsIdentityAndLiveness` | — | The Claude adapter never reports `authentication_required` ([05-providers.md](05-providers.md)) |
| AC-5 | A server restart preserves identities and creates no duplicates | `TestStorePersistsHostAndAgents`, `TestReconcilerMaintainsIdentityAndLiveness`, smoke §2 | §6 R-3 | |
| AC-6 | Resuming or backgrounding a conversation keeps the same agent | `TestLifecycleEventIDSeparatesLaterResume`, `TestReconcilerSkipsDuplicatesAndIncompatibleRows` | §6 R-3 | Not after the row was pruned or forgotten ([00-overview.md](00-overview.md) Known limitations) |
| AC-7 | A change of directory or worktree updates the workspace | `TestInspectorClassifiesMainCheckoutAndLinkedWorktree`, `TestInspectorFallsBackToDirectory`, `TestProcessorBuildsAgentPreviewFromHookSequence` | §6 R-4 | |
| AC-8 | Provider subagents and cloud sessions do not appear | `TestNormalizeRejectsNonLiveOrInternalThreads`, `TestDiscoverFromConnectionFiltersAndSanitizes`, `TestNormalizeExcludesClaudeCloud`, `TestReconcilerSkipsDuplicatesAndIncompatibleRows` | — | |
| AC-9 | Stopping the `noc` server does not affect provider sessions | By construction: the server holds no provider process or terminal (INV-001). `TestServerStopStopsManagedServer` | §6 R-5 | Confirmed manually |
| AC-10 | Missed hooks are repaired by reconciliation | `TestReconcilerPreservesHookPreviewAndConfirmsSnapshotCoverage`, `TestDiscoveryFailureDoesNotAgeAgents` | §6 R-2 with hooks uninstalled | |
| AC-11 | The same data is available as human output and as stable JSON | `TestRenderAgentIncludesDetailPreviewMetadata`, `TestSingleBinaryServerAndDefaultClient`, `TestListFiltersAndOrdersByBucket` | — | |
| AC-12 | No terminal or transcript parsing | §7 A-4, A-5; `TestNormalizeClaudePromptIsBoundedAndCapturesFrontend` | — | Audit-based |
| AC-13 | `show --json` carries model, effort, bounded previews, directory, worktree and frontend hints | `TestNativeAgentFromThreadBoundsPreviewMetadata`, `TestReconcilerUsesProviderMetadataOverHookFallback`, `TestNormalizeClaudePromptIsBoundedAndCapturesFrontend`, `TestRenderAgentIncludesDetailPreviewMetadata` | §6 R-1, R-4 | |

## 4. System invariants → evidence

The invariants are defined in [00-overview.md](00-overview.md) §3. Where
the last column names a limitation, its full statement is in the "Known
limitations" list of the file named.

| Invariant | Evidence | Qualification |
|---|---|---|
| INV-001 Control plane, not runtime | §7 A-4: only bounded query commands, Git reads, `ps`, the server's own re-exec, and attach/stop children started by the CLI | — |
| INV-002 Fail-open toward providers | smoke §2: silent exit 0 for valid, malformed and server-down input | A payload delivered after the ingest deadline is dropped (00) |
| INV-003 Supported interfaces only | §7 A-4, A-5 | — |
| INV-004 Hooks plus reconciliation | REC requirements in [04-server.md](04-server.md); AC-10 | A provider that fails its startup probe is not reconciled until restart (00) |
| INV-005 Top-level rows only | AC-8; smoke §2 last step | — |
| INV-006 Stable identity | AC-5; smoke §2 restart and stop/start | Not across prune or forget (00) |
| INV-007 No deletion from one missed poll | `TestReconcilerMaintainsIdentityAndLiveness`, `TestDiscoveryFailureDoesNotAgeAgents` | — |
| INV-008 Raw and normalized state are separate | [02-api.md](02-api.md), [05-providers.md](05-providers.md) | — |
| INV-009 Privacy bounds | smoke §2 events check; bounding tests in `go/internal/hooks` | Bounded previews accumulate in the journal and are never pruned (00) |
| INV-010 Secure by default | smoke §2 permission checks | SQLite sidecar files rely on the directory mode (00) |
| INV-011 No Git or workflow ownership | §7 A-3 | — |
| INV-012 Frontend independence | §7 A-1, A-2 | — |
| INV-013 Provider runtimes are never started silently | tests in `go/internal/provider/...` and `go/internal/setup`; §7 A-4 | — |
| INV-014 Completion is not idleness | `TestDeriveDisplayState` | — |

## 5. Zellij end-to-end test

`make zellij-e2e` builds the plugin and runs `zellij/e2e/nested.sh`. It
needs `zellij` at the version the plugin is pinned to
([01-build.md](01-build.md) BLD-011), `tmux` and `jq`; it exits 2 when one
is missing.

The script starts the development pair in its `--fake` form
([01-build.md](01-build.md) BLD-049) inside a detached tmux session, sends
real key presses, and asserts on what Zellij reports about panes, tabs,
clients and focus. The registry is `zellij/e2e/fake-noc`, so no server and
no provider is involved. The sessions are `noc-e2e` (inner),
`noc-e2e-host` and the tmux session `noc-e2e-tmux`; `NOC_DEV_NAME` changes
the prefix. The user's Zellij configuration directory and daily sessions
are not touched. `nested.sh --keep` leaves the pair running for inspection.

**Precondition.** Zellij asks for a plugin's permissions once per plugin
location, interactively. Run `make dev` once and grant them in the sidebar;
until then the script stops with a message saying the plugin is waiting for
its permissions. This is why the test is a local gate and not a CI job.

The 26 checks, in order:

| Area | Checks |
|---|---|
| Start | the host session has the sidebar and one guest pane; the sidebar lists the registry's agents; the guest pane holds exactly one client of the inner session; the keyboard starts in the inner session; the inner session takes its options from `noc-inner.kdl` |
| Keyboard hand-over | `Alt-m` gives the keyboard to the sidebar; the inner session wears the away theme meanwhile; `Alt-m` gives it back; the home theme returns |
| Open an agent | `Enter` opens the agent in a new inner tab named after it; that tab runs `noc attach` for the agent; the keyboard follows into the inner session; exactly one tab was added; `Enter` on an agent that is already open focuses it and opens no copy; the tab is renamed when the agent's name changes |
| Details | `D` opens the floating details window; `q` closes it |
| Search | `/` lists the agents that match what is typed; `Esc` ends the search |
| Sidebar visibility | `Alt-o` hides the sidebar; `Alt-o` shows it again |
| Lifecycle | `Alt-d` detaches the host client; both sessions keep running; the inner client stays attached in the guest pane; starting again attaches to the running pair; ending the inner session ends the host session |

Success prints `nested: all checks passed`; otherwise the script prints
`nested: N check(s) failed` and exits 1. The behavior these checks cover is
specified in [07-zellij-panel.md](07-zellij-panel.md) and
[08-zellij-host.md](08-zellij-host.md).

## 6. Real-provider checks (operator-initiated)

These read real provider state and are run deliberately, never by a gate.
They do not create, resume, subscribe to or change a conversation. Except
for R-2 they use an isolated state and runtime directory, so the daily
registry is untouched.

```sh
D=$(mktemp -d /tmp/noc-real.XXXXXX)
export NOC_STATE_DIR=$D/state NOC_RUNTIME_DIR=$D/run
go/bin/noc server --bg       # discovery on: reads the Claude roster and the Codex daemon
go/bin/noc server status     # both providers available, with counts
go/bin/noc ls --json         # R-1
go/bin/noc server stop
```

| ID | Procedure | Expected |
|---|---|---|
| R-1 | With live Claude sessions (interactive and background) and Codex sessions open, start the isolated server | Each live top-level session appears once. `source_kind` distinguishes the kinds. Codex rows carry `model`, `reasoning_effort` and an `initial_request` with source `codex_app_server_preview`. |
| R-2 | Against the normal server, because provider hooks run the installed `noc hook ingest` in the provider's own environment and so always reach the default socket and outbox. With `noc setup` applied, run `noc watch` and start a new Claude session in another terminal | An `ADDED` line for a hook-origin row appears within about two seconds, before the next discovery poll confirms it. |
| R-3 | Note the IDs. `server stop`, then `server --bg`. Background a foreground Claude session. Resume a Codex thread | The same `id` values, and no duplicates. |
| R-4 | Change directory into a linked worktree inside a Claude session | `workspace.kind` becomes `worktree`; `root_path`, `branch` and `head_sha` update. |
| R-5 | `server stop` while sessions run | Every provider session continues; nothing is printed in their terminals. |
| R-6 | Gate G-7 | PASS, by a direct socket handshake with no proxy. |

Afterwards delete `$D`: it now holds real session names and paths.

## 7. Architectural audits

Run from the repository root. Each audit lists the only output that is
acceptable; anything else is a boundary violation to investigate.

| ID | Command | Expect |
|---|---|---|
| A-1 | `grep -rniE 'zellij\|vscode' go --include='*.go' \| grep -v _test.go` | Only: the Codex source kind `vscode`; the hook environment capture of `ZELLIJ_SESSION_NAME` and `ZELLIJ_PANE_ID` into a binding of kind `zellij` (`internal/hooks/normalize.go`); the attach refusal text that names Zellij sessions (`internal/attach/plan.go`). No Zellij command, layout or transport. |
| A-2 | `grep -rnE 'sqlite\|\.claude/\|\.codex/\|noc\.db' zellij/src` | No output: the plugin never reads the database or provider files. |
| A-3 | `grep -rnE '"(commit\|push\|rebase\|checkout\|switch\|worktree\|reset\|merge\|stash)"' go --include='*.go' \| grep -v _test.go` | Only the workspace kind constant `"worktree"` in `internal/workspace/inspect.go`. No Git command that changes a repository. |
| A-4 | `grep -rn 'exec.Command\|CommandContext' go --include='*.go' \| grep -v _test.go` | Exactly: `workspace/inspect.go` (Git reads), `provider/command.go` (the bounded runner), `provider/codex/proxy.go` (`app-server proxy`), `cli/run.go` (attach restore, attach, stop), `cli/server_lifecycle.go` (the server's own re-exec for `--bg`), `attach/claude_roster.go` (`agents --json --all`), `proclineage/proclineage.go` (`ps`). |
| A-5 | `grep -rn 'transcript' go --include='*.go' \| grep -v _test.go` | Only comments in `pkg/api/v1/types.go` stating that transcripts are excluded. |
| A-6 | `grep -rn 'net.Listen' go --include='*.go' \| grep -v _test.go` | Only `net.Listen("unix", …)` in `internal/localapi/socket.go`. |
| A-7 | `cd go && go list ./... \| grep -v /internal/` | Only `noc.local/noc/cmd/noc` and `noc.local/noc/pkg/api/v1`. |

## 8. Change protocol

For any change in behavior:

1. **Update the requirement.** Edit the affected requirements in the
   section file that owns them: the behavior statement, the `Source` paths
   and the `Verified by` field. A new requirement takes the next free
   number of its prefix; a removed requirement's ID is not reused.
2. **Add or adjust a test** so the requirement is not `UNTESTED` where a
   test is practical, and name it in `Verified by`.
3. **Record a decision** in [`docs/DECISIONS.md`](../DECISIONS.md) when the
   change alters a product or architecture rule, and cite its identifier in
   the requirement's `Decision` field.
4. **Record what remains.** A known gap or sharp edge goes into the
   "Known limitations" list of the same section file.
5. **Re-run the gates.** G-1 through G-4 always; G-5 for any change under
   `zellij/`; §6 for a change in provider behavior. If a gate's expected
   output changed (a new smoke or end-to-end check, a new audit hit),
   update this file in the same change.

When the code and this specification disagree, one of them has a bug.
Decide which, fix it, and leave them agreeing.
