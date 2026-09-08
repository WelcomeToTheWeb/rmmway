-- 0010_clients.sql — gap #2: client/tenant (MSP) model.
--
-- Clients are the MSP tenants a device belongs to. A deployment starts
-- life with a single default client ('unassigned', id 'unassigned') so
-- the retrofit never strands a device; every pre-existing device is
-- backfilled onto it below.
--
-- Idempotent: safe to re-run (re-runs only re-assert the seed and the
-- backfill, both no-ops once applied).

CREATE TABLE IF NOT EXISTS clients (
    id          text PRIMARY KEY,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_clients_name ON clients (name);
COMMENT ON TABLE clients IS 'gap #2: MSP client/tenant registry (wave 1, lane B)';

ALTER TABLE devices
    ADD COLUMN IF NOT EXISTS client_id text
    REFERENCES clients (id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_devices_client ON devices (client_id);
COMMENT ON COLUMN devices.client_id IS
    'gap #2: owning client; NULL = unassigned (surfaced under the default client)';

-- Seed the default client and backfill pre-existing devices. The fixed id
-- 'unassigned' is stable across installs (the API and UI reference it
-- directly, mirroring the 'system' webhook id convention in 0008).
INSERT INTO clients (id, name, description)
VALUES ('unassigned', 'Unassigned',
        'Default client for devices without an explicit assignment')
ON CONFLICT (id) DO NOTHING;

UPDATE devices SET client_id = 'unassigned' WHERE client_id IS NULL;
