#!/usr/bin/env bash
# W4-1: install a pinned syft (Anchore) into bin/ if not already present.
#
# Syft generates the CycloneDX SBOMs for every release artifact (W4-1).
# Pinning the version here (and nowhere else) keeps the CI workflows and
# the local `make sbom` path generating SBOMs with the same scanner.
#
# Usage: scripts/install-syft.sh   (installs bin/syft, idempotent)
# Override: SYFT_VERSION=1.51.0 SYFT_PLATFORM=linux_arm64
set -euo pipefail
cd "$(dirname "$0")/.."

SYFT_VERSION="${SYFT_VERSION:-1.51.0}"
SYFT_PLATFORM="${SYFT_PLATFORM:-linux_amd64}"
OUT_DIR="bin"
OUT="${OUT_DIR}/syft"

if [ -x "$OUT" ] && [ "$(./"$OUT" version 2>/dev/null | awk '/^Version:/{print $2}')" = "$SYFT_VERSION" ]; then
  echo "==> syft ${SYFT_VERSION} already at ${OUT}"
  exit 0
fi

mkdir -p "$OUT_DIR"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
name="syft_${SYFT_VERSION}_${SYFT_PLATFORM}.tar.gz"
echo "==> downloading syft ${SYFT_VERSION} (${SYFT_PLATFORM})"
curl -fsSL -o "$tmp/$name" \
  "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/${name}"
# The release ships a sha256 checksums file; verify the tarball against it
# BEFORE installing anything.
curl -fsSL -o "$tmp/checksums.txt" \
  "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/syft_${SYFT_VERSION}_checksums.txt"
grep -q "$name" "$tmp/checksums.txt" || \
  { echo "ERROR: no checksum entry for $name in the release checksums file" >&2; exit 1; }
(cd "$tmp" && grep "$name" checksums.txt | sha256sum -c -)
tar -xzf "$tmp/$name" -C "$tmp" syft
mv "$tmp/syft" "$OUT"
chmod +x "$OUT"
echo "==> installed $(./"$OUT" version | awk '/^Version:/{print $2}') at ${OUT}"
