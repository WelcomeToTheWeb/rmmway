# RMMWay Operator Guide

This guide covers operating an RMMWay installation in production:
installation, configuration, device enrollment, client management,
alerting, reporting, patch management, and maintenance.

## Contents

- [Installation](#installation)
- [Configuration](#configuration)
- [Device Enrollment](#device-enrollment)
- [Client/Tenant Management](#clienttenant-management)
- [User Management & RBAC](#user-management--rbac)
- [Alerting & Flow Automation](#alerting--flow-automation)
- [Ticketing & Notifications](#ticketing--notifications)
- [Reporting & Compliance](#reporting--compliance)
- [Patch Management](#patch-management)
- [Monitoring & Maintenance](#monitoring--maintenance)
- [Backup & Disaster Recovery](#backup--disaster-recovery)
- [Security Hardening](#security-hardening)

---

## Installation

### System Requirements

- Linux server (Debian 12+ or Ubuntu 22.04+ recommended)
- 4 CPU cores minimum
- 8GB RAM minimum (16GB recommended)
- 100GB SSD storage minimum
- Docker 24+ or Go 1.24+ for building from source

### Quick Start (Docker)

```sh
# Clone the repo
git clone https://github.com/welcometotheweb/rmmway
cd rmmway

# Copy and edit the production environment file
cp .env.prod.example .env.prod
# Edit .env.prod with your secrets

# Build and start
make prod
```

The server will be accessible at the URL you configured in `RMMWAY_PUBLIC_URL`.

### First-Boot Setup

After the stack starts, visit the web UI and complete the first-boot wizard:

1. Create your root admin account
2. Configure the organization's mTLS CA (or use auto-generated)
3. Configure the SMTP outbox (or skip for now)

### Verifying Installation

```sh
# Check health
curl -fsS https://rmm.example.com/healthz

# Check server logs
docker compose logs server --tail 50
```

---

## Configuration

### Environment Variables

All configuration is via environment variables in `.env.prod`. Key variables:

| Variable | Purpose | Example |
| -------- | ------- | ------- |
| `RMMWAY_PUBLIC_URL` | Public operator URL | `https://rmm.example.com` |
| `RMMWAY_JWT_SECRET` | JWT signing secret | `openssl rand -hex 32` |
| `RMMWAY_PG_PASSWORD` | Postgres password | `openssl rand -hex 16` |
| `RMMWAY_MEILI_MASTER_KEY` | Meilisearch key | `openssl rand -hex 16` |
| `RMMWAY_MINIO_PASSWORD` | MinIO password | `openssl rand -hex 16` |
| `RMMWAY_ADMIN_USER` | Admin username | `admin` |
| `RMMWAY_ADMIN_PASSWORD` | Admin password | `openssl rand -hex 16` |
| `RMMWAY_AGENT_MTLS_PORT` | Agent mTLS port | `50052` |

### Agent Configuration

Agents are configured via environment variables or a config file:

| Variable | Purpose | Example |
| -------- | ------- | ------- |
| `RMMWAY_SERVER` | Server URL | `https://rmm.example.com` |
| `RMMWAY_AGENT_MTLS_PORT` | mTLS port | `50052` |
| `RMMWAY_HEARTBEAT_INTERVAL` | Heartbeat interval | `60s` |
| `RMMWAY_METRICS_INTERVAL` | Metric collection interval | `60s` |
| `RMMWAY_SERVICES` | Services to monitor | `nginx,postgresql` |
| `RMMWAY_AUTO_UPDATE` | Enable auto-updates | `on` |

---

## Device Enrollment

### One-Time Token Enrollment

1. In the web UI, click "Add Device"
2. Choose the operating system
3. Copy the generated command
4. Run the command on the target device

```sh
# Linux example
curl -fsSL https://rmm.example.com/install.sh | RMMWAY_SERVER=https://rmm.example.com RMMWAY_TOKEN=<token> bash
```

### Bootstrap Token API

For programmatic enrollment, mint a bootstrap token:

```sh
curl -X POST \
  -H "Authorization: Bearer <operator-token>" \
  https://rmm.example.com/api/bootstrap
```

The response contains a one-time token to use with the agent enrollment command.

### Verifying Enrollment

After enrollment, the device should appear in the device list as "online"
within 60 seconds. Verify by checking:

```sh
# List all devices
curl -s -H "Authorization: Bearer <token>" \
  https://rmm.example.com/api/devices | jq '.[] | {id, hostname, online}'
```

---

## Client/Tenant Management

### Creating a Client

```sh
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"name":"Acme Corp","address":"123 Main St"}' \
  https://rmm.example.com/api/clients
```

### Assigning Devices to a Client

```sh
curl -X PATCH \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"client_id":"<client-id>"}' \
  https://rmm.example.com/api/devices/<device-id>/client
```

### Scoping Queries by Client

```sh
# List devices for a specific client
curl -s -H "Authorization: Bearer <token>" \
  "https://rmm.example.com/api/devices?client=<client-id>"

# List alerts for a specific client
curl -s -H "Authorization: Bearer <token>" \
  "https://rmm.example.com/api/alerts?client=<client-id>"
```

---

## User Management & RBAC

### Creating Users

```sh
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"username":"technician","password":"secret","role":"technician"}' \
  https://rmm.example.com/api/users
```

### Available Roles

| Role | Permissions |
| ---- | ----------- |
| `admin` | Full access to all features |
| `technician` | Device management, alerts, tickets |
| `viewer` | Read-only access to dashboards and reports |

### MFA Setup

Users with MFA enabled can set up TOTP in their profile. Use any TOTP app
(Authy, Google Authenticator) to scan the QR code provided during setup.

---

## Alerting & Flow Automation

### Alert Rules

Alerts are generated automatically by the dynamic baselining engine. No
thresholds to configure — the engine learns each device's normal behavior
and alerts on deviations.

### Viewing Alerts

```sh
curl -s -H "Authorization: Bearer <token>" \
  "https://rmm.example.com/api/alerts?status=open" | jq '.'
```

### Acknowledging an Alert

```sh
curl -X POST \
  -H "Authorization: Bearer <token>" \
  https://rmm.example.com/api/alerts/<alert-id>/ack
```

### Flow Automation

Flows are event-driven automation chains. Create a flow via the UI or API.

Example flow: When disk usage exceeds 90% for 1 hour, send a Slack notification.

```json
{
  "name": "High disk usage",
  "triggers": [{"type": "alert", "metric": "disk.used_percent", "threshold": 90}],
  "actions": [{"type": "slack", "channel": "#alerts", "message": "High disk: {{device.hostname}}"}]
}
```

---

## Ticketing & Notifications

### Creating Tickets

```sh
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"device_id":"<id>","subject":"Printer offline","priority":"high"}' \
  https://rmm.example.com/api/tickets
```

### Notification Channels

Configure notification channels in Settings:

- **Email:** Uses the SMTP outbox
- **Slack:** Requires a webhook URL
- **Teams:** Requires a webhook URL
- **PagerDuty:** Requires service API key

### Notification Policies

Policies route alerts to specific channels based on severity, client, or
metric type. Configure via the UI.

---

## Reporting & Compliance

### Generating Reports

```sh
# Fleet status report
curl -X POST \
  -H "Authorization: Bearer <token>" \
  https://rmm.example.com/api/reports/fleet-status

# Device report
curl -X POST \
  -H "Authorization: Bearer <token>" \
  "https://rmm.example.com/api/reports/device/<device-id>"

# Patch compliance report
curl -X POST \
  -H "Authorization: Bearer <token>" \
  https://rmm.example.com/api/reports/patch-compliance
```

### Scheduled Reports

Configure report schedules in the Reports UI. Reports can be delivered
via email or stored in the report history.

### Available Report Types

| Report | Description |
| ------ | ----------- |
| Fleet Status | Overview of all devices, online/offline status |
| Device Report | Detailed metrics and history for a single device |
| Patch Compliance | Installed vs. available patches by device/client |
| License Compliance | Installed software versions and license tracking |
| Uptime/SLA | Per-client uptime and SLA compliance |

---

## Patch Management

### Querying Available Patches

```sh
# Query Windows Update for available patches
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -d '{"action":"patch_query"}' \
  https://rmm.example.com/api/devices/<device-id>/commands
```

### Installing Patches

```sh
# Approve and install specific patches
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -d '{"action":"patch_apply","patch_ids":["KB123456"]}' \
  https://rmm.example.com/api/devices/<device-id>/commands
```

### Patch Policies

Configure patch policies in the UI:

- Auto-approve critical security patches
- Schedule reboots during maintenance windows
- Roll back patches on failure

---

## Monitoring & Maintenance

### Server Health

```sh
# Overall health check
curl -fsS https://rmm.example.com/healthz

# Individual service health
docker compose ps
```

### Log Management

```sh
# Server logs
docker compose logs server --tail 100

# Agent logs (via Loki)
curl -s "https://rmm.example.com/api/devices/<device-id>/events?level=error"
```

### Database Maintenance

```sh
# Vacuum TimescaleDB
docker compose exec timescale psql -U rmmway -d rmmway -c "VACUUM ANALYZE;"

# Retention policy (delete metrics older than 90 days)
docker compose exec timescale psql -U rmmway -d rmmway -c \
  "SELECT add_retention_policy('metrics', INTERVAL '90 days');"
```

---

## Backup & Disaster Recovery

### Database Backup

```sh
# Backup TimescaleDB
docker compose exec timescale pg_dump -U rmmway rmmway > rmmway-backup.sql

# Restore
cat rmmway-backup.sql | docker compose exec -T timescale psql -U rmmway rmmway
```

### File Storage Backup

```sh
# Backup MinIO (using mc)
mc alias set local http://localhost:9000 rmmway rmmway-dev-secret
mc cp --recursive local/rmmway-backup ./minio-backup
```

### Complete Backup

```sh
# Backup all volumes
docker compose down
tar -czf rmmway-full-backup.tar.gz rmmway-timescale-data rmmway-nats-data \
  rmmway-redis-data rmmway-minio-data rmmway-meili-data rmmway-loki-data
docker compose up -d
```

---

## Security Hardening

### TLS Configuration

The bundled Caddy edge automatically obtains Let's Encrypt certificates.
Ensure `RMMWAY_PUBLIC_URL` is set correctly for DNS challenge validation.

### Firewall Rules

Open only necessary ports:

| Port | Protocol | Purpose |
| ---- | -------- | ------- |
| 80 | TCP | HTTP (Let's Encrypt challenge) |
| 443 | TCP | HTTPS (operator UI/API) |
| 50052 | TCP | Agent mTLS gRPC |

All other ports should be internal only.

### Secret Rotation

Rotate secrets annually or after suspected exposure:

```sh
# Generate new JWT secret
openssl rand -hex 32

# Update .env.prod and restart
docker compose up -d --build
```

### Regular Updates

```sh
# Update agent binaries
make agent && make sign

# Update server
docker compose pull server
docker compose up -d server
```

---

*This operator guide is part of the M5 release documentation package.*
