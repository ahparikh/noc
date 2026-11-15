//! The inner ("guest") session: the `relay` role that runs inside it, sends
//! the panel its panes, tabs and focus as they change and switches its
//! theme; and the panel's side: asking the relay to watch, driving the
//! inner session over the Zellij CLI, and the keyboard signals typed into it.

use crate::app::{is_plain_name, Config, PaneTitles};
use crate::model::{parse_terminal_pane_id, Agent};
use crate::panes::{
    agent_named_by_pane_command, compact_error, manifest_panes, LaunchedPane, ScanPane,
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
pub const CTX_HOST_ACTION: &str = "host-action";
pub const KEY_GUEST_AGENT: &str = "zellij-noc-guest-agent";

/// Zellij's nested-session "focus gained" frame, `ESC P 26661 n SgA= ESC \`.
/// The host session never descends into the inner one, so it never sends
/// this itself; without it the inner client draws itself as unfocused.
pub const FOCUS_FRAME: [u8; 14] = [27, 80, 50, 54, 54, 54, 49, 110, 83, 103, 65, 61, 27, 92];

const RUN_SCRIPT: &str = "guest=$1; host=$2; shift 2\nzellij --session \"$guest\" \"$@\"; status=$?\n[ -n \"$host\" ] && zellij --session \"$host\" action focus-previous-pane >/dev/null 2>&1\nexit $status";

#[derive(Debug, Default)]
pub struct GuestState {
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
    pub clients: usize,
    pub focused: Option<u32>,
    pub panes: Vec<ScanPane>,
}

/// The relay's side: what it last heard from Zellij and from the panel.
#[derive(Debug, Default)]
pub struct RelayState {
    /// The session whose panel asked to watch, and when it last asked.
    pub host: Option<String>,
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
        clients: clients.len(),
        focused,
        panes,
    }
}

/// The host session a `watch <session>` message names.
pub fn watch_host(payload: &str) -> Option<String> {
    let name = payload.trim().strip_prefix("watch ")?.trim();
    is_plain_name(name).then(|| name.to_string())
}

/// The panel's request to the relay: watch the inner session for the panel
/// in `host`. `zellij pipe` starts the relay when it is not running; the
/// configuration is the one the relay bindings load it with, so there is
/// one relay.
pub fn watch_command(guest: &str, plugin_url: &str, host: &str) -> Vec<String> {
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
        &format!("watch {host}"),
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
        if !self.permissions_granted || self.guest.in_flight {
            return false;
        }
        // The relay is this plugin, so the panel must know where it was
        // loaded from, and the relay must know which session to answer.
        let (Some(url), Some(host)) = (self.panel_url.clone(), self.session_name.clone()) else {
            return false;
        };
        let now = now_secs();
        let healthy = self.guest.snapshot.is_some() && self.guest.failing_since.is_none();
        let every = if healthy {
            GUEST_WATCH_SECS
        } else {
            GUEST_RETRY_SECS
        };
        if self.guest.last_watch.is_some_and(|t| now - t < every) {
            return false;
        }
        // The last request reached the inner session and nothing came back.
        let unanswered = self.guest.watch_ok
            && self
                .guest
                .last_watch
                .is_some_and(|asked| self.guest.last_push.is_none_or(|push| push < asked));
        self.guest.in_flight = true;
        self.guest.last_watch = Some(now);
        run(
            &watch_command(&self.config.guest_session, &url, &host),
            ctx(CTX_WATCH),
        );
        unanswered && self.on_guest_unreadable("is not answering")
    }

    /// The watch request ran: it fails when the inner session is not there.
    pub fn on_guest_watch(&mut self, exit: Option<i32>) -> bool {
        self.guest.in_flight = false;
        self.guest.watch_ok = exit == Some(0);
        if self.guest.watch_ok {
            return false;
        }
        self.on_guest_unreadable("is not running")
    }

    /// The relay sent a snapshot.
    pub fn on_guest_push(&mut self, payload: &str) -> bool {
        let Some(mut snapshot) = parse_guest_snapshot(payload) else {
            return false;
        };
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
                            Some(&config.guest_session),
                        );
                        pane_name(pane, agent, host, shell)
                    })
                }
                // The host name is not known yet.
                _ => Vec::new(),
            };
            for (pane, name) in renames {
                run(
                    &guest_run_command(
                        &self.config.guest_session,
                        "",
                        &rename_pane_args(pane, &name),
                    ),
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
        for (tab, name) in tab_renames(
            &self.app.agents,
            &snapshot,
            &self.guest.launched,
            &self.config.guest_session,
            &self.config.noc_bin,
            &mut self.guest.named_tabs,
        ) {
            // No host session name: the keyboard stays where it is.
            run(
                &guest_run_command(&self.config.guest_session, "", &rename_tab_args(tab, &name)),
                ctx(CTX_HOST_ACTION),
            );
        }
        self.guest.snapshot = Some(snapshot);
        if newly_attached && self.guest_has_keyboard() != Some(false) {
            // A client just attached in the guest pane and the keyboard is
            // there: tell it so.
            self.write_to_guest(&FOCUS_FRAME);
        }
        redraw |= self.follow_guest_focus();
        redraw
    }

    fn on_guest_unreadable(&mut self, what: &str) -> bool {
        self.guest.client_seen = false;
        let now = now_secs();
        let since = *self.guest.failing_since.get_or_insert(now);
        if now - since < GUEST_MISSING_SECS || self.app.guest_problem.is_some() {
            return false;
        }
        self.app.guest_problem = Some(format!(
            "inner session {:?} {what}",
            self.config.guest_session
        ));
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
            Some(&self.config.guest_session),
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
            &guest_run_command(&self.config.guest_session, &host, &args),
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
        let guest = self.config.guest_session.clone();
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
        if let Some(host) = watch_host(payload) {
            self.relay.host = Some(host);
            self.relay.watched_at = now_secs();
            // Every request is answered, changed or not.
            self.relay.sent = None;
            list_clients();
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
        let snapshot = relay_snapshot(&panes, &self.relay.tabs, clients);
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
            &panes,
            &tabs,
            &[client(PaneId::Terminal(3)), client(PaneId::Terminal(0))],
        );
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
            relay_snapshot(&panes, &tabs, &[client(PaneId::Plugin(1))]).focused,
            None
        );
        let nobody = relay_snapshot(&panes, &tabs, &[]);
        assert_eq!((nobody.clients, nobody.focused), (0, None));

        assert_eq!(watch_host("watch noc-host").as_deref(), Some("noc-host"));
        assert_eq!(watch_host("  watch   a.b_c "), Some("a.b_c".to_string()));
        for bad in ["watch", "watch ", "watch a b", "watch a;b", "theme x", ""] {
            assert_eq!(watch_host(bad), None, "{bad:?}");
        }
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
                "watch outer"
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
