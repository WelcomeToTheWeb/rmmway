-- 0001_init.sql — W1-6: TimescaleDB schema (first migration).
-- Idempotent; safe to re-run.
--
-- Design note: the partition dimension is a real `ts timestamptz` column,
-- set by the ingest writer from the agent's `timestamp_ms`
-- (ts = to_timestamp(timestamp_ms / 1000.0)). We keep BOTH:
--   timestamp_ms  bigint     — raw agent wall clock, part of the idempotency
--                              PK (ON CONFLICT DO NOTHING, IDEA.md §1 outbox)
--   ts            timestamptz— the hypertable dimension (Timescale wants a
--                              real time type; a bigint/generated column won't
--                              do)
-- 1-day chunks. This migration has NOT been applied yet, so it is free to
-- change; once shipped it must never be edited in place — add 0002_*.sql.

CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;

-- devices: registry of enrolled agents (source of truth; W1-7 mirrors it
-- into Meilisearch, the W2-1 device list reads it).
CREATE TABLE IF NOT EXISTS devices (
    id            text PRIMARY KEY,          -- server-assigned, stable
    hostname      text NOT NULL,
    os            text NOT NULL,             -- linux | windows | darwin
    arch          text NOT NULL,             -- amd64 | arm64 | ...
    agent_version text NOT NULL DEFAULT '',
    interfaces    text[] NOT NULL DEFAULT '{}',
    tags          text[] NOT NULL DEFAULT '{}',
    online        boolean NOT NULL DEFAULT false,
    first_seen    timestamptz NOT NULL DEFAULT now(),
    last_seen     timestamptz NOT NULL DEFAULT now(),
    heartbeat_interval_s int NOT NULL DEFAULT 30,
    metric_interval_s    int NOT NULL DEFAULT 30
);
COMMENT ON TABLE devices IS 'Enrolled agent devices (W1-6)';

-- metrics: one row per sample.
CREATE TABLE IF NOT EXISTS metrics (
    device_id     text      NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name          text      NOT NULL,        -- cpu.utilization_percent, ...
    source        text      NOT NULL DEFAULT '',  -- sda1, eth0, '' = host-wide
    value         double precision NOT NULL,
    labels        jsonb     NOT NULL DEFAULT '{}',
    timestamp_ms  bigint    NOT NULL,        -- agent wall clock, Unix ms
    ts            timestamptz NOT NULL,      -- derived; the dimension
    ingested_at   timestamptz NOT NULL DEFAULT now(),
    -- PK must include the partitioning column (ts) per Timescale's rule.
    -- (timestamp_ms and ts are 1:1, so uniqueness is preserved.)
    PRIMARY KEY (device_id, name, source, timestamp_ms, ts)
);

-- Timescale hypertable, partitioned by ts (1-day chunks).
SELECT create_hypertable('metrics', 'ts',
    if_not_exists => TRUE,
    chunk_time_interval => INTERVAL '1 day'
);

-- Ingest order: agents send newest samples last; queries are usually
-- "last N hours for device X" — keep the natural insertion index.
CREATE INDEX IF NOT EXISTS idx_metrics_lookup
    ON metrics (device_id, name, source, ts DESC);

-- Continuous aggregate: per-device per-metric 1-minute rollups for fast
-- long-range queries (W2-3 baselining + W2-1 device views).
CREATE MATERIALIZED VIEW IF NOT EXISTS metrics_1m
WITH (timescaledb.continuous) AS
SELECT
    device_id,
    name,
    source,
    time_bucket(INTERVAL '1 minute', ts) AS bucket,
    avg(value)   AS avg_value,
    min(value)   AS min_value,
    max(value)   AS max_value,
    count(*)     AS n
FROM metrics
GROUP BY device_id, name, source, bucket
WITH NO DATA;

-- Auto-materialize the aggregate for the last 5 minutes on a schedule
-- (also backfills history, since the CA was created WITH NO DATA).
SELECT add_continuous_aggregate_policy('metrics_1m',
    start_offset    => INTERVAL '10 minutes',
    end_offset      => INTERVAL '5 minutes',
    schedule_interval => INTERVAL '1 minute',
    if_not_exists   => TRUE
);

-- Retention: keep raw samples 90 days (rollups persist; W4-3 export +
-- MinIO cold tier can extend history beyond that).
SELECT add_retention_policy('metrics',
    drop_after => INTERVAL '90 days',
    if_not_exists => TRUE
);
