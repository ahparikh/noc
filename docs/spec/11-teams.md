# 11. A Zellij view of a team

> **Status: planned, first draft.** Nothing in this file is built, and it is
> expected to change: how Zellij should show a team is still to be designed.
> Every requirement's `Source` is `not built`, and `Verified by` names the
> evidence the implementation must bring. When a slice lands, its
> requirements move into the file that owns the component and leave this
> one.

## What is built, and where it is specified

Teams themselves are built. An agent may carry a team name and may be its
team's lead, `noc spawn` starts a background Claude agent in a team, and the
sidebar can group its rows by team.

| What | Where |
|---|---|
| The `team`, `lead` and `parent_id` fields; `PUT /v1/agents/{id}/team`; `POST /v1/agents` | [02-api.md](02-api.md) |
| `noc team`, `noc teams`, `noc spawn`, `--team`, the `TEAM` column, `team:<name>` in search | [03-cli.md](03-cli.md) |
| How the team, lead and parent are stored | [04-server.md](04-server.md) |
| The sidebar's `group_by`, the `v` key, team headings and the lead mark | [07-zellij-panel.md](07-zellij-panel.md) |
| Why | [DECISIONS.md](../DECISIONS.md) D-070, D-071, D-076 |

## Scope

This file is about what Zellij does with a team. Today every agent opens in
the one inner session, as a tab or a pane, whatever its team. The idea here
is that each team gets its own inner session, a **room**. The host session
shows one room at a time and switches between them. Inside a room, a tab is
one agent, and the panes in that tab are ways into that agent.

```text
host session (noc-host)
┌ sidebar ────────┐ ┌ guest pane: the room on show ───────────────────────┐
│ auth-migration  │ │ room "noc--auth-migration"                          │
│  ▸ lead         │ │  tab: lead │ tab: worker-1 │ tab: worker-2 │ …      │
│    worker-1     │ │  ┌ worker-1's own UI ─────────────────────────────┐ │
│    worker-2     │ │  ├ shell in worker-1's worktree ──────────────────┤ │
│ billing   2 · 1 │ │  └────────────────────────────────────────────────┘ │
│ no team       3 │ └─────────────────────────────────────────────────────┘
└─────────────────┘   other rooms keep running, out of sight
```

It also holds what is left of spawn: Codex.

## Terms

| Term | Meaning |
|---|---|
| room | The inner Zellij session that belongs to a team |
| default room | The inner session for agents with no team: today's inner session |
| room on show | The room the host session's guest pane is attached to |
| agent tab | A tab in a room that holds exactly one agent |
| entry point | A pane in an agent tab: the agent's own UI, a shell in its worktree, and so on |

Requirement prefix: `ZT`.

---

## 1. Rooms

#### ZT-001 A room per team
- **Behavior:** Each team has a room: the inner session named `<guest_session>--<team>` (`noc--auth-migration` with the default settings). A team name cannot start with `-`, so the double hyphen cannot collide with another session of the user's, such as the development pair's `noc-dev`. Agents in no team use the default room, the session named by `guest_session`, exactly as today. A room is created the first time something is opened in it, with the layout and options of `layouts/noc-inner.kdl` ([08-zellij-host.md](08-zellij-host.md) ZX-035). There is still one host session.
- **Source:** not built
- **Decision:** D-072
- **Verified by:** planned: an end-to-end check that opening an agent of a team creates its room

#### ZT-002 The guest pane shows one room
- **Behavior:** The host session's guest pane MUST hold a client of exactly one room at a time, the room on show. Moving to another room MUST NOT detach or restart the host session, and MUST leave every other room running with all of its tabs. No room ever has more than one client held by the guest pane. Until a team's agent is opened, the room on show is the default room.
- **Source:** not built
- **Decision:** D-072
- **Verified by:** planned: end-to-end checks that both rooms are alive after a switch and that the host kept its client

#### ZT-003 The sidebar reads the room on show
- **Behavior:** The inner-session read ([08-zellij-host.md](08-zellij-host.md) ZX-020) MUST target the room on show, and everything derived from it (which agent has the keyboard, tab names, the panes the panel opened) is kept per room. After a switch the panel MUST read the new room before it focuses or opens anything there. The panel does not poll rooms that are not on show.
- **Source:** not built
- **Verified by:** planned: unit tests of per-room state; an end-to-end check that the selection follows the focus after a switch

#### ZT-004 Entering a team
- **Behavior:** A key on a team heading MUST make that team's room the room on show, creating it if needed, and give it the keyboard. If the team has a lead with a pane in that room, that pane is focused. The same key on the `no team` heading shows the default room. `Enter` on a heading folds and unfolds the group today ([07-zellij-panel.md](07-zellij-panel.md)), so which key enters a team is to be decided.
- **Source:** not built
- **Verified by:** planned: a key test; an end-to-end check

#### ZT-005 When a room ends
- **Behavior:** When the room on show ends and it is not the default room, the guest pane MUST show the default room, creating it if needed. When the default room ends while on show, the host session ends, as today ([08-zellij-host.md](08-zellij-host.md) ZX-007); other rooms keep running and are found again at the next start. Ending every room named `<guest_session>--*` is left to the user (`zellij kill-session <room>`).
- **Source:** not built
- **Verified by:** planned: end-to-end checks for each case

## 2. Agent tabs

#### ZT-006 A tab is one agent
- **Behavior:** An agent tab holds one agent. `Enter` on an agent MUST first make the agent's room the room on show, then focus the agent's existing pane there, or open a new tab for it (ZX-027, ZX-028). The agent's room is the room that already holds a pane of it, if any, and otherwise its team's room. So changing an agent's team never moves or closes a pane: the agent is listed under its new team and is found wherever it is open.
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: end-to-end checks for opening an agent from another room, and for an agent whose team changed while it was open

#### ZT-007 Panes in an agent tab are ways into that agent
- **Behavior:** `w` on an agent MUST open its shell as a new pane in that agent's tab (opening the tab first if the agent has none), in the agent's workspace directory, and named `<title> · shell`. A pane opened this way belongs to the agent: the sidebar's selection follows it, and it does not count as a second agent in the tab. Today `w` opens an unnamed pane in whichever tab has the keyboard (ZX-030).
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: command-building unit tests; an end-to-end check that `w` adds a pane to the agent's own tab

#### ZT-008 The agent tab's shape is a layout file
- **Behavior:** When `layouts/noc-agent.kdl` exists, a new agent tab MUST be created from it, with the agent's own UI in the pane the layout marks for it; when it does not, the tab is a single pane, as today.
- **Source:** not built
- **Verified by:** planned: an end-to-end check with and without the file

#### ZT-009 Side by side stays possible
- **Behavior:** `p` and `s` keep opening the selected agent as a pane in the tab that has the keyboard (ZX-029), in the room on show. A tab that comes to hold two agents belongs to neither: it is not renamed for either ([08-zellij-host.md](08-zellij-host.md) ZX-036).
- **Source:** not built
- **Verified by:** planned: the existing tab-naming unit test, extended

#### ZT-010 Tabs outlive their agents
- **Behavior:** When an agent ends or goes quiet, its tab and its other panes MUST be left as they are. Only the user closes them.
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: an end-to-end check that a tab survives its agent's exit

## 3. Spawn

#### ZT-011 Spawning a Codex agent
- **Behavior:** `noc spawn --provider codex` MUST start a Codex thread through the app-server (`thread/start`, then `turn/start` with the prompt) and register it as `noc spawn` registers a Claude session ([03-cli.md](03-cli.md)). Today it exits `1` with `noc spawn: codex is not supported yet`.
- **Source:** not built
- **Decision:** D-076
- **Verified by:** planned: tests against a fake app-server, and a check against a real Codex

---

## Open questions

- **Whether a room per team is the right view at all.** The alternatives are a tab group per team inside the one inner session, or a view that is not a session. This is the first thing to design.
- **How the guest pane's client is moved.** The plugin can ask Zellij to switch a client's session, and already runs a hidden instance inside the inner session. Whether that, or re-attaching from the guest pane's loop, gives the cleaner switch needs a prototype. Either way the guest loop in `layouts/noc-host.kdl` changes, which is a file the user has installed.
- **Whether a new tab can take both a layout and the agent's command** in Zellij 0.46 (ZT-008). If not, the tab is opened and the extra panes are added afterwards.
- **A team across machines.** A room lives on one machine. Agents of the team that run elsewhere ([10-remote-nodes.md](10-remote-nodes.md)) would open as tabs over SSH, unless and until rooms can live on a remote host.
- **Rooms without teams.** Whether the user can make a room by hand, or only by having a team.
- **Many teams.** Whether folded groups need a limit or their own search.
- **Supersedes.** If rooms are built, "one inner session" ([08-zellij-host.md](08-zellij-host.md) ZX-001, D-055) becomes "one room on show".
