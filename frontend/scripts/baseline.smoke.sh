#!/usr/bin/env bash
# D-5 UI DoD: the baseline anomaly explorer against the real <App/> in jsdom
# (two devices, a seeded anomaly landscape): nav item, worst-first table,
# min-score filter (query carries min_score), device filter (query carries
# device_id), Recompute (POST /api/baseline/run) with the working state and
# pass summary then a refreshed landscape, and hostname -> device detail.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="$(pwd)/node_modules/.cache/baseline.smoke.mjs"
mkdir -p "$(dirname "$OUT")"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/baseline.smoke.jsx --bundle --platform=node \
  --format=esm --jsx=automatic --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
