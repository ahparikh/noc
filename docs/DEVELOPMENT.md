# Development

How to build and test noc, how to try a change without disturbing the copy
you use every day, and how to cut a release.

## Toolchains

| Tool | Version | Used for |
|---|---|---|
| Go | as in `go/go.mod` | the `noc` binary |
| Rust | pinned in `zellij/rust-toolchain.toml`, target `wasm32-wasip1` | the plugin |
| Zellij | 0.46 | running the plugin (`zellij-tile` is pinned to the same version) |
| tmux, jq | any recent | the end-to-end test only |

## Build and check

```sh
make check            # go test, go vet, gofmt
make build            # go/bin/noc
make zellij-check     # cargo fmt --check, clippy (wasm + host), cargo test
make zellij           # zellij/target/wasm32-wasip1/release/zellij-noc.wasm
docs/spec/verify/isolated-smoke.sh go/bin/noc    # end-to-end, in a temp dir
make zellij-e2e       # the nested pair, headless (below)
```

CI runs the first five on every push and pull request
(`.github/workflows/ci.yml`).

## Daily use and development are separate

| | Daily use | Development |
|---|---|---|
| `noc` binary | the release, installed with Homebrew (or `install.sh`) | `go/bin/noc` from this checkout |
| Plugin | the release URL in `~/.config/zellij/layouts/noc-host.kdl` | `zellij/target/…/zellij-noc.wasm` |
| Sessions | `noc`, `noc-host`, a `noc--<team>` per team | `noc-dev`, `noc-dev-host`, a `noc-dev--<team>` per team |
| Zellij config | `~/.config/zellij/` | `zellij/target/dev/noc-dev/` |
| noc state | the platform default | `~/.local/state/noc-dev`, `~/.cache/noc-dev` |

The two never share a session, a config file, a registry database or a
socket, so both can run at once. A change reaches daily use only through a
release.

### The dev pair

```sh
make dev              # build both, then start (or attach to) the dev pair
make dev-installed    # the same, with your installed Zellij configuration
make dev-stop         # end the dev sessions (teams' too) and the dev server
zellij/dev.sh status
```

The two start commands differ only in where the Zellij configuration comes
from:

| | `make dev` | `make dev-installed` |
|---|---|---|
| Tests | the new plugin with the new configuration files | the new plugin with the configuration you use every day |
| `config.kdl` | a plain one, with nothing of yours | yours |
| Layouts | `zellij/config/layouts/` | your installed `noc-host.kdl` and `noc-inner.kdl` |
| Theme | the shipped `noc-blue` pair | yours |
| Other files | none of yours | yours, linked |

Both replace the plugin location, `noc_bin` and the session names, and both
use the dev registry. `make dev-installed` needs noc's files in your Zellij
configuration (`make zellij-install-config`) and stops with a message when
they are not there. Only one of the two runs at a time: stop the pair to
switch.

`zellij/dev.sh` renders the chosen files into
`zellij/target/dev/noc-dev/` with these substitutions: the plugin location
becomes the local build, the session names become `noc-dev` and
`noc-dev-host`, `room_layout` names the rendered `noc-inner.kdl` by its
path, and `noc_bin` becomes a wrapper that runs `go/bin/noc` with
its own state and runtime directories. It then starts Zellij with
`--config-dir` pointing there. Because `make dev` renders the shipped files,
what you test is what users copy. It never writes to your own Zellij
configuration; `make zellij-install-config` is the only thing that does
(see [ZELLIJ.md](ZELLIJ.md#installing-the-files)).

`make dev` also starts the dev pair's own registry server (and `make
dev-stop` ends it). That server discovers the Claude Code and Codex sessions
that are live on the machine, so the dev sidebar lists your real agents
within a few seconds, from a database and socket of its own. Provider hooks
installed by `noc setup` belong to the daily binary, so the dev server
learns about agents from discovery polling only: states that only hooks
report (waiting for approval, for example) can lag or be missing.

A plain `noc` talks to the daily server. To reach the dev one, use the
wrapper:

```sh
zellij/target/dev/noc-dev/noc ls
zellij/target/dev/noc-dev/noc server status
```

With `make dev` the rendered config turns on full pane frames with rounded
corners and otherwise carries none of your own Zellij settings.

`zellij/dev.sh --fake start` uses `zellij/e2e/fake-noc` (two canned agents)
instead of a real registry, which is enough for most plugin work.

The first time, the dev sidebar shows Zellij's permission prompt for the
local build's path. Answer `y` once.

### End-to-end test

```sh
make zellij-e2e                 # or: zellij/e2e/nested.sh
zellij/e2e/nested.sh --keep     # leave it running: tmux attach -t noc-e2e-tmux
```

It starts the dev pair with the fake noc inside a detached tmux, under the
names `noc-e2e` and `noc-e2e-host`, presses real keys, and checks what
Zellij reports: the pair starts, the sidebar lists agents, `Alt m` moves the
keyboard both ways, the inner theme follows it, `Enter` opens an agent in a
new inner tab and focuses it the second time, the details window opens and
closes, `Alt o` hides and shows the sidebar, `Alt d` detaches and starting
again attaches, and ending the inner session ends the host.

It needs the plugin's permissions already granted for the build path (run
`make dev` once first) and stops with a message if they are not.

## Changing behavior

1. Change the code and its tests.
2. Update the requirement in `docs/spec/` that the change touches, or add
   one. If the user-facing setup or keys change, update `docs/ZELLIJ.md` and
   the files in `zellij/config/`.
3. If it is a design decision, record it in `docs/DECISIONS.md`.
4. Run the checks above.

## Releasing

A release is a tag. Pushing `vX.Y.Z` runs `.github/workflows/release.yml`,
which runs the checks, builds everything, and publishes a GitHub release
with:

- `noc-darwin-arm64`, `noc-darwin-amd64`, `noc-linux-arm64`,
  `noc-linux-amd64`: static binaries, stamped with the version and commit
  (`noc version`)
- `zellij-noc.wasm`
- `checksums.txt` (SHA-256)

It then writes `Formula/noc.rb` on `main`, generated by
`packaging/homebrew-formula.sh` from the release's checksums. The
repository is its own Homebrew tap
(`brew tap ahparikh/noc https://github.com/ahparikh/noc`), so that commit
is what makes `brew upgrade noc` see the release. `Formula/noc.rb` is never
edited by hand, and does not exist before the first release.

Steps:

1. Set the new version in the plugin URL in
   `zellij/config/layouts/noc-host.kdl` and `noc-inner.kdl`, in the URL in
   `README.md` and `docs/ZELLIJ.md`, and in `zellij/Cargo.toml`.
   `make dist VERSION=vX.Y.Z` refuses to build if a layout or `Cargo.toml`
   names a different version: the sidebar compares the latest
   release against the version in `Cargo.toml` to decide whether to show
   its `↑ vX.Y.Z` marker.
2. Commit, then `git tag vX.Y.Z && git push origin main vX.Y.Z`.
3. When the workflow finishes, upgrade your own machines. Each running
   sidebar shows `↑ vX.Y.Z` within six hours (or when it next starts):
   - binary: `brew update && brew upgrade noc` (or run `install.sh`
     again), then `noc server restart`; and `noc setup` once if the hooks
     were installed by v0.1.4 or earlier (`noc doctor` says so);
   - plugin: change the version in the plugin URL in your `noc-host.kdl`
     and `noc-inner.kdl`, then
     end the pair (`zellij kill-session noc`) and start it again. Zellij asks for the plugin's permissions again
     because the location changed.

`make dist VERSION=vX.Y.Z` builds the same artifacts locally into `dist/`.
To try the installer against them, serve `dist/` over HTTP and run
`NOC_BASE_URL=http://127.0.0.1:PORT NOC_INSTALL_DIR=/tmp/noc-test sh install.sh`.
