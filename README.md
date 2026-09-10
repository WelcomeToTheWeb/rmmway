# RMMWay

RMMWay is a self-hosted remote monitoring & management (RMM) platform. A small static agent runs on every machine you manage; a single Go server aggregates metrics, logs, alerts, and commands — and an operator web UI ties it together. Everything runs in your own infrastructure: the data, the TLS keys, and the automation all stay under your control.

## Features

- **Fleet monitoring** — live device list, continuous metrics, service status, and log collection
- **Dynamic baselining** — automatic anomaly detection against each device's own baseline
- **Alert inbox** — deduplicated alerts with auto-resolution
- **Remote management** — dispatch commands to live devices with results reported back
- **MSP-ready** — organize the fleet by client/tenant with RBAC
- **One-click enrollment** — mint a token in the UI, paste a single command on the target
- **Signed auto-updates** — agents self-update with cryptographic verification
- **Reporting & compliance** — scheduled and on-demand reports in CSV/PDF
- **Integrations** — webhooks (HMAC-SHA256 signed), SSE event stream, and client data export

## Quick Start

Requires Docker. The entire stack runs in containers with pre-built images.

### 1. Configure secrets

Copy the example environment file and set the required secrets:

```sh
cp .env.prod.example .env.prod
```

Edit `.env.prod` and set these required secrets (generate each with `openssl rand -hex 32`):

| Variable | Purpose |
| --- | --- |
| `RMMWAY_JWT_SECRET` | Signs operator JWTs and capability tokens |
| `RMMWAY_PG_PASSWORD` | Postgres/Timescale password |
| `RMMWAY_MEILI_MASTER_KEY` | Meilisearch master key |
| `RMMWAY_MINIO_PASSWORD` | MinIO root password |
| `RMMWAY_ADMIN_PASSWORD` | Break-glass admin password (wizard password is primary) |

Set `RMMWAY_PUBLIC_URL` to your server's public URL (e.g. `https://rmm.example.com`) so agents can reach it and TLS certs are correct.

### 2. Start the stack

```sh
# Bundled Caddy TLS edge (ports 80/443 + 50052 for agents)
docker compose -f docker-compose.release.yml up -d

# OR: bring your own reverse proxy (ports 8080/8081 + 50052 for agents)
docker compose -f docker-compose.release-byop.yml up -d
```

The stack includes the server, frontend, TimescaleDB, NATS, Redis, MinIO, Meilisearch, and Loki. On first run it automatically pulls images, applies database migrations, and starts all services.

### 3. Verify and log in

```sh
# Check all services are healthy
curl -fsS https://rmm.example.com/healthz
```

Open the UI in your browser. On a fresh database, a first-boot setup wizard guides you through creating your primary admin account, defining your organization, and optionally configuring SMTP for email alerts.

## Adding a Device

In the operator UI: **Devices → + Add a device**. The dialog mints a one-time token and shows a single copy-paste command for your target OS. The device appears in the list the moment the agent connects.

## Teardown

```sh
docker compose down    # stop containers (keeps data)
docker compose down -v # stop + remove volumes (destructive)
```

## For Developers

This README is aimed at deploying and using RMMWay. If you're building, testing, or extending RMMWay, see:

- **[DEVELOPER.md](DEVELOPER.md)** — repo layout, local dev workflow, test/e2e matrix, env knobs, and the full test suite
- **[TASKS.md](TASKS.md)** — the project's shared task board

## Architecture Overview

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

- **Enrollment** is one-time, per device, over your HTTPS origin using a short-lived token
- **Uplink** uses mutual TLS against your organization's own CA
- In production, only ports 443 (UI/API) and 50052 (agent mTLS) are exposed publicly
