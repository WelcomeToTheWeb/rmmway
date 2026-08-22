package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// newDispatchServer builds a Server with a capturing dispatcher: records the
// (device, action) pairs passed to it, and can be configured to simulate
// "offline" (push failure) or "not wired" (nil dispatch).
func newDispatchServer(t *testing.T, offline bool) (*Server, *store.MemoryDeviceStore, *[]dispatchCall) {
	t.Helper()
	devs := store.NewMemoryDeviceStore()
	if err := devs.Register(context.Background(), "dev-abc", "fileserver-01", "linux", "amd64", "0.1.0", []string{"10.0.0.9"}, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	calls := &[]dispatchCall{}
	dispatch := func(deviceID string, action any) (string, error) {
		*calls = append(*calls, dispatchCall{device: deviceID, action: action})
		if offline {
			return "", ingestErrNotReachable(deviceID)
		}
		return "cmd-1", nil
	}
	s := New(Config{
		Devices:       devs,
		JWTSecret:     []byte("test-secret"),
		TokenLifetime: time.Hour,
		AdminUser:     "admin",
		AdminPassword: "s3cret",
		Dispatch:      dispatch,
		MintBootstrap: func() (string, string) { return "bt-test", "dev-xyz" },
	})
	return s, devs, calls
}

type dispatchCall struct {
	device string
	action any
}

// ingestErrNotReachable mimics the Dispatcher's offline error wording so
// the handler maps it to 502 (the handler matches on "not reachable").
func ingestErrNotReachable(dev string) error {
	return errors.New("device " + dev + " not reachable (no live stream)")
}

func postDispatch(t *testing.T, s *Server, token, path string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
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

func TestDispatchAuthGate(t *testing.T) {
	s, _, calls := newDispatchServer(t, false)
	// No token -> 401 (auth runs before dispatch, so nothing is recorded).
	code, _ := postDispatch(t, s, "", "/api/devices/dev-abc/commands", dispatchRequest{Action: "reboot"})
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}
	// Valid token -> 200 + command_id, and the dispatcher is called.
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	code2, out := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "reboot"})
	if code2 != http.StatusOK {
		t.Fatalf("authed dispatch: got %d, want 200 (body %v)", code2, out)
	}
	if out["command_id"] != "cmd-1" || out["device_id"] != "dev-abc" {
		t.Fatalf("unexpected dispatch response: %v", out)
	}
	if len(*calls) != 1 || (*calls)[0].device != "dev-abc" {
		t.Fatalf("dispatcher not called as expected: %+v", *calls)
	}
}

func TestDispatchActionValidation(t *testing.T) {
	s, _, _ := newDispatchServer(t, false)
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)

	// Unknown action -> 400.
	if code, _ := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "nuke"}); code != http.StatusBadRequest {
		t.Fatalf("unknown action: got %d, want 400", code)
	}
	// Unsupported lang -> 400.
	if code, _ := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "run_script", Lang: "bat", Script: base64.StdEncoding.EncodeToString([]byte("x"))}); code != http.StatusBadRequest {
		t.Fatalf("unsupported lang: got %d, want 400", code)
	}
	// Non-base64 script -> 400.
	if code, _ := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "run_script", Lang: "sh", Script: "not-base64!!"}); code != http.StatusBadRequest {
		t.Fatalf("bad script: got %d, want 400", code)
	}
	// Valid run_script -> 200, and the action is a *Command_RunScript with the right lang.
	s2, _, calls := newDispatchServer(t, false)
	_, b2 := login(t, s2, "admin", "s3cret")
	tok2, _ := b2["token"].(string)
	code, _ := postDispatch(t, s2, tok2, "/api/devices/dev-abc/commands", dispatchRequest{Action: "run_script", Lang: "sh", Script: base64.StdEncoding.EncodeToString([]byte("echo hi"))})
	if code != http.StatusOK {
		t.Fatalf("valid run_script: got %d, want 200", code)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", len(*calls))
	}
	rs, ok := (*calls)[0].action.(*agentv1.Command_RunScript)
	if !ok {
		t.Fatalf("expected *Command_RunScript, got %T", (*calls)[0].action)
	}
	if rs.RunScript.GetLang() != "sh" || string(base64decode(t, rs.RunScript.GetScriptB64())) != "echo hi" {
		t.Fatalf("run_script payload wrong: lang=%q b64=%q", rs.RunScript.GetLang(), rs.RunScript.GetScriptB64())
	}
}

func TestDispatchUnknownDevice(t *testing.T) {
	s, _, calls := newDispatchServer(t, false)
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	code, _ := postDispatch(t, s, tok, "/api/devices/dev-nope/commands", dispatchRequest{Action: "reboot"})
	if code != http.StatusNotFound {
		t.Fatalf("unknown device: got %d, want 404", code)
	}
	if len(*calls) != 0 {
		t.Fatalf("dispatch must not be called for unknown device, got %d calls", len(*calls))
	}
}

func TestDispatchOffline(t *testing.T) {
	s, _, _ := newDispatchServer(t, true)
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	code, _ := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "reboot"})
	if code != http.StatusBadGateway {
		t.Fatalf("offline device: got %d, want 502", code)
	}
}

func TestDispatchNotWired(t *testing.T) {
	// Nil dispatch -> 503 (degraded), no crash.
	devs := store.NewMemoryDeviceStore()
	_ = devs.Register(context.Background(), "dev-abc", "h", "linux", "amd64", "0.1.0", nil, 30, 30)
	s := New(Config{Devices: devs, JWTSecret: []byte("test-secret"), AdminUser: "admin", AdminPassword: "s3cret"})
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	code, _ := postDispatch(t, s, tok, "/api/devices/dev-abc/commands", dispatchRequest{Action: "reboot"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("nil dispatch: got %d, want 503", code)
	}
}

func TestSearchAuthGate(t *testing.T) {
	// No live Meili in unit tests; search returns 503. The point here is the
	// auth gate: no token -> 401 (before the 503 degraded path is reached).
	s := New(Config{Devices: store.NewMemoryDeviceStore(), JWTSecret: []byte("test-secret"), AdminUser: "admin", AdminPassword: "s3cret"})
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token on /api/search: got %d, want 401", rec.Code)
	}
	// With a valid token, the search runs (and degrades to 503 with no index).
	_, body := login(t, s, "admin", "s3cret")
	tok, _ := body["token"].(string)
	req2 := httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("authed search with no index: got %d, want 503 (degraded)", rec2.Code)
	}
}

func base64decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	return b
}

// ensure the operator-JWT symbols used by tests still compile (cross-kind guard).
var _ = ingest.MintOperatorJWT
