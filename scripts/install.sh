#!/usr/bin/env bash
#
# RMMWay one-line bootstrap installer (W1-3) — Linux + macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/welcometotheweb/rmmway/main/scripts/install.sh \
#     | bash -s -- --server https://rmm.example.com --bootstrap <TOKEN>
#
#   Optional: --grpc-addr host:port when the server's gRPC port differs from
#   the --server URL's port (split-port deployments).
#
# What it does (all in one pasted line):
#   1. Detect OS + arch (linux/darwin × amd64/arm64).
#   2. Download the matching static agent from the GitHub release (default:
#      latest; pin with --version).
#   3. Install it to /usr/local/bin (or $HOME/.local/bin when not root).
#   4. Write config (server URL + one-time bootstrap token) to /etc/rmmway.
#   5. Install + start the agent as a system service (systemd / launchd).
#
# The bootstrap token is consumed by the agent at first enroll (W1-4); it is
# written to a root-only file, never echoed back.
#
set -euo pipefail

# --- defaults ---------------------------------------------------------------
REPO="welcometotheweb/rmmway"
VERSION="latest"          # resolved via the GitHub API
SERVER=""
BOOTSTRAP=""
GRPC_ADDR=""            # optional explicit agent->server gRPC host:port
CONFIG_DIR="/etc/rmmway"
SERVICE_USER="root"
# Base URLs are overridable so the installer works against a self-hosted
# mirror AND can be E2E-tested against a local mock (RMMWAY_GITHUB_API /
# RMMWAY_DOWNLOAD_BASE).
GITHUB="${RMMWAY_GITHUB_API:-https://api.github.com}"
RAW_DL="${RMMWAY_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/download}"

# --- parse args -------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --server)     SERVER="${2:?--server needs a value}"; shift 2 ;;
    --bootstrap)  BOOTSTRAP="${2:?--bootstrap needs a value}"; shift 2 ;;
    --grpc-addr)  GRPC_ADDR="${2:?--grpc-addr needs a value}"; shift 2 ;;
    --version)    VERSION="${2:?--version needs a value}"; shift 2 ;;
    --config-dir) CONFIG_DIR="${2:?--config-dir needs a value}"; shift 2 ;;
    -h|--help)
      sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# --- detect os/arch ---------------------------------------------------------
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  linux|darwin) : ;;
  *) die "unsupported OS '$OS' (use scripts/install.ps1 on Windows)" ;;
esac

RAW_ARCH="$(uname -m)"
case "$RAW_ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) die "unsupported arch '$RAW_ARCH'" ;;
esac

# --- resolve release + asset URL -------------------------------------------
if [ "$VERSION" = "latest" ]; then
  VERSION="$(curl -fsSL "$GITHUB/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || die "could not resolve latest release for ${REPO}"
fi
URL="${RAW_DL}/${VERSION}/rmmway-agent-${OS}-${ARCH}"

log "OS=${OS} ARCH=${ARCH}  release=${VERSION}"
log "asset: ${URL}"

# --- pick install dir -------------------------------------------------------
if [ "$(id -u)" = "0" ]; then
  INSTALL_DIR="/usr/local/bin"
  BIN="${INSTALL_DIR}/rmmway-agent"
else
  INSTALL_DIR="${HOME}/.local/bin"
  BIN="${INSTALL_DIR}/rmmway-agent"
  log "not root — installing to ${INSTALL_DIR}"
fi
mkdir -p "$INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "cannot write to ${INSTALL_DIR} (re-run as root or sudo)"

# --- download to a temp file first (atomic install) -------------------------
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
log "downloading agent ..."
curl -fSL --retry 3 -o "$TMP" "$URL" || die "download failed from ${URL}"
chmod +x "$TMP"

# sanity: it must run and report a version before we clobber the old binary
VER_OUT="$("$TMP" --version 2>&1)" || die "downloaded binary will not run: $VER_OUT"
log "verified: ${VER_OUT}"
install -m 0755 "$TMP" "$BIN"
log "installed -> ${BIN}"

# --- write config -----------------------------------------------------------
mkdir -p "$CONFIG_DIR"
CFG="${CONFIG_DIR}/agent.env"
{
  printf 'RMMWAY_SERVER=%s\n'      "${SERVER:-https://rmm.local}"
  printf 'RMMWAY_BOOTSTRAP_TOKEN=%s\n' "${BOOTSTRAP:-}"
  printf 'RMMWAY_DEVICE_ID=%s\n'    "$(hostname)"
  # Optional explicit gRPC endpoint. When set, the agent connects here
  # directly instead of deriving host:port from RMMWAY_SERVER (needed for
  # split-port deployments where HTTP and gRPC listen on different ports).
  [ -n "${GRPC_ADDR}" ] && printf 'RMMWAY_GRPC_ADDR=%s\n' "${GRPC_ADDR}"
} > "$CFG"
chmod 0600 "$CFG"
chown root:root "$CFG" 2>/dev/null || true
log "config -> ${CFG} (0600)"

# --- install the service ----------------------------------------------------
run_cmd="${BIN} run --config ${CFG}"

if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  UNIT="/etc/systemd/system/rmmway-agent.service"
  log "installing systemd unit -> ${UNIT}"
  cat > "$UNIT" <<EOF
[Unit]
Description=RMMWay agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
EnvironmentFile=${CFG}
ExecStart=${run_cmd}
Restart=on-failure
RestartSec=5
# hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
ReadWritePaths=${CONFIG_DIR}

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now rmmway-agent.service || log "unit enabled (start deferred — is systemd running?)"
elif [ "$OS" = "darwin" ] && [ -d /Library/LaunchDaemons ]; then
  PLIST="/Library/LaunchDaemons/io.rmmway.agent.plist"
  log "installing launchd plist -> ${PLIST}"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>io.rmmway.agent</string>
  <key>ProgramArguments</key>
  <array><string>${run_cmd}</string></array>
  <key>EnvironmentFile</key><string>${CFG}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  chmod 0600 "$PLIST"
  launchctl bootstrap system "$PLIST" 2>/dev/null \
    || launchctl load -w "$PLIST" 2>/dev/null \
    || log "plist written (load it with: launchctl bootstrap system ${PLIST})"
else
  log "no service manager found — run the agent with: ${run_cmd}"
fi

# --- summary ----------------------------------------------------------------
log "done. agent ${VER_OUT} installed."
log "  binary : ${BIN}"
log "  config : ${CFG}"
[ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1 \
  && systemctl is-active rmmway-agent.service 2>/dev/null | sed 's/^/  status : /'
log "note: enrollment + the connect/run loop land in W1-4; the binary and"
log "      service are in place now."
