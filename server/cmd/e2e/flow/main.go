// Command flow is the W5-2 definition-of-done harness: "compose
// disk>90% -> free -> if>90% -> notify and it fires correctly on the
// synthetic trigger."
//
// Unlike the heal e2e (in-process engine), this one runs the flow chain
// OVER A REAL NATS JETSTREAM BUS:
//
//   - the real gRPC ingest service (JWT, enroll, dispatch, W3-3 capability
//     tokens) on a plain listener,
//   - a NATS/JetStream stream (RMMWAY_EVENTS) that carries every flow hop,
//   - the real flow engine consuming that stream,
//   - the ingest OnCommandResult hook publishing command.result hops to the
//     same stream,
//   - two fake agents that mirror the REAL agent's command path (verify the
//     capability token against the pinned org root before acting, re-report
//     the volume metric, report SUCCEEDED).
//
// The synthetic trigger (disk=95) is published to the bus; every subsequent
// hop (step, command.result, step, notify) also travels the bus. agent A
// "heals" (re-reports 62 -> the check's condition is not held -> the chain
// ends quietly); agent B stays full (re-reports 95 -> the check holds ->
// the notify node fires exactly once).
//
// Usage: RMMWAY_PG_DSN=... RMMWAY_NATS_URL=... go run ./cmd/e2e/flow
package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
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
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
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

const streamName = "RMMWAY_EVENTS"

// resetStream drops the JetStream stream (and its durable consumer) so a
// re-run starts clean instead of rebinding a stale consumer.
func resetStream(ctx context.Context, natsURL string) error {
	nc, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	jsm, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	if err := jsm.DeleteStream(streamName); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No stream") {
			return nil
		}
		return fmt.Errorf("delete stream: %w", err)
	}
	return nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	}
	natsURL := os.Getenv("RMMWAY_NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	// ---- scratch Postgres ------------------------------------------------
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
	dbName := "rmmway_flow_e2e_" + time.Now().Format("20060102150405")
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
	info("8 migrations applied to scratch db %s", dbName)

	// ---- real NATS bus ---------------------------------------------------
	step("nats jetstream bus")
	if err := resetStream(ctx, natsURL); err != nil {
		die("reset stream: %v", err)
	}
	bus, err := flow.NewNatsBus(ctx, natsURL, streamName, "flow-engine")
	if err != nil {
		die("nats bus: %v", err)
	}
	defer bus.Close()
	info("stream %s ready at %s", streamName, natsURL)

	// ---- in-process server: real ingest + org CA + caps ------------------
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
		JWTSecret: []byte("e2e-flow-secret"),
		OrgCA:     caMgr,
		Caps:      issuer,
		OnCommandResult: func(res *agentv1.CommandResult) {
			// The W5-2 chain hop: a final agent command answer becomes a
			// bus event so the waiting flow script node advances.
			_ = bus.Publish(ctx, flow.SubjectCommand, &flow.Event{
				Type: flow.SubjectCommand, CommandID: res.GetCommandId(),
				Status: res.GetStatus().String(), Message: res.GetError(), At: time.Now().UTC(),
			})
		},
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

	// ---- the flow engine, consuming the NATS stream ----------------------
	notify := &captureNotifier{}
	st := flow.NewStore(pool)
	eng := flow.New(st, bus,
		func(ctx context.Context, deviceID, lang, script string) (string, error) {
			return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
				RunScript: &agentv1.RunScript{
					Lang:      lang,
					ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
				},
			})
		},
		svc.Dispatcher().Result, notify, 400*time.Millisecond, -1) // sampler off; sweep re-covers
	eng = eng.WithLogger(log.New(&logWriter{}, "flow: ", 0))
	if err := eng.Start(ctx); err != nil {
		die("engine start: %v", err)
	}
	info("flow engine subscribed to %s (sweep 400ms, sampler off)", streamName)

	// ---- compose the DoD chain -------------------------------------------
	step("compose: disk>90 -> free -> if>90 -> notify")
	f, err := st.CreateFlow(ctx, "disk-full", "W5-2 DoD chain", flow.Graph{Nodes: []flow.Node{
		{ID: "t", Kind: flow.KindTrigger, Name: "disk > 90%", Metric: "disk.used_percent", Op: ">", Threshold: 90, Next: "free"},
		{ID: "free", Kind: flow.KindScript, Name: "free space", Lang: "sh", Script: "# free reclaimable space\ndf -h\nexit 0", TimeoutS: 120, Next: "still"},
		{ID: "still", Kind: flow.KindCheck, Name: "if still > 90%", Metric: "disk.used_percent", Op: ">", Threshold: 90, Then: "notify", Else: ""},
		{ID: "notify", Kind: flow.KindNotify, Name: "notify", Message: "disk still full after cleanup — needs a human"},
	}}, 0, true)
	if err != nil {
		die("create flow: %v", err)
	}
	info("flow %d created: t -> free -> still(check) -> [then] notify / [else] end", f.ID)

	// ---- two fake agents (real enroll, real stream, real token check) ----
	step("enroll + stream two agents")
	// agentA "heals": its cleanup drops the volume to 62%.
	agentA := newFakeAgent("fileserver-a", rootCert, 62, func() string { tok, _ := svc.MintBootstrapToken(); return tok })
	// agentB stays full: the script runs, the volume remains at 95%.
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

	// ---- fire the synthetic trigger --------------------------------------
	step("fire synthetic trigger (disk = 95) on both devices")
	v := 95.0
	for _, fa := range []*fakeAgent{agentA, agentB} {
		if err := eng.Trigger(ctx, f.ID, fa.devID, "", &v); err != nil {
			die("trigger %s: %v", fa.hostname, err)
		}
	}
	info("two trigger events published to the bus")

	// Wait for both runs to reach a terminal state (the sweep re-covers any
	// hop the bus delayed; everything else is event-driven).
	deadline := time.Now().Add(30 * time.Second)
	for {
		ra, errA := runFor(ctx, st, f.ID, agentA.devID)
		rb, errB := runFor(ctx, st, f.ID, agentB.devID)
		if errA == nil && errB == nil && isTerminal(ra.Status) && isTerminal(rb.Status) {
			break
		}
		if time.Now().After(deadline) {
			dumpRun(ctx, st, f.ID, agentA.devID, "A")
			dumpRun(ctx, st, f.ID, agentB.devID, "B")
			die("timed out waiting for terminal runs")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Give the bus a moment to settle any final notify hop.
	time.Sleep(400 * time.Millisecond)

	// ---- assert the outcomes ---------------------------------------------
	step("assert outcomes")
	runA, err := runFor(ctx, st, f.ID, agentA.devID)
	if err != nil {
		die("run A: %v", err)
	}
	runB, err := runFor(ctx, st, f.ID, agentB.devID)
	if err != nil {
		die("run B: %v", err)
	}
	check(runA.Status == "succeeded", "run A status = %s, want succeeded (healed: 62 <= 90)", runA.Status)
	check(runB.Status == "succeeded", "run B status = %s, want succeeded (still-full: notify fired, chain complete)", runB.Status)

	// Exactly ONE notify total, and it must be agent B's (the still-full one).
	got := notify.drain()
	check(len(got) == 1, "notifications = %d: %v, want exactly 1", len(got), got)
	check(strings.Contains(got[0], agentB.devID), "notify was for %q, want agent B (%s): %q", got[0], agentB.devID, got[0])
	info("NOTIFY (agent B, still full): %s", got[0])

	// Both agents actually EXECUTED a script whose capability token verified
	// against the pinned org root (the real agent's W3-3 gate).
	for _, fa := range []*fakeAgent{agentA, agentB} {
		select {
		case <-fa.executed:
			info("%s: EXECUTED free-space script (capability token verified vs pinned org root)", fa.hostname)
		default:
			die("no executed command for %s", fa.hostname)
		}
	}

	// ---- the audit trail (every hop traveled the bus) ---------------------
	step("audit trail (flow_events)")
	evsA, err := st.Events(ctx, runA.ID)
	if err != nil {
		die("events A: %v", err)
	}
	evsB, err := st.Events(ctx, runB.ID)
	if err != nil {
		die("events B: %v", err)
	}
	info("run A: %s (status=%s)", seqNodes(evsA), runA.Status)
	for _, e := range evsA {
		info("   A %-6s %-9s %s", e.Node, e.Status, e.Reason)
	}
	info("run B: %s (status=%s)", seqNodes(evsB), runB.Status)
	for _, e := range evsB {
		info("   B %-6s %-9s %s", e.Node, e.Status, e.Reason)
	}
	check(seqNodes(evsA) == "t,free,still", "run A node order = %s, want t,free,still", seqNodes(evsA))
	check(seqNodes(evsB) == "t,free,still,notify", "run B node order = %s, want t,free,still,notify", seqNodes(evsB))

	step("PASS")
	fmt.Println("W5-2 DoD met: disk>90 -> free -> if>90 -> notify composed and fired on the synthetic")
	fmt.Println("trigger, every hop over NATS; healed chain quiet, still-full chain notified exactly once.")
}

// runFor returns the most recent run of a flow for a device.
func runFor(ctx context.Context, st *flow.Store, flowID int64, deviceID string) (*flow.Run, error) {
	fid := flowID
	runs, err := st.Runs(ctx, "", deviceID, &fid, 5)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no run for %s", deviceID)
	}
	return &runs[0], nil
}

func seqNodes(evs []flow.HopEvent) string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range evs {
		if !seen[e.Node] {
			seen[e.Node] = true
			out = append(out, e.Node)
		}
	}
	return strings.Join(out, ",")
}

func isTerminal(s string) bool {
	return s == "succeeded" || s == "failed" || s == "timeout"
}

func dumpRun(ctx context.Context, st *flow.Store, flowID int64, deviceID, label string) {
	fid := flowID
	runs, _ := st.Runs(ctx, "", deviceID, &fid, 5)
	if len(runs) == 0 {
		info("run %s: (none)", label)
		return
	}
	r := &runs[0]
	info("run %s: status=%s node=%s cmd=%v reason=%q", label, r.Status, r.CurrentNode, r.CommandID, r.Reason)
	if evs, err := st.Events(ctx, r.ID); err == nil {
		for _, e := range evs {
			info("   %-6s %-9s %s", e.Node, e.Status, e.Reason)
		}
	}
}

// ---- fake agent (mirrors the real agent's W3-3 command path) ------------------

type fakeAgent struct {
	hostname string
	devID    string
	jwt      string
	rootCert *x509.Certificate
	mint     func() string
	// cur is the volume % this agent currently reports (starts "full" at
	// 95; after "executing" the free-space script it becomes `after`).
	// Guarded by sendMu (no typed atomics in this toolchain).
	cur      float64
	after    float64
	ready    chan struct{}
	executed chan struct{}
	sendMu   sync.Mutex // serializes stream sends (one gRPC stream, 2 writers)
}

func newFakeAgent(hostname string, rootCert *x509.Certificate, after float64, mint func() string) *fakeAgent {
	fa := &fakeAgent{
		hostname: hostname, rootCert: rootCert, after: after, mint: mint,
		ready: make(chan struct{}), executed: make(chan struct{}),
	}
	fa.cur = 95 // the volume is full until the script runs
	return fa
}

// reportLoop mirrors the real agent's metric cadence: a fresh batch every
// ~150ms. The check node's re-measurement (and the sweep re-cover) rely on
// the agent KEEPING REPORT after the remediation, exactly like production.
func (fa *fakeAgent) reportLoop(ctx context.Context, stream agentv1.AgentService_StreamClient) {
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fa.sendMetrics(stream, "disk.used_percent", "", fa.getCur())
		}
	}
}

func (fa *fakeAgent) sendResult(stream agentv1.AgentService_StreamClient, cmdID string, st agentv1.CommandResult_Status, errMsg string) error {
	fa.sendMu.Lock()
	defer fa.sendMu.Unlock()
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_CommandResult{CommandResult: &agentv1.CommandResult{
			CommandId: cmdID, Status: st, Error: errMsg, CompletedAtMs: time.Now().UnixMilli(),
		}},
	})
}

func (fa *fakeAgent) sendMetrics(stream agentv1.AgentService_StreamClient, name, source string, v float64) error {
	fa.sendMu.Lock()
	defer fa.sendMu.Unlock()
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Metrics{Metrics: &agentv1.MetricBatch{
			CollectedAtMs: time.Now().UnixMilli(),
			Samples: []*agentv1.Metric{{
				Name: name, Source: source, Value: v, TimestampMs: time.Now().UnixMilli(),
			}},
		}},
	})
}

func (fa *fakeAgent) getCur() float64  { fa.sendMu.Lock(); defer fa.sendMu.Unlock(); return fa.cur }
func (fa *fakeAgent) setCur(v float64) { fa.sendMu.Lock(); fa.cur = v; fa.sendMu.Unlock() }

func (fa *fakeAgent) enroll(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	tok := fa.mint()
	resp, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: tok, Hostname: fa.hostname, Os: "linux", Arch: "amd64", AgentVersion: "e2e-flow",
	})
	if err != nil {
		return err
	}
	fa.devID, fa.jwt = resp.GetDeviceId(), resp.GetJwt()
	return nil
}

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
	go fa.reportLoop(ctx, stream) // real-agent metric cadence
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
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, "not a run_script")
			continue
		}
		capName, ok := caps.ForAction(cmd.GetAction())
		if !ok {
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, "unsupported action")
			continue
		}
		if err := caps.Verify(rs.GetCapabilityToken(), fa.rootCert, fa.devID, capName, cmd.GetId(), time.Now()); err != nil {
			info("agent %s: REFUSED %s (%s)", fa.hostname, cmd.GetId(), err)
			_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_REFUSED, err.Error())
			continue
		}
		_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_RECEIVED, "")
		// "Execute" the free-space script: its measurable effect is the
		// agent's NEXT metric batches reporting the freed volume.
		fa.setCur(fa.after)
		select {
		case <-fa.executed:
		default:
			close(fa.executed)
		}
		_ = fa.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, "executed by fake agent")
	}
}

func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// captureNotifier counts + records notifications (the "notify" half of the
// DoD); W6-2's NATS/webhook notifier plugs into the same seam.
type captureNotifier struct {
	mu       sync.Mutex
	devByRun map[int64]string
	reasons  []string
}

func (n *captureNotifier) Notify(ctx context.Context, r *flow.Run, nodeID, reason string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reasons = append(n.reasons, r.DeviceID+"/"+nodeID+": "+reason)
	info("NOTIFY: run %d %s/%s: %s", r.ID, r.FlowName, r.DeviceID, reason)
}

func (n *captureNotifier) drain() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := append([]string{}, n.reasons...)
	n.reasons = nil
	return out
}
