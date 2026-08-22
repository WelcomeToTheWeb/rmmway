# RMMWay — Phase 1 MVP Task Board

> Shared task board for the Phase 1 MVP. **Multiple engineers work this in
> parallel** — the file is designed so each task is an independent block that
> edits don't collide. This is the coordination of record; the repo's
> `IDEA.md` is the strategy behind it.

---

## How to use this board (read once)

**1. Claim a task before you touch it.** Open the task, then edit ONLY that
block:

```
- **Status:** ⬜ pending      →   - **Status:** 🔵 claimed
- **Claimed by:** —           →   - **Claimed by:** @your-handle
- **Started:** —              →   - **Started:** 2026-08-22
```

Commit + push **before writing any code** (or at least before opening your
branch). The claim is your lock — nobody else picks a `🔵 claimed` task.

**2. Finish a task:** change `Status` to `✅ done`, fill `Done`, and link the
PR/commit.

```
- **Status:** 🔵 claimed      →   - **Status:** ✅ done
- **Done:** —                 →   - **Done:** 2026-08-27 (PR #12)
```

**3. Blocked?** Set `Status` to `⛔ blocked` and add a `**Blocked on:**` line
naming the task + why. Don't leave a stale `🔵` claim on a task you've parked.

**4. Stale-claim rule.** If a `🔵 claimed` task has no linked PR/commit after
**3 working days**, treat it as stale — re-claim it and note it in `Claimed by`
(e.g. `@you (re-claim from @former, no activity 3d)`). Ping the original
claimer first.

### Concurrency: why this is safe
Each task is a self-contained block with a unique ID. Two engineers editing
*different* tasks never touch the same lines, so git merges cleanly. The only
way to conflict is two people editing the *same* task — which the claim
prevents. If a conflict still happens, the rule is: **keep the latest
Status/Claimed-by/Done, and the task that is furthest along wins.**

### Status legend
| Mark | Meaning |
|------|---------|
| ⬜ `pending` | Open, unclaimed |
| 🔵 `claimed` | Someone's working it (see `Claimed by`) |
| ✅ `done`    | Complete (see `Done` + PR/commit) |
| ⛔ `blocked`| Waiting on another task / external dep |

### Progress at a glance
- **W0 Scaffolding:** 2 / 3
- **W1 The Agent:**    1 / 7
- **W2 Monitoring+UX:** 0 / 5
- **W3/W4 Trust:**     0 / 8
- **W5/W6 Automation:** 0 / 5
- **Total:** 3 / 28

> *Update the counts above as tasks close (one line each, low-conflict).*

### Suggested parallel work streams
The board splits into **independent tracks** so 3–5 engineers can go in
parallel. Within a track, respect `Depends on` ordering.

- **Track A — Agent:** W1-1 → W1-2 → W1-3 → W1-4
- **Track B — Server/Data:** W1-5 → W1-6 → W1-7
- **Track C — Monitoring+UX:** W2-1 → (W2-2 ∥ W2-3) → W2-4 → W2-5
- **Track D — Security/Supply-chain:** W3-1 → (W3-2 ∥ W3-3 ∥ W3-4) → W4-*
- **Track E — Automation:** W5-1 ∥ W5-2 → W6-1 ∥ W6-2 → W6-3

(Tracks A and B only share the W0 foundation. A and B can fully overlap.)

---

## W0 — Scaffolding  *(foundation — must land before most tracks start)*

#### W0-1 — Monorepo scaffold + local dev stack
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `e4c2785` on `main`)
- **Depends on:** —
- **Effort/Impact:** M / High
- Monorepo: Go backend + React/Tauri frontend + shared `proto/` + `Makefile`.
  `docker-compose.yml` brings up the full local stack (Postgres+Timescale,
  NATS, Redis, MinIO, Meilisearch). `make dev` boots everything.
- **Definition of done:** `make dev` → all services healthy; a trivial Go
  server + React app build and run against them.

#### W0-2 — Agent protocol (gRPC protos)
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `265c35f` on `main`)
- **Depends on:** W0-1
- **Effort/Impact:** S / High
- Define `proto/` for the agent protocol: enroll, heartbeat, metrics push,
  command dispatch. Generate Go stubs.
- **Definition of done:** `make proto` regenerates; stubs compile on both
  agent and server sides.

#### W0-3 — CI skeleton
- **Status:** 🔵 claimed
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** S / Medium
- GitHub Actions: build+test for Go agent + server, lint, Docker image
  build/push.
- **Definition of done:** a push runs lint+test+build on CI and produces an
  image.

---

## W1 — The Agent  *(Track A = agent side, Track B = server side; overlap freely)*

#### W1-1 — Static Go agent binary + cross-compile
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-1, W0-2
- **Effort/Impact:** M / High
- Single static Go binary per OS (Windows/Linux/macOS), zero runtime deps,
  builds in CI.
- **Definition of done:** 3 static binaries from CI, run on their OS, print
  version, no external shared libs (verify with `ldd`/`file`).

#### W1-2 — Core collectors
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-1
- **Effort/Impact:** M / High
- CPU, memory, per-volume disk, network, uptime.
- **Definition of done:** agent emits all five metric families over the wire.

#### W1-3 — One-line bootstrap installer
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-1
- **Effort/Impact:** M / High
- `curl | sh` (Linux/macOS) and a PowerShell one-liner (Windows).
- **Definition of done:** a clean VM goes from 0 → installed agent in one
  pasted line on each OS.

#### W1-4 — Agent self-enrollment
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-2, W1-5
- **Effort/Impact:** M / High
- Request an agent JWT from the server, persist it, report back.
- **Definition of done:** a freshly-bootstrapped agent enrolls and appears as
  a device with a valid token; restart doesn't re-enroll.

#### W1-5 — Server: gRPC agent ingest service
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `899174e` on `main`)
- **Depends on:** W0-2
- **Effort/Impact:** M / High
- Auth (JWT) + metric receive + command dispatch.
- **Definition of done:** server accepts an enrolled agent's stream and
  rejects unauthenticated agents.

#### W1-6 — Server: TimescaleDB schema
- **Status:** 🔵 claimed
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Metrics hypertable(s), devices table, continuous aggregates.
- **Definition of done:** schema migrates cleanly; metrics land in the
  hypertable and a continuous aggregate rolls up.

#### W1-7 — Device inventory → Meilisearch
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-6
- **Effort/Impact:** S / Medium
- Index devices + inventory into Meilisearch on change.
- **Definition of done:** enroll a device → it's immediately findable by name,
  IP, tag, hostname.

---

## W2 — Monitoring & UX

#### W2-1 — Frontend: app shell + device list
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** M / Medium
- Tauri/React shell, authenticated nav, device list view.
- **Definition of done:** logs in, lists live devices with status.

#### W2-2 — Cmd-K command palette
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W2-1, W1-7
- **Effort/Impact:** M / High
- Fuzzy device/script/action search over Meilisearch; type-and-go.
- **Definition of done:** `Cmd-K` + "fileserver" jumps to that device; a
  known action is runnable from the palette.

#### W2-3 — Dynamic baselining engine
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-6
- **Effort/Impact:** M / High
- Per-metric rolling baseline by day-of-week/hour; MAD/robust z-score + EWMA
  trend. Deterministic Go background job, no ML deps.
- **Definition of done:** for a synthetic metric with a known weekly pattern,
  anomalies are flagged at the right times and quiet otherwise.

#### W2-4 — Baseline-driven alerts + inbox
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W2-3
- **Effort/Impact:** M / High
- Alert generation from the baseline engine; alert inbox in the UI.
- **Definition of done:** a real metric anomaly produces one deduped alert in
  the inbox (no storm).

#### W2-5 — 🎯 MILESTONE: "monitored in 5 min" E2E demo
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-1…W1-7, W2-1…W2-4
- **Effort/Impact:** S / High
- End-to-end: bootstrap agent → live metrics → baseline alerts → near-zero
  false positives. Time it.
- **Definition of done:** on a clean machine, first monitored device in
  **≤ 5 min**; alert precision documented on a test estate. **This gate
  closes Block 1.**

---

## W3/W4 — Trust & Supply Chain

#### W3-1 — mTLS agent channel
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-2, W1-1
- **Effort/Impact:** M / High
- Org root CA + per-device leaf certs; agent↔server over mTLS.
- **Definition of done:** agent connects only with a valid leaf cert from the
  org root; a random cert is rejected.

#### W3-2 — Short-lived cert rotation
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W3-1
- **Effort/Impact:** M / High
- Leaf certs ~1h, auto-rotate, no downtime.
- **Definition of done:** certs renew automatically across an hour boundary
  without dropping the agent connection.

#### W3-3 — Per-action capability tokens
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W3-1
- **Effort/Impact:** M / High
- Time-boxed session/capability tokens; an agent can't act beyond its minted
  scope.
- **Definition of done:** a command requiring a capability it lacks is refused
  even with a valid mTLS channel.

#### W3-4 — Sign all releases
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-3
- **Effort/Impact:** M / High
- cosign/Sigstore for server; minisign for agent binaries + installers.
- **Definition of done:** CI signs every artifact; a signature file ships
  alongside each release.

#### W4-1 — CycloneDX SBOM per artifact
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-3
- **Effort/Impact:** M / High
- Generate a CycloneDX SBOM for every image/binary in CI.
- **Definition of done:** each release includes an SBOM listing Go modules +
  OS packages.

#### W4-2 — Agent verifies release signature
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W3-4
- **Effort/Impact:** M / High
- Agent checks the server's release signature before auto-updating.
- **Definition of done:** an update with a valid signature is applied; a
  tampered/unsigned build is refused (test both).

#### W4-3 — Per-client full export
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-6
- **Effort/Impact:** M / High
- One-click export of a client's inventory + metrics + config to a portable
  bundle. This is the no-lock-in promise.
- **Definition of done:** export a client → a self-describing bundle
  (e.g. Parquet + JSON) that re-imports or opens in a standard tool.

#### W4-4 — 🎯 MILESTONE: "provable trust" demo
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W3-1…W3-4, W4-1…W4-3
- **Effort/Impact:** S / High
- Verify a signed release + SBOM externally, and run a full client export.
- **Definition of done:** a skeptic can (a) verify a release signature + read
  the SBOM, and (b) export a client and confirm the data is theirs. **Closes
  Block 2.**

---

## W5/W6 — Automation & Integrations

#### W5-1 — Self-healing playbook engine
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W1-2, W1-5
- **Effort/Impact:** M / High
- State machine: detect → verify-safe → remediate → confirm → escalate.
  Idempotent, replay-safe. Starter library: disk full, service down, WSUS stuck.
- **Definition of done:** a failing condition is detected, remediated, and the
  *confirm* step re-measures; on confirm-fail it escalates (ticket+notify).

#### W5-2 — Event-driven chains over NATS
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Model automations as DAGs of triggers→actions wired over NATS; visual
  composer.
- **Definition of done:** compose `disk>90% → free → if>90% → notify` and it
  fires correctly on the synthetic trigger.

#### W6-1 — Loki log integration
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Agent tails structured events → ship to Loki; keep indexed events in
  Timescale.
- **Definition of done:** agent log lines queryable in Loki; the RMM surfaces
  recent indexed events per device.

#### W6-2 — Webhook + event-stream framework
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W0-1
- **Effort/Impact:** S / High
- Expose the NATS bus as signed webhooks (HMAC) + SSE/subscription, with
  retries and replay.
- **Definition of done:** a user-defined endpoint receives signed, replayable
  alert/inventory/automation events.

#### W6-3 — 🎯 MILESTONE: full automation E2E
- **Status:** ⬜ pending
- **Claimed by:** —
- **Started:** —  ·  **Done:** —
- **Depends on:** W5-1, W5-2, W6-1, W6-2
- **Effort/Impact:** S / High
- End-to-end: alert fires → self-heal runs + confirms → ticket opened →
  webhook fires.
- **Definition of done:** one triggered condition drives all four, audited.
  **Closes Block 3 = Phase 1 MVP.**

---

## Cut from Phase 1 (do NOT start)
- Agent mesh / gossip (risky P2P surface)
- Full RDP/VNC remote-control passthrough
- Multi-tenant cloud offering
- Reverse ETL to data warehouses
- OT/ICS monitoring
- Community marketplace (security model first, later phase)
