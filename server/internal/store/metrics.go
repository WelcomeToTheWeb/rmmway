package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- metrics viewer (per-device series for the operator UI) ----------------

// MetricSeries is one (name, source) combination a device has reported:
// what the UI's metric picker lists.
type MetricSeries struct {
	Name   string  `json:"name"`
	Source string  `json:"source"`
	Last   float64 `json:"last"`
	Count  int64   `json:"count"`
}

// MetricPoint is one bucketed sample on a metric series.
type MetricPoint struct {
	T     time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// PostgresMetricsView serves the per-device metric viewer (the operator
// UI's device-detail panel): which series a device has reported, and the
// bucketed values of one series over a time range. It reads the raw
// metrics hypertable and aggregates server-side into fixed buckets, so a
// 30-day range stays a few hundred points no matter the agent's sample
// rate. (The metrics_1m continuous aggregate is the export path's
// rollup; this view buckets the raw table directly so history and
// future retention changes stay in one place.)
type PostgresMetricsView struct {
	db *pgxpool.Pool
}

func NewPostgresMetricsView(db *pgxpool.Pool) *PostgresMetricsView {
	return &PostgresMetricsView{db: db}
}

// Names lists the (name, source) series the device has reported since
// `since`, newest value first, ordered by name then source.
func (v *PostgresMetricsView) Names(ctx context.Context, deviceID string, since time.Time) ([]MetricSeries, error) {
	rows, err := v.db.Query(ctx, `
		SELECT name,
		       source,
		       (array_agg(value ORDER BY ts DESC))[1] AS last,
		       count(*)                                AS n
		FROM metrics
		WHERE device_id = $1 AND ts >= $2
		GROUP BY name, source
		ORDER BY name, source`, deviceID, since)
	if err != nil {
		return nil, fmt.Errorf("metrics names query: %w", err)
	}
	defer rows.Close()

	out := make([]MetricSeries, 0, 8)
	for rows.Next() {
		var (
			ms   MetricSeries
			last *float64
			n    int64
		)
		if err := rows.Scan(&ms.Name, &ms.Source, &last, &n); err != nil {
			return nil, fmt.Errorf("metrics names scan: %w", err)
		}
		if last != nil {
			ms.Last = *last
		}
		ms.Count = n
		out = append(out, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics names rows: %w", err)
	}
	return out, nil
}

// Series returns the bucketed samples of one (name, source) series for
// the device, from `since` to now: one point per bucket (the bucket's
// average value, stamped with the bucket start).
func (v *PostgresMetricsView) Series(ctx context.Context, deviceID, name, source string, since time.Time, bucket time.Duration) ([]MetricPoint, error) {
	if bucket <= 0 {
		return nil, fmt.Errorf("metrics series: bucket must be positive, got %s", bucket)
	}
	rows, err := v.db.Query(ctx, `
		SELECT time_bucket($4::interval, ts) AS b,
		       avg(value)                    AS mean
		FROM metrics
		WHERE device_id = $1 AND name = $2 AND source = $3 AND ts >= $5
		GROUP BY b
		ORDER BY b`, deviceID, name, source, bucket.String(), since)
	if err != nil {
		return nil, fmt.Errorf("metrics series query: %w", err)
	}
	defer rows.Close()

	out := make([]MetricPoint, 0, 256)
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.T, &p.Value); err != nil {
			return nil, fmt.Errorf("metrics series scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics series rows: %w", err)
	}
	return out, nil
}
