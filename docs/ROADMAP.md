# Roadmap

`noc` grows in layers. Each layer has to be useful and testable on its own,
and none may take ownership away from the native agent runtimes. The design
that exists today is described in [`ARCHITECTURE.md`](ARCHITECTURE.md);
settled constraints on future work are marked *planned* in
[`DECISIONS.md`](DECISIONS.md).

## Built today

- **One `noc` executable** with a foreground or background server, status,
  stop, and restart; setup, check, and uninstall of provider hooks; and a
  read-only `doctor`.
- **Local discovery** of live, top-level Claude Code and Codex conversations,
  including ones started outside `noc`, from provider snapshots and lifecycle
  hooks, with stable identity across server and provider-worker restarts.
- **A normalized registry** in SQLite: independent live, runtime, activity,
  and attention state; a derived display state and bucket (blocked, working,
  review, quiet); model and reasoning effort; bounded, sourced previews;
  read-only workspace and Git observation; frontend bindings.
- **A versioned local API** over a user-only Unix socket: health, list,
  detail, a snapshot watch stream, and a transcript-free event timeline.
- **CLI commands** `ls`, `show`, `watch`, `events`, `rename`, `attach`,
  `stop`, and `forget`, with stable JSON output.
- **Two-way names**, pushed to Codex.
- **Attach** that never leaves a Claude agent owned by a terminal, and
  **stop** for Claude background sessions.
- **Bounded retention** of ended rows.
- **`zellij-noc`**, a Zellij plugin that shows the registry as a sidebar
  beside the user's own session ([`ZELLIJ.md`](ZELLIJ.md)).
- **Releases** carrying `noc` binaries for macOS and Linux (amd64 and arm64)
  and the plugin ([`DEVELOPMENT.md`](DEVELOPMENT.md)).

Not built: service-manager integration, a live provider event subscription,
context headroom, remote hosts, the shared agent skill, messaging, objective
state, spawning, and any frontend other than Zellij.

## Planned

The order below is the intended order of work.

### 1. Provider depth

The registry should say everything a user needs to know about a running
agent, from supported provider interfaces only.

- **Live Codex connection.** Keep one subscribed app-server connection
  instead of periodic snapshots, for prompt state changes, token-usage
  notifications, and a basis for stopping Codex agents. The question to
  answer first: can a client receive a thread's notifications without
  resuming or otherwise disturbing it?
- **Claude context headroom.** Claude's status line command runs for
  background sessions even when nothing is attached, and its input carries
  context-window usage. A `noc statusline` command would record that reading
  and then run the user's existing status line unchanged. Because it sits in
  Claude's render path it must degrade to nothing: the user's status line
  always runs and prints unchanged whether or not the server is up; the
  reading goes to the same local spool the hooks use, keeping only the
  latest per session; recording has a budget of a few milliseconds, never
  prints, and never fails the command. The worst case is a stale figure.
- **Headroom as a registry field** that frontends can show.
- **Stop for Codex**, once a supported interface stops a running session
  rather than archiving a thread.

### 2. Service and hardening

- User-service units (`launchd`, systemd) that supervise the foreground
  server.
- Pruning of the observation journal and processed hook outbox.
- Retrying a provider whose CLI was missing when the server started, without
  a restart.
- Storage corruption and migration diagnostics in `noc doctor`.

### 3. Remote nodes and a consolidated registry

Each development host runs its own `noc server` next to its providers,
unchanged from the local case. The machine the developer sits at also reads
the others over SSH and merges their agents, so one list, one search and
one sidebar cover every machine.

```text
frontend -> hub node -> owning node -> provider
```

This is specified in [spec/10-remote-nodes.md](spec/10-remote-nodes.md):
configured and session hosts, link states, the merged registry, commands
routed to the owning node, and the sidebar's host status and grouping. Its
build order:

1. `noc api stdio`, the hosts configuration, `noc nodes`, and links.
2. The merged registry in `ls`, `search`, `watch` and `show`.
3. Attach, stop, rename and forget routed to the owning node.
4. The sidebar: unreachable agents greyed out, host labels, a hosts view.
5. Grouping by status, machine or provider.

Remote attach assumes background agents, which is what `noc spawn`
(section 4) will create.

### 4. Shared agent skill and cross-agent messaging

- **More in the `noc` skill.** `noc setup skill` installs a skill that
  teaches agents what the registry tracks, the boundary between top-level
  agents and internal subagents, and how to find their team and other
  agents. It will also teach messaging once that exists. Agent-to-agent
  messaging, in particular across machines, needs its own design and spec
  first.
- **A durable, provider-neutral mailbox.** Messages have an ID and
  idempotency key, sender and recipient agents, a bounded body with optional
  correlation metadata, and a delivery state (queued, delivered,
  acknowledged, failed, expired) with attempts and sanitized failure detail.
  Delivery uses supported provider inputs only, and queues while the
  recipient is unreachable.
- **Client surfaces:** `noc send <agent> <message>` and an inbox, and
  optionally an MCP server exposing search, inspect, and message tools over
  the same registry. MCP is a client surface, not a second source of state.
- **Guardrails before it is on by default:** de-duplication, loop limits,
  delivery diagnostics, and authorization rules. A delivery acknowledgement
  is not the recipient acting on a message, and an agent message never
  satisfies a human approval prompt.

### 5. Objectives and lifecycle

- **Reversible objective state** (`noc objective complete|reopen`), so that
  "finished, please review" can be told apart from "paused". Provider
  idleness never implies it.
- **A "we are done" flow in the skill:** the agent settles its own worktree
  and branch, then records completion. Teardown belongs to the agent, which
  knows what is unpushed; `noc` never touches Git.
- **More of spawn, then interrupt and archive.** `noc spawn` starts a
  background Claude agent today. Still to come: Codex, a host, an issue
  reference, an isolation preference, quotas, idempotency and cancellation.
  Spawning never becomes the only way to start a session.
- **A Zellij view of a team.** Teams, their leads and the sidebar's team
  grouping are built. What is left is a place to see one team's agents
  together, such as a session per team with a tab per agent. A first
  draft is in [spec/11-teams.md](spec/11-teams.md).
- **More of durable children:** `parent_id` records which agent spawned an
  agent. Still to come: a view of the tree, and links to external work
  items.
- **A warning** when several agents write to one workspace at once.

### 6. More frontends, history, and providers

- An editor frontend and a web UI over the same contract.
- History: import, search, and archival beyond the retained registry, with
  its own retention and privacy controls.
- A third provider, only from a concrete supported interface. A plugin
  protocol for provider adapters is worth designing once a third adapter has
  tested the abstraction.

## Where this is going

The goal is for many concurrent agent conversations to feel like one
coherent development environment. Providers keep running natively; the
registry makes their top-level conversations discoverable; interchangeable
frontends let a person move between them without tracking terminals,
provider IDs, worktrees, or hosts by hand.

### The problem

Several agents in one checkout collide: they change the same files and
index, one switching branches changes the view for all, and a person cannot
safely read one agent's work while another mutates it. Giving each agent a
worktree fixes isolation and creates bookkeeping: worktrees to name, find,
update, and remove; and session lists, panes, worktrees, issues, and pull
requests as separate inventories, on each machine separately. It becomes
hard to tell which conversation needs input, which directory it owns, and
whether opening a terminal should focus, attach, or resume.

### The intended experience

1. Keep one familiar main checkout on the default branch as the place to
   browse and launch from, not a shared writable workspace.
2. Ask any supported provider to look into an issue or a free-form question.
3. Let the top-level agent decide whether the work stays exploratory, needs
   one isolated worktree, or becomes several durable workers.
4. Let the provider and agent create worktrees under a known root, with
   names nobody has to invent or remember.
5. Let the agent own branches, commits, rebases, pull requests, review, and
   cleanup.
6. See every top-level conversation in one registry, however and wherever it
   was started.
7. Select an agent from any frontend and see enough to decide whether to
   engage: name, provider, state, initial request, latest update, host,
   directory, worktree, branch, and model.
8. Focus its terminal if one is already open; otherwise attach or resume the
   same conversation in a new one.
9. Let agents find and message each other, without a message ever carrying
   the authority of a human approval.
10. Mark an objective complete independently of whether a provider process
    is idle, detached, or resumable.

Work does not have to start from an issue tracker. A conversation that
begins as a question can later file issues, plan, create a worktree, and
implement, as the same registered agent. A long-lived coordinating agent can
hold a project's plan and delegate to durable workers when independent human
engagement is useful, while its internal subagents stay hidden.

### Who owns what

| Concern | Owner |
|---|---|
| Conversation execution, tools, provider-native subagents | The provider |
| Worktrees, branches, pull requests, rebases, review iteration | The top-level agent and the repository's workflow |
| Agent identity, normalized state, workspace observation, host routing, relationships, mailbox | `noc` |
| Panes, tabs, terminals, editors, selection, layout | The frontend |
| Approvals and irreversible product decisions | The human |

## Non-goals

- Replacing Claude Code, Codex, their terminal interfaces, or their subagent
  systems.
- Building another terminal multiplexer or hosting PTYs.
- One registry row per prompt, process, pull request, issue, or internal
  worker.
- Making branch, worktree, or pull-request decisions on an agent's behalf,
  or mutating Git at all.
- Parsing terminal output or transcripts to learn state.
- Requiring sessions to be launched through `noc`.
- Storing transcripts or replicating provider history by default.
- Coupling the backend to Zellij, an editor, a web UI, or any other
  frontend.
- Starting or installing provider runtimes on the user's behalf.
