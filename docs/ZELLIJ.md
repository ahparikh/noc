# The Zellij frontend

`zellij-noc` shows the noc registry as a sidebar beside your own Zellij
session. From it you open, focus, rename and stop agents. It is one WASM
plugin, loaded from a release URL, and two layout files and a theme that
live in your dotfiles.

Exact behavior is specified in [spec/07-zellij-panel.md](spec/07-zellij-panel.md)
(what the sidebar shows and its keys) and
[spec/08-zellij-host.md](spec/08-zellij-host.md) (how it works with Zellij).

## How it is arranged

```
┌ host session: noc-host ────────────────────────────────────────────┐
│ ┌ NOC ──────────┐ ┌ guest pane ──────────────────────────────────┐ │
│ │ Blocked  1    │ │ inner session: noc                           │ │
│ │ Working  2    │ │   your tabs, panes, keys and theme,          │ │
│ │ Review   2    │ │   exactly as you configured them             │ │
│ │ 5 agents    ● │ │                                              │ │
│ └───────────────┘ └──────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────┘
```

- The **host session** has one tab with two panes: the sidebar, and a guest
  pane that keeps a client of the inner session attached.
- The **inner session** is your ordinary Zellij session. Agents open in it
  as tabs and panes.
- The host sees every key first, but binds only three (below). Every other
  key goes to the inner session unchanged.

Requires Zellij 0.46, and `noc` installed and set up (see the
[README](../README.md)).

## Set up

Everything the sidebar needs is in two layout files and a theme file, all from [`zellij/config/`](../zellij/config/). You put them into
your Zellij configuration once per machine and keep them with the rest of
it. Nothing is generated, and your `config.kdl` is not touched.

One script copies them, straight from GitHub:

```sh
curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash
curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash -s -- --check   # only say what would change
```

From a checkout, `make zellij-install-config` runs the same script
(see [Installing the files](#installing-the-files)). By hand, it is:

1. **Host layout**: copy
   [`zellij/config/layouts/noc-host.kdl`](../zellij/config/layouts/noc-host.kdl)
   to `~/.config/zellij/layouts/noc-host.kdl`. It holds the sidebar plugin
   and every one of its settings.
2. **Inner layout**: copy
   [`zellij/config/layouts/noc-inner.kdl`](../zellij/config/layouts/noc-inner.kdl)
   to `~/.config/zellij/layouts/noc-inner.kdl`. It holds what your session
   has only inside the pair (see
   [Settings for the inner session only](#settings-for-the-inner-session-only)).
3. **Theme**: copy
   [`zellij/config/themes/noc-blue.kdl`](../zellij/config/themes/noc-blue.kdl)
   to `~/.config/zellij/themes/`.

## Start and stop

From a plain terminal (not inside Zellij):

```sh
zellij attach --create noc-host options --default-layout noc-host
```

That one line starts the pair, or attaches to it when it is already
running, so it is worth an alias:

```sh
alias znoc='zellij attach --create noc-host options --default-layout noc-host'
```

`Alt d` detaches and leaves both sessions running. To end both, end the
inner session: quit it, or run `zellij kill-session noc`. The host session
ends with it.

The first time, the sidebar shows Zellij's permission prompt for the plugin.
Answer `y`. Zellij remembers the answer for that plugin location.

If Zellij shows its own welcome screen in new sessions, it shows it in both
sessions of the pair. `Ctrl c` in that screen turns it off for good.

## Keys

In the host session, anywhere:

| Key | Action |
|---|---|
| `Alt m` | Move the keyboard to the sidebar, or back to the inner session |
| `Alt o` | Hide the sidebar (the inner session fills the window), or show it |
| `Alt d` | Detach. Both sessions keep running; the start line attaches again |

These three are bound in `layouts/noc-host.kdl`; change them there. The
sidebar's help view (`?`) lists the defaults.

In the sidebar (after `Alt m`):

| Key | Action |
|---|---|
| `j` / `k`, `gg` / `G`, `Ctrl d` / `Ctrl u` | Move |
| `Enter` / `l` | Open the agent in the inner session (focus it if it is already open), or open a Quiet section |
| `h` | Close a Quiet section |
| `p` / `s` | Open the agent as a pane / a stacked pane in the current inner tab |
| `w` | Open a shell in the agent's worktree |
| `d` / `D` | Toggle details / open the details window |
| `n` | Rename the agent |
| `x` | Stop the agent (asks `y`/`n`) |
| `r` | Refresh |
| `/` | Search (below) |
| `u` | Check now for a newer plugin release |
| `i` | Open or close the status box (also: click the status line) |
| `?` | Help |

After `Enter`, `p`, `s` or `w` the keyboard returns to the inner session.
The sidebar's selection follows the pane you are working in.

`/` searches. Type words to match an agent's name, folder, branch or
request; add `older:14d` for agents idle that long or more, or `newer:3d`
for ones active within it (`d` days, `w` weeks, `h` hours). The list
narrows as you type and includes agents too old for the normal view, so
`/older:3w` finds the conversations you left weeks ago. `Enter` keeps the
results and gives the keys back to the list, `/` edits the search, `Esc`
ends it. The same search is `noc search` in a shell.

A tab that holds one agent is named after it and renamed when the agent's
name changes. That applies to tabs the sidebar opened and to tabs still
called `Tab #N`; a tab you named yourself keeps your name.

## Configuration reference

Everything is in the `plugin` block of `layouts/noc-host.kdl`:

```kdl
plugin location="https://github.com/ahparikh/noc/releases/download/v0.1.3/zellij-noc.wasm" {
    noc_bin "noc"
    guest_session "noc"
    icons "unicode"
    poll_interval_ms "1000"
    home_theme "my-theme"
    away_theme "my-theme-away"
}
```

The settings are read when the host session starts: after a change, end the
pair and start it again.

| Setting | Default | Meaning |
|---|---|---|
| `noc_bin` | `noc` | The noc executable. A bare name is looked up on the `PATH` Zellij was started with. Use an absolute path if the sidebar reports that it cannot run noc. |
| `guest_session` | `noc` | The inner session's name. Must match `inner=` in the layout. |
| `icons` | `unicode` | `nerd` to use Nerd Font provider icons. |
| `poll_interval_ms` | `1000` | How often the sidebar reads `noc ls`. Clamped to 250..60000. |
| `home_theme`, `away_theme` | unset | Optional pair, see below. |
| `pane_titles` | `show` | `hide` blanks the title in the frame of every pane of the inner session, since the tab already names the agent. The shipped layout sets `hide`. A title a program sets later is then not shown. |
| `colors` | built-in | The sidebar's colours: by default a fixed dark palette (blue header, red Blocked, yellow Working, teal Review, grey Quiet). `zellij` follows your Zellij theme instead; use it on a light terminal. |
| `update_check` | `true` | `false` stops the sidebar from asking GitHub whether a newer release exists (see [Upgrading the plugin](#upgrading-the-plugin)). |
| `away_key`, `home_key` | `Ctrl Alt Shift F11`, `Ctrl Alt Shift F12` | The keys the sidebar types into the inner session to switch theme. Function keys `F1`..`F12` with any of `Ctrl`, `Alt`, `Shift`. Change them only if they collide with something. |
| `transport`, `pipe_name` | `poll`, `zellij-noc.registry` | With `transport "pipe"` the sidebar stops polling and instead takes registry snapshots sent to the Zellij pipe named by `pipe_name`: one JSON document per line, in the format `noc ls --json` prints. |

### The status line

The line under the list shows how many agents there are and, at its right
edge, a dot: green when connected, yellow when something needs a look (a
setting that cannot work, the inner session not running, still connecting),
red when the registry cannot be reached. An `↑` beside the dot means a
newer plugin release exists.

Click the line, or press `i` in the sidebar, for a small box that says it
in words: the connection, any problem, the version you are running and the
newer one. The click works whichever side has the keyboard. Click again, or
`Esc`, to close it.

When a setting is unusable the sidebar carries on without the affected
feature.

### Dimming the inner session

Both sessions draw a focused pane, so without help you would see two
highlighted frames. The inner session therefore changes theme as the
keyboard moves: it wears `home_theme` while it has the keyboard and
`away_theme` while the sidebar does, so only the side you are typing in
looks focused.

This is on as shipped, with the pair in `themes/noc-blue.kdl`: `noc-blue`
(Catppuccin Mocha with a blue accent) and `noc-blue-away`. It means your
session wears `noc-blue` inside the pair, whatever `theme` your `config.kdl`
names. To turn dimming off, comment out `home_theme` and `away_theme` in
the plugin block of `noc-host.kdl`. To dim with your own theme instead:

1. In your theme file, copy your theme's block and name the copy
   `<name>-away`. In the copy, set `frame_selected`'s `base` to the same
   colour as `frame_unselected`'s `base`. Keep the two blocks in step when
   you change your theme.
2. Set `home_theme "<name>"` and `away_theme "<name>-away"` in the plugin
   block.
3. In the two key bindings at the bottom of `noc-inner.kdl`, put the same
   two names in the payloads (`payload "theme <name>-away"` and
   `payload "theme <name>"`). The sidebar types those keys into the inner
   session; each binding tells a hidden instance of the plugin there which
   theme to switch to.

### Installing the files

The script is [`zellij/install-config.sh`](../zellij/install-config.sh). It
sets a machine up the first time and upgrades it every time after: run the
same command.

Piped from GitHub it downloads the files of the **latest release** (or of
the tag or branch in `NOC_REF`), so the layouts it installs load a plugin
that is published. Run from a checkout it uses the files beside it. It
writes to `$ZELLIJ_CONFIG_DIR`, or `~/.config/zellij`:

- **`layouts/noc-host.kdl`**: the new file, with your settings. Every
  setting you had set in the plugin block is carried over (`noc_bin`,
  `colors`, `icons` and the rest), as are your session names if you changed
  them, and it tells you which. Anything else you edited in that file (the
  host session's keys, for example) is not carried over; it is in the
  `.bak`.
- **`layouts/noc-inner.kdl`**: replaced when yours is the shipped file.
  When you have edited it, yours is kept, only the plugin location in its
  two bindings is updated, and the new file is left beside it as
  `noc-inner.kdl.new` to compare.
- **`themes/noc-blue.kdl`**: replaced when it differs.

Whatever it replaces is kept beside the new file as `<name>.bak`. Nothing
else is read or written: not `config.kdl`, not your shell profile. It ends
by naming the plugin version now installed and how to restart the pair.

One thing to know: a setting counts as yours only while it is set. If you
turned a shipped default off by commenting it out, an upgrade brings the
default back. To keep it off, set it to an empty value instead, for example
`home_theme ""` and `away_theme ""` to keep dimming off.

### Settings for the inner session only

Your inner session reads `config.kdl`, like every other Zellij session you
start. To change something only while it runs beside the sidebar, put the
option in `layouts/noc-inner.kdl`, outside its `layout` block. Any option
`config.kdl` accepts works there, and it overrides `config.kdl` for the
inner session alone. Sessions started any other way never read the file.

As shipped it sets three:

```kdl
session_indicator false            // no "[ N running ]" box in the tab bar

attention {
    enabled false                  // no pane status markers, inbox or notifications
}

ui {
    pane_frames {
        hide_session_name true     // "Zellij (noc)" becomes "Zellij"
    }
}
```

Delete a line to get Zellij's normal behaviour back. The options are read
when the inner session is created, so after a change end the pair
(`zellij kill-session noc`) and start it again. The file's `layout` block is the inner session's first tab; it
replaces `default_layout` for that session.

The host session's own options are at the bottom of `layouts/noc-host.kdl`.

### Upgrading the plugin

Zellij downloads a plugin URL once and then keeps using its cached copy, so
a "latest" URL would never upgrade, and Zellij never says that a newer
plugin exists. The URL therefore names the version, and the sidebar does
the telling: when a release newer than the loaded plugin is out, an `↑`
appears beside the dot on its status line, and the status box (click the
line, or `i`) names both versions.

```
5 agents                         ↑ ●
```

It finds out by asking GitHub, with `curl`, where the repository's "latest
release" link points: once when the sidebar starts and every six hours
after. Press `u` in the sidebar to check at once; it answers on the status
line (`v0.1.2 is available`, or that yours is the latest). Nothing is
downloaded. If `curl` or the network is missing, no
marker shows. `update_check "false"` turns the check off.

To upgrade, run the setup script again (it installs the latest release's
layouts and keeps your settings, see
[Installing the files](#installing-the-files)), then start the pair again
(`zellij kill-session noc`, then the start line):

```sh
curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash
```

Because the plugin's location changed, Zellij asks for its permissions once
more. By hand, the same upgrade is changing the version in the plugin URL,
which appears once in `noc-host.kdl` and twice in `noc-inner.kdl`.

To avoid the download and the repeated prompt, download `zellij-noc.wasm`
from the release to a fixed path and use `location="file:/that/path"`.

### Keys that should work from the sidebar

While the sidebar has the keyboard, your inner session's keys go to the
sidebar, not to the inner session. If you want some of them (say, your
new-tab key) to work from there too, bind them in `layouts/noc-host.kdl` to
hand the keyboard back and type the key:

```kdl
bind "Alt t" { FocusPreviousPane; Write 27 116; }   // ESC t
```

## How it works

- **One plugin, three roles.** The same file runs as the sidebar, as the
  floating details window, and as a hidden relay in the inner session that
  only switches themes.
- **The inner session is driven over the Zellij CLI.** A plugin can only
  act on its own session, so the sidebar reads the inner session with
  `zellij --session noc action list-clients` / `list-panes`, and opens and
  focuses agents there with `zellij --session noc action …` and `zellij run`.
- **The guest pane is a small shell loop**, written out in the layout. It
  creates the inner session on first start, re-attaches when the client
  detaches, and ends the host session when the inner session ends.
- **The registry is read through the CLI only**: `noc ls --json --all`,
  `noc search --json` and `noc show --json`. Opening an agent runs `noc attach <id>`; rename and stop
  run `noc rename` and `noc stop`.
- **Permissions.** The plugin asks for six: read and change application
  state, open panes, reconfigure (the relay's theme switch), run commands,
  and message other plugins.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Sidebar shows a permission prompt and nothing else | Move the keyboard to it (`Alt m`) and answer `y`. |
| `noc server is not running` | Run `noc server --bg`. The sidebar reconnects by itself. |
| A red dot; the status box says `disconnected · …` naming an executable | Zellij cannot find `noc`. Set `noc_bin` to its absolute path. |
| A yellow dot; the status box says `inner session "noc" is not running` | The guest pane's loop is not attached. Check that `guest_session` matches `inner=` in the layout. |
| The inner session's status bar looks unfocused | Press `Alt m` twice. The sidebar tells the inner client it has the keyboard each time it comes back. |
| The inner theme does not change | Both `home_theme` and `away_theme` must be set in `noc-host.kdl`, the bindings in `noc-inner.kdl` must name the same two themes, and both themes must exist. |
| The pair starts and ends at once | `layouts/noc-inner.kdl` is missing from `~/.config/zellij/layouts/`, so the inner session cannot be created. |
| A new plugin version is not picked up | The URL in `noc-host.kdl` still names the old version. |
| `↑ vX.Y.Z` stays after upgrading | The pair was not restarted (`zellij kill-session noc`, then the start line), so the old plugin is still running. |
