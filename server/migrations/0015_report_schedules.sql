-- 0015_report_schedules.sql — gap #8b: scheduled + on-demand reports
-- (wave 3, lane C).
--
-- report_schedules — named, recurring report jobs (e.g. "daily fleet
-- status CSV to ops@acme.com"). Each schedule runs its report type
-- (fleet_status, device, patch_compliance, license_compliance,
-- uptime_sla) on the specified cron interval, targeting an optional
-- client, and storing the generated output blob in MinIO.
--
-- report_runs — the history of report executions (scheduled or manual):
-- when it ran, who triggered it, where the output went, success/fail.
--
-- Idempotent: IF NOT EXISTS, no seed rows.

CREATE TABLE IF NOT EXISTS report_schedules (
    id           bigserial PRIMARY KEY,
    name         text NOT NULL,
    report_type  text NOT NULL
                  CHECK (report_type IN (
                      'fleet_status', 'device', 'patch_compliance',
                      'license_compliance', 'uptime_sla')),
    client_id    text,
    schedule     text NOT NULL,       -- ISO 8601 duration (e.g. '24h')
    output_format text NOT NULL DEFAULT 'csv'
                  CHECK (output_format IN ('csv', 'json', 'pdf')),
    enabled      boolean NOT NULL DEFAULT true,
    note         text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_report_schedules_type ON report_schedules (report_type);
CREATE INDEX IF NOT EXISTS idx_report_schedules_client ON report_schedules (client_id) WHERE client_id IS NOT NULL;
COMMENT ON TABLE report_schedules IS
    'gap #8b: named recurring report schedules (fleet status, device, patch, license, uptime/SLA)';

CREATE TABLE IF NOT EXISTS report_runs (
    id           bigserial PRIMARY KEY,
    schedule_id  bigint REFERENCES report_schedules (id) ON DELETE SET NULL,
    report_type  text NOT NULL,
    client_id    text,
    output_format text NOT NULL DEFAULT 'csv',
    triggered_by text NOT NULL,       -- 'schedule', 'api', or username
    status       text NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'completed', 'failed')),
    object_key   text,                -- MinIO object key for output
    error        text,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX IF NOT EXISTS idx_report_runs_schedule ON report_runs (schedule_id) WHERE schedule_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_report_runs_type ON report_runs (report_type);
CREATE INDEX IF NOT EXISTS idx_report_runs_started ON report_runs (started_at DESC);
COMMENT ON TABLE report_runs IS
    'gap #8b: report execution history (scheduled + manual)';
