#!/usr/bin/env bash
# Build tailnode for linux/darwin/windows on amd64 and arm64, then publish
# assets to a GitHub release via the gh CLI.
#
# Usage:
#   ./scripts/release.sh v0.1.0              # build + create/upload release
#   ./scripts/release.sh v0.1.0 --build-only # build only, skip gh release
#
# Requires: go, gh (authenticated), git tag matching <version> (optional but
# recommended before publishing).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${ROOT}/dist"
BINARY="tailnode"
BUILD_ONLY=false
VERSION=""

usage() {
	cat <<EOF
Usage: $(basename "$0") <version> [--build-only]

  version       Release tag, e.g. v0.1.0
  --build-only  Build archives under dist/ without creating a GitHub release

Examples:
  $(basename "$0") v0.1.0
  $(basename "$0") v0.1.0 --build-only
EOF
}

for arg in "$@"; do
	case "$arg" in
	--build-only) BUILD_ONLY=true ;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "unknown option: $arg" >&2
		usage >&2
		exit 1
		;;
	*)
		if [[ -n "$VERSION" ]]; then
			echo "unexpected argument: $arg" >&2
			usage >&2
			exit 1
		fi
		VERSION="$arg"
		;;
	esac
done

if [[ -z "$VERSION" ]]; then
	echo "error: version required (e.g. v0.1.0)" >&2
	usage >&2
	exit 1
fi

if ! command -v go >/dev/null; then
	echo "error: go not found in PATH" >&2
	exit 1
fi

if [[ "$BUILD_ONLY" == false ]] && ! command -v gh >/dev/null; then
	echo "error: gh not found in PATH (install GitHub CLI or pass --build-only)" >&2
	exit 1
fi

VERSION="${VERSION#v}"
TAG="v${VERSION}"

rm -rf "$DIST"
mkdir -p "$DIST"

build_one() {
	local goos=$1 goarch=$2
	local name="${BINARY}_${VERSION}_${goos}_${goarch}"
	local out="${DIST}/${name}"
	local archive

	echo "==> building ${goos}/${goarch}"
	if [[ "$goos" == "windows" ]]; then
		out+=".exe"
	fi

	GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath \
		-ldflags="-s -w" \
		-o "$out" \
		"${ROOT}"

	if [[ "$goos" == "windows" ]]; then
		archive="${DIST}/${name}.zip"
		(
			cd "$DIST"
			zip -q "$(basename "$archive")" "$(basename "$out")"
		)
	else
		archive="${DIST}/${name}.tar.gz"
		tar -C "$DIST" -czf "$archive" "$(basename "$out")"
	fi

	rm -f "$out"
}

build_one linux amd64
build_one linux arm64
build_one darwin amd64
build_one darwin arm64
build_one windows amd64
build_one windows arm64

(
	cd "$DIST"
	if command -v sha256sum >/dev/null; then
		sha256sum *.tar.gz *.zip > SHA256SUMS
	elif command -v shasum >/dev/null; then
		shasum -a 256 *.tar.gz *.zip > SHA256SUMS
	else
		echo "warning: no sha256sum or shasum; skipping checksums" >&2
	fi
)

echo
echo "Built artifacts in ${DIST}:"
ls -1 "$DIST"

if [[ "$BUILD_ONLY" == true ]]; then
	exit 0
fi

if ! gh auth status >/dev/null 2>&1; then
	echo "error: gh is not authenticated; run 'gh auth login'" >&2
	exit 1
fi

NOTES_FILE="$(mktemp)"
cat >"$NOTES_FILE" <<EOF
Cross-platform tailnode ${TAG} builds.

| OS | Arch | Archive |
|----|------|---------|
| Linux | amd64 | \`${BINARY}_${VERSION}_linux_amd64.tar.gz\` |
| Linux | arm64 | \`${BINARY}_${VERSION}_linux_arm64.tar.gz\` |
| macOS | amd64 | \`${BINARY}_${VERSION}_darwin_amd64.tar.gz\` |
| macOS | arm64 | \`${BINARY}_${VERSION}_darwin_arm64.tar.gz\` |
| Windows | amd64 | \`${BINARY}_${VERSION}_windows_amd64.zip\` |
| Windows | arm64 | \`${BINARY}_${VERSION}_windows_arm64.zip\` |

Verify with \`SHA256SUMS\` before use.
EOF

if gh release view "$TAG" >/dev/null 2>&1; then
	echo "==> uploading assets to existing release ${TAG}"
	gh release upload "$TAG" "$DIST"/* --clobber
else
	if git rev-parse "$TAG" >/dev/null 2>&1; then
		echo "==> creating release ${TAG} from existing tag"
		gh release create "$TAG" "$DIST"/* --title "$TAG" --notes-file "$NOTES_FILE"
	else
		echo "==> creating tag ${TAG} and release from HEAD"
		gh release create "$TAG" "$DIST"/* --title "$TAG" --notes-file "$NOTES_FILE" --target "$(git rev-parse HEAD)"
	fi
fi

rm -f "$NOTES_FILE"
echo "==> release ${TAG} published: $(gh release view "$TAG" --json url -q .url)"
