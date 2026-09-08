package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// newClientTestServer builds the standard in-memory test server WITH the
// client registry wired (gap #2).
func newClientTestServer(t *testing.T) (*Server, *store.MemoryDeviceStore, *store.MemoryClientStore) {
	t.Helper()
	devs := store.NewMemoryDeviceStore()
	if err := devs.Register(context.Background(),
		"dev-abc", "fileserver-01", "linux", "amd64", "0.1.0", []string{"10.0.0.9"}, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	clients := store.NewMemoryClientStore()
	clients.SeedDefaultClient()
	rateLimit := false
	s := New(Config{
		Devices:        devs,
		Clients:        clients,
		JWTSecret:      []byte("test-secret"),
		TokenLifetime:  time.Hour,
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		LoginRateLimit: &rateLimit,
	})
	return s, devs, clients
}

// TestClientRoutesUnwired: in-memory mode without the client registry —
// every /clients* route and the device-assignment route 503, while the
// ?client= scoping on the plain device list keeps working (it scopes the
// device store, not the registry).
func TestClientRoutesUnwired(t *testing.T) {
	s, _ := newTestServer(t) // no Clients wired
	tok := loginToken(t, s)
	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/clients"},
		{http.MethodPost, "/api/clients"},
		{http.MethodPatch, "/api/clients/unassigned"},
		{http.MethodGet, "/api/clients/unassigned/devices"},
		{http.MethodPatch, "/api/devices/dev-abc/client"},
		{http.MethodGet, "/admin/clients"},
	} {
		code, body := doJSON(t, s, c.method, c.path, tok, map[string]any{})
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: got %d (%v), want 503", c.method, c.path, code, body)
		}
	}
	// ?client= scoping does not depend on the registry.
	code := doAuthed(t, s, http.MethodGet, "/api/devices?client=unassigned", tok)
	if code != http.StatusOK {
		t.Fatalf("device list ?client= unwired: got %d, want 200", code)
	}
}

// doRaw performs a request like doAuthed but returns the raw body string
// (needed for endpoints that answer bare JSON arrays, like the device list).
func doRaw(t *testing.T, s *Server, method, path, token string, body any) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestClientRoutes covers the full client-registry surface against the
// in-memory store: list rollup, create (incl. 409/400), patch (404/400/
// 409), per-client devices, device assignment (incl. null-unassign and
// unknown device/client 404s), ?client= scoping, and the /admin mirror.
func TestClientRoutes(t *testing.T) {
	s, _, _ := newClientTestServer(t)
	tok := loginToken(t, s)

	// Unauthenticated -> 401.
	if code := doAuthed(t, s, http.MethodGet, "/api/clients", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", code)
	}

	// Seed: one client (Unassigned), one unassigned device.
	code, raw := doRaw(t, s, http.MethodGet, "/api/clients", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list: got %d: %s", code, raw)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("list: unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list: got %v, want 1 client", list)
	}

	// Create.
	code, raw = doRaw(t, s, http.MethodPost, "/api/clients", tok,
		map[string]any{"name": "Acme Corp", "description": " enterprise "})
	if code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", code, raw)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatalf("create: unmarshal: %v", err)
	}
	acmeID, _ := created["id"].(string)
	if acmeID == "" || !strings.HasPrefix(acmeID, "clt-") {
		t.Fatalf("created id = %q, want clt-*", acmeID)
	}
	if d, _ := created["description"].(string); d != "enterprise" {
		t.Fatalf("description not trimmed: %q", d)
	}
	if c, _ := created["device_count"].(float64); c != 0 {
		t.Fatalf("new client device_count = %v, want 0", c)
	}

	// Duplicate name -> 409 (case-insensitive, trimmed).
	if code, _ := doJSON(t, s, http.MethodPost, "/api/clients", tok,
		map[string]any{"name": " acme corp "}); code != http.StatusConflict {
		t.Fatalf("duplicate: got %d, want 409", code)
	}
	// Missing name -> 400.
	if code, _ := doJSON(t, s, http.MethodPost, "/api/clients", tok,
		map[string]any{"description": "x"}); code != http.StatusBadRequest {
		t.Fatalf("missing name: got %d, want 400", code)
	}
	// Oversized name -> 400.
	if code, _ := doJSON(t, s, http.MethodPost, "/api/clients", tok,
		map[string]any{"name": strings.Repeat("c", 129)}); code != http.StatusBadRequest {
		t.Fatalf("long name: got %d, want 400", code)
	}

	// List now has 2.
	code, raw = doRaw(t, s, http.MethodGet, "/api/clients", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list after create: got %d", code)
	}
	list = list[:0]
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("list after create: unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list after create: got %d, want 2", len(list))
	}

	// Rename.
	code, raw = doRaw(t, s, http.MethodPatch, "/api/clients/"+acmeID, tok,
		map[string]any{"name": "Acme Corporation"})
	if code != http.StatusOK {
		t.Fatalf("rename: got %d: %s", code, raw)
	}
	if err := json.Unmarshal([]byte(raw), &created); err != nil {
		t.Fatalf("rename: unmarshal: %v", err)
	}
	if n, _ := created["name"].(string); n != "Acme Corporation" {
		t.Fatalf("rename: name = %q", n)
	}
	// Empty patch -> 400.
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/clients/"+acmeID, tok, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("empty patch: got %d, want 400", code)
	}
	// Unknown client -> 404.
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/clients/clt-nosuch", tok,
		map[string]any{"name": "Ghost"}); code != http.StatusNotFound {
		t.Fatalf("unknown client: got %d, want 404", code)
	}
	// Renaming onto a taken name -> 409.
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/clients/unassigned", tok,
		map[string]any{"name": "Acme Corporation"}); code != http.StatusConflict {
		t.Fatalf("taken name: got %d, want 409", code)
	}

	// Per-client devices: empty before assignment.
	code, raw = doRaw(t, s, http.MethodGet, "/api/clients/"+acmeID+"/devices", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("client devices: got %d", code)
	}
	var devs []map[string]any
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("client devices: unmarshal: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("client devices: got %v, want empty", devs)
	}
	// Unknown client -> 404.
	if code := doAuthed(t, s, http.MethodGet, "/api/clients/clt-nosuch/devices", tok); code != http.StatusNotFound {
		t.Fatalf("unknown client devices: got %d, want 404", code)
	}
	// Garbage subpath -> 404.
	if code := doAuthed(t, s, http.MethodGet, "/api/clients/unassigned/bogus", tok); code != http.StatusNotFound {
		t.Fatalf("bogus subpath: got %d, want 404", code)
	}

	// Assign dev-abc to Acme.
	code, body := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc/client", tok,
		map[string]any{"client_id": acmeID})
	if code != http.StatusOK {
		t.Fatalf("assign: got %d: %v", code, body)
	}
	if cid, _ := body["device"].(map[string]any)["client_id"].(string); cid != acmeID {
		t.Fatalf("assigned client_id = %q, want %q", cid, acmeID)
	}

	// The device list carries client_id and ?client= scopes it.
	code, raw = doRaw(t, s, http.MethodGet, "/api/devices?client="+acmeID, tok, nil)
	if code != http.StatusOK {
		t.Fatalf("scoped list: got %d", code)
	}
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("scoped list: unmarshal: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("scoped list: got %v, want 1", devs)
	}
	code, raw = doRaw(t, s, http.MethodGet, "/api/devices?client=unassigned", tok, nil)
	devs = devs[:0]
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("unassigned scope: unmarshal: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("unassigned scope after assign: got %v, want empty", devs)
	}

	// The client's devices + rollup reflect the assignment.
	code, raw = doRaw(t, s, http.MethodGet, "/api/clients/"+acmeID+"/devices", tok, nil)
	devs = devs[:0]
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("client devices after assign: unmarshal: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("client devices after assign: got %v, want 1", devs)
	}
	code, raw = doRaw(t, s, http.MethodGet, "/api/clients", tok, nil)
	list = list[:0]
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("rollup list: unmarshal: %v", err)
	}
	rollup := map[string]float64{}
	for _, cm := range list {
		rollup[cm["name"].(string)] = cm["device_count"].(float64)
	}
	if rollup["Acme Corporation"] != 1 || rollup["Unassigned"] != 0 {
		t.Fatalf("rollup = %v", rollup)
	}

	// Unassign: client_id null.
	code, raw = doRaw(t, s, http.MethodPatch, "/api/devices/dev-abc/client", tok,
		map[string]any{"client_id": nil})
	if code != http.StatusOK {
		t.Fatalf("unassign: got %d: %s", code, raw)
	}
	var unassigned map[string]any
	if err := json.Unmarshal([]byte(raw), &unassigned); err != nil {
		t.Fatalf("unassign: unmarshal: %v", err)
	}
	if cid, present := unassigned["device"].(map[string]any)["client_id"]; present && cid != nil {
		t.Fatalf("unassigned client_id = %v, want null", cid)
	}
	code, raw = doRaw(t, s, http.MethodGet, "/api/clients/unassigned/devices", tok, nil)
	devs = devs[:0]
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("default client devices: unmarshal: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("default client devices after unassign: got %v, want 1", devs)
	}

	// Unknown device -> 404; unknown client -> 404.
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-ghost/client", tok,
		map[string]any{"client_id": acmeID}); code != http.StatusNotFound {
		t.Fatalf("unknown device: got %d, want 404", code)
	}
	if code, _ := doJSON(t, s, http.MethodPatch, "/api/devices/dev-abc/client", tok,
		map[string]any{"client_id": "clt-ghost"}); code != http.StatusNotFound {
		t.Fatalf("unknown client: got %d, want 404", code)
	}

	// /admin mirror.
	if code := doAuthed(t, s, http.MethodGet, "/admin/clients", tok); code != http.StatusOK {
		t.Fatalf("admin mirror: got %d", code)
	}
}

// TestDeviceListClientIDAbsentByDefault: a device row with no client must
// serialize with client_id: null (not "" — the UI keys the Unassigned
// group on null).
func TestDeviceListClientIDAbsentByDefault(t *testing.T) {
	s, _, _ := newClientTestServer(t)
	tok := loginToken(t, s)
	code, raw := doRaw(t, s, http.MethodGet, "/api/devices", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("device list: got %d", code)
	}
	var devs []map[string]any
	if err := json.Unmarshal([]byte(raw), &devs); err != nil {
		t.Fatalf("device list: unmarshal: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("device list: got %v", devs)
	}
	if cid, present := devs[0]["client_id"]; present && cid != nil {
		t.Fatalf("client_id = %v, want null", cid)
	}
}
