package flow

// Live-Postgres lifecycle tests for the W5-2 engine (skipped when
// RMMWAY_TEST_PG_DSN is not reachable — same convention as the store/heal
// packages). Each test runs in a scratch database it tears down.
//
// These exercise the ENGINE with the in-process memBus (synchronous
// delivery), so the full DoD chain `disk>90 -> free -> if>90 -> notify` is
// driven end-to-end: synthetic trigger -> run -> script dispatch -> command
// result hop -> check re-measurement -> branch -> notify. The NATS transport
// itself is covered by cmd/e2e/flow (real broker).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- scratch database ------------------------------------------------------

func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping flow Postgres test")
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
	dbName := "rmmway_flow_test_" + time.Now().Format("20060102150405") + "_" + hex.EncodeToString(rnd)
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
	return db
}

func insertDevice(t *testing.T, db *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO devices (id, hostname, os, arch) VALUES ($1, $1, 'linux', 'amd64') ON CONFLICT DO NOTHING`,
		id); err != nil {
		t.Fatalf("insert device: %v", err)
	}
}

// insertSample appends one metric sample. ts is the measurement time.
func insertSample(t *testing.T, db *pgxpool.Pool, deviceID, metric, source string, v float64, at time.Time) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1, $2, $3, $4, '{}', $5, $6) ON CONFLICT DO NOTHING`,
		deviceID, metric, source, v, at.UnixMilli(), at.UTC()); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

// captureNotifier records notify-node + run-failure notifications.
type captureNotifier struct {
	mu    chan string
	calls []string
}

func newCaptureNotifier() *captureNotifier { return &captureNotifier{mu: make(chan string, 16)} }

func (n *captureNotifier) Notify(ctx context.Context, run *Run, nodeID, reason string) {
	n.mu <- run.FlowName + "/" + nodeID + ": " + reason
}

// drain returns all captured notifications so far.
func (n *captureNotifier) drain() []string {
	out := []string{}
	for {
		select {
		case c := <-n.mu:
			out = append(out, c)
		default:
			return out
		}
	}
}

// fakeRemediator records dispatched scripts and returns a command id.
type fakeRemediator struct {
	dispatched []string // "device:lang"
	seq        int
}

func (f *fakeRemediator) rem(ctx context.Context, deviceID, lang, script string) (string, error) {
	f.seq++
	f.dispatched = append(f.dispatched, deviceID+":"+lang)
	return "cmd-" + strconv.Itoa(f.seq), nil
}

// ---- the DoD chain, end to end (memBus) ------------------------------------

// TestDoDChain is the W5-2 definition of done, engine-level: compose
// `disk>90% -> free -> if>90% -> notify` and verify it fires correctly on a
// synthetic trigger — both the healed branch (no notify) and the
// still-full branch (exactly one notify), plus replay safety.
func TestDoDChain(t *testing.T) {
	db := scratchPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st := NewStore(db)
	bus := NewMemBus()
	notif := newCaptureNotifier()
	rem := &fakeRemediator{}
	results := map[string]*agentv1.CommandResult{}
	lookup := func(id string) (*agentv1.CommandResult, bool) {
		r, ok := results[id]
		return r, ok
	}
	eng := New(st, bus, rem.rem, lookup, notif, -1, -1).WithLogger(log.New(logWriter{}, "flow: ", 0))
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	f, err := st.CreateFlow(ctx, "disk-full", "DoD chain", doDGraph(), 0, true)
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}

	// ---- branch 1: agent frees space (62 <= 90) -> chain ends, no notify --
	insertDevice(t, db, "dev-heal")
	now := time.Now().UTC()
	trig := &Event{Type: SubjectTrigger, FlowID: f.ID, DeviceID: "dev-heal", Value: f64(95), At: now}
	if err := bus.Publish(ctx, SubjectTrigger, trig); err != nil {
		t.Fatalf("publish trigger: %v", err)
	}
	// Synchronous memBus: the run is already at the script node, dispatched.
	run1, err := st.RunByCommand(ctx, "cmd-1")
	if err != nil || run1 == nil {
		t.Fatalf("run by command: %v %v", run1, err)
	}
	if run1.Status != "running" || run1.CurrentNode != "free" || run1.CommandID == nil || *run1.CommandID != "cmd-1" {
		t.Fatalf("after trigger, run = %+v, want running at free with cmd-1", run1)
	}

	// Replay the SAME trigger: the one-active-run index makes it a no-op.
	if err := bus.Publish(ctx, SubjectTrigger, trig); err != nil {
		t.Fatalf("replay trigger: %v", err)
	}
	runs, _ := st.Runs(ctx, "", "dev-heal", &f.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("replay created %d runs, want 1", len(runs))
	}

	// The agent "executes": it frees space (reports a fresh 62 sample) and
	// answers SUCCEEDED. The command.result hop is what advances the chain.
	insertSample(t, db, "dev-heal", "disk.used_percent", "", 62, now.Add(2*time.Second))
	results["cmd-1"] = &agentv1.CommandResult{CommandId: "cmd-1", Status: agentv1.CommandResult_SUCCEEDED, CompletedAtMs: now.Add(3 * time.Second).UnixMilli()}
	if err := bus.Publish(ctx, SubjectCommand, &Event{Type: SubjectCommand, CommandID: "cmd-1", Status: "SUCCEEDED", At: now.Add(3 * time.Second)}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	fresh, _ := st.Run(ctx, run1.ID)
	if fresh.Status != "succeeded" {
		t.Fatalf("healed branch: run = %+v, want succeeded", fresh)
	}
	if got := notif.drain(); len(got) != 0 {
		t.Fatalf("healed branch notified: %v, want none (62 <= 90 ends the chain)", got)
	}

	// ---- branch 2: agent does NOT free space (95 > 90) -> notify fires ----
	insertDevice(t, db, "dev-full")
	now2 := time.Now().UTC()
	if err := bus.Publish(ctx, SubjectTrigger, &Event{Type: SubjectTrigger, FlowID: f.ID, DeviceID: "dev-full", Value: f64(95), At: now2}); err != nil {
		t.Fatalf("publish trigger 2: %v", err)
	}
	run2, err := st.RunByCommand(ctx, "cmd-2")
	if err != nil || run2 == nil {
		t.Fatalf("run2 by command: %v %v", run2, err)
	}
	// Agent runs but the volume stays at 95.
	insertSample(t, db, "dev-full", "disk.used_percent", "", 95, now2.Add(2*time.Second))
	results["cmd-2"] = &agentv1.CommandResult{CommandId: "cmd-2", Status: agentv1.CommandResult_SUCCEEDED, CompletedAtMs: now2.Add(3 * time.Second).UnixMilli()}
	if err := bus.Publish(ctx, SubjectCommand, &Event{Type: SubjectCommand, CommandID: "cmd-2", Status: "SUCCEEDED", At: now2.Add(3 * time.Second)}); err != nil {
		t.Fatalf("publish result 2: %v", err)
	}
	fresh2, _ := st.Run(ctx, run2.ID)
	if fresh2.Status != "succeeded" {
		t.Fatalf("still-full branch: run = %+v, want succeeded (notify fired, chain complete)", fresh2)
	}
	got := notif.drain()
	if len(got) != 1 {
		t.Fatalf("still-full branch notified %d times: %v, want exactly 1", len(got), got)
	}
	if !containsSubstr(got[0], "notify") {
		t.Fatalf("notify came from wrong node: %q", got[0])
	}

	// Replay the command.result hop: no double notify.
	if err := bus.Publish(ctx, SubjectCommand, &Event{Type: SubjectCommand, CommandID: "cmd-2", Status: "SUCCEEDED", At: now2.Add(4 * time.Second)}); err != nil {
		t.Fatalf("replay result: %v", err)
	}
	if got := notif.drain(); len(got) != 0 {
		t.Fatalf("replayed result double-notified: %v", got)
	}

	// ---- the audit trail ---------------------------------------------------
	evs, err := st.Events(ctx, run2.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	seq := nodeSeq(evs)
	// trigger entered, free entered+dispatched, still entered+branched,
	// notify entered (branched) + ok (terminal). The exact status column is
	// asserted loosely: the node ORDER is what matters for the audit.
	if !seqContainsInOrder(seq, []string{"t", "free", "still", "notify"}) {
		t.Fatalf("run2 node order = %v, want t->free->still->notify", seq)
	}
	for _, e := range evs {
		t.Logf("run2 hop: node=%-6s status=%-9s %s", e.Node, e.Status, e.Reason)
	}
}

// TestCheckWaitsForFreshSample verifies a check node with no fresh sample yet
// records a single "waiting" hop and does not advance; once a sample lands,
// a sweep re-cover advances it.
func TestCheckWaitsForFreshSample(t *testing.T) {
	db := scratchPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := NewStore(db)
	bus := NewMemBus()
	notif := newCaptureNotifier()
	rem := &fakeRemediator{}
	results := map[string]*agentv1.CommandResult{}
	eng := New(st, bus, rem.rem, func(id string) (*agentv1.CommandResult, bool) {
		r, ok := results[id]
		return r, ok
	}, notif, -1, -1)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	f, err := st.CreateFlow(ctx, "wait", "check-wait", doDGraph(), 0, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	insertDevice(t, db, "dev-wait")
	now := time.Now().UTC()
	_ = bus.Publish(ctx, SubjectTrigger, &Event{Type: SubjectTrigger, FlowID: f.ID, DeviceID: "dev-wait", Value: f64(95), At: now})
	run, _ := st.RunByCommand(ctx, "cmd-1")
	if run == nil {
		t.Fatalf("no run")
	}
	// Script succeeds but NO fresh sample yet: the check must wait.
	results["cmd-1"] = &agentv1.CommandResult{CommandId: "cmd-1", Status: agentv1.CommandResult_SUCCEEDED, CompletedAtMs: now.Add(time.Second).UnixMilli()}
	_ = bus.Publish(ctx, SubjectCommand, &Event{Type: SubjectCommand, CommandID: "cmd-1", Status: "SUCCEEDED", At: now.Add(time.Second)})
	mid, _ := st.Run(ctx, run.ID)
	if mid.Status != "running" || mid.CurrentNode != "still" {
		t.Fatalf("after success without a sample, run = %+v, want running at check 'still'", mid)
	}
	// One waiting hop recorded (deduped), no advance.
	evs, _ := st.Events(ctx, run.ID)
	waits := countStatus(evs, "waiting")
	if waits != 1 {
		t.Fatalf("waiting hops = %d, want exactly 1 (deduped)", waits)
	}
	// A sample lands; the sweep re-cover advances the chain to completion.
	insertSample(t, db, "dev-wait", "disk.used_percent", "", 62, now.Add(5*time.Second))
	eng.Sweep(ctx, now.Add(6*time.Second))
	fresh, _ := st.Run(ctx, run.ID)
	if fresh.Status != "succeeded" {
		t.Fatalf("after sample + sweep, run = %+v, want succeeded", fresh)
	}
	if got := notif.drain(); len(got) != 0 {
		t.Fatalf("unexpected notify: %v", got)
	}
}

// TestScriptFailureFailsRunAndNotifies verifies a non-success script result
// fails the run and fires the failure notification exactly once.
func TestScriptFailureFailsRunAndNotifies(t *testing.T) {
	db := scratchPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := NewStore(db)
	bus := NewMemBus()
	notif := newCaptureNotifier()
	rem := &fakeRemediator{}
	results := map[string]*agentv1.CommandResult{}
	eng := New(st, bus, rem.rem, func(id string) (*agentv1.CommandResult, bool) {
		r, ok := results[id]
		return r, ok
	}, notif, -1, -1)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	f, _ := st.CreateFlow(ctx, "fail", "fail", doDGraph(), 0, true)
	insertDevice(t, db, "dev-fail")
	now := time.Now().UTC()
	_ = bus.Publish(ctx, SubjectTrigger, &Event{Type: SubjectTrigger, FlowID: f.ID, DeviceID: "dev-fail", Value: f64(95), At: now})
	run, _ := st.RunByCommand(ctx, "cmd-1")
	if run == nil {
		t.Fatalf("no run")
	}
	// Agent REFUSES the command (capability check failed).
	_ = bus.Publish(ctx, SubjectCommand, &Event{Type: SubjectCommand, CommandID: "cmd-1", Status: "REFUSED", Message: "token mismatch", At: now.Add(time.Second)})
	fresh, _ := st.Run(ctx, run.ID)
	if fresh.Status != "failed" {
		t.Fatalf("run = %+v, want failed", fresh)
	}
	if !containsSubstr(fresh.Reason, "REFUSED") {
		t.Fatalf("fail reason = %q, want REFUSED", fresh.Reason)
	}
	if got := notif.drain(); len(got) != 1 || !containsSubstr(got[0], "run failed") {
		t.Fatalf("failure notifications = %v, want exactly one 'run failed'", got)
	}
}

// ---- tiny helpers -----------------------------------------------------------

type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) { return len(b), nil }

func f64(v float64) *float64 { return &v }

func nodeSeq(evs []HopEvent) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range evs {
		if !seen[e.Node] {
			seen[e.Node] = true
			out = append(out, e.Node)
		}
	}
	return out
}

func seqContainsInOrder(seq, want []string) bool {
	i := 0
	for _, s := range seq {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}

func countStatus(evs []HopEvent, st string) int {
	n := 0
	for _, e := range evs {
		if e.Status == st {
			n++
		}
	}
	return n
}

func containsSubstr(s, sub string) bool { return strings.Contains(s, sub) }
