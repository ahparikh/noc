#!/usr/bin/env bash
# install-config.sh: copy noc's Zellij files into your Zellij configuration
# directory. You run it once per machine, then keep the result with the rest
# of your configuration. It does not touch your config.kdl.
#
# From GitHub, with nothing checked out:
#
#   curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/zellij/install-config.sh | bash -s -- --check
#
# From a checkout: zellij/install-config.sh [--check], or make zellij-install-config.
# --check says what would change and changes nothing.
#
# Run from a checkout it installs the files beside it (zellij/config/). Run
# from a pipe it downloads them from GitHub first: from the branch or tag in
# NOC_REF (default "main") of the repository in NOC_REPO.
#
# The directory written to is $ZELLIJ_CONFIG_DIR, or ~/.config/zellij:
#
#   layouts/noc-host.kdl    the outer session: the sidebar plugin with all of
#                           its settings, and the pane holding your session
#   layouts/noc-inner.kdl   options and key bindings your session has only
#                           while it runs beside the sidebar
#   themes/noc-blue.kdl     a blue theme and its dimmed twin; nothing uses
#                           them until you name them
#
# A different file already there is kept beside the new one as <name>.bak.
set -euo pipefail

FILES="layouts/noc-host.kdl layouts/noc-inner.kdl themes/noc-blue.kdl"
DEST="${ZELLIJ_CONFIG_DIR:-$HOME/.config/zellij}"
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

# Where the files come from: beside this script, or GitHub when there is no
# script file (it is being read from a pipe).
SELF="${BASH_SOURCE[0]:-}"
if [ -n "$SELF" ] && [ -f "$SELF" ] && [ -f "$(dirname "$SELF")/config/layouts/noc-host.kdl" ]; then
	SRC="$(cd -P "$(dirname "$SELF")" && pwd)/config"
else
	command -v curl >/dev/null 2>&1 || die "curl is needed to download the files"
	base="${NOC_CONFIG_URL:-https://raw.githubusercontent.com/${NOC_REPO:-ahparikh/noc}/${NOC_REF:-main}/zellij/config}"
	SRC="$(mktemp -d)"
	trap 'rm -rf "$SRC"' EXIT
	for file in $FILES; do
		mkdir -p "$SRC/$(dirname "$file")"
		curl -fsSL "$base/$file" -o "$SRC/$file" || die "could not download $base/$file"
	done
	say "from       $base"
fi

for file in $FILES; do
	from="$SRC/$file"
	to="$DEST/$file"
	if [ -f "$to" ] && cmp -s "$from" "$to"; then
		say "unchanged  $to"
		continue
	fi
	if [ -e "$to" ]; then
		say "replace    $to (previous kept as ${to##*/}.bak)"
		[ "$CHECK" = 1 ] || cp -p "$to" "$to.bak"
	else
		say "add        $to"
	fi
	[ "$CHECK" = 1 ] || {
		mkdir -p "$(dirname "$to")"
		cp "$from" "$to"
	}
done

if [ "$CHECK" = 1 ]; then
	say "(--check: nothing was changed)"
else
	say ""
	say "The sidebar's settings are in $DEST/layouts/noc-host.kdl."
	say "Start the pair, or attach to it, from a plain terminal (make it an alias):"
	say "    zellij attach --create noc-host options --default-layout noc-host"
	say "To end both sessions:"
	say "    zellij kill-session noc"
fi
