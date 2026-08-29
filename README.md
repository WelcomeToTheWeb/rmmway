# RMMWay

RMMWay is a **self-hosted remote monitoring & management (RMM) platform**. A small
static agent runs on every machine you manage; a single Go server aggregates
metrics, logs, alerts, and commands — and an operator web UI ties it together.
Everything runs in your own infrastructure: the data, the TLS keys, and the
automation all stay under your control.

## Features

**Fleet monitoring**
- Live device list — online/offline, hostname, OS, IP addresses, last heartbeat, agent version
- Continuous metrics collection, stored in a TimescaleDB hypertable with 1-minute rollups
- **Dynamic baselining** — every metric is scored against *that device's own*
  seasonal + trend baseline (a robust 45-day day-of-week/hour channel plus a
  same-day trend channel). No thresholds to tune, no ML dependencies.
- **Alert inbox** — anomalies are folded into one deduped alert per
  (device, metric, source); alerts auto-resolve when a series returns to
  baseline, or you ack/resolve them from the UI
- Agent log collection — every agent ships its structured logs, both to
  Loki and into a per-device store browsable right in the UI
- Full-text device search (hostname, ID, IP, tags, OS, agent version) with a
  Cmd-K (Ctrl/⌘+K) command palette from any screen

**Remote management**
- **One-click "Add a device"** — the UI mints a one-time enrollment token and
  shows a single copy-paste command per OS
- Dispatch commands to live devices (reboot, run a script) with results reported back
- Live server-sent-events (SSE) stream — status changes and alerts reach the
  UI immediately

**Agents**
- One static binary per platform — Linux, macOS, and Windows (amd64 + arm64).
  On Windows it runs as a real Windows service with auto-restart
- Enrollment uses a one-time, short-TTL token over your HTTPS origin; from
  that point the uplink is **mutual TLS** against your organization's own CA
- **Signed auto-updates** — agents periodically check for new releases and
  only ever install a binary that verifies against a release key pinned
  inside the agent. A tampered or unsigned build is refused

**Integrations & data ownership**
- **Webhooks** (HMAC-SHA256 signed, Stripe-style) and a public **SSE event
  stream** — every alert, inventory, and automation event is journaled,
  retried with backoff, and replayable
- **Client export** — one request exports everything RMMWay knows about a
  client (inventory, raw metrics + rollups in standard Parquet, full alert
  history) into a self-verifying, tamper-evident ZIP bundle. Portable data
  you can open in DuckDB/pandas or re-import anywhere — no lock-in

**Deployment & security**
- One-command hardened production stack behind an automatic-TLS Caddy edge —
  or bring your own reverse proxy (Nginx, Traefik, HAProxy, Caddy)
- First-boot setup wizard: create your root admin, define your organization's
  mTLS CA, and configure an SMTP outbox — all persisted to the database
- Every release artifact is cryptographically signed (minisign / Sigstore) and
  ships with a CycloneDX SBOM

## How it works

```
                    ┌─────────────────────────────┐
  Operator UI ──────┤  RMMWay server (Go)         │
  (TLS 443)         │  HTTP API + gRPC + engines  │
                    │  TimescaleDB · NATS · Redis │
                    │  Meilisearch · Loki · MinIO │
                    └──────────────┬──────────────┘
                                   │ mTLS gRPC (50052)
              ┌────────────────────┼────────────────────┐
              ▼                     ▼                    ▼
          Agent A              Agent B              Agent C
        (static binary)      (static binary)      (Windows service)
```

- **Enrollment** (one-time, per device) happens over the operator's HTTPS
  origin — a fresh agent proves its one-time token there and receives its
  long-lived credentials plus an mTLS leaf signed by your org CA.
- **Everything after** — heartbeats, metrics, logs, commands — flows over a
  mutual-TLS gRPC channel where the server verifies *both* the agent's JWT
  and its client certificate.
- In a production deployment the only publicly exposed ports are `443`
  (operator UI + API) and `50052` (agent mTLS channel). All backing
  services stay on an internal Docker network.

## Setting up the server

### Quick start (local development)

Requires Docker + Go 1.24+ + Node 18+. Three terminals:

```sh
# Terminal 1 — backing services (TimescaleDB, NATS JetStream, Redis,
# MinIO, Meilisearch, Loki); blocks until all 6 report healthy
make dev

# Terminal 2 — server (applies pending database migrations on boot)
make run-server     # HTTP :8080 · gRPC :50051 · mTLS :50052

# Terminal 3 — frontend
make frontend       # http://localhost:5173 — login admin / admin
```

The server reports `"ok": true` from `GET /healthz` only when every backing
service answers an *active* probe (a real Postgres query, a NATS JetStream
check, a Redis PING, …) — not just an open port:

```sh
curl -fsS localhost:8080/healthz
```

On a fresh database the UI shows a **first-boot setup wizard** instead of the
login screen (see below). In dev you can skip it and sign in with the built-in
`admin` / `admin` fallback. Teardown: `make down` (keeps data) or
`make clean` (deletes volumes — destructive).

### First-boot setup wizard

A fresh RMMWay deployment is not yet initialized: on first boot the web UI
redirects to a one-time setup wizard. It does three things, all persisted in
a single transaction:

1. **Mints the root admin account** — the username and password you'll sign
   in with (stored as a PBKDF2 hash).
2. **Defines your organization's CA** — the org name you enter is stamped
   into the root CA that every agent pins for mutual TLS.
3. **Configures the SMTP outbox** (optional) — host/port/from with optional
   auth, and a live "send test email" button to verify it.

After completion the wizard is gone for good — subsequent boots go straight
to the login screen. (A deployment that already has enrolled devices is never
treated as fresh, so the CA can't be swapped out from under pinned agents.)

### Production deployment

A hardened, single-command production deployment is provided. It builds the
server and frontend images and boots the whole stack behind a Caddy edge
that issues TLS automatically (Let's Encrypt for a public domain; Caddy's
internal CA for `localhost`).

**1. Configure:** copy `.env.prod.example` to `.env.prod` and set
`RMMWAY_DOMAIN` (your public hostname) plus the required secrets (JWT secret,
admin credentials, Meilisearch master key, …).

**2. Boot:**

```sh
make prod
# == docker compose --env-file .env.prod --profile edge -f docker-compose.prod.yml up -d --build
```

**3. Verify:**

```sh
curl -fsSk https://<domain>/healthz          # all probes ok
openssl s_client -connect <host>:443 </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -subject -ext subjectAltName
```

Then run the first-boot setup wizard in the UI (step 2 above).

The only public ports are:

| Port | What |
| --- | --- |
| `80` / `443` | Caddy — operator UI + API over TLS (ACME HTTP-01 for a public domain) |
| `50052` | Agent mTLS gRPC channel — end-to-end mTLS, isolated from the operator API |

Everything else (TimescaleDB, NATS, Redis, MinIO, Meilisearch, Loki, and the
server's plain gRPC bootstrap port) stays on the internal `rmmway-prod`
Docker network with no host ports.

Hardening: pinned image tags, `restart: unless-stopped`,
`no-new-privileges`, capabilities dropped to the minimum per service, and
secrets passed only via the git-ignored `.env.prod` (never baked into
images). The mTLS server certificate's SANs include `RMMWAY_DOMAIN`, so
remote agents' hostname verification passes when they dial
`<domain>:50052`. (For a public IP without a DNS name, Caddy ≥ 2.8 can issue
a Let's Encrypt cert for the bare IP; a real hostname is still recommended.)

Manage the stack with `make prod-down` (stop, keep data),
`make prod-clean` (stop + delete volumes — destructive), and
`make prod-logs`.

#### Bring your own reverse proxy

The bundled Caddy edge is optional. If you'd rather terminate TLS with your
own proxy (Nginx, Traefik, HAProxy, or your own Caddy) — e.g. it already
fronts other services or you manage certificates centrally — boot the stack
without the bundled Caddy:

```sh
make prod-byoproxy
```

This leaves host ports 80/443 free and instead publishes:

| Host port | Env var | Reaches | What |
| --- | --- | --- | --- |
| `8081` | `RMMWAY_FRONTEND_PORT` | frontend | the operator SPA **plus** `/api/*`, `/agent/*`, `/healthz*` (the frontend proxies these to the backend) |
| `8080` | `RMMWAY_HTTP_PORT` | server | the operator API directly (optional) |

**Simplest setup (recommended): point your whole proxy at the SPA port
(8081).** The frontend container serves the SPA *and* reverse-proxies the
API to the backend, so same-origin API calls, first-boot setup, login, and
the live SSE stream all work through a single upstream.

Nginx (host-level, Let's Encrypt via certbot) — single upstream:

```nginx
server {
    listen 443 ssl; http2 on;
    server_name rmm.example.com;
    ssl_certificate     /etc/letsencrypt/live/rmm.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/rmm.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_buffering off;          # SSE: stream, don't buffer
        proxy_cache off;
        proxy_read_timeout 3600s;
    }
}
```

Caddy on the host — single upstream (automatic Let's Encrypt; SSE just works):

```caddyfile
rmm.example.com {
    reverse_proxy 127.0.0.1:8081
}
```

Alternatively you can route `/api/*`, `/agent/*`, `/healthz*` straight to the
backend port (8080) and the SPA to the frontend port (8081):

```caddyfile
rmm.example.com {
    handle /api/*     { reverse_proxy 127.0.0.1:8080 }
    handle /agent/*   { reverse_proxy 127.0.0.1:8080 }
    handle /healthz*  { reverse_proxy 127.0.0.1:8080 }
    handle            { reverse_proxy 127.0.0.1:8081 }
}
```

If your proxy runs as a Docker container, attach it to the shared
`rmmway-prod` network and proxy `frontend:8080` directly — no host ports
needed at all.

The agent mTLS channel (`RMMWAY_AGENT_MTLS_PORT`, default `50052`) is
published directly by the server either way and is unaffected — agents still
dial `<domain>:50052`. Manage the BYO stack with
`make prod-byoproxy-down` / `prod-byoproxy-clean` / `prod-byoproxy-logs`.

## Installing the agent

### One-click: "Add a device"

In the operator UI: **Devices → + Add a device**. The dialog mints a
one-time token and shows a single copy-paste command for your target OS,
with your server URL pre-filled. The device appears in the list the moment
the agent connects.

**Linux / macOS:**

```sh
curl -fsSL https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.sh \
  | bash -s -- --server https://rmm.example.com --bootstrap <TOKEN>
```

**Windows (PowerShell):**

```powershell
iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.ps1 -OutFile install.ps1
powershell -ExecutionPolicy Bypass -File install.ps1 -Server https://rmm.example.com -Bootstrap <TOKEN>
```

What the one-liner does:

1. Downloads the static agent binary for that platform (and verifies it
   against the signed release, where a signature is published).
2. Registers it (as a service on Windows; a managed unit elsewhere).
3. The agent **enrolls over your HTTPS origin** with the one-time token and
   receives its long-lived credentials plus an mTLS leaf signed by your org
   CA — in the same round-trip.
4. From then on it streams over the **mTLS gRPC port** (`50052` by default).

So a remote machine only needs **your server host + port 50052** reachable.
The token is one-time and short-lived (~30 min); re-running the installer on
an already-enrolled device is safe — it reuses the persisted identity and
never re-enrolls.

### Managing the Windows agent

On Windows the agent runs as a service named `RmmWayAgent` (auto-restarts on
crash). Useful commands (elevated PowerShell):

```powershell
Get-Service RmmWayAgent            # status
Restart-Service RmmWayAgent        # restart
Stop-Service RmmWayAgent           # stop (registration stays in place)
sc.exe query RmmWayAgent           # raw service-control state + exit code
# run in the foreground to see live logs / the real error:
& "C:\Program Files\RMMWay\rmmway-agent.exe" run --config "C:\Program Files\RMMWay\agent.env"
```

If the service won't start, run the last command — the agent prints the
underlying reason (bad server URL, expired/used token, mTLS port unreachable)
to the console and the Application event log.

### Automatic agent updates

While running, each agent checks your server for new releases (every 15
minutes by default) and self-updates — but **only a correctly signed
release is ever installed**. Each download is verified against a minisign
release key pinned inside the agent binary before it replaces anything; a
tampered or unsigned build is refused and the running binary is left
untouched (agents also refuse downgrades).

To publish an update, build and sign the agent (`make agent && make sign`),
assemble the release directory, and point the server at it:

```sh
make agent && make sign
make release-dir DIR=releases-local
RMMWAY_RELEASES_DIR=releases-local make run-server
```

The agent can also update on demand:

```sh
rmmway-agent update --check --server https://rmm.example.com   # verify only
rmmway-agent update --server https://rmm.example.com           # update + restart
```

Tuning: `RMMWAY_AUTO_UPDATE=off` disables the background loop;
`RMMWAY_UPDATE_INTERVAL=5m` changes the cadence.

## Integrations

### Webhooks & event stream

Every event — **alerts**, **inventory** changes (device created / online /
offline), and **automation** actions — is journaled to the database with a
monotonic sequence, then delivered two ways:

- **Signed webhooks** to endpoints you define: each delivery is HMAC-SHA256
  signed (Stripe-style `t=<unix>,v1=<hex>`), verified with the secret you
  chose, and retried with exponential backoff. Failed endpoints are
  dead-lettered (not silently dropped) and can be replayed from the journal.
- **A live SSE stream** (`GET /api/events/stream`) with catch-up of the last
  200 events and `Last-Event-ID` resume.

Manage endpoints (URL, secret, subscribed categories, enable/disable,
delivery journal, replay) from the API or UI.

### Client export (no lock-in)

One request exports **everything RMMWay knows about a client** into a
portable ZIP bundle:

| file | what |
|------|------|
| `manifest.json` | per-file sha256 + size + row count — the bundle verifies itself |
| `device.json` | inventory + agent configuration |
| `metrics.parquet` | raw samples (opens in DuckDB / pandas / polars) |
| `metrics_1m.parquet` | 1-minute rollups (survive raw-data retention) |
| `alerts.json` | complete alert history |

Optionally filter with a time window (`?since=…&until=…`). Flip a single
byte in the bundle and verification fails.

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  https://rmm.example.com/api/devices/<id>/export -o client.zip

unzip client.zip
duckdb -c "SELECT * FROM 'metrics.parquet' WHERE name='cpu.utilization_percent' ORDER BY ts DESC LIMIT 100;"
```

### Agent logs

Each agent tails a structured JSON-lines log file and ships every batch to
**both** Loki (queryable by `device_id`/level) **and** the server (per-device
view in the UI). Delivery is at-least-once with content-derived dedup — a
restart resumes from where it left off instead of re-shipping history.

## Security model

- **Enrollment** — one-time, short-TTL tokens minted by an authenticated
  operator. No anonymous enrollment path exists.
- **Uplink** — mutual TLS against your organization's own CA (defined in the
  first-boot wizard). The server checks the agent's client certificate *and*
  its JWT; enrollment is the identity root.
- **Isolation** — in production the operator API (443) and the agent mTLS
  channel (50052) are fully isolated; all backing services are internal.
- **Releases** — every agent binary, installer, checksums file, and the
  server Docker image are cryptographically signed (minisign / cosign-Sigstore)
  with CycloneDX SBOMs attached, so anyone can verify what they install.
- **Updates** — agents verify against a release key pinned in the binary
  before self-updating; the server is only a carrier.

To verify a released agent binary yourself:

```sh
# from a GitHub release: rmmway-agent-<os>-<arch>, its .minisig, and minisign.pub
minisign -Vm rmmway-agent-linux-amd64 -P '<line 2 of minisign.pub>'
cosign verify ghcr.io/welcometotheweb/rmmway:<version>
```

## For developers

This README is aimed at deploying and using RMMWay. If you're contributing:

- **`DEVELOPER.md`** — repo layout, local dev workflow, and the full test /
  e2e matrix
- **`TASKS.md`** — the project's shared task board (claim a task before coding)
- **`DEBUG.md`** — engineering bug-review notes
