#!/usr/bin/env bash
# D-4 UI DoD: the webhook manager against the real <App/> in jsdom (a
# 60-event journal + a failing endpoint with a mid-journal cursor): nav
# item, endpoint list (URL, categories, cursor, failure count), add form
# (POST with secret + categories), disable (PATCH), per-endpoint deliveries
# journal colored delivered/pending against the cursor, Replay all with
# inline confirm + cursor-reset confirmation, delete.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="$(pwd)/node_modules/.cache/webhooks.smoke.mjs"
mkdir -p "$(dirname "$OUT")"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/webhooks.smoke.jsx --bundle --platform=node \
  --format=esm --jsx=automatic --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
