// Command webhook is the W6-2 definition-of-done harness: "a user-defined
// endpoint receives signed, replayable alert/inventory/automation events."
//
// It runs the WHOLE real stack the server runs, and drives all three event
// categories through their real producers onto a REAL NATS/JetStream bus:
//
//   - inventory: a fake agent ENROLLS (real gRPC ingest, real org CA) -> the
//     ingest OnDeviceEvent hook publishes a rmmway.events.device event;
//   - automation: a real flow engine runs a trigger -> notify chain on a
//     synthetic trigger -> the flow Notifier publishes a rmmway.events.
//     flow.notify event;
//   - alert: the real alert-store reconciler is driven with an anomaly -> it
//     fires a rmmway.events.alert event.
//
// The webhook framework (server/internal/webhook) subscribes to that bus,
// journals every event with a monotonic seq, and delivers HMAC-signed webhooks
// to a user-defined endpoint (a local httptest receiver). The harness then
// proves, through the REAL operator HTTP surface:
//
//  1. the endpoint received all three categories, and EVERY delivery
//     verifies its HMAC-SHA256 signature with the shared secret;
//  2. RETRY: an endpoint whose receiver 500s twice is re-driven until it
//     200s (at-least-once, the cursor only advances on a 2xx);
//  3. REPLAY: resetting an endpoint's cursor re-drives the journal range;
//  4. SSE: GET /events/stream streams the events live (text/event-stream).
//
// Usage: RMMWAY_PG_DSN=... RMMWAY_NATS_URL=... go run ./cmd/e2e/webhook
package main

import (
	"bufio"
	"context"
	"crypto/x509"
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

// flowNotifier is the W6-2 flow Notifier for the e2e: it publishes an
// automation event onto the bus (the same seam main.go wires).
type flowNotifier struct {
	pub func(subject, deviceID, message string, data map[string]any)
}

func (n flowNotifier) Notify(ctx context.Context, run *flow.Run, nodeID, reason string) {
	n.pub(flow.SubjectNotify, run.DeviceID, "flow "+run.FlowName+" node="+nodeID+": "+reason, map[string]any{
		"action": "notify", "run_id": run.ID, "flow": run.FlowName,
		"node": nodeID, "device_id": run.DeviceID, "message": reason,
	})
}

// ---- the user-defined endpoint's receiver ------------------------------------

// delivery is one received webhook (as the endpoint saw it).
type delivery struct {
	Body      []byte
	SigHeader string
	IDHeader  string
	EventHdr  string
	Status    int // the status the receiver returned
}

// recorder is a webhook receiver: it stores every delivery and, on the
// configurable "failFirst" count, returns 500 for the first N calls (to prove
// retry) and 200 after.
type recorder struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	dels      []delivery
}

func (r *recorder) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()
	st := http.StatusOK
	if n <= r.failFirst {
		st = http.StatusInternalServerError
	}
	r.mu.Lock()
	r.dels = append(r.dels, delivery{
		Body: body, SigHeader: req.Header.Get(webhook.SignHeader),
		IDHeader: req.Header.Get(webhook.IdHeader), EventHdr: req.Header.Get(webhook.EventHeader),
		Status: st,
	})
	r.mu.Unlock()
	w.WriteHeader(st)
}

func (r *recorder) seen() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery{}, r.dels...)
}
func (r *recorder) successes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dels {
		if d.Status == 200 {
			n++
		}
	}
	return n
}
func (r *recorder) failures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.dels {
		if d.Status != 200 {
			n++
		}
	}
	return n
}

// envelope is the signed payload shape the receiver decodes.
type envelope struct {
	ID       int64           `json:"id"`
	Category string          `json:"category"`
	Type     string          `json:"type"`
	DeviceID string          `json:"device_id,omitempty"`
	At       time.Time       `json:"at"`
	Event    json.RawMessage `json:"event"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	dbName := "rmmway_webhook_e2e_" + time.Now().Format("20060102150405")
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
	} else if n != 9 {
		die("expected 9 migrations, got %d", n)
	}
	info("8 migrations applied to scratch db %s", dbName)

	// ---- real NATS bus -----------------------------------------------------
	step("nats jetstream bus")
	if err := resetStream(ctx, natsURL); err != nil {
		die("reset stream: %v", err)
	}
	// Two durable consumers on ONE stream: the flow engine ("flow-engine")
	// and the webhook framework ("webhook-engine") each need to see EVERY
	// event, so they are separate consumers, not a shared one.
	bus, err := flow.NewNatsBus(ctx, natsURL, streamName, "flow-engine")
	if err != nil {
		die("nats bus (flow): %v", err)
	}
	defer bus.Close()
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

	// ---- event fan-out: alert / device / notify emitters -> the bus --------
	var flowBus = bus
	publishEvent := func(subject, deviceID, message string, data map[string]any) {
		if flowBus == nil {
			return
		}
		_ = flowBus.Publish(context.Background(), subject, &flow.Event{
			Type: subject, DeviceID: deviceID, Message: message, Data: data, At: time.Now().UTC(),
		})
	}
	// ---- alert store (with the W6-2 event sink) ----------------------------
	alertStore := store.NewAlertStore(pool, 1)
	alertStore.SetEventSink(func(action string, payload map[string]any) {
		devID, _ := payload["device_id"].(string)
		publishEvent(flow.SubjectAlert, devID, action+" alert "+fmt.Sprint(payload["name"]), payload)
	})

	// ---- real gRPC ingest (enroll + stream -> inventory events) ------------
	devices := store.NewPostgresDevices(pool)
	svc := ingest.NewService(ingest.Config{
		JWTSecret: []byte("e2e-webhook-secret"),
		OrgCA:     caMgr,
		Caps:      issuer,
		OnDeviceEvent: func(action string, payload map[string]any) {
			devID, _ := payload["device_id"].(string)
			publishEvent(flow.SubjectDevice, devID, action+" device", payload)
		},
		OnCommandResult: func(res *agentv1.CommandResult) {
			// A FINAL agent command answer is an automation event on the bus.
			publishEvent(flow.SubjectCommand, "", res.GetStatus().String(), map[string]any{
				"command_id": res.GetCommandId(), "status": res.GetStatus().String(),
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

	// ---- real flow engine (automation: trigger -> notify) ------------------
	flowEng := flow.New(flow.NewStore(pool), bus,
		func(ctx context.Context, deviceID, lang, script string) (string, error) {
			return "", fmt.Errorf("no script nodes in this e2e flow")
		},
		func(commandID string) (*agentv1.CommandResult, bool) { return nil, false },
		flowNotifier{pub: publishEvent}, 300*time.Millisecond, -1) // sampler off
	flowEng = flowEng.WithLogger(log.New(&logWriter{}, "flow: ", 0))
	if err := flowEng.Start(ctx); err != nil {
		die("flow engine start: %v", err)
	}
	info("flow engine subscribed (sweep 300ms, sampler off)")

	// ---- webhook framework (journal + signed delivery + SSE) ---------------
	whStore := webhook.NewStore(pool)
	whSvc := webhook.New(whStore, whBus).WithLogger(log.New(&logWriter{}, "webhook: ", 0)).WithSweepInterval(300 * time.Millisecond)
	if err := whSvc.Start(ctx); err != nil {
		die("webhook start: %v", err)
	}
	info("webhook framework subscribed (sweep 300ms)")

	// ---- operator HTTP surface (the real httpapi) --------------------------
	apiSrv := httpapi.New(httpapi.Config{
		Devices:   devices,
		JWTSecret: []byte("e2e-webhook-secret"),
		AdminUser: "admin", AdminPassword: "admin",
		Webhooks: whSvc,
	})
	mux := http.NewServeMux()
	apiSrv.Register(mux)
	apiSrv2 := httptest.NewServer(mux)
	defer apiSrv2.Close()
	apiBase := apiSrv2.URL
	info("operator HTTP surface on %s (/admin/webhooks, /admin/events/stream)", apiBase)

	// ---- the user-defined endpoint receivers -------------------------------
	recvA := &recorder{}             // always 200; all categories
	recvB := &recorder{failFirst: 2} // 500 twice, then 200; alerts only
	srvA := httptest.NewServer(http.HandlerFunc(recvA.serve))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(recvB.serve))
	defer srvB.Close()
	const secretA = "shared-secret-A"
	const secretB = "shared-secret-B"

	step("create user-defined endpoints (via the real HTTP surface)")
	epA, err := createWebhook(ctx, apiBase, "alerts+inventory+automation", srvA.URL, secretA, []string{
		webhook.CategoryAlert, webhook.CategoryInventory, webhook.CategoryAutomation,
	})
	if err != nil {
		die("create endpoint A: %v", err)
	}
	epB, err := createWebhook(ctx, apiBase, "retrying-alerts", srvB.URL, secretB, []string{webhook.CategoryAlert})
	if err != nil {
		die("create endpoint B: %v", err)
	}
	info("endpoint A id=%d url=%s (all categories)", epA.ID, srvA.URL)
	info("endpoint B id=%d url=%s (alerts only; receiver 500s twice)", epB.ID, srvB.URL)

	// ---- generate a REAL event of each category ----------------------------
	step("fire a real inventory event (enroll a device)")
	fa := newFakeAgent("webhook-host", rootCert, func() string { tok, _ := svc.MintBootstrapToken(); return tok })
	if err := fa.enroll(ctx, lis.Addr().String()); err != nil {
		die("enroll: %v", err)
	}
	info("%s enrolled as %s (inventory 'created')", fa.hostname, fa.devID)
	agentStreamCtx, agentStreamCancel := context.WithCancel(ctx)
	defer agentStreamCancel()
	go func() { _ = fa.run(agentStreamCtx, lis.Addr().String()) }()
	<-fa.ready
	info("%s stream live (inventory 'online')", fa.hostname)

	step("fire a real automation event (flow trigger -> notify)")
	flowDef, err := flowEng.Store().CreateFlow(ctx, "notify-me", "W6-2 DoD automation", flow.Graph{Nodes: []flow.Node{
		{ID: "t", Kind: flow.KindTrigger, Name: "disk > 50%", Metric: "disk.used_percent", Op: ">", Threshold: 50, Next: "n"},
		{ID: "n", Kind: flow.KindNotify, Name: "notify", Message: "disk is full — automation notify"},
	}}, 0, true)
	if err != nil {
		die("create flow: %v", err)
	}
	v := 95.0
	if err := flowEng.Trigger(ctx, flowDef.ID, fa.devID, "", &v); err != nil {
		die("trigger: %v", err)
	}
	info("synthetic trigger published (disk=95) -> flow %d will notify", flowDef.ID)

	step("fire a real alert event (baseline anomaly -> reconciler)")
	// A fresh series that spikes far from its (single-point) baseline -> the
	// reconciler flags it and fires an alert event through the sink.
	seeks := []baseline.Anomaly{{
		At: time.Now().UTC(), DeviceID: fa.devID, Name: "cpu.utilization_percent", Source: "",
		Value: 99, Score: 6.0,
		Seasonal: &baseline.CellScore{Z: 6.0, Median: 20, MAD: 1, EWMA: 20, Cells: 40},
	}}
	alertStore.Reconcile(seeks, map[baseline.SeriesKey]bool{{DeviceID: fa.devID, Name: "cpu.utilization_percent", Source: ""}: true})
	info("reconciler driven with a CPU anomaly for %s (alert 'fired')", fa.devID)

	// ---- wait for deliveries (the 300ms sweep drives them) -----------------
	step("await signed deliveries to both endpoints")
	deadline := time.Now().Add(40 * time.Second)
	for {
		// A should have at least one of each category; B should have its
		// (retried) alert delivery.
		if aCats := categoriesReceived(recvA.seen(), secretA); aCats["alert"] && aCats["inventory"] && aCats["automation"] && recvB.successes() >= 1 {
			break
		}
		if time.Now().After(deadline) {
			die("timed out; A categories=%v B successes=%d failures=%d",
				categoriesReceived(recvA.seen(), secretA), recvB.successes(), recvB.failures())
		}
		time.Sleep(150 * time.Millisecond)
	}

	// ---- assert 1: all three categories, every delivery signed ------------
	step("assert: 3 categories received + every delivery HMAC-verifies")
	delA := recvA.seen()
	check(len(delA) > 0, "endpoint A received no deliveries")
	cats := categoriesReceived(delA, secretA)
	check(cats["inventory"], "endpoint A received no inventory event")
	check(cats["automation"], "endpoint A received no automation event")
	check(cats["alert"], "endpoint A received no alert event")
	info("endpoint A received %d deliveries; categories present: inventory=%v automation=%v alert=%v",
		len(delA), cats["inventory"], cats["automation"], cats["alert"])
	signedOK := 0
	for i, d := range delA {
		if d.Status != 200 {
			continue
		}
		ok, err := webhook.Verify([]byte(secretA), d.SigHeader, d.Body)
		check(err == nil && ok, "delivery %d did not verify with secret A (err=%v)", i, err)
		signedOK++
	}
	check(signedOK >= 3, "expected >=3 signed+verified deliveries on A, got %d", signedOK)
	// A wrong secret must NOT verify the same signature (the point of HMAC).
	d0 := delA[0]
	if ok, _ := webhook.Verify([]byte("wrong-secret"), d0.SigHeader, d0.Body); ok {
		die("delivery verified with the WRONG secret — HMAC is not authenticating")
	}
	info("all %d successful deliveries verified with secret A; wrong secret rejected", signedOK)

	// ---- assert 2: retry (endpoint B 500ed twice, then delivered) ---------
	step("assert: retry (endpoint B failed twice, then delivered on a 2xx)")
	check(recvB.failures() >= 2, "endpoint B failed only %d time(s), want >=2 (retry not exercised)", recvB.failures())
	check(recvB.successes() >= 1, "endpoint B never delivered a 200 after retrying")
	// The successful delivery on B must be a valid, signed alert.
	var bAlertOK bool
	for _, d := range recvB.seen() {
		if d.Status != 200 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(d.Body, &env); err != nil {
			continue
		}
		if env.Category == webhook.CategoryAlert {
			if ok, _ := webhook.Verify([]byte(secretB), d.SigHeader, d.Body); ok {
				bAlertOK = true
			}
		}
	}
	check(bAlertOK, "endpoint B's retried delivery was not a signed alert event")
	info("endpoint B: %d failure(s) then a signed alert delivery (at-least-once retry proven)", recvB.failures())

	// ---- assert 3: replay (reset the cursor, re-drive the range) -----------
	step("assert: replay (re-drive the journal from an earlier seq)")
	beforeA := recvA.successes()
	var firstEnv envelope
	if err := json.Unmarshal(delA[0].Body, &firstEnv); err != nil || firstEnv.ID == 0 {
		die("could not read the first delivered event id: %v", err)
	}
	firstID := firstEnv.ID
	// Replay from before the first event -> the endpoint re-receives events.
	body := map[string]int64{"from_seq": firstID - 1}
	if _, err := postJSON(ctx, apiBase, fmt.Sprintf("/admin/webhooks/%d/replay", epA.ID), body); err != nil {
		die("replay: %v", err)
	}
	info("replay requested from seq %d", firstID-1)
	rdeadline := time.Now().Add(15 * time.Second)
	for {
		if recvA.successes() > beforeA {
			break
		}
		if time.Now().After(rdeadline) {
			die("replay did not re-drive deliveries (before=%d after=%d)", beforeA, recvA.successes())
		}
		time.Sleep(150 * time.Millisecond)
	}
	info("replay re-delivered %d event(s) to endpoint A", recvA.successes()-beforeA)

	// ---- assert 4: SSE live stream -----------------------------------------
	step("assert: SSE event stream (GET /events/stream)")
	frames, err := readSSE(ctx, apiBase+"/admin/events/stream", 3)
	check(err == nil, "read SSE: %v", err)
	check(len(frames) >= 1, "SSE stream delivered no frames")
	var env envelope
	ok := false
	for _, f := range frames {
		if err := json.Unmarshal(f, &env); err == nil && env.Category != "" && env.ID > 0 {
			ok = true
			break
		}
	}
	check(ok, "no SSE frame parsed as a valid event envelope")
	info("SSE stream delivered %d frame(s); sample: id=%d category=%s", len(frames), env.ID, env.Category)

	// ---- cleanup the flow so the DB drop is clean --------------------------
	_ = flowEng.Store().DeleteFlow(ctx, flowDef.ID)

	step("PASS")
	fmt.Println("W6-2 DoD met: a user-defined endpoint received signed, replayable")
	fmt.Println("alert + inventory + automation events (HMAC verified), with retries and")
	fmt.Println("replay, and the same bus is exposed as a live SSE stream.")
}

// categoriesReceived reports which categories the endpoint received among
// its 200 deliveries, verifying each signature along the way.
func categoriesReceived(dels []delivery, secret string) map[string]bool {
	out := map[string]bool{}
	for _, d := range dels {
		if d.Status != 200 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(d.Body, &env); err != nil {
			continue
		}
		if ok, _ := webhook.Verify([]byte(secret), d.SigHeader, d.Body); ok {
			out[env.Category] = true
		}
	}
	return out
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

func createWebhook(ctx context.Context, apiBase, name, url, secret string, categories []string) (*webhookOut, error) {
	in := map[string]any{
		"name": name, "url": url, "secret": secret, "categories": categories,
	}
	b, err := postJSON(ctx, apiBase, "/admin/webhooks", in)
	if err != nil {
		return nil, err
	}
	var out webhookOut
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func postJSON(ctx context.Context, apiBase, path string, in any) ([]byte, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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

// readSSE opens the event stream and returns up to n decoded data-payloads
// (each an Envelope JSON), honoring Last-Event-ID-free catch-up.
func readSSE(ctx context.Context, streamURL string, n int) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stream -> %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		return nil, fmt.Errorf("unexpected content-type %q", ct)
	}
	var out []json.RawMessage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-deadline:
			if len(out) > 0 {
				return out, nil
			}
			return out, fmt.Errorf("timed out reading SSE frames")
		default:
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return out, err
			}
			// Stream ended cleanly.
			return out, nil
		}
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			out = append(out, json.RawMessage(strings.TrimPrefix(line, "data: ")))
		}
	}
	return out, nil
}

// ---- fake agent (enroll + stream -> inventory events) ------------------------

type fakeAgent struct {
	hostname string
	devID    string
	jwt      string
	rootCert *x509.Certificate
	mint     func() string
	ready    chan struct{}
}

func newFakeAgent(hostname string, rootCert *x509.Certificate, mint func() string) *fakeAgent {
	return &fakeAgent{hostname: hostname, rootCert: rootCert, mint: mint, ready: make(chan struct{})}
}

func (fa *fakeAgent) enroll(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)
	tok := fa.mint()
	resp, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: tok, Hostname: fa.hostname, Os: "linux", Arch: "amd64", AgentVersion: "e2e-webhook",
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
	// Keep the stream up (heartbeats) for the life of the e2e.
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := stream.Send(&agentv1.StreamRequest{
				Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
			}); err != nil {
				return err
			}
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}
}

func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
