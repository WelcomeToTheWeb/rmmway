# MIGRATIONS.md — schema migration ledger

One file per migration number. This ledger is the **claim registry** for
numbers: a number is reserved here BEFORE its `.sql` file is written, so
parallel lanes (A/B/C) can never collide.

## Rules

1. **Append-only.** Migration files are never edited after merge, never
   deleted, never renumbered. Fix forward with a new number.
2. **One file per number.** File name is `NNNN_snake_name.sql`
   (e.g. `0010_clients.sql`). The number in the file name is the identity.
3. **Claim before writing.** Reserve the number in the PR description
   (and note the claimant lane) BEFORE creating the file. If the number is
   already reserved below by someone else, pick the next free one.
4. **Idempotent.** Every migration is `IF NOT EXISTS` / safe to re-run,
   matching the existing 0001–0009 style.
5. **Verify up/down in the PR.** The PR description must state that the
   migration was applied cleanly against a fresh DB (and, where the schema
   supports it, rolled back).

## Reserved & shipped

| # | File | Owner (lane) | Status | What it adds |
| --- | --- | --- | ------ | ------------ |
| 0001 | `0001_init.sql` | — | shipped | W1-6: TimescaleDB schema (devices, hypertabled metrics) |
| 0002 | `0002_baseline.sql` | — | shipped | W2-3: dynamic baselining engine state + anomalies |
| 0003 | `0003_alerts.sql` | — | shipped | W2-4: baseline-driven alerts + deduped inbox |
| 0004 | `0004_org_ca.sql` | — | shipped | W3-1: org PKI (org root CA + device leaf certs) |
| 0005 | `0005_selfheal.sql` | — | shipped | W5-1: self-healing playbook engine (playbooks + runs) |
| 0006 | `0006_flows.sql` | — | shipped | W5-2: event-driven automation chains (flows + runs) |
| 0007 | `0007_log_events.sql` | — | shipped | W6-1: indexed agent log events (Timescale hypertable) |
| 0008 | `0008_webhooks.sql` | — | shipped | W6-2: signed webhooks + event journal/replay |
| 0009 | `0009_setup.sql` | — | shipped | A-2: first-boot setup wizard state |
| 0010 | `0010_clients.sql` | — | shipped | B #2: clients table + devices.client_id + seeded 'unassigned' backfill |

## Pre-reserved (gap-closure plan — TEAM-PLAN.md §2 F4)

These numbers are claimed by lane at plan time; the files do not exist yet.
The owning lane writes them during its wave; the claim stays in this table.

| # | Reserved for | Owner (lane) | File (planned) |
| --- | ------------ | ------------ | -------------- |

| 0011 | users + roles + TOTP MFA | B — People & MSP | `0011_users.sql` |
| 0012 | tickets (helpdesk) | B — People & MSP | `0012_tickets.sql` |
| 0013 | deep inventory | A — Agent & Edge | `0013_inventory.sql` |
| 0014 | maintenance windows | C — Surfaces & Ops | `0014_maintenance_windows.sql` |
| 0015 | reports schedules | C — Surfaces & Ops | `0015_report_schedules.sql` |
| 0016 | notification policies | B — People & MSP | `0016_notification_policies.sql` |

## Free numbers

**0017+** — first-come, first-served: claim the number in the PR
description, add a row to the pre-reserved table above in the same PR, then
write the file.
