# RMMWay

Self-hosted RMM (remote monitoring & management): static Go agents, a Go/gRPC
server on TimescaleDB + NATS + Redis + Meilisearch, React/Tauri frontend.
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
make dev        # boots TimescaleDB, NATS (JetStream), Redis, MinIO, Meilisearch
                # and blocks until all 5 report healthy
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
| 5432 | TimescaleDB — device registry + `metrics` hypertable + 1-min rollups |
| 7700 | Meilisearch — device search index (Cmd-K palette backing) |

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

## Conventions

- Claims: `TASKS.md` is the coordination of record — claim before coding.
- Generated code (`server/gen`, `agent/gen`) stays out of git.
- One task at a time; commit the claim before code.
