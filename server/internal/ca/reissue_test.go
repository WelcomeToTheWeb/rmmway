package ca

import (
	"context"
	"crypto/x509"
	"testing"
	"time"
)

// TestGenerateRootNamed proves the A-2 wizard's org name is stamped into the
// root's Subject (Organization), while the CN stays the stable trust-anchor
// name. An empty org falls back to the default.
func TestGenerateRootNamed(t *testing.T) {
	root, err := GenerateRootNamed("Acme Corp")
	if err != nil {
		t.Fatalf("generate named root: %v", err)
	}
	cert, err := parseCertPEM(root.CertPEM())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cert.Subject.CommonName != orgCN {
		t.Fatalf("CN should stay %q, got %q", orgCN, cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != "Acme Corp" {
		t.Fatalf("Organization should be [Acme Corp], got %v", cert.Subject.Organization)
	}
	if root.OrgName() != "Acme Corp" {
		t.Fatalf("OrgName() = %q, want Acme Corp", root.OrgName())
	}

	def, err := GenerateRootNamed("")
	if err != nil {
		t.Fatalf("generate default root: %v", err)
	}
	if def.OrgName() != orgDefault {
		t.Fatalf("empty org should default to %q, got %q", orgDefault, def.OrgName())
	}
}

// TestManagerReissueRoot proves the wizard's re-issue: the manager swaps in a
// fresh root under the org name, persists it (a restart restores the NEW root,
// not the boot default), and a leaf from the new root verifies against the new
// pool but not the old one (different key pair).
func TestManagerReissueRoot(t *testing.T) {
	store := NewMemoryOrgStore()
	ctx := context.Background()

	m, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// Capture the boot root (cert + key) before the wizard runs.
	bootCertPEM, bootKeyPEM, err := store.LoadRoot(ctx)
	if err != nil || bootCertPEM == nil {
		t.Fatalf("load boot root: %v", err)
	}
	bootRoot, err := RootFromPEM(bootCertPEM, bootKeyPEM)
	if err != nil {
		t.Fatalf("parse boot root: %v", err)
	}
	if m.Root().OrgName() != orgDefault {
		t.Fatalf("boot root org = %q, want default", m.Root().OrgName())
	}

	// The wizard re-issues the root under the org name.
	if err := m.ReissueRoot(ctx, "Acme Corp"); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if m.Root().OrgName() != "Acme Corp" {
		t.Fatalf("post-reissue org = %q, want Acme Corp", m.Root().OrgName())
	}
	if string(m.RootCertPEM()) == string(bootCertPEM) {
		t.Fatal("re-issue did not change the root cert")
	}

	// A leaf signed by the NEW root verifies against the new pool but not the
	// old boot root's pool (the keys differ).
	newLeaf, _, _, err := m.IssueDevice(ctx, "dev-new", "host-new")
	if err != nil {
		t.Fatalf("issue post-reissue leaf: %v", err)
	}
	lp, _ := parseCertPEM(newLeaf)
	if _, err := lp.Verify(x509.VerifyOptions{
		Roots:     m.Root().CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("post-reissue leaf must verify against the new root, got %v", err)
	}
	if _, err := lp.Verify(x509.VerifyOptions{
		Roots:     bootRoot.CertPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Fatal("post-reissue leaf should NOT verify against the old boot root (different key)")
	}

	// Restart: a new manager restores the RE-ISSUED root (with the org name),
	// not the boot default.
	m2, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if m2.Root().OrgName() != "Acme Corp" {
		t.Fatalf("restart should restore the re-issued root (org %q), got %q", "Acme Corp", m2.Root().OrgName())
	}
	if string(m2.RootCertPEM()) != string(m.RootCertPEM()) {
		t.Fatal("restart restored a different root than the re-issued one")
	}
}

// TestManagerTLSConfigPicksUpReissuedRoot proves the mTLS listener's client
// trust pool is dynamic: the tls.Config is built ONCE at boot (as the real
// listener does), then the wizard re-issues the root, and the SAME config must
// hand out the new trust pool on the next handshake.
func TestManagerTLSConfigPicksUpReissuedRoot(t *testing.T) {
	store := NewMemoryOrgStore()
	ctx := context.Background()
	m, err := NewManager(store, time.Hour)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	cfg, err := m.TLSConfig([]string{"localhost"})
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}

	// The pool a handshake would use BEFORE the re-issue.
	pre, err := cfg.GetConfigForClient(nil)
	if err != nil || pre == nil || pre.ClientCAs == nil {
		t.Fatalf("pre-reissue GetConfigForClient: %v", err)
	}

	if err := m.ReissueRoot(ctx, "Acme Corp"); err != nil {
		t.Fatalf("reissue: %v", err)
	}

	// AFTER the re-issue, the SAME tls.Config must hand out the new pool.
	post, err := cfg.GetConfigForClient(nil)
	if err != nil || post == nil || post.ClientCAs == nil {
		t.Fatalf("post-reissue GetConfigForClient: %v", err)
	}

	// A cert signed by the new root is trusted by the post pool, not the pre
	// pool (this is exactly what the handshake enforces).
	leaf, _, err := m.Root().IssueLeaf("dev-x", "host-x", time.Hour)
	if err != nil {
		t.Fatalf("issue leaf: %v", err)
	}
	lp, _ := parseCertPEM(leaf)
	if _, err := lp.Verify(x509.VerifyOptions{
		Roots:     post.ClientCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("post-reissue pool must trust a new-root leaf, got %v", err)
	}
	if _, err := lp.Verify(x509.VerifyOptions{
		Roots:     pre.ClientCAs,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err == nil {
		t.Fatal("pre-reissue pool must NOT trust a new-root leaf")
	}
}
