#!/usr/bin/env bash
# W3-4: verify every release artifact that has a .minisig signature against
# the committed public key (keys/minisign.pub). Exit 0 = all good.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || { echo "ERROR: go not on PATH" >&2; exit 1; }
[ -f keys/minisign.pub ] || { echo "ERROR: keys/minisign.pub missing" >&2; exit 1; }

files=()
for f in agent/dist/rmmway-agent-* scripts/install.sh scripts/install.ps1 SHA256SUMS; do
  [ -f "${f}.minisig" ] && files+=("$f")
done
[ ${#files[@]} -gt 0 ] || { echo "no signed artifacts found (run: make sign)" >&2; exit 1; }

mkdir -p bin
go -C tools/signer build -o "$PWD/bin/rmmway-signer" .
"$PWD/bin/rmmway-signer" verify -p keys/minisign.pub "${files[@]}"
