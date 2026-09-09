package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

var rbacTestSecret = []byte("rbac-test-secret")

func rbacTestRBAC(t *testing.T, u store.UserStore) *RBAC {
	t.Helper()
	return &RBAC{Secret: rbacTestSecret, Users: u}
}

func mintRBACToken(t *testing.T, role, username string, caps []string) string {
	t.Helper()
	tok, err := MintSessionJWT(rbacTestSecret, time.Hour, SessionClaims{
		Role: role, Username: username, Caps: caps,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func sessionReq(t *testing.T, tok string, query string) *http.Request {
	t.Helper()
	u := &url.URL{Path: "/x"}
	if query != "" {
		u.RawQuery = query
	}
	r := httptest.NewRequest(http.MethodGet, u.String(), nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestRBACSessionFromRequest(t *testing.T) {
	usr := store.NewMemoryUserStore()
	rbac := rbacTestRBAC(t, usr)
	tech, err := usr.Create(context.Background(), "tech1", store.RoleTech, "pw", []string{"acme"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No token / garbage token.
	if _, ok := rbac.SessionFromRequest(sessionReq(t, "", "")); ok {
		t.Fatal("no token must not resolve")
	}
	if _, ok := rbac.SessionFromRequest(sessionReq(t, "not-a-jwt", "")); ok {
		t.Fatal("garbage token must not resolve")
	}

	// Admin JWT: no store round-trip, all clients.
	sess, ok := rbac.SessionFromRequest(sessionReq(t, mintRBACToken(t, "admin", "boss", nil), ""))
	if !ok || !sess.AllClients || sess.Role != "admin" || sess.Username != "boss" {
		t.Fatalf("admin session: got %+v, %v", sess, ok)
	}

	// Legacy JWT (no role claim): grandfathered admin.
	legacyTok, err := MintSessionJWT(rbacTestSecret, time.Hour, SessionClaims{Username: "old", Caps: []string{"rmmway.run_script"}})
	if err != nil {
		t.Fatalf("mint legacy: %v", err)
	}
	sess, ok = rbac.SessionFromRequest(sessionReq(t, legacyTok, ""))
	if !ok || !sess.Legacy || !sess.AllClients || sess.Role != "admin" {
		t.Fatalf("legacy session: got %+v, %v", sess, ok)
	}

	// Tech JWT: grants load LIVE from the users row.
	sess, ok = rbac.SessionFromRequest(sessionReq(t, mintRBACToken(t, "tech", "tech1", nil), ""))
	if !ok || sess.AllClients || len(sess.ClientIDs) != 1 || sess.ClientIDs[0] != "acme" {
		t.Fatalf("tech session: got %+v, %v", sess, ok)
	}

	// Disable the account → the 12-hour-old JWT is dead immediately.
	disabled := false
	if _, err := usr.Update(context.Background(), tech.ID, nil, &disabled, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := rbac.SessionFromRequest(sessionReq(t, mintRBACToken(t, "tech", "tech1", nil), "")); ok {
		t.Fatal("disabled user's session must not resolve")
	}

	// Unwired RBAC (in-memory mode): a tech JWT has no grants to load →
	// empty union (no access), not an error.
	unwired := &RBAC{Secret: rbacTestSecret}
	sess, ok = unwired.SessionFromRequest(sessionReq(t, mintRBACToken(t, "tech", "tech1", nil), ""))
	if !ok || sess.AllClients || len(sess.ClientIDs) != 0 {
		t.Fatalf("unwired tech session: got %+v, %v", sess, ok)
	}
}

func TestRBACAPITokens(t *testing.T) {
	usr := store.NewMemoryUserStore()
	rbac := rbacTestRBAC(t, usr)
	u, err := usr.Create(context.Background(), "carol", store.RoleViewer, "pw", []string{"acme", "globex"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tok, _, _, err := usr.CreateToken(context.Background(), u.ID, "ci", time.Hour)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	sess, ok := rbac.SessionFromRequest(sessionReq(t, tok, ""))
	if !ok || sess.Username != "carol" || sess.Role != "viewer" || len(sess.ClientIDs) != 2 {
		t.Fatalf("api-token session: got %+v, %v", sess, ok)
	}
	// Revoke → dead.
	toks, err := usr.ListTokens(context.Background(), u.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("list tokens: got %d, %v", len(toks), err)
	}
	if err := usr.RevokeToken(context.Background(), toks[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := rbac.SessionFromRequest(sessionReq(t, tok, "")); ok {
		t.Fatal("revoked token must not resolve")
	}
	// Unknown prefix shape.
	if _, ok := rbac.SessionFromRequest(sessionReq(t, "rmm_x", "")); ok {
		t.Fatal("unknown rmm_ token must not resolve")
	}
	// rmm_ token with no users store (in-memory mode) → no tokens exist.
	if _, ok := (&RBAC{Secret: rbacTestSecret}).SessionFromRequest(sessionReq(t, tok, "")); ok {
		t.Fatal("api token must not resolve without the users store")
	}
}

func TestRBACRequireAndRole(t *testing.T) {
	rbac := rbacTestRBAC(t, nil)
	inner := rbac.RequireRole("admin", "tech")(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := SessionFromContext(r.Context())
		_, _ = w.Write([]byte(sess.Role + ":" + sess.Username))
	})
	srv := http.HandlerFunc(inner)

	// 401 without a token.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, "", ""))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("no token: got %d %q", rec.Code, rec.Body.String())
	}
	// viewer → 403.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, mintRBACToken(t, "viewer", "v", nil), ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: got %d %q", rec.Code, rec.Body.String())
	}
	// admin → 200.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, mintRBACToken(t, "admin", "a", nil), ""))
	if rec.Code != http.StatusOK || rec.Body.String() != "admin:a" {
		t.Fatalf("admin: got %d %q", rec.Code, rec.Body.String())
	}
	// legacy → grandfathered admin.
	legacy, _ := MintSessionJWT(rbacTestSecret, time.Hour, SessionClaims{Username: "old"})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, legacy, ""))
	if rec.Code != http.StatusOK || rec.Body.String() != "admin:old" {
		t.Fatalf("legacy: got %d %q", rec.Code, rec.Body.String())
	}
}

func TestRBACClientScope(t *testing.T) {
	usr := store.NewMemoryUserStore()
	rbac := rbacTestRBAC(t, usr)
	if _, err := usr.Create(context.Background(), "tech1", store.RoleTech, "pw", []string{"acme"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var seen Session
	inner := rbac.RequireClientScope(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = SessionFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := http.HandlerFunc(inner)
	tok := mintRBACToken(t, "tech", "tech1", nil)

	// Granted client → 200, ActiveClient bound.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, tok, "client=acme"))
	if rec.Code != http.StatusOK || seen.ActiveClient != "acme" {
		t.Fatalf("granted client: got %d %+v", rec.Code, seen)
	}
	// Ungranted client → 403.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, tok, "client=globex"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted client: got %d", rec.Code)
	}
	// No ?client= → 200 with empty ActiveClient (handler unions grants).
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, tok, ""))
	if rec.Code != http.StatusOK || seen.ActiveClient != "" {
		t.Fatalf("no client param: got %d %+v", rec.Code, seen)
	}
	// Admin passes any client through.
	adminTok := mintRBACToken(t, "admin", "boss", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, sessionReq(t, adminTok, "client=anything"))
	if rec.Code != http.StatusOK || seen.ActiveClient != "anything" || !seen.AllClients {
		t.Fatalf("admin passthrough: got %d %+v", rec.Code, seen)
	}
}

// TestRBACTokenFromRequest covers the SSE ?token= path (EventSource cannot
// set headers) and header precedence.
func TestRBACTokenFromRequest(t *testing.T) {
	r := sessionReq(t, "hdr-token", "token=query-token")
	if got := TokenFromRequest(r); got != "hdr-token" {
		t.Fatalf("header precedence: got %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/x?token=only-query", nil)
	if got := TokenFromRequest(r); got != "only-query" {
		t.Fatalf("query token: got %q", got)
	}
}
