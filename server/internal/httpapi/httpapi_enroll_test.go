package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// enrollTestServer builds a Server with a scripted Enroll hook so the
// POST /agent/enroll and POST /api/bootstrap handlers can be exercised in
// isolation.
func enrollTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{
		JWTSecret:     []byte("test-secret"),
		AdminUser:     "admin",
		AdminPassword: "s3cret",
		MintBootstrap: func() (string, string) { return "bt-minted", "dev-minted" },
		Enroll: func(_ context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
			switch req.GetBootstrapToken() {
			case "":
				return nil, status.Error(codes.InvalidArgument, "bootstrap_token is required")
			case "bt-good":
				return &agentv1.EnrollResponse{
					DeviceId: "dev-good", Jwt: "jwt-good",
					HeartbeatIntervalS: 30, MetricIntervalS: 30,
					LeafCertPem: "CERT", LeafKeyPem: "KEY", OrgRootCaPem: "ROOT",
				}, nil
			default:
				return nil, status.Error(codes.PermissionDenied, "unknown or already-used bootstrap token")
			}
		},
	})
}

func doEnrollPost(t *testing.T, s *Server, path, body, token string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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

// TestAgentEnrollHTTP proves the open bootstrap-enroll endpoint over the
// operator origin: a valid token mints the identity (+ mTLS PEMs), a missing
// token is a 400, an unknown token is a 403.
func TestAgentEnrollHTTP(t *testing.T) {
	s := enrollTestServer(t)

	// Missing bootstrap_token -> 400 (definitive: the agent will NOT fall
	// back to gRPC for a 4xx).
	code, _ := doEnrollPost(t, s, "/agent/enroll", `{"hostname":"h"}`, "")
	if code != http.StatusBadRequest {
		t.Fatalf("missing token: got %d, want 400", code)
	}

	// Unknown token -> 403 with an error body.
	code, body := doEnrollPost(t, s, "/agent/enroll", `{"bootstrap_token":"bt-nope","hostname":"h"}`, "")
	if code != http.StatusForbidden {
		t.Fatalf("unknown token: got %d, want 403", code)
	}
	if body["error"] == "" {
		t.Fatalf("unknown token: missing error body: %v", body)
	}

	// Valid token -> 200 with the identity + mTLS PEMs.
	code, body = doEnrollPost(t, s, "/agent/enroll",
		`{"bootstrap_token":"bt-good","hostname":"h","os":"linux","arch":"amd64","agent_version":"v1","interfaces":["eth0:10.0.0.5/24"]}`, "")
	if code != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200: %v", code, body)
	}
	if body["device_id"] != "dev-good" || body["jwt"] != "jwt-good" {
		t.Fatalf("identity = %v/%v, want dev-good/jwt-good", body["device_id"], body["jwt"])
	}
	if body["leaf_cert_pem"] != "CERT" || body["leaf_key_pem"] != "KEY" || body["org_root_ca_pem"] != "ROOT" {
		t.Fatalf("mTLS PEMs not returned: %v", body)
	}
}

// TestAgentEnrollMethod proves only POST is accepted on /agent/enroll.
func TestAgentEnrollMethod(t *testing.T) {
	s := enrollTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/enroll", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /agent/enroll: got %d, want 405", rec.Code)
	}
}

// TestBootstrapMintAuthGate proves the operator "Add a device" mint is
// auth-gated: no/garbage token -> 401, a valid operator token -> 200 with the
// minted token + pre-allocated device id.
func TestBootstrapMintAuthGate(t *testing.T) {
	s := enrollTestServer(t)

	code, _ := doEnrollPost(t, s, "/api/bootstrap", `{}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", code)
	}

	code, _ = doEnrollPost(t, s, "/api/bootstrap", `{}`, "garbage")
	if code != http.StatusUnauthorized {
		t.Fatalf("garbage token: got %d, want 401", code)
	}

	_, lb := login(t, s, "admin", "s3cret")
	tok, _ := lb["token"].(string)
	if tok == "" {
		t.Fatal("no operator token from login")
	}
	code, body := doEnrollPost(t, s, "/api/bootstrap", `{}`, tok)
	if code != http.StatusOK {
		t.Fatalf("authed mint: got %d, want 200: %v", code, body)
	}
	if body["bootstrap_token"] != "bt-minted" || body["device_id"] != "dev-minted" {
		t.Fatalf("mint = %v/%v, want bt-minted/dev-minted", body["bootstrap_token"], body["device_id"])
	}
}
