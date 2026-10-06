#!/usr/bin/env bash
# Build release binaries for every supported platform.
#
#   scripts/release.sh 0.1.0             # build artifacts into dist/
#   scripts/release.sh 0.1.0 --publish   # build, tag, push, then upload the release
#
# Uploading needs a GitHub token with contents:write in $GH_TOKEN.

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

[ $# -ge 1 ] || {
	echo "usage: $0 <version> [--publish]" >&2
	exit 1
}

VERSION=${1#v}
TAG=v$VERSION
PUBLISH=${2:-}

case "$VERSION" in
'' | *[!0-9.]* | *.*.*.*)
	echo "version must look like 1.2.3, got: '$VERSION'" >&2
	exit 1
	;;
esac

command -v go >/dev/null 2>&1 || {
	echo "go not found" >&2
	exit 1
}

# Static binaries so the release runs on any distro without matching libc.
export CGO_ENABLED=0
export GOFLAGS=-trimpath
LDFLAGS="-s -w -X main.version=$VERSION"
# Optional: embed provider API keys so the binary works without manual config.
# Keys never touch git; they only land in the built binary via -ldflags.
#   NOVA_KIOSAI_KEY=sk-... NOVA_STEPFUN_KEY=... NOVA_CLINE_KEY=... scripts/release.sh 0.2.2
for kv in "embeddedKiosAIKey:${NOVA_KIOSAI_KEY:-}" "embeddedStepFunKey:${NOVA_STEPFUN_KEY:-}" "embeddedClineKey:${NOVA_CLINE_KEY:-}"; do
	key=${kv%%:*}
	val=${kv#*:}
	[ -n "$val" ] && LDFLAGS="$LDFLAGS -X github.com/nova-ai/nova/internal/config.$key=$val"
done

# Windows is not a target: internal/tui uses syscall.SIGWINCH and syscall.Kill,
# which only exist on Unix. Add it here once tui handles resize signals per-OS.
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

DIST=$ROOT/dist
rm -rf "$DIST"
mkdir -p "$DIST"

echo "==> building $VERSION"
for target in $TARGETS; do
	goos=${target%/*}
	goarch=${target#*/}
	name="nova_${TAG}_${goos}_${goarch}"
	out="$DIST/$name"

	printf '    %-30s' "$name"
	GOOS=$goos GOARCH=$goarch go build -ldflags "$LDFLAGS" -o "$out" ./cmd/nova
	echo "ok ($(du -h "$out" | cut -f1))"
done

echo "==> packaging"
stage=$DIST/.stage
mkdir -p "$stage"
for target in $TARGETS; do
	goos=${target%/*}
	goarch=${target#*/}
	name="nova_${TAG}_${goos}_${goarch}"

	# The binary ships as plain "nova" inside the archive, whatever the
	# versioned asset name is, so the installer and manual downloads agree.
	cp "$DIST/$name" "$stage/nova"
	cp "$ROOT/README.md" "$stage/README.md"
	tar -czf "$DIST/$name.tar.gz" -C "$stage" nova README.md
done
rm -rf "$stage"

# Keep only the archives in dist/ so it matches the release assets.
find "$DIST" -maxdepth 1 -type f ! -name '*.tar.gz' -delete

# Plain filenames, no ./ prefix: the installer matches these lines verbatim.
(cd "$DIST" && if command -v shasum >/dev/null 2>&1; then
	shasum -a 256 *.tar.gz >checksums.txt
else
	sha256sum *.tar.gz >checksums.txt
fi)

echo
echo "==> artifacts in dist/"
ls -lh "$DIST" | tail -n +2 | awk '{printf "    %-8s %s\n", $5, $9}'
echo

[ "$PUBLISH" = "--publish" ] || {
	echo "build only; run with --publish to tag, push and upload"
	exit 0
}

: "${GH_TOKEN:?set GH_TOKEN to a GitHub token with contents:write}"

echo "==> committing release files"
git add install.sh scripts README.md
if git diff --cached --quiet; then
	echo "    nothing to commit"
else
	git commit -m "release: $TAG installer and build scripts"
fi

echo "==> tagging $TAG"
if git rev-parse "$TAG" >/dev/null 2>&1; then
	echo "    $TAG already exists, reusing it"
else
	git tag -a "$TAG" -m "$TAG"
fi

echo "==> pushing to origin"
git push origin HEAD:main
git push origin "$TAG"

echo "==> uploading release assets"
python3 "$ROOT/scripts/publish_release.py" "$VERSION"
