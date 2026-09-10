-- 0016_notification_policies.sql — gap #6: notification routing policies
-- (wave 3, lane B).
--
-- Policies define which notification channels fire for which event
-- categories, scoped by client and role. A deployment starts with no
-- policies (every event goes nowhere); the admin adds policies to route
-- alerts to the appropriate people/channels.
--
-- Policy resolution: for each notification event, the router matches
-- policies by category, client_id (NULL = any), and role (NULL = any).
-- All matching policies' channels fire.
--
-- Idempotent: pure IF NOT EXISTS — safe to re-run.

CREATE TABLE IF NOT EXISTS notification_policies (
    id         text PRIMARY KEY,              -- 'pol-' + 12 hex
    category   text NOT NULL
               CHECK (category IN ('alert', 'escalation')),
    client_id  text REFERENCES clients (id) ON DELETE SET NULL,
    role       text CHECK (role IS NULL OR role IN ('admin', 'tech', 'viewer')),
    channels   text[] NOT NULL DEFAULT '{}',  -- channel IDs to fire
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_policies_category ON notification_policies (category);
CREATE INDEX IF NOT EXISTS idx_notification_policies_client ON notification_policies (client_id);
CREATE INDEX IF NOT EXISTS idx_notification_policies_role ON notification_policies (role);
COMMENT ON TABLE notification_policies IS
    'gap #6: notification routing policies (wave 3, lane B)';