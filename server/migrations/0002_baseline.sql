-- 0002_baseline.sql — W2-3: dynamic baselining engine state.
-- Idempotent; safe to re-run.
--
-- baseline_anomalies: one row per flagged (series, observation hour).
-- The dynamic baselining job (server/internal/baseline) runs
-- deterministically in-process and records its anomalies here via an
-- upsert keyed on (device_id, name, source, at_hour): each run re-scores
-- only a series' LATEST observation, so re-runs in the same hour update
-- the row instead of duplicating it (no alert storm from the engine
-- itself — W2-4 adds deduped alerts on top).

CREATE TABLE IF NOT EXISTS baseline_anomalies (
    id          bigserial PRIMARY KEY,
    device_id   text NOT NULL,
    name        text NOT NULL,
    source      text NOT NULL DEFAULT '',
    -- Observation time floored to the hour: the engine's natural dedup key
    -- (hourly means are what it scores).
    at_hour     timestamptz NOT NULL,
    at          timestamptz NOT NULL,      -- exact observation time
    value       double precision NOT NULL, -- the anomalous hourly mean
    score       double precision NOT NULL, -- max z of the fired channels
    channel     text NOT NULL,             -- seasonal | trend (dominant)
    seasonal_z  double precision,
    seasonal_med double precision,
    seasonal_mad double precision,
    seasonal_cells int,
    trend_z     double precision,
    trend_med   double precision,
    trend_mad   double precision,
    trend_cells int,
    detected_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (device_id, name, source, at_hour)
);
COMMENT ON TABLE baseline_anomalies IS 'Flagged observations from the dynamic baselining engine (W2-3)';

CREATE INDEX IF NOT EXISTS idx_baseline_anomalies_recent
    ON baseline_anomalies (at DESC);
CREATE INDEX IF NOT EXISTS idx_baseline_anomalies_device
    ON baseline_anomalies (device_id, at DESC);
