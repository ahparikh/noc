//! The `bar` role: the tab bar of a session. It is Zellij's tab bar with one
//! difference: where that one says "Zellij", this one says which team the
//! session belongs to.

use crate::guest::team_label;
use crate::view::{styled_span, truncate, Style};
use crate::State;
use unicode_width::UnicodeWidthStr;
use zellij_tile::prelude::{
    set_selectable, switch_tab_to, Event, InputMode, ModeInfo, Mouse, PaletteColor, TabInfo,
};

const ARROW: &str = "\u{e0b0}";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BarColors {
    pub label: PaletteColor,
    pub tab_text: PaletteColor,
    pub tab: PaletteColor,
    pub active_text: PaletteColor,
    pub active: PaletteColor,
}

impl BarColors {
    /// The colours Zellij's own tab bar gives its tabs; the label is in the
    /// colour of the active tab. The bar itself has no colour: it shows the
    /// terminal's background.
    pub fn from_mode_info(info: &ModeInfo) -> BarColors {
        let colors = &info.style.colors;
        BarColors {
            label: colors.ribbon_selected.background,
            tab_text: colors.ribbon_unselected.base,
            tab: colors.ribbon_unselected.background,
            active_text: colors.ribbon_selected.base,
            active: colors.ribbon_selected.background,
        }
    }
}

#[derive(Debug)]
pub struct BarState {
    pub tabs: Vec<TabInfo>,
    pub session: Option<String>,
    pub mode: InputMode,
    pub colors: BarColors,
    /// Whether the terminal draws the arrow glyph between tabs.
    pub arrows: bool,
    /// Where each tab was last drawn: first column, the column after its
    /// last, and the tab's position.
    pub regions: Vec<(usize, usize, usize)>,
}

impl Default for BarState {
    fn default() -> Self {
        BarState {
            tabs: Vec::new(),
            session: None,
            mode: InputMode::default(),
            colors: BarColors::from_mode_info(&ModeInfo::default()),
            arrows: true,
            regions: Vec::new(),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Label,
    Tab,
    Active,
}

/// One run of the bar: the label, a tab, or a "more tabs" marker (which is
/// drawn as a tab and leads to the first tab it stands for).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Piece {
    pub text: String,
    pub kind: Kind,
    pub tab: Option<usize>,
}

/// What a tab says: its name, and what Zellij's own bar adds to it.
pub fn tab_text(tab: &TabInfo, mode: InputMode) -> String {
    let mut text: String = tab.name.chars().filter(|c| !c.is_control()).collect();
    if tab.active && mode == InputMode::RenameTab && text.is_empty() {
        text = "Enter name...".to_string();
    }
    if tab.is_fullscreen_active {
        text.push_str(" (FULLSCREEN)");
    }
    if tab.is_sync_panes_active {
        text.push_str(" (SYNC)");
    }
    text
}

/// The columns a piece takes: its text, a space either side, and for a
/// tab the separator before and after it (one gap without arrow glyphs).
fn piece_width(text: &str, kind: Kind, arrows: bool) -> usize {
    let separators = match (kind, arrows) {
        (Kind::Label, _) => 0,
        (_, true) => 2,
        (_, false) => 1,
    };
    text.width() + 2 + separators
}

/// The bar's pieces for `cols` columns: the label, then every tab, or, when
/// they do not all fit, the active tab with as many neighbours as fit and a
/// marker counting the tabs left out on each side. `tabs` are `(text,
/// active)` in tab order.
pub fn bar_pieces(label: &str, tabs: &[(String, bool)], cols: usize, arrows: bool) -> Vec<Piece> {
    let mut pieces = vec![Piece {
        text: label.to_string(),
        kind: Kind::Label,
        tab: None,
    }];
    let space = cols.saturating_sub(piece_width(label, Kind::Label, arrows));
    let width = |text: &str| piece_width(text, Kind::Tab, arrows);
    let before = |lo: usize| format!("\u{2190} +{lo}");
    let after = |hi: usize| format!("+{} \u{2192}", tabs.len() - 1 - hi);
    // The columns the tabs `lo..=hi` take with the markers they need.
    let span = |lo: usize, hi: usize| {
        let mut total: usize = tabs[lo..=hi].iter().map(|(text, _)| width(text)).sum();
        if lo > 0 {
            total += width(&before(lo));
        }
        if hi + 1 < tabs.len() {
            total += width(&after(hi));
        }
        total
    };
    if tabs.is_empty() {
        return pieces;
    }
    let active = tabs.iter().position(|(_, active)| *active).unwrap_or(0);
    let (mut lo, mut hi) = (0, tabs.len() - 1);
    if span(lo, hi) > space {
        (lo, hi) = (active, active);
        loop {
            let mut grew = false;
            if hi + 1 < tabs.len() && span(lo, hi + 1) <= space {
                hi += 1;
                grew = true;
            }
            if lo > 0 && span(lo - 1, hi) <= space {
                lo -= 1;
                grew = true;
            }
            if !grew {
                break;
            }
        }
    }
    let tab = |text: String, kind: Kind, position: usize| Piece {
        text,
        kind,
        tab: Some(position),
    };
    if lo > 0 {
        pieces.push(tab(before(lo), Kind::Tab, lo - 1));
    }
    for (position, (text, is_active)) in tabs.iter().enumerate().take(hi + 1).skip(lo) {
        let mut text = text.clone();
        if lo == hi {
            // Alone and still too wide: cut the name.
            let spare = space.saturating_sub(span(lo, hi) - text.width());
            text = truncate(&text, spare.max(1));
        }
        let kind = if *is_active { Kind::Active } else { Kind::Tab };
        pieces.push(tab(text, kind, position));
    }
    if hi + 1 < tabs.len() {
        pieces.push(tab(after(hi), Kind::Tab, hi + 1));
    }
    pieces
}

/// The bar as one styled line of `cols` columns, and where its tabs are.
pub fn bar_line(
    pieces: &[Piece],
    colors: &BarColors,
    cols: usize,
    arrows: bool,
) -> (String, Vec<(usize, usize, usize)>) {
    // No background where there is no tab: the bar shows the terminal's own.
    let span = |text: &str, fg: PaletteColor, bg: Option<PaletteColor>, bold: bool| {
        styled_span(
            text,
            Style {
                fg: Some(fg),
                bg,
                bold,
                dim: false,
            },
        )
    };
    let mut line = String::new();
    let mut regions = Vec::new();
    let mut at = 0;
    for piece in pieces {
        let width = piece_width(&piece.text, piece.kind, arrows);
        if at + width > cols {
            break;
        }
        let text = format!(" {} ", piece.text);
        match piece.kind {
            Kind::Label => line.push_str(&span(&text, colors.label, None, true)),
            Kind::Tab | Kind::Active => {
                let (fg, bg) = if piece.kind == Kind::Active {
                    (colors.active_text, colors.active)
                } else {
                    (colors.tab_text, colors.tab)
                };
                // With arrow glyphs: a gap, the tab, its arrow. Without:
                // the tab, a gap.
                if arrows {
                    line.push(' ');
                    line.push_str(&span(&text, fg, Some(bg), true));
                    line.push_str(&span(ARROW, bg, None, false));
                } else {
                    line.push_str(&span(&text, fg, Some(bg), true));
                    line.push(' ');
                }
            }
        }
        if let Some(tab) = piece.tab {
            regions.push((at, at + width, tab));
        }
        at += width;
    }
    if at < cols {
        line.push_str(&" ".repeat(cols - at));
    }
    (line, regions)
}

impl State {
    pub fn update_bar(&mut self, event: Event) -> bool {
        match event {
            Event::PermissionRequestResult(_) => {
                set_selectable(false);
                true
            }
            Event::TabUpdate(tabs) => {
                self.bar.tabs = tabs;
                true
            }
            Event::ModeUpdate(info) => {
                self.bar.colors = BarColors::from_mode_info(&info);
                self.bar.arrows = !info.capabilities.arrow_fonts;
                self.bar.mode = info.mode;
                if let Some(name) = info.session_name {
                    self.bar.session = Some(name);
                }
                true
            }
            Event::Mouse(Mouse::LeftClick(_, col)) => {
                if let Some((_, _, tab)) = self
                    .bar
                    .regions
                    .iter()
                    .find(|(from, to, _)| (*from..*to).contains(&col))
                {
                    switch_tab_to(*tab as u32 + 1);
                }
                false
            }
            Event::Mouse(Mouse::ScrollUp(_)) => {
                self.bar_step(1);
                false
            }
            Event::Mouse(Mouse::ScrollDown(_)) => {
                self.bar_step(-1);
                false
            }
            _ => false,
        }
    }

    /// The tab after (`1`) or before (`-1`) the active one, if there is one.
    fn bar_step(&self, by: isize) {
        let Some(active) = self.bar.tabs.iter().position(|t| t.active) else {
            return;
        };
        let target = active as isize + by;
        if (0..self.bar.tabs.len() as isize).contains(&target) {
            switch_tab_to(target as u32 + 1);
        }
    }

    pub fn render_bar(&mut self, cols: usize) -> Vec<String> {
        let label = team_label(self.bar.session.as_deref().unwrap_or_default());
        let mut tabs = self.bar.tabs.clone();
        tabs.sort_by_key(|t| t.position);
        let texts: Vec<(String, bool)> = tabs
            .iter()
            .map(|t| (tab_text(t, self.bar.mode), t.active))
            .collect();
        let mut pieces = bar_pieces(label, &texts, cols, self.bar.arrows);
        // Pieces name tabs by index; Zellij by position.
        for piece in &mut pieces {
            piece.tab = piece.tab.and_then(|i| tabs.get(i)).map(|t| t.position);
        }
        let (line, regions) = bar_line(&pieces, &self.bar.colors, cols, self.bar.arrows);
        self.bar.regions = regions;
        vec![line]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tabs(names: &[&str], active: usize) -> Vec<(String, bool)> {
        names
            .iter()
            .enumerate()
            .map(|(i, name)| (name.to_string(), i == active))
            .collect()
    }

    fn shown(pieces: &[Piece]) -> Vec<&str> {
        pieces.iter().map(|p| p.text.as_str()).collect()
    }

    #[test]
    fn the_bar_starts_with_the_team_and_keeps_the_active_tab_in_view() {
        // Everything fits: the label, then every tab.
        let all = bar_pieces("auth", &tabs(&["lead", "worker-1"], 1), 80, true);
        assert_eq!(shown(&all), ["auth", "lead", "worker-1"]);
        assert_eq!(all[0].kind, Kind::Label);
        assert_eq!((all[1].kind, all[1].tab), (Kind::Tab, Some(0)));
        assert_eq!((all[2].kind, all[2].tab), (Kind::Active, Some(1)));
        // No tabs yet: the label alone.
        assert_eq!(shown(&bar_pieces("no-team", &[], 80, true)), ["no-team"]);

        // Too many: the active tab, the neighbours that fit, and how many
        // are left out on each side. A marker leads to the nearest of them.
        let names = ["first-one", "two", "three", "four", "five", "six"];
        let some = bar_pieces("auth", &tabs(&names, 2), 48, true);
        assert_eq!(
            shown(&some),
            ["auth", "\u{2190} +1", "two", "three", "four", "+2 \u{2192}"]
        );
        assert_eq!(some[1].tab, Some(0));
        assert_eq!(some[5].tab, Some(4));
        let first = bar_pieces("auth", &tabs(&names, 0), 30, true);
        assert_eq!(shown(&first), ["auth", "first-one", "+5 \u{2192}"]);
        // Space for the active tab only, and not all of its name.
        let cut = bar_pieces("auth", &tabs(&["a-very-long-tab-name", "b"], 0), 24, true);
        assert_eq!(shown(&cut), ["auth", "a-ver\u{2026}", "+1 \u{2192}"]);
    }

    #[test]
    fn the_bar_fills_its_row_and_knows_where_its_tabs_are() {
        let colors = BarColors::from_mode_info(&ModeInfo::default());
        let pieces = bar_pieces("auth", &tabs(&["lead", "w1"], 0), 30, true);
        let (line, regions) = bar_line(&pieces, &colors, 30, true);
        let plain: String = {
            // Drop the escape sequences.
            let mut out = String::new();
            let mut chars = line.chars();
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
        };
        assert_eq!(
            plain,
            format!(" auth   lead {ARROW}  w1 {ARROW}{}", " ".repeat(10))
        );
        assert_eq!(plain.width(), 30);
        // " auth " is 6 columns; "lead" takes 8 with its arrows, "w1" 6.
        assert_eq!(regions, [(6, 14, 0), (14, 20, 1)]);
        // Without arrow glyphs a tab is followed by a gap.
        let (line, regions) = bar_line(&pieces, &colors, 30, false);
        assert!(!line.contains(ARROW));
        assert_eq!(regions, [(6, 13, 0), (13, 18, 1)]);
    }

    #[test]
    fn a_tab_says_what_zellijs_own_bar_says() {
        let tab = |name: &str| TabInfo {
            name: name.to_string(),
            active: true,
            ..TabInfo::default()
        };
        assert_eq!(tab_text(&tab("lead"), InputMode::Normal), "lead");
        assert_eq!(tab_text(&tab(""), InputMode::RenameTab), "Enter name...");
        assert_eq!(tab_text(&tab("x\u{1b}[31m"), InputMode::Normal), "x[31m");
        let mut busy = tab("lead");
        busy.is_fullscreen_active = true;
        busy.is_sync_panes_active = true;
        assert_eq!(
            tab_text(&busy, InputMode::Normal),
            "lead (FULLSCREEN) (SYNC)"
        );
    }
}
