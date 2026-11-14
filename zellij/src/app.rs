//! Panel state: configuration, rows, sections, selection, notices and the
//! pending stop/rename prompts.

use crate::model::{age_seconds, Agent, AgentGroup, AgentList};
use std::collections::{BTreeMap, BTreeSet};

pub const NOTICE_REFRESHES: usize = 5;
pub const SPINNER: [&str; 10] = [
    "\u{280b}", "\u{2819}", "\u{2839}", "\u{2838}", "\u{283c}", "\u{2834}", "\u{2826}", "\u{2827}",
    "\u{2807}", "\u{280f}",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum QuietAge {
    Day,
    TwoDays,
    Week,
}

impl QuietAge {
    pub const ALL: [QuietAge; 3] = [QuietAge::Day, QuietAge::TwoDays, QuietAge::Week];

    pub fn label(self) -> &'static str {
        match self {
            QuietAge::Day => "< 24h",
            QuietAge::TwoDays => "< 48h",
            QuietAge::Week => "< 7d",
        }
    }

    /// `None` means older than seven days: not shown. An unknown age goes
    /// into the first section rather than disappearing.
    pub fn of(age: Option<i64>) -> Option<QuietAge> {
        match age {
            None => Some(QuietAge::Day),
            Some(s) if s < 86_400 => Some(QuietAge::Day),
            Some(s) if s < 172_800 => Some(QuietAge::TwoDays),
            Some(s) if s < 604_800 => Some(QuietAge::Week),
            Some(_) => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ListItem {
    Agent(usize),
    Section {
        age: QuietAge,
        count: usize,
        expanded: bool,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Transport {
    Poll,
    Pipe,
}

impl Transport {
    pub fn as_str(self) -> &'static str {
        match self {
            Transport::Poll => "poll",
            Transport::Pipe => "pipe",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Connection {
    Connecting,
    Connected(Transport),
    ServerUnavailable,
    Error(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PendingStop {
    pub agent_id: String,
    pub title: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PendingRename {
    pub agent_id: String,
    pub text: String,
}

/// A search: the query, and whether it is still being typed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Search {
    pub text: String,
    pub typing: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum SelectionKey {
    Agent(String),
    Section(QuietAge),
}

// ---- configuration ----

pub const DEFAULT_GUEST_SESSION: &str = "noc";
pub const DEFAULT_AWAY_KEY: &str = "Ctrl Alt Shift F11";
pub const DEFAULT_HOME_KEY: &str = "Ctrl Alt Shift F12";

/// The plugin block's settings (`plugin location="…" { … }` in the host layout).
#[derive(Debug, Clone)]
pub struct Config {
    /// The `noc` executable. A bare name is looked up on Zellij's `PATH`.
    pub noc_bin: String,
    pub nerd_icons: bool,
    pub transport: Transport,
    pub pipe_name: String,
    pub poll_interval_ms: f64,
    /// The inner session the panel opens, focuses and follows agents in.
    pub guest_session: String,
    /// The inner session's normal theme, and the one it wears while the
    /// panel has the keyboard. Both or neither.
    pub home_theme: Option<String>,
    pub away_theme: Option<String>,
    /// The keys the inner session binds to the theme relay.
    pub away_key: String,
    pub home_key: String,
    /// Whether the panel asks GitHub for the latest release.
    pub update_check: bool,
    /// Follow the Zellij theme instead of the built-in colours.
    pub zellij_colors: bool,
    /// Blank the titles of the inner session's panes.
    pub hide_pane_titles: bool,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            noc_bin: "noc".to_string(),
            nerd_icons: false,
            transport: Transport::Poll,
            pipe_name: "zellij-noc.registry".to_string(),
            poll_interval_ms: 1000.0,
            guest_session: DEFAULT_GUEST_SESSION.to_string(),
            home_theme: None,
            away_theme: None,
            away_key: DEFAULT_AWAY_KEY.to_string(),
            home_key: DEFAULT_HOME_KEY.to_string(),
            update_check: true,
            zellij_colors: false,
            hide_pane_titles: false,
        }
    }
}

/// A theme or session name that is safe to put inside generated KDL and on
/// a command line.
pub fn is_plain_name(name: &str) -> bool {
    !name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// The bytes a terminal sends for a function key with modifiers, e.g.
/// `Ctrl Alt Shift F11` is `ESC [ 2 3 ; 8 ~`. Only `F1`..`F12` with any of
/// `Ctrl`, `Alt` and `Shift` are accepted: these are keys nobody types, which
/// is what the theme relay wants.
pub fn encode_key(spec: &str) -> Option<Vec<u8>> {
    let mut modifiers = 0u8;
    let mut key = None;
    for token in spec.split_whitespace() {
        match token {
            "Shift" => modifiers |= 1,
            "Alt" => modifiers |= 2,
            "Ctrl" => modifiers |= 4,
            _ if key.is_none() => key = Some(token),
            _ => return None,
        }
    }
    let number: u8 = key?.strip_prefix('F')?.parse().ok()?;
    let parameter = modifiers + 1;
    let text = match number {
        1..=4 => {
            let letter = b"PQRS"[usize::from(number - 1)] as char;
            if modifiers == 0 {
                format!("\u{1b}O{letter}")
            } else {
                format!("\u{1b}[1;{parameter}{letter}")
            }
        }
        5..=12 => {
            let code = [15, 17, 18, 19, 20, 21, 23, 24][usize::from(number - 5)];
            if modifiers == 0 {
                format!("\u{1b}[{code}~")
            } else {
                format!("\u{1b}[{code};{parameter}~")
            }
        }
        _ => return None,
    };
    Some(text.into_bytes())
}

impl Config {
    pub fn from_map(map: &BTreeMap<String, String>) -> Config {
        let mut config = Config::default();
        let get = |key: &str| -> Option<&str> {
            map.get(key).map(|v| v.trim()).filter(|v| !v.is_empty())
        };
        if let Some(v) = get("noc_bin") {
            config.noc_bin = v.to_string();
        }
        config.nerd_icons = get("icons") == Some("nerd");
        if get("transport") == Some("pipe") {
            config.transport = Transport::Pipe;
        }
        if let Some(v) = get("pipe_name") {
            config.pipe_name = v.to_string();
        }
        if let Some(ms) = get("poll_interval_ms").and_then(|v| v.parse::<f64>().ok()) {
            if ms.is_finite() {
                config.poll_interval_ms = ms.clamp(250.0, 60_000.0);
            }
        }
        if let Some(v) = get("guest_session") {
            config.guest_session = v.to_string();
        }
        config.home_theme = get("home_theme").map(str::to_string);
        config.away_theme = get("away_theme").map(str::to_string);
        if let Some(v) = get("away_key") {
            config.away_key = v.to_string();
        }
        if let Some(v) = get("home_key") {
            config.home_key = v.to_string();
        }
        config.update_check = get("update_check") != Some("false");
        config.zellij_colors = get("colors") == Some("zellij");
        config.hide_pane_titles = get("pane_titles") == Some("hide");
        config
    }

    pub fn poll_interval_secs(&self) -> f64 {
        self.poll_interval_ms / 1000.0
    }

    /// Whether the inner session changes theme with the keyboard.
    pub fn themes_enabled(&self) -> bool {
        self.problems().is_empty() && self.home_theme.is_some() && self.away_theme.is_some()
    }

    /// What is wrong with the settings, worded for the panel. Empty when
    /// they are usable. A problem never stops the agent list from working;
    /// it only turns the theme switch off.
    pub fn problems(&self) -> Vec<String> {
        let mut problems = Vec::new();
        if !is_plain_name(&self.guest_session) {
            problems.push(format!(
                "guest_session {:?} is not a plain session name",
                self.guest_session
            ));
        }
        match (&self.home_theme, &self.away_theme) {
            (None, None) => return problems,
            (Some(_), None) => problems.push("home_theme is set but away_theme is not".into()),
            (None, Some(_)) => problems.push("away_theme is set but home_theme is not".into()),
            (Some(home), Some(away)) => {
                for (key, name) in [("home_theme", home), ("away_theme", away)] {
                    if !is_plain_name(name) {
                        problems.push(format!("{key} {name:?} is not a plain theme name"));
                    }
                }
                if home == away {
                    problems.push("home_theme and away_theme are the same theme".into());
                }
            }
        }
        for (key, spec) in [("away_key", &self.away_key), ("home_key", &self.home_key)] {
            if encode_key(spec).is_none() {
                problems.push(format!(
                    "{key} {spec:?} is not a function key such as \"Ctrl Alt Shift F11\""
                ));
            }
        }
        if self.away_key == self.home_key {
            problems.push("away_key and home_key are the same key".into());
        }
        problems
    }
}

// ---- panel state ----

#[derive(Debug)]
pub struct ZellijNoc {
    pub config: Config,
    pub agents: Vec<Agent>,
    pub items: Vec<ListItem>,
    pub selected_index: usize,
    pub generated_at: String,
    pub connection: Connection,
    pub notice: Option<String>,
    notice_age: usize,
    /// A settings problem, shown until the plugin is reloaded.
    pub config_problem: Option<String>,
    /// Why the inner session cannot be reached right now.
    pub guest_problem: Option<String>,
    /// A release newer than this build, as `vX.Y.Z`.
    pub update: Option<String>,
    pub pending_stop: Option<PendingStop>,
    pub pending_rename: Option<PendingRename>,
    pub search: Option<Search>,
    /// The list holds search results: every match is a row, Quiet agents
    /// are not folded into sections, and none is too old to show.
    pub flat: bool,
    pub show_details: bool,
    pub show_help: bool,
    /// The status box is open.
    pub show_info: bool,
    /// The row the status line was last drawn on, for clicks.
    pub status_row: Option<usize>,
    pub pending_g: bool,
    pub spinner_frame: usize,
    pub last_render_rows: usize,
    expanded: BTreeSet<QuietAge>,
    stopped: BTreeSet<String>,
}

impl ZellijNoc {
    pub fn new(config: Config) -> ZellijNoc {
        ZellijNoc {
            config,
            agents: Vec::new(),
            items: Vec::new(),
            selected_index: 0,
            generated_at: String::new(),
            connection: Connection::Connecting,
            notice: None,
            notice_age: 0,
            config_problem: None,
            guest_problem: None,
            update: None,
            pending_stop: None,
            pending_rename: None,
            search: None,
            flat: false,
            show_details: true,
            show_help: false,
            show_info: false,
            status_row: None,
            pending_g: false,
            spinner_frame: 0,
            last_render_rows: 20,
            expanded: BTreeSet::new(),
            stopped: BTreeSet::new(),
        }
    }

    // ---- snapshots ----

    pub fn apply_snapshot(&mut self, snapshot: AgentList, source: Transport) {
        let selection = self.selection_key();
        let previous_index = self.selected_index;
        let present: BTreeSet<&str> = snapshot.agents.iter().map(|a| a.id.as_str()).collect();
        self.stopped.retain(|id| present.contains(id.as_str()));
        self.generated_at = snapshot.generated_at;
        let stopped = &self.stopped;
        self.agents = snapshot
            .agents
            .into_iter()
            .filter(|a| !stopped.contains(&a.id))
            .collect();
        self.connection = Connection::Connected(source);
        self.flat = self.search_text().is_some();
        if self.notice.is_some() {
            self.notice_age += 1;
            if self.notice_age >= NOTICE_REFRESHES {
                self.notice = None;
            }
        }
        self.rebuild_items();
        self.restore_selection(selection, previous_index);
    }

    pub fn mark_stopped(&mut self, id: &str) {
        let selection = self.selection_key();
        let previous_index = self.selected_index;
        self.stopped.insert(id.to_string());
        self.agents.retain(|a| a.id != id);
        self.rebuild_items();
        self.restore_selection(selection, previous_index);
    }

    pub fn set_error(&mut self, error: String) {
        self.connection = Connection::Error(error);
    }

    pub fn set_server_unavailable(&mut self) {
        self.connection = Connection::ServerUnavailable;
    }

    // ---- rows ----

    fn quiet_age(&self, agent: &Agent) -> Option<QuietAge> {
        QuietAge::of(age_seconds(agent.recency(), &self.generated_at))
    }

    pub fn rebuild_items(&mut self) {
        let mut items = Vec::new();
        let mut sections: BTreeMap<QuietAge, Vec<usize>> = BTreeMap::new();
        for (index, agent) in self.agents.iter().enumerate() {
            if self.flat {
                items.push(ListItem::Agent(index));
            } else if agent.group() == AgentGroup::Quiet {
                if let Some(age) = self.quiet_age(agent) {
                    sections.entry(age).or_default().push(index);
                }
            } else {
                items.push(ListItem::Agent(index));
            }
        }
        for age in QuietAge::ALL {
            let Some(members) = sections.get(&age) else {
                continue;
            };
            let expanded = self.expanded.contains(&age);
            items.push(ListItem::Section {
                age,
                count: members.len(),
                expanded,
            });
            if expanded {
                items.extend(members.iter().map(|&i| ListItem::Agent(i)));
            }
        }
        self.items = items;
    }

    fn selection_key(&self) -> Option<SelectionKey> {
        match self.items.get(self.selected_index)? {
            ListItem::Agent(i) => self
                .agents
                .get(*i)
                .map(|a| SelectionKey::Agent(a.id.clone())),
            ListItem::Section { age, .. } => Some(SelectionKey::Section(*age)),
        }
    }

    fn position_of(&self, key: &SelectionKey) -> Option<usize> {
        self.items.iter().position(|item| match (item, key) {
            (ListItem::Agent(i), SelectionKey::Agent(id)) => {
                self.agents.get(*i).is_some_and(|a| &a.id == id)
            }
            (ListItem::Section { age, .. }, SelectionKey::Section(want)) => age == want,
            _ => false,
        })
    }

    fn restore_selection(&mut self, key: Option<SelectionKey>, previous_index: usize) {
        self.selected_index = key
            .and_then(|k| self.position_of(&k))
            .unwrap_or_else(|| previous_index.min(self.items.len().saturating_sub(1)));
    }

    pub fn selected_item(&self) -> Option<ListItem> {
        self.items.get(self.selected_index).copied()
    }

    pub fn selected_agent(&self) -> Option<&Agent> {
        match self.selected_item()? {
            ListItem::Agent(i) => self.agents.get(i),
            ListItem::Section { .. } => None,
        }
    }

    pub fn find_agent(&self, id: &str) -> Option<&Agent> {
        self.agents.iter().find(|a| a.id == id)
    }

    /// Select an agent by id, opening its Quiet section if needed. False if
    /// unknown or older than seven days.
    pub fn select_agent(&mut self, id: &str) -> bool {
        let Some(index) = self.agents.iter().position(|a| a.id == id) else {
            return false;
        };
        let target = SelectionKey::Agent(id.to_string());
        if self.position_of(&target).is_none() {
            let agent = &self.agents[index];
            let Some(age) = (agent.group() == AgentGroup::Quiet)
                .then(|| self.quiet_age(agent))
                .flatten()
            else {
                return false;
            };
            self.expanded.insert(age);
            self.rebuild_items();
        }
        let Some(position) = self.position_of(&target) else {
            return false;
        };
        self.selected_index = position;
        self.pending_g = false;
        true
    }

    // ---- sections ----

    pub fn toggle_selected_section(&mut self) -> bool {
        let Some(ListItem::Section { age, .. }) = self.selected_item() else {
            return false;
        };
        if !self.expanded.remove(&age) {
            self.expanded.insert(age);
        }
        self.rebuild_items();
        self.restore_selection(Some(SelectionKey::Section(age)), self.selected_index);
        true
    }

    pub fn collapse_selected(&mut self) {
        let age = match self.selected_item() {
            Some(ListItem::Section { age, .. }) => Some(age),
            Some(ListItem::Agent(i)) => self.agents.get(i).and_then(|agent| {
                (agent.group() == AgentGroup::Quiet)
                    .then(|| self.quiet_age(agent))
                    .flatten()
                    .filter(|age| self.expanded.contains(age))
            }),
            None => None,
        };
        let Some(age) = age else {
            return;
        };
        self.expanded.remove(&age);
        self.rebuild_items();
        self.restore_selection(Some(SelectionKey::Section(age)), self.selected_index);
    }

    // ---- spinner ----

    pub fn advance_spinner(&mut self) {
        self.spinner_frame = (self.spinner_frame + 1) % SPINNER.len();
    }

    pub fn has_working_agents(&self) -> bool {
        self.agents.iter().any(|a| a.display_state == "working")
    }

    // ---- movement ----

    pub fn move_selection(&mut self, delta: isize) {
        let last = self.items.len().saturating_sub(1);
        self.selected_index = self.selected_index.saturating_add_signed(delta).min(last);
        self.notice = None;
        self.pending_g = false;
    }

    pub fn move_half_page(&mut self, down: bool) {
        let step = (self.last_render_rows / 2).max(1) as isize;
        self.move_selection(if down { step } else { -step });
    }

    /// First `g` arms; the second selects the first row.
    pub fn press_g(&mut self) {
        if self.pending_g {
            self.pending_g = false;
            self.selected_index = 0;
            self.notice = None;
        } else {
            self.pending_g = true;
        }
    }

    pub fn select_last(&mut self) {
        self.selected_index = self.items.len().saturating_sub(1);
        self.pending_g = false;
        self.notice = None;
    }

    // ---- toggles and prompts ----

    pub fn toggle_details(&mut self) {
        self.show_details = !self.show_details;
        self.show_help = false;
    }

    pub fn toggle_help(&mut self) {
        self.show_help = !self.show_help;
    }

    pub fn toggle_info(&mut self) {
        self.show_info = !self.show_info;
    }

    /// A left click on a row of the panel. On the status line it opens or
    /// closes the status box; anywhere else it closes it. Returns whether
    /// anything changed.
    pub fn click(&mut self, row: isize) -> bool {
        let on_status = usize::try_from(row).ok() == self.status_row && self.status_row.is_some();
        if on_status {
            self.toggle_info();
            true
        } else {
            std::mem::take(&mut self.show_info)
        }
    }

    pub fn close_help(&mut self) {
        self.show_help = false;
    }

    /// The query the list should show results for, if there is one.
    pub fn search_text(&self) -> Option<&str> {
        self.search
            .as_ref()
            .map(|s| s.text.trim())
            .filter(|text| !text.is_empty())
    }

    /// Start typing a search, or go back to editing the one in effect.
    pub fn begin_search(&mut self) {
        match self.search.as_mut() {
            Some(search) => search.typing = true,
            None => {
                self.search = Some(Search {
                    text: String::new(),
                    typing: true,
                })
            }
        }
    }

    pub fn is_typing_search(&self) -> bool {
        self.search.as_ref().is_some_and(|s| s.typing)
    }

    pub fn begin_rename(&mut self) -> bool {
        let Some(agent) = self.selected_agent() else {
            return false;
        };
        self.pending_rename = Some(PendingRename {
            agent_id: agent.id.clone(),
            text: agent.title(),
        });
        true
    }

    pub fn set_notice(&mut self, text: impl Into<String>) {
        self.notice = Some(text.into());
        self.notice_age = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::parse_snapshot;

    const NOW: &str = "2026-09-24T12:00:00Z";

    fn list(agents: &str) -> AgentList {
        parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":{agents}}}"#
        ))
        .unwrap()
    }

    fn panel() -> ZellijNoc {
        ZellijNoc::new(Config::default())
    }

    fn row_ids(app: &ZellijNoc) -> Vec<String> {
        app.items
            .iter()
            .map(|item| match item {
                ListItem::Agent(i) => app.agents[*i].id.clone(),
                ListItem::Section {
                    age,
                    count,
                    expanded,
                } => {
                    format!(
                        "[{} {} {}]",
                        age.label(),
                        count,
                        if *expanded { "open" } else { "shut" }
                    )
                }
            })
            .collect()
    }

    fn working(ids: &[&str]) -> String {
        let rows: Vec<String> = ids
            .iter()
            .enumerate()
            .map(|(i, id)| {
                format!(
                    r#"{{"id":"{id}","name":"{id}","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:0{i}:00Z"}}"#
                )
            })
            .collect();
        format!("[{}]", rows.join(","))
    }

    #[test]
    fn preserves_selection_by_agent_id_across_snapshots() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a", "b", "c"])), Transport::Poll);
        assert_eq!(app.connection, Connection::Connected(Transport::Poll));
        assert_eq!(app.generated_at, NOW);
        let id = app.agents[2].id.clone();
        app.selected_index = 2;
        // The same agent moves to the front: the selection follows it.
        let mut agents = working(&["a", "b", "c"]);
        agents = agents
            .replace("11:02:00", "11:50:00")
            .replace("11:00:00", "11:01:00");
        app.apply_snapshot(list(&agents), Transport::Pipe);
        assert_eq!(app.connection, Connection::Connected(Transport::Pipe));
        assert_eq!(app.selected_agent().unwrap().id, id);
    }

    #[test]
    fn stopped_agents_stay_hidden_until_the_registry_drops_them() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a", "b"])), Transport::Poll);
        app.mark_stopped("a");
        assert_eq!(row_ids(&app), ["b"]);
        app.apply_snapshot(list(&working(&["a", "b"])), Transport::Poll);
        assert_eq!(row_ids(&app), ["b"], "still reported, still hidden");
        app.apply_snapshot(list(&working(&["b"])), Transport::Poll);
        app.apply_snapshot(list(&working(&["a", "b"])), Transport::Poll);
        assert_eq!(
            row_ids(&app).len(),
            2,
            "shown again after one snapshot without it"
        );
    }

    #[test]
    fn quiet_agents_fold_into_collapsed_time_sections() {
        let text = r#"[
            {"id":"w","name":"W","display_state":"working","bucket":"working"},
            {"id":"q1","name":"Today","display_state":"idle","bucket":"quiet","ended_at":"2026-09-24T07:00:00Z"},
            {"id":"q2","name":"Yesterday","display_state":"idle","bucket":"quiet","ended_at":"2026-09-23T06:00:00Z"},
            {"id":"q3","name":"Monday","display_state":"idle","bucket":"quiet","ended_at":"2026-09-21T08:00:00Z"},
            {"id":"q4","name":"Ancient","display_state":"idle","bucket":"quiet","ended_at":"2026-09-01T08:00:00Z"},
            {"id":"q5","name":"Unknown","display_state":"idle","bucket":"quiet"}]"#;
        let mut app = panel();
        app.apply_snapshot(list(text), Transport::Poll);
        assert_eq!(
            row_ids(&app),
            ["w", "[< 24h 2 shut]", "[< 48h 1 shut]", "[< 7d 1 shut]"],
            "unknown ages join the first section; week-old agents are not rows"
        );
        assert_eq!(app.agents.len(), 6, "old agents stay in the lookup list");
        assert!(
            !app.select_agent("q4"),
            "aged-out agents cannot be selected"
        );
        // Opening a section by selecting a member.
        assert!(app.select_agent("q2"));
        assert_eq!(
            row_ids(&app),
            [
                "w",
                "[< 24h 2 shut]",
                "[< 48h 1 open]",
                "q2",
                "[< 7d 1 shut]"
            ]
        );
        assert_eq!(app.selected_agent().unwrap().id, "q2");
        // h on a member closes its section and selects the section row.
        app.collapse_selected();
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Section {
                age: QuietAge::TwoDays,
                ..
            })
        ));
        // Enter toggles the section row, keeping the selection on it.
        assert!(app.toggle_selected_section());
        assert_eq!(row_ids(&app)[2], "[< 48h 1 open]");
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Section { .. })
        ));
        // Open state persists across snapshots.
        app.apply_snapshot(list(text), Transport::Poll);
        assert_eq!(row_ids(&app)[2], "[< 48h 1 open]");
        app.collapse_selected();
        assert_eq!(row_ids(&app)[2], "[< 48h 1 shut]");
    }

    #[test]
    fn section_open_state_survives_a_section_emptying() {
        let one = r#"[{"id":"q","name":"Q","display_state":"idle","bucket":"quiet","ended_at":"2026-09-24T07:00:00Z"}]"#;
        let mut app = panel();
        app.apply_snapshot(list(one), Transport::Poll);
        assert!(app.select_agent("q"));
        app.apply_snapshot(list("[]"), Transport::Poll);
        assert!(app.items.is_empty());
        app.apply_snapshot(list(one), Transport::Poll);
        assert_eq!(row_ids(&app), ["[< 24h 1 open]", "q"]);
    }

    #[test]
    fn selection_falls_back_to_the_old_index_when_the_row_disappears() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a", "b", "c"])), Transport::Poll);
        app.selected_index = 1;
        let gone = app.agents[1].id.clone();
        app.apply_snapshot(
            list(
                &working(&["a", "b", "c"]).replace(&format!("\"id\":\"{gone}\""), "\"id\":\"zz\""),
            ),
            Transport::Poll,
        );
        assert_eq!(app.selected_index, 1);
        app.selected_index = 2;
        app.apply_snapshot(list(&working(&["a"])), Transport::Poll);
        assert_eq!(app.selected_index, 0);
        app.apply_snapshot(list("[]"), Transport::Poll);
        assert_eq!(app.selected_index, 0);
    }

    #[test]
    fn notices_survive_a_few_refreshes() {
        let mut app = panel();
        app.set_notice("hello");
        for _ in 0..4 {
            app.apply_snapshot(list("[]"), Transport::Poll);
            assert_eq!(app.notice.as_deref(), Some("hello"));
        }
        app.apply_snapshot(list("[]"), Transport::Poll);
        assert_eq!(app.notice, None);
    }

    #[test]
    fn selects_listed_agents_by_id_only() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a", "b"])), Transport::Poll);
        assert!(app.select_agent("b"));
        assert_eq!(app.selected_agent().unwrap().id, "b");
        assert!(!app.select_agent("missing"));
        assert_eq!(
            app.selected_agent().unwrap().id,
            "b",
            "failure leaves it alone"
        );
        app.pending_g = true;
        assert!(app.select_agent("a"));
        assert!(!app.pending_g);
    }

    #[test]
    fn navigation_clamps_and_supports_gg_and_capital_g() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a", "b", "c"])), Transport::Poll);
        app.move_selection(-1);
        assert_eq!(app.selected_index, 0);
        app.move_selection(1);
        app.move_selection(1);
        app.move_selection(1);
        assert_eq!(app.selected_index, 2);
        app.set_notice("x");
        app.select_last();
        assert_eq!(app.selected_index, 2);
        assert_eq!(app.notice, None);
        app.press_g();
        assert!(app.pending_g);
        assert_eq!(app.selected_index, 2);
        app.press_g();
        assert_eq!(app.selected_index, 0);
        assert!(!app.pending_g);
        app.last_render_rows = 4;
        app.move_half_page(true);
        assert_eq!(app.selected_index, 2);
        app.move_half_page(false);
        assert_eq!(app.selected_index, 0);
        let mut empty = panel();
        empty.select_last();
        empty.move_selection(1);
        assert_eq!(empty.selected_index, 0);
    }

    fn map(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn parses_bounded_poll_configuration() {
        assert_eq!(Config::from_map(&map(&[])).poll_interval_ms, 1000.0);
        assert_eq!(
            Config::from_map(&map(&[("poll_interval_ms", "50")])).poll_interval_ms,
            250.0
        );
        assert_eq!(
            Config::from_map(&map(&[("poll_interval_ms", "999999")])).poll_interval_ms,
            60_000.0
        );
        assert_eq!(
            Config::from_map(&map(&[("poll_interval_ms", " 1500 ")])).poll_interval_secs(),
            1.5
        );
        assert_eq!(
            Config::from_map(&map(&[("poll_interval_ms", "fast")])).poll_interval_ms,
            1000.0
        );
        assert_eq!(
            Config::from_map(&map(&[("poll_interval_ms", "NaN")])).poll_interval_ms,
            1000.0
        );
        let defaults = Config::from_map(&map(&[("noc_bin", "  "), ("pipe_name", "")]));
        assert_eq!(defaults.noc_bin, "noc");
        assert_eq!(defaults.pipe_name, "zellij-noc.registry");
        assert_eq!(defaults.guest_session, "noc");
        assert_eq!(defaults.away_key, "Ctrl Alt Shift F11");
        assert_eq!(defaults.home_key, "Ctrl Alt Shift F12");
        assert!(defaults.update_check);
        assert!(!defaults.zellij_colors);
        assert!(!Config::from_map(&map(&[("colors", "herdr")])).zellij_colors);
        assert!(Config::from_map(&map(&[("colors", "zellij")])).zellij_colors);
        assert!(!defaults.hide_pane_titles);
        assert!(!Config::from_map(&map(&[("pane_titles", "show")])).hide_pane_titles);
        assert!(Config::from_map(&map(&[("pane_titles", "hide")])).hide_pane_titles);
        assert!(!Config::from_map(&map(&[("update_check", " false ")])).update_check);
        assert!(Config::from_map(&map(&[("update_check", "no")])).update_check);
        assert!(defaults.problems().is_empty() && !defaults.themes_enabled());
        let custom = Config::from_map(&map(&[
            ("noc_bin", "/opt/noc"),
            ("icons", "nerd"),
            ("transport", "pipe"),
            ("guest_session", " inner "),
            ("home_theme", "nord"),
            ("away_theme", "nord-away"),
        ]));
        assert_eq!(custom.noc_bin, "/opt/noc");
        assert!(custom.nerd_icons);
        assert_eq!(custom.transport, Transport::Pipe);
        assert_eq!(custom.guest_session, "inner");
        assert!(custom.problems().is_empty() && custom.themes_enabled());
        assert!(!Config::from_map(&map(&[("icons", "unicode")])).nerd_icons);
    }

    #[test]
    fn reports_unusable_settings_in_plain_words() {
        let problems = |pairs: &[(&str, &str)]| Config::from_map(&map(pairs)).problems();
        assert_eq!(
            problems(&[("home_theme", "nord")]),
            ["home_theme is set but away_theme is not"]
        );
        assert_eq!(
            problems(&[("away_theme", "nord-away")]),
            ["away_theme is set but home_theme is not"]
        );
        assert_eq!(
            problems(&[("home_theme", "nord"), ("away_theme", "nord")]),
            ["home_theme and away_theme are the same theme"]
        );
        assert_eq!(
            problems(&[("home_theme", "a\" { Quit; }"), ("away_theme", "b")]),
            ["home_theme \"a\\\" { Quit; }\" is not a plain theme name"]
        );
        assert_eq!(
            problems(&[
                ("home_theme", "a"),
                ("away_theme", "b"),
                ("away_key", "Alt o")
            ]),
            ["away_key \"Alt o\" is not a function key such as \"Ctrl Alt Shift F11\""]
        );
        assert_eq!(
            problems(&[
                ("home_theme", "a"),
                ("away_theme", "b"),
                ("away_key", "Alt F5"),
                ("home_key", "Alt F5")
            ]),
            ["away_key and home_key are the same key"]
        );
        assert_eq!(
            problems(&[("guest_session", "my session")]),
            ["guest_session \"my session\" is not a plain session name"]
        );
        // A bad key is only a problem when the theme switch is in use.
        assert!(problems(&[("away_key", "nonsense")]).is_empty());
        let broken = Config::from_map(&map(&[("home_theme", "nord")]));
        assert!(!broken.themes_enabled());
    }

    #[test]
    fn encodes_function_keys_the_way_a_terminal_sends_them() {
        assert_eq!(
            encode_key("Ctrl Alt Shift F11").unwrap(),
            [27, 91, 50, 51, 59, 56, 126]
        );
        assert_eq!(
            encode_key("Ctrl Alt Shift F12").unwrap(),
            [27, 91, 50, 52, 59, 56, 126]
        );
        assert_eq!(encode_key("Shift  F5").unwrap(), b"\x1b[15;2~");
        assert_eq!(encode_key("Alt F1").unwrap(), b"\x1b[1;3P");
        assert_eq!(encode_key("Ctrl F4").unwrap(), b"\x1b[1;5S");
        assert_eq!(encode_key("F2").unwrap(), b"\x1bOQ");
        assert_eq!(encode_key("F10").unwrap(), b"\x1b[21~");
        // Modifier order does not matter.
        assert_eq!(
            encode_key("Shift Ctrl Alt F11"),
            encode_key("Ctrl Alt Shift F11")
        );
        for bad in [
            "",
            "Ctrl",
            "Alt o",
            "F0",
            "F13",
            "Ctrl F11 F12",
            "Super F1",
            "f11",
        ] {
            assert_eq!(encode_key(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn a_new_panel_starts_with_the_documented_defaults() {
        let app = panel();
        assert_eq!(app.selected_index, 0);
        assert!(app.show_details && !app.show_help);
        assert_eq!(app.connection, Connection::Connecting);
        assert!(app.notice.is_none() && app.pending_stop.is_none() && app.pending_rename.is_none());
        assert_eq!((app.spinner_frame, app.last_render_rows), (0, 20));
    }

    #[test]
    fn the_spinner_cycles_and_only_working_agents_need_redraws() {
        let mut app = panel();
        app.apply_snapshot(list(&working(&["a"])), Transport::Poll);
        assert!(app.has_working_agents());
        for _ in 0..10 {
            app.advance_spinner();
        }
        assert_eq!(app.spinner_frame, 0);
        app.apply_snapshot(list("[]"), Transport::Poll);
        assert!(!app.has_working_agents());
    }
}
