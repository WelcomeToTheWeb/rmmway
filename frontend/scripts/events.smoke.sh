#!/usr/bin/env bash
# D-2 UI DoD: event journal browser against the real <App/> in jsdom (500-entry
# fake journal): nav item, newest-first tail page, category+device filtering,
# envelope detail + go-to-device, load-earlier paging, live SSE prepend.
set -euo pipefail
cd "$(dirname "$0")/.."
# Bundle inside the project tree: node resolves the external "jsdom" import
# by walking up to frontend/node_modules, which a /tmp output dir cannot see.
OUT="$(pwd)/node_modules/.cache/events.smoke.mjs"
mkdir -p "$(dirname "$OUT")"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/events.smoke.jsx --bundle --platform=node \
  --format=esm --jsx=automatic --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
