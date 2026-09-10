package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// http_enroll.go — the bootstrap enroll over the operator's HTTPS origin.
//
// A freshly bootstrapped agent has no mTLS material yet, so it cannot use the
// mTLS gRPC channel. Historically that forced it to enroll over the PLAIN gRPC
// bootstrap port (50051) — which a production deployment keeps internal-only
// (only the mTLS agent port, 50052, is published). HTTPEnroller lets the agent
// enroll over the SAME operator origin it already dials for the API
// (POST {server}/agent/enroll), so a remote machine needs only the operator
// origin (443) and the mTLS gRPC port open — not the internal plain-gRPC port.
//
// The server's /agent/enroll is the exact same one-time-token -> identity
// (device_id + agent JWT) + mTLS-leaf issuance as the gRPC Enroll RPC.

// HTTPEnroller performs the bootstrap enroll over the operator's HTTPS origin.
type HTTPEnroller struct {
	// BaseURL is the operator origin, e.g. "https://rmm.example.com" or
	// "http://localhost:8080". The enroll is POST {BaseURL}/agent/enroll.
	BaseURL string
	// Client is the HTTP client to use (a sensible default is applied when
	// nil).
	Client *http.Client
}

// httpEnrollError carries the underlying error plus whether it is TRANSIENT
// (connection failure / 5xx — worth retrying over the plain gRPC channel) as
// opposed to DEFINITIVE (4xx — the server answered; the gRPC channel would
// give the same verdict, so no point double-attempting with a now-consumed
// token).
type httpEnrollError struct {
	err       error
	transient bool
}

func (e *httpEnrollError) Error() string { return e.err.Error() }
func (e *httpEnrollError) Unwrap() error { return e.err }

// enrollHTTPBody / enrollHTTPOut mirror the server's /agent/enroll JSON
// (which itself mirrors the gRPC EnrollRequest/EnrollResponse 1:1).
type enrollHTTPBody struct {
	BootstrapToken string   `json:"bootstrap_token"`
	Hostname       string   `json:"hostname"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	AgentVersion   string   `json:"agent_version"`
	Interfaces     []string `json:"interfaces"`
}

type enrollHTTPOut struct {
	DeviceID           string `json:"device_id"`
	JWT                string `json:"jwt"`
	HeartbeatIntervalS int32  `json:"heartbeat_interval_s"`
	MetricIntervalS    int32  `json:"metric_interval_s"`
	LeafCertPem        string `json:"leaf_cert_pem"`
	LeafKeyPem         string `json:"leaf_key_pem"`
	OrgRootCaPem       string `json:"org_root_ca_pem"`
}

// Enroll POSTs the host facts + one-time bootstrap token to the operator
// origin and returns the minted identity. The returned error is a
// *httpEnrollError so the caller can decide whether to fall back to the plain
// gRPC channel.
func (h *HTTPEnroller) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(h.BaseURL, "/")
	if base == "" {
		return nil, &httpEnrollError{err: fmt.Errorf("http enroll: no server base URL"), transient: false}
	}
	body, err := json.Marshal(enrollHTTPBody{
		BootstrapToken: req.GetBootstrapToken(),
		Hostname:       req.GetHostname(),
		OS:             req.GetOs(),
		Arch:           req.GetArch(),
		AgentVersion:   req.GetAgentVersion(),
		Interfaces:     req.GetInterfaces(),
	})
	if err != nil {
		return nil, &httpEnrollError{err: fmt.Errorf("encode enroll body: %w", err), transient: false}
	}
	u := base + "/agent/enroll"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, &httpEnrollError{err: fmt.Errorf("build enroll request: %w", err), transient: true}
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	res, err := c.Do(r)
	if err != nil {
		// A network-level failure: the origin was unreachable (DNS, refused,
		// timeout). Transient — the plain gRPC channel may still be up (dev).
		return nil, &httpEnrollError{err: fmt.Errorf("POST %s: %w", u, err), transient: true}
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var out enrollHTTPOut
		if jerr := json.Unmarshal(data, &out); jerr != nil {
			return nil, &httpEnrollError{
				err:       fmt.Errorf("decode enroll response: %w", jerr),
				transient: false,
			}
		}
		return &agentv1.EnrollResponse{
			DeviceId:           out.DeviceID,
			Jwt:                out.JWT,
			HeartbeatIntervalS: out.HeartbeatIntervalS,
			MetricIntervalS:    out.MetricIntervalS,
			LeafCertPem:        out.LeafCertPem,
			LeafKeyPem:         out.LeafKeyPem,
			OrgRootCaPem:       out.OrgRootCaPem,
		}, nil
	}

	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	msg := e.Error
	if msg == "" {
		msg = res.Status
	}
	// 5xx and 404 are TRANSIENT for our purposes: 5xx = the server got the
	// request but failed; 404 = this server predates /agent/enroll (fall back
	// to the plain gRPC Enroll, which is the same logic). 4xx (400/403) = a
	// definitive business answer (bad/unknown token) — falling back to gRPC
	// would just repeat the refusal after the token is consumed.
	return nil, &httpEnrollError{
		err:       fmt.Errorf("enroll %s: %s", res.Status, msg),
		transient: res.StatusCode >= 500 || res.StatusCode == http.StatusNotFound,
	}
}
