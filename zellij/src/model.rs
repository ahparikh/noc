//! Serde model of `noc ls --json --all` / `noc show --json`, grouping,
//! ranks, symbols, labels and time helpers.

use serde::Deserialize;
use std::cmp::Ordering;
use std::time::{SystemTime, UNIX_EPOCH};

pub const SUPPORTED_SCHEMA_VERSION: u32 = 1;

#[derive(Debug, Clone, Default, Deserialize)]
pub struct AgentList {
    pub schema_version: u32,
    #[serde(default)]
    pub generated_at: String,
    #[serde(default)]
    pub agents: Vec<Agent>,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct Agent {
    pub id: String,
    #[serde(default)]
    pub host_id: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub native_session_id: String,
    #[serde(default)]
    pub source_kind: String,
    #[serde(default)]
    pub native_short_id: String,
    #[serde(default)]
    pub user_name: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub display_state: String,
    #[serde(default)]
    pub bucket: String,
    #[serde(default)]
    pub live_state: String,
    #[serde(default)]
    pub runtime_state: String,
    #[serde(default)]
    pub activity_state: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub reasoning_effort: String,
    #[serde(default)]
    pub initial_request: Option<TextPreview>,
    #[serde(default)]
    pub summary: String,
    #[serde(default)]
    pub summary_source: String,
    #[serde(default)]
    pub last_activity_at: Option<String>,
    #[serde(default)]
    pub last_seen_at: Option<String>,
    #[serde(default)]
    pub ended_at: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
    #[serde(default)]
    pub first_seen_at: Option<String>,
    #[serde(default)]
    pub attention: Attention,
    #[serde(default)]
    pub workspace: Option<Workspace>,
    #[serde(default)]
    pub frontend_bindings: Vec<FrontendBinding>,
    /// The team the agent is in; empty when it is in none.
    #[serde(default)]
    pub team: String,
    /// Whether the agent leads its team.
    #[serde(default)]
    pub lead: bool,
    #[serde(default)]
    pub parent_id: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct Attention {
    #[serde(rename = "type")]
    pub kind: String,
    pub detail: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct TextPreview {
    pub text: String,
    pub source: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct Workspace {
    pub path: String,
    pub root_path: String,
    pub kind: String,
    pub common_git_dir: String,
    pub branch: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(default)]
pub struct FrontendBinding {
    pub kind: String,
    pub session: String,
    pub pane_id: String,
    pub state: String,
    pub observed_at: String,
}

/// The final path component, ignoring trailing slashes.
fn final_component(path: &str) -> &str {
    path.trim_end_matches('/').rsplit('/').next().unwrap_or("")
}

/// The path without its final component (`""` when there is none).
fn parent_path(path: &str) -> &str {
    let trimmed = path.trim_end_matches('/');
    match trimmed.rfind('/') {
        Some(0) => "/",
        Some(i) => &trimmed[..i],
        None => "",
    }
}

impl Workspace {
    pub fn repo_name(&self) -> String {
        let git_dir = self.common_git_dir.trim();
        if final_component(git_dir) == ".git" {
            let name = final_component(parent_path(git_dir));
            if !name.is_empty() {
                return name.to_string();
            }
        }
        let root = self.root_path.trim();
        let base = if root.is_empty() {
            self.path.trim()
        } else {
            root
        };
        final_component(base).to_string()
    }

    /// `noc` reports a linked worktree as `worktree`; `linked_worktree` is
    /// accepted as a synonym.
    fn is_linked_worktree(&self) -> bool {
        matches!(self.kind.as_str(), "worktree" | "linked_worktree")
    }

    pub fn is_git(&self) -> bool {
        !self.common_git_dir.trim().is_empty()
            || self.kind == "main_checkout"
            || self.is_linked_worktree()
    }

    pub fn worktree_name(&self) -> Option<String> {
        if !self.is_linked_worktree() {
            return None;
        }
        let root = self.root_path.trim();
        let base = if root.is_empty() {
            self.path.trim()
        } else {
            root
        };
        let name = final_component(base);
        if name.is_empty() {
            None
        } else {
            Some(name.to_string())
        }
    }

    pub fn changes_directory(&self) -> String {
        let root = self.root_path.trim();
        if root.is_empty() {
            self.path.trim().to_string()
        } else {
            root.to_string()
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum AgentGroup {
    Blocked,
    Working,
    Review,
    Quiet,
}

impl AgentGroup {
    pub fn title(self) -> &'static str {
        match self {
            AgentGroup::Blocked => "Blocked",
            AgentGroup::Working => "Working",
            AgentGroup::Review => "Review",
            AgentGroup::Quiet => "Quiet",
        }
    }
}

/// Collapse whitespace runs to single spaces and trim the ends.
pub fn compact(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

pub fn state_rank(display_state: &str) -> u8 {
    match display_state {
        "needs_approval" => 0,
        "needs_input" => 1,
        "needs_authentication" => 2,
        "error" => 3,
        "working" => 4,
        "waiting" => 5,
        "ready" => 6,
        "idle" => 7,
        "stale" => 8,
        "not_live" => 9,
        _ => 10,
    }
}

pub fn state_symbol(display_state: &str) -> &'static str {
    match display_state {
        "needs_approval" => "!",
        "needs_input" => "?",
        "needs_authentication" => "@",
        "error" => "\u{d7}",
        "working" => "\u{25cf}",
        "waiting" => "\u{2026}",
        "ready" => "\u{25c6}",
        "idle" => "\u{25cb}",
        "stale" => "~",
        "not_live" => "\u{25cc}",
        _ => "\u{b7}",
    }
}

pub fn state_label(display_state: &str) -> String {
    match display_state {
        "needs_approval" => "needs approval".to_string(),
        "needs_input" => "needs input".to_string(),
        "needs_authentication" => "needs authentication".to_string(),
        "error" => "error".to_string(),
        "working" => "working".to_string(),
        "waiting" => "waiting".to_string(),
        "ready" => "review".to_string(),
        "idle" => "idle".to_string(),
        "stale" => "stale".to_string(),
        "not_live" => "not live".to_string(),
        "" => "unknown".to_string(),
        other => other.replace('_', " "),
    }
}

impl Agent {
    pub fn group(&self) -> AgentGroup {
        match self.bucket.as_str() {
            "blocked" => AgentGroup::Blocked,
            "working" => AgentGroup::Working,
            "review" => AgentGroup::Review,
            "quiet" => AgentGroup::Quiet,
            _ => match self.display_state.as_str() {
                "needs_approval" | "needs_input" | "needs_authentication" | "error" => {
                    AgentGroup::Blocked
                }
                "working" | "waiting" => AgentGroup::Working,
                "ready" => AgentGroup::Review,
                _ => AgentGroup::Quiet,
            },
        }
    }

    /// The agent's team name, trimmed; empty when it is in no team.
    pub fn team_name(&self) -> &str {
        self.team.trim()
    }

    /// Whether the agent leads a team. An agent in no team leads nothing.
    pub fn is_lead(&self) -> bool {
        self.lead && !self.team_name().is_empty()
    }

    fn name_is_id_placeholder(&self) -> bool {
        let name = self.name.trim();
        name == self.native_short_id.trim() || name == self.native_session_id.trim()
    }

    pub fn title(&self) -> String {
        if !self.user_name.trim().is_empty() {
            return compact(&self.user_name);
        }
        if !self.name.trim().is_empty() && !self.name_is_id_placeholder() {
            return compact(&self.name);
        }
        let short = self.native_short_id.trim();
        if !short.is_empty() {
            return short.to_string();
        }
        if !self.id.is_empty() {
            return self.id.chars().take(8).collect();
        }
        "unnamed agent".to_string()
    }

    pub fn state_rank(&self) -> u8 {
        state_rank(&self.display_state)
    }

    pub fn state_symbol(&self) -> &'static str {
        state_symbol(&self.display_state)
    }

    pub fn state_label(&self) -> String {
        state_label(&self.display_state)
    }

    /// Never `last_seen_at`: it is when noc last looked, not agent activity.
    pub fn recency(&self) -> Option<&str> {
        self.last_activity_at
            .as_deref()
            .or(self.ended_at.as_deref())
            .or(self.created_at.as_deref())
            .or(self.first_seen_at.as_deref())
    }

    pub fn provider_icon(&self, nerd: bool) -> String {
        match (self.provider.as_str(), nerd) {
            ("claude", true) => "\u{ec82}".to_string(),
            ("claude", false) => "\u{2733}".to_string(),
            ("codex", true) => "\u{ec81}".to_string(),
            ("codex", false) => "\u{2b21}".to_string(),
            ("", _) => "\u{b7}".to_string(),
            (other, _) => other.to_string(),
        }
    }

    pub fn provider_label(&self) -> String {
        if self.provider.is_empty() {
            "unknown".to_string()
        } else {
            self.provider.clone()
        }
    }

    pub fn provider_detail_label(&self) -> String {
        let mut parts = vec![self.provider_label()];
        if !self.source_kind.is_empty() {
            parts.push(self.source_kind.replace('_', " "));
        }
        if self.live_state == "not_live" {
            parts.push("ended".to_string());
        } else if matches!(
            self.runtime_state.as_str(),
            "starting" | "stopped" | "failed"
        ) {
            parts.push(self.runtime_state.clone());
        }
        parts.join(" \u{b7} ")
    }

    pub fn workspace_label(&self) -> String {
        let Some(workspace) = &self.workspace else {
            return String::new();
        };
        let repo = workspace.repo_name();
        let branch = workspace.branch.trim();
        match (repo.is_empty(), branch.is_empty()) {
            (false, false) => format!("{repo} \u{b7} {branch}"),
            (false, true) => repo,
            (true, false) => branch.to_string(),
            (true, true) => String::new(),
        }
    }

    fn zellij_binding_candidates(&self) -> impl Iterator<Item = &FrontendBinding> {
        self.frontend_bindings
            .iter()
            .filter(|b| b.kind == "zellij" && !b.pane_id.is_empty())
    }

    /// The binding shown on the details `pane` line.
    pub fn zellij_binding(&self) -> Option<&FrontendBinding> {
        self.zellij_binding_candidates()
            .max_by(|a, b| a.observed_at.cmp(&b.observed_at))
    }

    /// Bindings usable for navigation, newest first.
    pub fn zellij_bindings(&self) -> Vec<&FrontendBinding> {
        if self.live_state == "not_live" {
            return Vec::new();
        }
        let mut bindings: Vec<&FrontendBinding> = self
            .zellij_binding_candidates()
            .filter(|b| b.state != "disconnected")
            .collect();
        bindings.sort_by(|a, b| b.observed_at.cmp(&a.observed_at));
        bindings
    }
}

pub fn compare_agents(a: &Agent, b: &Agent) -> Ordering {
    let (ga, gb) = (a.group(), b.group());
    ga.cmp(&gb)
        .then_with(|| {
            if matches!(ga, AgentGroup::Blocked | AgentGroup::Working) {
                a.state_rank().cmp(&b.state_rank())
            } else {
                Ordering::Equal
            }
        })
        .then_with(|| b.recency().cmp(&a.recency()))
        .then_with(|| a.title().to_lowercase().cmp(&b.title().to_lowercase()))
        .then_with(|| a.id.cmp(&b.id))
}

pub fn parse_agent(text: &str) -> Result<Agent, String> {
    serde_json::from_str::<Agent>(text.trim()).map_err(|e| e.to_string())
}

pub fn parse_snapshot(text: &str) -> Result<AgentList, String> {
    let mut list: AgentList = serde_json::from_str(text.trim()).map_err(|e| e.to_string())?;
    if list.schema_version != SUPPORTED_SCHEMA_VERSION {
        return Err(format!(
            "unsupported noc schema version {} (expected {})",
            list.schema_version, SUPPORTED_SCHEMA_VERSION
        ));
    }
    list.agents.sort_by(compare_agents);
    Ok(list)
}

/// `7` or `terminal_7`; plugin ids and anything non-numeric are rejected.
pub fn parse_terminal_pane_id(text: &str) -> Option<u32> {
    let text = text.trim();
    let digits = text.strip_prefix("terminal_").unwrap_or(text);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    digits.parse().ok()
}

// ---- time ----

fn days_from_civil(year: i64, month: i64, day: i64) -> i64 {
    let y = if month <= 2 { year - 1 } else { year };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (month + 9) % 12;
    let doy = (153 * mp + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

fn civil_from_days(days: i64) -> (i64, i64, i64) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    (if month <= 2 { y + 1 } else { y }, month, day)
}

fn digits(bytes: &[u8]) -> Option<i64> {
    if bytes.is_empty() || !bytes.iter().all(u8::is_ascii_digit) {
        return None;
    }
    let mut value = 0i64;
    for b in bytes {
        value = value * 10 + i64::from(b - b'0');
    }
    Some(value)
}

/// Fixed-position RFC 3339 parsing; separators are not validated.
pub fn unix_seconds(text: &str) -> Option<i64> {
    let bytes = text.trim().as_bytes();
    if bytes.len() < 19 {
        return None;
    }
    let year = digits(&bytes[0..4])?;
    let month = digits(&bytes[5..7])?;
    let day = digits(&bytes[8..10])?;
    let hour = digits(&bytes[11..13])?;
    let minute = digits(&bytes[14..16])?;
    let second = digits(&bytes[17..19])?;
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) || hour > 23 || minute > 59 {
        return None;
    }
    if second > 60 {
        return None;
    }
    let mut rest = &bytes[19..];
    if rest.first() == Some(&b'.') {
        rest = &rest[1..];
        while rest.first().is_some_and(u8::is_ascii_digit) {
            rest = &rest[1..];
        }
    }
    let offset = match rest {
        [] | [b'Z'] | [b'z'] => 0,
        [sign @ (b'+' | b'-'), h1, h2, b':', m1, m2] => {
            let hours = digits(&[*h1, *h2])?;
            let minutes = digits(&[*m1, *m2])?;
            let total = hours * 3600 + minutes * 60;
            if *sign == b'+' {
                total
            } else {
                -total
            }
        }
        _ => return None,
    };
    Some(days_from_civil(year, month, day) * 86_400 + hour * 3600 + minute * 60 + second - offset)
}

pub fn age_seconds(then: Option<&str>, now: &str) -> Option<i64> {
    let then = unix_seconds(then?)?;
    let now = unix_seconds(now)?;
    Some((now - then).max(0))
}

/// Relative age; `0m` rather than `now` so ages align in a column.
pub fn age_label(then: Option<&str>, now: &str) -> String {
    let Some(s) = age_seconds(then, now) else {
        return String::new();
    };
    if s < 60 {
        "0m".to_string()
    } else if s < 3600 {
        format!("{}m", s / 60)
    } else if s < 86_400 {
        format!("{}h", s / 3600)
    } else {
        format!("{}d", s / 86_400)
    }
}

pub fn format_rfc3339(unix: i64) -> String {
    let days = unix.div_euclid(86_400);
    let secs = unix.rem_euclid(86_400);
    let (y, m, d) = civil_from_days(days);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z",
        secs / 3600,
        (secs % 3600) / 60,
        secs % 60
    )
}

pub fn now_rfc3339() -> String {
    let secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    format_rfc3339(secs)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn snapshot(agents: &str) -> String {
        format!(r#"{{"schema_version":1,"generated_at":"2026-09-24T12:00:00Z","agents":{agents}}}"#)
    }

    #[test]
    fn parses_and_sorts_attention_first() {
        let text = snapshot(
            r#"[
            {"id":"w","name":"Worker","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:00:00Z"},
            {"id":"b","user_name":"  Needs   approval ","name":"x","display_state":"needs_approval","bucket":"blocked","workspace":{"path":"/code/app","root_path":"/code/app","kind":"directory","branch":"main"}},
            {"id":"r","name":"Review me","display_state":"ready","bucket":"review"}
            ]"#,
        );
        let list = parse_snapshot(&text).unwrap();
        let ids: Vec<_> = list.agents.iter().map(|a| a.id.as_str()).collect();
        assert_eq!(ids, ["b", "w", "r"]);
        assert_eq!(list.agents[0].title(), "Needs approval");
        assert_eq!(list.agents[0].workspace_label(), "app \u{b7} main");
        assert_eq!(list.generated_at, "2026-09-24T12:00:00Z");
    }

    #[test]
    fn unnamed_agents_are_titled_by_short_id_not_first_prompt() {
        let a = parse_agent(
            r#"{"id":"abcdef123456","native_short_id":" 3cfff231 ","initial_request":{"text":"Please do the thing"}}"#,
        )
        .unwrap();
        assert_eq!(a.title(), "3cfff231");
        let placeholder =
            parse_agent(r#"{"id":"abcdef123456","name":"3cfff231","native_short_id":"3cfff231"}"#)
                .unwrap();
        assert_eq!(placeholder.title(), "3cfff231");
        let session =
            parse_agent(r#"{"id":"abcdef123456","name":"sess-1","native_session_id":"sess-1"}"#)
                .unwrap();
        assert_eq!(session.title(), "abcdef12");
        let bare = parse_agent(r#"{"id":""}"#).unwrap();
        assert_eq!(bare.title(), "unnamed agent");
    }

    #[test]
    fn reads_the_team_and_the_lead_mark() {
        let lead =
            parse_agent(r#"{"id":"a","team":" auth ","lead":true,"parent_id":"p"}"#).unwrap();
        assert_eq!(lead.team_name(), "auth");
        assert!(lead.is_lead());
        assert_eq!(lead.parent_id, "p");
        let member = parse_agent(r#"{"id":"b","team":"auth"}"#).unwrap();
        assert!(!member.is_lead());
        let alone = parse_agent(r#"{"id":"c","lead":true}"#).unwrap();
        assert_eq!(alone.team_name(), "");
        assert!(!alone.is_lead(), "no team, nothing to lead");
    }

    #[test]
    fn groups_finished_turns_apart_from_real_questions() {
        let text = snapshot(
            r#"[
            {"id":"parked","name":"Parked","display_state":"idle","bucket":"quiet"},
            {"id":"gone","name":"Gone","display_state":"not_live","bucket":"quiet","ended_at":"2026-09-24T08:00:00Z"},
            {"id":"ask","name":"Ask","display_state":"needs_input","bucket":"blocked"},
            {"id":"done","name":"Done","display_state":"ready","bucket":"review"}
            ]"#,
        );
        let list = parse_snapshot(&text).unwrap();
        let groups: Vec<_> = list
            .agents
            .iter()
            .map(|a| (a.id.as_str(), a.group()))
            .collect();
        assert_eq!(
            groups,
            [
                ("ask", AgentGroup::Blocked),
                ("done", AgentGroup::Review),
                ("gone", AgentGroup::Quiet),
                ("parked", AgentGroup::Quiet),
            ]
        );
        assert!(AgentGroup::Blocked < AgentGroup::Working);
        assert!(AgentGroup::Review < AgentGroup::Quiet);
    }

    #[test]
    fn prefers_the_bucket_noc_reports() {
        let ended: Agent = parse_agent(
            r#"{"id":"a","display_state":"needs_input","bucket":"quiet","live_state":"not_live"}"#,
        )
        .unwrap();
        assert_eq!(ended.group(), AgentGroup::Quiet);
        let old: Agent = parse_agent(r#"{"id":"a","display_state":"needs_input"}"#).unwrap();
        assert_eq!(old.group(), AgentGroup::Blocked);
        for (state, group) in [
            ("needs_approval", AgentGroup::Blocked),
            ("needs_authentication", AgentGroup::Blocked),
            ("error", AgentGroup::Blocked),
            ("working", AgentGroup::Working),
            ("waiting", AgentGroup::Working),
            ("ready", AgentGroup::Review),
            ("idle", AgentGroup::Quiet),
            ("stale", AgentGroup::Quiet),
            ("", AgentGroup::Quiet),
        ] {
            let agent = Agent {
                display_state: state.into(),
                bucket: "mystery".into(),
                ..Agent::default()
            };
            assert_eq!(agent.group(), group, "{state}");
        }
    }

    #[test]
    fn provider_icons_and_review_label_are_single_column() {
        use unicode_width::UnicodeWidthStr;
        let claude = Agent {
            provider: "claude".into(),
            ..Agent::default()
        };
        let codex = Agent {
            provider: "codex".into(),
            ..Agent::default()
        };
        assert_eq!(claude.provider_icon(true), "\u{ec82}");
        assert_eq!(codex.provider_icon(true), "\u{ec81}");
        assert_eq!(claude.provider_icon(false), "\u{2733}");
        assert_eq!(codex.provider_icon(false), "\u{2b21}");
        assert_eq!(claude.provider_icon(false).width(), 1);
        assert_eq!(codex.provider_icon(false).width(), 1);
        assert_eq!(Agent::default().provider_icon(true), "\u{b7}");
        let other = Agent {
            provider: "gemini".into(),
            ..Agent::default()
        };
        assert_eq!(other.provider_icon(false), "gemini");
        assert_eq!(state_label("ready"), "review");
        assert_eq!(state_symbol("ready"), "\u{25c6}");
        assert_eq!(state_label("custom_state"), "custom state");
        assert_eq!(state_label(""), "unknown");
        assert_eq!(state_rank("custom_state"), 10);
        assert_eq!(state_symbol("error"), "\u{d7}");
        assert_eq!(state_symbol("not_live"), "\u{25cc}");
    }

    #[test]
    fn the_clock_round_trips_through_the_age_parser() {
        let now = now_rfc3339();
        assert_eq!(now.len(), 20);
        assert_eq!(age_seconds(Some(&now), &now), Some(0));
        assert_eq!(
            format_rfc3339(unix_seconds("2026-09-24T12:00:00Z").unwrap()),
            "2026-09-24T12:00:00Z"
        );
        assert_eq!(format_rfc3339(0), "1970-01-01T00:00:00Z");
        // A future timestamp clamps to zero rather than going negative.
        assert_eq!(
            age_seconds(Some("2026-09-24T12:00:10Z"), "2026-09-24T12:00:00Z"),
            Some(0)
        );
        assert_eq!(age_seconds(None, "2026-09-24T12:00:00Z"), None);
        assert_eq!(age_seconds(Some("2026-09-24T12:00:00Z"), ""), None);
    }

    #[test]
    fn recency_ignores_when_noc_last_looked() {
        let mut agent = Agent {
            last_seen_at: Some("2026-09-24T12:00:00Z".into()),
            ..Agent::default()
        };
        assert_eq!(agent.recency(), None);
        agent.first_seen_at = Some("first".into());
        assert_eq!(agent.recency(), Some("first"));
        agent.created_at = Some("created".into());
        assert_eq!(agent.recency(), Some("created"));
        agent.ended_at = Some("ended".into());
        assert_eq!(agent.recency(), Some("ended"));
        agent.last_activity_at = Some("activity".into());
        assert_eq!(agent.recency(), Some("activity"));
    }

    #[test]
    fn labels_ages_from_rfc3339_times() {
        let now = "2026-09-24T12:00:00Z";
        assert_eq!(age_label(Some("2026-09-24T11:59:30Z"), now), "0m");
        assert_eq!(age_label(Some("2026-09-24T11:58:00Z"), now), "2m");
        assert_eq!(age_label(Some("2026-09-24T09:00:00Z"), now), "3h");
        assert_eq!(age_label(Some("2026-09-21T08:00:00Z"), now), "3d");
        assert_eq!(age_label(None, now), "");
        assert_eq!(age_label(Some("garbage"), now), "");
        // Fractional seconds and offsets.
        assert_eq!(age_label(Some("2026-09-24T11:58:00.123456Z"), now), "2m");
        assert_eq!(age_label(Some("2026-09-24T13:58:00+02:00"), now), "2m");
        // A zone without a colon is rejected, not guessed.
        assert_eq!(unix_seconds("2026-09-24T11:58:00+0000"), None);
        assert_eq!(age_label(Some("2026-09-24T11:58:00+0000"), now), "");
    }

    #[test]
    fn names_the_repository_not_the_checkout_kind() {
        let worktree = Workspace {
            path: "/code/app-wt/cache-fix/svc".into(),
            root_path: "/code/app-wt/cache-fix".into(),
            kind: "worktree".into(),
            common_git_dir: "/code/app/.git".into(),
            branch: "cache-fix".into(),
        };
        assert_eq!(worktree.repo_name(), "app");
        assert_eq!(worktree.worktree_name().as_deref(), Some("cache-fix"));
        assert_eq!(worktree.changes_directory(), "/code/app-wt/cache-fix");
        assert!(worktree.is_git());
        // The synonym works too.
        let legacy = Workspace {
            kind: "linked_worktree".into(),
            ..worktree.clone()
        };
        assert_eq!(legacy.worktree_name().as_deref(), Some("cache-fix"));
        let main = Workspace {
            path: "/code/app".into(),
            root_path: "/code/app".into(),
            kind: "main_checkout".into(),
            common_git_dir: "/code/app/.git".into(),
            branch: "main".into(),
        };
        assert_eq!(main.repo_name(), "app");
        assert_eq!(main.worktree_name(), None);
        let plain = Workspace {
            path: "/code/docs".into(),
            kind: "directory".into(),
            ..Workspace::default()
        };
        assert_eq!(plain.repo_name(), "docs");
        assert!(!plain.is_git());
        assert_eq!(plain.changes_directory(), "/code/docs");
        assert!(Workspace {
            kind: "main_checkout".into(),
            ..Workspace::default()
        }
        .is_git());
    }

    #[test]
    fn describes_the_provider_readably() {
        let background = Agent {
            provider: "claude".into(),
            source_kind: "background".into(),
            ..Agent::default()
        };
        assert_eq!(
            background.provider_detail_label(),
            "claude \u{b7} background"
        );
        let ended = Agent {
            provider: "codex".into(),
            source_kind: "cli".into(),
            live_state: "not_live".into(),
            ..Agent::default()
        };
        assert_eq!(
            ended.provider_detail_label(),
            "codex \u{b7} cli \u{b7} ended"
        );
        let failed = Agent {
            provider: "codex".into(),
            runtime_state: "failed".into(),
            ..Agent::default()
        };
        assert_eq!(failed.provider_detail_label(), "codex \u{b7} failed");
        assert_eq!(Agent::default().provider_label(), "unknown");
        let spaced = Agent {
            provider: "claude".into(),
            source_kind: "remote_control".into(),
            ..Agent::default()
        };
        assert_eq!(
            spaced.provider_detail_label(),
            "claude \u{b7} remote control"
        );
    }

    #[test]
    fn rejects_unknown_schema_versions() {
        let err = parse_snapshot(r#"{"schema_version":2,"agents":[]}"#).unwrap_err();
        assert_eq!(err, "unsupported noc schema version 2 (expected 1)");
        let missing = parse_snapshot(r#"{"agents":[]}"#).unwrap_err();
        assert!(
            missing.contains("missing field `schema_version`"),
            "{missing}"
        );
        let sparse = parse_snapshot(r#"{"schema_version":1}"#).unwrap();
        assert!(sparse.agents.is_empty());
        assert_eq!(sparse.generated_at, "");
        // Unknown fields are ignored at every level.
        let extra = parse_snapshot(
            r#"{"schema_version":1,"future":1,"agents":[{"id":"a","capabilities":{"x":1},"workspace":{"head_sha":"abc"}}]}"#,
        )
        .unwrap();
        assert_eq!(extra.agents.len(), 1);
    }

    #[test]
    fn parses_terminal_pane_ids_only() {
        assert_eq!(parse_terminal_pane_id("7"), Some(7));
        assert_eq!(parse_terminal_pane_id("terminal_7"), Some(7));
        assert_eq!(parse_terminal_pane_id(" terminal_12 "), Some(12));
        assert_eq!(parse_terminal_pane_id("plugin_7"), None);
        assert_eq!(parse_terminal_pane_id("terminal_"), None);
        assert_eq!(parse_terminal_pane_id("seven"), None);
        assert_eq!(parse_terminal_pane_id(""), None);
        assert_eq!(parse_terminal_pane_id("-1"), None);
    }

    #[test]
    fn null_is_accepted_only_for_option_fields() {
        let ok = parse_agent(
            r#"{"id":"a","initial_request":null,"workspace":null,"last_activity_at":null,"ended_at":null}"#,
        )
        .unwrap();
        assert!(ok.initial_request.is_none() && ok.workspace.is_none());
        for field in [
            r#""name":null"#,
            r#""attention":null"#,
            r#""frontend_bindings":null"#,
        ] {
            let text = format!(r#"{{"id":"a",{field}}}"#);
            assert!(parse_agent(&text).is_err(), "{field} must fail");
        }
        let err = parse_agent(r#"{"id":"a","name":null}"#).unwrap_err();
        assert!(err.contains("invalid type: null"), "{err}");
    }

    #[test]
    fn zellij_bindings_skip_ended_and_disconnected_agents() {
        let agent = parse_agent(
            r#"{"id":"a","live_state":"live","frontend_bindings":[
            {"kind":"zellij","session":"s","pane_id":"1","state":"connected","observed_at":"2026-01-01T00:00:00Z"},
            {"kind":"zellij","session":"s","pane_id":"2","state":"disconnected","observed_at":"2026-03-01T00:00:00Z"},
            {"kind":"zellij","session":"s","pane_id":"3","state":"connected","observed_at":"2026-02-01T00:00:00Z"},
            {"kind":"tmux","pane_id":"9","observed_at":"2026-04-01T00:00:00Z"},
            {"kind":"zellij","pane_id":"","observed_at":"2026-05-01T00:00:00Z"}]}"#,
        )
        .unwrap();
        let ids: Vec<_> = agent
            .zellij_bindings()
            .iter()
            .map(|b| b.pane_id.clone())
            .collect();
        assert_eq!(ids, ["3", "1"]);
        // The display binding ignores state.
        assert_eq!(agent.zellij_binding().unwrap().pane_id, "2");
        let ended = Agent {
            live_state: "not_live".into(),
            ..agent
        };
        assert!(ended.zellij_bindings().is_empty());
    }
}
