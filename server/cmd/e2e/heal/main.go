// Command heal is the W5-1 definition-of-done harness: "a failing
// condition is detected, remediated, and the confirm step re-measures; on
// confirm-fail it escalates (ticket+notify)."
//
// It boots the real stack pieces against a scratch Postgres database and
// tears it all down:
//
//   - the real gRPC ingest service (JWT auth, enroll, command dispatch,
//     W3-3 capability tokens) on a plain listener,
//   - the real heal engine (0005_selfheal.sql state machine + store),
//   - two fake agents that mirror the REAL agent's command path: enroll
//     for a JWT + org root, verify each command's capability token against
//     the pinned root before acting (caps.Verify — the server-side
//     reference of agent/internal/caps), report RECEIVED/SUCCEEDED, and
//     "execute" the disk.full script by re-reporting the volume metric
//     over their live stream (agent A's cleanup frees space: 95 -> 62;
//     agent B's does not: stays 95).
//
// Then:
//  1. both devices report a 95% volume (> 90 detect threshold) -> one
//     pass detects, verifies-safe and dispatches a real RunScript (token
//     verified by both agents) to each;
//  2. replay pass: no second run, no second dispatch (DB-level invariant);
//  3. confirm pass: the re-measurement resolves A (62 <= 90) and
//     ESCALATES B (95 still > 90) — ticket row + notify fired exactly once;
//  4. later passes: no double-notify, B's still-hot volume is a
//     cooldown-skip (not a remediation storm);
//  5. A's heal_events log is the full state machine:
//     detected -> verifying -> remediating -> confirming -> resolved.
//
// Usage: RMMWAY_PG_DSN=... go run ./cmd/e2e/heal
package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func die(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

var stepName = "(init)"

func step(name string) {
	stepName = name
	fmt.Printf("\n== %s ==\n", name)
}
func info(f string, a ...any) { fmt.Printf("[%s] %s\n", stepName, fmt.Sprintf(f, a...)) }
func check(cond bool, f string, a ...any) {
	if !cond {
		die(f, a...)
	}
}

type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) { fmt.Printf("%s", b); return len(b), nil }

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	}
	u, err := url.Parse(dsn)
	if err != nil {
		die("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		die("postgres not reachable: %v", err)
	}
	dbName := "rmmway_heal_e2e_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		die("create scratch db: %v", err)
	}
	defer func() {
		ctxC, cancelC := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelC()
		_, _ = admin.Exec(ctxC, `DROP DATABASE IF EXISTS `+dbName)
	}()

	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		die("scratch pool: %v", err)
	}
	defer pool.Close()

	step("migrate")
	if n, err := store.Migrate(ctx, pool, "migrations"); err != nil {
		die("migrate: %v (n=%d)", err, n)
	} else if n != 8 {
		die("expected 8 migrations, got %d", n)
	}
	info("8 migrations applied to scratch db %s (starter playbooks seeded)", dbName)

	// ---- in-process server: real ingest gRPC service + org CA + caps ----
	step("boot in-process server")
	caMgr, err := ca.NewManager(ca.NewMemoryOrgStore(), time.Hour)
	if err != nil {
		die("org CA: %v", err)
	}
	issuer := caps.NewIssuer(caMgr.Root(), 10*time.Minute)
	rootCert, err := parseCertPEM(caMgr.RootCertPEM())
	if err != nil {
		die("parse org root: %v", err)
	}
	devices := store.NewPostgresDevices(pool)
	svc := ingest.NewService(ingest.Config{
		JWTSecret: []byte("e2e-heal-secret"),
		OrgCA:     caMgr,
		Caps:      issuer,
	}, store.NewPostgresMetricsSink(pool), devices)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		die("listen: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(svc.JWTInterceptor))
	agentv1.RegisterAgentServiceServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()
	info("ingest gRPC on %s (JWT + org CA + capability tokens)", lis.Addr())

	// The real heal engine: same pool, remediation through the real
	// Dispatcher (capability token rides the command), results from the
	// Dispatcher's result table, escalations captured.
	notify := &captureNotifier{}
	eng := heal.New(heal.NewStore(pool), func(ctx context.Context, deviceID, lang, script string) (string, error) {
		return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
			RunScript: &agentv1.RunScript{
				Lang:      lang,
				ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
			},
		})
	}, svc.Dispatcher().Result, notify).WithLogger(log.New(&logWriter{}, "selfheal: ", 0))

	// ---- two fake agents (real enroll, real stream, real token check) ---
	step("enroll + stream two agents")
	// agentA "heals": its cleanup script drops the volume to 62%.
	agentA := newFakeAgent("fileserver-a", rootCert, 62, func() string { tok, _ := svc.MintBootstrapToken(); return tok })
	// agentB "fails to heal": the script runs, the volume stays at 95%.
	agentB := newFakeAgent("fileserver-b", rootCert, 95, func() string { tok, _ := svc.MintBootstrapToken(); return tok })
	for _, fa := range []*fakeAgent{agentA, agentB} {
		if err := fa.enroll(ctx, lis.Addr().String()); err != nil {
			die("enroll %s: %v", fa.hostname, err)
		}
		info("%s enrolled as %s (jwt + org root pinned)", fa.hostname, fa.devID)
	}
	for _, fa := range []*fakeAgent{agentA, agentB} {
		go func(fa *fakeAgent) {
			if err := fa.run(ctx, lis.Addr().String()); err != nil {
				info("%s stream ended: %v", fa.hostname, err)
			}
		}(fa)
		<-fa.ready
	}
	info("both streams live (registered for dispatch)")
	// Both volumes are hot: 95% > 90.
	step("inject failing condition (95% volumes)")
	now := time.Now().UTC()
	for _, fa := range []*fakeAgent{agentA, agentB} {
		insertSample(ctx, pool, fa.devID, "disk.used_percent", "/dev/sda1", 95, now.Add(-10*time.Second))
	}
	info("dev-a + dev-b: disk.used_percent(/dev/sda1) = 95 (fresh)")

	// ---- pass 1: detect -> verify-safe -> remediate ----------------------
	step("pass 1: detect + remediate")
	pass := eng.RunOnce(ctx, time.Now())
	printPass(pass)
	check(len(pass.Errors) == 0, "pass errors: %v", pass.Errors)
	check(pass.Detections == 2, "detections = %d, want 2", pass.Detections)
	check(pass.Started == 2, "started = %d, want 2", pass.Started)
	check(pass.ActiveRuns == 2, "active = %d, want 2", pass.ActiveRuns)

	// Both agents must have RECEIVED a real command whose capability token
	// verifies against the pinned org root (the real agent's W3-3 gate) —
	// otherwise they would have answered REFUSED and the run would escalate
	// instead of heal.
	for _, fa := range []*fakeAgent{agentA, agentB} {
		select {
		case <-fa.executed:
			info("%s: EXECUTED remediation script (capability token verified vs pinned org root)", fa.hostname)
		case <-time.After(10 * time.Second):
			die("no executed command for %s", fa.hostname)
		}
	}

	// Immediate replay: no second run, no second dispatch (the partial
	// unique index + conditional transitions make double-remediation
	// impossible even if two passes interleave). `total` accumulates every
	// pass from here on — a fast agent can complete a heal within the
	// replay pass itself, and the cumulative outcome is what's pinned.
	step("replay pass (immediate): idempotency")
	before := countRuns(ctx, pool)
	total := &heal.Pass{}
	pass = eng.RunOnce(ctx, time.Now())
	printPass(pass)
	total.Confirmed += pass.Confirmed
	total.Escalated += pass.Escalated
	total.Failed += pass.Failed
	total.Errors = append(total.Errors, pass.Errors...)
	check(pass.Started == 0, "replay started %d runs, want 0", pass.Started)
	check(countRuns(ctx, pool) == before, "replay created runs (%d -> %d)", before, countRuns(ctx, pool))

	// ---- wait for the agents' final command results -----------------------
	step("agents report final results")
	st := heal.NewStore(pool)
	waitAll := func(deadline time.Duration) []heal.Run {
		until := time.Now().Add(deadline)
		for {
			runs, err := st.Runs(ctx, "", "", 50)
			if err != nil {
				die("runs: %v", err)
			}
			allTerminalOrConfirming := len(runs) == 2
			for i := range runs {
				switch runs[i].Status {
				case "confirming", "resolved", "escalated", "failed":
				default:
					allTerminalOrConfirming = false
				}
			}
			if allTerminalOrConfirming {
				return runs
			}
			if time.Now().After(until) {
				die("timed out waiting for final command results; runs: %+v", runs)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitAll(15 * time.Second)
	info("both agents: SUCCEEDED results recorded (capability tokens verified)")

	// ---- advance to terminal (confirm re-measures) ------------------------
	// A run moves at most one stage per pass (remediating -> confirming ->
	// resolved|escalated), and how far pass 1 already got each run depends
	// on stream timing — so advance in passes and assert the CUMULATIVE
	// outcome, which is deterministic: exactly one heal, one escalation.
	step("confirm passes: the re-measurement")
	for i := 0; i < 3; i++ {
		p := eng.RunOnce(ctx, time.Now())
		printPass(p)
		total.Confirmed += p.Confirmed
		total.Escalated += p.Escalated
		total.Failed += p.Failed
		total.Errors = append(total.Errors, p.Errors...)
		if total.Confirmed == 1 && total.Escalated == 1 {
			break
		}
	}
	check(len(total.Errors) == 0, "pass errors: %v", total.Errors)
	check(total.Confirmed == 1, "confirmed (cumulative) = %d, want 1 (agent A healed: 62 <= 90)", total.Confirmed)
	check(total.Escalated == 1, "escalated (cumulative) = %d, want 1 (agent B confirm-fail: 95 > 90)", total.Escalated)
	check(notify.count() == 1, "notifications = %d, want exactly 1", notify.count())
	check(strings.Contains(notify.reasons[0], "confirm FAILED"), "notify reason = %q, want confirm FAILED", notify.reasons[0])

	// ---- replay of the escalation: no double-notify, no storm ------------
	step("replay pass: no storm")
	pass = eng.RunOnce(ctx, time.Now())
	printPass(pass)
	check(pass.Escalated == 0, "re-escalated %d runs, want 0", pass.Escalated)
	check(pass.Confirmed == 0, "re-confirmed %d runs, want 0", pass.Confirmed)
	check(notify.count() == 1, "notifications = %d, want still 1 (no double-notify)", notify.count())
	check(pass.Started == 0, "started %d remediations inside the cooldown, want 0", pass.Started)
	check(pass.Skipped >= 1, "skipped = %d, want >=1 (B's hot volume is a cooldown-skip)", pass.Skipped)
	check(countDispatched(ctx, pool) == 2, "actual remediations = %d, want still 2 (no remediation storm)", countDispatched(ctx, pool))

	// ---- the audit trail --------------------------------------------------
	step("audit trail (heal_events)")
	runs, err := st.Runs(ctx, "", "", 50)
	if err != nil {
		die("runs: %v", err)
	}
	var runA, runB *heal.Run
	for i := range runs {
		switch runs[i].DeviceID {
		case agentA.devID:
			runA = &runs[i]
		case agentB.devID:
			if runs[i].Status == "escalated" {
				runB = &runs[i]
			}
		}
	}
	check(runA != nil && runA.Status == "resolved", "run A = %+v, want resolved", runA)
	check(runA.ConfirmValue != nil && *runA.ConfirmValue == 62,
		"run A confirm_value = %v, want 62 (the RE-measurement)", runA.ConfirmValue)
	check(runB != nil && runB.EscalatedAt != nil && strings.Contains(runB.Reason, "confirm FAILED"),
		"run B ticket = %+v, want escalated/confirm FAILED", runB)
	// A replayed transition is a no-op (idempotent engine).
	applied, err := st.Transition(ctx, runA.ID, "confirming", "resolved", map[string]any{"confirm_value": 62.0})
	check(err == nil && !applied, "replayed transition: applied=%v err=%v, want false/nil", applied, err)

	evA, err := st.Events(ctx, runA.ID)
	if err != nil {
		die("events A: %v", err)
	}
	var seq []string
	for _, e := range evA {
		seq = append(seq, e.Status)
	}
	wantSeq := []string{"detected", "verifying", "remediating", "confirming", "resolved"}
	check(fmt.Sprint(seq) == fmt.Sprint(wantSeq), "run A events = %v, want %v", seq, wantSeq)
	for _, e := range evA {
		info("run A (healed):    %-12s %s", e.Status, e.At.Format("15:04:05.000"))
	}
	evB, err := st.Events(ctx, runB.ID)
	if err != nil {
		die("events B: %v", err)
	}
	info("run B (escalated):   status=%s ticket_reason=%q", runB.Status, runB.Reason)
	for _, e := range evB {
		if e.Status == "escalated" {
			info("run B (escalated):   %-12s %s (%s)", e.Status, e.At.Format("15:04:05.000"), e.Reason)
		}
	}

	step("PASS")
	fmt.Println("W5-1 DoD met: detect -> verify-safe -> remediate -> confirm (re-measured) -> resolved;")
	fmt.Println("confirm-fail -> escalated (ticket + notify), idempotent + replay-safe across passes.")
}

// ---- fake agent (mirrors the real agent's W3-3 command path) ------------------

type fakeAgent struct {
	hostname string
	devID    string
	jwt      string
	rootCert *x509.Certificate
	mint     func() string
	// after: the volume % this agent re-reports after "running" the script.
	after    float64
	ready    chan struct{}
	executed chan struct{}
}

func newFakeAgent(hostname string, rootCert *x509.Certificate, after float64, mint func() string) *fakeAgent {
	return &fakeAgent{
		hostname: hostname,
		rootCert: rootCert,
		after:    after,
		mint:     mint,
		ready:    make(chan struct{}),
		executed: make(chan struct{}),
	}
}

// enroll does the real W1-4 enroll RPC (bootstrap token -> JWT + org root).
func (fa *fakeAgent) enroll(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	// Mint a bootstrap token through the same one-time-code API the W1-3
	// installer uses (the server process owns the mint).
	tok := fa.mint()
	resp, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: tok,
		Hostname:       fa.hostname,
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "e2e-heal",
	})
	if err != nil {
		return err
	}
	fa.devID, fa.jwt = resp.GetDeviceId(), resp.GetJwt()
	return nil
}

// run drives the stream: heartbeat (registers for dispatch), then execute
// arriving RunScripts after the W3-3 token gate, reporting results.
func (fa *fakeAgent) run(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+fa.jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if _, err := stream.Recv(); err != nil {
		return fmt.Errorf("heartbeat ack: %w", err)
	}
	close(fa.ready)
	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		cmd := resp.GetCommand()
		if cmd == nil {
			continue
		}
		rs := cmd.GetRunScript()
		if rs == nil {
			_ = sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, "not a run_script")
			continue
		}
		capName, ok := caps.ForAction(cmd.GetAction())
		if !ok {
			_ = sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, "unsupported action")
			continue
		}
		if err := caps.Verify(rs.GetCapabilityToken(), fa.rootCert, fa.devID, capName, cmd.GetId(), time.Now()); err != nil {
			info("agent %s: REFUSED %s (%s)", fa.hostname, cmd.GetId(), err)
			_ = sendResult(stream, cmd.GetId(), agentv1.CommandResult_REFUSED, err.Error())
			continue
		}
		_ = sendResult(stream, cmd.GetId(), agentv1.CommandResult_RECEIVED, "")
		body, err := base64.StdEncoding.DecodeString(rs.GetScriptB64())
		if err != nil {
			die("decode script: %v", err)
		}
		info("agent %s: executing script (%d bytes, %s)", fa.hostname, len(body), rs.GetLang())
		// "Execute": the disk.full cleanup. The measurable effect is the
		// next metric batch the agent reports over its live stream — that
		// fresh sample is what the confirm stage re-measures.
		if err := sendMetrics(stream, fa.devID, "disk.used_percent", "/dev/sda1", fa.after); err != nil {
			return err
		}
		select {
		case <-fa.executed:
		default:
			close(fa.executed)
		}
		_ = sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, "executed by fake agent")
	}
}

func sendResult(stream agentv1.AgentService_StreamClient, cmdID string, st agentv1.CommandResult_Status, errMsg string) error {
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_CommandResult{CommandResult: &agentv1.CommandResult{
			CommandId:     cmdID,
			Status:        st,
			Error:         errMsg,
			CompletedAtMs: time.Now().UnixMilli(),
		}},
	})
}

func sendMetrics(stream agentv1.AgentService_StreamClient, deviceID, name, source string, v float64) error {
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Metrics{Metrics: &agentv1.MetricBatch{
			CollectedAtMs: time.Now().UnixMilli(),
			Samples: []*agentv1.Metric{{
				Name:        name,
				Source:      source,
				Value:       v,
				TimestampMs: time.Now().UnixMilli(),
			}},
		}},
	})
}

// ---- helpers -------------------------------------------------------------------

func printPass(p *heal.Pass) {
	b, _ := json.Marshal(p)
	info("pass: %s", b)
}

func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func insertSample(ctx context.Context, pool *pgxpool.Pool, deviceID, metric, source string, v float64, at time.Time) {
	if _, err := pool.Exec(ctx, `
		INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1, $2, $3, $4, '{}', $5, $6)
		ON CONFLICT DO NOTHING`, deviceID, metric, source, v, at.UnixMilli(), at.UTC()); err != nil {
		die("insert sample: %v", err)
	}
}

func countRuns(ctx context.Context, pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM heal_runs`).Scan(&n); err != nil {
		die("count runs: %v", err)
	}
	return n
}

// countDispatched counts runs that actually remediated (command dispatched)
// — the "storm" invariant: while a condition persists, only cooldown-skip
// rows may accumulate, never more remediations.
func countDispatched(ctx context.Context, pool *pgxpool.Pool) int {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM heal_runs WHERE dispatched_at IS NOT NULL`).Scan(&n); err != nil {
		die("count dispatched: %v", err)
	}
	return n
}

// captureNotifier counts + records escalations (the "notify" half of the
// DoD); W6-2's NATS/webhook notifier plugs into the same seam.
type captureNotifier struct {
	mu      sync.Mutex
	reasons []string
}

func (n *captureNotifier) Escalate(r *heal.Run, reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reasons = append(n.reasons, r.PlaybookKey+"/"+r.DeviceID+": "+reason)
	info("NOTIFY: run %d %s/%s: %s", r.ID, r.PlaybookKey, r.DeviceID, reason)
}

func (n *captureNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.reasons)
}
