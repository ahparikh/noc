# 07 — Zellij panel (model, view, keys)

This section specifies what a user sees in the `zellij-noc` panel and what
every key in the panel does. It covers the Rust model of noc's public JSON
(`noc ls --json --all` and `noc show --json <id>`), how agents are grouped,
sorted and folded into Quiet time sections, the exact rendering of every line
(status line and status box, group headings, agent rows, section rows, inline
details, help, empty states, footer, floating details window), the in-panel
key bindings and the intents they raise (open, pane, stack, worktree shell,
details window, refresh, stop, rename), and what the panel does with the
results of `noc stop`, `noc rename`, `noc team set` and `noc team clear`.

The plugin has one deployment: the panel is a sidebar in an outer "host"
Zellij session, beside a single guest pane that holds the user's own Zellij
session attached nested (the "inner" session). One WASM build serves three
roles chosen by the `role` configuration key: `panel` (the default),
`details` (the floating details window) and `relay` (the theme relay inside
the inner session). This section covers the `panel` and `details` roles.

Plugin lifecycle, permissions, polling cadence, pipe decoding, keyboard
ownership (`Alt-m`, `Alt-o`, `Alt-d`), the theme relay, reading and driving
the inner session, the static configuration and the end-to-end suite belong
to [08-zellij-host.md](08-zellij-host.md); this section only names the
hand-off. The user guide (install, configuration reference, keys) is
[`docs/ZELLIJ.md`](../ZELLIJ.md). Decisions cited as `D-0NN` are in
[`docs/DECISIONS.md`](../DECISIONS.md).

Source files covered (with their `#[cfg(test)]` modules):

- `zellij/src/model.rs` — JSON model, grouping, ranks, symbols, labels, time.
- `zellij/src/app.rs` — panel state: configuration, rows, selection,
  sections, notices, pending stop and rename.
- `zellij/src/view.rs` — rendering of the panel and the details window.
- `zellij/src/details.rs` — the floating details-window instance.
- `zellij/src/panel.rs` — key handling, intents, stop and rename commands,
  opening agents.

Referenced only for the hand-off: `zellij/src/main.rs` (roles, event
dispatch, command results, render output), `zellij/src/guest.rs` (commands
run in the inner session), `zellij/src/panes.rs` (pane records, error text,
tab names).

The plugin depends on `zellij-tile` 0.46.0 and is used with Zellij 0.46.

---

## 1. Data model (`model.rs`, `app.rs` configuration)

### 1.1 Snapshot envelope

#### ZM-001 Snapshot envelope fields
- **Behavior:** A registry snapshot MUST deserialize into `AgentList` with
  `schema_version: u32` (required; missing → parse error
  ``missing field `schema_version` ``), `generated_at: String` (default `""`
  when missing) and `agents: Vec<Agent>` (default `[]` when missing).
- **Source:** `zellij/src/model.rs` (AgentList)
- **Verified by:** `zellij/src/model.rs` rejects_unknown_schema_versions

#### ZM-002 Only schema version 1 is accepted
- **Behavior:** `parse_snapshot` MUST reject any `schema_version` other than
  `SUPPORTED_SCHEMA_VERSION = 1` with the exact error
  `unsupported noc schema version {N} (expected 1)`. There is no
  forward-compatibility mode: a version-2 snapshot is an error even if its
  fields are compatible.
- **Source:** `zellij/src/model.rs` (parse_snapshot)
- **Verified by:** `zellij/src/model.rs` rejects_unknown_schema_versions

#### ZM-003 Input is trimmed and parsed as one JSON document
- **Behavior:** `parse_snapshot` and `parse_agent` MUST trim surrounding
  whitespace before parsing and MUST return serde_json's error message
  verbatim (e.g. `invalid type: null, expected a string at line 1 column 51`)
  on failure.
- **Source:** `zellij/src/model.rs` (parse_agent, parse_snapshot)
- **Verified by:** `zellij/src/model.rs` null_is_accepted_only_for_option_fields (error text begins `invalid type: null`); trimming `UNTESTED`

#### ZM-004 Snapshots are sorted on parse, not on apply
- **Behavior:** `parse_snapshot` MUST sort `agents` with the comparator of
  ZG-001 before returning. `ZellijNoc::apply_snapshot` MUST NOT re-sort;
  every snapshot source (poll result, pipe frame) goes through
  `parse_snapshot`. `parse_agent` (details window) does not sort (one agent).
- **Source:** `zellij/src/model.rs` (parse_snapshot), `zellij/src/app.rs` (apply_snapshot)
- **Verified by:** `zellij/src/model.rs` parses_and_sorts_attention_first

### 1.2 Agent fields

#### ZM-005 Agent fields and defaults
- **Behavior:** Each agent MUST deserialize with `id` required (missing →
  ``missing field `id` ``) and every other field defaulted when absent, per
  the table below. Unknown fields at every level (envelope, agent, nested
  objects) MUST be ignored.
- **Source:** `zellij/src/model.rs` (Agent)
- **Verified by:** `zellij/src/model.rs` unnamed_agents_are_titled_by_short_id_not_first_prompt (sparse agents parse); `zellij/src/model.rs` rejects_unknown_schema_versions (unknown fields)

| JSON field | Rust type | Default when missing | Used for |
|---|---|---|---|
| `id` | `String` | required | identity, selection, argv |
| `host_id` | `String` | `""` | not displayed |
| `provider` | `String` | `""` | icon, provider label, stop policy |
| `native_session_id` | `String` | `""` | title placeholder test, details `id`/`session`, open precondition |
| `source_kind` | `String` | `""` | provider detail label, stop policy |
| `native_short_id` | `String` | `""` | title fallback, details `id`, stop precondition |
| `user_name` | `String` | `""` | title (first precedence), details `native` line condition |
| `name` | `String` | `""` | title, details `native` line |
| `display_state` | `String` | `""` | group fallback, rank, symbol, label |
| `bucket` | `String` | `""` | group (D-046) |
| `live_state` | `String` | `""` | Quiet symbol, provider detail `ended`, binding filtering |
| `runtime_state` | `String` | `""` | provider detail label |
| `activity_state` | `String` | `""` | not displayed |
| `model` | `String` | `""` | details `model` line |
| `reasoning_effort` | `String` | `""` | details `model`/`effort` line |
| `initial_request` | `Option<TextPreview>` | `None` | details `ask` preview |
| `summary` | `String` | `""` | details `latest` preview |
| `summary_source` | `String` | `""` | not displayed |
| `last_activity_at` | `Option<String>` | `None` | recency (1st) |
| `last_seen_at` | `Option<String>` | `None` | parsed, deliberately never used for recency |
| `ended_at` | `Option<String>` | `None` | recency (2nd) |
| `created_at` | `Option<String>` | `None` | recency (3rd) |
| `first_seen_at` | `Option<String>` | `None` | recency (4th) |
| `attention` | `Attention` | `{kind:"", detail:""}` | Blocked secondary line |
| `workspace` | `Option<Workspace>` | `None` | repo/branch/worktree/cwd, `w` directory, attach cwd |
| `frontend_bindings` | `Vec<FrontendBinding>` | `[]` | details `pane` line; pane resolution in the inner session |
| `team` | `String` | `""` | team grouping, the team on the secondary line (ZM-025) |
| `lead` | `bool` | `false` | the lead's place and mark (ZM-025) |
| `parent_id` | `String` | `""` | parsed, not displayed |

Nested types (all fields default to `""`/`false` when missing):

| Type | Fields |
|---|---|
| `Attention` | `type` (Rust `kind`), `detail` |
| `TextPreview` | `text`, `source`, `truncated` (bool, parsed, never displayed) |
| `Workspace` | `path`, `root_path`, `kind`, `common_git_dir`, `branch` |
| `FrontendBinding` | `kind`, `session`, `pane_id`, `state`, `observed_at` |

Public API fields that are not modelled are silently ignored, e.g.
`capabilities`, `permission_mode`, `native_state`, `process_id`,
`model_source`, `summary_truncated`, `summary_updated_at`, workspace
`head_sha` / `repository_id`. The API itself is specified in
[02-api.md](02-api.md) and the CLI JSON in [03-cli.md](03-cli.md).

#### ZM-006 JSON `null` tolerance
- **Behavior:** `null` MUST be accepted only for `Option` fields
  (`initial_request`, `workspace`, and the five timestamps). `null` for any
  `String`, `Vec` or struct field (e.g. `"name": null`, `"attention": null`,
  `"frontend_bindings": null`) MUST fail the whole document with serde's error
  (e.g. `invalid type: null, expected a string ...`). Defaults apply only to
  absent fields.
- **Source:** `zellij/src/model.rs` (Agent field attributes)
- **Verified by:** `zellij/src/model.rs` null_is_accepted_only_for_option_fields

### 1.3 Groups

#### ZM-007 Four groups in fixed order
- **Behavior:** `AgentGroup` MUST have exactly four variants ordered
  `Blocked < Working < Review < Quiet`, with titles `Blocked`, `Working`,
  `Review`, `Quiet`.
- **Source:** `zellij/src/model.rs` (AgentGroup, AgentGroup::title)
- **Decision:** D-046
- **Verified by:** `zellij/src/model.rs` groups_finished_turns_apart_from_real_questions

#### ZM-008 Group comes from `bucket`, with display-state fallback
- **Behavior:** `Agent::group` MUST return the group named by `bucket` when it
  is exactly `blocked`, `working`, `review` or `quiet`. Otherwise (empty or
  unknown bucket) it MUST map `display_state`:

  | `display_state` | Group |
  |---|---|
  | `needs_approval`, `needs_input`, `needs_authentication`, `error` | Blocked |
  | `working`, `waiting` | Working |
  | `ready` | Review |
  | anything else (incl. `idle`, `stale`, `not_live`, `""`) | Quiet |

  The fallback does not consider `live_state`, so a row with no bucket and an
  ended `needs_input` state maps to Blocked. "Only live agents are Blocked"
  (D-048) is enforced server-side through `bucket`.
- **Source:** `zellij/src/model.rs` (Agent::group)
- **Decision:** D-046, D-048
- **Verified by:** `zellij/src/model.rs` prefers_the_bucket_noc_reports; `zellij/src/model.rs` groups_finished_turns_apart_from_real_questions

### 1.4 Display identity

#### ZM-009 Title precedence: user_name > name > short id > id prefix
- **Behavior:** `Agent::title` MUST return the first applicable of:
  1. `user_name` with all whitespace runs collapsed to single spaces and ends
     trimmed (`compact`), if `user_name.trim()` is non-empty;
  2. `compact(name)`, if `name.trim()` is non-empty and `name` is not an id
     placeholder (ZM-010);
  3. `native_short_id.trim()`, if non-empty;
  4. the first 8 Unicode scalar values of `id`, if `id` is non-empty;
  5. the literal `unnamed agent`.

  The first prompt (`initial_request`) MUST NOT be used as a title.
- **Source:** `zellij/src/model.rs` (Agent::title, compact)
- **Decision:** D-035, D-042
- **Verified by:** `zellij/src/model.rs` parses_and_sorts_attention_first; `zellij/src/model.rs` unnamed_agents_are_titled_by_short_id_not_first_prompt

#### ZM-010 An id placeholder is not a name
- **Behavior:** `name` MUST be treated as absent when `name.trim()` equals
  `native_short_id.trim()` or `native_session_id.trim()`.
- **Source:** `zellij/src/model.rs` (Agent::name_is_id_placeholder)
- **Decision:** D-042
- **Verified by:** `zellij/src/model.rs` unnamed_agents_are_titled_by_short_id_not_first_prompt

#### ZM-011 Recency excludes `last_seen_at`
- **Behavior:** `Agent::recency` MUST return the first present of
  `last_activity_at`, `ended_at`, `created_at`, `first_seen_at`, else `None`.
  It MUST NOT use `last_seen_at` (when noc last looked), which would make
  every listed idle agent read `0m`.
- **Source:** `zellij/src/model.rs` (Agent::recency)
- **Verified by:** `zellij/src/model.rs` recency_ignores_when_noc_last_looked

### 1.5 State rank, symbol and label

#### ZM-012 State rank, symbol and label tables
- **Behavior:** `state_rank`, `state_symbol` and `state_label` MUST map
  `display_state` exactly as follows.

  | `display_state` | rank | symbol | label |
  |---|---|---|---|
  | `needs_approval` | 0 | `!` | `needs approval` |
  | `needs_input` | 1 | `?` | `needs input` |
  | `needs_authentication` | 2 | `@` | `needs authentication` |
  | `error` | 3 | `×` (U+00D7) | `error` |
  | `working` | 4 | `●` | `working` |
  | `waiting` | 5 | `…` | `waiting` |
  | `ready` | 6 | `◆` | `review` (named for its group) |
  | `idle` | 7 | `○` | `idle` |
  | `stale` | 8 | `~` | `stale` |
  | `not_live` | 9 | `◌` | `not live` |
  | `""` | 10 | `·` | `unknown` |
  | any other `x_y` | 10 | `·` | `x y` (underscores → spaces) |

- **Source:** `zellij/src/model.rs` (state_rank, state_symbol, state_label)
- **Decision:** D-044
- **Verified by:** `zellij/src/model.rs` provider_icons_and_review_label_are_single_column (`ready`, `error`, `not_live`, empty and unknown states); the remaining rows `UNTESTED`

### 1.6 Provider

#### ZM-013 Provider icon, nerd and unicode sets
- **Behavior:** `provider_icon(nerd)` MUST return:

  | provider | `icons "nerd"` | otherwise |
  |---|---|---|
  | `claude` | U+EC82 (Nerd Font Claude codicon) | `✳` (U+2733) |
  | `codex` | U+EC81 (Nerd Font OpenAI codicon) | `⬡` (U+2B21) |
  | `""` | `·` | `·` |
  | any other value | the provider string itself | the provider string itself |

  Each Claude/Codex icon MUST be a single scalar; the Unicode ones MUST have
  display width 1.
- **Source:** `zellij/src/model.rs` (Agent::provider_icon)
- **Verified by:** `zellij/src/model.rs` provider_icons_and_review_label_are_single_column

#### ZM-014 Provider labels
- **Behavior:** `provider_label` MUST be `provider`, or `unknown` when empty.
  `provider_detail_label` MUST join with ` · ` (space, U+00B7, space):
  the provider label; `source_kind` with `_` → space, if non-empty; and a
  process word — `ended` if `live_state == "not_live"`, else `starting`,
  `stopped` or `failed` when `runtime_state` is exactly that, else nothing.
  Example outputs: `claude · background`, `codex · cli · ended`.
- **Source:** `zellij/src/model.rs` (Agent::provider_label, Agent::provider_detail_label)
- **Verified by:** `zellij/src/model.rs` describes_the_provider_readably

### 1.7 Workspace

#### ZM-015 Repository name
- **Behavior:** `Workspace::repo_name` MUST return the final path component
  of the parent of `common_git_dir.trim()` when that path's final component is
  exactly `.git` and the parent has a name (so a linked worktree names its
  repository); otherwise the final component of `root_path.trim()` (or of
  `path.trim()` when `root_path` is empty); otherwise `""`. Trailing slashes
  are ignored.
- **Source:** `zellij/src/model.rs` (Workspace::repo_name)
- **Verified by:** `zellij/src/model.rs` names_the_repository_not_the_checkout_kind

#### ZM-016 Git detection, worktree name, changes directory
- **Behavior:** A workspace is a linked worktree when `kind` is `worktree`
  (the value noc emits) or `linked_worktree` (also accepted). `is_git` MUST be
  true iff `common_git_dir.trim()` is non-empty, or `kind` is `main_checkout`,
  or the workspace is a linked worktree. `worktree_name` MUST be
  `Some(final component of root_path, or of path when root_path is empty)`
  only for a linked worktree with a non-empty such component, else `None`.
  `changes_directory` MUST be `root_path.trim()` when non-empty, else
  `path.trim()`.
- **Source:** `zellij/src/model.rs` (Workspace::is_linked_worktree, Workspace::is_git, Workspace::worktree_name, Workspace::changes_directory)
- **Verified by:** `zellij/src/model.rs` names_the_repository_not_the_checkout_kind

#### ZM-017 Workspace label for the list
- **Behavior:** `workspace_label` MUST be `""` with no workspace; otherwise
  `{repo} · {branch}`, or `{repo}` when branch is empty, or `{branch}` when
  repo is empty. It does NOT consult `is_git`, so a plain directory shows its
  folder name (contrast ZR-027).
- **Source:** `zellij/src/model.rs` (Agent::workspace_label)
- **Verified by:** `zellij/src/model.rs` parses_and_sorts_attention_first; `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (`docs` for a plain directory)

### 1.8 Bindings

#### ZM-018 Display binding vs navigable bindings
- **Behavior:** `zellij_binding` (used only by the details `pane` line) MUST
  return the binding with `kind == "zellij"`, non-empty `pane_id` and the
  greatest `observed_at` (string order), regardless of `state` or
  `live_state`. `zellij_bindings` (used to find an agent's pane and to detect
  another session holding it; see [08-zellij-host.md](08-zellij-host.md))
  MUST return `[]` when `live_state == "not_live"`, else every
  `kind == "zellij"` binding with non-empty `pane_id` and
  `state != "disconnected"`, newest `observed_at` first.
- **Source:** `zellij/src/model.rs` (Agent::zellij_binding, Agent::zellij_bindings)
- **Decision:** D-034, D-050
- **Verified by:** `zellij/src/model.rs` zellij_bindings_skip_ended_and_disconnected_agents

### 1.9 Time

#### ZM-019 RFC 3339 parsing
- **Behavior:** Timestamps MUST be trimmed and parsed by fixed byte
  positions: year `[0..4]`, month `[5..7]`, day `[8..10]`, hour `[11..13]`,
  minute `[14..16]`, second `[17..19]`. Each field MUST be all ASCII digits;
  the separator characters between them are not validated. A text shorter
  than 19 bytes, a month outside 1–12, a day outside 1–31, an hour above 23,
  a minute above 59 or a second above 60 makes the timestamp unknown
  (`None`). After byte 19 a `.` and any ASCII digits (fractional seconds) are
  skipped and ignored. The remaining zone MUST be `Z`, `z` or empty (UTC), or
  `±HH:MM`; anything else (including `+0000` without a colon) makes the
  timestamp unknown.
- **Source:** `zellij/src/model.rs` (unix_seconds)
- **Verified by:** `zellij/src/model.rs` labels_ages_from_rfc3339_times; range checks `UNTESTED`

#### ZM-020 Age seconds clamp to zero
- **Behavior:** `age_seconds(then, now)` MUST be `None` if either is missing
  or unparsable, else `max(now - then, 0)` seconds.
- **Source:** `zellij/src/model.rs` (age_seconds)
- **Verified by:** `zellij/src/model.rs` the_clock_round_trips_through_the_age_parser

#### ZM-021 Relative age label
- **Behavior:** `age_label(then, now)` MUST return `""` when the age is
  unknown; otherwise, with `s` = age in whole seconds and floor division:
  `s < 60` → `0m` (never `now`, so ages align in a column); `s < 3600` →
  `{s/60}m`; `s < 86400` → `{s/3600}h`; else `{s/86400}d`.
- **Source:** `zellij/src/model.rs` (age_label)
- **Verified by:** `zellij/src/model.rs` labels_ages_from_rfc3339_times

#### ZM-022 Wall clock for the details window
- **Behavior:** `now_rfc3339` MUST format the current UTC time as
  `YYYY-MM-DDTHH:MM:SSZ` (20 characters).
- **Source:** `zellij/src/model.rs` (now_rfc3339, format_rfc3339)
- **Verified by:** `zellij/src/model.rs` the_clock_round_trips_through_the_age_parser

### 1.10 Plugin configuration that affects the panel

#### ZM-023 Configuration keys
- **Behavior:** `Config::from_map` MUST read the plugin configuration map as
  below. Values are trimmed; an empty value counts as absent. Keys not listed
  are ignored by `Config` (`role` and `agent_id` are read in `main.rs`).

  | Key | Default | Parsing | Panel effect |
  |---|---|---|---|
  | `noc_bin` | `noc` | non-empty trimmed | argv[0] of every `noc` command |
  | `icons` | (unicode) | exactly `nerd` → Nerd icons | ZM-013 |
  | `transport` | `poll` | exactly `pipe` → pipe | the words for the connection (ZR-003); `r` is a no-op in pipe |
  | `pipe_name` | `zellij-noc.registry` | non-empty trimmed | `◌  waiting for {pipe_name}` |
  | `poll_interval_ms` | 1000 | `f64`, finite, clamped to 250..60000; otherwise the default | registry polling cadence |
  | `guest_session` | `noc` | non-empty trimmed | the inner session agents are opened, focused and followed in |
  | `home_theme` | none | non-empty trimmed | the inner session's normal theme |
  | `away_theme` | none | non-empty trimmed | the inner session's theme while the panel has the keyboard |
  | `away_key` | `Ctrl Alt Shift F11` | non-empty trimmed | key typed into the inner session when the keyboard leaves it |
  | `home_key` | `Ctrl Alt Shift F12` | non-empty trimmed | key typed into the inner session when the keyboard returns |
  | `update_check` | on | exactly `false` → off | whether the header can show `↑ vX.Y.Z` (ZR-002) |
  | `colors` | built-in | exactly `zellij` → follow the Zellij theme | ZR-018 |
  | `pane_titles` | show | exactly `name` → name the inner session's panes; exactly `hide` → blank their titles | [08-zellij-host.md](08-zellij-host.md) ZX-038 |
  | `group_by` | status | exactly `team` → group by team; any other value is `status` | how the list is grouped when the panel starts (ZG-017) |

  What the theme and key settings do is specified in
  [08-zellij-host.md](08-zellij-host.md); the user-facing reference is
  [`docs/ZELLIJ.md`](../ZELLIJ.md).
- **Source:** `zellij/src/app.rs` (Config, Config::default, Config::from_map)
- **Verified by:** `zellij/src/app.rs` parses_bounded_poll_configuration

#### ZM-024 Settings validation and key encoding
- **Behavior:** `Config::problems()` MUST return, in this order, one message
  per problem (empty when the settings are usable). A "plain name" is
  non-empty and made only of ASCII alphanumerics, `-`, `_` and `.`; values
  are quoted Rust-`Debug` style.

  1. `guest_session {value} is not a plain session name`.
  2. Neither theme set: stop here (the keys are not checked).
  3. Only one theme set: `home_theme is set but away_theme is not`, or
     `away_theme is set but home_theme is not`.
  4. Both set: `{key} {value} is not a plain theme name` for each offender,
     then `home_theme and away_theme are the same theme` if they are equal.
  5. For `away_key` then `home_key`, if `encode_key` rejects it:
     `{key} {value} is not a function key such as "Ctrl Alt Shift F11"`.
  6. `away_key and home_key are the same key` if the two strings are equal.

  `encode_key` MUST accept exactly one `F1`..`F12` token plus any of the
  tokens `Shift`, `Alt`, `Ctrl` in any order (whitespace-separated,
  case-sensitive) and return the bytes a terminal sends: with modifier
  parameter `p = 1 + Shift(1) + Alt(2) + Ctrl(4)`, `F1`–`F4` are `ESC O P/Q/R/S`
  unmodified and `ESC [ 1 ; p P/Q/R/S` modified; `F5`–`F12` are `ESC [ c ~`
  unmodified and `ESC [ c ; p ~` modified with `c` = 15, 17, 18, 19, 20, 21,
  23, 24. Anything else returns `None`.

  `themes_enabled()` MUST be true only when there are no problems and both
  themes are set. A problem never stops the agent list from working. At
  load, the first problem (if any) is stored as `config_problem` and shown on
  the status dot and the status box (ZR-003, ZR-034) for the life of the instance.
- **Source:** `zellij/src/app.rs` (is_plain_name, encode_key, Config::problems, Config::themes_enabled), `zellij/src/main.rs` (load)
- **Verified by:** `zellij/src/app.rs` reports_unusable_settings_in_plain_words; `zellij/src/app.rs` encodes_function_keys_the_way_a_terminal_sends_them

---

### 1.11 Teams

#### ZM-025 Team and lead
- **Behavior:** An agent's team is its `team` field with surrounding
  whitespace trimmed; an empty result means the agent is in no team. An
  agent is its team's lead when `lead` is true and it is in a team; `lead`
  on an agent in no team is ignored. The panel compares team names exactly,
  never changes them, and reads nothing else about teams: which agents are
  in a team, and which one leads it, is decided by `noc`
  ([03-cli.md](03-cli.md)).
- **Source:** `zellij/src/model.rs` (Agent::team_name, Agent::is_lead)
- **Decision:** D-071
- **Verified by:** `zellij/src/model.rs` reads_the_team_and_the_lead_mark

## 2. Grouping, sorting, sections, selection (`model.rs`, `app.rs`)

### 2.1 Sorting

#### ZG-001 Agent comparator
- **Behavior:** Agents MUST be ordered by, in turn:
  1. group (`Blocked`, `Working`, `Review`, `Quiet`);
  2. for Blocked and Working only, `state_rank` ascending (ZM-012); Review
     and Quiet skip this key ("most recent first");
  3. `recency()` descending;
  4. `title().to_lowercase()` ascending;
  5. `id` ascending.
- **Source:** `zellij/src/model.rs` (compare_agents)
- **Verified by:** `zellij/src/model.rs` parses_and_sorts_attention_first; `zellij/src/model.rs` groups_finished_turns_apart_from_real_questions

#### ZG-002 Recency compares raw strings; unknown sorts last
- **Behavior:** The recency key MUST compare the `Option<&str>` values
  lexically (not parsed instants): `Some(_)` is newer than `None`, so agents
  without any timestamp sort after timed ones within the same group. Mixed
  offsets or fractional formats compare by string, not by instant.
- **Source:** `zellij/src/model.rs` (compare_agents)
- **Verified by:** `zellij/src/model.rs` groups_finished_turns_apart_from_real_questions (`gone` with `ended_at` before `parked` with none); mixed-format ordering `UNTESTED`

### 2.2 Rows (`items`)

#### ZG-003 `agents` vs `items`
- **Behavior:** `ZellijNoc.agents` MUST hold every agent of the last
  snapshot except those in the stopped set (ZG-012), for lookups.
  `ZellijNoc.items` MUST hold the visible rows in order, each either
  `Agent(index into agents)` or `Section { age, count, expanded }`, or,
  when the list is grouped by team (ZG-017), `Team { team, count, expanded,
  blocked, working, review }` or `TeamQuiet { team, count, expanded }`,
  where `team` indexes `ZellijNoc.teams`, the team names in list order;
  `selected_index` indexes `items`.
- **Source:** `zellij/src/app.rs` (ZellijNoc, ListItem)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections

#### ZG-004 Row layout
- **Behavior:** `rebuild_items` MUST emit, in `agents` order, one `Agent` row
  for every non-Quiet agent; then for each `QuietAge` in order `Day`,
  `TwoDays`, `Week` with at least one member: one `Section` row with the
  member count and expanded flag, followed (only if expanded) by its members'
  `Agent` rows in `agents` order (most recent first). Empty sections MUST NOT
  produce a row. A group with no agents produces no rows (and hence no
  heading, ZR-010).
- **Source:** `zellij/src/app.rs` (rebuild_items)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections; `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections

#### ZG-005 Quiet time sections and boundaries
- **Behavior:** A Quiet agent's section MUST be computed from
  `age_seconds(recency(), generated_at)` (snapshot time, not wall clock):

  | Section | Label | Membership (age in seconds) |
  |---|---|---|
  | `Day` | `< 24h` | unknown age, or `age < 86400` |
  | `TwoDays` | `< 48h` | `86400 <= age < 172800` |
  | `Week` | `< 7d` | `172800 <= age < 604800` |
  | (none) | — | `age >= 604800`: not shown in the panel |

  Sections MUST NOT overlap. An agent with no known time — including when
  `generated_at` is empty or unparsable — MUST go into `< 24h` rather than
  disappear. Non-Quiet agents never get a section.
- **Source:** `zellij/src/app.rs` (QuietAge, QuietAge::of, quiet_age)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections

#### ZG-006 Older than seven days is excluded from rows only
- **Behavior:** A Quiet agent aged `>= 7d` MUST NOT appear in `items`, in any
  heading count or in the header count, but MUST remain in `agents` (so
  lookups by id and by pane still find it). `select_agent` for such an agent
  MUST return `false`.
- **Source:** `zellij/src/app.rs` (rebuild_items, select_agent)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections

#### ZG-007 Sections start collapsed; open state persists
- **Behavior:** All sections MUST start collapsed. The set of expanded
  sections MUST persist across snapshots for the lifetime of the panel
  instance (it is keyed by `QuietAge`, not by members). A section that
  becomes empty disappears but stays "expanded" in the set, so it reappears
  open when it regains members.
- **Source:** `zellij/src/app.rs` (ZellijNoc `expanded`, rebuild_items)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections; `zellij/src/app.rs` section_open_state_survives_a_section_emptying

### 2.3 Selection

#### ZG-008 Selection preserved by identity across snapshots
- **Behavior:** Before replacing rows, the panel MUST capture the selection
  as `Agent(id)` or `Section(age)` and, after rebuilding, MUST select the row
  with the same agent id or the same section age, wherever it moved.
- **Source:** `zellij/src/app.rs` (apply_snapshot, selection_key, position_of, restore_selection)
- **Verified by:** `zellij/src/app.rs` preserves_selection_by_agent_id_across_snapshots

#### ZG-009 Selection when the selected row disappears
- **Behavior:** If the captured agent/section is no longer a row (agent gone,
  stopped, aged out, or moved into a collapsed section), the panel MUST keep
  the previous numeric index clamped to `items.len() - 1` (0 when empty), i.e.
  select whatever row now occupies that position. It MUST NOT auto-expand a
  section on refresh.
- **Source:** `zellij/src/app.rs` (restore_selection)
- **Verified by:** `zellij/src/app.rs` selection_falls_back_to_the_old_index_when_the_row_disappears

#### ZG-010 Programmatic selection opens a closed section
- **Behavior:** `select_agent(id)` MUST: if the agent is not a row but is a
  Quiet agent within 7 days, add its section to the expanded set and rebuild;
  then select its row, clear the pending `g`, and return `true`; return
  `false` (selection unchanged) if the agent is unknown or older than 7 days.
  When the list is grouped by team and the agent is not a row, it instead
  opens the agent's team and, for a Quiet agent that is not the lead, that
  team's Quiet row, and rebuilds; an agent that still is not a row (a Quiet
  agent in no team, older than 7 days) returns `false`.
  The panel uses this to make the selection follow the inner session's
  focused pane (except while the panel itself has the keyboard), so focusing
  a Quiet agent's pane opens its section. How the focused pane is read and
  matched to an agent is in [08-zellij-host.md](08-zellij-host.md).
- **Source:** `zellij/src/app.rs` (select_agent); caller `zellij/src/guest.rs` (follow_guest_focus)
- **Verified by:** `zellij/src/app.rs` selects_listed_agents_by_id_only; `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections; `zellij/src/app.rs` teams_group_by_name_with_the_lead_first_and_quiet_agents_in_one_row, team_groups_fold_and_keep_their_state_across_snapshots

#### ZG-011 Initial state
- **Behavior:** A new panel MUST start with `selected_index = 0`,
  `show_details = true`, `show_help = false`, connection `Connecting`, no
  notice, no settings or inner-session problem, no pending stop or rename,
  all sections collapsed, every team open and every team's Quiet row
  closed, the grouping set by `group_by` (ZM-023), spinner frame 0,
  `last_render_rows = 20`.
- **Source:** `zellij/src/app.rs` (ZellijNoc::new)
- **Verified by:** `zellij/src/app.rs` a_new_panel_starts_with_the_documented_defaults

### 2.4 Grouping by team

#### ZG-017 Two groupings
- **Behavior:** The list is grouped by `status` (ZG-004: Blocked, Working,
  Review, Quiet) or by `team` (ZG-018). The grouping starts as the
  `group_by` setting and `v` switches it (ZK-029). Switching MUST rebuild
  the rows and keep the selected agent selected; a selected row that does
  not exist in the other grouping (a section, a team heading, a team's
  Quiet row) leaves the selection at the same index, clamped as in ZG-009.
  While the list holds search results (ZK-026) it is the flat list of
  matches in either grouping; the grouping in effect returns when the
  search ends. Which sections and teams are open is kept separately for
  each grouping and survives switching.
- **Source:** `zellij/src/app.rs` (GroupBy, by_team, toggle_group_by, rebuild_items)
- **Decision:** D-070
- **Verified by:** `zellij/src/app.rs` switching_the_grouping_keeps_the_selected_agent, parses_bounded_poll_configuration

#### ZG-018 Rows when grouped by team
- **Behavior:** Grouped by team, `rebuild_items` MUST emit one group per
  team, ordered by team name (byte order), then one group for the agents in
  no team when there are any. A Quiet agent in no team that is older than 7
  days is not part of that group (as in ZG-006); every agent of a named
  team is part of its group, whatever its age. Each group is:
  1. a `Team` row carrying the number of agents in the group and how many
     of them are Blocked, Working and Review;
  2. only when the team is open:
     - the team's lead (ZM-025), whatever its state; if several agents are
       marked lead, the first in `agents` order;
     - the other agents that are not Quiet, in `agents` order (so Blocked,
       then Working, then Review, ZG-001);
     - when any other agent is Quiet, one `TeamQuiet` row with their count,
       followed, only when that row is open, by those agents in `agents`
       order.

  A team with no agents has no group. `ZellijNoc.teams` holds the group
  names in order, the agents in no team as the empty name.
- **Source:** `zellij/src/app.rs` (rebuild_team_items)
- **Decision:** D-070, D-071
- **Verified by:** `zellij/src/app.rs` teams_group_by_name_with_the_lead_first_and_quiet_agents_in_one_row

#### ZG-019 Teams start open; open state persists
- **Behavior:** Every team's group MUST start open and every team's Quiet
  row closed. The panel remembers, by team name, the teams the user folded
  and the Quiet rows the user opened, across snapshots and across switching
  the grouping, until the plugin is reloaded; a team that disappears and
  comes back is as it was left. Selection is preserved across snapshots by
  identity as in ZG-008: an agent by id, a `Team` or `TeamQuiet` row by
  team name.
- **Source:** `zellij/src/app.rs` (folded_teams, open_team_quiet, selection_key, position_of)
- **Decision:** D-070
- **Verified by:** `zellij/src/app.rs` team_groups_fold_and_keep_their_state_across_snapshots

### 2.5 Snapshot application side effects

#### ZG-012 Stopped agents stay hidden until the registry drops them
- **Behavior:** `apply_snapshot` MUST first drop from the stopped set every id
  not present in the incoming snapshot, then remove from `agents` every agent
  whose id is still in the stopped set. Consequently a stopped agent is hidden
  while the registry keeps reporting it, and shown again if it reappears
  after at least one snapshot without it. The set is per panel instance and
  in-memory only.
- **Source:** `zellij/src/app.rs` (apply_snapshot)
- **Decision:** D-043
- **Verified by:** `zellij/src/app.rs` stopped_agents_stay_hidden_until_the_registry_drops_them

#### ZG-013 Snapshot replaces time base and connection state
- **Behavior:** `apply_snapshot(snapshot, source)` MUST set `generated_at` to
  the snapshot's value (the reference time for every age in the panel) and
  set the connection to `Connected(source)` with `source` = `poll` or
  `pipe`.
- **Source:** `zellij/src/app.rs` (apply_snapshot); callers `zellij/src/panel.rs` (on_snapshot_result), `zellij/src/main.rs` (on_registry_payload)
- **Verified by:** `zellij/src/app.rs` preserves_selection_by_agent_id_across_snapshots

#### ZG-014 Notices age out after five snapshots
- **Behavior:** `set_notice` MUST reset the notice age to 0. Each
  `apply_snapshot` while a notice is shown MUST increment the age and clear
  the notice when it reaches `NOTICE_REFRESHES = 5` (so it survives four
  snapshots and is gone after the fifth). Notices are also cleared by
  `move_selection`, the second `g` of `gg`, `G`, and permission grant.
- **Source:** `zellij/src/app.rs` (set_notice, apply_snapshot, move_selection, press_g, select_last), `zellij/src/main.rs` (on_granted)
- **Verified by:** `zellij/src/app.rs` notices_survive_a_few_refreshes; `zellij/src/app.rs` navigation_clamps_and_supports_gg_and_capital_g

#### ZG-015 Registry results map to connection states
- **Behavior:** For a `noc ls --json --all` result: exit 0 with valid UTF-8
  and a valid snapshot → ZG-013; exit 0 with non-UTF-8 →
  `Error("noc returned non-UTF-8 output: {e}")`; exit 0 with a parse error →
  `Error(<ZM-002/ZM-003 message>)`; non-zero exit → stderr compacted to one
  line (whitespace collapsed, at most 180 characters then `…`) and, if it
  contains (case-insensitive) `is the server running?`,
  `noc server is not running`, or `dial unix` together with
  `no such file or directory` or `connection refused`, `ServerUnavailable`;
  otherwise `Error(<compacted stderr>)` or, when stderr is empty,
  `Error("noc ls failed with status {code|unknown}")`. A pipe frame that
  fails to parse → `Error("invalid registry snapshot: {e}")`. Denied
  permissions →
  `Error("permissions denied: the panel cannot read the registry or open agents")`.
  Errors MUST NOT clear `agents`/`items`: the last good list stays visible.
- **Source:** `zellij/src/panel.rs` (on_snapshot_result), `zellij/src/main.rs` (on_registry_payload, on_denied), `zellij/src/panes.rs` (compact_error, is_server_unavailable), `zellij/src/app.rs` (set_error, set_server_unavailable)
- **Verified by:** `zellij/src/panes.rs` compact_error_is_single_line_and_bounded; `zellij/src/panes.rs` recognizes_server_connection_failures_without_hiding_other_errors; `zellij/src/view.rs` errors_keep_the_last_good_list; the mapping in `on_snapshot_result` itself `UNTESTED`

#### ZG-016 Working spinner cadence
- **Behavior:** On every 0.25 s panel tick, if some agent in `agents` has
  `display_state == "working"`, the spinner frame MUST advance by one
  (mod 10) and the tick MUST request a redraw; otherwise the frame stays and
  the tick requests no redraw.
- **Source:** `zellij/src/app.rs` (advance_spinner, has_working_agents), `zellij/src/main.rs` (on_panel_tick, PANEL_TICK_SECONDS)
- **Verified by:** `zellij/src/app.rs` the_spinner_cycles_and_only_working_agents_need_redraws; `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections (the frame changes the row)

---

## 3. Rendering (`view.rs`)

### 3.1 Frame

#### ZR-001 Output geometry
- **Behavior:** `render(state, theme, rows, cols)` MUST record
  `last_render_rows = rows`, return no lines when `rows == 0 || cols == 0`,
  and otherwise return exactly `rows` lines (see ZR-016 for tiny panes),
  each padded or truncated to exactly `cols` display columns before
  styling. Control characters MUST be removed from every piece of text before
  it is measured and styled, so registry text cannot inject terminal escape
  sequences. The plugin prints the lines separated by newlines with no
  newline after the last (a trailing newline would scroll the pane by one).
- **Source:** `zellij/src/view.rs` (render, sanitize, Line::fitted, Line::render), `zellij/src/main.rs` (render)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` very_small_panes_keep_the_status_line; `zellij/src/view.rs` control_characters_in_names_never_reach_the_terminal

#### ZR-002 The status line
- **Behavior:** The panel has one status line (placed by ZR-033). While a
  prompt or a notice is active it is that text alone, the first that
  applies:

  | Condition (priority order) | Text | Colour |
  |---|---|---|
  | rename in progress | `Rename: {text}▏` (U+258F cursor) | accent |
  | team prompt open (ZK-030) | `team: {text}▏` (U+258F cursor) | accent |
  | stop awaiting confirmation | `Stop {title}?  y / n` | attention |
  | search being typed | `/ {text}▏` (U+258F cursor) | accent |
  | notice set | the notice text | info |

  Otherwise it MUST show, on the left, `{N} agent{s}` (`s` omitted only when
  `N == 1`; accent colour, bold), where `N` is the number of non-Quiet agent
  rows plus the sum of all Quiet section counts (open or closed); Quiet
  agents older than 7 days and stopped agents are not counted. Grouped by
  team, `N` is the sum of the team headings' counts (ZG-018). While the
  list holds search results (ZK-026) the count is `{N} match` or
  `{N} matches`, `N` being the number of rows, followed by two spaces and
  `/ {query}`. At the right edge it MUST show the status dot `●` (ZR-003),
  preceded by `↑` and a space (attention colour, bold) when a newer release
  is recorded ([08-zellij-host.md](08-zellij-host.md) ZH-022). The dot and
  the arrow are never dropped for lack of room: the count is cut with `…`
  instead. Neither says anything in words; the status box does (ZR-034).
- **Source:** `zellij/src/view.rs` (status_line, shown_agent_count, plural)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds, a_newer_release_is_marked_on_the_header_and_explained_in_help, search_results_are_a_flat_list_with_the_query_on_the_status_line, details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel

#### ZR-003 The status dot
- **Behavior:** The dot's colour MUST be the worst of the connection and
  the problems:

  | State | Colour | Words (in the status box) |
  |---|---|---|
  | `Connected(source)` | `ok` (green) | `connected · {source}` |
  | `Connecting`, poll | `warn` (yellow) | `connecting` |
  | `Connecting`, pipe | `warn` | `waiting for {pipe_name}` |
  | a settings problem (ZM-024) or an unreachable inner session, while connected or connecting | `warn` | the problem |
  | `ServerUnavailable` | `error` (red) | `server offline · retrying` |
  | `Error(e)` | `error` | `disconnected · {e}` |

- **Source:** `zellij/src/view.rs` (connection_state, problems, health)
- **Verified by:** `zellij/src/view.rs` renders_a_friendly_server_start_instruction (red, green), empty_and_error_states (yellow), problems_turn_the_dot_yellow_and_are_spelled_out_in_the_status_box

#### ZR-034 The status box
- **Behavior:** While the status box is open (ZK-028) the panel MUST draw a
  framed box over the last lines of the body, directly above the status
  line, as wide as the pane: a top border `╭─ status ─…─╮`, one line per
  entry as `│ {text} │` with the text cut to the inner width, and a bottom
  border `╰─…─╯` (borders in the muted colour). The entries, in order:
  1. `● {words}` for the connection, in the dot's connection colour
     (ZR-003).
  2. Each problem, settings first, as `! {problem}` (`warn`), wrapped with
     continuation lines indented two columns.
  3. With no newer release recorded: `version v{own version}` (muted). With
     one: `↑ vX.Y.Z is available` (attention, bold), `  this is v{own
     version}` (muted), and `set the new version in the plugin URL`
     wrapped and indented two columns (muted).

  When the body has fewer lines than the box, the box's last lines are
  shown. Nothing else moves. The box is not drawn in a pane narrower than 8
  columns.
- **Source:** `zellij/src/view.rs` (status_box_lines, render)
- **Verified by:** `zellij/src/view.rs` the_status_box_sits_on_the_status_line_and_a_click_toggles_it, problems_turn_the_dot_yellow_and_are_spelled_out_in_the_status_box, a_newer_release_is_marked_on_the_header_and_explained_in_help

#### ZR-004 Footer
- **Behavior:** The last line MUST be `[?] keys  [d]etails  [D] window` (two
  spaces between items; muted, dim) whenever the pane has more than two
  rows. With two rows or fewer there is no footer (ZR-016).
- **Source:** `zellij/src/view.rs` (render, FOOTER)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds, very_small_panes_keep_the_status_line

#### ZR-033 Layout: the list on top, the status line beneath it
- **Behavior:** From the top the panel MUST show: the body (the agent list,
  or the help view, or the empty state), cut or padded with empty lines to
  fill the rows the rest leaves; the status line (ZR-002); the inline
  details block when it is shown (ZR-008); and the footer (ZR-004). So the
  status line is directly above the details, and directly above the footer
  when details are hidden or do not fit. The pane's own title names the
  panel, so nothing is drawn above the list. The row the status line is
  drawn on is recorded for clicks (ZK-028).
- **Source:** `zellij/src/view.rs` (render)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (status between list and details), renders_keyboard_help_on_request (status above the footer)

### 3.2 Help, empty and error states

#### ZR-005 Help view
- **Behavior:** When `show_help` is true the body (ZR-033) MUST be, instead
  of the list, and with no details block: `In the list` (accent, bold); the 18 entries below
  (muted, dim); an empty line; `Anywhere` (accent, bold); then the three
  fixed `Anywhere` entries (muted, dim). Help is shown regardless of whether
  there are agents.

  ```text
  j / k       move selection
  gg / G      first / last
  Ctrl-d / u  half page
  Enter / l   open agent, or a section
  h           close a section or a team
  v           group by team / status
  p / s       pane / stacked pane here
  w           shell in its worktree
  d           toggle details
  D           details window
  n           rename agent
  t / T       join / leave a team
  x           stop agent (asks y/n)
  r           refresh
  /           search (also older:14d)
  u           check for a new release
  i           status and version
  ? / Esc     close help
  ```

  ```text
  Alt-m       jump to list and back
  Alt-o       hide / show this panel
  Alt-d       detach both sessions
  ```

  The `Anywhere` lines are fixed text naming the host layout's default
  keys; they do not follow a layout that rebinds them. Those keys are bound
  by the host session's configuration, not handled by the panel's key
  handler ([08-zellij-host.md](08-zellij-host.md)).

  Then always: an empty line, and `version     v<the plugin's version>`
  (muted, dim). When a newer release is recorded, two more lines follow:
  `↑ vX.Y.Z ` padded to at least 12 columns then `newer plugin release;`
  (attention colour); and, indented 12 columns, `set it in the plugin URL`
  (muted, dim).
- **Source:** `zellij/src/view.rs` (help_lines, version_help_lines, render)
- **Verified by:** `zellij/src/view.rs` renders_keyboard_help_on_request, a_newer_release_is_marked_on_the_header_and_explained_in_help

#### ZR-006 Empty list states
- **Behavior:** When `items` is empty (and help is off) the body (ZR-033)
  MUST be, by connection state:

  | Connection | Lines (style) |
  |---|---|
  | `ServerUnavailable` | `noc server is not running.` (attention, bold); empty; `Start it with:` (muted, dim); `  noc server --bg` (info); empty; `This panel will reconnect automatically.` (muted, dim) |
  | `Error(_)` | `No registry snapshot available.` (error) |
  | `Connecting` / `Connected` | `No live agents.` (muted, dim) |
  | `Connecting` / `Connected`, with a search | `No agents match.`; empty; `Words match names, folders,`; `branches and requests.`; `older:14d   idle two weeks or more`; `newer:3d    active in the last 3 days` (all muted, dim) |

  The raw socket error MUST NOT be shown in the body.
- **Source:** `zellij/src/view.rs` (empty_state_lines)
- **Verified by:** `zellij/src/view.rs` renders_a_friendly_server_start_instruction; `zellij/src/view.rs` empty_and_error_states

#### ZR-007 Errors keep the last good list
- **Behavior:** When `items` is non-empty and the connection is `Error` or
  `ServerUnavailable`, the list and details MUST render normally with only
  the status dot (ZR-003) reporting the problem.
- **Source:** `zellij/src/view.rs` (render)
- **Verified by:** `zellij/src/view.rs` errors_keep_the_last_good_list

### 3.3 List and details layout

#### ZR-008 Master/detail split
- **Behavior:** With `available = rows - 2` (the status line and the
  footer): `detail_height = available / 3` if
  `show_details && available >= 12`, else 0;
  `list_height = available - detail_height`. The list occupies the
  first `list_height` lines, padded with empty lines; then comes the status
  line; then the details block, exactly `detail_height` lines; then the
  footer. The split is fixed so neither the status line nor the details
  jump as the selection moves.
- **Source:** `zellij/src/view.rs` (render)
- **Verified by:** `zellij/src/view.rs` details_take_the_bottom_third_and_ready_agents_show_their_age

#### ZR-009 List scrolling keeps the selection centred
- **Behavior:** The list's rendered lines (headings, agent primary and
  secondary lines, section rows) MUST be windowed to `list_height` starting
  at `min(selected_line - list_height/2 (saturating), len - list_height
  (saturating))`, where `selected_line` is the first rendered line flagged
  selected (an agent's primary line or a section row; 0 if none).
- **Source:** `zellij/src/view.rs` (render, agent_list_lines)
- **Verified by:** `zellij/src/view.rs` the_selection_stays_inside_a_scrolled_list (the selected row is visible; exact offset `UNTESTED`)

#### ZR-010 Group headings
- **Behavior:** Before the first row of each group (in `items` order) the
  list MUST insert `{Group}  {count}` (two spaces), bold, in the group colour
  (ZR-017). Section rows belong to Quiet. Count for Blocked/Working/Review =
  number of that group's agent rows; for Quiet = sum of section counts (so it
  does not double-count members of an open section). Grouped by team these
  headings are not drawn; the team headings are rows (ZR-035).
- **Source:** `zellij/src/view.rs` (agent_list_lines, group_count, item_group)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections (`Quiet  3` with a section open)

### 3.4 Rows

#### ZR-011 Agent primary line
- **Behavior:** Each agent row's first line MUST be
  `" " + indent + symbol + " " + title`, where indent is two spaces for a
  Quiet agent (inside an open section, or under a team's Quiet row) and
  empty otherwise, and also empty for a team's lead listed first in its
  group (ZG-018) even when it is Quiet; symbol per ZR-012,
  and title = `Agent::title()` truncated to `cols - prefix_width` (ZR-020);
  the rest of the line is padded with spaces to `cols`. No selection glyph is
  drawn (the leading space keeps alignment; the highlight marks selection).
- **Source:** `zellij/src/view.rs` (agent_primary_line)
- **Verified by:** `zellij/src/view.rs` quiet_agents_rest_even_if_they_stopped_waiting_on_the_user (`   ◌ Pending`); `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds

#### ZR-012 Row status symbol
- **Behavior:** The row symbol MUST be:
  - Quiet group: `◌` if `display_state == "not_live"` or
    `live_state == "not_live"`, else `○` — whatever state it stopped in
    (a Quiet `needs_input` agent shows `◌`/`○`, not `?`);
  - otherwise, `display_state == "working"`: the spinner frame
    `SPINNER[spinner_frame % 10]` with frames
    `⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏`;
  - otherwise `state_symbol()` (ZM-012), e.g. `!` `?` `@` `×` `…` `◆`.
- **Source:** `zellij/src/view.rs` (agent_symbol), `zellij/src/app.rs` (SPINNER)
- **Decision:** D-048
- **Verified by:** `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections; `zellij/src/view.rs` quiet_agents_rest_even_if_they_stopped_waiting_on_the_user

#### ZR-013 Agent secondary line
- **Behavior:** Each agent row's second line MUST be
  `"   " + indent + icon + " " + age_right_aligned_to_3` then, if non-empty,
  `"  " + team` (ZR-036), then, if non-empty,
  `"  " + rest`, then for a Quiet agent whose `display_state` starts with
  `needs_` the suffix `"  stopped waiting on you"`; trailing whitespace is
  trimmed, then the line is truncated/padded to `cols`. `icon` is
  `provider_icon(config.nerd_icons)`; `age` is
  `age_label(recency(), generated_at)` (three spaces when unknown; `{:>3}`
  pads but does not truncate, so `100d` takes four columns). `rest` is
  `attention.detail.trim()` when the agent is Blocked and that is non-empty,
  else `workspace_label()` (ZM-017). `indent` is the primary line's
  (ZR-011).
- **Source:** `zellij/src/view.rs` (secondary_text, agent_secondary_line, row_indent)
- **Decision:** D-048
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (`   ✳  2m  Bash: make clean`); `zellij/src/view.rs` quiet_agents_rest_even_if_they_stopped_waiting_on_the_user

#### ZR-014 Section row
- **Behavior:** A section row MUST be
  `" " + arrow + " " + label padded to 6 + "  " + count`, arrow `▾` when
  expanded and `▸` when collapsed; e.g. `" ▸ < 24h   1"`, `" ▸ < 7d    1"`.
  The count follows the label (not right-aligned at the pane edge). A
  team's Quiet row (ZG-018) is drawn the same way with the label `Quiet`:
  `" ▸ Quiet   2"`.
- **Source:** `zellij/src/view.rs` (section_line)
- **Decision:** D-049
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections

#### ZR-015 Selection highlight and row colours
- **Behavior:** Styles MUST be:

  | Element | Unselected | Selected |
  |---|---|---|
  | primary prefix (marker, indent, symbol) | group colour, bold | `selected_text` on `selected_background`, bold |
  | primary title | `text` (Quiet: grey group colour), not bold | `selected_text` on `selected_background`, bold |
  | primary padding | no style | `selected_background` |
  | secondary line | `muted`, dim | `selected_text` on `selected_background`, not dim; padding on `selected_background` |
  | section row | grey group colour | `selected_text` on `selected_background`, bold; padding on `selected_background` |
  | group heading | group colour, bold (never selected) | — |
  | team heading: name and count | `accent`, bold | `selected_text` on `selected_background`, bold; padding on `selected_background` |
  | team heading: counts of a folded team | `muted`, dim | `selected_text` on `selected_background` |

  The selected agent's highlight MUST span both of its lines across the full
  width.
- **Source:** `zellij/src/view.rs` (agent_primary_line, agent_secondary_line, section_line, agent_list_lines)
- **Verified by:** `zellij/src/view.rs` selected_rows_highlight_both_lines_and_group_colours_are_fixed (background on both lines, heading colours); the remaining cells `UNTESTED`

#### ZR-035 Team heading
- **Behavior:** A `Team` row MUST be `{team}  {count}` (two spaces), with
  `no team` as the name of the group of agents in no team. When the team is
  folded and any of its agents is Blocked, Working or Review, two spaces and
  the counts follow: `{b} blocked · {w} working · {r} review`, the parts
  joined by ` · `, a part left out when its count is zero (for example
  `billing  4  1 blocked · 2 working`). An open team shows no counts. The
  line is cut to `cols` with `…` (ZR-020), never wrapped. Unlike a status
  heading (ZR-010) it is a row: it can be selected.
- **Source:** `zellij/src/view.rs` (team_line, attention_counts, agent_list_lines); `zellij/src/app.rs` (team_title, NO_TEAM)
- **Decision:** D-070, D-071
- **Verified by:** `zellij/src/view.rs` team_grouping_shows_headings_the_lead_and_folded_counts

#### ZR-036 The team on the secondary line
- **Behavior:** The `team` part of an agent's secondary line (ZR-013) MUST
  be:

  | List | Agent | Text |
  |---|---|---|
  | grouped by team | the team's lead | `★ lead` |
  | grouped by team | any other agent | empty |
  | grouped by status, or search results | lead of a team | `★ ` + the team name cut to 12 columns |
  | grouped by status, or search results | in a team, not its lead | the team name cut to 14 columns |
  | any | in no team | empty |

  Names are cut with `…` (ZR-020). The mark is `★` (U+2605), which no state uses (ZM-012).
- **Source:** `zellij/src/view.rs` (team_label, secondary_text)
- **Decision:** D-071
- **Verified by:** `zellij/src/view.rs` the_status_grouping_names_the_team_on_the_second_line, team_grouping_shows_headings_the_lead_and_folded_counts

### 3.5 Colour and text primitives

#### ZR-016 Very small panes
- **Behavior:** With `rows == 1` only the status line is output; with
  `rows == 2` one body line (for a non-empty list, the selected row's first
  line) above the status line, and no footer. From three rows the footer is
  shown.
- **Source:** `zellij/src/view.rs` (render)
- **Verified by:** `zellij/src/view.rs` very_small_panes_keep_the_status_line

#### ZR-017 Group colours
- **Behavior:** Each group has one colour, taken from the theme (ZR-018):
  `blocked`, `working`, `review` and `quiet`. An agent's state colour is its
  group's colour.
- **Source:** `zellij/src/view.rs` (Theme::group)
- **Verified by:** `zellij/src/view.rs` selected_rows_highlight_both_lines_and_group_colours_are_fixed

#### ZR-018 The theme
- **Behavior:** By default the theme MUST be the fixed colours of herdr's
  default theme (Catppuccin Mocha with a blue accent), and `ModeUpdate` MUST
  NOT change it. With the setting `colors "zellij"` the theme MUST instead be
  rebuilt from `mode_info.style.colors` on each `ModeUpdate` (the fixed
  colours apply until the first one), with the groups on entries of the
  terminal's 16-colour palette.

  | Role | Default (RGB) | With `colors "zellij"` |
  |---|---|---|
  | `text` | `205,214,244` | `text_unselected.base` |
  | `muted` | `147,153,178` | `list_unselected.base` |
  | `accent` | `137,180,250` (blue) | `text_unselected.emphasis_0` |
  | `selected_text` | `205,214,244` | `list_selected.base` |
  | `selected_background` | `49,50,68` | `list_selected.background` |
  | `attention` | `250,179,135` (peach) | `text_unselected.emphasis_0` (same as accent) |
  | `active` | `137,180,250` (blue) | `exit_code_success.base` |
  | `info` | `148,226,213` (teal) | `text_unselected.emphasis_1` |
  | `error` | `243,139,168` (red) | `exit_code_error.base` |
  | `blocked` | `243,139,168` (red) | palette `1` |
  | `working` | `249,226,175` (yellow) | palette `2` |
  | `review` | `148,226,213` (teal) | palette `6` |
  | `quiet` | `147,153,178` (grey) | palette `8` |

  The built-in theme MUST NOT use the terminal's dim attribute anywhere: its
  greys are already the intended shade, and dimmed they cannot be read on a
  dark background. Wherever this file calls text `dim`, that applies only
  with `colors "zellij"`.

  The details window uses the same theme; the panel passes `colors "zellij"`
  to it when that is set.
- **Source:** `zellij/src/view.rs` (Theme::herdr, Theme::from_mode_info); `zellij/src/app.rs` (Config::from_map); callers `zellij/src/main.rs` (on_mode_update), `zellij/src/details.rs` (update_details), `zellij/src/panel.rs` (open_details_window)
- **Decision:** D-063
- **Verified by:** `zellij/src/view.rs` selected_rows_highlight_both_lines_and_group_colours_are_fixed (both themes' group and selection colours; no dim attribute with the built-in theme); `zellij/src/app.rs` parses_bounded_poll_configuration (the setting); the `ModeUpdate` switch UNTESTED

#### ZR-019 SGR encoding
- **Behavior:** A styled span MUST be `ESC[` + `;`-joined codes in the order
  bold `1`, dim `2`, foreground, background + `m` + text + `ESC[0m`;
  foreground/background are `38;5;N`/`48;5;N` for `EightBit(N)` and
  `38;2;R;G;B`/`48;2;R;G;B` for `Rgb`. A span with no codes MUST be emitted
  as plain text. The agent primary line appends one extra `ESC[0m`.
- **Source:** `zellij/src/view.rs` (styled_span, color_code, Line::render)
- **Verified by:** `zellij/src/view.rs` selected_rows_highlight_both_lines_and_group_colours_are_fixed

#### ZR-020 Truncation by display width
- **Behavior:** `truncate(s, w)` MUST return `""` for `w == 0`; `s` if its
  display width (unicode-width) is `<= w`; `…` for `w == 1`; otherwise the
  longest prefix of whole characters with width `<= w - 1` followed by `…`
  (zero-width characters count 0). Wide characters are never split. A whole
  line wider than `cols` is cut the same way: characters are kept up to
  `cols - 1` columns and `…` is appended in the style of the span where the
  cut fell.
- **Source:** `zellij/src/view.rs` (truncate, Line::fitted)
- **Verified by:** `zellij/src/view.rs` truncates_unicode_by_terminal_width; `zellij/src/view.rs` renders_a_friendly_server_start_instruction (a whole line cut at 38 columns)

#### ZR-021 Padding and wrapping
- **Behavior:** `pad(s, w)` MUST right-pad with spaces to display width `w`
  (never truncating). `wrap(s, w)` MUST split on whitespace, greedily fill
  lines up to width `w`, put any word wider than `w` on its own line
  truncated per ZR-020, and return `[]` for `w == 0` or empty input.
- **Source:** `zellij/src/view.rs` (pad, wrap)
- **Verified by:** `zellij/src/view.rs` truncates_unicode_by_terminal_width

### 3.6 Details

#### ZR-022 Inline details block (agent selected)
- **Behavior:** When `detail_height > 0` and an agent row is selected, the
  block MUST be: a rule of `─` × `cols` (accent, dim); `Details` (accent,
  bold); then the agent detail lines (ZR-024) with preview lines per field =
  2 if `detail_height >= 16` else 1, and short identifiers (non-roomy);
  truncated to `detail_height` lines. With nothing selected the block is the
  rule alone.
- **Source:** `zellij/src/view.rs` (inline_detail_lines)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` details_take_the_bottom_third_and_ready_agents_show_their_age

#### ZR-023 Inline details block (section selected)
- **Behavior:** When a section row is selected the block MUST be: the `─`
  rule; `Quiet · {label}` (accent, bold); then, muted and dim,
  `Conversations with no process.`, `Enter opens this section; Enter on`,
  `an agent resumes it; h closes.` — the same text whether the section is
  open or closed. When a team heading is selected the block MUST be: the
  rule; `Team · {team}` (`No team` for the agents in no team; accent,
  bold); then, muted and dim, `{count} agent{s}.`,
  `Enter folds or opens this team;`, `v groups by status.`. When a team's
  Quiet row is selected: the rule; `Quiet · {team}` (`Quiet · no team`);
  then `Conversations with no process.`, `Enter opens this row; Enter on`,
  `an agent resumes it; h closes.`.
- **Source:** `zellij/src/view.rs` (inline_detail_lines)
- **Verified by:** `zellij/src/view.rs` working_agents_spin_and_quiet_agents_fold_into_counted_sections; `zellij/src/view.rs` team_grouping_shows_headings_the_lead_and_folded_counts (the team heading); the Quiet row's block `UNTESTED`

#### ZR-024 Agent detail lines
- **Behavior:** The shared agent detail lines MUST be, in order, omitting any
  line whose condition fails. All labels are left-aligned in a 10-column
  field (label + spaces); metadata lines are muted and dim.

  | # | Line | Condition / style |
  |---|---|---|
  | 1 | `{title}` | always; `text`, bold |
  | 2 | `{state_symbol}  {state_label} · {age} ago`, or `{state_symbol}  {state_label}` when age unknown | always; group colour. Uses `state_symbol()` (ZM-012), not the row's Quiet/spinner symbol, so a Quiet `needs_input` agent reads `?  needs input · 5h ago` and a working one `●  working` |
  | 3 | `provider  {provider_detail_label}` | always |
  | 4 | `native    {name}` | `user_name` and `name` both non-empty (untrimmed) and different; `name` printed as is |
  | 5 | ids | ZR-025 |
  | 6 | `model     {model} · {effort}` / `model     {model}` / `effort    {effort}` | ZR-026 |
  | 7 | `repo      {repo} · {branch}` / `{repo}` / `{branch}` | workspace present; ZR-027 |
  | 8 | `worktree  {worktree_name}` | workspace is a linked worktree with a name (ZM-016) |
  | 9 | `cwd       {workspace.path}` (trimmed) | `path.trim()` non-empty |
  | 10 | `ask` + up to P wrapped lines | ZR-028 |
  | 11 | `latest` + up to P wrapped lines | ZR-028 |
  | 12 | `pane      {session}/{pane_id} · observed`, or `pane      {pane_id} · observed` when session empty | `zellij_binding()` (ZM-018) exists; always says `observed` regardless of binding state |

  Fields never shown: `host_id`, `activity_state`, `runtime_state` (except via
  line 3), `attention.type`, `summary_source`, `last_seen_at`,
  `TextPreview.source`/`truncated`.
- **Source:** `zellij/src/view.rs` (agent_detail_lines)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (provider, native, repo, cwd, ask; no `runtime`); `zellij/src/view.rs` details_take_the_bottom_third_and_ready_agents_show_their_age (state line); `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel (latest, pane); `worktree` line `UNTESTED`

#### ZR-025 Identifier lines
- **Behavior:** Inline (non-roomy): one line `id        {short_id}`, or
  `id        {session_id}` when the short id is empty; none if both empty.
  Details window (roomy): `id        {short_id}` if non-empty, and
  `session   {session_id}` if non-empty and different from the short id.
  Both values are trimmed.
- **Source:** `zellij/src/view.rs` (id_lines)
- **Verified by:** `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel (roomy); `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (inline short id); session-id fallback `UNTESTED`

#### ZR-026 Model line
- **Behavior:** With both `model` and `reasoning_effort` non-blank:
  `model     {model} · {effort}`; only model: `model     {model}`; only effort:
  `effort    {effort}`; neither: no line.
- **Source:** `zellij/src/view.rs` (agent_detail_lines)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds (both set); the other three cases `UNTESTED`

#### ZR-027 Repository line requires a git workspace
- **Behavior:** The repo part MUST be `repo_name()` only when
  `workspace.is_git()` (ZM-016), else empty; the line is
  `repo      {repo} · {branch}`, `repo      {repo}`,
  `repo      {branch}` (non-git dir with a branch), or omitted when both are
  empty. (Unlike the list's `workspace_label`, a plain directory shows no
  repo.)
- **Source:** `zellij/src/view.rs` (agent_detail_lines)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel

#### ZR-028 Request and reply previews
- **Behavior:** `ask` MUST be shown when `initial_request.text.trim()` is
  non-empty; `latest` when `summary.trim()` is non-empty and differs from the
  trimmed request. Each is a label line (info, bold) followed by at most P
  lines of `"  " + line` (muted, dim) from `wrap(value, cols - 2)`. Excess
  lines are dropped silently (no ellipsis line).
- **Source:** `zellij/src/view.rs` (agent_detail_lines, append_preview)
- **Verified by:** `zellij/src/view.rs` renders_colored_grouped_master_detail_panel_within_bounds; `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel

### 3.7 Floating details window (`details.rs`, `view.rs`)

#### ZR-029 Details window rendering
- **Behavior:** `render_details_window` MUST return nothing for zero size;
  with an agent loaded, the roomy agent detail lines (ZR-024, ZR-025) with
  P = `clamp(rows / 5, 2, 8)` and no rule or `Details` heading; then pad to
  `rows - 1` and add `Esc / D  close` (muted, dim) if there is room; truncate
  to `rows`.
- **Source:** `zellij/src/view.rs` (render_details_window, DETAILS_FOOTER)
- **Verified by:** `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel

#### ZR-030 Details window placeholder states
- **Behavior:** With no agent loaded the first line MUST be `×  {error}`
  (error colour) for `Error`, `!  noc server is not running` (attention)
  for `ServerUnavailable`, else `◌  loading` (info). Once an agent has been
  loaded, later failures change only the (unrendered) connection state, so
  the window keeps showing the last good agent with no error indication.
- **Source:** `zellij/src/view.rs` (render_details_window)
- **Verified by:** `zellij/src/view.rs` details_window_placeholders_follow_the_connection

#### ZR-031 Details window data and title
- **Behavior:** A plugin instance loaded with `role "details"` MUST read
  `agent_id` (trimmed) from its configuration, and after permission is
  granted and on every timer, if no command is in flight and the id is
  non-empty, run `[noc_bin, "show", "--json", agent_id]`. After each result
  it MUST re-arm a 2.0 s timer. On exit 0 it parses one agent (ZM-003); on
  success it shows that single agent, sets `generated_at` to the wall clock
  (ZM-022), sets connection `Connected(poll)`, and renames its own pane to
  `noc · {title}` whenever the title changes. Non-UTF-8 output →
  `Error("noc returned non-UTF-8 output: {e}")`; parse failure → `Error(e)`;
  non-zero exit → `ServerUnavailable` per ZG-015's detection, else
  `Error(<compacted stderr>)` (empty string when stderr is empty).
  Permission denied →
  `Error("permissions denied: agent details are unavailable")`.
  The instance rebuilds its theme on `ModeUpdate` (ZR-018) and ignores pipes
  and every other event.
- **Source:** `zellij/src/details.rs` (update_details, refresh_details, on_details_result), `zellij/src/main.rs` (load, pipe)
- **Verified by:** `E2E: zellij/e2e/nested.sh` ("D opens the floating details window": a floating plugin pane titled with the agent's name); result mapping `UNTESTED`

#### ZR-032 Details window keys
- **Behavior:** In the details window, `Esc`, `D`, `d` or `q` (any
  modifiers) MUST close the window (`close_self`); every other key is
  ignored. Keys never request a redraw.
- **Source:** `zellij/src/details.rs` (is_close_key, update_details)
- **Verified by:** `zellij/src/details.rs` the_window_closes_on_esc_d_capital_d_and_q_only; `E2E: zellij/e2e/nested.sh` ("q closes it")

---

## 4. Panel keys and intents (`panel.rs`, `app.rs`)

Key events reach the key handler only in the `panel` role and only while the
panel pane has the keyboard. `process_key` is a pure function over the panel
state that returns whether to redraw and, optionally, an intent; `handle_key`
runs the intent. Every agent is opened in the inner session
(`guest_session`), never in the host session; the commands used, how the
inner session is read, and how the keyboard returns to the guest pane
afterwards are specified in [08-zellij-host.md](08-zellij-host.md).

### 4.1 Key binding summary

| Key | Precondition | Effect |
|---|---|---|
| any key | rename in progress | rename editing (ZK-020..ZK-023) |
| any key | team prompt open | team prompt editing (ZK-030..ZK-032) |
| `Ctrl-d` | — | down half a page (ZK-006) |
| `Ctrl-u` | — | up half a page (ZK-006) |
| other `Ctrl-…`, any `Alt`/`Super`/multi-modifier key | — | ignored, no redraw |
| `y` | stop pending | run `noc stop` (ZK-018) |
| any other unmodified key | stop pending | cancel stop, notice `stop cancelled` |
| `j`, `Down` | — | next row |
| `k`, `Up` | — | previous row |
| `g` `g` | — | first row |
| `G` | — | last row |
| `d` | — | toggle inline details; closes help |
| `?` | — | toggle help |
| `Esc` | — | close help; cancel pending `g` |
| `Enter`, `l` | section row | open/close section |
| `Enter`, `l`, `Space` | team heading, or a team's Quiet row | fold/open it (ZK-008) |
| `Enter`, `l` | agent row | open or focus the agent in the inner session (ZK-009) |
| `Space` | section row | open/close section |
| `Space` | agent row | nothing |
| `h`, `Left` | section row or agent in a section | close that section, select its row |
| `h`, `Left` | grouped by team | close the Quiet row or fold the team (ZK-015) |
| `v` | — | group by team, or by status again (ZK-029) |
| `p` | agent row | open the agent as a new pane in the inner session (ZK-010) |
| `s` | agent row | open the agent stacked in the inner session (ZK-011) |
| `w` | agent row | shell in its workspace, in the inner session (ZK-012) |
| `D` | agent row | floating details window (ZK-013) |
| `r` | poll transport | refresh now (ZK-014) |
| `x` | stoppable agent row | ask `Stop {title}?  y / n` (ZK-016) |
| `n` | agent row | start inline rename (ZK-020) |
| `t` | agent row | open the team prompt (ZK-030) |
| `T` | agent row in a team | take the agent out of its team (ZK-033) |
| `q`, `Right`, `Tab`, anything else | — | no effect except cancelling a pending `g`; redraw |

### 4.2 Dispatch rules

#### ZK-001 Rename mode captures every key
- **Behavior:** While a rename is pending, every key event (any modifiers)
  MUST go to the rename editor (ZK-021) and request a redraw; no navigation,
  confirmation or other binding applies.
- **Source:** `zellij/src/panel.rs` (process_key, handle_rename_key)
- **Verified by:** `zellij/src/panel.rs` rename_mode_captures_every_key

#### ZK-002 Modifier filtering
- **Behavior:** A key with `Ctrl` among its modifiers MUST be handled only as
  `Ctrl-d`/`Ctrl-u` (requesting a redraw), otherwise ignored with no redraw —
  this check precedes the stop confirmation, so `Ctrl-d`/`Ctrl-u` move the
  selection while a stop is pending and leave it pending. Any other key whose
  modifiers are not empty and not exactly `{Shift}` MUST be ignored with no
  redraw (also leaving a pending stop untouched). All remaining keys request
  a redraw.
- **Source:** `zellij/src/panel.rs` (process_key)
- **Verified by:** `zellij/src/panel.rs` modifier_rules_gate_every_binding; `zellij/src/panel.rs` modifier_checks_leave_a_pending_stop_alone

#### ZK-003 Unbound keys
- **Behavior:** An unmodified key with no binding (including `q`,
  `Right`, `Tab`, `Backspace`, digits) MUST only cancel a pending `g` and
  request a redraw.
- **Source:** `zellij/src/panel.rs` (process_key)
- **Verified by:** `zellij/src/panel.rs` navigation_and_intent_keys

### 4.3 Navigation

#### ZK-004 Move by one row
- **Behavior:** `j`/`Down` and `k`/`Up` MUST move `selected_index` by +1/−1
  over `items` (section rows and agent rows alike), clamped to
  `0..=items.len()-1` (0 when empty), clear any notice and cancel a pending
  `g`.
- **Source:** `zellij/src/panel.rs` (process_key), `zellij/src/app.rs` (move_selection)
- **Verified by:** `zellij/src/app.rs` navigation_clamps_and_supports_gg_and_capital_g; `zellij/src/panel.rs` navigation_and_intent_keys

#### ZK-005 `gg` and `G`
- **Behavior:** The first `g` MUST set a pending-`g` flag; a second `g` while
  pending MUST select row 0, clear the flag and clear any notice. Every other
  bound key, `Esc`, an unbound key, `select_agent`, and section toggles clear
  the flag. `G` MUST select the last row (0 when empty), clear the flag and
  the notice.
- **Source:** `zellij/src/panel.rs` (process_key), `zellij/src/app.rs` (press_g, select_last)
- **Verified by:** `zellij/src/app.rs` navigation_clamps_and_supports_gg_and_capital_g; `zellij/src/panel.rs` navigation_and_intent_keys

#### ZK-006 Half-page movement
- **Behavior:** `Ctrl-d`/`Ctrl-u` MUST move by
  `±max(last_render_rows / 2, 1)` rows of `items`, via ZK-004. The page size
  is half the whole panel height, not the list height, and each agent row
  is two screen lines.
- **Source:** `zellij/src/panel.rs` (process_key), `zellij/src/app.rs` (move_half_page)
- **Verified by:** `zellij/src/app.rs` navigation_clamps_and_supports_gg_and_capital_g; `zellij/src/panel.rs` modifier_rules_gate_every_binding

#### ZK-007 Details and help toggles
- **Behavior:** `d` MUST toggle `show_details` and set `show_help = false`.
  `?` MUST toggle `show_help`. `Esc` MUST set `show_help = false`. While help
  is shown, navigation and action keys still act on the (hidden) list.
- **Source:** `zellij/src/app.rs` (toggle_details, toggle_help, close_help)
- **Verified by:** `zellij/src/panel.rs` navigation_and_intent_keys; `zellij/src/view.rs` renders_keyboard_help_on_request (render)

### 4.4 Sections and intents

#### ZK-008 Enter / l / Space
- **Behavior:** `Enter`, `l` and `Space` MUST cancel a pending `g`; if a
  section row is selected they toggle that section's expanded state, rebuild
  the rows and keep the selection on the section row. A team heading and a
  team's Quiet row (ZG-018) are toggled the same way: the heading folds or
  opens the team, the Quiet row opens or closes. If an agent row is
  selected (or nothing), `Enter` and `l` raise the open intent (ZK-009);
  `Space` does nothing.
- **Source:** `zellij/src/panel.rs` (process_key), `zellij/src/app.rs` (toggle_selected_section)
- **Decision:** D-049
- **Verified by:** `zellij/src/panel.rs` navigation_and_intent_keys; `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections; `zellij/src/panel.rs` v_switches_the_grouping_and_enter_folds_a_team; `zellij/src/app.rs` team_groups_fold_and_keep_their_state_across_snapshots; `E2E: zellij/e2e/nested.sh` (Enter opens the Quiet section before opening its agent)

#### ZK-009 Open intent (Enter / l on an agent)
- **Behavior:** The panel MUST, in order:
  1. if permissions were not granted, notice
     `pane attachment permission was not granted`;
  2. if no agent row is selected, notice `no agent is selected`;
  3. if the last reading of the inner session shows a live pane for the
     agent, focus that pane there and stop;
  4. if `native_session_id` is blank, notice
     `this agent has no resumable native session ID`;
  5. if a navigable binding (ZM-018) names a running Zellij session that is
     neither the host session nor the inner session, notice
     `already open in Zellij session "{name}"; press p to open a second copy here`
     (name Debug-quoted) and stop;
  6. otherwise open a new tab in the inner session, named after the agent
     (its title, cut to 32 characters plus `…` when longer), running
     `noc_bin attach <id>` with cwd = trimmed `workspace.path` when non-empty.

  Enter on a Quiet agent resumes it through the same `noc attach`. If the
  inner-session command fails, the notice is
  `the inner Zellij session did not open it: {compacted stderr}`. How the
  agent's pane is found and how the commands run is in
  [08-zellij-host.md](08-zellij-host.md).
- **Source:** `zellij/src/panel.rs` (open_selected_agent, selected_agent_or_notice, other_running_session_for), `zellij/src/guest.rs` (guest_open_agent, guest_agent_pane, new_tab_args, on_guest_open), `zellij/src/panes.rs` (agent_tab_name, other_running_session)
- **Decision:** D-034, D-038, D-049
- **Verified by:** `E2E: zellij/e2e/nested.sh` (Enter opens the agent in a new inner tab named after it, running `noc attach`; Enter again focuses it instead of opening a copy); `zellij/src/guest.rs` builds_inner_session_commands; `zellij/src/guest.rs` finds_an_agents_inner_pane_in_order; `zellij/src/panes.rs` names_another_running_session_only_when_it_is_live_and_not_ours; `zellij/src/panes.rs` tab_names_are_the_agent_title_cut_to_a_limit; the notices `UNTESTED`

#### ZK-010 `p`: open as a pane
- **Behavior:** `p` MUST cancel a pending `g`, then require permission
  (`pane attachment permission was not granted`), a selected agent
  (`no agent is selected`) and a non-blank `native_session_id`
  (`this agent has no resumable native session ID`). It MUST NOT look for an
  existing pane (always a new copy) and MUST NOT check for another session.
  It opens `noc_bin attach <id>` as a new pane in the inner session, titled
  `NOC: {title}`, with cwd = trimmed `workspace.path` when non-empty, and
  remembers the pane it opened for that agent. Failure →
  `the inner Zellij session did not open it: {compacted stderr}`.
- **Source:** `zellij/src/panel.rs` (open_agent_pane), `zellij/src/guest.rs` (guest_open_agent_in_pane, run_pane_args, on_guest_open)
- **Verified by:** `zellij/src/guest.rs` builds_inner_session_commands; `zellij/src/guest.rs` reads_the_opened_pane_id; the key path end to end `UNTESTED`

#### ZK-011 `s`: open stacked
- **Behavior:** As ZK-010, with the new pane stacked onto the inner
  session's focused pane (`--stacked`).
- **Source:** `zellij/src/panel.rs` (open_agent_pane), `zellij/src/guest.rs` (run_pane_args)
- **Verified by:** `zellij/src/guest.rs` builds_inner_session_commands; the key path end to end `UNTESTED`

#### ZK-012 `w`: shell in the agent's workspace
- **Behavior:** `w` MUST cancel a pending `g`; require a selected agent
  (`no agent is selected`) and a non-empty `workspace.changes_directory()`
  (ZM-016) (`this agent has no known workspace`); then open a new shell pane
  at that directory in the inner session. There is no explicit permission
  check. Failure →
  `the inner Zellij session did not open it: {compacted stderr}`.
- **Source:** `zellij/src/panel.rs` (open_worktree_shell), `zellij/src/guest.rs` (guest_open_shell, shell_args)
- **Verified by:** `zellij/src/guest.rs` builds_inner_session_commands; the key path and notices `UNTESTED`

#### ZK-013 `D`: floating details window
- **Behavior:** `D` MUST cancel a pending `g`; require a selected agent
  (`no agent is selected`) and a known plugin URL
  (`the plugin location is not known yet; try again`); close the details
  window this panel opened before, if any; then open this plugin floating in
  the host session at x=`10%`, y=`10%`, width=`80%`, height=`80%` with
  configuration `role=details`, `agent_id=<id>`, `noc_bin=<noc_bin>`, plus
  `icons=nerd` when Nerd icons are on. Failure →
  `Zellij could not open the details window`. There is no explicit
  permission check.
- **Source:** `zellij/src/panel.rs` (open_details_window)
- **Verified by:** `E2E: zellij/e2e/nested.sh` ("D opens the floating details window"); notices `UNTESTED`

#### ZK-014 `r`: refresh
- **Behavior:** `r` MUST cancel a pending `g` and run
  `[noc_bin, "ls", "--json", "--all"]` immediately, unless the transport is
  `pipe`, permissions are not granted, or a registry command is already in
  flight — in which case it silently does nothing (no notice). A refresh
  marks the command in flight and schedules the next poll one poll interval
  later.
- **Source:** `zellij/src/panel.rs` (process_key, refresh)
- **Decision:** D-034
- **Verified by:** `zellij/src/panel.rs` navigation_and_intent_keys (the intent); the guards `UNTESTED`

#### ZK-015 `h` / `Left`: close a section
- **Behavior:** `h`/`Left` MUST cancel a pending `g`; if a section row is
  selected, collapse it and keep it selected; if a Quiet agent inside an open
  section is selected, collapse its section and select the section row;
  otherwise do nothing. Grouped by team: on a team's Quiet row, or on a
  Quiet agent that is not the lead, close that Quiet row and select it; on
  a team heading, or on any other agent, fold the agent's team and select
  its heading.
- **Source:** `zellij/src/app.rs` (collapse_selected, collapse_selected_team)
- **Decision:** D-049
- **Verified by:** `zellij/src/app.rs` quiet_agents_fold_into_collapsed_time_sections; `zellij/src/panel.rs` navigation_and_intent_keys; `zellij/src/app.rs` team_groups_fold_and_keep_their_state_across_snapshots; `zellij/src/panel.rs` v_switches_the_grouping_and_enter_folds_a_team

#### ZK-029 `v`: switch the grouping
- **Behavior:** `v` MUST cancel a pending `g`, switch the list between the
  status and the team grouping (ZG-017), and set the notice
  `grouped by team` or `grouped by status`. It raises no intent and runs no
  command. Every other key acts on an agent row exactly as in the status
  grouping; on a team heading or a team's Quiet row the keys that need an
  agent behave as on a section row (`no agent is selected`).
- **Source:** `zellij/src/panel.rs` (process_key); `zellij/src/app.rs` (toggle_group_by)
- **Decision:** D-070
- **Verified by:** `zellij/src/panel.rs` v_switches_the_grouping_and_enter_folds_a_team; `zellij/src/app.rs` switching_the_grouping_keeps_the_selected_agent

### 4.5 Stop (`x`)

#### ZK-016 Stop request and refusal policy
- **Behavior:** `x` MUST cancel a pending `g`, then: no agent row selected →
  notice `no agent is selected`; Quiet agent → `not running; nothing to stop`;
  otherwise by provider/source:

  | `provider` | `source_kind` | Result |
  |---|---|---|
  | `claude` | `background`, `native_short_id` non-blank | confirmation (ZK-017) |
  | `claude` | `background`, blank short id | notice `no background id to stop` |
  | `claude` | anything else | notice `stop it with /exit in its terminal` |
  | `codex` | any | notice `can't stop Codex agents yet` |
  | other | any | notice `can't stop this agent` |

- **Source:** `zellij/src/panel.rs` (request_stop)
- **Decision:** D-043
- **Verified by:** `zellij/src/panel.rs` stop_policy_and_confirmation

#### ZK-017 Stop confirmation
- **Behavior:** A stoppable request MUST record `PendingStop { agent_id,
  title }` and the status line MUST read `Stop {title}?  y / n`. The next
  unmodified key: `y` → ZK-018; anything else (including `n`, `Esc`, `Y`) →
  clear the pending stop and notice `stop cancelled`; the key is consumed and
  has no other effect. Snapshots, focus changes and `Ctrl-d`/`Ctrl-u` do not
  clear it; the stop always targets the recorded id even if the selection
  moved.
- **Source:** `zellij/src/panel.rs` (request_stop, process_key)
- **Decision:** D-043
- **Verified by:** `zellij/src/panel.rs` stop_policy_and_confirmation; `zellij/src/panel.rs` modifier_checks_leave_a_pending_stop_alone; `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel (the prompt)

#### ZK-018 Stop command and result
- **Behavior:** On `y` the panel MUST clear the pending stop, notice
  `stopping {title}…` and run `[noc_bin, "stop", agent_id]` with context
  `zellij-noc=stop-agent`, `zellij-noc-agent=<id>`,
  `zellij-noc-title=<title>`. On exit 0 it MUST mark the agent stopped
  (ZK-019) and notice `stopped {title}`; otherwise notice the compacted stderr
  (ZG-015) or, if empty, `could not stop {title}`.
- **Source:** `zellij/src/panel.rs` (process_key, run_intent, on_stop_result), `zellij/src/main.rs` (on_command_result)
- **Decision:** D-043
- **Verified by:** `zellij/src/panel.rs` modifier_checks_leave_a_pending_stop_alone (the intent and the `stopping` notice); the command and its result handling `UNTESTED`

#### ZK-019 Stopped agent leaves the list at once
- **Behavior:** `mark_stopped(id)` MUST add the id to the stopped set, remove
  the agent from `agents`, and rebuild rows preserving the selection by
  identity, falling back per ZG-009. Hiding then persists per ZG-012.
- **Source:** `zellij/src/app.rs` (mark_stopped)
- **Decision:** D-043
- **Verified by:** `zellij/src/app.rs` stopped_agents_stay_hidden_until_the_registry_drops_them

### 4.6 Rename (`n`)

#### ZK-020 Begin rename
- **Behavior:** `n` MUST cancel a pending `g` and, if an agent row is
  selected, start `PendingRename { agent_id, text: agent.title() }` (so an
  unnamed agent is pre-filled with its short id); otherwise notice
  `no agent is selected`.
- **Source:** `zellij/src/panel.rs` (process_key), `zellij/src/app.rs` (begin_rename)
- **Verified by:** `zellij/src/panel.rs` rename_mode_captures_every_key

#### ZK-021 Rename editing
- **Behavior:** While renaming:

  | Key | Effect |
  |---|---|
  | `Esc` (any modifiers) | cancel (ZK-023) |
  | `Backspace` | remove the last Unicode scalar (no-op when empty) |
  | `Ctrl-u` | clear the text |
  | any `Char(c)` without `Ctrl` (incl. Shift, Alt, Space) | append `c` |
  | `Enter` (any modifiers) | submit (ZK-022) |
  | `Ctrl-<other char>`, arrows, `Tab`, `Delete`, others | ignored |

  Editing is append-only at the end: there is no cursor movement. The prompt
  renders as `Rename: {text}▏`; a long text is truncated from the end, which
  hides the cursor and the characters being typed.
- **Source:** `zellij/src/panel.rs` (handle_rename_key), `zellij/src/view.rs` (connection_line)
- **Verified by:** `zellij/src/panel.rs` rename_mode_captures_every_key

#### ZK-022 Rename submit
- **Behavior:** `Enter` MUST end the rename and trim the text (internal
  whitespace kept). Empty → notice `a name cannot be empty` (no command).
  Otherwise run `[noc_bin, "rename", agent_id, name]` with context
  `zellij-noc=rename-agent`, `zellij-noc-title=<name>`. On exit 0:
  notice `renamed to {name}` and trigger an immediate refresh (ZK-014 rules)
  so the new name reaches the rows and details. On failure: notice the
  compacted stderr, or `could not rename` if empty. The panel never edits
  `agents` locally on rename: the new name arrives only with a snapshot. In
  pipe transport the refresh is a no-op, so the name updates with the next
  pipe frame.
- **Source:** `zellij/src/panel.rs` (handle_rename_key, run_intent, on_rename_result), `zellij/src/main.rs` (on_command_result)
- **Decision:** D-042
- **Verified by:** `zellij/src/panel.rs` rename_mode_captures_every_key (trim, empty name, the intent); the command and its result handling `UNTESTED`

#### ZK-023 Rename cancel
- **Behavior:** `Esc` MUST discard the pending rename and notice
  `rename cancelled`.
- **Source:** `zellij/src/panel.rs` (handle_rename_key)
- **Verified by:** `zellij/src/panel.rs` rename_mode_captures_every_key

### 4.7 Teams (`t`, `T`)

#### ZK-030 `t`: the team prompt
- **Behavior:** `t` MUST, with an agent row selected, open the team prompt
  for that agent, holding the agent's team (ZM-025), or nothing when it is
  in no team. With no agent selected (a section row, a team heading, a
  team's Quiet row, or an empty list) the notice is `no agent is selected`.
  The key works the same in both groupings and in search results. While
  the prompt is open every key event (any modifiers) MUST go to the prompt
  (ZK-031) and request a redraw, as in rename mode (ZK-001); a rename in
  progress takes precedence, and the prompt takes precedence over a search
  being typed. The status line shows `team: {text}▏` (ZR-002).
- **Source:** `zellij/src/panel.rs` (process_key); `zellij/src/app.rs` (begin_team, PendingTeam)
- **Decision:** D-071
- **Verified by:** `zellij/src/panel.rs` the_team_prompt_captures_every_key_and_takes_only_name_characters, capital_t_leaves_the_team_and_both_keys_need_an_agent; `zellij/src/view.rs` details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel (the status line)

#### ZK-031 Team prompt editing and completion
- **Behavior:** In the team prompt:

  | Key | Effect |
  |---|---|
  | `Esc` (any modifiers) | close the prompt, notice `team cancelled` |
  | `Enter` (any modifiers) | submit (ZK-032) |
  | `Tab` | complete (below) |
  | `Backspace` | delete the last character |
  | `Ctrl-u` | clear the text |
  | a character, without `Ctrl` or `Alt` | appended when a team name may hold it there: `A-Z`, `a-z`, `0-9`, `.`, `_`, `-`, the first character neither `-` nor `.`, at most `48` characters; any other character is ignored |
  | anything else | ignored |

  `Tab` MUST replace the text with the first team, in name order, that
  starts with the text (every team when the text is empty). The teams are
  those of the agents in the last snapshot (ZG-003), so in search results
  only the matches' teams. Pressed again with nothing typed in between, it
  moves to the next such team and after the last back to the first. When
  no team starts with the text, it does nothing. Any edit ends the walk:
  the next `Tab` completes the text as it then stands. Matching is exact
  about case.
- **Source:** `zellij/src/panel.rs` (handle_team_key); `zellij/src/app.rs` (complete_team, team_names, is_team_char)
- **Decision:** D-071
- **Verified by:** `zellij/src/panel.rs` the_team_prompt_captures_every_key_and_takes_only_name_characters, tab_in_the_team_prompt_completes_to_an_existing_team; `zellij/src/app.rs` tab_completes_and_then_cycles_through_the_teams, team_names_follow_the_rules_noc_applies

#### ZK-032 Team prompt submit
- **Behavior:** On `Enter`: empty text closes the prompt with the notice
  `team cancelled` and runs nothing. Text that is not a team name (`none`,
  or a name the prompt started with that breaks the rules of ZK-031) sets
  the notice `not a team name` and leaves the prompt open. Otherwise the
  prompt closes and the panel MUST run `<noc_bin> team set <team> <id>`
  with context `zellij-noc=join-team`, `zellij-noc-agent=<id>`,
  `zellij-noc-title=<team>`; a team that does not exist yet is made by
  that command. On exit 0 the notice is `joined <team>`; otherwise
  `could not join the team: <compacted stderr>`, or `could not join the
  team` when stderr is empty. Either way a registry poll starts at once
  (ZK-014 conditions). The selection follows the agent by id (ZG-008), so
  it stays on the agent when the list regroups.
- **Source:** `zellij/src/panel.rs` (handle_team_key, run_intent, team_set_argv, team_result_notice, on_team_result)
- **Decision:** D-071
- **Verified by:** `zellij/src/panel.rs` enter_in_the_team_prompt_joins_a_valid_team_only, team_results_are_said_in_plain_words; `zellij/src/app.rs` a_joined_agent_stays_selected_when_the_list_regroups; running the command and the poll `UNTESTED`

#### ZK-033 `T`: leave the team
- **Behavior:** `T` MUST, with no agent selected, set the notice
  `no agent is selected`; with an agent that is in no team, the notice
  `not in a team`. Otherwise it MUST run `<noc_bin> team clear <id>`, with
  no confirmation, with context `zellij-noc=leave-team`,
  `zellij-noc-agent=<id>`, `zellij-noc-title=<team>`. On exit 0 the notice
  is `left <team>`; otherwise `could not leave the team: <compacted
  stderr>`, or `could not leave the team` when stderr is empty. Either way
  a registry poll starts at once.
- **Source:** `zellij/src/panel.rs` (request_leave_team, run_intent, team_clear_argv, team_result_notice, on_team_result)
- **Decision:** D-071
- **Verified by:** `zellij/src/panel.rs` capital_t_leaves_the_team_and_both_keys_need_an_agent, team_results_are_said_in_plain_words; running the command and the poll `UNTESTED`

#### ZK-027 `u`: check for a new release
- **Behavior:** `u` MUST ask at once whether a newer release exists
  ([08-zellij-host.md](08-zellij-host.md) ZH-022), whatever `update_check`
  is set to, and the answer is shown as a notice. `Ctrl-u` is unaffected
  (ZK-006), and while a search is being typed `u` is text.
- **Source:** `zellij/src/panel.rs` (process_key, run_intent); `zellij/src/update.rs` (check_for_update_now)
- **Verified by:** `zellij/src/panel.rs` u_asks_for_a_release_check

#### ZK-028 Opening the status box
- **Behavior:** A left click on the row of the status line MUST open the
  status box (ZR-034), or close it when it is open; a left click on any
  other row closes it. `i` toggles it. `Esc` closes it before it closes
  help or ends a search. The click works whichever pane has the keyboard,
  and does not move the keyboard.
- **Source:** `zellij/src/app.rs` (click, toggle_info); `zellij/src/panel.rs` (process_key); `zellij/src/main.rs` (update_panel)
- **Verified by:** `zellij/src/view.rs` the_status_box_sits_on_the_status_line_and_a_click_toggles_it; `zellij/src/panel.rs` i_opens_the_status_box_and_esc_closes_it_first; MANUAL: click the status line while the keyboard is in the inner session

#### ZK-024 `/`: begin a search
- **Behavior:** `/` MUST close help and start typing a search: a new one
  with empty text, or the one in effect with its text kept. With the pipe
  transport it MUST instead notice `search needs the poll transport`.
- **Source:** `zellij/src/panel.rs` (process_key); `zellij/src/app.rs` (begin_search)
- **Verified by:** `zellij/src/panel.rs` slash_starts_a_search_that_captures_keys_until_enter_or_esc

#### ZK-025 Search typing
- **Behavior:** While a search is being typed (after rename mode, before
  every other binding) keys MUST be handled as: a character without Ctrl or
  Alt is appended; `Backspace` removes the last character; `Ctrl-u` clears
  the text; `Enter` keeps the search and returns the keys to the list, or
  ends the search when the text is blank; `Esc` ends the search. Each of
  these MUST ask for the list to be read again at once (ZK-026). Any other
  key does nothing. In the list, `Esc` closes help when it is open and
  otherwise ends a search in effect, reading the list again.
- **Source:** `zellij/src/panel.rs` (handle_search_key, process_key)
- **Verified by:** `zellij/src/panel.rs` slash_starts_a_search_that_captures_keys_until_enter_or_esc; `zellij/e2e/nested.sh` "/ lists the agents that match what is typed", "Esc ends the search"

#### ZK-026 Search results replace the list
- **Behavior:** The query is the search text, trimmed; a blank one is no
  query. With a query, every read of the registry MUST run
  `[noc_bin, "search", "--json", "--", query]` in place of `ls`
  ([08-zellij-host.md](08-zellij-host.md) ZH-009), so the matching is noc's
  ([03-cli.md](03-cli.md) CLI-037): words, `older:<age>`, `newer:<age>`. A
  snapshot applied while a query is in effect makes the list flat: every
  agent is a row in the order received, Quiet agents are not folded into
  sections, and none is hidden for its age; group headings count their rows.
  The first snapshot applied with no query folds the list again.
- **Source:** `zellij/src/panel.rs` (snapshot_argv, refresh); `zellij/src/app.rs` (search_text, apply_snapshot, rebuild_items); `zellij/src/view.rs` (shown_agent_count, group_count)
- **Decision:** D-065
- **Verified by:** `zellij/src/panel.rs` slash_starts_a_search_that_captures_keys_until_enter_or_esc; `zellij/src/view.rs` search_results_are_a_flat_list_with_the_query_on_the_second_line

---

## 5. Mockups

Rendered at 38 columns (the sidebar width in the host layout) with the
default theme, ANSI stripped, `generated_at = 2026-09-24T12:00:00Z`, Unicode
icons, spinner frame 0. `│` marks the pane edges and is not part of the
output; text to the right of the second `│` is commentary. The snapshot is
the `DEMO` fixture of `zellij/src/view.rs`: Blocked Claude background agent
`Auth migration` (user name, native name `auth`, approval detail
`Bash: make clean`, 2 minutes old, main checkout of `app` on `main`, cwd
`/code/app/svc`, model `opus`/`high`, a Zellij binding `work/7`); Working
Codex `Cache refactor` (linked worktree of `app` on `cache-fix`, 30 s old);
Review Claude `Docs pass` (plain directory `docs`, 3 h); Quiet:
`Old question` (ended `needs_input`, 5 h), `Yesterday` (Codex, idle, 30 h),
`Monday` (3 d 4 h), `Ancient` (23 d, not shown).

Panel, 40 rows, first row selected, sections closed (rows 14–25 are the
padded list area):

```text
│Blocked  1                            │  red, bold
│ ! Auth migration                     │  selected: both lines highlighted
│   ✳  2m  Bash: make clean            │
│Working  1                            │  yellow, bold
│ ⠋ Cache refactor                     │  spinner animates every 0.25 s
│   ⬡  0m  app · cache-fix             │
│Review  1                             │  teal, bold
│ ◆ Docs pass                          │
│   ✳  3h  docs                        │
│Quiet  3                              │  grey, bold
│ ▸ < 24h   1                          │
│ ▸ < 48h   1                          │
│ ▸ < 7d    1                          │
│                                      │
│  … (blank to the end of the list) …  │
│6 agents                             ●│  status: count accent, bold; dot green
│──────────────────────────────────────│  accent, dim
│Details                               │  accent, bold
│Auth migration                        │
│!  needs approval · 2m ago            │  red
│provider  claude · background         │
│native    auth                        │
│id        3cfff231                    │
│model     opus · high                 │
│repo      app · main                  │
│cwd       /code/app/svc               │
│ask                                   │  info, bold
│  Migrate the auth service to the new │  1 preview line (detail height 12 < 16)
│[?] keys  [d]etails  [D] window       │  muted, dim
```

Panel, 30 rows, `< 24h` opened and its agent selected:

```text
│Blocked  1                            │
│ ! Auth migration                     │
│   ✳  2m  Bash: make clean            │
│Working  1                            │
│ ⠋ Cache refactor                     │
│   ⬡  0m  app · cache-fix             │
│Review  1                             │
│ ◆ Docs pass                          │
│   ✳  3h  docs                        │
│Quiet  3                              │
│ ▾ < 24h   1                          │
│   ◌ Old question                     │  selected
│     ✳  5h  stopped waiting on you    │
│ ▸ < 48h   1                          │
│ ▸ < 7d    1                          │
│                                      │
│                                      │
│                                      │
│6 agents                             ●│
│──────────────────────────────────────│
│Details                               │
│Old question                          │
│?  needs input · 5h ago               │  details use the state symbol, not ◌
│provider  claude · ended              │
│                                      │
│                                      │
│                                      │
│                                      │
│[?] keys  [d]etails  [D] window       │
```

With the section row itself selected, the details block reads
`Quiet · < 24h` / `Conversations with no process.` /
`Enter opens this section; Enter on` / `an agent resumes it; h closes.`

The same panel with details hidden (`d`): the status line is the last one
above the footer.

```text
│ ▸ < 48h   1                          │
│ ▸ < 7d    1                          │
│  … (blank to the status line) …      │
│6 agents                             ●│
│[?] keys  [d]etails  [D] window       │
```

A prompt or a notice takes the whole status line while it lasts:

```text
│Rename: Old question▏                 │
│Stop Auth migration?  y / n           │
│/ older:2w▏                           │
│stop cancelled                        │
```

With a newer release recorded and a settings problem, and the status box
open (a click on the status line, or `i`):

```text
│╭─ status ───────────────────────────╮│
││ ● connected · poll                 ││
││ ! home_theme is set but away_theme ││
││   is not                           ││
││ ↑ v0.2.0 is available              ││
││   this is v0.1.3                   ││
││   set the new version in the       ││
││   plugin URL                       ││
│╰────────────────────────────────────╯│
│6 agents                           ↑ ●│  dot yellow: a problem
```

Floating details window, 70 × 24 (roomy):

```text
│Auth migration                                                        │
│!  needs approval · 2m ago                                            │
│provider  claude · background                                         │
│native    auth                                                        │
│id        3cfff231                                                    │
│session   3cfff231-aaaa-bbbb                                          │
│model     opus · high                                                 │
│repo      app · main                                                  │
│cwd       /code/app/svc                                               │
│ask                                                                   │
│  Migrate the auth service to the new token format and update all     │
│  callers                                                             │
│latest                                                                │
│  Waiting for approval to delete the build directory.                 │
│pane      work/7 · observed                                           │
│  … (blank to the last row) …                                         │
│Esc / D  close                                                        │
```

Empty list, before the first snapshot (6 rows):

```text
│No live agents.                       │
│                                      │
│                                      │
│                                      │
│0 agents                             ●│  dot yellow: connecting
│[?] keys  [d]etails  [D] window       │
```

Grouped by team (`v`), the `TEAMS` fixture of `zellij/src/view.rs`: team
`billing` with its lead `Lead` (Review), `Blocked one`, `Worker one` and one
Quiet agent; `Auth one` alone in a team with a long name; `Solo` in no team.
The first heading is selected:

```text
│a-very-long-team-name  1              │  selected; otherwise accent, bold
│ ◆ Auth one                           │
│   ✳  5m                              │
│billing  4                            │  accent, bold
│ ◆ Lead                               │  the lead first, whatever its state
│   ✳  4m  ★ lead  app · main          │
│ ? Blocked one                        │
│   ⬡  3m                              │
│ ⠋ Worker one                         │
│   ✳  2m  app · main                  │
│ ▸ Quiet   1                          │  the team's Quiet agents
│no team  1                            │
│ ⠋ Solo                               │
│   ✳  1m                              │
```

With `billing` folded, at 60 columns: `billing  4  1 blocked · 1 working ·
1 review`; at 38 columns the same line is cut to
`billing  4  1 blocked · 1 working · 1…`.

Server offline with no list to keep (10 rows):

```text
│noc server is not running.            │
│                                      │
│Start it with:                        │
│  noc server --bg                     │
│                                      │
│This panel will reconnect automatical…│  cut at 38 columns
│                                      │
│                                      │
│0 agents                             ●│  dot red: server offline
│[?] keys  [d]etails  [D] window       │
```

---

## Design choices

Implementation choices not recorded in
[`docs/DECISIONS.md`](../DECISIONS.md), with their rationale.

| Choice | Rationale | Source |
|---|---|---|
| Ages read `0m` instead of `now` | so ages line up in a column | `zellij/src/model.rs` (age_label) |
| Recency never uses `last_seen_at` | it is when noc last looked, every few seconds, and would make an idle agent read `0m` | `zellij/src/model.rs` (Agent::recency) |
| Ages are relative to the snapshot's `generated_at`, not the wall clock (the details window uses the wall clock) | the details window has no snapshot time of its own | `zellij/src/app.rs` (quiet_age), `zellij/src/details.rs` (on_details_result) |
| Review and Quiet sort by recency only; Blocked/Working by urgency first | Review and Quiet read as "most recent first"; the others by urgency | `zellij/src/model.rs` (compare_agents) |
| Unknown-age Quiet agents go into `< 24h` | rather than disappearing | `zellij/src/app.rs` (QuietAge::of) |
| Fixed two-thirds/one-third list/details split | so the details do not jump as the selection moves | `zellij/src/view.rs` (render) |
| Group colours are palette indices 1/2/6/8, not theme slots | they stay the same four colours under every theme | `zellij/src/view.rs` (group_color) |
| No selection glyph; a leading space keeps rows aligned | the row highlight marks the selection | `zellij/src/view.rs` (agent_primary_line) |
| Section count sits right after the label | it reads the same at any pane width | `zellij/src/view.rs` (section_line) |
| Secondary line leads with icon and fixed-width age | the short fields lead so a long label cannot push them out | `zellij/src/view.rs` (secondary_text) |
| Quiet agents show `○`/`◌` whatever state they stopped in | they have no process, so they read as at rest | `zellij/src/view.rs` (agent_symbol) |
| Working agents' symbol spins | activity reads at a glance | `zellij/src/view.rs` (agent_symbol) |
| A single 0.25 s panel tick; polling only when its interval is due | fast enough to animate without raising the poll rate | `zellij/src/main.rs` (on_panel_tick) |
| Notices survive five snapshots | so they can be read, then yield the line back to the connection state | `zellij/src/app.rs` (NOTICE_REFRESHES) |
| Stopped agents hidden until the registry stops reporting them | the registry learns of a stop through reconciliation, after the command returns | `zellij/src/app.rs` (apply_snapshot) |
| Theme keys must be function keys with modifiers | these are keys nobody types, which is what the theme relay wants | `zellij/src/app.rs` (encode_key) |
| A settings problem only turns the theme switch off | the agent list must keep working | `zellij/src/app.rs` (Config::problems) |
| Enter refuses when the agent is open in another Zellij session; `p` makes a second copy | opening it here is sometimes right and sometimes a fork, so the second copy is an explicit choice | `zellij/src/guest.rs` (guest_open_agent) |
| Stop policy is checked in the panel before `noc stop` runs | a refusal needs no command and no confirmation | `zellij/src/panel.rs` (request_stop) |
| One details window per panel, replaced on the next `D` | a second window would cover the first | `zellij/src/panel.rs` (open_details_window) |
| Control characters are dropped from every rendered text | they would otherwise reach the terminal verbatim | `zellij/src/view.rs` (sanitize) |
| Last output line printed without a newline | it would scroll the pane by one and hide the first row | `zellij/src/main.rs` (render) |

## Verification procedures

Unit tests run on the host target (from the repository root):

```sh
make zellij-check     # cargo fmt --check, clippy (wasm32-wasip1 and host), cargo test
make zellij-test      # cargo test only
```

Run one test or one module (from `zellij/`):

```sh
~/.cargo/bin/cargo test app::tests::quiet_agents_fold_into_collapsed_time_sections -- --exact
~/.cargo/bin/cargo test view::tests -- --nocapture
~/.cargo/bin/cargo test model::
```

Tests in scope for this section: `model::tests`, `app::tests`,
`view::tests`, `panel::tests`, `details::tests`, and the tests of
`guest::tests` and `panes::tests` cited above.

The end-to-end suite is `zellij/e2e/nested.sh` (`make -C zellij e2e`). It
starts an isolated host/inner pair with the fake `noc`
(`zellij/e2e/fake-noc`) and drives it with real key presses. Of this
section it covers: the panel lists the registry's agents, `Enter` opens a
Quiet section and then opens an agent in a new inner tab, `Enter` again
focuses that tab instead of opening a copy, `D` opens the details window
and `q` closes it. Its other checks, its prerequisites and the isolated dev
pair for manual checks are described in
[08-zellij-host.md](08-zellij-host.md),
[09-verification.md](09-verification.md) and
[`docs/DEVELOPMENT.md`](../DEVELOPMENT.md).

## Known limitations

- **Stopped agents may stay hidden indefinitely.** A stopped agent is
  hidden until a snapshot omits it (ZG-012). The panel polls `ls --all`,
  and the registry keeps a stopped agent as a quiet, resumable row (D-041,
  D-043), so the agent normally never leaves the snapshots and stays hidden
  in that panel instance until the plugin reloads, even though it could be
  resumed.
- **Commands have no timeout.** A registry poll marks a command in flight
  and clears the mark only when its result arrives (ZK-014). If a `noc ls`
  never returns, the panel stops polling and `r` does nothing until the
  plugin reloads. The details window's `noc show` has the same shape
  (ZR-031).
- **Recency ordering is lexical** (ZG-002): timestamps with different
  offsets or fractional precision can misorder.
- **Pipe-mode ages** are relative to the frame's `generated_at`; ages
  freeze between frames.
- **Only the first settings problem is shown** (ZM-024); the others appear
  one at a time as each is fixed and the plugin reloaded.
- **The help view's `Anywhere` keys are fixed text** (ZR-005) and do not
  reflect a host layout that rebinds them.
- **Names are sanitised only when rendered.** Control characters are
  removed from panel and details-window lines (ZR-001), but the agent title
  is passed as is to the details window's pane title (ZR-031) and to the tab
  and pane names used when opening an agent (ZK-009, ZK-010).
- **Opened tabs and panes keep the name they were opened with.** A later
  rename changes the panel rows and details (ZK-022) but not the name of a
  tab or pane already open in the inner session.
- **Offline text is truncated at the sidebar width.**
  `This panel will reconnect automatically.` is 40 columns and is cut with
  `…` in the 38-column sidebar (ZR-006).
- **The details window hides later errors** (ZR-030) and shows a bare
  `×  ` when `noc show` fails with empty stderr (ZR-031).
- **The details window receives `icons=nerd` but never uses it**: the
  detail lines have no provider icon (ZK-013, ZR-024).
- **Rename prompt**: long text truncates the cursor out of view;
  `Alt-<char>` inserts the character; there is no cursor movement (ZK-021).
- **Joining a folded team loses the selection.** Grouped by team, an agent
  put into a team whose group is folded is no longer a row, so the
  selection falls back to the same index (ZG-009) instead of opening the
  team.
- **The team prompt completes from the list in hand** (ZK-031): during a
  search, only the teams of the matching agents.
- **Half page** uses the whole panel height in rows of `items` (ZK-006).
- **Help hides the list but keys still act on it** (ZK-007): `x`, `n` and
  `Enter` operate on an invisible selection.
- **Section details text** says "Enter opens this section" even when it is
  open (ZR-023).
- **`Right` is not bound** although `Left` mirrors `h` (ZK-003), and
  `Space` toggles a section although the help does not list it (ZK-008).
- **`TextPreview.truncated`** is parsed but never indicated in previews.
