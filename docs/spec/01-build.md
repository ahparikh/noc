# 01 — Repository, toolchains, build, and release

Scope: the layout of the repository, language and toolchain pins,
dependencies, every Makefile target, build metadata, the artifacts a build
and a release produce, continuous integration, the release workflow, the
binary installer, and the development pair. The plugin's own configuration
and the host session are specified in [08-zellij-host.md](08-zellij-host.md);
the gates are collected in [09-verification.md](09-verification.md).

Sources: `Makefile`, `go/Makefile`, `go/go.mod`,
`go/internal/buildinfo/buildinfo.go`, `go/cmd/noc/main.go`,
`zellij/Makefile`, `zellij/Cargo.toml`, `zellij/rust-toolchain.toml`,
`zellij/dev.sh`, `zellij/config/`, `install.sh`, `.gitignore`,
`.github/workflows/ci.yml`, `.github/workflows/release.yml`.

## 1. Repository layout

```text
.
├── AGENTS.md                    agent bootstrap and scope (read first)
├── README.md
├── Makefile                     dispatcher: delegates to go/ and zellij/
├── install.sh                   installs a released `noc` binary
├── .github/workflows/           ci.yml (gates), release.yml (tagged releases)
├── docs/                        architecture, providers, decisions, and this spec (docs/spec/)
├── go/                          the Go module: one executable, `noc`
│   ├── Makefile
│   ├── go.mod, go.sum           module noc.local/noc
│   ├── cmd/noc/main.go          the only entrypoint
│   ├── pkg/api/v1/              public client-facing JSON types
│   └── internal/                private implementation packages
│       ├── attach/              provider attach and stop planning (CLI side)
│       ├── buildinfo/           Version, Commit, Date
│       ├── cli/                 command dispatch, rendering, server lifecycle
│       ├── config/              path resolution and environment overrides
│       ├── doctor/              read-only diagnostics
│       ├── hookconfig/          structured edits of provider hook files
│       ├── hooks/               hook normalization and outbox projection
│       ├── identity/            UUID generation
│       ├── localapi/            Unix-socket HTTP server and client
│       ├── proclineage/         hook process to provider ancestor PID
│       ├── provider/            adapter contract, bounded command runner
│       │   ├── claude/          Claude roster adapter
│       │   └── codex/           Codex app-server adapter, WebSocket, proxy
│       ├── reconcile/           identity and liveness reconciliation
│       ├── registry/            registry interface and in-memory store
│       ├── server/              server composition, discovery, health, retention
│       ├── setup/               setup, check, uninstall orchestration
│       ├── storage/sqlite/      migrations, store, outbox
│       └── workspace/           read-only Git inspection
└── zellij/                      the Rust/WASM plugin `zellij-noc`
    ├── Makefile, Cargo.toml, Cargo.lock, rust-toolchain.toml
    ├── src/                     main, app, model, view, details, panel, panes, guest
    ├── config/                  what a user copies: layouts/, themes/
    ├── dev.sh                   the isolated development pair
    └── e2e/                     nested.sh (headless end-to-end test), fake-noc
```

#### BLD-001 Two independent project roots
- **Behavior:** `go/` and `zellij/` MUST each have their own language
  manifest and Makefile and MUST build independently. `zellij/` MUST NOT
  import or mirror Go packages or declare a path dependency outside its own
  directory; it depends only on the CLI JSON contract
  ([02-api.md](02-api.md)).
- **Source:** `go/go.mod`; `zellij/Cargo.toml`
- **Decision:** D-018
- **Verified by:** MANUAL: `grep -n 'path *=' zellij/Cargo.toml` prints
  nothing. The plugin CI job builds and tests `zellij/` without a Go
  toolchain step (`.github/workflows/ci.yml`).

#### BLD-002 Go `internal` boundary
- **Behavior:** Every implementation package MUST live under
  `go/internal/...`, which the compiler keeps private. The only packages
  outside it MUST be the entrypoint `go/cmd/noc` and the public
  `go/pkg/api/v1`.
- **Source:** `go/cmd/noc/main.go`; `go/pkg/api/v1/types.go`
- **Verified by:** MANUAL: `cd go && go list ./... | grep -v /internal/`
  prints exactly `noc.local/noc/cmd/noc` and `noc.local/noc/pkg/api/v1`
  (audit A-7 in [09-verification.md](09-verification.md)).

#### BLD-003 Module path
- **Behavior:** The Go module path MUST be `noc.local/noc`. Every import of
  a package in this repository and every `-X` build stamp uses that prefix.
- **Source:** `go/go.mod`; `go/Makefile` (`PKG`)
- **Decision:** D-018
- **Verified by:** `make check` (the module does not compile with a
  mismatched path); MANUAL: `head -1 go/go.mod`.

## 2. Toolchains and dependencies

| Component | Pin or minimum | Where |
|---|---|---|
| Go | `go 1.25.0` minimum, no `toolchain` line | `go/go.mod` |
| SQLite driver | `modernc.org/sqlite v1.58.0` (pure Go) | `go/go.mod` |
| Go indirect dependencies | `dustin/go-humanize`, `google/uuid`, `mattn/go-isatty`, `ncruces/go-strftime`, `remyoudompheng/bigfft`, `golang.org/x/sys`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, at the versions in `go.mod` | `go/go.mod` |
| Rust | channel `1.98.1`, profile `minimal`, components `clippy` and `rustfmt`, target `wasm32-wasip1` | `zellij/rust-toolchain.toml` |
| Rust crate | `rust-version = "1.98"`, edition 2021, `publish = false` | `zellij/Cargo.toml` |
| Zellij plugin API | `zellij-tile = "=0.46.0"` (exact pin) | `zellij/Cargo.toml` |
| Rust dependencies | `serde 1.0` (derive), `serde_json 1.0`, `unicode-width 0.2` | `zellij/Cargo.toml` |
| Claude Code, Codex CLI | no version pin; capabilities are probed at run time ([05-providers.md](05-providers.md)) | — |

#### BLD-010 Pure-Go SQLite
- **Behavior:** The SQLite driver MUST be `modernc.org/sqlite` through
  `database/sql`, so the module builds with `CGO_ENABLED=0` and the binary
  is one self-contained file.
- **Source:** `go/go.mod`; `go/internal/storage/sqlite/store.go`
- **Decision:** D-022
- **Verified by:** MANUAL: `make dist VERSION=dev` builds all four platform
  binaries with `CGO_ENABLED=0` (BLD-042). The release workflow runs the
  same target on every tag.

#### BLD-011 Zellij API pinned exactly to the host version
- **Behavior:** `zellij-tile` MUST be pinned with `=` to the Zellij version
  the plugin is built for, currently `0.46.0`. The pin and the supported
  host version MUST be changed together.
- **Source:** `zellij/Cargo.toml`
- **Decision:** D-020
- **Verified by:** MANUAL: `grep zellij-tile zellij/Cargo.toml` shows
  `"=0.46.0"` and `zellij --version` reports the same version.

#### BLD-012 Rust toolchain resolution
- **Behavior:** `zellij/Makefile` MUST use `$HOME/.cargo/bin/cargo` when
  that file exists and `cargo` from `PATH` otherwise; setting `CARGO`
  overrides both. `zellij/rust-toolchain.toml` pins the channel, components
  and target, so a rustup-managed `cargo` selects and installs them without
  further configuration.
- **Source:** `zellij/Makefile` (`CARGO`); `zellij/rust-toolchain.toml`
- **Verified by:** MANUAL: `make -C zellij -n build` prints the resolved
  cargo path followed by `build --release --target wasm32-wasip1`. CI
  exercises the override with `CARGO=cargo`.

#### BLD-013 Release profile
- **Behavior:** The plugin's release profile MUST set `codegen-units = 1`,
  `lto = true` and `strip = true`.
- **Source:** `zellij/Cargo.toml` (`[profile.release]`)
- **Verified by:** MANUAL: read `zellij/Cargo.toml`.

## 3. Build metadata

#### BLD-020 Version string
- **Behavior:** `buildinfo.Version` MUST default to `"dev"`, `Commit` to
  `"none"` and `Date` to `"unknown"`. `buildinfo.String()` MUST return
  `Version` when `Commit` is empty or `"none"`, and `"<Version> (<Commit>)"`
  otherwise. `noc version` prints that string ([03-cli.md](03-cli.md)).
- **Source:** `go/internal/buildinfo/buildinfo.go`
- **Verified by:** `go/internal/buildinfo/buildinfo_test.go` TestString

#### BLD-022 Process entrypoint
- **Behavior:** `main` MUST create a context that is cancelled on `SIGINT`
  or `SIGTERM`, call `cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)`, and
  exit with its return value. All behavior lives in `internal/cli`.
- **Source:** `go/cmd/noc/main.go`
- **Verified by:** indirectly by `go/internal/cli/run_test.go`, which calls
  `cli.Run`; `main` itself has no test.

#### BLD-041 Builds are stamped
- **Behavior:** `make build` and `make dist` MUST link with
  `-ldflags '-s -w -X <pkg>.Version=$(VERSION) -X <pkg>.Commit=$(COMMIT) -X <pkg>.Date=$(DATE)'`,
  where `<pkg>` is `noc.local/noc/internal/buildinfo`. `VERSION` defaults to
  `dev`. `COMMIT` defaults to `git rev-parse --short HEAD`, or `none` when
  that fails. `DATE` defaults to the current UTC time as
  `YYYY-MM-DDTHH:MM:SSZ`. Each can be overridden on the command line. The
  root Makefile MUST pass `VERSION` through to `go/Makefile`.
- **Source:** `go/Makefile` (`LDFLAGS`, `build`, `dist`); `Makefile` (`build`, `dist`)
- **Verified by:** MANUAL: `make build VERSION=v9.9.9 && go/bin/noc version`
  prints `v9.9.9 (<short commit>)`; a plain `make build` prints
  `dev (<short commit>)`. No automated test.

## 4. Makefile targets

### 4.1 Root `Makefile`

| Target | Does |
|---|---|
| `build` | `make -C go build VERSION=$(VERSION)` |
| `test` | `make -C go test` |
| `check` | `make -C go check` (Go only) |
| `fmt` | `make -C go fmt` |
| `clean` | `make -C go clean`, `make -C zellij clean`, then removes `dist/` |
| `zellij` | `make -C zellij build` |
| `zellij-check`, `zellij-fmt`, `zellij-test`, `zellij-clean`, `zellij-e2e` | the matching target in `zellij/` (`check`, `fmt`, `test`, `clean`, `e2e`) |
| `dev` | depends on `build` and `zellij`, then runs `zellij/dev.sh start` |
| `dev-stop` | `zellij/dev.sh stop` |
| `dist` | release artifacts in `dist/` (BLD-042, BLD-043) |

#### BLD-030 The root Makefile is a dispatcher
- **Behavior:** `build`, `test`, `check`, `fmt` and every `zellij*` target
  MUST only delegate to `go/` or `zellij/`. Go work uses the unprefixed
  targets and plugin work the `zellij*` targets; `make check` MUST NOT run
  plugin checks. The root Makefile's own logic is limited to `clean`
  (removing `dist/`), `dev`/`dev-stop` (running `zellij/dev.sh`) and `dist`.
- **Source:** `Makefile`
- **Verified by:** MANUAL: `make -n check` shows only the `go/` check
  commands; `make -n zellij-check` shows only cargo commands.

#### BLD-051 The repository is its own Homebrew tap
- **Behavior:** `packaging/homebrew-formula.sh <tag> <checksums.txt>` MUST
  print a formula `Noc` whose version is the tag without its `v`, with one
  `url` and `sha256` per platform (`darwin` and `linux`, `arm64` and
  `amd64`) naming the release asset `noc-<os>-<arch>` and the checksum
  listed for it, and which installs that file as `noc`. It builds nothing
  and installs nothing else: the Zellij files are the user's own
  configuration, set up separately
  ([08-zellij-host.md](08-zellij-host.md) ZE-016).
  It MUST exit 2 when the tag does not start with `v` and a digit, and 1
  with `<file> has no checksum for <asset>` when one is missing. The
  release workflow writes this to `Formula/noc.rb`, commits it to `main` as
  `Formula: noc <tag>` when it changed, and pushes. `Formula/noc.rb` is
  never edited by hand and does not exist before the first release. Users
  tap the repository by URL:
  `brew tap ahparikh/noc https://github.com/ahparikh/noc`, then
  `brew install noc`.
- **Source:** `packaging/homebrew-formula.sh`; `.github/workflows/release.yml`
- **Decision:** D-058
- **Verified by:** MANUAL: `packaging/homebrew-formula.sh v0.1.0 dist/checksums.txt | ruby -c` after `make dist VERSION=v0.1.0`; the install itself after a release

#### BLD-050 No install target
- **Behavior:** No Makefile MUST define an `install` target for the
  binary, and none writes outside the repository except
  `make zellij-install-config`, which the user runs to copy the Zellij
  files into their own configuration
  ([08-zellij-host.md](08-zellij-host.md) ZE-016). Daily use installs a
  released binary with Homebrew (BLD-051) or `install.sh` (BLD-046) and
  loads the released plugin by URL (BLD-048); development runs the checkout
  through `make dev` (BLD-049).
- **Source:** `Makefile`; `go/Makefile`; `zellij/Makefile`
- **Verified by:** MANUAL: `grep -n '^install' Makefile go/Makefile zellij/Makefile`
  prints nothing.

### 4.2 `go/Makefile`

Variables: `GO` (`go`), `VERSION` (`dev`), `COMMIT`, `DATE` (BLD-041),
`DIST` (`go/dist` when run directly; the root passes `dist/`), `PLATFORMS`
(`darwin/arm64 darwin/amd64 linux/amd64 linux/arm64`).

| Target | Does |
|---|---|
| `build` | `mkdir -p bin`, then a stamped `go build -o bin/noc ./cmd/noc` |
| `dist` | one stamped, static `noc-<os>-<arch>` per platform into `$(DIST)` (BLD-042) |
| `test` | `go test ./...` |
| `check` | `test`, then `go vet ./...`, then the `gofmt` check (BLD-032) |
| `fmt` | `go fmt ./...` |
| `clean` | `go clean`, then removes `bin` and `dist` |

#### BLD-032 Go check gate
- **Behavior:** `make check` MUST run `go test ./...`, then `go vet ./...`,
  then `gofmt -l .`, and MUST fail when a test fails, when vet reports a
  problem, or when `gofmt -l` lists any file; in the last case it prints
  `gofmt needed:` followed by the file names. It does not run the race
  detector or any other linter.
- **Source:** `go/Makefile` (`check`)
- **Verified by:** `make check`, run by CI on Linux and macOS (BLD-044).

### 4.3 `zellij/Makefile`

Variables: `CARGO` (BLD-012), `WASM_TARGET` (`wasm32-wasip1`).

| Target | Does |
|---|---|
| `build` | `cargo build --release --target wasm32-wasip1`, producing `target/wasm32-wasip1/release/zellij-noc.wasm` |
| `check` | `fmt-check`, `lint`, `test` |
| `fmt`, `fmt-check` | `cargo fmt --all`; `cargo fmt --all -- --check` |
| `lint` | `cargo clippy --target wasm32-wasip1 -- -D warnings`, then `cargo clippy --all-targets -- -D warnings` |
| `test` | `cargo test` on the host target |
| `e2e` | depends on `build`, then runs `e2e/nested.sh` |
| `dev`, `dev-stop` | depends on `build`, then `./dev.sh start`; `./dev.sh stop` |
| `clean` | `cargo clean` |

#### BLD-040 Plugin check gate
- **Behavior:** `make zellij-check` MUST fail on formatting drift, on any
  clippy warning for the WASM target or for the host targets including
  tests, and on any failing host unit test. Clippy on the WASM target stands
  in for a separate `cargo check`.
- **Source:** `zellij/Makefile` (`check`, `fmt-check`, `lint`, `test`)
- **Verified by:** `make zellij-check`, run by CI on Linux (BLD-044).

## 5. Artifacts

| Artifact | Path | Ignored by Git |
|---|---|---|
| Development binary | `go/bin/noc` | yes (`/go/bin/`) |
| Plugin | `zellij/target/wasm32-wasip1/release/zellij-noc.wasm` | yes (`/zellij/target/`) |
| Rendered development config | `zellij/target/dev/<name>/` | yes (`/zellij/target/`) |
| Release artifacts | `dist/noc-<os>-<arch>`, `dist/zellij-noc.wasm`, `dist/checksums.txt` | yes (`/dist/`, `/go/dist/`) |

`.gitignore` also ignores `/.idea/`, `/.vscode/`, `.DS_Store` and
`coverage.out`.

#### BLD-042 `make dist` builds everything a release carries
- **Behavior:** `make dist` MUST delete and recreate `dist/`, then produce
  exactly these files in it: `noc-darwin-arm64`, `noc-darwin-amd64`,
  `noc-linux-amd64`, `noc-linux-arm64`, `zellij-noc.wasm` and
  `checksums.txt`. Each binary MUST be built with `CGO_ENABLED=0`, the
  matching `GOOS`/`GOARCH`, `-trimpath` and the stamps of BLD-041. The
  plugin is a copy of the release build. `checksums.txt` MUST hold one
  SHA-256 line per binary and one for the plugin, in `shasum -a 256` format
  (`<hex>  <file name>`), written by `shasum` or, when that is absent,
  `sha256sum`.
- **Source:** `Makefile` (`dist`); `go/Makefile` (`dist`, `PLATFORMS`)
- **Decision:** D-021, D-022
- **Verified by:** MANUAL: `make dist VERSION=dev && ls dist && (cd dist && shasum -a 256 -c checksums.txt)`.
  The release workflow runs the target on every tag (BLD-045).

#### BLD-043 A release version must match the shipped plugin URL
- **Behavior:** When `VERSION` starts with `v` followed by a digit,
  `make dist` MUST first check that every plugin location in
  `zellij/config/layouts/noc-host.kdl` and
  `zellij/config/layouts/noc-inner.kdl` is
  `…/releases/download/<VERSION>/zellij-noc.wasm`, and that each file has
  one. When that is not so, the target MUST print
  `<file> does not load the plugin from <VERSION>; update the plugin URL there (and in the docs) first`
  to standard error and exit non-zero before building anything. Any other
  `VERSION` (such as `dev`) skips the check.
- **Source:** `Makefile` (`dist`); `zellij/config/layouts/`
- **Verified by:** MANUAL: `make dist VERSION=v0.0.0-none` fails with that
  message and leaves `dist/` untouched. No automated test.

## 6. Distribution and installation

#### BLD-046 `install.sh` selects and downloads one release binary
- **Behavior:** `install.sh` MUST map `uname -s` `Darwin`/`Linux` to
  `darwin`/`linux` and `uname -m` `arm64`/`aarch64` to `arm64` and
  `x86_64`/`amd64` to `amd64`, and exit 1 with
  `install.sh: unsupported operating system: <name>` or
  `install.sh: unsupported architecture: <name>` for anything else. It MUST
  download `noc-<os>-<arch>` and `checksums.txt` with `curl -fsSL` from the
  latest release of the repository, or from the release named by
  `NOC_VERSION` when set. `NOC_REPO` replaces the repository and
  `NOC_BASE_URL` replaces the whole download base. It requires `curl` and
  either `shasum` or `sha256sum`, and exits 1 naming the missing tool. It
  MUST NOT install the Zellij plugin, edit provider or Zellij configuration,
  or start a server.
- **Source:** `install.sh`
- **Verified by:** MANUAL: serve a `make dist` directory over HTTP and run
  `NOC_BASE_URL=http://127.0.0.1:<port> NOC_INSTALL_DIR=<temp dir> sh install.sh`.
  No automated test.

#### BLD-047 `install.sh` verifies before it installs
- **Behavior:** The script MUST compute the SHA-256 of the downloaded
  binary and compare it with the `checksums.txt` entry whose file name is
  the asset name. A missing entry exits 1 with
  `install.sh: checksums.txt has no entry for <asset>`; a mismatch exits 1
  with `install.sh: checksum mismatch for <asset> (expected <want>, got <got>)`.
  In both cases nothing is installed. On success it MUST create the install
  directory (`NOC_INSTALL_DIR`, default `$HOME/.local/bin`), set the file
  mode to `0755`, move the file to `<dir>/.noc.new` and rename that to
  `<dir>/noc`, so an existing binary is replaced in one step. It then
  prints `Installed <dir>/noc (<noc version output>)`, a note when the
  directory is not on `PATH` or another `noc` comes first on it, and the
  next steps (`noc setup`, `noc server restart`, `noc doctor`). Downloads go
  to a temporary directory that is removed on exit.
- **Source:** `install.sh`
- **Verified by:** MANUAL: as BLD-046, then repeat after altering one byte
  of the served binary and expect the mismatch message and no change in the
  install directory. No automated test.

#### BLD-048 The plugin is loaded by pinned release URL
- **Behavior:** The shipped layouts MUST name the plugin by the URL of a
  tagged release asset (`.../releases/download/<tag>/zellij-noc.wasm`).
  Nothing in this repository copies the plugin into a user's Zellij
  configuration directory, and nothing is generated there: the user copies
  the layout files, by hand or with the installer
  ([08-zellij-host.md](08-zellij-host.md) ZE-016). Upgrading the plugin
  means changing the tag in that URL, in both layouts.
- **Source:** `zellij/config/layouts/noc-host.kdl`; `zellij/config/layouts/noc-inner.kdl`
- **Decision:** D-034
- **Verified by:** BLD-043 keeps the URL and the release tag in step at
  release time. Loading by URL is MANUAL: start the pair from the shipped
  configuration and confirm the sidebar loads.

#### BLD-049 The development pair is isolated from daily use
- **Behavior:** `make dev` MUST build `go/bin/noc` and the plugin and run
  `zellij/dev.sh start`, which renders a Zellij configuration directory
  under `zellij/target/dev/<name>/` from `zellij/config/` and starts (or
  attaches to) the host session `<name>-host` around the inner session
  `<name>`. `<name>` is `NOC_DEV_NAME`, default `noc-dev`; it MUST be made
  only of letters, digits, `.`, `_` and `-`, and `noc` MUST be refused with
  `dev.sh: NOC_DEV_NAME=noc is the daily pair; pick another name`. The
  rendered configuration MUST load the plugin from the checkout's build by
  `file:` location and set `noc_bin` to a generated wrapper that runs
  `go/bin/noc` with `NOC_STATE_DIR` and `NOC_RUNTIME_DIR` pointing at
  directories named `<name>` under the XDG state and cache homes. With
  `--fake` the wrapper is replaced by `zellij/e2e/fake-noc`. `start` MUST
  refuse to run inside Zellij (`run this from a plain terminal, not inside
  Zellij`) and MUST fail naming the missing build when the plugin or
  `go/bin/noc` is absent. `make dev-stop` ends both sessions. The script
  MUST NOT read or write the user's Zellij configuration directory or the
  daily registry.
- **Source:** `zellij/dev.sh`; `Makefile` (`dev`, `dev-stop`)
- **Verified by:** `zellij/e2e/nested.sh` renders and starts the `--fake`
  pair under the name `noc-e2e` and drives it (`make zellij-e2e`). The real
  wrapper and its state directories are MANUAL:
  `zellij/dev.sh render && cat zellij/target/dev/noc-dev/noc`.

## 7. Continuous integration and release

#### BLD-044 CI runs the gates
- **Behavior:** On every push to `main` and every pull request, CI MUST run
  two jobs. The `go` job runs on both `ubuntu-latest` and `macos-latest`,
  with the Go version taken from `go/go.mod`: `make check`, `make build`,
  then `docs/spec/verify/isolated-smoke.sh go/bin/noc`. The `plugin` job
  runs on `ubuntu-latest` with the toolchain from
  `zellij/rust-toolchain.toml`: `make zellij-check CARGO=cargo`, then
  `make zellij CARGO=cargo`. The workflow's token is read-only
  (`contents: read`). CI does not run the Zellij end-to-end test.
- **Source:** `.github/workflows/ci.yml`
- **Verified by:** the workflow itself; each step can be reproduced
  locally with the same commands.

#### BLD-045 A version tag publishes a release
- **Behavior:** Pushing a tag matching `v*` MUST run the release workflow
  on `ubuntu-latest`: `make check`, `make zellij-check CARGO=cargo`,
  `make dist VERSION=<tag> CARGO=cargo`, then
  `gh release create <tag> dist/* --title <tag> --generate-notes`. A failing
  gate or a failing `dist` (including the URL check of BLD-043) MUST stop
  the workflow before anything is published. The published assets are
  exactly the files of BLD-042, which are the names `install.sh`, the
  Homebrew formula (BLD-051) and the shipped layouts expect. After
  publishing it MUST update the formula on `main` (BLD-051).
- **Source:** `.github/workflows/release.yml`
- **Verified by:** MANUAL: the workflow run for a pushed tag. No test
  exercises it before a tag exists.

## Design choices

- **One binary, many modes.** Hooks, client and server are one executable
  so they always share a version (D-021).
- **A Makefile per project and a thin root dispatcher.** `go` and `cargo`
  stay independent, and either project can be built from its own directory
  (D-018).
- **Released artifacts for daily use, the checkout for development.** A
  released binary and a URL-pinned plugin give every installation a
  version that `noc version` and the config file state exactly. `make dev`
  runs the working tree beside it under different session names, a
  different configuration directory and separate state, so a broken build
  cannot disturb the daily pair.
- **The rustup `cargo` is preferred by path.** A system Rust that is older
  than the pinned toolchain would otherwise be picked up from `PATH`.

## Verification procedures

```sh
make check                         # go test, go vet, gofmt
make zellij-check                  # cargo fmt --check, clippy (wasm + host), cargo test
make build && go/bin/noc version   # dev (<commit>)
make zellij                        # zellij/target/wasm32-wasip1/release/zellij-noc.wasm
make dist VERSION=dev              # four binaries, the plugin, checksums.txt in dist/
(cd dist && shasum -a 256 -c checksums.txt)
(cd go && go list ./... | grep -v /internal/)   # BLD-002
```

## Known limitations

- **The installer and the release path have no automated test (BLD-042,
  BLD-043, BLD-045, BLD-046, BLD-047).** CI builds and tests the code but
  never runs `make dist` or `install.sh`; the first run of either for a
  given version is the release itself.
- **CI does not run the Zellij end-to-end test (BLD-044).**
  `make zellij-e2e` needs Zellij, tmux and plugin permissions granted for
  the build path, so it is a local gate
  ([09-verification.md](09-verification.md)).
- **`Date` is stamped but never shown (BLD-041).** `buildinfo.String()`
  uses only `Version` and `Commit`.
- **The commit stamp depends on Git (BLD-041).** A build from a source tree
  without Git metadata reports `Commit` as `none`, and `noc version` then
  prints the version alone.
- **`install.sh` trusts the release for its own checksum (BLD-047).**
  `checksums.txt` is downloaded from the same location as the binary and is
  not signed; the check detects a corrupted or mismatched download, not a
  compromised release.
