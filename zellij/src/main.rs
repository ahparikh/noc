//! `zellij-noc`: the Zellij frontend for noc.
//!
//! The plugin runs as a sidebar panel in an outer "host" session, beside one
//! pane that holds the user's own Zellij sessions attached nested, one session
//! per team, one on show at a time. One WASM build serves four roles, chosen
//! by the `role` configuration: the panel (default), a floating details
//! window, a relay that runs inside each session, and each session's tab bar.

mod app;
mod bar;
mod details;
mod guest;
mod model;
mod panel;
mod panes;
mod update;
mod view;

use app::{Config, Connection, Transport, ZellijNoc};
use details::DetailsState;
use guest::{GuestState, RelayState};
use panes::{manifest_panes, ScanPane};
use std::collections::BTreeMap;
use std::io::Write;
use std::time::{SystemTime, UNIX_EPOCH};
use view::Theme;
use zellij_tile::prelude::*;

// ---- constants ----

pub const KEY: &str = "zellij-noc";
pub const KEY_AGENT: &str = "zellij-noc-agent";
pub const KEY_TITLE: &str = "zellij-noc-title";
pub const KEY_QUERY: &str = "zellij-noc-query";

pub const CTX_SNAPSHOT: &str = "agent-snapshot";
pub const CTX_DETAILS: &str = "agent-details";
pub const CTX_STOP: &str = "stop-agent";
pub const CTX_RENAME: &str = "rename-agent";
pub const CTX_TEAM_JOIN: &str = "join-team";
pub const CTX_TEAM_LEAVE: &str = "leave-team";

/// Outer session: hand the keyboard to the panel, or back to the inner session.
pub const PIPE_GRAB: &str = "zellij-noc.grab";
/// Inner session: watch for a panel, or switch between the home and away
/// themes.
pub const PIPE_RELAY: &str = "zellij-noc.relay";
/// Outer session: the registry changed, from the panel's `noc watch`.
pub const PIPE_CHANGED: &str = "zellij-noc.changed";
/// Outer session: the inner session's panes, tabs and focus, from its relay.
pub const PIPE_GUEST: &str = "zellij-noc.guest";

pub const PANEL_TICK_SECONDS: f64 = 0.25;
pub const GRAB_WINDOW: f64 = 2.0;

pub fn ctx(kind: &str) -> BTreeMap<String, String> {
    BTreeMap::from([(KEY.to_string(), kind.to_string())])
}

pub fn now_secs() -> f64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs_f64())
        .unwrap_or(0.0)
}

/// Every role asks for the same set: Zellij records one grant per plugin
/// location, as the set its latest instance asked for.
pub fn permissions() -> Vec<PermissionType> {
    vec![
        PermissionType::ReadApplicationState,
        PermissionType::ChangeApplicationState,
        PermissionType::OpenTerminalsOrPlugins,
        PermissionType::Reconfigure,
        PermissionType::RunCommands,
        PermissionType::MessageAndLaunchOtherPlugins,
    ]
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum Role {
    #[default]
    Panel,
    Details,
    Relay,
    Bar,
}

pub struct State {
    pub role: Role,
    pub plugin_id: u32,
    pub config: Config,
    pub app: ZellijNoc,
    pub theme: Theme,
    pub permissions_granted: bool,
    // polling
    pub command_in_flight: bool,
    pub next_refresh: Option<f64>,
    pub next_update_check: Option<f64>,
    /// The release check in flight was asked for with `u`.
    pub update_asked: bool,
    pub tick_armed: bool,
    // what Zellij last told us about the host session
    pub tabs: Vec<TabInfo>,
    pub panes: Vec<ScanPane>,
    pub own_tab_position: Option<usize>,
    pub panel_url: Option<String>,
    pub session_name: Option<String>,
    // panel
    pub followed_agent: Option<String>,
    pub last_cols: Option<usize>,
    pub details_window: Option<u32>,
    pub pipe_buffer: String,
    pub selectable_set: Option<bool>,
    pub grab_until: f64,
    pub guest_had_keyboard: Option<bool>,
    /// The `noc watch` that says when the registry changed is running.
    pub registry_watching: bool,
    pub registry_watch_retry: f64,
    /// The registry changed while a read was in flight.
    pub registry_changed: bool,
    // roles
    pub guest: GuestState,
    pub relay: RelayState,
    pub details: DetailsState,
    pub bar: bar::BarState,
}

impl Default for State {
    fn default() -> Self {
        let config = Config::default();
        State {
            role: Role::Panel,
            plugin_id: 0,
            app: ZellijNoc::new(config.clone()),
            config,
            theme: Theme::default(),
            permissions_granted: false,
            command_in_flight: false,
            next_refresh: None,
            next_update_check: None,
            update_asked: false,
            tick_armed: false,
            tabs: Vec::new(),
            panes: Vec::new(),
            own_tab_position: None,
            panel_url: None,
            session_name: None,
            followed_agent: None,
            last_cols: None,
            details_window: None,
            pipe_buffer: String::new(),
            selectable_set: None,
            grab_until: 0.0,
            guest_had_keyboard: None,
            registry_watching: false,
            registry_watch_retry: 0.0,
            registry_changed: false,
            guest: GuestState::default(),
            relay: RelayState::default(),
            details: DetailsState::default(),
            bar: bar::BarState::default(),
        }
    }
}

impl ZellijPlugin for State {
    fn load(&mut self, configuration: BTreeMap<String, String>) {
        let config = Config::from_map(&configuration);
        self.role = match configuration.get("role").map(|r| r.trim()) {
            Some("relay") => Role::Relay,
            Some("details") => Role::Details,
            Some("bar") => Role::Bar,
            _ => Role::Panel,
        };
        // Until a team's session is shown, the session on show is the inner
        // session itself.
        self.guest.session = config.guest_session.clone();
        self.plugin_id = get_plugin_ids().plugin_id;
        if self.role == Role::Details {
            self.details.agent_id = configuration
                .get("agent_id")
                .map(|v| v.trim().to_string())
                .unwrap_or_default();
        }
        self.app = ZellijNoc::new(config.clone());
        self.app.config_problem = config.problems().into_iter().next();
        self.config = config;
        request_permission(&permissions());
        if self.role == Role::Relay {
            subscribe(&[
                EventType::CommandChanged,
                EventType::ListClients,
                EventType::PaneUpdate,
                EventType::PermissionRequestResult,
                EventType::RunCommandResult,
                EventType::TabUpdate,
            ]);
            return;
        }
        if self.role == Role::Bar {
            set_selectable(false);
            subscribe(&[
                EventType::ModeUpdate,
                EventType::Mouse,
                EventType::PermissionRequestResult,
                EventType::TabUpdate,
            ]);
            return;
        }
        subscribe(&[
            EventType::Key,
            EventType::ModeUpdate,
            EventType::Mouse,
            EventType::NestedSessionModeUpdate,
            EventType::PaneUpdate,
            EventType::PermissionRequestResult,
            EventType::RunCommandResult,
            EventType::TabUpdate,
            EventType::Timer,
            EventType::Visible,
        ]);
    }

    fn update(&mut self, event: Event) -> bool {
        match self.role {
            Role::Relay => {
                self.update_relay(event);
                false
            }
            Role::Details => self.update_details(event),
            Role::Bar => self.update_bar(event),
            Role::Panel => self.update_panel(event),
        }
    }

    fn pipe(&mut self, message: PipeMessage) -> bool {
        match self.role {
            Role::Relay => {
                self.relay_pipe(&message);
                false
            }
            Role::Details | Role::Bar => false,
            Role::Panel => self.handle_pipe(message),
        }
    }

    fn render(&mut self, rows: usize, cols: usize) {
        let lines = match self.role {
            Role::Panel => {
                self.last_cols = Some(cols);
                view::render(&mut self.app, &self.theme, rows, cols)
            }
            Role::Details => view::render_details_window(
                self.details.agent.as_ref(),
                &self.details.connection,
                &self.details.generated_at,
                &self.theme,
                rows,
                cols,
            ),
            Role::Relay => Vec::new(),
            Role::Bar => self.render_bar(cols),
        };
        // No trailing newline: it would scroll the pane by one line.
        print!("{}", lines.join("\n"));
        let _ = std::io::stdout().flush();
    }
}

impl State {
    fn update_panel(&mut self, event: Event) -> bool {
        match event {
            Event::PermissionRequestResult(PermissionStatus::Granted) => self.on_granted(),
            Event::PermissionRequestResult(PermissionStatus::Denied) => self.on_denied(),
            Event::RunCommandResult(exit, stdout, stderr, context) => {
                self.on_command_result(exit, &stdout, &stderr, &context)
            }
            Event::ModeUpdate(info) => self.on_mode_update(&info),
            Event::TabUpdate(tabs) => {
                self.tabs = tabs;
                false
            }
            Event::PaneUpdate(manifest) => self.on_pane_update(&manifest),
            Event::Timer(secs) if (secs - guest::SWITCH_POLL_SECS).abs() < 0.001 => {
                self.on_switch_poll()
            }
            Event::Timer(_) => self.on_panel_tick(),
            // The session attached in the guest pane said something, as it
            // does when a client arrives in it.
            Event::NestedSessionModeUpdate { .. } => {
                self.permissions_granted && self.read_shown_session()
            }
            Event::Visible(true) => {
                self.refresh();
                false
            }
            Event::Key(key) => self.handle_key(&key),
            Event::Mouse(Mouse::LeftClick(row, _)) => self.app.click(row),
            _ => false,
        }
    }

    fn on_granted(&mut self) -> bool {
        self.permissions_granted = true;
        self.app.notice = None;
        rename_plugin_pane(self.plugin_id, panel::PANEL_TITLE);
        self.sync_panel_selectable();
        self.refresh();
        self.check_for_update();
        if self.config.pane_titles == app::PaneTitles::Name {
            run_command(
                &["/bin/sh", "-c", guest::HOST_INFO_SCRIPT],
                ctx(guest::CTX_HOST_INFO),
            );
        }
        if !self.tick_armed {
            self.tick_armed = true;
            set_timeout(PANEL_TICK_SECONDS);
        }
        true
    }

    fn on_denied(&mut self) -> bool {
        self.permissions_granted = false;
        self.app.connection = Connection::Error(
            "permissions denied: the panel cannot read the registry or open agents".to_string(),
        );
        true
    }

    fn on_command_result(
        &mut self,
        exit: Option<i32>,
        stdout: &[u8],
        stderr: &[u8],
        context: &BTreeMap<String, String>,
    ) -> bool {
        let Some(kind) = context.get(KEY).map(String::as_str) else {
            return false;
        };
        match kind {
            CTX_SNAPSHOT => {
                let asked = context.get(KEY_QUERY).map(String::as_str).unwrap_or("");
                if asked != self.app.search_text().unwrap_or_default() {
                    // The search changed while this ran: read again.
                    self.command_in_flight = false;
                    self.refresh();
                    return false;
                }
                self.on_snapshot_result(exit, stdout, stderr)
            }
            CTX_STOP => self.on_stop_result(exit, stderr, context),
            CTX_RENAME => self.on_rename_result(exit, stderr, context),
            CTX_TEAM_JOIN => self.on_team_result(true, exit, stderr, context),
            CTX_TEAM_LEAVE => self.on_team_result(false, exit, stderr, context),
            panel::CTX_REGISTRY_WATCH => self.on_registry_watch_ended(),
            guest::CTX_WATCH => self.on_guest_watch(exit),
            guest::CTX_OPEN => self.on_guest_open(exit, stdout, stderr, context),
            guest::CTX_SWITCH => self.on_guest_switch(exit, stderr),
            guest::CTX_HOST_INFO => {
                if exit == Some(0) {
                    self.guest.host = guest::parse_host_info(&String::from_utf8_lossy(stdout));
                }
                false
            }
            update::CTX_CHECK => self.on_update_result(exit, stdout),
            _ => false,
        }
    }

    fn on_mode_update(&mut self, info: &ModeInfo) -> bool {
        if self.config.zellij_colors {
            self.theme = Theme::from_mode_info(info);
        }
        if let Some(name) = &info.session_name {
            self.session_name = Some(name.clone());
        }
        true
    }

    fn on_pane_update(&mut self, manifest: &PaneManifest) -> bool {
        self.panes = manifest_panes(manifest);
        if let Some(own) = panes::find_plugin_pane(&self.panes, self.plugin_id) {
            self.own_tab_position = Some(own.tab_position);
            if let Some(url) = &own.plugin_url {
                self.panel_url = Some(url.clone());
            }
        }
        if !self.permissions_granted {
            return false;
        }
        self.sync_panel_selectable();
        self.signal_keyboard_moves();
        if self.panel_has_keyboard() {
            self.followed_agent = None;
        }
        self.follow_shown_session()
    }

    fn on_panel_tick(&mut self) -> bool {
        self.tick_armed = true;
        set_timeout(PANEL_TICK_SECONDS);
        self.sync_panel_selectable();
        let mut redraw = self.permissions_granted && self.read_shown_session();
        redraw |= self.watch_guest();
        self.watch_registry();
        let now = now_secs();
        if self.next_refresh.is_none_or(|t| now >= t) {
            self.refresh();
        }
        self.check_for_update();
        if self.app.has_working_agents() {
            self.app.advance_spinner();
            redraw = true;
        }
        redraw
    }

    // ---- pipes ----

    fn handle_pipe(&mut self, message: PipeMessage) -> bool {
        match message.name.as_str() {
            PIPE_GRAB => {
                self.toggle_keyboard();
                false
            }
            PIPE_CHANGED => {
                self.on_registry_changed();
                false
            }
            PIPE_GUEST => match message.payload {
                Some(payload) => self.on_guest_push(&payload),
                None => false,
            },
            name => {
                if self.config.transport != Transport::Pipe || name != self.config.pipe_name {
                    return false;
                }
                match message.payload {
                    Some(payload) => self.on_registry_payload(&payload),
                    None => false,
                }
            }
        }
    }

    fn on_registry_payload(&mut self, payload: &str) -> bool {
        let frames = panes::decode_pipe_frames(&mut self.pipe_buffer, payload);
        let mut changed = false;
        for frame in frames {
            match frame {
                Ok(snapshot) => self.app.apply_snapshot(snapshot, Transport::Pipe),
                Err(e) => self
                    .app
                    .set_error(format!("invalid registry snapshot: {e}")),
            }
            changed = true;
        }
        changed
    }
}

register_plugin!(State);
