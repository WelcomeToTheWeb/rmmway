#!/usr/bin/env bash
# D-6 UI DoD: the real <App/> in jsdom against a fake export endpoint that
# serves a real STORE-method ZIP with a self-consistent manifest: the device
# detail's "Client export" panel (Export button), the confirmation naming
# the device, the Preparing… state, a download named
# <hostname>-rmmway-export-<date>.zip whose bytes ARE the bundle, and
# manifest SHA-256/size verification of every data file (PAR1-checked).
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="$(pwd)/node_modules/.cache/export.smoke.mjs"
mkdir -p "$(dirname "$OUT")"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/export.smoke.jsx --bundle --platform=node \
  --format=esm --jsx=automatic --external:jsdom --external:node:crypto --log-level=warning --outfile="$OUT"
node "$OUT"
