//! The inner ("guest") session: reading and driving it over the Zellij CLI,
//! the keyboard signals typed into it, and the `relay` role that runs inside
//! it to switch its theme.

use crate::app::{is_plain_name, Config};
use crate::model::{parse_terminal_pane_id, Agent};
use crate::panes::{attached_agent_id, compact_error, LaunchedPane, ScanPane};
use crate::{ctx, now_secs, State, PIPE_RELAY};
use std::collections::BTreeMap;
use zellij_tile::prelude::*;

pub const GUEST_SEPARATOR: &str = "--zellij-noc-guest-panes--";
pub const GUEST_POLL_SECS: f64 = 0.75;
/// How long the inner session may be unreadable before the panel says so.
/// It takes a moment to start, and it is briefly gone while it restarts.
pub const GUEST_MISSING_SECS: f64 = 5.0;
pub const CTX_SNAPSHOT: &str = "guest-snapshot";
pub const CTX_OPEN: &str = "guest-open";
pub const CTX_HOST_ACTION: &str = "host-action";
pub const KEY_GUEST_AGENT: &str = "zellij-noc-guest-agent";

/// Zellij's nested-session "focus gained" frame, `ESC P 26661 n SgA= ESC \`.
/// The host session never descends into the inner one, so it never sends
/// this itself; without it the inner client draws itself as unfocused.
pub const FOCUS_FRAME: [u8; 14] = [27, 80, 50, 54, 54, 54, 49, 110, 83, 103, 65, 61, 27, 92];

const SNAPSHOT_SCRIPT: &str = "zellij --session \"$1\" action list-clients && echo --zellij-noc-guest-panes-- && zellij --session \"$1\" action list-panes --all --json";
const RUN_SCRIPT: &str = "guest=$1; host=$2; shift 2\nzellij --session \"$guest\" \"$@\"; status=$?\n[ -n \"$host\" ] && zellij --session \"$host\" action focus-previous-pane >/dev/null 2>&1\nexit $status";

#[derive(Debug, Default)]
pub struct GuestState {
    pub snapshot: Option<GuestSnapshot>,
    pub in_flight: bool,
    pub last_read: Option<f64>,
    pub launched: BTreeMap<String, LaunchedPane>,
    /// Inner tabs named after an agent, by tab id: the agent and the name
    /// the tab had when the panel last looked or renamed it.
    pub named_tabs: BTreeMap<u32, (String, String)>,
    /// Whether the last snapshot showed a client attached to the inner session.
    pub client_seen: bool,
    pub failing_since: Option<f64>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct GuestSnapshot {
    pub clients: usize,
    pub focused: Option<u32>,
    pub panes: Vec<ScanPane>,
}

// ---- pure helpers ----

/// `list-clients` output, the separator, then `list-panes --all --json`.
/// The focused pane is the second field of the first client row.
pub fn parse_guest_snapshot(text: &str) -> Option<GuestSnapshot> {
    let (clients, panes) = text.split_once(GUEST_SEPARATOR)?;
    let panes: Vec<ScanPane> = serde_json::from_str(panes.trim()).ok()?;
    let rows: Vec<&str> = clients
        .lines()
        .filter(|l| !l.trim().is_empty())
        .skip(1)
        .collect();
    let focused = rows
        .first()
        .and_then(|row| row.split_whitespace().nth(1))
        .and_then(parse_terminal_pane_id);
    Some(GuestSnapshot {
        clients: rows.len(),
        focused,
        panes: panes.into_iter().filter(|p| !p.is_plugin).collect(),
    })
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
/// name)`. A tab is the panel's to name when exactly one agent runs in it
/// and its name is the one the panel knows it by (`named`), or one Zellij
/// made up (`Tab #3`). A tab the user named themselves is left alone.
/// `named` is brought up to date with what the snapshot shows.
pub fn tab_renames(
    agents: &[Agent],
    snapshot: &GuestSnapshot,
    launched: &BTreeMap<String, LaunchedPane>,
    guest_session: &str,
    noc_bin: &str,
    named: &mut BTreeMap<u32, (String, String)>,
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
        let [agent] = agents_in_tab.as_slice() else {
            named.remove(&tab);
            continue;
        };
        let want = crate::panes::agent_tab_name(agent);
        if want.is_empty() {
            continue;
        }
        let known = named
            .get(&tab)
            .is_some_and(|(id, last)| id == &agent.id && last == name);
        if name == want {
            named.insert(tab, (agent.id.clone(), want));
        } else if known || crate::panes::is_default_tab_name(name) {
            named.insert(tab, (agent.id.clone(), want.clone()));
            renames.push((tab, want));
        } else {
            named.remove(&tab);
        }
    }
    renames
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

/// An agent's live pane in the inner session: launched pane, attach command,
/// then a binding to exactly that session.
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
    if let Some(pane) = snapshot.panes.iter().find(|p| {
        !p.exited
            && p.terminal_command
                .as_deref()
                .and_then(|c| attached_agent_id(c, noc_bin))
                .as_deref()
                == Some(agent.id.as_str())
    }) {
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

pub fn snapshot_command(guest: &str) -> Vec<String> {
    strings(&["/bin/sh", "-c", SNAPSHOT_SCRIPT, "sh", guest])
}

fn run(command: &[String], context: BTreeMap<String, String>) {
    let argv: Vec<&str> = command.iter().map(String::as_str).collect();
    run_command(&argv, context);
}

// ---- state methods ----

impl State {
    /// Read the inner session, at most every `GUEST_POLL_SECS`. Returns
    /// whether the panel needs a redraw.
    pub fn poll_guest(&mut self) -> bool {
        if !self.permissions_granted || self.guest.in_flight {
            return false;
        }
        let now = now_secs();
        if self
            .guest
            .last_read
            .is_some_and(|t| now - t < GUEST_POLL_SECS)
        {
            return false;
        }
        self.guest.in_flight = true;
        self.guest.last_read = Some(now);
        run(
            &snapshot_command(&self.config.guest_session),
            ctx(CTX_SNAPSHOT),
        );
        false
    }

    pub fn on_guest_snapshot(&mut self, exit: Option<i32>, stdout: &[u8]) -> bool {
        self.guest.in_flight = false;
        let snapshot = (exit == Some(0))
            .then(|| std::str::from_utf8(stdout).ok())
            .flatten()
            .and_then(parse_guest_snapshot);
        let Some(snapshot) = snapshot else {
            return self.on_guest_unreadable();
        };
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

    fn on_guest_unreadable(&mut self) -> bool {
        self.guest.client_seen = false;
        let now = now_secs();
        let since = *self.guest.failing_since.get_or_insert(now);
        if now - since < GUEST_MISSING_SECS || self.app.guest_problem.is_some() {
            return false;
        }
        self.app.guest_problem = Some(format!(
            "inner session {:?} is not running",
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
        self.guest.last_read = None;
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

    /// Relay role: switch the inner session's theme live.
    pub fn relay_pipe(&mut self, message: &PipeMessage) {
        if message.name != PIPE_RELAY {
            return;
        }
        let Some(name) = message
            .payload
            .as_deref()
            .and_then(|p| relay_theme(p, &self.config))
        else {
            return;
        };
        reconfigure(format!("theme \"{name}\""), false);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::parse_agent;

    const CLIENTS: &str =
        "CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1         terminal_3     vim\n";

    fn panes_json() -> &'static str {
        r#"[{"id":3,"is_plugin":false,"is_focused":true,"title":"vim","terminal_command":null},
            {"id":4,"is_plugin":false,"title":"x","terminal_command":"noc attach a1","exited":false},
            {"id":5,"is_plugin":false,"terminal_command":"noc attach a2","exited":true},
            {"id":9,"is_plugin":true,"plugin_url":"zellij:status-bar"}]"#
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
        let text = format!("{CLIENTS}{GUEST_SEPARATOR}\n{}", panes_json());
        let snapshot = parse_guest_snapshot(&text).unwrap();
        assert_eq!(snapshot.focused, Some(3));
        assert_eq!(snapshot.clients, 1);
        let ids: Vec<u32> = snapshot.panes.iter().map(|p| p.id).collect();
        assert_eq!(ids, [3, 4, 5], "plugins are dropped");
        assert!(snapshot.panes[2].exited);
        assert_eq!(
            snapshot.panes[1].terminal_command.as_deref(),
            Some("noc attach a1")
        );
        // A bare numeric id parses too.
        let text = format!("CLIENT_ID PANE\n1 7 sh\n{GUEST_SEPARATOR}\n[]");
        assert_eq!(parse_guest_snapshot(&text).unwrap().focused, Some(7));
    }

    #[test]
    fn a_focused_plugin_or_garbage_is_no_focus() {
        let plugin = format!("CLIENT_ID P R\n1 plugin_3 x\n{GUEST_SEPARATOR}\n[]");
        assert_eq!(parse_guest_snapshot(&plugin).unwrap().focused, None);
        let empty = format!("CLIENT_ID P R\n{GUEST_SEPARATOR}\n[]");
        assert_eq!(parse_guest_snapshot(&empty).unwrap().focused, None);
        assert_eq!(parse_guest_snapshot(&empty).unwrap().clients, 0);
        let garbage_clients = format!("???\nwhat is this\n{GUEST_SEPARATOR}\n[]");
        assert_eq!(
            parse_guest_snapshot(&garbage_clients).unwrap().focused,
            None
        );
        // Output that does not parse at all keeps the previous snapshot.
        assert_eq!(parse_guest_snapshot("no separator here"), None);
        assert_eq!(
            parse_guest_snapshot(&format!("{CLIENTS}{GUEST_SEPARATOR}\nnot json")),
            None
        );
        assert_eq!(parse_guest_snapshot(""), None);
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
        let text = format!("{CLIENTS}{GUEST_SEPARATOR}\n{}", panes_json());
        let snapshot = parse_guest_snapshot(&text).unwrap();
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
        let snapshot = snapshot_command("a b");
        assert_eq!(snapshot[4], "a b");
        assert!(snapshot[2].contains(GUEST_SEPARATOR));
        assert!(!snapshot[2].contains("a b"));
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
