#!/bin/sh
# Package built plugin libraries into CLIProxyAPI plugin-store release assets.
#
# Usage: scripts/package-release.sh <version>        e.g. 0.1.0 (a leading "v" is stripped)
#
# Input : dist/linux/<arch>/neuralwatt.so            one directory per arch (scripts/build-linux.sh)
# Output: dist/release/neuralwatt_<version>_linux_<arch>.zip          neuralwatt.so at the zip root, mode 0755
#         dist/release/neuralwatt_<version>_linux_<arch>.zip.sha256   "<sha256>  <asset>" (sha256sum format)
#         dist/release/checksums.txt                                  all .sha256 lines, sorted
#
# The host (CLIProxyAPI v8 internal/pluginstore) looks for the release asset
# named exactly <id>_<version>_<goos>_<goarch>.zip plus checksums.txt, verifies
# the zip's sha256 against the checksums.txt line, and requires the archive to
# contain a single dynamic library named <id>.so at the zip root.
# Only tools shipped with macOS are used: zip, unzip, shasum, sort, mktemp.
set -eu

PLUGIN_ID="neuralwatt"
GOOS="linux"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/dist/release"

die() { printf 'package-release: %s\n' "$*" >&2; exit 1; }

VERSION="${1:-}"
[ -n "$VERSION" ] || die "usage: scripts/package-release.sh <version>   (e.g. 0.1.0)"
case "$VERSION" in v*|V*) VERSION="${VERSION#?}" ;; esac
# Host rule (registry.go pluginVersionPattern): ^[0-9][0-9A-Za-z.+-]*$ and no leading "v".
case "$VERSION" in
  [0-9]*) ;;
  *) die "version must start with a digit: '$VERSION'" ;;
esac
case "$VERSION" in
  *[!0-9A-Za-z.+-]*) die "version may only contain [0-9A-Za-z.+-]: '$VERSION'" ;;
esac

[ -d "$ROOT/dist/$GOOS" ] || die "missing $ROOT/dist/$GOOS (run scripts/build-linux.sh first)"

mkdir -p "$OUT"
rm -f "$OUT/${PLUGIN_ID}_${VERSION}_${GOOS}_"*.zip "$OUT/${PLUGIN_ID}_${VERSION}_${GOOS}_"*.zip.sha256 "$OUT/checksums.txt"

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/package-release.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT INT TERM

packaged=0
for dir in "$ROOT/dist/$GOOS"/*/; do
  [ -d "$dir" ] || continue
  arch="$(basename "$dir")"
  lib="$dir$PLUGIN_ID.so"
  if [ ! -f "$lib" ]; then
    printf 'package-release: skipping %s/%s (no %s)\n' "$GOOS" "$arch" "$lib" >&2
    continue
  fi
  asset="${PLUGIN_ID}_${VERSION}_${GOOS}_${arch}.zip"

  # Stage a clean copy so the zip holds exactly one root entry with a sane mode
  # and no directory prefix, resource forks, or ._* AppleDouble files.
  rm -rf "$STAGE/$arch"; mkdir -p "$STAGE/$arch"
  cp "$lib" "$STAGE/$arch/$PLUGIN_ID.so"
  chmod 0755 "$STAGE/$arch/$PLUGIN_ID.so"
  (cd "$STAGE/$arch" && zip -q -X "$OUT/$asset" "$PLUGIN_ID.so")

  # Prove the layout the host requires: a single entry named <id>.so at the root.
  entries="$(unzip -Z1 "$OUT/$asset")"
  [ "$entries" = "$PLUGIN_ID.so" ] || die "$asset must contain only $PLUGIN_ID.so at the zip root, got: $entries"

  (cd "$OUT" && shasum -a 256 "$asset" > "$asset.sha256")
  printf 'packaged %s\n' "$OUT/$asset"
  packaged=$((packaged + 1))
done
[ "$packaged" -gt 0 ] || die "no $ROOT/dist/$GOOS/<arch>/$PLUGIN_ID.so found"

# One checksums.txt per release, sha256sum format, same as the reference
# workflow's `sort dist/*.sha256 > dist/checksums.txt`.
(cd "$OUT" && cat ./*"_${VERSION}_${GOOS}_"*.zip.sha256 | sort > checksums.txt)
(cd "$OUT" && shasum -a 256 -c checksums.txt >/dev/null) || die "checksums.txt failed to verify"

printf '\nRelease assets for tag v%s:\n' "$VERSION"
(cd "$OUT" && ls -la ./*"_${VERSION}_${GOOS}_"*.zip checksums.txt)
printf '\nchecksums.txt:\n'
cat "$OUT/checksums.txt"
