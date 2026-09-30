#!/usr/bin/env bash
# Builds SoftafriqueBackupAgent.exe and validatepath.exe (amd64 Windows) and
# bundles restic.exe next to them so the agent needs no other dependencies on the
# customer PC.
#
# validatepath.exe is built here, not just in CI, because the MSI installs it and
# the installer cannot work without it: it is what refuses a backup folder on
# anything but a fixed disk, and it is what creates a missing one. A build script
# that quietly left a stale copy in build/bin is a way to ship an installer that
# enforces an older folder policy than the code does.
#
# Usage: scripts/build-windows.sh [version]
set -euo pipefail

VERSION="${1:-0.1.0}"
RESTIC_VERSION="${RESTIC_VERSION:-0.19.1}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/build/bin"

echo ">> Building SoftafriqueBackupAgent.exe (version $VERSION)"
mkdir -p "$OUT"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.Version=${VERSION}" \
  -o "$OUT/SoftafriqueBackupAgent.exe" \
  "$ROOT/cmd/agent"

# The helper the MSI calls to validate and create the folder to protect. Kept a
# separate binary on purpose: it has to run during a managed install as the
# installing user, from a deferred action, with no dependency on the agent's
# runtime state.
echo ">> Building validatepath.exe"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w" \
  -o "$OUT/validatepath.exe" \
  "$ROOT/cmd/validatepath"

if [ ! -f "$OUT/restic.exe" ]; then
  echo ">> Fetching restic ${RESTIC_VERSION} for Windows (one time)"
  TMP="$(mktemp -d)"
  curl -fsSL -o "$TMP/restic.zip" \
    "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_windows_amd64.zip"
  unzip -jo "$TMP/restic.zip" "restic_*_windows_amd64.exe" -d "$OUT"
  rm -rf "$TMP"
  mv "$OUT"/restic_*_windows_amd64.exe "$OUT/restic.exe"
fi

echo ">> Done. Contents of $OUT:"
ls -lh "$OUT"