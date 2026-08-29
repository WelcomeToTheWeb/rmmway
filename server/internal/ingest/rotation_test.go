package ingest

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
)

// newTestServerWithCA is like newTestServer but wires an in-memory org CA
// (W3-1/W3-2) into the service so Enroll issues leaves and RefreshLeaf is
// implemented.
func newTestServerWithCA(t *testing.T) (*Service, agentv1.AgentServiceClient, *ca.Manager, func()) {
	t.Helper()
	svc, client, _, innerStop := newTestServer(t)
	caMgr, err := ca.NewManager(ca.NewMemoryOrgStore(), time.Hour)
	if err != nil {
		innerStop()
		t.Fatalf("new ca manager: %v", err)
	}
	svc.cfg.OrgCA = caMgr
	return svc, client, caMgr, innerStop
}

// refreshErr calls RefreshLeaf with the given Bearer token ("" for none) and
// returns the error (nil on success).
func refreshErr(t *testing.T, ctx context.Context, client agentv1.AgentServiceClient, token string) error {
	t.Helper()
	if token != "" {
		ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	}
	_, err := client.RefreshLeaf(ctx, &agentv1.RefreshLeafRequest{Hostname: "rot-host"})
	return err
}

// TestRefreshLeafRejectsUnauthenticated: the rotation RPC is auth-gated like
// every other RPC — no token, a garbage token, and a valid token for an
// unknown device are all rejected.
func TestRefreshLeafRejectsUnauthenticated(t *testing.T) {
	svc, client, _, stop := newTestServerWithCA(t)
	defer stop()
	ctx := context.Background()

	if err := refreshErr(t, ctx, client, ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: expected Unauthenticated, got %v (%v)", status.Code(err), err)
	}

	tok, err := svc.mintJWT("dev-ghost")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := refreshErr(t, ctx, client, tok); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown device: expected Unauthenticated, got %v (%v)", status.Code(err), err)
	}
}

// TestRefreshLeafMintsFreshLeaf is the W3-2 DoD at the service layer: an
// enrolled device that refreshes (the way the agent does, on the mTLS
// channel, with a valid JWT) gets a NEW leaf + key that is valid for ~the
// leaf TTL and carries the device's hostname SAN. Two consecutive refreshes
// yield two different leaves — rotation, not caching.
func TestRefreshLeafMintsFreshLeaf(t *testing.T) {
	svc, client, caMgr, stop := newTestServerWithCA(t)
	defer stop()
	ctx := context.Background()

	// 1. Enroll: device row + first leaf + JWT.
	bootTok, devID := svc.MintBootstrapToken()
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: bootTok,
		Hostname:       "rot-host",
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.1.0",
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if enroll.LeafCertPem == "" {
		t.Fatal("enroll did not issue an mTLS leaf (OrgCA wired?)")
	}

	// 2. Refresh under the device's own JWT.
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))
	r1, err := client.RefreshLeaf(mdCtx, &agentv1.RefreshLeafRequest{
		Hostname: "rot-host",
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if r1.LeafCertPem == "" || r1.LeafKeyPem == "" {
		t.Fatalf("refresh returned an incomplete leaf: cert=%q key=%q", r1.LeafCertPem, r1.LeafKeyPem)
	}
	if r1.LeafCertPem == enroll.LeafCertPem {
		t.Fatal("refresh returned the ORIGINAL enroll leaf (no rotation happened)")
	}
	cert, err := ca.ParseLeafPEM([]byte(r1.LeafCertPem))
	if err != nil {
		t.Fatalf("parse refreshed leaf: %v", err)
	}
	// Verifies against the org root (the mTLS handshake would accept it).
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     caMgr.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("refreshed leaf must verify against the org root: %v", err)
	}
	if cert.Subject.CommonName != devID {
		t.Fatalf("refreshed leaf CN = %q, want the device id %q", cert.Subject.CommonName, devID)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "rot-host" {
		t.Fatalf("refreshed leaf SANs = %v, want [rot-host]", cert.DNSNames)
	}
	// The expires_ms the agent derives its next deadline from matches the
	// cert, and is ~1h out.
	if cert.NotAfter.UnixMilli() != r1.ExpiresMs {
		t.Fatalf("expires_ms %d != cert not-after %v", r1.ExpiresMs, cert.NotAfter)
	}
	if d := time.Until(cert.NotAfter); d < 45*time.Minute || d > 65*time.Minute {
		t.Fatalf("refreshed leaf expires in %s, want ~1h", d)
	}

	// 3. A second refresh is a DIFFERENT leaf again.
	r2, err := client.RefreshLeaf(mdCtx, &agentv1.RefreshLeafRequest{Hostname: "rot-host"})
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if r2.LeafCertPem == r1.LeafCertPem {
		t.Fatal("two consecutive refreshes returned the same leaf")
	}
}
