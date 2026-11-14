# 08 — Zellij host integration

This section specifies how the `zellij-noc` plugin integrates with Zellij:
its roles and lifecycle, permissions, polling, pipes and settings (`ZH`); the
nested pair of sessions it runs in, with the static configuration, the host
session's keys, the guest pane, keyboard ownership, and how the panel reads
and drives the inner session (`ZX`); and the build, the development pair and
the end-to-end harness (`ZE`).

What the panel draws and what each key in it means are specified in
[07-zellij-panel.md](07-zellij-panel.md); this section starts where a key has
become an intent. The user-facing guide is [../ZELLIJ.md](../ZELLIJ.md), the
developer guide is [../DEVELOPMENT.md](../DEVELOPMENT.md), decisions are in
[../DECISIONS.md](../DECISIONS.md), and how the suites are run is in
[09-verification.md](09-verification.md).

The code is the source of truth. Source files covered:

- `zellij/src/main.rs`: plugin state, constants, permissions, event and pipe dispatch
- `zellij/src/app.rs`: settings (`Config`), validation, key encoding
- `zellij/src/panel.rs`: intents, registry polling, keyboard ownership
- `zellij/src/guest.rs`: reading and driving the inner session, keyboard signals, the relay role
- `zellij/src/panes.rs`: pane records and pure helpers
- `zellij/src/details.rs`: the details window instance
- `zellij/config/layouts/noc-host.kdl`, `zellij/config/layouts/noc-inner.kdl`, `zellij/config/themes/noc-blue.kdl`
- `zellij/dev.sh`, `zellij/e2e/nested.sh`, `zellij/e2e/fake-noc`
- `zellij/Makefile`, `zellij/Cargo.toml`, `zellij/rust-toolchain.toml`, root `Makefile` (Zellij targets)

Terms:

- **Host session**: the outer Zellij session, `noc-host`. One tab, two panes.
- **Inner session**: the user's own Zellij session, `noc`, attached inside the host.
- **Panel**: the plugin instance in the host session's left pane (the sidebar).
- **Guest pane**: the host session's terminal pane that holds a client of the inner session.
- **Relay**: a plugin instance inside the inner session that changes its theme.
- **Details window**: a floating plugin instance in the host session showing one agent.
- **Has the keyboard**: is the focused pane of the host session's tab.

The plugin reads only the `noc` CLI's JSON and Zellij's own pane metadata, and
acts only through `noc` and `zellij` commands and the plugin API (D-034). All
pane, tab, focus and theme state stays in the plugin (D-036).

---

## 1. Lifecycle, roles and permissions

### ZH-001 One build, three roles
- **Behavior:** One WASM build MUST serve three roles, chosen at load by the `role` setting after trimming: `relay` → relay, `details` → details window, anything else or absent → panel. A details instance MUST also read `agent_id` (trimmed; default empty).
- **Source:** `zellij/src/main.rs` (`load`)
- **Decision:** D-055
- **Verified by:** `zellij/e2e/nested.sh` "host session has the sidebar and one guest pane", "D opens the floating details window", "inner session wears the away theme while the sidebar has the keyboard" (one check per role); no unit test

### ZH-002 One permission set for every role
- **Behavior:** Every role MUST request exactly these six permissions at load: `ReadApplicationState`, `ChangeApplicationState`, `OpenTerminalsOrPlugins`, `Reconfigure`, `RunCommands`, `MessageAndLaunchOtherPlugins`. The plugin MUST NOT request `WriteToStdin`.
- **Source:** `zellij/src/main.rs` (`permissions`, `load`)
- **Verified by:** UNTESTED

### ZH-003 Event subscriptions
- **Behavior:** A relay MUST subscribe only to `PermissionRequestResult`. A panel or details instance MUST subscribe to `Key`, `ModeUpdate`, `Mouse`, `PaneUpdate`, `PermissionRequestResult`, `RunCommandResult`, `TabUpdate`, `Timer`, `Visible`.
- **Source:** `zellij/src/main.rs` (`load`)
- **Verified by:** UNTESTED

### ZH-004 Panel: permissions granted
- **Behavior:** On `PermissionRequestResult(Granted)` the panel MUST clear any notice, name its pane `NOC`, synchronise its selectable state (ZX-011), start a registry poll (ZH-009), check for a newer release (ZH-022), and arm a `0.25` s timer if none is armed. Each `Timer` event re-arms the timer, so there is one tick chain.
- **Source:** `zellij/src/main.rs` (`on_granted`, `on_panel_tick`)
- **Verified by:** `zellij/e2e/nested.sh` "sidebar lists the registry's agents" (poll after grant); the rest UNTESTED

### ZH-005 Panel: permissions denied or not yet granted
- **Behavior:** On `PermissionRequestResult(Denied)` the panel MUST set its connection to the error `permissions denied: the panel cannot read the registry or open agents`. While permissions are not granted the panel MUST NOT poll the registry or the inner session, change its selectable state or title, act on the grab pipe, or type into the guest pane. It still records `PaneUpdate`, `TabUpdate` and `ModeUpdate` contents. Enter, `p` and `s` MUST answer with the notice `pane attachment permission was not granted`.
- **Source:** `zellij/src/main.rs` (`on_denied`, `on_pane_update`); `zellij/src/panel.rs` (`refresh`, `require_permission`, `toggle_keyboard`, `sync_panel_selectable`, `sync_panel_title`); `zellij/src/guest.rs` (`poll_guest`, `write_to_guest`)
- **Verified by:** UNTESTED

### ZH-006 Panel tick
- **Behavior:** On each `0.25` s tick the panel MUST, in order: re-arm the timer; synchronise selectable state and title; poll the inner session if due (ZX-020); start a registry poll if one is due; check for a newer release if one is due (ZH-022); and, if any agent is working, advance the spinner and redraw. The tick MUST NOT poll the registry more often than the configured interval.
- **Source:** `zellij/src/main.rs` (`on_panel_tick`)
- **Verified by:** `zellij/src/app.rs` `the_spinner_cycles_and_only_working_agents_need_redraws` (spinner only); the rest UNTESTED

### ZH-007 Mode updates
- **Behavior:** On `ModeUpdate` the panel MUST rebuild its colours from the reported theme, record the host session's name when one is reported, and redraw. The details window rebuilds its colours the same way.
- **Source:** `zellij/src/main.rs` (`on_mode_update`); `zellij/src/details.rs` (`update_details`)
- **Verified by:** UNTESTED

### ZH-008 Rendering
- **Behavior:** `render` MUST print the role's lines joined by `\n` with no trailing newline and flush. A relay MUST print nothing. The panel records the column count it was last rendered at.
- **Source:** `zellij/src/main.rs` (`render`)
- **Verified by:** UNTESTED

## 2. Registry transport

### ZH-009 Polling the registry
- **Behavior:** With the default transport the panel MUST read the registry by running `<noc_bin> ls --json --all`, or `<noc_bin> search --json -- <query>` while a search query is in effect ([07-zellij-panel.md](07-zellij-panel.md) ZK-026), with context `zellij-noc=agent-snapshot` and `zellij-noc-query=<query>` (empty with no query). A result whose `zellij-noc-query` differs from the query now in effect MUST be discarded and the registry read again at once. A poll starts only when the role is panel, the transport is not `pipe`, permissions are granted, and no poll is in flight. Starting a poll sets the next due time to now plus the poll interval. Polls start on grant, on a tick once due, on `Visible(true)`, on the Refresh intent (`r`), when the search query changes, and after a successful rename.
- **Source:** `zellij/src/panel.rs` (`refresh`); `zellij/src/main.rs` (`update_panel`, `on_panel_tick`)
- **Decision:** D-034
- **Verified by:** `zellij/e2e/nested.sh` "sidebar lists the registry's agents"

### ZH-010 Poll results
- **Behavior:** A result with context `agent-snapshot` MUST clear the in-flight flag and redraw. Exit `0` with a valid snapshot applies it and marks the connection `connected · poll`. Exit `0` with invalid UTF-8 sets the error `noc returned non-UTF-8 output: <error>`; a snapshot that does not parse sets the parser's error. Any other exit takes stderr compacted to one line (whitespace collapsed, at most 180 characters followed by `…`): if it matches ZH-011 the connection becomes server-unavailable; if it is empty the error is `noc ls failed with status <code>` (`unknown` when there is no exit code); otherwise the error is the compacted stderr. A failed poll MUST keep the last good list.
- **Source:** `zellij/src/panel.rs` (`on_snapshot_result`); `zellij/src/panes.rs` (`compact_error`)
- **Verified by:** `zellij/src/panes.rs` `compact_error_is_single_line_and_bounded`; `zellij/src/view.rs` `errors_keep_the_last_good_list`; result routing UNTESTED

### ZH-011 Recognising a stopped server
- **Behavior:** An error detail MUST count as "server unavailable" when, case-insensitively, it contains `is the server running?`, or `noc server is not running`, or `dial unix` together with `no such file or directory` or `connection refused`. No other error counts.
- **Source:** `zellij/src/panes.rs` (`is_server_unavailable`)
- **Verified by:** `zellij/src/panes.rs` `recognizes_server_connection_failures_without_hiding_other_errors`

### ZH-012 Command result routing
- **Behavior:** The panel MUST route `RunCommandResult` by the context value under key `zellij-noc`: `agent-snapshot` (ZH-010), `stop-agent`, `rename-agent` (ZH-013), `guest-snapshot` (ZX-021), `guest-open` (ZX-031), `update-check` (ZH-022), `host-info` (ZX-038). Results with no such key or any other value, including `host-action`, MUST be ignored. The details window handles only `agent-details`.
- **Source:** `zellij/src/main.rs` (`on_command_result`); `zellij/src/details.rs` (`update_details`)
- **Verified by:** UNTESTED

### ZH-013 Stop and rename commands
- **Behavior:** The Stop intent MUST run `<noc_bin> stop <id>` with context `stop-agent` plus `zellij-noc-agent=<id>` and `zellij-noc-title=<title>`. On exit `0` the agent is hidden and the notice is `stopped <title>`; otherwise the notice is the compacted stderr, or `could not stop <title>` when stderr is empty. The Rename intent MUST run `<noc_bin> rename <id> <name>` with context `rename-agent` plus `zellij-noc-title=<name>`. On exit `0` the notice is `renamed to <name>` and a registry poll starts; otherwise the notice is the compacted stderr, or `could not rename`. When each intent is raised is specified in [07-zellij-panel.md](07-zellij-panel.md).
- **Source:** `zellij/src/panel.rs` (`run_intent`, `on_stop_result`, `on_rename_result`)
- **Verified by:** `zellij/src/panel.rs` `stop_policy_and_confirmation`, `rename_mode_captures_every_key` (intents only); commands and results UNTESTED

### ZH-014 Pipe transport
- **Behavior:** With `transport "pipe"` the panel MUST NOT run `noc ls`. It MUST accept pipe messages whose name equals `pipe_name` (default `zellij-noc.registry`) and treat each payload as NDJSON registry snapshots; a message with no payload is ignored. Each decoded snapshot is applied with the connection `connected · pipe`; each invalid frame sets the error `invalid registry snapshot: <error>`. With the default transport, messages on that name MUST be ignored.
- **Source:** `zellij/src/main.rs` (`handle_pipe`, `on_registry_payload`); `zellij/src/panel.rs` (`refresh`)
- **Verified by:** UNTESTED (decoder only, see ZH-015)

### ZH-015 Pipe frame decoding
- **Behavior:** Payloads MUST be appended to a buffer. If the buffer exceeds `4194304` bytes it is cleared and one error `pipe frame exceeded the 4194304 byte limit` is reported. Every complete line (ending in `\n`) is trimmed and, if not empty, parsed as one snapshot. A remainder without a newline is parsed too: if it is a valid snapshot it is delivered and the buffer cleared; if not, it is kept for the next payload, except that an empty payload (the sender stopping) reports its parse error and clears the buffer.
- **Source:** `zellij/src/panes.rs` (`decode_pipe_frames`)
- **Verified by:** `zellij/src/panes.rs` `pipe_decoder_handles_fragmented_and_complete_snapshots`, `pipe_decoder_reports_complete_invalid_frames`

### ZH-016 Pipe names by role
- **Behavior:** The panel MUST handle `zellij-noc.grab` (ZX-012) and, in pipe transport, the registry pipe. The relay MUST handle only `zellij-noc.relay` (ZX-019). The details window MUST ignore every pipe message. No pipe handler requests a redraw except a registry payload that produced at least one frame.
- **Source:** `zellij/src/main.rs` (`pipe`, `handle_pipe`)
- **Verified by:** `zellij/e2e/nested.sh` "Alt-m gives the keyboard to the sidebar", "inner session wears the away theme while the sidebar has the keyboard"

## 3. Settings

### ZH-017 Settings and defaults
- **Behavior:** Settings come from the plugin's configuration map. Every value is trimmed and an empty value counts as unset.

  | Key | Default | Meaning |
  |---|---|---|
  | `role` | panel | `relay` or `details` (ZH-001) |
  | `noc_bin` | `noc` | the `noc` executable; a bare name is resolved on the `PATH` Zellij was started with |
  | `icons` | Unicode | `nerd` selects Nerd Font icons; any other value does not |
  | `transport` | poll | `pipe` selects the pipe transport; any other value does not |
  | `pipe_name` | `zellij-noc.registry` | registry pipe name |
  | `poll_interval_ms` | `1000` | registry poll interval; clamped to `250`..`60000`; a value that is not a finite number is ignored |
  | `guest_session` | `noc` | the inner session's name |
  | `home_theme`, `away_theme` | unset | the inner session's theme pair |
  | `away_key`, `home_key` | `Ctrl Alt Shift F11`, `Ctrl Alt Shift F12` | the keys typed into the inner session to switch theme |
  | `update_check` | on | `false` turns the release check (ZH-022) off; any other value does not |
  | `pane_titles` | show | what the frames of the inner session's panes say: `name` names each pane `<host> · <what runs in it>`, `hide` blanks the title, any other value leaves the titles alone (ZX-038) |
  | `group_by` | `status` | `team` groups the sidebar by team; any other value groups it by status ([07-zellij-panel.md](07-zellij-panel.md) ZG-017) |
  | `colors` | built-in | `zellij` makes the panel follow the Zellij theme; any other value keeps the built-in colours ([07-zellij-panel.md](07-zellij-panel.md) ZR-018) |
  | `agent_id` | empty | details role only |

- **Source:** `zellij/src/app.rs` (`Config::from_map`, `Config::default`); `zellij/src/main.rs` (`load`)
- **Verified by:** `zellij/src/app.rs` `parses_bounded_poll_configuration`

### ZH-018 Plain names
- **Behavior:** A session or theme name is plain when it is not empty and every character is an ASCII letter, an ASCII digit, `-`, `_` or `.`.
- **Source:** `zellij/src/app.rs` (`is_plain_name`)
- **Verified by:** `zellij/src/guest.rs` `relay_accepts_plain_theme_names_only`; `zellij/src/app.rs` `reports_unusable_settings_in_plain_words`

### ZH-019 Key specifications
- **Behavior:** A key specification is whitespace-separated tokens: any of `Shift`, `Alt`, `Ctrl`, and exactly one key `F1`..`F12`. Anything else is invalid. It encodes to the bytes a terminal sends, with modifier parameter `1 + Shift(1) + Alt(2) + Ctrl(4)`: `F1`..`F4` are `ESC O P|Q|R|S` unmodified and `ESC [ 1 ; <p> P|Q|R|S` modified; `F5`..`F12` use codes `15, 17, 18, 19, 20, 21, 23, 24` as `ESC [ <code> ~` unmodified and `ESC [ <code> ; <p> ~` modified. `Ctrl Alt Shift F11` is `ESC [ 2 3 ; 8 ~` and `Ctrl Alt Shift F12` is `ESC [ 2 4 ; 8 ~`.
- **Source:** `zellij/src/app.rs` (`encode_key`)
- **Verified by:** `zellij/src/app.rs` `encodes_function_keys_the_way_a_terminal_sends_them`

### ZH-020 Settings problems
- **Behavior:** `Config::problems()` MUST report, in this order:
  1. `guest_session "<value>" is not a plain session name`, when `guest_session` is not plain.
  2. If neither theme is set, nothing more (the keys are not checked).
  3. `home_theme is set but away_theme is not`, or `away_theme is set but home_theme is not`.
  4. With both set: `home_theme "<value>" is not a plain theme name` and the same for `away_theme`, then `home_theme and away_theme are the same theme`.
  5. For `away_key` then `home_key`: `<key> "<value>" is not a function key such as "Ctrl Alt Shift F11"`.
  6. `away_key and home_key are the same key`.

  Values are quoted as Rust debug strings.
- **Source:** `zellij/src/app.rs` (`Config::problems`)
- **Verified by:** `zellij/src/app.rs` `reports_unusable_settings_in_plain_words`

### ZH-021 Showing a settings problem
- **Behavior:** At load the first problem, if any, MUST be stored until the plugin is reloaded. It turns the panel's status dot yellow and is spelled out in the status box, ahead of the inner-session problem (ZX-022) ([07-zellij-panel.md](07-zellij-panel.md) ZR-003, ZR-034). A problem MUST NOT stop the agent list, polling, or opening agents. Theme switching is enabled only when there are no problems and both themes are set.
- **Source:** `zellij/src/main.rs` (`load`); `zellij/src/app.rs` (`Config::themes_enabled`); `zellij/src/view.rs` (`connection_line`)
- **Verified by:** `zellij/src/view.rs` `setup_problems_replace_the_connected_line_until_fixed`; `zellij/src/guest.rs` `keyboard_signals_carry_the_theme_keys_only_when_themes_are_set`

### ZH-022 Checking for a newer release
- **Behavior:** Unless `update_check` is `false`, a panel with permissions granted MUST, on grant and then on the first tick at least `21600` s (6 h) after the previous check started, run `curl -sS -o /dev/null --max-time 10 -w %{redirect_url} https://github.com/ahparikh/noc/releases/latest` with context `update-check`. The command prints where GitHub redirects "latest release" and does not follow it. On exit `0` with non-empty output, the output's text after its last `/releases/tag/` is the latest tag; when that tag and the plugin's own version (its `Cargo.toml` version) both have the form `[v]X.Y.Z` with decimal numbers, and the tag is greater compared number by number, the panel MUST record `vX.Y.Z` as the available release and redraw; in every other case it MUST clear it. On any other exit, or empty output, the panel MUST keep what it had recorded, show nothing, and check again after `600` s. The `u` key ([07-zellij-panel.md](07-zellij-panel.md) ZK-027) MUST run the same command at once, even when `update_check` is `false`, set the notice `checking for a newer release…`, and restart the six-hour interval; its result is handled the same way and MUST also set a notice: `<vX.Y.Z> is available (this is v<own version>)` when a newer release was found, `v<own version> is the latest release` when not, and `could not check for a newer release` when the command failed or printed nothing. The details window and the relay never check. The check MUST NOT download or change anything. How the available release is shown is specified in [07-zellij-panel.md](07-zellij-panel.md) (ZR-002, ZR-005).
- **Source:** `zellij/src/update.rs` (`check_command`, `parse_version`, `newer_release`, `check_for_update`, `check_for_update_now`, `check_notice`, `on_update_result`); `zellij/src/main.rs` (`on_granted`, `on_panel_tick`)
- **Decision:** D-060
- **Verified by:** `zellij/src/update.rs` `parses_release_versions_only`, `only_a_newer_release_is_reported`, `the_check_only_reads_the_redirect`, `a_check_the_user_asked_for_says_what_it_found`; scheduling and the command result UNTESTED

---

## 4. The pair and its static configuration

### ZX-001 One deployment
- **Behavior:** The plugin runs in one arrangement: a host session named `noc-host` with one tab holding the panel and the guest pane, and an inner session named `noc` attached in the guest pane. The plugin MUST NOT generate or edit Zellij configuration. Everything of noc's is in two layout files the user copies (ZX-004, ZX-035), by hand or with the installer (ZE-016), plus the theme file the shipped settings name (ZX-037). Nothing of noc's is in the user's `config.kdl`, which both sessions read unchanged.
- **Source:** `zellij/config/layouts/noc-host.kdl`; `zellij/config/layouts/noc-inner.kdl`
- **Decision:** D-055, D-056
- **Verified by:** `zellij/e2e/nested.sh` "host session has the sidebar and one guest pane"

### ZX-002 The plugin and its settings
- **Behavior:** `layouts/noc-host.kdl` MUST load the plugin in the panel's pane as `plugin location="<URL>" { … }`, where the URL is a release asset that names the version (`https://github.com/ahparikh/noc/releases/download/v0.1.4/zellij-noc.wasm` as shipped). The block's children are the settings of ZH-017, and this block is the only place they are set; the shipped block sets `noc_bin "noc"`, `guest_session "noc"`, `icons "unicode"`, `pane_titles "name"`, `poll_interval_ms "1000"`, `update_check "true"`, `home_theme "noc-blue"` and `away_theme "noc-blue-away"` (ZX-037), and carries `colors`, `away_key`, `home_key` as comments. `guest_session` MUST equal `inner=` in the same file's guest loop.
- **Source:** `zellij/config/layouts/noc-host.kdl`
- **Decision:** D-056
- **Verified by:** `zellij/e2e/nested.sh` "sidebar lists the registry's agents" (settings from the layout reach the plugin)

### ZX-003 The relay bindings
- **Behavior:** `layouts/noc-inner.kdl` MUST bind, in a `keybinds { shared { } }` block without `clear-defaults`, `Ctrl Alt Shift F11` to `MessagePlugin "<URL>" { floating true; role "relay"; name "zellij-noc.relay"; payload "theme noc-blue-away"; }` and `Ctrl Alt Shift F12` to the same with `payload "theme noc-blue"`, where the URL is the plugin location of ZX-002. A layout's bindings are added to the session's own (the user's `config.kdl` and Zellij's defaults); none is replaced. The two keys MUST match `away_key` and `home_key` (ZH-017), and the two themes are the ones the relay switches to (ZX-019). The bindings do nothing unless the host plugin types the keys, which it does only when `home_theme` and `away_theme` are set.
- **Source:** `zellij/config/layouts/noc-inner.kdl`
- **Decision:** D-056
- **Verified by:** `zellij/e2e/nested.sh` "inner session wears the away theme while the sidebar has the keyboard", "inner session returns to its home theme"; MANUAL: with a `config.kdl` that binds a key of its own, that key and Zellij's default keys still work in a session created from the layout

### ZX-004 The host layout
- **Behavior:** `layouts/noc-host.kdl` MUST define one vertically split pane with two children: a pane of `size=38` containing `plugin location="noc"`, and a pane with `borderless=true focus=true command="sh"` whose arguments are `-c` and the guest loop (ZX-007). The session is started from outside Zellij with `zellij --session noc-host --new-session-with-layout noc-host`.
- **Source:** `zellij/config/layouts/noc-host.kdl`
- **Verified by:** `zellij/e2e/nested.sh` "host session has the sidebar and one guest pane", "the keyboard starts in the inner session"

### ZX-005 Host session options
- **Behavior:** The host layout file MUST set, at top level, `session_serialization false`, `show_startup_tips false`, `show_release_notes false` and `nested_session_handling "never"`. These apply to the host session only.
- **Source:** `zellij/config/layouts/noc-host.kdl`
- **Verified by:** UNTESTED

### ZX-006 Host session keys
- **Behavior:** The host layout file MUST declare `keybinds clear-defaults=true` with exactly three `shared` bindings:
  - `Alt m` → `MessagePlugin { name "zellij-noc.grab"; payload "keyboard"; }`
  - `Alt o` → `FocusPreviousPane; ToggleFocusFullscreen;`
  - `Alt d` → `Detach;`

  The host session binds nothing else, so every other key is typed into the focused pane. The file shows, as comments, how to add keys that hand the keyboard back and type a key into the inner session (`FocusPreviousPane; Write <bytes>;`).
- **Source:** `zellij/config/layouts/noc-host.kdl`
- **Verified by:** `zellij/e2e/nested.sh` "Alt-m gives the keyboard to the sidebar", "Alt-o hides the sidebar", "Alt-o shows it again", "Alt-d detaches the host client"

### ZX-007 The guest loop
- **Behavior:** The guest pane MUST run this loop with `inner=noc; host=noc-host`:
  1. Unset `ZELLIJ`, `ZELLIJ_SESSION_NAME` and `ZELLIJ_PANE_ID`.
  2. Run `zellij attach --create "$inner" options --default-layout noc-inner`. This creates the inner session on first start, with the inner layout (ZX-035); when the session already exists the layout is not read.
  3. When that client returns, sleep `0.5` s and test whether `zellij list-sessions --no-formatting` has a line starting `<inner> ` that does not contain `EXITED`.
  4. If it does (the client detached): run `zellij --session "$host" action detach`, sleep `1` s, and go to step 2.
  5. If it does not (the inner session ended): run `zellij kill-session "$host"` and exit `0`.
- **Source:** `zellij/config/layouts/noc-host.kdl`
- **Verified by:** `zellij/e2e/nested.sh` "guest pane keeps exactly one client on the inner session", "the inner client stays attached in the guest pane", "ending the inner session ends the host session"; detaching the inner client (step 4) UNTESTED

### ZX-035 The inner layout
- **Behavior:** `layouts/noc-inner.kdl` MUST define the inner session's first tab as Zellij's default layout (a one-row borderless `tab-bar` pane, a pane, a one-row borderless `status-bar` pane) and MUST carry, outside the `layout` block, the options that apply to the inner session only; as shipped, `session_indicator false`, `attention { enabled false }` and `ui { pane_frames { hide_session_name true } }`; and the relay bindings (ZX-003). Zellij applies a layout file's options and bindings to the session that layout creates, over `config.kdl`, so sessions started any other way are unaffected. The options are read only when the inner session is created.
- **Source:** `zellij/config/layouts/noc-inner.kdl`
- **Decision:** D-061
- **Verified by:** `zellij/e2e/nested.sh` "inner session takes its options from noc-inner.kdl"

### ZX-037 The shipped theme pair
- **Behavior:** `themes/noc-blue.kdl` MUST define two Zellij themes: `noc-blue` (Catppuccin Mocha with the accent, and so the selected ribbon and the focused frame, in blue `137 180 250`) and `noc-blue-away`, identical except that `frame_selected`'s `base` equals `frame_unselected`'s (`69 71 90`). The shipped layouts select the pair: `home_theme` and `away_theme` in `noc-host.kdl` and the two payloads in `noc-inner.kdl` (ZX-002, ZX-003), so the inner session wears `noc-blue` while it has the keyboard, whatever `theme` the user's `config.kdl` names, and its focused frame is drawn in the unfocused colour while the panel has it.
- **Source:** `zellij/config/themes/noc-blue.kdl`
- **Verified by:** UNTESTED

### ZX-008 Starting and ending the pair
- **Behavior:** The pair MUST start, and be attached to again, with one command run outside Zellij: `zellij attach --create noc-host options --default-layout noc-host`. When `noc-host` does not exist it is created from `layouts/noc-host.kdl`, whose options and key bindings apply to it (ZX-005, ZX-006); when it exists the command attaches and the layout is not read. Nothing else is installed to start it: the user makes the line an alias if they want one. Ending the inner session (`zellij kill-session noc`, or quitting it) ends the host session too (ZX-007).
- **Source:** `zellij/config/layouts/noc-host.kdl` (header comment); `zellij/dev.sh` (`cmd_start`, which runs the same command)
- **Verified by:** `zellij/e2e/nested.sh` "host session has the sidebar and one guest pane", "Alt-m gives the keyboard to the sidebar" (the layout's keys apply), "starting again attaches to the running pair", "ending the inner session ends the host session"

## 5. Keyboard ownership

### ZX-009 Which pane has the keyboard
- **Behavior:** The panel MUST find its own tab from the `PaneUpdate` manifest entry whose plugin id is its own, and record that entry's plugin URL. The pane with the keyboard is, among the panes of that tab: the focused, non-suppressed floating pane if the tab's floating panes are visible and one is focused; otherwise the focused, non-suppressed tiled pane. The panel has the keyboard when that pane is its own plugin pane. The guest pane has the keyboard when that pane is a terminal; this is unknown until a tab has been reported.
- **Source:** `zellij/src/main.rs` (`on_pane_update`); `zellij/src/panel.rs` (`keyboard_pane`, `panel_has_keyboard`, `guest_has_keyboard`); `zellij/src/panes.rs` (`focused_pane`, `find_plugin_pane`)
- **Verified by:** `zellij/src/panes.rs` `focused_pane_prefers_visible_floating_panes`, `finds_its_own_plugin_pane_in_the_manifest`

### ZX-010 The guest pane
- **Behavior:** The guest pane is the first pane in the panel's tab that is a terminal, not suppressed and not floating.
- **Source:** `zellij/src/panel.rs` (`guest_pane`)
- **Verified by:** UNTESTED

### ZX-011 The panel is selectable only while it has the keyboard
- **Behavior:** The panel MUST call `set_selectable(true)` exactly while it has the keyboard or a grab window is open, and `set_selectable(false)` otherwise, calling only when the wanted value changes. Gaining the keyboard closes the grab window. This is re-evaluated on grant, on every `PaneUpdate` and on every tick. The effect is that focus moves made in the host session cannot land on the panel.
- **Source:** `zellij/src/panel.rs` (`sync_panel_selectable`)
- **Verified by:** `zellij/e2e/nested.sh` "the keyboard starts in the inner session", "Alt-m gives the keyboard back" (outcome only)

### ZX-012 The grab pipe
- **Behavior:** On a `zellij-noc.grab` message (any payload) with permissions granted, the panel MUST:
  - if it has the keyboard, run `zellij --session <host> action focus-previous-pane` (ZX-016), or call `focus_previous_pane()` if the host session's name is not yet known;
  - otherwise, if the panel's tab is fullscreen and a guest pane exists, toggle the guest pane out of fullscreen; open a grab window of `2.0` s and become selectable; then run `zellij --session <host> action move-focus left`, or call `focus_plugin_pane(<own id>, false, false)` if the session name is not yet known.
- **Source:** `zellij/src/panel.rs` (`toggle_keyboard`); `zellij/src/main.rs` (`handle_pipe`, `GRAB_WINDOW`)
- **Verified by:** `zellij/e2e/nested.sh` "Alt-m gives the keyboard to the sidebar", "Alt-m gives the keyboard back"; grabbing from fullscreen UNTESTED

### ZX-013 Signalling keyboard moves
- **Behavior:** On every `PaneUpdate` with permissions granted the panel MUST compare whether the guest pane has the keyboard with the previous known value. When the value is known, was known before, and changed, it MUST type the keyboard signal (ZX-014) into the guest pane (ZX-015), whatever caused the move. The first known value sends nothing.
- **Source:** `zellij/src/panel.rs` (`signal_keyboard_moves`); `zellij/src/main.rs` (`on_pane_update`)
- **Verified by:** `zellij/e2e/nested.sh` "inner session wears the away theme while the sidebar has the keyboard", "inner session returns to its home theme"

### ZX-014 The keyboard signal
- **Behavior:** When the keyboard leaves the guest pane the signal is the encoded `away_key`. When it arrives the signal is the encoded `home_key` followed by the focus frame. The theme keys MUST be included only when theme switching is enabled (ZH-021); otherwise the leaving signal is empty and the arriving signal is the focus frame alone. The focus frame is the 14 bytes `ESC P 2 6 6 6 1 n S g A = ESC \` (`27 80 50 54 54 54 49 110 83 103 65 61 27 92`), Zellij's nested-session "focus gained" frame.
- **Source:** `zellij/src/guest.rs` (`keyboard_signal`, `FOCUS_FRAME`)
- **Verified by:** `zellij/src/guest.rs` `keyboard_signals_carry_the_theme_keys_only_when_themes_are_set`

### ZX-015 Typing into the guest pane
- **Behavior:** Bytes MUST be typed into the guest pane by running `zellij --session <host> action write --pane-id terminal_<N> <b1> <b2> …`, with each byte as a decimal argument, so they arrive whichever pane has the keyboard. Nothing is run when the byte list is empty, permissions are not granted, there is no guest pane, or the host session's name is not yet known.
- **Source:** `zellij/src/guest.rs` (`write_to_guest`)
- **Verified by:** `zellij/e2e/nested.sh` "inner session wears the away theme while the sidebar has the keyboard"

### ZX-016 Host session actions
- **Behavior:** A host action MUST run `zellij --session <host> action <args…>` through `run_command` with context `zellij-noc=host-action`, where `<host>` is the session name from the last `ModeUpdate` that carried one. When no name is known nothing is run and the caller is told so. Results are ignored (ZH-012).
- **Source:** `zellij/src/guest.rs` (`host_action`)
- **Verified by:** UNTESTED directly (exercised by every `Alt-m` check)

### ZX-017 Focus frame for a newly attached client
- **Behavior:** When an inner-session snapshot shows at least one client and the previous read showed none (or failed, or there was none), the panel MUST type the focus frame into the guest pane, unless the guest pane is known not to have the keyboard.
- **Source:** `zellij/src/guest.rs` (`on_guest_snapshot`)
- **Verified by:** UNTESTED

## 6. The relay

### ZX-018 Relay lifecycle
- **Behavior:** A relay runs in the inner session, started by the bindings of ZX-003. On any `PermissionRequestResult` it MUST hide itself. It handles no other event and draws nothing.
- **Source:** `zellij/src/main.rs` (`update`, `render`)
- **Verified by:** UNTESTED

### ZX-019 Switching the theme
- **Behavior:** On a `zellij-noc.relay` message the relay MUST resolve a theme name from the trimmed payload: `away` → its own `away_theme` setting, `home` → its own `home_theme` setting, `theme <name>` → `<name>` trimmed. If the setting is unset, the payload has another form or is absent, or the name is not plain (ZH-018), it MUST do nothing. Otherwise it MUST call `reconfigure` with the text `theme "<name>"` and without writing the configuration to disk. Other pipe names are ignored.
- **Source:** `zellij/src/guest.rs` (`relay_pipe`, `relay_theme`)
- **Verified by:** `zellij/src/guest.rs` `relay_accepts_plain_theme_names_only`; `zellij/e2e/nested.sh` "inner session wears the away theme while the sidebar has the keyboard", "inner session returns to its home theme"

## 7. Reading the inner session

### ZX-020 Polling
- **Behavior:** With permissions granted and no read in flight, the panel MUST read the inner session when at least `0.75` s have passed since the last read started, by running `/bin/sh -c '<script>' sh <guest_session>` with context `zellij-noc=guest-snapshot`. The script is `zellij --session "$1" action list-clients && echo --zellij-noc-guest-panes-- && zellij --session "$1" action list-panes --all --json`. The session name MUST be passed as an argument, never placed in the script text.
- **Source:** `zellij/src/guest.rs` (`poll_guest`, `snapshot_command`)
- **Verified by:** `zellij/src/guest.rs` `builds_inner_session_commands`

### ZX-021 Parsing a snapshot
- **Behavior:** Output MUST be split at the first `--zellij-noc-guest-panes--`. The part after it MUST parse as a JSON array of panes; plugin panes are dropped. In the part before it, blank lines are dropped and the first remaining line is a header; each further line is one client. The focused pane is the second whitespace-separated field of the first client line, as `terminal_<N>` or a bare number; any other value means no focused pane. A result is usable only with exit `0`, valid UTF-8, a separator and valid JSON.
- **Source:** `zellij/src/guest.rs` (`parse_guest_snapshot`, `on_guest_snapshot`)
- **Verified by:** `zellij/src/guest.rs` `parses_the_inner_sessions_terminals_and_focus`, `a_focused_plugin_or_garbage_is_no_focus`; `zellij/src/model.rs` `parses_terminal_pane_ids_only`

### ZX-022 An unreadable inner session
- **Behavior:** An unusable read MUST keep the previous snapshot, mark no client as seen, and start (or continue) a failure period. Once the period has lasted `5.0` s the panel MUST record the problem `inner session "<guest_session>" is not running` (shown as in ZH-021) and redraw. The next usable read ends the period, clears the message and redraws.
- **Source:** `zellij/src/guest.rs` (`on_guest_unreadable`, `on_guest_snapshot`)
- **Verified by:** `zellij/src/view.rs` `setup_problems_replace_the_connected_line_until_fixed` (display only); timing UNTESTED

### ZX-023 Panes the panel opened
- **Behavior:** The panel MUST remember, per agent, the pane id reported by a successful `p` or `s` (ZX-031). On each usable snapshot it MUST forget entries whose pane is gone or exited.
- **Source:** `zellij/src/guest.rs` (`on_guest_open`, `on_guest_snapshot`)
- **Verified by:** UNTESTED

### ZX-024 The selection follows the inner focus
- **Behavior:** After each usable snapshot, unless the panel has the keyboard, the panel MUST find the agent in the inner session's focused pane (ZX-025) and, if it differs from the agent last followed, select it and redraw when the selection changed. When the panel has the keyboard (seen on a snapshot or a `PaneUpdate`) the followed agent is forgotten, so handing the keyboard back selects the focused agent again.
- **Source:** `zellij/src/guest.rs` (`follow_guest_focus`); `zellij/src/main.rs` (`on_pane_update`)
- **Verified by:** `zellij/e2e/nested.sh` "Enter on an agent that is already open focuses it instead of opening a copy" (the harness waits for the panel to follow the inner focus onto the opened agent before pressing Enter again); not asserted directly

### ZX-025 Which agent is in a pane
- **Behavior:** A plugin pane or an exited pane holds no agent. Otherwise the first match wins:
  1. the agent whose remembered pane (ZX-023) is this pane;
  2. the agent named by the pane's command, when the command has at least three tokens (quotes stripped), its second-to-last token is `attach`, and the basename of its first token equals the basename of `noc_bin`; the agent id is the last token;
  3. the single non-Quiet agent whose user name or name equals the pane title after removing a leading `NOC: ` and any leading non-alphanumeric characters; no match when several agents qualify;
  4. the agent with the most recently observed Zellij binding for this pane id whose session is empty or equals `guest_session`.

  Bindings of agents that are not live, and bindings in state `disconnected`, are never used.
- **Source:** `zellij/src/panes.rs` (`agent_in_pane`, `attached_agent_id`, `agent_named_by_pane_title`); `zellij/src/model.rs` (`zellij_bindings`)
- **Verified by:** `zellij/src/panes.rs` `identifies_the_agent_in_a_pane_from_launches_commands_and_bindings`, `recognizes_attach_commands_for_any_agent`, `recognises_a_pane_by_the_title_the_panel_gave_it`, `matches_a_hand_attached_pane_by_its_claude_title`; `zellij/src/model.rs` `zellij_bindings_skip_ended_and_disconnected_agents`

## 8. Opening agents in the inner session

### ZX-026 Everything opens in the inner session
- **Behavior:** The Open, Pane and Worktree intents MUST act only on the inner session, by running Zellij CLI commands against `guest_session` (ZX-031). The panel MUST NOT open agent panes or tabs in the host session.
- **Source:** `zellij/src/panel.rs` (`open_selected_agent`, `open_agent_pane`, `open_worktree_shell`); `zellij/src/guest.rs`
- **Decision:** D-055
- **Verified by:** `zellij/e2e/nested.sh` "Enter opens the agent in a new inner tab named after it"

### ZX-027 Enter: focus the agent's existing pane
- **Behavior:** With no agent selected the notice is `no agent is selected`. Otherwise the panel MUST look for the agent's live pane in the last inner snapshot, first match winning: its remembered pane if still present and not exited; a non-exited pane whose command attaches that agent (ZX-025 rule 2); a pane named by one of the agent's bindings whose session equals a non-empty `guest_session` and whose pane is present and not exited. If found it MUST run `action focus-pane-id terminal_<N>` in the inner session.
- **Source:** `zellij/src/guest.rs` (`guest_open_agent`, `guest_agent_pane`, `focus_pane_args`)
- **Verified by:** `zellij/src/guest.rs` `finds_an_agents_inner_pane_in_order`, `builds_inner_session_commands`; `zellij/e2e/nested.sh` "Enter on an agent that is already open focuses it instead of opening a copy"

### ZX-028 Enter: open a new tab
- **Behavior:** When no pane is found the panel MUST refuse with `this agent has no resumable native session ID` if the agent's native session id is blank, or with `already open in Zellij session "<name>"; press p to open a second copy here` if one of the agent's bindings names a session that is running and is neither the host session nor `guest_session`. Otherwise it MUST run, in the inner session, `action new-tab --name <tab name> [--cwd <path>] -- <noc_bin> attach <id>`. The tab name is the agent's title cut to `32` characters plus `…` when longer. `--cwd` is the agent's trimmed workspace path, omitted when blank.
- **Source:** `zellij/src/guest.rs` (`guest_open_agent`, `new_tab_args`, `agent_cwd`); `zellij/src/panel.rs` (`other_running_session_for`); `zellij/src/panes.rs` (`other_running_session`, `agent_tab_name`)
- **Verified by:** `zellij/src/guest.rs` `builds_inner_session_commands`, `agent_cwd_is_the_trimmed_workspace_path`; `zellij/src/panes.rs` `tab_names_are_the_agent_title_cut_to_a_limit`, `names_another_running_session_only_when_it_is_live_and_not_ours`; `zellij/e2e/nested.sh` "Enter opens the agent in a new inner tab named after it", "the new tab runs noc attach for that agent", "exactly one tab was added"

### ZX-036 Tab names follow the agent
- **Behavior:** After each successful read of the inner session the panel MUST work out, for every inner tab (by the `tab_id` and `tab_name` of its panes), which agents run in it (ZX-025, over the tab's panes that have not exited). A tab with exactly one agent whose name equals that agent's tab name (ZX-028: the title cut to `32` characters plus `…`) is remembered as named after that agent. A tab with exactly one agent and a different name MUST be renamed to the agent's tab name when either the panel remembers that tab under that agent with the name it has now, or the name is one Zellij assigned (`Tab #` followed only by digits); it is then remembered under the new name. Any other tab MUST be left alone and forgotten: a tab with no agent or several, and a tab the user named or renamed. The rename runs `action rename-tab --tab-id <id> -- <name>` in the inner session and does not move the host keyboard; its result is ignored. So a tab the panel opened, or one Zellij named, follows a rename in the registry within one read (ZX-020).
- **Source:** `zellij/src/guest.rs` (`tab_renames`, `rename_tab_args`, `on_guest_snapshot`); `zellij/src/panes.rs` (`is_default_tab_name`, `agent_tab_name`)
- **Decision:** D-064
- **Verified by:** `zellij/src/guest.rs` `tab_names_follow_the_agent_unless_the_user_named_the_tab`; `zellij/e2e/nested.sh` "the tab is renamed when the agent's name changes"

### ZX-029 `p` and `s`: a pane beside, or stacked
- **Behavior:** With an agent selected and a non-blank native session id (same notices as above otherwise), the panel MUST run, in the inner session, `run --name "NOC: <title>" [--cwd <path>] [--stacked] -- <noc_bin> attach <id>`, with `--stacked` for `s` only, and pass the agent id in the command context under `zellij-noc-guest-agent`. These always open a new pane, even when the agent already has one.
- **Source:** `zellij/src/panel.rs` (`open_agent_pane`); `zellij/src/guest.rs` (`guest_open_agent_in_pane`, `run_pane_args`)
- **Verified by:** `zellij/src/guest.rs` `builds_inner_session_commands`; end to end UNTESTED

### ZX-030 `w`: a shell in the worktree
- **Behavior:** With no agent selected the notice is `no agent is selected`. The directory is the workspace's root path, or its path when the root is blank; if there is none the notice is `this agent has no known workspace`. Otherwise the panel MUST run `action new-pane --cwd <dir>` in the inner session.
- **Source:** `zellij/src/panel.rs` (`open_worktree_shell`); `zellij/src/guest.rs` (`guest_open_shell`, `shell_args`)
- **Verified by:** `zellij/src/guest.rs` `builds_inner_session_commands`; end to end UNTESTED

### ZX-031 Running a command in the inner session
- **Behavior:** Each command of ZX-027..ZX-030 MUST run as `/bin/sh -c '<script>' sh <guest_session> <host> <args…>` with context `zellij-noc=guest-open`, where `<host>` is the host session's name or empty when unknown. The script runs `zellij --session "$guest" "$@"`, then, if `<host>` is not empty, `zellij --session "$host" action focus-previous-pane` with output discarded, and exits with the first command's status. Arguments MUST be passed as arguments, never placed in the script text. The effect is that the host keyboard returns to the guest pane after the command, whether or not it succeeded.

  On the result the panel MUST make the next inner-session read due immediately. A non-zero exit sets the notice `the inner Zellij session did not open it: <compacted stderr>`. On exit `0`, if the context names an agent and stdout contains a token `terminal_<N>`, the panel remembers pane `N` for that agent (ZX-023).
- **Source:** `zellij/src/guest.rs` (`guest_run`, `guest_run_command`, `on_guest_open`, `opened_pane_id`)
- **Verified by:** `zellij/src/guest.rs` `builds_inner_session_commands`, `reads_the_opened_pane_id`; `zellij/e2e/nested.sh` "the keyboard follows into the inner session"

## 9. Details window and panel title

### ZX-032 Opening the details window
- **Behavior:** On the Details intent (`D`): with no agent selected the notice is `no agent is selected`; with the panel's own plugin URL not yet seen in a `PaneUpdate` the notice is `the plugin location is not known yet; try again`. Otherwise the panel MUST close the details window it opened before, if any, and call `open_plugin_pane_floating` at its own plugin URL with settings `role "details"`, `agent_id <id>`, `noc_bin <noc_bin>`, and `icons "nerd"` when Nerd icons are on, at x `10%`, y `10%`, width `80%`, height `80%`. If Zellij returns no plugin pane the notice is `Zellij could not open the details window`.
- **Source:** `zellij/src/panel.rs` (`open_details_window`)
- **Verified by:** `zellij/e2e/nested.sh` "D opens the floating details window"

### ZX-033 The details instance
- **Behavior:** On grant the details window MUST run `<noc_bin> show --json <agent_id>` with context `zellij-noc=agent-details`; nothing runs when `agent_id` is empty or a read is in flight. After each result it arms a `2.0` s timer, and the timer starts the next read. On exit `0` with a valid agent it names its pane `noc · <title>` when the title changed and shows `connected`; invalid UTF-8 gives `noc returned non-UTF-8 output: <error>`; other failures give server-unavailable (ZH-011) or the compacted stderr. On denial its connection error is `permissions denied: agent details are unavailable`. `Esc`, `D`, `d` or `q`, with any modifiers, MUST close it; no other key does anything. Its content is specified in [07-zellij-panel.md](07-zellij-panel.md).
- **Source:** `zellij/src/details.rs` (`update_details`, `refresh_details`, `on_details_result`, `is_close_key`)
- **Verified by:** `zellij/src/details.rs` `the_window_closes_on_esc_d_capital_d_and_q_only`; `zellij/e2e/nested.sh` "D opens the floating details window" (title), "q closes it"

### ZX-034 Panel pane title
- **Behavior:** The panel's pane title MUST be `NOC`, set once when permissions are granted (ZH-004) and left where Zellij draws a title, at the left of the frame's top border.
- **Source:** `zellij/src/main.rs` (`on_granted`); `zellij/src/panel.rs` (`PANEL_TITLE`)
- **Verified by:** `zellij/e2e/nested.sh` "host session has the sidebar and one guest pane" (finds the panel by that title)

### ZX-038 Pane titles in the inner session
- **Behavior:** The setting `pane_titles` chooses what the frames of the inner session's terminal panes say. With `show` (the default in code) the panel renames nothing. Otherwise, after each successful read of the inner session, the panel MUST give every terminal pane there a name, by running `action rename-pane --pane-id terminal_<id> -- <name>` in the inner session without moving the host keyboard (the result is ignored), once per pane and again only when the wanted name changes:
  - `hide`: a single space, so the frame shows no title. An empty name is not used, because Zellij then shows the pane's own title again.
  - `name`: `<host> · <what>`, where `<host>` is this machine's short host name and `<what>` is, for a pane that holds an agent (ZX-025), the agent's provider (`claude`, `codex`); for any other pane started with a command, the last path component of the command's first word, quotes removed; and otherwise the name of the user's shell. The host and the shell come from running `hostname -s 2>/dev/null || hostname; basename "${SHELL:-sh}"` once when permissions are granted (context `host-info`); until that has answered, nothing is renamed.

  The panel MUST remember the title each pane had by itself, for as long as the pane exists, and use that title wherever a pane's title identifies an agent (ZX-025): a pane showing the name the panel gave it is treated as having its own title. A title that is neither is taken as the pane's new own title, and the pane is named again. Plugin panes are never renamed.
- **Source:** `zellij/src/guest.rs` (`restore_titles`, `title_renames`, `pane_name`, `rename_pane_args`, `parse_host_info`, `on_guest_snapshot`); `zellij/src/main.rs` (`on_granted`, `on_command_result`); `zellij/src/app.rs` (`Config::from_map`)
- **Decision:** D-074
- **Verified by:** `zellij/src/guest.rs` `pane_names_are_given_once_and_the_real_titles_remembered`, `a_pane_is_named_for_its_machine_and_what_runs_in_it`; `zellij/src/app.rs` `parses_bounded_poll_configuration` (the setting); the rename in a running pair is MANUAL: start the dev pair and look at the inner panes' frames

## 10. Build

### ZE-001 Toolchain and target
- **Behavior:** The plugin MUST build with the Rust toolchain pinned in `rust-toolchain.toml`: channel `1.98.1`, profile `minimal`, components `clippy` and `rustfmt`, target `wasm32-wasip1`. The crate is `zellij-noc` version `0.1.0`, edition `2021`, `rust-version = "1.98"`, `publish = false`.
- **Source:** `zellij/rust-toolchain.toml`; `zellij/Cargo.toml`
- **Verified by:** MANUAL: `make zellij`

### ZE-002 Dependencies and release profile
- **Behavior:** Dependencies MUST be `zellij-tile = "=0.46.0"` (exact), `serde` 1.0 with `derive`, `serde_json` 1.0 and `unicode-width` 0.2. The release profile sets `codegen-units = 1`, `lto = true`, `strip = true`.
- **Source:** `zellij/Cargo.toml`
- **Verified by:** MANUAL: `make zellij`

### ZE-003 Make targets
- **Behavior:** `zellij/Makefile` uses `$HOME/.cargo/bin/cargo` when it exists, else `cargo` (`CARGO` overrides), and `WASM_TARGET ?= wasm32-wasip1`. Targets:

  | `make -C zellij …` | Root alias | Runs |
  |---|---|---|
  | `build` | `make zellij` | `cargo build --release --target wasm32-wasip1` → `zellij/target/wasm32-wasip1/release/zellij-noc.wasm` |
  | `check` | `make zellij-check` | `fmt-check`, `lint`, `test` |
  | `fmt` / `fmt-check` | `make zellij-fmt` / none | `cargo fmt --all` / `cargo fmt --all -- --check` |
  | `lint` | none | `cargo clippy --target wasm32-wasip1 -- -D warnings` and `cargo clippy --all-targets -- -D warnings` |
  | `test` | `make zellij-test` | `cargo test` (host target) |
  | `e2e` | `make zellij-e2e` | `build`, then `e2e/nested.sh` |
  | `dev` / `dev-stop` | `make dev` / `make dev-stop` | `build`, then `./dev.sh start` / `./dev.sh stop`; the root `dev` also builds `go/bin/noc` |
  | (none) | `make dev-installed` | builds `go/bin/noc` and the plugin, then `zellij/dev.sh --installed start` |
  | (none) | `make zellij-install-config` | `zellij/install-config.sh` (ZE-016) |
  | `clean` | `make zellij-clean` | `cargo clean` |

- **Source:** `zellij/Makefile`; root `Makefile`
- **Verified by:** MANUAL: `make zellij-check`

### ZE-004 Release packaging
- **Behavior:** Root `make dist` MUST, when `VERSION` starts with `v` and a digit, fail unless every plugin location in `zellij/config/layouts/noc-host.kdl` and `zellij/config/layouts/noc-inner.kdl` (every line naming `zellij-noc.wasm`) contains `releases/download/<VERSION>/zellij-noc.wasm` and each file has at least one, printing `<file> does not load the plugin from <VERSION>; update the plugin URL there (and in the docs) first`. In the same case it MUST also fail unless `zellij/Cargo.toml` has the line `version = "<VERSION without the v>"`, printing `zellij/Cargo.toml is not version <X.Y.Z>; the plugin compares releases against it` (ZH-022). It then builds the plugin, copies it to `$(DIST)/zellij-noc.wasm`, and includes it in `checksums.txt` (SHA-256).
- **Source:** root `Makefile` (`dist`)
- **Verified by:** MANUAL: `make dist VERSION=v0.0.0` fails with the first message

## 11. The development pair

### ZE-005 Commands and names
- **Behavior:** `zellij/dev.sh [--fake|--installed] [start|render|stop|status]` (default `start`) runs a copy of the pair that uses this checkout's builds. Without `--installed` the configuration is rendered from `zellij/config/` (ZE-007, ZE-008); with it, from the user's Zellij configuration directory (ZE-017). `render` writes the source in use, `shipped` or `installed`, to `<out>/mode`, after removing the previous `config.kdl`, host layout, inner layout and every symbolic link from `<out>` and `<out>/layouts`. The inner session is `$NOC_DEV_NAME` (default `noc-dev`) and the host is `<inner>-host`. The script MUST exit `1` with `dev.sh: NOC_DEV_NAME must be a plain name` when the name is empty or has a character outside `A-Za-z0-9._-`, and with `dev.sh: NOC_DEV_NAME=noc is the daily pair; pick another name` when it is `noc`. An unknown command prints the usage to stderr and exits `2`. Every `zellij` call is made with `ZELLIJ`, `ZELLIJ_SESSION_NAME` and `ZELLIJ_PANE_ID` removed from the environment.
- **Source:** `zellij/dev.sh`
- **Verified by:** `zellij/e2e/nested.sh` (runs under `NOC_DEV_NAME=noc-e2e`); error paths UNTESTED

### ZE-006 Rendering: the `noc` executable
- **Behavior:** `render` MUST fail with `missing <wasm> (run: make zellij)` when the plugin is not built. It writes to `zellij/target/dev/<inner>/`. With `--fake`, `noc_bin` is `zellij/e2e/fake-noc`. Without it, `go/bin/noc` MUST exist (else `missing <path> (run: make build)`), and `noc_bin` is a generated executable wrapper `<out>/noc` that runs `go/bin/noc` with `NOC_STATE_DIR=${XDG_STATE_HOME:-$HOME/.local/state}/<inner>` and `NOC_RUNTIME_DIR=${XDG_CACHE_HOME:-$HOME/.cache}/<inner>`.
- **Source:** `zellij/dev.sh` (`render`)
- **Verified by:** `zellij/e2e/nested.sh` "sidebar lists the registry's agents" (`--fake` only); the wrapper UNTESTED

### ZE-007 Rendering: `config.kdl`
- **Behavior:** Without `--installed`, `zellij/config/themes/noc-blue.kdl` is copied to `<out>/themes/`, and `<out>/config.kdl` MUST be a header comment, `theme "<home>"`, `show_startup_tips false`, `show_release_notes false`, `session_serialization false`, `pane_frames true`, `pane_frame_style "full"` and `ui { pane_frames { rounded_corners true; }; }` (Zellij's default draws only a title above each pane), and nothing of noc's. The theme pair `<home>`, `<away>` is `noc-blue` and `noc-blue-away`; with `--fake` it is the built-in `catppuccin-mocha` and `catppuccin-macchiato`, whose tab bars differ, which is what the harness looks at (ZE-013).
- **Source:** `zellij/dev.sh` (`render_shipped`)
- **Verified by:** `zellij/e2e/nested.sh` "inner session wears the away theme while the sidebar has the keyboard"

### ZE-008 Rendering: the layouts
- **Behavior:** The two layouts are rendered from the source (`zellij/config/layouts/`, or the user's `layouts/` with `--installed`). In both, every quoted plugin location ending `zellij-noc.wasm` becomes `"file:<wasm path>"`.
  - `<out>/layouts/<host>.kdl` is `noc-host.kdl` with `inner=noc; host=noc-host;` replaced by `inner=<inner>; host=<host>; export ZELLIJ_CONFIG_DIR='<out>';`, so the inner session reads the rendered configuration; every `noc_bin`, `guest_session` and `update_check` line removed; and, directly after the line that opens the plugin block, `noc_bin "<executable of ZE-006>"`, `guest_session "<inner>"` and `update_check "<false with --fake, else true>"`. Without `--installed`, the `home_theme` and `away_theme` lines name `<home>` and `<away>` (ZE-007).
  - `<out>/layouts/noc-inner.kdl` is `noc-inner.kdl`; without `--installed`, its two payloads `theme noc-blue-away` and `theme noc-blue` name `<away>` and `<home>`.

  Rendering MUST fail with `could not point the plugin at <wasm>` when the host layout does not load `file:<wasm>`, `could not point the plugin at <executable>` when `noc_bin` was not set, `could not rename the sessions` when the session names were not replaced, and `could not point the theme bindings at <wasm>` when the inner layout still names a remote `zellij-noc.wasm`.
- **Source:** `zellij/dev.sh` (`render`, `host_layout`, `local_plugin`, `render_shipped`)
- **Verified by:** `zellij/e2e/nested.sh` "guest pane keeps exactly one client on the inner session", "sidebar lists the registry's agents", "inner session wears the away theme while the sidebar has the keyboard"

### ZE-009 Start, stop, status
- **Behavior:** `start` MUST fail with `dev.sh: run this from a plain terminal, not inside Zellij` when `ZELLIJ` is set. If the host session is running it attaches, except that without `--fake` it MUST first fail with `dev.sh: the dev pair is running with the <mode> configuration; stop it first (make dev-stop)` when `<out>/mode` names the other source. Otherwise it renders, starts the development registry server and deletes any dead host session. Either way it then runs the command users run (ZX-008) against the rendered directory: `zellij --config-dir <out> attach --create <host> options --default-layout <host>`. Starting the server means running `<out>/noc server stop` (failure ignored) and then `<out>/noc server --bg`, so the server is always the current build; if that fails, `start` MUST exit `1` with `dev.sh: could not start the dev server (see <runtime dir>/noc.log)`. With `--fake` no server is started. `stop` runs `zellij delete-session --force` for the host then the inner session, then `<out>/noc server stop` when the wrapper exists, ignoring failures. `status` prints `== configuration: <mode>` when `<out>/mode` exists, then, for each session, `== <name>` and its client list, or `== <name>: not running`, then `== dev server: running (<N> agents)` or `== dev server: not running`. Ending the pair from inside Zellij leaves the server running until `stop`. `render` prints the output directory.
- **Source:** `zellij/dev.sh` (`cmd_start`, `cmd_stop`, `cmd_status`)
- **Verified by:** `zellij/e2e/nested.sh` "starting again attaches to the running pair"; `stop` is used by the harness cleanup; the server steps UNTESTED

### ZE-010 Isolation
- **Behavior:** The development pair MUST NOT write to `~/.config/zellij`, and reads it only with `--installed`. It MUST NOT read or write the sessions `noc` and `noc-host`, or the daily `noc` state and runtime directories. Its server has its own database and socket and learns about agents only from provider discovery, which reads the same live Claude Code and Codex sessions the daily server sees; provider hooks stay with the daily binary. Both of its sessions take their configuration from `<out>` (`--config-dir` for the host, `ZELLIJ_CONFIG_DIR` for the inner session).
- **Source:** `zellij/dev.sh`
- **Verified by:** UNTESTED (by construction of ZE-005..ZE-009)

### ZE-017 Rendering from the installed configuration
- **Behavior:** With `--installed` the source is `$ZELLIJ_CONFIG_DIR`, or `~/.config/zellij`. Rendering MUST fail with `missing <source>/layouts/<name> (run: make zellij-install-config)` when `noc-host.kdl` or `noc-inner.kdl` is absent, and with `<source>/layouts/noc-host.kdl does not load zellij-noc.wasm (run: make zellij-install-config)` when the host layout has no such plugin location. Every entry of the source directory except `layouts` (so `config.kdl`, `themes` and the rest), and every entry of its `layouts` except the two noc layouts, is symbolically linked into `<out>` under the same name; the user's `config.kdl` is used as it is. The two layouts are rendered as in ZE-008.
- **Source:** `zellij/dev.sh` (`render_installed`, `host_layout`, `local_plugin`)
- **Decision:** D-062
- **Verified by:** UNTESTED

### ZE-016 The configuration installer
- **Behavior:** `zellij/install-config.sh [--check]` installs `layouts/noc-host.kdl`, `layouts/noc-inner.kdl` and `themes/noc-blue.kdl` into `$ZELLIJ_CONFIG_DIR`, or `~/.config/zellij`, and brings an existing installation up to date. It MUST NOT read or write `config.kdl`. Any other argument prints `usage: install-config.sh [--check]` and exits `2`.
  1. **Source.** Run as a file with `config/layouts/noc-host.kdl` beside it (a checkout; also root `make zellij-install-config`) it uses the files beside it. Otherwise (piped: `curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash`, with `bash -s -- --check` for the check) it MUST download the three files into a temporary directory, removed on exit, from `$NOC_CONFIG_URL`, or else from `https://raw.githubusercontent.com/<NOC_REPO, default ahparikh/noc>/<ref>/zellij/config`, where `<ref>` is `$NOC_REF` or, when that is unset, the tag GitHub's `releases/latest` redirects to, so that the layouts name a published plugin. It prints `from       <source>`. It MUST stop, having changed nothing, with `install-config.sh: could not find the latest release of <repo> (set NOC_REF to a tag or branch)` when there is no such redirect, and with `install-config.sh: could not download <url>` when a download fails, both exit `1`.
  2. **Host layout.** When the destination exists and loads `zellij-noc.wasm`, the file installed is the new layout with the user's settings. To tell the user's choices from defaults, it fetches the host layout that the release named in the user's plugin location shipped (`https://raw.githubusercontent.com/<repo>/<that tag>/zellij/config/layouts/noc-host.kdl`, or the file in `$NOC_SHIPPED_HOST`). A setting of ZH-017 is the user's when it is set (not commented out) in their plugin block with a value that release did not ship; a setting that release set and the user's file does not is one the user turned off. When that layout cannot be had, every setting that is set is the user's and none is turned off. Each of the user's settings is given the user's value, in place where the new layout sets it, uncommented where the new layout only mentions it, and added at the end of the plugin block otherwise; each turned-off setting is commented out where the new layout sets it; and `inner=noc; host=noc-host;` in the guest loop is replaced by the user's `inner=…; host=…;` when theirs differs. Nothing else of the user's file is carried over. Otherwise the new layout is installed as it is.
  3. **Inner layout.** When the destination exists and differs from the new layout in more than its plugin locations, the user's file MUST be kept: only its plugin locations are set to the new one, and the new layout is written beside it as `noc-inner.kdl.new`. Otherwise the new layout is installed.
  4. **Theme.** The new file is installed.
  5. **Reporting.** For each file it prints `unchanged  <path>` when the result is identical to what is there; `add        <path>` when there was none; `update     <path>[, keeping your <names>] (previous kept as <name>.bak)` after copying the existing file to `<path>.bak`, where `<names>` lists, sorted, the settings whose value differs from the new layout's own, the turned-off ones as `<name> (off)`, and `session names`; for a kept inner layout, `update     <path>: plugin location only, the rest is yours (previous kept as noc-inner.kdl.bak)` or, when nothing changed, `kept       <path> (yours differs from the new file)`, each followed by a line naming the `.new` file.
  6. With `--check` it prints the same lines, changes nothing, and ends with `(--check: nothing was changed)`. Otherwise it ends by naming the plugin version installed, the file that holds the settings, and the two commands that restart the pair (ZX-008). It never edits a shell profile.
- **Source:** `zellij/install-config.sh`; root `Makefile` (`zellij-install-config`)
- **Decision:** D-062
- **Verified by:** MANUAL: with `ZELLIJ_CONFIG_DIR` set to scratch directories, run it on an empty one, on a copy of an older installation, on a copy with settings, session names and `noc-inner.kdl` edited, each twice, and piped (`cat zellij/install-config.sh | bash -s -- --check`) against the latest release; `zellij --config-dir <dir> setup --check` accepts the result. No automated test

## 12. End-to-end harness

### ZE-011 What the harness is
- **Behavior:** `zellij/e2e/nested.sh [--keep]` MUST start `dev.sh --fake start` inside a detached tmux session of 170×45, drive it with `tmux send-keys`, and check what the Zellij CLI and the tmux screen report. Names: inner `noc-e2e` (or `$NOC_DEV_NAME`), host `<inner>-host`, tmux `<inner>-tmux`. It requires `zellij`, `tmux` and `jq`, printing `nested.sh: <tool> is required` and exiting `2` when one is missing; it exits `2` when rendering fails. It first kills any earlier tmux session of that name and stops any earlier pair of those names.
- **Source:** `zellij/e2e/nested.sh`
- **Verified by:** MANUAL: `make zellij-e2e`

### ZE-012 Start-up, welcome screen and permissions
- **Behavior:** The harness MUST wait up to 30 s for each session to be listed and not `EXITED`, else print `nested.sh: the pair did not start; last screen:` with the last 15 screen lines and exit `1`. It then presses `Escape`, up to four times `1.5` s apart, while either session's client list shows `zellij:configuration` (Zellij's welcome screen). If the screen then contains `asks permission to`, it MUST print `nested.sh: the plugin is waiting for its permissions.` and `Run 'make dev' once, answer the prompt in the sidebar with y, then run this again.` and exit `1`. It MUST NOT edit Zellij's permission cache.
- **Source:** `zellij/e2e/nested.sh` (`start_pair`, `dismiss_welcome`)
- **Verified by:** MANUAL: `make zellij-e2e` on a machine with no grant

### ZE-013 The checks
- **Behavior:** The harness runs 26 checks in order, printing `ok    <name>` or `FAIL  <name>` for each:

  | # | Check name | Asserts |
  |---|---|---|
  | 1 | host session has the sidebar and one guest pane | a tiled plugin pane titled `NOC` and a tiled terminal exist in the host |
  | 2 | sidebar lists the registry's agents | the screen shows `2 agents` within 10 s |
  | 3 | guest pane keeps exactly one client on the inner session | inner client count is 1 |
  | 4 | the keyboard starts in the inner session | host focus is the guest pane |
  | 5 | inner session takes its options from noc-inner.kdl | screen row 1 contains `Zellij ` and neither `(<inner>)` nor `running` |
  | 6 | Alt-m gives the keyboard to the sidebar | host focus becomes the panel within 5 s |
  | 7 | inner session wears the away theme while the sidebar has the keyboard | the dominant background colour of screen row 1 changed |
  | 8 | Alt-m gives the keyboard back | host focus becomes the guest pane |
  | 9 | inner session returns to its home theme | row 1 colour equals the first reading |
  | 10 | Enter opens the agent in a new inner tab named after it | an inner tab `E2E agent` exists within 10 s |
  | 11 | the new tab runs noc attach for that agent | an inner pane command matches `attach agent-e2e` |
  | 12 | the keyboard follows into the inner session | host focus becomes the guest pane |
  | 13 | exactly one tab was added | inner tab count rose by 1 |
  | 14 | Enter on an agent that is already open focuses it instead of opening a copy | inner tab count unchanged |
  | 15 | the tab is renamed when the agent's name changes | after the fake registry reports the agent as `Renamed agent`, an inner tab has that name within 10 s |
  | 16 | D opens the floating details window | a floating plugin pane titled with `E2E agent` exists in the host |
  | 17 | q closes it | no visible floating plugin pane remains |
  | 18 | / lists the agents that match what is typed | with the keyboard on the panel, after `/`, `second` and `Enter` the screen shows `1 match` within 8 s |
  | 19 | Esc ends the search | after `Esc` the screen shows `2 agents` within 8 s |
  | 20 | Alt-o hides the sidebar | a host tab reports `is_fullscreen_active` true |
  | 21 | Alt-o shows it again | it reports false |
  | 22 | Alt-d detaches the host client | host client count is 0 |
  | 23 | both sessions keep running | both listed and not `EXITED` |
  | 24 | the inner client stays attached in the guest pane | inner client count is 1 |
  | 25 | starting again attaches to the running pair | host client count is 1 within 10 s |
  | 26 | ending the inner session ends the host session | after `kill-session <inner>`, the host is gone within 10 s (skipped with `--keep`) |

  It ends with `nested: all checks passed` and exit `0`, or `nested: <N> check(s) failed` and exit `1`.
- **Source:** `zellij/e2e/nested.sh`
- **Verified by:** MANUAL: `make zellij-e2e`

### ZE-014 Cleanup
- **Behavior:** On exit, unless `--keep` was given, the harness MUST kill its tmux session and run `dev.sh stop`. With `--keep` the pair stays up for inspection (`tmux attach -t noc-e2e-tmux`).
- **Source:** `zellij/e2e/nested.sh` (`cleanup`)
- **Verified by:** MANUAL: session list before and after a run

### ZE-015 The fake `noc`
- **Behavior:** `zellij/e2e/fake-noc` is a plain `sh` stand-in. `ls` prints `zellij/target/e2e/agents.json` if it exists, else a built-in snapshot of two Quiet agents: `agent-e2e` (Codex, name `E2E agent`) and `agent-two` (Claude background, name `Second agent`), both with workspace `/tmp`. `show --json <id>` prints that agent's line, or `noc server: agent was not found` with exit `1`. `rename <id> <name>` appends its arguments to `zellij/target/e2e/renames.log` and prints `Named <id> "<name>".`. `stop <id>` prints `stopped <id>`. `attach <id>` for the two known ids prints `attached <id>` and then blocks (reading lines on a terminal, where `exit <N>` exits with `N`); for other ids it prints `noc: refusing to attach <id>` and exits `1`. Any other command prints `unexpected test command: …` and exits `2`.
- **Source:** `zellij/e2e/fake-noc`
- **Verified by:** `zellij/e2e/nested.sh` checks 2, 9, 10, 14

---

## Zellij behaviors relied upon

These are facts about Zellij 0.46.0 that the design depends on. Each was
confirmed by running it.

1. A layout file's top-level options and its `keybinds` block apply to the
   session that layout starts, over `config.kdl`: with `clear-defaults=true`
   the block replaces every binding (the host), and without it the bindings
   are added to the session's own (the inner session). This holds for a
   session created with `--new-session-with-layout` and with
   `attach --create … options --default-layout`. It is what lets the user's
   `config.kdl` serve both sessions untouched.
2. A layout's `plugin location="<URL>" { … }` passes its children to the
   plugin as settings, and `MessagePlugin "<URL>" { … }` passes the keys it
   does not know (`role`) the same way, so no `plugins { }` alias is needed.
3. Zellij has no include directive: an unknown top-level name in
   `config.kdl` is ignored without a message.
4. A plugin loaded by URL is downloaded once and cached by URL. A later
   session does not download again, so a `releases/latest/download/…` URL
   never upgrades. The plugin URL must name the version.
5. Permission grants are recorded per plugin location.
6. `zellij action write --pane-id` types into a pane that does not have focus.
7. A session created by `attach --create` from the guest pane has exactly one
   client.
8. Zellij opens a welcome screen in a new session until it is dismissed
   permanently. This happens in both sessions of the pair.

## Design choices

**The outer session sees keys first.** A terminal delivers every key to the
outermost Zellij. The host session clears the default bindings and binds only
`Alt m`, `Alt o` and `Alt d`, so every other key is typed into the guest pane
and reaches the inner session, which keeps all of the user's own bindings.
`nested_session_handling "never"` stops the host from taking over or zooming
into the inner session. The cost is that the host never tells the inner client
it has focus, which is why the panel types the focus frame itself (ZX-014,
ZX-017).

**The inner session is driven over the CLI.** The plugin API reaches only the
session the plugin runs in. The panel runs in the host session, and agents
live in the inner one, so it reads the inner session with `list-clients` and
`list-panes` and acts on it with `zellij --session <inner> …` (ZX-020,
ZX-031). The theme is the one thing the CLI cannot change, hence the relay
instance inside the inner session, reached by keys typed into the guest pane.

**Bytes are typed with `action write`, not `WriteToStdin`.** A keyboard
signal is sent just after the keyboard has moved, so it must reach the guest
pane whichever pane has the keyboard. `action write --pane-id` names its
target pane, runs under the `RunCommands` permission the panel already needs,
and leaves `WriteToStdin` out of the permission set.

**One permission set.** Zellij records one grant per plugin location, and the
panel, details window and relay share a location. If roles asked for different
sets, each new role would prompt again and replace the recorded grant. All
three ask for the same six, so the user answers once per location (ZH-002).

**Static configuration.** The user's files are theirs: two layout files and
a theme, readable and removable by hand, and nothing in
`config.kdl`. Nothing is
generated at run time and nothing edits the user's configuration, so there is
no installer to keep correct and no hidden state to repair. The plugin is
loaded by a versioned URL, so upgrading is a one-line change (D-034, D-036).

## Coverage gaps

Neither the unit tests nor `nested.sh` cover:

- A settings problem or the inner-session message reaching the screen in a
  running pair (only the rendering is unit tested), and the 5 s timing of
  ZX-022.
- The pipe transport end to end (only the decoder is unit tested).
- `p`, `s` and `w` end to end, the remembered-pane path, and the
  other-session refusal of ZX-028.
- The real `noc`: the harness always uses `fake-noc`, so nothing checks the
  plugin's model against the Go CLI's JSON here, and the `dev.sh` wrapper is
  not exercised.
- Stop and rename commands and their results in a running pair.
- Permission denial, and behaviour before permissions are granted.
- Detaching the inner client (ZX-007 step 4), and the shipped
  layouts as they are (the harness runs rendered
  copies with different names, a `file:` plugin location and built-in themes).
- Grabbing the keyboard while the guest pane is fullscreen, and `Alt m` while
  the details window has the keyboard.
- The focus frame: nothing asserts that the inner client draws itself focused.
- More than one terminal attached to the host session.
- Loading the plugin from a release URL, and the first-run permission prompt.
- Whether keys the host does not bind reach the inner session unchanged,
  including kitty-protocol keys such as Shift-Enter. MANUAL: press them in a
  program inside the inner session and compare with a plain Zellij session.

## Known limitations

- The focus frame is forged. It depends on Zellij's nested-session protocol
  (`ESC P 26661 n … ESC \`), which is not a public interface and may change
  between Zellij versions.
- The release check needs `curl` on the `PATH` Zellij was started with and
  access to github.com. Without either it finds nothing and says nothing.
- Polls have no timeout. The registry poll, the inner-session read and the
  details read each wait for their `RunCommandResult`; if one is lost, that
  poll never runs again until the plugin reloads.
- The panel's help text lists `Alt-m`, `Alt-o` and `Alt-d` as fixed text. It
  does not follow a layout that rebinds them.
- Keyboard signals and focus moves need the host session's name, which
  arrives with the first `ModeUpdate` that carries one. Before that, nothing
  is typed into the guest pane, and the grab falls back to the plugin API.
- Host actions are fire-and-forget: a failed `action write` or focus move is
  not reported or retried, so a missed theme key leaves the inner session in
  the wrong theme until the keyboard moves again.
- After a command in the inner session the host keyboard goes back to the
  guest pane even when the command failed; the failure shows as a notice in a
  panel that no longer has the keyboard.
- The inner session's focus is read from its first client only. With several
  clients attached to the inner session, following tracks the first one.
- Following and the missing-session message lag by up to one `0.75` s read.
- The session names are written in three places that must agree:
  `guest_session` and `inner=`/`host=` in `noc-host.kdl`, and the start
  command. Only a non-plain `guest_session` is detected; a mismatch shows
  as `inner session "<name>" is not running`.
- The theme keys are checked only against the plugin's own settings. If the
  bindings in `noc-inner.kdl` use different keys, name other themes than
  `home_theme` and `away_theme`, or the themes do not exist, nothing is
  reported and the theme does not change.
- With `pane_titles "hide"` or `"name"` a pane's title is frozen as the panel first saw
  it: a title its program sets later is not seen, by the user or by the
  panel, so an agent recognised only by its pane title is recognised by the
  title it had at first.
- The plugin URL is written three times: once in `noc-host.kdl` and twice
  in `noc-inner.kdl`. Upgrading means changing all three, which the
  installer does (ZE-016).
- The installer tells the user's choices from old defaults by fetching the
  host layout of the release the user has. When it cannot (no network, or
  a plugin location that names no release), every setting that is set
  counts as the user's, so a changed default does not arrive.
- `w` and `D` do not check that permissions were granted before acting, unlike
  Enter, `p` and `s`.
- The inner session is local and single: remote inner sessions and several
  inner sessions are out of scope.
