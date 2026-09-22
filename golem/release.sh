#!/usr/bin/env bash
# Build and publish a Golem Go toolchain release.
#
#   golem/release.sh <tag> [--dry-run]
#
# Cross-builds the bootstrap toolchain tarballs this fork distributes — the same
# asset names componentize-go expects — and uploads them to the GitHub release
# for <tag>, creating that release if it does not exist yet.
#
# Run it from a clean checkout of <tag> on Linux or macOS. Needs bash, tar,
# bzip2, a host Go new enough to bootstrap this one (GOROOT_BOOTSTRAP, defaults
# to the `go` on PATH) and, unless --dry-run, the GitHub CLI authenticated with
# write access to this repository.
#
# See GOLEM.md for what this fork is and when to cut a release.

set -euo pipefail

TARGETS=(
	linux/amd64
	linux/arm64
	darwin/amd64
	darwin/arm64
	windows/amd64
)

usage() {
	echo "usage: golem/release.sh <tag> [--dry-run]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
TAG="$1"
shift
DRY_RUN=false
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=true ;;
	*) usage ;;
	esac
	shift
done

cd "$(dirname "$0")/.."
GOROOT_SRC="$(pwd)"
OUT="$(cd .. && pwd)"

if [ -n "$(git status --porcelain)" ]; then
	echo "error: working tree is not clean; release from a pristine checkout" >&2
	exit 1
fi
if [ "$(git describe --tags --exact-match 2>/dev/null || true)" != "$TAG" ]; then
	echo "error: HEAD is not tagged $TAG (tag it and push before releasing)" >&2
	exit 1
fi

if [ -z "${GOROOT_BOOTSTRAP:-}" ]; then
	command -v go >/dev/null || {
		echo "error: no 'go' on PATH; set GOROOT_BOOTSTRAP to a Go toolchain" >&2
		exit 1
	}
	GOROOT_BOOTSTRAP="$(go env GOROOT)"
fi
export GOROOT_BOOTSTRAP
echo "==> bootstrap toolchain: $GOROOT_BOOTSTRAP"

GO_VERSION="$(cat VERSION | head -1)"
echo "==> building $TAG (base $GO_VERSION) for: ${TARGETS[*]}"

ASSETS=()
for target in "${TARGETS[@]}"; do
	os="${target%%/*}"
	arch="${target##*/}"
	name="go-${os}-${arch}-bootstrap"
	echo "==> $name"
	# bootstrap.bash is Go's own cross-compiling bootstrap builder: it copies the
	# committed tree to ../../$name, builds it for GOOS/GOARCH and packs the
	# result as ../../$name.tbz.
	rm -rf "${OUT:?}/$name" "${OUT:?}/$name.tbz"
	(cd src && GOOS="$os" GOARCH="$arch" ./bootstrap.bash -force >/dev/null)
	rm -rf "${OUT:?}/$name"
	(cd "$OUT" && shasum -a 256 "$name.tbz" >"$name.tbz.sha256")
	ASSETS+=("$OUT/$name.tbz" "$OUT/$name.tbz.sha256")
done

echo "==> verifying $OUT/go-linux-amd64-bootstrap.tbz"
VERIFY="$(mktemp -d)"
trap 'rm -rf "$VERIFY"' EXIT
tar xf "$OUT/go-linux-amd64-bootstrap.tbz" -C "$VERIFY"
root="$VERIFY/go-linux-amd64-bootstrap"
grep -q wasiOnIdle "$root/src/runtime/lock_wasip1.go" || {
	echo "error: the wasiOnIdle patch is missing from the built toolchain" >&2
	exit 1
}
grep -q goroutineTracking "$root/src/runtime/proc.go" || {
	echo "error: the scheduler-sampling patch is missing from the built toolchain" >&2
	exit 1
}
if [ "$(uname -s)-$(uname -m)" = "Linux-x86_64" ]; then
	"$root/bin/go" version
fi
echo "    patches present"

if $DRY_RUN; then
	echo "==> dry run; built:"
	printf '    %s\n' "${ASSETS[@]}"
	exit 0
fi

command -v gh >/dev/null || {
	echo "error: the GitHub CLI (gh) is required to upload the release" >&2
	exit 1
}
if ! gh release view "$TAG" >/dev/null 2>&1; then
	echo "==> creating release $TAG"
	gh release create "$TAG" --verify-tag --title "$TAG" --notes "$(
		cat <<NOTES
Golem's Go toolchain, based on $GO_VERSION.

Patches on top of upstream:

- \`runtime.wasiOnIdle\` for wasip1, from [dicej/go](https://github.com/dicej/go) ([golang/go#76775](https://github.com/golang/go/pull/76775))
- goroutine scheduling-latency sampling disabled on wasip1, so the runtime issues no clock reads whose placement depends on execution history

The assets are bootstrap toolchain trees in the layout [componentize-go](https://github.com/bytecodealliance/componentize-go) expects. The Golem CLI downloads the one matching your platform; see [GOLEM.md](https://github.com/golemcloud/go/blob/golem-go1.27/GOLEM.md).
NOTES
	)"
fi
echo "==> uploading assets"
gh release upload "$TAG" --clobber "${ASSETS[@]}"
echo "==> done: $(gh release view "$TAG" --json url --jq .url)"
