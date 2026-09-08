#!/usr/bin/env bash
# test-install-sh.sh — unit test for install.sh's mTLS-addr derivation (W0 gap #9).
#
# The bug this guards: for the most common --server input (a scheme with no
# path, e.g. https://rmm.example.com) the `*)` case branch stripped the
# ORIGINAL value at the first ':' instead of the scheme-stripped one, leaving
# "https" behind and writing RMMWAY_GRPC_MTLS_ADDR=https:50052 — a dead mTLS
# target that broke one-click onboarding.
#
# The test extracts the real derivation block from install.sh (not a copy) so
# the test and the installer cannot drift, then runs it against a matrix of
# --server shapes and asserts the emitted RMMWAY_GRPC_MTLS_ADDR line.
#
# Usage: ./scripts/test-install-sh.sh   (exit 0 = all cases pass)
set -euo pipefail
cd "$(dirname "$0")/.."

SNIPPET="$(
  awk '
    $0 ~ /^[[:space:]]*_srv=/ { found = 1 }
    found { print }
    found && /printf '"'"'RMMWAY_GRPC_MTLS_ADDR=%s:50052/ { exit }
  ' scripts/install.sh
)"

# The extraction must yield a non-empty, executable snippet (the anchors are
# stable lines of the installer; if this fails the installer changed shape).
[ -n "$SNIPPET" ] || {
  echo "FAIL: could not extract the mTLS-addr derivation block from scripts/install.sh" >&2
  exit 1
}
bash -n <(printf '%s\n' "$SNIPPET") || {
  echo "FAIL: extracted block is not valid bash" >&2
  exit 1
}

FAILURES=0

run_case() {
  local server="$1" want="$2" got
  got="$(SERVER="$server" bash -c "$SNIPPET")"
  if [ "$got" != "$want" ]; then
    echo "FAIL: SERVER=$server" >&2
    echo "      got : ${got:-<no RMMWAY_GRPC_MTLS_ADDR line>}" >&2
    echo "      want: ${want:-<no RMMWAY_GRPC_MTLS_ADDR line>}" >&2
    FAILURES=$((FAILURES + 1))
  else
    echo "ok: SERVER=$server -> ${got:-<no RMMWAY_GRPC_MTLS_ADDR line>}"
  fi
}

# scheme, no path — the gap #9 case (was: RMMWAY_GRPC_MTLS_ADDR=https:50052)
run_case "https://rmm.example.com" "RMMWAY_GRPC_MTLS_ADDR=rmm.example.com:50052"
# scheme + explicit port
run_case "https://rmm.example.com:8443" "RMMWAY_GRPC_MTLS_ADDR=rmm.example.com:50052"
# scheme + path
run_case "https://rmm.example.com/agent/enroll" "RMMWAY_GRPC_MTLS_ADDR=rmm.example.com:50052"
# no scheme (bare host)
run_case "rmm.example.com" "RMMWAY_GRPC_MTLS_ADDR=rmm.example.com:50052"
# no scheme + explicit port
run_case "rmm.example.com:8443" "RMMWAY_GRPC_MTLS_ADDR=rmm.example.com:50052"
# no --server at all: no derivation line (the agent-side default applies)
run_case "" ""

if [ "$FAILURES" -gt 0 ]; then
  echo "install.sh mTLS-addr derivation: $FAILURES case(s) FAILED" >&2
  exit 1
fi
echo "install.sh mTLS-addr derivation: all cases pass"
