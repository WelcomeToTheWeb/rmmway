-- 0005_selfheal.sql — W5-1: self-healing playbook engine.
-- Idempotent; safe to re-run.
--
-- The engine is a deterministic, replay-safe state machine over two tables:
--
--   playbooks  — declarative rules: detect (metric condition) -> verify-safe
--                -> remediate (per-OS script dispatched as a RunScript) ->
--                confirm (RE-measure the metric) -> escalate (ticket+notify).
--   heal_runs  — one row per (playbook, device, source) remediation attempt.
--                Every stage is timestamped on the row AND appended to
--                heal_events, so a run's history is auditable and the
--                current status can be reconstructed from the event log.
--
-- Replay safety (the whole point of W5-1):
--   * at most ONE active run per (playbook, device, source) — the partial
--     unique index below makes a double-remediation impossible at the DB
--     layer, not just in code; a colliding INSERT is a no-op skip.
--   * state transitions are conditional UPDATEs (WHERE status = <from>);
--     re-applying an already-applied transition affects 0 rows = no-op.
--   * a server restart mid-run leaves the run in its last persisted stage;
--     the next pass picks it up from the row (no in-memory run state).

CREATE TABLE IF NOT EXISTS playbooks (
    key                    text PRIMARY KEY,
    name                   text NOT NULL,
    description            text NOT NULL DEFAULT '',
    -- detect: the LATEST sample of this metric (source '' = any source)
    -- must satisfy the condition. Freshness + os_filter are the safety
    -- guard: we only act on devices that are reporting AND that the
    -- playbook knows how to remediate.
    metric                 text NOT NULL,
    source                 text NOT NULL DEFAULT '',
    detect_op              text NOT NULL,      -- '>'|'>='|'=='|'<'|'<='
    detect_threshold       double precision NOT NULL,
    os_filter              text NOT NULL DEFAULT '',   -- '' = all; 'linux', 'windows', 'darwin', or comma list
    fresh_within_seconds   int NOT NULL DEFAULT 900,   -- sample staleness limit (no acting on stale data)
    cooldown_seconds       int NOT NULL DEFAULT 3600,  -- min gap between ACTUAL remediations (dispatched_at set)
    -- remediate: per-OS script (placeholder {{source}} is substituted with
    -- the detected source, e.g. the volume or service name). Shipped as a
    -- RunScript (lang sh|powershell) with the W3-3 capability token.
    remediate_sh           text NOT NULL DEFAULT '',
    remediate_powershell   text NOT NULL DEFAULT '',
    -- confirm: the post-remediation RE-measurement must satisfy this
    -- condition to count as resolved.
    confirm_op             text NOT NULL,
    confirm_threshold      double precision NOT NULL,
    remediate_timeout_seconds int NOT NULL DEFAULT 300, -- no command result by then -> escalate
    confirm_wait_seconds    int NOT NULL DEFAULT 600,   -- no fresh re-measurement by then -> escalate
    enabled                boolean NOT NULL DEFAULT true,
    updated_at             timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE playbooks IS 'W5-1 self-healing playbook definitions (detect -> verify-safe -> remediate -> confirm -> escalate)';

CREATE TABLE IF NOT EXISTS heal_runs (
    id            bigserial PRIMARY KEY,
    playbook_key  text NOT NULL REFERENCES playbooks(key),
    device_id     text NOT NULL,
    source        text NOT NULL DEFAULT '',    -- the detected series source (volume / service)
    -- The state machine. Terminal states: resolved, escalated, failed,
    -- skipped. Active states: detected, verifying, remediating, confirming.
    status        text NOT NULL,
    reason        text NOT NULL DEFAULT '',    -- why a run stopped (skip/fail/escalate detail)
    detect_value  double precision,            -- the failing measurement (detect stage)
    detect_at     timestamptz,                 -- ts of that measurement
    command_id    text,                        -- the dispatched remediation command
    dispatched_at timestamptz,                 -- remediate stage
    remediated_at timestamptz,                 -- the command's SUCCEEDED report
    confirm_value double precision,            -- the re-measurement (confirm stage)
    confirmed_at  timestamptz,                 -- set when resolved
    escalated_at  timestamptz,                 -- set when escalated (ticket)
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE heal_runs IS 'W5-1 self-heal runs: one row per remediation attempt, stage-timestamped; the escalated row IS the ticket';

-- Replay-safety backstop: one active run per (playbook, device, source).
CREATE UNIQUE INDEX IF NOT EXISTS uq_heal_one_active
    ON heal_runs (playbook_key, device_id, source)
    WHERE status IN ('detected', 'verifying', 'remediating', 'confirming');

CREATE INDEX IF NOT EXISTS idx_heal_runs_lookup
    ON heal_runs (playbook_key, device_id, source, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_heal_runs_status
    ON heal_runs (status, created_at DESC);

-- Append-only stage log: every transition lands here (audit trail; W6-3's
-- "audited" E2E reads this). Also reconstructs a run's history after the
-- run row's terminal state is long gone.
CREATE TABLE IF NOT EXISTS heal_events (
    id      bigserial PRIMARY KEY,
    run_id  bigint NOT NULL REFERENCES heal_runs(id) ON DELETE CASCADE,
    status  text NOT NULL,            -- the state the run moved TO
    reason  text NOT NULL DEFAULT '',
    at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_heal_events_run ON heal_events (run_id, at);

-- ---- starter library (W5-1 DoD) --------------------------------------------
-- disk.full      — volume > 90% used: free reclaimable space, re-measure.
-- service.down   — monitored service stopped (service.status == 0, source =
--                  service name): restart it, re-measure (== 1).
-- wsus.stuck     — Windows Update client stuck (wsus.update_state == 3):
--                  reset WU client state, re-trigger detection, re-measure.
-- ON CONFLICT DO NOTHING: seeded once; operators may then edit the rows
-- (scripts, thresholds, enabled) — edits survive restart.
INSERT INTO playbooks (key, name, description, metric, source, detect_op, detect_threshold,
                       os_filter, fresh_within_seconds, cooldown_seconds,
                       remediate_sh, remediate_powershell,
                       confirm_op, confirm_threshold,
                       remediate_timeout_seconds, confirm_wait_seconds, enabled)
VALUES
('disk.full', 'Disk nearly full',
 'A volume is above 90% used. Free reclaimable space (old logs, temp files) and re-measure; escalate if the volume is still full.',
 'disk.used_percent', '', '>', 90.0,
 '', 900, 3600,
 $sh_disk$#!/bin/sh
# rmmway self-heal: disk.full — free reclaimable space, then report usage
journalctl --vacuum-time=7d 2>/dev/null || true
find /tmp /var/tmp -xdev -type f -atime +3 -delete 2>/dev/null || true
df -hP / 2>/dev/null || df -h
exit 0
$sh_disk$,
 $ps_disk$# rmmway self-heal: disk.full — free temp + recycle-bin space, then report
Get-ChildItem -Path $env:TEMP -File -ErrorAction SilentlyContinue |
    Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-3) } |
    Remove-Item -Force -ErrorAction SilentlyContinue
Clear-RecycleBin -Force -ErrorAction SilentlyContinue
Get-PSDrive -PSProvider FileSystem | Format-Table Name, Used, Free
exit 0
$ps_disk$,
 '<=', 90.0,
 300, 600, true),
('service.down', 'Service down',
 'A monitored service (metric service.status, source = service name) is stopped (0). Restart it and re-measure; escalate if it is still stopped.',
 'service.status', '', '==', 0.0,
 '', 900, 1800,
 $sh_svc$#!/bin/sh
# rmmway self-heal: service.down — restart the stopped service "{{source}}"
set -e
systemctl restart "{{source}}"
systemctl is-active "{{source}}"
$sh_svc$,
 $ps_svc$# rmmway self-heal: service.down — restart the stopped service "{{source}}"
Restart-Service -Name "{{source}}" -Force
(Get-Service -Name "{{source}}").Status
exit 0
$ps_svc$,
 '==', 1.0,
 300, 300, true),
('wsus.stuck', 'WSUS / Windows Update stuck',
 'The Windows Update client is stuck (metric wsus.update_state == 3: detection/scan timeout). Reset WU client state, re-trigger detection, and re-measure; escalate if it is still stuck.',
 'wsus.update_state', '', '==', 3.0,
 'windows', 900, 7200,
 '',
 $ps_wsus$# rmmway self-heal: wsus.stuck — reset the WU client and re-trigger detection
Stop-Service -Name UsoSvc -Force -ErrorAction SilentlyContinue
wuauclt /resetauthorization /rebootnow
wuauclt /detectnow
Start-Service -Name UsoSvc -ErrorAction SilentlyContinue
Get-Service -Name UsoSvc | Select-Object Name, Status
exit 0
$ps_wsus$,
 '<=', 2.0,
 600, 1800, true)
ON CONFLICT (key) DO NOTHING;
