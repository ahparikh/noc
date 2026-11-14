#!/usr/bin/env bash
# install-config.sh: install noc's Zellij files, or bring them up to date.
#
# Run it once to set a machine up, and again whenever you want the latest
# plugin and layouts. It does not touch your config.kdl.
#
#   curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash -s -- --check
#
# From a checkout: zellij/install-config.sh [--check], or make zellij-install-config.
# --check says what would change and changes nothing.
#
# Where the files come from:
#   from a pipe       the latest release of the repository in NOC_REPO, so the
#                     layouts name a plugin that is published. NOC_REF picks
#                     another tag or branch.
#   from a checkout   the files beside the script (zellij/config/).
#
# What it does in $ZELLIJ_CONFIG_DIR, or ~/.config/zellij:
#
#   layouts/noc-host.kdl    The outer session and the sidebar plugin with its
#                           settings. Installed new. On an upgrade the new
#                           file is installed with your settings carried
#                           over, and your session names. A setting is yours
#                           when it differs from what the release you had
#                           shipped, so new defaults reach you and your own
#                           choices stay, including defaults you turned off.
#   layouts/noc-inner.kdl   What your session has only beside the sidebar.
#                           Installed new. On an upgrade, replaced if you
#                           have not changed it; if you have, yours is kept,
#                           only its plugin location is updated, and the new
#                           file is left beside it as noc-inner.kdl.new.
#   themes/noc-blue.kdl     The blue theme and its dimmed twin.
#
# Whatever it replaces is kept beside the new file as <name>.bak.
set -euo pipefail

FILES="layouts/noc-host.kdl layouts/noc-inner.kdl themes/noc-blue.kdl"
# The settings of the sidebar plugin that are carried over on an upgrade.
SETTINGS="noc_bin guest_session icons colors pane_titles poll_interval_ms update_check home_theme away_theme away_key home_key transport pipe_name"
DEST="${ZELLIJ_CONFIG_DIR:-$HOME/.config/zellij}"
REPO="${NOC_REPO:-ahparikh/noc}"
CHECK=0

case "${1:-}" in
"") ;;
--check) CHECK=1 ;;
*)
	echo "usage: install-config.sh [--check]" >&2
	exit 2
	;;
esac

say() { printf '%s\n' "$*"; }
die() {
	printf 'install-config.sh: %s\n' "$*" >&2
	exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ---- where the files come from ----

SELF="${BASH_SOURCE[0]:-}"
if [ -n "$SELF" ] && [ -f "$SELF" ] && [ -f "$(dirname "$SELF")/config/layouts/noc-host.kdl" ]; then
	SRC="$(cd -P "$(dirname "$SELF")" && pwd)/config"
	say "from       $SRC"
else
	command -v curl >/dev/null 2>&1 || die "curl is needed to download the files"
	if [ -n "${NOC_CONFIG_URL:-}" ]; then
		base="$NOC_CONFIG_URL"
	else
		ref="${NOC_REF:-}"
		if [ -z "$ref" ]; then
			# GitHub redirects "latest" to the newest release's tag.
			latest="$(curl -fsS -o /dev/null --max-time 15 -w '%{redirect_url}' "https://github.com/$REPO/releases/latest" || true)"
			ref="${latest##*/releases/tag/}"
			[ -n "$latest" ] && [ "$ref" != "$latest" ] || die "could not find the latest release of $REPO (set NOC_REF to a tag or branch)"
		fi
		base="https://raw.githubusercontent.com/$REPO/$ref/zellij/config"
	fi
	SRC="$WORK/src"
	for file in $FILES; do
		mkdir -p "$SRC/$(dirname "$file")"
		curl -fsSL "$base/$file" -o "$SRC/$file" || die "could not download $base/$file"
	done
	say "from       $base"
fi

# The plugin location the new layouts load, and its version.
LOCATION="$(grep -o '"[^"]*zellij-noc\.wasm"' "$SRC/layouts/noc-host.kdl" | head -1 || true)"
[ -n "$LOCATION" ] || die "the host layout does not load zellij-noc.wasm"
VERSION="$(printf '%s' "$LOCATION" | sed -n 's|.*/download/\([^/]*\)/zellij-noc\.wasm"|\1|p')"

# ---- helpers ----

# plugin_location <file>: the first plugin location naming zellij-noc.wasm.
plugin_location() {
	grep -o '"[^"]*zellij-noc\.wasm"' "$1" 2>/dev/null | head -1 || true
}

# without_location <file>: the file with every such location blanked, to
# compare two files apart from the plugin version.
without_location() {
	sed 's|"[^"]*zellij-noc\.wasm"|"PLUGIN"|g' "$1"
}

# user_settings <noc-host.kdl>: "key<TAB>value" for each setting that is set
# (not commented out) in the sidebar plugin's block.
user_settings() {
	awk -v keys="$SETTINGS" '
		BEGIN { n = split(keys, list, " "); for (i = 1; i <= n; i++) known[list[i]] = 1 }
		/plugin[ \t]+location="[^"]*zellij-noc\.wasm"[ \t]*\{[ \t]*$/ { inside = 1; next }
		inside && /^[ \t]*\}/ { exit }
		inside && /^[ \t]*[a-z_]+[ \t]+"[^"]*"[ \t]*$/ {
			key = $0; sub(/^[ \t]*/, "", key); sub(/[ \t].*$/, "", key)
			value = $0; sub(/^[ \t]*[a-z_]+[ \t]+"/, "", value); sub(/"[ \t]*$/, "", value)
			if (key in known) printf "%s\t%s\n", key, value
		}
	' "$1"
}

# only_changed <yours> <shipped>: the lines of <yours> ("key<TAB>value") that
# are not in <shipped>: the settings you set differently, or added.
only_changed() {
	awk 'NR == FNR { shipped[$0] = 1; next } !($0 in shipped)' "$2" "$1"
}

# turned_off <yours> <shipped>: the keys <shipped> sets and <yours> does not.
turned_off() {
	awk -F '\t' 'NR == FNR { yours[$1] = 1; next } !($1 in yours) { print $1 }' "$1" "$2"
}

# with_settings <new noc-host.kdl> <settings file> <changed file> <off file>:
# the new layout with each of those settings set to the given value: in
# place where the layout sets it, uncommented where the layout only mentions
# it, and added at the end of the plugin block otherwise; and with each
# setting named in <off file> commented out where the layout sets it. The
# names of the settings this changed go to <changed file>.
with_settings() {
	awk -v settings="$2" -v changed="$3" -v offs="$4" '
		BEGIN {
			while ((getline line < settings) > 0) {
				tab = index(line, "\t")
				value[substr(line, 1, tab - 1)] = substr(line, tab + 1)
			}
			while ((getline line < offs) > 0) off[line] = 1
			printf "" > changed
		}
		function note(key) { print key >> changed }
		/plugin[ \t]+location="[^"]*zellij-noc\.wasm"[ \t]*\{[ \t]*$/ { inside = 1; print; next }
		inside && /^[ \t]*\}/ {
			indent = $0; sub(/\}.*$/, "", indent)
			for (key in value) if (!(key in done)) {
				printf "%s    %s \"%s\"\n", indent, key, value[key]
				note(key)
			}
			inside = 0; print; next
		}
		inside && /^[ \t]*(\/\/[ \t]*)?[a-z_]+[ \t]+"[^"]*"[ \t]*$/ {
			indent = $0; sub(/[^ \t].*$/, "", indent)
			rest = $0; sub(/^[ \t]*/, "", rest)
			commented = (rest ~ /^\/\//)
			sub(/^\/\/[ \t]*/, "", rest)
			key = rest; sub(/[ \t].*$/, "", key)
			old = rest; sub(/^[a-z_]+[ \t]+"/, "", old); sub(/"[ \t]*$/, "", old)
			if ((key in value) && !(key in done)) {
				printf "%s%s \"%s\"\n", indent, key, value[key]
				done[key] = 1
				if (commented || old != value[key]) note(key)
				next
			}
			if ((key in off) && !commented) {
				printf "%s// %s \"%s\"\n", indent, key, old
				note(key " (off)")
				next
			}
		}
		{ print }
	' "$1"
}

# install <prepared file> <destination> <note>: put the file in place,
# keeping what it replaces as .bak, and say what happened.
install() {
	local from="$1" to="$2" note="${3:-}"
	if [ -f "$to" ] && cmp -s "$from" "$to"; then
		say "unchanged  $to"
		return 0
	fi
	if [ -e "$to" ]; then
		say "update     $to$note (previous kept as ${to##*/}.bak)"
		[ "$CHECK" = 1 ] || cp -p "$to" "$to.bak"
	else
		say "add        $to"
	fi
	[ "$CHECK" = 1 ] || {
		mkdir -p "$(dirname "$to")"
		cp "$from" "$to"
	}
}

# ---- the host layout: the new file, with your settings ----

host_new="$SRC/layouts/noc-host.kdl"
host_old="$DEST/layouts/noc-host.kdl"
host_out="$WORK/noc-host.kdl"
host_note=""
if [ -f "$host_old" ] && [ -n "$(plugin_location "$host_old")" ]; then
	user_settings "$host_old" >"$WORK/yours"
	cp "$WORK/yours" "$WORK/settings"
	: >"$WORK/off"
	# What the release you have shipped tells your choices from its defaults.
	# Without it, every setting you have set counts as yours.
	had="$(plugin_location "$host_old" | sed -n 's|.*/download/\([^/]*\)/zellij-noc\.wasm"|\1|p')"
	shipped="${NOC_SHIPPED_HOST:-}"
	if [ -z "$shipped" ] && [ -n "$had" ] && command -v curl >/dev/null 2>&1; then
		shipped="$WORK/shipped-host.kdl"
		curl -fsSL --max-time 15 "https://raw.githubusercontent.com/$REPO/$had/zellij/config/layouts/noc-host.kdl" \
			-o "$shipped" 2>/dev/null || shipped=""
	fi
	if [ -n "$shipped" ] && [ -f "$shipped" ] && [ -n "$(plugin_location "$shipped")" ]; then
		user_settings "$shipped" >"$WORK/shipped"
		only_changed "$WORK/yours" "$WORK/shipped" >"$WORK/settings"
		turned_off "$WORK/yours" "$WORK/shipped" >"$WORK/off"
	fi
	with_settings "$host_new" "$WORK/settings" "$WORK/changed" "$WORK/off" >"$host_out"
	# Session names you changed in the guest pane's loop.
	names="$(grep -o 'inner=[^;"]*; host=[^;"]*;' "$host_old" | head -1 || true)"
	if [ -n "$names" ] && [ "$names" != "inner=noc; host=noc-host;" ]; then
		sed "s|inner=noc; host=noc-host;|$names|" "$host_out" >"$host_out.tmp" && mv "$host_out.tmp" "$host_out"
		echo "session names" >>"$WORK/changed"
	fi
	if [ -s "$WORK/changed" ]; then
		host_note=", keeping your $(sort "$WORK/changed" | paste -s -d , - | sed 's/,/, /g')"
	fi
else
	cp "$host_new" "$host_out"
fi
install "$host_out" "$host_old" "$host_note"

# ---- the inner layout: replaced unless you changed it ----

inner_new="$SRC/layouts/noc-inner.kdl"
inner_old="$DEST/layouts/noc-inner.kdl"
if [ -f "$inner_old" ] && ! cmp -s "$inner_new" "$inner_old" &&
	[ "$(without_location "$inner_old")" != "$(without_location "$inner_new")" ]; then
	# Yours differs in more than the plugin version: keep it, and only point
	# its bindings at the new plugin.
	sed "s|\"[^\"]*zellij-noc\\.wasm\"|$LOCATION|g" "$inner_old" >"$WORK/noc-inner.kdl"
	if cmp -s "$WORK/noc-inner.kdl" "$inner_old"; then
		say "kept       $inner_old (yours differs from the new file)"
	else
		say "update     $inner_old: plugin location only, the rest is yours (previous kept as noc-inner.kdl.bak)"
		[ "$CHECK" = 1 ] || {
			cp -p "$inner_old" "$inner_old.bak"
			cp "$WORK/noc-inner.kdl" "$inner_old"
		}
	fi
	say "           the new file is $inner_old.new, to compare with yours"
	[ "$CHECK" = 1 ] || cp "$inner_new" "$inner_old.new"
else
	install "$inner_new" "$inner_old"
fi

# ---- the theme ----

install "$SRC/themes/noc-blue.kdl" "$DEST/themes/noc-blue.kdl"

# ---- what next ----

if [ "$CHECK" = 1 ]; then
	say "(--check: nothing was changed)"
	exit 0
fi
say ""
say "The sidebar plugin is ${VERSION:-the one named in noc-host.kdl}. Its settings are in"
say "$DEST/layouts/noc-host.kdl."
say "Running sessions keep the old plugin and layouts. To pick these up, end the"
say "pair and start it again. Zellij asks for the plugin's permissions once when"
say "its location has changed: move to the sidebar with Alt m and answer y."
say "    zellij kill-session noc"
say "    zellij attach --create noc-host options --default-layout noc-host"
