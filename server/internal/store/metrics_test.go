package store

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresMetricsView verifies the per-device metrics viewer queries
// (Names + bucketed Series) against a real TimescaleDB hypertable: the
// viewer must list exactly the series the device reported (with the most
// recent value) and must aggregate raw samples into one point per bucket
// (server-side bucketing, so long ranges stay a few hundred points).
//
// Requires RMMWAY_TEST_PG_DSN to be reachable; skipped otherwise (same
// pattern as the other Postgres tests in this package).
func TestPostgresMetricsView(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping Postgres metrics view test")
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
	if _, err := Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const dev = "dev-metrics-1"
	if _, err := db.Exec(ctx,
		`INSERT INTO devices (id, hostname, os, arch) VALUES ($1, 'host1', 'linux', 'amd64')`,
		dev,
	); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	// Align to the minute: time_bucket('1 minute', ...) stamps points at
	// wall-clock minute boundaries, so samples start exactly on the grid.
	now := time.Now().Truncate(time.Minute)
	insert := func(name, source string, ts time.Time, value float64) {
		t.Helper()
		_, err := db.Exec(ctx, `
			INSERT INTO metrics (device_id, name, source, value, timestamp_ms, ts)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			dev, name, source, value, ts.UnixMilli(), ts)
		if err != nil {
			t.Fatalf("insert %s/%s: %v", name, source, err)
		}
	}
	// cpu: 5 samples across three minutes (60s buckets: bucket0 = 1.0,
	// 2.0 -> 1.5; bucket1 = 3.0, 4.0 -> 3.5; bucket2 = 5.0 -> 5.0).
	insert("cpu.utilization_percent", "", now, 1.0)
	insert("cpu.utilization_percent", "", now.Add(30*time.Second), 2.0)
	insert("cpu.utilization_percent", "", now.Add(60*time.Second), 3.0)
	insert("cpu.utilization_percent", "", now.Add(90*time.Second), 4.0)
	insert("cpu.utilization_percent", "", now.Add(120*time.Second), 5.0)
	// A second series with a source (per-disk usage) and a third name.
	insert("disk.used_percent", "sda1", now.Add(time.Minute), 55.0)
	insert("memory.used_percent", "", now.Add(2*time.Minute), 70.0)

	v := NewPostgresMetricsView(db)

	// Names: exactly the three (name, source) series, ordered, with the
	// most recent value per series.
	names, err := v.Names(ctx, dev, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	want := []MetricSeries{
		{Name: "cpu.utilization_percent", Source: "", Last: 5.0, Count: 5},
		{Name: "disk.used_percent", Source: "sda1", Last: 55.0, Count: 1},
		{Name: "memory.used_percent", Source: "", Last: 70.0, Count: 1},
	}
	if len(names) != len(want) {
		t.Fatalf("names: got %d (%+v), want %d", len(names), names, len(want))
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("names[%d] = %+v, want %+v", i, names[i], w)
		}
	}

	// Series: 60s buckets over the 150s of cpu samples -> three points
	// (1.5, 3.5, 5.0), stamped at bucket starts.
	pts, err := v.Series(ctx, dev, "cpu.utilization_percent", "", now.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("series: got %d points (%+v), want 3", len(pts), pts)
	}
	if pts[0].Value != 1.5 || !pts[0].T.Equal(now) {
		t.Fatalf("series[0] = %+v, want ts=%s value=1.5", pts[0], now)
	}
	if pts[1].Value != 3.5 || !pts[1].T.Equal(now.Add(time.Minute)) {
		t.Fatalf("series[1] = %+v, want ts=%s value=3.5", pts[1], now.Add(time.Minute))
	}
	if pts[2].Value != 5.0 || !pts[2].T.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("series[2] = %+v, want ts=%s value=5.0", pts[2], now.Add(2*time.Minute))
	}

	// A source filter must exclude other sources (and a missing series
	// returns an empty slice, not an error).
	disk, err := v.Series(ctx, dev, "disk.used_percent", "sda1", now.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("disk series: %v", err)
	}
	if len(disk) != 1 || disk[0].Value != 55.0 {
		t.Fatalf("disk series = %+v, want one point at 55.0", disk)
	}
	none, err := v.Series(ctx, dev, "net.bytes_total", "eth0", now.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("empty series: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty series: got %+v, want none", none)
	}

	// Window: `since` outside the samples returns nothing.
	future, err := v.Series(ctx, dev, "cpu.utilization_percent", "", now.Add(10*time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("future window: %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("future window: got %+v, want none", future)
	}
}
