#!/usr/bin/env bash
# Metrics-viewer frontend smoke test runner: bundles the jsdom smoke test
# with esbuild (shipped with vite) and runs it under node. Verifies the
# REAL <App/>: expanding a device row shows the Metrics panel (series
# picker from GET /api/devices/{id}/metrics), the bucketed chart renders
# from GET /api/devices/{id}/metrics/series, the range selector re-requests
# with the new range, per-source series send name+source, and a device with
# no samples shows the empty state.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-metrics-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/metrics.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
