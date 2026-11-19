//! Rendering of the panel and the floating details window.

use crate::app::{Connection, Focus, ListItem, Transport, ZellijNoc, SESSION_ROWS, SPINNER};
use crate::model::{age_label, Agent, AgentGroup};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};
use zellij_tile::prelude::{ModeInfo, PaletteColor};

pub const FOOTER: &str = "[?] keys  [D]etails  [d] window";
pub const DETAILS_FOOTER: &str = "Esc / d  close";

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
    /// The status dot when all is well, and when something needs a look.
    pub ok: PaletteColor,
    pub warn: PaletteColor,
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
            ok: rgb(166, 227, 161),
            warn: rgb(249, 226, 175),
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
            ok: colors.exit_code_success.base,
            warn: colors.text_unselected.emphasis_0,
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
    /// The columns left out at the start: the line is scrolled sideways.
    shift: usize,
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

    /// The line without its first `shift` columns.
    fn shifted(&self) -> Line {
        let mut skip = self.shift;
        let mut spans = Vec::new();
        for span in &self.spans {
            let mut text = String::new();
            for ch in span.text.chars() {
                let w = ch.width().unwrap_or(0);
                if skip >= w && w > 0 {
                    skip -= w;
                } else if skip > 0 && w > 0 {
                    // Half of a wide character: a blank in its place.
                    skip = 0;
                    text.push(' ');
                } else {
                    text.push(ch);
                }
            }
            spans.push(Span {
                text,
                style: span.style,
            });
        }
        Line {
            spans,
            shift: 0,
            ..self.clone()
        }
    }

    fn fitted(&self, cols: usize) -> Vec<Span> {
        if self.shift > 0 {
            return self.shifted().fitted(cols);
        }
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
            ListItem::Agent(i) => usize::from(
                !app.by_team() && (app.flat || app.agents[*i].group() != AgentGroup::Quiet),
            ),
            ListItem::Section { count, .. } | ListItem::Team { count, .. } => *count,
            ListItem::TeamQuiet { .. } | ListItem::QuietTeams { .. } => 0,
        })
        .sum()
}

fn item_group(app: &ZellijNoc, item: &ListItem) -> AgentGroup {
    match item {
        ListItem::Agent(i) => app.agents[*i].group(),
        ListItem::Section { .. }
        | ListItem::Team { .. }
        | ListItem::TeamQuiet { .. }
        | ListItem::QuietTeams { .. } => AgentGroup::Quiet,
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
            ListItem::Team { .. } | ListItem::TeamQuiet { .. } | ListItem::QuietTeams { .. } => 0,
        })
        .sum()
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

/// How things stand, for the colour of the status dot.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Health {
    Ok,
    Warn,
    Bad,
}

impl Health {
    fn color(self, theme: &Theme) -> PaletteColor {
        match self {
            Health::Ok => theme.ok,
            Health::Warn => theme.warn,
            Health::Bad => theme.error,
        }
    }
}

/// The connection to the registry, in words, and how good it is.
fn connection_state(app: &ZellijNoc) -> (Health, String) {
    match &app.connection {
        Connection::Connecting => {
            let text = if app.config.transport == Transport::Pipe {
                format!("waiting for {}", app.config.pipe_name)
            } else {
                "connecting".to_string()
            };
            (Health::Warn, text)
        }
        Connection::Connected(source) => {
            (Health::Ok, format!("connected \u{b7} {}", source.as_str()))
        }
        Connection::ServerUnavailable => {
            (Health::Bad, "server offline \u{b7} retrying".to_string())
        }
        Connection::Error(e) => (Health::Bad, format!("disconnected \u{b7} {e}")),
    }
}

/// What is wrong besides the connection: a setting, or the inner session.
fn problems(app: &ZellijNoc) -> Vec<&String> {
    app.config_problem
        .iter()
        .chain(app.guest_problem.iter())
        .collect()
}

/// The worst of the connection and the problems.
fn health(app: &ZellijNoc) -> Health {
    let (connection, _) = connection_state(app);
    if problems(app).is_empty() {
        connection
    } else {
        connection.max(Health::Warn)
    }
}

/// The one status line. A prompt or a notice takes the whole line while it
/// lasts. Otherwise: how many agents on the left, and on the right an `↑`
/// when a newer release exists and a dot coloured by `health`. What the dot
/// and the arrow stand for is in the status box (`status_box_lines`).
fn status_line(app: &ZellijNoc, theme: &Theme, cols: usize) -> Line {
    if let Some(rename) = &app.pending_rename {
        return Line::plain(
            &format!("Rename: {}\u{258f}", rename.text),
            Style::fg(theme.accent),
        );
    }
    if let Some(team) = &app.pending_team {
        return Line::plain(
            &format!("team: {}\u{258f}", team.text),
            Style::fg(theme.accent),
        );
    }
    if let Some(stop) = &app.pending_stop {
        return Line::plain(
            &format!("Stop {}?  y / n", stop.title),
            Style::fg(theme.attention),
        );
    }
    if let Some(name) = &app.pending_end {
        return Line::plain(
            &format!("End session {}?  y / n", app.session_label(name)),
            Style::fg(theme.attention),
        );
    }
    if let Some(search) = app.search.as_ref().filter(|s| s.typing) {
        return Line::plain(
            &format!("/ {}\u{258f}", search.text),
            Style::fg(theme.accent),
        );
    }
    if let Some(notice) = &app.notice {
        return Line::plain(notice, Style::fg(theme.info));
    }
    let count = shown_agent_count(app);
    let mut left = if app.flat {
        format!("{count} match{}", if count == 1 { "" } else { "es" })
    } else {
        format!("{count} agent{}", plural(count))
    };
    if let Some(query) = app.search_text() {
        left.push_str(&format!("  / {query}"));
    }
    let arrow = if app.update.is_some() {
        "\u{2191} "
    } else {
        ""
    };
    let right = arrow.width() + 1;
    // Keep the dot: the count is cut before the dot is dropped.
    let left = truncate(&left, cols.saturating_sub(right + 1));
    let gap = cols.saturating_sub(left.width() + right);
    let mut line = Line::plain(&left, Style::fg(theme.accent).bold());
    if cols < right + 1 {
        return line;
    }
    line = line.with(&" ".repeat(gap), Style::default());
    if !arrow.is_empty() {
        line = line.with(arrow, Style::fg(theme.attention).bold());
    }
    line.with("\u{25cf}", Style::fg(health(app).color(theme)))
}

/// The box that explains the status line: the connection in words, anything
/// else that is wrong, and the plugin's version with the newer release when
/// there is one. Drawn over the last lines of the list, above the status
/// line.
fn status_box_lines(app: &ZellijNoc, theme: &Theme, cols: usize) -> Vec<Line> {
    if cols < 8 {
        return Vec::new();
    }
    let inner = cols - 4;
    let frame = theme.muted_style();
    let muted = theme.muted_style();
    let mut content: Vec<(String, Style)> = Vec::new();
    let (state, text) = connection_state(app);
    content.push((format!("\u{25cf} {text}"), Style::fg(state.color(theme))));
    for problem in problems(app) {
        for (i, part) in wrap(problem, inner.saturating_sub(2))
            .into_iter()
            .enumerate()
        {
            let lead = if i == 0 { "! " } else { "  " };
            content.push((format!("{lead}{part}"), Style::fg(theme.warn)));
        }
    }
    let current = crate::update::CURRENT;
    match &app.update {
        Some(version) => {
            content.push((
                format!("\u{2191} {version} is available"),
                Style::fg(theme.attention).bold(),
            ));
            content.push((format!("  this is v{current}"), muted));
            for part in wrap(
                "set the new version in the plugin URL",
                inner.saturating_sub(2),
            ) {
                content.push((format!("  {part}"), muted));
            }
        }
        None => content.push((format!("version v{current}"), muted)),
    }
    let title = " status ";
    let mut lines = vec![Line::plain(
        &format!(
            "\u{256d}\u{2500}{title}{}\u{256e}",
            "\u{2500}".repeat(cols.saturating_sub(title.width() + 3))
        ),
        frame,
    )];
    for (text, style) in content {
        let text = truncate(&text, inner);
        let fill = " ".repeat(inner - text.width());
        lines.push(
            Line::plain("\u{2502} ", frame)
                .with(&text, style)
                .with(&fill, Style::default())
                .with(" \u{2502}", frame),
        );
    }
    lines.push(Line::plain(
        &format!("\u{2570}{}\u{256f}", "\u{2500}".repeat(cols - 2)),
        frame,
    ));
    lines
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
        "h           close a section or a team",
        "v           group by team / status",
        "p / s       pane / stacked pane here",
        "w           shell in its worktree",
        "o           show its team's session",
        "Tab         agents / sessions",
        "Shift-Tab   into the details, and out",
        "S           close / open sessions",
        "D           close / open details",
        "d           details window",
        "n           rename agent",
        "t / T       join / leave a team",
        "x           stop agent (asks y/n)",
        "r           refresh",
        "/           search (also older:14d)",
        "u           check for a new release",
        "i           status and version",
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

/// Quiet agents sit two columns in, under the row that holds them. A team's
/// lead is listed first in its group whatever its state, so it does not.
fn row_indent(app: &ZellijNoc, agent: &Agent) -> &'static str {
    if agent.group() == AgentGroup::Quiet && !(app.by_team() && agent.is_lead()) {
        "  "
    } else {
        ""
    }
}

/// The columns of an agent row's age (`100d` is the widest), and the fewest
/// columns a title keeps beside it.
const AGE_WIDTH: usize = 4;
const AGE_MIN_TITLE: usize = 8;

fn agent_primary_line(
    app: &ZellijNoc,
    theme: &Theme,
    agent: &Agent,
    selected: bool,
    cols: usize,
) -> Line {
    let group = agent.group();
    let indent = row_indent(app, agent);
    let prefix = format!(" {indent}{} ", agent_symbol(app, agent));
    // The age sits in a column of its own at the right edge, so it is in
    // the same place on every row whatever the title. A pane too narrow to
    // leave the title a few columns has no age.
    let room = cols.saturating_sub(prefix.width());
    let age = age_label(agent.recency(), &app.generated_at);
    let age = if age.is_empty() || room < AGE_WIDTH + 1 + AGE_MIN_TITLE {
        String::new()
    } else {
        format!(" {age:>AGE_WIDTH$}")
    };
    let title_width = room - age.width().min(room);
    let mut title = truncate(&agent.title(), title_width);
    if !age.is_empty() {
        title = pad(&title, title_width);
    }
    let mut line = if selected {
        let style = Style::selected(theme);
        Line::plain(&prefix, style.bold())
            .with(&title, style.bold())
            .with(&age, style)
            .filled(Style::fill(theme))
    } else {
        let title_color = if group == AgentGroup::Quiet {
            theme.group(group)
        } else {
            theme.text
        };
        Line::plain(&prefix, Style::fg(theme.group(group)).bold())
            .with(&title, Style::fg(title_color))
            .with(&age, theme.muted_style())
    };
    line.trailing_reset = true;
    line
}

/// The row that holds the teams with no agent at work: `Quiet teams  N`,
/// at the left edge like a team heading, in the Quiet colour.
fn quiet_teams_line(theme: &Theme, count: usize, expanded: bool, selected: bool) -> Line {
    let arrow = if expanded { "\u{25be}" } else { "\u{25b8}" };
    let text = format!("{arrow} {QUIET_TEAMS}  {count}");
    if selected {
        Line::plain(&text, Style::selected(theme).bold()).filled(Style::fill(theme))
    } else {
        Line::plain(&text, Style::fg(theme.group(AgentGroup::Quiet)).bold())
    }
}

pub const QUIET_TEAMS: &str = "Quiet teams";

fn section_line(theme: &Theme, label: &str, count: usize, expanded: bool, selected: bool) -> Line {
    let arrow = if expanded { "\u{25be}" } else { "\u{25b8}" };
    let text = format!(" {arrow} {}  {count}", pad(label, 6));
    if selected {
        Line::plain(&text, Style::selected(theme).bold()).filled(Style::fill(theme))
    } else {
        Line::plain(&text, Style::fg(theme.group(AgentGroup::Quiet)))
    }
}

/// The counts a folded team shows after its heading: `1 blocked · 2 working
/// · 1 review`, zero counts left out.
fn attention_counts(blocked: usize, working: usize, review: usize) -> String {
    [
        (blocked, "blocked"),
        (working, "working"),
        (review, "review"),
    ]
    .iter()
    .filter(|(count, _)| *count > 0)
    .map(|(count, label)| format!("{count} {label}"))
    .collect::<Vec<_>>()
    .join(" \u{b7} ")
}

/// A team's heading, which is a row of the list: `{team}  {count}`, and
/// for a folded team what in it needs attention.
fn team_line(theme: &Theme, title: &str, count: usize, counts: &str, selected: bool) -> Line {
    let heading = format!("{title}  {count}");
    let tail = if counts.is_empty() {
        String::new()
    } else {
        format!("  {counts}")
    };
    if selected {
        Line::plain(&heading, Style::selected(theme).bold())
            .with(&tail, Style::selected(theme))
            .filled(Style::fill(theme))
    } else {
        Line::plain(&heading, Style::fg(theme.accent).bold()).with(&tail, theme.muted_style())
    }
}

/// Returns the list lines and the index of the first selected line.
fn agent_list_lines(app: &ZellijNoc, theme: &Theme, cols: usize) -> (Vec<Line>, usize) {
    let mut lines = Vec::new();
    let mut selected_line = None;
    let mut current_group: Option<AgentGroup> = None;
    let status_headings = !app.by_team();
    for (index, item) in app.items.iter().enumerate() {
        let group = item_group(app, item);
        if status_headings && current_group != Some(group) {
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
        // Highlighted only while the agents have the keys.
        let selected = selected && app.list_focus() == Focus::Agents;
        match item {
            ListItem::Agent(i) => {
                let agent = &app.agents[*i];
                lines.push(agent_primary_line(app, theme, agent, selected, cols));
            }
            ListItem::Section {
                age,
                count,
                expanded,
            } => lines.push(section_line(
                theme,
                age.label(),
                *count,
                *expanded,
                selected,
            )),
            ListItem::Team {
                team,
                count,
                expanded,
                blocked,
                working,
                review,
            } => {
                let counts = if *expanded {
                    String::new()
                } else {
                    attention_counts(*blocked, *working, *review)
                };
                lines.push(team_line(
                    theme,
                    app.team_title(*team),
                    *count,
                    &counts,
                    selected,
                ));
            }
            ListItem::TeamQuiet {
                count, expanded, ..
            } => lines.push(section_line(
                theme,
                AgentGroup::Quiet.title(),
                *count,
                *expanded,
                selected,
            )),
            ListItem::QuietTeams { count, expanded } => {
                lines.push(quiet_teams_line(theme, *count, *expanded, selected))
            }
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
    // Where the work is, then what it is, then which program does it.
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
    let team = agent.team_name();
    if !team.is_empty() {
        let lead = if agent.is_lead() { " \u{b7} lead" } else { "" };
        lines.push(label_line("team", &format!("{team}{lead}"), theme));
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

/// The titles of the panel's three sections.
pub const AGENTS_TITLE: &str = "Agents";
pub const DETAILS_TITLE: &str = "Details";
pub const SESSIONS_TITLE: &str = "Sessions";

/// A section's title row: the title, then a rule to the edge of the pane. A
/// section that closes has an arrow before it, down while it is open. The
/// section that has the keys is in the accent colour, the others muted.
fn section_heading(
    theme: &Theme,
    open: Option<bool>,
    text: &str,
    focused: bool,
    cols: usize,
) -> Line {
    let arrow = match open {
        Some(true) => "\u{25be} ",
        Some(false) => "\u{25b8} ",
        None => "",
    };
    let title = truncate(&format!("{arrow}{text}"), cols);
    let style = if focused {
        Style::fg(theme.accent).bold()
    } else {
        theme.muted_style().bold()
    };
    let rest = cols.saturating_sub(title.width() + 1);
    let mut rule = Style::fg(theme.accent);
    rule.dim = theme.dim;
    Line::plain(&title, style).with(&format!(" {}", "\u{2500}".repeat(rest)), rule)
}

/// What a session row says after the session's name: what its agents need,
/// `quiet` when none needs anything, `no agents` when it has none.
fn session_summary(app: &ZellijNoc, name: &str) -> String {
    match app.session_counts(name) {
        None => String::new(),
        Some(counts) if counts.agents == 0 => "no agents".to_string(),
        Some(counts) => match attention_counts(counts.blocked, counts.working, counts.review) {
            text if text.is_empty() => "quiet".to_string(),
            text => text,
        },
    }
}

/// The widest a session's name is drawn in its row.
const SESSION_LABEL_WIDTH: usize = 16;

/// The Sessions section: a rule, the title with the number of sessions, and
/// a row for each running inner session, the one on show marked `●`. At most
/// `SESSION_ROWS` rows, around the selected one.
fn session_lines(app: &ZellijNoc, theme: &Theme, cols: usize) -> Vec<Line> {
    let focused = app.list_focus() == Focus::Sessions;
    // Open, the count is the section's last line, as the agents' count is
    // the list's; closed, the title carries it.
    let count = app.sessions.len();
    let title = if app.show_sessions {
        SESSIONS_TITLE.to_string()
    } else {
        format!("{SESSIONS_TITLE}  {count}")
    };
    let mut lines = vec![section_heading(
        theme,
        Some(app.show_sessions),
        &title,
        app.focus == Focus::Sessions,
        cols,
    )];
    if !app.show_sessions {
        return lines;
    }
    let width = app
        .sessions
        .iter()
        .map(|row| app.session_label(&row.name).width())
        .max()
        .unwrap_or(0)
        .min(SESSION_LABEL_WIDTH);
    let start = app
        .session_index
        .saturating_sub(SESSION_ROWS / 2)
        .min(app.sessions.len().saturating_sub(SESSION_ROWS));
    for (index, row) in app
        .sessions
        .iter()
        .enumerate()
        .skip(start)
        .take(SESSION_ROWS)
    {
        let mark = if row.on_show { " \u{25cf} " } else { "   " };
        let label = pad(&truncate(app.session_label(&row.name), width), width);
        let summary = session_summary(app, &row.name);
        let tail = if summary.is_empty() {
            String::new()
        } else {
            format!("  {summary}")
        };
        lines.push(if focused && index == app.session_index {
            let style = Style::selected(theme);
            Line::plain(mark, style.bold())
                .with(&label, style.bold())
                .with(&tail, style)
                .filled(Style::fill(theme))
        } else {
            Line::plain(mark, Style::fg(theme.ok).bold())
                .with(&label, Style::fg(theme.text))
                .with(&tail, theme.muted_style())
        });
    }
    lines.push(Line::plain(
        &format!("{count} session{}", plural(count)),
        Style::fg(theme.accent).bold(),
    ));
    lines
}

/// The details of a session, shown while the Sessions section has the keys.
fn session_detail_lines(app: &ZellijNoc, theme: &Theme, name: &str, on_show: bool) -> Vec<Line> {
    let muted = theme.muted_style();
    let mut lines = vec![Line::plain(
        &format!("Session \u{b7} {}", app.session_label(name)),
        Style::fg(theme.text).bold(),
    )];
    lines.push(label_line("zellij", name, theme));
    lines.push(label_line(
        "state",
        if on_show {
            "on show"
        } else {
            "running, out of sight"
        },
        theme,
    ));
    if let Some(counts) = app.session_counts(name) {
        let needs = attention_counts(counts.blocked, counts.working, counts.review);
        lines.push(label_line("agents", &counts.agents.to_string(), theme));
        if !needs.is_empty() {
            // On a line of its own: three counts do not fit beside a label.
            lines.push(Line::plain(&format!("  {needs}"), muted));
        }
    }
    let hint = if name == app.config.guest_session {
        "Enter shows it."
    } else {
        "Enter shows it; x ends it."
    };
    lines.push(Line::plain(hint, muted));
    lines
}

/// The most wrapped lines of an agent's request or latest message in the
/// Details section, which scrolls.
const DETAIL_PREVIEW_LINES: usize = 12;

/// What the Details section is about, to tell when it changes: the
/// session, or the agent, or the row of the list.
fn detail_subject(app: &ZellijNoc) -> String {
    if let Some(row) = app.focused_session() {
        return format!("session {}", row.name);
    }
    match app.selected_agent() {
        Some(agent) => format!("agent {}", agent.id),
        None => format!("row {}", app.selected_index),
    }
}

/// Everything the Details section has to say, under its title. The section
/// shows as many of these lines as it has rows, from where it is scrolled.
fn inline_detail_lines(app: &ZellijNoc, theme: &Theme, cols: usize) -> Vec<Line> {
    let mut lines = Vec::new();
    if let Some(row) = app.focused_session() {
        lines.extend(session_detail_lines(app, theme, &row.name, row.on_show));
        return lines;
    }
    let detail_height = DETAIL_PREVIEW_LINES;
    match app.selected_item() {
        Some(ListItem::Agent(i)) => {
            let preview = detail_height;
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
        Some(ListItem::Team { team, count, .. }) => {
            let title = match app.team_name(team) {
                "" => "No team".to_string(),
                name => format!("Team \u{b7} {name}"),
            };
            lines.push(Line::plain(&title, Style::fg(theme.accent).bold()));
            let muted = theme.muted_style();
            for text in [
                format!("{count} agent{}.", plural(count)).as_str(),
                "Enter folds or opens this team;",
                "v groups by status.",
            ] {
                lines.push(Line::plain(text, muted));
            }
        }
        Some(ListItem::QuietTeams { count, .. }) => {
            lines.push(Line::plain(QUIET_TEAMS, Style::fg(theme.accent).bold()));
            let muted = theme.muted_style();
            for text in [
                format!("{count} team{} with no agent at work.", plural(count)).as_str(),
                "Enter opens this row; a team",
                "leaves it when an agent wakes.",
            ] {
                lines.push(Line::plain(text, muted));
            }
        }
        Some(ListItem::TeamQuiet { team, .. }) => {
            lines.push(Line::plain(
                &format!("Quiet \u{b7} {}", app.team_title(team)),
                Style::fg(theme.accent).bold(),
            ));
            let muted = theme.muted_style();
            for text in [
                "Conversations with no process.",
                "Enter opens this row; Enter on",
                "an agent resumes it; h closes.",
            ] {
                lines.push(Line::plain(text, muted));
            }
        }
        None => {}
    }
    lines
}

/// The Details section in `height` rows: its title row, and while it is
/// open the details from where they are scrolled, down and sideways. How
/// far they can be scrolled is kept for the keys, and a new subject starts
/// at the top.
fn detail_section(app: &mut ZellijNoc, theme: &Theme, cols: usize, height: usize) -> Vec<Line> {
    let focused = app.focus == Focus::Details;
    if !app.show_details || height < 2 {
        return vec![section_heading(
            theme,
            Some(false),
            DETAILS_TITLE,
            focused,
            cols,
        )];
    }
    let subject = detail_subject(app);
    if subject != app.detail_of {
        app.detail_of = subject;
        app.detail_scroll = 0;
        app.detail_shift = 0;
    }
    let content = inline_detail_lines(app, theme, cols);
    let rows = height - 1;
    app.detail_rows = rows;
    app.detail_max_scroll = content.len().saturating_sub(rows);
    app.detail_scroll = app.detail_scroll.min(app.detail_max_scroll);
    let widest = content.iter().map(Line::width).max().unwrap_or(0);
    app.detail_max_shift = widest.saturating_sub(cols);
    app.detail_shift = app.detail_shift.min(app.detail_max_shift);
    // The title says which lines are in view when they are not all.
    let title = if app.detail_max_scroll > 0 {
        format!(
            "{DETAILS_TITLE}  {}-{}/{}",
            app.detail_scroll + 1,
            app.detail_scroll + rows,
            content.len()
        )
    } else {
        DETAILS_TITLE.to_string()
    };
    let mut lines = vec![section_heading(theme, Some(true), &title, focused, cols)];
    lines.extend(
        content
            .into_iter()
            .skip(app.detail_scroll)
            .take(rows)
            .map(|mut line| {
                line.shift = app.detail_shift;
                line
            }),
    );
    lines.resize(height, Line::empty());
    lines
}

// ---- entry points ----

/// The panel: the list (or help, or the empty state) from the top, and at
/// the bottom the status line, the inline details when they are shown, and
/// the footer. The status line is the last thing to give way.
pub fn render(app: &mut ZellijNoc, theme: &Theme, rows: usize, cols: usize) -> Vec<String> {
    app.last_render_rows = rows;
    app.status_row = None;
    if rows == 0 || cols == 0 {
        return Vec::new();
    }
    let status = status_line(app, theme, cols);
    let footer = rows > 2;
    let body_rows = rows - 1 - usize::from(footer);
    // The list is a section with a title; help is not.
    let titled = !app.show_help && body_rows >= 3;
    let inner_rows = body_rows - usize::from(titled);
    let (mut body, mut details) = if app.show_help {
        let mut lines = help_lines(theme);
        lines.extend(version_help_lines(app.update.as_deref(), theme));
        (lines, Vec::new())
    } else if app.items.is_empty() {
        (empty_state_lines(app, theme), Vec::new())
    } else {
        // Below the list: the sessions, then the details. The sessions take
        // their rows first, being few and of a known number; closed, each
        // section is its title row.
        let sessions = if !app.sessions.is_empty() && inner_rows >= 14 {
            session_lines(app, theme, cols)
        } else {
            Vec::new()
        };
        let shared = inner_rows - sessions.len();
        let detail_height = match (shared >= 12, app.show_details) {
            (true, true) => shared / 3,
            (true, false) => 1,
            (false, _) => 0,
        };
        let list_height = shared - detail_height;
        let (list, selected_line) = agent_list_lines(app, theme, cols);
        let start = selected_line
            .saturating_sub(list_height / 2)
            .min(list.len().saturating_sub(list_height));
        let window: Vec<Line> = list.into_iter().skip(start).take(list_height).collect();
        let mut below = sessions;
        if detail_height > 0 {
            below.extend(detail_section(app, theme, cols, detail_height));
        }
        (window, below)
    };
    if titled {
        body.insert(
            0,
            section_heading(theme, None, AGENTS_TITLE, app.focus == Focus::Agents, cols),
        );
    }
    body.resize(body_rows - details.len(), Line::empty());
    if app.show_info {
        // The box covers the end of the body, directly above the status line.
        let mut box_lines = status_box_lines(app, theme, cols);
        let keep = box_lines.len().min(body.len());
        let from = body.len() - keep;
        let skip = box_lines.len() - keep;
        for (slot, line) in body[from..].iter_mut().zip(box_lines.drain(skip..)) {
            *slot = line;
        }
    }
    app.status_row = Some(body.len());
    let mut lines = body;
    lines.push(status);
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

    /// The panel below the title of its first section: the tests of the
    /// list, the status line and the details count rows from the first row
    /// of the list. `panel` is the whole of it.
    fn render(app: &mut ZellijNoc, theme: &Theme, rows: usize, cols: usize) -> Vec<String> {
        let mut lines = panel(app, theme, rows + 1, cols);
        if lines
            .first()
            .is_some_and(|l| strip(l).starts_with(AGENTS_TITLE))
        {
            lines.remove(0);
            app.last_render_rows = rows;
            app.status_row = app.status_row.map(|row| row - 1);
            lines
        } else {
            panel(app, theme, rows, cols)
        }
    }

    fn panel(app: &mut ZellijNoc, theme: &Theme, rows: usize, cols: usize) -> Vec<String> {
        super::render(app, theme, rows, cols)
    }

    fn plain(lines: &[String]) -> Vec<String> {
        lines.iter().map(|l| strip(l)).collect()
    }

    /// Where the status line is: above the title of the first section under
    /// the list when one is drawn, and above the footer.
    fn status_at(text: &[String]) -> usize {
        let rule = text
            .iter()
            .position(|l| l.starts_with('\u{25be}') || l.starts_with('\u{25b8}'));
        match rule {
            Some(rule) => rule - 1,
            None if text.last().is_some_and(|l| l.trim_end() == FOOTER) => text.len() - 2,
            None => text.len() - 1,
        }
    }

    /// The status line as drawn: a count with its marks, or a prompt.
    fn status(text: &[String]) -> &str {
        text[status_at(text)].trim_end()
    }

    /// The count on the status line, without the arrow and the dot.
    fn header(text: &[String]) -> &str {
        status(text)
            .trim_end_matches('\u{25cf}')
            .trim_end()
            .trim_end_matches('\u{2191}')
            .trim_end()
    }

    /// A row of the list without the age at its right edge.
    fn row(line: &str) -> &str {
        let line = line.trim_end();
        match line.rsplit_once(' ') {
            Some((rest, age))
                if age.len() >= 2
                    && age.ends_with(['s', 'm', 'h', 'd'])
                    && age[..age.len() - 1].chars().all(|c| c.is_ascii_digit()) =>
            {
                rest.trim_end()
            }
            _ => line,
        }
    }

    /// The lines inside the status box, opened for the occasion.
    fn info(app: &mut ZellijNoc, rows: usize, cols: usize) -> Vec<String> {
        app.show_info = true;
        let text = plain(&render(app, &Theme::default(), rows, cols));
        app.show_info = false;
        text.iter()
            .filter_map(|l| l.strip_prefix("\u{2502} "))
            .map(|l| l.trim_end_matches('\u{2502}').trim_end().to_string())
            .collect()
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
            "Working  1",
            " \u{280b} Cache refactor",
            "Review  1",
            " \u{25c6} Docs pass",
            "Quiet  3",
            " \u{25b8} < 24h   1",
            " \u{25b8} < 48h   1",
            " \u{25b8} < 7d    1",
        ];
        for (i, want) in expect.iter().enumerate() {
            assert_eq!(row(&text[i]), *want, "line {i}");
        }
        // The age is a column at the right edge, whatever the title.
        assert_eq!(text[1], format!(" ! Auth migration{}2m", " ".repeat(19)));
        assert_eq!(
            text[3],
            format!(" \u{280b} Cache refactor{}0m", " ".repeat(19))
        );
        assert_eq!(text[5], format!(" \u{25c6} Docs pass{}3h", " ".repeat(24)));
        let rest = text[10..26].iter().all(|l| l.trim().is_empty());
        assert!(rest, "list area is padded");
        // The status line sits between the list and the details: the count
        // on the left, the dot at the right edge.
        assert_eq!(text[26], format!("6 agents{}\u{25cf}", " ".repeat(29)));
        let details = &text[27..];
        let wanted = [
            format!("\u{25be} Details  1-11/15 {}", "\u{2500}".repeat(19)),
            "Auth migration".to_string(),
            "!  needs approval \u{b7} 2m ago".to_string(),
            "repo      app \u{b7} main".to_string(),
            "cwd       /code/app/svc".to_string(),
            "ask".to_string(),
            "  Migrate the auth service to the new".to_string(),
            "  token format and update all callers".to_string(),
            "latest".to_string(),
            "  Waiting for approval to delete the".to_string(),
            "  build directory.".to_string(),
            "provider  claude \u{b7} background".to_string(),
            FOOTER.to_string(),
        ];
        let got: Vec<&str> = details.iter().map(|l| l.trim_end()).collect();
        assert_eq!(got, wanted);
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
        // available = 28, detail height 9: the list takes 19 lines and the
        // status line follows, so the rule sits on line 20.
        assert_eq!(
            text[20],
            format!("\u{25be} Details {}", "\u{2500}".repeat(28))
        );
        assert_eq!(text[21].trim_end(), "Docs pass");
        assert_eq!(text[22].trim_end(), "\u{25c6}  review \u{b7} 3h ago");
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
        let text = plain(&render(&mut app, &Theme::default(), 13, 38));
        assert!(!text.iter().any(|l| l.starts_with("Details")));
    }

    #[test]
    fn working_agents_spin_and_quiet_agents_fold_into_counted_sections() {
        let mut app = app_with(DEMO);
        let first = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&first), "6 agents");
        assert!(first[3].starts_with(" \u{280b} Cache refactor"));
        app.advance_spinner();
        let second = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert!(second[3].starts_with(" \u{2819} Cache refactor"));
        assert_eq!(first[6].trim_end(), "Quiet  3");
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
        assert_eq!(text[6].trim_end(), "Quiet  3");
        assert_eq!(text[7].trim_end(), " \u{25be} < 24h   1");
        assert_eq!(row(&text[8]), "   \u{25cc} Old question");
        assert_eq!(header(&text), "6 agents");
    }

    const TEAMS: &str = r#"[
      {"id":"solo","provider":"claude","name":"Solo","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:59:00Z"},
      {"id":"w1","provider":"claude","name":"Worker one","team":"billing","display_state":"working","bucket":"working","last_activity_at":"2026-09-24T11:58:00Z","workspace":{"path":"/code/app","branch":"main"}},
      {"id":"b1","provider":"codex","name":"Blocked one","team":"billing","display_state":"needs_input","bucket":"blocked","last_activity_at":"2026-09-24T11:57:00Z"},
      {"id":"lead","provider":"claude","name":"Lead","team":"billing","lead":true,"display_state":"ready","bucket":"review","last_activity_at":"2026-09-24T11:56:00Z","workspace":{"path":"/code/app","branch":"main"}},
      {"id":"q1","provider":"claude","name":"Quiet one","team":"billing","display_state":"idle","bucket":"quiet","ended_at":"2026-09-24T07:00:00Z"},
      {"id":"a1","provider":"claude","name":"Auth one","team":"a-very-long-team-name","display_state":"ready","bucket":"review","last_activity_at":"2026-09-24T11:55:00Z"}]"#;

    #[test]
    fn team_grouping_shows_headings_the_lead_and_folded_counts() {
        let mut app = app_with(TEAMS);
        app.toggle_group_by();
        app.notice = None;
        let raw = render(&mut app, &Theme::default(), 40, 38);
        let text = plain(&raw);
        let rows: Vec<&str> = text.iter().take(10).map(|l| row(l)).collect();
        assert_eq!(
            rows,
            [
                "a-very-long-team-name  1",
                " \u{25c6} Auth one",
                "billing  4",
                " \u{25c6} Lead",
                " ? Blocked one",
                " \u{280b} Worker one",
                " \u{25b8} Quiet   1",
                "no team  1",
                " \u{280b} Solo",
                "",
            ]
        );
        assert_eq!(header(&text), "6 agents");
        // Headings are in the accent colour, bold; the first one is selected.
        let accent = color_code(Theme::default().accent, false);
        assert!(raw[2].starts_with(&format!("\u{1b}[1;{accent}mbilling  4")));
        assert!(raw[7].starts_with(&format!("\u{1b}[1;{accent}mno team  1")));
        // A heading's details name the team.
        app.selected_index = 2;
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert!(text.iter().any(|l| l.trim_end() == "Team \u{b7} billing"));
        assert!(text.iter().any(|l| l.trim_end() == "4 agents."));
        // Folded, the heading says what in the team needs attention.
        assert!(app.toggle_selected_section());
        let text = plain(&render(&mut app, &Theme::default(), 40, 60));
        assert_eq!(
            text[2].trim_end(),
            "billing  4  1 blocked \u{b7} 1 working \u{b7} 1 review"
        );
        assert_eq!(text[3].trim_end(), "no team  1");
        // Too long for the pane, it is cut, not wrapped.
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(
            text[2],
            "billing  4  1 blocked \u{b7} 1 working \u{b7} 1\u{2026}"
        );
        assert_eq!(header(&text), "6 agents");
        app.selected_index = 3;
        assert!(app.toggle_selected_section());
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(text[3].trim_end(), "no team  1  1 working");
        // The Quiet row opens like a Quiet section, its agents indented.
        app.selected_index = 2;
        assert!(app.toggle_selected_section());
        assert!(app.select_agent("q1"));
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(text[6].trim_end(), " \u{25be} Quiet   1");
        assert_eq!(row(&text[7]), "   \u{25cb} Quiet one");
        assert_eq!(attention_counts(0, 0, 0), "");
        assert_eq!(attention_counts(2, 0, 1), "2 blocked \u{b7} 1 review");
    }

    #[test]
    fn an_agent_is_one_row_and_its_details_name_the_team() {
        let mut app = app_with(TEAMS);
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        // One row an agent: the next row is the next agent, or a heading.
        let after = |title: &str| -> String {
            let at = text.iter().position(|l| row(l).ends_with(title)).unwrap();
            text[at + 1].trim_end().to_string()
        };
        assert_eq!(row(&after("Blocked one")), "Working  2");
        assert_eq!(row(&after("Solo")), " \u{280b} Worker one");
        let mut details = |id: &str| -> Vec<String> {
            assert!(app.select_agent(id));
            plain(&render(&mut app, &Theme::default(), 40, 38))
                .iter()
                .map(|l| l.trim_end().to_string())
                .collect()
        };
        assert!(details("w1").contains(&"team      billing".to_string()));
        assert!(details("lead").contains(&"team      billing \u{b7} lead".to_string()));
        assert!(!details("solo").iter().any(|l| l.starts_with("team")));
        let narrow = render(&mut app, &Theme::default(), 40, 16);
        assert!(plain(&narrow).iter().all(|l| l.width() <= 16));
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
        assert_eq!(row(&text[pending]), "   \u{25cc} Pending");
        let idle = text.iter().position(|l| l.contains("Idle")).unwrap();
        assert_eq!(row(&text[idle]), "   \u{25cb} Idle");
        // Details use the state symbol, not the resting one.
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "?  needs input \u{b7} 5h ago"));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "provider  claude \u{b7} ended"));
    }

    fn sessions(app: &mut ZellijNoc, names: &[&str], on_show: &str) {
        let names: Vec<String> = names.iter().map(|n| n.to_string()).collect();
        app.set_sessions(&names, on_show);
    }

    #[test]
    fn the_panel_is_three_titled_sections() {
        let mut app = app_with(TEAMS);
        let rule = |title: &str| format!("{title} {}", "\u{2500}".repeat(37 - title.width()));
        // Agents, the status line, Sessions, Details, the footer.
        sessions(
            &mut app,
            &["noc", "noc--billing", "noc--auth"],
            "noc--billing",
        );
        let text = plain(&panel(&mut app, &Theme::default(), 40, 38));
        assert_eq!(text.len(), 40);
        assert!(text.iter().all(|l| l.width() == 38), "{text:?}");
        let at =
            |text: &[String], title: &str| text.iter().position(|l| l.contains(title)).unwrap();
        assert_eq!(text[0], rule("Agents"));
        assert_eq!(text[1].trim_end(), "Blocked  1");
        let (list, details) = (at(&text, "Sessions"), at(&text, "Details"));
        assert!(text[list - 1].starts_with("6 agents"), "the status line");
        assert_eq!(text[list], rule("\u{25be} Sessions"));
        // The teams' sessions by name, then the inner session; the one on
        // show is marked, and each says what its team's agents need.
        assert_eq!(text[list + 1].trim_end(), "   auth     no agents");
        assert_eq!(
            text[list + 2].trim_end(),
            " \u{25cf} billing  1 blocked \u{b7} 1 working \u{b7} 1\u{2026}"
        );
        assert_eq!(text[list + 3].trim_end(), "   no-team  1 working");
        // The section ends with its count, as the list does.
        assert_eq!(text[list + 4].trim_end(), "3 sessions");
        assert_eq!(details, list + 5);
        assert_eq!(text[details], rule("\u{25be} Details"));
        assert_eq!(text[details + 1].trim_end(), "Blocked one");
        assert_eq!(text[39].trim_end(), FOOTER);
        // The selection waits on the session on show.
        assert_eq!(app.selected_session().unwrap().name, "noc--billing");
        // Closed, a section is its title row, and the list has its rows.
        app.toggle_sessions();
        let text = plain(&panel(&mut app, &Theme::default(), 40, 38));
        let list = at(&text, "Sessions");
        assert_eq!(text[list], rule("\u{25b8} Sessions  3"));
        assert_eq!(text[list + 1], rule("\u{25be} Details"));
        app.toggle_details();
        let text = plain(&panel(&mut app, &Theme::default(), 40, 38));
        assert_eq!(
            text[36].trim_end(),
            "6 agents                             \u{25cf}"
        );
        assert_eq!(text[37], rule("\u{25b8} Sessions  3"));
        assert_eq!(text[38], rule("\u{25b8} Details"));
        assert_eq!(text[39].trim_end(), FOOTER);
        app.toggle_sessions();
        app.toggle_details();
        // The inner session alone is a session too.
        sessions(&mut app, &["noc"], "noc");
        let text = plain(&panel(&mut app, &Theme::default(), 40, 38));
        let list = at(&text, "Sessions");
        assert_eq!(text[list], rule("\u{25be} Sessions"));
        assert_eq!(text[list + 1].trim_end(), " \u{25cf} no-team  1 working");
        assert_eq!(text[list + 2].trim_end(), "1 session");
        // A short pane has no room for the section.
        let text = plain(&panel(&mut app, &Theme::default(), 14, 38));
        assert!(!text.iter().any(|l| l.contains("Sessions")));
        // More sessions than rows: the selected one stays in view.
        let many: Vec<String> = (0..9).map(|i| format!("noc--t{i}")).collect();
        let names: Vec<&str> = many.iter().map(String::as_str).collect();
        sessions(&mut app, &names, "noc--t7");
        let text = plain(&panel(&mut app, &Theme::default(), 40, 38));
        let list = at(&text, "Sessions");
        let shown: Vec<&str> = text[list + 1..list + 6].iter().map(|l| l.trim()).collect();
        assert_eq!(shown[0], "t4  no agents");
        assert_eq!(shown[3], "\u{25cf} t7  no agents");
        assert_eq!(text[list + 6].trim_end(), "9 sessions");
        assert!(text[list + 7].contains("Details"));
    }

    #[test]
    fn the_section_with_the_keys_shows_its_selection_and_its_details() {
        let mut app = app_with(TEAMS);
        sessions(&mut app, &["noc", "noc--billing"], "noc");
        let theme = Theme::default();
        let highlight = "48;2;49;50;68";
        let accent = color_code(theme.accent, false);
        // A title is in the accent colour while its section has the keys.
        let titled = |raw: &[String], title: &str| {
            let at = plain(raw).iter().position(|l| l.contains(title)).unwrap();
            raw[at].starts_with(&format!("\u{1b}[1;{accent}m"))
        };
        let raw = panel(&mut app, &theme, 40, 38);
        let text = plain(&raw);
        let list = text.iter().position(|l| l.contains("Sessions")).unwrap();
        let details = text.iter().position(|l| l.contains("Details")).unwrap();
        // The agents have the keys: their selected row is highlighted, no
        // session row is, and the details are the agent's.
        assert!(titled(&raw, "Agents") && !titled(&raw, "Sessions") && !titled(&raw, "Details"));
        assert!(raw[2].contains(highlight));
        assert!(!raw[list + 1..].iter().any(|l| l.contains(highlight)));
        assert_eq!(text[details + 1].trim_end(), "Blocked one");
        // `Tab`: the sessions have them, and the details are the session's.
        app.toggle_focus(true);
        assert_eq!(app.focus, Focus::Sessions);
        let raw = panel(&mut app, &theme, 40, 38);
        let text = plain(&raw);
        assert!(!titled(&raw, "Agents") && titled(&raw, "Sessions"));
        assert!(!raw[2].contains(highlight));
        assert!(raw[list + 2].contains(highlight), "the session on show");
        let got: Vec<&str> = text[details + 1..details + 7]
            .iter()
            .map(|l| l.trim_end())
            .collect();
        assert_eq!(
            got,
            [
                "Session \u{b7} no-team",
                "zellij    noc",
                "state     on show",
                "agents    1",
                "  1 working",
                "Enter shows it.",
            ]
        );
        app.move_session(-1);
        let text = plain(&panel(&mut app, &theme, 40, 38));
        let got: Vec<&str> = text[details + 1..details + 7]
            .iter()
            .map(|l| l.trim_end())
            .collect();
        assert_eq!(
            got,
            [
                "Session \u{b7} billing",
                "zellij    noc--billing",
                "state     running, out of sight",
                "agents    4",
                "  1 blocked \u{b7} 1 working \u{b7} 1 review",
                "Enter shows it; x ends it.",
            ]
        );
        // `Shift Tab`: the details have the keys, and are still about the
        // session, whose row stays highlighted.
        app.toggle_focus(false);
        assert_eq!(
            (app.focus, app.list_focus()),
            (Focus::Details, Focus::Sessions)
        );
        let raw = panel(&mut app, &theme, 40, 38);
        assert!(titled(&raw, "Details") && !titled(&raw, "Sessions"));
        assert!(raw[list + 1].contains(highlight));
        assert_eq!(
            plain(&raw)[details + 1].trim_end(),
            "Session \u{b7} billing"
        );
        // And round to the agents.
        app.toggle_focus(true);
        assert_eq!(
            (app.focus, app.list_focus()),
            (Focus::Agents, Focus::Agents)
        );
        app.toggle_focus(false);
        assert_eq!(
            (app.focus, app.list_focus()),
            (Focus::Details, Focus::Agents)
        );
        app.focus_agents();
        // Asking to end a session takes the status line.
        app.pending_end = Some("noc--billing".to_string());
        let text = plain(&panel(&mut app, &theme, 40, 38));
        assert!(text
            .iter()
            .any(|l| l.trim_end() == "End session billing?  y / n"));
        app.pending_end = None;
        // A search holds some agents only, so the rows say nothing of them.
        app.flat = true;
        let text = plain(&panel(&mut app, &theme, 40, 38));
        assert_eq!(text[list + 1].trim_end(), "   billing");
    }

    #[test]
    fn the_details_scroll_down_and_sideways() {
        let mut app = app_with(DEMO);
        let theme = Theme::default();
        let details = |app: &mut ZellijNoc, cols: usize| -> Vec<String> {
            let text = plain(&panel(app, &theme, 41, cols));
            let at = text.iter().position(|l| l.contains("Details")).unwrap();
            text[at..text.len() - 1]
                .iter()
                .map(|l| l.trim_end().to_string())
                .collect()
        };
        // Fifteen lines in eleven rows: the title says which are in view.
        let shown = details(&mut app, 38);
        assert_eq!(shown.len(), 12);
        assert!(shown[0].starts_with("\u{25be} Details  1-11/15 \u{2500}"));
        assert_eq!(shown[1], "Auth migration");
        assert_eq!((app.detail_max_scroll, app.detail_rows), (4, 11));
        app.scroll_details(2);
        let shown = details(&mut app, 38);
        assert!(shown[0].starts_with("\u{25be} Details  3-13/15 "));
        assert_eq!(shown[1], "repo      app \u{b7} main");
        // No further than the last line.
        app.scroll_details(isize::MAX);
        let shown = details(&mut app, 38);
        assert!(shown[0].starts_with("\u{25be} Details  5-15/15 "));
        assert_eq!(shown[11], "pane      work/7 \u{b7} observed");
        app.scroll_details(isize::MIN);
        assert_eq!(app.detail_scroll, 0);
        // Sideways, in a pane narrower than its longest line, as far as
        // that line's end and no further.
        let narrow = details(&mut app, 24);
        let cwd = narrow.iter().position(|l| l.starts_with("cwd")).unwrap();
        assert_eq!(narrow[cwd], "cwd       /code/app/svc");
        assert!(app.detail_max_shift > 0);
        app.shift_details(4);
        let shifted = details(&mut app, 24);
        assert_eq!(shifted[cwd], "      /code/app/svc");
        assert_eq!(shifted[1], " migration");
        assert!(
            shifted[0].starts_with("\u{25be} Details"),
            "the title stays"
        );
        app.shift_details(isize::MAX);
        assert_eq!(app.detail_shift, app.detail_max_shift);
        // Another agent: its details start at the top again.
        app.scroll_details(3);
        app.move_selection(1);
        let shown = details(&mut app, 38);
        assert_eq!((app.detail_scroll, app.detail_shift), (0, 0));
        assert_eq!(shown[1], "Cache refactor");
    }

    #[test]
    fn renders_keyboard_help_on_request() {
        let mut app = app_with("[]");
        app.toggle_help();
        let text = plain(&render(&mut app, &Theme::default(), 34, 38));
        assert_eq!(text[0].trim_end(), "In the list");
        assert_eq!(text[1].trim_end(), "j / k       move selection");
        assert_eq!(text[5].trim_end(), "h           close a section or a team");
        assert_eq!(text[6].trim_end(), "v           group by team / status");
        assert_eq!(text[7].trim_end(), "p / s       pane / stacked pane here");
        assert_eq!(text[9].trim_end(), "o           show its team's session");
        assert_eq!(text[10].trim_end(), "Tab         agents / sessions");
        assert_eq!(text[11].trim_end(), "Shift-Tab   into the details, and out");
        assert_eq!(text[12].trim_end(), "S           close / open sessions");
        assert_eq!(text[15].trim_end(), "n           rename agent");
        assert_eq!(text[16].trim_end(), "t / T       join / leave a team");
        assert_eq!(text[17].trim_end(), "x           stop agent (asks y/n)");
        assert_eq!(text[19].trim_end(), "/           search (also older:14d)");
        assert_eq!(text[20].trim_end(), "u           check for a new release");
        assert_eq!(text[21].trim_end(), "i           status and version");
        assert_eq!(text[22].trim_end(), "? / Esc     close help");
        assert_eq!(text[23], " ".repeat(38));
        assert_eq!(text[24].trim_end(), "Anywhere");
        assert_eq!(text[25].trim_end(), "Alt-m       jump to list and back");
        assert_eq!(text[26].trim_end(), "Alt-o       hide / show this panel");
        assert_eq!(text[27].trim_end(), "Alt-d       detach both sessions");
        assert_eq!(text[28], " ".repeat(38));
        assert_eq!(
            text[29].trim_end(),
            format!("version     v{}", crate::update::CURRENT)
        );
        // The status line last, above the footer.
        assert_eq!(header(&text), "0 agents");
        assert_eq!(status_at(&text), 32);
        assert_eq!(text[33].trim_end(), FOOTER);
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
        // Where the work is, what it is, then which program does it. The
        // ask wraps at 68 columns.
        let want = [
            "Auth migration",
            "!  needs approval \u{b7} 2m ago",
            "repo      app \u{b7} main",
            "cwd       /code/app/svc",
            "ask",
            "  Migrate the auth service to the new token format and update all",
            "  callers",
            "latest",
            "  Waiting for approval to delete the build directory.",
            "provider  claude \u{b7} background",
            "native    auth",
            "id        3cfff231",
            "session   3cfff231-aaaa-bbbb",
            "model     opus \u{b7} high",
            "pane      work/7 \u{b7} observed",
        ];
        for (i, w) in want.iter().enumerate() {
            assert_eq!(text[i].trim_end(), *w, "line {i}");
        }
        assert_eq!(text[23].trim_end(), "Esc / d  close");

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
        // The team prompt takes the status line the same way.
        assert!(app.begin_team());
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(status(&text), "team: \u{258f}");
        app.pending_team.as_mut().unwrap().text = "auth".into();
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(status(&text), "team: auth\u{258f}");
        app.pending_team = None;
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
        assert_eq!(header(&text), "0 agents");
        assert_eq!(status_at(&text), 12);
        assert_eq!(text[13].trim_end(), FOOTER);
        // At the installed 38-column width the last line is cut with an ellipsis.
        let narrow = plain(&render(&mut app, &Theme::default(), 14, 38));
        assert_eq!(
            narrow[5].trim_end(),
            "This panel will reconnect automatical\u{2026}"
        );
        // The dot is red while the server is offline and green once connected;
        // the words are in the status box.
        let raw = render(&mut app, &Theme::default(), 14, 42);
        assert!(
            raw[12].contains("38;2;243;139;168m\u{25cf}"),
            "{:?}",
            raw[12]
        );
        assert_eq!(
            info(&mut app, 14, 42)[0],
            "\u{25cf} server offline \u{b7} retrying"
        );
        let mut app = app_with("[]");
        let raw = render(&mut app, &Theme::default(), 14, 42);
        assert!(
            raw[12].contains("38;2;166;227;161m\u{25cf}"),
            "{:?}",
            raw[12]
        );
        assert_eq!(info(&mut app, 14, 42)[0], "\u{25cf} connected \u{b7} poll");
    }

    #[test]
    fn empty_and_error_states() {
        let mut app = app_with("[]");
        let text = plain(&render(&mut app, &Theme::default(), 8, 38));
        assert_eq!(text[0].trim_end(), "No live agents.");
        app.set_error("bad".into());
        let text = plain(&render(&mut app, &Theme::default(), 8, 38));
        assert_eq!(header(&text), "0 agents");
        assert_eq!(info(&mut app, 8, 38)[0], "\u{25cf} disconnected \u{b7} bad");
        assert_eq!(text[0].trim_end(), "No registry snapshot available.");
        // A pipe panel that has heard nothing says what it waits for.
        let mut app = ZellijNoc::new(Config {
            transport: Transport::Pipe,
            ..Config::default()
        });
        assert_eq!(
            info(&mut app, 8, 38)[0],
            "\u{25cf} waiting for zellij-noc.registry"
        );
        // Yellow while it waits.
        let raw = render(&mut app, &Theme::default(), 8, 38);
        assert!(raw[6].contains("38;2;249;226;175m\u{25cf}"), "{:?}", raw[6]);
    }

    #[test]
    fn a_newer_release_is_marked_on_the_header_and_explained_in_help() {
        let mut app = app_with("[]");
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        assert_eq!(header(&text), "0 agents");
        app.update = Some("v0.2.0".into());
        let text = plain(&render(&mut app, &Theme::default(), 30, 38));
        // Only an arrow beside the dot; the versions are in the status box.
        assert_eq!(
            text[status_at(&text)],
            format!("0 agents{}\u{2191} \u{25cf}", " ".repeat(27))
        );
        assert_eq!(
            info(&mut app, 30, 38)[1..],
            [
                "\u{2191} v0.2.0 is available".to_string(),
                format!("  this is v{}", crate::update::CURRENT),
                "  set the new version in the".to_string(),
                "  plugin URL".to_string(),
            ]
        );
        // Narrow: both still fit.
        let text = plain(&render(&mut app, &Theme::default(), 30, 22));
        assert_eq!(
            text[28],
            format!("0 agents{}\u{2191} \u{25cf}", " ".repeat(11))
        );
        // Narrower than the count: the count is cut and the dot stays.
        let text = plain(&render(&mut app, &Theme::default(), 30, 8));
        assert_eq!(text[28], "0 a\u{2026} \u{2191} \u{25cf}");
        app.show_help = true;
        let text = plain(&render(&mut app, &Theme::default(), 34, 38));
        assert_eq!(
            text[30].trim_end(),
            "\u{2191} v0.2.0    newer plugin release;"
        );
        assert_eq!(text[31].trim_end(), "            set it in the plugin URL");
        assert_eq!(text[33].trim_end(), FOOTER);
    }

    #[test]
    fn search_results_are_a_flat_list_with_the_query_on_the_status_line() {
        use crate::app::Search;
        let mut app = app_with(DEMO);
        let folded = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&folded), "6 agents");
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

        // Results: every match is a row, however old, and none is folded.
        let list = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":{DEMO}}}"#
        ))
        .unwrap();
        app.apply_snapshot(list, Transport::Poll);
        app.search.as_mut().unwrap().typing = false;
        let found = plain(&render(&mut app, &Theme::default(), 60, 38));
        assert_eq!(header(&found), "7 matches  / an");
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
        assert_eq!(header(&text), "1 match  / an");
        let none = parse_snapshot(&format!(
            r#"{{"schema_version":1,"generated_at":"{NOW}","agents":[]}}"#
        ))
        .unwrap();
        app.apply_snapshot(none, Transport::Poll);
        let text = plain(&render(&mut app, &Theme::default(), 20, 38));
        assert_eq!(header(&text), "0 matches  / an");
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
        assert_eq!(header(&text), "6 agents");
    }

    #[test]
    fn problems_turn_the_dot_yellow_and_are_spelled_out_in_the_status_box() {
        let mut app = app_with("[]");
        app.guest_problem = Some("inner session \"noc\" is not running".into());
        let text = plain(&render(&mut app, &Theme::default(), 12, 60));
        assert_eq!(header(&text), "0 agents");
        let raw = render(&mut app, &Theme::default(), 12, 60);
        assert!(
            raw[10].contains("38;2;249;226;175m\u{25cf}"),
            "{:?}",
            raw[10]
        );
        assert_eq!(
            info(&mut app, 12, 60),
            [
                "\u{25cf} connected \u{b7} poll".to_string(),
                "! inner session \"noc\" is not running".to_string(),
                format!("version v{}", crate::update::CURRENT),
            ]
        );
        // A settings problem is listed first. In the sidebar's width both wrap.
        app.config_problem = Some("home_theme is set but away_theme is not".into());
        assert_eq!(
            info(&mut app, 14, 38)[1..5],
            [
                "! home_theme is set but away_theme",
                "  is not",
                "! inner session \"noc\" is not",
                "  running",
            ]
        );
        // An error is worse than a problem: the dot is red.
        app.set_error("boom".into());
        let raw = render(&mut app, &Theme::default(), 12, 60);
        assert!(
            raw[10].contains("38;2;243;139;168m\u{25cf}"),
            "{:?}",
            raw[10]
        );
        // A notice takes the whole status line while it lasts.
        app.set_notice("renamed to x");
        let text = plain(&render(&mut app, &Theme::default(), 12, 60));
        assert_eq!(status(&text), "renamed to x");
    }

    #[test]
    fn the_status_box_sits_on_the_status_line_and_a_click_toggles_it() {
        let mut app = app_with(DEMO);
        let closed = plain(&render(&mut app, &Theme::default(), 40, 38));
        let row = status_at(&closed);
        assert_eq!(app.status_row, Some(row));
        // A click elsewhere does nothing; on the status line it opens the box.
        assert!(!app.click(0));
        assert!(app.click(row as isize) && app.show_info);
        let open = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(open.len(), 40);
        assert!(open.iter().all(|l| l.width() == 38));
        assert_eq!(
            open[row - 4..row],
            [
                format!("\u{256d}\u{2500} status {}\u{256e}", "\u{2500}".repeat(27)),
                format!(
                    "\u{2502} \u{25cf} connected \u{b7} poll{}\u{2502}",
                    " ".repeat(17)
                ),
                format!(
                    "\u{2502} {:<34} \u{2502}",
                    format!("version v{}", crate::update::CURRENT)
                ),
                format!("\u{2570}{}\u{256f}", "\u{2500}".repeat(36)),
            ]
        );
        // Nothing else moved.
        assert_eq!(open[..row - 4], closed[..row - 4]);
        assert_eq!(open[row..], closed[row..]);
        // A click anywhere else closes it; so does a second click on the line.
        assert!(app.click(2) && !app.show_info);
        assert!(app.click(row as isize) && app.click(row as isize) && !app.show_info);
        // A pane too short for the whole box shows its end.
        app.show_info = true;
        let tiny = plain(&render(&mut app, &Theme::default(), 4, 38));
        assert_eq!(
            tiny[1],
            format!("\u{2570}{}\u{256f}", "\u{2500}".repeat(36))
        );
        assert_eq!(header(&tiny), "6 agents");
    }

    #[test]
    fn errors_keep_the_last_good_list() {
        let mut app = app_with(DEMO);
        app.set_error("boom".into());
        let text = plain(&render(&mut app, &Theme::default(), 40, 38));
        assert_eq!(header(&text), "6 agents");
        assert_eq!(
            info(&mut app, 40, 38)[0],
            "\u{25cf} disconnected \u{b7} boom"
        );
        assert_eq!(text[0].trim_end(), "Blocked  1");
    }

    #[test]
    fn very_small_panes_keep_the_status_line() {
        let mut app = app_with(DEMO);
        let theme = Theme::default();
        assert!(render(&mut app, &theme, 0, 38).is_empty());
        assert!(render(&mut app, &theme, 10, 0).is_empty());
        let one = plain(&render(&mut app, &theme, 1, 38));
        assert_eq!(one.len(), 1);
        assert!(one[0].starts_with("6 agents"));
        // Two rows: one list line, windowed to the selected row, and the status.
        let two = plain(&render(&mut app, &theme, 2, 38));
        assert_eq!(two.len(), 2);
        assert_eq!(row(&two[0]), " ! Auth migration");
        assert!(two[1].starts_with("6 agents"));
        // From three rows the footer has room too.
        let three = plain(&render(&mut app, &theme, 3, 38));
        assert_eq!(three.len(), 3);
        assert_eq!(row(&three[0]), " ! Auth migration");
        assert!(three[1].starts_with("6 agents"));
        assert_eq!(three[2].trim_end(), FOOTER);
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
    fn the_selected_row_is_highlighted_and_group_colours_are_fixed() {
        let mut app = app_with(DEMO);
        let theme = Theme::default();
        let raw = render(&mut app, &theme, 40, 38);
        // The selected row carries the selection background.
        assert!(raw[1].contains("48;2;49;50;68"));
        assert!(!raw[3].contains("48;2;49;50;68"));
        // herdr's colours: a blue header, red, yellow, teal and grey groups.
        let at = status_at(&plain(&raw));
        assert!(raw[at].contains("38;2;137;180;250"), "header is blue");
        assert!(raw[0].contains("38;2;243;139;168"), "Blocked is red");
        assert!(raw[2].contains("38;2;249;226;175"), "Working is yellow");
        assert!(raw[4].contains("38;2;148;226;213"), "Review is teal");
        assert!(raw[6].contains("38;2;147;153;178"), "Quiet is grey");
        // The built-in colours are never dimmed: dim grey on a dark
        // background cannot be read.
        assert!(!raw
            .iter()
            .any(|l| l.contains("\x1b[2;") || l.contains(";2;38;")));
        // Following Zellij's theme puts the groups on fixed ANSI entries.
        let zellij = Theme::from_mode_info(&ModeInfo::default());
        let raw = render(&mut app, &zellij, 40, 38);
        assert!(raw[0].contains("38;5;1"), "Blocked heading is ANSI 1");
        assert!(raw[2].contains("38;5;2"), "Working heading is ANSI 2");
        assert!(raw[4].contains("38;5;6"), "Review heading is ANSI 6");
        assert!(raw[6].contains("38;5;8"), "Quiet heading is ANSI 8");
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
