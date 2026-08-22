package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// ---- Postgres MetricsSink ----------------------------------------------------

// PostgresMetricsSink writes metric batches into the metrics hypertable.
// Writes are idempotent: the PK (device_id, name, source, timestamp_ms) +
// ON CONFLICT DO NOTHING makes offline replay at-least-once without
// double-counting (IDEA.md §1 outbox story).
type PostgresMetricsSink struct {
	db *pgxpool.Pool
}

func NewPostgresMetricsSink(db *pgxpool.Pool) *PostgresMetricsSink {
	return &PostgresMetricsSink{db: db}
}

func (s *PostgresMetricsSink) Write(deviceID string, batch *agentv1.MetricBatch) error {
	if len(batch.GetSamples()) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, m := range batch.GetSamples() {
		// ts is derived from timestamp_ms so it always matches the row's
		// PK. DO NOTHING makes replay idempotent (a re-sent sample with the
		// same (device, name, source, timestamp_ms) is a no-op — IDEA.md §1).
		_, err = tx.Exec(ctx, `
			INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1, $2, $3, $4, $5, $6::bigint, to_timestamp(($6::bigint) / 1000.0))
			ON CONFLICT DO NOTHING`,
			deviceID, m.GetName(), m.GetSource(), m.GetValue(),
			labelsJSON(m.GetLabels()), m.GetTimestampMs())
		if err != nil {
			return fmt.Errorf("insert metric %s: %w", m.GetName(), err)
		}
	}
	return tx.Commit(ctx)
}

func labelsJSON(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- Postgres DeviceStore ----------------------------------------------------

// PostgresDevices implements DeviceStore on the devices table.
type PostgresDevices struct {
	db *pgxpool.Pool
}

func NewPostgresDevices(db *pgxpool.Pool) *PostgresDevices {
	return &PostgresDevices{db: db}
}

func (d *PostgresDevices) Register(ctx context.Context, id, hostname, os, arch, agentVersion string, interfaces []string, heartbeatIntS, metricIntS int32) error {
	if interfaces == nil {
		interfaces = []string{}
	}
	_, err := d.db.Exec(ctx, `
		INSERT INTO devices (id, hostname, os, arch, agent_version, interfaces,
			heartbeat_interval_s, metric_interval_s, online, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,true,now())
		ON CONFLICT (id) DO UPDATE SET
			hostname = EXCLUDED.hostname,
			os = EXCLUDED.os,
			arch = EXCLUDED.arch,
			agent_version = EXCLUDED.agent_version,
			interfaces = EXCLUDED.interfaces,
			heartbeat_interval_s = EXCLUDED.heartbeat_interval_s,
			metric_interval_s = EXCLUDED.metric_interval_s,
			online = true,
			last_seen = now()`,
		id, hostname, os, arch, agentVersion, interfaces,
		heartbeatIntS, metricIntS)
	return err
}

func (d *PostgresDevices) Contains(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := d.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1)`, id).Scan(&ok)
	return ok, err
}

func (d *PostgresDevices) Touch(ctx context.Context, id string) error {
	_, err := d.db.Exec(ctx, `UPDATE devices SET online=true, last_seen=now() WHERE id=$1`, id)
	return err
}

// List returns all devices (W2-1 device list / W1-7 indexing source).
func (d *PostgresDevices) List(ctx context.Context) ([]*Device, error) {
	rows, err := d.db.Query(ctx, `
		SELECT id, hostname, os, arch, agent_version, interfaces, tags,
		       online, first_seen, last_seen,
		       metric_interval_s, heartbeat_interval_s
		FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var o Device
		if err := rows.Scan(&o.ID, &o.Hostname, &o.OS, &o.Arch, &o.AgentVersion,
			&o.Interfaces, &o.Tags, &o.Online, &o.FirstSeen, &o.LastSeen,
			&o.MetricIntS, &o.HeartbeatIntS); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}
