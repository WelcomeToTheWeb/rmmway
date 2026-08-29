-- 0003_alerts.sql — W2-4: baseline-driven alerts + inbox.
-- Idempotent; safe to re-run.
--
-- The dynamic baselining engine (W2-3) flags an observation on every pass
-- while a metric stays anomalous. Without dedup that is one inbox row per
-- pass = an alert storm. This table is the deduped layer on top:
--
--   * ONE open alert per (device_id, name, source) series. A re-fired
--     anomaly on the same series bumps the existing open alert
--     (events++, max score, freshest observation) instead of creating a
--     second row — enforced by the partial unique index below.
--   * The alert auto-resolves when the series returns to baseline (the
--     engine stops flagging it for N consecutive passes); ack/resolve are
--     also available from the UI.
--
-- So a sustained anomaly on one series == one alert (no storm), a second
-- independent series == a second alert, and recovery == the alert closes.

CREATE TABLE IF NOT EXISTS alerts (
    id            bigserial PRIMARY KEY,
    device_id     text NOT NULL,
    name          text NOT NULL,           -- metric name (series key)
    source        text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'open',   -- open | acked | resolved
    -- Severity = peak robust z-score seen across the alert's lifetime.
    score         double precision NOT NULL,
    channel       text NOT NULL,           -- dominant fired channel (seasonal|trend)
    value         double precision NOT NULL,     -- the flagged observation (latest)
    expected      double precision,         -- the baseline median it deviated from
    -- How many anomalous observations fed this alert (dedup counter).
    events        integer NOT NULL DEFAULT 1,
    first_at      timestamptz NOT NULL,    -- first anomalous observation
    last_at       timestamptz NOT NULL,    -- most recent anomalous observation
    resolved_at   timestamptz,             -- set when status -> resolved
    acked_at      timestamptz,             -- set when status -> acked
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE alerts IS 'Deduped, baseline-driven operator alerts (W2-4): one open alert per anomalous series';

-- The dedup invariant: at most ONE row in a non-resolved state per series.
-- A new series-hour anomaly on an already-open series must UPDATE the open
-- row, never INSERT a sibling. Once resolved the unique slot frees up so the
-- series can re-alert on a later independent anomaly.
CREATE UNIQUE INDEX IF NOT EXISTS uq_alerts_one_open_per_series
    ON alerts (device_id, name, source)
    WHERE status IN ('open', 'acked');

CREATE INDEX IF NOT EXISTS idx_alerts_status_time
    ON alerts (status, last_at DESC);
CREATE INDEX IF NOT EXISTS idx_alerts_device
    ON alerts (device_id, last_at DESC);
CREATE INDEX IF NOT EXISTS idx_alerts_last_at
    ON alerts (last_at DESC);
