-- 0007_webhooks.sql — W6-2: expose the NATS event bus as signed webhooks
-- (HMAC) + a live SSE/subscription stream, with retries and replay.
-- Idempotent; safe to re-run.
--
-- Two tables:
--
--   webhooks       — user-defined endpoints. Each row is a URL the operator
--                    wants events delivered to, an HMAC secret shared with
--                    that endpoint (so it can verify authenticity), and the
--                    set of event categories it is interested in
--                    (alert | inventory | automation).
--
--   webhook_events — the append-only journal of every event the framework
--                    saw on the bus. This is the "replayable" half: a
--                    monotonic `seq` is the delivery cursor AND the id a
--                    receiver dedupes on. An endpoint's `last_seq` (on the
--                    webhooks row) is how far it has been delivered; the
--                    sweep re-drives seq > last_seq until it 200s, so a
--                    flaky or downed receiver catches up from its cursor
--                    instead of losing events. `data` holds the full bus
--                    event (flow.Event) so a replay is byte-faithful.
--
-- Delivery / retry / dead-letter (all on the webhooks row, driven by the
-- sweep, no separate queue table needed):
--   * attempts     — consecutive failed deliveries of the NEXT pending event;
--                    reset to 0 on a 200.
--   * next_retry_at — exponential backoff watermark; the sweep only touches
--                    an endpoint once now() >= next_retry_at.
--   * status       — "ok" (delivering) | "failing" (dead-lettered after
--                    max_attempts consecutive failures; an operator resumes
--                    it with PATCH enabled / replay).
--
-- New endpoints start with last_seq = the current max journal seq (they
-- only receive events from the moment they are created, not the whole
-- history); a replay resets last_seq to resend.

CREATE TABLE IF NOT EXISTS webhooks (
    id             bigserial PRIMARY KEY,
    name           text NOT NULL,
    url            text NOT NULL,
    -- HMAC-SHA256 key shared with the endpoint. The signature header is
    -- X-RMMway-Signature: t=<unix>,v1=<hex hmac(secret, ts+"."+body)>; the
    -- receiver recomputes it to verify authenticity + a freshness window.
    secret         text NOT NULL,
    -- Which categories this endpoint receives: subset of
    -- {alert, inventory, automation, other}. Empty array = none.
    categories     text[] NOT NULL DEFAULT '{}',
    enabled        boolean NOT NULL DEFAULT true,
    -- Delivery knobs (defaults chosen for a typical external endpoint).
    max_attempts   int NOT NULL DEFAULT 5,
    timeout_ms     int NOT NULL DEFAULT 5000,
    -- Delivery state (cursor + retry / dead-letter), see header.
    last_seq       bigint NOT NULL DEFAULT 0,
    attempts       int NOT NULL DEFAULT 0,
    next_retry_at  timestamptz NOT NULL DEFAULT now(),
    status         text NOT NULL DEFAULT 'ok',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhooks_enabled ON webhooks (enabled);

CREATE TABLE IF NOT EXISTS webhook_events (
    seq        bigserial PRIMARY KEY,
    -- alert | inventory | automation | other (derived from the bus subject).
    category   text NOT NULL,
    -- the bus subject (e.g. rmmway.events.alert) — the event's concrete type.
    type       text NOT NULL,
    device_id  text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL,
    -- the full flow.Event payload (lossless; a replay is byte-faithful).
    data       jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhook_events_category_seq ON webhook_events (category, seq);
CREATE INDEX IF NOT EXISTS idx_webhook_events_seq ON webhook_events (seq);
