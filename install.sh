#!/bin/sh
# Install the noc binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/ahparikh/noc/main/install.sh | sh
#
# Run it again to upgrade. Environment:
#   NOC_VERSION      release tag to install, e.g. v0.1.0 (default: the latest release)
#   NOC_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   NOC_REPO         GitHub repository (default: ahparikh/noc)
#   NOC_BASE_URL     download from this URL instead of GitHub (a mirror, or a
#                    local `make dist` directory served over HTTP)
#
# The Zellij plugin needs no install step: Zellij loads it from the release
# URL named in your config.kdl. See docs/ZELLIJ.md.
set -eu

repo="${NOC_REPO:-ahparikh/noc}"
version="${NOC_VERSION:-latest}"
dir="${NOC_INSTALL_DIR:-$HOME/.local/bin}"

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) die "unsupported architecture: $(uname -m)" ;;
esac
asset="noc-$os-$arch"

if [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi

base="${NOC_BASE_URL:-$base}"

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{ print $1 }'; }
elif command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{ print $1 }'; }
else
	die "shasum or sha256sum is required to verify the download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $asset ($version) from $base"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "could not download $base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "could not download $base/checksums.txt"

want="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $asset"
got="$(sha256 "$tmp/$asset")"
[ "$want" = "$got" ] || die "checksum mismatch for $asset (expected $want, got $got)"

mkdir -p "$dir"
chmod 0755 "$tmp/$asset"
# Rename within the target directory, so a running noc is replaced in one step.
mv "$tmp/$asset" "$dir/.noc.new"
mv "$dir/.noc.new" "$dir/noc"

installed="$("$dir/noc" version 2>/dev/null || echo unknown)"
echo "Installed $dir/noc ($installed)"

on_path="$(command -v noc 2>/dev/null || true)"
if [ -z "$on_path" ]; then
	echo "Note: $dir is not on your PATH. Add it, or call $dir/noc directly."
elif [ "$on_path" != "$dir/noc" ]; then
	echo "Note: another noc comes first on your PATH: $on_path"
fi

cat <<EOF

Next:
  noc setup            install the Claude Code and Codex hook handlers
  noc server restart   (or: noc server --bg) run the registry server on this build
  noc doctor           check the installation
EOF
