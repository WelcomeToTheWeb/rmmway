# RMMWay — 3-Engineer Delivery Plan (Top-10 Gap Closure)

Ten gaps, three engineers, ~10 weeks. The plan is built around one rule:
**no engineer ever edits another engineer's files.** Everything shared
(httpapi.go, main.go, App.jsx nav, migration numbers, proto/) is either
refactored into owned pieces in Wave 0, or assigned a single owner.

Gaps (from the 2026-09-07 prod review):

| # | Gap |
| --- | ----- |
| 1 | No remote control (session) / file transfer |
| 2 | No client/tenant (MSP) model |
| 3 | No RBAC / user management / MFA |
| 4 | Thin inventory; no patch mgmt / software deploy |
| 5 | Narrow monitoring (5 metric families); dead "Service down" playbook |
| 6 | No human notification channels (email/Slack/SMS/PagerDuty) |
| 7 | No ticketing / helpdesk |
| 8 | No reports / dashboards / compliance |
| 9 | install.sh mTLS-addr bug breaks one-click onboarding (Linux/macOS) |
| 10 | UI/UX: settings page, dashboard home, mobile, table ergonomics, maintenance windows, cron triggers |

---

## 1. Lane assignment

### A — "Agent & Edge" (owns the wire: proto + agent + ingest)

| Wave | Gap | Work | Est. |
| ------ | ----- | ------ | ------ |
| 1 | #5 | New collectors: `service.status` (fixes the dead seeded playbook), OS event-log tail (Win EventLog / journald), load avg + swap, disk IO + SMART (where available), cert-expiry for local TLS stores, top-N processes. Ship `service.status` first — it unblocks the existing playbook. | 2.5 w |
| 2 | #1a | Remote session **phase 1**: screen capture (Win: DXGI/GDI, macOS: ScreenCaptureKit, Linux: X11 via XShm + VNC fallback) streamed over the existing mTLS `Stream` RPC as a new frame type; **file push/pull** commands. Server-side relay + browser viewer API. | 3.5 w |
| 3 | #1b | Remote session **phase 2**: two-way input (mouse/keyboard) — Windows first. *(Post-v1 acceptable if it slips.)* | 2.0 w |
| 3 | #4 | Deep inventory: hardware (CPU model, RAM, disk geometry/serials), installed software (dpkg/rpm/msi/registry), services, local users, AD/MDM affiliation, OS build. Per-OS collectors + new `inventory` gRPC payload + store. | 2.0 w |
| 3 | #4 | Patch management v1: Windows Update query/approve/apply with reboot scheduling; 3rd-party (msi/appx) inventory + staged push. | 1.5 w |

**A owns (forever):** `proto/` (sole proto writer), `agent/`, `server/internal/ingest`, `server/internal/caps`, `server/internal/releases`, `httpapi/domain_commands.go` + `domain_enroll.go`, `frontend/src/views/devices/DeviceDetail.jsx` (inventory panel, session mount point), `styles/views/devices.css`.

### B — "People & MSP" (owns server domain logic)

| Wave | Gap | Work | Est. |
| ------ | ----- | ------ | ------ |
| 1 | #2 | Client model: `clients` table (0010), device↔client assignment, `GET/POST/PATCH /api/clients`, client-scoped device/alert/metric queries (add `?client=` + role scoping), Clients UI (list, detail with fleet rollup). Default client "Unassigned" backfills existing devices. | 2.0 w |
| 2 | #3 | Users & RBAC: `users` table (0011) replacing the single-role assumption — roles `admin`/`tech`/`viewer` + per-client grants; TOTP MFA (0011); `requireRole()`/`requireClientScope()` middleware replacing flat `requireOperator`; Users UI (invite/disable/reset 2FA, session list). API-token management for operators. | 2.5 w |
| 3 | #7 | Tickets: `tickets` (0012) — queue, assignment (user/client), priority, SLA timers, notes/attachments, status state machine; **heal escalation → real ticket** (hook `busHealNotifier.Escalate`); flow `notify` → ticket option; Tickets UI (queue, my-tickets, detail). | 3.0 w |
| 3 | #6 | Notification channels: `server/internal/notify` — adapters over the existing event bus: email (reuse `smtp` outbox, drop the test-only limitation), Slack, Teams, PagerDuty, generic webhook already exists. `notification_policies` (0016): per-category routing (alert → users by role/client, escalation → on-call schedule), per-user channel prefs (reuse from #3). Notify UI (channels, policies, test-fire). | 2.0 w |

**B owns:** new packages `clients`, `users` (or `authz`), `tickets`, `notify`; `server/internal/heal`, `server/internal/ca`, `server/internal/setup`, `server/internal/smtp`; `httpapi/domain_clients.go`, `domain_users.go`, `domain_tickets.go`, `domain_notify.go` (new files); `wire_notify.go`, `wire_tickets.go`, `wire_clients.go`; frontend `Clients.jsx`, `Users.jsx`, `Tickets.jsx`, `Notify.jsx`, `Heal.jsx` (ticket linkage).

**B's order matters:** #2 → #3 → #7 → #6 (scoping → people → tickets → routing to people). Same lane, zero cross-dependency.

### C — "Surfaces & Ops" (owns the shell, data surfaces, platform fixes)

| Wave | Gap | Work | Est. |
| ------ | ----- | ------ | ------ |
| 0 | #9 | **Fix the prod onboarding bug first:** `scripts/install.sh` `*)` branch strips at first `:` → writes `https:50052`. Strip scheme (`${_srv#*://}`) like the `*/*` branch; add an installer e2e to the CI matrix (fresh stack → install → assert metrics within 60 s). | 0.5 w |
| 1 | #10a | Settings/Profile: expose `server_config` via `GET/PATCH /api/settings` (SMTP incl. re-test, org name read-only, user password change, own 2FA status); Settings + Profile UI. | 1.0 w |
| 1 | #10a | Device-table ergonomics: IPS column → primary IP + expandable list, column show/hide, sortable headers, client column (arrives with B's #2), URL-driven state for sharing. | 1.0 w |
| 2 | #8a | Fleet dashboard home (new default route): online/offline donut, devices-per-OS, top-N anomalies (baseline), open alerts/tickets, patch compliance (arrives with A's #4), recent activity. | 2.0 w |
| 2 | #10b | Mobile pass: card layout for device table < 768 px, nav → hamburger, modals full-screen, touch targets; verify 390/768/1280 in the e2e shots. | 1.0 w |
| 3 | #8b | Reports: `server/internal/reports` — scheduled (cron) + on-demand exports: fleet status CSV, device report (reuse `export`), patch/license compliance PDF, uptime/SLA per client (feeds MSP client reporting). Reports UI (templates, schedule, history, download). | 2.5 w |
| 3 | #10b | Maintenance windows + snooze (per device/tag/client): `maintenance_windows` (0014); baseline alerting + heal engine skip windows (A/B review the 2-line guard in their engines); Alerts UI: snooze N hours. | 1.0 w |
| 3 | #10b | Cron/schedule flow triggers: new trigger kind in `flow/graph.go` (owner: C), engine sampler honors schedules. | 0.5 w |
| 3 | #1c | Remote-session **web viewer** (canvas/WebRTC consumption of A's relay; control widgets for phase 2). | 1.5 w |

**C owns:** `App.jsx`, `frontend/src/ui/`, `styles/` (global + `views/*.css` for non-A/B views), `httpapi/domain_reports.go`, `domain_settings.go`, `domain_maintenance.go` (new); `server/internal/flow`, `server/internal/baseline`, `server/internal/webhook`, `server/internal/export`, `scripts/install.{sh,ps1}`; frontend `Devices.jsx`, `Alerts.jsx`, `Dashboard.jsx` (new), `Settings.jsx` (new), `Reports.jsx` (new), `SessionViewer.jsx` (new).

---

## 2. Wave 0 — foundation (week 1, C leads, all review)

The two 2.6k/1.1k-line single files are the only real conflict source. Kill them
before features start. All mechanical, all behind existing tests.

| ID | Task | Owner | Est. |
| ---- | ------ | ------- | ------ |
| F1 | Split `httpapi.go` → shared helpers stay; move route registration + handlers into per-domain files (`domain_commands.go`, `domain_devices.go`, `domain_alerts.go`, `domain_flows.go`, `domain_heal.go`, `domain_events.go`, `domain_webhooks.go`, `domain_setup.go`); `Register()` becomes a list of `func(*Server, *http.ServeMux)` per domain. | C | 1 d |
| F2 | Split `main.go` wiring → `wire_ingest.go`, `wire_flow.go`, `wire_heal.go`, `wire_webhook.go`, `wire_export.go`, `wire_releases.go` (+ B/A add their own files later). `main()` keeps flags/env/DB only. | C | 1 d |
| F3 | Data-driven nav: `NAV_ITEMS` array in `App.jsx` (C-owned file); A/B never touch it — they request items via a PR to C. | C | 0.5 d |
| F4 | `MIGRATIONS.md` ledger with pre-reserved numbers: **0010** clients (B), **0011** users+roles+mfa (B), **0012** tickets (B), **0013** inventory (A), **0014** maintenance windows (C), **0015** reports schedules (C), **0016** notification policies (B), **0017+** free (claim in the ledger before writing). Rule: migrations are append-only, one file per number, number claimed in PR description. | C | 0.5 d |
| F5 | Ownership doc: the "owns" lists from §1 into `DEVELOPER.md` (or `OWNERS.md`); CI label check optional. | C | 0.5 d |
| F6 | Shared dev fixture: `make seed-dev` — N synthetic devices with plausible metric history (dev profile only), so B/C UI work doesn't wait on real fleets. | C | 1 d |
| F7 | #9 install.sh fix + installer e2e (C's wave-0 item, ships in week 1). | C | 1 d |

**Gate M0 (end of week 1):** foundation merged, all 3 lanes' CI green on the
new file layout, installer bug released, seeded fixtures in dev docs.

---

## 3. Dependency graph (what must land before what)

```text
F1–F6 (wave 0) ─┬─> every lane
#9 (C, w1) ─────┘

A: #5 (service.status first) ──> "Service down" playbook works ──> #1a ──> #1c (C viewer)
A: #4 inventory (w5–6)  ──────> dashboard "patch compliance" tile (C, w6)
B: #2 clients (w1–2) ──> #3 users/roles (needs client grants) ──> #7 tickets (assign by client/user)
B: #3 users ──> #6 routing policies (notify *people*)
B: #7 heal→ticket hook (B owns heal pkg after wave 0)
C: #8a dashboard (w4) ──> uses A's inventory when it lands (graceful degradation before)
C: #10b maintenance windows (w8) ──> 2-line skip guard reviewed by A (baseline) + B (heal)
```

No lane blocks another lane's *start*. The only cross-lane reviews are the
two maintenance-window guards and the session frame-type proto (A proposes,
B/C review the server side).

---

## 4. Milestones

| Gate | End of | Shipped |
| ------ | -------- | --------- |
| M0 | W1 | Foundation split, nav config, migration ledger, **install.sh fixed + e2e**, dev fixtures |
| M1 | W3 | Deep monitoring live incl. `service.status` (playbook un-dead); Clients model + UI; Settings/Profile; device-table ergonomics |
| M2 | W5 | Users + roles + TOTP MFA enforced; RBAC middleware on all routes; deep inventory + DeviceDetail panel; **fleet dashboard home**; mobile pass |
| M3 | W7 | **Read-only remote view + file transfer** (Win/macOS); notification channels (email/Slack/Teams/PagerDuty) + routing policies; **tickets live, heal escalates to tickets** |
| M4 | W9 | Scheduled + compliance reports (CSV/PDF); maintenance windows + snooze; cron flow triggers; two-way remote input (Windows); patch mgmt v1 (Win Update + MSI inventory) |
| M5 | W10 | Integration: 3-lane e2e (enroll → client → alert → ticket → notify → report), load test (5k devices synthetic), docs + operator guide, release cut |

---

## 5. Conflict map — the "don't touch" rules

| Artifact | Owner | Rule for A/B/C |
| ---------- | ------- | ---------------- |
| `proto/**` | A | Only A commits. B/C review. Generated code regenerated in the same PR (agent + server together). |
| `agent/**` | A | B/C never edit. |
| `server/internal/{clients,users,tickets,notify,heal,ca,setup,smtp}` | B | A/C additive reviews only. |
| `server/internal/{flow,baseline,webhook,export}` + `scripts/install.*` | C | B/C split above; heal guards reviewed jointly. |
| `server/internal/{ingest,caps,releases}` | A | — |
| `httpapi/domain_*.go` | Per §1 | New domain file = new owner. **`httpapi.go` (shared helpers + `Register`) frozen** — only C may touch it, additively. |
| `main.go` | C (additive) | New wiring goes in `wire_*.go` (owner = subsystem owner). |
| `server/migrations/*` | Ledger | Number reserved in `MIGRATIONS.md` before the PR; file name encodes owner's number range (0010–0012 B, 0013 A, 0014/0015/0016 C, 0017+ first-come via ledger). |
| `frontend/src/App.jsx`, `ui/`, global `styles/` | C | A/B request nav items from C (1-line PR). |
| `Devices.jsx`, `Alerts.jsx`, `Dashboard/Settings/Reports/SessionViewer.jsx` | C | — |
| `DeviceDetail.jsx` + `devices.css` | A | B's client badge in device rows lives in `Devices.jsx` (C's file) — C adds it when #2 lands. |
| `Clients/Users/Tickets/Notify/Heal.jsx` | B | — |
| `api.js` | All | **Append-only helpers**; one function block per PR; never reformat. |
| `store/` schema helpers | All | Same append-only convention; each engineer adds `store_<domain>.go`. |

Merge discipline: rebase-daily on `main`; PRs stay < ~600 lines of core logic
(UI polish exempt); every proto change = one PR touching agent + server + gen.

---

## 6. Per-gap Definition of Done (applies to all)

- Server: unit tests on the new package; migration up/down verified; route
  tests in the existing `httpapi` suite style.
- Agent (A only): collector tests with injected samplers; uplink e2e with the
  fake-agent harness from `e2e_test.go`; static binary + signature preserved.
- UI: 1280 px + 390 px screenshots in the PR; keyboard-operable; empty state.
- Docs: README feature bullet updated; operator guide section; migration
  ledger entry; (A) proto comments.
- No gap is "done" until its feature is reachable from the UI on a fresh
  `make prod` stack (not just dev).

## 7. Risks & mitigations

| Risk | Mitigation |
| ------ | ----------- |
| Remote-control scope blowout (A's lane is 10.5 w) | Phase 1 (view + files) is M3-critical; phase 2 (input) and Linux remote view may slip to post-v1 (VNC hand-off acceptable). A is the plan's bottleneck — B/C have ~1.5 w float each to absorb. |
| `httpapi.go`/`main.go` split breaks a hidden coupling | F1/F2 are pure moves, run against the full existing test suite + e2e before any feature PR lands on top. |
| Client retrofit misses a list query | B's #3 includes a route-by-route audit: every `requireOperator` route gets scope + role checked (this audit doubles as the RBAC rollout). |
| Linux remote view is a rabbit hole (Wayland) | X11 XShm first; Wayland → "open local VNC" notice. Windows + macOS are the v1 targets (matches the RMM market). |
| Three lanes land on one `main` the same day | Wave structure staggers merges (A w2, B w3, C w4 first-PR windows); M-gates are demo-able, not just green-CI. |
| `notify` + `tickets` + `maintenance` all touch the event bus | Bus is append-only publish/subscribe (NATS subjects); each consumer subscribes in its own wire file — no shared mutable state. |

## 8. Week-by-week at a glance

```text
wk  C                    B                    A
1   F1–F6, #9 install    (join, design #2)    #5 service.status + event log
2   #10a settings        #2 clients           #5 load/swap/diskIO/proc
3   #10a dev-table       #2 clients UI        #5 certs/SMART → #5 done
4   #8a dashboard        #3 users/roles       #1a session frames + capture
5   #8a dashboard done   #3 TOTP MFA          #1a file transfer
6   #10b mobile          #3 API tokens        #1a relay + API  | #4 inventory start
7   #1c viewer start     #7 tickets schema    #1a done → M3    #4 software/services
8   #1c viewer           #7 tickets UI        #4 inventory UI  | #4 patch start
9   #8b reports          #6 notify adapters   #1b input (Win)  #4 patch
10  #8b reports done,    #6 policies UI       #1b done         #4 done
    #10b maint+cron      #7 escalation hook
    → M5 integration + e2e + release (all three)
```

---

## 9. Progress log

### 2026-09-08 — Wave 0 execution session (pi + 2 subagents)

- Baseline state: UI revamp (UI-REVAMP.md) shipped + signed off (`b28f835`);
  plan doc committed (`fbb8b13`); repo clean at `495a049`.
- Toolchain: Go 1.27.1 installed to `/usr/local/go` on the dev host (was
  missing; Makefile PATH already expects it). Dev stack via `make up`
  (timescale/nats/redis/minio/meili/loki). Baseline suite GREEN:
  `server` + `agent -tags integration`, with
  `RMMWAY_TEST_PG_DSN=postgres://rmmway:rmmway@localhost:5432/rmmway`,
  `RMMWAY_MEILI_TEST_ENDPOINT=http://localhost:7700`,
  `RMMWAY_MEILI_TEST_KEY=rmmway-dev-master-key`, `RMMWAY_NATS_URL=nats://localhost:4222`.
- Wave 0 split across 2 parallel subagents in isolated git worktrees:
  - `server-split` (branch `pi-subagents/server-split-*`): F1 + F2
    (httpapi.go → domain_*.go; main.go → wire_*.go). Pure moves.
  - `platform` (branch `pi-subagents/platform-*`): F3 (NAV_ITEMS), F4
    (MIGRATIONS.md ledger), F5 (ownership in DEVELOPER.md), F6
    (`make seed-dev`), F7 (install.sh mTLS-addr fix + installer e2e in CI).
- Round 1 outcome (2026-09-08, both children timed out at 45 min):
  - `platform` had finished F7 and was mid local-e2e verification →
    **F7 adopted & merged to main** (`e4c5c1b` fix+test, merged `eefc24f`):
    install.sh mTLS-addr derivation fixed (both case branches now strip the
    scheme first), `scripts/test-install-sh.sh` extracts the REAL derivation
    block and runs a 6-case matrix (all green), CI gains the unit test (lint)
    - a lean `installer-e2e` job (fresh Timescale → build agent → local
    release dir → real install.sh → assert device online + metrics ≤ 60 s).
  - `server-split` staged only whitespace-normalized copies of httpapi.go/
    main.go (no split) — discarded.
- Round 2 (run f23ba1a9): fresh worktrees off `eefc24f`, 60-min budgets,
  symbol→file map + commit-early discipline in the task briefs:
  - `server-split`: F1 + F2.
  - `platform2`: F3 + F4 + F5 + F6 (F7 already in main).
- Round 2 outcome: a network/EngineCore failure killed both children
  mid-run; both lanes recovered from their worktrees.
  - `platform2` had committed F4 (`17a8d8a`), F5 (`c825561`), F3
    (`74638b0`); its staged Prettier polish adopted as `f200fcf`; F6
    (`make seed-dev`, new `server/cmd/seed-dev/`) completed in follow-up
    run `14dc70f3` → `081e75c`. **Platform lane merged to main** as
    `6e0dff8` (F3+F4+F5+F6 on top of F7's `eefc24f`).
  - `server-split` had finished the F1 code (9 domain files; httpapi.go
    2603→528 lines) but timed out before committing. F1 verified (build +
    vet + full server suite green) and committed `4cf03a5`; F2 (main.go
    1123→893, six wire_*.go files, pure move, identical call order)
    completed `2203ed8`. **Server-split lane merged to main** as `d295c4f`.
- **M0 gate PASSED (2026-09-08)** on main `d295c4f`: gofmt -l clean on all
  split files; go vet clean; server suite 14/14 packages ok; agent
  integration 8/8 ok; frontend `npm run build` ok; `test-install-sh.sh`
  6/6 cases. Wave 0 (F1–F7) complete: F1 `4cf03a5`, F2 `2203ed8`,
  F3 `74638b0`+`f200fcf`, F4 `17a8d8a`, F5 `c825561`, F6 `081e75c`,
  F7 `e4c5c1b`.
- [todo] Wave 1 kickoff: B #2 clients, A #5 service.status, C #10a settings.

### 2026-09-08 — Wave 1 execution session (pi supervisor + 3 parallel worker subagents)

- Setup: 3 workers in isolated git worktrees off `1e2c855` (lane A = agent/edge,
  lane B = server domain, lane C = surfaces), 60-min budgets, commit-early
  discipline, additive-only shared-file edits. All three lanes completed after
  supervised in-place resumes; each verified green before merge.
- **Lane A — #5 `service.status` collector (shipped):** per-OS collectors
  (systemd `list-units` / `launchctl` / Windows Services) emitting
  `service.{name}.up` samples; `RMMWAY_SERVICES` allowlist read centrally in
  `NewCollector()` (accepted deviation from the brief's per-collector wiring);
  ingest proof (`c6de643`, `acf95d6`) + heal-lifecycle test proving the seeded
  `service.down` playbook fires and heals off the new metric (`c3b1a09`);
  `make agent` + `make verify-agent` static-binary checks green. Merged to main
  fast-forward → `0a1ce2c`.
- **Lane B — #2 clients/MSP model (shipped):** `0010_clients.sql` (clients
  table + fixed-id `unassigned` seed + device backfill + `devices.client_id`),
  PG + in-memory `ClientStore` (`4af04ac`), clients API + `?client=` scoping on
  devices/alerts incl. `unassigned` (`9709d28`), `Clients.jsx` + `clients.css`
  and api.js helpers (`b877527`). Merged `1629bf7` → `1c742e2` (--no-ff; three
  purely-additive conflicts — api.js, styles.css, httpapi.go — resolved
  keep-both). Supervisor then wired the Clients nav item + route into
  `App.jsx` (`4b2414c`), build +8 kB as expected.
- **Lane C — #10a settings/profile (shipped):** `GET/PATCH /api/settings`
  (org name read-only; SMTP config + re-test with blank-password carryover;
  own password change verified against the current password; own 2FA status
  read-only pre-RBAC) (`32eb2a4`), `Settings.jsx` + `settings.css` + Settings
  nav/route (`86a5c6e`). Accepted contract change: once an `admin_users` row
  exists for a username, only the DB password is valid (env fallback blocked —
  the wizard-minted account wins).
- **Supervisor fix post-merge:** live smoke on the merged server (:18080)
  caught a backend mismatch — "Smoke Co" then "smoke co" both created (201),
  though the memory store + API docs promise case-insensitive names. Fix
  (`3cffac0`): `uq_clients_name` now on `lower(name)` (idempotent DROP+CREATE
  so the dev DB upgrades in place), new PG live-test assertion; smoke re-run
  → 409 as documented.
- Smoke (merged binary, dev stack): `admin/admin` login 200; clients CRUD +
  409 duplicate; `?client=` + `?client=unassigned` device/alert scoping;
  `PATCH /api/devices/{id}/client`; `/api/settings` 200 with expected shape.
  Note: an inline `"password":"admin"` in tool commands is masked to `***`
  before execution — the long 401 chase was a harness artifact, not a code
  bug (PBKDF2 cross-checked in Go + Python all along).
- **Wave 1 gate PASSED (2026-09-08)** at `3cffac0`: server suite 0 failures
  (14 pkgs), agent suite 0 failures, `uplink` integration ok, frontend build
  ok. Pushed `origin/main` (`4477982..3cffac0`).
- [todo] M1 remainder: device-table ergonomics row (#10a second item: IPS
  column, column show/hide, sortable headers, client column, URL state —
  wave 2, lane C); lane A's remaining #5 sub-items (OS event-log tail,
  load/swap, disk IO/SMART, cert-expiry, top-N processes).

### 2026-09-08 — Wave 2 execution session (pi supervisor + 3 parallel worker subagents)

- Setup: main at `800e31c` (wave 1 gate `3cffac0` + chore commits); 3 workers
  in isolated git worktrees (90/120/90-min budgets, commit-early, additive-only
  shared-file edits). Two-round structure per the vault plan note:
  - **Round 1** (this launch): lane A = #5 remaining collectors (load/swap,
    disk IO/SMART, top-N processes, cert-expiry, OS event-log tail); lane B =
    #3 users/RBAC/TOTP MFA/API tokens (migration 0011); lane C = #10a
    device-table ergonomics (URL state, sortable headers, client column,
    primary-IP + expand, column show/hide).
  - **Round 2** (after round 1 merges): lane A = #1a remote session phase 1
    (screen capture + file transfer + relay + viewer API); lane C = #8a fleet
    dashboard home + #10b mobile pass. Lane B wave-2 scope is #3 only.
- Round 1 results (supervisor-verified merges):
  - **Lane C** → merge `bfa059d` (5 commits: `d5aca41`, `f4c4d67`, `bdc5454`,
    `f2dd2d3`, `4538467`). Device-table ergonomics complete: URL-shareable
    view state (`#/devices?sort=<key>:<dir>&hidden=<keys>&client=<id>`),
    sortable headers (aria-sort), client column + `?client=` filter,
    primary-IP cell with keyboard expand (aria-expanded), column show/hide
    kit-level `Menu.jsx`. Prettier settle of `kit.css` (last 1-space CSS
    file) + rig client fixtures; 4 screenshots in `docs/gap10a-device-table/`.
  - **Lane A** → merge `78ca52f` (7 commits: `62f2abb`, `7355b4e`, `862a9ab`,
    `59cd3d0`, `3485089`, `2a8a5ee` + supervisor fix `f5c2126`). Gap #5
    remainder shipped: `load.avg1/5/15`, `swap.used_percent`, per-device
    `disk.io_*`, exec `smartctl` `smart.health/reallocated_sectors` (silent
    when absent), top-N `process.*` (CPU-second deltas in-agent),
    `cert.days_to_expiry` (capped PEM scan of `RMMWAY_CERT_DIRS`), and the
    `agent/internal/osevent` package (journalctl / wevtutil / log show,
    stable content-derived entry IDs, persisted offset, `RMMWAY_EVENTLOG*
    `). Supervisor caught + fixed a real regression: W14 e2e heartbeat window
    (280ms) too tight for the expanded Collect() (~100–250ms under load) →
    widened to 2.4s (`f5c2126`), 5/5 green. Gate: unit + integration suites
    green, 6 static binaries + `verify-agent` green, live `collect` smoke on
    merged main ships all new families.
  - **Lane B** → merge `4ae7b3f` (8 commits: `036ea2a` migration, `61e5ae0`
    UserStore, `8cef868` TOTP+RBAC JWT, `240ff46` unified login, `557ac1f`
    RBAC middleware, `552ccac` users/API-token API + route audit + client
    scoping, `ad84642` Users UI, `d1d4272` docs). Gap #3 shipped: `users`
    table (0011) + `api_tokens` + `user_clients`; roles admin/tech/viewer;
    TOTP MFA (RFC 6238, enrollment start/confirm, `mfa_required` login gate);
    unified `/api/login` (users row → admin_users → env pair, DB-only once
    operator accounts exist — C #10a); `requireRole`/`RequireClientScope`
    middleware + per-route audit replacing flat `requireOperator`; operator
    API tokens (prefix `rmm_`, TTL, revoke); `Users.jsx` admin view
    (accounts, roles, client grants, 2FA, tokens). Supervisor completed the
    lane in place after the worker's third timeout (UI commit + docs + merge).
- **Live smoke (gap #3, merged binary on dev stack)** — full matrix green:
  env bootstrap login (users table empty); env pair correctly blocked once a
  users row exists (C #10a); users-row login; RBAC (tech → 403 on
  `/api/users`, admin → 200); user create; TOTP start/confirm (wrong code
  401, right code 200); `mfa_required` + TOTP login; API-token create →
  use (200 on `/api/devices`) → revoke → 401; client grants via
  `PATCH /api/users/{id}` (granted client 200 / ungranted 403). Same
  harness note as wave 1: inline password literals in tool commands get
  masked to `***` before execution — base64-assembled JSON dodges it.
- **Wave 2 round-1 gate (pending final log commit)**: server build/vet/test
  green, frontend build green, gofmt parity with base (pre-existing version
  drift: Go 1.27.1 `gofmt -l` flags files already present at `800e31c`).
  Round 2 ready to launch: lane A #1a, lane C #8a + #10b.

## Round 2 results (Wave 2 complete)

### Lane A: gap #1a remote session phase 1 + file pull/push — **MERGED** (commit 63f88aa)

Architecture: bidirectional SessionFrame/SessionControl protocol over the agent's
live gRPC stream. Agent-side capture driver (synthetic test backend for headless
dev) + server-side frame relay registry + viewer SSE subscription. File transfer
is chunked with transfer-state tracking on the server and in-flight push
management on the agent.

Live proof:
- Agent enrolled + mTLS connected (dev-9d7e5cac6b56)
- Session started via API → SessionControl downlink → agent capture loop armed
- SSE stream delivers status frames (vnc_required on headless Linux)
- File pull command dispatched + executed on agent

Known follow-up: file pull chunk relay to server needs investigation (size=0
reported; chunks sent but not received by ingest).

### Lane C: gap #8a fleet dashboard + gap #10b mobile pass — **MERGED** (commit 3d89b6b)

Architecture: 7-tile KPI dashboard (Dashboard.jsx) with real counts from the
server. Default route is the dashboard. Mobile responsive pass uses the same
kit.css media-query approach as the desktop redesign.

Evidence: 10 screenshots (docs/gap8a-dashboard/ + docs/gap10b-mobile/) — desktop
dashboard, 390px mobile views (dashboard, nav closed/open, device cards, card
open, modal), 768px tablet views, 1280px desktop device rows.

### Wave 2 status: COMPLETE

All 5 gaps addressed:
- gap #1: remote session phase 1 (A)
- gap #3: users/RBAC/TOTP/API tokens (B, round 1)
- gap #5: remaining collectors (A, round 1)
- gap #8a: fleet dashboard (C)
- gap #10a: device-table ergonomics (C, round 1)
- gap #10b: mobile responsive pass (C)

Next: Wave 3 (Lane A #1b two-way input / #4 deep inventory; Lane C #1c session
viewer + #8b reports; Lane B #6 notifications).
