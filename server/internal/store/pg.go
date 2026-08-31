package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// tsSkewWindow (L5) bounds how far an agent-reported timestamp_ms may stray
// from the server clock before it is clamped to ingest time. A skewed agent
// clock (or a spoofed token) must not write rows days/months away — that
// wrecks the rolling-baseline windows. The clamped value is used for BOTH
// timestamp_ms and ts so the row's PK and its hypertable partition line up.
const tsSkewWindow = 24 * time.Hour

func (s *PostgresMetricsSink) Write(deviceID string, batch *agentv1.MetricBatch) error {
	n := len(batch.GetSamples())
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// M9: one INSERT per batch (array-unnest), not one round-trip per
	// sample — a busy device used to be n round-trips every push.
	names := make([]string, 0, n)
	sources := make([]string, 0, n)
	values := make([]float64, 0, n)
	labels := make([]string, 0, n)
	tss := make([]int64, 0, n)
	now := time.Now()
	for _, m := range batch.GetSamples() {
		ts := m.GetTimestampMs()
		// L5: clamp a wildly skewed agent clock to ingest time.
		if tsMs := ts; tsMs < now.Add(-tsSkewWindow).UnixMilli() || tsMs > now.Add(tsSkewWindow).UnixMilli() {
			ts = now.UnixMilli()
		}
		names = append(names, m.GetName())
		sources = append(sources, m.GetSource())
		values = append(values, m.GetValue())
		labels = append(labels, labelsJSON(m.GetLabels()))
		tss = append(tss, ts)
	}

	// ts is derived from timestamp_ms so it always matches the row's PK.
	// DO NOTHING makes replay idempotent (a re-sent sample with the same
	// (device, name, source, timestamp_ms) is a no-op — IDEA.md §1).
	_, err := s.db.Exec(ctx, `
		INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		SELECT $1,
		       unnest($2::text[]),
		       unnest($3::text[]),
		       unnest($4::double precision[]),
		       unnest($5::text[])::jsonb,
		       unnest($6::bigint[]),
		       to_timestamp(unnest($6::bigint[]) / 1000.0)
		ON CONFLICT DO NOTHING`,
		deviceID, names, sources, values, labels, tss)
	if err != nil {
		return fmt.Errorf("insert %d metrics for %s: %w", n, deviceID, err)
	}
	return nil
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

// Get returns one device by id (W4-3 export); store.ErrNotFound when
// unknown.
func (d *PostgresDevices) Get(ctx context.Context, id string) (*Device, error) {
	var o Device
	err := d.db.QueryRow(ctx, `
		SELECT id, hostname, os, arch, agent_version, interfaces, tags,
		       online, first_seen, last_seen,
		       metric_interval_s, heartbeat_interval_s
		FROM devices WHERE id = $1`, id).Scan(
		&o.ID, &o.Hostname, &o.OS, &o.Arch, &o.AgentVersion,
		&o.Interfaces, &o.Tags, &o.Online, &o.FirstSeen, &o.LastSeen,
		&o.MetricIntS, &o.HeartbeatIntS)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &o, nil
}

func (d *PostgresDevices) Touch(ctx context.Context, id string) error {
	_, err := d.db.Exec(ctx, `UPDATE devices SET online=true, last_seen=now() WHERE id=$1`, id)
	return err
}

// SweepOffline (M4) flips every online device whose last_seen has gone stale
// (older than 3× its heartbeat interval, minimum 90s) to offline, returning
// the ids it flipped so the caller can re-sync their search documents.
func (d *PostgresDevices) SweepOffline(ctx context.Context) ([]string, error) {
	rows, err := d.db.Query(ctx, `
		UPDATE devices
		SET online = false
		WHERE online
		  AND last_seen < now() - make_interval(secs => GREATEST(heartbeat_interval_s * 3, 90))
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetTags replaces the device's tag list (B-2 operator tagging); the
// caller passes an already-normalized list. store.ErrNotFound when unknown.
func (d *PostgresDevices) SetTags(ctx context.Context, id string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	res, err := d.db.Exec(ctx, `UPDATE devices SET tags = $2 WHERE id = $1`, id, tags)
	if err != nil {
		return err
	}
	n := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
