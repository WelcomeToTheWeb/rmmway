package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/baseline"
)

// ---- baseline data source (W2-3) -------------------------------------------

// PostgresBaselineSource feeds the dynamic baselining job with hourly means
// of the raw metrics hypertable (one row per (device, metric, source,
// hour-bucket)). It reads the hypertable directly rather than the metrics_1m
// continuous aggregate so history that predates the CA's policy window is
// still scored.
type PostgresBaselineSource struct {
	db *pgxpool.Pool
}

func NewPostgresBaselineSource(db *pgxpool.Pool) *PostgresBaselineSource {
	return &PostgresBaselineSource{db: db}
}

// Samples implements baseline.Source.
func (s *PostgresBaselineSource) Samples(ctx context.Context, since, until time.Time) ([]baseline.TimeSeries, error) {
	rows, err := s.db.Query(ctx, `
		SELECT device_id, name, source,
		       date_trunc('hour', ts) AS hr,
		       avg(value)              AS mean
		FROM metrics
		WHERE ts >= $1 AND ts <= $2
		GROUP BY device_id, name, source, hr
		ORDER BY device_id, name, source, hr`,
		since, until)
	if err != nil {
		return nil, fmt.Errorf("baseline sample query: %w", err)
	}
	defer rows.Close()

	bySeries := make(map[string]*baseline.TimeSeries)
	var order []string
	for rows.Next() {
		var (
			dev, name, source string
			hr                 time.Time
			mean               float64
		)
		if err := rows.Scan(&dev, &name, &source, &hr, &mean); err != nil {
			return nil, fmt.Errorf("baseline sample scan: %w", err)
		}
		key := dev + "\x00" + name + "\x00" + source
		ts, ok := bySeries[key]
		if !ok {
			ts = &baseline.TimeSeries{DeviceID: dev, Name: name, Source: source}
			bySeries[key] = ts
			order = append(order, key)
		}
		ts.Points = append(ts.Points, baseline.Point{At: hr, Mean: mean})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]baseline.TimeSeries, 0, len(order))
	for _, k := range order {
		out = append(out, *bySeries[k])
	}
	return out, nil
}

// ---- anomaly persistence ----------------------------------------------------

// PostgresAnomalySink records flagged observations in baseline_anomalies
// (W2-3). The upsert key (device_id, name, source, at_hour) makes repeated
// engine runs idempotent within an hour.
type PostgresAnomalySink struct {
	db *pgxpool.Pool
}

func NewPostgresAnomalySink(db *pgxpool.Pool) *PostgresAnomalySink {
	return &PostgresAnomalySink{db: db}
}

// Record upserts one anomaly (implements the baseline job's Handle func).
func (s *PostgresAnomalySink) Record(a baseline.Anomaly) {
	channel := "trend"
	if a.Seasonal != nil && (a.Trend == nil || a.Seasonal.Z >= a.Trend.Z) {
		channel = "seasonal"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		INSERT INTO baseline_anomalies
			(device_id, name, source, at_hour, at, value, score, channel,
			 seasonal_z, seasonal_med, seasonal_mad, seasonal_cells,
			 trend_z, trend_med, trend_mad, trend_cells)
		VALUES ($1, $2, $3, date_trunc('hour', $4::timestamptz), $4::timestamptz, $5, $6, $7,
		        $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (device_id, name, source, at_hour) DO UPDATE SET
			at = EXCLUDED.at,
			value = EXCLUDED.value,
			score = EXCLUDED.score,
			channel = EXCLUDED.channel,
			seasonal_z = EXCLUDED.seasonal_z,
			seasonal_med = EXCLUDED.seasonal_med,
			seasonal_mad = EXCLUDED.seasonal_mad,
			seasonal_cells = EXCLUDED.seasonal_cells,
			trend_z = EXCLUDED.trend_z,
			trend_med = EXCLUDED.trend_med,
			trend_mad = EXCLUDED.trend_mad,
			trend_cells = EXCLUDED.trend_cells,
			detected_at = now()`,
		a.DeviceID, a.Name, a.Source, a.At, a.Value, a.Score, channel,
		nullF64(a.Seasonal, "z"), nullF64(a.Seasonal, "med"), nullF64(a.Seasonal, "mad"), nullInt(a.Seasonal, "cells"),
		nullF64(a.Trend, "z"), nullF64(a.Trend, "med"), nullF64(a.Trend, "mad"), nullInt(a.Trend, "cells"))
	if err != nil {
		// Persistence failure must not kill the engine's run; the in-memory
		// view still serves the API. Logged for diagnosis.
		log.Printf("baseline: persist anomaly %s/%s: %v", a.DeviceID, a.Name, err)
	}
}

// StoredAnomaly is a row of baseline_anomalies for the API/e2e.
type StoredAnomaly struct {
	ID          int64      `json:"id"`
	DeviceID    string     `json:"device_id"`
	Name        string     `json:"name"`
	Source      string     `json:"source"`
	At          time.Time  `json:"at"`
	Value       float64    `json:"value"`
	Score       float64    `json:"score"`
	Channel     string     `json:"channel"`
	SeasonalZ   *float64   `json:"seasonal_z,omitempty"`
	TrendZ      *float64   `json:"trend_z,omitempty"`
	DetectedAt  time.Time  `json:"detected_at"`
}

// Recent returns the most recent stored anomalies (newest first).
func (s *PostgresAnomalySink) Recent(ctx context.Context, limit int) ([]StoredAnomaly, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, device_id, name, source, at, value, score, channel,
		       seasonal_z, trend_z, detected_at
		FROM baseline_anomalies
		ORDER BY at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredAnomaly
	for rows.Next() {
		var a StoredAnomaly
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.Name, &a.Source, &a.At,
			&a.Value, &a.Score, &a.Channel, &a.SeasonalZ, &a.TrendZ, &a.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- baseline job wiring ----------------------------------------------------

// Baseline couples the deterministic engine (internal/baseline.Job) to the
// Postgres anomaly sink so the server can run passes, expose state, and
// persist findings. Nil-safe when Postgres is down (in-memory mode): the
// engine still runs on whatever source is wired, and Record becomes a no-op
// logger.
type Baseline struct {
	Job  *baseline.Job
	Sink *PostgresAnomalySink
}

// NewBaseline builds a Baseline whose anomaly handle persists to sink.
func NewBaseline(src baseline.Source, sink *PostgresAnomalySink) *Baseline {
	j := &baseline.Job{Source: src}
	if sink != nil {
		j.Handle = sink.Record
	}
	return &Baseline{Job: j, Sink: sink}
}

// RunNow performs one scoring pass at the current time.
func (b *Baseline) RunNow(ctx context.Context) ([]baseline.Anomaly, error) {
	return b.Job.RunOnce(ctx, time.Now())
}

// Recent returns stored anomalies (nil when no sink is wired).
func (b *Baseline) Recent(ctx context.Context, limit int) ([]StoredAnomaly, error) {
	if b.Sink == nil {
		return nil, nil
	}
	return b.Sink.Recent(ctx, limit)
}

// Start launches the background job (W2-3 "Go background job").
func (b *Baseline) Start(ctx context.Context, interval time.Duration, errCh chan<- error) {
	b.Job.Run(ctx, interval, errCh)
}

// nullF64 / nullInt lift an optional CellScore field into a nullable arg.
func nullF64(c *baseline.CellScore, which string) *float64 {
	if c == nil {
		return nil
	}
	switch which {
	case "z":
		v := c.Z
		return &v
	case "med":
		v := c.Median
		return &v
	case "mad":
		v := c.MAD
		return &v
	}
	return nil
}

func nullInt(c *baseline.CellScore, which string) *int {
	if c == nil {
		return nil
	}
	switch which {
	case "cells":
		v := c.Cells
		return &v
	}
	return nil
}
