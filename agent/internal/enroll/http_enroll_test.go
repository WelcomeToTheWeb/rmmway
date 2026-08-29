package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// testFacts is a minimal host-facts set for enroll tests.
func testFacts() Facts {
	return Facts{Hostname: "host", OS: "linux", Arch: "amd64", AgentVersion: "v1", Interfaces: []string{"eth0:10.0.0.5/24"}}
}

// newTestEnrollReq builds the request EnsureEnrolled would send.
func newTestEnrollReq() *agentv1.EnrollRequest {
	return &agentv1.EnrollRequest{
		BootstrapToken: "bt-token",
		Hostname:       "host",
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "v1",
		Interfaces:     []string{"eth0:10.0.0.5/24"},
	}
}

// TestHTTPEnroller_Success posts to the operator origin and returns the
// minted identity (incl. the mTLS PEMs the agent persists).
func TestHTTPEnroller_Success(t *testing.T) {
	var gotPath, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"dev-http","jwt":"jwt-http","heartbeat_interval_s":30,"metric_interval_s":15,
			"leaf_cert_pem":"CERT","leaf_key_pem":"KEY","org_root_ca_pem":"ROOT"}`))
	}))
	defer srv.Close()

	h := &HTTPEnroller{BaseURL: srv.URL}
	resp, err := h.Enroll(context.Background(), newTestEnrollReq())
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if gotPath != "/agent/enroll" {
		t.Errorf("path = %q, want /agent/enroll", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotBody == nil || gotBody["bootstrap_token"] != "bt-token" || gotBody["hostname"] != "host" {
		t.Errorf("body = %v, want bootstrap_token=bt-token hostname=host", gotBody)
	}
	if resp.GetDeviceId() != "dev-http" || resp.GetJwt() != "jwt-http" {
		t.Errorf("identity = %q/%q, want dev-http/jwt-http", resp.GetDeviceId(), resp.GetJwt())
	}
	if resp.GetLeafCertPem() != "CERT" || resp.GetLeafKeyPem() != "KEY" || resp.GetOrgRootCaPem() != "ROOT" {
		t.Errorf("mTLS PEMs not surfaced: %q/%q/%q", resp.GetLeafCertPem(), resp.GetLeafKeyPem(), resp.GetOrgRootCaPem())
	}
}

// TestHTTPEnroller_Transientness classifies the error kinds: 4xx is DEFINITIVE
// (do not fall back), 5xx + connection failures are TRANSIENT (fall back to
// the plain gRPC channel).
func TestHTTPEnroller_Transientness(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		transient bool
	}{
		{"403 unknown token is definitive", http.StatusForbidden, false},
		{"400 bad request is definitive", http.StatusBadRequest, false},
		{"500 server error is transient", http.StatusInternalServerError, true},
		{"503 unavailable is transient", http.StatusServiceUnavailable, true},
		{"404 no such route is transient (older server -> gRPC fallback)", http.StatusNotFound, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"denied"}`))
			}))
			defer srv.Close()
			h := &HTTPEnroller{BaseURL: srv.URL}
			_, err := h.Enroll(context.Background(), newTestEnrollReq())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			var he *httpEnrollError
			if !errors.As(err, &he) {
				t.Fatalf("error type = %T, want *httpEnrollError", err)
			}
			if he.transient != tc.transient {
				t.Errorf("transient = %v, want %v", he.transient, tc.transient)
			}
		})
	}

	// Connection failure (dead port) is transient.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()
	h := &HTTPEnroller{BaseURL: dead}
	_, err := h.Enroll(context.Background(), newTestEnrollReq())
	var he *httpEnrollError
	if !errors.As(err, &he) || !he.transient {
		t.Errorf("dead-port error = %v, want transient *httpEnrollError", err)
	}
}

// TestEnsureEnrolled_HTTPPreferred proves the operator-origin enroll is tried
// FIRST and, on success, the plain gRPC enroller is never touched.
func TestEnsureEnrolled_HTTPPreferred(t *testing.T) {
	store := tmpStore(t)
	grpc := &fakeEnroller{devID: "dev-grpc", jwt: "jwt-grpc"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"dev-http","jwt":"jwt-http"}`))
	}))
	defer srv.Close()
	a := New(grpc, store, testFacts(), "bt-token", WithHTTPEenroller(&HTTPEnroller{BaseURL: srv.URL}))
	res, err := a.EnsureEnrolled(context.Background())
	if err != nil {
		t.Fatalf("EnsureEnrolled: %v", err)
	}
	if res.Identity.DeviceID != "dev-http" {
		t.Errorf("device = %q, want the HTTP-minted dev-http (not the gRPC fallback)", res.Identity.DeviceID)
	}
	if grpc.calls != 0 {
		t.Errorf("gRPC enroller called %d times, want 0 (HTTP path should win)", grpc.calls)
	}
}

// TestEnsureEnrolled_HTTPTransientFallsBackToGRPC proves that a TRANSIENT HTTP
// failure (5xx) falls back to the plain gRPC channel — the dev / split-port
// story where the operator origin is down but the plain gRPC port is up.
func TestEnsureEnrolled_HTTPTransientFallsBackToGRPC(t *testing.T) {
	store := tmpStore(t)
	grpc := &fakeEnroller{devID: "dev-grpc", jwt: "jwt-grpc"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := New(grpc, store, testFacts(), "bt-token", WithHTTPEenroller(&HTTPEnroller{BaseURL: srv.URL}))
	res, err := a.EnsureEnrolled(context.Background())
	if err != nil {
		t.Fatalf("EnsureEnrolled: %v", err)
	}
	if res.Identity.DeviceID != "dev-grpc" {
		t.Errorf("device = %q, want the gRPC-fallback dev-grpc", res.Identity.DeviceID)
	}
	if grpc.calls != 1 {
		t.Errorf("gRPC enroller called %d times, want 1 (after the transient HTTP 500)", grpc.calls)
	}
}

// TestEnsureEnrolled_HTTPDefinitiveNoFallback proves a DEFINITIVE HTTP failure
// (4xx) does NOT fall back to gRPC — the server already answered (bad/unknown
// token) and the gRPC channel would just repeat the refusal.
func TestEnsureEnrolled_HTTPDefinitiveNoFallback(t *testing.T) {
	store := tmpStore(t)
	grpc := &fakeEnroller{devID: "dev-grpc", jwt: "jwt-grpc"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"unknown or already-used bootstrap token"}`))
	}))
	defer srv.Close()
	a := New(grpc, store, testFacts(), "bt-bad", WithHTTPEenroller(&HTTPEnroller{BaseURL: srv.URL}))
	_, err := a.EnsureEnrolled(context.Background())
	if err == nil {
		t.Fatal("expected an error (definitive 4xx), got nil")
	}
	if grpc.calls != 0 {
		t.Errorf("gRPC enroller called %d times, want 0 (definitive 4xx must not fall back)", grpc.calls)
	}
	if !strings.Contains(err.Error(), "unknown or already-used bootstrap token") {
		t.Errorf("error = %q, want it to carry the server's 4xx message", err.Error())
	}
}
