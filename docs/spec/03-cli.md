# 03 — CLI and server lifecycle

This section specifies the whole command-line surface of the single `noc`
executable: process entry and signal handling, argument dispatch, every
command, subcommand, alias, flag, positional argument, validation rule, exit
code, stdout/stderr split, and the exact human and JSON output formats. It also
specifies the manual server process lifecycle (foreground and `--bg` modes, the
readiness pipe, the PID record, duplicate-instance rejection, stale-record
handling, `server stop`, `server restart`, `server status`), the resolution of
every configuration path and environment override, and the provider-command
planning that `noc attach`, `noc stop`, `noc respawn`, and `noc rename` perform in the
CLI process, the team commands, `noc spawn`, which starts a provider
session from the CLI process, `noc whoami`, and the installation of the noc
skill (`noc setup skill`). Server internals (registry, discovery, HTTP routing) are covered
in [04-server.md](04-server.md), [02-api.md](02-api.md), [05-providers.md](05-providers.md)
and [06-hooks.md](06-hooks.md), and are referenced here only where the CLI's
observable behaviour depends on them.

Source files covered:

- `go/cmd/noc/main.go`
- `go/internal/cli/run.go`, `go/internal/cli/search.go`, `go/internal/cli/teams.go`,
  `go/internal/cli/skill.go`
- `go/internal/skill/skill.go`, `go/internal/skill/SKILL.md`
- `go/internal/cli/server_lifecycle.go`
- `go/internal/cli/server_process_unix.go`
- `go/internal/cli/server_process_unsupported.go`
- `go/internal/cli/run_test.go`, `go/internal/cli/search_test.go`,
  `go/internal/cli/teams_test.go`, `go/internal/cli/skill_test.go`,
  `go/internal/cli/server_lifecycle_unix_test.go`
- `go/internal/config/paths.go`, `go/internal/config/paths_test.go`
- `go/internal/buildinfo/buildinfo.go`
- `go/internal/attach/plan.go`, `go/internal/attach/claude_roster.go`,
  `go/internal/attach/stop.go` and their tests

Neighbours read for observable behaviour: `go/internal/localapi/client.go`,
`go/internal/localapi/socket.go`, `go/internal/localapi/server.go`,
`go/internal/server/run.go`, `go/internal/storage/sqlite/store.go`,
`go/internal/registry/registry.go`, `go/pkg/api/v1/types.go`.

## Command reference

| Command line | Aliases | Purpose | Talks to server | Reference |
|---|---|---|---|---|
| `noc` | — | Same as `noc ls` with no flags | yes | CLI-002 |
| `noc help` | `-h`, `--help` | Print usage to stdout | no | CLI-003 |
| `noc version` | `--version` | Print build version | no | CLI-004 |
| `noc ls [--json] [--all] [--bucket list] [--team name\|none] [--socket path]` | `list` | List agents | yes | CLI-013..CLI-017, CLI-040 |
| `noc search [--json] [--older-than age] [--newer-than age] [--bucket list] [--team name\|none] [--socket path] [text...]` | `find` | List the agents that match a query | yes | CLI-037..CLI-039, CLI-040 |
| `noc show [--json] [--socket path] <agent-id>` | — | Show one agent | yes | CLI-018, CLI-019 |
| `noc attach [--force] [--socket path] <agent-id>` | — | Open the provider's terminal UI on an agent | yes (lookup only) | ATT-001..ATT-017 |
| `noc rename [--clear] [--socket path] <agent-id> [name...]` | `name` | Set/clear the user name; push to provider where supported | yes | CLI-027..CLI-029 |
| `noc stop [--socket path] <agent-id>` | — | Stop a Claude background session via the provider | yes (lookup only) | ATT-018..ATT-021 |
| `noc respawn [--socket path] <agent-id>` | — | Restart a Claude background session on the Claude binary installed now, via the provider | yes (lookup only) | ATT-022 |
| `noc forget [--socket path] <agent-id>...` | — | Delete registry rows | yes | CLI-030 |
| `noc events [--json] [--provider p] [--limit n] [--socket path] [agent-id]` | — | Bounded event diagnostics | yes | CLI-020, CLI-021 |
| `noc watch [--json] [--all] [--team name\|none] [--socket path]` | — | Stream registry changes | yes (streaming) | CLI-022..CLI-026, CLI-040 |
| `noc teams [--json] [--socket path]` | — | Summarise agents by team | yes | CLI-044 |
| `noc team set [--socket path] <name> [agent-id...]` | — | Put agents in a team | yes | CLI-041..CLI-043 |
| `noc team clear [--socket path] [agent-id...]` | — | Take agents out of their team | yes | CLI-041..CLI-043 |
| `noc team lead [--socket path] <name> [agent-id]` | — | Mark a team's lead | yes | CLI-041..CLI-043 |
| `noc spawn [--provider claude] [--team name\|none] [--name name] [--dir path] [--socket path] <prompt...> [-- provider-arguments]` | — | Start a background Claude session and register it | yes | CLI-045..CLI-047 |
| `noc doctor [--json] [--socket path] [--database path] [--timeout d]` | — | Installation diagnostics | optional | CLI-031, CLI-032 |
| `noc setup [--check\|--uninstall] [--json] [--executable path] [claude\|codex]` | — | Manage provider hook handlers | no | CLI-033, CLI-034 |
| `noc setup skill [--check\|--uninstall] [--json] [claude\|codex]` | — | Install, check or remove the noc skill | no | CLI-048..CLI-051 |
| `noc whoami [--json] [--socket path]` | — | Show the agent the command runs inside | yes | CLI-052 |
| `noc server [launch flags] [--bg]` | `server run`, `server start` | Run the server (foreground, or detached with `--bg`) | — | LIFE-001..LIFE-016 |
| `noc server stop [--socket] [--pid-file] [--timeout]` | — | SIGTERM the managed server and wait | probe only | LIFE-017..LIFE-019 |
| `noc server restart [launch flags] [--timeout]` | — | Stop a background server, start a new background server | probe only | LIFE-020..LIFE-022 |
| `noc server status [--json] [--socket] [--pid-file]` | — | Health, process record, provider discovery | yes | LIFE-023..LIFE-026 |
| `noc hook ingest --provider claude\|codex [--database] [--socket]` | — | Internal, silent, fail-open hook entrypoint | best-effort wake | CLI-035, CLI-036 |

## Dispatch and global conventions

#### CLI-001 Process entry and signal context
- **Behavior:** `main` MUST create a context cancelled on `SIGINT` (`os.Interrupt`) or `SIGTERM`, pass `os.Args[1:]`, `os.Stdout`, `os.Stderr` to `cli.Run`, and exit the process with the returned integer. `cli.Run` MUST use `os.Stdin` as standard input (`RunWithIO`). The first delivered SIGINT/SIGTERM cancels the context instead of killing the process; every command observes that context (server shuts down, watch returns 0, attach child is killed via `exec.CommandContext`).
- **Source:** `go/cmd/noc/main.go` (main); `go/internal/cli/run.go` (Run, RunWithIO)
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerHelper (reproduces the same NotifyContext wiring); MANUAL: run `noc server --discovery=false` in an isolated env and press Ctrl-C; expect exit 0 and the socket and PID file removed.

#### CLI-002 No arguments means `ls`
- **Behavior:** When `args` is empty, `noc` MUST behave exactly like `noc ls` with no flags (live agents only, human table). Any first argument that is not a recognised command — including flags such as `--json` — MUST NOT be routed to `ls` (see CLI-005).
- **Source:** `go/internal/cli/run.go` (RunWithIO)
- **Verified by:** `go/internal/cli/run_test.go` TestSingleBinaryServerAndDefaultClient

#### CLI-003 Top-level help
- **Behavior:** First argument exactly `help`, `-h`, or `--help` MUST print the usage text below to **stdout** and exit 0. Remaining arguments are ignored. `-help` (single dash, long) is NOT recognised and falls to CLI-005. The usage text is exactly (trailing newline added by `Fprintln`):
  ```text
  noc manages the local coding-agent registry.

  Usage:
    noc
    noc version
    noc server [--bg] [--socket path] [--database path]
    noc server restart
    noc server stop
    noc server status [--json]
    noc ls [--json] [--all] [--bucket blocked,working,review,quiet] [--team name|none]
    noc search [--json] [--older-than age] [--newer-than age] [--bucket ...] [--team name|none] [text...]
    noc show [--json] <agent-id>
    noc attach [--force] <agent-id>
    noc rename [--clear] <agent-id> [name]
    noc stop <agent-id>
    noc respawn <agent-id>
    noc forget <agent-id> [<agent-id>...]
    noc events [--json] [--provider claude|codex] [--limit count] [agent-id]
    noc watch [--json] [--all] [--team name|none]
    noc teams [--json]
    noc team set <name> [agent-id...]
    noc team clear [agent-id...]
    noc team lead <name> [agent-id]
    noc spawn [--provider claude] [--team name|none] [--name name] [--dir path] <prompt> [-- provider-arguments]
    noc doctor [--json]
    noc setup [--check|--uninstall] [claude|codex]
    noc setup skill [--check|--uninstall] [claude|codex]
    noc whoami [--json]

  Internal provider hook entrypoint:
    noc hook ingest --provider claude|codex

  The server runs in the foreground unless --bg is supplied. Provider discovery
  and durable hook ingestion are implemented. Run noc setup after installing the
  binary to configure Claude and Codex hooks, and noc setup skill to teach
  agents how to use noc.
  ```
  The usage text intentionally does not list aliases or most flags; the per-command flag usage (CLI-006) is the complete flag reference.
- **Source:** `go/internal/cli/run.go` (RunWithIO, usageText)
- **Verified by:** `go/internal/cli/run_test.go` TestRunHelp (checks presence of server status, `server [--bg]`, restart, stop, events, doctor, watch, setup lines)

#### CLI-004 Version
- **Behavior:** First argument `version` or `--version` MUST print `buildinfo.String()` plus a newline to stdout and exit 0; remaining arguments are ignored. `buildinfo.String()` MUST return `Version` when `Commit` is `""` or `"none"`, otherwise `"<Version> (<Commit>)"`. Defaults are `Version="dev"`, `Commit="none"`, `Date="unknown"` (`Date` is never printed). These are package variables set at build time with `-ldflags -X` (see [01-build.md](01-build.md)); a binary built without them, e.g. by a plain `go build`, prints `dev`. The server's health `version` field uses `buildinfo.Version` alone and `commit` carries `buildinfo.Commit` (so `server status` human output shows `(<Version>)` without the commit).
- **Source:** `go/internal/cli/run.go` (RunWithIO); `go/internal/buildinfo/buildinfo.go` (String); `go/internal/server/run.go` (Run)
- **Verified by:** `go/internal/cli/run_test.go` TestVersion; `go/internal/buildinfo/buildinfo_test.go` TestString; MANUAL: `go build -o $T/noc ./cmd/noc && $T/noc version` prints `dev`; with `-ldflags "-X noc.local/noc/internal/buildinfo.Version=1.2.3 -X noc.local/noc/internal/buildinfo.Commit=abc"` prints `1.2.3 (abc)`.

#### CLI-005 Unknown command
- **Behavior:** Any other first argument MUST write `noc: unknown command "<arg>"` (Go `%q` quoting), a blank line, then the full usage text (CLI-003) to **stderr**, and exit **2**.
- **Source:** `go/internal/cli/run.go` (RunWithIO)
- **Verified by:** `go/internal/cli/run_test.go` TestRunRejectsUnknownCommand

#### CLI-006 Flag parsing conventions (all subcommands)
- **Behavior:** Every subcommand except `hook ingest` MUST parse flags with Go's `flag` package in `ContinueOnError` mode with output to **stderr**, which implies:
  - Flags may be spelled with one or two dashes (`-json` ≡ `--json`); values as `--flag=value` or `--flag value` (non-bool only).
  - Boolean flags can only be negated with `=`: `--discovery=false`. `--discovery false` sets true and leaves `false` as a positional argument (then rejected by the positional check).
  - Flag parsing stops at the first non-flag argument; flags after a positional are positionals. E.g. `noc attach <id> --force` has two positionals and is a usage error; `noc rename <id> --clear` names the agent `--clear`.
  - `-h`, `-help`, `--help` MUST print `Usage of <flagset name>:` and the flag defaults to stderr and exit **0**.
  - An undefined flag or bad value MUST print `flag provided but not defined: -x` (or the flag package's value error) plus usage to stderr and exit **2**.
  - Flag-set names (shown in usage) are `noc ls`, `noc search`, `noc show`, `noc attach`, `noc stop`, `noc respawn`, `noc forget`, `noc rename`, `noc events`, `noc watch`, `noc teams`, `noc team <verb>` (the verb as typed), `noc spawn`, `noc whoami`, `noc doctor`, `noc setup`, `noc setup skill`, `noc server`, `noc server status`, `noc server restart`, `noc server stop`, `noc hook ingest`. Note `ls` uses the flag-set name `noc ls` even when invoked as `list`, and `rename` uses `noc rename` even when invoked as `name`.
- **Source:** `go/internal/cli/run.go` (newFlagSet, parseFlags); the same pattern is used by every `run*` function
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (`ls -h`, an undefined flag, `server restart --bg`); `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (restart accepts launch flags and `--timeout`); one-dash spelling and flag-after-positional for `rename` UNTESTED.

#### CLI-007 Exit codes
- **Behavior:** Commands MUST use these exit codes:

  | Code | Meaning | Where |
  |---|---|---|
  | 0 | Success; help; `-h` on any flag set; watch ended by signal; `hook ingest` always (except CLI-035) | all |
  | 1 | Runtime failure: server unreachable, API error, path resolution failure, provider command could not be found/started, lifecycle errors, attach/stop refusals, doctor/setup reports with failures, a `setup skill` result with an error or a `setup skill --check` that finds a provider's skill not current (CLI-050), `whoami` outside an agent noc knows, any `forget` or `team set`/`team clear` ID failed, `team lead` for an agent outside the team, any `spawn` failure after validation (CLI-047) | `printError`, `printServerError`, `printServerLifecycleError` |
  | 2 | Usage error: unknown command, unknown `server` or `team` subcommand, flag parse error, wrong positional count, invalid flag value caught by validation (including an invalid team name), a `team` command with no agent ID run outside an agent | all |
  | N ≥ 0 | `attach`: the exit status of the provider child process when it exited normally | ATT-013 |
  | N > 0 | `stop`, `respawn`: the provider child's nonzero exit status | ATT-021, ATT-022 |
  | 130 | `attach`: the child was killed by a signal (`ExitCode() < 0`) **and** the CLI context was cancelled | ATT-013 |
- **Source:** `go/internal/cli/run.go` (printServerError, printError, printServerLifecycleError, runAttach, runLifecycle)
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (2), TestServerDownShowsConnectionHint (1), TestAttachCodexRunsResume and TestAttachClaudeDecisionTable (child status 7); 130 UNTESTED.

#### CLI-008 Output streams and error formatting
- **Behavior:** Normal results MUST go to stdout; every diagnostic MUST go to stderr. Error formats:
  - Generic: `noc: <error>` (one line), exit 1.
  - Server connection error (any error wrapping `localapi.ErrConnection`, whose text is `connect to noc server`): `noc: connect to noc server: <transport error>` followed by the line `Is the server running? Start it with: noc server`, exit 1.
  - API error responses with a JSON `message`: `noc: noc server: <message>` (e.g. `noc: noc server: agent was not found`); without a decodable message: `noc: noc server: unexpected HTTP status <status>`.
  - Already-running lifecycle error (wrapping `errServerAlreadyRunning` or `localapi.ErrServerRunning`): exactly `noc: noc server is already running` then `Stop the previous instance by running: noc server stop`, exit 1. The `(pid N)` detail carried by the wrapped error is deliberately not printed.
  - Validation errors raised by the CLI itself use the flag-set prefix, e.g. `noc ls: unknown bucket ...`, `noc events: limit must be between 1 and 200`, exit 2.
- **Source:** `go/internal/cli/run.go` (printError, printServerError, printServerLifecycleError); `go/internal/localapi/client.go` (ErrConnection, statusError)
- **Verified by:** `go/internal/cli/run_test.go` TestBackgroundStartRejectsExistingServerWithStopGuidance (already-running text), TestServerDownShowsConnectionHint (connection hint), TestRunningServerProcessesHookAndExposesPreview (API message); MANUAL M-1.

#### CLI-009 JSON output encoding
- **Behavior:** Every `--json` flag except `watch --json` MUST write exactly one JSON document using `encoding/json` with two-space indentation (`SetIndent("", "  ")`) followed by a newline. An encode failure MUST print `noc: write JSON: <err>` and exit 1. `watch --json` MUST write compact (unindented) one-object-per-line NDJSON (CLI-025). The JSON shapes are the public `apiv1` types: `ls` → `AgentList` (`schema_version`, `generated_at`, `agents`); `show` → `Agent`; `events` → `AgentEventList`; `server status` → `Health`; `teams` → `TeamList`; `whoami` → `Agent`; `setup skill` → the report of CLI-050; `doctor` → `doctor.Report`; `setup` → `setup.Report`.
- **Source:** `go/internal/cli/run.go` (writeJSON, runWatch)
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (decodes `ls`, `show`, `events` JSON)

#### CLI-010 Client request timeouts
- **Behavior:** Client commands MUST bound their API calls: `ls`, `show`, `events`, `forget` (one shared deadline for all IDs), `server status` (shared by its health and list calls), `teams`, `team` and `whoami` (each one shared deadline for the whole command, including finding the calling agent), and the agent lookup in `attach`/`stop` use **3 s** (`requestTimeout`). `spawn` uses 3 s for finding its caller and 3 s for registering, and bounds the provider separately (CLI-046). `rename` uses **10 s** (`renameRequestTimeout`) because the server pushes the name to the provider (the server bounds that push at 5 s). `watch` has no deadline. `doctor` uses its `--timeout` (default 15 s) for all diagnostics. `hook ingest` uses a 900 ms context for its SQLite calls and wake (150 ms for the wake call). It is not an overall bound on the process: the stdin read is unbounded and the process-table snapshot (`ps`, killed after 2 s) is not tied to it; both run after the 900 ms clock has started (CLI-036, HK-005).
- **Source:** `go/internal/cli/run.go` (requestTimeout, renameRequestTimeout, hookIngestTimeout, hookWakeTimeout, defaultServerTimeout); `go/internal/localapi/server.go` (providerRenameTimeout); `go/internal/proclineage/proclineage.go` (snapshotTimeout)
- **Verified by:** UNTESTED

#### CLI-011 Agent reference resolution
- **Behavior:** Every `<agent-id>` positional (`show`, `attach`, `stop`, `respawn`, `rename`, `forget`, `events`, `team`) MUST be sent verbatim (URL path-escaped) to the server and MUST be matched **only as an exact, full noc agent ID** (a UUID string; SQL `WHERE id = ?`). There is no prefix matching, no matching by name, user name, short ID, or native session ID, and therefore no ambiguity error. A non-matching ID MUST yield HTTP 404 `agent_not_found` → stderr `noc: noc server: agent was not found`, exit 1. An empty string ID yields HTTP 400 → `noc: noc server: an agent ID is required`, exit 1. In particular a leading prefix of a real ID (e.g. the first 8 characters, as frontends often display) MUST NOT resolve: `noc show <8-char-prefix>` → `noc: noc server: agent was not found`, exit 1. The CLI performs no client-side resolution (no list-then-match) before the request, and neither `Store.Get` nor `Store.Forget` uses `LIKE` or prefix comparison.
- **Source:** `go/internal/localapi/client.go` (GetAgent); `go/internal/localapi/server.go` (handleAgent); `go/internal/storage/sqlite/store.go` (Get, Forget)
- **Verified by:** `go/internal/localapi/server_test.go` TestClientAndServerOverUnixSocket; `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (`show nope`, `forget nope`); prefix rejection UNTESTED.

#### CLI-012 `--socket` on every client command
- **Behavior:** `ls`, `search`, `show`, `attach`, `stop`, `respawn`, `forget`, `rename`, `events`, `watch`, `teams`, `team`, `spawn`, `whoami`, `doctor`, `server status`, `server stop`, `server restart`, `server` and `hook ingest` MUST accept `--socket <path>` (string) defaulting to the resolved `SocketPath` (CFG-003). Every command that needs paths MUST call `config.ResolvePaths()` before parsing flags; a resolution error MUST print `noc: <err>` and exit 1 (for `hook ingest`, exit 0 silently). `setup` does not resolve paths; `setup skill` resolves only the skill files (CLI-049) and takes no `--socket`.
- **Source:** `go/internal/cli/run.go` (every `run*` function)
- **Verified by:** indirectly by every test that sets `NOC_SOCKET` (`go/internal/cli/run_test.go`)

## Registry read commands

#### CLI-013 `ls` flags and arguments
- **Behavior:** `noc ls` (alias `list`) flags:

  | Flag | Type | Default | Meaning |
  |---|---|---|---|
  | `--json` | bool | false | Write the `AgentList` JSON |
  | `--all` | bool | false | Request `GET /v1/agents?all=true` (include retained non-live rows) |
  | `--bucket` | string | `""` | Comma-separated bucket filter (CLI-014) |
  | `--team` | string | `""` | Team filter (CLI-040) |
  | `--socket` | string | resolved socket | Server socket |

  Any positional MUST print `noc ls: unexpected positional arguments` to stderr, exit 2. The bucket list and then the team are validated before contacting the server.
- **Source:** `go/internal/cli/run.go` (runList)
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (positional, bad bucket); filtering via CLI-014.

#### CLI-014 Bucket filter parsing
- **Behavior:** `--bucket` MUST be split on `,`; each part is trimmed and lower-cased; empty parts are skipped (so `""`, `","`, `" "` mean "all buckets"). Each remaining name MUST be one of `blocked`, `working`, `review`, `quiet`; otherwise stderr `noc ls: unknown bucket "<lowercased-name>" (want blocked, working, review, or quiet)`, exit 2. Duplicates are harmless.
- **Source:** `go/internal/cli/run.go` (parseBuckets); `go/pkg/api/v1/types.go` (Buckets, Rank)
- **Decision:** D-046
- **Verified by:** `go/internal/cli/run_test.go` TestListFiltersAndOrdersByBucket (`"review, Blocked"` accepted, `"bogus"` rejected)

#### CLI-015 `ls` filtering and ordering
- **Behavior:** After the server responds, the CLI MUST (1) drop agents whose `bucket` is not selected (no-op when the selection is empty), (2) drop agents the team filter does not keep (CLI-040), and (3) stable-sort by bucket rank ascending — `blocked`(0), `working`(1), `review`(2), `quiet`(3), any other/empty value (4) — then by recency descending. Recency is the first non-nil of `last_activity_at`, `ended_at`, `created_at`, else `first_seen_at`; `last_seen_at` MUST NOT be used (it is refreshed by every reconciliation). The filtered, re-ordered list is what both human and JSON output show (the JSON `agents` order is the CLI's, not the server's).
- **Source:** `go/internal/cli/run.go` (runList, filterBuckets, sortByBucket, agentRecency)
- **Decision:** D-046, D-048
- **Verified by:** `go/internal/cli/run_test.go` TestListFiltersAndOrdersByBucket; `go/internal/cli/run_test.go` TestRecencyIgnoresWhenNocLastLooked

#### CLI-016 `ls` human output
- **Behavior:** With zero agents (after filtering) MUST print exactly `No live agents.` (also with `--all` or `--bucket`). Otherwise MUST print a `text/tabwriter` table (minwidth 0, tabwidth 4, padding 2, pad char space, no flags) with header `ID  PROVIDER  BUCKET  STATE  WORKSPACE  NAME` and one row per agent: `id`, `provider`, `bucket`, `display_state`, `workspace.path` or `-` when workspace is nil or path empty, display name or `-`. Display name = `user_name` if non-empty else provider `name` (D-035/D-042 overlay). When any listed agent is in a team the table gains a `TEAM` column after `ID` (CLI-040). No column is truncated; no timestamps are printed.
- **Source:** `go/internal/cli/run.go` (renderAgents, agentDisplayName)
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/cli/run_test.go` TestSingleBinaryServerAndDefaultClient (empty message); table layout UNTESTED; MANUAL M-3.

#### CLI-017 `ls --json`
- **Behavior:** MUST write the `AgentList` returned by the server (`schema_version` 1, server `generated_at`, `agents` always an array — `[]` when empty) after CLI-015 filtering/sorting, per CLI-009.
- **Source:** `go/internal/cli/run.go` (runList); `go/internal/localapi/server.go` (handleAgents)
- **Decision:** D-019
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview

#### CLI-037 `search` query
- **Behavior:** `noc search` (alias `find`) takes the flags `--json`, `--older-than <age>`, `--newer-than <age>`, `--bucket <list>` (CLI-014), `--team <name|none>` (CLI-040) and `--socket`, then any number of positional words. It builds one query; an agent matches when every part holds:
  - **Text.** Each positional is lower-cased and split on whitespace into terms. Every term MUST occur, case-insensitively, as a substring of the agent's text: its id, user name, name, team, provider, native session id and short id, model, display state, bucket, summary, attention detail, initial request text, and workspace path, root path and branch.
  - **Age.** An agent's age is the time since its recency (CLI-015: last activity, else ended, else created, else first seen; never when noc last looked). With an older-than bound the age MUST be at least the bound; with a newer-than bound, at most the bound.
  - **Ages** are a positive whole number followed by `d` (days; also the default with no letter), `w` (weeks) or `h` (hours), case-insensitive, at most `100000`.
  - **Bounds in the text.** A term `older:<age>` or `newer:<age>` that parses sets that bound (overriding the flag) and is not matched as text; one that does not parse is an ordinary term, so a query being typed never fails.
  - **Team in the text.** A term `team:<name>` with a non-empty name is not matched as text: the agent's team MUST equal the name exactly, compared case-insensitively (terms are lower-cased), and `team:none` keeps only agents in no team. The name is not validated, so a name no team has matches nothing. Several such terms must all hold. A bare `team:` is an ordinary term.

  An unparsable flag value MUST print `noc search: invalid age "<value>" (want a number of days such as 14, or 14d, 2w, 36h)` and exit 2. An older-than bound greater than the newer-than bound MUST print `noc search: no agent is both older than <age> and newer than <age>` and exit 2. Bucket errors are as in CLI-014 and team-flag errors as in CLI-040, with the `noc search:` prefix. All of this is validated before contacting the server.
- **Source:** `go/internal/cli/search.go` (runSearch, parseSearchQuery, parseAge, agentSearchText, searchQuery.matches)
- **Decision:** D-065
- **Verified by:** `go/internal/cli/search_test.go` TestParseAge, TestParseSearchQuery, TestSearchAgentsByTextAndAge, TestSearchRejectsBadFlags; `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (the team as text, `team:<name>` exact and case-insensitive, `team:none`). A bare `team:` and two team terms are UNTESTED.

#### CLI-038 `search` results
- **Behavior:** `search` MUST read every agent the server retains (as `ls --all`), keep those in the selected buckets and kept by the team flag that match the query, and order them as CLI-015. With no words and no bounds every agent matches. `--json` MUST write the same `AgentList` document as `ls --json` (CLI-017), holding the matches.
- **Source:** `go/internal/cli/search.go` (runSearch, searchAgents)
- **Decision:** D-065
- **Verified by:** `go/internal/cli/search_test.go` TestSearchAgentsByTextAndAge; `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (the server round trip, `--json`, `--team` with text)

#### CLI-039 `search` human output
- **Behavior:** With no match it MUST print `No agents match.`. Otherwise a table with the columns `ID`, `PROVIDER`, `BUCKET`, `STATE`, `AGE`, `WORKSPACE`, `NAME`, as CLI-016 (including its `TEAM` column after `ID` when any match is in a team) plus `AGE`: the agent's age in its largest whole unit, `<n>w` from 14 days, `<n>d` from 1 day, `<n>h` from 1 hour, else `<n>m` (`0m` for a time in the future).
- **Source:** `go/internal/cli/search.go` (renderSearchResults, formatAge)
- **Verified by:** `go/internal/cli/search_test.go` TestSearchAgentsByTextAndAge

#### CLI-018 `show` arguments
- **Behavior:** `noc show` flags: `--json` (bool, false), `--socket`. Exactly one positional is required; otherwise stderr `usage: noc show [--json] <agent-id>`, exit 2. It MUST `GET /v1/agents/<id>` (CLI-011) and print JSON (`Agent`) or the human form.
- **Source:** `go/internal/cli/run.go` (runShow)
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (`show --json`), TestValidationPaths (usage)

#### CLI-019 `show` human output
- **Behavior:** MUST print a two-column tabwriter (same parameters as CLI-016) of `Label:<TAB>value` lines in this order; conditional lines are omitted when their value is empty/nil:
  1. `ID:`, `Provider:`, `Native session:`, `Host:`, `State:` (display_state), `Runtime:` (runtime_state), `Activity:` (activity_state) — always.
  2. `User name:` if `user_name`; `Provider name:` if `name`; `Team:` if `team`, written `<team>` or, for the team's lead, `<team> (lead)`; `Parent:` if `parent_id`; `Model:` if `model`; `Reasoning effort:` if `reasoning_effort`.
  3. If `initial_request` non-nil: `Initial request:` text and `Initial request source:` source.
  4. If `summary` non-empty: `Current summary:` and, if `summary_source` non-empty, `Summary source:`.
  5. If `workspace` non-nil: `Directory:` path (always, even if empty), then `Workspace root:` if root_path, `Workspace kind:` if kind, `Branch:` if branch.
  6. One `Frontend:` line per frontend binding: `<kind>[:<session>][/<pane_id>] (<state>)`.
  Bucket, live state, capabilities, timestamps, and native state are not shown in human form.
- **Source:** `go/internal/cli/run.go` (renderAgent)
- **Verified by:** `go/internal/cli/run_test.go` TestRenderAgentIncludesDetailPreviewMetadata; `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (`auth (lead)`). The `Parent:` line is UNTESTED.

#### CLI-020 `events` flags and validation
- **Behavior:** Flags:

  | Flag | Type | Default | Validation |
  |---|---|---|---|
  | `--json` | bool | false | — |
  | `--provider` | string | `""` | `""`, `claude`, or `codex`; else `noc events: provider must be claude or codex`, exit 2 |
  | `--limit` | int | 50 (`registry.DefaultEventLimit`) | 1..200 (`registry.MaxEventLimit`); else `noc events: limit must be between 1 and 200`, exit 2 |
  | `--socket` | string | resolved socket | — |

  More than one positional MUST print `usage: noc events [--json] [--provider claude|codex] [--limit count] [agent-id]`, exit 2 (checked before provider/limit validation). With one positional the CLI MUST request `/v1/agents/<id>/events`, otherwise `/v1/events`, adding `provider` and `limit` query parameters.
- **Source:** `go/internal/cli/run.go` (runEvents); `go/internal/registry/registry.go` (DefaultEventLimit, MaxEventLimit); `go/internal/localapi/client.go` (ListEvents)
- **Decision:** D-033
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (`events --json <id>`), TestValidationPaths (limit, provider, positionals); MANUAL M-3.

#### CLI-021 `events` human output
- **Behavior:** Zero events MUST print `No events found.` Otherwise a tabwriter table (CLI-016 parameters) with header `TIME  PROVIDER  SOURCE  EVENT  AGENT  DETAIL`, rows in server order. TIME = `occurred_at` if set else `received_at`, rendered in local time as `2006-01-02 15:04:05`. AGENT = `agent_id` or `-`. DETAIL joins, with `"; "`, the parts that apply in this order: `native=<native_event>`; `<previous_live_state or -> -> <live_state or ->` when either is set; `reason=<reason>`; `error=<error>`; or `-` if none.
- **Source:** `go/internal/cli/run.go` (renderEvents, renderEventDetail)
- **Decision:** D-033
- **Verified by:** `go/internal/cli/run_test.go` TestRenderEventsShowsSafeTimeline

#### CLI-022 `watch` flags
- **Behavior:** Flags: `--json` (bool), `--all` (bool; adds `?all=true` to `/v1/agents/watch`), `--team` (CLI-040), `--socket`. Any positional MUST print `noc watch: unexpected positional arguments`, exit 2; the team is validated next, before the request. The stream request has no timeout. With `--team`, every received snapshot is reduced to the agents the filter keeps before anything else is done with it, so an agent that joins or leaves the team shows as `ADDED` or `REMOVED` (CLI-023).
- **Source:** `go/internal/cli/run.go` (runWatch); `go/internal/localapi/client.go` (WatchAgents)
- **Decision:** D-031
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (positional); `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (invalid `--team`); `go/internal/localapi/server_test.go` TestWatchAgentsStreamsSemanticSnapshots (transport). Filtering a stream by team is UNTESTED.

#### CLI-023 `watch` human output
- **Behavior:** On the first snapshot MUST print `Watching agents; press Ctrl-C to stop.` then the CLI-016 table (or `No live agents.`) of that snapshot **in server order** (no CLI-015 sort). For each later snapshot MUST diff by agent ID against the previous snapshot and print one tabwriter row (no header) per change: `<HH:MM:SS>  <KIND>  <id>  <provider>  <state>  <workspace or ->  <name>`, where HH:MM:SS is the snapshot's `generated_at` in local time; KIND is `ADDED`, `UPDATED` (semantic change, CLI-024), or `REMOVED`; rows sorted by KIND lexically (`ADDED` < `REMOVED` < `UPDATED`) then ID. For UPDATED, state is `<old> -> <new>` when display_state changed, and name is `<old or -> -> <new or ->` when the display name changed. A snapshot with no changes prints nothing.
- **Source:** `go/internal/cli/run.go` (runWatch, renderAgentChanges)
- **Decision:** D-031, D-042
- **Verified by:** `go/internal/cli/run_test.go` TestRenderAgentChangesShowsMembershipAndState

#### CLI-024 `watch` semantic equality
- **Behavior:** Two snapshots of one agent MUST be considered equal (no UPDATED row) when they differ only in `last_seen_at`, `snapshot_confirmed_at`, `model_observed_at`, `reasoning_observed_at`, `summary_updated_at`, `initial_request.observed_at`, `workspace.observed_at`, or any `frontend_bindings[].observed_at`; any other field difference (deep equality) is a change.
- **Source:** `go/internal/cli/run.go` (watchAgentsEqual, watchAgentProjection)
- **Verified by:** `go/internal/cli/run_test.go` TestWatchAgentsEqualIgnoresReconciliationTimestamps

#### CLI-025 `watch --json`
- **Behavior:** MUST write every received `AgentList` snapshot as one compact JSON line (NDJSON) as it arrives; no banner and no diffing. The snapshot is unmodified except that with `--team` its `agents` hold only the agents the filter keeps (CLI-022); a snapshot is written even when the filter leaves its agents unchanged from the previous line.
- **Source:** `go/internal/cli/run.go` (runWatch)
- **Decision:** D-031
- **Verified by:** UNTESTED (CLI)

#### CLI-026 `watch` termination
- **Behavior:** If the stream ends because the context was cancelled or its deadline passed (SIGINT/SIGTERM), `watch` MUST exit 0 silently. If the server closes the stream, MUST print `noc: connect to noc server: agent watch stream closed` plus the CLI-008 hint, exit 1. Any other error (including decode errors and non-2xx responses) prints per CLI-008, exit 1.
- **Source:** `go/internal/cli/run.go` (runWatch); `go/internal/localapi/client.go` (WatchAgents)
- **Verified by:** `go/internal/localapi/server_test.go` TestWatchAgentsPreservesCanceledContext (transport); CLI UNTESTED

## Mutating registry commands

#### CLI-027 `rename` arguments
- **Behavior:** `noc rename` (alias `name`) flags: `--clear` (bool), `--socket`. Positional rules: with `--clear`, exactly one positional (`<agent-id>`); without, at least two (`<agent-id> <name words...>`). Violations MUST print `usage: noc rename [--clear] <agent-id> [name]`, exit 2. The name MUST be `strings.Join(args[1:], " ")` trimmed of surrounding whitespace; if empty MUST print `noc rename: name cannot be empty; use --clear to remove it`, exit 2. `--clear` sends an empty name. The server additionally rejects names longer than 120 runes (`noc: noc server: agent name must be at most 120 characters`, exit 1).
- **Source:** `go/internal/cli/run.go` (runRename); `go/internal/localapi/server.go` (handleAgentName)
- **Decision:** D-035, D-042
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (multi-word name `Hook ingestion`, 121-character rejection), TestValidationPaths (usage, empty name); MANUAL M-4.

#### CLI-028 `rename` request
- **Behavior:** MUST send `PUT /v1/agents/<id>/name` with JSON body `{"name":"<name>"}` and `Content-Type: application/json`, timeout 10 s; MUST read the updated `Agent` body and the `Noc-Provider-Name-Sync` and `Noc-Provider-Name-Sync-Error` response headers. Server/connection errors print per CLI-008, exit 1.
- **Source:** `go/internal/cli/run.go` (runRename); `go/internal/localapi/client.go` (RenameAgent); `go/pkg/api/v1/types.go` (ProviderNameSyncHeader, ProviderNameSyncErrorHeader)
- **Decision:** D-042
- **Verified by:** `go/internal/localapi/rename_test.go` TestRenamePushesNameToProvidersThatSupportIt (header); `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview

#### CLI-029 `rename` output by provider sync result
- **Behavior:** On success (exit 0) the CLI MUST print to stdout, using the `id`, `user_name`, and `provider` from the response body (`%q` Go quoting for the name):

  | Condition | stdout | stderr |
  |---|---|---|
  | `--clear` (any header) | `Cleared the name for <id>.` | — |
  | header `synced` | `Named <id> "<name>"; the <provider> session was renamed too.` | — |
  | header `unsupported` | `Named <id> "<name>" in noc. <Claude\|Codex\|provider> cannot be renamed from outside, so the session keeps its own name until you rename it there.` | — |
  | header `failed` | `Named <id> "<name>" in noc.` | `noc rename: could not rename the <provider> session: <Sync-Error header>` |
  | header absent, `not_attempted`, or anything else | `Named <id> "<name>".` | — |

  `failed` still exits 0 (the noc name is kept). Provider title mapping: `claude`→`Claude`, `codex`→`Codex`, else the raw provider string. Server side (context): the header is `not_attempted` for an empty name, `unsupported` when no renamer is registered for the provider (Claude today) or it returns `ErrNotImplemented`, `failed` with a whitespace-collapsed error bounded to 300 runes plus `…`, else `synced`.
- **Source:** `go/internal/cli/run.go` (runRename, providerTitle); `go/internal/localapi/server.go` (pushNameToProvider, boundedSyncError)
- **Decision:** D-042
- **Verified by:** `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (output contains the name; `--clear`); per-branch wording UNTESTED; MANUAL M-4.

#### CLI-030 `forget`
- **Behavior:** `noc forget [--socket] <agent-id> [<agent-id>...]`. Zero positionals MUST print `usage: noc forget <agent-id> [<agent-id>...]`, exit 2. For each ID in order, MUST send `DELETE /v1/agents/<id>` (204 on success); on success print `forgot <id>` to stdout; on failure print the CLI-008 error (including the connection hint, repeated per failing ID) to stderr and continue. All IDs share one 3 s deadline. Exit 1 if any ID failed, else 0. Forget only removes noc's row; a still-live conversation reappears on its next hook or snapshot.
- **Source:** `go/internal/cli/run.go` (runForget); `go/internal/localapi/client.go` (ForgetAgent)
- **Decision:** D-040, D-041
- **Verified by:** `go/internal/localapi/server_test.go` TestForgetAgentRemovesTheRowOverTheSocket (transport); `go/internal/cli/run_test.go` TestRunningServerProcessesHookAndExposesPreview (`forget <id> nope`), TestValidationPaths (usage); MANUAL M-4.

## Teams and spawn

A team is a name that agents share, and one of its agents may be its lead
([02-api.md](02-api.md) API-034, REG-022). The CLI puts agents in teams, lists
and filters by team, and starts new agents that join one.

#### CLI-040 The `--team` filter and the `TEAM` column
- **Behavior:** `ls`, `search` and `watch` take `--team <value>` (string, default `""`). The value MUST be empty (no filter), `none`, or a valid team name (API-034); anything else MUST print `noc <ls|search|watch>: invalid team "<value>": a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none` to stderr and exit 2, before the server is contacted. `--team <name>` keeps exactly the agents whose `team` equals the name (case-sensitive); `--team none` keeps the agents in no team. A valid name that no agent carries keeps nothing and is not an error. The filter applies to the human and the JSON output alike.

  When at least one agent in a table is in a team, the tables of `ls`, `search` and the first `watch` snapshot MUST gain a `TEAM` column after `ID`: the team's name, `<name>*` for the team's lead, and `-` for an agent in no team. When none is, the tables are exactly as without teams.
- **Source:** `go/internal/cli/teams.go` (parseTeamFilter, filterTeam, anyInTeam, teamCell); `go/internal/cli/run.go` (runList, renderAgents, runWatch); `go/internal/cli/search.go` (runSearch, renderSearchResults)
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (`ls` with and without the column, the three cell forms, `--team` a name, `none`, an unknown name, invalid values on all three commands, `search --team`). The `search` table's column and the `watch` table's column are UNTESTED.

#### CLI-041 `team` arguments
- **Behavior:** `noc team <verb> [--socket path] ...` with the verbs `set`, `clear` and `lead`. Checks, in order:
  1. No verb MUST print the usage below to stderr and exit 2.
  2. Flags after the verb are parsed (CLI-006) with the flag-set name `noc team <verb>`; the only flag is `--socket`.
  3. `set` needs a team name and then takes any number of agent IDs; `lead` needs a team name and takes at most one agent ID; `clear` takes any number of agent IDs and no name. A missing name, or more than one ID for `lead`, MUST print the usage and exit 2. Any other verb MUST print `noc team: unknown command "<verb>"` and then the usage, exit 2.
  4. A team name that is not valid (API-034) MUST print `noc team <verb>: invalid team "<name>": a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none` and exit 2.

  All of this happens before the server is contacted. The usage is exactly:
  ```text
  usage: noc team set <name> [<agent-id>...]
         noc team clear [<agent-id>...]
         noc team lead <name> [<agent-id>]

  With no agent ID, the command acts on the agent it is run inside.
  ```
- **Source:** `go/internal/cli/teams.go` (runTeam, teamUsage)
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (no verb, unknown verb, `set` with no name, `set none`, an invalid name, `lead` with no name and with two IDs). Exact usage text UNTESTED.

#### CLI-042 `team` actions and output
- **Behavior:** With no agent ID given, the command acts on the calling agent (CLI-043); when there is none it MUST print `noc team <verb>: no agent ID was given and this is not running inside an agent` and exit 2. One 3 s deadline covers the whole command.

  | Verb | For each agent ID, in order | stdout on success |
  |---|---|---|
  | `set <name>` | `PUT /v1/agents/<id>/team` with `{"team":"<name>"}`: the agent joins the team, leaving any other | `<id> is in team <name>` |
  | `clear` | the same request with `{"team":""}` | `<id> is in no team` |
  | `lead <name>` | `GET /v1/agents/<id>`; if its team is not `<name>`, print `noc: <id> is not in team <name>` and exit 1 without changing anything; else `PUT /v1/agents/<id>/team` with `{"team":"<name>","lead":true}` | `<id> leads team <name>` |

  For `set` and `clear`, a failing ID prints the CLI-008 error and the command continues with the next; the exit is 1 if any ID failed, else 0. For `lead`, any error ends the command with exit 1. What these do to a team's lead (moving teams drops the mark, a new lead replaces the old) is [02-api.md](02-api.md) REG-022. The commands change only noc's registry; nothing is sent to a provider.
- **Source:** `go/internal/cli/teams.go` (runTeam); `go/internal/localapi/client.go` (SetAgentTeam, SetTeamLead)
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestTeamCommandsListsAndFilters (every output line, `lead` outside the team, an unknown agent, no caller, and each verb acting on the caller)

#### CLI-043 The calling agent
- **Behavior:** `team` with no agent ID, and `spawn` when it records a parent, MUST work out which agent the command is running inside, taking the first of these that applies:
  1. **The Claude session.** When `CLAUDE_CODE_SESSION_ID` is set (non-blank after trimming), the agent whose provider is `claude` and whose `native_session_id` equals it, among every agent the server retains. Claude sets this variable in the commands a session runs, so it names the session nearest to the command.
  2. **`NOC_AGENT_ID`** (trimmed), when set. `noc attach` sets it for the provider it starts (ATT-012). It is used as given, without checking that the agent exists.
  3. **The provider process.** When `NOC_AGENT_ID` is not set, the nearest ancestor process of this command named `claude` or `codex` (the lineage walk the hook path uses, [06-hooks.md](06-hooks.md) PL-001), matched against the `process_id` of an agent that is not `not_live`.

  When none applies there is no calling agent. The server is asked (`GET /v1/agents?all=true`) only when step 1 or step 3 has something to match; a failure of that request is a server error (CLI-008). The Claude session wins over `NOC_AGENT_ID` because a session started by hand inside another agent's terminal inherits that agent's `NOC_AGENT_ID`, which then names the wrong agent. A background Claude session does not inherit the environment of the command that started it, so `NOC_AGENT_ID` never reaches an agent that `noc spawn` started; step 1 is what identifies it.
- **Source:** `go/internal/cli/teams.go` (callerAgentID, providerProcessID); `go/internal/proclineage/proclineage.go` (ProviderProcessID)
- **Decision:** D-076
- **Verified by:** `go/internal/cli/teams_test.go` TestCallerIsFoundByItsProviderProcess (every order case), TestSpawnStartsClaudeInTheBackground (a spawn from inside a Claude session), TestTeamCommandsListsAndFilters (`NOC_AGENT_ID`). That Claude sets `CLAUDE_CODE_SESSION_ID`, and that a background session lacks `NOC_AGENT_ID`, were observed on Claude Code 2.1.325 and are not tested.

#### CLI-044 `teams`
- **Behavior:** `noc teams` flags: `--json` (bool), `--socket`. Any positional MUST print `noc teams: unexpected positional arguments`, exit 2. It MUST read every agent the server retains (`GET /v1/agents?all=true`), live or not, and summarise them by team: one row per team name in ascending byte order, then, when at least one agent is in no team, a last row for those agents. Each row counts its agents and, of those, how many are in the `blocked`, `working`, `review` and `quiet` buckets (any other bucket value counts as quiet). A team's lead is the first agent in the server's order that is marked lead.

  Human output: with no agents at all, exactly `No agents.`. Otherwise a tabwriter table (CLI-016 parameters) with header `TEAM  LEAD  AGENTS  BLOCKED  WORKING  REVIEW  QUIET`. `TEAM` is the name, or `(none)` for the last row. `LEAD` is the lead's display name (`user_name`, else `name`), else its id when it has no name, else `-` when the team has no lead.

  `--json` MUST write a `TeamList` ([02-api.md](02-api.md) API-037) per CLI-009, with `generated_at` taken from the server's list and the same rows in the same order; the row for agents in no team has `"team": ""`, and `teams` is `[]` when there are no agents.
- **Source:** `go/internal/cli/teams.go` (runTeams, summarizeTeams, renderTeams)
- **Decision:** D-071
- **Verified by:** `go/internal/cli/teams_test.go` TestSummarizeAndRenderTeams (ordering, counts, an odd bucket, the lead's name and id, `(none)`, `No agents.`, no last row when every agent is in a team), TestTeamCommandsListsAndFilters (the table and `--json` against a server, the positional error)

#### CLI-045 `spawn` arguments and validation
- **Behavior:** `noc spawn [flags] <prompt...> [-- <provider arguments>]`. The command line is cut at the first argument that is exactly `--`: what is before it is noc's, what is after it is passed to the provider unchanged (CLI-046). Flags, parsed from noc's part with the flag-set name `noc spawn`:

  | Flag | Type | Default | Meaning |
  |---|---|---|---|
  | `--provider` | string | `claude` | The provider to start. Only `claude` is built. |
  | `--team` | string | `""` | A team for the new agent, or `none`. Empty means the calling agent's team ([02-api.md](02-api.md) HTTP-037). |
  | `--name` | string | `""` | A name for the agent (trimmed). |
  | `--dir` | string | `""` | The directory to start in; empty means the current directory. |
  | `--socket` | string | resolved socket | Server socket |

  The prompt is the positionals joined with single spaces and trimmed. Because flag parsing stops at the first positional (CLI-006), a flag written after the prompt is part of the prompt. Checks, in order, all before anything is started:
  1. An empty prompt MUST print `usage: noc spawn [--provider claude] [--team name|none] [--name name] [--dir path] <prompt> [-- <provider arguments>]`, exit 2.
  2. A `--team` that is not empty, `none`, or a valid name MUST print `noc spawn: invalid team "<value>": a team name is 1 to 48 characters from A-Za-z0-9._-, not starting with - or ., and not none`, exit 2.
  3. `--provider codex` MUST print `noc spawn: codex is not supported yet`, exit **1**. Any other provider than `claude` MUST print `noc spawn: unknown provider "<value>" (want claude)`, exit 2.
  4. The directory is made absolute; one that cannot be opened MUST print `noc: open directory "<path>": <err>`, and a path that is not a directory `noc: "<path>" is not a directory`, exit 1.
  5. The Claude executable (CFG-009) is resolved with `exec.LookPath`; failure MUST print `noc: find claude executable "<name>": <err>`, exit 1.
- **Source:** `go/internal/cli/teams.go` (runSpawn, splitProviderArguments, spawnUsage)
- **Decision:** D-076
- **Verified by:** `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground (no prompt, a flag but no prompt, only provider arguments, an invalid team, an unknown provider, codex, a missing directory, a missing executable, the default directory, and that a refused spawn registers nothing). A path that is not a directory is UNTESTED.

#### CLI-046 `spawn` sequence
- **Behavior:** After validation `spawn` MUST do the following, in this order. The noc server starts nothing: the provider is a child of this CLI process.
  1. **Ask the server.** Work out the calling agent (CLI-043). If there is one, `GET /v1/agents/<id>`: a connection error ends the command (CLI-008); any other error, such as an agent the registry no longer knows, means there is no parent. If there is none, `GET /v1/health`, so that an unreachable server ends the command before anything is started. 3 s deadline.
  2. **Start Claude.** Run `<claude> --bg --permission-mode auto [--name <name>] <prompt> <provider arguments...>` with the directory as its working directory, the CLI's environment unchanged, no stdin, and stdout and stderr captured together. `--name` is present only when a name was given. The provider arguments come last, so that one of them, such as `--permission-mode plan`, overrides what noc set. A **60 s** deadline covers this step and the next. A non-zero exit, a timeout or a failure to start MUST write the captured output to stderr, then `noc: start Claude in the background: <err>`, and exit 1; nothing is registered. This is how a refusal by Claude reaches the user, for example a directory Claude does not trust.
  3. **Find the session.** Claude chooses the session ID of a background session itself (it ignores `--session-id` with `--bg`) and prints the job's ID. `spawn` MUST read Claude's roster (ATT-004, ATT-005) and take the entry of kind `background` with a non-blank `sessionId` whose `id` occurs in the captured output as a whole word, where a word is a run of `A-Za-z0-9_-`. The read is repeated every 250 ms, for up to **10 s** from the first read, until such an entry is listed. If none is, it MUST write the captured output to stderr, then `noc: Claude started a session, but noc could not identify it, so it has no team or parent: <err>`, and exit 1: the session is running, and nothing is registered.
  4. **Register it.** `POST /v1/agents` ([02-api.md](02-api.md) HTTP-036) with provider `claude`, the roster entry's `sessionId`, the `--team` value as given, the parent's id, the name, the absolute directory, and the prompt. 3 s deadline. A failure MUST print `noc: Claude started session <job id>, but it has no team or parent:` and then the CLI-008 error, exit 1.
  5. Print the agent's noc `id` and a newline to stdout, and nothing else; exit 0. Claude's own output is not shown on success.

  The team the agent ends up in, and the one case in which the calling agent becomes a team's lead, are decided by the server (HTTP-037). `spawn` creates no worktree and runs no Git command; it sets no limit on how many agents are started.
- **Source:** `go/internal/cli/teams.go` (runSpawn, claudeSpawnArguments, startedClaudeSession, awaitClaudeSession); `go/internal/attach/claude_roster.go` (ReadClaudeRoster); `go/internal/localapi/client.go` (RegisterSpawn)
- **Decision:** D-076
- **Verified by:** `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground, with a fake Claude: the exact command line and working directory, the session ID taken from the roster, the id on stdout, team and parent from inside and outside an agent, an unknown caller, a session the hooks reported first, a provider that fails to start, and a session the roster never lists. The 60 s deadline, the health check with the server down, and a failed registration are UNTESTED. The behaviour of the real `claude --bg` (the printed job ID, the ignored `--session-id`, the untrusted-directory refusal) was observed on Claude Code 2.1.325 and is not tested.

#### CLI-047 `spawn` and the calling agent
- **Behavior:** A spawn from inside an agent MUST record that agent as the new agent's `parent_id`; a spawn from outside any agent, or from an agent the registry does not know, records none. With no `--team`, the new agent joins the calling agent's team; `--team none` puts it in no team; `--team <name>` puts it in that team. When the calling agent is in no team and names a team that has no lead, the calling agent joins that team and becomes its lead; in every other case the calling agent is unchanged. These are the server's rules ([02-api.md](02-api.md) HTTP-037), stated here for the command's user.
- **Source:** `go/internal/cli/teams.go` (runSpawn); `go/internal/localapi/teams.go` (handleSpawn)
- **Decision:** D-071, D-076
- **Verified by:** `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground; `go/internal/localapi/teams_test.go` TestSpawnRegistrationRules

## Diagnostics, setup, and hook commands (CLI surface)

#### CLI-031 `doctor` flags
- **Behavior:** Flags: `--json` (bool), `--socket` (resolved socket), `--database` (resolved DB path), `--timeout` (duration, default `15s`). Positionals MUST print `noc doctor: unexpected positional arguments`, exit 2. `--timeout <= 0` MUST print `noc doctor: timeout must be positive`, exit 2. The flag values override `Paths.SocketPath`/`Paths.DatabasePath` passed to `doctor.Run` under a context with that timeout.
- **Source:** `go/internal/cli/run.go` (runDoctor)
- **Decision:** D-029
- **Verified by:** `go/internal/cli/run_test.go` TestDoctorRejectsInvalidTimeout (timeout, positionals)

#### CLI-032 `doctor` output and exit
- **Behavior:** Human output MUST be `noc doctor: <status>` then a tabwriter with, per check, `<UPPERCASE status>  <id>  <summary>`, an optional continuation `\t\t<detail>`, and an optional `\t\tNext: <remediation>`. JSON writes the report (CLI-009). Exit 1 when `report.HasFailures()`, else 0 (warnings do not fail).
- **Source:** `go/internal/cli/run.go` (runDoctor, renderDoctorReport)
- **Decision:** D-029
- **Verified by:** UNTESTED (CLI rendering)

#### CLI-033 `setup` flags and arguments
- **Behavior:** When any argument of `noc setup` is exactly the word `skill`, the command is `noc setup skill` (CLI-048) and none of the following applies. Otherwise `setup` is about provider hooks only. Flags: `--check` (bool), `--uninstall` (bool), `--json` (bool), `--executable` (string, default `""` meaning the setup package chooses). `--check` with `--uninstall` MUST print `noc setup: --check and --uninstall are mutually exclusive`, exit 2. More than one positional MUST print `usage: noc setup [--check|--uninstall] [claude|codex]`, exit 2. A positional other than `claude`/`codex` MUST print `noc setup: unsupported provider "<arg>"`, exit 2. No positional = all providers. Action: `check` if `--check`, `uninstall` if `--uninstall`, else `install`. `setupcmd.Run` errors print `noc: <err>`, exit 1.
- **Source:** `go/internal/cli/run.go` (runSetup)
- **Decision:** D-030
- **Verified by:** `go/internal/cli/run_test.go` TestSetupRejectsConflictingActions (conflict, unsupported provider, positionals); `go/internal/cli/skill_test.go` TestSetupSkillInstallsChecksAndRemoves (plain `setup --check` is still the hook check)

#### CLI-034 `setup` output and exit
- **Behavior:** Human output MUST start with `noc setup <action> using <executable>`, then per provider result: `  <p>: error — <error>` if the result has an error; else by action — install: `  <p>: installed in <path>` (changed) or `  <p>: already configured in <path>`, followed by inspection lines when incomplete; check: `  <p>: configured` or `  <p>: incomplete` + inspection; uninstall: `  <p>: removed <n> hook handlers from <path>` or `  <p>: no managed hooks found in <path>`. For Codex install (non-error) also `    Review and trust the new hook definitions when Codex prompts you.` Inspection lines (4-space indent): `errors: a; b`, `hooks are disabled in Codex [features]`, `missing events: a, b`, `events using another executable: a, b`. Exit 1 when `report.HasFailures()`.
- **Source:** `go/internal/cli/run.go` (renderSetupReport, renderSetupInspection)
- **Decision:** D-030
- **Verified by:** UNTESTED (CLI rendering)

#### CLI-048 `setup skill` arguments
- **Behavior:** `noc setup skill [--check|--uninstall] [--json] [claude|codex]`. The first argument of `noc setup` that is exactly `skill` selects this command and is removed; it may come before or after the flags. The rest is parsed with the flag-set name `noc setup skill`: `--check` (bool), `--uninstall` (bool), `--json` (bool). Flags may also follow the provider (`noc setup skill claude --check`), which the other commands do not allow (CLI-006). Checks, in order:
  1. A positional other than `claude`/`codex` MUST print `noc setup skill: unsupported provider "<arg>"`, exit 2.
  2. `--check` with `--uninstall` MUST print `noc setup skill: --check and --uninstall are mutually exclusive`, exit 2.
  3. More than one provider MUST print `usage: noc setup skill [--check|--uninstall] [--json] [claude|codex]`, exit 2.

  No provider means both, in the order `claude`, `codex`; noc does not detect which providers are installed. The action is `check` with `--check`, `uninstall` with `--uninstall`, else `install`. The command never contacts the noc server and never runs a provider. A failure to resolve the skill files (CLI-049) MUST print `noc: <err>`, exit 1.
- **Source:** `go/internal/cli/run.go` (runSetup); `go/internal/cli/skill.go` (withoutSkillWord, runSetupSkill)
- **Decision:** D-006
- **Verified by:** `go/internal/cli/skill_test.go` TestSetupSkillInstallsChecksAndRemoves (both providers by default, one provider, a flag after the provider, `skill` after a flag, the conflict, an unsupported provider, two providers). The exact error texts are UNTESTED.

#### CLI-049 The skill file and what each action does to it
- **Behavior:** The skill is one file per provider: `<dir>/skills/noc/SKILL.md`, where `<dir>` is the trimmed `CLAUDE_CONFIG_DIR` or else `~/.claude` for Claude, and the trimmed `CODEX_HOME` or else `~/.codex` for Codex (the home directory is looked up only when one of the two variables is unset; failure is `resolve home directory: <err>`). The content is the document embedded in the binary (CLI-051), the same bytes for both providers.

  A file at that path is one of:

  | Status | The file |
  |---|---|
  | `current` | is byte-for-byte this binary's document |
  | `outdated` | differs, and contains the marker `<!-- noc-skill:` (noc installed it) |
  | `foreign` | differs, and does not contain the marker (noc did not install it) |
  | `missing` | does not exist |

  Any other read failure is the error `read skill "<path>": <err>`.

  | Action | `missing` | `outdated` | `current` | `foreign` |
  |---|---|---|---|---|
  | `install` | written | replaced | untouched | untouched; error |
  | `uninstall` | nothing | removed | removed | untouched; error |
  | `check` | nothing | nothing | nothing | nothing |

  A `foreign` file MUST never be written or removed; the error is `a skill named noc that noc did not install is already there; it was not modified`. Writing MUST create the directory with mode `0700` (parents included, when missing), write the document to a temporary file beside the target, set it to `0600`, and rename it over the target, so a reader never sees a partial file. Removing MUST delete the file and then the `skills/noc` directory when that leaves it empty; a directory that holds anything else stays. Nothing else in the provider's configuration is read or changed. After the action each provider's file is inspected again, and that status is what is reported.
- **Source:** `go/internal/skill/skill.go` (ResolvePaths, Inspect, Install, Uninstall, marker, ErrForeign); `go/internal/cli/skill.go` (runSetupSkill)
- **Decision:** D-006
- **Verified by:** `go/internal/cli/skill_test.go` TestSetupSkillInstallsChecksAndRemoves (both environment overrides, install into empty directories, file mode `0600`, a second install changes nothing, an older noc skill replaced, another's skill neither changed nor removed, the empty directory removed). The package has no tests of its own; the `~/.claude` and `~/.codex` defaults, the directory mode, a directory that is not empty, and the read and write error paths are UNTESTED. That Claude loads a personal skill from `~/.claude/skills/<name>/SKILL.md` is Claude's documented behaviour. That Codex loads `skills/noc/SKILL.md` from `CODEX_HOME` is NOT verified: the directory `~/.codex/skills` exists on the author's machine, but Codex could not be run there.

#### CLI-050 `setup skill` output and exit
- **Behavior:** Human output MUST start with `noc setup skill <action>`, then one line per provider, the first of these that applies:

  | Condition | Line |
  |---|---|
  | the result has an error | `  <p>: error — <error> (<path>)` |
  | uninstall, a file was removed | `  <p>: removed <path>` |
  | uninstall, nothing removed | `  <p>: no noc skill found at <path>` |
  | install, the file was written | `  <p>: installed in <path>` |
  | install, nothing written | `  <p>: already installed in <path>` |
  | check, `current` | `  <p>: installed` |
  | check, `outdated` | `  <p>: outdated; run noc setup skill to update <path>` |
  | check, `foreign` | `  <p>: <path> was not installed by noc` |
  | check, `missing` | `  <p>: not installed; run noc setup skill` |

  `--json` MUST write, per CLI-009, `{"schema_version": 1, "action": "<install|check|uninstall>", "results": [...]}` with one result per provider in order: `provider`, `path`, and, each omitted when empty or false, `status` (the status after the action, CLI-049), `changed` (a file was written or removed) and `error`. A result with an error has no `status` when the error came from the inspection.

  The exit is **1** when any result has an error, or when the action is `check` and any provider's status is not `current`; otherwise 0. So `install` over another's skill and `uninstall` beside another's skill exit 1, and the other provider is still handled.
- **Source:** `go/internal/cli/skill.go` (runSetupSkill, renderSkillReport, skillReport, skillResult, failed)
- **Decision:** D-006
- **Verified by:** `go/internal/cli/skill_test.go` TestSetupSkillInstallsChecksAndRemoves (every line of the table; the JSON report for one outdated provider; exit 1 on a failed check, on a foreign file during install and uninstall; exit 0 otherwise)

#### CLI-051 The skill document
- **Behavior:** The document is `go/internal/skill/SKILL.md`, embedded in the binary at build time. It MUST begin with YAML front matter giving `name: noc` and a `description` that says when to use the skill, and MUST contain the marker of CLI-049. It is written for an agent, and covers, in this order: what the registry is and that a failing `noc` command is not a reason to stop; what counts as an agent (a top-level conversation, not a subagent); finding out which agent the reader is (`noc whoami`); listing the reader's team and all teams; finding another agent by search, and which fields of a result to read; that noc delivers no messages, so the reader uses the provider's own means and addresses the other session by the name and native session ID noc reports; starting a worker with `noc spawn`; and changing a team or a name. Every command it names MUST be a command of this binary. Its text is the document itself and is not repeated here.
- **Source:** `go/internal/skill/SKILL.md`; `go/internal/skill/skill.go` (document, Document)
- **Decision:** D-006
- **Verified by:** `go/internal/cli/skill_test.go` TestSkillNamesOnlyCommandsThatExist (the front matter, the marker, and that each of ten commands the document names appears in the usage text of CLI-003). What the document says is otherwise UNTESTED; no test runs an agent with it.

#### CLI-052 `whoami`
- **Behavior:** `noc whoami` flags: `--json` (bool), `--socket`. Any positional MUST print `usage: noc whoami [--json]`, exit 2. It MUST work out the calling agent (CLI-043) and `GET /v1/agents/<id>`, under one 3 s deadline, and print that agent exactly as `show` does: the `Agent` JSON (CLI-009) with `--json`, else the human form (CLI-019). When there is no calling agent it MUST print `noc whoami: this is not running inside an agent that noc knows` to stderr and exit 1, with nothing on stdout. Server errors print per CLI-008, exit 1; in particular a `NOC_AGENT_ID` that names no agent yields `noc: noc server: agent was not found`. The command changes nothing.
- **Source:** `go/internal/cli/skill.go` (runWhoami); `go/internal/cli/teams.go` (callerAgentID)
- **Decision:** D-006
- **Verified by:** `go/internal/cli/skill_test.go` TestWhoami (outside an agent, `--json` and human output for the caller named by `NOC_AGENT_ID`, a stale `NOC_AGENT_ID`, an extra argument). The Claude-session and process paths of CLI-043 are tested there, not through `whoami`.

#### CLI-035 `hook` dispatcher
- **Behavior:** `noc hook` with no argument or a first argument other than `ingest` MUST print `usage: noc hook ingest --provider claude|codex` to stderr and exit 2. `noc hook ingest ...` MUST NOT write to stdout or stderr under any circumstances.
- **Source:** `go/internal/cli/run.go` (runHook)
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (usage); silence by `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown

#### CLI-036 `hook ingest` fail-open contract
- **Behavior:** Flags (output discarded): `--provider` (`claude`|`codex`, required), `--database` (resolved DB), `--socket` (resolved socket). It MUST exit 0 and stay silent when: path resolution fails; flags fail to parse (including `-h`); any positional is present; provider is missing/unsupported; the DB cannot be opened or host ID read within 900 ms; the payload is a cloud session (`hooks.ErrCloudSession`, dropped); enqueue fails; or anything panics (recovered). The 900 ms context starts before the SQLite open and also bounds the enqueue, but the stdin read and the process-table snapshot that happen in between are not bounded by it, so a provider that is slow to close stdin makes the enqueue fail and the event is lost (HK-005, HK-042). A payload that fails normalization MUST be enqueued as a rejected observation. After enqueue it MUST best-effort `POST /v1/hooks/wake` with a 150 ms deadline and ignore the result. It MUST work with no server running (the observation stays spooled).
- **Source:** `go/internal/cli/run.go` (runHookIngest)
- **Decision:** D-007, D-028
- **Verified by:** `go/internal/cli/run_test.go` TestHookIngestSpoolsSilentlyWhileServerIsDown, TestRunningServerProcessesHookAndExposesPreview

## Server process lifecycle

#### LIFE-001 `server` subcommand routing
- **Behavior:** Within `noc server <args>`: if `args[0]` is `status`, `stop`, or `restart`, route to that subcommand with the rest. Else if `args[0]` is `run` or `start`, drop it (alias for launch mode). Then, if an argument remains whose first character is not `-` (or is the empty string), MUST print `noc server: unknown command "<arg>"`, exit 2 (e.g. `noc server run stop` → `unknown command "stop"`). Otherwise parse launch flags (LIFE-002). Subcommand names are recognised only in first position (`noc server --bg status` → `noc server: unexpected positional arguments`, exit 2).
- **Source:** `go/internal/cli/run.go` (runServer)
- **Decision:** D-021, D-026
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (unknown subcommand, `server run stop`, subcommand after a flag); launching through the `run`/`start` aliases UNTESTED; MANUAL M-2.

#### LIFE-002 Launch flags and validation
- **Behavior:** `noc server` and `noc server restart` share these flags (`--bg` only on `server`):

  | Flag | Type | Default | Validation |
  |---|---|---|---|
  | `--socket` | string | resolved socket (CFG-003) | — |
  | `--database` | string | resolved DB (CFG-004) | — |
  | `--pid-file` | string | `""` → `dir(socket)/noc.pid` | — |
  | `--bg` | bool | false | `server` only |
  | `--log-format` | string | `text` | `text` or `json`; else `invalid log format "<v>"` |
  | `--discovery` | bool | true | — |
  | `--discovery-interval` | duration | `5s` (`server.DefaultDiscoveryInterval`) | `> 0` only when discovery enabled: `discovery interval must be positive` |
  | `--stale-after` | duration | `30s` (`server.DefaultStaleAfter`) | `> 0` only when discovery enabled: `stale-after must be positive` |
  | `--timeout` | duration | `15s` | `restart` only; `> 0`: `noc server restart: timeout must be positive` |

  Validation failures print `noc server: <msg>` (or `noc server restart: <msg>`), exit 2. Positionals print `noc server: unexpected positional arguments` / `noc server restart: unexpected positional arguments`, exit 2. When discovery is disabled, non-positive interval/stale values are accepted and passed through; `server.Run` substitutes the defaults for values `<= 0`.
- **Source:** `go/internal/cli/run.go` (serverLaunchFlags: bind, validate, options; runServer; runServerRestart); `go/internal/server/run.go` (Run)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths; MANUAL M-2.

#### LIFE-003 Foreground mode
- **Behavior:** Without `--bg`, the process MUST: build the startup notifier from the environment (LIFE-013; absent → foreground); acquire the PID record with mode `foreground` (or `background` when a notifier exists) before opening the database; on acquisition failure report via LIFE-009/CLI-008 and exit 1; create an slog handler on **stderr** (`TextHandler` for `text`, `JSONHandler` for `json`, default options/level); call `server.Run` with the socket, database, logger, readiness callback, discovery interval, stale-after, and `DisableDiscovery = !--discovery`; on `server.Run` error report it (already-running errors per LIFE-009) and exit 1; on normal return (context cancelled by SIGINT/SIGTERM) exit 0. The PID record MUST be released on every return path after acquisition (LIFE-008). Foreground mode writes nothing to stdout.
- **Source:** `go/internal/cli/run.go` (runServerForeground)
- **Decision:** D-021, D-026
- **Verified by:** `go/internal/cli/run_test.go` TestSingleBinaryServerAndDefaultClient

#### LIFE-004 PID record format
- **Behavior:** The PID record MUST be a JSON object followed by a newline, written once at creation:
  `{"pid":<int>,"socket_path":"<filepath.Clean(socket)>","started_at":"<RFC 3339 UTC with nanoseconds>","mode":"foreground"|"background"}`.
  `mode` is omitted when empty; a record without `mode` is still accepted when read (LIFE-006). The file MUST be created with mode `0600` in a directory ensured by `EnsurePrivateDir` (CFG-007). `pid` is the server process's own PID (in `--bg` mode, the detached child writes it).
- **Source:** `go/internal/cli/server_lifecycle.go` (serverPIDRecord, acquireServerPIDFile)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (PID > 1, mode `background`, socket path, file mode)

#### LIFE-005 Atomic PID acquisition
- **Behavior:** `acquireServerPIDFile(path, socket, mode)` MUST: ensure the parent directory (CFG-007; error → fail); then up to **3** attempts: `open(path, O_WRONLY|O_CREAT|O_EXCL, 0600)`; on success encode the record and close — if encode or close fails, remove the file and fail with `write server PID file "<path>": <err>` / `close server PID file "<path>": <err>`; return a release function. If the error is not "exists", fail with `create server PID file "<path>": <err>`. If it exists: read it (LIFE-006) — if it vanished in the meantime, retry; any other read/validation error MUST be returned as-is (a corrupt record blocks startup, it is not treated as stale); probe liveness (LIFE-007) — error returned as-is; if alive fail with `noc server is already running (pid <pid>)` (wraps `errServerAlreadyRunning`); if dead remove it (ignoring not-exist; other removal errors → `remove stale server PID file "<path>": <err>`) and retry. After 3 attempts fail with `acquire server PID file "<path>" after concurrent updates`. The record's socket path is not compared during acquisition.
- **Source:** `go/internal/cli/server_lifecycle.go` (acquireServerPIDFile)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/run_test.go` TestBackgroundStartRejectsExistingServerWithStopGuidance (live record rejects a second start); `go/internal/cli/server_lifecycle_unix_test.go` TestPIDRecordEdgeCases (stale record replaced, live owner rejected); corrupt record during acquisition UNTESTED; MANUAL M-5.

#### LIFE-006 PID record reading and validation
- **Behavior:** `readServerPIDFile` MUST: return the raw `os.Open` error when the file cannot be opened (so callers can test `os.ErrNotExist`); read at most 16384+1 bytes (read failure → `read server PID file "<path>": <err>`) and reject >16384 with `server PID file "<path>" exceeds 16384 bytes`; JSON-decode, failing with `decode server PID file "<path>": <err>`; reject `pid <= 1` or blank `socket_path` with `server PID file "<path>" is invalid`; reject a non-empty mode other than `foreground`/`background` with `server PID file "<path>" has invalid mode "<mode>"`; return the record with `socket_path` cleaned. Unknown JSON fields are ignored.
- **Source:** `go/internal/cli/server_lifecycle.go` (readServerPIDFile, maxPIDFileBytes)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestPIDRecordEdgeCases (corrupt, invalid mode, `pid` 1, oversized), TestBackgroundServerLifecycle (valid read); MANUAL M-5.

#### LIFE-007 Process liveness probe (macOS/Linux)
- **Behavior:** `processIsAlive(pid)` MUST return an error `invalid server process ID <pid>` for `pid <= 1`; otherwise call `kill(pid, 0)`: success → alive; `ESRCH` → not alive; `EPERM` → alive (exists, owned by another user); any other errno → error `inspect server process <pid>: <err>`. The probe does not verify that the PID belongs to a noc process.
- **Source:** `go/internal/cli/server_process_unix.go` (processIsAlive)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestPIDRecordEdgeCases (alive, gone, `pid` 1); `EPERM` branch UNTESTED.

#### LIFE-008 Owned removal of the PID record
- **Behavior:** The release function and stop cleanup MUST call `removeOwnedPIDFile(path, pid)`, which removes the file only if it still parses and its `pid` equals the owner's PID; a missing file is success; a record owned by another PID is left untouched (returns nil); a parse error is returned (and ignored by callers).
- **Source:** `go/internal/cli/server_lifecycle.go` (acquireServerPIDFile, removeOwnedPIDFile, stopServerProcess)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestPIDRecordEdgeCases (foreign owner left alone, release removes), TestServerStopStopsManagedServer (PID file gone after stop)

#### LIFE-009 Duplicate-instance rejection output
- **Behavior:** Whether detected by the PID record (LIFE-005, LIFE-011), by the socket probe (LIFE-011), by `localapi.Listen` finding a reachable socket (LIFE-010), or reported by a background child as `already-running` (LIFE-014), `noc server` and `noc server --bg` MUST print to stderr exactly:
  ```text
  noc: noc server is already running
  Stop the previous instance by running: noc server stop
  ```
  and exit 1, leaving the existing server, its socket, and its PID record untouched.
- **Source:** `go/internal/cli/run.go` (printServerLifecycleError), `go/internal/cli/server_lifecycle.go` (isServerAlreadyRunningError)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/run_test.go` TestBackgroundStartRejectsExistingServerWithStopGuidance; `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle

#### LIFE-010 Socket-level single instance
- **Behavior:** (Server-side, observable from the CLI.) When the socket path exists: if it is not a socket, fail `refusing to replace non-socket path "<path>"`; if a dial succeeds within 200 ms, fail with `localapi.ErrServerRunning` (rendered per LIFE-009); otherwise remove the stale socket. The new socket MUST be chmod `0600`. This catches a second server using a different `--pid-file` but the same socket.
- **Source:** `go/internal/localapi/socket.go` (Listen)
- **Decision:** D-019, D-026
- **Verified by:** `go/internal/localapi/server_test.go` TestListenSocketMode, TestListenRefusesNonSocketPath, TestListenReplacesStaleSocket, TestListenDetectsLiveServer

#### LIFE-011 Background pre-check
- **Behavior:** Before spawning, `--bg` MUST run `checkServerAlreadyRunning(socket, pidFile)`: if the PID record reads successfully and its PID is alive → already running (`(pid N)`); if dead → remove the record (non-not-exist removal error → `remove stale server PID file ...`); if reading fails with anything other than not-exist (corrupt/oversized/invalid record) → return that error (exit 1 with `noc: decode server PID file ...` etc.). Then probe `GET /v1/health` on the socket with a **300 ms** timeout; success → already running (no PID shown). The PID record's socket path is not compared.
- **Source:** `go/internal/cli/server_lifecycle.go` (checkServerAlreadyRunning, serverSocketReachable, serverProbeTimeout)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/run_test.go` TestBackgroundStartRejectsExistingServerWithStopGuidance; `go/internal/cli/server_lifecycle_unix_test.go` TestPIDRecordEdgeCases (corrupt record); MANUAL M-5.

#### LIFE-012 Background spawn
- **Behavior:** `--bg` MUST: resolve its own executable with `os.Executable()` (error → `resolve noc executable: <err>`); compute the log path (CFG-006), ensure its directory, open it `O_WRONLY|O_CREATE|O_APPEND` mode `0600` and then `chmod 0600` (errors → `open background server log "<p>": <err>` / `secure background server log "<p>": <err>`); open `/dev/null` for stdin; create a pipe; spawn `<exe> server --socket=<s> --database=<d> --pid-file=<pid> --log-format=<f> --discovery=<true|false> --discovery-interval=<Duration.String()> --stale-after=<Duration.String()>` (always all eight arguments, always `=` form, PID file always explicit) with stdin `/dev/null`, stdout and stderr both appended to the log, the pipe's write end as `ExtraFiles[0]` (fd 3), environment = parent environment with `NOC_INTERNAL_SERVER_READY_FD=3` (replacing any existing value), and `Setsid: true` (new session, detached from the terminal and its SIGINT). The parent closes its copy of the write end after `Start`. Start failure → `start background noc server: <err>`.
- **Source:** `go/internal/cli/server_lifecycle.go` (startBackgroundServer, backgroundServerArgs, replaceEnvironmentValue), `go/internal/cli/server_process_unix.go` (configureBackgroundCommand)
- **Decision:** D-021, D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle; `go/internal/cli/run_test.go` TestServerLaunchFlagsAndBackgroundArgs (argument list, environment replacement)

#### LIFE-013 Readiness protocol — child side
- **Behavior:** In launch mode the process MUST read `NOC_INTERNAL_SERVER_READY_FD`; if empty → no notifier (foreground). Otherwise it MUST unset the variable (so it does not leak to provider subprocesses), parse it as an integer, and ignore it if invalid or `< 3`. A valid fd makes the process a background server (PID mode `background`). The notifier sends at most one line (guarded by `sync.Once`) and then closes the fd: `ready` when `server.Run` invokes its `Ready` callback (only after the Unix listener is accepting and the HTTP server goroutine started); `already-running` if startup fails with an already-running error; `error:<message with \n replaced by spaces>` for any other startup failure (PID acquisition or `server.Run` error before readiness). Errors after `ready` send nothing further; on exit the fd is closed.
- **Source:** `go/internal/cli/server_lifecycle.go` (startupNotifierFromEnvironment, startupNotifier); `go/internal/cli/run.go` (runServerForeground); `go/internal/server/run.go` (Run)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestStartupNotifierProtocol (one line only, `already-running`, `error:`, fd < 3 ignored, variable unset), TestBackgroundServerLifecycle (ready path)

#### LIFE-014 Readiness protocol — parent side
- **Behavior:** The parent MUST read one `\n`-terminated line from the pipe (EOF with no data → `closed`), trimmed, and wait up to **45 s** (`backgroundStartTimeout`):

  | Child report | Parent action | Error text (printed `noc: ...`, exit 1) |
  |---|---|---|
  | `ready` | `Process.Release()`; succeed with child PID and log path (Release error → `release background server process: <err>`) | — |
  | `already-running` | Release; fail | LIFE-009 text |
  | `error:<detail>` | Release; fail | `background server failed to start: <detail trimmed>` |
  | EOF/other | Release; fail | `background server exited before becoming ready; see <log>` |
  | 45 s timeout | SIGTERM child, wait for it; fail | `background server did not become ready within 45s; see <log>` |
  | parent context cancelled | SIGTERM child, wait; fail | `context canceled` |
- **Source:** `go/internal/cli/server_lifecycle.go` (startBackgroundServer, backgroundStartTimeout)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (ready, duplicate); `error:`, EOF, timeout and cancellation branches UNTESTED.

#### LIFE-015 Background success output
- **Behavior:** On success `noc server --bg` MUST print to stdout exactly two lines, `noc server started in background (pid <pid>)` and `logs: <log path>`, and exit 0.
- **Source:** `go/internal/cli/run.go` (runServerBackground)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle

#### LIFE-016 Background log file
- **Behavior:** The log is `dir(pidFile)/noc.log`, append-only, mode `0600` (re-chmodded on every start), shared across restarts, never rotated or truncated by noc. It receives the child's stdout and stderr, i.e. the slog lines (text or JSON per `--log-format`, e.g. `level=INFO msg="noc server listening" socket=... database=... host_id=... version=...`) and any startup error printed by CLI-008.
- **Source:** `go/internal/cli/server_lifecycle.go` (defaultServerLogFile, startBackgroundServer)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (log path printed, user-only modes); MANUAL M-2.

#### LIFE-017 `server stop` flags
- **Behavior:** Flags: `--socket` (resolved), `--pid-file` (`""` → `dir(socket)/noc.pid`), `--timeout` (duration, default `15s`; `<= 0` → `noc server stop: timeout must be positive`, exit 2). Positionals → `noc server stop: unexpected positional arguments`, exit 2. Works for both foreground and background servers.
- **Source:** `go/internal/cli/run.go` (runServerStop)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestServerStopStopsManagedServer (foreground), TestBackgroundServerLifecycle (background); `go/internal/cli/run_test.go` TestValidationPaths (timeout)

#### LIFE-018 Stop algorithm
- **Behavior:** `stopServerProcess(socket, pidFile, timeout)` MUST:
  1. Read the record. If missing: if the socket answers health within 300 ms fail `server is reachable at "<socket>" but PID file "<pidFile>" is missing`; else fail `noc server is not running`. Other read errors are returned.
  2. If `Clean(socket) != record.socket_path` fail `server PID file targets socket "<record>", not "<requested>"`.
  3. Probe liveness; if dead, remove the owned record and fail `noc server is not running`.
  4. Send `SIGTERM` (error → `stop noc server process <pid>: <err>`).
  5. Every **50 ms** until the timeout: succeed if both the PID file and socket path (`Lstat`) are gone; else succeed (after removing the owned record) if the process no longer exists; fail `server process <pid> did not stop within <timeout>` at the deadline; return the context error if cancelled. No escalation to SIGKILL.
- **Source:** `go/internal/cli/server_lifecycle.go` (stopServerProcess), `go/internal/cli/server_process_unix.go` (signalProcessTermination)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestServerStopStopsManagedServer, TestBackgroundServerLifecycle, TestPIDRecordEdgeCases (dead process, socket mismatch); missing-record-with-reachable-socket and timeout branches UNTESTED; MANUAL M-5.

#### LIFE-019 Stop output
- **Behavior:** Success MUST print `noc server stopped (pid <pid>)` to stdout, exit 0. Every failure prints `noc: <err>` to stderr, exit 1 (including "not running", which is therefore not idempotent-success).
- **Source:** `go/internal/cli/run.go` (runServerStop)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestServerStopStopsManagedServer, TestPIDRecordEdgeCases (not running)

#### LIFE-020 `server restart` flags
- **Behavior:** `restart` MUST accept exactly these flags: `--socket`, `--database`, `--pid-file`, `--log-format`, `--discovery`, `--discovery-interval`, `--stale-after` (bound by the same `serverLaunchFlags.bind` with `includeBackground=false`, same defaults as LIFE-002), plus `--timeout` (default 15 s, used only for the stop phase). `--bg` is a parse error (exit 2). The replacement MUST be launched through `startBackgroundServer` with `values.options()` built **only from the flags given to `restart`**: every flag not given takes its default (socket/database from `ResolvePaths`, `--log-format=text`, `--discovery=true`, `--discovery-interval=5s`, `--stale-after=30s`, PID file `dir(socket)/noc.pid`). Nothing is read from the old process: the PID record stores only `pid`, `socket_path`, `started_at`, `mode` (LIFE-004), and the old server's launch flags are not persisted anywhere. All seven launch values are then forwarded explicitly to the child per LIFE-012 (`--socket=… --database=… --pid-file=… --log-format=… --discovery=<bool> --discovery-interval=… --stale-after=…`). Consequence: a server started with `noc server --bg --discovery=false` and restarted with a bare `noc server restart` comes back with provider discovery **enabled**, and will invoke the real `claude`/`codex` (or `NOC_CLAUDE_BIN`/`NOC_CODEX_BIN`). To keep a setting across restart, repeat it: `noc server restart --discovery=false`. The same applies to `--log-format`, `--discovery-interval`, `--stale-after`, and non-default `--database`/`--socket`/`--pid-file` (if the env vars that set the defaults differ between the original start and the restart, the replacement uses the restart's environment).
- **Source:** `go/internal/cli/run.go` (runServerRestart; serverLaunchFlags: bind, options); `go/internal/cli/server_lifecycle.go` (startBackgroundServer, backgroundServerArgs, serverPIDRecord)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (restart with explicit flags); `go/internal/cli/run_test.go` TestValidationPaths (`--bg` rejected); that omitted flags fall back to defaults UNTESTED.

#### LIFE-021 Restart eligibility by mode
- **Behavior:** Before stopping anything, restart MUST read the PID record: missing → `noc server is not running` (exit 1, nothing started); unreadable/invalid → that error (exit 1); mode `foreground` → `noc server is running in the foreground; stop it with Ctrl-C` (exit 1, no signal sent); mode `background` or absent → proceed.

  | PID record | Restart outcome |
  |---|---|
  | absent | exit 1 `noc server is not running` |
  | invalid/corrupt | exit 1 with read error |
  | `mode:"foreground"` | exit 1, refuse, no signal |
  | `mode:"background"` alive | stop then start background |
  | `mode` absent, alive | stop then start background |
  | background or absent mode, process dead | stop fails `noc server is not running` (record removed), nothing started, exit 1 |
- **Source:** `go/internal/cli/server_lifecycle.go` (requireRestartableServer)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestServerRestartRejectsForegroundServer; records without `mode` and dead-process records UNTESTED through `restart`.

#### LIFE-022 Restart sequence and output
- **Behavior:** Restart MUST run LIFE-018 (stop errors → `noc: <err>`, exit 1), print `noc server stopped (pid <old>)`, then run the full background start (LIFE-011..LIFE-014). Start failure MUST print `noc: server stopped but restart failed: <err>` to stderr, exit 1 (no already-running guidance line). Success MUST print `noc server restarted in background (pid <new>)` and `logs: <log>` to stdout, exit 0.
- **Source:** `go/internal/cli/run.go` (runServerRestart)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle; start-failure message UNTESTED.

#### LIFE-023 `server status` flags and JSON
- **Behavior:** Flags: `--json` (bool), `--socket` (resolved), `--pid-file` (`""` → `dir(socket)/noc.pid`). Positionals → `noc server status: unexpected positional arguments`, exit 2. It MUST `GET /v1/health` (3 s; failure per CLI-008, exit 1). With `--json` it MUST write the `Health` object only (no PID record, no registry counts) and exit 0.
- **Source:** `go/internal/cli/run.go` (runServerStatus)
- **Decision:** D-032
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (positional); `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (`server status --json` exit status); JSON shape MANUAL M-2.

#### LIFE-024 `server status` human output
- **Behavior:** Human mode MUST additionally `GET /v1/agents` (live only; error → exit 1) and read the PID record, showing it only if it parses and its socket path equals `Clean(--socket)`. Output:
  ```text
  noc server <health.status> (<health.version>)
    started: <started_at>
    host: <host_id>
    process: pid <pid> (<mode or "unknown">)        # only with a matching record

  <provider>: <status>                               # per provider, preceded by a blank line
    version: <version>                               # if non-empty
    source: <discovery_source>                       # if non-empty
    registry: <n> live agent[s][ (origin: <o> <k>, ...)]
    startup: <startup_status>[ at <time>][ — <startup_detail>]   # if startup_status non-empty
    latest: <status> at <observed_at>[ — <detail>]
    hint: start or inspect the shared daemon with `codex app-server daemon start` or `codex app-server daemon version`   # Codex only, status degraded|unavailable
  ```
  Times use local time in RFC 3339 (`2006-01-02T15:04:05-07:00`), or `unknown` for the zero time. The separator before details is ` — ` (space, U+2014, space).
- **Source:** `go/internal/cli/run.go` (runServerStatus, renderServerStatus, formatStatusTime)
- **Decision:** D-032
- **Verified by:** `go/internal/cli/run_test.go` TestRenderServerStatusIncludesStartupAndLatestDiscovery; `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (process line)

#### LIFE-025 `server status` registry line
- **Behavior:** For each provider section MUST count agents with that provider and `live_state == "live"`, grouped by `origin` (`""` → `unknown`, `reconcile` → `snapshot`), and print `  registry: <total> live agent` + `s` unless total is 1, then, if total > 0, ` (origin: <origin> <count>, ...)` with origins sorted lexically.
- **Source:** `go/internal/cli/run.go` (renderProviderRegistryStatus)
- **Decision:** D-032
- **Verified by:** `go/internal/cli/run_test.go` TestRenderServerStatusIncludesStartupAndLatestDiscovery (`registry: 1 live agent (origin: hook 1)`, mixed origins)

#### LIFE-026 Codex daemon hint
- **Behavior:** The hint line MUST appear only for provider `codex` with status `degraded` or `unavailable` (not for `disabled`, `available`, etc.).
- **Source:** `go/internal/cli/run.go` (renderServerStatus)
- **Decision:** D-024, D-032
- **Verified by:** `go/internal/cli/run_test.go` TestRenderServerStatusDoesNotSuggestDaemonWhenDiscoveryIsDisabled

#### LIFE-027 Unsupported platforms
- **Behavior:** On platforms other than darwin and linux, `configureBackgroundCommand`, `processIsAlive`, and `signalProcessTermination` MUST return `background server mode is supported only on macOS and Linux`. Consequences: `--bg`, `restart` and `stop` fail; foreground mode works only when no PID record already exists (an existing record cannot be liveness-checked and fails acquisition).
- **Source:** `go/internal/cli/server_process_unsupported.go`
- **Decision:** D-026
- **Verified by:** UNTESTED

## Configuration and paths

| Variable | Used by | Effect | Default when unset/empty |
|---|---|---|---|
| `NOC_STATE_DIR` | `ResolvePaths` | State directory | see path table |
| `XDG_STATE_HOME` | `ResolvePaths` | `$XDG_STATE_HOME/noc` state dir (all platforms, incl. macOS) | — |
| `NOC_RUNTIME_DIR` | `ResolvePaths` | Runtime directory | see path table |
| `XDG_RUNTIME_DIR` | `ResolvePaths` | `$XDG_RUNTIME_DIR/noc` runtime dir | — |
| `NOC_SOCKET` | `ResolvePaths` | Socket path (default for every `--socket`) | `<runtime>/noc.sock` |
| `NOC_DATABASE` | `ResolvePaths` | DB path (default for every `--database`) | `<state>/noc.db` |
| `HOME` (via `os.UserHomeDir`) | `ResolvePaths` | Required even when all overrides are set | error if unresolvable |
| `NOC_CLAUDE_BIN` | `attach`, `stop`, `respawn` (and server adapters) | Claude executable (trimmed) | `claude` on `PATH` |
| `NOC_CODEX_BIN` | `attach` (and server adapters) | Codex executable (trimmed) | `codex` on `PATH` |
| `NOC_INTERNAL_SERVER_READY_FD` | `server` | Readiness fd; set by `--bg` parent; unset by child | absent → foreground |
| `NOC_AGENT_ID` | set for `attach` children | Agent ID exported to the provider process | — |

| Path | Override precedence | macOS default | Linux default |
|---|---|---|---|
| State dir | `NOC_STATE_DIR` → `$XDG_STATE_HOME/noc` | `~/Library/Application Support/noc` | `~/.local/state/noc` |
| Runtime dir | `NOC_RUNTIME_DIR` → `$XDG_RUNTIME_DIR/noc` | `~/Library/Caches/noc` (`os.UserCacheDir`) | `${XDG_CACHE_HOME:-~/.cache}/noc` (`os.UserCacheDir`) |
| Socket | `--socket` → `NOC_SOCKET` | `<runtime>/noc.sock` | same |
| Database | `--database` → `NOC_DATABASE` | `<state>/noc.db` | same |
| PID record | `--pid-file` | `dir(<socket>)/noc.pid` | same |
| Background log | none | `dir(<pid record>)/noc.log` | same |

#### CFG-001 State directory resolution
- **Behavior:** `StateDir` MUST be `NOC_STATE_DIR` if non-empty; else `$XDG_STATE_HOME/noc` if `XDG_STATE_HOME` non-empty (on any OS); else on darwin `~/Library/Application Support/noc`; else `~/.local/state/noc`.
- **Source:** `go/internal/config/paths.go` (ResolvePaths)
- **Verified by:** `go/internal/config/paths_test.go` TestResolvePathsHonorsOverrides, TestResolvePathsDefaults (`XDG_STATE_HOME`); platform defaults UNTESTED.

#### CFG-002 Runtime directory resolution
- **Behavior:** `RuntimeDir` MUST be `NOC_RUNTIME_DIR` if non-empty; else `$XDG_RUNTIME_DIR/noc` if set; else `os.UserCacheDir()/noc`; a `UserCacheDir` error MUST fail resolution with `resolve user cache directory: <err>`.
- **Source:** `go/internal/config/paths.go` (ResolvePaths)
- **Verified by:** `go/internal/config/paths_test.go` TestResolvePathsHonorsOverrides, TestResolvePathsDefaults (`XDG_RUNTIME_DIR`); `UserCacheDir` fallback UNTESTED.

#### CFG-003 Socket path resolution
- **Behavior:** `SocketPath` MUST be `NOC_SOCKET` if non-empty, else `<RuntimeDir>/noc.sock`. Per-command `--socket` overrides it. noc does not check Unix-socket path length limits (≈104 bytes on macOS); long paths fail at listen/dial time.
- **Source:** `go/internal/config/paths.go` (ResolvePaths)
- **Decision:** D-019
- **Verified by:** `go/internal/config/paths_test.go` TestResolvePathsHonorsOverrides

#### CFG-004 Database path resolution
- **Behavior:** `DatabasePath` MUST be `NOC_DATABASE` if non-empty, else `<StateDir>/noc.db`. Per-command `--database` overrides it (`server`, `server restart`, `doctor`, `hook ingest`).
- **Source:** `go/internal/config/paths.go` (ResolvePaths)
- **Decision:** D-022
- **Verified by:** `go/internal/config/paths_test.go` TestResolvePathsHonorsOverrides

#### CFG-005 Path normalisation and home requirement
- **Behavior:** All four paths MUST be returned through `filepath.Clean` (relative values stay relative to the process cwd). `os.UserHomeDir()` MUST be resolved first, unconditionally; failure returns `resolve home directory: <err>` even if every override is set. An empty environment value is treated as unset.
- **Source:** `go/internal/config/paths.go` (ResolvePaths)
- **Verified by:** UNTESTED

#### CFG-006 Derived PID and log paths
- **Behavior:** Default PID record = `filepath.Join(filepath.Dir(socketPath), "noc.pid")` — derived from the **effective socket** (flag or env), not from `RuntimeDir`. Log = `filepath.Join(filepath.Dir(pidFile), "noc.log")` — derived from the effective PID path; there is no log flag or env override. Status, stop, and restart MUST derive the default PID path the same way from their own `--socket`.
- **Source:** `go/internal/cli/server_lifecycle.go` (defaultServerPIDFile, defaultServerLogFile)
- **Decision:** D-026
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle

#### CFG-007 Private directory creation
- **Behavior:** `EnsurePrivateDir(path)` MUST: if `path` exists and is a directory, return nil **without changing its permissions** (callers may deliberately use e.g. `/tmp`); if it exists and is not a directory, fail `private directory path "<p>" is not a directory`; if `Stat` fails for another reason, fail `inspect private directory "<p>": <err>`; otherwise `MkdirAll(path, 0700)` then `chmod(path, 0700)` (errors → `create private directory ...` / `secure private directory ...`). Only the leaf is explicitly chmodded. Used for the PID directory, the log directory, the socket directory, and the database directory.
- **Source:** `go/internal/config/paths.go` (EnsurePrivateDir); callers `go/internal/cli/server_lifecycle.go` (acquireServerPIDFile, startBackgroundServer), `go/internal/localapi/socket.go` (Listen), `go/internal/storage/sqlite/store.go` (Open)
- **Verified by:** `go/internal/config/paths_test.go` TestEnsurePrivateDirDoesNotChmodExistingDirectory, TestEnsurePrivateDirSecuresNewDirectory; `go/internal/localapi/server_test.go` TestListenCreatesPrivateParent

#### CFG-008 File permissions
- **Behavior:** The PID record MUST be created `0600`; the background log `0600` (forced on each start); the socket chmod `0600`; the database file chmod `0600`. SQLite's `-wal`/`-shm` side files are created by SQLite under the process umask (`0644` with the usual `022`); they rely on the `0700` directory when noc created it (DB-005 in [04-server.md](04-server.md)).
- **Source:** `go/internal/cli/server_lifecycle.go` (acquireServerPIDFile, startBackgroundServer); `go/internal/localapi/socket.go` (Listen); `go/internal/storage/sqlite/store.go` (Open)
- **Verified by:** `go/internal/cli/server_lifecycle_unix_test.go` TestBackgroundServerLifecycle (file and directory modes); `go/internal/localapi/server_test.go` TestListenSocketMode; `go/internal/storage/sqlite/store_test.go` TestStorePersistsHostAndAgents (database `0600`); side-file modes MANUAL M-2 (`ls -la` of the temp dirs).

#### CFG-009 Provider executables in the CLI
- **Behavior:** `attach` MUST start from `{Claude: "claude", Codex: "codex"}` and replace each with `strings.TrimSpace(NOC_CLAUDE_BIN)` / `strings.TrimSpace(NOC_CODEX_BIN)` when non-empty. `stop` honours only `NOC_CLAUDE_BIN`, and so does `spawn`. The final program is resolved with `exec.LookPath` (a value containing `/` is used as a path). The roster read in `attach` uses the configured name directly (Go resolves it on `PATH`); the one in `spawn` uses the resolved path.
- **Source:** `go/internal/attach/plan.go` (DefaultBinaries); `go/internal/cli/run.go` (providerBinaries, runAttach, runLifecycle); `go/internal/cli/teams.go` (runSpawn)
- **Decision:** D-025
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume (`NOC_CODEX_BIN`), TestAttachClaudeDecisionTable (`NOC_CLAUDE_BIN`); `go/internal/cli/teams_test.go` TestSpawnStartsClaudeInTheBackground (`NOC_CLAUDE_BIN` for `spawn`)

#### CFG-010 Build metadata
- **Behavior:** See CLI-004. `Version` and `Commit` are the only build-time values the binary reports; `Date` is unused.
- **Source:** `go/internal/buildinfo/buildinfo.go`
- **Verified by:** `go/internal/buildinfo/buildinfo_test.go` TestString

## Attach, stop, and provider planning

### Attach decision table

Inputs: provider; registry fields `native_session_id`, `native_short_id`, `live_state`, `capabilities.attach`, `capabilities.resume`, `workspace.path`, `frontend_bindings`; whether `claude agents --json --all` succeeded and what it lists for this `sessionId`; `--force`. "dir" = trimmed `workspace.path` (empty → inherit the CLI's cwd). All refusals go to stderr and exit 1.

| # | Provider | Registry precondition | Roster (Claude only) | `--force` | Result: argv / mode, cwd | Refusal text |
|---|---|---|---|---|---|---|
| 1 | any | `native_session_id` blank | — | any | refuse before roster | `noc: agent has no native session ID` |
| 2 | other | — | — | any | refuse | `noc: provider "<p>" does not support terminal attachment` |
| 3 | codex | `capabilities.resume` present and unsupported | — | any | refuse | `noc: agent cannot resume[: <reason>]` |
| 4 | codex | otherwise | not read | any | `codex resume <native_session_id>`, mode resume, dir | never refused (D-038) |
| 5 | claude | (registry plan would be resume) `capabilities.resume` present and unsupported, and not row 6's attach condition | — | any | refuse before roster | `noc: agent cannot resume[: <reason>]` |
| 6 | claude | any valid | listed: an entry `kind=="interactive"` with `pid > 0` | false | refuse | `noc: refusing to attach: this Claude session is already running in a terminal (pid <pid>); focus that terminal instead.` + `Re-run with --force to open it here anyway.` |
| 7 | claude | any valid | listed: an entry `kind=="background"` with non-blank `id` (any state, running or not); interactive entries ignored when `--force` | any (6 takes precedence when not forced) | `claude attach <roster id>`, mode attach, dir | — |
| 8 | claude | any valid | readable but no background entry with an id for this session (and no blocking interactive entry, or forced) | any | `claude --bg --resume <native_session_id>` in dir with `NOC_AGENT_ID`; then re-read roster; then `claude attach <job id>` in dir | restore failure: provider output, then `noc: resume Claude session in the background: <err>`; re-read failure: `noc: list Claude sessions: <err>` / `noc: decode Claude sessions: <err>`; no job: `noc: Claude resumed the session but lists no background job for it` |
| 9 | claude | `capabilities.attach` supported, `native_short_id` non-blank, `live_state=="live"` | unreadable (error/timeout/bad JSON) | any | `claude attach <native_short_id>`, mode attach, dir | — |
| 10 | claude | not row 9; `live_state=="live"` | unreadable | false | refuse (D-038) | `noc: refusing to attach: this Claude session is live and has no background attach ID, so resuming it here would start a second independent process on the same conversation rather than joining the existing one[; it appears to be open in Zellij session "<s>" \| Zellij sessions <s1>, <s2>].` + `Focus the terminal that already has it, or re-run with --force to start a second process anyway.` |
| 11 | claude | not row 9; `live_state=="live"` | unreadable | true | `claude --resume <native_session_id>` (foreground), mode resume, dir | — |
| 12 | claude | not row 9; `live_state` ≠ `live` (candidate_stale, not_live, unknown) | unreadable | any | `claude --resume <native_session_id>` (foreground), mode resume, dir | — |

After a plan is chosen (rows 4, 7–9, 11, 12) the common tail applies: executable lookup (ATT-008), workspace check (ATT-009), execution (ATT-012), exit status (ATT-013).

#### ATT-001 `attach` arguments
- **Behavior:** Flags: `--socket`, `--force` (bool, "attach even when doing so would run a second process on a live conversation"). Exactly one positional; else `usage: noc attach [--force] <agent-id>`, exit 2.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Decision:** D-038
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (flag after the positional is a usage error)

#### ATT-002 Agent lookup
- **Behavior:** `attach` MUST fetch the agent with `GET /v1/agents/<id>` under a 3 s timeout, close the client, and fail per CLI-008/CLI-011 (exit 1) before touching any provider. The server is not contacted again; attach and stop never notify the server of what they did (the registry learns through hooks/reconciliation).
- **Source:** `go/internal/cli/run.go` (lookupAgent, runAttach)
- **Decision:** D-043
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume

#### ATT-003 Registry-based plan (`PlanForAgent`)
- **Behavior:** MUST fail `agent has no native session ID` when the trimmed native session ID is empty. Directory = trimmed `workspace.path` or `""`. Claude: program = trimmed Claude binary (empty → `Claude executable is not configured`); if `capabilities.attach.supported` is true **and** trimmed `native_short_id` non-empty **and** `live_state == "live"` → `[attach, <short id>]`, mode `attach`; else if `capabilities.resume` exists with `supported:false` → `agent cannot resume: <reason>` (or `agent cannot resume` without reason); else `[--resume, <native session id>]`, mode `resume`. Codex: resume denied → same error (checked before the binary); program = trimmed Codex binary (empty → `Codex executable is not configured`); `[resume, <native session id>]`, mode `resume`. Other providers → `provider "<p>" does not support terminal attachment`. All arguments are trimmed. A missing capability counts as neither supported nor denied. Errors print `noc: <err>`, exit 1.
- **Source:** `go/internal/attach/plan.go` (PlanForAgent, capabilityState, resumeDenied); `go/internal/cli/run.go` (runAttach)
- **Decision:** D-048
- **Verified by:** `go/internal/attach/plan_test.go` TestPlanForAgentUsesNativeProviderEntryPoints; `go/internal/attach/plan_test.go` TestPlanForAgentRejectsMissingIdentityAndDeniedResume

#### ATT-004 Claude roster read
- **Behavior:** For Claude agents only, after ATT-003 succeeds, `attach` MUST run `<claude bin> agents --json --all` with a **5 s** timeout (`attach.RosterTimeout`), inheriting the CLI's cwd and environment, capturing stdout only (stderr discarded). The command also sets a 1 s `WaitDelay`, so a wrapper executable whose descendant keeps stdout open cannot hold the read past the timeout plus that second. Non-zero exit/timeout/missing binary → `list Claude sessions: <err>`; stdout that is not valid JSON decodable into an array of entry objects → `decode Claude sessions: <err>` (a JSON `null` decodes to an empty roster without error). On the first (planning) read, any roster error MUST be swallowed and the ATT-003 plan used (rows 9–12). The read is not retried.
- **Source:** `go/internal/attach/claude_roster.go` (RosterTimeout, ReadClaudeRoster); `go/internal/cli/run.go` (runAttach)
- **Decision:** D-051
- **Verified by:** `go/internal/attach/claude_roster_test.go` TestReadClaudeRoster (decode, `null`, exit status, missing binary, bad JSON); `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (fallback on roster failure); timeout UNTESTED.

#### ATT-005 Roster entry schema
- **Behavior:** Each roster entry MUST be decoded from `{"id": string, "sessionId": string, "kind": string, "state": string, "pid": int|null}`; unknown fields ignored. An entry is "running" iff `pid` is non-null and `> 0`. Matching to the agent uses `strings.TrimSpace(sessionId) == trimmed native_session_id`. `state` is decoded but not used for decisions.
- **Source:** `go/internal/attach/claude_roster.go` (ClaudeSession, running)
- **Decision:** D-051
- **Verified by:** `go/internal/attach/claude_roster_test.go` TestReadClaudeRoster, TestPlanClaudeWithRosterKeepsBackgroundSessionsInTheBackground

#### ATT-006 Roster-based plan (`PlanClaudeWithRoster`)
- **Behavior:** Iterating entries matching the session in roster order: an entry with `kind=="interactive"` that is running and `force==false` MUST immediately fail with `ErrRunningInTerminal` wrapped as `this Claude session is already running in a terminal (pid <pid>); focus that terminal instead`; an entry with `kind=="background"` and a non-blank `id` is remembered (the **last** such entry wins). If a background entry was found → `[attach, <trimmed id>]`, mode `attach`. Otherwise → `[--bg, --resume, <session id>]`, mode `background-resume` (`attach.ModeBackgroundResume`). Directory as in ATT-003. With `force==true` interactive entries are ignored. A blank native session ID fails `agent has no native session ID` (unreachable after ATT-003). In the CLI, `ErrRunningInTerminal` MUST produce the row-6 refusal (exit 1); any other error keeps the ATT-003 plan; success replaces it.
- **Source:** `go/internal/attach/claude_roster.go` (PlanClaudeWithRoster, ErrRunningInTerminal); `go/internal/cli/run.go` (runAttach)
- **Decision:** D-051, D-054
- **Verified by:** `go/internal/attach/claude_roster_test.go` TestPlanClaudeWithRosterKeepsBackgroundSessionsInTheBackground; `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (rows 6, 7, 8)

#### ATT-007 Concurrent-use guard
- **Behavior:** After planning, `ConcurrentUseRisk(agent, plan)` MUST return risky only when provider is `claude`, plan mode is exactly `resume`, and `live_state == "live"`; unless `--force`, the CLI MUST print the row-10 refusal and exit 1. Because a readable roster never yields mode `resume`, this guard only fires when the roster was unreadable. The suffix names distinct, non-blank `session` values of `frontend_bindings` with `kind=="zellij"` in first-seen order: one → `; it appears to be open in Zellij session "<s>"` (`%q`); several → `; it appears to be open in Zellij sessions a, b` (unquoted, `, `-joined); none → no suffix.
- **Source:** `go/internal/attach/plan.go` (ConcurrentUseRisk, zellijSessions); `go/internal/cli/run.go` (runAttach)
- **Decision:** D-038
- **Verified by:** `go/internal/attach/plan_test.go` TestConcurrentUseRiskOnlyBlocksLiveClaudeResume; `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (rows 10, 11)

#### ATT-008 Executable lookup
- **Behavior:** After the guards, the CLI MUST resolve `plan.Program` with `exec.LookPath`; failure → `noc: find <provider> executable "<program>": <err>`, exit 1. Note the Claude roster read happens before this check, so a missing Claude binary silently falls back to the registry plan first.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Decision:** D-025
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume (missing executable)

#### ATT-009 Workspace check
- **Behavior:** If `plan.Directory` is non-empty it MUST exist and be a directory; else `noc: open agent workspace "<dir>": <err>` or `noc: agent workspace "<dir>" is not a directory`, exit 1. Empty directory → child inherits the CLI's cwd.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume (missing workspace); not-a-directory UNTESTED.

#### ATT-010 Background restore (`--bg --resume`)
- **Behavior:** For mode `background-resume` the CLI MUST run `<resolved claude> --bg --resume <session id>` with cwd = plan directory, environment = CLI environment with `NOC_AGENT_ID=<agent id>` (replacing any existing value), no stdin, combined stdout+stderr captured, bounded only by the CLI context (no timeout and no `WaitDelay`: the CLI waits until the command and anything that inherited its output pipe have closed it). On failure it MUST write the captured output verbatim to stderr, then `noc: resume Claude session in the background: <err>`, exit 1. On success the captured output is discarded.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Decision:** D-054
- **Verified by:** `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (row 8: restore runs in the workspace with `NOC_AGENT_ID`, output discarded); failure output UNTESTED.

#### ATT-011 Restored job lookup
- **Behavior:** After a successful restore the CLI MUST read the roster once more (ATT-004, 5 s; here errors are fatal: `noc: list Claude sessions: ...`/`noc: decode Claude sessions: ...`, exit 1) and take `BackgroundJobFor(roster, trimmed session id)`: the trimmed `id` of the **first** entry with matching `sessionId` and `kind=="background"` (an entry with a blank id yields `""`). Empty → `noc: Claude resumed the session but lists no background job for it`, exit 1. There is no polling or retry loop. Otherwise the plan becomes `[attach, <job id>]`, mode `attach`, same directory.
- **Source:** `go/internal/cli/run.go` (runAttach); `go/internal/attach/claude_roster.go` (BackgroundJobFor)
- **Decision:** D-054
- **Verified by:** `go/internal/attach/claude_roster_test.go` TestBackgroundJobForFindsTheRestoredJob; `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (row 8 and its no-job failure)

#### ATT-012 Attach execution
- **Behavior:** The CLI MUST run the final plan as a child with the resolved program and `plan.Arguments`, stdin/stdout/stderr connected to the CLI's own streams, cwd = plan directory, environment = CLI environment with `NOC_AGENT_ID=<agent id>`, killed if the CLI context is cancelled. It does not `exec(2)`-replace itself.
- **Source:** `go/internal/cli/run.go` (runAttach); `go/internal/cli/server_lifecycle.go` (replaceEnvironmentValue)
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume, TestAttachClaudeDecisionTable (cwd, `NOC_AGENT_ID`, arguments)

#### ATT-013 Attach exit status
- **Behavior:** Child success → exit 0. Child exited with code N ≥ 0 → exit N (no message). Child killed by a signal (`ExitCode() == -1`) and CLI context cancelled → exit 130. Otherwise (start failure, or signal death without cancellation) → `noc: attach <provider> agent: <err>`, exit 1.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Verified by:** `go/internal/cli/run_test.go` TestAttachCodexRunsResume, TestAttachClaudeDecisionTable (child exit 7 → 7); 130 and signal death UNTESTED.

#### ATT-014 Provider plan modes are the only branching keys
- **Behavior:** `attach.Plan` MUST carry `Program`, `Arguments`, `Directory`, and `Mode` ∈ {`attach`, `resume`, `background-resume`, `stop`, `respawn`}; the CLI branches only on `Mode` (`background-resume` → ATT-010/011; `resume` → ATT-007 guard).
- **Source:** `go/internal/attach/plan.go` (Plan, Mode)
- **Verified by:** `go/internal/attach/plan_test.go` TestPlanForAgentUsesNativeProviderEntryPoints; `go/internal/attach/claude_roster_test.go` TestPlanClaudeWithRosterKeepsBackgroundSessionsInTheBackground

#### ATT-015 Codex attach
- **Behavior:** For Codex the CLI MUST NOT read any roster and MUST NOT apply the concurrent-use guard (mode `resume` but provider codex): the result is always `codex resume <thread id>` in the workspace (D-038: both TUIs mirror one app-server thread), unless resume is denied by capability.
- **Source:** `go/internal/attach/plan.go` (PlanForAgent, ConcurrentUseRisk); `go/internal/cli/run.go` (runAttach)
- **Decision:** D-038
- **Verified by:** `go/internal/attach/plan_test.go` TestPlanForAgentUsesNativeProviderEntryPoints, TestConcurrentUseRiskOnlyBlocksLiveClaudeResume; `go/internal/cli/run_test.go` TestAttachCodexRunsResume

#### ATT-016 Attach never owns or reserves sessions
- **Behavior:** `attach` MUST NOT lock, reserve, or record anything in the registry; the only provider commands it may run are `claude agents --json --all`, `claude --bg --resume <id>`, `claude attach <id>`, `claude --resume <id>`, and `codex resume <id>`.
- **Source:** `go/internal/cli/run.go` (runAttach)
- **Decision:** D-002, D-038, D-043
- **Verified by:** code inspection; UNTESTED

#### ATT-017 Workspace field trimming
- **Behavior:** Both planners MUST trim whitespace from `workspace.path`, `native_session_id`, `native_short_id`, roster `id` and `sessionId` before use; a whitespace-only value counts as absent.
- **Source:** `go/internal/attach/plan.go` (PlanForAgent, workspaceDirectory); `go/internal/attach/claude_roster.go` (PlanClaudeWithRoster, BackgroundJobFor)
- **Verified by:** UNTESTED (no whitespace cases)

### Stop

| Provider | `live_state` | `source_kind` | `native_short_id` | Result |
|---|---|---|---|---|
| codex | any | any | any | `noc: stopping Codex agents is not supported yet` |
| other | any | any | any | `noc: provider "<p>" does not support stopping agents` |
| claude | `not_live` | any | any | `noc: agent is not running` |
| claude | `live`, `candidate_stale`, `unknown`, `""` | ≠ `background` | any | `noc: only background Claude sessions can be stopped; end this one from its terminal with /exit` |
| claude | not `not_live` | `background` | blank | `noc: Claude session has no background session ID to stop` |
| claude | not `not_live` | `background` | set | run `claude stop <short id>` |

#### ATT-018 `stop` arguments and lookup
- **Behavior:** Flags: `--socket`. Exactly one positional; else `usage: noc stop <agent-id>`, exit 2. Agent lookup as ATT-002 (3 s).
- **Source:** `go/internal/cli/run.go` (runLifecycle, lookupAgent)
- **Decision:** D-043
- **Verified by:** `go/internal/cli/run_test.go` TestValidationPaths (usage); MANUAL M-7.

#### ATT-019 `StopPlan` rules
- **Behavior:** MUST follow the stop table in that precedence order (provider, then `not_live`, then source kind, then short ID, then empty Claude binary → `Claude executable is not configured`), producing `{Program: <claude>, Arguments: ["stop", <trimmed short id>], Mode: "stop"}` with no directory. Errors print `noc: <err>`, exit 1. `claude rm` is never used.
- **Source:** `go/internal/attach/stop.go` (StopPlan)
- **Decision:** D-043
- **Verified by:** `go/internal/attach/stop_test.go` TestStopPlanStopsOnlyLiveClaudeBackgroundSessions; `go/internal/cli/run_test.go` TestAttachClaudeDecisionTable (refusal for a non-background session)

#### ATT-020 Stop execution
- **Behavior:** MUST `exec.LookPath` the program (failure → `noc: find <provider> executable "<program>": <err>`, exit 1) and run it with stdout/stderr connected to the CLI's, **no stdin**, the CLI's cwd, and the CLI's environment unchanged (no `NOC_AGENT_ID`), cancelled with the CLI context.
- **Source:** `go/internal/cli/run.go` (runLifecycle)
- **Decision:** D-043
- **Verified by:** UNTESTED; MANUAL M-7.

#### ATT-021 Stop exit status
- **Behavior:** Child success → exit 0 with no noc output (the provider's own output passes through). Child exit code > 0 → that code. Any other failure (start failure, signal death) → `noc: stop <provider> agent: <err>`, exit 1.
- **Source:** `go/internal/cli/run.go` (runLifecycle)
- **Decision:** D-043
- **Verified by:** UNTESTED

### Respawn

#### ATT-022 `respawn`
- **Behavior:** `noc respawn <agent-id>` MUST behave as `stop` does (ATT-018..ATT-021) with these differences. Usage: `usage: noc respawn <agent-id>`. `RespawnPlan` follows the stop table and its precedence, producing `{Program: <claude>, Arguments: ["respawn", <trimmed short id>], Mode: "respawn"}`, with these refusals in place of the stop ones: codex → `respawning Codex agents is not supported yet`; other provider → `provider "<p>" does not support respawning agents`; not a background session → `only background Claude sessions can be respawned; restart this one from its terminal`; blank short ID → `Claude session has no background session ID to respawn`. `agent is not running` and `Claude executable is not configured` are the same. A failure that is not the child's exit status prints `noc: respawn <provider> agent: <err>`. `claude respawn --all` is never used: one command acts on one agent.
- **Source:** `go/internal/attach/stop.go` (RespawnPlan); `go/internal/cli/run.go` (runLifecycle)
- **Decision:** D-086
- **Verified by:** `go/internal/attach/stop_test.go` TestRespawnPlanRespawnsOnlyLiveClaudeBackgroundSessions; `go/internal/cli/run_test.go` TestValidationPaths (usage), TestAttachClaudeDecisionTable (refusal for a non-background session); running `claude respawn` UNTESTED

## 03 · Design choices (local)

- **Exact IDs only.** Agent references are passed through unchanged to `GET /v1/agents/<id>`; no prefix or name matching exists. This avoids ambiguity handling and treats names as display labels only.
- **The CLI re-sorts `ls` but not `watch`.** `ls` sorts by bucket then recency so the listing reads the same way as a frontend that groups by bucket; the first `watch` table is printed in server order.
- **Recency ignores `last_seen_at`** because every reconciliation refreshes it, so it says nothing about when the conversation last did anything.
- **Rename uses a longer timeout (10 s)** because the server also renames the provider session where it can, and that waits on the provider (bounded at 5 s server-side).
- **`hook ingest` is silent and always exits 0** so a provider hook never blocks or pollutes an agent conversation when noc is unavailable or an input schema changes.
- **`forget` exists** for a session that no provider roster covers and that has no recorded process ID, which therefore has no signal to retire it.
- **Readiness uses an inherited pipe fd (fd 3) named by `NOC_INTERNAL_SERVER_READY_FD`** and a one-line text protocol (`ready` / `already-running` / `error:<msg>`), not socket polling; the child unsets the variable so provider subprocesses never inherit it.
- **The background child always receives explicit `--pid-file`** and every launch flag, so it never re-derives paths differently from its parent.
- **Corrupt PID records are fatal, not stale.** Only a well-formed record whose process the OS reports as gone (`ESRCH`) is removed; `EPERM` counts as alive. This errs toward refusing to start rather than deleting another process's record.
- **Stop success = files gone or process gone**, polled every 50 ms; no SIGKILL escalation, so a hung server is reported rather than killed.
- **Restart passes only its own flags** to the replacement; nothing is read back from the old server.
- **`EnsurePrivateDir` never chmods an existing directory.** The caller can intentionally place a socket in an existing directory such as `/tmp`; noc never changes permissions on a directory it did not create.
- **Attach asks Claude at attach time, and falls back silently.** If Claude cannot be asked, the registry-based plan stands. The roster read is bounded at 5 s (plus a 1 s `WaitDelay`) so a hung provider cannot hang attach.
- **Attach runs the provider as a child, not via exec(2)**, so it can set `NOC_AGENT_ID` and map exit codes (including 130 on interrupt).
- **Spawn starts the provider first and registers second.** `claude --bg` picks the session ID, so there is nothing to register until the roster lists the job. The cost is a session that can exist without its team when the roster or the server fails in between; the command says so instead of guessing (CLI-046).
- **The job is matched by the ID Claude printed, not by the format of the line it printed it in.** Any word of the output that is the `id` of a background roster entry identifies the session, so a change in Claude's wording does not break spawn.
- **The Claude session outranks `NOC_AGENT_ID`** when working out the calling agent, because the variable is inherited by everything started in an attached terminal while the session ID is set by the session itself (CLI-043).
- **The skill is one embedded document and one marker.** The same bytes go to both providers, so there is nothing to keep in step; the marker is how noc tells its own file from someone else's without keeping a record of what it installed (CLI-049).
- **`setup skill` is separate from `setup`.** Hooks are what make the registry work and are safe to install silently; a skill changes what agents do, so it is a step the user takes on purpose. Plain `noc setup` never writes a skill.
- **`noc teams` is computed in the CLI** from the agent list, like the bucket filter, so the server has no team route.
- **Stop is Claude-background only** because a terminal session is ended with `/exit` and Codex has no equivalent stop; D-043 in `docs/DECISIONS.md` records the product decision.
- **Respawn shares stop's planner and runner** (ATT-022): both are one provider command on one live background session, so the rules cannot drift apart. D-086 records why it exists.

## 03 · Verification procedures

### Automated

```sh
cd go
go test -count=1 ./internal/cli/... ./internal/attach/... ./internal/config/...
go test -list . ./internal/cli/ ./internal/attach/ ./internal/config/
# neighbours relied on by this section
go test -count=1 ./internal/localapi/...
```

`go/internal/cli/server_lifecycle_unix_test.go` runs a real detached child by re-executing the test binary (`TestBackgroundServerHelper`) and only builds on darwin/linux.

### Manual (isolated; never touches the real registry or providers)

Set up once per shell. Every command below MUST be run with these variables. Two independent guards keep the procedures away from real providers:

1. **Every** `noc server` start — foreground, `--bg`, and **every `noc server restart`** — MUST pass `--discovery=false`. Restart does not inherit the original server's flags (LIFE-020); a bare `noc server restart` re-enables discovery.
2. `NOC_CLAUDE_BIN` and `NOC_CODEX_BIN` are exported to `/usr/bin/false` from the start, so even a server accidentally launched with discovery enabled (or a stray `attach`/`stop`) can only run a binary that exits 1. The background child inherits this environment (LIFE-012). M-7 later replaces `NOC_CLAUDE_BIN` with a scripted fake.

Keep `$T` short (under `/tmp`) so the socket path stays below the macOS Unix-socket limit.

```sh
T=$(mktemp -d /tmp/noc-spec-XXXX)
(cd go && go build -o "$T/noc" ./cmd/noc)
export NOC_STATE_DIR="$T/state" NOC_RUNTIME_DIR="$T/run"
export NOC_CLAUDE_BIN=/usr/bin/false NOC_CODEX_BIN=/usr/bin/false
unset NOC_SOCKET NOC_DATABASE NOC_AGENT_ID
M="$T/noc"
```

Sanity check after any start or restart: `$M server status` MUST show `claude: disabled` and `codex: disabled`. If either shows another status, stop the server immediately (`$M server stop`) and restart it with `--discovery=false`.

**M-1 Server down.** `$M ls; echo $?` → stderr `noc: connect to noc server: Get "http://noc/v1/agents": dial unix $T/run/noc.sock: connect: no such file or directory` and `Is the server running? Start it with: noc server`; exit 1. `$M bogus; echo $?` → `noc: unknown command "bogus"` + usage on stderr, exit 2. `$M version` → `dev`.

**M-2 Background lifecycle.**
1. `$M server --bg --discovery=false` → `noc server started in background (pid N)` / `logs: $T/run/noc.log`, exit 0.
2. `cat $T/run/noc.pid` → `{"pid":N,"socket_path":"$T/run/noc.sock","started_at":"...Z","mode":"background"}`. `ls -la $T/run $T/state` → dirs `drwx------`; `noc.pid`, `noc.log`, `noc.db` `-rw-------`; socket `srw-------`.
3. `$M server --bg --discovery=false; echo $?` and `$M server --discovery=false; echo $?` → both `noc: noc server is already running` / `Stop the previous instance by running: noc server stop`, exit 1.
4. `$M server status` → first line `noc server ok (dev)`, `  process: pid N (background)`, a `claude: disabled` and `codex: disabled` section each with `  registry: 0 live agents` and `startup:`/`latest: disabled at <RFC3339> — provider discovery disabled`, and no Codex hint. `$M server status --json` → `Health` JSON with `"version": "dev"`, `"commit": "none"`.
5. Validation (each rejected before anything starts; `--discovery=false` is still passed wherever a start could otherwise happen): `$M server bogus` → `noc server: unknown command "bogus"` (2); `$M server run stop` → `unknown command "stop"` (2); `$M server --discovery=false --bg status` → `noc server: unexpected positional arguments` (2); `$M server --discovery=false --log-format=xml` → `noc server: invalid log format "xml"` (2); `$M server --discovery-interval=0` → `noc server: discovery interval must be positive` (2) (validation only fires with discovery enabled, and it exits before starting); `$M server restart --discovery=false --bg` → `flag provided but not defined: -bg` (2); `$M server restart --discovery=false --timeout=0` → `noc server restart: timeout must be positive` (2).
6. `$M server restart --discovery=false --timeout=3s` → `noc server stopped (pid N)`, `noc server restarted in background (pid M)`, `logs: ...`; new PID in the record. `$M server status` still shows both providers `disabled`. (Do **not** run a bare `$M server restart` here: the replacement would have discovery enabled, LIFE-020.)
7. `$M server stop` → `noc server stopped (pid M)`; `ls $T/run` shows only `noc.log`. Again: `noc: noc server is not running`, exit 1. `$M server restart --discovery=false` → same message, exit 1 (nothing is started).

**M-3 Read commands** (server running from M-2 step 1). `$M ls` → `No live agents.`; `$M ls --json` → `{"schema_version": 1, "generated_at": "...", "agents": []}` indented; `$M ls --bucket bogus` → `noc ls: unknown bucket "bogus" (want blocked, working, review, or quiet)` (2); `$M show nope` → `noc: noc server: agent was not found` (1); `$M events` → `No events found.`; `$M events --limit 0` → `noc events: limit must be between 1 and 200` (2); `$M events --provider x` → `noc events: provider must be claude or codex` (2); `$M events a b` → usage (2).

**M-4 Create a hook-only Claude agent, rename, forget.**
```sh
mkdir -p "$T/work"
env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID -u CLAUDE_CODE_REMOTE \
  "$M" hook ingest --provider claude <<EOF
{"session_id":"11111111-2222-3333-4444-555555555555","hook_event_name":"SessionStart","cwd":"$T/work","source":"startup"}
EOF
echo $?            # 0, no output
sleep 1; $M ls     # one row: <uuid>  claude  review  ready  $T/work  -
ID=$($M ls --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["agents"][0]["id"])')
```
`$M rename $ID My agent` → `Named <ID> "My agent" in noc. Claude cannot be renamed from outside, so the session keeps its own name until you rename it there.` (0). `$M rename $ID "   "` → `noc rename: name cannot be empty; use --clear to remove it` (2). `$M rename --clear $ID` → `Cleared the name for <ID>.` (0). `$M rename --clear $ID extra` → usage (2). A 121-character name → `noc: noc server: agent name must be at most 120 characters` (1). Defer `forget` until after M-7.

**M-5 PID record edge cases** (server stopped). `echo 'not json' > $T/run/noc.pid; $M server --bg --discovery=false` → `noc: decode server PID file "$T/run/noc.pid": invalid character ...` (1); `$M server stop` → same (1). `echo '{"pid":999999,"socket_path":"'$T'/run/noc.sock","mode":"background"}' > $T/run/noc.pid; $M server stop` → `noc: noc server is not running` (1) and the record is removed. A record with `"mode":"weird"` → `noc: server PID file ".../noc.pid" has invalid mode "weird"` (1). Remove the file before continuing.

**M-6 Foreground.** In a second terminal with the same exports: `$M server --discovery=false` (logs to stderr, e.g. `level=INFO msg="noc server listening" ...`). From the first terminal: `cat $T/run/noc.pid` shows `"mode":"foreground"`; `$M server restart --discovery=false` → `noc: noc server is running in the foreground; stop it with Ctrl-C` (1) and the server keeps running. Ctrl-C in the second terminal → exit 0; socket and PID file removed. (Alternatively `$M server stop` stops it.)

**M-7 Attach/stop decision table with a fake Claude** (server from M-2 running, agent from M-4 present).
```sh
cat > "$T/fake-claude" <<'EOF'
#!/bin/sh
echo "$PWD :: NOC_AGENT_ID=$NOC_AGENT_ID :: $*" >> "$FAKE_LOG"
case "$1" in
  agents) [ -n "$FAKE_ROSTER_FAIL" ] && exit 1; cat "$FAKE_ROSTER" ;;
  --bg) cp "$FAKE_ROSTER_AFTER" "$FAKE_ROSTER"; echo restored ;;
  attach) echo "attached $2"; exit "${FAKE_ATTACH_EXIT:-0}" ;;
  stop) echo "stopped $2" ;;
  respawn) echo "respawned $2" ;;
esac
EOF
chmod +x "$T/fake-claude"
export NOC_CLAUDE_BIN="$T/fake-claude" FAKE_LOG="$T/fake.log" \
  FAKE_ROSTER="$T/roster.json" FAKE_ROSTER_AFTER="$T/roster-after.json"
SID=11111111-2222-3333-4444-555555555555
```
| Step | Setup | Command | Expected |
|---|---|---|---|
| a | `FAKE_ROSTER_FAIL=1` | `$M attach $ID` | row 10 refusal (hook-only agent is live with no short ID), exit 1 |
| b | roster `[{"id":"","sessionId":"$SID","kind":"interactive","state":"working","pid":4242}]` | `$M attach $ID` | `noc: refusing to attach: this Claude session is already running in a terminal (pid 4242); focus that terminal instead.` / `Re-run with --force to open it here anyway.`, exit 1 |
| c | roster `[{"id":"ab12cd34","sessionId":"$SID","kind":"background","state":"done","pid":null}]` | `$M attach $ID` | `attached ab12cd34`, exit 0; log line cwd `$T/work` (may show as `/private/tmp/...`), `NOC_AGENT_ID=$ID`, args `attach ab12cd34` |
| d | roster `[]`, roster-after `[{"id":"ef56ab78","sessionId":"$SID","kind":"background","state":"working","pid":999}]` | `$M attach $ID` | log shows `--bg --resume $SID` in `$T/work` then `attach ef56ab78`; stdout `attached ef56ab78` (the `restored` text is not shown); exit 0 |
| e | roster `[]`, roster-after `[]` | `$M attach $ID` | `noc: Claude resumed the session but lists no background job for it`, exit 1 |
| f | as c, `FAKE_ATTACH_EXIT=7` | `$M attach $ID` | exit 7 |
| g | — | `$M attach $ID --force` | `usage: noc attach [--force] <agent-id>`, exit 2 |
| h | — | `$M stop $ID` | `noc: only background Claude sessions can be stopped; end this one from its terminal with /exit`, exit 1 |
| i | — | `$M respawn $ID` | `noc: only background Claude sessions can be respawned; restart this one from its terminal`, exit 1 |

Then `$M forget $ID nope` → stdout `forgot <ID>`, stderr `noc: noc server: agent was not found`, exit 1. Finally `$M server stop` and `trash "$T"` (or remove it manually).

## 03 · Coverage gaps

- No CLI-level tests for: flag defaults; `ls` table layout; `watch` human streaming, `watch --json` output and termination; `rename` wording per provider-sync result; `doctor`/`setup` human rendering; the 3 s/10 s request timeouts.
- Lifecycle branches without tests: a corrupt record met during foreground PID acquisition; `EPERM` handling; stop with a missing PID file but a reachable socket; stop timeout; the parent's readiness `error:`/EOF/45 s timeout/context-cancel branches; restart of a record without `mode`; the restart start-failure message; unsupported platforms. No test pins that restart does **not** inherit the old server's flags (LIFE-020): the lifecycle test passes `--discovery=false` to restart explicitly, so a change in either direction would go unnoticed.
- `runAttach`: untested are `--force` over a running interactive roster entry, the output written when the background restore fails, a workspace path that is not a directory, the roster timeout, and the 130 mapping.
- `runLifecycle` (`stop`, `respawn`): only the refusal path is exercised through the CLI; executing `claude stop` or `claude respawn` and propagating its exit status are untested. `StopPlan` and `RespawnPlan` themselves are unit-tested.
- Teams: the exact `team` usage text; the `TEAM` column in the `search` and `watch` tables; `watch --team` on a live stream; the `Parent:` line of `show`; a bare `team:` search term.
- `runSpawn`: only a fake Claude is used, so the real `claude --bg` output, the ignored `--session-id`, the untrusted-directory refusal, and `CLAUDE_CODE_SESSION_ID` in a real session are untested (observed by hand on Claude Code 2.1.325); also untested are the 60 s start deadline, the health check when the server is down, a registration that fails after the session started, and `--dir` naming a file.
- Skill: `go/internal/skill` has no tests of its own; through the CLI test the untested parts are the home-directory defaults, the directory mode, a skill directory that holds other files, and read and write failures (CLI-048..CLI-050). Whether either provider actually loads the installed file is not tested, and for Codex not verified at all (CLI-049). `whoami` is tested only with `NOC_AGENT_ID` (CLI-052).
- `ResolvePaths`: platform defaults (macOS/Linux), the `UserCacheDir` fallback and home-directory failure are untested; overrides and `XDG_*` are covered.

## Known limitations

- `server restart` configures the replacement only from its own flags and environment. Nothing is read back from the old server, so a setting such as `--discovery=false` must be repeated or it reverts to the default (LIFE-020; `go/internal/cli/run.go` runServerRestart).
- When `claude agents --json --all` cannot be read, attach falls back to the registry-based plan, which for a session without a usable attach ID is a foreground `claude --resume`; a live session is refused unless `--force` (decision-table rows 9–12; `go/internal/cli/run.go` runAttach, `go/internal/attach/plan.go` PlanForAgent).
- After `claude --bg --resume`, the roster is read once. A provider that registers the job asynchronously produces `Claude resumed the session but lists no background job for it` (ATT-011; `go/internal/cli/run.go` runAttach).
- The background restore command has no timeout and no `WaitDelay`; it is bounded only by Ctrl-C (ATT-010; `go/internal/cli/run.go` runAttach).
- The server liveness probe does not check that a PID belongs to a noc process. After a crash and PID reuse, `server stop` can send `SIGTERM` to an unrelated process of the same user when the record's socket path matches, and a start can be refused as already running (LIFE-007, LIFE-018; `go/internal/cli/server_process_unix.go` processIsAlive).
- `server stop` never escalates to `SIGKILL`; a hung server is reported after the timeout and left running (LIFE-018; `go/internal/cli/server_lifecycle.go` stopServerProcess).
- `hook ingest` loses the event when the provider takes longer than about 900 ms to deliver the payload on stdin, because the enqueue shares the deadline that started before the read (CLI-036; `go/internal/cli/run.go` runHookIngest).
- File privacy depends on noc having created the directory: an existing directory is never tightened, and SQLite's `-wal`/`-shm` side files follow the umask (CFG-007, CFG-008; `go/internal/config/paths.go` EnsurePrivateDir).
- Unix-socket path length is not checked; an over-long path fails at listen or dial time with the operating system's error (CFG-003).
- Agent references are exact IDs only; a prefix or a name never resolves (CLI-011).
- A file that contains the text `<!-- noc-skill:` anywhere counts as noc's own, whoever wrote it, and is replaced by `setup skill` and removed by `setup skill --uninstall` (CLI-049; `go/internal/skill/skill.go` Inspect).
- Any argument of `noc setup` that is exactly `skill` selects `setup skill`, including a flag's value: `noc setup --executable skill` is read as `noc setup skill --executable` and fails on the unknown flag (CLI-033, CLI-048; `go/internal/cli/skill.go` withoutSkillWord).
- The Codex skill location is unverified: noc writes `$CODEX_HOME/skills/noc/SKILL.md`, and whether Codex loads it from there has not been checked against a running Codex (CLI-049).
- `spawn` starts Claude only; `--provider codex` is refused (CLI-045; `go/internal/cli/teams.go` runSpawn).
- A spawned session can be left without its team and parent: when Claude starts it but the roster does not list the job within 10 s, or the registration fails, the session keeps running, appears in the registry through its hooks or the next snapshot like any other session, and `spawn` exits 1. `noc team set` then puts it in a team; nothing can add the parent afterwards (CLI-046; `go/internal/cli/teams.go` runSpawn).
- `spawn` identifies the job by a word of Claude's output that equals the `id` of a background roster entry. Output that happened to contain the ID of a different background job would match that job instead (CLI-046; `go/internal/cli/teams.go` startedClaudeSession).
- `spawn` passes `--permission-mode auto` unless a provider argument overrides it, and sets no quota: an agent that spawns agents can start as many as it asks for (CLI-046).
- The calling agent is found from `NOC_AGENT_ID` without checking that the agent exists; `team` then reports the server's `agent was not found`. A Codex session is found only through `NOC_AGENT_ID` or its process ID, and a background Claude session only through `CLAUDE_CODE_SESSION_ID` (CLI-043; `go/internal/cli/teams.go` callerAgentID).
- `team lead` reads the agent and then marks it; a team change between the two requests is caught by the server's own check, which answers `noc: noc server: <id> is not in team <name>` (CLI-042).
