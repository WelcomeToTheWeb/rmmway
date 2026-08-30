#!/usr/bin/env bash
# B-1 frontend smoke test runner: bundles the jsdom smoke test with esbuild
# (shipped with vite) and runs it under node. Verifies the REAL <App/> in
# TWO operator sessions: a device going offline flips both sessions' status
# badge, and a new alert bumps both sessions' nav badge + open inbox —
# instantly, off the live SSE stream, not the polls.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-sse-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/sse.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
