# RMMWay

Self-hosted RMM (remote monitoring & management): static Go agents, a Go/gRPC
server on TimescaleDB + NATS + Redis + Meilisearch + Loki, React/Tauri frontend.
Strategy: [`IDEA.md`](IDEA.md) · Task board: [`TASKS.md`](TASKS.md).

## Layout

```
agent/       Go agent (cmd/agent) — W1-1..W1-4
server/      Go backend (cmd/server) — W1-5+
proto/       shared gRPC protocol — W0-2
frontend/    React (Vite) app — W2-1+
scripts/     dev-stack helper scripts
Makefile     make dev / up / down / health / build / test / proto
```

## Local dev

Requires Docker + Go 1.24+ + Node 18+.

```sh
make dev        # boots TimescaleDB, NATS (JetStream), Redis, MinIO, Meilisearch, Loki
                # and blocks until all 6 report healthy
make run-server # Go backend on :8080 (curl localhost:8080/healthz)
make frontend   # React dev server on :5173 (proxies /healthz → :8080)
make down       # stop the stack (volumes kept)
make clean      # stop + delete volumes (destructive)
```

`/healthz` actively probes every service (Postgres query, NATS JetStream,
Redis PING, MinIO S3 API, Meilisearch health) — it's the W0-1 DoD check.

| Port | Service |
| --- | --- |
| 8080 | Server HTTP API — health, bootstrap-token mint, device list/search (admin) |
| 50051 | gRPC agent ingest — enroll + authenticated uplink |
| 5432 | TimescaleDB — device registry + `metrics` hypertable + 1-min rollups + `log_events` (W6-1) |
| 7700 | Meilisearch — device search index (Cmd-K palette backing) |
| 3100 | Loki — agent log push + query API (W6-1) |

## Getting all components up

Everything runs locally on defaults — the server's built-in defaults point at
the compose stack, and the admin login is `admin` / `admin`. Four components,
three terminals:

```sh
# Terminal 1 — backing services (Timescale, NATS JetStream, Redis, MinIO,
# Meilisearch, Loki); blocks until all 6 report healthy
make dev

# Terminal 2 — server (applies pending SQL migrations on boot)
make run-server     # HTTP :8080 · gRPC :50051 · mTLS :50052

# Terminal 3 — frontend
make frontend       # http://localhost:5173 — login admin / admin
```

The "everything is up" signal is `/healthz` — it returns 200 only when all
five backing services answer (active probes, not just open ports):

```sh
curl -fsS localhost:8080/healthz | python3 -m json.tool
# {"ok": true, "version": "…", "probes": [{"service": "timescale", "ok": true}, …]}
```

| Component | Start | What it does |
| --- | --- | --- |
| TimescaleDB | `make dev` | device registry + `metrics` hypertable + rollups |
| NATS (JetStream) | `make dev` | event bus for automation (W5/W6) |
| Redis | `make dev` | probed by `/healthz`; reserved for dispatch/session work |
| MinIO | `make dev` | S3 blob store |
| Meilisearch | `make dev` | device search index (Cmd-K palette) |
| Loki | `make dev` | agent log push + query API (W6-1) |
| Server (Go) | `make run-server` | gRPC ingest + HTTP API + baseline/alert engine |
| Frontend (React) | `make frontend` | operator UI |

Teardown: `make down` (volumes kept) or `make clean` (volumes deleted).

## Production stack (A-1)

A hardened, single-command production deployment: `docker-compose.prod.yml`
builds the server + frontend images and boots the whole stack behind a
Caddy edge with automatic TLS.

```sh
cp .env.prod.example .env.prod   # set RMMWAY_DOMAIN + the 5 required secrets
make prod                         # == docker compose --env-file .env.prod --profile edge -f docker-compose.prod.yml up -d --build
```

Public surface (the only host ports):

| Port | What | Notes |
| --- | --- | --- |
| `80` | Caddy | HTTP → HTTPS redirect, ACME HTTP-01 challenge |
| `443` (+udp) | Caddy | TLS operator API + frontend; Let's Encrypt for a public domain, Caddy's internal CA for `localhost` |
| `50052` (=`RMMWAY_AGENT_MTLS_PORT`) | server, raw TCP | mTLS gRPC agent channel (W3-1), published directly — Caddy's Caddyfile has no L4/TCP passthrough. Org-CA mTLS is end-to-end; this port is completely isolated from the operator API on 443 |

Everything else (Timescale 5432, NATS 4222, Redis 6379, MinIO 9000,
Meilisearch 7700, Loki 3100, server 8080/50051) stays on the internal
`rmmway-prod` network with no host ports — the plain gRPC bootstrap port
(50051) is reachable only from inside.

Hardening: pinned images, `restart: unless-stopped`, `no-new-privileges`,
caps dropped to the minimum per service, secrets only via `.env.prod`
(git-ignored; never baked into images). The mTLS server cert's SANs include
`RMMWAY_DOMAIN` (via `RMMWAY_GRPC_MTLS_SANs`) so remote agents' hostname
verification passes when they dial `<domain>:50052`.

```sh
# verify
openssl s_client -connect <host>:443 </dev/null 2>/dev/null | openssl x509 -noout -issuer -subject -ext subjectAltName
curl -fsSk https://<domain>/healthz          # all probes ok
# agents: point at <domain>:50052 (mTLS), enroll as usual
make prod-down        # stop (volumes kept)     make prod-clean  # stop + delete volumes
```

For a public IP without a DNS name, Caddy ≥ 2.8 can issue Let's Encrypt
certs for the bare IP; a real hostname is still recommended.

### Bring your own reverse proxy (self-hosting)

The bundled Caddy edge is optional. If you'd rather terminate TLS with your
own proxy (Nginx, Traefik, HAProxy, or your own Caddy) — e.g. it already
fronts other services or you manage certs centrally — bring it up **without**
the bundled Caddy and point your proxy at the stack:

```sh
make prod-byoproxy
# == docker compose --env-file .env.prod \
#      -f docker-compose.prod.yml -f docker-compose.byoproxy.yml up -d --build
```

`prod-byoproxy` leaves the bundled Caddy **off** (its `edge` profile is not
selected, so host ports 80/443 are free) and publishes host ports for your
proxy to forward to (override in `.env.prod`):

| Host port | Env | Reaches | What |
| --- | --- | --- | --- |
| `8081` | `RMMWAY_FRONTEND_PORT` | `frontend:8080` | the operator SPA **plus** `/api/*`, `/agent/*`, `/healthz*` (the frontend proxies those to the backend) |
| `8080` | `RMMWAY_HTTP_PORT` | `server:8080` | the operator API directly (optional — only if you'd rather route `/api/*` straight to the backend) |

**Simplest setup (recommended): point your whole proxy at the SPA port.**
The frontend container serves the SPA *and* reverse-proxies `/api/*`,
`/agent/*` and `/healthz*` to the backend, so the SPA's same-origin API calls
work through a single upstream — no route splitting required. (This is what
makes first-boot setup, login, and the live SSE stream all work from one
origin.)

**Nginx (host-level, Let's Encrypt via certbot) — single upstream:**

```nginx
server {
    listen 443 ssl; http2 on;
    server_name rmm.example.com;
    ssl_certificate     /etc/letsencrypt/live/rmm.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/rmm.example.com/privkey.pem;

    # Everything (SPA + /api + /agent + /healthz + SSE) goes to the frontend,
    # which proxies the API to the backend itself.
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

**Caddy on the host — single upstream** (automatic Let's Encrypt; SSE works
out of the box):

```caddyfile
rmm.example.com {
    reverse_proxy 127.0.0.1:8081
}
```

**Alternative — route `/api/*` directly to the backend** (skips the frontend
hop; use `RMMWAY_HTTP_PORT` for the API, `RMMWAY_FRONTEND_PORT` for the SPA):

```caddyfile
rmm.example.com {
    handle /api/*     { reverse_proxy 127.0.0.1:8080 }
    handle /agent/*   { reverse_proxy 127.0.0.1:8080 }
    handle /healthz*  { reverse_proxy 127.0.0.1:8080 }
    handle            { reverse_proxy 127.0.0.1:8081 }
}
```

The mTLS gRPC agent channel (`RMMWAY_AGENT_MTLS_PORT`, default 50052) is
published directly by the server either way and is **unchanged** — agents
dial `<domain>:50052` as usual, and `RMMWAY_DOMAIN` still has to be the
public hostname (it's stamped into the mTLS cert SANs).

**Containerized proxy instead of host ports:** if your proxy runs as a Docker
container, attach it to the shared `rmmway-prod` network and proxy
`frontend:8080` directly (single upstream — it proxies the API itself; or use
`server:8080` for `/api/*` + `frontend:8080` for the SPA). Then you don't need
the `docker-compose.byoproxy.yml` host-port publishing at all (just run the
base file without the `edge` profile).

Manage the BYO stack with `make prod-byoproxy-down` / `prod-byoproxy-clean` /
`prod-byoproxy-logs`.

## First-boot setup wizard (A-2)

A **fresh database is not yet initialized.** On first boot the operator UI
redirects to a setup wizard instead of the login screen. The wizard runs
exactly once per database; every subsequent boot reads the persisted state
and goes straight to the app.

It does three things, all persisted to Timescale in a single transaction
(`0009_setup.sql` → `server_setup`, `admin_users`, `server_config`):

1. **Mints the initial root admin** — a username + password (PBKDF2-hashed,
   never stored in plaintext). This is the account you sign in with.
2. **Defines the organization CA** — the org name you enter is stamped into
   the org root CA (`Subject.O`) that every agent pins for mTLS. The wizard
   re-issues the boot-time root under your org name. This only happens on a
   truly fresh install: a deployment that already has enrolled devices (i.e.
   predates the wizard) is treated as initialized — the UI skips the wizard
   and the env-admin login stays the way in — because the root can't be
   swapped out from under the agents that pinned it. A direct
   `POST /api/setup/complete` on such a database is refused with `409`
   ("devices already enrolled") and leaves the root untouched.
3. **Configures the SMTP outbox** — optional. Host/port/from + optional auth,
   with a live "send test email" (port 587 = STARTTLS, 465 = implicit TLS,
   25 = plaintext). The test is open pre-setup and operator-gated after, so a
   running deployment can't be used as an unauthenticated open relay.

The mTLS listener's client-trust pool and the capability-token issuer pick up
the re-issued root **in place** (no listener restart): after the wizard, a
leaf signed by the new root passes a real handshake and one signed by the old
boot root no longer does.

```sh
# the flow, by hand
GET  /api/setup/status            # {available, setup:false} on a fresh db
POST /api/setup/complete          # {admin_user, admin_password, org_name, smtp{...}}
GET  /api/setup/status            # now {setup:true, org_name, admin_user, smtp_host}
POST /api/login                   # sign in with the minted credentials
```

`RMMWAY_ADMIN_USER` / `RMMWAY_ADMIN_PASSWORD` remain a **fallback** login
(dev default `admin`/`admin`) so a server with no completed wizard is still
reachable; the wizard's minted account is the primary one. On an
in-memory (Postgres-down) server the wizard is unavailable and only the env
fallback works.

**Proof:** `make setup-e2e` runs the whole real server in-process against a
scratch Timescale DB — a fresh db triggers the wizard, one POST mints the
admin + re-issues the org CA under the org name + persists the SMTP config
(delivered to a real in-process SMTP sink), the mTLS trust pool live-swaps,
login works with the minted creds (and the env fallback), and a **second boot
over the same database restores the same re-issued root and bypasses the
wizard**. The UI half is proven by `make setup-ui-smoke` (jsdom drives the
real `<App/>` through wizard → complete → auto-sign-in → wizard-gone).

## Agent lifecycle

1. **Add / bootstrap** — the operator clicks **Add a device** in the UI (or
   `POST /api/bootstrap`, auth-gated; `POST /admin/bootstrap` stays open for
   machine callers). This mints a one-time token bound to a pre-allocated
   `device_id` and the UI hands back a copy-paste install command (see below).
2. **Install + enroll** — the operator runs that one-liner on the target. The
   installer drops the static agent + a service, and the agent **enrolls over
   the operator's HTTPS origin** (`POST {server}/agent/enroll`) — a fresh agent
   has no mTLS material yet, so it proves the one-time token here. The server
   returns the long-lived agent JWT **+ the org-root mTLS leaf** (leaf cert +
   key + org root CA) in the same round-trip.
3. **Uplink** — the agent persists its identity and streams over the **mTLS
   gRPC port** (default `50052`); the server validates the JWT *and* the client
   leaf, and that the device_id is enrolled (enroll is the identity root).
   Heartbeats/commands/metric batches flow over it; `StreamMetrics` batches
   up to 500 samples / 5s before flushing to the sink.

Because enrollment runs over the operator origin, a remote machine needs only
**the server host + the mTLS gRPC port (50052) reachable** — the plain gRPC
bootstrap port (`50051`) stays internal to the deployment. (For local dev /
split-port layouts the agent falls back to the plain gRPC `Enroll` RPC if the
HTTP enroll is unreachable, and `--grpc-addr` / `--grpc-mtls-addr` still
override either endpoint.)

### Add a device (one-click)

In the operator UI: **Devices → + Add a device**. The modal mints a one-time
token and shows a single copy-paste command per OS, with the server URL
pre-filled from the current origin (editable) and the token embedded. The
device appears in the list the moment the agent connects.

```sh
# Linux / macOS (what the modal generates for a server at https://rmm.example.com)
curl -fsSL https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.sh \
  | bash -s -- --server https://rmm.example.com --bootstrap <TOKEN>
```

```powershell
# Windows (PowerShell)
iwr -useb https://raw.githubusercontent.com/welcometotheweb/rmmway/Phase-1/scripts/install.ps1 -OutFile install.ps1
powershell -ExecutionPolicy Bypass -File install.ps1 -Server https://rmm.example.com -Bootstrap <TOKEN>
```

Only `--server` / `-Server` and `--bootstrap` / `-Bootstrap` are required — the
mTLS port is derived from the server host (default `50052`). The token is
one-time and short-TTL (~30 min); re-running the installer on an already
enrolled device reuses its persisted identity (it never re-enrolls).

The two endpoints backing this (both also curl-able without the UI):

```sh
# operator mints a one-time enroll token (auth-gated)
curl -fsS -X POST -H "Authorization: Bearer <OPERATOR_JWT>" \
  https://rmm.example.com/api/bootstrap          # -> {"bootstrap_token":"bt-…","device_id":"dev-…"}
# a fresh agent proves it over the operator origin (open, machine caller)
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"bootstrap_token":"bt-…","hostname":"…","os":"linux","arch":"amd64"}' \
  https://rmm.example.com/agent/enroll           # -> {"device_id","jwt","leaf_cert_pem","leaf_key_pem","org_root_ca_pem",…}
```

### Managing the Windows agent (service)

On Windows the agent runs as a real Windows service named `RmmWayAgent`
(registered by the installer). It reports its status to the Service Control
Manager on start and auto-restarts if the process ever crashes. Useful
commands (elevated PowerShell):

```powershell
Get-Service RmmWayAgent            # status
Restart-Service RmmWayAgent        # restart
Stop-Service RmmWayAgent           # stop (leaves the registration in place)
sc.exe query RmmWayAgent           # raw SCM state + exit code
# run in the foreground to see live logs / the real error:
& "C:\Program Files\RMMWay\rmmway-agent.exe" run --config "C:\Program Files\RMMWay\agent.env"
```

If the service won't start, run the last command above — the agent prints the
underlying reason (a bad server URL, an expired/used bootstrap token, or the
mTLS port not being reachable) to the console, and the same goes to the
Application event log.

### Verify the data landed in TimescaleDB

```bash
docker exec rmmway-timescale psql -U rmmway -d rmmway \
  -c "SELECT count(*) FROM metrics WHERE device_id='dev-…';"
# 1-minute rollups (lag ~5 min by design — end_offset):
docker exec rmmway-timescale psql -U rmmway -d rmmway \
  -c "SELECT device_id, name, bucket, round(avg_value::numeric,2), n FROM metrics_1m ORDER BY 1,4 DESC;"
```

## Agent logs (W6-1)

Each agent TEEs its structured `slog` events into a JSON-lines file
(default `agent.jsonl` next to the persisted identity, override with
`RMMWAY_LOG_FILE`) and tails it, shipping every batch **two ways**:

- **Loki** (`RMMWAY_LOKI_URL`, e.g. `http://localhost:3100`) — pushed to
  `/loki/api/v1/push` with `device_id` / `job` / `level` labels, so the
  lines are queryable in the log stack;
- **the server** — as `LogBatch` frames on the existing gRPC uplink, where
  they are indexed per device in the `log_events` Timescale hypertable and
  surfaced in the RMM (`GET /api/devices/{id}/events`, newest first, level
  filter; the Devices view expands a row to show them).

Delivery is at-least-once: every entry carries a stable content-derived id
so both sinks dedup a re-sent batch (reconnect replay is a no-op). The
persisted offset (`<file>.shipoffset`) means a restart resumes where it
left off instead of re-shipping history.

```bash
# Query an agent's lines in Loki:
curl -s 'http://localhost:3100/loki/api/v1/query_range?query={device_id="dev-…",job="rmmway-agent"}&limit=50'
# Or the RMM's per-device copy:
curl -s http://localhost:8080/admin/devices/dev-…/events?limit=50
```

## Device search (Meilisearch)

Enrolled devices are indexed into Meilisearch (index `devices`, primary key
`devices_id`) the moment they enroll or (re)connect — a debounced,
single-flight hook keeps the index current, and a full re-sync runs at every
server boot (self-healing: a stale index without the right primary key is
detected and recreated). Searchable: hostname, id, IP (interfaces), tags,
os, arch, agent version.

```bash
curl "http://localhost:8080/admin/search?q=fileserver"
curl "http://localhost:8080/admin/search?q=10.0.0.9"
# empty query = browse (first 20 devices)
curl "http://localhost:8080/admin/search"
```

The response is Meilisearch's search payload (`hits[]` + `estimatedTotalHits`).
If Meilisearch is unreachable the server degrades gracefully: search returns
503 and everything else keeps working.

Server env knobs (defaults shown):

| Var | Default |
| --- | --- |
| `RMMWAY_PG_DSN` | `postgres://rmmway:***@localhost:5432/rmmway?sslmode=disable` |
| `RMMWAY_MEILI_ENDPOINT` | `http://localhost:7700` |
| `RMMWAY_MEILI_KEY` | `` (dev instance; set the master key in prod) |
| `RMMWAY_JWT_SECRET` | random per boot (tokens rotate on restart — dev only) |
| `RMMWAY_ADMIN_USER` | `admin` (env fallback admin; the first-boot wizard mints the real root admin — see A-2) |
| `RMMWAY_ADMIN_PASSWORD` | `admin` (env fallback admin password; the wizard's minted account is the primary login) |
| `RMMWAY_HTTP_ADDR` / `RMMWAY_GRPC_ADDR` | `:8080` / `:50051` |

## Operator UI (W2-1)

The React frontend (Vite, `make frontend` → `http://localhost:5173`) is a small
operator app: a login screen, then a device list with live status. The Vite dev
server proxies `/api/*` to the Go server, so the browser only ever talks to
`:5173`.

**Login** mints a short-lived operator JWT (subject `operator`, issuer
`rmmway`), distinct from the agent JWTs:

```bash
curl -fsS -X POST http://localhost:8080/api/login \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin"}'
# -> {"token":"eyJ...","expiry":"2026-08-23T05:00:00Z"}
```

**Device list** is auth-gated on that token:

```bash
TOKEN=*** -s -X POST localhost:8080/api/login -d '{"username":"admin","password":"admin"}' -H 'Content-Type: application/json' | jq -r .token)
curl -fsS http://localhost:8080/api/devices -H "Authorization: Bearer $TOKEN"
# -> [{"id":"dev-…","hostname":"…","online":true,"last_seen":"…", …}]
```

`/admin/*` (bootstrap mint, device list, search) stays open for machine callers
(the `curl|sh` installer, the e2e harness, the search examples above). The
frontend polls `/api/devices` every 5 s and re-logs the operator out if the
token goes invalid (e.g. a server restart rotated `RMMWAY_JWT_SECRET`).

## Cmd-K palette (W2-2)

From any signed-in screen, **Ctrl+K / ⌘K** (or the `Search` nav item) opens a
command palette:

- **Type a hostname, id, or IP** → debounced `GET /api/search?q=…` (the same
  Meilisearch index, now auth-gated); each hit is a device row.
- **Enter on a device row** → closes the palette and filters the device list
  to that host.
- **Run an action** — `Reboot selected device` / `Run script on selected
  device` target the highlighted device (or the top hit) and call
  `POST /api/devices/{id}/commands`, which pushes the command to the device's
  live gRPC stream via the Dispatcher.

```bash
# search (auth-gated)
curl -fsS "localhost:8080/api/search?q=fileserver" -H "Authorization: Bearer ***"
# -> {"hits":[{"id":"dev-…","hostname":"fileserver-01","ip":[…],"online":true}],…}

# dispatch a command (auth-gated)
curl -fsS -X POST localhost:8080/api/devices/dev-…/commands \
  -H "Authorization: Bearer ***" -H 'Content-Type: application/json' \
  -d '{"action":"run_script","lang":"sh","script":"ZWNobyBoaQ=="}'
# -> {"command_id":"cmd-N","device_id":"dev-…"}
```

Status codes: `401` no/bad token, `404` unknown device, `400` unknown action,
`502` device offline (no live stream), `503` search index down (degraded).

> **Scope note:** the agent *executes* dispatched commands and reports
> results back (W5-1). Today it only logs receipt — the server side of the
> dispatch path (mint → push to live stream) is what W2-2 delivers, and the
> e2e proves the command reaches an open stream end-to-end.

## Dynamic baselining engine (W2-3)

A deterministic Go background job (`server/internal/baseline`) scores every
(device, metric, source) series' latest hourly mean against **this device's
own** rolling baseline — no ML deps, no thresholds to tune:

- **Seasonal channel** — per (day-of-week, hour) slot over a 45-day
  lookback: robust z-score of the latest hourly mean vs the slot's
  median/MAD ("this CPU is 4σ above *this server's* 3pm-Friday norm").
- **Trend channel** — same-day 4-hour lookback: catches sudden shifts
  before the seasonal history has enough cells (restricted to the same
  calendar day so a normal weekly step at midnight isn't a "spike").
- **EWMA** per slot tracks the smoothed level (trend signal; alerting
  input for W2-4).
- An observation flags when either channel's z ≥ 4.0 (`DefaultZFlag`).
  Flat baselines (MAD 0) use a 1%-of-level scale floor, so "flat metric
  moved ≥ 4%" flags and normal jitter doesn't.

Findings persist to the `baseline_anomalies` hypertable (upsert keyed on
series + hour — repeated runs can't storm). The engine runs every 5 min
(`RMMWAY_BASELINE_INTERVAL`) and needs ≥ 3 same-slot observations
(`DefaultMinCells`) before the seasonal channel arms.

```bash
# force one deterministic scoring pass (also /api/baseline/run, auth-gated)
curl -fsS -X POST localhost:8080/admin/baseline/run
# -> {"anomalies":[…],"series":N,"runs":M}

# anomaly feed, newest first (also /api/baseline/anomalies, auth-gated)
curl -fsS "localhost:8080/admin/baseline/anomalies?limit=100"
```

The e2e (`make e2e`) seeds two 44-day synthetic weekly-pattern series and
asserts the engine flags exactly the spiked series' final hour (z ≥ 4,
seasonal channel) while the clean series stays quiet.

## Alert inbox (W2-4)

The engine flags a series on **every** pass while it stays anomalous — so
raw anomalies would storm. The alert layer folds them into **one deduped
inbox alert per (device, metric, source)**, enforced by a partial unique
index (`0003_alerts.sql`). Repeated anomalies on the same series bump the
existing alert (`events++`, highest `score` / most recent `value`) instead
of stacking a new row; a **re-fire** after the series returns to baseline is
a genuinely new incident and starts a fresh alert.

- **Auto-resolve** — when a series comes back on-baseline, its open alert
  auto-resolves after a short quiet streak. Disable with
  `RMMWAY_ALERT_AUTO_RESOLVE=off` so only manual triage clears the inbox.
- **Manual triage** — `open → acked | resolved`, `acked → resolved`
  (re-opening is refused). Acked alerts leave the "open" tab but stay in the
  inbox for the record.
- **Inbox UI** — the `# /alerts` route (nav item + live open-count badge)
  lists alerts with status tabs, a metric/host filter, the deviation badge,
  the dedup `×N` pass count, and inline ack/resolve actions.

```bash
# inbox (open by default), newest first (also /api/alerts, auth-gated)
curl -fsS "localhost:8080/admin/alerts?status=open&limit=100"
# badge counts
curl -fsS "localhost:8080/admin/alerts/counts"
# -> {"open":1,"acked":0,"resolved":3}

# triage
curl -fsS -X PATCH localhost:8080/admin/alerts/1 -d '{"status":"acked"}'
curl -fsS -X PATCH localhost:8080/admin/alerts/1 -d '{"status":"resolved"}'
```

The e2e (`make e2e`) walks the live pipeline: spike the current hour →
exactly **1** open alert; re-run → still 1, bumped (no storm); clean the
hour → it auto-resolves; spike again → a fresh alert; then ack/resolve via
the auth-gated API (and assert `/api/alerts` is 401 without a token, 200
with one).

## Release signing (W3-4) + SBOMs (W4-1)

Every release artifact is signed before it ships, and every artifact ships a
**CycloneDX SBOM** next to it (Go modules for the binaries, Go modules + OS
packages for the server image):

| Artifact | Scheme | Where the signature lives | SBOM |
|---|---|---|---|
| Agent binaries (5 static) | minisign (Ed25519, prehashed) | `<artifact>.minisig` next to each release asset | `<artifact>.cdx.json` (+ `.minisig`) |
| Installers (`install.sh`, `install.ps1`) | minisign | `<installer>.minisig` release asset | — |
| `SHA256SUMS` | minisign | `SHA256SUMS.minisig` release asset | — |
| Server Docker image (GHCR) | cosign / Sigstore (keyless, GitHub OIDC) | attached to the image digest in GHCR | `rmmway-server.cdx.json` release asset (+ `.minisig`), also attached to the digest in GHCR (`cosign download sbom`) |

**Verify an agent release asset** (anyone, no trust in us required):

```sh
# download rmmway-agent-linux-amd64 + rmmway-agent-linux-amd64.minisig + minisign.pub
# from a GitHub release, then:
go -C tools/signer run . verify -p minisign.pub rmmway-agent-linux-amd64
# — or with the reference minisign(1) CLI:
minisign -Vm rmmway-agent-linux-amd64 -P <line 2 of minisign.pub>
```

**Read the SBOM for an agent binary:** the `<artifact>.cdx.json` release
asset (CycloneDX 1.7 JSON, generated by a pinned [syft](https://github.com/anchore/syft)
version via `scripts/install-syft.sh`) lists the exact Go modules the binary
was built with, plus the binary's own sha256. Verify it the same way as the
binary (it has its own `.minisig`).

**Verify the server image:**

```sh
cosign verify ghcr.io/welcometotheweb/rmmway:v0.4.0   # keyless (Sigstore TUF)
cosign download sbom ghcr.io/welcometotheweb/rmmway:v0.4.0   # W4-1: SBOM attached to the digest
```

Local SBOM generation: `make sbom` (needs `make agent` + `make image`;
`make sbom-agent` skips the docker image). CI generates SBOMs on every build
(GitHub artifacts) and every release (signed release assets + GHCR attach).

Key management:

- `keys/minisign.pub` is committed and ships in every release. The secret
  key never lives in git — in CI it's the `MINISIGN_PRIVKEY` repo secret
  (passphrase in `MINISIGN_PASS`).
- Local signing: `MINISIGN_PASS=<pwd> make sign` (signs + verifies
  `agent/dist/*`, both installers, and `SHA256SUMS`). `make verify-sigs`
  re-checks everything with the public key.
- Rotating the minisign key: `go -C tools/signer run . keygen -dir keys
  -pass <new-pwd> -force`, commit `keys/minisign.pub`, update the
  `MINISIGN_PRIVKEY`/`MINISIGN_PASS` secrets, re-cut the release.
- The signer is `tools/signer` — a thin CLI over
  [go-minisign](https://github.com/jedisct1/go-minisign), so keys/signatures
  interop with the reference `minisign(1)` CLI. W4-2 reuses the same
  library in the agent itself (release self-verification before update).

## Agent auto-update (W4-2)

Agents can update themselves from the server — but only a **correctly signed**
release is ever installed. The trust anchor is the minisign **public key
pinned inside the agent binary** (`agent/internal/update/minisign.pub`, the
W3-4 release key, `019BF5A0CA5040DD`). The server is only a carrier: it serves
a release manifest + the binaries + their `.minisig` files, and the agent
verifies each download against the pinned key **before** replacing itself. A
tampered or unsigned build is refused and the running binary is left
untouched.

**How an update is gated** (all must pass, in order):

1. The manifest's `public_key` is identical to the agent's pinned key
   (a server naming a different publisher is a refusal).
2. The release version is newer than the running one (no silent downgrades).
3. The downloaded bytes match the manifest sha256 (when present).
4. The `.minisig` verifies against the pinned key (the primary gate).

Only then is the binary atomically installed (and the agent re-execs; on
Windows, where the in-use `.exe` can't be replaced in place, it is staged as
`<exe>.pending` and applied at the next start).

**Publish a release** for agents to pick up:

```sh
make agent && make sign        # build + sign agent/dist (W3-4 key)
make release-dir DIR=releases-local   # assemble release.json + binaries + .minisig
# point the server at it:
RMMWAY_RELEASES_DIR=releases-local make run-server
```

**The agent side** — automatic while `run` is live (every 15m by default), or
on demand:

```sh
rmmway-agent update --server http://rmm.local          # verify + install + re-exec
rmmway-agent update --server http://rmm.local --check  # verify only
```

Tuning: `RMMWAY_AUTO_UPDATE=off` disables the background loop,
`RMMWAY_UPDATE_INTERVAL=5m` sets its cadence, and `RMMWAY_UPDATE_PUBKEY=<pub>`
overrides the pinned key at runtime (key rotation / test harnesses).

**Proof:** `make update-e2e` builds two real agent binaries, signs one with a
fresh throwaway key using the real signer, serves it through an in-process
server, and runs the real `update` command — a valid release is applied
(1.0.0 → 2.0.0), a tampered build is refused by the **signature** gate,
and an unsigned build is refused, each leaving the previous binary intact.

## Client export (W4-3)

The no-lock-in promise: one request exports **everything RMMWay knows about a
client** into a portable, self-describing bundle — inventory + config,
raw metrics + 1-minute rollups (standard Apache **Parquet**), the complete
alert history, and a manifest that drives verification. The bundle is
portable data (Parquet + JSON), not a database dump: it opens in any
standard tool and re-imports anywhere.

**One-click export** (operator JWT, or the open `/admin` mirror for ops):

```sh
TOKEN=$(curl -s -X POST localhost:8080/api/login -d '{"username":"admin","password":"admin"}' | jq -r .token)
# full history (bounded by the raw retention window):
curl -s -H "Authorization: Bearer $TOKEN" \
  localhost:8080/api/devices/<id>/export -o client.zip
# a time window (RFC3339), rollups optional:
curl -s -H "Authorization: Bearer $TOKEN" \
  'localhost:8080/api/devices/<id>/export?since=2026-08-01T00:00:00Z&until=2026-08-15T00:00:00Z&rollups=0' \
  -o client-window.zip
```

**Bundle contents** (a ZIP):

| file | what | opens in |
|------|------|----------|
| `manifest.json` | the contract: format/version, device, per-file **sha256 + size + row count** | any JSON tool |
| `device.json` | inventory (id, hostname, os/arch, agent version, interfaces) + config (intervals, tags) | any JSON tool |
| `metrics.parquet` | raw samples: `timestamp_ms, ts, name, source, value, labels`(JSON-string) | duckdb / pandas / polars |
| `metrics_1m.parquet` | 1-minute rollups: `bucket, name, source, avg/min/max, n` (survives raw retention) | duckdb / pandas / polars |
| `alerts.json` | complete alert history (all statuses) | any JSON tool |
| `README.md` | how to open + verify + re-import | — |

**Open it with a standard tool:**

```sh
unzip client.zip
duckdb -c "SELECT * FROM 'metrics.parquet' WHERE name='cpu.utilization_percent' ORDER BY ts DESC LIMIT 100;"
python -c "import pandas as pd; print(pd.read_parquet('metrics.parquet').head())"
```

**Self-describing / tamper-evident:** `manifest.json` alone is enough to
verify the whole bundle — every file's sha256 + size + row count is checked,
and each Parquet section is re-read with a standard Parquet reader. Flip a
single byte and verification fails:

```sh
jq -r '.files[] | select(.sha256 != null) | "\(.sha256)  \(.name)"' manifest.json | sha256sum -c -
```

The Go server ships the same check (`export.Verify`), which is what the
e2e uses.

**Proof:** `make export-e2e` boots the real operator HTTP surface against a
scratch Timescale DB (one device, 2 days × 3 series = 17,280 samples,
materialized rollups, 3 alerts) and proves the full no-lock-in story: the
one-click export streams a bundle that (1) passes `export.Verify` against its
own manifest, (2) is **rejected** after a single flipped byte in
`metrics.parquet`, (3) honors `?since=&until=` exactly, (4) re-reads with an
independent standard Parquet reader (ts ≡ timestamp_ms on every row,
values + labels round-trip), and (5) **re-imports** into a fresh database
with identical row count + time range.

## Webhooks + event stream (W6-2)

The event bus is exposed to the outside world two ways: **signed webhooks**
(HMAC-SHA256, Stripe-style) to a user-defined HTTP endpoint, and a **SSE**
server-sent-events stream. Every event — **alert**, **inventory**
(device created / online / offline), and **automation** (flow triggers /
steps / notifies / command results) — is first journaled to Postgres with a
monotonic `seq`, then delivered from that journal. The journal is the source
of truth: it makes delivery **at-least-once, retryable, replayable, and
survivable across restarts** (the in-process NATS consumer is only the fast
path).

**Event envelope** (each delivery body, signed as `t=<unix>,v1=<hex>`):

```json
{"id":12,"category":"alert","type":"fired","device_id":"dev-…","at":"…","event":{ "id":3,"name":"cpu utilization high",… }}
```

**Create a user-defined endpoint** (operator JWT): the endpoint is called
once per journaled event it subscribes to, each delivery signed with the
shared `secret`. New endpoints start *now* (only new events); `categories`
filters what you receive (`["alert","inventory","automation"]`).

```sh
TOKEN=$(curl -s -X POST localhost:8080/api/login -d '{"username":"admin","password":"admin"}' | jq -r .token)
curl -s -X POST localhost:8080/api/webhooks -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"pagerduty","url":"https://hooks.example.com/rmmway","secret":"shh","categories":["alert"]}'
# -> {"id":1,"last_seq":11,"status":"ok",…}
```

**Verify the signature** (anti-tamper + anti-replay — the timestamp must be
fresh, e.g. within 5 min) with the same `secret`:

```go
ok, err := webhook.Verify([]byte("shh"), r.Header.Get("Webhook-Signature"), body)
```

**Operator surface** (JWT, open `/admin` mirror for ops):

| route | what |
|-------|------|
| `POST /api/webhooks` | create an endpoint (secret kept server-side; the secret is **not** returned after creation) |
| `GET /api/webhooks` · `GET /api/webhooks/{id}` · `PATCH` · `DELETE` | list / get / update (URL, secret, categories, enabled) / delete |
| `GET /api/webhooks/{id}/events` | this endpoint's delivery journal (newest first) |
| `POST /api/webhooks/{id}/replay` | `{"from_seq":0}` (or `{"to_seq":N}`) — re-drive the range from the journal |
| `GET /api/events/stream` · `…/{category}` | **SSE** live stream (open); last 200 catch-up, then live; `Last-Event-ID` resumes |

**Delivery semantics:** the endpoint is only advanced past an event after a
`2xx`. A non-2xx is retried with exponential backoff (1s,2s,4s… to 5m), and
after 5 consecutive failures the endpoint is **dead-lettered** (`status=error`)
while its remaining events stay in the journal (re-enable to resume, or
replay). `GET /events/stream` mirrors every journaled event live.

**Proof:** `make webhook-e2e` runs the whole real stack — a fake agent
**enrolls** (inventory), a real **flow** fires a `trigger→notify` (automation),
and the **alert** reconciler fires on an anomaly — all onto a real NATS/
JetStream bus, and asserts a user-defined endpoint received **all three
categories** with every delivery **HMAC-verified** (a wrong secret is
rejected), **retries** (500,500 → 200), **replays** (cursor reset re-drives),
and streams the same events over **SSE**.

## Conventions

- Claims: `TASKS.md` is the coordination of record — claim before coding.
- Generated code (`server/gen`, `agent/gen`) stays out of git.
- One task at a time; commit the claim before code.
