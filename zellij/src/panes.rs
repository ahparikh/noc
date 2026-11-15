//! Pane records and the pure pane, command, title and pipe helpers.

use crate::model::{parse_snapshot, parse_terminal_pane_id, Agent, AgentGroup, AgentList};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use zellij_tile::prelude::{PaneInfo, PaneManifest};

pub const MAX_PIPE_FRAME_BYTES: usize = 4 * 1024 * 1024;
pub const TAB_NAME_LIMIT: usize = 32;

// ---- pane records ----

/// One pane, built from a `PaneUpdate` manifest: the host session's own, or
/// the inner session's as its relay sends them.
#[derive(Debug, Clone, PartialEq, Deserialize, Serialize)]
#[serde(default)]
pub struct ScanPane {
    pub id: u32,
    pub is_plugin: bool,
    pub is_focused: bool,
    pub is_floating: bool,
    pub is_suppressed: bool,
    pub is_selectable: bool,
    pub exited: bool,
    pub exit_status: Option<i32>,
    pub title: String,
    pub plugin_url: Option<String>,
    pub terminal_command: Option<String>,
    /// Only for the inner session's panes: what runs in the pane right now,
    /// which for a shell is the command typed into it.
    pub pane_command: Option<String>,
    pub pane_columns: usize,
    pub pane_rows: usize,
    pub tab_position: usize,
    /// Only for the inner session's panes: the tab's stable id and its name.
    pub tab_id: Option<u32>,
    pub tab_name: String,
}

impl Default for ScanPane {
    fn default() -> Self {
        ScanPane {
            id: 0,
            is_plugin: false,
            is_focused: false,
            is_floating: false,
            is_suppressed: false,
            is_selectable: true,
            exited: false,
            exit_status: None,
            title: String::new(),
            plugin_url: None,
            terminal_command: None,
            pane_command: None,
            pane_columns: 0,
            pane_rows: 0,
            tab_position: 0,
            tab_id: None,
            tab_name: String::new(),
        }
    }
}

impl ScanPane {
    pub fn from_info(tab_position: usize, info: &PaneInfo) -> ScanPane {
        ScanPane {
            id: info.id,
            is_plugin: info.is_plugin,
            is_focused: info.is_focused,
            is_floating: info.is_floating,
            is_suppressed: info.is_suppressed,
            is_selectable: info.is_selectable,
            exited: info.exited,
            exit_status: info.exit_status,
            title: info.title.clone(),
            plugin_url: info.plugin_url.clone(),
            terminal_command: info.terminal_command.clone(),
            pane_command: None,
            pane_columns: info.pane_columns,
            pane_rows: info.pane_rows,
            tab_position,
            tab_id: None,
            tab_name: String::new(),
        }
    }
}

/// The manifest as a flat list, tabs in position order, panes in the order
/// Zellij reported them.
pub fn manifest_panes(manifest: &PaneManifest) -> Vec<ScanPane> {
    let mut positions: Vec<&usize> = manifest.panes.keys().collect();
    positions.sort();
    let mut out = Vec::new();
    for position in positions {
        for info in &manifest.panes[position] {
            out.push(ScanPane::from_info(*position, info));
        }
    }
    out
}

// ---- errors ----

/// One line, whitespace collapsed, at most 180 characters then `…`.
pub fn compact_error(text: &str) -> String {
    let line = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if line.chars().count() > 180 {
        let mut cut: String = line.chars().take(180).collect();
        cut.push('\u{2026}');
        cut
    } else {
        line
    }
}

pub fn is_server_unavailable(detail: &str) -> bool {
    let lower = detail.to_lowercase();
    lower.contains("is the server running?")
        || lower.contains("noc server is not running")
        || (lower.contains("dial unix")
            && (lower.contains("no such file or directory")
                || lower.contains("connection refused")))
}

// ---- manifest and tabs ----

/// The tab position and pane entry of one plugin instance.
pub fn find_plugin_pane(panes: &[ScanPane], plugin_id: u32) -> Option<&ScanPane> {
    panes.iter().find(|p| p.is_plugin && p.id == plugin_id)
}

pub fn tab_panes(panes: &[ScanPane], tab: usize) -> Vec<ScanPane> {
    panes
        .iter()
        .filter(|p| p.tab_position == tab)
        .cloned()
        .collect()
}

// ---- titles ----

/// Centre the panel title in its frame: plain when it would not fit with
/// padding on both sides.
pub fn agent_tab_name(agent: &Agent) -> String {
    let title = agent.title();
    if title.chars().count() > TAB_NAME_LIMIT {
        let mut cut: String = title.chars().take(TAB_NAME_LIMIT).collect();
        cut.push('\u{2026}');
        cut
    } else {
        title
    }
}

/// The name Zellij gives a tab nobody named: `Tab #<n>`.
pub fn is_default_tab_name(name: &str) -> bool {
    name.strip_prefix("Tab #")
        .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
}

// ---- focus ----

/// The pane holding a tab's keyboard. `is_focused` is per layer, so floating
/// panes win only while they are visible.
pub fn focused_pane(panes_in_tab: &[ScanPane], floating_visible: bool) -> Option<&ScanPane> {
    let tiled = || {
        panes_in_tab
            .iter()
            .find(|p| p.is_focused && !p.is_suppressed && !p.is_floating)
    };
    if floating_visible {
        panes_in_tab
            .iter()
            .find(|p| p.is_focused && !p.is_suppressed && p.is_floating)
            .or_else(tiled)
    } else {
        tiled()
    }
}

// ---- attach commands and agent resolution ----

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LaunchedPane {
    pub id: u32,
    pub observed: bool,
}

fn basename(path: &str) -> &str {
    path.trim_end_matches('/')
        .rsplit('/')
        .next()
        .unwrap_or(path)
}

/// `<noc_bin> attach <id>`, with optional flags between; quotes stripped.
pub fn attached_agent_id(command: &str, noc_bin: &str) -> Option<String> {
    let tokens: Vec<&str> = command
        .split_whitespace()
        .map(|t| t.trim_matches(|c| c == '\'' || c == '"'))
        .collect();
    if tokens.len() < 3 || tokens[tokens.len() - 2] != "attach" {
        return None;
    }
    (basename(tokens[0]) == basename(noc_bin)).then(|| tokens[tokens.len() - 1].to_string())
}

/// The agent a command line names: `<noc_bin> attach <id>`, or else the one
/// agent whose native session id (whole or short) is one of the command's
/// words, as in `claude attach 2f5ac2d2` or `codex resume <id>`. A word is
/// also what follows `=` (`--resume=<id>`).
pub fn agent_named_by_command<'a>(
    agents: &'a [Agent],
    command: &str,
    noc_bin: &str,
) -> Option<&'a Agent> {
    if let Some(agent) =
        attached_agent_id(command, noc_bin).and_then(|id| agents.iter().find(|a| a.id == id))
    {
        return Some(agent);
    }
    let words: Vec<&str> = command
        .split_whitespace()
        .map(|t| t.trim_matches(|c| c == '\'' || c == '"'))
        .map(|t| t.rsplit('=').next().unwrap_or(t))
        .filter(|t| !t.is_empty())
        .collect();
    let mut matches = agents.iter().filter(|a| {
        [a.native_session_id.trim(), a.native_short_id.trim()]
            .iter()
            .any(|id| !id.is_empty() && words.contains(id))
    });
    let first = matches.next()?;
    matches.next().is_none().then_some(first)
}

/// The agent named by the command a pane was started with or, failing
/// that, by the command running in it now.
pub fn agent_named_by_pane_command<'a>(
    agents: &'a [Agent],
    pane: &ScanPane,
    noc_bin: &str,
) -> Option<&'a Agent> {
    [&pane.terminal_command, &pane.pane_command]
        .into_iter()
        .flatten()
        .find_map(|command| agent_named_by_command(agents, command, noc_bin))
}

/// `NOC: ` and a leading `✳ ` (or any non-alphanumerics) are not the name.
fn pane_title_name(title: &str) -> &str {
    let title = title.strip_prefix("NOC: ").unwrap_or(title);
    title
        .trim_start_matches(|c: char| !c.is_alphanumeric())
        .trim()
}

/// The one live (non-Quiet) agent whose name equals the pane title.
pub fn agent_named_by_pane_title<'a>(agents: &'a [Agent], title: &str) -> Option<&'a Agent> {
    let name = pane_title_name(title);
    if name.is_empty() {
        return None;
    }
    let mut matches = agents.iter().filter(|a| {
        a.group() != AgentGroup::Quiet && (a.user_name.trim() == name || a.name.trim() == name)
    });
    let first = matches.next()?;
    matches.next().is_none().then_some(first)
}

pub fn agent_in_pane<'a>(
    agents: &'a [Agent],
    pane: &ScanPane,
    launched: &BTreeMap<String, LaunchedPane>,
    noc_bin: &str,
    session_name: Option<&str>,
) -> Option<&'a Agent> {
    if pane.is_plugin || pane.exited {
        return None;
    }
    if let Some(agent) = launched
        .iter()
        .find(|(_, l)| l.id == pane.id)
        .and_then(|(id, _)| agents.iter().find(|a| &a.id == id))
    {
        return Some(agent);
    }
    if let Some(agent) = agent_named_by_pane_command(agents, pane, noc_bin) {
        return Some(agent);
    }
    if let Some(agent) = agent_named_by_pane_title(agents, &pane.title) {
        return Some(agent);
    }
    agents
        .iter()
        .filter_map(|agent| {
            agent
                .zellij_bindings()
                .into_iter()
                .filter(|b| {
                    parse_terminal_pane_id(&b.pane_id) == Some(pane.id)
                        && (b.session.is_empty() || session_name.is_none_or(|s| s == b.session))
                })
                .map(|b| b.observed_at.clone())
                .max()
                .map(|observed| (observed, agent))
        })
        .max_by(|a, b| a.0.cmp(&b.0))
        .map(|(_, agent)| agent)
}

/// A validated binding naming another running session (not this one, not the
/// nested inner session).
pub fn other_running_session(
    agent: &Agent,
    current: Option<&str>,
    guest: Option<&str>,
    live_sessions: &[String],
) -> Option<String> {
    agent.zellij_bindings().into_iter().find_map(|b| {
        let session = b.session.as_str();
        (!session.is_empty()
            && Some(session) != current
            && Some(session) != guest
            && live_sessions.iter().any(|s| s == session))
        .then(|| session.to_string())
    })
}

// ---- registry pipe ----

/// NDJSON snapshots from the registry pipe. Complete lines are parsed; a
/// remainder is parsed only if complete, kept if not, and reported when the
/// sender stops (an empty payload).
pub fn decode_pipe_frames(buffer: &mut String, payload: &str) -> Vec<Result<AgentList, String>> {
    let mut out = Vec::new();
    buffer.push_str(payload);
    if buffer.len() > MAX_PIPE_FRAME_BYTES {
        buffer.clear();
        out.push(Err(format!(
            "pipe frame exceeded the {MAX_PIPE_FRAME_BYTES} byte limit"
        )));
        return out;
    }
    while let Some(at) = buffer.find('\n') {
        let line: String = buffer.drain(..=at).collect();
        let line = line.trim();
        if !line.is_empty() {
            out.push(parse_snapshot(line));
        }
    }
    let rest = buffer.trim();
    if rest.is_empty() {
        buffer.clear();
    } else {
        match parse_snapshot(rest) {
            Ok(list) => {
                out.push(Ok(list));
                buffer.clear();
            }
            Err(e) if payload.is_empty() => {
                out.push(Err(e));
                buffer.clear();
            }
            Err(_) => {}
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::parse_agent;
    use zellij_tile::prelude::PaneInfo;

    const URL: &str = "file:/p/zellij-noc.wasm";

    fn terminal(id: u32, tab: usize, rows: usize, cols: usize) -> ScanPane {
        ScanPane {
            id,
            tab_position: tab,
            pane_rows: rows,
            pane_columns: cols,
            ..ScanPane::default()
        }
    }

    fn command_pane(id: u32, tab: usize, command: &str) -> ScanPane {
        ScanPane {
            terminal_command: Some(command.to_string()),
            ..terminal(id, tab, 10, 10)
        }
    }

    fn agent(json: &str) -> Agent {
        parse_agent(json).unwrap()
    }

    #[test]
    fn compact_error_is_single_line_and_bounded() {
        assert_eq!(compact_error("  one\n two\t three  "), "one two three");
        assert_eq!(compact_error(""), "");
        let long = "x".repeat(400);
        let out = compact_error(&long);
        assert_eq!(out.chars().count(), 181);
        assert!(out.ends_with('\u{2026}'));
        assert_eq!(compact_error(&"y".repeat(180)).chars().count(), 180);
    }

    #[test]
    fn recognizes_server_connection_failures_without_hiding_other_errors() {
        for text in [
            "Error: is the server running?",
            "NOC SERVER IS NOT RUNNING",
            "dial unix /tmp/noc.sock: connect: no such file or directory",
            "Dial Unix /tmp/x: connect: Connection refused",
        ] {
            assert!(is_server_unavailable(text), "{text}");
        }
        for text in [
            "executable file not found in $PATH",
            "dial unix /tmp/x: permission denied",
            "no such file or directory",
            "",
        ] {
            assert!(!is_server_unavailable(text), "{text}");
        }
    }

    #[test]
    fn finds_its_own_plugin_pane_in_the_manifest() {
        let mut manifest = PaneManifest::default();
        manifest.panes.insert(
            1,
            vec![PaneInfo {
                id: 4,
                is_plugin: true,
                plugin_url: Some(URL.to_string()),
                ..PaneInfo::default()
            }],
        );
        manifest.panes.insert(
            0,
            vec![PaneInfo {
                id: 4,
                is_plugin: false,
                ..PaneInfo::default()
            }],
        );
        let panes = manifest_panes(&manifest);
        let own = find_plugin_pane(&panes, 4).unwrap();
        assert_eq!(own.tab_position, 1, "terminal 4 is not plugin 4");
        assert_eq!(own.plugin_url.as_deref(), Some(URL));
        assert!(find_plugin_pane(&panes, 99).is_none());
    }

    #[test]
    fn names_another_running_session_only_when_it_is_live_and_not_ours() {
        let a = agent(
            r#"{"id":"a1","live_state":"live","frontend_bindings":[
              {"kind":"zellij","session":"other","pane_id":"3","state":"connected","observed_at":"2026-05-01T00:00:00Z"},
              {"kind":"zellij","session":"mine","pane_id":"9","state":"connected","observed_at":"2026-04-01T00:00:00Z"}]}"#,
        );
        let live = vec!["mine".to_string(), "other".to_string(), "inner".to_string()];
        assert_eq!(
            other_running_session(&a, Some("mine"), None, &live).as_deref(),
            Some("other")
        );
        // The inner session is ours too.
        assert_eq!(
            other_running_session(&a, Some("mine"), Some("other"), &live),
            None
        );
        assert_eq!(
            other_running_session(&a, Some("mine"), None, &["mine".to_string()]),
            None
        );
    }

    fn frame(text: &str) -> String {
        format!(r#"{{"schema_version":1,"generated_at":"t","agents":[{text}]}}"#)
    }

    #[test]
    fn pipe_decoder_handles_fragmented_and_complete_snapshots() {
        let mut buffer = String::new();
        let whole = frame(r#"{"id":"a"}"#);
        // Fragmented across payloads.
        assert!(decode_pipe_frames(&mut buffer, &whole[..20]).is_empty());
        let out = decode_pipe_frames(&mut buffer, &format!("{}\n", &whole[20..]));
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].as_ref().unwrap().agents[0].id, "a");
        assert!(buffer.is_empty());
        // Several lines in one payload, blank lines skipped.
        let out = decode_pipe_frames(&mut buffer, &format!("{whole}\n\n{whole}\n"));
        assert_eq!(out.len(), 2);
        // A complete frame without a trailing newline is emitted at once.
        let out = decode_pipe_frames(&mut buffer, &whole);
        assert_eq!(out.len(), 1);
        assert!(buffer.is_empty());
        // An oversized buffer is dropped with one error.
        let big = "x".repeat(MAX_PIPE_FRAME_BYTES + 1);
        let out = decode_pipe_frames(&mut buffer, &big);
        assert_eq!(out.len(), 1);
        assert_eq!(
            out[0].as_ref().unwrap_err(),
            "pipe frame exceeded the 4194304 byte limit"
        );
        assert!(buffer.is_empty());
    }

    #[test]
    fn pipe_decoder_reports_complete_invalid_frames() {
        let mut buffer = String::new();
        let out = decode_pipe_frames(&mut buffer, "{\"schema_version\":2,\"agents\":[]}\n");
        assert_eq!(out.len(), 1);
        assert_eq!(
            out[0].as_ref().unwrap_err(),
            "unsupported noc schema version 2 (expected 1)"
        );
        // An incomplete remainder is kept, then reported when the sender stops.
        assert!(decode_pipe_frames(&mut buffer, "{\"schema_ver").is_empty());
        assert_eq!(buffer, "{\"schema_ver");
        let out = decode_pipe_frames(&mut buffer, "");
        assert_eq!(out.len(), 1);
        assert!(out[0].is_err());
        assert!(buffer.is_empty());
        let out = decode_pipe_frames(&mut buffer, "not json\n");
        assert!(out[0].is_err());
    }

    #[test]
    fn tab_names_are_the_agent_title_cut_to_a_limit() {
        let long = agent(&format!(r#"{{"id":"a","user_name":"{}"}}"#, "n".repeat(40)));
        assert_eq!(agent_tab_name(&long), format!("{}\u{2026}", "n".repeat(32)));
        let exact = agent(&format!(r#"{{"id":"a","user_name":"{}"}}"#, "n".repeat(32)));
        assert_eq!(agent_tab_name(&exact), "n".repeat(32));
    }

    #[test]
    fn recognizes_attach_commands_for_any_agent() {
        assert_eq!(
            attached_agent_id("noc attach agent-1", "noc").as_deref(),
            Some("agent-1")
        );
        assert_eq!(
            attached_agent_id("'/bin/noc' \"attach\" 'a b'", "noc"),
            None
        );
        assert_eq!(
            attached_agent_id("'/bin/noc' --verbose attach 'x'", "/other/noc").as_deref(),
            Some("x")
        );
        assert_eq!(attached_agent_id("noc attach", "noc"), None);
        assert_eq!(attached_agent_id("noc attach a b", "noc"), None);
        assert_eq!(attached_agent_id("claude attach x", "noc"), None);
        assert_eq!(
            attached_agent_id("noc  attach   a1", "noc").as_deref(),
            Some("a1")
        );
        assert_eq!(attached_agent_id("", "noc"), None);
    }

    #[test]
    fn recognises_a_pane_by_the_title_the_panel_gave_it() {
        let agents = vec![
            agent(
                r#"{"id":"a1","name":"Baggage review","display_state":"working","bucket":"working"}"#,
            ),
            agent(r#"{"id":"a2","name":"Other","display_state":"working","bucket":"working"}"#),
        ];
        let pane = ScanPane {
            title: "NOC: Baggage review".into(),
            ..terminal(1, 0, 10, 10)
        };
        let found = agent_in_pane(&agents, &pane, &BTreeMap::new(), "noc", None);
        assert_eq!(found.map(|a| a.id.as_str()), Some("a1"));
        assert!(agent_named_by_pane_title(&agents, "NOC: ").is_none());
        assert!(agent_named_by_pane_title(&agents, "NOC: Unknown").is_none());
        // Two agents with one name match nothing.
        let twins = vec![
            agent(r#"{"id":"a1","name":"Twin","display_state":"working","bucket":"working"}"#),
            agent(r#"{"id":"a2","user_name":"Twin","display_state":"working","bucket":"working"}"#),
        ];
        assert!(agent_named_by_pane_title(&twins, "Twin").is_none());
    }

    #[test]
    fn matches_a_hand_attached_pane_by_its_claude_title() {
        let agents = vec![
            agent(
                r#"{"id":"live","user_name":"Hand attached","display_state":"idle","bucket":"review"}"#,
            ),
            agent(
                r#"{"id":"ended","name":"Ended one","display_state":"not_live","bucket":"quiet"}"#,
            ),
        ];
        let titled = |t: &str| ScanPane {
            title: t.to_string(),
            ..terminal(1, 0, 10, 10)
        };
        let found = agent_in_pane(
            &agents,
            &titled("\u{2733} Hand attached"),
            &BTreeMap::new(),
            "noc",
            None,
        );
        assert_eq!(found.map(|a| a.id.as_str()), Some("live"));
        // A Quiet agent has no process, so a title never claims it.
        assert!(agent_in_pane(
            &agents,
            &titled("\u{2733} Ended one"),
            &BTreeMap::new(),
            "noc",
            None
        )
        .is_none());
        // Plugin and exited panes resolve to nothing.
        let mut plugin_pane = titled("Hand attached");
        plugin_pane.is_plugin = true;
        assert!(agent_in_pane(&agents, &plugin_pane, &BTreeMap::new(), "noc", None).is_none());
        let mut gone = titled("Hand attached");
        gone.exited = true;
        assert!(agent_in_pane(&agents, &gone, &BTreeMap::new(), "noc", None).is_none());
    }

    fn pane_at(id: u32, tab: usize, focused: bool, floating: bool) -> ScanPane {
        ScanPane {
            is_focused: focused,
            is_floating: floating,
            ..terminal(id, tab, 10, 10)
        }
    }

    #[test]
    fn focused_pane_prefers_visible_floating_panes() {
        let panes = vec![
            pane_at(1, 0, true, false),
            pane_at(2, 0, true, true),
            ScanPane {
                is_suppressed: true,
                ..pane_at(3, 0, true, true)
            },
        ];
        assert_eq!(focused_pane(&panes, true).map(|p| p.id), Some(2));
        assert_eq!(focused_pane(&panes, false).map(|p| p.id), Some(1));
        // Falls back to the tiled pane when no floating pane is focused.
        let panes = vec![pane_at(1, 0, true, false), pane_at(2, 0, false, true)];
        assert_eq!(focused_pane(&panes, true).map(|p| p.id), Some(1));
        assert!(focused_pane(&[pane_at(1, 0, false, false)], true).is_none());
    }

    #[test]
    fn identifies_the_agent_in_a_pane_from_launches_commands_and_bindings() {
        let agents = vec![
            agent(r#"{"id":"launched","name":"L","display_state":"idle","bucket":"review"}"#),
            agent(r#"{"id":"cmd","name":"C","display_state":"idle","bucket":"review"}"#),
            agent(
                r#"{"id":"bound","name":"B","display_state":"idle","bucket":"review","live_state":"live","frontend_bindings":[
                  {"kind":"zellij","session":"mine","pane_id":"30","state":"connected","observed_at":"2026-02-01T00:00:00Z"},
                  {"kind":"zellij","session":"elsewhere","pane_id":"31","state":"connected","observed_at":"2026-02-01T00:00:00Z"}]}"#,
            ),
            agent(
                r#"{"id":"newer","name":"N","display_state":"idle","bucket":"review","live_state":"live","frontend_bindings":[
                  {"kind":"zellij","session":"","pane_id":"30","state":"connected","observed_at":"2026-03-01T00:00:00Z"}]}"#,
            ),
            agent(
                r#"{"id":"ended","name":"E","display_state":"idle","bucket":"review","live_state":"not_live","frontend_bindings":[
                  {"kind":"zellij","session":"mine","pane_id":"32","state":"connected","observed_at":"2026-04-01T00:00:00Z"}]}"#,
            ),
            agent(
                r#"{"id":"disc","name":"D","display_state":"idle","bucket":"review","live_state":"live","frontend_bindings":[
                  {"kind":"zellij","session":"mine","pane_id":"33","state":"disconnected","observed_at":"2026-04-01T00:00:00Z"}]}"#,
            ),
        ];
        let launched: BTreeMap<String, LaunchedPane> = [(
            "launched".to_string(),
            LaunchedPane {
                id: 10,
                observed: true,
            },
        )]
        .into();
        let id_of = |pane: &ScanPane, session: Option<&str>| {
            agent_in_pane(&agents, pane, &launched, "noc", session).map(|a| a.id.clone())
        };
        assert_eq!(
            id_of(&terminal(10, 0, 1, 1), None).as_deref(),
            Some("launched")
        );
        assert_eq!(
            id_of(&command_pane(20, 0, "noc attach cmd"), None).as_deref(),
            Some("cmd")
        );
        // A shell pane running a provider command that names the agent's
        // native session, whole or short.
        let ids = vec![
            agent(
                r#"{"id":"n1","native_session_id":"2f5ac2d2-f543-4b7e","native_short_id":"2f5ac2d2"}"#,
            ),
            agent(
                r#"{"id":"n2","native_session_id":"128fa5db-09e1-40e5","native_short_id":"128fa5db"}"#,
            ),
            agent(r#"{"id":"n3"}"#),
        ];
        let running = |command: &str| {
            let pane = ScanPane {
                pane_command: Some(command.to_string()),
                ..terminal(40, 0, 1, 1)
            };
            agent_in_pane(&ids, &pane, &BTreeMap::new(), "/opt/bin/noc", None).map(|a| a.id.clone())
        };
        assert_eq!(running("claude attach 2f5ac2d2").as_deref(), Some("n1"));
        assert_eq!(
            running("claude --resume=128fa5db-09e1-40e5").as_deref(),
            Some("n2")
        );
        assert_eq!(
            running("codex resume '128fa5db-09e1-40e5'").as_deref(),
            Some("n2")
        );
        assert_eq!(running("noc attach n3").as_deref(), Some("n3"));
        for other in [
            "claude",
            "vim 2f5ac2d2.txt",
            "claude attach 2f5ac2",
            "",
            "diff 2f5ac2d2 128fa5db",
        ] {
            assert_eq!(running(other), None, "{other:?}");
        }
        // The command the pane was started with comes first.
        let both = ScanPane {
            pane_command: Some("claude attach 2f5ac2d2".to_string()),
            ..command_pane(41, 0, "noc attach n3")
        };
        assert_eq!(
            agent_in_pane(&ids, &both, &BTreeMap::new(), "noc", None).map(|a| a.id.as_str()),
            Some("n3")
        );
        // Two bindings on pane 30: the newest observation wins; its empty session matches.
        assert_eq!(
            id_of(&terminal(30, 0, 1, 1), Some("mine")).as_deref(),
            Some("newer")
        );
        // A binding for another session never claims this session's pane.
        assert_eq!(id_of(&terminal(31, 0, 1, 1), Some("mine")), None);
        // Unknown session name: any session matches.
        assert_eq!(
            id_of(&terminal(31, 0, 1, 1), None).as_deref(),
            Some("bound")
        );
        // Ended and disconnected bindings are stale.
        assert_eq!(id_of(&terminal(32, 0, 1, 1), Some("mine")), None);
        assert_eq!(id_of(&terminal(33, 0, 1, 1), Some("mine")), None);
        // A launched pane that exited no longer resolves.
        let mut exited = terminal(10, 0, 1, 1);
        exited.exited = true;
        assert_eq!(id_of(&exited, None), None);
    }
}
