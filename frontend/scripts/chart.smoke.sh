#!/usr/bin/env bash
# Chart frontend smoke test runner: bundles the jsdom smoke test with esbuild
# (shipped with vite) and runs it under node. Verifies the REAL <App/>: the
# revamped per-device metric chart renders an SVG with the series <polyline>,
# y gridlines with nice tick labels, human time ticks, a unit-aware stats row
# (now/min/max + samples/range/bucket), a category-grouped human series
# picker, hover crosshair + snapped tooltip (value + relative/absolute time),
# and the live/updated affordance.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-chart-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/chart.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
