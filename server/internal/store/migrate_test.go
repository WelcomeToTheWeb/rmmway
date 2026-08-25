package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrateAppliesInitInTempDB verifies that 0001_init.sql creates the
// W1-6 objects: devices table, metrics hypertable, the metrics_1m
// continuous aggregate, and that a second Migrate() call is a no-op
// (idempotent restart) — plus replay-safe metric inserts (W1-6 DoD:
// "schema migrates cleanly; metrics land in the hypertable and a
// continuous aggregate rolls up").
//
// Runs against a scratch database so dev data is never touched. Requires
// RMMWAY_TEST_PG_DSN (or the default local dev stack DSN) to be
// reachable; skipped otherwise.
func TestMigrateAppliesInitInTempDB(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping Postgres migration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// Point admin connections at the server DB (any) to create the scratch db.
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

	// Migrations dir is cwd-relative ("migrations" from the server module
	// root) — same resolution as cmd/server and `make migrate`. The test
	// runs from the package dir (server/internal/store), so the repo root
	// is three levels up.
	t.Chdir("../../..")

	n, err := Migrate(ctx, db, "server/migrations")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 6 {
		t.Fatalf("expected 6 migrations applied, got %d", n)
	}

	mustScan := func(query string, dst ...any) {
		t.Helper()
		if err := db.QueryRow(ctx, query).Scan(dst...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	var count int
	mustScan(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('devices','metrics')`, &count)
	if count != 2 {
		t.Fatalf("expected devices+metrics tables, got %d", count)
	}
	// W3-1: the org PKI tables (org_ca single-row root + per-device leaf
	// record) must be created by 0004_org_ca.sql.
	mustScan(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('org_ca','device_certs')`, &count)
	if count != 2 {
		t.Fatalf("expected org_ca+device_certs tables, got %d", count)
	}
	mustScan(`SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name='metrics'`, &count)
	if count != 1 {
		t.Fatalf("expected metrics hypertable, got %d", count)
	}
	mustScan(`SELECT count(*) FROM timescaledb_information.continuous_aggregates WHERE view_name='metrics_1m'`, &count)
	if count != 1 {
		t.Fatalf("expected metrics_1m continuous aggregate, got %d", count)
	}

	// W5-1: the self-healing tables (playbooks + heal_runs + heal_events)
	// must be created by 0005_selfheal.sql, with the starter library seeded.
	mustScan(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('playbooks','heal_runs','heal_events')`, &count)
	if count != 3 {
		t.Fatalf("expected playbooks+heal_runs+heal_events tables, got %d", count)
	}
	mustScan(`SELECT count(*) FROM playbooks WHERE key IN ('disk.full','service.down','wsus.stuck')`, &count)
	if count != 3 {
		t.Fatalf("expected 3 starter playbooks seeded, got %d", count)
	}
	// Idempotency: a second run (server restart) applies nothing.
	if n, err := Migrate(ctx, db, "server/migrations"); err != nil || n != 0 {
		t.Fatalf("second migrate should be a no-op, got n=%d err=%v", n, err)
	}

	// Replay safety: the same (device, name, source, timestamp_ms) sample
	// inserted twice must yield exactly one row (ON CONFLICT DO NOTHING).
	if _, err := db.Exec(ctx, `INSERT INTO devices (id, hostname, os, arch) VALUES ($1,'h','linux','amd64')`, "dev-t1"); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	const ins = `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ('dev-t1','cpu.utilization_percent','',50.0,'{}',1755858941000, to_timestamp(1755858941000/1000.0))
		ON CONFLICT DO NOTHING`
	if _, err := db.Exec(ctx, ins); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	if _, err := db.Exec(ctx, ins); err != nil { // outbox replay
		t.Fatalf("replay insert: %v", err)
	}
	mustScan(`SELECT count(*) FROM metrics WHERE device_id='dev-t1'`, &count)
	if count != 1 {
		t.Fatalf("idempotent replay expected 1 row, got %d", count)
	}
}

// TestMigrationsDirResolves pins the directory the server and
// `make migrate` read from: from the repo root, server/migrations must
// exist and contain 0001_init.sql.
func TestMigrationsDirResolves(t *testing.T) {
	t.Chdir("../../..")
	st, err := os.Stat(filepath.Join("server", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("server/migrations/0001_init.sql: %v", err)
	}
	if st.IsDir() {
		t.Fatalf("0001_init.sql is a directory?")
	}
}
