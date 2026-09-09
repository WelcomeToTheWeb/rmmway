package httpapi

import (
	"encoding/json"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// handleBootstrap mints a one-time enroll code (W1-3 installer / e2e).
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.mintBootstrap == nil {
		http.Error(w, "bootstrap mint not configured", http.StatusServiceUnavailable)
		return
	}
	tok, devID := s.mintBootstrap()
	writeJSON(w, http.StatusOK, map[string]string{"bootstrap_token": tok, "device_id": devID})
}

// handleBootstrapMint is the auth-gated "Add a device" action: it mints a
// one-time bootstrap token for the operator UI to hand to an installer. Same
// mint as /admin/bootstrap, but only a signed-in operator may call it.
//
//	POST /api/bootstrap   200 {bootstrap_token, device_id}   401 no/bad token
//	503 mint not wired
func (s *Server) handleBootstrapMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.mintBootstrap == nil {
		http.Error(w, "bootstrap mint not configured", http.StatusServiceUnavailable)
		return
	}
	tok, devID := s.mintBootstrap()
	writeJSON(w, http.StatusOK, map[string]string{"bootstrap_token": tok, "device_id": devID})
}

// enrollRequest / enrollResponse are the JSON shape of the bootstrap enroll
// over the operator's HTTPS origin (POST /agent/enroll). They mirror the
// gRPC EnrollRequest/EnrollResponse 1:1 — the agent speaks HTTP here so a
// fresh agent (no mTLS material yet) can join without the internal-only plain
// gRPC bootstrap port; only 443 + the mTLS gRPC port need to be open.
type enrollRequest struct {
	BootstrapToken string   `json:"bootstrap_token"`
	Hostname       string   `json:"hostname"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	AgentVersion   string   `json:"agent_version"`
	Interfaces     []string `json:"interfaces"`
}

type enrollResponse struct {
	DeviceID           string `json:"device_id"`
	JWT                string `json:"jwt"`
	HeartbeatIntervalS int32  `json:"heartbeat_interval_s"`
	MetricIntervalS    int32  `json:"metric_interval_s"`
	LeafCertPem        string `json:"leaf_cert_pem"`
	LeafKeyPem         string `json:"leaf_key_pem"`
	OrgRootCaPem       string `json:"org_root_ca_pem"`
}

// httpStatusFromGRPC maps a gRPC status code (as returned by the ingest
// service's Enroll) onto an HTTP status, so the agent's HTTP enroll can tell
// a definitive business error (4xx — do not retry another channel) from a
// transient one (5xx / connection error — fall back to plain gRPC).
func httpStatusFromGRPC(err error) int {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument:
			return http.StatusBadRequest
		case codes.Unauthenticated, codes.PermissionDenied:
			return http.StatusForbidden
		case codes.NotFound:
			return http.StatusNotFound
		case codes.Unavailable:
			return http.StatusServiceUnavailable
		default:
			return http.StatusInternalServerError
		}
	}
	return http.StatusInternalServerError
}

// handleAgentEnroll is the bootstrap enroll over the operator's HTTPS origin
// (POST /agent/enroll). It reuses the ingest service's Enroll — the exact
// same one-time-token -> identity (device_id + agent JWT) + mTLS-leaf
// issuance as the gRPC RPC — so a remote agent can enroll with only the
// operator origin (443) and the mTLS gRPC port open; the plain gRPC bootstrap
// port stays internal. Open for machine callers (the agent), protected by the
// one-time bootstrap token, like /agent/releases.
//
//	POST /agent/enroll  {bootstrap_token, hostname, os, arch, agent_version, interfaces[]}
//
//	200 {device_id, jwt, leaf_cert_pem, leaf_key_pem, org_root_ca_pem, ...}
//	400 bad body / missing bootstrap_token
//	403 unknown or already-used bootstrap token
//	503 enroll not wired (in-memory mode without an org CA path)
//	500 server error
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.enroll == nil {
		http.Error(w, "enroll not configured", http.StatusServiceUnavailable)
		return
	}
	var in enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req := &agentv1.EnrollRequest{
		BootstrapToken: in.BootstrapToken,
		Hostname:       in.Hostname,
		Os:             in.OS,
		Arch:           in.Arch,
		AgentVersion:   in.AgentVersion,
		Interfaces:     in.Interfaces,
	}
	resp, err := s.enroll(r.Context(), req)
	if err != nil {
		code := httpStatusFromGRPC(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{
		DeviceID:           resp.GetDeviceId(),
		JWT:                resp.GetJwt(),
		HeartbeatIntervalS: resp.GetHeartbeatIntervalS(),
		MetricIntervalS:    resp.GetMetricIntervalS(),
		LeafCertPem:        resp.GetLeafCertPem(),
		LeafKeyPem:         resp.GetLeafKeyPem(),
		OrgRootCaPem:       resp.GetOrgRootCaPem(),
	})
}

// registerEnroll mounts the device-join routes: the auth-gated bootstrap mint, the /admin mirror, and the OPEN agent enroll a remote installer calls.
func registerEnroll(s *Server, mux *http.ServeMux) {
	// C1: /admin/* is the operator surface — every route is auth-gated.
	// (It used to be open "for machine callers", but the only machine
	// endpoints the agent needs are /agent/enroll + /agent/releases/*;
	// minting an enroll token is an operator decision.)
	mux.HandleFunc("/admin/bootstrap", s.rbacRoleGate(s.handleBootstrap, "admin"))
	// "Add a device": the auth-gated mint (the UI mints a token to hand to an
	// installer) and the OPEN bootstrap enroll a remote agent calls over the
	// operator's HTTPS origin (machine caller, like /agent/releases).
	mux.HandleFunc("/api/bootstrap", s.rbacRoleGate(s.handleBootstrapMint, "admin"))
	mux.HandleFunc("/agent/enroll", s.handleAgentEnroll)
}
