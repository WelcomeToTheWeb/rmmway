-- 0004_org_ca.sql: W3-1 mTLS agent channel — org PKI tables.
--
-- org_ca: the single org root CA (one row, id=1). Persisted so a server
-- restart reuses the same root (existing devices' leaf certs stay valid).
-- device_certs: the most recent leaf issued to each device (audit/reissue
-- record; the device keeps its own copy on its disk).

CREATE TABLE IF NOT EXISTS org_ca (
    id             smallint      PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    root_cert_pem  bytea         NOT NULL,
    root_key_pem   bytea         NOT NULL,
    created_at     timestamptz   NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS device_certs (
    device_id      text          PRIMARY KEY,
    leaf_cert_pem  bytea         NOT NULL,
    leaf_key_pem   bytea         NOT NULL,
    issued_at      timestamptz   NOT NULL DEFAULT now()
);
