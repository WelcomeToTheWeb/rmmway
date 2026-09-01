#!/usr/bin/env bash
# A-2 frontend smoke test runner: bundles the jsdom smoke test with esbuild
# (shipped with vite) and runs it under node. Verifies the REAL <App/>
# redirects a fresh database to the setup wizard, completes + auto-signs-in,
# and skips the wizard on subsequent boots.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=".tmp-setup-smoke.mjs"
trap 'rm -f "$OUT"' EXIT
./node_modules/.bin/esbuild scripts/setup-wizard.smoke.jsx \
  --bundle --platform=node --format=esm --jsx=automatic \
  --external:jsdom --log-level=warning --outfile="$OUT"
node "$OUT"
