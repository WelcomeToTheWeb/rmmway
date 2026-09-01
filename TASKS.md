# Phase 2 MVP Task Board
Shared task board for Phase 2: Production Server Setup, Frontend UX, Frontend Parity, and Advanced Integrations.

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
- **Status:** ✅ done
- **Claimed by:** HermesAgent
- **Started:** 2026-08-30
- **Done:** 2026-08-30 (commit db104d2; server SSE framework from 06a8f43)
- **Depends on:** Phase 1 W6-2
- **Effort/Impact:** M / High
**Description:** Wire the React/Tauri app to consume the Server-Sent Events (SSE) framework. Device status changes (online/offline) and new alerts should reflect in the DOM immediately without polling.
**Definition of done:** A device going offline updates the UI badge instantly across all open operator sessions.

_Proof: `make sse-ui-smoke` (jsdom drives the real <App/> in TWO operator sessions: each signs in through the real Login form and opens its own live SSE stream with the operator JWT; a device going offline flips both sessions' status badge in ~20 ms and a new alert bumps both sessions' nav badge AND the open inbox in ~20–30 ms — far inside the 5 s device poll / 15 s alert-counts / 10 s inbox polls, so the updates are provably from the stream, not polling) + `make webhook-e2e` (server half of the DoD: the offline sweeper's inventory event journals + streams over real SSE, device-scoped catch-up filter verified)._

### B-2 — Dynamic Device Grouping & Bulk Actions
- **Status:** ✅ done
- **Claimed by:** HermesAgent
- **Started:** 2026-08-30
- **Done:** 2026-08-31 (commit 7fc26de)
- **Depends on:** B-1
- **Effort/Impact:** M / Medium
**Description:** Implement custom tagging in the UI backed by Meilisearch. Allow operators to select a group (e.g., `tag:windows-servers`) and dispatch a capability-gated script or playbook to the entire cohort.
**Definition of done:** Operator can filter to a specific tag and execute a single command that fans out to all matched agents.

_Proof: `make groups-e2e` (in-process server + 3 devices with real mTLS identities, 2 online / 1 offline: the operator tags the cohort via `PATCH /api/devices/{id}`, filters to the `tag:web` group, and ONE `POST /api/devices/bulk/commands` run_script fans out — both online agents verify their OWN per-device capability token and execute exactly once (SUCCEEDED), the offline device is reported in `offline[]` (not faked); empty group → 404; reboot fan-out → 403 because the session lacks `rmmway.reboot`, nothing dispatched). The harness caught a real bug in the first pass: the bulk route reused one action struct for the whole cohort, and the dispatcher stamps the capability token into it in place — every queued command ended up carrying the LAST device's token and the agents REFUSED them as misbound; fixed by building a fresh action per device) + `make groups-ui-smoke` (jsdom drives the real <App/>: tag a cohort through the per-device tag editor (PATCH with the full tag list per device), narrow the device list with `tag:web` (exact group, 2 of 3), then "Dispatch to group" fires ONE bulk command — the request carries tag/action/base64 script and the result panel reports 2 matched · 1 pushed (device → command id) · 1 offline). Server: Meilisearch `tags` filterable + `/api/search?tag=…` / `tag:<name>` exact-group queries. All four UI smokes, both Go modules (build + test) and the vite production build are clean._

### B-3 — Customizable Telemetry Dashboards
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —
- **Done:** —
- **Depends on:** B-1
- **Effort/Impact:** L / High
**Description:** Build a drag-and-drop dashboard view. Allow pinning specific Timescale metrics (e.g., fleet-wide CPU average, disk space outliers) as visual widgets next to the alert inbox.
**Definition of done:** A user can create a custom dashboard layout containing at least 3 distinct time-series charts that persist across sessions.

_Progress: the per-device metrics viewer (first step) is done — `GET /api/devices/{id}/metrics` (series picker: the (name, source) series a device has reported) + `GET /api/devices/{id}/metrics/series?name=&source=&range=1h|6h|24h|7d|30d` (server-side `time_bucket` averaging over the raw hypertable, so every range stays a few hundred points) + a Metrics panel in the device detail row (series picker, range selector, SVG line chart with now/min/max, 30s auto-refresh). Proof: `make metrics-ui-smoke` (jsdom drives the real <App/>: picker from /metrics, chart from /metrics/series, range switch, name+source on per-source series, empty state) + `TestPostgresMetricsView` (real Timescale: picker list + bucketed series) + `TestDeviceMetricsEndpoints` (auth gate, 400/404/503, payload shape). The drag-and-drop fleet dashboard itself is still pending._

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

---

## Track D: Frontend Parity — Exposing Built Backend Features
*Goal: Wire already-built and tested backend API surfaces into the operator UI so every documented feature is reachable without reading source code.*

> **How this differs from Track B:** Track B builds new frontend capabilities (dashboards, SSE reactivity). Track D closes the gap where the backend is complete but the UI was never built — lower risk, faster ROI, no schema or protocol changes.

### D-1 — Command Results & History View
- **Status:** ✅ done
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** 2026-09-01 (commit 50da089; fix 186f00a)
- **Depends on:** —
- **Effort/Impact:** M / High
**Description:** In the device detail panel, add a "Commands" tab (or section below the existing dispatch form) that lists all commands ever dispatched to that device, newest first. Each row shows: command ID, action type (run_script / reboot), timestamp dispatched, current status (PENDING / RUNNING / SUCCEEDED / FAILED / TIMEOUT), and a expandable detail area showing the agent's reported output (stdout/stderr or exit code). Wire to `GET /api/devices/{id}/commands` (already registered in `httpapi.go` deviceSub handler). Add the corresponding `api.commands(token, deviceId, {limit})` method to `api.js`. Auto-refresh the list on SSE events matching the device's command category, and provide a manual "Refresh" button as fallback.
**Definition of done:** Operator dispatches a reboot via the existing form; within 5 seconds (or on next SSE event) a new row appears in the Commands section with status RUNNING, then transitions to SUCCEEDED or FAILED with the agent's exit output visible on expand. Refreshing the page preserves the full history (server-persisted).

### D-2 — Event Journal Browser
- **Status:** ✅ done
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** 2026-09-01 (commit b0c61a0)
- **Depends on:** —
- **Effort/Impact:** M / High
**Description:** Add a top-level "Events" nav item (4th in the header nav, after Flows) routing to a new `#/events` page. The page shows the global event journal backed by `GET /api/events?after=<seq>&limit=200&category=&device=&type=` (server returns events with sequence > `after`, oldest-first, up to `limit`). Server-side filters: `category` (alert / inventory / automation / command — validated server-side, 400 on unknown), `device` (device ID or hostname), `type` (event type within a category). Each row shows: timestamp, category badge (color-coded), device hostname, event type, one-line summary. Clicking a row expands an inline detail pane with the full event envelope JSON (pretty-printed) and a "Go to device" link. **Paging model:** on first visit the client pages forward (`after=0`, then `after=<last_seq+1>`, …) until a response returns fewer than `limit` items (the end of the journal), then displays that final batch (the most recent ≤200 entries). A "Load earlier" button sets `after` to `<first_seq_of_current_page − limit>` and fetches the preceding batch. The existing SSE stream (`sse.js`) already pushes new events for live reactivity; this page adds the *browsing, filtering, and historical paging* layer the stream alone doesn't provide. Add `api.eventJournal(token, {after, limit, category, device, type})` to `api.js`.
**Definition of done:** Operator navigates to Events, sees the most recent 200 journal entries with category color-coding; applies the `category=alert` + `device=web-01` filter to isolate one device's alert events; clicks an event row to see its full JSON envelope in the detail pane; clicks "Load earlier" to page to the next batch; a new event arriving via SSE appears at the top without a page refresh.

### D-3 — Heal Engine Dashboard
- **Status:** ✅ done
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** 2026-09-01 (claim 35bb30c, feat 61a9f7e)
- **Depends on:** —
- **Effort/Impact:** M-L / High
**Description:** Add a top-level "Heal" nav item (5th in header) routing to `#/heal`. The page has two panels: (1) **Playbooks** — a table listing all heal playbooks (`GET /api/heal/playbooks`): name, target scope (device / tag / all), trigger condition (which alert or metric threshold activates it), action (script / reboot / restart-service), enabled/disabled toggle, last-run timestamp. A "New playbook" button opens a form (name, scope picker, trigger condition, action + parameters) posting to `POST /api/heal/playbooks`. (2) **Runs** — a filterable list of heal executions (`GET /api/heal/runs?status=&device_id=&limit=`): timestamp, playbook name, target device, status (RUNNING / SUCCEEDED / FAILED / SKIPPED), duration. Filter dropdowns for status and device narrow the list server-side. Clicking a run navigates to a detail view (`GET /api/heal/runs/{id}`) showing the step-by-step execution trace: which trigger fired, what action was dispatched, the agent's response, and any error output (the server returns `{run, events[]}`). A prominent "Run Pass Now" button (`POST /api/heal/pass`) triggers an immediate evaluation across all enabled playbooks; the page shows a spinner and then refreshes the Runs panel with new entries. Add `api.healPlaybooks(token)`, `api.healCreatePlaybook(token, body)`, `api.healRuns(token, {status, device_id, limit})`, `api.healRun(token, id)`, `api.healPass(token)` to `api.js`.
**Definition of done:** Operator sees 3 pre-existing playbooks in the table; toggles one off (PATCH); clicks "Run Pass Now" and watches a new run appear in the Runs panel within 10 seconds with status transitioning from RUNNING to SUCCEEDED; clicks the run to see the full execution trace including the agent's script output.

### D-4 — Webhook Management
- **Status:** ✅ done
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** 2026-09-01 (claim 043a42a, feat c4902d6)
- **Depends on:** —
- **Effort/Impact:** M / Medium
**Description:** Add a "Webhooks" section (either a 6th nav item or a sub-section within a "Settings" page — implementer's choice, but it must be reachable from the main nav without digging). The page lists all configured webhook endpoints (`GET /api/webhooks`): name, URL, subscribed event categories (checkboxes: alert, inventory, automation, command), enabled/disabled, last delivery timestamp, consecutive failure count. A "Add webhook" form (name, URL, category checkboxes, secret for HMAC signing) posts to `POST /api/webhooks`. Each row has an "Edit" (PATCH) and "Delete" (DELETE) action, plus a "View deliveries" link that opens the per-endpoint event journal (`GET /api/webhooks/{id}/events?after=&limit=&category=`): the events this endpoint is subscribed to, oldest-first, with delivery status. A "Replay from…" button (`POST /api/webhooks/{id}/replay` with body `{"from_seq": N}`) resets the endpoint's delivery cursor to sequence N, causing the sweeper to re-deliver all events from that point forward; the UI offers "Replay all" (from_seq=0) and "Replay since last success" (from_seq = last successfully delivered seq). Add `api.webhooks(token)`, `api.webhookCreate(token, body)`, `api.webhookUpdate(token, id, body)`, `api.webhookDelete(token, id)`, `api.webhookEvents(token, id, {after, limit, category})`, `api.webhookReplay(token, id, {from_seq})` to `api.js`.
**Definition of done:** Operator adds a webhook pointing at `https://hooks.slack.com/T000/B000/000` subscribed to `alert` events with a shared secret; disables it (toggles off); clicks "View deliveries" to see the journaled events this endpoint is subscribed to (with sequence numbers and timestamps); clicks "Replay all" and sees the cursor reset confirmation (from_seq=0, last_seq reported); deletes the webhook and it disappears from the list.

### D-5 — Baseline Anomaly Explorer
- **Status:** ✅ done
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** 2026-09-01 (claim c62a58c, feat 028a0e3)
- **Depends on:** —
- **Effort/Impact:** S / Medium
**Description:** Add a "Baseline" nav item (or a tab within the Alerts page — "Alerts | Baseline" tab switcher) routing to `#/baseline`. The page shows the current anomaly score landscape: a table (`GET /api/baseline/anomalies`) with columns: device hostname, metric name, source, current value, expected range (lower–upper), anomaly score (0–1, color-coded green→yellow→red), channel (seasonal / trend), last computed timestamp. Rows are sortable by score (descending by default) so the most anomalous readings float to the top. A device+metric filter narrows the view. A "Recompute" button (`POST /api/baseline/run`) triggers a fresh baseline computation for the selected scope (all devices, or a filtered subset); the page shows a progress indicator and refreshes the table on completion. Clicking a row's device hostname navigates to that device's detail page (existing `#/devices` with focus filter). Add `api.baselineAnomalies(token, {device_id, name, min_score})` and `api.baselineRun(token, {device_id?})` to `api.js`.
**Definition of done:** Operator opens Baseline, sees all devices' current anomaly scores sorted worst-first; filters to `min_score=0.7` to see only significant deviations; clicks "Recompute" for the full fleet and watches scores update after the run completes; clicks a device name to jump to its detail page with the metrics chart visible.

### D-6 — One-Click Client Export
- **Status:** 🔵 claimed
- **Claimed by:** Sisyphus
- **Started:** 2026-09-01
- **Done:** —
- **Depends on:** —
- **Effort/Impact:** S / Medium
**Description:** In the device detail panel (`Devices.jsx`), add an "Export" button in the device's action bar (next to the existing dispatch and tag controls). Clicking it opens a small confirmation dialog: "Export all data for <hostname>? Includes inventory, raw metrics (Parquet), 1-min rollups (Parquet), and full alert history. Estimated size: ~<n> MB." (size estimate from the device's metrics row count if available, or omit). On confirm, the button transitions to a progress state ("Preparing…") while the browser fetches `GET /api/devices/{id}/export` (returns a ZIP). On completion, triggers a browser download of the ZIP file named `<hostname>-rmmway-export-<date>.zip`. Add `api.exportDevice(token, deviceId)` to `api.js` (returns a blob, not JSON — use `fetch` directly with `response.blob()` since the existing `request()` helper assumes JSON). No time-range filter in v1 (the server supports `?since=&until=` but the UI exposes the full history; a range picker is a future enhancement).
**Definition of done:** Operator clicks Export on a device with 30 days of metrics; a ZIP downloads within 10 seconds; unzipping reveals `manifest.json`, `device.json`, `metrics.parquet`, `metrics_1m.parquet`, and `alerts.json`; the manifest's SHA-256 hashes match the actual file contents (self-verifying bundle).
