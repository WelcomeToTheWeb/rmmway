package store

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/baseline"
)

// TestBaselineSchemaSourceAndSink verifies, against a scratch database:
//   - migration 0002 creates baseline_anomalies (+ indexes),
//   - PostgresBaselineSource returns hourly means grouped per series,
//   - PostgresAnomalySink upserts (idempotent per series-hour).
//
// Requires RMMWAY_TEST_PG_DSN; skipped otherwise (same pattern as
// TestMigrateAppliesInitInTempDB).
func TestBaselineSchemaSourceAndSink(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping baseline Postgres test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	suffix := time.Now().Format("20060102150405")
	dbName := "rmmway_test_" + suffix
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName)

	u.Path = "/" + dbName
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping scratch db: %v", err)
	}

	t.Chdir("../../..")
	if n, err := Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	} else if n != 9 {
		t.Fatalf("expected 9 migrations applied, got %d", n)
	}

	var count int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public' AND table_name='baseline_anomalies'`).Scan(&count); err != nil {
		t.Fatalf("table check: %v", err)
	}
	if count != 1 {
		t.Fatalf("baseline_anomalies table missing")
	}
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name='metrics'`).Scan(&count); err != nil {
		t.Fatalf("metrics check: %v", err)
	}
	if count != 1 {
		t.Fatalf("metrics table missing — migration 0002 must not drop 0001 objects")
	}

	// Seed a device + hourly metric rows (two series, two samples each in
	// hour 14, two samples in hour 15 for cpu).
	if _, err := db.Exec(ctx,
		`INSERT INTO devices (id, hostname, os, arch) VALUES ('dev-b1','h','linux','amd64')`,
	); err != nil {
		t.Fatalf("device insert: %v", err)
	}
	hour := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	ins := `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1,$2,$3,$4,'{}',$5::bigint, to_timestamp($5::bigint/1000.0)) ON CONFLICT DO NOTHING`
	// Two series, two samples each in hour 14.
	seeds := []struct {
		name, source string
		vals         []float64
	}{
		{"cpu.utilization_percent", "", []float64{50, 60}}, // hour 14: mean 55
		{"memory.used_percent", "", []float64{40, 44}},     // hour 14: mean 42
	}
	for _, s := range seeds {
		for i, v := range s.vals {
			ts := hour.Add(time.Duration(i) * time.Minute).UnixMilli()
			if _, err := db.Exec(ctx, ins, "dev-b1", s.name, s.source, v, ts); err != nil {
				t.Fatalf("metric insert: %v", err)
			}
		}
	}
	// cpu hour 15: two more samples in the next hour bucket to check
	// bucketing across hours (mean 75).
	ts15 := hour.Add(time.Hour).UnixMilli()
	for i, v := range []float64{70, 80} {
		ts := ts15 + int64(i)*int64(time.Minute/time.Millisecond)
		if _, err := db.Exec(ctx, ins, "dev-b1", "cpu.utilization_percent", "", v, ts); err != nil {
			t.Fatalf("metric insert h15: %v", err)
		}
	}

	src := NewPostgresBaselineSource(db)
	samples, err := src.Samples(ctx, hour.Add(-time.Hour), hour.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("source samples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(samples), samples)
	}
	var cpu *baseline.TimeSeries
	for i := range samples {
		if samples[i].Name == "cpu.utilization_percent" {
			cpu = &samples[i]
		}
	}
	if cpu == nil {
		t.Fatalf("cpu series missing: %+v", samples)
	}
	if len(cpu.Points) != 2 {
		t.Fatalf("cpu expected 2 hourly means, got %d: %+v", len(cpu.Points), cpu.Points)
	}
	if !cpu.Points[0].At.Equal(hour) || !cpu.Points[1].At.Equal(hour.Add(time.Hour)) {
		t.Fatalf("cpu bucket times wrong: %+v", cpu.Points)
	}
	if cpu.Points[0].Mean != 55 || cpu.Points[1].Mean != 75 {
		t.Fatalf("cpu means wrong: %+v", cpu.Points)
	}

	// Sink: upsert twice for the same series-hour -> exactly 1 row.
	sink := NewPostgresAnomalySink(db)
	a := baseline.Anomaly{
		At: hour.Add(30 * time.Minute), DeviceID: "dev-b1", Name: "cpu.utilization_percent",
		Value: 99, Score: 6.2,
		Seasonal: &baseline.CellScore{Z: 6.2, Median: 55, MAD: 1, EWMA: 56, Cells: 6},
	}
	sink.Record(a)
	sink.Record(a) // idempotent re-run
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM baseline_anomalies WHERE device_id='dev-b1'`).Scan(&count); err != nil {
		t.Fatalf("anomaly count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 stored anomaly after 2 Records, got %d", count)
	}
	recent, err := sink.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 1 || recent[0].Channel != "seasonal" || recent[0].Score != 6.2 {
		t.Fatalf("recent wrong: %+v", recent)
	}
	if recent[0].SeasonalZ == nil || *recent[0].SeasonalZ != 6.2 {
		t.Fatalf("seasonal_z not stored: %+v", recent[0])
	}
	if recent[0].TrendZ != nil {
		t.Fatalf("trend_z should be NULL: %+v", recent[0])
	}
	// Update path: a re-Record with a different value updates in place.
	a.Value = 99.5
	a.Score = 7.0
	sink.Record(a)
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM baseline_anomalies WHERE device_id='dev-b1'`).Scan(&count); err != nil {
		t.Fatalf("anomaly count 2: %v", err)
	}
	if count != 1 {
		t.Fatalf("update-in-place expected 1 row, got %d", count)
	}
	recent, _ = sink.Recent(ctx, 10)
	if recent[0].Value != 99.5 || recent[0].Score != 7.0 {
		t.Fatalf("update did not apply: %+v", recent[0])
	}
}
