package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/smtp"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// newTestServerWithSetup builds the standard test server with a wired setup
// service (in-memory store), so the settings routes have a real store to
// read from and write to.
func newTestServerWithSetup(t *testing.T) (*Server, *setup.Service) {
	t.Helper()
	devs := store.NewMemoryDeviceStore()
	if err := devs.Register(context.Background(),
		"dev-abc", "fileserver-01", "linux", "amd64", "0.1.0", []string{"10.0.0.9"}, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := setup.New(setup.Config{Store: setup.NewMemoryStore()})
	rateLimit := false
	s := New(Config{
		Devices:        devs,
		JWTSecret:      []byte("test-secret"),
		TokenLifetime:  time.Hour,
		AdminUser:      "admin",
		AdminPassword:  "s3cret",
		MintBootstrap:  func() (string, string) { return "bt-test", "dev-xyz" },
		LoginRateLimit: &rateLimit,
		Setup:          svc,
	})
	return s, svc
}

// doSettings performs one settings request and returns (status, parsed body).
func doSettings(t *testing.T, s *Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out == nil {
		out = map[string]any{}
	}
	return rec.Code, out
}

// loginToken performs /api/login and returns the bearer token ("" on failure).
func loginAs(t *testing.T, s *Server, user, pass string) string {
	t.Helper()
	code, body := login(t, s, user, pass)
	if code != http.StatusOK {
		t.Fatalf("login %s: got %d %v", user, code, body)
	}
	tok, _ := body["token"].(string)
	return tok
}

func TestSettingsAuthGate(t *testing.T) {
	s, _ := newTestServerWithSetup(t)
	for _, path := range []string{"/api/settings", "/admin/settings"} {
		code, _ := doSettings(t, s, http.MethodGet, path, "", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s without token: got %d, want 401", path, code)
		}
	}
	code, _ := doSettings(t, s, http.MethodPost, "/api/settings/profile/password", "", map[string]string{
		"current_password": "s3cret", "new_password": "newpass123",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("password change without token: got %d, want 401", code)
	}
}

// TestSettingsGetFresh covers the pre-wizard read model: env-admin identity,
// empty org, unconfigured outbox — and that the password is never in the
// body at all (not even as an empty string).
func TestSettingsGetFresh(t *testing.T) {
	s, _ := newTestServerWithSetup(t)
	tok := loginAs(t, s, "admin", "s3cret")
	code, body := doSettings(t, s, http.MethodGet, "/api/settings", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET: got %d %v", code, body)
	}
	if body["org_name"] != "" {
		t.Fatalf("org_name: got %v, want empty", body["org_name"])
	}
	profile, _ := body["profile"].(map[string]any)
	if profile["username"] != "admin" {
		t.Fatalf("profile.username: got %v, want admin (env fallback)", profile["username"])
	}
	if profile["mfa_enabled"] != false {
		t.Fatalf("profile.mfa_enabled: got %v, want false (pre-wave-2)", profile["mfa_enabled"])
	}
	smtpOut, _ := body["smtp"].(map[string]any)
	if smtpOut["configured"] != false || smtpOut["pass_set"] != false {
		t.Fatalf("smtp: got %v, want configured=false pass_set=false", smtpOut)
	}
	raw, _ := json.Marshal(body)
	if bytes.Contains(raw, []byte(`"password"`)) {
		t.Fatalf("password leaked in read model: %s", raw)
	}
}

// TestSettingsGetAfterWizard covers the post-wizard read model: the DB admin
// identity, the org name, and the sanitized SMTP config.
func TestSettingsGetAfterWizard(t *testing.T) {
	s, svc := newTestServerWithSetup(t)
	if err := svc.Complete(context.Background(), setup.CompleteRequest{
		AdminUser:     "alice",
		AdminPassword: "wizpass123",
		OrgName:       "Acme Corp",
		SMTP: smtp.Config{
			Host: "smtp.acme.test", Port: 587, From: "rmm@acme.test",
			Username: "rmm-auth", Password: "hunter22",
		},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// The wizard's own admin can sign in (DB-first login) and sees the model.
	tok := loginAs(t, s, "alice", "wizpass123")
	code, body := doSettings(t, s, http.MethodGet, "/api/settings", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("GET: got %d %v", code, body)
	}
	if body["org_name"] != "Acme Corp" {
		t.Fatalf("org_name: got %v", body["org_name"])
	}
	if profile := body["profile"].(map[string]any); profile["username"] != "alice" {
		t.Fatalf("profile.username: got %v, want alice", profile["username"])
	}
	smtpOut := body["smtp"].(map[string]any)
	if smtpOut["host"] != "smtp.acme.test" || int(smtpOut["port"].(float64)) != 587 ||
		smtpOut["from"] != "rmm@acme.test" || smtpOut["username"] != "rmm-auth" ||
		smtpOut["configured"] != true || smtpOut["pass_set"] != true {
		t.Fatalf("smtp read model wrong: %v", smtpOut)
	}
	raw, _ := json.Marshal(body)
	if bytes.Contains(raw, []byte(`"password"`)) || bytes.Contains(raw, []byte("hunter22")) {
		t.Fatalf("smtp password leaked: %s", raw)
	}
}

// TestSettingsPatchSMTP validates the save path: invalid configs are
// rejected with 400, valid ones persist to the store.
func TestSettingsPatchSMTP(t *testing.T) {
	s, svc := newTestServerWithSetup(t)
	tok := loginAs(t, s, "admin", "s3cret")

	code, body := doSettings(t, s, http.MethodPatch, "/api/settings", tok, map[string]any{
		"smtp": map[string]any{"host": "smtp.acme.test", "port": 99999, "from": "rmm@acme.test"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid port: got %d %v, want 400", code, body)
	}

	code, body = doSettings(t, s, http.MethodPatch, "/api/settings", tok, map[string]any{
		"smtp": map[string]any{
			"host": "smtp.acme.test", "port": 465, "from": "rmm@acme.test",
			"username": "rmm-auth", "password": "s3cret-pass",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("valid patch: got %d %v", code, body)
	}
	stored, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.SMTP.Host != "smtp.acme.test" || stored.SMTP.Port != 465 ||
		stored.SMTP.Password != "s3cret-pass" || !stored.SMTPConfigured {
		t.Fatalf("store not updated: %+v", stored.SMTP)
	}
	// The echo is sanitized: no raw password field in the response.
	raw, _ := json.Marshal(body)
	if bytes.Contains(raw, []byte(`"password"`)) {
		t.Fatalf("password in PATCH echo: %s", raw)
	}

	// Clearing the outbox: a zero host is a valid config that un-configures.
	code, _ = doSettings(t, s, http.MethodPatch, "/api/settings", tok, map[string]any{
		"smtp": map[string]any{"host": "", "port": 0, "from": "", "username": "", "password": ""},
	})
	if code != http.StatusOK {
		t.Fatalf("clear: got %d", code)
	}
	stored, _ = svc.Load(context.Background())
	if stored.SMTPConfigured {
		t.Fatalf("outbox should be cleared, got %+v", stored.SMTP)
	}
}

// TestSettingsSMTPTest covers the stored-config test send: 502 when nothing
// is configured, 200 + sink receipt when it works, default recipient = from.
func TestSettingsSMTPTest(t *testing.T) {
	s, svc := newTestServerWithSetup(t)
	tok := loginAs(t, s, "admin", "s3cret")

	// Not configured yet → 502.
	code, body := doSettings(t, s, http.MethodPost, "/api/settings/smtp/test", tok, nil)
	if code != http.StatusBadGateway {
		t.Fatalf("unconfigured test: got %d %v, want 502", code, body)
	}

	sink, err := smtp.NewSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()
	cfg := smtp.Config{
		Host: "127.0.0.1", Port: sink.Port(), From: "rmm@acme.test",
		Username: "u", Password: "p",
	}
	if err := svc.SaveSMTP(context.Background(), cfg); err != nil {
		t.Fatalf("save smtp: %v", err)
	}

	code, body = doSettings(t, s, http.MethodPost, "/api/settings/smtp/test", tok, map[string]string{"to": "op@example.com"})
	if code != http.StatusOK || body["ok"] != true || body["to"] != "op@example.com" {
		t.Fatalf("explicit-recipient test: got %d %v", code, body)
	}
	code, body = doSettings(t, s, http.MethodPost, "/api/settings/smtp/test", tok, nil)
	if code != http.StatusOK || body["to"] != "rmm@acme.test" {
		t.Fatalf("default-recipient test: got %d %v", code, body)
	}
	if n := len(sink.Mails()); n != 2 {
		t.Fatalf("sink received %d mails, want 2: %v", n, sink.Mails())
	}
}

// TestSettingsPasswordChange covers the DB-admin path: wrong current
// password → 400, right one → the new password signs in and the old stops
// working (the DB row wins over the env fallback).
func TestSettingsPasswordChange(t *testing.T) {
	s, svc := newTestServerWithSetup(t)
	if err := svc.Complete(context.Background(), setup.CompleteRequest{
		AdminUser:     "alice",
		AdminPassword: "wizpass123",
		OrgName:       "Acme",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	tok := loginAs(t, s, "alice", "wizpass123")

	code, _ := doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "wrongpass1", "new_password": "newpass123",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("wrong current: got %d, want 400", code)
	}
	code, _ = doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "wizpass123", "new_password": "short",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("short new: got %d, want 400", code)
	}
	code, body := doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "wizpass123", "new_password": "newpass123",
	})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("change: got %d %v", code, body)
	}
	if c, b := login(t, s, "alice", "newpass123"); c != http.StatusOK {
		t.Fatalf("new password login: got %d %v", c, b)
	}
	if c, b := login(t, s, "alice", "wizpass123"); c != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: got %d %v", c, b)
	}
}

// TestSettingsPasswordChangeEnvAdmin covers the pre-wizard path: changing
// the env admin's password MINTS a DB row, which then takes over login (new
// password works, old env pair stops).
func TestSettingsPasswordChangeEnvAdmin(t *testing.T) {
	s, _ := newTestServerWithSetup(t)
	tok := loginAs(t, s, "admin", "s3cret")

	code, _ := doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "nope", "new_password": "envnew1234",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("wrong current: got %d, want 400", code)
	}
	code, body := doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "s3cret", "new_password": "envnew1234",
	})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("env-admin change: got %d %v", code, body)
	}
	if c, b := login(t, s, "admin", "envnew1234"); c != http.StatusOK {
		t.Fatalf("minted row login: got %d %v", c, b)
	}
	if c, _ := login(t, s, "admin", "s3cret"); c != http.StatusUnauthorized {
		t.Fatalf("old env password still accepted: got %d", c)
	}
}

// TestSettingsUnwired covers the in-memory-mode server (no setup service):
// the read model degrades to env defaults, writes and sends report 503.
func TestSettingsUnwired(t *testing.T) {
	s, _ := newTestServer(t)
	tok := loginAs(t, s, "admin", "s3cret")

	code, body := doSettings(t, s, http.MethodGet, "/api/settings", tok, nil)
	if code != http.StatusOK || body["org_name"] != "" {
		t.Fatalf("unwired GET: got %d %v", code, body)
	}
	code, _ = doSettings(t, s, http.MethodPatch, "/api/settings", tok, map[string]any{
		"smtp": map[string]any{"host": "h", "port": 25, "from": "a@b.c"},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unwired PATCH: got %d, want 503", code)
	}
	code, _ = doSettings(t, s, http.MethodPost, "/api/settings/smtp/test", tok, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unwired test: got %d, want 503", code)
	}
	code, _ = doSettings(t, s, http.MethodPost, "/api/settings/profile/password", tok, map[string]string{
		"current_password": "s3cret", "new_password": "newpass123",
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unwired password: got %d, want 503", code)
	}
	// Method gating: PUT /api/settings → 405.
	code, _ = doSettings(t, s, http.MethodPut, "/api/settings", tok, nil)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: got %d, want 405", code)
	}
}

// TestSettingsAdminMirror proves the /admin/* mirror behaves identically.
func TestSettingsAdminMirror(t *testing.T) {
	s, _ := newTestServerWithSetup(t)
	tok := loginAs(t, s, "admin", "s3cret")
	code, body := doSettings(t, s, http.MethodGet, "/admin/settings", tok, nil)
	if code != http.StatusOK || body["profile"] == nil {
		t.Fatalf("/admin/settings: got %d %v", code, body)
	}
	code, _ = doSettings(t, s, http.MethodPost, "/admin/settings/smtp/test", tok, nil)
	if code != http.StatusBadGateway {
		t.Fatalf("/admin test (unconfigured): got %d, want 502", code)
	}
	// Sanity: the route table still serves both trees.
	mux := http.NewServeMux()
	s.Register(mux)
	for _, p := range []string{
		"/api/settings", "/admin/settings",
		"/api/settings/smtp/test", "/admin/settings/smtp/test",
		"/api/settings/profile/password", "/admin/settings/profile/password",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s not routed (404): %s", p, rec.Body.String())
		}
	}
}
