-- 0014_maintenance_windows.sql — gap #10b: maintenance windows + snooze
-- (wave 3, lane C).
--
-- Per-device / per-tag / per-client maintenance windows that suppress
-- baseline alerting and self-healing during the window. Two table styles
-- (matching the existing flow/alert/alert patterns in this schema):
--
--   maintenance_windows — the schedule table: named, recurring (optional
--                          cron) or one-off windows scoped to a device,
--                          a tag, or a client (mutually exclusive scope
--                          via nullable FKs; validation in the API layer).
--   window_snoozes      — transient, operator-initiated snoozes from the
--                          Alerts UI ("snooze N hours"). Not recurring,
--                          short-lived, no named schedule.
--
-- Both tables carry a device_id OR tag OR client_id scope — the baseline
-- engine checks all three scopes when it evaluates an anomaly.
--
-- Idempotent: IF NOT EXISTS, no seed rows.

CREATE TABLE IF NOT EXISTS maintenance_windows (
    id           bigserial PRIMARY KEY,
    name         text NOT NULL,
    device_id    text,
    tag          text,
    client_id    text,
    starts_at    timestamptz NOT NULL,
    ends_at      timestamptz NOT NULL,
    recurrence   text,          -- cron expression; NULL = one-off
    note         text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_mw_device ON maintenance_windows (device_id) WHERE device_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_mw_tag ON maintenance_windows (tag) WHERE tag IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_mw_client ON maintenance_windows (client_id) WHERE client_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_mw_range ON maintenance_windows (starts_at, ends_at);
COMMENT ON TABLE maintenance_windows IS
    'gap #10b: per-device/tag/client maintenance windows that suppress alerting + self-healing';

CREATE TABLE IF NOT EXISTS window_snoozes (
    id           bigserial PRIMARY KEY,
    device_id    text,
    tag          text,
    client_id    text,
    metric       text,
    alert_id     bigint,
    starts_at    timestamptz NOT NULL DEFAULT now(),
    ends_at      timestamptz NOT NULL,
    note         text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_snooze_device ON window_snoozes (device_id) WHERE device_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_snooze_tag ON window_snoozes (tag) WHERE tag IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_snooze_client ON window_snoozes (client_id) WHERE client_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_snooze_metric ON window_snoozes (device_id, metric) WHERE device_id IS NOT NULL AND metric IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_snooze_range ON window_snoozes (starts_at, ends_at);
COMMENT ON TABLE window_snoozes IS
    'gap #10b: transient per-device/tag/client snoozes from the Alerts UI';
