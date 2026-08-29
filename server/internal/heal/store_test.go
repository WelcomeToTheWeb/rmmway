package heal

// Live-Postgres lifecycle tests for the W5-1 state machine (skipped when
// RMMWAY_TEST_PG_DSN is not reachable — same convention as the store
// package's live tests). Each test runs in a scratch database it tears
// down, so dev data is never touched and runs are repeatable.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- scratch database ------------------------------------------------------

func scratchPool(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping heal Postgres test")
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
	rnd := make([]byte, 4)
	_, _ = rand.Read(rnd)
	dbName := "rmmway_heal_test_" + time.Now().Format("20060102150405") + "_" + hex.EncodeToString(rnd)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		ctxC, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for attempt := 0; attempt < 5; attempt++ {
			if _, err := admin.Exec(ctxC, `DROP DATABASE IF EXISTS `+dbName); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	u.Path = "/" + dbName
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping scratch db: %v", err)
	}

	t.Chdir("../../..")
	if n, err := store.Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v (n=%d)", err, n)
	}
	return db, admin
}

// fakeAgent mirrors the remediation side of a live agent: the remediator
// records the dispatched script and (on demand) "executes" it by reporting
// a CommandResult and, optionally, writing the post-remediation sample.
type fakeAgent struct {
	mu       sync.Mutex
	seq      int
	dispatch map[string]fakeDispatch
	results  map[string]*agentv1.CommandResult
}

type fakeDispatch struct {
	DeviceID string
	Lang     string
	Script   string
	Command  string
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		dispatch: make(map[string]fakeDispatch),
		results:  make(map[string]*agentv1.CommandResult),
	}
}

func (f *fakeAgent) remediator(ctx context.Context, deviceID, lang, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	cmd := "cmd-" + strconv.Itoa(f.seq)
	f.dispatch[cmd] = fakeDispatch{DeviceID: deviceID, Lang: lang, Script: script, Command: cmd}
	return cmd, nil
}

func (f *fakeAgent) lookup(commandID string) (*agentv1.CommandResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[commandID]
	return r, ok
}

func (f *fakeAgent) report(commandID string, st agentv1.CommandResult_Status, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[commandID] = &agentv1.CommandResult{
		CommandId: commandID, Status: st, Error: detail,
		CompletedAtMs: time.Now().UnixMilli(),
	}
}

func (f *fakeAgent) dispatches() map[string]fakeDispatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]fakeDispatch, len(f.dispatch))
	for k, v := range f.dispatch {
		out[k] = v
	}
	return out
}

// captureNotifier counts + records escalations (the "notify" half of DoD).
type captureNotifier struct {
	mu      sync.Mutex
	reasons []string
}

func (n *captureNotifier) Escalate(r *Run, reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reasons = append(n.reasons, r.PlaybookKey+"/"+r.DeviceID+": "+reason)
}

func (n *captureNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.reasons)
}

// insertDevice + insertSample are the test's synthetic estate.
func insertDevice(t *testing.T, db *pgxpool.Pool, id, os string, online bool) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO devices (id, hostname, os, arch, online) VALUES ($1, $1, $2, 'amd64', $3)
		 ON CONFLICT (id) DO NOTHING`, id, os, online); err != nil {
		t.Fatalf("insert device %s: %v", id, err)
	}
}

func insertSample(t *testing.T, db *pgxpool.Pool, deviceID, metric, source string, v float64, at time.Time) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1, $2, $3, $4, '{}', $5, $6)
		ON CONFLICT DO NOTHING`, deviceID, metric, source, v, at.UnixMilli(), at.UTC()); err != nil {
		t.Fatalf("insert sample %s/%s/%s: %v", deviceID, metric, source, err)
	}
}

func runRows(t *testing.T, db *pgxpool.Pool) []Run {
	t.Helper()
	st := NewStore(db)
	runs, err := st.Runs(context.Background(), "", "", 500)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	return runs
}

func findRun(runs []Run, playbook, deviceID string) *Run {
	for i := range runs {
		if runs[i].PlaybookKey == playbook && runs[i].DeviceID == deviceID {
			return &runs[i]
		}
	}
	return nil
}

// ---- the lifecycle ----------------------------------------------------------

// TestHealLifecycleIsReplaySafe is the W5-1 definition of done, exercised
// against live Postgres: a failing condition is detected, remediated, and
// the confirm step RE-MEASURES; on confirm-fail the run escalates
// (ticket + notify). Along the way: double-passes never double-remediate,
// a refused remediation escalates, and the cooldown suppresses the retry
// storm while the condition persists.
func TestHealLifecycleIsReplaySafe(t *testing.T) {
	db, _ := scratchPool(t)
	ctx := context.Background()
	st := NewStore(db)
	agent := newFakeAgent()
	notify := &captureNotifier{}
	eng := New(st, agent.remediator, agent.lookup, notify).WithLogger(log.New(io.Discard, "", 0))

	// Starter library landed (3 playbooks, disk.full shape pinned).
	pbs, err := st.Playbooks(ctx, true)
	if err != nil {
		t.Fatalf("playbooks: %v", err)
	}
	if len(pbs) != 3 {
		t.Fatalf("starter library: expected 3 playbooks, got %d", len(pbs))
	}
	var disk *Playbook
	for i := range pbs {
		if pbs[i].Key == "disk.full" {
			disk = &pbs[i]
		}
	}
	if disk == nil || disk.Metric != "disk.used_percent" || disk.DetectThreshold != 90 || disk.DetectOp != ">" {
		t.Fatalf("disk.full playbook wrong: %+v", disk)
	}

	// Three devices, all with a hot volume (95% > 90).
	t0 := time.Now().UTC()
	for _, id := range []string{"dev-heal-1", "dev-heal-2", "dev-heal-3"} {
		insertDevice(t, db, id, "linux", true)
		insertSample(t, db, id, "disk.used_percent", "/dev/sda1", 95, t0.Add(-10*time.Second))
	}

	// Pass 1: detect -> verify-safe -> remediate. All three dispatch.
	pass := eng.RunOnce(ctx, t0)
	if len(pass.Errors) > 0 {
		t.Fatalf("pass 1 errors: %v", pass.Errors)
	}
	if pass.Detections != 3 || pass.Started != 3 || pass.ActiveRuns != 3 {
		t.Fatalf("pass 1: detections=%d started=%d active=%d, want 3/3/3", pass.Detections, pass.Started, pass.ActiveRuns)
	}
	runs := runRows(t, db)
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	for _, id := range []string{"dev-heal-1", "dev-heal-2", "dev-heal-3"} {
		r := findRun(runs, "disk.full", id)
		if r == nil || r.Status != "remediating" || r.CommandID == nil || *r.DetectValue != 95 {
			t.Fatalf("device %s: run = %+v, want remediating with command", id, r)
		}
	}
	// The dispatched scripts are the per-OS (sh) starter library, {{source}}
	// substituted with the detected volume.
	d := agent.dispatches()
	if len(d) != 3 {
		t.Fatalf("dispatches: got %d, want 3", len(d))
	}
	for _, dd := range d {
		if dd.Lang != "sh" || !strings.Contains(dd.Script, "rmmway self-heal: disk.full") {
			t.Fatalf("bad dispatch: %+v", dd)
		}
	}

	// Pass 2 (REPLAY, nothing changed): no results yet. Must NOT create a
	// second run or a second dispatch (partial unique index + conditional
	// transitions).
	pass = eng.RunOnce(ctx, t0.Add(30*time.Second))
	if pass.Started != 0 || pass.Skipped != 3 || len(agent.dispatches()) != 3 {
		t.Fatalf("replay pass: started=%d skipped=%d dispatches=%d, want 0/3/3",
			pass.Started, pass.Skipped, len(agent.dispatches()))
	}
	if runs := runRows(t, db); len(runs) != 3 {
		t.Fatalf("replay created runs: %d", len(runs))
	}

	// The agents report: dev-1 fixed the disk (next sample 62%), dev-2 ran
	// the script but the disk is still 95%, dev-3 REFUSED (capability).
	cmdOf := func(deviceID string) string {
		for _, dd := range agent.dispatches() {
			if dd.DeviceID == deviceID {
				return dd.Command
			}
		}
		t.Fatalf("no dispatch for %s", deviceID)
		return ""
	}
	agent.report(cmdOf("dev-heal-1"), agentv1.CommandResult_SUCCEEDED, "")
	insertSample(t, db, "dev-heal-1", "disk.used_percent", "/dev/sda1", 62, t0.Add(45*time.Second))
	agent.report(cmdOf("dev-heal-2"), agentv1.CommandResult_SUCCEEDED, "")
	insertSample(t, db, "dev-heal-2", "disk.used_percent", "/dev/sda1", 95, t0.Add(45*time.Second))
	agent.report(cmdOf("dev-heal-3"), agentv1.CommandResult_REFUSED, "capability token invalid")

	// Pass 3: dev-1/dev-2 -> confirming; dev-3 -> escalated (remediation
	// refused) + notify #1.
	pass = eng.RunOnce(ctx, t0.Add(60*time.Second))
	if pass.Escalated != 1 || notify.count() != 1 {
		t.Fatalf("pass 3: escalated=%d notify=%d, want 1/1", pass.Escalated, notify.count())
	}
	runs = runRows(t, db)
	if r := findRun(runs, "disk.full", "dev-heal-1"); r.Status != "confirming" || r.RemediatedAt == nil {
		t.Fatalf("dev-1: %+v, want confirming", r)
	}
	if r := findRun(runs, "disk.full", "dev-heal-2"); r.Status != "confirming" {
		t.Fatalf("dev-2: %+v, want confirming", r)
	}
	if r := findRun(runs, "disk.full", "dev-heal-3"); r.Status != "escalated" || r.EscalatedAt == nil ||
		!strings.Contains(r.Reason, "REFUSED") {
		t.Fatalf("dev-3: %+v, want escalated/REFUSED", r)
	}
	if !strings.Contains(notify.reasons[0], "dev-heal-3") {
		t.Fatalf("notify[0] = %q, want dev-heal-3", notify.reasons[0])
	}

	// Pass 4: the CONFIRM re-measurement. dev-1 healed (62 <= 90); dev-2
	// confirm-FAILS (95 still > 90) -> escalated + notify #2. dev-2's still
	//-hot detect this pass hits the ACTIVE run (no row yet — it is still
	// confirming when the detect stage runs); the cooldown-skip row lands
	// on pass 5, once the escalated run is terminal.
	pass = eng.RunOnce(ctx, t0.Add(75*time.Second))
	if pass.Confirmed != 1 || pass.Escalated != 1 || notify.count() != 2 {
		t.Fatalf("pass 4: confirmed=%d escalated=%d notify=%d, want 1/1/2",
			pass.Confirmed, pass.Escalated, notify.count())
	}
	runs = runRows(t, db)
	r1 := findRun(runs, "disk.full", "dev-heal-1")
	if r1.Status != "resolved" || r1.ConfirmValue == nil || *r1.ConfirmValue != 62 || r1.ConfirmedAt == nil {
		t.Fatalf("dev-1: %+v, want resolved with confirm_value=62", r1)
	}
	// dev-heal-2's LATEST run is the cooldown-skipped one; the escalated
	// run is still in the table (the ticket).
	var escalated, skipped *Run
	for i := range runs {
		if runs[i].DeviceID == "dev-heal-2" {
			switch runs[i].Status {
			case "escalated":
				escalated = &runs[i]
			case "skipped":
				skipped = &runs[i]
			}
		}
	}
	if escalated == nil || !strings.Contains(escalated.Reason, "confirm FAILED") {
		t.Fatalf("dev-2 escalated run missing/wrong: %+v", escalated)
	}

	// Pass 5 (REPLAY of the escalation): no double-notify, no new
	// remediation; dev-2's still-hot detect now lands as a cooldown-skip
	// row (the escalated run is terminal, so the active-run slot is free).
	before := notify.count()
	pass = eng.RunOnce(ctx, t0.Add(90*time.Second))
	if pass.Escalated != 0 || notify.count() != before || pass.Started != 0 {
		t.Fatalf("pass 5: escalated=%d notify delta=%d started=%d, want 0/0/0",
			pass.Escalated, notify.count()-before, pass.Started)
	}
	if pass.Skipped < 1 {
		t.Fatalf("pass 5: skipped=%d, want >=1 (dev-2 cooldown)", pass.Skipped)
	}
	runs = runRows(t, db)
	for i := range runs {
		if runs[i].DeviceID == "dev-heal-2" && runs[i].Status == "skipped" {
			skipped = &runs[i]
		}
	}
	if skipped == nil || !strings.Contains(skipped.Reason, "cooldown") {
		t.Fatalf("dev-2 cooldown skip missing: %+v", skipped)
	}

	// Audit trail: the healed run's event log is the full state machine.
	events, err := st.Events(ctx, r1.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var seq []string
	for _, e := range events {
		seq = append(seq, e.Status)
	}
	want := []string{"detected", "verifying", "remediating", "confirming", "resolved"}
	if len(seq) != len(want) {
		t.Fatalf("event log = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("event log = %v, want %v", seq, want)
		}
	}

	// Transition replay: re-applying an already-applied transition is a
	// no-op (false, no error) and appends no event.
	applied, err := st.Transition(ctx, r1.ID, "confirming", "resolved",
		map[string]any{"confirm_value": 62.0})
	if err != nil || applied {
		t.Fatalf("replayed transition: applied=%v err=%v, want false/nil", applied, err)
	}
	if events, _ := st.Events(ctx, r1.ID); len(events) != len(want) {
		t.Fatalf("replay appended an event: %d", len(events))
	}
}

// TestHealEscalatesOnRemediationTimeout covers the "no answer from the
// agent" path: dispatched, never answered past the remediation timeout ->
// escalated (ticket + notify), not silently retried.
func TestHealEscalatesOnRemediationTimeout(t *testing.T) {
	db, _ := scratchPool(t)
	ctx := context.Background()
	st := NewStore(db)
	agent := newFakeAgent()
	notify := &captureNotifier{}
	eng := New(st, agent.remediator, agent.lookup, notify).WithLogger(log.New(io.Discard, "", 0))

	t0 := time.Now().UTC()
	insertDevice(t, db, "dev-heal-4", "linux", true)
	insertSample(t, db, "dev-heal-4", "disk.used_percent", "/dev/sda1", 95, t0.Add(-10*time.Second))

	pass := eng.RunOnce(ctx, t0)
	if pass.Started != 1 {
		t.Fatalf("pass 1: started=%d errors=%v, want 1", pass.Started, pass.Errors)
	}
	// 301s later (remediate timeout = 300s) still no result: escalate.
	pass = eng.RunOnce(ctx, t0.Add(301*time.Second))
	if pass.Escalated != 1 || notify.count() != 1 {
		t.Fatalf("pass 2: escalated=%d notify=%d, want 1/1", pass.Escalated, notify.count())
	}
	runs := runRows(t, db)
	r := findRun(runs, "disk.full", "dev-heal-4")
	if r.Status != "escalated" || !strings.Contains(r.Reason, "no final result") {
		t.Fatalf("run = %+v, want escalated/no-final-result", r)
	}
}

// TestHealSkipsOfflineAndStale pins the verify-safe guards: a device that
// is NOT online at verify time is skipped (no dispatch), and a stale
// sample (older than fresh_within_seconds) is not detected at all.
func TestHealSkipsOfflineAndStale(t *testing.T) {
	db, _ := scratchPool(t)
	ctx := context.Background()
	st := NewStore(db)
	agent := newFakeAgent()
	eng := New(st, agent.remediator, agent.lookup, &captureNotifier{}).WithLogger(log.New(io.Discard, "", 0))

	t0 := time.Now().UTC()
	// Offline device with a fresh hot volume -> skipped, not dispatched.
	insertDevice(t, db, "dev-heal-off", "linux", false)
	insertSample(t, db, "dev-heal-off", "disk.used_percent", "/dev/sda1", 95, t0.Add(-10*time.Second))
	// Online device but the last sample is 2h old (stale) -> not detected.
	insertDevice(t, db, "dev-heal-stale", "linux", true)
	insertSample(t, db, "dev-heal-stale", "disk.used_percent", "/dev/sda1", 95, t0.Add(-2*time.Hour))

	pass := eng.RunOnce(ctx, t0)
	if len(pass.Errors) > 0 {
		t.Fatalf("pass errors: %v", pass.Errors)
	}
	if pass.Started != 0 || len(agent.dispatches()) != 0 {
		t.Fatalf("pass: started=%d dispatches=%d, want 0/0", pass.Started, len(agent.dispatches()))
	}
	runs := runRows(t, db)
	r := findRun(runs, "disk.full", "dev-heal-off")
	if r == nil || r.Status != "skipped" || !strings.Contains(r.Reason, "not online") {
		t.Fatalf("offline run = %+v, want skipped/not-online", r)
	}
	if r := findRun(runs, "disk.full", "dev-heal-stale"); r != nil {
		t.Fatalf("stale device must not be detected, got run %+v", r)
	}
}

// TestHealServiceDownAndWSUS pins the other two starter playbooks end to
// end: service.status == 0 -> restart script ({{source}} = service name)
// -> confirm == 1; wsus.update_state == 3 on windows -> reset script ->
// confirm <= 2. Also pins the os_filter: a linux device with a stuck WU
// metric is NOT acted on by wsus.stuck.
func TestHealServiceDownAndWSUS(t *testing.T) {
	db, _ := scratchPool(t)
	ctx := context.Background()
	st := NewStore(db)
	agent := newFakeAgent()
	notify := &captureNotifier{}
	eng := New(st, agent.remediator, agent.lookup, notify).WithLogger(log.New(io.Discard, "", 0))

	t0 := time.Now().UTC()
	insertDevice(t, db, "dev-heal-svc", "linux", true)
	insertSample(t, db, "dev-heal-svc", "service.status", "nginx", 0, t0.Add(-10*time.Second))
	insertDevice(t, db, "dev-heal-wsus", "windows", true)
	insertSample(t, db, "dev-heal-wsus", "wsus.update_state", "", 3, t0.Add(-10*time.Second))
	insertDevice(t, db, "dev-heal-wsus-linux", "linux", true)
	insertSample(t, db, "dev-heal-wsus-linux", "wsus.update_state", "", 3, t0.Add(-10*time.Second))

	pass := eng.RunOnce(ctx, t0)
	if len(pass.Errors) > 0 {
		t.Fatalf("pass 1 errors: %v", pass.Errors)
	}
	if pass.Started != 2 {
		t.Fatalf("pass 1: started=%d, want 2 (svc + wsus-windows; linux wsus filtered)", pass.Started)
	}
	d := agent.dispatches()
	if len(d) != 2 {
		t.Fatalf("dispatches: %d, want 2", len(d))
	}
	var sawSvcSH, sawWSUSPS bool
	for _, dd := range d {
		switch dd.DeviceID {
		case "dev-heal-svc":
			if dd.Lang != "sh" || !strings.Contains(dd.Script, `systemctl restart "nginx"`) {
				t.Fatalf("svc dispatch wrong: %+v", dd)
			}
			sawSvcSH = true
		case "dev-heal-wsus":
			if dd.Lang != "powershell" || !strings.Contains(dd.Script, "wsus.stuck") {
				t.Fatalf("wsus dispatch wrong: %+v", dd)
			}
			sawWSUSPS = true
		default:
			t.Fatalf("unexpected dispatch to %s", dd.DeviceID)
		}
	}
	if !sawSvcSH || !sawWSUSPS {
		t.Fatalf("dispatch coverage: svc=%v wsus=%v", sawSvcSH, sawWSUSPS)
	}

	// Both agents fix their condition; the re-measurement confirms.
	for cmd, dd := range d {
		agent.report(cmd, agentv1.CommandResult_SUCCEEDED, "")
		switch dd.DeviceID {
		case "dev-heal-svc":
			insertSample(t, db, "dev-heal-svc", "service.status", "nginx", 1, t0.Add(45*time.Second))
		case "dev-heal-wsus":
			insertSample(t, db, "dev-heal-wsus", "wsus.update_state", "", 0, t0.Add(45*time.Second))
		}
	}
	pass = eng.RunOnce(ctx, t0.Add(60*time.Second)) // -> confirming
	pass = eng.RunOnce(ctx, t0.Add(75*time.Second)) // -> resolved
	if pass.Confirmed != 2 || pass.Errors != nil {
		t.Fatalf("confirm pass: confirmed=%d errors=%v, want 2", pass.Confirmed, pass.Errors)
	}
	runs := runRows(t, db)
	if r := findRun(runs, "service.down", "dev-heal-svc"); r.Status != "resolved" || r.ConfirmValue == nil || *r.ConfirmValue != 1 {
		t.Fatalf("svc run = %+v, want resolved confirm=1", r)
	}
	if r := findRun(runs, "wsus.stuck", "dev-heal-wsus"); r.Status != "resolved" || r.ConfirmValue == nil || *r.ConfirmValue != 0 {
		t.Fatalf("wsus run = %+v, want resolved confirm=0", r)
	}
	if r := findRun(runs, "wsus.stuck", "dev-heal-wsus-linux"); r != nil {
		t.Fatalf("os_filter violated: linux wsus run %+v", r)
	}
	if notify.count() != 0 {
		t.Fatalf("no escalations expected, got %d", notify.count())
	}
}
