#!/usr/bin/env bash
# Builds SoftafriqueBackupAgent.exe (amd64 Windows) and bundles restic.exe
# next to it so the agent needs no other dependencies on the customer PC.
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