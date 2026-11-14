//! Key handling and intents, polling, stop/rename, opening agents, and
//! keyboard ownership between the panel and the inner session.

use crate::app::{PendingStop, Transport, ZellijNoc};
use crate::guest::keyboard_signal;
use crate::model::{parse_snapshot, Agent, AgentGroup};
use crate::panes::{
    centered_frame_title, compact_error, focused_pane, is_server_unavailable,
    other_running_session, tab_panes, ScanPane,
};
use crate::{ctx, now_secs, Role, State, CTX_RENAME, CTX_STOP, GRAB_WINDOW, KEY_AGENT, KEY_TITLE};
use std::collections::BTreeMap;
use zellij_tile::prelude::*;

pub const PANEL_TITLE: &str = "NOC";

// ---- keys -> intents (pure) ----

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Intent {
    Open,
    Pane {
        stacked: bool,
    },
    Worktree,
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

/// Apply one key to the panel state. Returns whether to redraw and the
/// intent (if any) the host must act on.
pub fn process_key(app: &mut ZellijNoc, key: &KeyWithModifier) -> (bool, Option<Intent>) {
    if app.pending_rename.is_some() {
        return (true, handle_rename_key(app, key));
    }
    if app.is_typing_search() {
        return (true, handle_search_key(app, key));
    }
    let modifiers = &key.key_modifiers;
    if modifiers.contains(&KeyModifier::Ctrl) {
        return match key.bare_key {
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
        BareKey::Char('d') => app.toggle_details(),
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
        BareKey::Char('p') => intent = Some(Intent::Pane { stacked: false }),
        BareKey::Char('s') => intent = Some(Intent::Pane { stacked: true }),
        BareKey::Char('w') => intent = Some(Intent::Worktree),
        BareKey::Char('D') => intent = Some(Intent::Details),
        BareKey::Char('r') => intent = Some(Intent::Refresh),
        BareKey::Char('u') => intent = Some(Intent::CheckUpdate),
        BareKey::Char('x') => request_stop(app),
        BareKey::Char('n') => {
            let began = app.begin_rename();
            if !began {
                app.set_notice("no agent is selected");
            }
        }
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

    pub fn on_snapshot_result(&mut self, exit: Option<i32>, stdout: &[u8], stderr: &[u8]) -> bool {
        self.command_in_flight = false;
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
        focused_pane(&in_tab, self.floating_visible(tab)).cloned()
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

    /// Another running Zellij session (not the host, not the inner session)
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
            Some(PaneId::Plugin(id)) => self.details_window = Some(id),
            _ => self
                .app
                .set_notice("Zellij could not open the details window"),
        }
    }

    // ---- panel title ----

    /// `NOC`, centred in the frame once the width is known, whether or not
    /// the panel has the keyboard. Renames only when the width changed.
    pub fn sync_panel_title(&mut self) {
        if !self.permissions_granted || self.role != Role::Panel {
            return;
        }
        if self.panel_title_state == Some(self.last_cols) {
            return;
        }
        self.panel_title_state = Some(self.last_cols);
        let title = match self.last_cols {
            Some(cols) => centered_frame_title(PANEL_TITLE, cols),
            None => PANEL_TITLE.to_string(),
        };
        rename_plugin_pane(self.plugin_id, title);
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
            ('D', Intent::Details),
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
            key('t'),
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
        process_key(&mut app, &key('d'));
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
