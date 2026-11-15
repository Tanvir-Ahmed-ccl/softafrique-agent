#!/usr/bin/env bash
# Checks that the version a build will ship is the version the sources declare,
# and, for a release tag, that the tag names the commit being built and is not a
# rerun of the previous release.
#
# This exists because the same version was shipped twice: Windows treats that as
# a reinstall, skips RemoveExistingProducts, and the upgrade path that was
# supposed to be tested never runs.
#
# Usage: scripts/check-version.sh <version> [--tag]
#
#   <version>  the version the build passes to the compiler and to WiX
#   --tag      <version> came from a git tag and must be a new release
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <version> [--tag]" >&2
  exit 2
fi

VERSION="$1"
MODE="${2:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

if [ -n "$MODE" ] && [ "$MODE" != "--tag" ]; then
  fail "unknown option '$MODE' (expected --tag)"
fi

make_version="$(sed -n 's/^VERSION[[:space:]]*?=[[:space:]]*//p' "$ROOT/Makefile" | head -n1)"
wxs_version="$(sed -n 's/.*<?define ProductVersion="\([^"]*\)".*/\1/p' "$ROOT/installer/Product.wxs" | head -n1)"
script_version="$(sed -n 's/^VERSION="\${1:-\([^}]*\)}"/\1/p' "$ROOT/scripts/build-windows.sh" | head -n1)"

[ -n "$make_version" ] || fail "could not read the default version from Makefile"
[ -n "$wxs_version" ] || fail "could not read the default version from installer/Product.wxs"
[ -n "$script_version" ] || fail "could not read the default version from scripts/build-windows.sh"

[ "$make_version" = "$VERSION" ] || \
  fail "Makefile defaults to '$make_version', but this build ships $VERSION"
[ "$wxs_version" = "$VERSION" ] || \
  fail "installer/Product.wxs defaults to '$wxs_version', but this build ships $VERSION"
[ "$script_version" = "$VERSION" ] || \
  fail "scripts/build-windows.sh defaults to '$script_version', but this build ships $VERSION"

if [ "$MODE" = "--tag" ]; then
  tag="v$VERSION"
  git -C "$ROOT" rev-parse -q --verify "refs/tags/$tag" >/dev/null || \
    fail "no git tag $tag exists"
  head_commit="$(git -C "$ROOT" rev-parse HEAD)"
  tag_commit="$(git -C "$ROOT" rev-parse "$tag^{commit}")"
  [ "$head_commit" = "$tag_commit" ] || \
    fail "tag $tag points at $tag_commit, not the commit being built ($head_commit)"

  previous="$(git -C "$ROOT" tag --list 'v*' --sort=-v:refname | \
    grep -Fvx "v$VERSION" | head -n1 || true)"
  [ -n "$previous" ] || fail "no previous release tag to compare against"
  [ "v$VERSION" != "$previous" ] || \
    fail "the release version did not move; the previous release is $previous"

  echo "ok  $tag is a new release (previous $previous)"
fi

echo "ok  Makefile, Product.wxs and build-windows.sh all declare $VERSION"
