package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.MemoryDeviceStore) {
	t.Helper()
	devs := store.NewMemoryDeviceStore()
	if err := devs.Register(context.Background(),
		"dev-abc", "fileserver-01", "linux", "amd64", "0.1.0", []string{"10.0.0.9"}, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	rateLimit := false // L8 limiter tested separately (TestLoginRateLimit)
	s := New(Config{
		Devices:        devs,
		JWTSecret:      []byte("test-secret"),
		TokenLifetime:  time.Hour,
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		MintBootstrap:  func() (string, string) { return "bt-test", "dev-xyz" },
		LoginRateLimit: &rateLimit,
	})
	return s, devs
}

// login performs a POST /api/login and returns the status + parsed body.
func login(t *testing.T, s *Server, user, pass string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

func doAuthed(t *testing.T, s *Server, method, path, token string) int {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestOperatorJWTRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	tok, err := ingest.MintOperatorJWT(secret, time.Hour, []string{"rmmway.run_script"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	capList, ok := ingest.ParseOperatorJWT(secret, tok)
	if !ok {
		t.Fatal("valid operator token rejected")
	}
	if len(capList) != 1 || capList[0] != "rmmway.run_script" {
		t.Fatalf("caps claim not round-tripped: %v", capList)
	}
	// A token minted without capabilities parses with an empty set
	// (secure by default: it cannot dispatch anything).
	tokNoCaps, err := ingest.MintOperatorJWT(secret, time.Hour, nil)
	if err != nil {
		t.Fatalf("mint no-caps: %v", err)
	}
	if c, ok := ingest.ParseOperatorJWT(secret, tokNoCaps); !ok || len(c) != 0 {
		t.Fatalf("no-caps token: caps=%v ok=%v, want empty+ok", c, ok)
	}
	// Wrong secret must not verify.
	if _, ok := ingest.ParseOperatorJWT([]byte("other-secret"), tok); ok {
		t.Fatal("token verified under the wrong secret")
	}
	// Expired token must not verify.
	expired, err := ingest.MintOperatorJWT(secret, -time.Minute, []string{"rmmway.reboot"})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if _, ok := ingest.ParseOperatorJWT(secret, expired); ok {
		t.Fatal("expired token accepted")
	}
	// A garbage token must not verify.
	if _, ok := ingest.ParseOperatorJWT(secret, "not-a-jwt"); ok {
		t.Fatal("malformed token accepted")
	}
}

func TestLoginSuccessAndFailures(t *testing.T) {
	s, _ := newTestServer(t)

	code, body := login(t, s, "admin", "s3cret")
	if code != http.StatusOK {
		t.Fatalf("good login: got %d, want 200", code)
	}
	if _, ok := body["token"]; !ok {
		t.Fatalf("good login: missing token: %v", body)
	}
	if body["expiry"] == "" {
		t.Fatal("good login: missing expiry")
	}

	if code, _ := login(t, s, "admin", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad password: got %d, want 401", code)
	}
	if code, _ := login(t, s, "nobody", "s3cret"); code != http.StatusUnauthorized {
		t.Fatalf("bad username: got %d, want 401", code)
	}
}

func TestDeviceListAuthGate(t *testing.T) {
	s, _ := newTestServer(t)

	// No token -> 401.
	if code := doAuthed(t, s, http.MethodGet, "/api/devices", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
	// Garbage token -> 401.
	if code := doAuthed(t, s, http.MethodGet, "/api/devices", "garbage"); code != http.StatusUnauthorized {
		t.Fatalf("garbage token: got %d, want 401", code)
	}
	// Valid operator token -> 200 with the seeded device.
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatal("no token from login")
	}

	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed devices: got %d, want 200", rec.Code)
	}
	var devs []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&devs); err != nil {
		t.Fatalf("decode devices: %v", err)
	}
	if len(devs) != 1 || devs[0]["id"] != "dev-abc" || devs[0]["hostname"] != "fileserver-01" {
		t.Fatalf("unexpected device list: %v", devs)
	}
	if devs[0]["online"] != true {
		t.Fatalf("expected online=true, got %v", devs[0]["online"])
	}
}

// TestAgentTokenRejectedOnOperatorRoute proves an agent JWT (subject = a
// device id, no issuer) is not accepted where an operator JWT is required —
// the two token kinds are not interchangeable.
func TestAgentTokenRejectedOnOperatorRoute(t *testing.T) {
	s, _ := newTestServer(t)
	// Build a genuine agent-shaped token: device id subject, no issuer.
	agentTok := makeAgentTokenForTest(t)
	code := doAuthed(t, s, http.MethodGet, "/api/devices", agentTok)
	if code != http.StatusUnauthorized {
		t.Fatalf("agent token on operator route: got %d, want 401", code)
	}
}

// TestAdminDevicesAuthGate (C1) ensures /admin/* is operator-gated like
// /api/* — the legacy "open admin surface" is gone.
func TestAdminDevicesAuthGate(t *testing.T) {
	s, _ := newTestServer(t)
	// No token -> 401.
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices", ""); code != http.StatusUnauthorized {
		t.Fatalf("admin devices, no token: got %d, want 401", code)
	}
	// Valid operator token -> 200 with the seeded device.
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatal("no token from login")
	}
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices", tok); code != http.StatusOK {
		t.Fatalf("admin devices, authed: got %d, want 200", code)
	}
}

// TestAdminBootstrapStillWorks (C1) ensures /admin/bootstrap still mints —
// now behind the operator token, like every other /admin route.
func TestAdminBootstrapStillWorks(t *testing.T) {
	s, _ := newTestServer(t)
	// No token -> 401.
	if code := doAuthed(t, s, http.MethodPost, "/admin/bootstrap", ""); code != http.StatusUnauthorized {
		t.Fatalf("bootstrap, no token: got %d, want 401", code)
	}
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap: got %d, want 200", rec.Code)
	}
	var out map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out["bootstrap_token"] != "bt-test" || out["device_id"] != "dev-xyz" {
		t.Fatalf("bootstrap output: %v", out)
	}
}

// TestLoginRateLimit (L8) proves the per-IP failure budget: loginMaxFails
// failures lock the IP out (429 + Retry-After, even for CORRECT credentials
// while locked), and a different IP is unaffected.
func TestLoginRateLimit(t *testing.T) {
	devs := store.NewMemoryDeviceStore()
	rateLimit := true
	s := New(Config{
		Devices:        devs,
		JWTSecret:      []byte("test-secret"),
		TokenLifetime:  time.Hour,
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		LoginRateLimit: &rateLimit,
	})

	// Burn the failure budget from one IP.
	doLogin := func(ip string, user, pass string) (int, http.Header) {
		mux := http.NewServeMux()
		s.Register(mux)
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if ip != "" {
			req.Header.Set("X-Forwarded-For", ip)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Header()
	}
	for i := 0; i < loginMaxFails; i++ {
		if code, _ := doLogin("10.1.1.1", "admin", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("fail #%d: got %d, want 401", i+1, code)
		}
	}
	// Locked: even correct credentials are rejected with 429 + Retry-After.
	code, hdr := doLogin("10.1.1.1", "admin", "s3cret")
	if code != http.StatusTooManyRequests {
		t.Fatalf("locked login: got %d, want 429", code)
	}
	if ra := hdr.Get("Retry-After"); ra == "" {
		t.Fatal("locked login: missing Retry-After header")
	}
	// A different IP is unaffected.
	if code, _ := doLogin("10.1.1.2", "admin", "s3cret"); code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", code)
	}
}

// TestDeviceEventsEndpoint (W6-1): GET {/api|/admin}/devices/{id}/events
// serves the device's recent indexed log events, auth-gated under /api.
func TestDeviceEventsEndpoint(t *testing.T) {
	devs := store.NewMemoryDeviceStore()
	_ = devs.Register(context.Background(), "dev-abc", "fileserver-01", "linux", "amd64", "0.1.0", []string{"10.0.0.9"}, 30, 30)
	mem := store.NewMemoryLogStore(0)
	_ = mem.Write(context.Background(), "dev-abc", &agentv1.LogBatch{Entries: []*agentv1.LogEntry{
		{Id: "e1", TimestampMs: 100, Level: "INFO", Msg: "agent ready"},
		{Id: "e2", TimestampMs: 300, Level: "WARN", Msg: "uplink stream ended"},
	}})
	s := New(Config{
		Devices:       devs,
		JWTSecret:     []byte("test-secret"),
		AdminUser:     "admin",
		AdminPassword: "s3cret",
		LogEvents: func(deviceID string, limit int, level string) ([]store.LogEvent, error) {
			return mem.Recent(context.Background(), deviceID, limit, level)
		},
	})
	// /api without a token -> 401.
	if code := doAuthed(t, s, http.MethodGet, "/api/devices/dev-abc/events", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
	// C1: /admin is operator-gated too.
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices/dev-abc/events", ""); code != http.StatusUnauthorized {
		t.Fatalf("admin events, no token: got %d, want 401", code)
	}
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatal("no token from login")
	}
	// Authed /admin: 200 with newest-first events.
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/admin/devices/dev-abc/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin events: got %d, want 200", rec.Code)
	}
	var out struct {
		DeviceID string           `json:"device_id"`
		Events   []store.LogEvent `json:"events"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.DeviceID != "dev-abc" || len(out.Events) != 2 || out.Events[0].ID != "e2" || out.Events[1].ID != "e1" {
		t.Fatalf("events = %+v, want newest-first e2,e1", out.Events)
	}
	// Level filter.
	req = httptest.NewRequest(http.MethodGet, "/admin/devices/dev-abc/events?level=warn", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	out = struct {
		DeviceID string           `json:"device_id"`
		Events   []store.LogEvent `json:"events"`
	}{}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode warn: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].ID != "e2" {
		t.Fatalf("warn filter = %+v, want exactly e2", out.Events)
	}
	// Bad level -> 400; bad limit -> 400.
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices/dev-abc/events?level=verbose", tok); code != http.StatusBadRequest {
		t.Fatalf("bad level: got %d, want 400", code)
	}
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices/dev-abc/events?limit=abc", tok); code != http.StatusBadRequest {
		t.Fatalf("bad limit: got %d, want 400", code)
	}
	// Unknown device -> 404.
	if code := doAuthed(t, s, http.MethodGet, "/admin/devices/dev-nope/events", tok); code != http.StatusNotFound {
		t.Fatalf("unknown device: got %d, want 404", code)
	}
	// Not wired -> 503.
	s2 := New(Config{Devices: devs, JWTSecret: []byte("test-secret"), AdminUser: "admin", AdminPassword: "s3cret"})
	if code := doAuthed(t, s2, http.MethodGet, "/admin/devices/dev-abc/events", tok); code != http.StatusServiceUnavailable {
		t.Fatalf("nil logEvents: got %d, want 503", code)
	}
}

// TestEventsStreamAuth (W6-2 build-out): the SSE stream + /events catch-up
// routes are operator-gated and the stream also accepts the JWT via ?token=
// (EventSource can't set an Authorization header). With the webhook
// framework unwired (in-memory server) auth is what we can assert: 401
// without a token, 401 on a bad ?token=, and 503 (auth passed, no framework)
// on a valid ?token= or header token. No Postgres needed.
func TestEventsStreamAuth(t *testing.T) {
	s, _ := newTestServer(t)
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)

	// Unwired server (no webhook framework) — auth is what's under test.
	s2 := New(Config{Devices: store.NewMemoryDeviceStore(), JWTSecret: []byte("test-secret"), AdminUser: "admin", AdminPassword: "s3cret"})
	mux2 := http.NewServeMux()
	s2.Register(mux2)
	code := func(method, path string, headerTok string) int {
		req := httptest.NewRequest(method, path, nil)
		if headerTok != "" {
			req.Header.Set("Authorization", "Bearer "+headerTok)
		}
		rec := httptest.NewRecorder()
		mux2.ServeHTTP(rec, req)
		return rec.Code
	}

	// No token -> 401 on both routes.
	if c := code(http.MethodGet, "/api/events", ""); c != http.StatusUnauthorized {
		t.Fatalf("/api/events no auth: got %d, want 401", c)
	}
	if c := code(http.MethodGet, "/api/events/stream", ""); c != http.StatusUnauthorized {
		t.Fatalf("stream no auth: got %d, want 401", c)
	}
	// ?token= accepted for EventSource: valid token passes auth (then 503,
	// unwired); garbage ?token= is 401.
	if c := code(http.MethodGet, "/api/events/stream?token="+tok, ""); c != http.StatusServiceUnavailable {
		t.Fatalf("stream ?token=valid: got %d, want 503 (auth passed)", c)
	}
	if c := code(http.MethodGet, "/api/events/stream?token=***", ""); c != http.StatusUnauthorized {
		t.Fatalf("stream ?token=garbage: got %d, want 401", c)
	}
	// Header form still honored on the stream route.
	if c := code(http.MethodGet, "/api/events/stream", tok); c != http.StatusServiceUnavailable {
		t.Fatalf("stream header token: got %d, want 503", c)
	}
	// /events catch-up is the REST twin (called via fetch with the
	// Authorization header, so it uses the standard gate, not ?token=):
	// auth-gated, GET only.
	if c := code(http.MethodGet, "/api/events", tok); c != http.StatusServiceUnavailable {
		t.Fatalf("/api/events header token: got %d, want 503", c)
	}
	if c := code(http.MethodPost, "/api/events", tok); c != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/events: got %d, want 405", c)
	}
	if c := code(http.MethodGet, "/api/events", ""); c != http.StatusUnauthorized {
		t.Fatalf("/api/events no auth: got %d, want 401", c)
	}
}

// ---- B-2: device tag editing + bulk fan-out -------------------------------

// loginToken performs POST /api/login and returns the operator token.
func loginToken(t *testing.T, s *Server) string {
	t.Helper()
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("login: no token in %v", body)
	}
	return tok
}

// doJSON issues an authed request with a JSON body and returns the status
// + parsed response.
func doJSON(t *testing.T, s *Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

// TestPatchDeviceTags (B-2): the operator replaces a device's whole tag
// list from the UI. Normalization (trim/lowercase/dedupe), validation
// (shape, limits), the unknown-device and method gates, and the
// degraded search re-index (Search=nil -> indexed=false, 200 anyway).
func TestPatchDeviceTags(t *testing.T) {
	s, _ := newTestServer(t)

	// No token -> 401.
	if code := doAuthed(t, s, http.MethodPatch, "/api/devices/dev-abc", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", code)
	}
	tok := loginToken(t, s)

	// Invalid tags -> 400 (shape).
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc", tok,
		map[string]any{"tags": []string{"bad tag!"}}); code != http.StatusBadRequest {
		t.Fatalf("invalid tag: got %d, want 400", code)
	}
	// Empty tag strings are dropped; pure-empty list clears the tags.
	code, body := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc", tok,
		map[string]any{"tags": []string{"Web", "web", " Prod-X ", "prod-x"}})
	if code != http.StatusOK {
		t.Fatalf("valid patch: got %d: %v", code, body)
	}
	dev, _ := body["device"].(map[string]any)
	tags, _ := dev["tags"].([]any)
	if len(tags) != 2 || tags[0] != "web" || tags[1] != "prod-x" {
		t.Fatalf("normalized tags = %v, want [web prod-x]", tags)
	}
	if indexed, _ := body["indexed"].(bool); indexed {
		t.Fatal("indexed=true with Search nil, want false (degraded)")
	}
	// The device list reflects the new tags.
	if code := doAuthed(t, s, http.MethodGet, "/api/devices", tok); code != http.StatusOK {
		t.Fatalf("device list: got %d", code)
	}
	// Too many tags -> 400.
	many := make([]string, 21)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc", tok,
		map[string]any{"tags": many}); code != http.StatusBadRequest {
		t.Fatalf("21 tags: got %d, want 400", code)
	}
	// Long tag -> 400.
	long := []string{strings.Repeat("a", 65)}
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc", tok,
		map[string]any{"tags": long}); code != http.StatusBadRequest {
		t.Fatalf("65-char tag: got %d, want 400", code)
	}
	// Unknown device -> 404; wrong method -> 405; clear tags -> 200 [].
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-nope", tok,
		map[string]any{"tags": []string{"web"}}); code != http.StatusNotFound {
		t.Fatalf("unknown device: got %d, want 404", code)
	}
	if code := doAuthed(t, s, http.MethodGet, "/api/devices/dev-abc", tok); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on {id}: got %d, want 405", code)
	}
	code, body = doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc", tok,
		map[string]any{"tags": []string{}})
	if code != http.StatusOK {
		t.Fatalf("clear tags: got %d", code)
	}
	dev, _ = body["device"].(map[string]any)
	tags, _ = dev["tags"].([]any)
	if len(tags) != 0 {
		t.Fatalf("cleared tags = %v, want []", tags)
	}
}

// TestBulkCommandFanOut (B-2 DoD): one command fans out to every device
// carrying a tag; offline devices are reported, not retried; unknown tags
// 404; malformed requests 400; unwired dispatch 503.
func TestBulkCommandFanOut(t *testing.T) {
	ctx := context.Background()
	devs := store.NewMemoryDeviceStore()
	for _, id := range []string{"dev-web1", "dev-web2", "dev-off", "dev-db"} {
		if err := devs.Register(ctx, id, id, "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	for _, id := range []string{"dev-web1", "dev-web2", "dev-off"} {
		if err := devs.SetTags(ctx, id, []string{"web"}); err != nil {
			t.Fatalf("set tags %s: %v", id, err)
		}
	}
	if err := devs.SetTags(ctx, "dev-db", []string{"db"}); err != nil {
		t.Fatalf("set tags dev-db: %v", err)
	}
	var mu sync.Mutex
	var got []string
	dispatch := func(deviceID string, action any) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if deviceID == "dev-off" {
			return "", fmt.Errorf("device %s not reachable", deviceID)
		}
		got = append(got, deviceID)
		return "cmd-" + deviceID, nil
	}
	rateLimit := false
	s := New(Config{
		Devices:        devs,
		JWTSecret:      []byte("test-secret"),
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		Dispatch:       dispatch,
		MintBootstrap:  func() (string, string) { return "bt", "dev-xyz" },
		LoginRateLimit: &rateLimit,
	})
	tok := loginToken(t, s)

	script := base64.StdEncoding.EncodeToString([]byte("echo b2"))
	code, body := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "run_script", "lang": "sh", "script": script,
	})
	if code != http.StatusOK {
		t.Fatalf("bulk: got %d: %v", code, body)
	}
	if n, _ := body["requested"].(float64); n != 3 {
		t.Fatalf("requested = %v, want 3", body["requested"])
	}
	pushed, _ := body["pushed"].([]any)
	if len(pushed) != 2 {
		t.Fatalf("pushed = %v, want 2 entries", pushed)
	}
	offline, _ := body["offline"].([]any)
	if len(offline) != 1 || offline[0] != "dev-off" {
		t.Fatalf("offline = %v, want [dev-off]", offline)
	}
	failed, _ := body["failed"].(map[string]any)
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want empty", failed)
	}
	mu.Lock()
	if len(got) != 2 || got[0] != "dev-web1" || got[1] != "dev-web2" {
		t.Fatalf("dispatched = %v, want [dev-web1 dev-web2] (dev-off skipped, dev-db untouched)", got)
	}
	mu.Unlock()

	// The "db" group fans out to exactly one device.
	code, body = doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "db", "action": "reboot",
	})
	if code != http.StatusOK {
		t.Fatalf("bulk db: got %d: %v", code, body)
	}
	if n, _ := body["requested"].(float64); n != 1 {
		t.Fatalf("db requested = %v, want 1", body["requested"])
	}

	// Unknown tag -> 404.
	if code, _ := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "nope", "action": "run_script", "script": script,
	}); code != http.StatusNotFound {
		t.Fatalf("unknown tag: got %d, want 404", code)
	}
	// Malformed action -> 400; missing tag -> 400; not base64 -> 400.
	if code, _ := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "reboot_hard",
	}); code != http.StatusBadRequest {
		t.Fatalf("bad action: got %d, want 400", code)
	}
	if code, _ := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"action": "reboot",
	}); code != http.StatusBadRequest {
		t.Fatalf("missing tag: got %d, want 400", code)
	}
	if code, _ := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "run_script", "script": "not-base64!!",
	}); code != http.StatusBadRequest {
		t.Fatalf("bad script: got %d, want 400", code)
	}
	// POST only.
	if code := doAuthed(t, s, http.MethodGet, "/api/devices/bulk/commands", tok); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET bulk: got %d, want 405", code)
	}
	// Unwired dispatch -> 503.
	s2 := New(Config{Devices: devs, JWTSecret: []byte("test-secret"), AdminUser: "admin", AdminPassword: "s3cret"})
	if code, _ := doJSON(t, s2, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "reboot",
	}); code != http.StatusServiceUnavailable {
		t.Fatalf("nil dispatch: got %d, want 503", code)
	}
}

// TestBulkCommandCapabilityGate (B-2 + W3-3): a session whose operator JWT
// lacks the action's capability gets 403 BEFORE any device is touched.
func TestBulkCommandCapabilityGate(t *testing.T) {
	ctx := context.Background()
	devs := store.NewMemoryDeviceStore()
	if err := devs.Register(ctx, "dev-a", "dev-a", "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
		t.Fatal(err)
	}
	if err := devs.SetTags(ctx, "dev-a", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	var called int
	rateLimit := false
	s := New(Config{
		Devices:        devs,
		JWTSecret:      []byte("test-secret"),
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		AdminCaps:      []string{caps.CapRunScript}, // session lacks rmmway.reboot
		Dispatch:       func(deviceID string, action any) (string, error) { called++; return "cmd", nil },
		MintBootstrap:  func() (string, string) { return "bt", "dev-xyz" },
		LoginRateLimit: &rateLimit,
	})
	tok := loginToken(t, s)

	// reboot is outside this session's grant -> 403, no dispatch.
	code, body := doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "reboot",
	})
	if code != http.StatusForbidden {
		t.Fatalf("reboot bulk: got %d, want 403: %v", code, body)
	}
	if err, _ := body["error"].(string); !strings.Contains(err, "rmmway.reboot") {
		t.Fatalf("403 error = %q, want capability mention", err)
	}
	// run_script is granted -> allowed through to dispatch.
	script := base64.StdEncoding.EncodeToString([]byte("echo ok"))
	code, _ = doJSON(t, s, http.MethodPost, "/api/devices/bulk/commands", tok, map[string]any{
		"tag": "web", "action": "run_script", "script": script,
	})
	if code != http.StatusOK {
		t.Fatalf("run_script bulk: got %d, want 200", code)
	}
	if called != 1 {
		t.Fatalf("dispatch called %d times, want 1 (the 403 must not touch devices)", called)
	}
}

// TestTagFilterExpr (B-2): the `tag:web` search syntax maps to an exact
// Meilisearch filter, and quote/backslash injection is neutralized.
func TestTagFilterExpr(t *testing.T) {
	if got := tagFilterExpr("web"); got != `tags = "web"` {
		t.Fatalf("tagFilterExpr(web) = %q", got)
	}
	if got := tagFilterExpr(`a"b\c`); got != `tags = "abc"` {
		t.Fatalf("injection neutralized: %q", got)
	}
}
