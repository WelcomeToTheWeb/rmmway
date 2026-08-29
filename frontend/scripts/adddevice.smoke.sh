#!/usr/bin/env bash
# Add-device frontend smoke test runner: bundles the jsdom smoke test with
# esbuild (shipped with vite) and runs it under node. Verifies the REAL <App/>:
# sign in -> Devices -> "Add a device" mints a token (POST /api/bootstrap) and
# renders copy-paste install commands with the origin + token + device id.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-adddevice-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/adddevice.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
