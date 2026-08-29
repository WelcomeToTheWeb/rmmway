package ca

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"
)

// parseCertPEM decodes a single CERTIFICATE PEM block into a parsed cert.
func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// TestRootRoundTrip is the core W3-1 DoD at the crypto layer: a leaf issued by
// the org root verifies against the root's pool; a leaf from a *different*
// root (a "random" / external CA) does not. This is exactly what the mTLS
// handshake enforces on the wire.
func TestRootRoundTrip(t *testing.T) {
	org, err := GenerateRoot()
	if err != nil {
		t.Fatalf("generate org root: %v", err)
	}
	other, err := GenerateRoot() // stands in for an attacker's CA
	if err != nil {
		t.Fatalf("generate other root: %v", err)
	}

	leafPEM, _, err := org.IssueLeaf("dev-001", "fileserver-01", time.Hour)
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	leaf, err := parseCertPEM(leafPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     org.CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("org-issued leaf must verify against the org root, got %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     other.CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Fatal("a leaf from a different CA must NOT verify (the 'random cert rejected' DoD)")
	}
}

// TestServerCertVerifiesAgainstRoot proves the server cert a listener serves
// chains to the org root — so an agent that pins the root genuinely verifies
// the server (mutual, not one-way, trust).
func TestServerCertVerifiesAgainstRoot(t *testing.T) {
	org, err := GenerateRoot()
	if err != nil {
		t.Fatalf("generate org root: %v", err)
	}
	certPEM, keyPEM, err := org.IssueServerCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	srv, err := parseCertPEM(certPEM)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	if _, err := srv.Verify(x509.VerifyOptions{
		Roots:     org.CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSName:   "localhost",
	}); err != nil {
		t.Fatalf("server cert must verify against the org root (localhost SAN), got %v", err)
	}
}

// TestManagerBootPersistsAndRestartsRoot proves a server restart reuses the
// same org root (so existing devices' leaves stay valid) instead of minting a
// new one that would orphan every enrolled device.
func TestManagerBootPersistsAndRestartsRoot(t *testing.T) {
	store := NewMemoryOrgStore()

	m1, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	firstRoot := m1.RootCertPEM()
	leaf1, _, _, err := m1.IssueDevice(context.Background(), "dev-persist", "host-a")
	if err != nil {
		t.Fatalf("issue device: %v", err)
	}

	// Simulate a restart: a brand-new manager over the same store.
	m2, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if string(m2.RootCertPEM()) != string(firstRoot) {
		t.Fatal("restart minted a NEW org root — existing devices' certs would be orphaned")
	}

	// The leaf issued before the restart must still verify after it.
	leaf, err := parseCertPEM(leaf1)
	if err != nil {
		t.Fatalf("parse pre-restart leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     m2.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("pre-restart leaf must still verify post-restart, got %v", err)
	}
}

// TestIssueDeviceRecordsLeafInStore is the audit half: a device's issued leaf
// is recorded so the server can reissue/inspect it later.
func TestIssueDeviceRecordsLeafInStore(t *testing.T) {
	store := NewMemoryOrgStore()
	m, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, _, _, err := m.IssueDevice(context.Background(), "dev-9", "host-9"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	certPEM, keyPEM, ok := store.Leaf("dev-9")
	if !ok {
		t.Fatal("issued leaf not recorded in the org store")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("recorded leaf is empty")
	}
}
