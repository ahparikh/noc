//! The inner ("guest") sessions, one session per team: the `relay` role that
//! runs inside each, sends the panel its panes, tabs and focus as they
//! change, switches its theme and moves its client to another session; and the
//! panel's side: asking the relay of the session on show to watch, driving that
//! session over the Zellij CLI, moving between sessions, and the keyboard signals
//! typed into the guest pane.

use crate::app::{is_plain_name, Config, PaneTitles};
use crate::model::{parse_terminal_pane_id, Agent};
use crate::panes::{
    agent_named_by_pane_command, compact_error, is_default_tab_name, manifest_panes, LaunchedPane,
    ScanPane,
};
use crate::{ctx, now_secs, State, KEY, PIPE_GUEST, PIPE_RELAY};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use zellij_tile::prelude::*;

/// How often the panel asks the relay to watch. Each ask is answered with a
/// snapshot, and starts the relay when it is not running.
pub const GUEST_WATCH_SECS: f64 = 2.0;
/// The same, while the panel has no usable answer.
pub const GUEST_RETRY_SECS: f64 = 0.75;
/// How long the inner session may be unreadable before the panel says so.
/// It takes a moment to start, and it is briefly gone while it restarts.
pub const GUEST_MISSING_SECS: f64 = 5.0;
/// How long a relay keeps sending after it was last asked to watch.
pub const RELAY_WATCH_SECS: f64 = 10.0;
pub const CTX_WATCH: &str = "guest-watch";
pub const CTX_RELAY_PUSH: &str = "relay-push";
pub const CTX_OPEN: &str = "guest-open";
pub const CTX_SWITCH: &str = "guest-switch";
pub const CTX_END: &str = "end-session";
/// How often the panel reads which inner sessions are running.
pub const SESSIONS_READ_SECS: f64 = 1.0;
/// How long a move to another session may take before the panel stops waiting
/// for it.
pub const SWITCH_SECS: f64 = 5.0;
/// How long the panel waits for Zellij to say the client is in the new session
/// before it asks that session's relay anyway.
pub const SWITCH_LANDING_SECS: f64 = 1.0;
/// How often the panel asks Zellij where the client is while it waits. It
/// must differ from the panel's tick: the two timers are told apart by
/// their length.
pub const SWITCH_POLL_SECS: f64 = 0.03;
/// How long after the client landed in a session that was just created the
/// panel waits before it starts that session's relay: `zellij pipe` would wait
/// for ever on a relay started before the session has a tab to hold it.
pub const SWITCH_SETTLE_SECS: f64 = 0.25;
/// While a move is under way: how often the panel asks the new session's
/// relay, and how long it waits for one request before it sends another.
pub const SWITCH_WATCH_SECS: f64 = 0.1;
pub const SWITCH_WATCH_LOST_SECS: f64 = 1.0;
/// How long after the guest pane's client exited the panel decides whether
/// the user detached. Shorter than the pause of the guest pane's loop before
/// it attaches again, and long enough for an ended session to be gone.
pub const CLIENT_EXIT_SECS: f64 = 0.5;
/// How long a watch request may run before the panel sends another.
pub const GUEST_WATCH_LOST_SECS: f64 = 5.0;
/// What joins the inner session's name and a team's name in a session's name.
pub const TEAM_SEPARATOR: &str = "--";
/// What the tab bar calls the session of the agents in no team.
pub const NO_TEAM: &str = "no-team";
/// The layout a session is created from, as the guest pane's loop names it.
pub const INNER_LAYOUT: &str = "noc-inner";
pub const CTX_HOST_ACTION: &str = "host-action";
pub const KEY_GUEST_AGENT: &str = "zellij-noc-guest-agent";

/// Zellij's nested-session "focus gained" frame, `ESC P 26661 n SgA= ESC \`.
/// The host session never descends into the inner one, so it never sends
/// this itself; without it the inner client draws itself as unfocused.
pub const FOCUS_FRAME: [u8; 14] = [27, 80, 50, 54, 54, 54, 49, 110, 83, 103, 65, 61, 27, 92];

const RUN_SCRIPT: &str = "guest=$1; host=$2; shift 2\nzellij --session \"$guest\" \"$@\"; status=$?\n[ -n \"$host\" ] && zellij --session \"$host\" action focus-previous-pane >/dev/null 2>&1\nexit $status";

#[derive(Debug, Default)]
pub struct GuestState {
    /// The session on show: the inner session the guest pane's client is in,
    /// as far as the panel knows. Everything below is about this session.
    pub session: String,
    /// What the panel knew about the sessions that are not on show.
    pub sessions: BTreeMap<String, InnerSession>,
    /// A move to `session` was asked for and its relay has not answered yet.
    pub switching: bool,
    pub switched_at: Option<f64>,
    /// When the running sessions were last read for the Sessions section.
    pub sessions_read: Option<f64>,
    /// The agent to open once the session on show has been read.
    pub pending_open: Option<String>,
    /// Zellij has said that the client is in `session` since the move began.
    pub landed: bool,
    /// The move is to a session that was not running: it is being created,
    /// and its relay is not asked before `settle_until`.
    pub fresh: bool,
    pub settle_until: Option<f64>,
    /// The session Zellij last said is attached in the guest pane; none
    /// once that client has exited.
    pub shown: Option<String>,
    /// The session the guest pane's client was in when it exited, and when
    /// the panel saw that, until it is known why.
    pub client_exit: Option<(String, f64)>,
    /// The tab a session was created with, when the panel created the session to
    /// open an agent in it: closed once the agent's tab is there.
    pub spare_tab: Option<u32>,
    pub snapshot: Option<GuestSnapshot>,
    /// A watch request is running.
    pub in_flight: bool,
    /// When the last watch request started, and whether it reached the
    /// inner session.
    pub last_watch: Option<f64>,
    pub watch_ok: bool,
    /// When the relay last sent a snapshot.
    pub last_push: Option<f64>,
    pub launched: BTreeMap<String, LaunchedPane>,
    /// Inner tabs whose name is the panel's to change, by tab id.
    pub named_tabs: BTreeMap<u32, TabName>,
    /// For each pane the panel has seen: the title it had by itself, and
    /// the name the panel gave it (empty when it gave none).
    pub titles: BTreeMap<u32, (String, String)>,
    /// This machine's short host name and the user's shell, for pane names.
    pub host: Option<(String, String)>,
    /// Whether the last snapshot showed a client attached to the inner session.
    pub client_seen: bool,
    pub failing_since: Option<f64>,
}

/// A session that is not on show, as the panel last saw it.
#[derive(Debug, Default)]
pub struct InnerSession {
    pub snapshot: Option<GuestSnapshot>,
    pub launched: BTreeMap<String, LaunchedPane>,
    pub named_tabs: BTreeMap<u32, TabName>,
    pub titles: BTreeMap<u32, (String, String)>,
}

/// A tab name the panel gave, or found to be an agent's.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TabName {
    pub name: String,
    /// The name the tab had when the panel asked for `name`, until a
    /// snapshot shows the rename.
    pub before: Option<String>,
}

/// The inner session as its relay sees it: how many clients are attached,
/// the terminal pane the first of them has the keyboard in, and the
/// terminal panes of every tab.
#[derive(Debug, Clone, Default, PartialEq, Deserialize, Serialize)]
#[serde(default)]
pub struct GuestSnapshot {
    /// The session the relay was asked to watch as: its own.
    pub session: String,
    pub clients: usize,
    pub focused: Option<u32>,
    pub panes: Vec<ScanPane>,
}

/// The relay's side: what it last heard from Zellij and from the panel.
#[derive(Debug, Default)]
pub struct RelayState {
    /// The session whose panel asked to watch, and when it last asked.
    pub host: Option<String>,
    /// This session's name, as the panel addressed it.
    pub session: String,
    pub watched_at: f64,
    pub panes: Vec<ScanPane>,
    pub tabs: Vec<TabInfo>,
    /// The snapshot last sent, as sent.
    pub sent: Option<String>,
    pub in_flight: bool,
    /// The newest snapshot, waiting for the one being sent.
    pub pending: Option<String>,
}

// ---- pure helpers ----

/// A snapshot as the relay sent it. Plugin panes are dropped.
pub fn parse_guest_snapshot(text: &str) -> Option<GuestSnapshot> {
    let mut snapshot: GuestSnapshot = serde_json::from_str(text.trim()).ok()?;
    snapshot.panes.retain(|p| !p.is_plugin);
    Some(snapshot)
}

/// The relay's snapshot: the session's terminal panes, each with its tab's
/// id and name, and the pane the first client has the keyboard in.
pub fn relay_snapshot(
    session: &str,
    panes: &[ScanPane],
    tabs: &[TabInfo],
    clients: &[ClientInfo],
) -> GuestSnapshot {
    let panes = panes
        .iter()
        .filter(|p| !p.is_plugin)
        .map(|pane| {
            let mut pane = pane.clone();
            if let Some(tab) = tabs.iter().find(|t| t.position == pane.tab_position) {
                pane.tab_id = u32::try_from(tab.tab_id).ok();
                pane.tab_name = tab.name.clone();
            }
            pane
        })
        .collect();
    let focused = clients.first().and_then(|client| match client.pane_id {
        PaneId::Terminal(id) => Some(id),
        PaneId::Plugin(_) => None,
    });
    GuestSnapshot {
        session: session.to_string(),
        clients: clients.len(),
        focused,
        panes,
    }
}

/// The host session and the relay's own session that a `watch <host>
/// <session>` message names.
pub fn watch_host(payload: &str) -> Option<(String, String)> {
    let mut names = payload.trim().strip_prefix("watch ")?.split_whitespace();
    let (host, session) = (names.next()?, names.next()?);
    (names.next().is_none() && is_plain_name(host) && is_plain_name(session))
        .then(|| (host.to_string(), session.to_string()))
}

/// The session a `switch <session> <layout>` message asks the relay to move its
/// client to, and the layout to create it from when it is not running. The
/// layout is a file name or a path, so it may hold anything but control
/// characters.
pub fn switch_request(payload: &str) -> Option<(String, String)> {
    let rest = payload.trim().strip_prefix("switch ")?.trim();
    let (name, layout) = rest.split_once(char::is_whitespace)?;
    let layout = layout.trim();
    (is_plain_name(name) && !layout.is_empty() && !layout.chars().any(char::is_control))
        .then(|| (name.to_string(), layout.to_string()))
}

/// The session of a team: the inner session itself for the agents in no team
/// (and for a name that could not be part of a session's), and otherwise the
/// session `<inner>--<team>`.
pub fn team_session(guest: &str, team: &str) -> String {
    if team.is_empty() || !is_plain_name(team) {
        guest.to_string()
    } else {
        format!("{guest}{TEAM_SEPARATOR}{team}")
    }
}

/// Whether a session is one of the sessions of the inner session `guest`.
pub fn is_inner_session(guest: &str, session: &str) -> bool {
    session == guest
        || session
            .strip_prefix(guest)
            .and_then(|rest| rest.strip_prefix(TEAM_SEPARATOR))
            .is_some_and(|team| !team.is_empty())
}

/// What the tab bar calls a session: its team, or `no-team`.
pub fn team_label(session: &str) -> &str {
    match session.split_once(TEAM_SEPARATOR) {
        Some((_, team)) if !team.is_empty() => team,
        _ => NO_TEAM,
    }
}

/// The only tab of a session that was just created, if nothing has been done
/// in it: one tab, with one pane that was not started with a command.
pub fn untouched_tab(snapshot: &GuestSnapshot) -> Option<u32> {
    let [pane] = snapshot.panes.as_slice() else {
        return None;
    };
    (pane.terminal_command.is_none() && !pane.exited && is_default_tab_name(&pane.tab_name))
        .then_some(pane.tab_id)
        .flatten()
}

/// What to do about the tab a session was created with, now that the session
/// looks like `snapshot`: `Some(true)` to close it (the agent's tab is
/// there and nothing was done in the spare one), `Some(false)` to leave it
/// for good, `None` to look again at the next snapshot.
pub fn spare_tab_verdict(snapshot: &GuestSnapshot, spare: u32) -> Option<bool> {
    let in_spare: Vec<&ScanPane> = snapshot
        .panes
        .iter()
        .filter(|p| p.tab_id == Some(spare))
        .collect();
    let [pane] = in_spare.as_slice() else {
        // Gone, or panes were added to it.
        return Some(false);
    };
    if pane.terminal_command.is_some() || pane.exited || !is_default_tab_name(&pane.tab_name) {
        return Some(false);
    }
    snapshot
        .panes
        .iter()
        .any(|p| p.tab_id.is_some_and(|tab| tab != spare))
        .then_some(true)
}

pub fn close_tab_args(tab: u32) -> Vec<String> {
    strings(&["action", "close-tab-by-id", &tab.to_string()])
}

/// The session an agent opens in: a session that already holds a live pane of
/// it (the one on show first), and otherwise its team's session.
pub fn agent_session(
    agent: &Agent,
    guest: &str,
    session: &str,
    snapshot: Option<&GuestSnapshot>,
    launched: &BTreeMap<String, LaunchedPane>,
    sessions: &BTreeMap<String, InnerSession>,
    noc_bin: &str,
) -> String {
    if snapshot.is_some_and(|s| guest_agent_pane(agent, s, launched, session, noc_bin).is_some()) {
        return session.to_string();
    }
    sessions
        .iter()
        .find(|(name, other)| {
            other.snapshot.as_ref().is_some_and(|s| {
                guest_agent_pane(agent, s, &other.launched, name, noc_bin).is_some()
            })
        })
        .map(|(name, _)| name.clone())
        .unwrap_or_else(|| team_session(guest, agent.team_name()))
}

/// The panel's request to a session's relay: watch this session for the panel
/// in `host`.
pub fn watch_command(guest: &str, plugin_url: &str, host: &str) -> Vec<String> {
    relay_command(guest, plugin_url, &format!("watch {host} {guest}"))
}

/// The panel's request to the relay of the session on show: move the client
/// to the session `target`, creating it from `layout` when it is not running.
pub fn switch_command(guest: &str, plugin_url: &str, target: &str, layout: &str) -> Vec<String> {
    relay_command(guest, plugin_url, &format!("switch {target} {layout}"))
}

/// A message to the relay of the session `guest`. `zellij pipe` starts the
/// relay when it is not running; the configuration is the one the relay
/// bindings load it with, so there is one relay in a session.
fn relay_command(guest: &str, plugin_url: &str, message: &str) -> Vec<String> {
    strings(&[
        "zellij",
        "--session",
        guest,
        "pipe",
        "--plugin",
        plugin_url,
        "--plugin-configuration",
        "role=relay",
        "--name",
        PIPE_RELAY,
        "--",
        message,
    ])
}

/// The relay's message to the panel: one snapshot.
pub fn push_command(host: &str, snapshot: &str) -> Vec<String> {
    strings(&[
        "zellij",
        "--session",
        host,
        "pipe",
        "--name",
        PIPE_GUEST,
        "--",
        snapshot,
    ])
}

/// The theme a relay message asks for: `away` or `home` (named by the
/// relay's own settings), or `theme <name>`. Anything that is not a plain
/// name is refused, since it ends up inside KDL.
pub fn relay_theme(payload: &str, config: &Config) -> Option<String> {
    let payload = payload.trim();
    let name = match payload {
        "away" => config.away_theme.clone()?,
        "home" => config.home_theme.clone()?,
        _ => payload.strip_prefix("theme ")?.trim().to_string(),
    };
    is_plain_name(&name).then_some(name)
}

/// What to type into the guest pane when the keyboard arrives in the inner
/// session (`to_guest`) or leaves it for the panel.
pub fn keyboard_signal(config: &Config, to_guest: bool) -> Vec<u8> {
    let themes = config.themes_enabled();
    let key = |spec: &str| {
        themes
            .then(|| crate::app::encode_key(spec))
            .flatten()
            .unwrap_or_default()
    };
    if to_guest {
        let mut bytes = key(&config.home_key);
        bytes.extend_from_slice(&FOCUS_FRAME);
        bytes
    } else {
        key(&config.away_key)
    }
}

/// The inner tabs whose name should follow their agent, as `(tab id, new
/// name)`. A tab's name is the panel's to change when it is one the panel
/// gave or found to be an agent's (`named`), or one Zellij made up
/// (`Tab #3`); the tab is renamed while exactly one agent runs in it. A tab
/// the user named themselves is left alone. `named` is brought up to date
/// with what the snapshot shows.
pub fn tab_renames(
    agents: &[Agent],
    snapshot: &GuestSnapshot,
    launched: &BTreeMap<String, LaunchedPane>,
    guest_session: &str,
    noc_bin: &str,
    named: &mut BTreeMap<u32, TabName>,
) -> Vec<(u32, String)> {
    // tab id -> (current name, the agents in it)
    let mut tabs: BTreeMap<u32, (&str, Vec<&Agent>)> = BTreeMap::new();
    for pane in &snapshot.panes {
        let Some(tab) = pane.tab_id else { continue };
        let entry = tabs
            .entry(tab)
            .or_insert((pane.tab_name.as_str(), Vec::new()));
        if let Some(agent) =
            crate::panes::agent_in_pane(agents, pane, launched, noc_bin, Some(guest_session))
        {
            if !entry.1.iter().any(|a| a.id == agent.id) {
                entry.1.push(agent);
            }
        }
    }
    named.retain(|tab, _| tabs.contains_key(tab));
    let mut renames = Vec::new();
    for (tab, (name, agents_in_tab)) in tabs {
        match named.get_mut(&tab) {
            Some(known) if known.name == name => known.before = None,
            // A rename the panel asked for has not shown yet.
            Some(known) if known.before.as_deref() == Some(name) => {}
            // Someone else named the tab: it is theirs.
            Some(_) => {
                named.remove(&tab);
            }
            None => {}
        }
        let [agent] = agents_in_tab.as_slice() else {
            continue;
        };
        let want = crate::panes::agent_tab_name(agent);
        if want.is_empty() {
            continue;
        }
        let known = named.get(&tab).map(|known| known.name.as_str());
        if name == want {
            named.entry(tab).or_insert(TabName {
                name: want,
                before: None,
            });
        } else if known == Some(want.as_str()) {
            // Asked for already.
        } else if known.is_some() || crate::panes::is_default_tab_name(name) {
            named.insert(
                tab,
                TabName {
                    name: want.clone(),
                    before: Some(name.to_string()),
                },
            );
            renames.push((tab, want));
        }
    }
    renames
}

/// The name that makes a pane's frame show no title. An empty name would
/// give the pane its own title back.
pub const BLANK_TITLE: &str = " ";

pub const CTX_HOST_INFO: &str = "host-info";
/// Prints the short host name, then the name of the user's shell.
pub const HOST_INFO_SCRIPT: &str = "hostname -s 2>/dev/null || hostname; basename \"${SHELL:-sh}\"";

/// The two lines of `HOST_INFO_SCRIPT`.
pub fn parse_host_info(stdout: &str) -> Option<(String, String)> {
    let mut lines = stdout.lines().map(str::trim).filter(|l| !l.is_empty());
    Some((lines.next()?.to_string(), lines.next()?.to_string()))
}

/// Undo the panel's own names in a snapshot, so that everything after sees
/// each pane's real title: a pane showing the name the panel gave it gets
/// the title it had before, and any other title is remembered as the
/// pane's own. `titles` is kept to the panes that still exist.
pub fn restore_titles(snapshot: &mut GuestSnapshot, titles: &mut BTreeMap<u32, (String, String)>) {
    titles.retain(|id, _| snapshot.panes.iter().any(|p| p.id == *id));
    for pane in &mut snapshot.panes {
        match titles.get(&pane.id) {
            Some((own, name)) if !name.is_empty() && pane.title == *name => {
                pane.title = own.clone();
            }
            // The rename has not arrived yet: nothing to undo or to learn.
            Some((own, _)) if pane.title == *own => {}
            _ => {
                titles.insert(pane.id, (pane.title.clone(), String::new()));
            }
        }
    }
}

/// The panes whose name is not the wanted one yet, as `(pane, name)`, after
/// `restore_titles`. Each is recorded as given, so it is asked for once.
pub fn title_renames(
    snapshot: &GuestSnapshot,
    titles: &mut BTreeMap<u32, (String, String)>,
    wanted: impl Fn(&ScanPane) -> String,
) -> Vec<(u32, String)> {
    let mut renames = Vec::new();
    for pane in &snapshot.panes {
        let want = wanted(pane);
        if let Some((_, name)) = titles.get_mut(&pane.id) {
            if *name != want {
                *name = want.clone();
                renames.push((pane.id, want));
            }
        }
    }
    renames
}

/// A pane's name: the machine, then what runs in it. For an agent that is
/// its provider; for a pane started with a command, the command's name;
/// otherwise the shell.
pub fn pane_name(pane: &ScanPane, agent: Option<&Agent>, host: &str, shell: &str) -> String {
    let what = match agent {
        Some(agent) => agent.provider_label(),
        None => pane
            .terminal_command
            .as_deref()
            .and_then(|command| command.split_whitespace().next())
            .map(|word| {
                let word = word.trim_matches(|c| c == '"' || c == '\'');
                word.rsplit('/').next().unwrap_or(word).to_string()
            })
            .filter(|word| !word.is_empty())
            .unwrap_or_else(|| shell.to_string()),
    };
    format!("{host} \u{b7} {what}")
}

pub fn rename_pane_args(pane: u32, name: &str) -> Vec<String> {
    strings(&[
        "action",
        "rename-pane",
        "--pane-id",
        &format!("terminal_{pane}"),
        "--",
        name,
    ])
}

pub fn rename_tab_args(tab: u32, name: &str) -> Vec<String> {
    strings(&[
        "action",
        "rename-tab",
        "--tab-id",
        &tab.to_string(),
        "--",
        name,
    ])
}

/// The `terminal_N` token `zellij run` prints for the pane it opened.
pub fn opened_pane_id(stdout: &str) -> Option<u32> {
    stdout
        .split_whitespace()
        .find_map(|t| t.strip_prefix("terminal_").and_then(|d| d.parse().ok()))
}

pub fn agent_cwd(agent: &Agent) -> Option<String> {
    let path = agent.workspace.as_ref()?.path.trim();
    (!path.is_empty()).then(|| path.to_string())
}

/// An agent's live pane in the inner session: launched pane, a command that
/// names it, then a binding to exactly that session.
pub fn guest_agent_pane(
    agent: &Agent,
    snapshot: &GuestSnapshot,
    launched: &BTreeMap<String, LaunchedPane>,
    guest_session: &str,
    noc_bin: &str,
) -> Option<u32> {
    if let Some(l) = launched.get(&agent.id) {
        if snapshot.panes.iter().any(|p| p.id == l.id && !p.exited) {
            return Some(l.id);
        }
    }
    let one = std::slice::from_ref(agent);
    if let Some(pane) = snapshot
        .panes
        .iter()
        .find(|p| !p.exited && agent_named_by_pane_command(one, p, noc_bin).is_some())
    {
        return Some(pane.id);
    }
    agent.zellij_bindings().into_iter().find_map(|b| {
        if b.session != guest_session || guest_session.is_empty() {
            return None;
        }
        let id = parse_terminal_pane_id(&b.pane_id)?;
        snapshot
            .panes
            .iter()
            .any(|p| p.id == id && !p.exited)
            .then_some(id)
    })
}

fn strings(args: &[&str]) -> Vec<String> {
    args.iter().map(|s| s.to_string()).collect()
}

pub fn focus_pane_args(id: u32) -> Vec<String> {
    strings(&["action", "focus-pane-id", &format!("terminal_{id}")])
}

pub fn new_tab_args(name: &str, cwd: Option<&str>, noc_bin: &str, agent_id: &str) -> Vec<String> {
    let mut args = strings(&["action", "new-tab", "--name", name]);
    if let Some(cwd) = cwd {
        args.extend(strings(&["--cwd", cwd]));
    }
    args.extend(strings(&["--", noc_bin, "attach", agent_id]));
    args
}

pub fn run_pane_args(
    title: &str,
    cwd: Option<&str>,
    stacked: bool,
    noc_bin: &str,
    agent_id: &str,
) -> Vec<String> {
    let mut args = strings(&["run", "--name", &format!("NOC: {title}")]);
    if let Some(cwd) = cwd {
        args.extend(strings(&["--cwd", cwd]));
    }
    if stacked {
        args.push("--stacked".to_string());
    }
    args.extend(strings(&["--", noc_bin, "attach", agent_id]));
    args
}

pub fn shell_args(dir: &str) -> Vec<String> {
    strings(&["action", "new-pane", "--cwd", dir])
}

/// The argv for `run_command` running a CLI action in the inner session and
/// then moving the outer keyboard back to the guest pane.
pub fn guest_run_command(guest: &str, host: &str, args: &[String]) -> Vec<String> {
    let mut command = strings(&["/bin/sh", "-c", RUN_SCRIPT, "sh", guest, host]);
    command.extend(args.iter().cloned());
    command
}

fn run(command: &[String], context: BTreeMap<String, String>) {
    let argv: Vec<&str> = command.iter().map(String::as_str).collect();
    run_command(&argv, context);
}

// ---- state methods ----

impl State {
    /// Ask the inner session's relay to watch for this panel: every
    /// `GUEST_WATCH_SECS`, or every `GUEST_RETRY_SECS` while there is no
    /// usable answer. Returns whether the panel needs a redraw.
    pub fn watch_guest(&mut self) -> bool {
        let now = now_secs();
        // A request that never came back is given up on: `zellij pipe`
        // waits for as long as the plugin it started does not answer.
        let patience = if self.guest.switching {
            SWITCH_WATCH_LOST_SECS
        } else {
            GUEST_WATCH_LOST_SECS
        };
        let lost = self.guest.last_watch.is_none_or(|t| now - t > patience);
        if !self.permissions_granted || (self.guest.in_flight && !lost) {
            return false;
        }
        if self.guest.settle_until.is_some_and(|t| now < t) {
            // A session that is being created has no tab yet for its relay.
            return false;
        }
        // The relay is this plugin, so the panel must know where it was
        // loaded from, and the relay must know which session to answer.
        let (Some(url), Some(host)) = (self.panel_url.clone(), self.session_name.clone()) else {
            return false;
        };
        let mut redraw = false;
        if self.guest.switching {
            let waited = self.guest.switched_at.map_or(0.0, |t| now - t);
            if waited > SWITCH_SECS {
                // The move did not happen: stop waiting, and show the session
                // the client is in.
                self.guest.switching = false;
                if self.guest.pending_open.take().is_some() {
                    self.app.set_notice(format!(
                        "session {:?} did not open",
                        team_label(&self.guest.session)
                    ));
                    redraw = true;
                }
                redraw |= self.follow_shown_session();
            } else if !self.guest.landed && waited < SWITCH_LANDING_SECS {
                // The client is on its way. A relay started in a session that
                // is still being created may never answer.
                return redraw;
            }
        }
        let healthy = self.guest.snapshot.is_some() && self.guest.failing_since.is_none();
        let every = if self.guest.switching {
            // Until the new session answers with its client.
            SWITCH_WATCH_SECS
        } else if healthy {
            GUEST_WATCH_SECS
        } else {
            GUEST_RETRY_SECS
        };
        if self.guest.last_watch.is_some_and(|t| now - t < every) {
            return redraw;
        }
        // The last request reached the inner session and nothing came back.
        let unanswered = !self.guest.switching
            && self.guest.watch_ok
            && self
                .guest
                .last_watch
                .is_some_and(|asked| self.guest.last_push.is_none_or(|push| push < asked));
        self.guest.in_flight = true;
        self.guest.last_watch = Some(now);
        run(
            &watch_command(&self.guest.session, &url, &host),
            ctx(CTX_WATCH),
        );
        redraw | (unanswered && self.on_guest_unreadable("is not answering"))
    }

    /// The watch request ran: it fails when the session is not there.
    pub fn on_guest_watch(&mut self, exit: Option<i32>) -> bool {
        self.guest.in_flight = false;
        self.guest.watch_ok = exit == Some(0);
        if self.guest.watch_ok || self.guest.switching {
            // A session being created is not there yet.
            return false;
        }
        self.on_guest_unreadable("is not running")
    }

    /// Ask Zellij which session is attached in the guest pane right now,
    /// and act on it (`follow_shown_session`). A `PaneUpdate` says so too, but
    /// Zellij does not send one when only that changes.
    pub fn read_shown_session(&mut self) -> bool {
        let Some(guest) = self.guest_pane() else {
            return false;
        };
        if let Some(info) = get_pane_info(PaneId::Terminal(guest.id)) {
            if let Some(pane) = self
                .panes
                .iter_mut()
                .find(|p| !p.is_plugin && p.id == guest.id)
            {
                pane.nested_session = info.nested_session_name.clone();
            }
            match info.nested_session_name {
                Some(name) => self.guest.shown = Some(name),
                // The client that was in the guest pane has exited.
                None => {
                    if let Some(left) = self.guest.shown.take() {
                        self.guest.client_exit = Some((left, now_secs()));
                    }
                }
            }
        }
        self.settle_client_exit();
        self.follow_shown_session()
    }

    /// The guest pane's client exited a while ago and none has taken its
    /// place yet. If the session it was in is still running, the user detached
    /// from it, and one detach leaves both sessions: the host's client is
    /// detached too. If the session is gone, it ended, and the guest pane's
    /// loop is about to show the inner session: nothing to do.
    fn settle_client_exit(&mut self) {
        let Some((left, at)) = self.guest.client_exit.clone() else {
            return;
        };
        if self.guest.shown.is_some() {
            // A client is there: it only moved, or it is attached again.
            self.guest.client_exit = None;
            return;
        }
        if now_secs() - at < CLIENT_EXIT_SECS {
            return;
        }
        self.guest.client_exit = None;
        let running =
            get_session_list().is_ok_and(|list| list.live_sessions.iter().any(|s| s.name == left));
        if running && is_inner_session(&self.config.guest_session, &left) {
            detach();
        }
    }

    /// While the client is on its way to another session: look for it, and
    /// look again shortly.
    pub fn on_switch_poll(&mut self) -> bool {
        if !self.guest.switching {
            return false;
        }
        let redraw = if self.guest.landed {
            // It is there: ask the session's relay until it says so too.
            self.watch_guest()
        } else {
            self.read_shown_session()
        };
        if self.guest.switching {
            set_timeout(SWITCH_POLL_SECS);
        }
        redraw
    }

    /// Zellij says which session is attached in the guest pane. While a
    /// move is under way that tells the panel the client has landed, so the
    /// new session can be read at once. Otherwise, when it is a session other
    /// than the one the panel took to be on show (that session ended and
    /// Zellij moved the client back, or the user moved it), the panel shows
    /// that one.
    pub fn follow_shown_session(&mut self) -> bool {
        let Some(shown) = self.guest_pane().and_then(|pane| pane.nested_session) else {
            return false;
        };
        if !is_inner_session(&self.config.guest_session, &shown) {
            return false;
        }
        if self.guest.switching {
            if shown == self.guest.session && !self.guest.landed {
                self.guest.landed = true;
                self.guest.last_watch = None;
                if self.guest.fresh {
                    self.guest.settle_until = Some(now_secs() + SWITCH_SETTLE_SECS);
                }
                self.watch_guest();
            }
            return false;
        }
        if shown == self.guest.session {
            return false;
        }
        self.adopt_session(shown);
        self.watch_guest();
        true
    }

    /// Make `target` the session on show in the panel's books: what it knew of
    /// the session it leaves is put away, and what it knew of the new one is
    /// taken out again. The session is read anew before anything is done there.
    fn adopt_session(&mut self, target: String) {
        let left = InnerSession {
            snapshot: self.guest.snapshot.take(),
            launched: std::mem::take(&mut self.guest.launched),
            named_tabs: std::mem::take(&mut self.guest.named_tabs),
            titles: std::mem::take(&mut self.guest.titles),
        };
        let previous = std::mem::replace(&mut self.guest.session, target);
        self.guest.sessions.insert(previous, left);
        if let Some(known) = self.guest.sessions.remove(&self.guest.session) {
            self.guest.launched = known.launched;
            self.guest.named_tabs = known.named_tabs;
            self.guest.titles = known.titles;
        }
        // A request to the session it leaves no longer holds the next one up.
        self.guest.in_flight = false;
        self.guest.last_watch = None;
        self.guest.last_push = None;
        self.guest.watch_ok = false;
        self.guest.client_seen = false;
        self.guest.failing_since = None;
        self.guest.landed = false;
        self.guest.fresh = false;
        self.guest.settle_until = None;
        self.guest.spare_tab = None;
        self.app.guest_problem = None;
        self.followed_agent = None;
        // The Sessions section marks the new one at once, and is read anew
        // on the next tick: the session may have just been created.
        let mut names: Vec<String> = self.app.sessions.iter().map(|s| s.name.clone()).collect();
        if !names.contains(&self.guest.session) {
            names.push(self.guest.session.clone());
        }
        self.app.set_sessions(&names, &self.guest.session);
        self.guest.sessions_read = None;
    }

    /// Show another session in the guest pane: ask the relay of the session on
    /// show to move its client there, which creates the session when it is not
    /// running. False, with a notice, when that cannot be asked yet.
    pub fn show_session(&mut self, target: &str) -> bool {
        if target == self.guest.session {
            return true;
        }
        let (true, Some(url)) = (self.permissions_granted, self.panel_url.clone()) else {
            self.app
                .set_notice("the plugin location is not known yet; try again");
            return false;
        };
        run(
            &switch_command(&self.guest.session, &url, target, &self.config.team_layout),
            ctx(CTX_SWITCH),
        );
        let running = get_session_list()
            .is_ok_and(|list| list.live_sessions.iter().any(|s| s.name == target));
        self.adopt_session(target.to_string());
        self.guest.fresh = !running;
        self.guest.switching = true;
        self.guest.switched_at = Some(now_secs());
        self.guest.pending_open = None;
        set_timeout(SWITCH_POLL_SECS);
        true
    }

    /// The request to move ran: it fails when the session the panel took to be
    /// on show is not running.
    pub fn on_guest_switch(&mut self, exit: Option<i32>, stderr: &[u8]) -> bool {
        if exit == Some(0) {
            return false;
        }
        self.guest.switching = false;
        self.guest.pending_open = None;
        let detail = compact_error(&String::from_utf8_lossy(stderr));
        self.app
            .set_notice(format!("could not change sessions: {detail}"));
        self.follow_shown_session();
        true
    }

    /// `o`: show the session of the selected team, or of the selected agent,
    /// and give it the keyboard.
    pub fn show_selected_team(&mut self) {
        let Some(team) = self.app.selected_team() else {
            self.app.set_notice("no team or agent is selected");
            return;
        };
        let session = team_session(&self.config.guest_session, &team);
        self.show_and_focus(&session);
    }

    /// Show a session and give it the keyboard.
    pub fn show_and_focus(&mut self, session: &str) {
        if self.show_session(session) {
            self.app.focus_agents();
            self.list_sessions();
            if self.panel_has_keyboard() && !self.host_action(&["focus-previous-pane"]) {
                focus_previous_pane();
            }
        }
    }

    /// Read which inner sessions are running, for the Sessions section:
    /// the pair's own among the sessions Zellij lists, and always the one
    /// on show, which Zellij may not list yet. Returns whether they changed.
    pub fn list_sessions(&mut self) -> bool {
        self.guest.sessions_read = Some(now_secs());
        let mut names: Vec<String> = get_session_list()
            .map(|list| {
                list.live_sessions
                    .iter()
                    .map(|s| s.name.clone())
                    .filter(|name| is_inner_session(&self.config.guest_session, name))
                    .collect()
            })
            .unwrap_or_default();
        if !names.contains(&self.guest.session) {
            names.push(self.guest.session.clone());
        }
        self.app.set_sessions(&names, &self.guest.session)
    }

    /// On a tick: read the running sessions when that is due.
    pub fn list_sessions_if_due(&mut self) -> bool {
        let due = self
            .guest
            .sessions_read
            .is_none_or(|t| now_secs() - t >= SESSIONS_READ_SECS);
        due && self.permissions_granted && self.list_sessions()
    }

    /// `x` in the Sessions section, confirmed: end a team's session.
    pub fn end_session(&mut self, name: &str) {
        if !self.permissions_granted
            || name == self.config.guest_session
            || !is_inner_session(&self.config.guest_session, name)
        {
            return;
        }
        let mut context = ctx(CTX_END);
        context.insert(crate::KEY_TITLE.to_string(), name.to_string());
        run_command(&["zellij", "kill-session", name], context);
    }

    pub fn on_end_session(
        &mut self,
        exit: Option<i32>,
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        let name = context
            .get(crate::KEY_TITLE)
            .map(String::as_str)
            .unwrap_or_default();
        let label = self.app.session_label(name).to_string();
        if exit == Some(0) {
            self.app.set_notice(format!("ended {label}"));
            self.guest.sessions.remove(name);
        } else {
            let detail = compact_error(&String::from_utf8_lossy(stderr));
            self.app
                .set_notice(format!("could not end {label}: {detail}"));
        }
        self.list_sessions();
        true
    }

    /// The relay sent a snapshot.
    pub fn on_guest_push(&mut self, payload: &str) -> bool {
        let Some(mut snapshot) = parse_guest_snapshot(payload) else {
            return false;
        };
        if snapshot.session != self.guest.session {
            // From the relay of a session that is no longer on show.
            return false;
        }
        let session = self.guest.session.clone();
        self.guest.last_push = Some(now_secs());
        if self.config.pane_titles != PaneTitles::Show {
            restore_titles(&mut snapshot, &mut self.guest.titles);
            let renames = match (&self.config.pane_titles, &self.guest.host) {
                (PaneTitles::Hide, _) => title_renames(&snapshot, &mut self.guest.titles, |_| {
                    BLANK_TITLE.to_string()
                }),
                (PaneTitles::Name, Some((host, shell))) => {
                    let agents = &self.app.agents;
                    let launched = &self.guest.launched;
                    let config = &self.config;
                    title_renames(&snapshot, &mut self.guest.titles, |pane| {
                        let agent = crate::panes::agent_in_pane(
                            agents,
                            pane,
                            launched,
                            &config.noc_bin,
                            Some(&session),
                        );
                        pane_name(pane, agent, host, shell)
                    })
                }
                // The host name is not known yet.
                _ => Vec::new(),
            };
            for (pane, name) in renames {
                run(
                    &guest_run_command(&session, "", &rename_pane_args(pane, &name)),
                    ctx(CTX_HOST_ACTION),
                );
            }
        }
        self.guest.failing_since = None;
        let mut redraw = self.app.guest_problem.take().is_some();
        self.guest
            .launched
            .retain(|_, l| snapshot.panes.iter().any(|p| p.id == l.id && !p.exited));
        let attached = snapshot.clients > 0;
        let newly_attached = attached && !self.guest.client_seen;
        self.guest.client_seen = attached;
        // The client has arrived in the session it was moved to.
        let arrived = attached && self.guest.switching;
        if attached {
            self.guest.switching = false;
        }
        for (tab, name) in tab_renames(
            &self.app.agents,
            &snapshot,
            &self.guest.launched,
            &session,
            &self.config.noc_bin,
            &mut self.guest.named_tabs,
        ) {
            // No host session name: the keyboard stays where it is.
            run(
                &guest_run_command(&session, "", &rename_tab_args(tab, &name)),
                ctx(CTX_HOST_ACTION),
            );
        }
        if arrived && self.guest.pending_open.is_some() && session != self.config.guest_session {
            // A team's session with nothing in it but the tab it was created
            // with: that tab is not wanted once the agent has its own.
            self.guest.spare_tab = untouched_tab(&snapshot);
        } else if let Some(spare) = self.guest.spare_tab {
            if let Some(close) = spare_tab_verdict(&snapshot, spare) {
                self.guest.spare_tab = None;
                if close {
                    run(
                        &guest_run_command(&session, "", &close_tab_args(spare)),
                        ctx(CTX_HOST_ACTION),
                    );
                }
            }
        }
        self.guest.snapshot = Some(snapshot);
        if newly_attached {
            match (self.guest_has_keyboard(), arrived) {
                // The keyboard is in the panel. A session the client was just
                // moved to has not heard so: it still wears its home theme.
                (Some(false), true) => {
                    let bytes = keyboard_signal(&self.config, false);
                    self.write_to_guest(&bytes);
                }
                (Some(false), false) => {}
                // The keyboard is in the guest pane: tell the client so.
                (_, true) => {
                    let bytes = keyboard_signal(&self.config, true);
                    self.write_to_guest(&bytes);
                }
                (_, false) => self.write_to_guest(&FOCUS_FRAME),
            }
        }
        redraw |= self.follow_guest_focus();
        if attached {
            if let Some(agent) = self
                .guest
                .pending_open
                .take()
                .and_then(|id| self.app.agents.iter().find(|a| a.id == id).cloned())
            {
                self.guest_open_agent(&agent);
                redraw = true;
            }
        }
        redraw
    }

    fn on_guest_unreadable(&mut self, what: &str) -> bool {
        self.guest.client_seen = false;
        let now = now_secs();
        let since = *self.guest.failing_since.get_or_insert(now);
        if now - since < GUEST_MISSING_SECS || self.app.guest_problem.is_some() {
            return false;
        }
        self.app.guest_problem = Some(format!("inner session {:?} {what}", self.guest.session));
        true
    }

    /// The selection follows the inner session's focused pane, except while
    /// the panel itself has the keyboard.
    fn follow_guest_focus(&mut self) -> bool {
        if self.panel_has_keyboard() {
            self.followed_agent = None;
            return false;
        }
        let Some(snapshot) = &self.guest.snapshot else {
            return false;
        };
        let Some(pane) = snapshot
            .focused
            .and_then(|id| snapshot.panes.iter().find(|p| p.id == id))
        else {
            return false;
        };
        let Some(agent) = crate::panes::agent_in_pane(
            &self.app.agents,
            pane,
            &self.guest.launched,
            &self.config.noc_bin,
            Some(&self.guest.session),
        ) else {
            return false;
        };
        if self.followed_agent.as_deref() == Some(agent.id.as_str()) {
            return false;
        }
        let id = agent.id.clone();
        self.followed_agent = Some(id.clone());
        self.app.select_agent(&id)
    }

    pub fn guest_run(&mut self, args: Vec<String>, agent_id: Option<&str>) {
        let host = self.session_name.clone().unwrap_or_default();
        let mut context = ctx(CTX_OPEN);
        if let Some(id) = agent_id {
            context.insert(KEY_GUEST_AGENT.to_string(), id.to_string());
        }
        run(
            &guest_run_command(&self.guest.session, &host, &args),
            context,
        );
    }

    pub fn on_guest_open(
        &mut self,
        exit: Option<i32>,
        stdout: &[u8],
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        self.guest.last_watch = None;
        if exit != Some(0) {
            let detail = compact_error(&String::from_utf8_lossy(stderr));
            self.app.set_notice(format!(
                "the inner Zellij session did not open it: {detail}"
            ));
            return true;
        }
        if let (Some(agent), Some(pane)) = (
            context.get(KEY_GUEST_AGENT),
            opened_pane_id(&String::from_utf8_lossy(stdout)),
        ) {
            self.guest.launched.insert(
                agent.clone(),
                LaunchedPane {
                    id: pane,
                    observed: false,
                },
            );
        }
        false
    }

    /// `zellij --session <host session> action <args>`; false when the
    /// session name is not known yet (the caller falls back to the plugin API).
    pub fn host_action(&mut self, args: &[&str]) -> bool {
        let Some(session) = self.session_name.clone() else {
            return false;
        };
        let mut command = vec!["zellij", "--session", session.as_str(), "action"];
        command.extend_from_slice(args);
        run_command(&command, ctx(CTX_HOST_ACTION));
        true
    }

    /// Type bytes into the guest pane, whichever pane has the keyboard.
    pub fn write_to_guest(&mut self, bytes: &[u8]) {
        if bytes.is_empty() || !self.permissions_granted {
            return;
        }
        let Some(guest) = self.guest_pane() else {
            return;
        };
        let pane = format!("terminal_{}", guest.id);
        let values: Vec<String> = bytes.iter().map(u8::to_string).collect();
        let mut args = vec!["write", "--pane-id", pane.as_str()];
        args.extend(values.iter().map(String::as_str));
        self.host_action(&args);
    }

    pub fn guest_open_agent(&mut self, agent: &Agent) {
        let target = agent_session(
            agent,
            &self.config.guest_session,
            &self.guest.session,
            self.guest.snapshot.as_ref(),
            &self.guest.launched,
            &self.guest.sessions,
            &self.config.noc_bin,
        );
        if target != self.guest.session && !self.show_session(&target) {
            return;
        }
        if self.guest.switching {
            // The session is read before anything is focused or opened there.
            self.guest.pending_open = Some(agent.id.clone());
            return;
        }
        let guest = self.guest.session.clone();
        let found = self.guest.snapshot.as_ref().and_then(|s| {
            guest_agent_pane(agent, s, &self.guest.launched, &guest, &self.config.noc_bin)
        });
        if let Some(id) = found {
            self.guest_run(focus_pane_args(id), None);
            return;
        }
        if agent.native_session_id.trim().is_empty() {
            self.app
                .set_notice("this agent has no resumable native session ID");
            return;
        }
        if let Some(other) = self.other_running_session_for(agent) {
            self.app.set_notice(format!(
                "already open in Zellij session {other:?}; press p to open a second copy here"
            ));
            return;
        }
        let args = new_tab_args(
            &crate::panes::agent_tab_name(agent),
            agent_cwd(agent).as_deref(),
            &self.config.noc_bin,
            &agent.id,
        );
        self.guest_run(args, None);
    }

    pub fn guest_open_agent_in_pane(&mut self, agent: &Agent, stacked: bool) {
        let args = run_pane_args(
            &agent.title(),
            agent_cwd(agent).as_deref(),
            stacked,
            &self.config.noc_bin,
            &agent.id,
        );
        self.guest_run(args, Some(&agent.id));
    }

    pub fn guest_open_shell(&mut self, dir: &str) {
        self.guest_run(shell_args(dir), None);
    }

    /// Relay role: start watching for a panel, or switch the inner
    /// session's theme live.
    pub fn relay_pipe(&mut self, message: &PipeMessage) {
        if message.name != PIPE_RELAY {
            return;
        }
        let payload = message.payload.as_deref().unwrap_or_default();
        if let Some((host, session)) = watch_host(payload) {
            self.relay.host = Some(host);
            self.relay.session = session;
            self.relay.watched_at = now_secs();
            // Every request is answered, changed or not.
            self.relay.sent = None;
            list_clients();
            return;
        }
        if let Some((session, layout)) = switch_request(payload) {
            // Moves this session's client. A session that is not running is
            // created from the layout; one that is, is left as it is.
            switch_session_with_layout(
                Some(&session),
                LayoutInfo::File(layout, Default::default()),
                None,
            );
            return;
        }
        if let Some(name) = relay_theme(payload, &self.config) {
            reconfigure(format!("theme \"{name}\""), false);
        }
    }

    /// Relay role: Zellij's events in the inner session.
    pub fn update_relay(&mut self, event: Event) {
        match event {
            Event::PermissionRequestResult(_) => hide_self(),
            Event::PaneUpdate(manifest) => {
                self.relay.panes = manifest_panes(&manifest);
                self.relay_changed();
            }
            Event::TabUpdate(tabs) => {
                self.relay.tabs = tabs;
                self.relay_changed();
            }
            // What runs in a pane changed: a command was typed, or ended.
            // Zellij says so a moment after the `PaneUpdate` for the same
            // key press, by which time the new command is certain to show.
            Event::CommandChanged(..) => self.relay_changed(),
            Event::ListClients(clients) => self.relay_push(&clients),
            Event::RunCommandResult(_, _, _, context)
                if context.get(KEY).map(String::as_str) == Some(CTX_RELAY_PUSH) =>
            {
                self.relay.in_flight = false;
                if let Some(snapshot) = self.relay.pending.take() {
                    self.relay_send(snapshot);
                }
            }
            _ => {}
        }
    }

    /// Something changed: ask who is attached and where, which comes back
    /// as `ListClients` and completes the snapshot.
    fn relay_changed(&mut self) {
        if self.relay_host().is_some() {
            list_clients();
        }
    }

    /// The session to send to, while its panel keeps asking.
    fn relay_host(&self) -> Option<String> {
        let host = self.relay.host.clone()?;
        (now_secs() - self.relay.watched_at < RELAY_WATCH_SECS).then_some(host)
    }

    fn relay_push(&mut self, clients: &[ClientInfo]) {
        if self.relay_host().is_none() {
            return;
        }
        // Zellij's pane records do not say what runs in a pane; it answers
        // when asked, pane by pane.
        let mut panes = self.relay.panes.clone();
        for pane in panes.iter_mut().filter(|p| !p.is_plugin && !p.exited) {
            if let Ok(command) = get_pane_running_command(PaneId::Terminal(pane.id)) {
                pane.pane_command = Some(command.join(" "));
            }
        }
        let snapshot = relay_snapshot(&self.relay.session, &panes, &self.relay.tabs, clients);
        let Ok(snapshot) = serde_json::to_string(&snapshot) else {
            return;
        };
        if self.relay.sent.as_deref() == Some(snapshot.as_str()) {
            return;
        }
        self.relay_send(snapshot);
    }

    /// One send at a time; the newest snapshot waits for it.
    fn relay_send(&mut self, snapshot: String) {
        if self.relay.in_flight {
            self.relay.pending = Some(snapshot);
            return;
        }
        let Some(host) = self.relay_host() else {
            return;
        };
        run(&push_command(&host, &snapshot), ctx(CTX_RELAY_PUSH));
        self.relay.in_flight = true;
        self.relay.sent = Some(snapshot);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::parse_agent;

    /// A snapshot as the relay sends it.
    fn pushed() -> &'static str {
        r#"{"clients":1,"focused":3,"panes":[
            {"id":3,"is_plugin":false,"is_focused":true,"title":"vim","terminal_command":null},
            {"id":4,"is_plugin":false,"title":"x","terminal_command":"noc attach a1","exited":false},
            {"id":5,"is_plugin":false,"terminal_command":"noc attach a2","exited":true},
            {"id":9,"is_plugin":true,"plugin_url":"zellij:status-bar"}]}"#
    }

    #[test]
    fn pane_names_are_given_once_and_the_real_titles_remembered() {
        let pane = |id: u32, title: &str| ScanPane {
            id,
            title: title.to_string(),
            ..ScanPane::default()
        };
        let snapshot = |panes: Vec<ScanPane>| GuestSnapshot {
            session: "inner".to_string(),
            clients: 1,
            focused: None,
            panes,
        };
        let blank = |_: &ScanPane| BLANK_TITLE.to_string();
        let mut titles = BTreeMap::new();
        // First sight: both are renamed, and their titles kept.
        let mut first = snapshot(vec![pane(1, "\u{2733} Auth migration"), pane(2, "zsh")]);
        restore_titles(&mut first, &mut titles);
        assert_eq!(
            title_renames(&first, &mut titles, blank),
            [(1, " ".to_string()), (2, " ".to_string())]
        );
        // The rename has not arrived: it is not asked for again.
        let mut pending = snapshot(vec![pane(1, "\u{2733} Auth migration"), pane(2, "zsh")]);
        restore_titles(&mut pending, &mut titles);
        assert!(title_renames(&pending, &mut titles, blank).is_empty());
        // Renamed: nothing to do, and the panel still sees the real titles.
        let mut second = snapshot(vec![pane(1, " "), pane(2, " "), pane(3, "")]);
        restore_titles(&mut second, &mut titles);
        assert_eq!(second.panes[0].title, "\u{2733} Auth migration");
        assert_eq!(second.panes[1].title, "zsh");
        assert_eq!(
            title_renames(&second, &mut titles, blank),
            [(3, " ".to_string())]
        );
        // The wanted name changes: the pane is renamed again, once.
        let named = |p: &ScanPane| format!("box \u{b7} {}", p.id);
        let mut third = snapshot(vec![pane(2, " ")]);
        restore_titles(&mut third, &mut titles);
        assert_eq!(
            title_renames(&third, &mut titles, named),
            [(2, "box \u{b7} 2".to_string())]
        );
        let mut fourth = snapshot(vec![pane(2, "box \u{b7} 2")]);
        restore_titles(&mut fourth, &mut titles);
        assert_eq!(fourth.panes[0].title, "zsh");
        assert!(title_renames(&fourth, &mut titles, named).is_empty());
        // A title from somewhere else becomes the pane's own, and is renamed.
        let mut fifth = snapshot(vec![pane(2, "mine")]);
        restore_titles(&mut fifth, &mut titles);
        assert_eq!(fifth.panes[0].title, "mine");
        assert_eq!(title_renames(&fifth, &mut titles, named).len(), 1);
        // A pane that is gone is forgotten.
        assert_eq!(titles.keys().copied().collect::<Vec<_>>(), [2]);
        assert_eq!(
            rename_pane_args(4, "-x"),
            [
                "action",
                "rename-pane",
                "--pane-id",
                "terminal_4",
                "--",
                "-x"
            ]
        );
    }

    #[test]
    fn a_pane_is_named_for_its_machine_and_what_runs_in_it() {
        let claude = parse_agent(r#"{"id":"a1","provider":"claude"}"#).unwrap();
        let command = |c: Option<&str>| ScanPane {
            terminal_command: c.map(str::to_string),
            ..ScanPane::default()
        };
        let name = |p: &ScanPane, a: Option<&Agent>| pane_name(p, a, "ohm-2", "zsh");
        // An agent's pane says which provider, whatever started it.
        assert_eq!(
            name(&command(Some("noc attach a1")), Some(&claude)),
            "ohm-2 \u{b7} claude"
        );
        // A pane started with a command says the command, without its path.
        assert_eq!(
            name(&command(Some("/usr/bin/vim notes.md")), None),
            "ohm-2 \u{b7} vim"
        );
        assert_eq!(name(&command(Some("'htop'")), None), "ohm-2 \u{b7} htop");
        // A plain pane says the shell.
        assert_eq!(name(&command(None), None), "ohm-2 \u{b7} zsh");
        assert_eq!(name(&command(Some("  ")), None), "ohm-2 \u{b7} zsh");

        assert_eq!(
            parse_host_info("ohm-2\nzsh\n"),
            Some(("ohm-2".to_string(), "zsh".to_string()))
        );
        assert_eq!(parse_host_info("ohm-2\n"), None);
        assert_eq!(parse_host_info(""), None);
    }

    #[test]
    fn tab_names_follow_the_agent_unless_the_user_named_the_tab() {
        let agent = |id: &str, name: &str| {
            parse_agent(&format!(r#"{{"id":"{id}","name":"{name}"}}"#)).unwrap()
        };
        let snapshot = |tabs: &[(u32, &str, &str)]| GuestSnapshot {
            session: "inner".to_string(),
            clients: 1,
            focused: None,
            panes: tabs
                .iter()
                .enumerate()
                .map(|(i, (tab, name, command))| ScanPane {
                    id: i as u32,
                    tab_id: Some(*tab),
                    tab_name: name.to_string(),
                    terminal_command: (!command.is_empty()).then(|| command.to_string()),
                    ..ScanPane::default()
                })
                .collect(),
        };
        let launched = BTreeMap::new();
        let mut named = BTreeMap::new();
        let mut renames = |agents: &[Agent], tabs: &[(u32, &str, &str)]| {
            tab_renames(
                agents,
                &snapshot(tabs),
                &launched,
                "inner",
                "noc",
                &mut named,
            )
        };
        let before = [agent("a1", "Old name"), agent("a2", "Other")];
        let after = [agent("a1", "New name"), agent("a2", "Other")];

        // A tab the panel opened carries the agent's name: nothing to do.
        let opened = [(1, "Old name", "noc attach a1"), (2, "Tab #2", "")];
        assert!(renames(&before, &opened).is_empty());
        // The agent is renamed: its tab follows. A tab with no agent does not.
        assert_eq!(renames(&after, &opened), [(1, "New name".to_string())]);
        // Until Zellij reports the new name the rename is not sent again.
        assert!(renames(&after, &opened).is_empty());
        assert!(renames(&after, &[(1, "New name", "noc attach a1")]).is_empty());

        // A tab Zellij named is claimed by the one agent in it, shell or not.
        assert_eq!(
            renames(&after, &[(3, "Tab #3", "noc attach a2"), (3, "Tab #3", "")]),
            [(3, "Other".to_string())]
        );
        // A tab the user named is theirs, even after the agent is renamed.
        let mine = [(4, "my work", "noc attach a1")];
        assert!(renames(&before, &mine).is_empty());
        assert!(renames(&after, &mine).is_empty());
        // The user renames a tab the panel had named: it is theirs from then on.
        assert!(renames(&before, &[(5, "Old name", "noc attach a1")]).is_empty());
        assert!(renames(&after, &[(5, "pinned", "noc attach a1")]).is_empty());
        // Two agents in one tab: neither names it.
        let shared = [
            (6, "Tab #6", "noc attach a1"),
            (6, "Tab #6", "noc attach a2"),
        ];
        assert!(renames(&after, &shared).is_empty());

        // The agent is renamed again before the first rename shows: the tab
        // still follows, and stays the panel's.
        let last = [agent("a1", "Last name")];
        let slow = [(8, "Old name", "noc attach a1")];
        assert!(renames(&before, &slow).is_empty());
        assert_eq!(renames(&after, &slow), [(8, "New name".to_string())]);
        assert_eq!(renames(&last, &slow), [(8, "Last name".to_string())]);
        assert!(renames(&last, &[(8, "Last name", "noc attach a1")]).is_empty());
        assert_eq!(
            renames(&after, &[(8, "Last name", "noc attach a1")]),
            [(8, "New name".to_string())]
        );
        // A tab the panel named goes on to another agent once the first
        // has left it, also with no agent in between.
        let moved = [(9, "Tab #9", "noc attach a1")];
        assert_eq!(renames(&after, &moved), [(9, "New name".to_string())]);
        assert!(renames(&after, &[(9, "New name", "")]).is_empty());
        assert_eq!(
            renames(&after, &[(9, "New name", "noc attach a2")]),
            [(9, "Other".to_string())]
        );
        // A hand-attached agent, known only by the command in its shell.
        let native = [
            parse_agent(r#"{"id":"n1","name":"By hand","native_short_id":"2f5ac2d2"}"#).unwrap(),
        ];
        let by_hand = GuestSnapshot {
            session: "inner".to_string(),
            clients: 1,
            focused: Some(0),
            panes: vec![ScanPane {
                tab_id: Some(10),
                tab_name: "Tab #10".to_string(),
                pane_command: Some("claude attach 2f5ac2d2".to_string()),
                ..ScanPane::default()
            }],
        };
        assert_eq!(
            tab_renames(&native, &by_hand, &launched, "inner", "noc", &mut named),
            [(10, "By hand".to_string())]
        );
        assert_eq!(
            guest_agent_pane(&native[0], &by_hand, &launched, "inner", "noc"),
            Some(0)
        );

        assert_eq!(
            rename_tab_args(7, "-x"),
            ["action", "rename-tab", "--tab-id", "7", "--", "-x"]
        );
        assert!(crate::panes::is_default_tab_name("Tab #12"));
        for name in ["Tab #", "Tab #1a", "tab #1", "Tab 1", ""] {
            assert!(!crate::panes::is_default_tab_name(name), "{name:?}");
        }
    }

    #[test]
    fn parses_the_inner_sessions_terminals_and_focus() {
        let snapshot = parse_guest_snapshot(pushed()).unwrap();
        assert_eq!(snapshot.focused, Some(3));
        assert_eq!(snapshot.clients, 1);
        let ids: Vec<u32> = snapshot.panes.iter().map(|p| p.id).collect();
        assert_eq!(ids, [3, 4, 5], "plugins are dropped");
        assert!(snapshot.panes[2].exited);
        assert_eq!(
            snapshot.panes[1].terminal_command.as_deref(),
            Some("noc attach a1")
        );
        // Missing fields take their defaults; anything else is refused.
        assert_eq!(parse_guest_snapshot("{}"), Some(GuestSnapshot::default()));
        for bad in ["", "not json", "7", r#"{"panes":3}"#] {
            assert_eq!(parse_guest_snapshot(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn the_relay_sends_terminals_with_their_tab_and_the_first_clients_focus() {
        let pane = |id: u32, tab: usize, plugin: bool, command: &str| ScanPane {
            id,
            is_plugin: plugin,
            tab_position: tab,
            pane_command: (!command.is_empty()).then(|| command.to_string()),
            ..ScanPane::default()
        };
        let panes = [
            pane(0, 0, false, ""),
            pane(1, 0, true, ""),
            pane(3, 1, false, "claude attach 2f5ac2d2"),
            pane(4, 2, false, ""),
        ];
        let tab = |position: usize, id: usize, name: &str| TabInfo {
            position,
            tab_id: id,
            name: name.to_string(),
            ..TabInfo::default()
        };
        let tabs = [tab(0, 0, "Tab #1"), tab(1, 7, "work")];
        let client = |pane| ClientInfo::new(1, pane, "zsh".to_string(), true);
        let snapshot = relay_snapshot(
            "inner--auth",
            &panes,
            &tabs,
            &[client(PaneId::Terminal(3)), client(PaneId::Terminal(0))],
        );
        assert_eq!(snapshot.session, "inner--auth");
        assert_eq!(snapshot.clients, 2);
        assert_eq!(snapshot.focused, Some(3), "the first client's pane");
        let seen: Vec<_> = snapshot
            .panes
            .iter()
            .map(|p| (p.id, p.tab_id, p.tab_name.as_str()))
            .collect();
        assert_eq!(
            seen,
            [
                (0, Some(0), "Tab #1"),
                (3, Some(7), "work"),
                // A tab Zellij has not reported yet: no id, so no rename.
                (4, None, ""),
            ]
        );
        // What the relay sends is what the panel reads.
        let sent = serde_json::to_string(&snapshot).unwrap();
        assert_eq!(parse_guest_snapshot(&sent), Some(snapshot));
        // The keyboard in a plugin pane, or nobody attached: no focus.
        assert_eq!(
            relay_snapshot("inner", &panes, &tabs, &[client(PaneId::Plugin(1))]).focused,
            None
        );
        let nobody = relay_snapshot("inner", &panes, &tabs, &[]);
        assert_eq!((nobody.clients, nobody.focused), (0, None));

        // A watch request names the panel's session, then the relay's own.
        let names = |host: &str, session: &str| Some((host.to_string(), session.to_string()));
        assert_eq!(watch_host("watch noc-host noc"), names("noc-host", "noc"));
        assert_eq!(
            watch_host("  watch   a.b_c   noc--auth "),
            names("a.b_c", "noc--auth")
        );
        for bad in [
            "watch",
            "watch ",
            "watch a",
            "watch a b c",
            "watch a;b c",
            "theme x",
            "",
        ] {
            assert_eq!(watch_host(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn a_team_has_a_session_and_the_agents_in_no_team_share_the_inner_session() {
        assert_eq!(team_session("noc", "auth"), "noc--auth");
        assert_eq!(team_session("noc", ""), "noc");
        // A name that could not be part of a session's: the default session.
        assert_eq!(team_session("noc", "a b"), "noc");
        for session in ["noc", "noc--auth", "noc--a--b"] {
            assert!(is_inner_session("noc", session), "{session}");
        }
        // The dev pair's sessions are not sessions of the daily pair.
        for other in ["noc-host", "noc-dev", "noc-dev--auth", "noc--", "work", ""] {
            assert!(!is_inner_session("noc", other), "{other}");
        }
        assert_eq!(team_label("noc--auth"), "auth");
        assert_eq!(team_label("noc-dev--auth"), "auth");
        assert_eq!(team_label("noc"), "no-team");
        assert_eq!(team_label("noc--"), "no-team");
        assert_eq!(team_label(""), "no-team");

        // A request names the session, then the layout to make it from.
        let request = |session: &str, layout: &str| Some((session.to_string(), layout.to_string()));
        assert_eq!(
            switch_request("switch noc--auth noc-inner"),
            request("noc--auth", "noc-inner")
        );
        assert_eq!(
            switch_request("  switch   noc   /my layouts/noc-inner "),
            request("noc", "/my layouts/noc-inner")
        );
        for bad in [
            "switch",
            "switch ",
            "switch a",
            "switch a;b c",
            "switch a x\u{1b}y",
            "watch a b",
            "",
        ] {
            assert_eq!(switch_request(bad), None, "{bad:?}");
        }
        assert_eq!(
            switch_command("noc", "file:/p/zellij-noc.wasm", "noc--auth", "noc-inner")[4..],
            [
                "--plugin",
                "file:/p/zellij-noc.wasm",
                "--plugin-configuration",
                "role=relay",
                "--name",
                "zellij-noc.relay",
                "--",
                "switch noc--auth noc-inner"
            ]
        );
    }

    #[test]
    fn the_tab_a_new_session_came_with_is_closed_once_the_agent_has_its_own() {
        let pane = |id: u32, tab: u32, name: &str, command: Option<&str>| ScanPane {
            id,
            tab_id: Some(tab),
            tab_name: name.to_string(),
            terminal_command: command.map(str::to_string),
            ..ScanPane::default()
        };
        let session = |panes: Vec<ScanPane>| GuestSnapshot {
            session: "noc--auth".to_string(),
            clients: 1,
            focused: None,
            panes,
        };
        let shell = pane(0, 0, "Tab #1", None);
        let agent = pane(1, 1, "Auth lead", Some("noc attach a1"));
        // A session as its layout made it: one tab, one shell.
        assert_eq!(untouched_tab(&session(vec![shell.clone()])), Some(0));
        assert_eq!(untouched_tab(&session(vec![])), None);
        assert_eq!(
            untouched_tab(&session(vec![shell.clone(), agent.clone()])),
            None
        );
        assert_eq!(
            untouched_tab(&session(vec![pane(0, 0, "mine", None)])),
            None
        );
        assert_eq!(untouched_tab(&session(vec![agent.clone()])), None);

        // The agent's tab is not there yet: wait.
        assert_eq!(spare_tab_verdict(&session(vec![shell.clone()]), 0), None);
        // It is: the spare tab goes.
        assert_eq!(
            spare_tab_verdict(&session(vec![shell.clone(), agent.clone()]), 0),
            Some(true)
        );
        // Something was done in the spare tab, or it is gone: it stays.
        let split = session(vec![
            shell.clone(),
            pane(2, 0, "Tab #1", None),
            agent.clone(),
        ]);
        assert_eq!(spare_tab_verdict(&split, 0), Some(false));
        let named = session(vec![pane(0, 0, "notes", None), agent.clone()]);
        assert_eq!(spare_tab_verdict(&named, 0), Some(false));
        assert_eq!(spare_tab_verdict(&session(vec![agent]), 0), Some(false));
        assert_eq!(close_tab_args(3), ["action", "close-tab-by-id", "3"]);
    }

    #[test]
    fn an_agent_opens_where_it_is_open_and_otherwise_in_its_teams_session() {
        let agent = |json: &str| parse_agent(json).unwrap();
        let teamed = agent(r#"{"id":"a1","team":"auth"}"#);
        let alone = agent(r#"{"id":"a2"}"#);
        let holding = |id: &str| GuestSnapshot {
            session: String::new(),
            clients: 1,
            focused: None,
            panes: vec![ScanPane {
                id: 4,
                terminal_command: Some(format!("noc attach {id}")),
                ..ScanPane::default()
            }],
        };
        let none = BTreeMap::new();
        let session = |agent: &Agent, on_show: &str, snapshot: Option<&GuestSnapshot>, sessions| {
            agent_session(agent, "noc", on_show, snapshot, &none, sessions, "noc")
        };
        let no_sessions = BTreeMap::new();
        // Not open anywhere: the team's session, or the inner session.
        assert_eq!(session(&teamed, "noc", None, &no_sessions), "noc--auth");
        assert_eq!(session(&alone, "noc--auth", None, &no_sessions), "noc");
        // Open in the session on show, whatever its team says now.
        assert_eq!(
            session(&teamed, "noc", Some(&holding("a1")), &no_sessions),
            "noc"
        );
        // Open in a session that is not on show: it is found there.
        let elsewhere: BTreeMap<String, InnerSession> = [(
            "noc--billing".to_string(),
            InnerSession {
                snapshot: Some(holding("a1")),
                ..InnerSession::default()
            },
        )]
        .into();
        assert_eq!(
            session(&teamed, "noc", Some(&holding("a2")), &elsewhere),
            "noc--billing"
        );
        assert_eq!(session(&alone, "noc--auth", None, &elsewhere), "noc");
    }

    fn config(pairs: &[(&str, &str)]) -> Config {
        Config::from_map(
            &pairs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
        )
    }

    #[test]
    fn relay_accepts_plain_theme_names_only() {
        let none = Config::default();
        assert_eq!(relay_theme("theme nord", &none).as_deref(), Some("nord"));
        assert_eq!(
            relay_theme("  theme   gruvbox-dark_2.1 ", &none).as_deref(),
            Some("gruvbox-dark_2.1")
        );
        for bad in [
            "theme ",
            "theme",
            "theme a\" { Quit; }",
            "theme a b",
            "color nord",
            "theme x;y",
            "",
            "away",
            "home",
        ] {
            assert_eq!(relay_theme(bad, &none), None, "{bad:?}");
        }
        // `away` and `home` name the relay's own settings.
        let themed = config(&[("home_theme", "nord"), ("away_theme", "nord-away")]);
        assert_eq!(relay_theme(" away ", &themed).as_deref(), Some("nord-away"));
        assert_eq!(relay_theme("home", &themed).as_deref(), Some("nord"));
        let unsafe_name = config(&[("home_theme", "a b"), ("away_theme", "c")]);
        assert_eq!(relay_theme("home", &unsafe_name), None);
    }

    #[test]
    fn keyboard_signals_carry_the_theme_keys_only_when_themes_are_set() {
        let plain = Config::default();
        assert!(keyboard_signal(&plain, false).is_empty());
        assert_eq!(keyboard_signal(&plain, true), FOCUS_FRAME);
        let themed = config(&[("home_theme", "nord"), ("away_theme", "nord-away")]);
        assert_eq!(
            keyboard_signal(&themed, false),
            [27, 91, 50, 51, 59, 56, 126],
            "Ctrl Alt Shift F11"
        );
        let mut home = vec![27, 91, 50, 52, 59, 56, 126];
        home.extend_from_slice(&FOCUS_FRAME);
        assert_eq!(keyboard_signal(&themed, true), home);
        // Broken settings switch the theme keys off, never the focus frame.
        let broken = config(&[("home_theme", "nord")]);
        assert!(keyboard_signal(&broken, false).is_empty());
        assert_eq!(keyboard_signal(&broken, true), FOCUS_FRAME);
        assert_eq!(&FOCUS_FRAME[..], b"\x1bP26661nSgA=\x1b\\");
    }

    #[test]
    fn reads_the_opened_pane_id() {
        assert_eq!(opened_pane_id("terminal_12\n"), Some(12));
        assert_eq!(opened_pane_id("some text terminal_7 more"), Some(7));
        assert_eq!(opened_pane_id("plugin_3"), None);
        assert_eq!(opened_pane_id("terminal_x"), None);
        assert_eq!(opened_pane_id(""), None);
    }

    #[test]
    fn finds_an_agents_inner_pane_in_order() {
        let snapshot = parse_guest_snapshot(pushed()).unwrap();
        let a1 = parse_agent(r#"{"id":"a1"}"#).unwrap();
        let a2 = parse_agent(r#"{"id":"a2"}"#).unwrap();
        let launched: BTreeMap<String, LaunchedPane> = [(
            "a2".to_string(),
            LaunchedPane {
                id: 3,
                observed: false,
            },
        )]
        .into();
        assert_eq!(
            guest_agent_pane(&a1, &snapshot, &BTreeMap::new(), "inner", "noc"),
            Some(4)
        );
        assert_eq!(
            guest_agent_pane(&a2, &snapshot, &launched, "inner", "noc"),
            Some(3)
        );
        // a2's attach pane exited and nothing else names it.
        assert_eq!(
            guest_agent_pane(&a2, &snapshot, &BTreeMap::new(), "inner", "noc"),
            None
        );
        // Bindings count only for exactly the inner session.
        let bound = parse_agent(
            r#"{"id":"b","live_state":"live","frontend_bindings":[
              {"kind":"zellij","session":"","pane_id":"3","observed_at":"2026-02-01T00:00:00Z"},
              {"kind":"zellij","session":"other","pane_id":"3","observed_at":"2026-03-01T00:00:00Z"},
              {"kind":"zellij","session":"inner","pane_id":"terminal_4","observed_at":"2026-01-01T00:00:00Z"}]}"#,
        )
        .unwrap();
        assert_eq!(
            guest_agent_pane(&bound, &snapshot, &BTreeMap::new(), "inner", "noc"),
            Some(4)
        );
    }

    #[test]
    fn builds_inner_session_commands() {
        assert_eq!(
            focus_pane_args(4),
            ["action", "focus-pane-id", "terminal_4"]
        );
        assert_eq!(
            new_tab_args("Alpha", Some("/tmp/x"), "/bin/noc", "a1"),
            [
                "action", "new-tab", "--name", "Alpha", "--cwd", "/tmp/x", "--", "/bin/noc",
                "attach", "a1"
            ]
        );
        assert_eq!(
            new_tab_args("Alpha", None, "noc", "a1"),
            ["action", "new-tab", "--name", "Alpha", "--", "noc", "attach", "a1"]
        );
        assert_eq!(
            run_pane_args("Alpha", None, true, "noc", "a1"),
            [
                "run",
                "--name",
                "NOC: Alpha",
                "--stacked",
                "--",
                "noc",
                "attach",
                "a1"
            ]
        );
        assert_eq!(
            run_pane_args("Alpha", Some("/w"), false, "noc", "a1"),
            [
                "run",
                "--name",
                "NOC: Alpha",
                "--cwd",
                "/w",
                "--",
                "noc",
                "attach",
                "a1"
            ]
        );
        assert_eq!(shell_args("/w"), ["action", "new-pane", "--cwd", "/w"]);
        let command = guest_run_command("inner", "outer", &shell_args("/w; rm -rf /"));
        assert_eq!(&command[..2], ["/bin/sh", "-c"]);
        assert_eq!(&command[3..7], ["sh", "inner", "outer", "action"]);
        assert!(
            !command[2].contains("rm -rf"),
            "arguments never enter the script"
        );
        assert!(command[2].contains("focus-previous-pane"));
        assert_eq!(
            watch_command("inner", "file:/p/zellij-noc.wasm", "outer"),
            [
                "zellij",
                "--session",
                "inner",
                "pipe",
                "--plugin",
                "file:/p/zellij-noc.wasm",
                "--plugin-configuration",
                "role=relay",
                "--name",
                "zellij-noc.relay",
                "--",
                "watch outer inner"
            ]
        );
        assert_eq!(
            push_command("outer", "{}"),
            [
                "zellij",
                "--session",
                "outer",
                "pipe",
                "--name",
                "zellij-noc.guest",
                "--",
                "{}"
            ]
        );
    }

    #[test]
    fn agent_cwd_is_the_trimmed_workspace_path() {
        let a = parse_agent(r#"{"id":"a","workspace":{"path":" /tmp/x "}}"#).unwrap();
        assert_eq!(agent_cwd(&a).as_deref(), Some("/tmp/x"));
        let b = parse_agent(r#"{"id":"a","workspace":{"path":"  "}}"#).unwrap();
        assert_eq!(agent_cwd(&b), None);
        assert_eq!(agent_cwd(&parse_agent(r#"{"id":"a"}"#).unwrap()), None);
    }
}
