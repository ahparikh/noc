#!/usr/bin/env bash
# End-to-end check of the nested pair, headless.
#
# Starts `dev.sh --fake` (this checkout's plugin, the fake noc) under its own
# session names inside a detached tmux, drives it with real key presses, and
# checks what Zellij reports. Needs zellij, tmux and jq, and a built plugin
# (`make zellij`). It never touches ~/.config/zellij or the daily sessions.
#
# The plugin's permissions must already be granted for the build path: run
# `make dev` once and answer the prompt in the sidebar. The script fails with
# a clear message when they are not.
#
# usage: nested.sh           run every check
#        nested.sh --keep    leave the pair running afterwards (tmux attach -t noc-e2e-tmux)
set -uo pipefail

ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export NOC_DEV_NAME="${NOC_DEV_NAME:-noc-e2e}"
INNER="$NOC_DEV_NAME"
HOST="$INNER-host"
TMUX_SESSION="$INNER-tmux"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

for tool in zellij tmux jq; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "nested.sh: $tool is required" >&2
		exit 2
	}
done

z() { env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID zellij "$@"; }
keys() { tmux send-keys -t "$TMUX_SESSION" "$@"; }
screen() { tmux capture-pane -p -t "$TMUX_SESSION"; }

alive() {
	{ z list-sessions --no-formatting 2>/dev/null || true; } |
		awk -v n="$1" '$1 == n && $0 !~ /EXITED/ { found = 1 } END { exit !found }'
}

# wait_for SECONDS COMMAND...: poll until the command succeeds.
wait_for() {
	local limit="$1" i
	shift
	for ((i = 0; i < limit * 4; i++)); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.25
	done
	return 1
}

panes() { z --session "$1" action list-panes --all --json 2>/dev/null; }
clients() { z --session "$1" action list-clients 2>/dev/null | tail -n +2 | grep -c . || true; }

# The pane holding the host session's keyboard: "P<id>" or "T<id>".
host_focus() {
	panes "$HOST" | jq -r '
		(map(select(.is_focused and (.is_suppressed | not) and .is_floating)) | first) //
		(map(select(.is_focused and (.is_suppressed | not))) | first) //
		empty | (if .is_plugin then "P" else "T" end) + (.id | tostring)'
}
host_focus_is() { [ "$(host_focus)" = "$1" ]; }
host_focus_kind() { case "$(host_focus)" in "$1"*) return 0 ;; *) return 1 ;; esac }

inner_tab_count() { z --session "$INNER" action list-tabs 2>/dev/null | tail -n +2 | grep -c . || true; }
inner_has_tab() { z --session "$INNER" action list-tabs 2>/dev/null | grep -q "$1"; }

# The most common background colour of the inner session's tab bar (row 1).
bar_colour() {
	tmux capture-pane -p -e -t "$TMUX_SESSION" | sed -n 1p |
		LC_ALL=C grep -o -E '48;2;[0-9]+;[0-9]+;[0-9]+' | sort | uniq -c | sort -rn | awk 'NR == 1 { print $2 }'
}

FAILED=0
ok() { printf 'ok    %s\n' "$1"; }
fail() {
	printf 'FAIL  %s\n' "$1"
	FAILED=$((FAILED + 1))
}
check() {
	local name="$1"
	shift
	if "$@"; then ok "$name"; else fail "$name"; fi
}

cleanup() {
	if [ "$KEEP" = 0 ]; then
		tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
		"$ROOT/dev.sh" stop
	fi
}
trap cleanup EXIT

# Zellij 0.46 opens a welcome screen in new sessions until it is told not
# to show it again. Esc dismisses it, exactly as pressing Esc yourself would.
dismiss_welcome() {
	local i
	for i in 1 2 3 4; do
		if z --session "$HOST" action list-clients 2>/dev/null | grep -q 'zellij:configuration' ||
			z --session "$INNER" action list-clients 2>/dev/null | grep -q 'zellij:configuration'; then
			keys Escape
			sleep 1.5
		else
			return 0
		fi
	done
}

start_pair() {
	tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
	"$ROOT/dev.sh" stop
	tmux new-session -d -s "$TMUX_SESSION" -x 170 -y 45
	keys "env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID NOC_DEV_NAME=$INNER '$ROOT/dev.sh' --fake start" Enter
}

# ---- start ----
"$ROOT/dev.sh" --fake render >/dev/null || exit 2
start_pair
if ! wait_for 30 alive "$HOST" || ! wait_for 30 alive "$INNER"; then
	echo "nested.sh: the pair did not start; last screen:" >&2
	screen | tail -n 15 >&2
	exit 1
fi
sleep 2
dismiss_welcome

if screen | grep -q 'asks permission to'; then
	echo "nested.sh: the plugin is waiting for its permissions." >&2
	echo "Run 'make dev' once, answer the prompt in the sidebar with y, then run this again." >&2
	exit 1
fi

SIDEBAR="P$(panes "$HOST" | jq -r 'map(select(.is_plugin and (.is_floating | not) and (.title | test("(^|[ ─])NOC$")))) | first | .id')"
GUEST="T$(panes "$HOST" | jq -r 'map(select((.is_plugin | not) and (.is_floating | not))) | first | .id')"

# ---- 1. the pair ----
check "host session has the sidebar and one guest pane" \
	[ "$SIDEBAR" != "Pnull" -a "$GUEST" != "Tnull" ]
check "sidebar lists the registry's agents" \
	wait_for 10 sh -c "tmux capture-pane -p -t '$TMUX_SESSION' | grep -q '2 agents'"
check "guest pane keeps exactly one client on the inner session" \
	[ "$(clients "$INNER")" = 1 ]
check "the keyboard starts in the inner session" host_focus_is "$GUEST"
# noc-inner.kdl turns the session name and the sessions box off; config.kdl
# leaves both on.
check "inner session takes its options from noc-inner.kdl" \
	sh -c "tmux capture-pane -p -t '$TMUX_SESSION' | sed -n 1p | grep 'Zellij ' | grep -v -e '($INNER)' -e 'running' >/dev/null"

# ---- 2. keyboard hand-off and the theme relay ----
HOME_COLOUR="$(bar_colour)"
keys M-m
check "Alt-m gives the keyboard to the sidebar" wait_for 5 host_focus_is "$SIDEBAR"
sleep 1.5
AWAY_COLOUR="$(bar_colour)"
check "inner session wears the away theme while the sidebar has the keyboard" \
	[ -n "$AWAY_COLOUR" -a "$AWAY_COLOUR" != "$HOME_COLOUR" ]
keys M-m
check "Alt-m gives the keyboard back" wait_for 5 host_focus_is "$GUEST"
sleep 1.5
check "inner session returns to its home theme" [ "$(bar_colour)" = "$HOME_COLOUR" ]

# ---- 3. opening an agent in the inner session ----
TABS_BEFORE="$(inner_tab_count)"
keys M-m
wait_for 5 host_focus_is "$SIDEBAR"
keys Enter # open the Quiet section
sleep 0.7
keys j # first agent
sleep 0.3
keys Enter
check "Enter opens the agent in a new inner tab named after it" wait_for 10 inner_has_tab 'E2E agent'
check "the new tab runs noc attach for that agent" \
	wait_for 5 sh -c "env -u ZELLIJ -u ZELLIJ_SESSION_NAME zellij --session '$INNER' action list-panes --all --json | jq -e 'any(.[]; (.terminal_command // \"\") | test(\"attach agent-e2e\"))'"
check "the keyboard follows into the inner session" wait_for 5 host_focus_is "$GUEST"
TABS_OPEN="$(inner_tab_count)"
check "exactly one tab was added" [ "$TABS_OPEN" = "$((TABS_BEFORE + 1))" ]

sleep 1.5 # let the sidebar follow the inner focus onto that agent
keys M-m
wait_for 5 host_focus_is "$SIDEBAR"
keys Enter
wait_for 5 host_focus_is "$GUEST"
sleep 1
check "Enter on an agent that is already open focuses it instead of opening a copy" \
	[ "$(inner_tab_count)" = "$TABS_OPEN" ]

# The registry gives the agent another name: its tab follows, both ways.
RENAMED="$ROOT/target/e2e/agents.json"
mkdir -p "$ROOT/target/e2e"
"$ROOT/e2e/fake-noc" ls | sed 's/"name":"E2E agent"/"name":"Renamed agent"/' >"$RENAMED.tmp"
mv "$RENAMED.tmp" "$RENAMED"
check "the tab is renamed when the agent's name changes" wait_for 10 inner_has_tab 'Renamed agent'
rm -f "$RENAMED"
wait_for 10 inner_has_tab 'E2E agent' || true

# ---- 4. details window ----
keys M-m
wait_for 5 host_focus_is "$SIDEBAR"
keys D
check "D opens the floating details window" \
	wait_for 8 sh -c "env -u ZELLIJ -u ZELLIJ_SESSION_NAME zellij --session '$HOST' action list-panes --all --json | jq -e 'any(.[]; .is_plugin and .is_floating and (.title | test(\"E2E agent\")))'"
keys q
check "q closes it" \
	wait_for 5 sh -c "env -u ZELLIJ -u ZELLIJ_SESSION_NAME zellij --session '$HOST' action list-panes --all --json | jq -e 'any(.[]; .is_plugin and .is_floating and (.is_suppressed | not)) | not'"

# ---- 5. search ----
# Closing the details window leaves the keyboard in the inner session.
wait_for 5 host_focus_is "$GUEST"
keys M-m
wait_for 5 host_focus_is "$SIDEBAR"
keys /
sleep 0.3
keys second
# While it is typed the status line is the prompt; Enter keeps the search and
# brings the count back.
keys Enter
check "/ lists the agents that match what is typed" \
	wait_for 8 sh -c "tmux capture-pane -p -t '$TMUX_SESSION' | grep -q '1 match'"
keys Escape
check "Esc ends the search" \
	wait_for 8 sh -c "tmux capture-pane -p -t '$TMUX_SESSION' | grep -q '2 agents'"
keys M-m
wait_for 5 host_focus_is "$GUEST"

# ---- 6. hide and show the sidebar ----
fullscreen() { z --session "$HOST" action list-tabs --all --json 2>/dev/null | jq -e "any(.[]; .is_fullscreen_active == $1)" >/dev/null; }
keys M-o
check "Alt-o hides the sidebar" wait_for 5 fullscreen true
keys M-o
check "Alt-o shows it again" wait_for 5 fullscreen false

# ---- 7. detach, attach, end together ----
keys M-d
check "Alt-d detaches the host client" wait_for 5 sh -c "[ \"\$(env -u ZELLIJ -u ZELLIJ_SESSION_NAME zellij --session '$HOST' action list-clients | tail -n +2 | grep -c .)\" = 0 ]"
check "both sessions keep running" sh -c "$(declare -f z alive); alive '$HOST' && alive '$INNER'"
check "the inner client stays attached in the guest pane" [ "$(clients "$INNER")" = 1 ]
keys "env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID NOC_DEV_NAME=$INNER '$ROOT/dev.sh' --fake start" Enter
check "starting again attaches to the running pair" wait_for 10 sh -c "[ \"\$(env -u ZELLIJ -u ZELLIJ_SESSION_NAME zellij --session '$HOST' action list-clients | tail -n +2 | grep -c .)\" = 1 ]"

if [ "$KEEP" = 0 ]; then
	z kill-session "$INNER" >/dev/null 2>&1
	host_gone() { ! alive "$HOST"; }
	check "ending the inner session ends the host session" wait_for 10 host_gone
fi

echo
if [ "$FAILED" = 0 ]; then
	echo "nested: all checks passed"
else
	echo "nested: $FAILED check(s) failed"
	exit 1
fi
