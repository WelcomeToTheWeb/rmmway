#!/usr/bin/env bash
# D-3 UI DoD: heal dashboard against the real <App/> in jsdom (3 playbooks,
# 2 seeded runs): nav item, playbook table, toggle-off PATCH, Run Pass Now
# walking a fresh run detected->verifying->remediating->confirming->resolved,
# full stage trace (trigger, dispatch, agent output), status filter, create
# form.
set -euo pipefail
cd "$(dirname "$0")/.."
# Bundle inside the project tree: node resolves the external "jsdom" import
# by walking up to frontend/node_modules, which a /tmp output dir cannot see.
OUT="$(pwd)/node_modules/.cache/heal.smoke.mjs"
mkdir -p "$(dirname "$OUT")"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/heal.smoke.jsx --bundle --platform=node \
  --format=esm --jsx=automatic --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
