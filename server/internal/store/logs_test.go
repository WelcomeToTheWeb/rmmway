package store

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// TestLogEventsReplaySafeInTempDB is the W6-1 server-side proof against a
// real Timescale: log batches land in the log_events hypertable, a RE-sent
// batch (reconnect replay) is a no-op (dedup by entry id), and the
// per-device read is newest-first with a level filter.
//
// Requires RMMWAY_TEST_PG_DSN (or the default local dev stack DSN);
// skipped otherwise. Runs against a scratch database (dev data untouched).
func TestLogEventsReplaySafeInTempDB(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping Postgres log-events test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	dbName := "rmmway_logtest_" + time.Now().Format("20060102150405")
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

	if _, err := db.Exec(ctx,
		`INSERT INTO devices (id, hostname, os, arch) VALUES ($1,'h','linux','amd64')`,
		"dev-log1"); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	s := NewPostgresLogStore(db)
	batch := &agentv1.LogBatch{Entries: []*agentv1.LogEntry{
		{Id: "e1", TimestampMs: 1_000_000, Level: "INFO", Msg: "agent ready"},
		{Id: "e2", TimestampMs: 2_000_000, Level: "WARN", Msg: "uplink stream ended", Attrs: map[string]string{"err": "eof"}},
		{Id: "e3", TimestampMs: 3_000_000, Level: "ERROR", Msg: "collect failed"},
	}}
	if err := s.Write(ctx, "dev-log1", batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	// REPLAY: the same batch again (a reconnect re-send) must be a no-op.
	if err := s.Write(ctx, "dev-log1", batch); err != nil {
		t.Fatalf("replay write: %v", err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM log_events WHERE device_id='dev-log1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows after replay = %d, want 3 (replay must dedup by id)", n)
	}

	// Newest first, all three.
	evs, err := s.Recent(ctx, "dev-log1", 0, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(evs) != 3 || evs[0].Msg != "collect failed" || evs[2].Msg != "agent ready" {
		t.Fatalf("recent order/content = %+v", evs)
	}
	if evs[0].ID != "e3" || evs[0].Level != "error" {
		t.Fatalf("recent[0] = %+v (want id=e3 level=error)", evs[0])
	}
	// Attrs round-trip as JSONB.
	if evs[1].Attrs["err"] != "eof" {
		t.Fatalf("attrs = %v, want err=eof", evs[1].Attrs)
	}

	// Level filter.
	warns, err := s.Recent(ctx, "dev-log1", 0, "warn")
	if err != nil {
		t.Fatalf("recent warn: %v", err)
	}
	if len(warns) != 1 || warns[0].ID != "e2" {
		t.Fatalf("warn filter = %+v, want exactly e2", warns)
	}

	// Limit.
	one, err := s.Recent(ctx, "dev-log1", 1, "")
	if err != nil {
		t.Fatalf("recent limit: %v", err)
	}
	if len(one) != 1 || one[0].ID != "e3" {
		t.Fatalf("limit = %+v, want exactly the newest (e3)", one)
	}

	// A second device's events must not leak in.
	if _, err := db.Exec(ctx,
		`INSERT INTO devices (id, hostname, os, arch) VALUES ($1,'h2','linux','amd64')`,
		"dev-log2"); err != nil {
		t.Fatalf("seed device2: %v", err)
	}
	other, err := s.Recent(ctx, "dev-log2", 0, "")
	if err != nil {
		t.Fatalf("recent other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("cross-device leak: %v", other)
	}
}

// TestMemoryLogStore covers the no-Postgres fallback (same interfaces).
func TestMemoryLogStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryLogStore(0)
	batch := &agentv1.LogBatch{Entries: []*agentv1.LogEntry{
		{Id: "a", TimestampMs: 100, Level: "INFO", Msg: "one"},
		{Id: "b", TimestampMs: 300, Level: "WARN", Msg: "three"},
		{Id: "c", TimestampMs: 200, Level: "INFO", Msg: "two"},
	}}
	if err := s.Write(ctx, "dev-1", batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Replay is a no-op (dedup by id).
	if err := s.Write(ctx, "dev-1", batch); err != nil {
		t.Fatalf("replay: %v", err)
	}
	evs, err := s.Recent(ctx, "dev-1", 0, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(evs) != 3 || evs[0].ID != "b" || evs[1].ID != "c" || evs[2].ID != "a" {
		t.Fatalf("recent = %+v, want newest-first b,c,a", evs)
	}
	warns, err := s.Recent(ctx, "dev-1", 0, "warn")
	if err != nil {
		t.Fatalf("recent warn: %v", err)
	}
	if len(warns) != 1 || warns[0].ID != "b" {
		t.Fatalf("warn = %+v, want b", warns)
	}
	one, err := s.Recent(ctx, "dev-1", 1, "")
	if err != nil {
		t.Fatalf("recent limit: %v", err)
	}
	if len(one) != 1 || one[0].ID != "b" {
		t.Fatalf("limit = %+v, want b", one)
	}
	// Cap: overflow drops the OLDEST.
	small := NewMemoryLogStore(2)
	_ = small.Write(ctx, "dev-1", &agentv1.LogBatch{Entries: []*agentv1.LogEntry{
		{Id: "x", TimestampMs: 1, Level: "INFO", Msg: "x"},
		{Id: "y", TimestampMs: 2, Level: "INFO", Msg: "y"},
		{Id: "z", TimestampMs: 3, Level: "INFO", Msg: "z"},
	}})
	capped, _ := small.Recent(ctx, "dev-1", 0, "")
	if len(capped) != 2 || capped[0].ID != "z" || capped[1].ID != "y" {
		t.Fatalf("capped = %+v, want z,y (oldest x dropped)", capped)
	}
}
