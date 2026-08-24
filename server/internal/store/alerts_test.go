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

// ---- pure reconciler logic (no database) ------------------------------------

func key(dev, name, src string) baseline.SeriesKey {
	return baseline.SeriesKey{DeviceID: dev, Name: name, Source: src}
}

func TestPlanReconcileDedupAndResolve(t *testing.T) {
	k := key("dev-1", "cpu.utilization_percent", "")
	anom := func(score float64) []baseline.Anomaly {
		return []baseline.Anomaly{{DeviceID: "dev-1", Name: "cpu.utilization_percent", Score: score, At: time.Now()}}
	}
	scored := map[baseline.SeriesKey]bool{k: true}

	// Pass 1: anomaly, no open alert -> create.
	q := map[baseline.SeriesKey]int{}
	open := map[baseline.SeriesKey]bool{}
	p := planReconcile(anom(5), scored, open, q, 1)
	if len(p.create) != 1 || len(p.bump) != 0 || len(p.resolve) != 0 {
		t.Fatalf("pass1 want create-only, got %+v", p)
	}

	// Pass 2: same series still anomalous, now open -> bump (NO create,
	// NO storm).
	open = map[baseline.SeriesKey]bool{k: true}
	p = planReconcile(anom(8), scored, open, q, 1)
	if len(p.create) != 0 || len(p.bump) != 1 || len(p.resolve) != 0 {
		t.Fatalf("pass2 want bump-only, got %+v", p)
	}

	// Pass 3: series scored but quiet -> resolves after the streak.
	p = planReconcile(nil, scored, open, q, 1)
	if len(p.resolve) != 1 || p.resolve[0] != k {
		t.Fatalf("pass3 want resolve %v, got %+v", k, p)
	}
	if len(q) != 0 {
		t.Fatalf("quiet streak should reset after resolve, got %+v", q)
	}

	// Pass 4: still quiet, no open alert -> nothing happens (no re-alert
	// from silence alone).
	open = map[baseline.SeriesKey]bool{}
	p = planReconcile(nil, scored, open, q, 1)
	if len(p.create)+len(p.bump)+len(p.resolve) != 0 {
		t.Fatalf("pass4 want no-op, got %+v", p)
	}
}

func TestPlanReconcileQuietStreak(t *testing.T) {
	k := key("dev-1", "m", "")
	scored := map[baseline.SeriesKey]bool{k: true}
	open := map[baseline.SeriesKey]bool{k: true}
	q := map[baseline.SeriesKey]int{}
	anom := []baseline.Anomaly{{DeviceID: "dev-1", Name: "m", Score: 6, At: time.Now()}}

	// quietNeeded=2: first clean pass must NOT resolve (streak 1 < 2).
	p := planReconcile(nil, scored, open, q, 2)
	if len(p.resolve) != 0 {
		t.Fatalf("1 clean pass with quietNeeded=2 must not resolve, got %+v", p)
	}
	// Second clean pass resolves.
	p = planReconcile(nil, scored, open, q, 2)
	if len(p.resolve) != 1 {
		t.Fatalf("2nd clean pass should resolve, got %+v", p)
	}

	// A hot pass resets the streak: quiet, hot, quiet -> still not resolved.
	q = map[baseline.SeriesKey]int{}
	planReconcile(nil, scored, open, q, 2) // streak 1
	if _, ok := planReconcile(anom, scored, open, q, 2).bump[k]; !ok {
		t.Fatalf("hot pass should bump")
	}
	p = planReconcile(nil, scored, open, q, 2) // streak back to 1
	if len(p.resolve) != 0 {
		t.Fatalf("hot pass must reset the streak; resolve not expected, got %+v", p)
	}
}

func TestPlanReconcileSilenceIsNotRecovery(t *testing.T) {
	// A device whose series the source no longer returns (offline) is not
	// in `scored` — its open alert must be left alone, never resolved.
	k := key("dev-gone", "m", "")
	open := map[baseline.SeriesKey]bool{k: true}
	q := map[baseline.SeriesKey]int{}
	p := planReconcile(nil, map[baseline.SeriesKey]bool{}, open, q, 1)
	if len(p.resolve) != 0 {
		t.Fatalf("silence must not resolve; got %+v", p)
	}
	if len(q) != 0 {
		t.Fatalf("no streak should be tracked for unscored series: %+v", q)
	}
}

func TestPlanReconcileTwoSeries(t *testing.T) {
	// Two independent anomalous series -> two alerts (one each).
	k1, k2 := key("a", "m1", ""), key("b", "m2", "")
	scored := map[baseline.SeriesKey]bool{k1: true, k2: true}
	anoms := []baseline.Anomaly{
		{DeviceID: "a", Name: "m1", Score: 5, At: time.Now()},
		{DeviceID: "b", Name: "m2", Score: 7, At: time.Now()},
	}
	p := planReconcile(anoms, scored, map[baseline.SeriesKey]bool{}, map[baseline.SeriesKey]int{}, 1)
	if len(p.create) != 2 {
		t.Fatalf("two independent series -> two creates, got %+v", p)
	}
}

// ---- live Postgres (alerts table + AlertStore) --------------------------------

// TestAlertsStoreLive verifies, against a scratch database:
//   - migration 0003 creates alerts (+ the dedup partial unique index),
//   - the reconciler creates ONE alert per anomalous series, bumps it on
//     repeated anomalies (no storm), and auto-resolves on a clean pass,
//   - a resolved series can re-alert (the partial unique slot frees up),
//   - manual ack/resolve transitions work and invalid ones are refused,
//   - List/Counts return the right rows.
//
// Requires RMMWAY_TEST_PG_DSN; skipped otherwise.
func TestAlertsStoreLive(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping alerts Postgres test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	dbName := "rmmway_test_" + time.Now().Format("20060102150405")
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
	} else if n != 4 {
		t.Fatalf("expected 4 migrations applied, got %d", n)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema='public' AND table_name='alerts'`).Scan(&count); err != nil {
		t.Fatalf("table check: %v", err)
	}
	if count != 1 {
		t.Fatalf("alerts table missing")
	}
	// Hostname join: seed a device.
	if _, err := db.Exec(ctx, `INSERT INTO devices (id, hostname, os, arch) VALUES ('dev-a1','host-a','linux','amd64')`); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	st := NewAlertStore(db, 1)
	at := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	anom := baseline.Anomaly{
		At: at, DeviceID: "dev-a1", Name: "cpu.utilization_percent", Score: 9.5, Value: 99,
		Seasonal: &baseline.CellScore{Z: 9.5, Median: 45, MAD: 1, EWMA: 46, Cells: 6},
	}
	scored := map[baseline.SeriesKey]bool{key("dev-a1", "cpu.utilization_percent", ""): true}

	// Pass 1: create.
	st.Reconcile([]baseline.Anomaly{anom}, scored)
	got, err := st.List(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("pass1: want 1 alert, got %d: %+v", len(got), got)
	}
	a := got[0]
	if a.Status != "open" || a.Events != 1 || a.Score != 9.5 || a.Channel != "seasonal" {
		t.Fatalf("pass1 alert wrong: %+v", a)
	}
	if a.Hostname != "host-a" {
		t.Fatalf("hostname join failed: %+v", a)
	}
	if a.Expected == nil || *a.Expected != 45 {
		t.Fatalf("expected baseline median not stored: %+v", a)
	}
	id := a.ID

	// Pass 2: same anomaly again -> bump, still exactly 1 row (no storm).
	anom2 := anom
	anom2.Score = 12
	anom2.Value = 104
	anom2.At = at.Add(time.Hour)
	st.Reconcile([]baseline.Anomaly{anom2}, scored)
	got, _ = st.List(ctx, "", "", 10)
	if len(got) != 1 {
		t.Fatalf("pass2: dedup broke — want 1 alert, got %d: %+v", len(got), got)
	}
	if got[0].Events != 2 || got[0].Score != 12 || !got[0].LastAt.Equal(at.Add(time.Hour)) ||
		!got[0].FirstAt.Equal(at) {
		t.Fatalf("pass2 bump wrong: %+v", got[0])
	}

	// Pass 3: series scored but clean -> auto-resolves.
	st.Reconcile(nil, scored)
	got, _ = st.List(ctx, "", "", 10)
	if len(got) != 0 {
		t.Fatalf("pass3: open inbox should be empty after auto-resolve, got %+v", got)
	}
	res, err := st.List(ctx, "resolved", "", 10)
	if err != nil || len(res) != 1 {
		t.Fatalf("pass3: want 1 resolved alert, got %+v (err %v)", res, err)
	}
	if res[0].ResolvedAt == nil {
		t.Fatalf("resolved_at not set: %+v", res[0])
	}

	// Pass 4: same series spikes again -> a NEW alert (re-fire after
	// resolve is a distinct incident, allowed by the partial unique index).
	st.Reconcile([]baseline.Anomaly{anom2}, scored)
	got, _ = st.List(ctx, "", "", 10)
	if len(got) != 1 || got[0].ID == id {
		t.Fatalf("pass4: want a fresh open alert, got %+v", got)
	}
	// The partial unique index must hold: at most one non-resolved row
	// per series, even if the table is force-inserted directly.
	if _, err := db.Exec(ctx, `INSERT INTO alerts (device_id, name, source, status, score, channel, value, events, first_at, last_at)
		VALUES ('dev-a1','cpu.utilization_percent','','open',1,'trend',1,1,$1,$1)`, at); err == nil {
		t.Fatalf("partial unique index failed to block a second open alert")
	}

	// Manual transitions on the fresh alert.
	freshID := got[0].ID
	acked, err := st.SetStatus(ctx, freshID, "acked")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.Status != "acked" || acked.AckedAt == nil {
		t.Fatalf("ack wrong: %+v", acked)
	}
	if _, err := st.SetStatus(ctx, freshID, "open"); err == nil {
		t.Fatalf("re-opening a resolved/acked alert must be refused")
	}
	resolved, err := st.SetStatus(ctx, freshID, "resolved")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Status != "resolved" || resolved.ResolvedAt == nil {
		t.Fatalf("resolve wrong: %+v", resolved)
	}
	if _, err := st.SetStatus(ctx, 99999999, "acked"); err == nil {
		t.Fatalf("unknown id must error")
	}

	// Counts + filters.
	counts, err := st.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["resolved"] != 2 || counts["open"] != 0 || counts["acked"] != 0 {
		t.Fatalf("counts wrong: %+v", counts)
	}
	byDev, err := st.List(ctx, "resolved", "dev-a1", 10)
	if err != nil || len(byDev) != 2 {
		t.Fatalf("device filter: got %d (err %v)", len(byDev), err)
	}
	other, err := st.List(ctx, "resolved", "dev-nobody", 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("device filter leak: got %+v", other)
	}
	if _, err := st.List(ctx, "bogus", "", 10); err == nil {
		t.Fatalf("unknown status must error")
	}
}
