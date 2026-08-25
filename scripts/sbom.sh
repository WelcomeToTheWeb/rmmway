#!/usr/bin/env bash
# W4-1: generate CycloneDX SBOMs for every artifact.
#
# Outputs (all CycloneDX JSON, scanned by the pinned syft):
#   agent/dist/rmmway-agent-<os>-<arch>[.exe].cdx.json   (5 agent binaries)
#   dist/rmmway-server.cdx.json                          (server Docker image)
#
# The agent binaries must already be built (make agent). The server image
# is scanned via the local Docker daemon; build it first with:
#   make image        (docker build -t rmmway-server:local -f server/Dockerfile .)
# RMMWAY_SERVER_IMAGE overrides the image reference (default rmmway-server:local).
#
# Usage: scripts/sbom.sh [--skip-image]
set -euo pipefail
cd "$(dirname "$0")/.."

SKIP_IMAGE=0
[ "${1:-}" = "--skip-image" ] && SKIP_IMAGE=1

SERVER_IMAGE="${RMMWAY_SERVER_IMAGE:-rmmway-server:local}"

./scripts/install-syft.sh
SYFT="bin/syft"

AGENTS="rmmway-agent-linux-amd64
rmmway-agent-linux-arm64
rmmway-agent-darwin-amd64
rmmway-agent-darwin-arm64
rmmway-agent-windows-amd64.exe"

echo "==> agent binary SBOMs (CycloneDX JSON)"
for a in $AGENTS; do
  f="agent/dist/$a"
  [ -f "$f" ] || { echo "ERROR: $f not found — run: make agent" >&2; exit 1; }
  echo "  $f -> $f.cdx.json"
  "$SYFT" "file:$f" -o cyclonedx-json="$f.cdx.json" -q
done

if [ "$SKIP_IMAGE" = 0 ]; then
  echo "==> server image SBOM (CycloneDX JSON)"
  if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: docker not found — build the image with 'make image' first, or rerun with --skip-image" >&2
    exit 1
  fi
  if ! docker image inspect "$SERVER_IMAGE" >/dev/null 2>&1; then
    echo "ERROR: image $SERVER_IMAGE not present — build it with: make image (or set RMMWAY_SERVER_IMAGE)" >&2
    exit 1
  fi
  mkdir -p dist
  echo "  $SERVER_IMAGE -> dist/rmmway-server.cdx.json"
  "$SYFT" "docker:$SERVER_IMAGE" -o cyclonedx-json=dist/rmmway-server.cdx.json -q
fi

echo "==> SBOMs:"
ls -lh agent/dist/*.cdx.json
[ -f dist/rmmway-server.cdx.json ] && ls -lh dist/rmmway-server.cdx.json || true
