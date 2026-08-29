-- 0007_log_events.sql — W6-1: indexed agent log events (Timescale).
-- Idempotent; safe to re-run.
--
-- The agent tails its structured JSON-lines log and ships every batch two
-- ways: to Loki (queryable in the log stack) AND as LogBatch frames on the
-- gRPC Stream uplink, where they land in this hypertable so the RMM can
-- surface recent indexed events per device without querying Loki.
--
-- Replay safety (same pattern as the metrics table, IDEA.md §1 outbox):
-- each entry carries a STABLE agent-generated id (sha256 of device|line),
-- so ON CONFLICT DO NOTHING makes a re-sent batch after a reconnect a
-- no-op. The hypertable dimension is ts (derived from timestamp_ms); the
-- PK includes ts per Timescale's rule (id and ts are 1:1 in practice —
-- the id is derived from the line, which carries the timestamp).

CREATE TABLE IF NOT EXISTS log_events (
    id           text        NOT NULL,       -- agent-generated, content-derived (dedup key)
    device_id    text        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    level        text        NOT NULL DEFAULT 'info',  -- DEBUG | INFO | WARN | ERROR
    msg          text        NOT NULL DEFAULT '',
    attrs        jsonb       NOT NULL DEFAULT '{}',    -- structured slog attributes
    timestamp_ms bigint      NOT NULL,       -- agent wall clock, Unix ms
    ts           timestamptz NOT NULL,       -- derived; the dimension
    ingested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, ts)
);
COMMENT ON TABLE log_events IS 'Indexed agent log events, per-device (W6-1)';

SELECT create_hypertable('log_events', 'ts',
    if_not_exists => TRUE,
    chunk_time_interval => INTERVAL '1 day'
);

-- The RMM's read pattern is "recent events for device X" (newest first).
CREATE INDEX IF NOT EXISTS idx_log_events_lookup
    ON log_events (device_id, ts DESC);
