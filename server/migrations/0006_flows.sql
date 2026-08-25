-- 0006_flows.sql — W5-2: event-driven automation chains over NATS.
-- Idempotent; safe to re-run.
--
-- A flow is a small DAG (trigger -> actions) stored as validated JSONB. The
-- engine is event-driven: every hop of every run travels the NATS bus
-- (rmmway.events.*), and the database only holds the replay-safe state:
--
--   flows       — the composed DAGs (name, graph JSONB, enabled).
--   flow_runs   — one row per chain execution (trigger event -> terminal
--                 state). Every node hop is a conditional UPDATE
--                 (WHERE status='running' AND current_node=<node>) + an
--                 append to flow_events, so re-delivered NATS events are
--                 no-ops (at-least-once delivery is safe by construction).
--   flow_events — append-only audit log: every node start/complete/branch
--                 lands here (W6-3's "audited" E2E reads this).
--
-- Replay safety (same pattern as 0005_selfheal):
--   * at most ONE active run per (flow, device, source) — a partial unique
--     index makes a double-start from a re-delivered trigger impossible at
--     the DB layer, not just in code;
--   * state hops are conditional UPDATEs; a replay affects 0 rows = no-op;
--   * a server restart mid-run leaves the run at its last persisted node;
--     the engine's sweep re-publishes the step event and the chain resumes.

CREATE TABLE IF NOT EXISTS flows (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    -- The composed DAG: { nodes: [ {id, kind, name, ...} ] } where kind is
    -- trigger|script|check|notify. Exactly one trigger (the entry node);
    -- each node carries its outgoing edge(s): linear nodes have "next",
    -- check nodes have "then"/"else". Validated server-side on write.
    graph       jsonb NOT NULL,
    -- Min gap between runs started for the same (device, source) — 0 =
    -- none (only the one-active-run invariant applies).
    cooldown_seconds int NOT NULL DEFAULT 0,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE flows IS 'W5-2 automation chains: DAGs of trigger -> script/check/notify nodes, executed over the NATS event bus';
CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_name ON flows (name);
CREATE INDEX IF NOT EXISTS idx_flows_enabled ON flows (enabled) WHERE enabled;

CREATE TABLE IF NOT EXISTS flow_runs (
    id           bigserial PRIMARY KEY,
    flow_id      bigint REFERENCES flows(id) ON DELETE SET NULL,
    flow_name    text NOT NULL,            -- denormalized: survives flow delete
    device_id    text NOT NULL,
    source       text NOT NULL DEFAULT '', -- the triggered series source ('' = any)
    -- Terminal: succeeded, failed, timeout. Active: running.
    status       text NOT NULL,
    reason       text NOT NULL DEFAULT '', -- why a run ended (fail/timeout/complete)
    trigger_value double precision,        -- the measurement that fired the trigger
    triggered_at timestamptz,              -- ts of that measurement
    current_node text NOT NULL,            -- the node this run is executing
    check_after  timestamptz,              -- a check node re-measures samples strictly after this
    command_id   text,                     -- the dispatched command of the current script node
    dispatched_at timestamptz,             -- when that script was dispatched
    finished_at  timestamptz,
    started_at   timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE flow_runs IS 'W5-2 chain executions: one row per trigger event, node-hops conditional + audited in flow_events';

-- Replay-safety backstop: one active run per (flow, device, source).
CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_one_active
    ON flow_runs (flow_id, device_id, source)
    WHERE status = 'running';

CREATE INDEX IF NOT EXISTS idx_flow_runs_lookup
    ON flow_runs (flow_id, device_id, source, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_flow_runs_status
    ON flow_runs (status, created_at DESC);

-- Append-only node log (audit trail): every node hop lands here.
CREATE TABLE IF NOT EXISTS flow_events (
    id       bigserial PRIMARY KEY,
    run_id   bigint NOT NULL REFERENCES flow_runs(id) ON DELETE CASCADE,
    node     text NOT NULL,           -- the node id the hop concerns
    status   text NOT NULL,           -- entered|dispatched|waiting|ok|failed|branched
    reason   text NOT NULL DEFAULT '',
    at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_flow_events_run ON flow_events (run_id, at);
