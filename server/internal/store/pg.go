package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	rows, err := d.db.Query(ctx, `
		SELECT `+deviceColumns+`
		FROM devices WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	out, err := scanDeviceRows(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out[0], nil
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
	return d.ListByClient(ctx, "")
}

// SetClient assigns a device to one client (gap #2); an empty clientID
// unassigns it (NULL). store.ErrNotFound when the device is unknown.
func (d *PostgresDevices) SetClient(ctx context.Context, id, clientID string) error {
	var target any
	if clientID != "" {
		target = clientID
	}
	res, err := d.db.Exec(ctx, `UPDATE devices SET client_id = $2 WHERE id = $1`, id, target)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByClient returns the devices assigned to one client (gap #2
// scoping). clientID "" returns every device; DefaultClientID also
// matches unassigned (NULL) devices.
func (d *PostgresDevices) ListByClient(ctx context.Context, clientID string) ([]*Device, error) {
	q := `SELECT ` + deviceColumns + ` FROM devices`
	args := make([]any, 0, 1)
	switch {
	case clientID == "":
	case clientID == DefaultClientID:
		args = append(args, clientID)
		q += ` WHERE client_id IS NULL OR client_id = $1`
	default:
		args = append(args, clientID)
		q += ` WHERE client_id = $1`
	}
	q += ` ORDER BY id`
	rows, err := d.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanDeviceRows(rows)
}

// deviceColumns is the canonical devices SELECT list (gap #2 added
// client_id — nullable, decoded by scanDeviceRows).
const deviceColumns = `id, hostname, os, arch, agent_version, interfaces, tags,
	       online, first_seen, last_seen,
	       metric_interval_s, heartbeat_interval_s, client_id`

// scanDeviceRows decodes a devices-table scan (shared by Get / List /
// ListByClient); client_id is NULL for unassigned devices.
func scanDeviceRows(rows pgx.Rows) ([]*Device, error) {
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var o Device
		var clientID *string
		if err := rows.Scan(&o.ID, &o.Hostname, &o.OS, &o.Arch, &o.AgentVersion,
			&o.Interfaces, &o.Tags, &o.Online, &o.FirstSeen, &o.LastSeen,
			&o.MetricIntS, &o.HeartbeatIntS, &clientID); err != nil {
			return nil, err
		}
		if clientID != nil {
			o.ClientID = *clientID
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// SaveDeviceHardware stores hardware inventory for a device (gap #4).
func (d *PostgresDevices) SaveDeviceHardware(ctx context.Context, deviceID string, hardware map[string]any) error {
	_, err := d.db.Exec(ctx, `
		INSERT INTO device_hardware (
			device_id, cpu_model, cpu_vendor, cpu_cores, cpu_logical,
			ram_total_bytes, os_name, os_version, os_arch, hostname, collected_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (device_id) DO UPDATE SET
			cpu_model = EXCLUDED.cpu_model,
			cpu_vendor = EXCLUDED.cpu_vendor,
			cpu_cores = EXCLUDED.cpu_cores,
			cpu_logical = EXCLUDED.cpu_logical,
			ram_total_bytes = EXCLUDED.ram_total_bytes,
			os_name = EXCLUDED.os_name,
			os_version = EXCLUDED.os_version,
			os_arch = EXCLUDED.os_arch,
			hostname = EXCLUDED.hostname,
			collected_at = NOW()
	`, deviceID,
		toNullString(hardware["cpu_model"]), toNullString(hardware["cpu_vendor"]),
		toNullInt32(hardware["cpu_cores"]), toNullInt32(hardware["cpu_logical"]),
		toNullInt64(hardware["ram_total_bytes"]), toNullString(hardware["os_name"]),
		toNullString(hardware["os_version"]), toNullString(hardware["os_arch"]),
		toNullString(hardware["hostname"]))
	return err
}

// GetDeviceHardware retrieves hardware inventory for a device (gap #4).
func (d *PostgresDevices) GetDeviceHardware(ctx context.Context, deviceID string) (map[string]any, error) {
	row := d.db.QueryRow(ctx, `
		SELECT cpu_model, cpu_vendor, cpu_cores, cpu_logical,
			ram_total_bytes, os_name, os_version, os_arch, hostname, collected_at
		FROM device_hardware WHERE device_id = $1
	`, deviceID)

	hw := make(map[string]any)
	var cpuModel, cpuVendor, osName, osVersion, osArch, hostname *string
	var cpuCores, cpuLogical *int32
	var ramTotal *int64
	var collectedAt time.Time

	err := row.Scan(&cpuModel, &cpuVendor, &cpuCores, &cpuLogical,
		&ramTotal, &osName, &osVersion, &osArch, &hostname, &collectedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No hardware row for this device (e.g. seeded device) — return empty map.
			return hw, nil
		}
		return nil, err
	}

	hw["cpu_model"] = cpuModel
	hw["cpu_vendor"] = cpuVendor
	hw["cpu_cores"] = cpuCores
	hw["cpu_logical"] = cpuLogical
	hw["ram_total_bytes"] = ramTotal
	hw["os_name"] = osName
	hw["os_version"] = osVersion
	hw["os_arch"] = osArch
	hw["hostname"] = hostname
	hw["collected_at"] = collectedAt

	return hw, nil
}

// SaveDeviceSoftware stores software inventory for a device (gap #4).
func (d *PostgresDevices) SaveDeviceSoftware(ctx context.Context, deviceID string, software []map[string]any) error {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Delete old entries
	if _, err := tx.Exec(ctx, "DELETE FROM device_software WHERE device_id = $1", deviceID); err != nil {
		return err
	}

	// Insert new entries
	for _, sw := range software {
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_software (device_id, name, version, vendor, install_date, arch, source, collected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`, deviceID,
			toNullString(sw["name"]), toNullString(sw["version"]),
			toNullString(sw["vendor"]), toNullString(sw["install_date"]),
			toNullString(sw["arch"]), toNullString(sw["source"])); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// GetDeviceSoftware retrieves software inventory for a device (gap #4).
func (d *PostgresDevices) GetDeviceSoftware(ctx context.Context, deviceID string) ([]map[string]any, error) {
	rows, err := d.db.Query(ctx, `
		SELECT name, version, vendor, install_date, arch, source, collected_at
		FROM device_software WHERE device_id = $1 ORDER BY name
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var software []map[string]any
	for rows.Next() {
		s := make(map[string]any)
		var name string
		var version, vendor, installDate, arch, source *string
		var collectedAt time.Time
		if err := rows.Scan(&name, &version, &vendor, &installDate, &arch, &source, &collectedAt); err != nil {
			return nil, err
		}
		s["name"] = name
		s["version"] = version
		s["vendor"] = vendor
		s["install_date"] = installDate
		s["arch"] = arch
		s["source"] = source
		software = append(software, s)
	}
	return software, rows.Err()
}

// Helper functions to convert map values to pointer types for pgx.
func toNullString(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func toNullInt32(v any) *int32 {
	if n, ok := v.(int32); ok {
		return &n
	}
	return nil
}

func toNullInt64(v any) *int64 {
	if n, ok := v.(int64); ok {
		return &n
	}
	return nil
}
