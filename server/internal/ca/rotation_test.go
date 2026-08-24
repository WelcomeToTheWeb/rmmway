package ca

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// TestRefreshLeafReissuesUnderSameRoot proves the W3-2 server half: a
// RefreshLeaf for an enrolled device produces a NEW leaf (new key, new
// serial) that still verifies against the SAME org root, carries the device's
// identity + hostname SAN, and is recorded in the store (the audit trail of
// the rotation).
func TestRefreshLeafReissuesUnderSameRoot(t *testing.T) {
	store := NewMemoryOrgStore()
	m, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	// Enroll the device (W3-1 path).
	leaf0, key0, _, err := m.IssueDevice(context.Background(), "dev-rot", "rot-host")
	if err != nil {
		t.Fatalf("issue device: %v", err)
	}
	// Rotate it (W3-2 path).
	leaf1, key1, expires, err := m.RefreshLeaf(context.Background(), "dev-rot", "rot-host")
	if err != nil {
		t.Fatalf("refresh leaf: %v", err)
	}
	if string(leaf1) == string(leaf0) || string(key1) == string(key0) {
		t.Fatal("refresh must mint a NEW leaf + key (rotation, not cache)")
	}
	// The refreshed leaf must verify against the same org root.
	c1, err := ParseLeafPEM(leaf1)
	if err != nil {
		t.Fatalf("parse refreshed leaf: %v", err)
	}
	if _, err := c1.Verify(x509.VerifyOptions{
		Roots:     m.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("refreshed leaf must verify against the org root: %v", err)
	}
	// Identity is preserved: same CN (device id) + hostname SAN.
	if c1.Subject.CommonName != "dev-rot" {
		t.Fatalf("refreshed leaf CN = %q, want dev-rot", c1.Subject.CommonName)
	}
	if len(c1.DNSNames) != 1 || c1.DNSNames[0] != "rot-host" {
		t.Fatalf("refreshed leaf SANs = %v, want [rot-host]", c1.DNSNames)
	}
	// Expiry: the cert's not-after is issued at now+ttl (its NotBefore is
	// backdated 1h for clock skew, so the total span is ttl+1h).
	if !expires.Equal(c1.NotAfter) {
		t.Fatalf("reported expires %v != cert not-after %v", expires, c1.NotAfter)
	}
	if d := time.Until(c1.NotAfter); d > time.Hour+time.Minute || d < time.Hour-time.Minute {
		t.Fatalf("refreshed leaf expires in %s, want ~1h (the leaf TTL)", d)
	}
	// Store recorded the NEW leaf (rotation is auditable).
	storedCert, _, ok := store.Leaf("dev-rot")
	if !ok {
		t.Fatal("refreshed leaf not recorded in the store")
	}
	if string(storedCert) != string(leaf1) {
		t.Fatal("store does not hold the refreshed leaf")
	}
}

// TestServerCertRotatesInPlace is the W3-2 no-downtime half on the server:
// the mTLS listener's tls.Config serves the manager's server cert through a
// GetCertificate hook. When the current cert enters its rotation window, the
// hook hands out a FRESH root-signed server cert — the listener itself is
// never touched. The test proves (a) the served cert always verifies against
// the org root, (b) forcing a rotation actually swaps the served cert (new
// serial), and (c) a live TLS handshake completes across the rotation.
func TestServerCertRotatesInPlace(t *testing.T) {
	store := NewMemoryOrgStore()
	m, err := NewManager(store, time.Minute)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	names := []string{"localhost", "127.0.0.1"}
	cfg, err := m.TLSConfig(names)
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}
	if cfg.GetCertificate == nil {
		t.Fatal("W3-2 requires the server cert to be served via GetCertificate (in-place rotation)")
	}

	// A client leaf for the handshake (any org-issued leaf works).
	leafPEM, leafKeyPEM, err := m.Root().IssueLeaf("dev-c1", "localhost", time.Hour)
	if err != nil {
		t.Fatalf("client leaf: %v", err)
	}
	kp, err := tls.X509KeyPair(leafPEM, leafKeyPEM)
	if err != nil {
		t.Fatalf("client keypair: %v", err)
	}
	clientCfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{kp},
		RootCAs:      m.Root().CertPool(),
		ServerName:   "localhost",
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, cfg)
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			// Drain + close; the handshake is what's under test.
			go func(c net.Conn) {
				c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				buf := make([]byte, 8)
				_, _ = c.Read(buf)
				c.Close()
			}(c)
		}
	}()
	defer tlsLn.Close()

	handshake := func() *x509.Certificate {
		nc, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conn := tls.Client(nc, clientCfg)
		if err := conn.Handshake(); err != nil {
			nc.Close()
			t.Fatalf("handshake: %v", err)
		}
		defer conn.Close()
		state := conn.ConnectionState()
		return state.PeerCertificates[0]
	}

	// 1. First handshake: the served server cert verifies against the org root.
	served1 := handshake()
	if _, err := served1.Verify(x509.VerifyOptions{
		Roots:     m.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:   "localhost",
	}); err != nil {
		t.Fatalf("served server cert must verify against the org root: %v", err)
	}

	// 2. Force the current server cert into its rotation window. The rotation
	// threshold is min(ttl/5, 1h); the cert was issued with a 1h life, so
	// raising ttl makes the threshold cap at 1h — which the (just-under-1h)
	// remaining life is now below, so the hook re-issues. The served cert
	// must change (new serial) while the SAME listener keeps answering.
	m.mu.Lock()
	m.ttl = 24 * time.Hour
	m.mu.Unlock()
	served2 := handshake()
	if _, err := served2.Verify(x509.VerifyOptions{
		Roots:     m.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:   "localhost",
	}); err != nil {
		t.Fatalf("post-rotation server cert must verify against the org root: %v", err)
	}
	if served1.SerialNumber.Cmp(served2.SerialNumber) == 0 {
		t.Fatal("the server cert was NOT rotated in place (same serial served twice)")
	}

	// 3. A third handshake right after still completes — the listener never
	// stopped serving (the "no downtime" property).
	_ = handshake()
}
