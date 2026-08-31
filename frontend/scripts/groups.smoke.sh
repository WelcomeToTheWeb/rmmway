#!/usr/bin/env bash
# B-2 frontend smoke test runner: bundles the jsdom smoke test with esbuild
# (shipped with vite) and runs it under node. Verifies the REAL <App/>:
# tag a cohort through the per-device tag editor (PATCH /api/devices/{id}),
# filter the device list with `tag:web` (the exact group), then fire ONE
# bulk command at the whole group (POST /api/devices/bulk/commands) and
# read the matched/pushed/offline fan-out result.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-groups-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/groups.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
