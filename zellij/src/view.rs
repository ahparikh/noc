//! Rendering of the panel and the floating details window.

use crate::app::{Connection, ListItem, QuietAge, Transport, ZellijNoc, SPINNER};
use crate::model::{age_label, Agent, AgentGroup};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};
use zellij_tile::prelude::{ModeInfo, PaletteColor};

pub const FOOTER: &str = "[?] keys  [d]etails  [D] window";
pub const DETAILS_FOOTER: &str = "Esc / D  close";

// ---- theme ----

#[derive(Debug, Clone, Copy)]
pub struct Theme {
    pub text: PaletteColor,
    pub muted: PaletteColor,
    pub accent: PaletteColor,
    pub selected_text: PaletteColor,
    pub selected_background: PaletteColor,
    pub attention: PaletteColor,
    pub active: PaletteColor,
    pub info: PaletteColor,
    pub error: PaletteColor,
    pub blocked: PaletteColor,
    pub working: PaletteColor,
    pub review: PaletteColor,
    pub quiet: PaletteColor,
    /// Whether secondary text is also drawn with the terminal's dim
    /// attribute. The built-in colours are already the intended shade, and
    /// dimming them makes them unreadable on a dark background.
    pub dim: bool,
}

const fn rgb(r: u8, g: u8, b: u8) -> PaletteColor {
    PaletteColor::Rgb((r, g, b))
}

impl Default for Theme {
    fn default() -> Self {
        Theme::herdr()
    }
}

impl Theme {
    /// The colours of herdr's default theme (Catppuccin Mocha with a blue
    /// accent), on the same roles: blocked red, working yellow, finished
    /// teal, everything secondary in the overlay greys.
    pub fn herdr() -> Theme {
        let blue = rgb(137, 180, 250);
        let teal = rgb(148, 226, 213);
        let red = rgb(243, 139, 168);
        Theme {
            text: rgb(205, 214, 244),
            muted: rgb(147, 153, 178),
            accent: blue,
            selected_text: rgb(205, 214, 244),
            selected_background: rgb(49, 50, 68),
            attention: rgb(250, 179, 135),
            active: blue,
            info: teal,
            error: red,
            blocked: red,
            working: rgb(249, 226, 175),
            review: teal,
            quiet: rgb(147, 153, 178),
            dim: false,
        }
    }

    /// The colours of the Zellij theme in use, with the groups on fixed
    /// ANSI palette entries.
    pub fn from_mode_info(info: &ModeInfo) -> Theme {
        let colors = &info.style.colors;
        Theme {
            text: colors.text_unselected.base,
            muted: colors.list_unselected.base,
            accent: colors.text_unselected.emphasis_0,
            selected_text: colors.list_selected.base,
            selected_background: colors.list_selected.background,
            attention: colors.text_unselected.emphasis_0,
            active: colors.exit_code_success.base,
            info: colors.text_unselected.emphasis_1,
            error: colors.exit_code_error.base,
            blocked: PaletteColor::EightBit(1),
            working: PaletteColor::EightBit(2),
            review: PaletteColor::EightBit(6),
            quiet: PaletteColor::EightBit(8),
            dim: true,
        }
    }

    /// The style of secondary text: labels, second lines, the footer.
    pub fn muted_style(&self) -> Style {
        let mut style = Style::fg(self.muted);
        style.dim = self.dim;
        style
    }

    pub fn group(&self, group: AgentGroup) -> PaletteColor {
        match group {
            AgentGroup::Blocked => self.blocked,
            AgentGroup::Working => self.working,
            AgentGroup::Review => self.review,
            AgentGroup::Quiet => self.quiet,
        }
    }
}

// ---- styled text ----

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Style {
    pub fg: Option<PaletteColor>,
    pub bg: Option<PaletteColor>,
    pub bold: bool,
    pub dim: bool,
}

impl Style {
    fn fg(color: PaletteColor) -> Style {
        Style {
            fg: Some(color),
            ..Style::default()
        }
    }

    fn bold(mut self) -> Style {
        self.bold = true;
        self
    }

    fn on(mut self, bg: PaletteColor) -> Style {
        self.bg = Some(bg);
        self
    }

    fn selected(theme: &Theme) -> Style {
        Style::fg(theme.selected_text).on(theme.selected_background)
    }

    fn fill(theme: &Theme) -> Style {
        Style::default().on(theme.selected_background)
    }
}

fn color_code(color: PaletteColor, background: bool) -> String {
    let base = if background { 48 } else { 38 };
    match color {
        PaletteColor::EightBit(n) => format!("{base};5;{n}"),
        PaletteColor::Rgb((r, g, b)) => format!("{base};2;{r};{g};{b}"),
    }
}

pub fn styled_span(text: &str, style: Style) -> String {
    let mut codes: Vec<String> = Vec::new();
    if style.bold {
        codes.push("1".to_string());
    }
    if style.dim {
        codes.push("2".to_string());
    }
    if let Some(fg) = style.fg {
        codes.push(color_code(fg, false));
    }
    if let Some(bg) = style.bg {
        codes.push(color_code(bg, true));
    }
    if codes.is_empty() {
        text.to_string()
    } else {
        format!("\x1b[{}m{}\x1b[0m", codes.join(";"), text)
    }
}

#[derive(Debug, Clone)]
struct Span {
    text: String,
    style: Style,
}

#[derive(Debug, Clone, Default)]
struct Line {
    spans: Vec<Span>,
    fill: Style,
    trailing_reset: bool,
}

/// Control characters would reach the terminal verbatim; drop them.
fn sanitize(text: &str) -> String {
    text.chars().filter(|c| !c.is_control()).collect()
}

impl Line {
    fn empty() -> Line {
        Line::default()
    }

    fn plain(text: &str, style: Style) -> Line {
        Line::empty().with(text, style)
    }

    fn with(mut self, text: &str, style: Style) -> Line {
        self.spans.push(Span {
            text: sanitize(text),
            style,
        });
        self
    }

    fn filled(mut self, fill: Style) -> Line {
        self.fill = fill;
        self
    }

    fn width(&self) -> usize {
        self.spans.iter().map(|s| s.text.width()).sum()
    }

    fn fitted(&self, cols: usize) -> Vec<Span> {
        let total = self.width();
        let mut out: Vec<Span> = Vec::new();
        let mut used = 0usize;
        if total <= cols {
            out.extend(self.spans.iter().cloned());
            used = total;
        } else {
            let budget = cols.saturating_sub(1);
            let mut last_style = Style::default();
            'spans: for span in &self.spans {
                let mut text = String::new();
                let mut cut = false;
                for ch in span.text.chars() {
                    let w = ch.width().unwrap_or(0);
                    if used + w > budget {
                        cut = true;
                        break;
                    }
                    text.push(ch);
                    used += w;
                }
                last_style = span.style;
                if !text.is_empty() {
                    out.push(Span {
                        text,
                        style: span.style,
                    });
                }
                if cut {
                    break 'spans;
                }
            }
            if cols > 0 {
                out.push(Span {
                    text: "\u{2026}".to_string(),
                    style: last_style,
                });
                used += 1;
            }
        }
        if used < cols {
            out.push(Span {
                text: " ".repeat(cols - used),
                style: self.fill,
            });
        }
        out
    }

    fn render(&self, cols: usize) -> String {
        let mut out: String = self
            .fitted(cols)
            .iter()
            .map(|s| styled_span(&s.text, s.style))
            .collect();
        if self.trailing_reset {
            out.push_str("\x1b[0m");
        }
        out
    }
}

// ---- text primitives ----

pub fn truncate(text: &str, width: usize) -> String {
    if width == 0 {
        return String::new();
    }
    if text.width() <= width {
        return text.to_string();
    }
    if width == 1 {
        return "\u{2026}".to_string();
    }
    let mut out = String::new();
    let mut used = 0;
    for ch in text.chars() {
        let w = ch.width().unwrap_or(0);
        if used + w > width - 1 {
            break;
        }
        out.push(ch);
        used += w;
    }
    out.push('\u{2026}');
    out
}

pub fn pad(text: &str, width: usize) -> String {
    let w = text.width();
    if w >= width {
        text.to_string()
    } else {
        format!("{text}{}", " ".repeat(width - w))
    }
}

pub fn wrap(text: &str, width: usize) -> Vec<String> {
    if width == 0 {
        return Vec::new();
    }
    let mut lines: Vec<String> = Vec::new();
    let mut current = String::new();
    for word in text.split_whitespace() {
        if word.width() > width {
            if !current.is_empty() {
                lines.push(std::mem::take(&mut current));
            }
            lines.push(truncate(word, width));
            continue;
        }
        if current.is_empty() {
            current.push_str(word);
        } else if current.width() + 1 + word.width() <= width {
            current.push(' ');
            current.push_str(word);
        } else {
            lines.push(std::mem::replace(&mut current, word.to_string()));
        }
    }
    if !current.is_empty() {
        lines.push(current);
    }
    lines
}

fn plural(count: usize) -> &'static str {
    if count == 1 {
        ""
    } else {
        "s"
    }
}

// ---- frame ----

fn footer_line(theme: &Theme, text: &str) -> Line {
    Line::plain(text, theme.muted_style())
}

/// Pad to `rows - 1` and put the footer last, but only if content has not
/// already filled `rows`; then truncate to `rows`.
fn fill_and_footer(mut lines: Vec<Line>, rows: usize, cols: usize, footer: Line) -> Vec<String> {
    if lines.len() < rows {
        while lines.len() < rows - 1 {
            lines.push(Line::empty());
        }
        lines.push(footer);
    }
    lines.truncate(rows);
    lines.iter().map(|l| l.render(cols)).collect()
}

fn shown_agent_count(app: &ZellijNoc) -> usize {
    app.items
        .iter()
        .map(|item| match item {
            ListItem::Agent(i) => {
                usize::from(app.flat || app.agents[*i].group() != AgentGroup::Quiet)
            }
            ListItem::Section { count, .. } => *count,
        })
        .sum()
}

fn item_group(app: &ZellijNoc, item: &ListItem) -> AgentGroup {
    match item {
        ListItem::Agent(i) => app.agents[*i].group(),
        ListItem::Section { .. } => AgentGroup::Quiet,
    }
}

fn group_count(app: &ZellijNoc, group: AgentGroup) -> usize {
    app.items
        .iter()
        .map(|item| match item {
            ListItem::Agent(i) => usize::from(
                (app.flat || group != AgentGroup::Quiet) && app.agents[*i].group() == group,
            ),
            ListItem::Section { count, .. } => {
                if group == AgentGroup::Quiet {
                    *count
                } else {
                    0
                }
            }
        })
        .sum()
}

/// The header, with `↑ vX.Y.Z` at the right edge when a newer release
/// exists and there is room for it.
fn header_line(header: &str, update: Option<&str>, theme: &Theme, cols: usize) -> Line {
    let line = Line::plain(header, Style::fg(theme.accent).bold());
    let Some(version) = update else {
        return line;
    };
    let marker = format!("\u{2191} {version}");
    let used = line.width() + marker.width();
    if used + 2 > cols {
        return line;
    }
    line.with(&" ".repeat(cols - used), Style::default())
        .with(&marker, Style::fg(theme.attention).bold())
}

/// The plugin's own version, and under it the newer release when one is
/// known.
fn version_help_lines(update: Option<&str>, theme: &Theme) -> Vec<Line> {
    let muted = theme.muted_style();
    let mut lines = vec![
        Line::empty(),
        Line::plain(&format!("version     v{}", crate::update::CURRENT), muted),
    ];
    if let Some(version) = update {
        lines.push(Line::plain(
            &format!(
                "{}newer plugin release;",
                pad(&format!("\u{2191} {version} "), 12)
            ),
            Style::fg(theme.attention),
        ));
        lines.push(Line::plain("            set it in the plugin URL", muted));
    }
    lines
}

fn connection_line(app: &ZellijNoc, theme: &Theme) -> Line {
    let dim = app.connection != Connection::ServerUnavailable;
    let (text, color) = if let Some(rename) = &app.pending_rename {
        (format!("Rename: {}\u{258f}", rename.text), theme.accent)
    } else if let Some(stop) = &app.pending_stop {
        (format!("Stop {}?  y / n", stop.title), theme.attention)
    } else if let Some(search) = app.search.as_ref().filter(|s| s.typing) {
        (format!("/ {}\u{258f}", search.text), theme.accent)
    } else if let Some(notice) = &app.notice {
        (notice.clone(), theme.info)
    } else if let Some(problem) = app.config_problem.as_ref().or(app.guest_problem.as_ref()) {
        (format!("!  {problem}"), theme.attention)
    } else {
        match &app.connection {
            Connection::Connecting => {
                if app.config.transport == Transport::Pipe {
                    (
                        format!("\u{25cc}  waiting for {}", app.config.pipe_name),
                        theme.info,
                    )
                } else {
                    ("\u{25cc}  connecting".to_string(), theme.info)
                }
            }
            Connection::Connected(source) => match app.search_text() {
                Some(query) => (format!("/ {query}"), theme.accent),
                None => (
                    format!("\u{25cf}  connected \u{b7} {}", source.as_str()),
                    theme.active,
                ),
            },
            Connection::ServerUnavailable => (
                "!  server offline \u{b7} retrying".to_string(),
                theme.attention,
            ),
            Connection::Error(e) => (format!("\u{d7}  disconnected \u{b7} {e}"), theme.error),
        }
    };
    let mut style = Style::fg(color);
    style.dim = dim && theme.dim;
    Line::plain(&text, style)
}

/// The "Anywhere" keys are the host layout's defaults; a layout that rebinds
/// them is the place to look for the real ones.
fn help_lines(theme: &Theme) -> Vec<Line> {
    let heading = Style::fg(theme.accent).bold();
    let muted = theme.muted_style();
    let mut lines = vec![Line::plain("In the list", heading)];
    for entry in [
        "j / k       move selection",
        "gg / G      first / last",
        "Ctrl-d / u  half page",
        "Enter / l   open agent, or a section",
        "h           close a Quiet section",
        "p           open as a pane here",
        "s           open stacked here",
        "w           shell in its worktree",
        "d           toggle details",
        "D           details window",
        "n           rename agent",
        "x           stop agent (asks y/n)",
        "r           refresh",
        "/           search (also older:14d)",
        "u           check for a new release",
        "? / Esc     close help",
    ] {
        lines.push(Line::plain(entry, muted));
    }
    lines.push(Line::empty());
    lines.push(Line::plain("Anywhere", heading));
    for entry in [
        "Alt-m       jump to list and back",
        "Alt-o       hide / show this panel",
        "Alt-d       detach both sessions",
    ] {
        lines.push(Line::plain(entry, muted));
    }
    lines
}

fn empty_state_lines(app: &ZellijNoc, theme: &Theme) -> Vec<Line> {
    let muted = theme.muted_style();
    let mut lines = Vec::new();
    match &app.connection {
        Connection::ServerUnavailable => {
            lines.push(Line::plain(
                "noc server is not running.",
                Style::fg(theme.attention).bold(),
            ));
            lines.push(Line::empty());
            lines.push(Line::plain("Start it with:", muted));
            lines.push(Line::plain("  noc server --bg", Style::fg(theme.info)));
            lines.push(Line::empty());
            lines.push(Line::plain(
                "This panel will reconnect automatically.",
                muted,
            ));
        }
        Connection::Error(_) => {
            lines.push(Line::plain(
                "No registry snapshot available.",
                Style::fg(theme.error),
            ));
        }
        Connection::Connecting | Connection::Connected(_) => {
            if app.search.is_some() {
                lines.push(Line::plain("No agents match.", muted));
                lines.push(Line::empty());
                lines.push(Line::plain("Words match names, folders,", muted));
                lines.push(Line::plain("branches and requests.", muted));
                lines.push(Line::plain("older:14d   idle two weeks or more", muted));
                lines.push(Line::plain("newer:3d    active in the last 3 days", muted));
            } else {
                lines.push(Line::plain("No live agents.", muted));
            }
        }
    }
    lines
}

fn agent_symbol(app: &ZellijNoc, agent: &Agent) -> String {
    if agent.group() == AgentGroup::Quiet {
        if agent.display_state == "not_live" || agent.live_state == "not_live" {
            "\u{25cc}".to_string()
        } else {
            "\u{25cb}".to_string()
        }
    } else if agent.display_state == "working" {
        SPINNER[app.spinner_frame % SPINNER.len()].to_string()
    } else {
        agent.state_symbol().to_string()
    }
}

fn agent_primary_line(
    app: &ZellijNoc,
    theme: &Theme,
    agent: &Agent,
    selected: bool,
    cols: usize,
) -> Line {
    let group = agent.group();
    let indent = if group == AgentGroup::Quiet { "  " } else { "" };
    let prefix = format!(" {indent}{} ", agent_symbol(app, agent));
    let title = truncate(&agent.title(), cols.saturating_sub(prefix.width()));
    let mut line = if selected {
        let style = Style::selected(theme).bold();
        Line::plain(&prefix, style)
            .with(&title, style)
            .filled(Style::fill(theme))
    } else {
        let title_color = if group == AgentGroup::Quiet {
            theme.group(group)
        } else {
            theme.text
        };
        Line::plain(&prefix, Style::fg(theme.group(group)).bold())
            .with(&title, Style::fg(title_color))
    };
    line.trailing_reset = true;
    line
}

fn secondary_text(app: &ZellijNoc, agent: &Agent) -> String {
    let group = agent.group();
    let indent = if group == AgentGroup::Quiet { "  " } else { "" };
    let age = age_label(agent.recency(), &app.generated_at);
    let rest = if group == AgentGroup::Blocked && !agent.attention.detail.trim().is_empty() {
        agent.attention.detail.trim().to_string()
    } else {
        agent.workspace_label()
    };
    let mut text = format!(
        "   {indent}{} {age:>3}",
        agent.provider_icon(app.config.nerd_icons)
    );
    if !rest.is_empty() {
        text.push_str("  ");
        text.push_str(&rest);
    }
    if group == AgentGroup::Quiet && agent.display_state.starts_with("needs_") {
        text.push_str("  stopped waiting on you");
    }
    text.trim_end().to_string()
}

fn agent_secondary_line(app: &ZellijNoc, theme: &Theme, agent: &Agent, selected: bool) -> Line {
    let text = secondary_text(app, agent);
    if selected {
        Line::plain(&text, Style::selected(theme)).filled(Style::fill(theme))
    } else {
        Line::plain(&text, theme.muted_style())
    }
}

fn section_line(
    theme: &Theme,
    age: QuietAge,
    count: usize,
    expanded: bool,
    selected: bool,
) -> Line {
    let arrow = if expanded { "\u{25be}" } else { "\u{25b8}" };
    let text = format!(" {arrow} {}  {count}", pad(age.label(), 6));
    if selected {
        Line::plain(&text, Style::selected(theme).bold()).filled(Style::fill(theme))
    } else {
        Line::plain(&text, Style::fg(theme.group(AgentGroup::Quiet)))
    }
}

/// Returns the list lines and the index of the first selected line.
fn agent_list_lines(app: &ZellijNoc, theme: &Theme, cols: usize) -> (Vec<Line>, usize) {
    let mut lines = Vec::new();
    let mut selected_line = None;
    let mut current_group: Option<AgentGroup> = None;
    for (index, item) in app.items.iter().enumerate() {
        let group = item_group(app, item);
        if current_group != Some(group) {
            current_group = Some(group);
            lines.push(Line::plain(
                &format!("{}  {}", group.title(), group_count(app, group)),
                Style::fg(theme.group(group)).bold(),
            ));
        }
        let selected = index == app.selected_index;
        if selected && selected_line.is_none() {
            selected_line = Some(lines.len());
        }
        match item {
            ListItem::Agent(i) => {
                let agent = &app.agents[*i];
                lines.push(agent_primary_line(app, theme, agent, selected, cols));
                lines.push(agent_secondary_line(app, theme, agent, selected));
            }
            ListItem::Section {
                age,
                count,
                expanded,
            } => lines.push(section_line(theme, *age, *count, *expanded, selected)),
        }
    }
    (lines, selected_line.unwrap_or(0))
}

// ---- details ----

fn label_line(label: &str, value: &str, theme: &Theme) -> Line {
    Line::plain(&format!("{}{value}", pad(label, 10)), theme.muted_style())
}

fn append_preview(
    lines: &mut Vec<Line>,
    theme: &Theme,
    label: &str,
    value: &str,
    cols: usize,
    max: usize,
) {
    lines.push(Line::plain(label, Style::fg(theme.info).bold()));
    for text in wrap(value, cols.saturating_sub(2)).into_iter().take(max) {
        lines.push(Line::plain(&format!("  {text}"), theme.muted_style()));
    }
}

fn id_lines(agent: &Agent, theme: &Theme, roomy: bool) -> Vec<Line> {
    let short = agent.native_short_id.trim();
    let session = agent.native_session_id.trim();
    if roomy {
        let mut lines = Vec::new();
        if !short.is_empty() {
            lines.push(label_line("id", short, theme));
        }
        if !session.is_empty() && session != short {
            lines.push(label_line("session", session, theme));
        }
        lines
    } else if !short.is_empty() {
        vec![label_line("id", short, theme)]
    } else if !session.is_empty() {
        vec![label_line("id", session, theme)]
    } else {
        Vec::new()
    }
}

fn agent_detail_lines(
    agent: &Agent,
    theme: &Theme,
    cols: usize,
    roomy: bool,
    preview_lines: usize,
    now: &str,
) -> Vec<Line> {
    let mut lines = vec![Line::plain(&agent.title(), Style::fg(theme.text).bold())];
    let age = age_label(agent.recency(), now);
    let mut state = format!("{}  {}", agent.state_symbol(), agent.state_label());
    if !age.is_empty() {
        state.push_str(&format!(" \u{b7} {age} ago"));
    }
    lines.push(Line::plain(&state, Style::fg(theme.group(agent.group()))));
    lines.push(label_line(
        "provider",
        &agent.provider_detail_label(),
        theme,
    ));
    if !agent.user_name.is_empty() && !agent.name.is_empty() && agent.user_name != agent.name {
        lines.push(label_line("native", &agent.name, theme));
    }
    lines.extend(id_lines(agent, theme, roomy));
    let model = agent.model.trim();
    let effort = agent.reasoning_effort.trim();
    match (model.is_empty(), effort.is_empty()) {
        (false, false) => lines.push(label_line(
            "model",
            &format!("{model} \u{b7} {effort}"),
            theme,
        )),
        (false, true) => lines.push(label_line("model", model, theme)),
        (true, false) => lines.push(label_line("effort", effort, theme)),
        (true, true) => {}
    }
    if let Some(workspace) = &agent.workspace {
        let repo = if workspace.is_git() {
            workspace.repo_name()
        } else {
            String::new()
        };
        let branch = workspace.branch.trim();
        match (repo.is_empty(), branch.is_empty()) {
            (false, false) => lines.push(label_line(
                "repo",
                &format!("{repo} \u{b7} {branch}"),
                theme,
            )),
            (false, true) => lines.push(label_line("repo", &repo, theme)),
            (true, false) => lines.push(label_line("repo", branch, theme)),
            (true, true) => {}
        }
        if let Some(name) = workspace.worktree_name() {
            lines.push(label_line("worktree", &name, theme));
        }
        if !workspace.path.trim().is_empty() {
            lines.push(label_line("cwd", workspace.path.trim(), theme));
        }
    }
    let request = agent
        .initial_request
        .as_ref()
        .map(|r| r.text.trim().to_string())
        .unwrap_or_default();
    if !request.is_empty() {
        append_preview(&mut lines, theme, "ask", &request, cols, preview_lines);
    }
    let summary = agent.summary.trim();
    if !summary.is_empty() && summary != request {
        append_preview(&mut lines, theme, "latest", summary, cols, preview_lines);
    }
    if let Some(binding) = agent.zellij_binding() {
        let place = if binding.session.is_empty() {
            binding.pane_id.clone()
        } else {
            format!("{}/{}", binding.session, binding.pane_id)
        };
        lines.push(label_line(
            "pane",
            &format!("{place} \u{b7} observed"),
            theme,
        ));
    }
    lines
}

fn rule(theme: &Theme, cols: usize) -> Line {
    let mut style = Style::fg(theme.accent);
    style.dim = theme.dim;
    Line::plain(&"\u{2500}".repeat(cols), style)
}

fn inline_detail_lines(
    app: &ZellijNoc,
    theme: &Theme,
    cols: usize,
    detail_height: usize,
) -> Vec<Line> {
    let mut lines = vec![rule(theme, cols)];
    match app.selected_item() {
        Some(ListItem::Agent(i)) => {
            lines.push(Line::plain("Details", Style::fg(theme.accent).bold()));
            let preview = if detail_height >= 16 { 2 } else { 1 };
            lines.extend(agent_detail_lines(
                &app.agents[i],
                theme,
                cols,
                false,
                preview,
                &app.generated_at,
            ));
        }
        Some(ListItem::Section { age, .. }) => {
            lines.push(Line::plain(
                &format!("Quiet \u{b7} {}", age.label()),
                Style::fg(theme.accent).bold(),
            ));
            let muted = theme.muted_style();
            for text in [
                "Conversations with no process.",
                "Enter opens this section; Enter on",
                "an agent resumes it; h closes.",
            ] {
                lines.push(Line::plain(text, muted));
            }
        }
        None => {}
    }
    lines.truncate(detail_height);
    lines
}

// ---- entry points ----

/// The panel: the list (or help, or the empty state) from the top, and at
/// the bottom the two status lines, the inline details when they are shown,
/// and the footer. The status lines are the last thing to give way.
pub fn render(app: &mut ZellijNoc, theme: &Theme, rows: usize, cols: usize) -> Vec<String> {
    app.last_render_rows = rows;
    if rows == 0 || cols == 0 {
        return Vec::new();
    }
    let count = shown_agent_count(app);
    let header = if app.flat {
        format!("noc  {count} match{}", if count == 1 { "" } else { "es" })
    } else {
        format!("noc  {count} agent{}", plural(count))
    };
    let status = [
        header_line(&header, app.update.as_deref(), theme, cols),
        connection_line(app, theme),
    ];
    if rows <= status.len() {
        return status[..rows].iter().map(|l| l.render(cols)).collect();
    }
    let footer = rows > status.len() + 1;
    let body_rows = rows - status.len() - usize::from(footer);
    let (mut body, mut details) = if app.show_help {
        let mut lines = help_lines(theme);
        lines.extend(version_help_lines(app.update.as_deref(), theme));
        (lines, Vec::new())
    } else if app.items.is_empty() {
        (empty_state_lines(app, theme), Vec::new())
    } else {
        let detail_height = if app.show_details && body_rows >= 12 {
            body_rows / 3
        } else {
            0
        };
        let list_height = (body_rows - detail_height).max(1);
        let (list, selected_line) = agent_list_lines(app, theme, cols);
        let start = selected_line
            .saturating_sub(list_height / 2)
            .min(list.len().saturating_sub(list_height));
        let window: Vec<Line> = list.into_iter().skip(start).take(list_height).collect();
        let mut details = Vec::new();
        if detail_height > 0 {
            details = inline_detail_lines(app, theme, cols, detail_height);
            details.resize(detail_height, Line::empty());
        }
        (window, details)
    };
    body.resize(body_rows - details.len(), Line::empty());
    let mut lines = body;
    lines.extend(status);
    lines.append(&mut details);
    if footer {
        lines.push(footer_line(theme, FOOTER));
    }
    lines.iter().map(|l| l.render(cols)).collect()
}

pub fn render_details_window(
    agent: Option<&Agent>,
    connection: &Connection,
    generated_at: &str,
    theme: &Theme,
    rows: usize,
    cols: usize,
) -> Vec<String> {
    if rows == 0 || cols == 0 {
        return Vec::new();
    }
    let lines = match agent {
        Some(agent) => {
            let preview = (rows / 5).clamp(2, 8);
            agent_detail_lines(agent, theme, cols, true, preview, generated_at)
        }
        None => vec![match connection {
            Connection::Error(e) => Line::plain(&format!("\u{d7}  {e}"), Style::fg(theme.error)),
            Connection::ServerUnavailable => {
                Line::plain("!  noc server is not running", Style::fg(theme.attention))
            }
            _ => Line::plain("\u{25cc}  loading", Style::fg(theme.info)),
        }],
    };
    fill_and_footer(lines, rows, cols, footer_line(theme, DETAILS_FOOTER))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::app::{Config, PendingStop};
    use crate::model::{parse_agent, parse_snapshot};

    const NOW: &str = "2026-09-24T12:00:00Z";

    fn strip(text: &str) -> String {
        let mut out = String::new();
        let mut chars = text.chars();
        while let Some(c) = chars.next() {
            if c == '\x1b' {
                for c in chars.by_ref() {
                    if c == 'm' {
                        break;
                    }
                }
            } else {
                out.push(c);
            }
        }
        out
    }

    fn plain(lines: &[String]) -> Vec<String> {
        lines.iter().map(|l| strip(l)).collect()
    }

    /// Where the header line is. The two status lines sit under the list:
    /// above the details when those are shown, and above the footer.
    fn header_at(text: &[String]) -> usize {
        text.iter()
            .position(|l| l.starts_with("noc  "))
            .expect("a header line")
    }

    fn header(text: &[String]) -> &str {
        text[header_at(text)].trim_end()
    }

    /// The line under the header: connection state, a prompt or a notice.
    fn status(text: &[String]) -> &str {
        text[header_at(text) + 1].trim_end()
    }

    fn app_with(agents: &str) -> ZellijNoc {
        let mut app = ZellijNoc::new(Config::default());
        let list = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":{agents}}}"#
        ))
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        app
    }

    const DEMO: &str = r#"[
      {"id":"b1","provider":"claude","source_kind":"background","native_short_id":"3cfff231","native_session_id":"3cfff231-aaaa-bbbb","user_name":"Auth migration","name":"auth","display_state":"needs_approval","bucket":"blocked","live_state":"live","last_activity_at":"2026-09-24T11:58:00Z","model":"opus","reasoning_effort":"high","attention":{"type":"approval_required","detail":"Bash: make clean"},"workspace":{"path":"/code/app/svc","root_path":"/code/app","kind":"main_checkout","common_git_dir":"/code/app/.git","branch":"main"},"initial_request":{"text":"Migrate the auth service to the new token format and update all callers"},"summary":"Waiting for approval to delete the build directory.","frontend_bindings":[{"kind":"zellij","session":"work","pane_id":"7","state":"connected","observed_at":"2026-09-24T11:58:00Z"}]},
      {"id":"w1","provider":"codex","name":"Cache refactor","display_state":"working","bucket":"working","live_state":"live","last_activity_at":"2026-09-24T11:59:30Z","workspace":{"path":"/code/app-wt/cache-fix","root_path":"/code/app-wt/cache-fix","kind":"worktree","common_git_dir":"/code/app/.git","branch":"cache-fix"}},
      {"id":"r1","provider":"claude","name":"Docs pass","display_state":"ready","bucket":"review","last_activity_at":"2026-09-24T09:00:00Z","workspace":{"path":"/code/docs","kind":"directory"}},
      {"id":"q1","provider":"claude","name":"Old question","display_state":"needs_input","bucket":"quiet","live_state":"not_live","ended_at":"2026-09-24T07:00:00Z"},
      {"id":"q2","provider":"codex","name":"Yesterday","display_state":"idle","bucket":"quiet","last_activity_at":"2026-09-23T06:00:00Z"},
      {"id":"q3","provider":"claude","name":"Monday","display_state":"not_live","bucket":"quiet","ended_at":"2026-09-21T08:00:00Z"},
      {"id":"q4","provider":"claude","name":"Ancient","display_state":"not_live","bucket":"quiet","ended_at":"2026-09-01T08:00:00Z"}]"#;

    #[test]
    fn renders_colored_grouped_master_detail_panel_within_bounds() {
        let mut app = app_with(DEMO);
        let lines = render(&mut app, &Theme::default(), 40, 38);
        assert_eq!(lines.len(), 40);
        assert!(lines.iter().any(|l| l.contains("\x1b[38;2;")));
        let text = plain(&lines);
        assert!(text.iter().all(|l| l.width() == 38), "{text:?}");
        let expect = [
            "Blocked  1",
            " ! Auth migration",
            "   \u{2733}  2m  Bash: make clean",
            "Working  1",
            " \u{280b} Cache refactor",
            "   \u{2b21}  0m  app \u{b7} cache-fix",
            "Review  1",
            " \u{25c6} Docs pass",
            "   \u{2733}  3h  docs",
            "Quiet  3",
            " \u{25b8} < 24h   1",
            " \u{25b8} < 48h   1",
            " \u{25b8} < 7d    1",
        ];
        for (i, want) in expect.iter().enumerate() {
            assert_eq!(text[i].trim_end(), *want, "line {i}");
        }
        let rest = text[13..25].iter().all(|l| l.trim().is_empty());
        assert!(rest, "list area is padded");
        // The status lines sit between the list and the details.
        assert_eq!(text[25].trim_end(), "noc  6 agents");
        assert_eq!(text[26].trim_end(), "\u{25cf}  connected \u{b7} poll");
        let details = &text[27..];
        let wanted = [
            "\u{2500}".repeat(38),
            "Details".to_string(),
            "Auth migration".to_string(),
            "!  needs approval \u{b7} 2m ago".to_string(),
            "provider  claude \u{b7} background".to_string(),
            "native    auth".to_string(),
            "id        3cfff231".to_string(),
            "model     opus \u{b7} high".to_string(),
            "repo      app \u{b7} main".to_string(),
            "cwd       /code/app/svc".to_string(),
            "ask".to_string(),
            "  Migrate the auth service to the new".to_string(),
            FOOTER.to_string(),
        ];
        for (got, want) in details.iter().zip(wanted.iter()) {
            assert_eq!(got.trim_end(), want);
        }
        assert!(!text.join("\n").contains("runtime"));
        assert!(!text.join("\n").contains("Ancient"));
    }

    #[test]
    fn details_take_the_bottom_third_and_ready_agents_show_their_age() {
        let mut app = app_with(DEMO);
        app.selected_index = 2; // Docs pass
        assert_eq!(app.selected_agent().unwrap().id, "r1");
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(text.len(), 30);
        // available = 27, detail height 9: the list takes 18 lines and the
        // two status lines follow, so the rule sits on line 20.
        assert_eq!(text[20].trim_end(), "\u{2500}".repeat(38));
        assert_eq!(text[21].trim_end(), "Details");
        assert_eq!(text[22].trim_end(), "Docs pass");
        assert_eq!(text[23].trim_end(), "\u{25c6}  review \u{b7} 3h ago");
        assert_eq!(text[29].trim_end(), FOOTER);
        let review = text
            .iter()
            .position(|l| l.starts_with("Review  1"))
            .unwrap();
        assert!(review < 20);
        // Details off: the list takes the whole body.
        app.toggle_details();
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert!(!text.iter().any(|l| l == "Details"));
        // Short panes hide the details.
        app.toggle_details();
        let text = plain(&render(&mut app, &Theme::default(), 14, 38));
        assert!(!text.iter().any(|l| l.starts_with("Details")));
    }

    #[test]
    fn working_agents_spin_and_quiet_agents_fold_into_counted_sections() {
        let mut app = app_with(DEMO);
        let first = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&first), "noc  6 agents");
        assert!(first[4].starts_with(" \u{280b} Cache refactor"));
        app.advance_spinner();
        let second = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert!(second[4].starts_with(" \u{2819} Cache refactor"));
        assert_eq!(first[9].trim_end(), "Quiet  3");
        // Selecting a section shows the section details.
        app.selected_index = app.items.len() - 3;
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert!(text.iter().any(|l| l.trim_end() == "Quiet \u{b7} < 24h"));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "Conversations with no process."));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "Enter opens this section; Enter on"));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "an agent resumes it; h closes."));
        // Open one: the count does not double and members indent.
        assert!(app.toggle_selected_section());
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(text[9].trim_end(), "Quiet  3");
        assert_eq!(text[10].trim_end(), " \u{25be} < 24h   1");
        assert_eq!(text[11].trim_end(), "   \u{25cc} Old question");
        assert_eq!(header(&text), "noc  6 agents");
    }

    #[test]
    fn quiet_agents_rest_even_if_they_stopped_waiting_on_the_user() {
        let mut app = app_with(
            r#"[{"id":"q","provider":"claude","name":"Pending","display_state":"needs_input","bucket":"quiet","live_state":"not_live","ended_at":"2026-09-24T07:00:00Z"},
                {"id":"i","provider":"codex","name":"Idle","display_state":"idle","bucket":"quiet","last_activity_at":"2026-09-24T09:00:00Z"}]"#,
        );
        assert!(app.select_agent("q"));
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        let pending = text.iter().position(|l| l.contains("Pending")).unwrap();
        assert_eq!(text[pending].trim_end(), "   \u{25cc} Pending");
        assert_eq!(
            text[pending + 1].trim_end(),
            "     \u{2733}  5h  stopped waiting on you"
        );
        let idle = text.iter().position(|l| l.contains("Idle")).unwrap();
        assert_eq!(text[idle].trim_end(), "   \u{25cb} Idle");
        assert_eq!(text[idle + 1].trim_end(), "     \u{2b21}  3h");
        // Details use the state symbol, not the resting one.
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "?  needs input \u{b7} 5h ago"));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "provider  claude \u{b7} ended"));
    }

    #[test]
    fn renders_keyboard_help_on_request() {
        let mut app = app_with("[]");
        app.toggle_help();
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(text[0].trim_end(), "In the list");
        assert_eq!(text[1].trim_end(), "j / k       move selection");
        assert_eq!(text[14].trim_end(), "/           search (also older:14d)");
        assert_eq!(text[15].trim_end(), "u           check for a new release");
        assert_eq!(text[16].trim_end(), "? / Esc     close help");
        assert_eq!(text[17], " ".repeat(38));
        assert_eq!(text[18].trim_end(), "Anywhere");
        assert_eq!(text[19].trim_end(), "Alt-m       jump to list and back");
        assert_eq!(text[20].trim_end(), "Alt-o       hide / show this panel");
        assert_eq!(text[21].trim_end(), "Alt-d       detach both sessions");
        assert_eq!(text[22], " ".repeat(38));
        assert_eq!(
            text[23].trim_end(),
            format!("version     v{}", crate::update::CURRENT)
        );
        // Status lines last, above the footer.
        assert_eq!(text[27].trim_end(), "noc  0 agents");
        assert_eq!(text[28].trim_end(), "\u{25cf}  connected \u{b7} poll");
        assert_eq!(text[29].trim_end(), FOOTER);
    }

    #[test]
    fn details_window_shows_full_identifiers_and_a_confirmation_prompt_shows_in_the_panel() {
        let agent = parse_agent(
            DEMO[DEMO.find('{').unwrap()..DEMO.find("\n      {\"id\":\"w1\"").unwrap()]
                .trim_end_matches(','),
        )
        .unwrap();
        let text = plain(&render_details_window(
            Some(&agent),
            &Connection::Connected(Transport::Poll),
            NOW,
            &Theme::default(),
            24,
            70,
        ));
        assert_eq!(text.len(), 24);
        let want = [
            "Auth migration",
            "!  needs approval \u{b7} 2m ago",
            "provider  claude \u{b7} background",
            "native    auth",
            "id        3cfff231",
            "session   3cfff231-aaaa-bbbb",
            "model     opus \u{b7} high",
            "repo      app \u{b7} main",
            "cwd       /code/app/svc",
            "ask",
            "  Migrate the auth service to the new token format and update all callers",
            "latest",
            "  Waiting for approval to delete the build directory.",
            "pane      work/7 \u{b7} observed",
        ];
        // The ask text wraps at 68 columns.
        assert_eq!(text[9].trim_end(), want[9]);
        assert_eq!(
            text[10].trim_end(),
            "  Migrate the auth service to the new token format and update all"
        );
        assert_eq!(text[11].trim_end(), "  callers");
        for (i, w) in want[..9].iter().enumerate() {
            assert_eq!(text[i].trim_end(), *w);
        }
        assert_eq!(text[12].trim_end(), "latest");
        assert_eq!(text[13].trim_end(), want[12]);
        assert_eq!(text[14].trim_end(), want[13]);
        assert_eq!(text[23].trim_end(), "Esc / D  close");

        // The stop prompt takes the connection line.
        let mut app = app_with(DEMO);
        app.pending_stop = Some(PendingStop {
            agent_id: "b1".into(),
            title: "Auth migration".into(),
        });
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(status(&text), "Stop Auth migration?  y / n");
        app.pending_stop = None;
        app.begin_rename();
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(status(&text), "Rename: Auth migration\u{258f}");
        app.pending_rename = None;
        app.set_notice("stop cancelled");
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(status(&text), "stop cancelled");
    }

    #[test]
    fn details_window_placeholders_follow_the_connection() {
        let theme = Theme::default();
        let loading = plain(&render_details_window(
            None,
            &Connection::Connecting,
            NOW,
            &theme,
            5,
            30,
        ));
        assert_eq!(loading[0].trim_end(), "\u{25cc}  loading");
        let down = plain(&render_details_window(
            None,
            &Connection::ServerUnavailable,
            NOW,
            &theme,
            5,
            40,
        ));
        assert_eq!(down[0].trim_end(), "!  noc server is not running");
        let err = plain(&render_details_window(
            None,
            &Connection::Error("boom".into()),
            NOW,
            &theme,
            5,
            30,
        ));
        assert_eq!(err[0].trim_end(), "\u{d7}  boom");
        assert!(
            render_details_window(None, &Connection::Connecting, NOW, &theme, 0, 30).is_empty()
        );
    }

    #[test]
    fn truncates_unicode_by_terminal_width() {
        assert_eq!(truncate("hello", 0), "");
        assert_eq!(truncate("hello", 5), "hello");
        assert_eq!(truncate("hello", 1), "\u{2026}");
        assert_eq!(truncate("hello world", 6), "hello\u{2026}");
        // Wide characters are never split.
        assert_eq!(
            truncate("\u{4f60}\u{597d}\u{4e16}\u{754c}", 5),
            "\u{4f60}\u{597d}\u{2026}"
        );
        assert_eq!(
            truncate("\u{4f60}\u{597d}\u{4e16}\u{754c}", 4),
            "\u{4f60}\u{2026}"
        );
        assert_eq!(pad("ab", 4), "ab  ");
        assert_eq!(pad("abcdef", 4), "abcdef");
        assert_eq!(wrap("", 10), Vec::<String>::new());
        assert_eq!(wrap("a b", 0), Vec::<String>::new());
        assert_eq!(wrap("one two three four", 9), ["one two", "three", "four"]);
        assert_eq!(
            wrap("short extraordinarily", 8),
            ["short", "extraor\u{2026}"]
        );
    }

    #[test]
    fn renders_a_friendly_server_start_instruction() {
        let mut app = app_with("[]");
        app.set_server_unavailable();
        let text = plain(&render(&mut app, &Theme::default(), 14, 42));
        let want = [
            "noc server is not running.",
            "",
            "Start it with:",
            "  noc server --bg",
            "",
            "This panel will reconnect automatically.",
        ];
        for (i, w) in want.iter().enumerate() {
            assert_eq!(text[i].trim_end(), *w, "line {i}");
        }
        assert_eq!(text[11].trim_end(), "noc  0 agents");
        assert_eq!(text[12].trim_end(), "!  server offline \u{b7} retrying");
        assert_eq!(text[13].trim_end(), FOOTER);
        // At the installed 38-column width the last line is cut with an ellipsis.
        let narrow = plain(&render(&mut app, &Theme::default(), 14, 38));
        assert_eq!(
            narrow[5].trim_end(),
            "This panel will reconnect automatical\u{2026}"
        );
        // Following the Zellij theme, the offline line is not dim and other
        // connection lines are.
        let zellij = Theme::from_mode_info(&ModeInfo::default());
        let raw = render(&mut app, &zellij, 14, 42);
        assert!(!raw[12].contains("\x1b[2;"));
        let mut app = app_with("[]");
        let raw = render(&mut app, &zellij, 14, 42);
        assert!(raw[12].contains("\x1b[2;"));
        // The built-in colours do not dim it either way.
        let raw = render(&mut app, &Theme::default(), 14, 42);
        assert!(!raw[12].contains("\x1b[2;"));
    }

    #[test]
    fn empty_and_error_states() {
        let mut app = app_with("[]");
        let text = plain(&render(&mut app, &Theme::default(), 8, 38));
        assert_eq!(text[0].trim_end(), "No live agents.");
        app.set_error("bad".into());
        let text = plain(&render(&mut app, &Theme::default(), 8, 38));
        assert_eq!(status(&text), "\u{d7}  disconnected \u{b7} bad");
        assert_eq!(text[0].trim_end(), "No registry snapshot available.");
        // A pipe panel that has heard nothing says what it waits for.
        let mut app = ZellijNoc::new(Config {
            transport: Transport::Pipe,
            ..Config::default()
        });
        let text = plain(&render(&mut app, &Theme::default(), 8, 38));
        assert_eq!(status(&text), "\u{25cc}  waiting for zellij-noc.registry");
    }

    #[test]
    fn a_newer_release_is_marked_on_the_header_and_explained_in_help() {
        let mut app = app_with("[]");
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(header(&text), "noc  0 agents");
        app.update = Some("v0.2.0".into());
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(
            text[header_at(&text)],
            format!("noc  0 agents{}\u{2191} v0.2.0", " ".repeat(17))
        );
        // Too narrow for both: the count stays, the marker goes.
        let text = plain(&render(&mut app, &Theme::default(), 30, 22));
        assert_eq!(header(&text), "noc  0 agents");
        app.show_help = true;
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(
            text[24].trim_end(),
            "\u{2191} v0.2.0    newer plugin release;"
        );
        assert_eq!(text[25].trim_end(), "            set it in the plugin URL");
        assert_eq!(text[29].trim_end(), FOOTER);
    }

    #[test]
    fn search_results_are_a_flat_list_with_the_query_on_the_second_line() {
        use crate::app::Search;
        let mut app = app_with(DEMO);
        let folded = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&folded), "noc  6 agents");
        assert!(
            !folded.iter().any(|l| l.contains("Ancient")),
            "too old to list"
        );

        // Typing: the prompt replaces the connection line; the list waits.
        app.search = Some(Search {
            text: "an".into(),
            typing: true,
        });
        let typing = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(status(&typing), "/ an\u{258f}");
        assert_eq!(header(&typing), "noc  6 agents");

        // Results: every match is a row, however old, and none is folded.
        let list = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":{DEMO}}}"#
        ))
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        app.search.as_mut().unwrap().typing = false;
        let found = plain(&render(&mut app, &Theme::default(), 60, 38));
        assert_eq!(header(&found), "noc  7 matches");
        assert_eq!(status(&found), "/ an");
        assert!(found.iter().any(|l| l.contains("Ancient")), "{found:?}");
        assert!(
            found.iter().any(|l| l.trim_end() == "Quiet  4"),
            "{found:?}"
        );
        assert!(!found.iter().any(|l| l.contains("< 24h")), "no sections");

        // One match, then none.
        let one = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":[{{"id":"a","name":"Only"}}]}}"#
        ))
        .unwrap();
        app.apply_snapshot(one, Transport::Poll);
        let text = plain(&render(&mut app, &Theme::default(), 20, 38));
        assert_eq!(header(&text), "noc  1 match");
        let none = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":[]}}"#
        ))
        .unwrap();
        app.apply_snapshot(none, Transport::Poll);
        let text = plain(&render(&mut app, &Theme::default(), 20, 38));
        assert_eq!(header(&text), "noc  0 matches");
        assert_eq!(text[0].trim_end(), "No agents match.");
        assert_eq!(text[4].trim_end(), "older:14d   idle two weeks or more");

        // Leaving the search folds the list again at the next snapshot.
        app.search = None;
        let list = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":{DEMO}}}"#
        ))
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&text), "noc  6 agents");
        assert_eq!(status(&text), "\u{25cf}  connected \u{b7} poll");
    }

    #[test]
    fn setup_problems_replace_the_connected_line_until_fixed() {
        let mut app = app_with("[]");
        app.guest_problem = Some("inner session \"noc\" is not running".into());
        let text = plain(&render(&mut app, &Theme::default(), 8, 60));
        assert_eq!(status(&text), "!  inner session \"noc\" is not running");
        // A settings problem wins, and a notice wins over both.
        app.config_problem = Some("home_theme is set but away_theme is not".into());
        let text = plain(&render(&mut app, &Theme::default(), 8, 60));
        assert_eq!(status(&text), "!  home_theme is set but away_theme is not");
        app.set_notice("renamed to x");
        let text = plain(&render(&mut app, &Theme::default(), 8, 60));
        assert_eq!(status(&text), "renamed to x");
    }

    #[test]
    fn errors_keep_the_last_good_list() {
        let mut app = app_with(DEMO);
        app.set_error("boom".into());
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(status(&text), "\u{d7}  disconnected \u{b7} boom");
        assert_eq!(text[0].trim_end(), "Blocked  1");
    }

    #[test]
    fn very_small_panes_keep_the_status_lines() {
        let mut app = app_with(DEMO);
        let theme = Theme::default();
        assert!(render(&mut app, &theme, 0, 38).is_empty());
        assert!(render(&mut app, &theme, 10, 0).is_empty());
        let one = plain(&render(&mut app, &theme, 1, 38));
        assert_eq!(one.len(), 1);
        assert!(one[0].starts_with("noc  6 agents"));
        let two = plain(&render(&mut app, &theme, 2, 38));
        assert_eq!(two.len(), 2);
        assert!(two[0].starts_with("noc  6 agents"));
        assert!(two[1].starts_with('\u{25cf}'));
        let three = plain(&render(&mut app, &theme, 3, 38));
        assert_eq!(three.len(), 3);
        // One list line, windowed to the selected row, above the status.
        assert_eq!(three[0].trim_end(), " ! Auth migration");
        assert!(three[1].starts_with("noc  6 agents"));
        assert!(three[2].starts_with('\u{25cf}'));
        // From four rows the footer has room too.
        let four = plain(&render(&mut app, &theme, 4, 38));
        assert_eq!(four[3].trim_end(), FOOTER);
        assert!(four[1].starts_with("noc  6 agents"));
        let _ = plain(&render(&mut app, &theme, 3, 38));
        assert!(!three.iter().any(|l| l.contains("[?]")));
        assert_eq!(app.last_render_rows, 3);
    }

    #[test]
    fn the_selection_stays_inside_a_scrolled_list() {
        let many: Vec<String> = (0..30)
            .map(|i| format!(r#"{{"id":"a{i:02}","name":"Agent {i:02}","display_state":"working","bucket":"working"}}"#))
            .collect();
        let mut app = app_with(&format!("[{}]", many.join(",")));
        app.selected_index = 25;
        let text = plain(&render(&mut app, &Theme::default(), 20, 38));
        assert!(text.iter().any(|l| l.contains("Agent 25")), "{text:?}");
        assert!(!text.iter().any(|l| l.contains("Agent 00")));
    }

    #[test]
    fn selected_rows_highlight_both_lines_and_group_colours_are_fixed() {
        let mut app = app_with(DEMO);
        let theme = Theme::default();
        let raw = render(&mut app, &theme, 40, 38);
        // Selected primary and secondary lines carry the selection background.
        assert!(raw[1].contains("48;2;49;50;68"));
        assert!(raw[2].contains("48;2;49;50;68"));
        assert!(!raw[4].contains("48;2;49;50;68"));
        // herdr's colours: a blue header, red, yellow, teal and grey groups.
        let at = header_at(&plain(&raw));
        assert!(raw[at].contains("38;2;137;180;250"), "header is blue");
        assert!(raw[0].contains("38;2;243;139;168"), "Blocked is red");
        assert!(raw[3].contains("38;2;249;226;175"), "Working is yellow");
        assert!(raw[6].contains("38;2;148;226;213"), "Review is teal");
        assert!(raw[9].contains("38;2;147;153;178"), "Quiet is grey");
        // The built-in colours are never dimmed: dim grey on a dark
        // background cannot be read.
        assert!(!raw
            .iter()
            .any(|l| l.contains("\x1b[2;") || l.contains(";2;38;")));
        // Following Zellij's theme puts the groups on fixed ANSI entries.
        let zellij = Theme::from_mode_info(&ModeInfo::default());
        let raw = render(&mut app, &zellij, 40, 38);
        assert!(raw[0].contains("38;5;1"), "Blocked heading is ANSI 1");
        assert!(raw[3].contains("38;5;2"), "Working heading is ANSI 2");
        assert!(raw[6].contains("38;5;6"), "Review heading is ANSI 6");
        assert!(raw[9].contains("38;5;8"), "Quiet heading is ANSI 8");
        assert_eq!(zellij.group(AgentGroup::Quiet), PaletteColor::EightBit(8));
        assert_eq!(styled_span("x", Style::default()), "x");
        let rgb = Style {
            fg: Some(PaletteColor::Rgb((1, 2, 3))),
            bg: Some(PaletteColor::EightBit(4)),
            bold: true,
            dim: true,
        };
        assert_eq!(styled_span("x", rgb), "\x1b[1;2;38;2;1;2;3;48;5;4mx\x1b[0m");
        assert!(raw[1].ends_with("\x1b[0m"));
    }

    #[test]
    fn control_characters_in_names_never_reach_the_terminal() {
        let mut app = app_with(
            r#"[{"id":"x","name":"evil\u001b[31mred","display_state":"idle","bucket":"review"}]"#,
        );
        let raw = render(&mut app, &Theme::default(), 10, 38);
        assert!(raw.iter().all(|l| !l.contains("\x1b[31m")));
        assert!(raw.iter().any(|l| l.contains("evil[31mred")));
    }
}
