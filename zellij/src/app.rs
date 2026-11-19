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
    /// A team's heading when the list is grouped by team. `team` indexes
    /// `ZellijNoc::teams`.
    Team {
        team: usize,
        count: usize,
        expanded: bool,
        blocked: usize,
        working: usize,
        review: usize,
    },
    /// The Quiet agents of one team, folded into one row.
    TeamQuiet {
        team: usize,
        count: usize,
        expanded: bool,
    },
    /// The teams whose agents are all Quiet, folded into one row at the end
    /// of the team grouping. `count` is the number of teams.
    QuietTeams {
        count: usize,
        expanded: bool,
    },
}

/// How the list is grouped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GroupBy {
    /// Blocked, Working, Review, Quiet.
    Status,
    /// One group per team, then the agents in no team.
    Team,
}

impl GroupBy {
    pub fn as_str(self) -> &'static str {
        match self {
            GroupBy::Status => "status",
            GroupBy::Team => "team",
        }
    }
}

/// The heading of the group of agents that are in no team.
pub const NO_TEAM: &str = "no team";

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

/// The section of the panel that takes the keys.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum Focus {
    #[default]
    Agents,
    Sessions,
    Details,
}

/// How far `h` and `l` move the details sideways.
pub const DETAIL_SHIFT: usize = 4;

/// A running inner session, as a row of the Sessions section.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SessionRow {
    /// The Zellij session's name.
    pub name: String,
    /// The guest pane shows it.
    pub on_show: bool,
}

/// What a session's row says of its agents: how many are Blocked, Working
/// and in Review.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct SessionCounts {
    pub blocked: usize,
    pub working: usize,
    pub review: usize,
    /// Every agent of the session's team, the Quiet ones too.
    pub agents: usize,
}

/// The most session rows the section shows at once.
pub const SESSION_ROWS: usize = 5;

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

/// The prompt that puts an agent in a team.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PendingTeam {
    pub agent_id: String,
    pub text: String,
    /// What was typed before `Tab` completed it; repeated `Tab`s walk the
    /// teams that start with it.
    pub typed: Option<String>,
}

/// The longest team name `noc` accepts.
pub const TEAM_NAME_MAX: usize = 48;

/// Whether `c` may stand at position `at` of a team name: letters, digits,
/// `.`, `_` and `-`, the first neither `-` nor `.`.
pub fn is_team_char(c: char, at: usize) -> bool {
    (c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
        && !(at == 0 && matches!(c, '-' | '.'))
}

/// Whether `noc` takes `name` as a team name. `none` means "no team" there.
pub fn is_team_name(name: &str) -> bool {
    !name.is_empty()
        && name != "none"
        && name.chars().count() <= TEAM_NAME_MAX
        && name.chars().enumerate().all(|(at, c)| is_team_char(c, at))
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
    Team(String),
    TeamQuiet(String),
    QuietTeams,
}

// ---- configuration ----

pub const DEFAULT_GUEST_SESSION: &str = "noc";
pub const DEFAULT_AWAY_KEY: &str = "Ctrl Alt Shift F11";
pub const DEFAULT_HOME_KEY: &str = "Ctrl Alt Shift F12";

/// What the panel does with the titles of the inner session's panes.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PaneTitles {
    /// Leave them alone.
    Show,
    /// Blank them.
    Hide,
    /// Name each pane `<host> · <what runs in it>`.
    Name,
}

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
    /// The layout a team's session is created from: a name in the layout
    /// directory of the guest pane's client, or a path.
    pub team_layout: String,
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
    pub pane_titles: PaneTitles,
    /// Whether a new inner session starts in Zellij's locked mode.
    pub lock_inner: bool,
    /// How the list is grouped when the panel starts.
    pub group_by: GroupBy,
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
            team_layout: crate::guest::INNER_LAYOUT.to_string(),
            home_theme: None,
            away_theme: None,
            away_key: DEFAULT_AWAY_KEY.to_string(),
            home_key: DEFAULT_HOME_KEY.to_string(),
            update_check: true,
            zellij_colors: false,
            pane_titles: PaneTitles::Show,
            lock_inner: false,
            group_by: GroupBy::Status,
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
        if let Some(v) = get("team_layout") {
            config.team_layout = v.to_string();
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
        config.pane_titles = match get("pane_titles") {
            Some("hide") => PaneTitles::Hide,
            Some("name") => PaneTitles::Name,
            _ => PaneTitles::Show,
        };
        config.lock_inner = get("inner_mode") == Some("locked");
        config.group_by = match get("group_by") {
            Some("team") => GroupBy::Team,
            _ => GroupBy::Status,
        };
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
    pub pending_team: Option<PendingTeam>,
    /// The session the user was asked whether to end.
    pub pending_end: Option<String>,
    /// The running inner sessions: the teams' by team name, then the inner
    /// session itself.
    pub sessions: Vec<SessionRow>,
    pub session_index: usize,
    pub focus: Focus,
    /// The list the details are about: the one that last had the keys.
    pub source: Focus,
    /// The Sessions section is open (`S`); closed, it is its title alone.
    pub show_sessions: bool,
    /// How far the details are scrolled down and sideways, and how far
    /// they can be, as of the last time they were drawn.
    pub detail_scroll: usize,
    pub detail_shift: usize,
    pub detail_max_scroll: usize,
    pub detail_max_shift: usize,
    /// The rows of details last drawn, and what they were about.
    pub detail_rows: usize,
    pub detail_of: String,
    pub search: Option<Search>,
    /// The list holds search results: every match is a row, Quiet agents
    /// are not folded into sections, and none is too old to show.
    pub flat: bool,
    /// How the list is grouped; starts as the `group_by` setting.
    pub group_by: GroupBy,
    /// The team names behind `ListItem::Team`, in list order. The agents in
    /// no team are the empty name, last.
    pub teams: Vec<String>,
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
    /// Teams whose group is folded; every group starts open.
    folded_teams: BTreeSet<String>,
    /// Teams whose Quiet row is open.
    open_team_quiet: BTreeSet<String>,
    /// The row that holds the teams with no agent at work is open.
    quiet_teams_open: bool,
    stopped: BTreeSet<String>,
}

impl ZellijNoc {
    pub fn new(config: Config) -> ZellijNoc {
        let group_by = config.group_by;
        ZellijNoc {
            config,
            group_by,
            teams: Vec::new(),
            folded_teams: BTreeSet::new(),
            open_team_quiet: BTreeSet::new(),
            quiet_teams_open: false,
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
            pending_team: None,
            pending_end: None,
            sessions: Vec::new(),
            session_index: 0,
            focus: Focus::Agents,
            source: Focus::Agents,
            show_sessions: true,
            detail_scroll: 0,
            detail_shift: 0,
            detail_max_scroll: 0,
            detail_max_shift: 0,
            detail_rows: 0,
            detail_of: String::new(),
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

    /// Whether the list shows team groups: grouped by team, and not showing
    /// search results.
    pub fn by_team(&self) -> bool {
        self.group_by == GroupBy::Team && !self.flat
    }

    /// The name of a team row's team; empty for the agents in no team.
    pub fn team_name(&self, team: usize) -> &str {
        self.teams.get(team).map(String::as_str).unwrap_or("")
    }

    /// The heading text of a team row's team.
    pub fn team_title(&self, team: usize) -> &str {
        match self.team_name(team) {
            "" => NO_TEAM,
            name => name,
        }
    }

    /// One group per team by name, then the agents in no team. In a group:
    /// the lead, the other agents that are not Quiet in list order, then one
    /// row that holds the Quiet ones. A Quiet agent in no team that is older
    /// than seven days is not listed, as in the status grouping.
    fn rebuild_team_items(&mut self) {
        let mut members: BTreeMap<&str, Vec<usize>> = BTreeMap::new();
        let mut unteamed = Vec::new();
        for (index, agent) in self.agents.iter().enumerate() {
            match agent.team_name() {
                "" => {
                    if agent.group() != AgentGroup::Quiet || self.quiet_age(agent).is_some() {
                        unteamed.push(index);
                    }
                }
                name => members.entry(name).or_default().push(index),
            }
        }
        // A team with an agent that is not Quiet is at work. The others
        // rest: they go last, behind one row.
        type Group = (String, Vec<usize>);
        let (mut groups, resting): (Vec<Group>, Vec<Group>) = members
            .into_iter()
            .map(|(name, indexes)| (name.to_string(), indexes))
            .partition(|(_, indexes)| {
                indexes
                    .iter()
                    .any(|&i| self.agents[i].group() != AgentGroup::Quiet)
            });
        if !unteamed.is_empty() {
            groups.push((String::new(), unteamed));
        }
        let at_work = groups.len();
        let resting_count = resting.len();
        let quiet_teams_open = self.quiet_teams_open;
        groups.extend(resting);
        let mut items = Vec::new();
        let mut teams = Vec::new();
        for (team, (name, indexes)) in groups.into_iter().enumerate() {
            if team == at_work {
                items.push(ListItem::QuietTeams {
                    count: resting_count,
                    expanded: quiet_teams_open,
                });
            }
            if team >= at_work && !quiet_teams_open {
                teams.push(name);
                continue;
            }
            let count_of = |group: AgentGroup| {
                indexes
                    .iter()
                    .filter(|&&i| self.agents[i].group() == group)
                    .count()
            };
            let expanded = !self.folded_teams.contains(&name);
            items.push(ListItem::Team {
                team,
                count: indexes.len(),
                expanded,
                blocked: count_of(AgentGroup::Blocked),
                working: count_of(AgentGroup::Working),
                review: count_of(AgentGroup::Review),
            });
            if expanded {
                let lead = indexes.iter().copied().find(|&i| self.agents[i].is_lead());
                items.extend(lead.map(ListItem::Agent));
                let (quiet, active): (Vec<usize>, Vec<usize>) = indexes
                    .iter()
                    .filter(|&&i| Some(i) != lead)
                    .partition(|&&i| self.agents[i].group() == AgentGroup::Quiet);
                items.extend(active.into_iter().map(ListItem::Agent));
                if !quiet.is_empty() {
                    let open = self.open_team_quiet.contains(&name);
                    items.push(ListItem::TeamQuiet {
                        team,
                        count: quiet.len(),
                        expanded: open,
                    });
                    if open {
                        items.extend(quiet.into_iter().map(ListItem::Agent));
                    }
                }
            }
            teams.push(name);
        }
        self.items = items;
        self.teams = teams;
    }

    pub fn rebuild_items(&mut self) {
        if self.by_team() {
            self.rebuild_team_items();
            return;
        }
        self.teams.clear();
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
            ListItem::Team { team, .. } => {
                Some(SelectionKey::Team(self.team_name(*team).to_string()))
            }
            ListItem::TeamQuiet { team, .. } => {
                Some(SelectionKey::TeamQuiet(self.team_name(*team).to_string()))
            }
            ListItem::QuietTeams { .. } => Some(SelectionKey::QuietTeams),
        }
    }

    fn position_of(&self, key: &SelectionKey) -> Option<usize> {
        self.items.iter().position(|item| match (item, key) {
            (ListItem::Agent(i), SelectionKey::Agent(id)) => {
                self.agents.get(*i).is_some_and(|a| &a.id == id)
            }
            (ListItem::Section { age, .. }, SelectionKey::Section(want)) => age == want,
            (ListItem::Team { team, .. }, SelectionKey::Team(want)) => {
                self.team_name(*team) == want
            }
            (ListItem::TeamQuiet { team, .. }, SelectionKey::TeamQuiet(want)) => {
                self.team_name(*team) == want
            }
            (ListItem::QuietTeams { .. }, SelectionKey::QuietTeams) => true,
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

    /// The team of the selected row: a team heading's, its Quiet row's, or
    /// the selected agent's. Empty for no team; `None` on a status section
    /// or with nothing selected.
    pub fn selected_team(&self) -> Option<String> {
        match self.selected_item()? {
            ListItem::Agent(i) => self.agents.get(i).map(|a| a.team_name().to_string()),
            ListItem::Team { team, .. } | ListItem::TeamQuiet { team, .. } => {
                Some(self.team_name(team).to_string())
            }
            ListItem::Section { .. } | ListItem::QuietTeams { .. } => None,
        }
    }

    pub fn selected_agent(&self) -> Option<&Agent> {
        match self.selected_item()? {
            ListItem::Agent(i) => self.agents.get(i),
            ListItem::Section { .. }
            | ListItem::Team { .. }
            | ListItem::TeamQuiet { .. }
            | ListItem::QuietTeams { .. } => None,
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
        if self.by_team() && self.position_of(&target).is_none() {
            let agent = &self.agents[index];
            let team = agent.team_name().to_string();
            // Its team may be among those at rest.
            self.quiet_teams_open = true;
            self.folded_teams.remove(&team);
            if agent.group() == AgentGroup::Quiet && !agent.is_lead() {
                self.open_team_quiet.insert(team);
            }
            self.rebuild_items();
        } else if self.position_of(&target).is_none() {
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
        let key = match self.selected_item() {
            Some(ListItem::Section { age, .. }) => {
                if !self.expanded.remove(&age) {
                    self.expanded.insert(age);
                }
                SelectionKey::Section(age)
            }
            Some(ListItem::Team { team, .. }) => {
                let name = self.team_name(team).to_string();
                if !self.folded_teams.remove(&name) {
                    self.folded_teams.insert(name.clone());
                }
                SelectionKey::Team(name)
            }
            Some(ListItem::TeamQuiet { team, .. }) => {
                let name = self.team_name(team).to_string();
                if !self.open_team_quiet.remove(&name) {
                    self.open_team_quiet.insert(name.clone());
                }
                SelectionKey::TeamQuiet(name)
            }
            Some(ListItem::QuietTeams { .. }) => {
                self.quiet_teams_open = !self.quiet_teams_open;
                SelectionKey::QuietTeams
            }
            _ => return false,
        };
        self.rebuild_items();
        self.restore_selection(Some(key), self.selected_index);
        true
    }

    /// Switch between the status and the team grouping, keeping the
    /// selected agent selected.
    pub fn toggle_group_by(&mut self) {
        let selection = self.selection_key();
        let previous_index = self.selected_index;
        self.group_by = match self.group_by {
            GroupBy::Status => GroupBy::Team,
            GroupBy::Team => GroupBy::Status,
        };
        self.rebuild_items();
        self.restore_selection(selection, previous_index);
        self.set_notice(format!("grouped by {}", self.group_by.as_str()));
    }

    /// `h` in the team grouping: on a Quiet row or one of its agents, close
    /// that row; on a heading or any other agent, fold the team.
    fn collapse_selected_team(&mut self) {
        let key = match self.selected_item() {
            Some(ListItem::Team { team, .. }) => {
                let name = self.team_name(team).to_string();
                self.folded_teams.insert(name.clone());
                SelectionKey::Team(name)
            }
            Some(ListItem::TeamQuiet { team, .. }) => {
                let name = self.team_name(team).to_string();
                self.open_team_quiet.remove(&name);
                SelectionKey::TeamQuiet(name)
            }
            Some(ListItem::QuietTeams { .. }) => {
                self.quiet_teams_open = false;
                SelectionKey::QuietTeams
            }
            Some(ListItem::Agent(i)) => {
                let Some(agent) = self.agents.get(i) else {
                    return;
                };
                let name = agent.team_name().to_string();
                if agent.group() == AgentGroup::Quiet && !agent.is_lead() {
                    self.open_team_quiet.remove(&name);
                    SelectionKey::TeamQuiet(name)
                } else {
                    self.folded_teams.insert(name.clone());
                    SelectionKey::Team(name)
                }
            }
            _ => return,
        };
        self.rebuild_items();
        self.restore_selection(Some(key), self.selected_index);
    }

    pub fn collapse_selected(&mut self) {
        if self.by_team() {
            self.collapse_selected_team();
            return;
        }
        let age = match self.selected_item() {
            Some(ListItem::Section { age, .. }) => Some(age),
            Some(ListItem::Agent(i)) => self.agents.get(i).and_then(|agent| {
                (agent.group() == AgentGroup::Quiet)
                    .then(|| self.quiet_age(agent))
                    .flatten()
                    .filter(|age| self.expanded.contains(age))
            }),
            _ => None,
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

    // ---- sessions ----

    /// The running inner sessions, as Zellij lists them, and the one on
    /// show. Team sessions come first, by team name, then the inner session
    /// itself. The selection stays on the session it was on while the
    /// section has the keys, and otherwise sits on the session on show.
    pub fn set_sessions(&mut self, names: &[String], on_show: &str) -> bool {
        let inner = self.config.guest_session.clone();
        let mut names: Vec<&String> = names.iter().collect();
        names.sort_by(|a, b| (**a == inner, a.as_str()).cmp(&(**b == inner, b.as_str())));
        names.dedup();
        let rows: Vec<SessionRow> = names
            .into_iter()
            .map(|name| SessionRow {
                name: name.clone(),
                on_show: name == on_show,
            })
            .collect();
        let selected = self.selected_session().map(|row| row.name.clone());
        let changed = rows != self.sessions;
        self.sessions = rows;
        let keep = (self.focus == Focus::Sessions)
            .then_some(selected)
            .flatten()
            .and_then(|name| self.sessions.iter().position(|row| row.name == name));
        let on_show = self.sessions.iter().position(|row| row.on_show);
        let index = keep.or(on_show).unwrap_or(0);
        let moved = index != self.session_index;
        self.session_index = index;
        if self.sessions.is_empty() {
            self.leave(Focus::Sessions);
        }
        changed || moved
    }

    /// Whether the Sessions section lists its sessions: it is open, and there
    /// are sessions to list.
    pub fn sessions_shown(&self) -> bool {
        self.show_sessions && !self.sessions.is_empty()
    }

    /// The list the details are about, and whose selected row is
    /// highlighted: the one that has the keys, or had them before Details.
    pub fn list_focus(&self) -> Focus {
        match self.focus {
            Focus::Details => self.source,
            list => list,
        }
    }

    /// A section that closed or went away gives the keys up: to the list
    /// the details were about, or to the agents.
    fn leave(&mut self, section: Focus) {
        if self.source == section {
            self.source = Focus::Agents;
        }
        if self.focus == section {
            self.focus = self.source;
        }
    }

    pub fn selected_session(&self) -> Option<&SessionRow> {
        self.sessions.get(self.session_index)
    }

    /// The session the details are about, if they are about one.
    pub fn focused_session(&self) -> Option<&SessionRow> {
        (self.list_focus() == Focus::Sessions)
            .then(|| self.selected_session())
            .flatten()
    }

    /// The team a session belongs to; empty for the inner session.
    pub fn session_team<'a>(&self, name: &'a str) -> &'a str {
        if name == self.config.guest_session {
            ""
        } else {
            crate::guest::team_label(name)
        }
    }

    /// What a session is called: its team, or `no-team`.
    pub fn session_label<'a>(&self, name: &'a str) -> &'a str {
        match self.session_team(name) {
            "" => crate::guest::NO_TEAM,
            team => team,
        }
    }

    /// The agents of a session's team, counted by what they need. `None`
    /// while the list holds search results, which are not every agent.
    pub fn session_counts(&self, name: &str) -> Option<SessionCounts> {
        if self.flat {
            return None;
        }
        let team = self.session_team(name);
        let mut counts = SessionCounts::default();
        for agent in self.agents.iter().filter(|a| a.team_name() == team) {
            counts.agents += 1;
            match agent.group() {
                AgentGroup::Blocked => counts.blocked += 1,
                AgentGroup::Working => counts.working += 1,
                AgentGroup::Review => counts.review += 1,
                AgentGroup::Quiet => {}
            }
        }
        Some(counts)
    }

    /// `Tab` (`lists`): give the keys to the other list, Agents or Sessions.
    /// `Shift Tab`: give them to the details of the list that has them, or
    /// back to that list. So the details are reached without passing through
    /// the other list, which would change what they are about.
    pub fn toggle_focus(&mut self, lists: bool) {
        self.pending_g = false;
        let target = if !lists {
            match self.focus {
                Focus::Details => self.source,
                _ if self.show_details => Focus::Details,
                _ => {
                    self.set_notice("details are closed; D opens them");
                    return;
                }
            }
        } else if self.list_focus() == Focus::Sessions {
            Focus::Agents
        } else if self.sessions_shown() {
            Focus::Sessions
        } else if self.show_sessions {
            self.set_notice("no session is listed yet");
            return;
        } else {
            self.set_notice("sessions are closed; S opens them");
            return;
        };
        if target != Focus::Details {
            self.source = target;
        }
        self.focus = target;
        self.show_help = false;
        self.notice = None;
    }

    /// Give the keys to the agents, and make the details about them.
    pub fn focus_agents(&mut self) {
        self.source = Focus::Agents;
        self.focus = Focus::Agents;
        self.pending_g = false;
    }

    /// `S`: close the Sessions section to its title, or open it again.
    pub fn toggle_sessions(&mut self) {
        self.show_sessions = !self.show_sessions;
        self.show_help = false;
        if !self.show_sessions {
            self.leave(Focus::Sessions);
        }
    }

    /// The details moved down by `delta` rows, within what there is.
    pub fn scroll_details(&mut self, delta: isize) {
        self.detail_scroll = self
            .detail_scroll
            .saturating_add_signed(delta)
            .min(self.detail_max_scroll);
        self.notice = None;
        self.pending_g = false;
    }

    /// The details moved sideways by `delta` columns, within their width.
    pub fn shift_details(&mut self, delta: isize) {
        self.detail_shift = self
            .detail_shift
            .saturating_add_signed(delta)
            .min(self.detail_max_shift);
        self.notice = None;
        self.pending_g = false;
    }

    pub fn move_session(&mut self, delta: isize) {
        let last = self.sessions.len().saturating_sub(1);
        self.session_index = self.session_index.saturating_add_signed(delta).min(last);
        self.notice = None;
        self.pending_g = false;
    }

    // ---- toggles and prompts ----

    /// `d`: close the Details section to its title, or open it again.
    pub fn toggle_details(&mut self) {
        self.show_details = !self.show_details;
        self.show_help = false;
        if !self.show_details {
            self.leave(Focus::Details);
        }
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

    /// Open the team prompt for the selected agent, holding its team.
    pub fn begin_team(&mut self) -> bool {
        let Some(agent) = self.selected_agent() else {
            return false;
        };
        self.pending_team = Some(PendingTeam {
            agent_id: agent.id.clone(),
            text: agent.team_name().to_string(),
            typed: None,
        });
        true
    }

    /// The names of the teams that have an agent, in name order.
    pub fn team_names(&self) -> Vec<&str> {
        let names: BTreeSet<&str> = self
            .agents
            .iter()
            .map(Agent::team_name)
            .filter(|name| !name.is_empty())
            .collect();
        names.into_iter().collect()
    }

    /// `Tab` in the team prompt: fill in the first team that starts with
    /// what was typed; again, the next one, and round to the first.
    pub fn complete_team(&mut self) {
        let Some(pending) = self.pending_team.as_ref() else {
            return;
        };
        let typed = pending
            .typed
            .clone()
            .unwrap_or_else(|| pending.text.clone());
        let matches: Vec<&str> = self
            .team_names()
            .into_iter()
            .filter(|name| name.starts_with(typed.as_str()))
            .collect();
        if matches.is_empty() {
            return;
        }
        let next = match (
            &pending.typed,
            matches.iter().position(|m| *m == pending.text),
        ) {
            (Some(_), Some(at)) => (at + 1) % matches.len(),
            _ => 0,
        };
        let text = matches[next].to_string();
        if let Some(pending) = self.pending_team.as_mut() {
            pending.text = text;
            pending.typed = Some(typed);
        }
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
                ListItem::Team {
                    team,
                    count,
                    expanded,
                    blocked,
                    working,
                    review,
                } => format!(
                    "<{} {count} {} {blocked}/{working}/{review}>",
                    app.team_title(*team),
                    if *expanded { "open" } else { "shut" }
                ),
                ListItem::TeamQuiet {
                    team,
                    count,
                    expanded,
                } => format!(
                    "[{} quiet {count} {}]",
                    app.team_title(*team),
                    if *expanded { "open" } else { "shut" }
                ),
                ListItem::QuietTeams { count, expanded } => format!(
                    "[quiet teams {count} {}]",
                    if *expanded { "open" } else { "shut" }
                ),
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

    const TEAMS: &str = r#"[
        {"id":"solo","name":"Solo","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:59:00Z"},
        {"id":"old","name":"Old","display_state":"idle","bucket":"quiet","ended_at":"2026-09-01T08:00:00Z"},
        {"id":"w1","name":"Worker one","team":"billing","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:58:00Z"},
        {"id":"b1","name":"Blocked one","team":"billing","display_state":"needs_input","bucket":"blocked","last_activity_at":"2026-09-24T11:57:00Z"},
        {"id":"lead","name":"Lead","team":"billing","lead":true,"display_state":"ready","bucket":"review","last_activity_at":"2026-09-24T11:56:00Z"},
        {"id":"q1","name":"Quiet one","team":"billing","display_state":"idle","bucket":"quiet","ended_at":"2026-09-01T08:00:00Z"},
        {"id":"a1","name":"Auth one","team":"auth","display_state":"ready","bucket":"review","last_activity_at":"2026-09-24T11:55:00Z"},
        {"id":"qlead","name":"Quiet lead","team":"auth","lead":true,"display_state":"idle","bucket":"quiet","ended_at":"2026-09-24T07:00:00Z"}]"#;

    fn team_panel() -> ZellijNoc {
        let mut app = panel();
        app.toggle_group_by();
        app.apply_snapshot(list(TEAMS), Transport::Poll);
        app
    }

    #[test]
    fn teams_group_by_name_with_the_lead_first_and_quiet_agents_in_one_row() {
        let mut app = team_panel();
        // Teams by name; the lead first whatever its state, then blocked,
        // working, review; a week-old agent in no team is not listed.
        assert_eq!(
            row_ids(&app),
            [
                "<auth 2 open 0/0/1>",
                "qlead",
                "a1",
                "<billing 4 open 1/1/1>",
                "lead",
                "b1",
                "w1",
                "[billing quiet 1 shut]",
                "<no team 1 open 0/1/0>",
                "solo",
            ]
        );
        // A team's old Quiet agents stay reachable.
        assert!(app.select_agent("q1"));
        assert_eq!(row_ids(&app)[7..9], ["[billing quiet 1 open]", "q1"]);
        assert!(!app.select_agent("old"));
    }

    #[test]
    fn teams_with_no_agent_at_work_rest_behind_one_row() {
        let agents = |auth_state: &str, auth_bucket: &str| {
            format!(
                r#"[
            {{"id":"solo","name":"Solo","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:59:00Z"}},
            {{"id":"w1","name":"Worker","team":"billing","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:58:00Z"}},
            {{"id":"a1","name":"Auth one","team":"auth","display_state":"{auth_state}","bucket":"{auth_bucket}","ended_at":"2026-09-01T08:00:00Z"}},
            {{"id":"alead","name":"Auth lead","team":"auth","lead":true,"display_state":"idle","bucket":"quiet","ended_at":"2026-09-01T08:00:00Z"}},
            {{"id":"z1","name":"Zed","team":"zed","display_state":"idle","bucket":"quiet","ended_at":"2026-09-24T07:00:00Z"}}]"#
            )
        };
        let mut app = panel();
        app.toggle_group_by();
        app.apply_snapshot(list(&agents("idle", "quiet")), Transport::Poll);
        // The teams at work, the agents in no team, then one row for the
        // teams whose agents are all Quiet, however old.
        assert_eq!(
            row_ids(&app),
            [
                "<billing 1 open 0/1/0>",
                "w1",
                "<no team 1 open 0/1/0>",
                "solo",
                "[quiet teams 2 shut]",
            ]
        );
        // It opens like a section, onto the teams' own headings.
        app.selected_index = 4;
        assert!(app.toggle_selected_section());
        assert_eq!(
            row_ids(&app)[4..],
            [
                "[quiet teams 2 open]",
                "<auth 2 open 0/0/0>",
                "alead",
                "[auth quiet 1 shut]",
                "<zed 1 open 0/0/0>",
                "[zed quiet 1 shut]",
            ]
        );
        assert_eq!(app.selected_index, 4);
        // `h` on the row closes it; an agent of a resting team opens it.
        app.collapse_selected();
        assert_eq!(row_ids(&app)[4], "[quiet teams 2 shut]");
        assert!(app.select_agent("z1"));
        assert_eq!(
            row_ids(&app)[8..],
            ["<zed 1 open 0/0/0>", "[zed quiet 1 open]", "z1"]
        );
        // One of its agents wakes: the team is at work again, all of it.
        app.apply_snapshot(list(&agents("working", "working")), Transport::Poll);
        assert_eq!(
            row_ids(&app)[..5],
            [
                "<auth 2 open 0/1/0>",
                "alead",
                "a1",
                "<billing 1 open 0/1/0>",
                "w1",
            ]
        );
        assert!(row_ids(&app).contains(&"[quiet teams 1 open]".to_string()));
        // No resting team: no row.
        let mut app = team_panel();
        assert!(!row_ids(&app).iter().any(|row| row.contains("quiet teams")));
        app.selected_index = 0;
    }

    #[test]
    fn team_groups_fold_and_keep_their_state_across_snapshots() {
        let mut app = team_panel();
        app.selected_index = 3;
        assert!(app.toggle_selected_section());
        assert_eq!(
            row_ids(&app),
            [
                "<auth 2 open 0/0/1>",
                "qlead",
                "a1",
                "<billing 4 shut 1/1/1>",
                "<no team 1 open 0/1/0>",
                "solo",
            ]
        );
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::Team {
                expanded: false,
                ..
            })
        ));
        app.apply_snapshot(list(TEAMS), Transport::Poll);
        assert_eq!(row_ids(&app)[3], "<billing 4 shut 1/1/1>");
        // Selecting a member opens its group again.
        assert!(app.select_agent("w1"));
        assert_eq!(app.selected_agent().unwrap().id, "w1");
        // h on a member folds the team and selects its heading.
        app.collapse_selected();
        assert_eq!(row_ids(&app)[3], "<billing 4 shut 1/1/1>");
        assert_eq!(app.selected_index, 3);
        // h on an agent of an open Quiet row closes that row only.
        assert!(app.select_agent("q1"));
        app.collapse_selected();
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::TeamQuiet {
                expanded: false,
                ..
            })
        ));
        assert!(app.toggle_selected_section());
        assert!(matches!(
            app.selected_item(),
            Some(ListItem::TeamQuiet { expanded: true, .. })
        ));
    }

    #[test]
    fn switching_the_grouping_keeps_the_selected_agent() {
        let mut app = panel();
        app.apply_snapshot(list(TEAMS), Transport::Poll);
        assert!(!app.by_team());
        assert!(app.select_agent("w1"));
        app.toggle_group_by();
        assert!(app.by_team());
        assert_eq!(app.notice.as_deref(), Some("grouped by team"));
        assert_eq!(app.selected_agent().unwrap().id, "w1");
        app.toggle_group_by();
        assert_eq!(app.notice.as_deref(), Some("grouped by status"));
        assert_eq!(app.selected_agent().unwrap().id, "w1");
        assert!(app.teams.is_empty());
        // Search results are a flat list in either grouping.
        app.toggle_group_by();
        app.search = Some(Search {
            text: "one".into(),
            typing: false,
        });
        app.apply_snapshot(list(TEAMS), Transport::Poll);
        assert!(!app.by_team());
        assert!(app
            .items
            .iter()
            .all(|item| matches!(item, ListItem::Agent(_))));
    }

    #[test]
    fn team_names_follow_the_rules_noc_applies() {
        for good in ["auth", "a", "Auth_2.x-y", "9lives", &"a".repeat(48)] {
            assert!(is_team_name(good), "{good}");
        }
        for bad in [
            "",
            "none",
            "-auth",
            ".auth",
            "two words",
            "caf\u{e9}",
            &"a".repeat(49),
        ] {
            assert!(!is_team_name(bad), "{bad:?}");
        }
        assert!(is_team_name("None") && is_team_name("nonesuch"));
        assert!(is_team_char('-', 1) && !is_team_char('-', 0));
        assert!(is_team_char('.', 3) && !is_team_char('.', 0));
        assert!(!is_team_char(' ', 1) && !is_team_char('/', 1));
    }

    #[test]
    fn tab_completes_and_then_cycles_through_the_teams() {
        let mut app = panel();
        app.apply_snapshot(list(TEAMS), Transport::Poll);
        assert_eq!(app.team_names(), ["auth", "billing"]);
        // Starting from the agent's own team, or from nothing.
        assert!(app.select_agent("w1"));
        assert!(app.begin_team());
        assert_eq!(app.pending_team.as_ref().unwrap().text, "billing");
        assert!(app.select_agent("solo"));
        assert!(app.begin_team());
        let text = |app: &ZellijNoc| app.pending_team.as_ref().unwrap().text.clone();
        assert_eq!(text(&app), "");
        // Nothing typed: every team, in name order, and round again.
        app.complete_team();
        assert_eq!(text(&app), "auth");
        app.complete_team();
        assert_eq!(text(&app), "billing");
        app.complete_team();
        assert_eq!(text(&app), "auth");
        // A prefix: only the teams that start with it.
        let pending = app.pending_team.as_mut().unwrap();
        pending.text = "b".into();
        pending.typed = None;
        app.complete_team();
        assert_eq!(text(&app), "billing");
        app.complete_team();
        assert_eq!(text(&app), "billing", "the only match");
        // No team starts with it: the text stays.
        let pending = app.pending_team.as_mut().unwrap();
        pending.text = "zz".into();
        pending.typed = None;
        app.complete_team();
        assert_eq!(text(&app), "zz");
        // No prompt, nothing to complete; no agent, no prompt.
        app.pending_team = None;
        app.complete_team();
        app.toggle_group_by();
        app.selected_index = 0;
        assert!(!app.begin_team(), "a heading is not an agent");
    }

    #[test]
    fn a_joined_agent_stays_selected_when_the_list_regroups() {
        let mut app = team_panel();
        assert!(app.select_agent("solo"));
        let joined = TEAMS.replace(
            r#"{"id":"solo","name":"Solo","#,
            r#"{"id":"solo","name":"Solo","team":"auth","#,
        );
        app.apply_snapshot(list(&joined), Transport::Poll);
        assert_eq!(app.selected_agent().unwrap().id, "solo");
        assert_eq!(row_ids(&app)[0], "<auth 3 open 0/1/1>");
        let left = TEAMS.replace(r#""team":"billing","lead":true,"#, "");
        assert!(app.select_agent("lead"));
        app.apply_snapshot(list(&left), Transport::Poll);
        assert_eq!(app.selected_agent().unwrap().id, "lead");
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
        assert_eq!(defaults.pane_titles, PaneTitles::Show);
        assert_eq!(defaults.group_by, GroupBy::Status);
        for (value, want) in [
            ("team", GroupBy::Team),
            ("status", GroupBy::Status),
            ("host", GroupBy::Status),
        ] {
            let config = Config::from_map(&map(&[("group_by", value)]));
            assert_eq!(config.group_by, want, "{value}");
        }
        assert!(ZellijNoc::new(Config::from_map(&map(&[("group_by", "team")]))).by_team());
        for (value, want) in [
            ("show", PaneTitles::Show),
            ("hide", PaneTitles::Hide),
            ("name", PaneTitles::Name),
            ("other", PaneTitles::Show),
        ] {
            let config = Config::from_map(&map(&[("pane_titles", value)]));
            assert_eq!(config.pane_titles, want, "{value}");
        }
        assert!(!defaults.lock_inner);
        assert!(Config::from_map(&map(&[("inner_mode", "locked")])).lock_inner);
        assert!(!Config::from_map(&map(&[("inner_mode", "normal")])).lock_inner);
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
