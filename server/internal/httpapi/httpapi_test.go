package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	s := New(Config{
		Devices:       devs,
		JWTSecret:     []byte("test-secret"),
		TokenLifetime: time.Hour,
		AdminUser:     "admin",
		AdminPassword: "s3cret",
		MintBootstrap: func() (string, string) { return "bt-test", "dev-xyz" },
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
	tok, err := ingest.MintOperatorJWT(secret, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !ingest.ParseOperatorJWT(secret, tok) {
		t.Fatal("valid operator token rejected")
	}
	// Wrong secret must not verify.
	if ingest.ParseOperatorJWT([]byte("other-secret"), tok) {
		t.Fatal("token verified under the wrong secret")
	}
	// Expired token must not verify.
	expired, err := ingest.MintOperatorJWT(secret, -time.Minute)
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if ingest.ParseOperatorJWT(secret, expired) {
		t.Fatal("expired token accepted")
	}
	// A garbage token must not verify.
	if ingest.ParseOperatorJWT(secret, "not-a-jwt") {
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

// TestAdminDevicesOpen ensures the legacy /admin/devices stays open (no auth)
// so the installer / e2e / README workflows keep working.
func TestAdminDevicesOpen(t *testing.T) {
	s, _ := newTestServer(t)
	code := doAuthed(t, s, http.MethodGet, "/admin/devices", "")
	if code != http.StatusOK {
		t.Fatalf("open admin devices: got %d, want 200", code)
	}
}

// TestAdminBootstrapStillWorks ensures /admin/bootstrap still mints.
func TestAdminBootstrapStillWorks(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", strings.NewReader("{}"))
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
