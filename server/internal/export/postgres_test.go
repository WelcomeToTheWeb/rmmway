package export

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// TestPostgresExportLive verifies, against a scratch Timescale database:
//   - the Postgres readers stream the device's rows (raw + rollups + alerts),
//   - a real export builds a bundle whose manifest passes Verify,
//   - the Parquet sections re-open with the standard reader at the right
//     row counts, and the since/until window bounds the raw section.
//
// Requires RMMWAY_TEST_PG_DSN (a Timescale-capable Postgres); skipped
// otherwise — same convention as the other live-Postgres tests.
func TestPostgresExportLive(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping export Postgres test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	dbName := "rmmway_export_test_" + time.Now().Format("20060102150405")
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
	if n, err := store.Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	} else if n < 5 {
		t.Fatalf("expected >=5 migrations (W1-6..W5-1), got %d", n)
	}

	// ---- fixture: one device, 24 hourly samples over 2 days + 2 alerts ---
	dev := &store.Device{ID: "dev_pg", Hostname: "pgbox", OS: "linux", Arch: "amd64"}
	devices := store.NewPostgresDevices(db)
	if err := devices.Register(ctx, dev.ID, dev.Hostname, dev.OS, dev.Arch, "0.5.0", []string{"10.0.0.9"}, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 24; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if _, err := db.Exec(ctx, `
			INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1,'cpu.utilization_percent','',$2,'{"host":"pgbox"}',$3,$4)
			ON CONFLICT DO NOTHING`,
			dev.ID, float64(10+i), ts.UnixMilli(), ts); err != nil {
			t.Fatalf("insert sample: %v", err)
		}
	}
	// A second series to exercise multi-series streaming.
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if _, err := db.Exec(ctx, `
			INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
			VALUES ($1,'disk.used_percent','/dev/sda1',$2,'{}',$3,$4)
			ON CONFLICT DO NOTHING`,
			dev.ID, float64(60+i), ts.UnixMilli(), ts); err != nil {
			t.Fatalf("insert disk sample: %v", err)
		}
	}
	// Materialize the 1-minute continuous aggregate (the CA policy lags in
	// a test DB; refresh it explicitly so rollups exist).
	if _, err := db.Exec(ctx, `CALL refresh_continuous_aggregate('metrics_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh CA: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO alerts (device_id, name, source, status, score, channel, value, expected, events, first_at, last_at, resolved_at)
		VALUES
		 ($1,'cpu.utilization_percent','', 'open', 12.5, 'seasonal', 91, 20, 4, $2, $2, NULL),
		 ($1,'disk.used_percent','/dev/sda1','resolved', 7.0, 'trend', 88, 62, 2, $2, $2, $2)`,
		dev.ID, base); err != nil {
		t.Fatalf("insert alerts: %v", err)
	}

	// ---- full export -------------------------------------------------------
	s := New(Config{
		Devices: devices,
		Metrics: NewPostgresMetrics(db),
		Rollups: NewPostgresRollups(db),
		Alerts:  NewPostgresAlerts(db),
		Version: "rmmway-server/test",
	})
	var buf bytes.Buffer
	if _, err := s.Export(ctx, dev.ID, time.Time{}, time.Time{}, true, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	mf, err := Verify(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if mf.Device.ID != dev.ID {
		t.Fatalf("manifest device = %s", mf.Device.ID)
	}
	if got := manifestRows(*mf, MetricsName); got != 30 { // 24 + 6
		t.Fatalf("metrics rows = %d, want 30", got)
	}
	if got := manifestRows(*mf, AlertsName); got != 2 {
		t.Fatalf("alerts rows = %d, want 2", got)
	}
	if got := manifestRows(*mf, RollupsName); got == 0 {
		t.Fatalf("rollups rows = 0, want > 0 (CA was refreshed)")
	}
	// Standard reader re-opens the sections.
	rows, err := ReadMetrics(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if len(rows) != 30 {
		t.Fatalf("re-read metrics = %d, want 30", len(rows))
	}
	var disk int
	for _, r := range rows {
		if r.Name == "disk.used_percent" {
			disk++
		}
	}
	if disk != 6 {
		t.Fatalf("disk samples re-read = %d, want 6", disk)
	}

	// ---- windowed export ---------------------------------------------------
	since := base.Add(10 * time.Hour)
	until := base.Add(20 * time.Hour)
	var wbuf bytes.Buffer
	if _, err := s.Export(ctx, dev.ID, since, until, false, &wbuf); err != nil {
		t.Fatalf("windowed export: %v", err)
	}
	wmf, err := Verify(bytes.NewReader(wbuf.Bytes()), int64(wbuf.Len()))
	if err != nil {
		t.Fatalf("windowed Verify: %v", err)
	}
	if got := manifestRows(*wmf, MetricsName); got != 10 { // hours 10..19 cpu
		t.Fatalf("windowed metrics rows = %d, want 10 (disk series ends at hour 5)", got)
	}
	// rollups=0 must omit the section.
	if _, ok := manifestRowsOK(*wmf, RollupsName); ok {
		t.Fatalf("windowed bundle should omit rollups")
	}
}
