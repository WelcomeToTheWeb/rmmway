#!/usr/bin/env bash
# W1-1: cross-compile the RMMWay agent as static binaries.
#
# Targets: linux/darwin/windows × amd64/arm64 (windows arm64 skipped — the
# agent's DoD targets Windows on x86_64; add GOOS=windows GOARCH=arm64 if needed).
# Output: agent/dist/rmmway-agent-<os>-<arch>[.exe]
#
# Flags: CGO_ENABLED=0 (pure-Go static — no libc dependency), -trimpath,
# stripped, version/commit/date stamped via -ldflags.
# Usage: scripts/build-agent.sh [version]   (default: git describe)
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT=agent/dist
mkdir -p "$OUT"

LDFLAGS="-s -w \
  -X main.version=${VERSION} \
  -X main.commit=${COMMIT} \
  -X main.date=${DATE}"

build() {
  local goos=$1 goarch=$2 name
  case "$goos" in
    windows) name="rmmway-agent-${goos}-${goarch}.exe" ;;
    *)        name="rmmway-agent-${goos}-${goarch}" ;;
  esac
  echo "==> ${goos}/${goarch} -> agent/dist/${name}"
  (cd agent && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build \
    -trimpath -ldflags "$LDFLAGS" -o "dist/${name}" ./cmd/agent)
}

build linux amd64
build linux arm64
build darwin amd64
build darwin arm64
build windows amd64

echo "==> built for $(git -c core.abbrev=short rev-parse --abbrev-ref HEAD 2>/dev/null || echo dev) version ${VERSION}:"
ls -lh agent/dist/
