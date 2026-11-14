//! The floating details-window instance (`role "details"`).

use crate::app::{Connection, Transport};
use crate::model::{now_rfc3339, parse_agent, Agent};
use crate::panes::{compact_error, is_server_unavailable};
use crate::view::Theme;
use crate::{ctx, State, CTX_DETAILS, KEY};
use zellij_tile::prelude::*;

const DETAILS_POLL_SECONDS: f64 = 2.0;

pub struct DetailsState {
    pub agent_id: String,
    pub agent: Option<Agent>,
    pub connection: Connection,
    pub generated_at: String,
    pub in_flight: bool,
    pub title: Option<String>,
}

impl Default for DetailsState {
    fn default() -> Self {
        DetailsState {
            agent_id: String::new(),
            agent: None,
            connection: Connection::Connecting,
            generated_at: String::new(),
            in_flight: false,
            title: None,
        }
    }
}

/// `Esc`, `D`, `d` or `q`, with any modifiers.
pub fn is_close_key(key: &KeyWithModifier) -> bool {
    matches!(key.bare_key, BareKey::Esc | BareKey::Char('D' | 'd' | 'q'))
}

impl State {
    pub fn update_details(&mut self, event: Event) -> bool {
        match event {
            Event::PermissionRequestResult(PermissionStatus::Granted) => {
                self.permissions_granted = true;
                self.refresh_details();
                true
            }
            Event::PermissionRequestResult(PermissionStatus::Denied) => {
                self.permissions_granted = false;
                self.details.connection = Connection::Error(
                    "permissions denied: agent details are unavailable".to_string(),
                );
                true
            }
            Event::ModeUpdate(info) => {
                if self.config.zellij_colors {
                    self.theme = Theme::from_mode_info(&info);
                }
                true
            }
            Event::Timer(_) => {
                self.refresh_details();
                false
            }
            Event::RunCommandResult(exit, stdout, stderr, context) => {
                if context.get(KEY).map(String::as_str) != Some(CTX_DETAILS) {
                    return false;
                }
                self.details.in_flight = false;
                set_timeout(DETAILS_POLL_SECONDS);
                self.on_details_result(exit, &stdout, &stderr);
                true
            }
            Event::Key(key) => {
                if is_close_key(&key) {
                    close_self();
                }
                false
            }
            _ => false,
        }
    }

    fn refresh_details(&mut self) {
        if !self.permissions_granted || self.details.in_flight || self.details.agent_id.is_empty() {
            return;
        }
        self.details.in_flight = true;
        run_command(
            &[
                self.config.noc_bin.as_str(),
                "show",
                "--json",
                self.details.agent_id.as_str(),
            ],
            ctx(CTX_DETAILS),
        );
    }

    fn on_details_result(&mut self, exit: Option<i32>, stdout: &[u8], stderr: &[u8]) {
        if exit == Some(0) {
            let parsed = std::str::from_utf8(stdout)
                .map_err(|e| format!("noc returned non-UTF-8 output: {e}"))
                .and_then(parse_agent);
            match parsed {
                Ok(agent) => {
                    let title = agent.title();
                    if self.details.title.as_deref() != Some(title.as_str()) {
                        rename_plugin_pane(self.plugin_id, format!("noc \u{b7} {title}"));
                        self.details.title = Some(title);
                    }
                    self.details.agent = Some(agent);
                    self.details.generated_at = now_rfc3339();
                    self.details.connection = Connection::Connected(Transport::Poll);
                }
                Err(e) => self.details.connection = Connection::Error(e),
            }
        } else {
            let detail = compact_error(&String::from_utf8_lossy(stderr));
            self.details.connection = if is_server_unavailable(&detail) {
                Connection::ServerUnavailable
            } else {
                Connection::Error(detail)
            };
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeSet;

    #[test]
    fn the_window_closes_on_esc_d_capital_d_and_q_only() {
        for key in [
            KeyWithModifier::new(BareKey::Esc),
            KeyWithModifier::new(BareKey::Char('D')),
            KeyWithModifier::new(BareKey::Char('d')),
            KeyWithModifier::new(BareKey::Char('q')),
            KeyWithModifier::new_with_modifiers(
                BareKey::Char('q'),
                BTreeSet::from([KeyModifier::Ctrl]),
            ),
        ] {
            assert!(is_close_key(&key), "{key:?}");
        }
        for key in [
            KeyWithModifier::new(BareKey::Enter),
            KeyWithModifier::new(BareKey::Char('x')),
            KeyWithModifier::new(BareKey::Char('j')),
        ] {
            assert!(!is_close_key(&key), "{key:?}");
        }
    }
}
