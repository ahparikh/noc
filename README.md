# noc

`noc` keeps a registry of your coding-agent conversations. It discovers the
live, local Claude Code and Codex sessions on a machine (including ones it
did not start), puts their identity, state, model, workspace and previews
into one model, and answers: which agents exist, which need me, which are
working, and which have finished a turn.

It is a control plane, not a runtime: it never owns a terminal or runs an
agent itself. It comes in two parts:

- **`noc`**: one binary. A small local server, a CLI with a stable JSON
  contract, and the hook handler the providers call.
- **`zellij-noc`**: a Zellij plugin that shows the registry as a sidebar
  beside your own Zellij session and lets you open, focus, rename and stop
  agents from it.

The name comes from the NOC list: the roster of operatives.

## Install

With Homebrew (macOS or Linux):

```sh
brew tap ahparikh/noc https://github.com/ahparikh/noc
brew install noc
```

`brew upgrade noc` upgrades it. Without Homebrew:

```sh
curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/install.sh | sh
```

which installs the latest released binary for your platform to
`~/.local/bin/noc`, verified against the release's checksums. Run it again
to upgrade; `NOC_VERSION=v0.1.0` pins a version and `NOC_INSTALL_DIR`
changes the directory.

Then:

```sh
noc setup          # install the Claude Code and Codex hook handlers
noc server --bg    # start the registry server
noc doctor         # check the installation
```

## Use

```sh
noc ls                      # Blocked / Working / Review / Quiet
noc search auth older:14d   # by text, and idle for two weeks or more
noc show <id>
noc attach <id>             # open the provider's own UI on an agent
noc rename <id> <name>
noc stop <id>
noc ls --json               # the contract frontends read
```

## Zellij sidebar

The sidebar runs in an outer Zellij session, beside a pane that holds your
own session:

```
┌ outer session: noc-host ───────────────────────────────────────────┐
│ ┌ sidebar ──────┐ ┌ your session: noc ───────────────────────────┐ │
│ │ noc  5 agents │ │   your tabs, panes, keys and theme,          │ │
│ │ Blocked  1    │ │   exactly as you configured them             │ │
│ │ Working  2    │ │                                              │ │
│ └───────────────┘ └──────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────┘
```

It needs Zellij 0.46 and `noc` installed and set up (above).

### Set up, once per machine

Everything the sidebar needs is in two layout files and a theme file, in
your own Zellij configuration (`~/.config/zellij`). You put them
there once and keep them with the rest of your dotfiles. Nothing
regenerates them, and your `config.kdl` is not touched.

| File | What it is |
|---|---|
| `layouts/noc-host.kdl` | The outer session: the sidebar plugin with **all of its settings**, the pane holding your session, and the three keys the outer session owns |
| `layouts/noc-inner.kdl` | What your session gets only while it runs beside the sidebar: its first tab, Zellij options, and two key bindings the sidebar uses for dimming. Edit freely |
| `themes/noc-blue.kdl` | A blue theme and a twin with no focused frame. Your session wears the first while it has the keyboard and the twin while the sidebar does, so only one side looks focused |

**With the script**, straight from GitHub:

```sh
curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash
```

Add `-s -- --check` after `bash` to see what it would do without changing
anything. It only copies those three files; one that is already there and
differs is kept as `<name>.bak`. From a checkout the same script is
`make zellij-install-config`.

**By hand, or by an agent.** These steps are the whole installation:

1. Create `~/.config/zellij/layouts/` and `~/.config/zellij/themes/` if they
   do not exist.
2. Copy [`zellij/config/layouts/noc-host.kdl`](zellij/config/layouts/noc-host.kdl)
   to `~/.config/zellij/layouts/noc-host.kdl`.
3. Copy [`zellij/config/layouts/noc-inner.kdl`](zellij/config/layouts/noc-inner.kdl)
   to `~/.config/zellij/layouts/noc-inner.kdl`.
4. Copy [`zellij/config/themes/noc-blue.kdl`](zellij/config/themes/noc-blue.kdl)
   to `~/.config/zellij/themes/noc-blue.kdl`. To keep your own theme
   instead, see "Dimming" in [docs/ZELLIJ.md](docs/ZELLIJ.md).
5. Do not edit `config.kdl`: nothing of noc's goes there.
6. If `noc` is not on the `PATH` Zellij starts with, set `noc_bin` in
   `noc-host.kdl` to its absolute path (`command -v noc`).
7. Check that Zellij accepts the result: `zellij setup --check`.

The settings live in the `plugin` block of `noc-host.kdl`:

```kdl
plugin location="https://github.com/ahparikh/noc/releases/download/v0.1.0/zellij-noc.wasm" {
    noc_bin "noc"
    guest_session "noc"
    icons "unicode"
    update_check "true"
}
```

### Start it

From a plain terminal, not inside Zellij:

```sh
zellij attach --create noc-host options --default-layout noc-host
```

This one line starts the sidebar beside your session, or attaches to it
when it is already running, so make it an alias:

```sh
alias znoc='zellij attach --create noc-host options --default-layout noc-host'
```

The first time, the sidebar shows Zellij's permission prompt for the
plugin: press `Alt m` to move to it and answer `y`.

### Use it

| Key | Action |
|---|---|
| `Alt m` | Move the keyboard to the sidebar, or back to your session |
| `Alt o` | Hide the sidebar, or show it |
| `Alt d` | Detach. Both sessions keep running; the start line attaches again |

In the sidebar: `j`/`k` move, `Enter` opens the agent in a tab of your
session, `/` searches, `n` renames, `x` stops, `?` lists every key. To end
both sessions, quit your session or run `zellij kill-session noc`.

### Upgrade the plugin

Zellij downloads a plugin URL once and keeps it, so the URL names the
version. When a newer release exists the sidebar's top line shows
`↑ vX.Y.Z`. To upgrade, change the version in the plugin URL (once in
`noc-host.kdl`, twice in `noc-inner.kdl`) and restart the pair:

```sh
sed -i.bak 's|/download/v[0-9.]*/zellij-noc.wasm|/download/vX.Y.Z/zellij-noc.wasm|' \
  ~/.config/zellij/layouts/noc-host.kdl ~/.config/zellij/layouts/noc-inner.kdl
```

Zellij asks for the plugin's permissions again, because its location
changed.

[docs/ZELLIJ.md](docs/ZELLIJ.md) has the rest: every setting, all the keys,
dimming, settings for your session only, and troubleshooting.

## Documentation

| | |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | What noc is and how it is built |
| [docs/PROVIDERS.md](docs/PROVIDERS.md) | Claude Code and Codex integration |
| [docs/ZELLIJ.md](docs/ZELLIJ.md) | The Zellij frontend |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Build, test, the dev pair, releasing |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Design decisions |
| [docs/ROADMAP.md](docs/ROADMAP.md) | What is planned |
| [docs/spec/](docs/spec/README.md) | The behavioral specification |

## Develop

```sh
make check && make build          # Go: tests, vet, gofmt, go/bin/noc
make zellij-check && make zellij  # Rust: fmt, clippy, tests, the .wasm
make dev                          # run this checkout as an isolated pair
make dev-installed                # the same, with your installed Zellij config
```

`make dev` never touches the installed binary, your Zellij configuration or
your registry. See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).
