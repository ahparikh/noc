# 11. A Zellij view of a team

> **Status: planned, first draft.** Nothing in this file is built, and it is
> expected to change. Every requirement's `Source` is `not built`, and
> `Verified by` names the evidence the implementation must bring. When a
> slice lands, its requirements move into the file that owns the component
> and leave this one.

## What is built, and where it is specified

Teams themselves are built. An agent may carry a team name and may be its
team's lead, `noc spawn` starts a background Claude agent in a team, the
sidebar can group its rows by team, and in Zellij each team has an inner
session of its own.

| What | Where |
|---|---|
| The `team`, `lead` and `parent_id` fields; `PUT /v1/agents/{id}/team`; `POST /v1/agents` | [02-api.md](02-api.md) |
| `noc team`, `noc teams`, `noc spawn`, `--team`, the `TEAM` column, `team:<name>` in search | [03-cli.md](03-cli.md) |
| How the team, lead and parent are stored | [04-server.md](04-server.md) |
| The sidebar's `group_by`, the `v` key, team headings and the lead mark; the `o` key | [07-zellij-panel.md](07-zellij-panel.md) |
| Team sessions: a session per team, moving between them, the session an agent opens in, the tab bar that names the team (ZT-001 to ZT-006, ZT-012, ZT-013) | [08-zellij-host.md](08-zellij-host.md) §10 |
| Why | [DECISIONS.md](../DECISIONS.md) D-070, D-071, D-072, D-076, D-080, D-081 |

## Scope

This file is about what is still to come for a team in Zellij: inside a
session, a tab is one agent, and the panes in that tab are ways into that
agent.

```text
host session (noc-host)
┌ sidebar ────────┐ ┌ guest pane: the session on show ───────────────────────┐
│ auth-migration  │ │ session "noc--auth-migration"                          │
│  ▸ lead         │ │  auth-migration │ lead │ worker-1 │ worker-2 │ …    │
│    worker-1     │ │  ┌ worker-1's own UI ─────────────────────────────┐ │
│    worker-2     │ │  ├ shell in worker-1's worktree ──────────────────┤ │
│ billing   2 · 1 │ │  └────────────────────────────────────────────────┘ │
│ no team       3 │ └─────────────────────────────────────────────────────┘
└─────────────────┘   other sessions keep running, out of sight
```

It also holds what is left of spawn: Codex.

## Terms

| Term | Meaning |
|---|---|
| session | The inner Zellij session that belongs to a team |
| session on show | The session the host session's guest pane is attached to |
| agent tab | A tab in a session that holds exactly one agent |
| entry point | A pane in an agent tab: the agent's own UI, a shell in its worktree, and so on |

Requirement prefix: `ZT`.

---

## 1. Agent tabs

#### ZT-007 Panes in an agent tab are ways into that agent
- **Behavior:** `w` on an agent MUST open its shell as a new pane in that agent's tab (opening the tab first if the agent has none), in the agent's workspace directory, and named `<title> · shell`. A pane opened this way belongs to the agent: the sidebar's selection follows it, and it does not count as a second agent in the tab. Today `w` opens an unnamed pane in whichever tab has the keyboard, in the session on show ([08-zellij-host.md](08-zellij-host.md) ZX-030).
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: command-building unit tests; an end-to-end check that `w` adds a pane to the agent's own tab

#### ZT-008 The agent tab's shape is a layout file
- **Behavior:** When `layouts/noc-agent.kdl` exists, a new agent tab MUST be created from it, with the agent's own UI in the pane the layout marks for it; when it does not, the tab is a single pane, as today.
- **Source:** not built
- **Verified by:** planned: an end-to-end check with and without the file

#### ZT-009 Side by side stays possible
- **Behavior:** `p` and `s` keep opening the selected agent as a pane in the tab that has the keyboard (ZX-029), in the session on show. A tab that comes to hold two agents belongs to neither: it is not renamed for either ([08-zellij-host.md](08-zellij-host.md) ZX-036). This is how it works today; what is planned is its test.
- **Source:** `zellij/src/guest.rs`
- **Verified by:** planned: the existing tab-naming unit test, extended

#### ZT-010 Tabs outlive their agents
- **Behavior:** When an agent ends or goes quiet, its tab and its other panes MUST be left as they are. Only the user closes them. This is how it works today; what is planned is its test.
- **Source:** `zellij/src/guest.rs`
- **Decision:** D-073
- **Verified by:** planned: an end-to-end check that a tab survives its agent's exit

## 2. Spawn

#### ZT-011 Spawning a Codex agent
- **Behavior:** `noc spawn --provider codex` MUST start a Codex thread through the app-server (`thread/start`, then `turn/start` with the prompt) and register it as `noc spawn` registers a Claude session ([03-cli.md](03-cli.md)). Today it exits `1` with `noc spawn: codex is not supported yet`.
- **Source:** not built
- **Decision:** D-076
- **Verified by:** planned: tests against a fake app-server, and a check against a real Codex

---

## Open questions

- **Whether a session per team is the right view.** It is built to be tried. The alternatives are a tab group per team inside the one inner session, or a view that is not a session.
- **Which session a detach comes back to.** Attaching again shows the inner session, not the team's session that was on show.
- **Whether the sidebar should say which session is on show**, and whether moving to a session by hand (Zellij's session manager) should select its team.
- **Whether a new tab can take both a layout and the agent's command** in Zellij 0.46 (ZT-008). If not, the tab is opened and the extra panes are added afterwards.
- **A team across machines.** A session lives on one machine. Agents of the team that run elsewhere ([10-remote-nodes.md](10-remote-nodes.md)) would open as tabs over SSH, unless and until sessions can live on a remote host.
- **Sessions without teams.** Whether the user can make a session by hand, or only by having a team.
- **Sessions that are no longer needed.** A session keeps running after its team's last agent is gone. Whether noc should end it, or offer to.
- **Many teams.** Whether folded groups need a limit or their own search.
