-- 0009: first-boot setup wizard (A-2)
--
-- A fresh database is not yet "initialized": the org identity (the CA that
-- agents pin), the operator's root admin credentials, and the SMTP outbox are
-- all things the operator should define on first boot rather than via
-- environment. Three small tables make that state explicit so the UI can
-- redirect a fresh install to a setup wizard, and skip it on every later boot.
--
-- server_setup:   one row (id=1) once setup is complete. NO ROW = fresh
--                 (the wizard triggers). A deployment that predates the
--                 wizard — already has enrolled devices — is grandfathered
--                 as set up by the setup service's Status (device count), so
--                 this stays a pure schema change.
-- admin_users:    operator accounts minted by the wizard (PBKDF2: salt +
--                 hash; the env admin stays a fallback and login checks the
--                 DB first).
-- server_config:  small key/value store for operator-defined settings:
--                 key='org_name' (JSON string) and key='smtp' (JSON config).

CREATE TABLE IF NOT EXISTS server_setup (
    id INT PRIMARY KEY DEFAULT 1,
    done BOOLEAN NOT NULL DEFAULT false,
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS admin_users (
    username TEXT PRIMARY KEY,
    salt BYTEA NOT NULL,
    pass_hash BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS server_config (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
