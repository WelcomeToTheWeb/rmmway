# Phase 2 MVP Task Board
Shared task board for Phase 2: Production Server Setup, Frontend UX, and Advanced Integrations.

## How to use this board
1. **Claim a task** before starting. Change Status to `🔵 claimed` and add your handle and start date.
2. **Commit + push** before writing code. Your claim is your lock.
3. **Finish a task** by changing Status to `✅ done`, filling the Done date, and linking the PR.
4. **Blocked?** Set Status to `⛔ blocked` and add a `Blocked on:` line.

## Concurrency for 2 Engineers
To avoid collisions, Engineer 1 and Engineer 2 should ideally own different tracks, or coordinate at the Track level before diving into individual tasks.
*   **Suggested Split:** Engineer 1 focuses on Track A (Server/DevOps) and Track C (AI/Integrations). Engineer 2 focuses on Track B (Frontend UX).
*   **Alternative:** Both swarm Track A to get production infrastructure up, then split B and C.

---

## Track A: Production Server & DevOps
*Goal: Move from local dev to a hardened, deployable production environment.*

### A-1 — Production Docker Compose & Reverse Proxy
- **Status:** ✅ done
- **Claimed by:** WelcomeToTheWeb
- **Started:** 2026-08-26
- **Done:** 2026-08-26 (commit ecaf661)
- **Depends on:** —
- **Effort/Impact:** M / High
**Description:** Create a hardened `docker-compose.prod.yml` encompassing Postgres, NATS, Meilisearch, and the backend. Integrate Caddy or Traefik as a reverse proxy for automatic Let's Encrypt TLS on the public operator API, completely isolating it from the mTLS gRPC agent port. 
**Definition of done:** `docker compose -f docker-compose.prod.yml up` boots a secure stack with valid TLS on the frontend/API, ready for a public IP.

**Additional step — bring-your-own reverse proxy (self-hosting):** A self-hoster may run their own reverse proxy (Nginx, Traefik, HAProxy, or their own Caddy) in front of the stack instead of the bundled Caddy edge. The `caddy` service sits behind an opt-in compose profile (`profiles: ["edge"]`); the default `make prod` passes `--profile edge` so Caddy still comes up, while a self-hoster runs `make prod-byoproxy` (base file + `docker-compose.byoproxy.yml`, no `edge` profile) — freeing host ports 80/443 — which publishes the SPA on `${RMMWAY_FRONTEND_PORT:-8081}:8080` and (optionally) the API on `${RMMWAY_HTTP_PORT:-8080}:8080`. The frontend container also reverse-proxies `/api/*`, `/agent/*` and `/healthz*` to the server, so the **simplest** setup is to point the whole proxy at the SPA port (single origin; SSE buffering off, `X-Forwarded-Proto`/`X-Forwarded-For` preserved) — alternatively route `/api/*` straight to the API port. (A containerized proxy on the shared `rmmway-prod` network can proxy `frontend:8080` directly, no host ports.) The mTLS gRPC agent port (`RMMWAY_AGENT_MTLS_PORT`, default 50052) is published directly by the server either way and is unchanged. Documented in the README "Production deployment" section (Nginx + Caddy-on-host examples) and `.env.prod.example`.
**Definition of done (BYO proxy):** `make prod-byoproxy` boots the stack with the bundled Caddy off; pointing a proxy at the SPA port serves the UI + API over TLS (including the live SSE stream and the first-boot setup wizard), and agents still reach `<domain>:50052` unchanged.

### A-2 — First-Boot Setup Wizard
- **Status:** ✅ done
- **Claimed by:** WelcomeToTheWeb
- **Started:** 2026-08-26
- **Done:** 2026-08-26 (commit 5ba91a1)
- **Depends on:** A-1
- **Effort/Impact:** S / Medium
**Description:** Build an initialization state for the server. On first boot, the UI redirects to a setup wizard to mint the initial root admin credentials, define the organizational CA structure, and configure the SMTP outbox.
**Definition of done:** A fresh database triggers the setup flow; subsequent boots bypass it.

_Proof: `make setup-e2e` (real server vs scratch Timescale — fresh DB triggers the wizard, one POST mints the root admin + re-issues the org CA under the org name + persists/verifies the SMTP outbox, the mTLS trust pool live-swaps, and a second boot over the same DB restores the re-issued root and bypasses the wizard; an already-enrolled deployment is grandfathered and the root is never swapped under leaves) + `make setup-ui-smoke` (jsdom drives the real <App/>: fresh DB -> wizard -> complete -> auto-sign-in -> wizard gone). Verified on the prod stack: an upgraded deployment boots `setup: grandfathered`._

### A-3 — Automated State Backup & Restore CLI
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** A-1
- **Effort/Impact:** M / High
**Description:** Implement a cron-based backup service in Go that bundles the Postgres dump, Timescale continuous aggregates, and the CA keys, pushing the bundle to MinIO/S3. Add an `rmmway-server restore <bundle>` command.
**Definition of done:** A scheduled backup runs, and a destroyed database is fully recovered from the resulting bundle.

---

## Track B: Frontend UX & Fleet Management
*Goal: Make the telemetry actionable and reactive.*

### B-1 — SSE Integration for Reactive UI
- **Status:** 🔵 claimed
- **Claimed by:** HermesAgent
- **Started:** 2026-08-30
- **Done:** —
- **Depends on:** Phase 1 W6-2
- **Effort/Impact:** M / High
**Description:** Wire the React/Tauri app to consume the Server-Sent Events (SSE) framework. Device status changes (online/offline) and new alerts should reflect in the DOM immediately without polling.
**Definition of done:** A device going offline updates the UI badge instantly across all open operator sessions.

_Progress (in flight):_
- [x] Audit: W6-2 SSE framework (server) + `sse.js` + Devices/badge wiring already landed in 06a8f43 — verified end to end
- [x] Gap closed: Alerts inbox now reacts to live alert events (App `alertTick` → `Alerts liveTick`), not just the nav badge
- [x] DoD proof: `make sse-ui-smoke` — real `<App/>` in TWO operator sessions (jsdom): offline flip in 24 ms, alert badge+inbox in 28 ms, far inside the 5 s / 15 s polls
- [ ] Final verification (build + existing smokes) + close-out (done date + commit)

### B-2 — Dynamic Device Grouping & Bulk Actions
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** B-1
- **Effort/Impact:** M / Medium
**Description:** Implement custom tagging in the UI backed by Meilisearch. Allow operators to select a group (e.g., `tag:windows-servers`) and dispatch a capability-gated script or playbook to the entire cohort.
**Definition of done:** Operator can filter to a specific tag and execute a single command that fans out to all matched agents.

### B-3 — Customizable Telemetry Dashboards
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** B-1
- **Effort/Impact:** L / High
**Description:** Build a drag-and-drop dashboard view. Allow pinning specific Timescale metrics (e.g., fleet-wide CPU average, disk space outliers) as visual widgets next to the alert inbox.
**Definition of done:** A user can create a custom dashboard layout containing at least 3 distinct time-series charts that persist across sessions.

---

## Track C: AI & ChatOps Extensions
*Goal: Accelerate operator workflows with local, privacy-respecting LLMs.*

### C-1 — Model Context Protocol (MCP) Server Integration
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** A-1
- **Effort/Impact:** M / High
**Description:** Wrap the RMM operator API into a standard MCP server interface. Secure the endpoint using HTTP bearer token authentication so it can be safely added to Claude Desktop configurations. Expose tools for querying device status, reading alerts, and executing scoped playbooks.
**Definition of done:** Claude Desktop, configured with the RMM's MCP URL and bearer token, can successfully query "Which servers have disk space alerts?" and return accurate data from Meilisearch/Timescale.

### C-2 — Local LLM Log Analysis Pipeline (vLLM)
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** Phase 1 W6-1
- **Effort/Impact:** L / High
**Description:** Build an inference pipeline to summarize complex Loki crash logs and Timescale anomaly data. Provide a Docker Compose service definition for `intel/llm-scaler-vllm` to run local, privacy-safe models (e.g., Qwen3.8-27B-INT4). Ensure the pipeline can utilize multi-GPU VRAM pools for speculative decoding and fast context processing.
**Definition of done:** An alert in the RMM UI features an "AI Root Cause" button that queries the local vLLM container and streams back a technical summary of the surrounding logs and metrics.
