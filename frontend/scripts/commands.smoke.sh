#!/usr/bin/env bash
# D-1 command results frontend smoke test runner: bundles the jsdom smoke
# test with esbuild (shipped with vite) and runs it under node. Verifies
# the REAL <App/>: expanding a device row shows the Commands panel seeded
# from GET /api/devices/{id}/commands (newest first, PENDING/RUNNING/
# SUCCEEDED statuses, action types), expanding a finished row reveals the
# agent's exit code + stdout, a command-category SSE envelope re-fetches
# the list without a page refresh, and the manual refresh button is the
# fallback.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-commands-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/commands.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
