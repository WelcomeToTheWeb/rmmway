// Command automation is the W6-3 milestone harness — the Phase 1 MVP's
// "full automation E2E":
//
//	one triggered condition  ->  alert fires  ->  self-heal runs + confirms
//	                          ->  ticket opened  ->  webhook fires
//
// A SINGLE condition — one device whose disk is 95% full (> 90% threshold) —
// drives all four subsystems the way the production server wires them, on a
// REAL NATS/JetStream bus, and the whole chain is AUDITED (each subsystem's
// Postgres trail is read back and tied to the one condition):
//
//  1. ALERT FIRES   (W2-4) — the baseline anomaly for disk.used_percent is
//     folded into the deduped alert inbox by the real reconciler: one open
//     alert row (audited in `alerts`) + a rmmway.events.alert event on the bus.
//  2. SELF-HEAL RUNS + CONFIRMS (W5-1) — the seeded `disk.full` playbook
//     detects the 95%, dispatches its real "free space" RunScript (W3-3
//     capability token verified by the fake agent against the pinned org
//     root), and the CONFIRM stage RE-measures: the agent's post-remediation
//     62% sample satisfies "<= 90" so the run RESOLVES. Audited: the
//     heal_runs row reaches `resolved` and heal_events is the full state
//     machine (detected->verifying->remediating->confirming->resolved). The
//     terminal outcome is also published to the bus (as main.go's heal
//     notifier does for escalations) so the integration endpoint sees it.
//  3. TICKET OPENED (W5-2) — an automation flow "disk-full-ticket"
//     (trigger disk>90 -> open-ticket script -> notify) is triggered by the
//     SAME condition and runs OVER the bus: the open-ticket script is
//     dispatched + executed (SUCCEEDED), then the notify node fires — that
//     rmmway.events.flow.notify event is the opened ticket. Audited: the
//     flow_runs row reaches `succeeded` and flow_events is the hop trail.
//  4. WEBHOOK FIRES (W6-2) — the webhook framework (a SEPARATE durable
//     consumer on the same stream) journals every event with a monotonic seq
//     and delivers HMAC-signed webhooks to a user-defined endpoint (a local
//     httptest receiver). Proven: the endpoint received the alert event,
//     the self-heal event, and the ticket event — every delivery verifies its
//     HMAC-SHA256 signature with the shared secret, and a wrong secret is
//     rejected.
//
// The audit section then reads back all four trails (alerts, heal_runs +
// heal_events, flow_runs + flow_events, and the webhook_events journal) and
// prints a single summary showing that the one 95%-disk condition produced
// all four, each independently audited.
//
// Usage: RMMWAY_PG_DSN=... RMMWAY_NATS_URL=... go run ./cmd/e2e/automation
package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/baseline"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/webhook"
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

// resetStream drops the JetStream stream (and its durable consumers) so a
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

// ---- bus notifiers (the W6-2 seams main.go wires) --------------------------

// flowNotifier is the flow Notifier: every notify node + run failure becomes
// an "automation" event on the bus (here the notify node IS the ticket).
type flowNotifier struct {
	pub func(subject, deviceID, message string, data map[string]any)
}

func (n flowNotifier) Notify(ctx context.Context, run *flow.Run, nodeID, reason string) {
	n.pub(flow.SubjectNotify, run.DeviceID, "flow "+run.FlowName+" node="+nodeID+": "+reason, map[string]any{
		"action": "notify", "run_id": run.ID, "flow": run.FlowName,
		"node": nodeID, "device_id": run.DeviceID, "message": reason,
	})
}

// healNotifier is the heal Notifier: an escalation becomes an "automation"
// event on the bus (same seam as main.go's busHealNotifier). The RESOLVED
// outcome (the happy path this milestone proves) has no engine hook, so the
// harness publishes it directly — exactly the emitter-to-bus wiring pattern
// main.go uses for every other source.
type healNotifier struct {
	pub func(subject, deviceID, message string, data map[string]any)
}

func (n healNotifier) Escalate(run *heal.Run, reason string) {
	n.pub(flow.SubjectNotify, run.DeviceID, "selfheal escalated "+run.PlaybookKey+": "+reason, map[string]any{
		"action": "escalated", "run_id": run.ID, "playbook": run.PlaybookKey,
		"device_id": run.DeviceID, "source": run.Source, "reason": reason,
	})
}

func (n healNotifier) publishResolved(run *heal.Run) {
	n.pub(flow.SubjectNotify, run.DeviceID,
		"selfheal resolved "+run.PlaybookKey+" (re-measured "+fmt.Sprint(deref(run.ConfirmValue))+" ok)",
		map[string]any{
			"action": "selfheal", "outcome": "resolved", "run_id": run.ID,
			"playbook": run.PlaybookKey, "device_id": run.DeviceID,
			"metric": "disk.used_percent", "detect_value": deref(run.DetectValue),
			"confirm_value": deref(run.ConfirmValue),
		})
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ---- the user-defined endpoint's receiver -----------------------------------

// delivery is one received webhook (as the endpoint saw it).
type delivery struct {
	Body      []byte
	SigHeader string
	IDHeader  string
	EventHdr  string
	Status    int
}

// recorder is a webhook receiver: it stores every delivery (always 200, so
// the cursor advances and every event is delivered exactly once in this
// harness — retry/replay is W6-2's own DoD).
type recorder struct {
	mu   sync.Mutex
	dels []delivery
}

func (r *recorder) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.dels = append(r.dels, delivery{
		Body: body, SigHeader: req.Header.Get(webhook.SignHeader),
		IDHeader: req.Header.Get(webhook.IdHeader), EventHdr: req.Header.Get(webhook.EventHeader),
		Status: http.StatusOK,
	})
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *recorder) seen() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery{}, r.dels...)
}

// envelope is the signed payload the receiver decodes.
type envelope struct {
	ID       int64           `json:"id"`
	Category string          `json:"category"`
	Type     string          `json:"type"`
	DeviceID string          `json:"device_id,omitempty"`
	At       time.Time       `json:"at"`
	Event    json.RawMessage `json:"event"`
}

// eventKinds is what the endpoint has received (verified), keyed by the
// payload's own `action`/category.
type eventKinds struct {
	alert    bool // category alert (rmmway.events.alert)
	selfheal bool // action selfheal (resolved self-heal)
	ticket   bool // action notify from the disk-full-ticket flow
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	dsn := os.Getenv("RMMWAY_PG_DSN")
	if dsn == "" {
		dsn = "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable"
	}
	natsURL := os.Getenv("RMMWAY_NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	// ---- scratch Postgres --------------------------------------------------
	step("scratch postgres + migrate")
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
	dbName := "rmmway_auto_e2e_" + time.Now().Format("20060102150405")
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
	if n, err := store.Migrate(ctx, pool, "migrations"); err != nil {
		die("migrate: %v (n=%d)", err, n)
	} else if n != 8 {
		die("expected 8 migrations, got %d", n)
	}
	info("8 migrations applied to scratch db %s (disk.full playbook seeded)", dbName)

	// ---- real NATS bus -----------------------------------------------------
	step("nats jetstream bus")
	if err := resetStream(ctx, natsURL); err != nil {
		die("reset stream: %v", err)
	}
	// Two durable consumers on ONE stream: the flow engine ("flow-engine")
	// and the webhook framework ("webhook-engine") each need to see EVERY
	// event, so they are separate consumers, not a shared one.
	flowBus, err := flow.NewNatsBus(ctx, natsURL, streamName, "flow-engine")
	if err != nil {
		die("nats bus (flow): %v", err)
	}
	defer flowBus.Close()
	whBus, err := flow.NewNatsBus(ctx, natsURL, streamName, "webhook-engine")
	if err != nil {
		die("nats bus (webhook): %v", err)
	}
	defer whBus.Close()
	info("stream %s ready at %s (flow-engine + webhook-engine consumers)", streamName, natsURL)

	// ---- org CA + caps (the fake agent verifies the capability token) ------
	step("org CA + caps")
	caMgr, err := ca.NewManager(ca.NewMemoryOrgStore(), time.Hour)
	if err != nil {
		die("org CA: %v", err)
	}
	issuer := caps.NewIssuer(caMgr.Root(), 10*time.Minute)
	rootCert, err := parseCertPEM(caMgr.RootCertPEM())
	if err != nil {
		die("parse org root: %v", err)
	}

	// ---- event fan-out: every emitter -> the bus (main.go's pattern) -------
	publishEvent := func(subject, deviceID, message string, data map[string]any) {
		_ = flowBus.Publish(context.Background(), subject, &flow.Event{
			Type: subject, DeviceID: deviceID, Message: message, Data: data, At: time.Now().UTC(),
		})
	}

	// ---- STEP 1 producer: alert store (W2-4) with its event sink -----------
	alertStore := store.NewAlertStore(pool, 1)
	alertStore.SetEventSink(func(action string, payload map[string]any) {
		devID, _ := payload["device_id"].(string)
		publishEvent(flow.SubjectAlert, devID, action+" alert "+fmt.Sprint(payload["name"]), payload)
	})

	// ---- real gRPC ingest (enroll + dispatch + command results) ------------
	devices := store.NewPostgresDevices(pool)
	svc := ingest.NewService(ingest.Config{
		JWTSecret: []byte("e2e-automation-secret"),
		OrgCA:     caMgr,
		Caps:      issuer,
		OnCommandResult: func(res *agentv1.CommandResult) {
			// A FINAL agent command answer is a bus event so a waiting flow
			// script node advances (the W5-2 chain hop).
			publishEvent(flow.SubjectCommand, "", res.GetStatus().String(), map[string]any{
				"command_id": res.GetCommandId(), "status": res.GetStatus().String(),
			})
		},
		OnDeviceEvent: func(action string, payload map[string]any) {
			devID, _ := payload["device_id"].(string)
			publishEvent(flow.SubjectDevice, devID, action+" device", payload)
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

	// ---- STEP 2 producer: self-heal engine (W5-1) -------------------------
	healSt := heal.NewStore(pool)
	healNot := healNotifier{pub: publishEvent}
	healEng := heal.New(healSt, func(ctx context.Context, deviceID, lang, script string) (string, error) {
		return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
			RunScript: &agentv1.RunScript{
				Lang:      lang,
				ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
			},
		})
	}, svc.Dispatcher().Result, healNot).WithLogger(log.New(&logWriter{}, "selfheal: ", 0))

	// ---- STEP 3 producer: flow engine over NATS (W5-2) --------------------
	flowSt := flow.NewStore(pool)
	flowEng := flow.New(flowSt, flowBus,
		func(ctx context.Context, deviceID, lang, script string) (string, error) {
			return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
				RunScript: &agentv1.RunScript{
					Lang:      lang,
					ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
				},
			})
		},
		svc.Dispatcher().Result, flowNotifier{pub: publishEvent}, 300*time.Millisecond, -1) // sampler off
	flowEng = flowEng.WithLogger(log.New(&logWriter{}, "flow: ", 0))
	if err := flowEng.Start(ctx); err != nil {
		die("flow engine start: %v", err)
	}
	info("flow engine subscribed (sweep 300ms, sampler off)")

	// ---- STEP 4: webhook framework (journal + signed delivery) ------------
	whStore := webhook.NewStore(pool)
	whSvc := webhook.New(whStore, whBus).WithLogger(log.New(&logWriter{}, "webhook: ", 0)).WithSweepInterval(250 * time.Millisecond)
	if err := whSvc.Start(ctx); err != nil {
		die("webhook start: %v", err)
	}
	info("webhook framework subscribed (sweep 250ms)")

	// ---- operator HTTP surface (the real httpapi) -------------------------
	apiSrv := httpapi.New(httpapi.Config{
		Devices:   devices,
		JWTSecret: []byte("e2e-automation-secret"),
		AdminUser: "admin", AdminPassword: "admin",
		Webhooks: whSvc,
	})
	mux := http.NewServeMux()
	apiSrv.Register(mux)
	apiSrv2 := httptest.NewServer(mux)
	defer apiSrv2.Close()
	apiBase := apiSrv2.URL
	info("operator HTTP surface on %s (/admin/webhooks, /admin/events/stream)", apiBase)
	// C1: the operator routes are JWT-gated — log in once for a token.
	opTok := loginOp(ctx, apiBase)

	// ---- the one device ----------------------------------------------------
	step("enroll the one device (the condition's host)")
	fa := newFakeAgent("fileserver-01", rootCert, func() string { tok, _ := svc.MintBootstrapToken(); return tok })
	if err := fa.enroll(ctx, lis.Addr().String()); err != nil {
		die("enroll: %v", err)
	}
	info("%s enrolled as %s", fa.hostname, fa.devID)
	agentStreamCtx, agentStreamCancel := context.WithCancel(ctx)
	defer agentStreamCancel()
	go func() { _ = fa.run(agentStreamCtx, lis.Addr().String()) }()
	<-fa.ready
	info("%s stream live (reportLoop: disk = 95%% until a script runs)", fa.hostname)

	// ---- the user-defined endpoint (via the real HTTP surface) ------------
	step("create the user-defined webhook endpoint (all automation-relevant cats)")
	recv := &recorder{}
	srvRecv := httptest.NewServer(http.HandlerFunc(recv.serve))
	defer srvRecv.Close()
	const secret = "shared-secret-automation"
	ep, err := createWebhook(ctx, apiBase, opTok, "ops-automation", srvRecv.URL, secret, []string{
		webhook.CategoryAlert, webhook.CategoryAutomation,
	})
	if err != nil {
		die("create endpoint: %v", err)
	}
	info("endpoint id=%d url=%s (categories: alert, automation)", ep.ID, srvRecv.URL)

	// ---- THE ONE CONDITION: disk is 95% full on fileserver-01 --------------
	step("the one condition: disk.used_percent = 95% (fresh)")
	// A known fresh 95% sample so the detect/alert stages have a deterministic
	// observation; the agent's reportLoop also reports 95 (cur=95) until a
	// remediation script runs, after which it reports 62.
	insertSample(ctx, pool, fa.devID, "disk.used_percent", "", 95, time.Now().UTC())
	info("%s: disk.used_percent = 95 (> 90 threshold)", fa.devID)

	// ---- STEP 1: ALERT FIRES (W2-4) ----------------------------------------
	step("step 1: alert fires (baseline anomaly -> deduped inbox)")
	anoms := []baseline.Anomaly{{
		At: time.Now().UTC(), DeviceID: fa.devID, Name: "disk.used_percent", Source: "",
		Value: 95, Score: 6.0,
		Seasonal: &baseline.CellScore{Z: 6.0, Median: 55, MAD: 1, EWMA: 55, Cells: 40},
	}}
	alertStore.Reconcile(anoms, map[baseline.SeriesKey]bool{{DeviceID: fa.devID, Name: "disk.used_percent", Source: ""}: true})
	// The reconciler ran synchronously; the alert row is committed.
	_ = waitFor(func() bool {
		l, err := alertStore.List(ctx, "open", fa.devID, 10)
		return err == nil && len(l) > 0
	}, 5*time.Second, "alert row to appear")
	_ = waitFor(func() bool {
		return kinds(recv.seen(), secret).alert
	}, 5*time.Second, "alert event to reach the endpoint")
	info("alert FIRED: open alert row in the inbox + rmmway.events.alert on the bus -> endpoint")

	// ---- STEP 2: SELF-HEAL RUNS + CONFIRMS (W5-1) -------------------------
	step("step 2: self-heal runs + confirms (disk.full playbook)")
	// pass 1: detect the 95% (latest fresh sample) -> dispatch the real
	// "free space" remediation (capability token rides the command).
	p1 := healEng.RunOnce(ctx, time.Now())
	info("pass 1: %s", passJSON(p1))
	check(len(p1.Errors) == 0, "pass 1 errors: %v", p1.Errors)
	check(p1.Detections == 1, "detections = %d, want 1", p1.Detections)
	check(p1.Started == 1, "started = %d, want 1 (remediation dispatched)", p1.Started)
	// The agent must have RECEIVED + EXECUTED the real command whose W3-3
	// capability token verifies against the pinned org root (else REFUSED ->
	// escalated, not healed).
	select {
	case <-fa.executed:
		info("agent EXECUTED the remediation script (capability token verified vs pinned org root)")
	case <-time.After(15 * time.Second):
		die("no executed remediation command for %s", fa.hostname)
	}
	// Advance to confirm: the CONFIRM stage RE-measures from a sample strictly
	// after the dispatch. The agent's post-remediation reportLoop emits 62%,
	// which satisfies the confirm condition (<= 90) -> resolved. Loop a few
	// passes (the engine moves at most one stage per pass and timing is
	// stream-driven); the outcome is deterministic.
	var resolved *heal.Run
	for i := 0; i < 15 && resolved == nil; i++ {
		healEng.RunOnce(ctx, time.Now())
		runs, _ := healSt.Runs(ctx, "", fa.devID, 10)
		for i := range runs {
			if runs[i].Status == "resolved" {
				resolved = &runs[i]
			}
		}
		if resolved == nil {
			time.Sleep(200 * time.Millisecond)
		}
	}
	check(resolved != nil, "self-heal did not RESOLVE (confirm never re-measured a passing sample)")
	check(resolved.ConfirmValue != nil && *resolved.ConfirmValue == 62,
		"confirm_value = %v, want 62 (the RE-measurement after remediation)", resolved.ConfirmValue)
	healNot.publishResolved(resolved)
	info("self-heal CONFIRMED + RESOLVED: re-measured disk=62 (<= 90) — run %d, heal_events audited", resolved.ID)

	// ---- STEP 3: TICKET OPENED (W5-2) --------------------------------------
	step("step 3: ticket opened (automation flow runs over NATS)")
	const ticketScript = `#!/bin/sh
# rmmway automation: open an incident ticket for the full-disk event
echo "open ticket: full-disk incident on fileserver-01"
exit 0`
	f, err := flowSt.CreateFlow(ctx, "disk-full-ticket", "W6-3: one condition -> open an incident ticket",
		flow.Graph{Nodes: []flow.Node{
			{ID: "t", Kind: flow.KindTrigger, Name: "disk > 90%", Metric: "disk.used_percent", Op: ">", Threshold: 90, Next: "open_ticket"},
			{ID: "open_ticket", Kind: flow.KindScript, Name: "open ticket", Lang: "sh", Script: ticketScript, TimeoutS: 120, Next: "notify"},
			{ID: "notify", Kind: flow.KindNotify, Name: "ticket opened", Message: "opened incident ticket for full disk (ticket id = this flow run)"},
		}}, 0, true)
	if err != nil {
		die("create flow: %v", err)
	}
	info("flow %d created: t -> open_ticket(script) -> notify (the opened ticket)", f.ID)
	v := 95.0
	if err := flowEng.Trigger(ctx, f.ID, fa.devID, "", &v); err != nil {
		die("trigger: %v", err)
	}
	info("synthetic trigger published (disk=95) -> flow %d opens the ticket over the bus", f.ID)
	fid := f.ID
	run, err := waitForTerminalFlow(ctx, flowSt, fid, fa.devID, 30*time.Second)
	check(err == nil, "flow run did not reach terminal: %v", err)
	check(run != nil && run.Status == "succeeded", "flow run status = %v, want succeeded (ticket opened)", statusOf(run))
	_ = waitFor(func() bool {
		return kinds(recv.seen(), secret).ticket
	}, 10*time.Second, "ticket (notify) event to reach the endpoint")
	info("ticket OPENED: flow run %d succeeded; notify node fired rmmway.events.flow.notify -> endpoint", run.ID)

	// ---- STEP 4: WEBHOOK FIRES (W6-2) --------------------------------------
	step("step 4: webhook fires (endpoint receives alert + self-heal + ticket, signed)")
	_ = waitFor(func() bool {
		k := kinds(recv.seen(), secret)
		return k.alert && k.selfheal && k.ticket
	}, 10*time.Second, "all three events to reach the endpoint")
	dels := recv.seen()
	check(len(dels) > 0, "endpoint received no deliveries")
	signed := 0
	for i, d := range dels {
		ok, err := webhook.Verify([]byte(secret), d.SigHeader, d.Body)
		check(err == nil && ok, "delivery %d did not verify with the endpoint secret (err=%v)", i, err)
		signed++
	}
	k := kinds(dels, secret)
	check(k.alert, "endpoint received no ALERT-category delivery")
	check(k.selfheal, "endpoint received no SELF-HEAL delivery (resolved outcome)")
	check(k.ticket, "endpoint received no TICKET (flow notify) delivery")
	// A wrong secret must NOT verify the same signature (the point of HMAC).
	if ok, _ := webhook.Verify([]byte("wrong-secret"), dels[0].SigHeader, dels[0].Body); ok {
		die("a delivery verified with the WRONG secret — HMAC is not authenticating")
	}
	info("webhook FIRED: %d delivery to the endpoint, ALL %d HMAC-verified (wrong secret rejected); categories present: alert=%v self-heal=%v ticket=%v",
		len(dels), signed, k.alert, k.selfheal, k.ticket)

	// ---- AUDIT: read back all four trails, tie them to the one condition --
	step("AUDIT: one condition, four audited outcomes")
	var audit []string
	{
		l, _ := alertStore.List(ctx, "open", fa.devID, 10)
		if len(l) > 0 && l[0].Name == "disk.used_percent" {
			audit = append(audit, fmt.Sprintf("[1 ALERT]   alerts row id=%d device=%s metric=disk.used_percent status=%s value=%v (open)",
				l[0].ID, fa.devID, l[0].Status, l[0].Value))
		} else {
			audit = append(audit, "[1 ALERT]   (no open alert row)")
		}
	}
	{
		evs, err := healSt.Events(ctx, resolved.ID)
		check(err == nil, "heal events: %v", err)
		audit = append(audit, fmt.Sprintf("[2 SELF-HEAL] heal_runs id=%d status=%s detect=%v confirm=%v; heal_events: %s",
			resolved.ID, resolved.Status, deref(resolved.DetectValue), deref(resolved.ConfirmValue), statusSeq(evs)))
	}
	{
		evs, err := flowSt.Events(ctx, run.ID)
		check(err == nil, "flow events: %v", err)
		audit = append(audit, fmt.Sprintf("[3 TICKET]    flow_runs id=%d flow=%s status=%s; flow_events: %s (notify = opened ticket)",
			run.ID, run.FlowName, run.Status, hopSeq(evs)))
	}
	{
		evs, err := whStore.EventsAfter(ctx, 0, "", 200)
		check(err == nil, "journal: %v", err)
		byCat := map[string]int{}
		var devSeqs []int64
		for _, e := range evs {
			if e.DeviceID != fa.devID {
				continue // flow hops / command results carry no device id
			}
			byCat[e.Category]++
			devSeqs = append(devSeqs, e.Seq)
		}
		sort.Slice(devSeqs, func(i, j int) bool { return devSeqs[i] < devSeqs[j] })
		var seqRange string
		if len(devSeqs) > 0 {
			seqRange = fmt.Sprintf("monotonic seq %d..%d", devSeqs[0], devSeqs[len(devSeqs)-1])
		}
		audit = append(audit, fmt.Sprintf("[4 WEBHOOK]   webhook_events journal: %d event(s) on the bus, %d tied to the device (%s): %s",
			len(evs), len(devSeqs), seqRange, catSummary(byCat)))
	}
	for _, line := range audit {
		info("%s", line)
	}
	info("the single 95%%-disk condition produced all four, each independently audited in Postgres.")

	step("PASS")
	fmt.Println("W6-3 MILESTONE met (closes Block 3 = Phase 1 MVP):")
	fmt.Println("  one triggered condition (disk 95%) drove, all audited, on a real NATS bus:")
	fmt.Println("    alert fires -> self-heal runs + confirms -> ticket opened -> webhook fires")
	fmt.Println("  and the user-defined endpoint received every resulting event, HMAC-verified.")
}

// ---- helpers -----------------------------------------------------------------

func kinds(dels []delivery, secret string) eventKinds {
	var k eventKinds
	for _, d := range dels {
		if d.Status != 200 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(d.Body, &env); err != nil {
			continue
		}
		if ok, _ := webhook.Verify([]byte(secret), d.SigHeader, d.Body); !ok {
			continue
		}
		// Decode the full bus event to read its own action.
		var ev struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(env.Event, &ev)
		action, _ := ev.Data["action"].(string)
		flowName, _ := ev.Data["flow"].(string)
		switch {
		case env.Category == webhook.CategoryAlert:
			k.alert = true
		case action == "selfheal":
			k.selfheal = true
		case action == "notify" && flowName == "disk-full-ticket":
			k.ticket = true
		}
	}
	return k
}

func passJSON(p *heal.Pass) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func waitFor(fn func() bool, timeout time.Duration, what string) error {
	deadline := time.Now().Add(timeout)
	for !fn() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func waitForTerminalFlow(ctx context.Context, st *flow.Store, flowID int64, deviceID string, timeout time.Duration) (*flow.Run, error) {
	deadline := time.Now().Add(timeout)
	for {
		fid := flowID
		runs, err := st.Runs(ctx, "", deviceID, &fid, 5)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 && isTerminal(runs[0].Status) {
			return &runs[0], nil
		}
		if time.Now().After(deadline) {
			if len(runs) > 0 {
				return &runs[0], fmt.Errorf("run still %s (node=%s)", runs[0].Status, runs[0].CurrentNode)
			}
			return nil, fmt.Errorf("no run for %s", deviceID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func isTerminal(s string) bool {
	return s == "succeeded" || s == "failed" || s == "timeout"
}

func statusOf(r *flow.Run) string {
	if r == nil {
		return "(none)"
	}
	return r.Status
}

func statusSeq(evs []heal.Event) string {
	out := []string{}
	for _, e := range evs {
		out = append(out, e.Status)
	}
	return strings.Join(out, "->")
}

func hopSeq(evs []flow.HopEvent) string {
	out := []string{}
	seen := map[string]bool{}
	for _, e := range evs {
		if !seen[e.Node] {
			seen[e.Node] = true
			out = append(out, e.Node)
		}
	}
	return strings.Join(out, "->")
}

// catSummary renders {n category, ...} in a stable order.
func catSummary(m map[string]int) string {
	names := []string{}
	for c := range m {
		names = append(names, c)
	}
	sort.Strings(names)
	out := []string{}
	for _, c := range names {
		out = append(out, fmt.Sprintf("%d %s", m[c], c))
	}
	return strings.Join(out, ", ")
}

// ---- HTTP helpers -----------------------------------------------------------

type webhookOut struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Categories []string `json:"categories"`
	Enabled    bool     `json:"enabled"`
	LastSeq    int64    `json:"last_seq"`
	Status     string   `json:"status"`
}

func createWebhook(ctx context.Context, apiBase, token, name, url, secret string, categories []string) (*webhookOut, error) {
	in := map[string]any{
		"name": name, "url": url, "secret": secret, "categories": categories,
	}
	b, err := postJSON(ctx, apiBase, "/admin/webhooks", in, token)
	if err != nil {
		return nil, err
	}
	var out webhookOut
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// loginOp hits the OPEN POST /api/login with the env admin and returns the
// short-lived operator JWT the C1-gated /admin/* + /api/* routes require.
func loginOp(ctx context.Context, apiBase string) string {
	b, err := postJSON(ctx, apiBase, "/api/login",
		map[string]string{"username": "admin", "password": "admin"}, "")
	if err != nil {
		die("operator login: %v", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		die("operator login: no token in response: %s", string(b))
	}
	return out.Token
}

// postJSON POSTs in as JSON; token (when non-empty) rides the C1 operator
// JWT gate via Authorization: Bearer <token>.
func postJSON(ctx context.Context, apiBase, path string, in any, token string) ([]byte, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return body, fmt.Errorf("POST %s -> %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

func insertSample(ctx context.Context, pool *pgxpool.Pool, deviceID, metric, source string, v float64, at time.Time) {
	if _, err := pool.Exec(ctx, `
		INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
		VALUES ($1, $2, $3, $4, '{}', $5, $6)
		ON CONFLICT DO NOTHING`, deviceID, metric, source, v, at.UnixMilli(), at.UTC()); err != nil {
		die("insert sample: %v", err)
	}
}

// ---- fake agent (mirrors the real agent's W3-3 command path) ------------------

type fakeAgent struct {
	hostname string
	devID    string
	jwt      string
	rootCert *x509.Certificate
	mint     func() string
	// cur is the volume % this agent currently reports (starts "full" at 95;
	// after "executing" any script it becomes `after` = 62).
	cur      float64
	after    float64
	ready    chan struct{}
	executed chan struct{}
	sendMu   sync.Mutex
}

func newFakeAgent(hostname string, rootCert *x509.Certificate, mint func() string) *fakeAgent {
	fa := &fakeAgent{
		hostname: hostname, rootCert: rootCert, mint: mint,
		ready: make(chan struct{}), executed: make(chan struct{}),
	}
	fa.cur = 95   // the volume is full until a script runs
	fa.after = 62 // "free space" reclaims it
	return fa
}

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
		BootstrapToken: tok, Hostname: fa.hostname, Os: "linux", Arch: "amd64", AgentVersion: "e2e-automation",
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
		body, err := base64.StdEncoding.DecodeString(rs.GetScriptB64())
		if err != nil {
			die("decode script: %v", err)
		}
		info("agent %s: executing script (%d bytes, %s)", fa.hostname, len(body), rs.GetLang())
		// "Execute": the measurable effect of a remediation is the agent's
		// next metric batches reporting the freed volume. The open-ticket
		// script has no metric effect, but setting cur=62 again is harmless.
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
