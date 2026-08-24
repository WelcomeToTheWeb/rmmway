package rotate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// ---- synthetic CA + leaves (same shape as secure_test's testCA) ----

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign ca: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &testCA{cert: cert, key: key}
}

// leafPEMs signs a client-auth leaf with the given validity window.
func (c *testCA) leafPEMs(t *testing.T, cn, san string, notBefore, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{san},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	derKey, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: derKey}))
}

// fakeIdentity implements Identity with an in-memory leaf pair.
type fakeIdentity struct {
	leafCertPEM, leafKeyPEM string
}

func (f *fakeIdentity) CurrentLeafPEM() []byte { return []byte(f.leafCertPEM) }
func (f *fakeIdentity) SwapLeaf(c, k []byte) {
	f.leafCertPEM, f.leafKeyPEM = string(c), string(k)
}
func (f *fakeIdentity) Valid() bool { return f.leafCertPEM != "" && f.leafKeyPEM != "" }

// fakeRefresher implements Refresher with a scripted response sequence.
type fakeRefresher struct {
	resps   []*agentv1.RefreshLeafResponse
	errs    []error
	calls   int
	hosts   []string
	expires []int64
}

func (f *fakeRefresher) RefreshLeaf(ctx context.Context, in *agentv1.RefreshLeafRequest, _ ...grpc.CallOption) (*agentv1.RefreshLeafResponse, error) {
	i := f.calls
	f.calls++
	f.hosts = append(f.hosts, in.GetHostname())
	f.expires = append(f.expires, in.GetCurrentLeafExpiresMs())
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.resps) {
		return f.resps[i], nil
	}
	return nil, errors.New("fakeRefresher: no scripted response left")
}

// fastCfg gives a Config with all waits shrunk to sub-second for tests.
func fastCfg() Config {
	return Config{
		RotateFrac:    0.25,
		MaxRotateLead: time.Hour, // don't let the 30m cap distort the tiny certs
		MinInterval:   time.Millisecond,
		MaxBackoff:    50 * time.Millisecond,
	}
}

// TestRotatorRefreshesExpiredImmediately is the W3-2 core DoD at the loop
// level: a leaf ALREADY inside its rotation window (5ms of life left) is
// refreshed on the first iteration — the RPC carries the hostname + the
// CURRENT leaf's not-after, and the fresh leaf is swapped into the identity
// (plus persisted).
func TestRotatorRefreshesExpiredImmediately(t *testing.T) {
	ca := newTestCA(t, "Org Root (test)")
	now := time.Now()
	id := &fakeIdentity{}
	id.leafCertPEM, id.leafKeyPEM = ca.leafPEMs(t, "dev-001", "ws-01", now.Add(-time.Hour), now.Add(5*time.Millisecond))

	newCert, newKey := ca.leafPEMs(t, "dev-001", "ws-01", now, now.Add(time.Hour))
	ref := &fakeRefresher{resps: []*agentv1.RefreshLeafResponse{{
		LeafCertPem: newCert,
		LeafKeyPem:  newKey,
		ExpiresMs:   now.Add(time.Hour).UnixMilli(),
	}}}

	persisted := 0
	r := New(ref, id, "dev-001", "ws-01", fastCfg(), WithPersist(func() error {
		persisted++
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Run should return ctx.Err() after the first successful refresh because
	// the deadline (3s) expires while waiting out the fresh 1h leaf.
	err := r.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run: expected DeadlineExceeded after the swap, got %v", err)
	}
	if ref.calls != 1 {
		t.Fatalf("expected exactly 1 refresh, got %d", ref.calls)
	}
	if ref.hosts[0] != "ws-01" {
		t.Fatalf("refresh requested SAN %q, want ws-01", ref.hosts[0])
	}
	// The request reports the OLD (near-expired) leaf's not-after — the
	// server cross-checks it.
	wantExp := now.Add(5 * time.Millisecond).UnixMilli()
	if ref.expires[0] < wantExp-2*time.Second.Milliseconds() || ref.expires[0] > wantExp+2000 {
		t.Fatalf("CurrentLeafExpiresMs = %d, want ~%d", ref.expires[0], wantExp)
	}
	if id.leafCertPEM != newCert || id.leafKeyPEM != newKey {
		t.Fatal("identity was not swapped to the refreshed leaf")
	}
	if persisted != 1 {
		t.Fatalf("persist called %d times, want 1", persisted)
	}
}

// TestRotatorWaitsForRotationWindow: a healthy leaf (1h life, far from
// expiry) is NOT refreshed eagerly — the loop waits out its deadline.
func TestRotatorWaitsForRotationWindow(t *testing.T) {
	ca := newTestCA(t, "Org Root (test)")
	now := time.Now()
	origCert, origKey := ca.leafPEMs(t, "dev-001", "ws-01", now.Add(-time.Hour), now.Add(time.Hour))
	id := &fakeIdentity{}
	id.leafCertPEM, id.leafKeyPEM = origCert, origKey

	newCert, newKey := ca.leafPEMs(t, "dev-001", "ws-01", now, now.Add(time.Hour))
	ref := &fakeRefresher{resps: []*agentv1.RefreshLeafResponse{{
		LeafCertPem: newCert, LeafKeyPem: newKey, ExpiresMs: now.Add(2 * time.Hour).UnixMilli(),
	}}}

	r := New(ref, id, "dev-001", "ws-01", fastCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if ref.calls != 0 {
		t.Fatalf("a leaf with an hour left must not be refreshed eagerly; got %d calls", ref.calls)
	}
	if id.leafCertPEM != origCert {
		t.Fatal("identity changed without a refresh")
	}
}

// TestRotatorRetriesAfterFailure: a failed refresh (server down / channel
// dropped) backs off and retries — the second attempt succeeds and the
// rotated leaf lands in the identity. This is the "no expiry gap" property:
// the loop keeps trying until inside the validity window.
func TestRotatorRetriesAfterFailure(t *testing.T) {
	ca := newTestCA(t, "Org Root (test)")
	now := time.Now()
	id := &fakeIdentity{}
	id.leafCertPEM, id.leafKeyPEM = ca.leafPEMs(t, "dev-001", "ws-01", now.Add(-time.Hour), now.Add(5*time.Millisecond))

	newCert, newKey := ca.leafPEMs(t, "dev-001", "ws-01", now, now.Add(time.Hour))
	ref := &fakeRefresher{
		errs: []error{errors.New("connection reset by peer"), nil},
		resps: []*agentv1.RefreshLeafResponse{
			{}, // consumed by the failed first call (response ignored)
			{LeafCertPem: newCert, LeafKeyPem: newKey, ExpiresMs: now.Add(time.Hour).UnixMilli()},
		},
	}

	r := New(ref, id, "dev-001", "ws-01", fastCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.Run(ctx)

	if ref.calls != 2 {
		t.Fatalf("expected 1 failure + 1 success = 2 calls, got %d", ref.calls)
	}
	if id.leafCertPEM != newCert {
		t.Fatal("identity not swapped after the retried refresh")
	}
}

// TestRotatorRejectsIncompleteLeaf: an empty response (cert or key missing)
// is treated as a failure — the OLD leaf is kept, and the loop retries.
func TestRotatorRejectsIncompleteLeaf(t *testing.T) {
	ca := newTestCA(t, "Org Root (test)")
	now := time.Now()
	id := &fakeIdentity{}
	id.leafCertPEM, id.leafKeyPEM = ca.leafPEMs(t, "dev-001", "ws-01", now.Add(-time.Hour), now.Add(5*time.Millisecond))
	orig := id.leafCertPEM

	newCert, _ := ca.leafPEMs(t, "dev-001", "ws-01", now, now.Add(time.Hour))
	ref := &fakeRefresher{
		resps: []*agentv1.RefreshLeafResponse{{
			LeafCertPem: newCert, // key missing
		}},
	}

	r := New(ref, id, "dev-001", "ws-01", fastCfg())
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if id.leafCertPEM != orig {
		t.Fatal("an incomplete refresh response must not replace the current leaf")
	}
	if ref.calls < 2 {
		t.Fatalf("expected the loop to retry after a rejected leaf, got %d calls", ref.calls)
	}
}

// TestThreshold covers the Config.rotateThreshold math: capped at
// MaxRotateLead (so a legacy 24h leaf converges quickly), floored at
// MinInterval (so a tiny-TTL cert still gets room to retry).
func TestThreshold(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		life time.Duration
		want time.Duration
	}{
		{"quarter of 1h", fastCfg(), time.Hour, 15 * time.Minute},
		{"capped at lead", fastCfg(), 24 * time.Hour, time.Hour}, // 6h -> 1h cap
		{"floored at min", Config{RotateFrac: 0.25, MaxRotateLead: time.Hour, MinInterval: time.Minute}, 4*time.Second, time.Minute},
	}
	for _, tc := range cases {
		got := tc.cfg.rotateThreshold(tc.life)
		if got != tc.want {
			t.Errorf("%s: rotateThreshold(%s) = %s, want %s", tc.name, tc.life, got, tc.want)
		}
	}
}
