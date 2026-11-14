# 10. Remote nodes

> **Status: planned.** Nothing in this file is built. It specifies how `noc`
> will show and drive agents that run on other machines, so that the work
> can be done in slices against a fixed target. Every requirement's `Source`
> is `not built`, and `Verified by` names the evidence the implementation
> must bring. When a slice lands, its requirements move into the file that
> owns the component (02 to 08) and leave this one.

## Scope

A developer has agents on more than one machine: a laptop and one or more
remote hosts. Each machine runs its own `noc server` beside its providers,
exactly as today. One of them, the machine the developer sits at, also
gathers the others' agents, so that one `noc ls`, one `noc search` and one
Zellij sidebar cover every machine.

```text
                    ┌ local machine ───────────────────────────────────┐
 zellij sidebar ───▶│ noc (CLI) ──▶ noc server ── local providers      │
 (one host session, │                 │  hub: merges remote nodes      │
  one inner session)│                 ├── ssh ──▶ noc server on devbox │── its providers
                    │                 └── ssh ──▶ noc server on gpu-1  │── its providers
                    └──────────────────────────────────────────────────┘
```

What does not change: there is one host session and one inner session
([08-zellij-host.md](08-zellij-host.md) ZX-001), the plugin reads only the
`noc` CLI, and a machine with no remotes behaves exactly as it does now.

## Terms

| Term | Meaning |
|---|---|
| node | A machine's `noc server` and the agents it owns. Identified by its `host_id` |
| local node | The node the CLI's socket belongs to |
| remote node | A node reached over SSH |
| hub | A node that has remote nodes. Not a separate program or mode: any server becomes one when it is given a remote |
| host | The SSH alias a remote node is reached by, for example `devbox`. The local node's host is `local` |
| configured host | A host named in the configuration file. The hub connects to it at every start |
| session host | A host added to a running server. It lasts until that server stops |
| owning node | The node whose providers run an agent. The only writer of that agent's state |

Requirement prefixes: `NODE` (the hub and its links), `NCLI` (configuration
and commands), `ZN` (the Zellij plugin).

---

## 1. Nodes and the hub

#### NODE-001 Every server is a node; a hub is a node with remotes
- **Behavior:** A `noc server` with no remote hosts MUST behave exactly as specified in [04-server.md](04-server.md). A server with one or more hosts additionally keeps a link to each (NODE-004) and serves their agents alongside its own. A remote node needs no configuration and does not know it is being read: it MUST NOT be required to run a different command, listen on a port, or hold credentials for the hub.
- **Source:** not built
- **Decision:** D-012, D-066
- **Verified by:** planned: the existing gates pass unchanged with no hosts configured

#### NODE-002 Two kinds of host
- **Behavior:** The hub's hosts are the union of the configured hosts (NCLI-001), read once at start, and the session hosts added since start (NCLI-003). At start the hub MUST connect to every configured host and to no other. A session host MUST NOT survive a restart: it is not written anywhere, and its agents are dropped from the hub when the server stops. A host named both ways is a configured host.
- **Source:** not built
- **Decision:** D-067
- **Verified by:** planned: a server restarted after `noc nodes add` lists only the configured hosts

#### NODE-003 Host state
- **Behavior:** The hub MUST hold, for each host, exactly one state:

  | State | Meaning |
  |---|---|
  | `connecting` | A link is being opened and no snapshot has been applied since the link last dropped |
  | `connected` | The link is up, the remote's API is compatible, and a snapshot has been applied |
  | `unreachable` | The link cannot be opened or has dropped: SSH failed, or the remote has no `noc`, or its server is not running |
  | `incompatible` | The link opens, but the remote's API schema version differs from the hub's (NODE-006) |

  and with it: the host's alias, its origin (`configured` or `session`), the remote's `host_id`, `noc` version and commit once known, the time the state was entered, the time it was last `connected`, and, for `unreachable` and `incompatible`, one line saying why. The local node is always reported, with state `local`.
- **Source:** not built
- **Verified by:** planned: unit tests driving a link through each transition

#### NODE-004 The link is SSH stdio
- **Behavior:** The hub MUST reach a host by running `ssh -o BatchMode=yes -o ConnectTimeout=10 <alias> noc api stdio` and speaking the HTTP API of [02-api.md](02-api.md) over that process's standard input and output. `noc api stdio` (new, run on the remote) MUST connect its stdio to the remote server's Unix socket and do nothing else. The hub MUST NOT open a listening port, MUST NOT start or install anything on the remote, and MUST NOT prompt: authentication is whatever the user's SSH configuration provides non-interactively. A remote without a running server yields `unreachable` with the reason `noc server is not running on <alias>`; a remote without `noc`, `noc is not installed on <alias>`.
- **Source:** not built
- **Decision:** D-066
- **Verified by:** planned: an integration test with `ssh` replaced by a local command that runs `noc api stdio` against a second isolated server

#### NODE-005 Snapshot, then a stream, then retry
- **Behavior:** On opening a link the hub MUST read the remote's health, then its full agent list (retained agents included), apply it, and then follow the remote's watch stream to stay current. When a link drops, the host becomes `unreachable` and the hub MUST retry, waiting `5` s after the first failure and doubling up to `60` s, for as long as the host is known. Each successful connect starts from a fresh full snapshot; the hub does not replay missed events.
- **Source:** not built
- **Verified by:** planned: an integration test that stops and restarts the remote server and observes `unreachable` then `connected` with the remote's current agents

#### NODE-006 Compatibility and version skew
- **Behavior:** The hub MUST compare the remote health's `schema_version` with its own. If they differ the host is `incompatible`, with the reason `<alias> speaks API version <n>, this noc speaks <m>`, and no agents are read from it. If they agree but the `noc` version strings differ, the host is `connected` and is reported with `version_skew` true. The hub MUST NOT refuse a host for a version difference alone.
- **Source:** not built
- **Verified by:** planned: unit tests with a fake remote health

#### NODE-007 The merged registry
- **Behavior:** The hub MUST serve, from one list, its own agents and the agents last read from every known host. A remote agent is the remote's `Agent` unchanged: its `id` and `host_id` are the owning node's, and the hub MUST NOT rewrite its state, bucket, names or timestamps. When a host is not `connected`, its agents MUST stay in the list as last read; the hub does not mark them, because the host's state (NODE-003) already says they may be out of date. The agents of a configured host are kept across hub restarts and served, with the host `connecting` or `unreachable`, until a new snapshot replaces them. Removing a host (NCLI-004) removes its agents.
- **Source:** not built
- **Decision:** D-068
- **Verified by:** planned: an integration test that lists remote agents, kills the link, and lists them again unchanged with the host `unreachable`

#### NODE-008 Remotes never slow or break the local node
- **Behavior:** Each link MUST run independently of local discovery, hook ingest, and every other link. A request to the hub MUST be answered from what the hub already holds and MUST NOT wait on any remote. With every host unreachable, every local behavior specified in files 02 to 06 MUST be unaffected.
- **Source:** not built
- **Decision:** D-066
- **Verified by:** planned: the isolated smoke test passes with a configured host that never answers

#### NODE-009 What crosses the link
- **Behavior:** Only the public API model crosses a link: health, agents, events, and the commands of NCLI-007. Provider credentials, provider-native state, hook payloads and the remote's database MUST NOT. An agent's workspace path is part of the model and does cross; the hub MUST treat it as a label and MUST NOT use it as a local path.
- **Source:** not built
- **Verified by:** planned: review of the link's request set; a test that a remote agent's path is never passed as a local working directory

---

## 2. Configuration and commands

#### NCLI-001 The configuration file
- **Behavior:** Configured hosts live in `nodes.toml` in the configuration directory: `NOC_CONFIG_DIR`, else `$XDG_CONFIG_HOME/noc`, else `~/.config/noc`. The file is TOML with one `[[hosts]]` table per host, each with an `alias` string:

  ```toml
  [[hosts]]
  alias = "devbox"

  [[hosts]]
  alias = "gpu-1"   # only up on weekdays
  ```

  Unknown keys are ignored. It is parsed with one pure-Go TOML module, the only dependency this adds. A missing file means no configured hosts. A file that does not parse, or names an invalid alias (NCLI-003), MUST NOT stop the server: the server starts with no configured hosts and reports the problem in `noc nodes` and `noc doctor`. The file is read at server start only. It is the user's: hand edits are supported, and the only command that writes it is `noc nodes … --save`.
- **Source:** not built
- **Decision:** D-067
- **Verified by:** planned: unit tests for path resolution, parsing, and the failure cases

#### NCLI-002 `noc nodes`
- **Behavior:** `noc nodes [--json]` MUST list every node the server knows, the local one first, then hosts by alias. The table has the columns `HOST`, `ORIGIN` (`local`, `configured`, `session`), `STATE` (NODE-003), `VERSION` (followed by `*` when `version_skew`), `AGENTS` (how many of the host's agents the hub holds), `SINCE` (how long it has been in this state), and `DETAIL` (the reason line, or `-`). `--json` writes `{"schema_version": 1, "generated_at": …, "nodes": [ … ]}` with one object per node carrying every field of NODE-003 plus `version_skew` and `agents`.
- **Source:** not built
- **Verified by:** planned: a unit test of the table and JSON from fixed node records

#### NCLI-003 Adding a host
- **Behavior:** `noc nodes add [--save] <alias>` MUST add a session host to the running server, which connects to it at once. An alias is 1 to 64 characters from `A-Za-z0-9._-`, not starting with `-`, and not `local`; anything else is refused with exit 2 before contacting the server. Adding a known host is not an error and changes nothing. With `--save` the host is also written to `nodes.toml` by appending a `[[hosts]]` table (the file is created if absent, and its existing lines, comments included, are left as they are), which makes it a configured host from the next start. The command returns as soon as the host is recorded and prints its state; it does not wait for the connection.
- **Source:** not built
- **Decision:** D-067
- **Verified by:** planned: an integration test covering add, add again, `--save`, and a refused alias

#### NCLI-004 Removing a host
- **Behavior:** `noc nodes remove [--save] <alias>` MUST close the host's link and drop its agents from the running server. With `--save` it also removes that host's `[[hosts]]` table from `nodes.toml`, leaving the other lines as they are. Without `--save`, removing a configured host lasts only until the next start, and the command says so. An unknown alias exits 1 with `noc: no host named <alias>`.
- **Source:** not built
- **Verified by:** planned: an integration test covering both forms

#### NCLI-005 Listing covers every node and reports link state
- **Behavior:** `noc ls`, `noc search`, `noc watch` and `noc show` MUST cover the merged registry (NODE-007). They MUST NOT hide, reorder or alter agents because their host is not connected: reporting the state of the links is all `noc` does, and what to do with it is the frontend's choice (ZN-002).
  - `--host <alias>` (or `local`) on `ls`, `search` and `watch` keeps one node's agents. An unknown alias exits 2.
  - When the server knows any host, the tables gain a `HOST` column after `ID`, and after the table one line per host that is not `connected`: `<alias>: <state> for <duration> (<reason>)`.
  - The JSON `AgentList` gains a `nodes` array, as in NCLI-002. It is present whenever the server knows any host and absent otherwise, so existing readers are unaffected. An agent's node is found by its `host_id`.
- **Source:** not built
- **Decision:** D-068
- **Verified by:** planned: unit tests of the table and JSON with one unreachable host; the `--host` filter

#### NCLI-006 Search covers every agent
- **Behavior:** `noc search` ([03-cli.md](03-cli.md) CLI-037) MUST match over the merged registry, including the last-read agents of hosts that are not connected. A host's alias is added to an agent's searchable text, so `noc search devbox` finds that host's agents. Age is computed from the agent's own timestamps, which the owning node set.
- **Source:** not built
- **Verified by:** planned: a unit test with agents from two nodes, one unreachable

#### NCLI-007 Commands go to the owning node
- **Behavior:** For an agent owned by a remote node:
  - `noc attach <id>` MUST replace itself with `ssh -t <alias> noc attach <id>`, so the decision of how to open the agent is the owning node's ([03-cli.md](03-cli.md) ATT-001..ATT-017).
  - `noc stop`, `noc rename` and `noc forget` MUST be sent over the host's link and applied by the owning node; the hub applies the result it is sent back.
  - When the host is not `connected`, each of these MUST fail without side effects, exit 1, with `noc: <alias> is <state>; cannot <verb> <id>`.

  For a local agent nothing changes.
- **Source:** not built
- **Verified by:** planned: an integration test for rename and stop through a link, and for each refusal; MANUAL: attach to a remote background agent

#### NCLI-008 Remote attach is for background agents
- **Behavior:** Remote attach is specified for agents the provider runs in the background, which outlive the SSH connection that views them: a dropped connection ends the view, and attaching again resumes it. An agent that lives in a terminal on the remote is not given a terminal by `noc`; attaching to it does whatever the owning node's `noc attach` does for that agent locally. Creating background agents is the job of a future `noc spawn` (see the roadmap), which this file assumes and does not specify beyond: it MUST accept the same `--host <alias>` and run the provider on the owning node.
- **Source:** not built
- **Decision:** D-069
- **Verified by:** planned: MANUAL: attach, drop the connection, attach again

#### NCLI-009 Status and diagnostics
- **Behavior:** `noc server status` MUST add a `hosts:` section with one line per host (alias, state, version, reason). `noc doctor` MUST report a configuration file that failed to load and each host that is `unreachable` or `incompatible`, each as a warning that does not change its exit code.
- **Source:** not built
- **Verified by:** planned: unit tests of both renderings

---

## 3. The Zellij plugin

#### ZN-001 The plugin learns about nodes from the list it already reads
- **Behavior:** The plugin MUST take node information only from the `nodes` array of the snapshot it reads (`noc ls --json --all`, `noc search --json`). It runs no additional command and never contacts a host. With no `nodes` array it behaves exactly as specified in [07-zellij-panel.md](07-zellij-panel.md).
- **Source:** not built
- **Verified by:** planned: a model test parsing a snapshot with and without `nodes`

#### ZN-002 Agents of a host that is not connected are greyed out
- **Behavior:** An agent whose node's state is `connecting`, `unreachable` or `incompatible` MUST be drawn entirely in the `quiet` colour (marker, title and second line), in its usual place, and counted as usual. `Enter`, `p`, `s`, `w`, `x` and `n` on it MUST do nothing but show the notice `<alias> is <state>`. The setting `unreachable` chooses the treatment: `grey` (default) as above, or `hide`, which leaves those agents out of the list and the counts. The plugin, not `noc`, makes this choice (NCLI-005).
- **Source:** not built
- **Decision:** D-068
- **Verified by:** planned: view tests for both settings; a key test for the refused actions

#### ZN-003 Rows say which machine
- **Behavior:** When the snapshot names any host, an agent row's second line MUST begin its workspace part with the host alias for a remote agent: `{icon} {age}  {alias} · {workspace label}`. Local agents are unlabelled. The inline details and the details window gain a `host` line (`local`, or the alias and its state).
- **Source:** not built
- **Verified by:** planned: view tests

#### ZN-004 The connection line counts hosts
- **Behavior:** When the snapshot names any host and nothing of higher priority is shown ([07-zellij-panel.md](07-zellij-panel.md) ZR-003), the connected entry MUST read `●  connected · {c}/{n} hosts`, where `n` is the number of hosts and `c` those `connected`, in the `active` colour when `c == n` and the `attention` colour otherwise. With `version_skew` on any connected host the line ends with ` · skew`.
- **Source:** not built
- **Verified by:** planned: view tests for all connected, one down, and skew

#### ZN-005 The hosts view
- **Behavior:** `H` MUST toggle a hosts view in place of the list, as `?` does for help; `Esc` or `H` closes it. It lists the local node, then each host by alias, two lines each:

  ```text
  ● devbox          configured   12 agents
    v0.3.0 · connected 2h
  × gpu-1           session       4 agents
    unreachable 5m · ssh: connect timed out
  ! build           configured    0 agents
    incompatible · speaks API version 2
  ```

  The symbol and its colour are `●` `active` for `connected` and `local`, `◌` `info` for `connecting`, `×` `error` for `unreachable`, `!` `attention` for `incompatible`. The second line is the version, followed by ` (differs)` in the `attention` colour when `version_skew`, then the state and how long, then the reason when there is one. This view is how the plugin shows connected and disconnected servers and version skew; it changes nothing.
- **Source:** not built
- **Verified by:** planned: view tests for each state; a key test for `H`

#### ZN-006 Grouping
- **Behavior:** The list MUST be shown in one of three groupings, chosen by the setting `group_by` and cycled with `v`:

  | `group_by` | Groups | Heading |
  |---|---|---|
  | `status` (default) | Blocked, Working, Review, Quiet, with every machine's agents together, as today | `{Group}  {count}` |
  | `host` | The local node, then each host by alias | `{alias}  {count}`, in the `accent` colour; for a host that is not connected, in the `quiet` colour followed by ` · {state}` |
  | `provider` | One per provider, by name | `{provider}  {count}`, in the `accent` colour |

  Within a `host` or `provider` group, agents are ordered Blocked, Working, Review by recency, and the group's Quiet agents fold into a single `Quiet  {count}` row that opens and closes like a Quiet section. Each row keeps its state's colour and symbol in every grouping. The chosen grouping lasts until the plugin is reloaded. Search results (ZK-026) are grouped the same way, without folding.
- **Source:** not built
- **Decision:** D-070
- **Verified by:** planned: model tests for the three orderings; view tests for the headings

#### ZN-007 Opening a remote agent
- **Behavior:** Opening a remote agent MUST run the same `noc attach <id>` in the inner session as for a local one ([08-zellij-host.md](08-zellij-host.md) ZX-028, ZX-029); `noc` routes it (NCLI-007). The plugin MUST NOT pass `--cwd` for a remote agent, because its workspace path is not a local path, and `w` on a remote agent MUST open `ssh -t <alias>` rather than a local shell. Tab naming, following and focus work as for a local agent.
- **Source:** not built
- **Verified by:** planned: command-building unit tests; MANUAL: open a remote agent from the sidebar

---

## Build order

Each slice is usable by itself and leaves the gates green.

1. `noc api stdio`, the configuration file, `noc nodes`, and links that reach `connected` or `unreachable` (NODE-001..NODE-006, NCLI-001..NCLI-004, NCLI-009).
2. The merged registry in `ls`, `search`, `watch`, `show`, with `--host` and the `nodes` array (NODE-007..NODE-009, NCLI-005, NCLI-006).
3. Routed `attach`, `stop`, `rename`, `forget` (NCLI-007, NCLI-008).
4. The plugin: grey rows, host labels, the connection line, the hosts view (ZN-001..ZN-005, ZN-007).
5. Grouping (ZN-006).

`noc spawn` is separate work; slice 3 does not wait for it.

## Open questions

- How long the hub keeps the agents of a configured host that never comes back.
- Whether a remote hub's own remotes are ever followed. As specified, no: a link reads only the agents the remote node owns.
- Clock differences between machines: ages are computed from the owning node's timestamps against the hub's clock.
- Whether `group_by` should also offer workspace or repository.
- Authorization: anyone who can SSH to a host as the user can read and drive that user's agents there, which is already true without `noc`.
