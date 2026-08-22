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

## Conventions

- Claims: `TASKS.md` is the coordination of record — claim before coding.
- Generated code (`server/gen`, `agent/gen`) stays out of git.
- One task at a time; commit the claim before code.
