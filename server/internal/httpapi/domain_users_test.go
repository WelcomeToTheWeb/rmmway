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
		Clients:        store.NewMemoryClientStore(),
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

// ---- users API + RBAC scoping (gap #3) --------------------------------------

// doUsers performs an admin-authed request against the users surface.
func doUsers(t *testing.T, s *Server, method, path string, body any) (int, any) {
	t.Helper()
	// newUsersTestServer has no users row for "admin" yet — mint a session
	// the same way login does (admin claim) instead of fighting the
	// unified login's DB-only gate in these tests.
	tok, err := users.MintSessionJWT([]byte("test-secret"), time.Hour, users.SessionClaims{Role: "admin", Username: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

// doUsersAs performs the request with a role-bearing session (scoping
// tests). The users store must contain the account for non-admins (grants
// load live).
func doUsersAs(t *testing.T, s *Server, usr *store.MemoryUserStore, username, role string, grants []string, method, path string, body any) (int, any) {
	t.Helper()
	if role != "admin" {
		if _, err := usr.GetByUsername(context.Background(), username); err != nil {
			if _, err := usr.Create(context.Background(), username, role, "pw-12345", grants); err != nil {
				t.Fatalf("create %s: %v", username, err)
			}
		}
	}
	tok, err := users.MintSessionJWT([]byte("test-secret"), time.Hour, users.SessionClaims{Role: role, Username: username})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

// asMap asserts a decoded JSON object body (the list endpoints return
// arrays, which the do* helpers hand back as []any).
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON object, got %T", v)
	}
	return m
}

// TestUsersAPILifecycle exercises the full admin-only account surface:
// create → list → patch role/grants → password → TOTP start/confirm →
// tokens create/list/revoke, plus the admin-only gate for a tech session.
func TestUsersAPILifecycle(t *testing.T) {
	s, usr := newUsersTestServer(t)

	// Create.
	code, out := doUsers(t, s, http.MethodPost, "/api/users", map[string]any{
		"username": "alice", "password": "alice-pass-1", "role": "tech", "clients": []string{"acme"},
	})
	if code != http.StatusCreated || asMap(t, out)["username"] != "alice" || asMap(t, out)["role"] != "tech" {
		t.Fatalf("create: got %d %v", code, out)
	}
	// Short password / bad role / duplicate.
	if code, _ := doUsers(t, s, http.MethodPost, "/api/users", map[string]any{"username": "x", "password": "short", "role": "tech"}); code != http.StatusBadRequest {
		t.Fatalf("short password: got %d", code)
	}
	if code, _ := doUsers(t, s, http.MethodPost, "/api/users", map[string]any{"username": "y", "password": "longenough", "role": "boss"}); code != http.StatusBadRequest {
		t.Fatalf("bad role: got %d", code)
	}
	if code, _ := doUsers(t, s, http.MethodPost, "/api/users", map[string]any{"username": "ALICE", "password": "longenough", "role": "tech"}); code != http.StatusConflict {
		t.Fatalf("duplicate (case-insensitive): got %d", code)
	}

	// List (one account, grants filled, no salt/hash keys).
	code, list := doUsers(t, s, http.MethodGet, "/api/users", nil)
	if code != http.StatusOK {
		t.Fatalf("list: got %d %v", code, list)
	}
	rows, _ := list.([]any)
	if len(rows) != 1 {
		t.Fatalf("list rows: got %d", len(rows))
	}

	// The id for patch (re-create-free: list order is by username).
	id, _ := rows[0].(map[string]any)["id"].(string)

	// Patch: role → viewer + add a grant (stays enabled — later checks
	// use alice's live session, which loads the account row).
	enabled := true
	code, _ = doUsers(t, s, http.MethodPatch, "/api/users/"+id, map[string]any{
		"role": "viewer", "clients": []string{"acme", "globex"}, "enabled": &enabled,
	})
	if code != http.StatusOK {
		t.Fatalf("patch: got %d", code)
	}
	got, _ := usr.Get(context.Background(), id)
	if got.Role != "viewer" || len(got.Clients) != 2 {
		t.Fatalf("patch applied: got %+v", got)
	}

	// TOTP start → confirm (wrong code first, then the live code).
	code, out = doUsers(t, s, http.MethodPost, "/api/users/"+id+"/totp/start", nil)
	if code != http.StatusOK {
		t.Fatalf("totp start: got %d %v", code, out)
	}
	secret, _ := asMap(t, out)["secret"].(string)
	if code, _ = doUsers(t, s, http.MethodPost, "/api/users/"+id+"/totp/confirm", map[string]any{"code": "000000"}); code != http.StatusUnauthorized {
		t.Fatalf("totp wrong code: got %d", code)
	}
	if code, _ = doUsers(t, s, http.MethodPost, "/api/users/"+id+"/totp/confirm", map[string]any{"code": users.TotpCodeNow(secret)}); code != http.StatusOK {
		t.Fatalf("totp confirm: got %d", code)
	}
	got, _ = usr.Get(context.Background(), id)
	if got.TotpVerified == nil {
		t.Fatal("totp verified stamp missing")
	}
	// Settings now reports the enrollment for that account's session.
	if code, _ := doUsersAs(t, s, usr, "alice", "viewer", []string{"acme", "globex"}, http.MethodGet, "/api/settings", nil); code != http.StatusOK {
		t.Fatalf("settings as alice: got %d", code)
	}

	// Tokens: create (shown once) → list (no full token) → revoke.
	code, out = doUsers(t, s, http.MethodPost, "/api/users/"+id+"/tokens", map[string]any{"name": "ci", "ttl_days": 7})
	if code != http.StatusCreated || asMap(t, out)["token"] == nil || asMap(t, out)["prefix"] == nil {
		t.Fatalf("token create: got %d %v", code, out)
	}
	fullTok, _ := asMap(t, out)["token"].(string)
	code, list = doUsers(t, s, http.MethodGet, "/api/users/"+id+"/tokens", nil)
	if code != http.StatusOK {
		t.Fatalf("token list: got %d", code)
	}
	trows, _ := list.([]any)
	if len(trows) != 1 {
		t.Fatalf("token list rows: got %d", len(trows))
	}
	tokID, _ := trows[0].(map[string]any)["id"].(string)
	// The full token works against an auth-gated route (RBAC token path).
	if got := doAuthed(t, s, http.MethodGet, "/api/devices", fullTok); got != http.StatusOK {
		t.Fatalf("api token on devices: got %d", got)
	}
	// Revoke → 401 on the same route.
	if code, _ := doUsers(t, s, http.MethodDelete, "/api/users/"+id+"/tokens/"+tokID, nil); code != http.StatusOK {
		t.Fatalf("token revoke: got %d", code)
	}
	if got := doAuthed(t, s, http.MethodGet, "/api/devices", fullTok); got != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d", got)
	}
}

// TestUsersAPIRoleGate: the account surface is admin-only — a tech
// session (which otherwise passes the auth gate) gets 403.
func TestUsersAPIRoleGate(t *testing.T) {
	s, usr := newUsersTestServer(t)
	if _, err := usr.Create(context.Background(), "alice", store.RoleTech, "pw-12345", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", nil, http.MethodGet, "/api/users", nil); code != http.StatusForbidden {
		t.Fatalf("tech on /api/users: got %d", code)
	}
}

// TestRBACDeviceScoping: a tech session granted one client sees only that
// client's devices (list union, ?client= validation, single-device 403)
// and viewers are read-only on the operational sub-routes.
func TestRBACDeviceScoping(t *testing.T) {
	s, usr := newUsersTestServer(t)
	devs := s.devices
	// Seed two clients' devices + one unassigned.
	if err := devs.Register(context.Background(), "dev-a", "host-a", "linux", "amd64", "0.1.0", []string{"10.0.0.1"}, 30, 30); err != nil {
		t.Fatal(err)
	}
	if err := devs.Register(context.Background(), "dev-b", "host-b", "linux", "amd64", "0.1.0", []string{"10.0.0.2"}, 30, 30); err != nil {
		t.Fatal(err)
	}
	if err := devs.Register(context.Background(), "dev-c", "host-c", "linux", "amd64", "0.1.0", []string{"10.0.0.3"}, 30, 30); err != nil {
		t.Fatal(err)
	}
	if err := devs.SetClient(context.Background(), "dev-a", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := devs.SetClient(context.Background(), "dev-b", "globex"); err != nil {
		t.Fatal(err)
	}
	// dev-c stays unassigned (default client).

	// tech granted acme only:
	code, out := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodGet, "/api/devices", nil)
	if code != http.StatusOK {
		t.Fatalf("tech device list: got %d", code)
	}
	rows, _ := out.([]any)
	if len(rows) != 1 {
		t.Fatalf("tech should see exactly acme's device, got %d", len(rows))
	}
	// ?client=granted passes; ?client=ungranted is a 403 at the gate.
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodGet, "/api/devices?client=acme", nil); code != http.StatusOK {
		t.Fatalf("?client=granted: got %d", code)
	}
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodGet, "/api/devices?client=globex", nil); code != http.StatusForbidden {
		t.Fatalf("?client=ungranted: got %d", code)
	}
	// Single-device scoping via PATCH tags (the in-memory probe that
	// reaches the store; the read sub-routes 503 unwired in tests):
	// granted 200, ungranted 403, unassigned-for-acme-only 403.
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodPatch, "/api/devices/dev-a", map[string]any{"tags": []string{"a"}}); code != http.StatusOK {
		t.Fatalf("granted device tags: got %d", code)
	}
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodPatch, "/api/devices/dev-b", map[string]any{"tags": []string{"a"}}); code != http.StatusForbidden {
		t.Fatalf("ungranted device tags: got %d", code)
	}
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodPatch, "/api/devices/dev-c", map[string]any{"tags": []string{"a"}}); code != http.StatusForbidden {
		t.Fatalf("unassigned device for acme-only grant: got %d", code)
	}
	// Unassigned grant ("unassigned" client id) reaches the default-client device.
	if code, _ := doUsersAs(t, s, usr, "bob", "tech", []string{store.DefaultClientID}, http.MethodPatch, "/api/devices/dev-c", map[string]any{"tags": []string{"u"}}); code != http.StatusOK {
		t.Fatalf("unassigned grant: got %d", code)
	}
	// Viewer is read-only: PATCH tags on a granted device → 403.
	if code, _ := doUsersAs(t, s, usr, "carol", "viewer", []string{"acme"}, http.MethodPatch, "/api/devices/dev-a", map[string]any{"tags": []string{"x"}}); code != http.StatusForbidden {
		t.Fatalf("viewer tags: got %d", code)
	}
	// Admin sees everything.
	if code, out := doUsers(t, s, http.MethodGet, "/api/devices", nil); code != http.StatusOK && out == nil {
		t.Fatalf("admin list: got %d", code)
	} else if rows, _ := out.([]any); len(rows) != 3 {
		t.Fatalf("admin should see all 3, got %d", len(rows))
	}
}

// TestRBACAdminOnlyRoutes: the audited admin-only surface (events, flows,
// heal, webhooks, enroll, baseline, client CRUD) rejects a tech session.
func TestRBACAdminOnlyRoutes(t *testing.T) {
	s, usr := newUsersTestServer(t)
	if _, err := usr.Create(context.Background(), "alice", store.RoleTech, "pw-12345", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, path := range []string{
		"/api/events", "/api/flows", "/api/heal/playbooks", "/api/webhooks",
		"/api/baseline/anomalies", "/api/bootstrap",
	} {
		if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodGet, path, nil); code != http.StatusForbidden {
			t.Errorf("tech on %s: got %d, want 403", path, code)
		}
	}
	// Client CRUD: tech reads but cannot create.
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodGet, "/api/clients", nil); code != http.StatusOK {
		t.Fatalf("tech client list: got %d", code)
	}
	if code, _ := doUsersAs(t, s, usr, "alice", "tech", []string{"acme"}, http.MethodPost, "/api/clients", map[string]any{"name": "New Co"}); code != http.StatusForbidden {
		t.Fatalf("tech client create: got %d", code)
	}
}
