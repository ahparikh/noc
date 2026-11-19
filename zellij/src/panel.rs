//! Key handling and intents, polling, stop/rename, opening agents, and
//! keyboard ownership between the panel and the inner session.

use crate::app::{
    is_team_char, is_team_name, Focus, PendingStop, Transport, ZellijNoc, DETAIL_SHIFT,
    TEAM_NAME_MAX,
};
use crate::guest::keyboard_signal;
use crate::model::{parse_snapshot, Agent, AgentGroup};
use crate::panes::{
    compact_error, focused_pane, is_server_unavailable, other_running_session, tab_panes, ScanPane,
};
use crate::{
    ctx, now_secs, Role, State, CTX_RENAME, CTX_STOP, CTX_TEAM_JOIN, CTX_TEAM_LEAVE, GRAB_WINDOW,
    KEY_AGENT, KEY_TITLE,
};
use std::collections::BTreeMap;
use zellij_tile::prelude::*;

/// The panel's pane has no title of its own: its first row says `Agents`.
/// A blank, because Zellij shows the plugin's location for an empty name.
pub const PANEL_TITLE: &str = " ";
/// How long the panel waits for the guest pane to start before it sends
/// the Enter again.
pub const GUEST_START_RETRY: f64 = 2.0;

// ---- keys -> intents (pure) ----

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Intent {
    Open,
    Pane {
        stacked: bool,
    },
    Worktree,
    /// Show the session of the selected team or agent.
    TeamSession,
    /// Show the session selected in the Sessions section.
    ShowSession {
        name: String,
    },
    /// End a team's session.
    EndSession {
        name: String,
    },
    Details,
    Refresh,
    /// The search query changed: read the list for the new one.
    Search,
    /// Ask now whether a newer release exists.
    CheckUpdate,
    Stop {
        agent_id: String,
        title: String,
    },
    Rename {
        agent_id: String,
        name: String,
    },
    /// Put the agent in the team, making the team if it is new.
    JoinTeam {
        agent_id: String,
        team: String,
    },
    /// Take the agent out of the team it is in.
    LeaveTeam {
        agent_id: String,
        team: String,
    },
}

/// Decide what a stoppable selection does: a confirmation or a refusal.
fn request_stop(app: &mut ZellijNoc) {
    let Some(agent) = app.selected_agent() else {
        app.set_notice("no agent is selected");
        return;
    };
    if agent.group() == AgentGroup::Quiet {
        app.set_notice("not running; nothing to stop");
        return;
    }
    let confirm = match (agent.provider.as_str(), agent.source_kind.as_str()) {
        ("claude", "background") => {
            if agent.native_short_id.trim().is_empty() {
                Err("no background id to stop")
            } else {
                Ok(())
            }
        }
        ("claude", _) => Err("stop it with /exit in its terminal"),
        ("codex", _) => Err("can't stop Codex agents yet"),
        _ => Err("can't stop this agent"),
    };
    match confirm {
        Ok(()) => {
            app.pending_stop = Some(PendingStop {
                agent_id: agent.id.clone(),
                title: agent.title(),
            });
        }
        Err(notice) => app.set_notice(notice),
    }
}

fn handle_rename_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> Option<Intent> {
    let ctrl = key.key_modifiers.contains(&KeyModifier::Ctrl);
    match key.bare_key {
        BareKey::Esc => {
            app.pending_rename = None;
            app.set_notice("rename cancelled");
        }
        BareKey::Enter => {
            let rename = app.pending_rename.take()?;
            let name = rename.text.trim().to_string();
            if name.is_empty() {
                app.set_notice("a name cannot be empty");
            } else {
                return Some(Intent::Rename {
                    agent_id: rename.agent_id,
                    name,
                });
            }
        }
        BareKey::Backspace => {
            if let Some(rename) = app.pending_rename.as_mut() {
                rename.text.pop();
            }
        }
        BareKey::Char('u') if ctrl => {
            if let Some(rename) = app.pending_rename.as_mut() {
                rename.text.clear();
            }
        }
        BareKey::Char(c) if !ctrl => {
            if let Some(rename) = app.pending_rename.as_mut() {
                rename.text.push(c);
            }
        }
        _ => {}
    }
    None
}

/// Keys while the team prompt is open. Only characters a team name may hold
/// are taken; `Tab` completes to a team that exists.
fn handle_team_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> Option<Intent> {
    let ctrl = key.key_modifiers.contains(&KeyModifier::Ctrl);
    let alt = key.key_modifiers.contains(&KeyModifier::Alt);
    match key.bare_key {
        BareKey::Esc => {
            app.pending_team = None;
            app.set_notice("team cancelled");
        }
        BareKey::Enter => {
            let team = app.pending_team.as_ref()?.text.clone();
            if team.is_empty() {
                app.pending_team = None;
                app.set_notice("team cancelled");
            } else if !is_team_name(&team) {
                app.set_notice("not a team name");
            } else {
                let pending = app.pending_team.take()?;
                return Some(Intent::JoinTeam {
                    agent_id: pending.agent_id,
                    team,
                });
            }
        }
        BareKey::Tab => app.complete_team(),
        BareKey::Backspace => {
            if let Some(pending) = app.pending_team.as_mut() {
                pending.text.pop();
                pending.typed = None;
            }
        }
        BareKey::Char('u') if ctrl => {
            if let Some(pending) = app.pending_team.as_mut() {
                pending.text.clear();
                pending.typed = None;
            }
        }
        BareKey::Char(c) if !ctrl && !alt => {
            if let Some(pending) = app.pending_team.as_mut() {
                let at = pending.text.chars().count();
                if at < TEAM_NAME_MAX && is_team_char(c, at) {
                    pending.text.push(c);
                    pending.typed = None;
                }
            }
        }
        _ => {}
    }
    None
}

/// `T`: take the selected agent out of its team, if it is in one.
fn request_leave_team(app: &mut ZellijNoc) -> Option<Intent> {
    let Some(agent) = app.selected_agent() else {
        app.set_notice("no agent is selected");
        return None;
    };
    let team = agent.team_name().to_string();
    if team.is_empty() {
        app.set_notice("not in a team");
        return None;
    }
    Some(Intent::LeaveTeam {
        agent_id: agent.id.clone(),
        team,
    })
}

/// `noc team set <team> <id>`.
pub fn team_set_argv<'a>(noc_bin: &'a str, team: &'a str, agent_id: &'a str) -> Vec<&'a str> {
    vec![noc_bin, "team", "set", team, agent_id]
}

/// `noc team clear <id>`.
pub fn team_clear_argv<'a>(noc_bin: &'a str, agent_id: &'a str) -> Vec<&'a str> {
    vec![noc_bin, "team", "clear", agent_id]
}

/// What the panel says when `noc team set` (`joined`) or `noc team clear`
/// has run.
pub fn team_result_notice(joined: bool, exit: Option<i32>, stderr: &str, team: &str) -> String {
    if exit == Some(0) {
        return format!("{} {team}", if joined { "joined" } else { "left" });
    }
    let what = if joined {
        "could not join the team"
    } else {
        "could not leave the team"
    };
    let detail = compact_error(stderr);
    if detail.is_empty() {
        what.to_string()
    } else {
        format!("{what}: {detail}")
    }
}

/// Keys while a search is being typed. Every change asks for new results;
/// Enter keeps the search and returns to the list, Esc drops it.
fn handle_search_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> Option<Intent> {
    let ctrl = key.key_modifiers.contains(&KeyModifier::Ctrl);
    let alt = key.key_modifiers.contains(&KeyModifier::Alt);
    let search = app.search.as_mut()?;
    match key.bare_key {
        BareKey::Esc => app.search = None,
        BareKey::Enter => {
            if search.text.trim().is_empty() {
                app.search = None;
            } else {
                search.typing = false;
            }
        }
        BareKey::Backspace => {
            search.text.pop();
        }
        BareKey::Char('u') if ctrl => search.text.clear(),
        BareKey::Char(c) if !ctrl && !alt => search.text.push(c),
        _ => return None,
    }
    Some(Intent::Search)
}

/// Keys while the Sessions section has them. `None` leaves the key to the
/// panel's own keys (help, search, grouping and the like); the keys that act
/// on an agent are refused, since no agent is selected.
fn handle_session_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> Option<Option<Intent>> {
    match key.bare_key {
        BareKey::Char('j') | BareKey::Down => app.move_session(1),
        BareKey::Char('k') | BareKey::Up => app.move_session(-1),
        BareKey::Char('g') => {
            if std::mem::take(&mut app.pending_g) {
                app.session_index = 0;
            } else {
                app.pending_g = true;
            }
            return Some(None);
        }
        BareKey::Char('G') => app.move_session(isize::MAX),
        BareKey::Enter | BareKey::Char('l') | BareKey::Char('o') => {
            app.pending_g = false;
            let name = app.selected_session()?.name.clone();
            return Some(Some(Intent::ShowSession { name }));
        }
        BareKey::Char('x') => {
            app.pending_g = false;
            let name = app.selected_session()?.name.clone();
            if name == app.config.guest_session {
                app.set_notice("ending it would end the pair; quit it from inside");
            } else {
                app.pending_end = Some(name);
            }
        }
        BareKey::Char('h') | BareKey::Left => app.focus_agents(),
        BareKey::Esc if !app.show_info && !app.show_help && app.search.is_none() => {
            app.focus_agents()
        }
        BareKey::Char('p' | 's' | 'w' | 'd' | 'n' | 't' | 'T' | ' ') => {
            app.pending_g = false;
            app.set_notice("that key is for an agent; Tab goes back to them");
        }
        _ => return None,
    }
    Some(None)
}

/// Keys while the Details section has them: they move what it shows, down
/// and sideways. Whether a key was one of them; any other goes to the list
/// the details are about.
fn handle_details_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> bool {
    match key.bare_key {
        BareKey::Char('j') | BareKey::Down => app.scroll_details(1),
        BareKey::Char('k') | BareKey::Up => app.scroll_details(-1),
        BareKey::Char('l') | BareKey::Right => app.shift_details(DETAIL_SHIFT as isize),
        BareKey::Char('h') | BareKey::Left => app.shift_details(-(DETAIL_SHIFT as isize)),
        BareKey::Char('g') => {
            if std::mem::take(&mut app.pending_g) {
                app.scroll_details(isize::MIN);
            } else {
                app.pending_g = true;
            }
        }
        BareKey::Char('G') => app.scroll_details(isize::MAX),
        BareKey::Esc if !app.show_info && !app.show_help && app.search.is_none() => {
            app.focus = app.source;
        }
        _ => return false,
    }
    true
}

/// Apply one key to the panel state. Returns whether to redraw and the
/// intent (if any) the host must act on.
pub fn process_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> (bool, Option<Intent>) {
    if app.pending_rename.is_some() {
        return (true, handle_rename_key(app, key));
    }
    if app.pending_team.is_some() {
        return (true, handle_team_key(app, key));
    }
    if app.is_typing_search() {
        return (true, handle_search_key(app, key));
    }
    let modifiers = &key.key_modifiers;
    if modifiers.contains(&KeyModifier::Ctrl) {
        return match key.bare_key {
            BareKey::Char('d') | BareKey::Char('u') if app.focus == Focus::Details => {
                let half = (app.detail_rows / 2).max(1) as isize;
                let down = key.bare_key == BareKey::Char('d');
                app.scroll_details(if down { half } else { -half });
                (true, None)
            }
            BareKey::Char('d') => {
                app.move_half_page(true);
                (true, None)
            }
            BareKey::Char('u') => {
                app.move_half_page(false);
                (true, None)
            }
            _ => (false, None),
        };
    }
    if !modifiers.is_empty() && !(modifiers.len() == 1 && modifiers.contains(&KeyModifier::Shift)) {
        return (false, None);
    }
    if let Some(stop) = app.pending_stop.take() {
        if key.bare_key == BareKey::Char('y') && modifiers.is_empty() {
            app.set_notice(format!("stopping {}\u{2026}", stop.title));
            return (
                true,
                Some(Intent::Stop {
                    agent_id: stop.agent_id,
                    title: stop.title,
                }),
            );
        }
        app.set_notice("stop cancelled");
        return (true, None);
    }
    if let Some(name) = app.pending_end.take() {
        if key.bare_key == BareKey::Char('y') && modifiers.is_empty() {
            app.set_notice(format!("ending {}\u{2026}", app.session_label(&name)));
            return (true, Some(Intent::EndSession { name }));
        }
        app.set_notice("end cancelled");
        return (true, None);
    }
    match key.bare_key {
        BareKey::Tab => {
            app.toggle_focus(modifiers.is_empty());
            return (true, None);
        }
        BareKey::Char('S') => {
            app.toggle_sessions();
            return (true, None);
        }
        _ => {}
    }
    if app.focus == Focus::Details && handle_details_key(app, key) {
        return (true, None);
    }
    // Any other key in Details is for the list the details are about.
    if app.list_focus() == Focus::Sessions {
        if let Some(handled) = handle_session_key(app, key) {
            return (true, handled);
        }
    }
    let was_pending_g = app.pending_g;
    if key.bare_key != BareKey::Char('g') {
        app.pending_g = false;
    }
    let mut intent = None;
    match key.bare_key {
        BareKey::Char('j') | BareKey::Down => app.move_selection(1),
        BareKey::Char('k') | BareKey::Up => app.move_selection(-1),
        BareKey::Char('g') => {
            app.pending_g = was_pending_g;
            app.press_g();
        }
        BareKey::Char('G') => app.select_last(),
        BareKey::Char('D') => app.toggle_details(),
        BareKey::Char('?') => app.toggle_help(),
        BareKey::Char('i') => app.toggle_info(),
        BareKey::Esc => {
            if app.show_info {
                app.show_info = false;
            } else if app.show_help {
                app.close_help();
            } else if app.search.take().is_some() {
                intent = Some(Intent::Search);
            }
        }
        BareKey::Char('/') => {
            if app.config.transport == Transport::Pipe {
                app.set_notice("search needs the poll transport");
            } else {
                app.close_help();
                app.begin_search();
            }
        }
        BareKey::Enter | BareKey::Char('l') => {
            if !app.toggle_selected_section() {
                intent = Some(Intent::Open);
            }
        }
        BareKey::Char(' ') => {
            app.toggle_selected_section();
        }
        BareKey::Char('h') | BareKey::Left => app.collapse_selected(),
        BareKey::Char('v') => app.toggle_group_by(),
        BareKey::Char('p') => intent = Some(Intent::Pane { stacked: false }),
        BareKey::Char('s') => intent = Some(Intent::Pane { stacked: true }),
        BareKey::Char('w') => intent = Some(Intent::Worktree),
        BareKey::Char('o') => intent = Some(Intent::TeamSession),
        BareKey::Char('d') => intent = Some(Intent::Details),
        BareKey::Char('r') => intent = Some(Intent::Refresh),
        BareKey::Char('u') => intent = Some(Intent::CheckUpdate),
        BareKey::Char('x') => request_stop(app),
        BareKey::Char('n') => {
            let began = app.begin_rename();
            if !began {
                app.set_notice("no agent is selected");
            }
        }
        BareKey::Char('t') => {
            if !app.begin_team() {
                app.set_notice("no agent is selected");
            }
        }
        BareKey::Char('T') => intent = request_leave_team(app),
        _ => {}
    }
    (true, intent)
}

/// The command that reads the list: every agent, or the matches of a
/// search. Both print the same document.
pub fn snapshot_argv<'a>(noc_bin: &'a str, query: &'a str) -> Vec<&'a str> {
    if query.is_empty() {
        vec![noc_bin, "ls", "--json", "--all"]
    } else {
        vec![noc_bin, "search", "--json", "--", query]
    }
}

pub const CTX_REGISTRY_WATCH: &str = "registry-watch";
/// How long after the registry watcher ended before it is started again.
pub const REGISTRY_WATCH_RETRY_SECS: f64 = 5.0;

/// Runs until the registry or the host session goes away. `$1` is noc,
/// `$2` the host session, `$3` a tag naming it. It first ends any watcher an
/// earlier load of the plugin left for the same session, then says
/// "changed" to the panel for every line `noc watch` prints. The lines
/// themselves are not read: the panel reads the registry with `noc ls`.
/// `zellij pipe` must not inherit the loop's input: it would read it, and
/// wait on it.
const REGISTRY_WATCH_SCRIPT: &str = "for pid in $(pgrep -f -- \"$3\\$\"); do [ \"$pid\" != \"$$\" ] && kill \"$pid\"; done 2>/dev/null\n\"$1\" watch --json --all | while IFS= read -r _; do zellij --session \"$2\" pipe --name zellij-noc.changed -- 1 </dev/null >/dev/null 2>&1 || exit 1; done";

pub fn registry_watch_command(noc_bin: &str, host: &str) -> Vec<String> {
    [
        "/bin/sh",
        "-c",
        REGISTRY_WATCH_SCRIPT,
        "sh",
        noc_bin,
        host,
        &format!("zellij-noc-registry-watch={host}"),
    ]
    .iter()
    .map(|s| s.to_string())
    .collect()
}

// ---- host side ----

fn lossy(bytes: &[u8]) -> String {
    String::from_utf8_lossy(bytes).into_owned()
}

impl State {
    pub fn handle_key(&mut self, key: &KeyWithModifier) -> bool {
        let (redraw, intent) = process_key(&mut self.app, key);
        if let Some(intent) = intent {
            self.run_intent(intent);
        }
        redraw
    }

    fn run_intent(&mut self, intent: Intent) {
        match intent {
            Intent::Open => self.open_selected_agent(),
            Intent::Pane { stacked } => self.open_agent_pane(stacked),
            Intent::Worktree => self.open_worktree_shell(),
            Intent::TeamSession => self.show_selected_team(),
            Intent::ShowSession { name } => self.show_and_focus(&name),
            Intent::EndSession { name } => self.end_session(&name),
            Intent::Details => self.open_details_window(),
            Intent::Refresh | Intent::Search => self.refresh(),
            Intent::CheckUpdate => self.check_for_update_now(),
            Intent::Stop { agent_id, title } => {
                let mut context = ctx(CTX_STOP);
                context.insert(KEY_AGENT.to_string(), agent_id.clone());
                context.insert(KEY_TITLE.to_string(), title);
                run_command(
                    &[self.config.noc_bin.as_str(), "stop", agent_id.as_str()],
                    context,
                );
            }
            Intent::Rename { agent_id, name } => {
                let mut context = ctx(CTX_RENAME);
                context.insert(KEY_TITLE.to_string(), name.clone());
                run_command(
                    &[
                        self.config.noc_bin.as_str(),
                        "rename",
                        agent_id.as_str(),
                        name.as_str(),
                    ],
                    context,
                );
            }
            Intent::JoinTeam { agent_id, team } => {
                let mut context = ctx(CTX_TEAM_JOIN);
                context.insert(KEY_AGENT.to_string(), agent_id.clone());
                context.insert(KEY_TITLE.to_string(), team.clone());
                run_command(
                    &team_set_argv(&self.config.noc_bin, &team, &agent_id),
                    context,
                );
            }
            Intent::LeaveTeam { agent_id, team } => {
                let mut context = ctx(CTX_TEAM_LEAVE);
                context.insert(KEY_AGENT.to_string(), agent_id.clone());
                context.insert(KEY_TITLE.to_string(), team);
                run_command(&team_clear_argv(&self.config.noc_bin, &agent_id), context);
            }
        }
    }

    // ---- polling ----

    pub fn refresh(&mut self) {
        if self.role != Role::Panel
            || self.config.transport == Transport::Pipe
            || !self.permissions_granted
            || self.command_in_flight
        {
            return;
        }
        self.command_in_flight = true;
        self.next_refresh = Some(now_secs() + self.config.poll_interval_secs());
        let query = self.app.search_text().unwrap_or_default().to_string();
        let mut context = ctx(crate::CTX_SNAPSHOT);
        context.insert(crate::KEY_QUERY.to_string(), query.clone());
        run_command(&snapshot_argv(&self.config.noc_bin, &query), context);
    }

    /// Keep one `noc watch` running that tells the panel when the registry
    /// changed. The timed poll goes on underneath it.
    pub fn watch_registry(&mut self) {
        if self.role != Role::Panel
            || self.config.transport == Transport::Pipe
            || !self.permissions_granted
            || self.registry_watching
            || now_secs() < self.registry_watch_retry
        {
            return;
        }
        let Some(host) = self.session_name.clone() else {
            return;
        };
        self.registry_watching = true;
        let command = registry_watch_command(&self.config.noc_bin, &host);
        let argv: Vec<&str> = command.iter().map(String::as_str).collect();
        run_command(&argv, ctx(CTX_REGISTRY_WATCH));
    }

    /// The watcher ended: the registry is not running, or was restarted.
    pub fn on_registry_watch_ended(&mut self) -> bool {
        self.registry_watching = false;
        self.registry_watch_retry = now_secs() + REGISTRY_WATCH_RETRY_SECS;
        false
    }

    /// The registry changed: read it now, or as soon as the read in flight
    /// is back.
    pub fn on_registry_changed(&mut self) {
        if self.command_in_flight {
            self.registry_changed = true;
        } else {
            self.refresh();
        }
    }

    pub fn on_snapshot_result(&mut self, exit: Option<i32>, stdout: &[u8], stderr: &[u8]) -> bool {
        self.command_in_flight = false;
        if std::mem::take(&mut self.registry_changed) {
            self.refresh();
        }
        if exit == Some(0) {
            match std::str::from_utf8(stdout) {
                Err(e) => self
                    .app
                    .set_error(format!("noc returned non-UTF-8 output: {e}")),
                Ok(text) => match parse_snapshot(text) {
                    Ok(snapshot) => self.app.apply_snapshot(snapshot, Transport::Poll),
                    Err(e) => self.app.set_error(e),
                },
            }
        } else {
            let detail = compact_error(&lossy(stderr));
            if is_server_unavailable(&detail) {
                self.app.set_server_unavailable();
            } else if detail.is_empty() {
                let status = exit.map_or_else(|| "unknown".to_string(), |c| c.to_string());
                self.app
                    .set_error(format!("noc ls failed with status {status}"));
            } else {
                self.app.set_error(detail);
            }
        }
        true
    }

    pub fn on_stop_result(
        &mut self,
        exit: Option<i32>,
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        let id = context.get(KEY_AGENT).cloned().unwrap_or_default();
        let title = context.get(KEY_TITLE).cloned().unwrap_or_default();
        if exit == Some(0) {
            self.app.mark_stopped(&id);
            self.app.set_notice(format!("stopped {title}"));
        } else {
            let detail = compact_error(&lossy(stderr));
            self.app.set_notice(if detail.is_empty() {
                format!("could not stop {title}")
            } else {
                detail
            });
        }
        true
    }

    pub fn on_rename_result(
        &mut self,
        exit: Option<i32>,
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        if exit == Some(0) {
            let name = context.get(KEY_TITLE).cloned().unwrap_or_default();
            self.app.set_notice(format!("renamed to {name}"));
            self.refresh();
        } else {
            let detail = compact_error(&lossy(stderr));
            self.app.set_notice(if detail.is_empty() {
                "could not rename".to_string()
            } else {
                detail
            });
        }
        true
    }

    /// The result of `noc team set` (`joined`) or `noc team clear`. Either
    /// way the list is read again, so a change that did land is shown.
    pub fn on_team_result(
        &mut self,
        joined: bool,
        exit: Option<i32>,
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        let team = context.get(KEY_TITLE).cloned().unwrap_or_default();
        self.app
            .set_notice(team_result_notice(joined, exit, &lossy(stderr), &team));
        self.refresh();
        true
    }

    // ---- the host tab: the panel beside the guest pane ----

    fn floating_visible(&self, tab: usize) -> bool {
        self.tabs
            .iter()
            .find(|t| t.position == tab)
            .is_some_and(|t| t.are_floating_panes_visible)
    }

    /// The pane holding the keyboard in the panel's own tab.
    fn keyboard_pane(&self) -> Option<ScanPane> {
        let tab = self.own_tab_position?;
        let in_tab = tab_panes(&self.panes, tab);
        focused_pane(&in_tab, self.floating_visible(tab), self.plugin_id).cloned()
    }

    pub fn panel_has_keyboard(&self) -> bool {
        self.keyboard_pane()
            .is_some_and(|p| p.is_plugin && p.id == self.plugin_id)
    }

    /// Whether the keyboard is in the guest pane, so in the inner session.
    /// `None` until Zellij has reported the tab.
    pub fn guest_has_keyboard(&self) -> Option<bool> {
        self.keyboard_pane().map(|p| !p.is_plugin)
    }

    /// The guest pane: the first tiled, visible terminal of the panel's tab.
    pub fn guest_pane(&self) -> Option<ScanPane> {
        let tab = self.own_tab_position?;
        self.panes
            .iter()
            .find(|p| p.tab_position == tab && !p.is_plugin && !p.is_suppressed && !p.is_floating)
            .cloned()
    }

    // ---- opening agents (always in the inner session) ----

    fn require_permission(&mut self) -> bool {
        if !self.permissions_granted {
            self.app
                .set_notice("pane attachment permission was not granted");
            return false;
        }
        true
    }

    fn selected_agent_or_notice(&mut self) -> Option<Agent> {
        if !self.require_permission() {
            return None;
        }
        let agent = self.app.selected_agent().cloned();
        if agent.is_none() {
            self.app.set_notice("no agent is selected");
        }
        agent
    }

    /// Another running Zellij session (not the host, not one of the sessions)
    /// that an agent is already bound to.
    pub fn other_running_session_for(&self, agent: &Agent) -> Option<String> {
        let list = get_session_list().ok()?;
        let live: Vec<String> = list.live_sessions.iter().map(|s| s.name.clone()).collect();
        let current = list
            .live_sessions
            .iter()
            .find(|s| s.is_current_session)
            .map(|s| s.name.clone())
            .or_else(|| self.session_name.clone());
        other_running_session(
            agent,
            current.as_deref(),
            Some(self.config.guest_session.as_str()),
            &live,
        )
    }

    pub fn open_selected_agent(&mut self) {
        if let Some(agent) = self.selected_agent_or_notice() {
            self.guest_open_agent(&agent);
        }
    }

    pub fn open_agent_pane(&mut self, stacked: bool) {
        let Some(agent) = self.selected_agent_or_notice() else {
            return;
        };
        if agent.native_session_id.trim().is_empty() {
            self.app
                .set_notice("this agent has no resumable native session ID");
            return;
        }
        self.guest_open_agent_in_pane(&agent, stacked);
    }

    pub fn open_worktree_shell(&mut self) {
        let Some(agent) = self.app.selected_agent().cloned() else {
            self.app.set_notice("no agent is selected");
            return;
        };
        let dir = agent
            .workspace
            .as_ref()
            .map(|w| w.changes_directory())
            .unwrap_or_default();
        if dir.is_empty() {
            self.app.set_notice("this agent has no known workspace");
            return;
        }
        self.guest_open_shell(&dir);
    }

    pub fn open_details_window(&mut self) {
        let Some(agent) = self.app.selected_agent().cloned() else {
            self.app.set_notice("no agent is selected");
            return;
        };
        let Some(url) = self.panel_url.clone() else {
            self.app
                .set_notice("the plugin location is not known yet; try again");
            return;
        };
        if let Some(previous) = self.details_window.take() {
            close_plugin_pane(previous);
        }
        let mut config = BTreeMap::from([
            ("role".to_string(), "details".to_string()),
            ("agent_id".to_string(), agent.id.clone()),
            ("noc_bin".to_string(), self.config.noc_bin.clone()),
        ]);
        if self.config.nerd_icons {
            config.insert("icons".to_string(), "nerd".to_string());
        }
        if self.config.zellij_colors {
            config.insert("colors".to_string(), "zellij".to_string());
        }
        let coordinates = FloatingPaneCoordinates::new(
            Some("10%".to_string()),
            Some("10%".to_string()),
            Some("80%".to_string()),
            Some("80%".to_string()),
            None,
            None,
        );
        match open_plugin_pane_floating(&url, config, coordinates, BTreeMap::new()) {
            Some(PaneId::Plugin(id)) => {
                self.details_window = Some(id);
                self.details_window_seen = false;
            }
            _ => self
                .app
                .set_notice("Zellij could not open the details window"),
        }
    }

    // ---- keyboard ownership ----

    /// Selectable exactly while the panel has the keyboard or a grab window
    /// is open, so the host session's directional moves cannot land on it.
    pub fn sync_panel_selectable(&mut self) {
        if !self.permissions_granted {
            return;
        }
        let has_keyboard = self.panel_has_keyboard();
        if has_keyboard {
            self.grab_until = 0.0;
        }
        let wanted = has_keyboard || now_secs() < self.grab_until;
        if self.selectable_set != Some(wanted) {
            set_selectable(wanted);
            self.selectable_set = Some(wanted);
        }
    }

    /// The `zellij-noc.grab` pipe: give the keyboard to the panel, or hand
    /// it back to the inner session.
    pub fn toggle_keyboard(&mut self) {
        if !self.permissions_granted {
            return;
        }
        if self.panel_has_keyboard() {
            if !self.host_action(&["focus-previous-pane"]) {
                focus_previous_pane();
            }
            return;
        }
        self.grab_keyboard();
    }

    /// Bring the keyboard to the panel.
    pub fn grab_keyboard(&mut self) {
        if let Some(guest) = self.guest_pane() {
            let fullscreen = self
                .own_tab_position
                .and_then(|t| self.tabs.iter().find(|tab| tab.position == t))
                .is_some_and(|t| t.is_fullscreen_active);
            if fullscreen {
                toggle_pane_id_fullscreen(PaneId::Terminal(guest.id));
            }
        }
        self.grab_until = now_secs() + GRAB_WINDOW;
        self.sync_panel_selectable();
        if !self.host_action(&["move-focus", "left"]) {
            focus_plugin_pane(self.plugin_id, false, false);
        }
    }

    /// The guest pane's command starts suspended, and the panel starts it,
    /// with the Enter its "press Enter to run" waits for. So the inner
    /// session, whose tab bar is this plugin, is created only once the
    /// panel runs, by which time Zellij has the plugin: two sessions that
    /// fetch a plugin from its URL at the same moment both write the one
    /// file Zellij keeps it in, and spoil it.
    pub fn start_guest(&mut self) {
        if !self.permissions_granted {
            return;
        }
        let Some(guest) = self.guest_pane() else {
            return;
        };
        if !guest.is_held || guest.exited {
            return;
        }
        let now = now_secs();
        if self
            .guest_started
            .is_some_and(|t| now - t < GUEST_START_RETRY)
        {
            return;
        }
        let pane = format!("terminal_{}", guest.id);
        if self.host_action(&["write", "--pane-id", pane.as_str(), "13"]) {
            self.guest_started = Some(now);
        }
    }

    /// The details window was opened from the panel, so when it closes the
    /// keyboard comes back to the panel; Zellij would give it to the guest
    /// pane. Closed means gone from the panes after having been among them.
    pub fn follow_details_window(&mut self) {
        let Some(id) = self.details_window else {
            return;
        };
        let there = self.panes.iter().any(|p| p.is_plugin && p.id == id);
        if there {
            self.details_window_seen = true;
        } else if self.details_window_seen {
            self.details_window = None;
            self.details_window_seen = false;
            if !self.panel_has_keyboard() {
                self.grab_keyboard();
            }
        }
    }

    /// Tell the inner session when the keyboard leaves it or comes back,
    /// however it moved.
    pub fn signal_keyboard_moves(&mut self) {
        let Some(has) = self.guest_has_keyboard() else {
            return;
        };
        let previous = self.guest_had_keyboard.replace(has);
        if previous.is_none() || previous == Some(has) {
            return;
        }
        let bytes = keyboard_signal(&self.config, has);
        self.write_to_guest(&bytes);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::app::{Config, ListItem};
    use crate::model::parse_snapshot;
    use std::collections::BTreeSet;

    fn key(c: char) -> KeyWithModifier {
        KeyWithModifier::new(BareKey::Char(c))
    }

    fn bare(k: BareKey) -> KeyWithModifier {
        KeyWithModifier::new(k)
    }

    fn with(mods: &[KeyModifier], k: BareKey) -> KeyWithModifier {
        KeyWithModifier::new_with_modifiers(k, mods.iter().copied().collect::<BTreeSet<_>>())
    }

    fn app() -> ZellijNoc {
        let mut app = ZellijNoc::new(Config::default());
        let list = parse_snapshot(
            r#"{"schema_version":1,"generated_at":"2026-09-24T12:00:00Z","agents":[
            {"id":"bg","provider":"claude","source_kind":"background","native_short_id":"5e55a2b0","name":"Stoppable","display_state":"needs_input","bucket":"blocked","last_activity_at":"2026-09-24T11:59:00Z"},
            {"id":"nobg","provider":"claude","source_kind":"background","name":"No short id","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:58:00Z"},
            {"id":"cli","provider":"claude","source_kind":"cli","name":"Terminal one","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:57:00Z"},
            {"id":"cx","provider":"codex","name":"Codex one","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:56:00Z"},
            {"id":"other","provider":"gemini","name":"Other","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:55:00Z"},
            {"id":"q","provider":"claude","name":"Quiet one","display_state":"idle","bucket":"quiet","last_activity_at":"2026-09-24T10:00:00Z"}]}"#,
        )
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        app
    }

    fn select(app: &mut ZellijNoc, id: &str) {
        assert!(app.select_agent(id), "{id}");
    }

    #[test]
    fn i_opens_the_status_box_and_esc_closes_it_first() {
        let mut app = app();
        let (redraw, intent) = process_key(&mut app, &key('i'));
        assert!(redraw && intent.is_none() && app.show_info);
        // Esc closes the box before it closes help or ends a search.
        app.toggle_help();
        process_key(&mut app, &bare(BareKey::Esc));
        assert!(!app.show_info && app.show_help);
        process_key(&mut app, &bare(BareKey::Esc));
        assert!(!app.show_help);
        process_key(&mut app, &key('i'));
        process_key(&mut app, &key('i'));
        assert!(!app.show_info);
    }

    #[test]
    fn u_asks_for_a_release_check() {
        let mut app = app();
        let (redraw, intent) = process_key(&mut app, &key('u'));
        assert!(redraw && matches!(intent, Some(Intent::CheckUpdate)));
        // Ctrl-u is still half a page up, and typing a search takes the key.
        let (_, intent) = process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        assert!(intent.is_none());
        process_key(&mut app, &key('/'));
        let (_, intent) = process_key(&mut app, &key('u'));
        assert!(matches!(intent, Some(Intent::Search)));
        assert_eq!(app.search.as_ref().unwrap().text, "u");
    }

    #[test]
    fn slash_starts_a_search_that_captures_keys_until_enter_or_esc() {
        let search = |r: (bool, Option<Intent>)| matches!(r, (true, Some(Intent::Search)));
        let mut app = app();
        let (redraw, intent) = process_key(&mut app, &key('/'));
        assert!(redraw && intent.is_none() && app.is_typing_search());
        assert_eq!(app.search_text(), None, "nothing typed yet");

        // Every key is text, including the list's own keys; each asks for results.
        for c in ['j', 'x', ' ', '?', '/', 'G'] {
            assert!(search(process_key(&mut app, &key(c))), "{c}");
        }
        assert_eq!(app.search.as_ref().unwrap().text, "jx ?/G");
        assert!(!app.show_help && app.pending_stop.is_none());
        assert!(search(process_key(&mut app, &bare(BareKey::Backspace))));
        assert_eq!(app.search.as_ref().unwrap().text, "jx ?/");
        // Alt and other Ctrl chords are not text.
        let (_, intent) = process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Char('m')));
        assert!(intent.is_none());
        assert!(search(process_key(
            &mut app,
            &with(&[KeyModifier::Ctrl], BareKey::Char('u'))
        )));
        assert_eq!(app.search.as_ref().unwrap().text, "");
        for c in "older:14d auth".chars() {
            process_key(&mut app, &key(c));
        }

        // Enter keeps the search and gives the keys back to the list.
        assert!(search(process_key(&mut app, &bare(BareKey::Enter))));
        assert!(!app.is_typing_search());
        assert_eq!(app.search_text(), Some("older:14d auth"));
        let before = app.selected_index;
        let (_, intent) = process_key(&mut app, &key('j'));
        assert!(intent.is_none() && app.selected_index == before + 1);
        // `/` edits it again; Esc while typing drops it.
        process_key(&mut app, &key('/'));
        assert!(app.is_typing_search());
        assert_eq!(app.search.as_ref().unwrap().text, "older:14d auth");
        assert!(search(process_key(&mut app, &bare(BareKey::Esc))));
        assert!(app.search.is_none());

        // Enter on an empty search ends it.
        process_key(&mut app, &key('/'));
        process_key(&mut app, &key(' '));
        assert!(search(process_key(&mut app, &bare(BareKey::Enter))));
        assert!(app.search.is_none());

        // Esc in the list closes help first, then clears a search.
        process_key(&mut app, &key('/'));
        process_key(&mut app, &key('a'));
        process_key(&mut app, &bare(BareKey::Enter));
        app.toggle_help();
        let (_, intent) = process_key(&mut app, &bare(BareKey::Esc));
        assert!(intent.is_none() && !app.show_help && app.search.is_some());
        assert!(search(process_key(&mut app, &bare(BareKey::Esc))));
        assert!(app.search.is_none());
        let (_, intent) = process_key(&mut app, &bare(BareKey::Esc));
        assert!(intent.is_none());

        // The pipe transport has nothing to ask.
        app.config.transport = Transport::Pipe;
        process_key(&mut app, &key('/'));
        assert!(app.search.is_none());
        assert_eq!(
            app.notice.as_deref(),
            Some("search needs the poll transport")
        );

        assert_eq!(snapshot_argv("noc", ""), ["noc", "ls", "--json", "--all"]);
        assert_eq!(
            snapshot_argv("/bin/noc", "--json older:2w"),
            ["/bin/noc", "search", "--json", "--", "--json older:2w"]
        );
    }

    #[test]
    fn the_registry_watcher_takes_its_names_as_arguments() {
        let command = registry_watch_command("/opt/my noc", "outer");
        assert_eq!(command[..2], ["/bin/sh", "-c"]);
        assert_eq!(
            command[3..],
            [
                "sh",
                "/opt/my noc",
                "outer",
                "zellij-noc-registry-watch=outer"
            ]
        );
        let script = &command[2];
        assert!(script.contains("\"$1\" watch --json --all"));
        assert!(script.contains("pipe --name zellij-noc.changed -- 1 </dev/null"));
        assert!(
            !script.contains("outer") && !script.contains("my noc"),
            "arguments never enter the script"
        );
        assert_eq!(crate::PIPE_CHANGED, "zellij-noc.changed");
    }

    #[test]
    fn modifier_rules_gate_every_binding() {
        let mut app = app();
        // Ctrl-d / Ctrl-u move by half pages and request a redraw.
        app.last_render_rows = 4;
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('d')));
        assert!(redraw);
        assert_eq!(app.selected_index, 2);
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        assert!(redraw);
        assert_eq!(app.selected_index, 0);
        // Other Ctrl keys, and any Alt/Super/multi-modifier key, are ignored.
        for ignored in [
            with(&[KeyModifier::Ctrl], BareKey::Char('x')),
            with(&[KeyModifier::Alt], BareKey::Char('j')),
            with(&[KeyModifier::Super], BareKey::Char('j')),
            with(&[KeyModifier::Shift, KeyModifier::Alt], BareKey::Char('j')),
        ] {
            let (redraw, intent) = process_key(&mut app, &ignored);
            assert!(!redraw && intent.is_none());
            assert_eq!(app.selected_index, 0);
        }
        // Shift alone is fine.
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Char('G')));
        assert!(redraw);
        assert_eq!(app.selected_index, app.items.len() - 1);
    }

    #[test]
    fn modifier_checks_leave_a_pending_stop_alone() {
        let mut app = app();
        select(&mut app, "bg");
        process_key(&mut app, &key('x'));
        assert!(app.pending_stop.is_some());
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Char('y')));
        assert!(!redraw && app.pending_stop.is_some());
        app.last_render_rows = 4;
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('d')));
        assert!(app.pending_stop.is_some(), "Ctrl-d leaves it pending");
        let (redraw, intent) = process_key(&mut app, &key('y'));
        assert!(redraw);
        assert_eq!(
            intent,
            Some(Intent::Stop {
                agent_id: "bg".into(),
                title: "Stoppable".into()
            })
        );
        assert_eq!(app.notice.as_deref(), Some("stopping Stoppable\u{2026}"));
        assert!(app.pending_stop.is_none());
    }

    #[test]
    fn tab_moves_between_the_lists_and_shift_tab_into_the_details() {
        let mut app = app();
        let tab = bare(BareKey::Tab);
        let details = with(&[KeyModifier::Shift], BareKey::Tab);
        // No session is listed yet: Tab has nowhere to go.
        let (redraw, intent) = process_key(&mut app, &tab);
        assert!(redraw && intent.is_none());
        assert_eq!(app.focus, Focus::Agents);
        assert_eq!(app.notice.as_deref(), Some("no session is listed yet"));
        // Shift Tab: into the agent's details, and out again.
        process_key(&mut app, &details);
        assert_eq!((app.focus, app.source), (Focus::Details, Focus::Agents));
        process_key(&mut app, &details);
        assert_eq!(app.focus, Focus::Agents);
        // `d` closes the details: nothing to go into.
        process_key(&mut app, &key('D'));
        process_key(&mut app, &details);
        assert_eq!(app.focus, Focus::Agents);
        assert_eq!(
            app.notice.as_deref(),
            Some("details are closed; D opens them")
        );
        process_key(&mut app, &key('D'));
        // With sessions, Tab goes between the two lists and never through
        // the details, which stay about the list the keys are in.
        let names = [
            "noc".to_string(),
            "noc--auth".to_string(),
            "noc--web".to_string(),
        ];
        app.set_sessions(&names, "noc--web");
        for want in [Focus::Sessions, Focus::Agents, Focus::Sessions] {
            process_key(&mut app, &tab);
            assert_eq!((app.focus, app.source), (want, want));
        }
        // From a list's details, Tab goes to the other list.
        process_key(&mut app, &details);
        assert_eq!((app.focus, app.source), (Focus::Details, Focus::Sessions));
        process_key(&mut app, &tab);
        assert_eq!((app.focus, app.source), (Focus::Agents, Focus::Agents));
        // A closed section gives the keys up, and is not gone to.
        process_key(&mut app, &details);
        process_key(&mut app, &key('D'));
        assert_eq!(app.focus, Focus::Agents);
        process_key(&mut app, &key('D'));
        process_key(&mut app, &tab);
        assert_eq!(app.focus, Focus::Sessions);
        process_key(&mut app, &key('S'));
        assert_eq!((app.focus, app.source), (Focus::Agents, Focus::Agents));
        process_key(&mut app, &tab);
        assert_eq!(app.focus, Focus::Agents);
        assert_eq!(
            app.notice.as_deref(),
            Some("sessions are closed; S opens them")
        );
        process_key(&mut app, &key('S'));
    }

    #[test]
    fn the_sessions_section_has_its_own_keys() {
        let mut app = app();
        let names = [
            "noc".to_string(),
            "noc--auth".to_string(),
            "noc--web".to_string(),
        ];
        app.set_sessions(&names, "noc--web");
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(app.focus, Focus::Sessions);
        assert_eq!(app.selected_session().unwrap().name, "noc--web");
        // j, k, gg and G move among the sessions, not the agents.
        let agent = app.selected_index;
        process_key(&mut app, &key('k'));
        assert_eq!(app.selected_session().unwrap().name, "noc--auth");
        process_key(&mut app, &key('G'));
        assert_eq!(app.selected_session().unwrap().name, "noc");
        process_key(&mut app, &key('g'));
        process_key(&mut app, &key('g'));
        assert_eq!(app.selected_session().unwrap().name, "noc--auth");
        assert_eq!(app.selected_index, agent);
        // Enter, l and o show the selected session.
        for k in [bare(BareKey::Enter), key('l'), key('o')] {
            let (_, intent) = process_key(&mut app, &k);
            assert_eq!(
                intent,
                Some(Intent::ShowSession {
                    name: "noc--auth".to_string()
                })
            );
        }
        // x asks, and only y ends it.
        let (_, intent) = process_key(&mut app, &key('x'));
        assert!(intent.is_none());
        assert_eq!(app.pending_end.as_deref(), Some("noc--auth"));
        let (_, intent) = process_key(&mut app, &key('n'));
        assert!(intent.is_none() && app.pending_end.is_none());
        assert_eq!(app.notice.as_deref(), Some("end cancelled"));
        process_key(&mut app, &key('x'));
        let (_, intent) = process_key(&mut app, &key('y'));
        assert_eq!(
            intent,
            Some(Intent::EndSession {
                name: "noc--auth".to_string()
            })
        );
        // The inner session is not ended from here.
        process_key(&mut app, &key('G'));
        let (_, intent) = process_key(&mut app, &key('x'));
        assert!(intent.is_none() && app.pending_end.is_none());
        assert_eq!(
            app.notice.as_deref(),
            Some("ending it would end the pair; quit it from inside")
        );
        // An agent's keys are refused; the panel's own still work.
        for c in ['p', 's', 'w', 'd', 'n', 't', 'T'] {
            let (_, intent) = process_key(&mut app, &key(c));
            assert!(intent.is_none(), "{c}");
            assert!(app.pending_rename.is_none() && app.pending_team.is_none());
        }
        let (_, intent) = process_key(&mut app, &key('r'));
        assert_eq!(intent, Some(Intent::Refresh));
        process_key(&mut app, &key('v'));
        assert_eq!(app.group_by, crate::app::GroupBy::Team);
        // h and Esc go back to the agents.
        for back in [key('h'), bare(BareKey::Esc)] {
            app.focus = Focus::Sessions;
            app.source = Focus::Sessions;
            process_key(&mut app, &back);
            assert_eq!((app.focus, app.source), (Focus::Agents, Focus::Agents));
        }
    }

    #[test]
    fn the_details_section_scrolls_and_leaves_other_keys_to_its_list() {
        let mut app = app();
        process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Tab));
        assert_eq!((app.focus, app.source), (Focus::Details, Focus::Agents));
        // What there is to scroll is known from the last drawing.
        app.detail_max_scroll = 5;
        app.detail_max_shift = 6;
        app.detail_rows = 6;
        let agent = app.selected_index;
        process_key(&mut app, &key('j'));
        process_key(&mut app, &bare(BareKey::Down));
        assert_eq!(app.detail_scroll, 2);
        process_key(&mut app, &key('k'));
        assert_eq!(app.detail_scroll, 1);
        process_key(&mut app, &key('G'));
        assert_eq!(app.detail_scroll, 5);
        process_key(&mut app, &key('g'));
        process_key(&mut app, &key('g'));
        assert_eq!(app.detail_scroll, 0);
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('d')));
        assert_eq!(app.detail_scroll, 3);
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        assert_eq!(app.detail_scroll, 0);
        // l and h, and the arrows, move it sideways, within its width.
        process_key(&mut app, &key('l'));
        assert_eq!(app.detail_shift, 4);
        process_key(&mut app, &bare(BareKey::Right));
        assert_eq!(app.detail_shift, 6);
        process_key(&mut app, &key('h'));
        process_key(&mut app, &bare(BareKey::Left));
        assert_eq!(app.detail_shift, 0);
        assert_eq!(app.selected_index, agent, "the list did not move");
        // Any other key is for the agent the details are about.
        let (_, intent) = process_key(&mut app, &bare(BareKey::Enter));
        assert_eq!(intent, Some(Intent::Open));
        let (_, intent) = process_key(&mut app, &key('w'));
        assert_eq!(intent, Some(Intent::Worktree));
        assert_eq!(app.focus, Focus::Details);
        // Esc gives the keys back to that list.
        process_key(&mut app, &bare(BareKey::Esc));
        assert_eq!(app.focus, Focus::Agents);
    }

    #[test]
    fn stop_policy_and_confirmation() {
        let mut app = app();
        for (id, notice) in [
            ("nobg", "no background id to stop"),
            ("cli", "stop it with /exit in its terminal"),
            ("cx", "can't stop Codex agents yet"),
            ("other", "can't stop this agent"),
            ("q", "not running; nothing to stop"),
        ] {
            select(&mut app, id);
            process_key(&mut app, &key('x'));
            assert_eq!(app.notice.as_deref(), Some(notice), "{id}");
            assert!(app.pending_stop.is_none(), "{id}");
        }
        select(&mut app, "bg");
        process_key(&mut app, &key('x'));
        assert_eq!(app.pending_stop.as_ref().unwrap().title, "Stoppable");
        for cancel in [key('n'), bare(BareKey::Esc), key('Y'), key('j')] {
            app.pending_stop = Some(PendingStop {
                agent_id: "bg".into(),
                title: "Stoppable".into(),
            });
            let before = app.selected_index;
            let (redraw, intent) = process_key(&mut app, &cancel);
            assert!(redraw && intent.is_none());
            assert_eq!(app.notice.as_deref(), Some("stop cancelled"));
            assert!(app.pending_stop.is_none());
            assert_eq!(
                app.selected_index, before,
                "the cancelling key does nothing else"
            );
        }
    }

    #[test]
    fn rename_mode_captures_every_key() {
        let mut app = app();
        select(&mut app, "bg");
        process_key(&mut app, &key('n'));
        assert_eq!(app.pending_rename.as_ref().unwrap().text, "Stoppable");
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('d')));
        assert!(redraw, "even keys that normally scroll are captured");
        process_key(&mut app, &bare(BareKey::Backspace));
        process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Char('!')));
        process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Char('Z')));
        process_key(&mut app, &bare(BareKey::Left));
        assert_eq!(app.pending_rename.as_ref().unwrap().text, "Stoppabl!Z");
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        assert_eq!(app.pending_rename.as_ref().unwrap().text, "");
        // Empty names are refused without a command.
        let (_, intent) = process_key(&mut app, &bare(BareKey::Enter));
        assert!(intent.is_none());
        assert_eq!(app.notice.as_deref(), Some("a name cannot be empty"));
        assert!(app.pending_rename.is_none());
        // Submit trims and keeps inner whitespace.
        process_key(&mut app, &key('n'));
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        for c in "  New   name ".chars() {
            process_key(&mut app, &key(c));
        }
        let (_, intent) = process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Enter));
        assert_eq!(
            intent,
            Some(Intent::Rename {
                agent_id: "bg".into(),
                name: "New   name".into()
            })
        );
        // Esc cancels with a notice; n on a section row has no agent.
        process_key(&mut app, &key('n'));
        process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Esc));
        assert_eq!(app.notice.as_deref(), Some("rename cancelled"));
        assert!(app.pending_rename.is_none());
        app.select_last();
        process_key(&mut app, &key('n'));
        assert_eq!(app.notice.as_deref(), Some("no agent is selected"));
    }

    const TEAM_AGENTS: &str = r#"{"schema_version":1,"generated_at":"2026-09-24T12:00:00Z","agents":[
        {"id":"a","provider":"claude","name":"A","team":"auth","lead":true,"display_state":"working","bucket":"working","native_session_id":"s-a"},
        {"id":"b","provider":"claude","name":"B","team":"billing","display_state":"ready","bucket":"review","native_session_id":"s-b"},
        {"id":"c","provider":"claude","name":"C","display_state":"working","bucket":"working","native_session_id":"s-c"},
        {"id":"q","provider":"claude","name":"Q","display_state":"idle","bucket":"quiet","last_activity_at":"2026-09-24T10:00:00Z"}]}"#;

    fn team_app() -> ZellijNoc {
        let mut app = ZellijNoc::new(Config::default());
        app.apply_snapshot(parse_snapshot(TEAM_AGENTS).unwrap(), Transport::Poll);
        app
    }

    fn team_text(app: &ZellijNoc) -> &str {
        &app.pending_team.as_ref().unwrap().text
    }

    #[test]
    fn the_team_prompt_captures_every_key_and_takes_only_name_characters() {
        let mut app = team_app();
        select(&mut app, "c");
        let (redraw, intent) = process_key(&mut app, &key('t'));
        assert!(redraw && intent.is_none());
        assert_eq!(team_text(&app), "", "an agent in no team starts empty");
        // A name cannot start with - or ., and holds no spaces or symbols.
        for c in "-. /!".chars() {
            process_key(&mut app, &key(c));
        }
        assert_eq!(team_text(&app), "");
        for c in "we b-1._x".chars() {
            process_key(&mut app, &key(c));
        }
        process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Char('Z')));
        process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Char('q')));
        assert_eq!(team_text(&app), "web-1._xZ");
        // Keys that normally move, group or quit the prompt's neighbours.
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('d')));
        assert!(redraw);
        process_key(&mut app, &bare(BareKey::Left));
        process_key(&mut app, &bare(BareKey::Backspace));
        assert_eq!(team_text(&app), "web-1._x");
        assert_eq!(app.selected_agent().unwrap().id, "c");
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        assert_eq!(team_text(&app), "");
        // At most 48 characters.
        for _ in 0..60 {
            process_key(&mut app, &key('a'));
        }
        assert_eq!(team_text(&app).len(), 48);
        // Esc cancels with a notice, whatever its modifiers.
        process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Esc));
        assert!(app.pending_team.is_none());
        assert_eq!(app.notice.as_deref(), Some("team cancelled"));
        // An agent in a team starts with it.
        select(&mut app, "b");
        process_key(&mut app, &key('t'));
        assert_eq!(team_text(&app), "billing");
    }

    #[test]
    fn tab_in_the_team_prompt_completes_to_an_existing_team() {
        let mut app = team_app();
        select(&mut app, "c");
        process_key(&mut app, &key('t'));
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "auth");
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "billing");
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "auth");
        // Typing ends the walk: the next Tab completes the new text.
        process_key(&mut app, &with(&[KeyModifier::Ctrl], BareKey::Char('u')));
        process_key(&mut app, &key('b'));
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "billing");
        process_key(&mut app, &bare(BareKey::Backspace));
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "billing");
        // No team starts with it: Tab leaves the text alone.
        process_key(&mut app, &key('x'));
        process_key(&mut app, &bare(BareKey::Tab));
        assert_eq!(team_text(&app), "billingx");
    }

    #[test]
    fn enter_in_the_team_prompt_joins_a_valid_team_only() {
        let mut app = team_app();
        select(&mut app, "c");
        // Empty: closed, nothing run.
        process_key(&mut app, &key('t'));
        let (_, intent) = process_key(&mut app, &bare(BareKey::Enter));
        assert!(intent.is_none() && app.pending_team.is_none());
        assert_eq!(app.notice.as_deref(), Some("team cancelled"));
        // `none` is not a team: the prompt stays open.
        process_key(&mut app, &key('t'));
        for c in "none".chars() {
            process_key(&mut app, &key(c));
        }
        let (_, intent) = process_key(&mut app, &bare(BareKey::Enter));
        assert!(intent.is_none());
        assert_eq!(app.notice.as_deref(), Some("not a team name"));
        assert_eq!(team_text(&app), "none");
        // A new name makes the team.
        process_key(&mut app, &key('w'));
        let (_, intent) = process_key(&mut app, &bare(BareKey::Enter));
        assert_eq!(
            intent,
            Some(Intent::JoinTeam {
                agent_id: "c".into(),
                team: "nonew".into()
            })
        );
        assert!(app.pending_team.is_none());
        assert_eq!(
            team_set_argv("/opt/noc", "nonew", "c"),
            ["/opt/noc", "team", "set", "nonew", "c"]
        );
    }

    #[test]
    fn capital_t_leaves_the_team_and_both_keys_need_an_agent() {
        let mut app = team_app();
        select(&mut app, "a");
        assert_eq!(
            process_key(&mut app, &with(&[KeyModifier::Shift], BareKey::Char('T'))).1,
            Some(Intent::LeaveTeam {
                agent_id: "a".into(),
                team: "auth".into()
            })
        );
        assert_eq!(team_clear_argv("noc", "a"), ["noc", "team", "clear", "a"]);
        select(&mut app, "c");
        assert_eq!(process_key(&mut app, &key('T')).1, None);
        assert_eq!(app.notice.as_deref(), Some("not in a team"));
        // A section row, a team heading and a team's Quiet row are no agent.
        app.select_last();
        for c in ['t', 'T'] {
            app.notice = None;
            assert_eq!(process_key(&mut app, &key(c)).1, None);
            assert_eq!(app.notice.as_deref(), Some("no agent is selected"), "{c}");
            assert!(app.pending_team.is_none());
        }
        process_key(&mut app, &key('v'));
        for row in [0, app.items.len() - 1] {
            app.selected_index = row;
            assert!(app.selected_agent().is_none());
            for c in ['t', 'T'] {
                app.notice = None;
                assert_eq!(process_key(&mut app, &key(c)).1, None);
                assert_eq!(app.notice.as_deref(), Some("no agent is selected"));
            }
        }
        // Grouped by team, and in search results, the keys act on the agent.
        select(&mut app, "b");
        assert!(matches!(
            process_key(&mut app, &key('T')).1,
            Some(Intent::LeaveTeam { .. })
        ));
        process_key(&mut app, &key('t'));
        assert_eq!(team_text(&app), "billing");
        let mut empty = ZellijNoc::new(Config::default());
        process_key(&mut empty, &key('t'));
        assert_eq!(empty.notice.as_deref(), Some("no agent is selected"));
    }

    #[test]
    fn team_results_are_said_in_plain_words() {
        assert_eq!(team_result_notice(true, Some(0), "", "auth"), "joined auth");
        assert_eq!(team_result_notice(false, Some(0), "", "auth"), "left auth");
        assert_eq!(
            team_result_notice(true, Some(1), "noc: agent was\n  not found\n", "auth"),
            "could not join the team: noc: agent was not found"
        );
        assert_eq!(
            team_result_notice(false, None, "  ", "auth"),
            "could not leave the team"
        );
        assert_eq!(
            team_result_notice(true, Some(2), "", "auth"),
            "could not join the team"
        );
    }

    #[test]
    fn v_switches_the_grouping_and_enter_folds_a_team() {
        let mut app = ZellijNoc::new(Config::default());
        let list = parse_snapshot(
            r#"{"schema_version":1,"generated_at":"2026-09-24T12:00:00Z","agents":[
            {"id":"a","provider":"claude","name":"A","team":"auth","lead":true,"display_state":"working","bucket":"working","native_session_id":"s-a"},
            {"id":"b","provider":"claude","name":"B","team":"auth","display_state":"ready","bucket":"review","native_session_id":"s-b"},
            {"id":"c","provider":"claude","name":"C","display_state":"working","bucket":"working","native_session_id":"s-c"}]}"#,
        )
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        select(&mut app, "b");
        let (redraw, intent) = process_key(&mut app, &key('v'));
        assert!(redraw && intent.is_none());
        assert!(app.by_team());
        assert_eq!(app.notice.as_deref(), Some("grouped by team"));
        assert_eq!(app.selected_agent().unwrap().id, "b");
        // Agent rows keep every key they had.
        assert_eq!(
            process_key(&mut app, &bare(BareKey::Enter)).1,
            Some(Intent::Open)
        );
        assert_eq!(process_key(&mut app, &key('w')).1, Some(Intent::Worktree));
        // Enter on a heading folds the team and opens it again; it never
        // opens an agent.
        process_key(&mut app, &key('g'));
        process_key(&mut app, &key('g'));
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Team { expanded: true, .. })
        ));
        assert_eq!(process_key(&mut app, &bare(BareKey::Enter)).1, None);
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Team {
                expanded: false,
                count: 2,
                ..
            })
        ));
        assert_eq!(
            app.items.len(),
            3,
            "the heading, then no team and its agent"
        );
        assert_eq!(process_key(&mut app, &key('l')).1, None);
        assert_eq!(app.items.len(), 5);
        process_key(&mut app, &key('h'));
        assert_eq!(app.items.len(), 3);
        // A heading is not an agent.
        process_key(&mut app, &key('n'));
        assert_eq!(app.notice.as_deref(), Some("no agent is selected"));
        process_key(&mut app, &key('v'));
        assert!(!app.by_team());
        // With a modifier it is not the binding.
        let (redraw, _) = process_key(&mut app, &with(&[KeyModifier::Alt], BareKey::Char('v')));
        assert!(!redraw && !app.by_team());
    }

    #[test]
    fn navigation_and_intent_keys() {
        let mut app = app();
        let (_, intent) = process_key(&mut app, &key('j'));
        assert!(intent.is_none());
        assert_eq!(app.selected_index, 1);
        process_key(&mut app, &bare(BareKey::Up));
        assert_eq!(app.selected_index, 0);
        process_key(&mut app, &key('g'));
        assert!(app.pending_g);
        process_key(&mut app, &key('j'));
        assert!(!app.pending_g, "any other bound key cancels a pending g");
        process_key(&mut app, &key('G'));
        process_key(&mut app, &key('g'));
        process_key(&mut app, &key('g'));
        assert_eq!(app.selected_index, 0);
        for (c, want) in [
            ('p', Intent::Pane { stacked: false }),
            ('s', Intent::Pane { stacked: true }),
            ('w', Intent::Worktree),
            ('d', Intent::Details),
            ('r', Intent::Refresh),
        ] {
            assert_eq!(process_key(&mut app, &key(c)).1, Some(want), "{c}");
        }
        assert_eq!(
            process_key(&mut app, &bare(BareKey::Enter)).1,
            Some(Intent::Open)
        );
        assert_eq!(process_key(&mut app, &key('l')).1, Some(Intent::Open));
        assert_eq!(
            process_key(&mut app, &key(' ')).1,
            None,
            "space never opens an agent"
        );
        // Unbound keys only redraw.
        for unbound in [
            key('q'),
            bare(BareKey::Right),
            bare(BareKey::Tab),
            key('7'),
            bare(BareKey::Backspace),
        ] {
            let (redraw, intent) = process_key(&mut app, &unbound);
            assert!(redraw && intent.is_none());
        }
        // Help and details toggles.
        process_key(&mut app, &key('?'));
        assert!(app.show_help);
        process_key(&mut app, &key('D'));
        assert!(!app.show_help && !app.show_details);
        process_key(&mut app, &key('?'));
        process_key(&mut app, &bare(BareKey::Esc));
        assert!(!app.show_help);
        // Enter on a section row toggles it instead of opening an agent.
        app.select_last();
        assert_eq!(process_key(&mut app, &bare(BareKey::Enter)).1, None);
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Section { expanded: true, .. })
        ));
        process_key(&mut app, &key('h'));
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Section {
                expanded: false,
                ..
            })
        ));
    }
}
