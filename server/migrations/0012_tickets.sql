-- 0012_tickets.sql — gap #7: helpdesk / ticketing (wave 3, lane B).
--
-- Tickets are the "someone should look" half of the self-heal escalation
-- flow (busHealNotifier.Escalate -> real ticket) and also exist as
-- manually created items in a queue with SLA timers.
--
-- Status state machine: open -> in_progress -> resolved -> closed.
-- Tickets can be re-opened at any time (resolved/closed -> open).
--
-- SLA: first_response_due and resolution_due are computed at creation
-- time from the SLA policy (stored in the server_config SLA rules; the
-- tickets domain reads them). Tracked timestamps: first_response_at,
-- resolved_at, closed_at.
--
-- Idempotent: pure IF NOT EXISTS — safe to re-run.

CREATE TABLE IF NOT EXISTS tickets (
    id                     text PRIMARY KEY,           -- 'tkt-' + 12 hex
    title                  text NOT NULL,
    description            text NOT NULL DEFAULT '',
    queue                  text NOT NULL DEFAULT 'general',
    status                 text NOT NULL DEFAULT 'open'
                           CHECK (status IN ('open', 'in_progress', 'resolved', 'closed')),
    priority               text NOT NULL DEFAULT 'medium'
                           CHECK (priority IN ('low', 'medium', 'high', 'critical')),
    device_id              text,
    client_id              text REFERENCES clients (id) ON DELETE SET NULL,
    assigned_to            text REFERENCES users (id) ON DELETE SET NULL,
    reporter               text,
    sla_first_response_due timestamptz,
    sla_resolution_due     timestamptz,
    first_response_at      timestamptz,
    resolved_at            timestamptz,
    closed_at              timestamptz,
    heal_run_id            bigint,
    source                 text NOT NULL DEFAULT 'manual'
                           CHECK (source IN ('manual', 'heal', 'flow', 'alert')),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets (status);
CREATE INDEX IF NOT EXISTS idx_tickets_queue ON tickets (queue);
CREATE INDEX IF NOT EXISTS idx_tickets_assigned_to ON tickets (assigned_to);
CREATE INDEX IF NOT EXISTS idx_tickets_client ON tickets (client_id);
CREATE INDEX IF NOT EXISTS idx_tickets_device ON tickets (device_id);
COMMENT ON TABLE tickets IS
    'gap #7: helpdesk tickets with queue/assignment/priority/SLA (wave 3, lane B)';

CREATE TABLE IF NOT EXISTS ticket_notes (
    id          text PRIMARY KEY,
    ticket_id   text NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    author      text,
    content     text NOT NULL,
    is_internal boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ticket_notes_ticket ON ticket_notes (ticket_id);
COMMENT ON TABLE ticket_notes IS
    'gap #7: ticket activity notes (wave 3, lane B)';