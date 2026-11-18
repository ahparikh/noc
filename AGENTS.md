# noc: agent bootstrap

`noc` is a provider-neutral registry and control plane for durable
coding-agent conversations (Claude Code, Codex). It ships as two artifacts:

- `noc`: one Go executable (server, client, hook handler, setup, doctor).
- `zellij-noc.wasm`: a Zellij plugin that shows the registry as a sidebar
  beside your own Zellij session and opens, focuses, renames and stops agents.

## Source of truth

`docs/spec/` is the behavioral specification (see `docs/spec/README.md`).
When the spec and the code disagree, one of them has a bug: fix it, and keep
the other in step in the same change. Record design decisions in
`docs/DECISIONS.md`.

| Document | What it holds |
|---|---|
| `README.md` | What noc is, install, quick start |
| `docs/ARCHITECTURE.md` | Product definition, components, data model |
| `docs/PROVIDERS.md` | Claude Code and Codex integration |
| `docs/ZELLIJ.md` | The Zellij frontend: setup, configuration, keys |
| `docs/DEVELOPMENT.md` | Build, test, the dev pair, releasing |
| `docs/DECISIONS.md` | Design decisions and their reasons |
| `docs/ROADMAP.md` | What is planned |
| `docs/spec/` | Exact behavior, one file per component |

Say each thing in one place. When a change makes a document wrong, fix that
document; do not add a second one.

## Non-negotiable boundaries

- Control plane, not runtime: no PTYs, no multiplexing, no spawning provider
  conversations from the server.
- Fail-open toward providers: `noc hook ingest` always exits 0 and prints
  nothing.
- Supported interfaces only: no terminal or transcript parsing.
- Frontend independence: the Go code has no Zellij logic, and the plugin
  reads only the CLI JSON contract (`noc ls --json`, `noc search --json`,
  `noc show --json`).
- Secure by default: user-only socket, files and directories.
- Never start or install provider runtimes silently.

## The Zellij frontend has one shape

The plugin runs in a host session (`noc-host`) as a sidebar beside a single
guest pane that holds the user's own session (`noc`), attached nested, or
the session of one team (`noc--<team>`); the sidebar moves the guest
pane between them. There is no per-tab sidebar and no generated configuration: everything is in two
layout files the user copies once. `zellij/config/layouts/noc-host.kdl`
starts the pair and holds the plugin with all of its settings, and
`layouts/noc-inner.kdl` holds each inner session's first tab with its tab bar, the Zellij
options that apply to it only inside the pair, and the theme relay's key
bindings. Nothing of noc's goes in the user's `config.kdl`. Do not add a second
deployment. Nothing writes into the user's Zellij configuration except
`make zellij-install-config`, which the user runs themselves; never run it
for them unasked.

## Layout

```
go/        Go module `noc.local/noc` → the `noc` executable
zellij/    Rust/WASM plugin `zellij-noc`
  src/       the plugin
  config/    the static Zellij files users copy (config snippet, layouts, theme)
  install-config.sh  copies them into the user's Zellij config directory
  dev.sh     the isolated dev pair
  e2e/       headless end-to-end test and the fake noc it uses
docs/      documentation and the spec
install.sh installs a released `noc` binary (machines without Homebrew)
packaging/ the Homebrew formula generator; the release writes Formula/noc.rb
```

## Build and verify

```
make check            # go test, go vet, gofmt
make build            # go/bin/noc
make zellij-check     # cargo fmt --check, clippy (wasm + host), cargo test
make zellij           # zellij/target/wasm32-wasip1/release/zellij-noc.wasm
docs/spec/verify/isolated-smoke.sh go/bin/noc
make zellij-e2e       # headless nested-pair test (zellij, tmux, jq)
```

## Daily use and development are separate

Daily use runs released artifacts: `noc` installed with Homebrew from this
repository's own tap (or by `install.sh`), and the
plugin loaded by the release URL in the user's `noc-host.kdl`. Never point the
daily configuration at a local build, and never copy a local build over the
installed binary.

To try a change, use the dev pair: this checkout's binary and plugin, under
the session names `noc-dev` and `noc-dev-host`, with their own Zellij config
directory and their own noc state. `make dev` runs it with this checkout's
`zellij/config/` files; `make dev-mine` adds the user's own `config.kdl`
and themes around them; `make dev-installed` runs it with the user's
installed Zellij configuration. Details are in
`docs/DEVELOPMENT.md`.

Zellij asks the user to approve a plugin's permissions once per plugin
location. Do not write to Zellij's permission cache to skip that prompt.
