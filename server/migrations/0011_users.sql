-- 0011_users.sql — gap #3: operator users, RBAC roles, client grants,
-- TOTP MFA, and operator API tokens (wave 2, lane B).
--
-- Replaces the single-role operator assumption (one wizard/env admin,
-- every route flat requireOperator) with:
--
--   users        — named operator accounts: PBKDF2-SHA256 password (the
--                  same 100k/32-byte params admin_users uses), one of the
--                  roles admin|tech|viewer, an enabled flag, optional
--                  TOTP MFA (base32 secret + first-verification stamp),
--                  and a last-login stamp (the Users UI's session view).
--   user_clients — per-client grants for non-admin roles. Empty grants =
--                  the account sees no clients; the admin role bypasses
--                  the grant table entirely.
--   api_tokens   — operator API tokens ("rmm_…"). Only the SHA-256 hash of
--                  the full token is stored; the token itself is shown
--                  once at creation.
--
-- The legacy admin_users table (0009_setup) is NOT touched: the wizard's
-- root admin stays there and keeps signing in — login checks users first,
-- then falls back to the legacy admin_users/env path for usernames with no
-- users row. New accounts are always minted into users (the Users API).
--
-- Idempotent: pure IF NOT EXISTS, no seed rows — safe to re-run.

CREATE TABLE IF NOT EXISTS users (
    id               text PRIMARY KEY,          -- 'usr-' + 12 hex (server-minted)
    username         text NOT NULL,
    role             text NOT NULL
                     CHECK (role IN ('admin', 'tech', 'viewer')),
    enabled          boolean NOT NULL DEFAULT true,
    totp_secret      text NOT NULL DEFAULT '',  -- base32; '' = not enrolled
    totp_verified_at timestamptz,               -- NULL = enrolled, unverified
    password_salt    bytea NOT NULL,
    password_hash    bytea NOT NULL,
    last_login_at    timestamptz,               -- session view in the Users UI
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
-- Case-INSENSITIVE username uniqueness (the API and the in-memory store
-- treat "admin" and "Admin" as the same account). Re-asserted with a DROP
-- so a dev database that ran an earlier draft (plain (username)) upgrades
-- in place; idempotent on fresh installs.
DROP INDEX IF EXISTS uq_users_username;
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_username ON users (lower(username));
COMMENT ON TABLE users IS
    'gap #3: operator accounts with RBAC roles + TOTP MFA (wave 2, lane B)';

CREATE TABLE IF NOT EXISTS user_clients (
    user_id   text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client_id text NOT NULL REFERENCES clients (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, client_id)
);
COMMENT ON TABLE user_clients IS
    'gap #3: per-client grants for non-admin users (wave 2, lane B)';

CREATE TABLE IF NOT EXISTS api_tokens (
    id           text PRIMARY KEY,              -- 'apit-' + 12 hex
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    token_prefix text NOT NULL,                 -- first 12 chars, for display
    token_hash   text NOT NULL,                 -- SHA-256 (hex) of the full token
    expires_at   timestamptz,                   -- NULL = no expiry
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_api_tokens_hash ON api_tokens (token_hash);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens (user_id);
COMMENT ON TABLE api_tokens IS
    'gap #3: operator API tokens, hash-only storage (wave 2, lane B)';
