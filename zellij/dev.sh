#!/usr/bin/env bash
# dev.sh: run a development copy of the noc pair, isolated from daily use.
#
# usage: dev.sh [--fake|--installed] [start]   start the dev pair, or attach
#        dev.sh [--fake|--installed] render    write the dev config, print its directory
#        dev.sh stop                           end both dev sessions and the dev server
#        dev.sh status                         show the dev sessions and server
#
# Daily use loads the released plugin by URL and the released `noc` binary.
# The dev pair instead uses this checkout's build of both, under different
# session names, with its own Zellij config directory and its own noc state:
#
#   sessions   $NOC_DEV_NAME (inner, default "noc-dev"), $NOC_DEV_NAME-host,
#              and one $NOC_DEV_NAME--<team> per team whose room was opened
#   plugin     zellij/target/wasm32-wasip1/release/zellij-noc.wasm
#   noc        go/bin/noc through a wrapper that points it at a separate
#              state and runtime directory (--fake: e2e/fake-noc instead)
#   registry   its own server, started by `start` and ended by `stop`. It
#              finds the Claude Code and Codex sessions that are live on this
#              machine by discovery, so the sidebar lists your real agents.
#   config     zellij/target/dev/$NOC_DEV_NAME/, written from one of two
#              sources:
#
#     (default)     this checkout's zellij/config/: the new plugin with the
#                   new configuration files, on a plain Zellij config.
#     --installed   your installed configuration ($ZELLIJ_CONFIG_DIR or
#                   ~/.config/zellij: config.kdl, layouts, themes): the new
#                   plugin with the configuration you use every day.
#
# In both, the plugin location, `noc_bin` and the session names are replaced.
# Nothing here writes to ~/.config/zellij or touches the daily noc registry.
set -euo pipefail

ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd -P "$ROOT/.." && pwd)"

FAKE=0
MODE=shipped
UPDATE_CHECK=true
case "${1:-}" in
--fake)
	FAKE=1
	shift
	;;
--installed)
	MODE=installed
	shift
	;;
esac

INNER="${NOC_DEV_NAME:-noc-dev}"
HOST="$INNER-host"
WASM="$ROOT/target/wasm32-wasip1/release/zellij-noc.wasm"
OUT="$ROOT/target/dev/$INNER"
USER_CONFIG="${ZELLIJ_CONFIG_DIR:-$HOME/.config/zellij}"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/$INNER"
RUNTIME="${XDG_CACHE_HOME:-$HOME/.cache}/$INNER"

# Never join a session this script happens to run in.
z() { env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID zellij "$@"; }

die() {
	printf 'dev.sh: %s\n' "$*" >&2
	exit 1
}

alive() {
	{ z list-sessions --no-formatting 2>/dev/null || true; } |
		awk -v n="$1" '$1 == n && $0 !~ /EXITED/ { found = 1 } END { exit !found }'
}

case "$INNER" in
*[!A-Za-z0-9._-]* | "") die "NOC_DEV_NAME must be a plain name" ;;
noc) die "NOC_DEV_NAME=noc is the daily pair; pick another name" ;;
esac

render() {
	[ -f "$WASM" ] || die "missing $WASM (run: make zellij)"
	mkdir -p "$OUT/layouts"

	local noc
	if [ "$FAKE" = 1 ]; then
		noc="$ROOT/e2e/fake-noc"
		UPDATE_CHECK=false
	else
		[ -x "$REPO/go/bin/noc" ] || die "missing $REPO/go/bin/noc (run: make build)"
		noc="$OUT/noc"
		{
			printf '#!/bin/sh\n'
			printf '# The dev noc: this checkout'"'"'s build, with its own state.\n'
			printf 'NOC_STATE_DIR=%q NOC_RUNTIME_DIR=%q exec %q "$@"\n' \
				"$STATE" "$RUNTIME" "$REPO/go/bin/noc"
		} >"$noc"
		chmod +x "$noc"
	fi

	# The links first. With --installed, `themes` is a link to the user's own
	# themes directory, and removing a file through it would remove theirs.
	find "$OUT" -maxdepth 2 -type l -delete
	rm -f "$OUT/config.kdl" "$OUT/layouts/$HOST.kdl" "$OUT/layouts/noc-inner.kdl"
	if [ -d "$OUT/themes" ] && [ ! -L "$OUT/themes" ]; then
		rm -f "$OUT/themes/noc-blue.kdl"
		rmdir "$OUT/themes" 2>/dev/null || true
	fi
	if [ "$MODE" = installed ]; then
		render_installed
	else
		render_shipped
	fi
	printf '%s\n' "$MODE" >"$OUT/mode"

	grep -q "plugin location=\"file:$WASM\"" "$OUT/layouts/$HOST.kdl" || die "could not point the plugin at $WASM"
	grep -q "noc_bin \"$noc\"" "$OUT/layouts/$HOST.kdl" || die "could not point the plugin at $noc"
	grep -q "inner=$INNER; host=$HOST;" "$OUT/layouts/$HOST.kdl" || die "could not rename the sessions"
	! grep -q 'https\{0,1\}://[^"]*zellij-noc\.wasm' "$OUT/layouts/noc-inner.kdl" ||
		die "could not point the theme bindings at $WASM"
}

# local_plugin: every plugin location that names a zellij-noc.wasm becomes
# this checkout's build.
local_plugin() {
	sed -e "s|\"[^\"]*zellij-noc\.wasm\"|\"file:$WASM\"|g"
}

# host_layout <noc-host.kdl> <noc executable>: the host layout with the dev
# session names, the inner session reading the dev config directory, and the
# plugin loading this checkout's build, running the dev noc and naming the
# dev inner session, with its rooms made from the dev copy of the inner
# layout. The four settings are replaced where they are set and added where
# they are not.
host_layout() {
	sed \
		-e "s|inner=noc; host=noc-host;|inner=$INNER; host=$HOST; export ZELLIJ_CONFIG_DIR='$OUT';|" \
		-e '/^[[:space:]]*noc_bin[[:space:]]/d' \
		-e '/^[[:space:]]*guest_session[[:space:]]/d' \
		-e '/^[[:space:]]*room_layout[[:space:]]/d' \
		-e '/^[[:space:]]*update_check[[:space:]]/d' \
		"$1" | local_plugin |
		awk -v noc="$2" -v inner="$INNER" -v check="$UPDATE_CHECK" -v layout="$OUT/layouts/noc-inner" '
			{ print }
			/^[ \t]*plugin[ \t]+location=.*zellij-noc\.wasm.*\{[ \t]*$/ {
				printf "                noc_bin \"%s\"\n", noc
				printf "                guest_session \"%s\"\n", inner
				# A client that moves to a new room looks for its layout
				# in the default config directory, not in this one.
				printf "                room_layout \"%s\"\n", layout
				printf "                update_check \"%s\"\n", check
			}'
}

# This checkout's layouts on a plain Zellij config.
render_shipped() {
	# The shipped blue theme and its dimmed twin. The headless test keeps a
	# built-in pair, whose tab bars differ, to see the theme change.
	local home=noc-blue away=noc-blue-away
	if [ "$FAKE" = 1 ]; then
		home=catppuccin-mocha away=catppuccin-macchiato
	fi
	mkdir -p "$OUT/themes"
	cp "$ROOT/config/themes/noc-blue.kdl" "$OUT/themes/noc-blue.kdl"
	{
		printf '// Written by zellij/dev.sh; do not edit.\n'
		printf 'theme "%s"\n' "$home"
		printf 'show_startup_tips false\nshow_release_notes false\nsession_serialization false\n'
		# Zellij's default draws only a title above each pane.
		printf 'pane_frames true\npane_frame_style "full"\n'
		printf 'ui { pane_frames { rounded_corners true; }; }\n'
	} >"$OUT/config.kdl"
	# Dimming with the pair above.
	host_layout "$ROOT/config/layouts/noc-host.kdl" "$noc" | sed \
		-e "s|^\\([[:space:]]*\\)home_theme \"[^\"]*\"|\\1home_theme \"$home\"|" \
		-e "s|^\\([[:space:]]*\\)away_theme \"[^\"]*\"|\\1away_theme \"$away\"|" \
		>"$OUT/layouts/$HOST.kdl"
	local_plugin <"$ROOT/config/layouts/noc-inner.kdl" | sed \
		-e "s|payload \"theme noc-blue-away\"|payload \"theme $away\"|" \
		-e "s|payload \"theme noc-blue\"|payload \"theme $home\"|" \
		>"$OUT/layouts/noc-inner.kdl"
}

# Your installed configuration: everything in it is linked into the dev
# directory, except the two noc layouts, which are copied with the plugin,
# the noc executable and the session names replaced.
render_installed() {
	local entry
	for entry in noc-host.kdl noc-inner.kdl; do
		[ -f "$USER_CONFIG/layouts/$entry" ] ||
			die "missing $USER_CONFIG/layouts/$entry (run: make zellij-install-config)"
	done
	grep -q 'plugin[[:space:]][[:space:]]*location="[^"]*zellij-noc\.wasm"' "$USER_CONFIG/layouts/noc-host.kdl" ||
		die "$USER_CONFIG/layouts/noc-host.kdl does not load zellij-noc.wasm (run: make zellij-install-config)"
	for entry in "$USER_CONFIG"/* "$USER_CONFIG"/layouts/*; do
		[ -e "$entry" ] || continue
		case "$entry" in
		"$USER_CONFIG/layouts" | "$USER_CONFIG/layouts/noc-host.kdl" | "$USER_CONFIG/layouts/noc-inner.kdl") ;;
		"$USER_CONFIG"/layouts/*) ln -s "$entry" "$OUT/layouts/${entry##*/}" ;;
		*) ln -s "$entry" "$OUT/${entry##*/}" ;;
		esac
	done
	host_layout "$USER_CONFIG/layouts/noc-host.kdl" "$noc" >"$OUT/layouts/$HOST.kdl"
	local_plugin <"$USER_CONFIG/layouts/noc-inner.kdl" >"$OUT/layouts/noc-inner.kdl"
}

# The dev registry server. A fresh start replaces a running one, so the
# server is always this checkout's current build.
start_server() {
	[ "$FAKE" = 0 ] || return 0
	"$OUT/noc" server stop >/dev/null 2>&1 || true
	"$OUT/noc" server --bg >/dev/null || die "could not start the dev server (see $RUNTIME/noc.log)"
}

cmd_start() {
	[ -z "${ZELLIJ:-}" ] || die "run this from a plain terminal, not inside Zellij"
	if alive "$HOST"; then
		local running
		running="$(cat "$OUT/mode" 2>/dev/null || echo shipped)"
		[ "$FAKE" = 1 ] || [ "$running" = "$MODE" ] ||
			die "the dev pair is running with the $running configuration; stop it first (make dev-stop)"
	else
		render
		start_server
		z delete-session --force "$HOST" >/dev/null 2>&1 || true
	fi
	# The line users run: it creates the pair or attaches to it.
	exec env -u ZELLIJ -u ZELLIJ_SESSION_NAME -u ZELLIJ_PANE_ID \
		zellij --config-dir "$OUT" attach --create "$HOST" options --default-layout "$HOST"
}

# rooms: the inner session and every team's session beside it, running or not.
rooms() {
	echo "$INNER"
	{ z list-sessions --no-formatting --short 2>/dev/null || true; } |
		awk -v p="$INNER--" 'index($1, p) == 1 && length($1) > length(p) { print $1 }'
}

cmd_stop() {
	local room
	z delete-session --force "$HOST" >/dev/null 2>&1 || true
	for room in $(rooms); do
		z delete-session --force "$room" >/dev/null 2>&1 || true
	done
	# The sidebar's registry watcher would otherwise last until the
	# registry next changes, which the fake one never does.
	pkill -f -- "zellij-noc-registry-watch=$HOST\$" 2>/dev/null || true
	if [ -x "$OUT/noc" ]; then
		"$OUT/noc" server stop >/dev/null 2>&1 || true
	fi
}

cmd_status() {
	local name
	[ ! -f "$OUT/mode" ] || echo "== configuration: $(cat "$OUT/mode")"
	for name in "$HOST" $(rooms); do
		if alive "$name"; then
			echo "== $name"
			z --session "$name" action list-clients 2>&1 || true
		else
			echo "== $name: not running"
		fi
	done
	if [ -x "$OUT/noc" ] && "$OUT/noc" server status >/dev/null 2>&1; then
		echo "== dev server: running ($("$OUT/noc" ls --json --all 2>/dev/null | grep -o '"id"' | wc -l | tr -d ' ') agents)"
	else
		echo "== dev server: not running"
	fi
}

case "${1:-start}" in
start) cmd_start ;;
render)
	render
	echo "$OUT"
	;;
stop) cmd_stop ;;
status) cmd_status ;;
*)
	sed -n '2,7p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
