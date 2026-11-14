#!/bin/sh
# homebrew-formula.sh: print the Homebrew formula for one release.
#
# usage: homebrew-formula.sh <version> <checksums.txt>
#
# <version> is the release tag (v1.2.3). <checksums.txt> is the file a
# release carries: "<sha256>  <asset name>" per line. The formula installs
# the released binary for the machine's platform; it builds nothing.
#
# The release workflow writes the output to Formula/noc.rb on main, which is
# what `brew tap ahparikh/noc https://github.com/ahparikh/noc` reads.
set -eu

[ $# -eq 2 ] || {
	sed -n '2,4p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}
tag=$1
sums=$2
repo=${NOC_REPO:-ahparikh/noc}

case "$tag" in
v[0-9]*) ;;
*)
	echo "homebrew-formula.sh: version must look like v1.2.3, got $tag" >&2
	exit 2
	;;
esac

sha() {
	value=$(awk -v name="$1" '$2 == name { print $1 }' "$sums")
	[ -n "$value" ] || {
		echo "homebrew-formula.sh: $sums has no checksum for $1" >&2
		exit 1
	}
	printf '%s' "$value"
}

base="https://github.com/$repo/releases/download/$tag"
darwin_arm64=$(sha noc-darwin-arm64)
darwin_amd64=$(sha noc-darwin-amd64)
linux_arm64=$(sha noc-linux-arm64)
linux_amd64=$(sha noc-linux-amd64)

cat <<FORMULA
# Written by packaging/homebrew-formula.sh when $tag was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/$repo"
  version "${tag#v}"

  on_macos do
    on_arm do
      url "$base/noc-darwin-arm64"
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "$base/noc-darwin-amd64"
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    on_arm do
      url "$base/noc-linux-arm64"
      sha256 "$linux_arm64"
    end
    on_intel do
      url "$base/noc-linux-amd64"
      sha256 "$linux_amd64"
    end
  end

  def install
    bin.install Dir["noc-*"].first => "noc"
  end

  def caveats
    <<~EOS
      Set noc up once:
        noc setup          # install the Claude Code and Codex hook handlers
        noc server --bg    # start the registry server
        noc doctor

      After an upgrade, restart the server:
        noc server restart

      The Zellij sidebar is set up once, in your own Zellij configuration,
      and loads its plugin by URL from the same release:
        https://github.com/$repo/releases/download/$tag/zellij-noc.wasm
      See https://github.com/$repo#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
FORMULA
