# 11. Cells

> **Status: planned, first draft.** Nothing in this file is built, and it is
> expected to change. It specifies how `noc` groups agents that work
> together, and how the Zellij frontend gives each group its own session.
> Every requirement's `Source` is `not built`, and `Verified by` names the
> evidence the implementation must bring. When a slice lands, its
> requirements move into the file that owns the component and leave this
> one.

## Scope

Some agents belong together: an orchestrator and the agents it started, or
a handful of conversations that are jointly responsible for one piece of
work. `noc` calls such a set a **cell**, and the agent that runs it its
**handler**.

This file covers two layers:

- **`noc`:** how agents get a cell and a handler, and how the CLI lists,
  filters and summarises by cell.
- **Zellij:** each cell gets its own inner session, a **room**. The host
  session shows one room at a time and switches between them. Inside a
  room, a tab is one agent, and the panes in that tab are ways into that
  agent.

```text
host session (noc-host)
┌ sidebar ────────┐ ┌ guest pane: the room on show ───────────────────────┐
│ auth-migration  │ │ room "noc-auth-migration"                           │
│  ▸ handler      │ │  tab: handler │ tab: worker-1 │ tab: worker-2 │ …   │
│    worker-1     │ │  ┌ worker-1's own UI ─────────────────────────────┐ │
│    worker-2     │ │  ├ shell in worker-1's worktree ──────────────────┤ │
│ billing   2 · 1 │ │  └────────────────────────────────────────────────┘ │
│ (no cell)     3 │ └─────────────────────────────────────────────────────┘
└─────────────────┘   other rooms keep running, out of sight
```

It builds on two other pieces of planned work and does not repeat them:
`noc spawn` (the roadmap) creates agents, and
[10-remote-nodes.md](10-remote-nodes.md) brings in agents on other machines.

## Terms

| Term | Meaning |
|---|---|
| cell | A named set of agents that work together. It exists for as long as any agent carries its name |
| handler | The agent that runs a cell: usually the orchestrator that started the others |
| parent | The agent that spawned an agent. Independent of cells |
| room | The inner Zellij session that belongs to a cell |
| default room | The inner session for agents with no cell: today's inner session |
| room on show | The room the host session's guest pane is attached to |
| agent tab | A tab in a room that holds exactly one agent |
| entry point | A pane in an agent tab: the agent's own UI, a shell in its worktree, and so on |

"Workspace" keeps its existing meaning in `noc`: an agent's directory and
branch. A cell is not a workspace.

Requirement prefixes: `CELL` (the model and the CLI), `ZC` (the Zellij
plugin and its static configuration).

---

## 1. Cells in `noc`

#### CELL-001 A cell is a name on an agent
- **Behavior:** An agent MAY carry one cell name. A cell has no record of its own: it exists while at least one agent carries its name, and two agents are in the same cell exactly when their names are equal, on whichever nodes they run. A cell name is 1 to 48 characters from `A-Za-z0-9._-`, not starting with `-` or `.`, and not `none`. The name is stored by the agent's owning node with the agent's identity, like its user-assigned name, and survives the agent going quiet or the server restarting.
- **Source:** not built
- **Decision:** D-071
- **Verified by:** planned: storage tests for set, clear and restart; a validation table for names

#### CELL-002 The handler
- **Behavior:** An agent in a cell MAY be marked as that cell's handler. On one node at most one agent per cell is the handler: marking another clears the first. An agent that leaves a cell stops being its handler. A cell need not have a handler. When agents from several nodes are merged and more than one is marked handler of the same cell, the one first seen is treated as the handler and the others as ordinary members.
- **Source:** not built
- **Decision:** D-071
- **Verified by:** planned: storage tests; a merge test with two marked handlers

#### CELL-003 The parent link
- **Behavior:** An agent created by `noc spawn` from inside another agent (identified by `NOC_AGENT_ID` in the spawning environment) MUST record that agent's id as its `parent_id`. The link is a fact about how the agent started: it never changes, and it does not depend on cells. An agent started any other way has no parent.
- **Source:** not built
- **Verified by:** planned: a spawn test with and without `NOC_AGENT_ID`

#### CELL-004 How agents get a cell
- **Behavior:**
  - `noc spawn --cell <name>` MUST create the agent in that cell.
  - `noc spawn` with no `--cell`, run from inside an agent that is in a cell, MUST put the new agent in the same cell. `--cell none` overrides this.
  - When an agent that is in no cell spawns into a cell that has no handler on this node, the spawning agent MUST join the cell and become its handler. So an orchestrator that names a cell for its first worker ends up running it, with nothing else to do.
  - `noc cell set <name> <id>…` puts existing agents in a cell, moving them out of any other; `noc cell clear <id>…` takes them out of theirs.
  - `noc cell handler <name> <id>` marks the handler; the agent MUST already be in that cell, else exit 1 with `noc: <id> is not in cell <name>`.

  An invalid name exits 2 before contacting the server. For an agent on a remote node these are routed to the owning node ([10-remote-nodes.md](10-remote-nodes.md) NCLI-007).
- **Source:** not built
- **Decision:** D-071
- **Verified by:** planned: integration tests for each rule

#### CELL-005 The model
- **Behavior:** The public `Agent` gains three optional fields: `cell` (the name), `handler` (true only for a cell's handler), and `parent_id`. They are omitted when unset, so existing readers are unaffected.
- **Source:** not built
- **Verified by:** planned: API golden tests with and without the fields

#### CELL-006 Listing and searching by cell
- **Behavior:** `noc ls`, `noc search` and `noc watch` gain `--cell <name>`, keeping that cell's agents, and `--cell none`, keeping agents in no cell. When any listed agent is in a cell, the tables gain a `CELL` column after `ID`, with the handler's cell written `<name>*`. In `noc search`, a cell's name is part of an agent's searchable text, and the term `cell:<name>` keeps exactly that cell's agents ([03-cli.md](03-cli.md) CLI-037).
- **Source:** not built
- **Verified by:** planned: unit tests of filtering, the table and the search term

#### CELL-007 `noc cells`
- **Behavior:** `noc cells [--json]` MUST list every cell that has an agent, by name, then a last row `(none)` for agents in no cell when there are any. The table has the columns `CELL`, `HANDLER` (its display name, or `-`), `AGENTS`, `BLOCKED`, `WORKING`, `REVIEW`, `QUIET`, and, when the server knows any remote host, `HOSTS` (the aliases its agents run on). `--json` writes `{"schema_version": 1, "generated_at": …, "cells": [ … ]}` with the same fields and the handler's id.
- **Source:** not built
- **Verified by:** planned: a unit test of the table and JSON from fixed agents

---

## 2. Rooms in Zellij

#### ZC-001 A room per cell
- **Behavior:** Each cell has a room: the inner session named `<guest_session>-<cell>` (`noc-auth-migration` with the default settings). Agents in no cell use the default room, the session named by `guest_session`, exactly as today. A room is created the first time something is opened in it, with the layout and options of `layouts/noc-inner.kdl` ([08-zellij-host.md](08-zellij-host.md) ZX-035). There is still one host session.
- **Source:** not built
- **Decision:** D-072
- **Verified by:** planned: an end-to-end check that opening an agent of a cell creates its room

#### ZC-002 The guest pane shows one room
- **Behavior:** The host session's guest pane MUST hold a client of exactly one room at a time, the room on show. Moving to another room MUST NOT detach or restart the host session, and MUST leave every other room running with all of its tabs. No room ever has more than one client held by the guest pane. Until a cell's agent is opened, the room on show is the default room.
- **Source:** not built
- **Decision:** D-072
- **Verified by:** planned: end-to-end checks that both rooms are alive after a switch and that the host kept its client

#### ZC-003 The sidebar reads the room on show
- **Behavior:** The inner-session read ([08-zellij-host.md](08-zellij-host.md) ZX-020) MUST target the room on show, and everything derived from it (which agent has the keyboard, tab names, the panes the panel opened) is kept per room. After a switch the panel MUST read the new room before it focuses or opens anything there. The panel does not poll rooms that are not on show.
- **Source:** not built
- **Verified by:** planned: unit tests of per-room state; an end-to-end check that the selection follows the focus after a switch

#### ZC-004 A tab is one agent
- **Behavior:** An agent tab holds one agent. `Enter` on an agent MUST first make the agent's room the room on show, then focus the agent's existing pane there, or open a new tab for it (ZX-027, ZX-028). The agent's room is the room that already holds a pane of it, if any, and otherwise its cell's room. So changing an agent's cell never moves or closes a pane: the agent is listed under its new cell and is found wherever it is open.
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: end-to-end checks for opening an agent from another room, and for an agent whose cell changed while it was open

#### ZC-005 Panes in an agent tab are ways into that agent
- **Behavior:** `w` on an agent MUST open its shell as a new pane in that agent's tab (opening the tab first if the agent has none), in the agent's workspace directory, with `NOC_AGENT_ID` set to the agent's id, and named `<title> · shell`. A pane opened this way belongs to the agent: the sidebar's selection follows it, and it does not count as a second agent in the tab. For a remote agent the shell is opened as in [10-remote-nodes.md](10-remote-nodes.md) ZN-007.
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: command-building unit tests; an end-to-end check that `w` adds a pane to the agent's own tab

#### ZC-006 The agent tab's shape is a layout file
- **Behavior:** When `layouts/noc-agent.kdl` exists, a new agent tab MUST be created from it, with the agent's own UI in the pane the layout marks for it; when it does not, the tab is a single pane, as today. The shipped file holds the agent's UI with a collapsed shell stacked beneath it.
- **Source:** not built
- **Verified by:** planned: an end-to-end check with and without the file

#### ZC-007 Side by side stays possible
- **Behavior:** `p` and `s` keep opening the selected agent as a pane in the tab that has the keyboard (ZX-029), in the room on show. A tab that comes to hold two agents belongs to neither: it is not renamed for either ([08-zellij-host.md](08-zellij-host.md) ZX-036).
- **Source:** not built
- **Verified by:** planned: the existing tab-naming unit test, extended

#### ZC-008 Tabs outlive their agents
- **Behavior:** When an agent ends or goes quiet, its tab and its other panes MUST be left as they are. Only the user closes them.
- **Source:** not built
- **Decision:** D-073
- **Verified by:** planned: an end-to-end check that a tab survives its agent's exit

#### ZC-009 The sidebar groups by cell
- **Behavior:** `group_by "cell"` (also reached with `v`, [10-remote-nodes.md](10-remote-nodes.md) ZN-006) MUST show one group per cell, by name, then agents in no cell under the heading `no cell`. A heading reads `{cell}  {count}`, in the `accent` colour, followed for a closed group by its counts of agents needing attention: `{b} blocked · {w} working · {r} review`, zero counts left out. The group of the room on show is open and marked `▸`; other groups are closed until opened like a Quiet section. Inside a group the handler comes first, marked `◆ handler` on its second line, then the other agents ordered Blocked, Working, Review, with the group's Quiet agents folded into one row. Every row keeps its state's colour and symbol.
- **Source:** not built
- **Verified by:** planned: model tests for ordering; view tests for headings and the handler

#### ZC-010 Entering a cell
- **Behavior:** `Enter` on a cell heading MUST make that cell's room the room on show, creating it if needed, and give it the keyboard. If the cell has a handler with a pane in that room, that pane is focused. `Enter` on the `no cell` heading shows the default room.
- **Source:** not built
- **Verified by:** planned: a key test; an end-to-end check

#### ZC-011 When a room ends
- **Behavior:** When the room on show ends and it is not the default room, the guest pane MUST show the default room, creating it if needed. When the default room ends while on show, the host session ends, as today ([08-zellij-host.md](08-zellij-host.md) ZX-007); other rooms keep running and are found again at the next start. Ending every room named `<guest_session>-*` is left to the user (`zellij kill-session <room>`).
- **Source:** not built
- **Verified by:** planned: end-to-end checks for each case

---

## Build order

1. Cells in `noc`: the fields, `noc cell`, `noc cells`, and the `--cell` filters (CELL-001, CELL-002, CELL-004 without spawn, CELL-005..CELL-007).
2. Grouping by cell in the sidebar, still with one inner session (ZC-009 without rooms).
3. Agent tabs: `w` in the agent's tab, the tab layout, tabs outliving agents (ZC-004 without rooms, ZC-005..ZC-008).
4. Rooms: one per cell, switching, per-room state, room endings (ZC-001..ZC-003, ZC-010, ZC-011).
5. With `noc spawn`: inheritance, the handler rule, and the parent link (CELL-003, the spawn rules of CELL-004).

Slices 1 to 3 need no change to how the host and inner sessions relate.

## Later: rooms on other machines

A room could live on a remote host, with the guest pane attached to it
through `ssh -t <alias> zellij attach --create <room>`. The remote Zellij
would keep every agent's terminal alive, so attaching is instant, a dropped
connection loses nothing, and remote attach would no longer be limited to
background agents ([10-remote-nodes.md](10-remote-nodes.md) NCLI-008). It is
not specified yet. What it has to answer: every sidebar action and read on
such a room is an SSH round trip, so reads must be slower there; the remote
needs a compatible Zellij; and the theme switch works only if the plugin is
installed on that host too.

## Open questions

- **How the guest pane's client is moved.** The plugin can ask Zellij to switch a client's session, and already runs a hidden instance inside the inner session. Whether that, or re-attaching from the guest pane's loop, gives the cleaner switch needs a prototype.
- **Whether a new tab can take both a layout and the agent's command** in Zellij 0.46 (ZC-006). If not, the tab is opened and the extra panes are added afterwards.
- **A cell across machines.** A room lives on one machine. Agents of the cell that run elsewhere open as tabs over SSH, unless and until remote rooms exist.
- **Rooms without cells.** Whether the user can make a room by hand, or only by having a cell.
- **Many cells.** Whether closed groups need a limit or their own search.
- **Supersedes.** When rooms are built, "one inner session" ([08-zellij-host.md](08-zellij-host.md) ZX-001, D-055) becomes "one room on show".
