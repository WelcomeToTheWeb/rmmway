package webhook

// Tests for the W6-2 webhook + event-stream framework. Pure tests (HMAC,
// category, envelope, Deliver) run always; the journal/sweep/cursor/replay/
// retry tests are live-Postgres and skip when RMMWAY_TEST_PG_DSN is not
// reachable (same convention as the store/heal/flow live tests). The bus is
// the in-process memBus so no NATS broker is needed.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- scratch database -------------------------------------------------------

func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping webhook Postgres test")
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
	dbName := "rmmway_wh_test_" + time.Now().Format("20060102150405") + "_" + hex.EncodeToString(rnd)
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

// ---- pure: Deliver builds a signed request ----------------------------------

func TestDeliverSignsRequest(t *testing.T) {
	const secret = "receiver-secret"
	var gotBody []byte
	var gotSig, gotID, gotEvent, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(SignHeader)
		gotID = r.Header.Get(IdHeader)
		gotEvent = r.Header.Get(EventHeader)
		gotTS = r.Header.Get(TimestampHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := &Service{client: srv.Client()}
	ev := Event{Seq: 123, Category: CategoryAlert, Type: "rmmway.events.alert",
		DeviceID: "dev-9", At: time.Now().UTC(), Data: []byte(`{"action":"fired"}`)}
	ok, err := svc.Deliver(context.Background(), Endpoint{URL: srv.URL, Secret: secret, TimeoutMS: 5000}, ev)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !ok {
		t.Fatalf("expected 200 to count as delivered")
	}
	if gotID != "123" || gotEvent != "rmmway.events.alert" || gotTS == "" {
		t.Fatalf("bad headers: id=%q event=%q ts=%q", gotID, gotEvent, gotTS)
	}
	okv, verr := Verify([]byte(secret), gotSig, gotBody)
	if verr != nil {
		t.Fatalf("verify: %v", verr)
	}
	if !okv {
		t.Fatalf("delivered body did not verify with the shared secret")
	}
	var env Envelope
	if err := json.Unmarshal(gotBody, &env); err != nil {
		t.Fatalf("body is not a JSON envelope: %v", err)
	}
	if env.ID != 123 || env.Category != CategoryAlert || env.DeviceID != "dev-9" {
		t.Fatalf("bad envelope: %+v", env)
	}
}

func TestDeliverNon2xxIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	svc := &Service{client: srv.Client()}
	ok, err := svc.Deliver(context.Background(), Endpoint{URL: srv.URL, Secret: "s", TimeoutMS: 5000},
		Event{Seq: 1, Category: CategoryAlert, At: time.Now(), Data: []byte(`{}`)})
	if err == nil || ok {
		t.Fatalf("expected a 500 to be a failure, got ok=%v err=%v", ok, err)
	}
}

func TestDeliverWrongSecretRejectedByReceiver(t *testing.T) {
	// A receiver that verifies the signature and 401s on mismatch.
	var lastSig string
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		lastSig = r.Header.Get(SignHeader)
		if ok, _ := Verify([]byte("correct-horse"), lastSig, lastBody); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	svc := &Service{client: srv.Client()}
	ev := Event{Seq: 5, Category: CategoryInventory, At: time.Now(), Data: []byte(`{}`)}
	// Wrong shared secret -> receiver 401s -> delivery fails.
	if ok, _ := svc.Deliver(context.Background(), Endpoint{URL: srv.URL, Secret: "WRONG", TimeoutMS: 5000}, ev); ok {
		t.Fatalf("delivery with the wrong secret should be rejected (401)")
	}
	// Correct secret -> 200 -> delivered.
	if ok, _ := svc.Deliver(context.Background(), Endpoint{URL: srv.URL, Secret: "correct-horse", TimeoutMS: 5000}, ev); !ok {
		t.Fatalf("delivery with the right secret should succeed")
	}
}

// ---- pure: live SSE fan-out (memBus) -----------------------------------------

func TestLiveFanoutByCategory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := flow.NewMemBus()
	svc := New(nil, bus)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	all, cancelAll := svc.AddLive(ctx, "")
	alerts, cancelAlerts := svc.AddLive(ctx, CategoryAlert)
	defer cancelAll()
	defer cancelAlerts()

	at := time.Now().UTC()
	// An alert event: both the "all" and "alerts" subscribers should get it.
	_ = bus.Publish(ctx, "rmmway.events.alert", &flow.Event{
		Type: "rmmway.events.alert", DeviceID: "dev-1", Message: "fired",
		Data: map[string]any{"action": "fired"}, At: at,
	})
	// An automation event: only the "all" subscriber should get it.
	_ = bus.Publish(ctx, "rmmway.events.flow.notify", &flow.Event{
		Type: "rmmway.events.flow.notify", DeviceID: "dev-1", Message: "n",
		Data: map[string]any{"action": "notify"}, At: at,
	})

	expect := func(ch <-chan Event, n int, label string) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case ev := <-ch:
				_ = ev
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out expecting an event on %s", label)
			}
		}
	}
	expect(all, 2, "all")
	expect(alerts, 1, "alerts")

	// The alerts subscriber should NOT receive the automation event.
	select {
	case ev := <-alerts:
		t.Fatalf("alerts subscriber got an unexpected event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLiveFanoutByDeviceAndType proves the structured Filter (category +
// device + type) routes each event only to subscribers whose set fields all
// match — the "monitors/alerts per device" primitive behind the SSE route.
func TestLiveFanoutByDeviceAndType(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := flow.NewMemBus()
	svc := New(nil, bus)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	all, cancelAll := svc.AddLiveFilter(ctx, Filter{})
	dev1, cancelDev1 := svc.AddLiveFilter(ctx, Filter{Device: "dev-1"})
	dev1Alerts, cancelDev1A := svc.AddLiveFilter(ctx, Filter{Device: "dev-1", Category: CategoryAlert})
	dev2Device, cancelDev2D := svc.AddLiveFilter(ctx, Filter{Device: "dev-2", Type: "rmmway.events.device"})
	defer func() { cancelAll(); cancelDev1(); cancelDev1A(); cancelDev2D() }()

	at := time.Now().UTC()
	pub := func(subject, dev string) {
		_ = bus.Publish(ctx, subject, &flow.Event{
			Type: subject, DeviceID: dev, At: at, Data: map[string]any{"action": "x"},
		})
	}
	pub("rmmway.events.alert", "dev-1")  // alert on dev-1
	pub("rmmway.events.device", "dev-2") // device/online on dev-2
	pub("rmmway.events.alert", "dev-3")  // alert on dev-3

	expect := func(ch <-chan Event, n int, label string) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case ev := <-ch:
				_ = ev
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out expecting an event on %s", label)
			}
		}
	}
	noMore := func(ch <-chan Event, label string) {
		t.Helper()
		select {
		case ev := <-ch:
			t.Fatalf("%s got an unexpected extra event: %+v", label, ev)
		case <-time.After(200 * time.Millisecond):
		}
	}

	expect(all, 3, "all")
	expect(dev1, 1, "dev-1 (device filter)")
	expect(dev1Alerts, 1, "dev-1 + alert (device+category)")
	expect(dev2Device, 1, "dev-2 + device type (device+type)")

	// dev-3's alert must not leak to the dev-1 subscribers.
	noMore(dev1, "dev-1")
	noMore(dev1Alerts, "dev-1+alert")
}

// ---- live-Postgres: journal -> sweep -> cursor --------------------------------

// testHarness wires a scratch DB + memBus + service.
type harness struct {
	db  *pgxpool.Pool
	bus flow.Bus
	svc *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := scratchPool(t)
	bus := flow.NewMemBus()
	svc := New(NewStore(db), bus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return &harness{db: db, bus: bus, svc: svc}
}

func publishTestEvent(t *testing.T, h *harness, subject, deviceID, action string) {
	t.Helper()
	at := time.Now().UTC()
	err := h.bus.Publish(context.Background(), subject, &flow.Event{
		Type: subject, DeviceID: deviceID, Message: action,
		Data: map[string]any{"action": action}, At: at,
	})
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
}

// sweepN clears every endpoint's backoff watermark before EACH sweep, then
// runs it (so a test doesn't wait real backoff, and each sweep is eligible).
// It is idempotent and pacing-only.
func sweepN(t *testing.T, h *harness, n int) {
	t.Helper()
	st := h.svc.Store()
	ps, _ := st.ListEndpoints(context.Background())
	clear := func() {
		for i := range ps {
			_ = st.SetNextRetry(context.Background(), ps[i].ID, time.Now().Add(-time.Second))
		}
	}
	for i := 0; i < n; i++ {
		clear()
		h.svc.Sweep(context.Background(), time.Now())
	}
}

func TestJournalAndDelivery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var got []Envelope
	var sigs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env Envelope
		_ = json.Unmarshal(body, &env)
		mu.Lock()
		got = append(got, env)
		sigs = append(sigs, r.Header.Get(SignHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := h.svc.Store()
	const secret = "s1"
	// Subscribe only to alerts.
	ep, err := st.CreateEndpoint(ctx, "alerts-only", srv.URL, secret, []string{CategoryAlert}, 5, 5000, 0)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Two alert events + one automation event (the endpoint must not get it).
	publishTestEvent(t, h, "rmmway.events.alert", "dev-1", "fired")
	publishTestEvent(t, h, "rmmway.events.alert", "dev-1", "fired")
	publishTestEvent(t, h, "rmmway.events.flow.notify", "dev-1", "notify")

	// Deliver one event per sweep.
	sweepN(t, h, 4)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("endpoint received %d events, want 2 (2 alerts, not the automation): %+v", len(got), got)
	}
	if got[0].Category != CategoryAlert || got[1].Category != CategoryAlert {
		t.Fatalf("bad categories: %+v", got)
	}
	// Both deliveries must verify with the shared secret.
	for i, sig := range sigs {
		if ok, _ := Verify([]byte(secret), sig, mustMarshal(t, got[i])); !ok {
			t.Fatalf("delivery %d did not verify", i)
		}
	}
	// The cursor advanced to the last delivered seq.
	fresh, err := st.Endpoint(ctx, ep.ID)
	if err != nil {
		t.Fatalf("fetch endpoint: %v", err)
	}
	if fresh.LastSeq != got[1].ID {
		t.Fatalf("cursor = %d, want last delivered seq %d", fresh.LastSeq, got[1].ID)
	}
	// The journal holds all three (automation was journaled, just not delivered).
	if mx, _ := st.MaxSeq(ctx); mx != got[1].ID+1 {
		t.Fatalf("max seq = %d, want %d (3 events journaled)", mx, got[1].ID+1)
	}
}

func TestRetryAndDeadLetter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway) // always fail
	}))
	defer srv.Close()

	st := h.svc.Store()
	ep, err := st.CreateEndpoint(ctx, "flaky", srv.URL, "s", []string{CategoryAlert}, 3, 5000, 0)
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	publishTestEvent(t, h, "rmmway.events.alert", "dev-1", "fired")

	// Each sweep retries one event; the backoff is cleared by sweepN so the
	// attempt counter accumulates across sweeps until max_attempts (3) ->
	// status "failing".
	sweepN(t, h, 5)

	fresh, _ := st.Endpoint(ctx, ep.ID)
	if fresh.LastSeq != 0 {
		t.Fatalf("no event should be delivered (all fail); cursor advanced to %d", fresh.LastSeq)
	}
	if fresh.Status != "failing" {
		t.Fatalf("status = %q, want failing after max attempts", fresh.Status)
	}
	if fresh.Attempts < 3 {
		t.Fatalf("attempts = %d, want >= 3", fresh.Attempts)
	}
	mu.Lock()
	seen := attempts
	mu.Unlock()
	if seen < 3 {
		t.Fatalf("server saw %d attempts, want >= 3", seen)
	}
	// A failing endpoint is skipped by the sweep (no further delivery).
	sweepN(t, h, 1)
	mu.Lock()
	seen2 := attempts
	mu.Unlock()
	if seen2 != seen {
		t.Fatalf("failing endpoint was still delivered (attempts %d -> %d)", seen, seen2)
	}
}

func TestReplayRedrives(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	var delivered []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env Envelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		mu.Lock()
		delivered = append(delivered, env.ID)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := h.svc.Store()
	ep, _ := st.CreateEndpoint(ctx, "ep", srv.URL, "s", []string{}, 5, 5000, 0) // all categories

	// Journal 3 events and deliver them all.
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a1")
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a2")
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a3")
	for i := 0; i < 3; i++ {
		sweepN(t, h, 1)
	}
	mu.Lock()
	firstThree := append([]int64{}, delivered...)
	mu.Unlock()
	if len(firstThree) != 3 {
		t.Fatalf("expected 3 deliveries, got %v", firstThree)
	}

	// Replay from the first seq: the endpoint should re-receive all 3.
	_ = st.SetCursor(ctx, ep.ID, firstThree[0]-1)
	for i := 0; i < 3; i++ {
		sweepN(t, h, 1)
	}
	mu.Lock()
	total := append([]int64{}, delivered...)
	mu.Unlock()
	if len(total) != 6 {
		t.Fatalf("after replay, expected 6 total deliveries, got %d: %v", len(total), total)
	}
	// The replayed half must equal the original (same seqs, same order).
	if len(total) >= 6 && total[3] != firstThree[0] {
		t.Fatalf("replay did not re-drive from the start: %v", total)
	}
}

func TestNewEndpointStartsAtCurrentMax(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Journal 2 events BEFORE the endpoint exists.
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a1")
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a2")
	st := h.svc.Store()
	if mx, _ := st.MaxSeq(ctx); mx != 2 {
		t.Fatalf("precondition: max seq = %d, want 2", mx)
	}
	ep, err := st.CreateEndpoint(ctx, "late", "http://x", "s", []string{CategoryAlert}, 5, 5000, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ep.LastSeq != 2 {
		t.Fatalf("new endpoint cursor = %d, want current max (2) — only NEW events", ep.LastSeq)
	}
	// A new event is pending; the old two are not.
	pend, err := st.PendingEvents(ctx, ep.ID, ep.LastSeq, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("no events should be pending for a fresh endpoint, got %d", len(pend))
	}
	publishTestEvent(t, h, "rmmway.events.alert", "d", "a3")
	pend, _ = st.PendingEvents(ctx, ep.ID, ep.LastSeq, 10)
	if len(pend) != 1 {
		t.Fatalf("one new event should be pending, got %d", len(pend))
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
