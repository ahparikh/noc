//! Noticing a newer release of the plugin.
//!
//! Zellij keeps the copy of a plugin it first downloaded for a URL, so a
//! running panel is the only thing that can say a newer release exists. It
//! asks GitHub where "latest release" redirects to and compares that tag
//! with its own version. Nothing is downloaded or changed.

use crate::{ctx, now_secs, Role, State};
use zellij_tile::prelude::*;

pub const CTX_CHECK: &str = "update-check";
/// Redirects to `…/releases/tag/<tag>` of the newest non-prerelease.
pub const LATEST_URL: &str = "https://github.com/ahparikh/noc/releases/latest";
pub const CURRENT: &str = env!("CARGO_PKG_VERSION");
pub const CHECK_INTERVAL_SECS: f64 = 6.0 * 3600.0;
pub const RETRY_INTERVAL_SECS: f64 = 600.0;

/// Prints the redirect target and nothing else; it does not follow it.
pub fn check_command() -> [&'static str; 9] {
    [
        "curl",
        "-sS",
        "-o",
        "/dev/null",
        "--max-time",
        "10",
        "-w",
        "%{redirect_url}",
        LATEST_URL,
    ]
}

/// `1.2.3` or `v1.2.3`. Anything else (a prerelease, a date) is not a
/// version this plugin compares.
pub fn parse_version(text: &str) -> Option<(u64, u64, u64)> {
    let text = text.trim();
    let mut parts = text.strip_prefix('v').unwrap_or(text).split('.');
    let mut next = || -> Option<u64> {
        let part = parts.next()?;
        if part.is_empty() || !part.bytes().all(|b| b.is_ascii_digit()) {
            return None;
        }
        part.parse().ok()
    };
    let version = (next()?, next()?, next()?);
    parts.next().is_none().then_some(version)
}

/// The release the redirect names, as `vX.Y.Z`, when it is newer than
/// `current`. `None` for the same or an older release and for anything that
/// is not a release redirect.
pub fn newer_release(redirect: &str, current: &str) -> Option<String> {
    let (_, tag) = redirect.trim().rsplit_once("/releases/tag/")?;
    let latest = parse_version(tag)?;
    let current = parse_version(current)?;
    (latest > current).then(|| format!("v{}.{}.{}", latest.0, latest.1, latest.2))
}

/// What a check the user asked for reports: `None` when it failed, else the
/// newer release, if there is one.
pub fn check_notice(found: Option<Option<&str>>) -> String {
    match found {
        None => "could not check for a newer release".to_string(),
        Some(Some(version)) => format!("{version} is available (this is v{CURRENT})"),
        Some(None) => format!("v{CURRENT} is the latest release"),
    }
}

impl State {
    pub fn check_for_update(&mut self) {
        if self.role != Role::Panel || !self.config.update_check || !self.permissions_granted {
            return;
        }
        let now = now_secs();
        if self.next_update_check.is_some_and(|t| now < t) {
            return;
        }
        self.next_update_check = Some(now + CHECK_INTERVAL_SECS);
        run_command(&check_command(), ctx(CTX_CHECK));
    }

    /// Check now, because the user asked: whatever `update_check` says and
    /// however recent the last check was, and say what was found.
    pub fn check_for_update_now(&mut self) {
        if self.role != Role::Panel || !self.permissions_granted {
            return;
        }
        self.update_asked = true;
        self.next_update_check = Some(now_secs() + CHECK_INTERVAL_SECS);
        self.app.set_notice("checking for a newer release\u{2026}");
        run_command(&check_command(), ctx(CTX_CHECK));
    }

    /// A failed check keeps what an earlier one found. It says nothing,
    /// unless the user asked for this check.
    pub fn on_update_result(&mut self, exit: Option<i32>, stdout: &[u8]) -> bool {
        let asked = std::mem::take(&mut self.update_asked);
        let redirect = std::str::from_utf8(stdout).unwrap_or("").trim();
        if exit != Some(0) || redirect.is_empty() {
            self.next_update_check = Some(now_secs() + RETRY_INTERVAL_SECS);
            if asked {
                self.app.set_notice(check_notice(None));
            }
            return asked;
        }
        let update = newer_release(redirect, CURRENT);
        let changed = self.app.update != update;
        if asked {
            self.app.set_notice(check_notice(Some(update.as_deref())));
        }
        self.app.update = update;
        changed || asked
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_release_versions_only() {
        assert_eq!(parse_version("v0.1.0"), Some((0, 1, 0)));
        assert_eq!(parse_version(" 12.0.345\n"), Some((12, 0, 345)));
        for bad in [
            "",
            "v",
            "1.2",
            "1.2.3.4",
            "v1.2.x",
            "v1.2.3-rc1",
            "1..3",
            "+1.2.3",
            "latest",
        ] {
            assert_eq!(parse_version(bad), None, "{bad:?}");
        }
    }

    #[test]
    fn only_a_newer_release_is_reported() {
        let url = |tag: &str| format!("https://github.com/ahparikh/noc/releases/tag/{tag}\n");
        assert_eq!(
            newer_release(&url("v0.2.0"), "0.1.0").as_deref(),
            Some("v0.2.0")
        );
        // Numeric, not textual, comparison.
        assert_eq!(
            newer_release(&url("v0.10.0"), "0.9.3").as_deref(),
            Some("v0.10.0")
        );
        assert_eq!(
            newer_release(&url("1.0.0"), "0.9.3").as_deref(),
            Some("v1.0.0")
        );
        assert_eq!(newer_release(&url("v0.1.0"), "0.1.0"), None);
        assert_eq!(newer_release(&url("v0.1.0"), "0.2.0"), None);
        assert_eq!(newer_release(&url("v0.2.0-rc1"), "0.1.0"), None);
        // A repository with no release redirects nowhere.
        assert_eq!(newer_release("", "0.1.0"), None);
        assert_eq!(newer_release("https://github.com/login", "0.1.0"), None);
    }

    #[test]
    fn a_check_the_user_asked_for_says_what_it_found() {
        assert_eq!(check_notice(None), "could not check for a newer release");
        assert_eq!(
            check_notice(Some(Some("v9.0.0"))),
            format!("v9.0.0 is available (this is v{CURRENT})")
        );
        assert_eq!(
            check_notice(Some(None)),
            format!("v{CURRENT} is the latest release")
        );
    }

    #[test]
    fn the_check_only_reads_the_redirect() {
        let command = check_command();
        assert_eq!(command[0], "curl");
        assert_eq!(command[8], LATEST_URL);
        assert!(!command.contains(&"-L"));
        assert!(parse_version(CURRENT).is_some());
    }
}
