package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/users"
)

// newUsersTestServer builds the suite's standard server with the users
// store wired (gap #3) and the L8 limiter off (tested separately).
func newUsersTestServer(t *testing.T) (*Server, *store.MemoryUserStore) {
	t.Helper()
	usr := store.NewMemoryUserStore()
	rateLimit := false
	s := New(Config{
		Devices:        store.NewMemoryDeviceStore(),
		JWTSecret:      []byte("test-secret"),
		TokenLifetime:  time.Hour,
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		LoginRateLimit: &rateLimit,
		Users:          usr,
	})
	return s, usr
}

// loginFull posts {username, password, totp?} to /api/login and returns
// the status + parsed body (the mfa_required challenge arrives on a 401,
// so the body must be read on that path too).
func loginFull(t *testing.T, s *Server, user, pass, totp string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass, "totp": totp})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

// TestUnifiedLoginEmptyUsersTable: while the users table is empty the env
// bootstrap pair still works (dev mode) and mints a grandfathered-admin
// session.
func TestUnifiedLoginEmptyUsersTable(t *testing.T) {
	s, _ := newUsersTestServer(t)
	code, out := loginFull(t, s, "admin", "s3cret", "")
	if code != http.StatusOK {
		t.Fatalf("env bootstrap login: got %d %v", code, out)
	}
	if out["role"] != "admin" || out["username"] != "admin" {
		t.Fatalf("claims: got %v", out)
	}
	tok, _ := out["token"].(string)
	claims, ok := users.ParseSessionJWT([]byte("test-secret"), tok)
	if !ok || claims.Role != "admin" || claims.Username != "admin" {
		t.Fatalf("session claims: got %+v, %v", claims, ok)
	}
}

// TestUnifiedLoginUsersRow: a users account logs in with its own password
// and mints a role/username-bearing session that also parses through the
// legacy operator path (the frozen requireOperator contract).
func TestUnifiedLoginUsersRow(t *testing.T) {
	s, usr := newUsersTestServer(t)
	if _, err := usr.Create(context.Background(), "alice", "tech", "alice-pass", []string{"unassigned"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	code, out := loginFull(t, s, "ALICE", "alice-pass", "") // case-insensitive
	if code != http.StatusOK {
		t.Fatalf("users login: got %d %v", code, out)
	}
	if out["role"] != "tech" || out["username"] != "alice" {
		t.Fatalf("claims: got %v", out)
	}
	tok, _ := out["token"].(string)
	claims, ok := users.ParseSessionJWT([]byte("test-secret"), tok)
	if !ok || claims.Role != "tech" || claims.Username != "alice" || len(claims.Caps) == 0 {
		t.Fatalf("session claims: got %+v, %v", claims, ok)
	}
	// The token works on an auth-gated route (legacy parser path).
	if got := doAuthed(t, s, http.MethodGet, "/api/devices", tok); got != http.StatusOK {
		t.Fatalf("devices with session token: got %d", got)
	}
	// Wrong password → the one 401 shape.
	code, out = loginFull(t, s, "alice", "nope", "")
	if code != http.StatusUnauthorized || out["error"] != "invalid username or password" {
		t.Fatalf("wrong password: got %d %v", code, out)
	}
}

// TestUnifiedLoginMFAContract pins the mfa_required 401: enrolled + no
// code → challenge; enrolled + right code → 200; enrolled + wrong code →
// the generic 401 (never a second challenge).
func TestUnifiedLoginMFAContract(t *testing.T) {
	s, usr := newUsersTestServer(t)
	u, err := usr.Create(context.Background(), "bob", "viewer", "bob-pass", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // RFC 6238 appendix A
	if err := usr.SetTotpSecret(context.Background(), u.ID, secret); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Challenge.
	code, out := loginFull(t, s, "bob", "bob-pass", "")
	if code != http.StatusUnauthorized || out["error"] != "mfa_required" {
		t.Fatalf("challenge: got %d %v", code, out)
	}

	// Right code → 200 + the verified stamp lands.
	code, out = loginFull(t, s, "bob", "bob-pass", users.TotpCodeNow(secret))
	if code != http.StatusOK {
		t.Fatalf("mfa login: got %d %v", code, out)
	}
	got, _ := usr.Get(context.Background(), u.ID)
	if got.TotpVerified == nil {
		t.Fatal("first-verified stamp missing after TOTP login")
	}

	// Wrong code → the generic 401.
	code, out = loginFull(t, s, "bob", "bob-pass", "000000")
	if code != http.StatusUnauthorized || out["error"] != "invalid username or password" {
		t.Fatalf("wrong code: got %d %v", code, out)
	}
}

// TestUnifiedLoginDBOnlyGating: once ANY operator account exists, the env
// bootstrap pair is dead (login is DB-only) — but the account itself still
// logs in.
func TestUnifiedLoginDBOnlyGating(t *testing.T) {
	s, usr := newUsersTestServer(t)
	if _, err := usr.Create(context.Background(), "alice", "tech", "alice-pass", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	code, out := loginFull(t, s, "admin", "s3cret", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("env pair must be refused once users exist: got %d %v", code, out)
	}
	code, _ = loginFull(t, s, "alice", "alice-pass", "")
	if code != http.StatusOK {
		t.Fatalf("users login: got %d", code)
	}
}

// TestUnifiedLoginDisabledUser: a disabled account's credentials are
// refused (one 401 shape, no fallback to other tables).
func TestUnifiedLoginDisabledUser(t *testing.T) {
	s, usr := newUsersTestServer(t)
	u, err := usr.Create(context.Background(), "carol", "admin", "carol-pass", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	disabled := false
	if _, err := usr.Update(context.Background(), u.ID, nil, &disabled, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	code, out := loginFull(t, s, "carol", "carol-pass", "")
	if code != http.StatusUnauthorized || out["error"] != "invalid username or password" {
		t.Fatalf("disabled user: got %d %v", code, out)
	}
}

// TestUnifiedLoginDelegatesWhenUnwired: with no users store the route
// keeps the legacy handler (body, rate limiting, env pair) unchanged.
func TestUnifiedLoginDelegatesWhenUnwired(t *testing.T) {
	s, _ := newTestServer(t)
	code, out := login(t, s, "admin", "s3cret")
	if code != http.StatusOK || out["token"] == nil {
		t.Fatalf("legacy login through the new registration: got %d %v", code, out)
	}
	code, _ = login(t, s, "admin", "nope")
	if code != http.StatusUnauthorized {
		t.Fatalf("legacy wrong password: got %d", code)
	}
}
