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
- **W0 Scaffolding:** 3 / 3
- **W1 The Agent:**    7 / 7
- **W2 Monitoring+UX:** 5 / 5
- **W3/W4 Trust:**     7 / 8
- **W5/W6 Automation:** 3 / 5
- **Total:** 25 / 28

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
- **Status:** ✅ done
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commits `ef1b63a`…`6d7babb`; CI green on `46a6c94`)
- **Depends on:** W0-1
- **Effort/Impact:** S / Medium
- GitHub Actions: build+test for Go agent + server, lint, Docker image
  build/push.
- **Definition of done:** a push runs lint+test+build on CI and produces an
  image.

---

## W1 — The Agent  *(Track A = agent side, Track B = server side; overlap freely)*

#### W1-1 — Static Go agent binary + cross-compile
- **Status:** ✅ done
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `2d3f911`; CI builds 5 static binaries, verifies `ldd`, on `46a6c94`)
- **Depends on:** W0-1, W0-2
- **Effort/Impact:** M / High
- Single static Go binary per OS (Windows/Linux/macOS), zero runtime deps,
  builds in CI.
- **Definition of done:** 3 static binaries from CI, run on their OS, print
  version, no external shared libs (verify with `ldd`/`file`).

#### W1-2 — Core collectors
- **Status:** ✅ done
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `4435ded`; all five families emitted, tested, CI-green)
- **Depends on:** W1-1
- **Effort/Impact:** M / High
- CPU, memory, per-volume disk, network, uptime.
- **Definition of done:** agent emits all five metric families over the wire.

#### W1-3 — One-line bootstrap installer
- **Status:** ✅ done
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `6a9b094`; CI-green on `6a9b094`)
- **Depends on:** W1-1
- **Effort/Impact:** M / High
- `curl | sh` (Linux/macOS) and a PowerShell one-liner (Windows).
- **Definition of done:** a clean VM goes from 0 → installed agent in one
  pasted line on each OS.

#### W1-4 — Agent self-enrollment
- **Status:** ✅ done
- **Claimed by:** @eng1way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commits `fe4d59a`, `d599f53`; CI green on `d599f53` incl. e2e enroll→uplink)
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
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `8e97b3d` on `main`; e2e enrolls a fresh device and asserts rows in Timescale)
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Metrics hypertable(s), devices table, continuous aggregates.
- **Definition of done:** schema migrates cleanly; metrics land in the
  hypertable and a continuous aggregate rolls up.

#### W1-7 — Device inventory → Meilisearch
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `042347a` on `main`; e2e enrolls a fresh device and asserts it is the top `/admin/search` hit by hostname, id, and IP; live Meilisearch tests in `internal/store`)
- **Depends on:** W1-6
- **Effort/Impact:** S / Medium
- Index devices + inventory into Meilisearch on change.
- **Definition of done:** enroll a device → it's immediately findable by name,
  IP, tag, hostname. ✅ verified: top-hit by hostname/id/IP in e2e; tag +
  IP + self-heal covered by live tests in `server/internal/store/meili_test.go`.

---

## W2 — Monitoring & UX

#### W2-1 — Frontend: app shell + device list
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `7f72ccd` on `main`; live e2e: operator login mints a JWT, `/api/devices` lists the freshly enrolled device online, agent token rejected, `/admin/*` unchanged)
- **Depends on:** W0-1
- **Effort/Impact:** M / Medium
- Tauri/React shell, authenticated nav, device list view.
- **Definition of done:** logs in, lists live devices with status. ✅ verified: `POST /api/login` (admin/admin → operator JWT), `GET /api/devices` returns the e2e-enrolled device `online:true` with last_seen; wrong creds / no token / garbage token all 401; agent-shaped JWT rejected on the operator route; React build + SSR render pass.

#### W2-2 — Cmd-K command palette
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-22 (commit `a6663a1` on `main`; live e2e: operator login + `POST /api/devices/{id}/commands` pushed through the Dispatcher and asserted on the live agent stream)
- **Depends on:** W2-1, W1-7
- **Effort/Impact:** M / High
- Fuzzy device/script/action search over Meilisearch; type-and-go.
- **Definition of done:** `Cmd-K` + "fileserver" jumps to that device; a
  known action is runnable from the palette.

#### W2-3 — Dynamic baselining engine
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-22  ·  **Done:** 2026-08-23 (live e2e: synthetic 44-day weekly-pattern series — spike flagged at the right hour with z=87.5, clean series quiet, anomaly persisted + served)
- **Depends on:** W1-6
- **Effort/Impact:** M / High
- Per-metric rolling baseline by day-of-week/hour; MAD/robust z-score + EWMA
  trend. Deterministic Go background job, no ML deps.
- **Definition of done:** for a synthetic metric with a known weekly pattern,
  anomalies are flagged at the right times and quiet otherwise.

#### W2-4 — Baseline-driven alerts + inbox
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-23  ·  **Done:** 2026-08-23 (commit `4a1fcbf` on `main`; live e2e: spike hour → exactly 1 open alert, re-run bumps it (no storm), clean hour auto-resolves, re-spike fires fresh alert, ack/resolve via auth-gated API)
- **Depends on:** W2-3
- **Effort/Impact:** M / High
- Alert generation from the baseline engine; alert inbox in the UI.
- **Definition of done:** a real metric anomaly produces one deduped alert in
  the inbox (no storm). ✅ `0003_alerts.sql` + `store.AlertStore` reconciler
  (one open alert per series, `events++` on repeats, auto-resolve on quiet
  streak), `GET/PATCH /api/alerts` (+ `/admin`), `#/alerts` inbox UI with
  open-count badge, unit + live-Postgres + e2e proof.

#### W2-5 — 🎯 MILESTONE: "monitored in 5 min" E2E demo
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-23  ·  **Done:** 2026-08-23 (commit below; live e2e: real installer + systemd + real agent binary on a clean machine — bootstrap one-liner → device online in **1s**; test estate 12×5×45d with 25 injected faults → **precision 100%, recall 100% (0 FP / 0 FN)**; live fault on the real device's own series → 1 deduped alert, no storm, auto-resolve + manual ack/resolve)
- **Depends on:** W1-1…W1-7, W2-1…W2-4
- **Effort/Impact:** S / High
- End-to-end: bootstrap agent → live metrics → baseline alerts → near-zero
  false positives. Time it.
- **Definition of done:** on a clean machine, first monitored device in
  **≤ 5 min**; alert precision documented on a test estate. ✅ `cmd/e2e/milestone`
  (self-tearing-down, reproducible) — first monitored device in ~1s from the
  bootstrap one-liner (gate ≤5m), alert precision on a 60-series/45-day estate
  with exact ground truth: 100% precision, 100% recall, two engine paths
  (in-process + live server pass) agreeing exactly; evidence + run notes in
  [`MILESTONE-W2-5.md`](MILESTONE-W2-5.md). Also added `--grpc-addr` to the
  installer (split-port HTTP/gRPC deployments). **Closes Block 1.**

---

## W3/W4 — Trust & Supply Chain

#### W3-1 — mTLS agent channel
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-23  ·  **Done:** 2026-08-23
- **Depends on:** W0-2, W1-1
- **Effort/Impact:** M / High
- Org root CA + per-device leaf certs; agent↔server over mTLS.
- **Definition of done:** agent connects only with a valid leaf cert from the
  org root; a random cert is rejected.
- **Notes:** Org root CA (ECDSA P-256) in `server/internal/ca`, persisted via
  migration `0004_org_ca.sql` (idempotent bootstrap-or-load). Enroll issues a
  1y device leaf (SANs: device_id + hostname + agent IP) and returns
  leaf+key+root in the response. Server runs a **second gRPC listener**
  (`RMMWAY_GRPC_MTLS_ADDR`, default `:50052`) with
  `RequireAndVerifyClientCert` against the org root — a non-org leaf is
  rejected at the TLS handshake, before any RPC. Agent persists the issued
  identity and switches its uplink 50051→50052 (plain channel stays
  bootstrap-only); `RMMWAY_GRPC_MTLS_ADDR` override, installer
  `--grpc-mtls-addr`. Proof: `agent/internal/secure` unit test (handshake
  accept/reject), `cmd/mtlscheck` smoke, and milestone step 4b (real systemd
  agent leaf streamed live on :50052; random cert rejected).

#### W3-2 — Short-lived cert rotation
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-24  ·  **Done:** 2026-08-24
- **Depends on:** W3-1
- **Effort/Impact:** M / High
- Leaf certs ~1h, auto-rotate, no downtime.
- **Definition of done:** certs renew automatically across an hour boundary
  without dropping the agent connection.
- Proof: leaves default to ~1h (`RMMWAY_LEAF_TTL`); the agent's `rotate`
  loop renews via the new `RefreshLeaf` gRPC RPC over the mTLS channel it
  already holds (auth-gated, JWT + org-verified leaf), then atomically swaps
  the fresh pair into the persisted identity — `secure` reads the leaf at
  each handshake, so the next connect presents the new cert. The server's
  own mTLS cert also rotates in place via `GetCertificate`. The server cert
  stays the same listener across rotation (no restart). Unit tests: `ca`
  (RefreshLeaf under same root, in-place server cert), `ingest` (auth-gated
  fresh-leaf RPC), `agent/rotate` (threshold, immediate refresh, retry
  after failure, reject incomplete), `agent/secure` (swapped leaf presented
  on next handshake). Live proof: milestone step 4c (`RMMWAY_ROTATE_AFTER`
  e2e knob) — real systemd agent's leaf rotated `...1299 -> ...2532` in
  place, same root, same server cert, uplink kept streaming.

#### W3-3 — Per-action capability tokens
- **Status:** ✅ done
- **Claimed by:** @eng2way
- **Started:** 2026-08-24  ·  **Done:** 2026-08-24 ([a0831af](https://github.com/WelcomeToTheWeb/rmmway/commit/a0831af))
- **Depends on:** W3-1
- **Effort/Impact:** M / High
- Time-boxed session/capability tokens; an agent can't act beyond its minted
  scope.
- **Definition of done:** a command requiring a capability it lacks is refused
  even with a valid mTLS channel. ✅ in-process e2e (`server/cmd/e2e/caps`):
  two devices on valid mTLS channels — misbound (cross-device), tokenless and
  expired tokens all REFUSED without execution; in-scope command executes +
  SUCCEEDED recorded; operator session without the capability gets 403. Real
  systemd agent proves it live in the milestone e2e (step 4d: token verified
  against the pinned org root, real script executed, marker on disk).
  Tokens: ES256 JWT signed by the org root CA (no new key distribution),
  bound to device + capability + command id, TTL `RMMWAY_CAP_TTL` (default
  10m); agent verifies before acting, answers `CommandResult.REFUSED`
  otherwise; unknown action → UNSUPPORTED; legacy plain channel stays
  log-only.

#### W3-4 — Sign all releases
- **Status:** ✅ done
- **Claimed by:** @pi (re-claim; board showed pending)
- **Started:** 2026-08-24  ·  **Done:** 2026-08-25 (commits `6ca926f`…`e4fc4fb` on `main`; release v0.4.0, GH runs 32792842116/32792838791 green)
- **Depends on:** W0-3
- **Effort/Impact:** M / High
- cosign/Sigstore for server; minisign for agent binaries + installers.
- **Definition of done:** CI signs every artifact; a signature file ships
  alongside each release. ✅ first signed release is **v0.4.0** (17 assets):
  5 static agent binaries, both one-line installers and `SHA256SUMS` each
  ship with a minisign `.minisig` alongside (prehashed Ed25519/Blake2b-512),
  plus `minisign.pub`. The server image on GHCR is cosign/Sigstore-signed
  keyless (GitHub OIDC) for both the `v0.4.0` tag and every main push; both
  workflows run a `cosign verify` self-check bound to this repo's Actions
  identity after signing. Signer: `tools/signer` (thin Go CLI over
  go-minisign; keygen emits C-reference key file formats, interop-tested
  against keys generated by the reference minisign(1) CLI — reusable in the
  agent for W4-2). CI (release.yml) signs with the `MINISIGN_PRIVKEY` repo
  secret and enforces an asset-inventory invariant: every artifact, its
  `.minisig`, and `minisign.pub` must be in the release or the job fails.
  External skeptic pass on v0.4.0: all 17 assets downloaded →
  `sha256sum -c` 7/7 OK → `rmmway-signer verify` 8/8 OK against the
  release-shipped `minisign.pub` (== committed key, id 019BF5A0CA5040DD).
  Local: `MINISIGN_PASS=… make sign` / `make verify-sigs`; verification
  guide + key rotation in README ("Release signing").

#### W4-1 — CycloneDX SBOM per artifact
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (commits `fed58d3`, `2513256`; release v0.5.0, GH run 32800419608 green)
- **Depends on:** W0-3
- **Effort/Impact:** M / High
- Generate a CycloneDX SBOM for every image/binary in CI.
- **Definition of done:** each release includes an SBOM listing Go modules +
  OS packages. ✅ release **v0.5.0** ships **29 assets**: every artifact now
  carries a CycloneDX 1.7 JSON SBOM (`<artifact>.cdx.json`) — 5 agent binaries
  (13 Go modules each, binary sha256 cross-checked into the SBOM metadata),
  the server image (23 Go modules + 5 distroless deb OS packages + OS
  component) — each minisigned like the artifacts themselves (13-artifact
  sign/verify set, SHA256SUMS extended). Scanner: pinned **syft 1.51.0**
  (`scripts/install-syft.sh`, sha256-verified download; `scripts/sbom.sh`,
  `make sbom` / `make sbom-agent`). CI: every build generates SBOMs (GitHub
  artifacts: `rmmway-sboms`, `rmmway-server-sbom`); the server image is
  scanned from the exact pushed bytes (buildx `outputs: type=docker` tar →
  `syft docker-archive:`) and the SBOM is `cosign attach sbom`-ed to the GHCR
  digest (media type `application/vnd.cyclonedx+json`). Release: asset
  inventory invariant now includes the SBOMs; a new `release` job creates the
  GitHub release first (the parallel server job raced "release not found" on
  the first v0.5.0 cut — fixed in `2513256`). External skeptic pass on
  v0.5.0: all 29 assets downloaded → `sha256sum -c` 12/12 → signer verify
  14/14 (incl. all 6 SBOMs) against the release-shipped `minisign.pub`.

#### W4-2 — Agent verifies release signature
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (commit `4c293c7` on `main`)
- **Depends on:** W3-4
- **Effort/Impact:** M / High
- Agent checks the server's release signature before auto-updating.
- **Definition of done:** an update with a valid signature is applied; a
  tampered/unsigned build is refused (test both). ✅ Agent pins the W3-4
  minisign public key in-binary (`agent/internal/update/minisign.pub`;
  `RMMWAY_UPDATE_PUBKEY` overrides it) and self-updates from the server's
  `/agent/releases/*` (served from `RMMWAY_RELEASES_DIR`). An update is
  installed only if the manifest names the pinned key AND the binary's
  `.minisig` verifies against it (sha256 + no-downgrade gates too); a
  tampered/unsigned/wrong-key release is refused and the running binary is
  left byte-identical. `rmmway-agent update [--check]` + background
  auto-update in `run` (re-execs on success; Windows stages `.pending`).
  `make release-dir` assembles a signed release dir from `agent/dist`.
  **Proof** (`make update-e2e`, also in CI): builds two real agent binaries,
  signs one with a fresh throwaway key via the real `tools/signer`, serves
  it through an in-process server, runs the real `update` command — valid
  applied (1.0.0→2.0.0), tampered refused by the **signature** gate, unsigned
  refused; refusals leave the binary untouched. Unit tests cover the full
  verification matrix + pinned-key==repo-key.

#### W4-3 — Per-client full export
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (commit `635fa16` on `main`; CI adds a Timescale service so the live-Postgres tests + export e2e now run in CI)
- **Depends on:** W1-6
- **Effort/Impact:** M / High
- One-click export of a client's inventory + metrics + config to a portable
  bundle. This is the no-lock-in promise.
- **Definition of done:** export a client → a self-describing bundle
  (e.g. Parquet + JSON) that re-imports or opens in a standard tool.
  ✅ `GET /api/devices/{id}/export` (operator JWT) + `/admin` mirror
  (`?since=&until=` RFC3339 window, `&rollups=0` to skip) streams one
  self-describing ZIP — `server/internal/export`: `manifest.json` (format
  `rmmway-client-bundle` v1; every file's size + sha256 + row count — the
  contract `export.Verify` checks end-to-end, incl. re-reading each Parquet
  section with a standard reader), `device.json` (inventory + server-side
  config), `metrics.parquet` (raw samples streamed straight from the
  hypertable: `timestamp_ms, ts, name, source, value, labels-JSON`; pure-Go
  parquet-go, no CGO), `metrics_1m.parquet` (1-minute rollups — full history
  beyond the 90d raw retention), `alerts.json` (complete history, all
  statuses), `README.md` (duckdb/pandas/polars open + `sha256sum -c` verify
  + re-import recipes). `DeviceStore.Get` added (store.ErrNotFound). **Proof:**
  `make export-e2e` (also in CI on a new Timescale service): real operator
  HTTP surface on a scratch Timescale DB (1 device, 2 days × 3 series =
  17,280 samples, materialized CA, 3 alerts) — 401/404 gates; one-click
  export (login + ONE GET, `application/zip` attachment) verifies against its
  own manifest (6 files, all hashes/sizes/rows); a flipped byte in
  metrics.parquet is rejected; `?since=&until=` bounds the raw section
  exactly (8,640); an independent standard Parquet reader re-reads all rows
  (ts ≡ timestamp_ms everywhere, value + labels round-trip); and the exported
  Parquet **re-imports** into a fresh migrated database with identical count
  + time range. Live-Postgres unit test in `internal/export` runs in CI via
  the new `RMMWAY_TEST_PG_DSN` (which also enables the previously-skipped
  W1-6/W2-3/W2-4/W5-1 live tests).

#### W4-4 — 🎯 MILESTONE: "provable trust" demo
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (`server/cmd/e2e/trust`, in CI on every push; evidence in [`MILESTONE-W4-4.md`](MILESTONE-W4-4.md))
- **Depends on:** W3-1…W3-4, W4-1…W4-3
- **Effort/Impact:** S / High
- Verify a signed release + SBOM externally, and run a full client export.
- **Definition of done:** a skeptic can (a) verify a release signature + read
  the SBOM, and (b) export a client and confirm the data is theirs. ✅
  `server/cmd/e2e/trust` (`make trust-e2e`), self-contained + self-tearing-down,
  runs in CI (test job). **Part A — skeptic verifies a signed release + reads
  the SBOM:** the builder side cross-builds the real agent, scans a CycloneDX
  SBOM with the pinned syft, and signs binary+SBOM+SHA256SUMS with the real
  `tools/signer` under a throwaway release key; the skeptic side is a fresh
  trust domain holding ONLY the artifacts + `.minisig`s + manifest + public
  key (secret key never enters) and, with an independent implementation
  (`go-minisign`, interop-proven vs the reference CLI in W3-4): verifies the
  SHA256SUMS signature + every listed checksum, verifies the binary + SBOM
  signatures, **rejects a tampered byte** (`Invalid signature`), **rejects a
  wrong key** (`Incompatible key identifiers`), and reads the SBOM —
  CycloneDX 1.7, `metadata.component` sha256 == the real binary's recomputed
  hash, agent's actual deps present. **Part B — a client owner exports a
  client and confirms it's theirs:** one device + known samples (1 day ×
  3 series × 30 s = 8,640) on a scratch Timescale DB, one-click export
  through the REAL operator HTTP surface (401/404 gates), the bundle
  verifies against its OWN manifest (W4-3), `device.json` identity matches
  the client they own (id/hostname/os/arch/IP), the Parquet holds EXACTLY the
  samples they fed (row count + spot value/labels + `ts ≡ timestamp_ms` under
  an independent standard reader + exact time range), and a flipped byte in
  `metrics.parquet` is REJECTED. Same flow cross-checked on the REAL GitHub
  release `v0.5.0` (29 assets) with only the committed `keys/minisign.pub`:
  12/12 sha256, 14/14 minisign (incl. server image SBOM), release pub ==
  committed pub, 5/5 SBOMs tied to their binaries. **Closes Block 2.**

---

## W5/W6 — Automation & Integrations

#### W5-1 — Self-healing playbook engine
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (commit `035dd69` on `main`; e2e `server/cmd/e2e/heal` green on the live stack)
- **Depends on:** W1-2, W1-5
- **Effort/Impact:** M / High
- State machine: detect → verify-safe → remediate → confirm → escalate.
  Idempotent, replay-safe. Starter library: disk full, service down, WSUS stuck.
- **Definition of done:** a failing condition is detected, remediated, and the
  *confirm* step re-measures; on confirm-fail it escalates (ticket+notify).
  ✅ `server/internal/heal`: Playbook model (detect/confirm conditions, per-OS
  scripts, `{{source}}` substitution, os + sample-freshness + cooldown safety
  guards); Postgres state machine in `0005_selfheal.sql` (playbooks seeded with
  disk.full / service.down / wsus.stuck, heal_runs stage-timestamped, heal_events
  append-only audit log); every transition is a conditional UPDATE so runs are
  idempotent and a restart mid-run resumes from the persisted stage; one active
  run per (playbook, device, source) enforced by a partial unique index (no
  double-remediation, even across interleaved passes). Remediation = RunScript
  through the W3-3 capability-gated dispatch (token rides the command); confirm
  re-measures from a sample strictly after the dispatch — only a passing
  re-measurement resolves; confirm-fail / refused / timed-out / no-fresh-sample
  escalates: the run row is the ticket, the Notifier (LogNotifier now, W6-2
  plugs NATS/webhook into the same seam) is the notification. API: `GET
  /api|admin/heal/playbooks`, `GET /api|admin/heal/runs[/{id}]` (+event trail),
  `POST /api|admin/heal/pass`; engine runs on `RMMWAY_HEAL_INTERVAL` (5m,
  `off` disables). Proof: unit tests (conditions/os/scripts), live-Postgres
  lifecycle tests (replay-safe resolve at the re-measured 62, confirm-fail
  escalation at 95 + exactly-one notify, REFUSED escalation, remediation-timeout
  escalation, offline/stale verify-safe skips, service.down + wsus.stuck incl.
  os_filter), and `cmd/e2e/heal` (real gRPC ingest + org CA + caps + engine on a
  scratch DB; two agents verify the capability token vs the pinned root like the
  real agent: A heals 95→62 → resolved; B stays 95 → escalated ticket + one
  notify; replay passes add no run/dispatch/notify).

#### W5-2 — Event-driven chains over NATS
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** 2026-08-25 (commit `5e81a56` on `main`)
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Model automations as DAGs of triggers→actions wired over NATS; visual
  composer.
- **Definition of done:** compose `disk>90% → free → if>90% → notify` and it
  fires correctly on the synthetic trigger. ✅ `server/internal/flow`: flows
  are validated DAGs (trigger|script|check|notify) stored in
  `0006_flows.sql` and EXECUTED over the NATS/JetStream bus
  (stream `RMMWAY_EVENTS`, durable `flow-engine`) — every hop of every run
  (trigger, step, command.result, notify) is a bus event; Postgres holds
  only the replay-safe run state (conditional transitions + append-only
  `flow_events` audit; one active run per (flow, device, source) at the DB
  layer). Script nodes dispatch through the same capability-gated command
  path (W3-3); the ingest `OnCommandResult` hook turns final agent answers
  into `command.result` hops; check nodes RE-measure the metric (fresh
  sample after the previous node) and branch then/else; sweep + sampler
  tickers re-cover in-flight runs and publish real-metric triggers.
  API: `GET/POST /api|admin/flows`, `GET/PATCH/DELETE /{id}`, `POST
  /{id}/trigger` (synthetic), `GET /runs[/{id}]` (+node log), `POST
  sweep|sample`. Visual composer: `#/flows` (chain pipeline renderer, step
  builder, per-flow test trigger, runs table with per-node audit trail).
  Proof: unit (graph validation) + live-Postgres engine tests (both check
  branches, waiting re-measure, replay no-ops, failure+notify) and
  `cmd/e2e/flow` over a REAL NATS stream + real ingest + two
  capability-verified agents — the DoD chain fires on the synthetic
  trigger: healed branch (62) ends quietly, still-full branch (95) notifies
  exactly once, audit trail `t→free→still(→notify)`; full-stack smoke: HTTP
  trigger → real agent over mTLS runs the dispatched script → check
  re-measures the live disk sample → terminal run.

#### W6-1 — Loki log integration
- **Status:** ✅ done
- **Claimed by:** @pi
- **Started:** 2026-08-23  ·  **Done:** 2026-08-23 (commit `a31b2da` on `main`)
- **Depends on:** W0-1
- **Effort/Impact:** M / High
- Agent tails structured events → ship to Loki; keep indexed events in
  Timescale.
- **Definition of done:** agent log lines queryable in Loki; the RMM surfaces
  recent indexed events per device. ✅ `agent/internal/logship` TEEs the
  agent's `slog` events into a JSON-lines file and tails it, shipping each
  batch to BOTH Loki (HTTP push, `device_id`/`job`/`level` labels) and the
  server over the existing gRPC Stream (new `LogBatch` frame, `logs.proto`).
  The server indexes them in the `log_events` Timescale hypertable
  (`0007_log_events.sql`) and serves `GET /{api|admin}/devices/{id}/events`
  (newest first, level filter, auth-gated under `/api`); the Devices view
  expands a row into a live recent-events panel. Replay-safe end to end:
  stable content-derived entry ids (dedup by id at both sinks) + a persisted
  offset state, so a reconnect or restart re-sends at most a no-op. Loki 3.5.4
  runs in the compose stack (`:3100`, single-binary local config); health
  gates on all 6 services. Proof: unit (tail/batch/both-sinks/retry/offset-persist)
  + `make logs-e2e` over a REAL agent binary, scratch Timescale, in-process
  ingest/API, and REAL Loki — a dispatched command's receipt + `agent ready`
  are queryable in Loki by `device_id` AND surfaced per-device in the RMM,
  with cross-store consistency and a 401-gated `/api` mirror.

#### W6-2 — Webhook + event-stream framework
- **Status:** 🔵 claimed
- **Claimed by:** @pi
- **Started:** 2026-08-25  ·  **Done:** —
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
