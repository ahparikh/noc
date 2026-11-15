---
name: noc
description: Find and identify other coding agents on this machine with the noc registry. Use when you need to know which agent you are, which team you are on and who else is in it, to find another agent by what it is working on before messaging it, to start a worker agent, or to put agents in a team.
---

<!-- noc-skill: installed by "noc setup skill"; edits are overwritten on the next install -->

# noc: find the other agents

`noc` is a registry of the coding-agent conversations on this machine
(Claude Code and Codex). It knows which agents exist, what each is working
on, what state each is in, and which agents work together as a team. It
does not run agents and it does not carry messages between them.

Every command below is read-only unless it says otherwise. Add `--json`
when you will read the output yourself; the tables are for people. If a
`noc` command fails because the server is not running, carry on without
it: nothing you are doing depends on `noc`.

## What counts as an agent

An agent is one top-level conversation: a session a person started, or one
started with `noc spawn`. Subagents and tasks you start inside your own
conversation are not agents and are not in the registry. Use your own
subagents for work that should report back into your conversation, and a
spawned agent for work that should go on by itself.

## Who am I

```sh
noc whoami --json
```

This prints your own agent: `id` (what every other `noc` command takes),
`name` or `user_name`, `team` and `lead` when you are in a team, and
`parent_id` when another agent spawned you. If it says you are not an agent
`noc` knows, you are a subagent or the session is not registered yet.

## My team

```sh
noc ls --json --all --team <team>   # everyone in my team, with their state
noc teams                           # every team, its lead, and counts by state
```

`team` is the name from `noc whoami`. In the list, the agent with
`"lead": true` leads the team, and `parent_id` says who spawned whom.
`--team none` lists the agents that are in no team.

## Find an agent

```sh
noc search --json auth migration     # every word must match
noc search --json team:billing       # exactly that team
noc search --json newer:2d tests     # active within two days
noc show --json <id>                 # everything about one agent
```

Search matches an agent's names, its first request, its current summary,
its directory and branch, its team, and its state. Read these fields of a
result:

| Field | Meaning |
|---|---|
| `id` | The agent's `noc` ID |
| `user_name`, `name` | What it is called; prefer `user_name` |
| `provider`, `native_session_id` | Which provider runs it, and that provider's own session ID |
| `bucket` | `blocked` (waiting for a person), `working`, `review` (finished its turn), `quiet` (no process) |
| `initial_request`, `summary` | What it was asked, and what it last said it was doing |
| `workspace` | Its directory, repository root and branch |
| `team`, `lead`, `parent_id` | Its team, whether it leads it, and who spawned it |

## Talking to another agent

`noc` does not deliver messages yet. Use `noc` to work out which agent you
mean, then use whatever your provider gives you to reach a session, and
address it by the `name` and `native_session_id` that `noc` reports. Only
an agent whose `bucket` is not `quiet` has a running process to receive
anything. If you have no way to message a session, say so to the person
you are working for and give them the agent's `id` and name.

## Start a worker

This starts a new background Claude agent, so do it only when the work was
asked for or clearly calls for it.

```sh
noc spawn --team <team> --name <short-name> "<the whole task, self-contained>"
```

It prints the new agent's ID. The worker starts in your current directory
(`--dir <path>` for another) and knows nothing of your conversation, so the
prompt must carry everything it needs. With no `--team` it joins your team;
`--team none` keeps it out. If you are in no team and name one, you join it
and become its lead. Check on workers with `noc ls --json --team <team>`.

## Change a team or a name

These change the registry, so do them only when asked.

```sh
noc team set <team> [<id>...]    # join a team; with no ID, you join
noc team clear [<id>...]         # leave a team
noc team lead <team> [<id>]      # mark the lead; it must be in the team
noc rename <id> <name>           # name an agent so people can find it
```

A team name is 1 to 48 characters from `A-Za-z0-9._-`. A team exists for
as long as some agent carries its name.
