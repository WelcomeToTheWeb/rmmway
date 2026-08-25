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

## Agent lifecycle

1. **Bootstrap** — `POST /admin/bootstrap/mint` (admin) issues a one-time
   token → pre-paired `device_id` + short-TTL JWT (30 min, single use).
2. **Enroll** — `Enroll` RPC over gRPC, idempotent; server returns the
   long-lived agent JWT + per-device schedule.
3. **Uplink** — authenticated stream; server validates the JWT *and* that
   the device_id is already enrolled (enroll is the identity root).
   Heartbeats/commands/metric batches flow over it; `StreamMetrics` batches
   up to 500 samples / 5s before flushing to the sink.

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
| `RMMWAY_ADMIN_USER` | `admin` (operator UI login username) |
| `RMMWAY_ADMIN_PASSWORD` | `admin` (operator UI login password) |
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
