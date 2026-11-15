# noc behavioral specification

This directory states what `noc` does: the Go executable `noc` (registry
server, client, hook handler, setup and diagnostics) and the Zellij plugin
`zellij-noc`. It is the authoritative statement of behavior, and the code
implements it. When the two disagree, one of them has a bug; decide which,
fix it, and leave them agreeing.

The other documents explain rather than specify:
[`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) gives the shape of the system,
[`docs/PROVIDERS.md`](../PROVIDERS.md) the provider interfaces it relies on,
[`docs/ZELLIJ.md`](../ZELLIJ.md) how to use the plugin,
[`docs/DEVELOPMENT.md`](../DEVELOPMENT.md) how to work on the code, and
[`docs/DECISIONS.md`](../DECISIONS.md) why it is built this way. Where one
of them disagrees with this specification, the specification wins.

## Contents

| File | Scope | Requirement prefixes |
|---|---|---|
| [00-overview.md](00-overview.md) | Product definition, scope, system invariants, context, state ownership, terminology | INV |
| [01-build.md](01-build.md) | Repository layout, toolchains, Makefile targets, build metadata, artifacts, CI, releases, the installer, the development pair | BLD |
| [02-api.md](02-api.md) | Public JSON model, derived display state and bucket, HTTP API over the Unix socket, watch stream, events, registry | API, HTTP, REG |
| [03-cli.md](03-cli.md) | Every command and flag, output and exit codes, server process lifecycle, paths and environment, attach, stop, rename | CLI, LIFE, CFG, ATT |
| [04-server.md](04-server.md) | Server composition and loops, health, retention, reconciliation, SQLite schema and migrations, Git workspace inspection | SRV, REC, DB, WS |
| [05-providers.md](05-providers.md) | Adapter contract, bounded command runner, Claude roster adapter, Codex app-server adapter | PRV, CLA, CDX |
| [06-hooks.md](06-hooks.md) | `hook ingest`, normalization and bounds, event IDs, outbox and projection, process lineage, provider hook configuration, doctor | HK, OBX, PL, HC, DOC |
| [07-zellij-panel.md](07-zellij-panel.md) | The sidebar: data model, grouping and sorting, rendering, details, keys | `ZM`, `ZG`, `ZR`, `ZK` |
| [08-zellij-host.md](08-zellij-host.md) | The host session: layout, plugin configuration and permissions, lifecycle, keyboard ownership, actions in the inner session | `ZH`, `ZX`, `ZE` |
| [09-verification.md](09-verification.md) | Gates, isolated smoke test, acceptance evidence, Zellij end-to-end test, real-provider checks, audits, change protocol | — |
| [10-remote-nodes.md](10-remote-nodes.md) | **Planned, not built:** agents on other machines: hosts, the hub, `noc nodes`, routed commands, and the sidebar's host status and grouping | `NODE`, `NCLI`, `ZN` |
| [11-teams.md](11-teams.md) | **Planned, first draft:** a Zellij view of a team: a room per team, a tab per agent, panes as ways into the agent. Teams themselves are built and specified in 02, 03, 04 and 07 | `ZT` |
| [verify/isolated-smoke.sh](verify/isolated-smoke.sh) | Self-asserting end-to-end smoke test of the binary, fully isolated | — |

## Requirement format

```text
#### PREFIX-NNN Short title
- **Behavior:**    a testable statement (MUST / MUST NOT) with exact constants, strings and codes
- **Source:**      `path/to/file.go` (FunctionName): the code that implements it
- **Decision:**    D-NNN: the decision in docs/DECISIONS.md it enforces, if any
- **Verified by:** a named test | a gate | a smoke or end-to-end check | MANUAL: steps | UNTESTED
```

- An ID is stable. A removed requirement's ID is not reused, so numbering
  has gaps.
- `Source` names files and functions, not line numbers.
- `Verified by` says what evidence exists. `MANUAL:` describes how to check
  by hand; it is not a record that someone did. `UNTESTED` means there is
  no test and no procedure.
- A file marked **planned** specifies work that is not built. Its
  requirements have `Source: not built` and a `Verified by` that names the
  evidence the implementation must bring. They move into the file that owns
  the component when they are built.
- Each file ends with a **Known limitations** list: behavior that is real,
  intended or not, that qualifies the requirements above it.

## Verify

```sh
make check && make zellij-check                               # Go tests, vet, gofmt; Rust fmt, clippy, tests
make build && docs/spec/verify/isolated-smoke.sh go/bin/noc   # end to end, isolated: expect "passed: 34  failed: 0"
make zellij-e2e                                               # the plugin in a real, headless Zellij pair
```

The first two lines are what CI runs. The third needs `zellij`, `tmux`
and `jq`, and plugin permissions granted once through `make dev`. What each
command proves, and the checks that touch real providers, are in
[09-verification.md](09-verification.md).

## Keeping it current

Change the specification in the same change as the code, following the
protocol in [09-verification.md](09-verification.md#8-change-protocol):

1. Update the requirement in the file that owns it.
2. Add or adjust a test and name it in `Verified by`.
3. Record a decision in [`docs/DECISIONS.md`](../DECISIONS.md) when a
   product or architecture rule changes.
4. Put any gap that remains in that file's Known limitations.
5. Re-run the gates.
