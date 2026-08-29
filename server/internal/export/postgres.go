package export

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresMetrics streams raw samples from the metrics hypertable (the
// W1-6 schema). The query is device-scoped and window-scoped; rows stream
// in ts order so the Parquet section is time-ordered for any downstream
// tool.
type PostgresMetrics struct {
	db *pgxpool.Pool
}

// NewPostgresMetrics builds a PostgresMetrics.
func NewPostgresMetrics(db *pgxpool.Pool) *PostgresMetrics {
	return &PostgresMetrics{db: db}
}

// Stream implements MetricsReader.
func (m *PostgresMetrics) Stream(ctx context.Context, deviceID string, since, until time.Time, fn func(Sample) error) (int64, error) {
	q := `SELECT ts, timestamp_ms, name, source, value, labels
	      FROM metrics WHERE device_id = $1`
	args := []any{deviceID}
	if !since.IsZero() {
		args = append(args, since)
		q += fmt.Sprintf(" AND ts >= $%d", len(args))
	}
	if !until.IsZero() {
		args = append(args, until)
		q += fmt.Sprintf(" AND ts < $%d", len(args))
	}
	q += " ORDER BY ts, name, source, timestamp_ms"
	rows, err := m.db.Query(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("query metrics: %w", err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var s Sample
		var labelsJSON []byte
		if err := rows.Scan(&s.TS, &s.TimestampMs, &s.Name, &s.Source, &s.Value, &labelsJSON); err != nil {
			return n, fmt.Errorf("scan metric: %w", err)
		}
		if len(labelsJSON) > 0 {
			if err := json.Unmarshal(labelsJSON, &s.Labels); err != nil {
				// A corrupt labels column must not kill the export: keep
				// the sample, drop the labels.
				s.Labels = nil
			}
		}
		if err := fn(s); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// PostgresRollups streams the 1-minute continuous aggregate (full history;
// note a CA can lag the newest raw samples by its materialization window).
type PostgresRollups struct {
	db *pgxpool.Pool
}

// NewPostgresRollups builds a PostgresRollups.
func NewPostgresRollups(db *pgxpool.Pool) *PostgresRollups {
	return &PostgresRollups{db: db}
}

// Stream implements RollupReader.
func (m *PostgresRollups) Stream(ctx context.Context, deviceID string, fn func(Rollup) error) (int64, error) {
	rows, err := m.db.Query(ctx, `
		SELECT bucket, name, source, avg_value, min_value, max_value, n
		FROM metrics_1m
		WHERE device_id = $1
		ORDER BY bucket, name, source, device_id`, deviceID)
	if err != nil {
		return 0, fmt.Errorf("query rollups: %w", err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var r Rollup
		if err := rows.Scan(&r.Bucket, &r.Name, &r.Source, &r.Avg, &r.Min, &r.Max, &r.N); err != nil {
			return n, fmt.Errorf("scan rollup: %w", err)
		}
		if err := fn(r); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// PostgresAlerts reads the device's complete alert history (all statuses),
// oldest first — the export is the client's full record, not just the open
// inbox.
type PostgresAlerts struct {
	db *pgxpool.Pool
}

// NewPostgresAlerts builds a PostgresAlerts.
func NewPostgresAlerts(db *pgxpool.Pool) *PostgresAlerts {
	return &PostgresAlerts{db: db}
}

// List implements AlertReader.
func (m *PostgresAlerts) List(ctx context.Context, deviceID string) ([]Alert, error) {
	rows, err := m.db.Query(ctx, `
		SELECT id, name, source, status, score, channel, value, expected,
		       events, first_at, last_at, resolved_at
		FROM alerts
		WHERE device_id = $1
		ORDER BY first_at, id`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Name, &a.Source, &a.Status, &a.Score,
			&a.Channel, &a.Value, &a.Expected, &a.Events,
			&a.FirstAt, &a.LastAt, &a.ResolvedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		out = append(out, a)
	}
	if out == nil {
		out = []Alert{}
	}
	return out, rows.Err()
}
