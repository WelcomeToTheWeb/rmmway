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

## Conventions

- Claims: `TASKS.md` is the coordination of record — claim before coding.
- Generated code (`server/gen`, `agent/gen`) stays out of git.
- One task at a time; commit the claim before code.
