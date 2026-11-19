#!/usr/bin/env bash
# beta.sh: publish this checkout's Zellij frontend as the beta.
#
# usage: packaging/beta.sh
#
# A beta is the plugin and the layouts, not the noc binary: the binary a
# beta runs with is the released one. Publishing replaces what is at one
# fixed place, so the layouts of a beta never change their plugin location:
#
#   release   the pre-release "beta-latest" of the repository, holding
#             zellij-noc.wasm built from this checkout
#   tag       beta-latest, on a commit that is this checkout's HEAD with the
#             two layouts naming that release's plugin instead of a
#             version's. The commit is on no branch.
#
# To install it, run zellij/install-config.sh from that tag with
# NOC_REF=beta-latest; see docs/DEVELOPMENT.md.
#
# The tag is not "v…", so the release workflow does not run and the
# Homebrew formula is not touched; a pre-release is not "latest", so
# nobody on a release is told to upgrade. Needs gh, logged in, and a
# checkout with nothing uncommitted.
set -euo pipefail

TAG=beta-latest
ROOT="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${NOC_REPO:-ahparikh/noc}"
URL="https://github.com/$REPO/releases/download/$TAG/zellij-noc.wasm"
WASM="$ROOT/zellij/target/wasm32-wasip1/release/zellij-noc.wasm"
LAYOUTS="zellij/config/layouts/noc-host.kdl zellij/config/layouts/noc-inner.kdl"

die() {
	printf 'beta.sh: %s\n' "$*" >&2
	exit 1
}

cd "$ROOT"
command -v gh >/dev/null 2>&1 || die "gh is needed to publish"
git diff --quiet && git diff --cached --quiet || die "commit your changes first: the beta is built from HEAD"

make zellij-check
make zellij
[ -f "$WASM" ] || die "missing $WASM"

# The commit the tag names: HEAD's tree with the layouts loading the beta
# plugin. It is built in a scratch index, so the checkout is not touched.
index="$(mktemp)"
trap 'rm -f "$index"' EXIT
export GIT_INDEX_FILE="$index"
git read-tree HEAD
for file in $LAYOUTS; do
	grep -q 'zellij-noc\.wasm"' "$file" || die "$file does not load zellij-noc.wasm"
	blob="$(sed "s|\"[^\"]*zellij-noc\\.wasm\"|\"$URL\"|g" "$file" | git hash-object -w --stdin)"
	git update-index --cacheinfo "100644,$blob,$file"
done
tree="$(git write-tree)"
unset GIT_INDEX_FILE
head="$(git rev-parse --short HEAD)"
commit="$(git commit-tree "$tree" -p HEAD -m "beta: the layouts load the $TAG plugin (from $head)")"

# Replace the release and its tag. The tag is removed and made again, not
# forced: nothing but this script moves it.
gh release delete "$TAG" --repo "$REPO" --cleanup-tag --yes >/dev/null 2>&1 || true
git push origin ":refs/tags/$TAG" >/dev/null 2>&1 || true
git push origin "$commit:refs/tags/$TAG"
gh release create "$TAG" "$WASM" --repo "$REPO" --prerelease --verify-tag \
	--title "beta ($head)" \
	--notes "The Zellij plugin and layouts built from $head. Replaced by every beta; install with zellij/install-config.sh from this tag (NOC_REF=$TAG)."

printf 'beta.sh: published %s from %s\n  plugin   %s\n  layouts  https://github.com/%s/tree/%s/zellij/config\n' \
	"$TAG" "$head" "$URL" "$REPO" "$TAG"
