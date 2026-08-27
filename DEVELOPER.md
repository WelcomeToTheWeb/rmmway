# RMMWay — Developer Guide

Deployment and usage docs live in [`README.md`](README.md). This file is for
people who build, test, or extend RMMWay. Coordination happens on the shared
task board — [`TASKS.md`](TASKS.md) (claim a task before coding); see also
[`DEBUG.md`](DEBUG.md) for engineering review notes.

## Repo layout

```
agent/       Go agent (cmd/agent)
server/      Go backend (cmd/server)
proto/       shared gRPC protocol
frontend/    React (Vite) app
scripts/     dev-stack + installer + signing helper scripts
deploy/      Caddyfiles (prod edge + frontend internal proxy)
keys/        minisign public key (the private key never lives in git)
Makefile     make dev / up / down / health / build / test / proto / prod / e2e …
```

## Local dev

Requires Docker + Go 1.24+ + Node 18+.

```sh
make dev        # boots TimescaleDB, NATS (JetStream), Redis, MinIO, Meilisearch,
                # Loki; blocks until all 6 report healthy
make run-server # Go backend on :8080 (curl localhost:8080/healthz)
make frontend   # React dev server on :5173 (proxies /api/* → :8080)
make down       # stop the stack (volumes kept)
make clean      # stop + delete volumes (destructive)
```

`/healthz` actively probes every service (Postgres query, NATS JetStream,
Redis PING, MinIO S3 API, Meilisearch health) — not just open ports.
Dev login is `admin` / `admin` (the env fallback; the first-boot wizard's
minted account is the primary login).

| Port | Service |
| --- | --- |
| 8080 | Server HTTP API — health, bootstrap-token mint, device list/search (auth-gated `/api/*`, open `/admin/*`) |
| 50051 | gRPC agent ingest — enroll + authenticated uplink (plain) |
| 50052 | gRPC mTLS agent uplink |
| 5432 | TimescaleDB — device registry, `metrics` hypertable + 1-min rollups, `log_events`, `baseline_anomalies`, `alerts`, setup/webhook tables |
| 7700 | Meilisearch — device search index (Cmd-K palette backing) |
| 3100 | Loki — agent log push + query API |

### Server env knobs (dev defaults)

| Var | Default |
| --- | --- |
| `RMMWAY_PG_DSN` | `postgres://rmmway:***@localhost:5432/rmmway?sslmode=disable` |
| `RMMWAY_MEILI_ENDPOINT` / `RMMWAY_MEILI_KEY` | `http://localhost:7700` / `` (dev instance) |
| `RMMWAY_JWT_SECRET` | random per boot (tokens rotate on restart — dev only) |
| `RMMWAY_ADMIN_USER` / `RMMWAY_ADMIN_PASSWORD` | `admin` / `admin` (env fallback) |
| `RMMWAY_HTTP_ADDR` / `RMMWAY_GRPC_ADDR` | `:8080` / `:50051` |
| `RMMWAY_BASELINE_INTERVAL` | `5m` |
| `RMMWAY_ALERT_AUTO_RESOLVE` | `on` |
| `RMMWAY_LOKI_URL` (agent) | e.g. `http://localhost:3100` |
| `RMMWAY_RELEASES_DIR` (server) | unset = no auto-updates served |
| `RMMWAY_AUTO_UPDATE` / `RMMWAY_UPDATE_INTERVAL` (agent) | `on` / `15m` |

## Test & e2e matrix

`make test` runs the unit suites; the e2e targets drive real binaries /
in-process servers against scratch databases:

| Target | Proves |
| --- | --- |
| `make e2e` | full pipeline: enroll → metric spike → baseline engine flags the spiked series → exactly 1 deduped inbox alert (re-runs bump, don't storm) → auto-resolve on recovery → fresh alert on re-spike → ack/resolve via API |
| `make setup-e2e` | first-boot wizard against a scratch DB (mint admin, re-issue org CA under the org name, SMTP to a real in-process sink, live mTLS trust-pool swap, second boot bypasses the wizard); `make setup-ui-smoke` drives the real `<App/>` through wizard → login |
| `make adddevice-e2e` | token mint → HTTP enroll over the operator origin with the plain gRPC port **dead** → real agent comes online over the mTLS port; `make adddevice-ui-smoke` covers the UI half |
| `make update-e2e` | signed release is applied (1.0.0 → 2.0.0); a tampered build is refused by the signature gate; an unsigned build is refused — previous binary intact |
| `make export-e2e` | client export: bundle verifies against its own manifest, a flipped byte is rejected, `since/until` honored, re-read by an independent Parquet reader, re-imported into a fresh DB |
| `make webhook-e2e` | real enroll + flow + alert onto the NATS journal: user endpoint receives all 3 categories, HMAC-verified (wrong secret rejected), retried (500→200), replayed, and mirrored over SSE |
| `make logs-e2e` | real agent's log lines land in Loki **and** the per-device `log_events` store (needs the stack up) |

## Key endpoints (dev stack)

```sh
# operator auth (short-lived operator JWT, distinct from agent JWTs)
curl -fsS -X POST localhost:8080/api/login -d '{"username":"admin","password":"admin"}' -H 'Content-Type: application/json'

# device list (auth-gated)
curl -fsS localhost:8080/api/devices -H "Authorization: Bearer $TOKEN"

# search (Meilisearch; /admin/search is the open mirror)
curl -fsS "localhost:8080/api/search?q=fileserver" -H "Authorization: Bearer $TOKEN"

# dispatch a command to a live stream
curl -fsS -X POST localhost:8080/api/devices/dev-…/commands \
  -H "Authorization: Bearer $TOKEN" -d '{"action":"run_script","lang":"sh","script":"…base64…"}'
# 401 no token · 404 unknown device · 400 unknown action · 502 offline · 503 search index down

# baseline engine + alerts
curl -fsS -X POST localhost:8080/admin/baseline/run
curl -fsS "localhost:8080/admin/alerts?status=open&limit=100"
curl -fsS -X PATCH localhost:8080/admin/alerts/1 -d '{"status":"acked"}'

# agent logs
curl -s 'http://localhost:3100/loki/api/v1/query_range?query={device_id="dev-…"}&limit=50'
curl -s localhost:8080/admin/devices/dev-…/events?limit=50

# verify data landed in Timescale
docker exec rmmway-timescale psql -U rmmway -d rmmway \
  -c "SELECT count(*) FROM metrics WHERE device_id='dev-…';"
```

Notes:

- `/api/*` is operator-JWT-gated; `/admin/*` mirrors stay open for machine
  callers (installers, the e2e harness).
- The Vite dev server proxies `/api/*` to `:8080`, so the browser only talks
  to `:5173`. The frontend polls `/api/devices` every 5 s and logs the
  operator out if the token becomes invalid (e.g. a server restart rotated
  `RMMWAY_JWT_SECRET`).
- Enroll round-trip (what the installer drives): `POST /api/bootstrap`
  (auth-gated) → `{"bootstrap_token","device_id"}`; the agent proves the
  token via `POST /agent/enroll` (open, machine caller) and receives
  `jwt + leaf cert/key + org root CA`. If HTTP enroll is unreachable (local
  split-port layouts) the agent falls back to the plain gRPC `Enroll` RPC;
  `--grpc-addr` / `--grpc-mtls-addr` override either endpoint.

## Building & signing releases

```sh
make build          # server + agent
make agent          # cross-compile 5 static binaries (linux/darwin/windows × amd64/arm64)
make verify-agent   # confirm binaries are static + run --version
make image          # server Docker image

MINISIGN_PASS=<pwd> make sign        # sign agent/dist/*, installers, SHA256SUMS
make verify-sigs                      # re-check everything with the public key
make sbom                             # CycloneDX SBOMs (5 agent binaries + server image)
make release-dir DIR=releases-local   # assemble release.json + binaries + .minisig
```

Key management: `keys/minisign.pub` is committed and ships in every release;
the secret key is the `MINISIGN_PRIVKEY` repo secret in CI (passphrase in
`MINISIGN_PASS`). To rotate: `go -C tools/signer run . keygen -dir keys
-pass <pwd> -force`, commit the new pub, update the secrets, run
`make pin-release-key` (re-pins the key into the agent), then re-cut the
release. The signer is a thin CLI over go-minisign (interop with
`minisign(1)`); the same library is what the agent uses for update
self-verification. The server image is signed keyless with cosign
(Sigstore TUF) in CI.

## Conventions

- `TASKS.md` is the coordination of record — claim before coding; one task
  at a time; commit the claim before code.
- Generated code (`server/gen`, `agent/gen`) stays out of git; use `make proto`.
- Production hardening (compose) lives in `docker-compose.prod.yml` +
  `deploy/Caddyfile`; BYO-proxy variant in `docker-compose.byoproxy.yml`.
