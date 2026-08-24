package secure

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// ---- tiny self-contained CA for tests (no dependency on the server module) ----

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
		NotAfter:              time.Now().Add(time.Hour),
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

func caRootPEM(ca *testCA) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// leaf signs a client-auth leaf; returns PEMs shaped like enroll's response.
func (c *testCA) leaf(t *testing.T, cn, san string) (certPEM, keyPEM string) {
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
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	derKey, _ := x509.MarshalPKCS8PrivateKey(key)
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: derKey}))
	return certPEM, keyPEM
}

// testIdentity satisfies TLSIdentity with in-memory PEM fields.
type testIdentity struct {
	leafCertPEM, leafKeyPEM, orgRootPEM string
}

func (i *testIdentity) KeyPair() (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(i.leafCertPEM), []byte(i.leafKeyPEM))
}

func (i *testIdentity) RootCAs() (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM([]byte(i.orgRootPEM)) {
		return nil, errors.New("no PEM block in org root")
	}
	return p, nil
}

func (i *testIdentity) Valid() bool {
	return i.leafCertPEM != "" && i.leafKeyPEM != "" && i.orgRootPEM != ""
}

func newTestIdentity(t *testing.T, cn, san string, ca *testCA) *testIdentity {
	t.Helper()
	certPEM, keyPEM := ca.leaf(t, cn, san)
	return &testIdentity{leafCertPEM: certPEM, leafKeyPEM: keyPEM, orgRootPEM: string(caRootPEM(ca))}
}

// clientTLS builds the same tls.Config secure.TransportCredentials produces,
// so the test drives the real handshake semantics.
func clientTLS(t *testing.T, identity TLSIdentity, serverName string) *tls.Config {
	t.Helper()
	kp, err := identity.KeyPair()
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	roots, err := identity.RootCAs()
	if err != nil {
		t.Fatalf("roots: %v", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{kp},
		RootCAs:      roots,
		ServerName:   serverName,
	}
}

// ---- a TLS listener that requires + verifies a client leaf (org-root only) ----

func startTLSListener(t *testing.T, clientCAs *x509.CertPool, serverCert tls.Certificate) string {
	t.Helper()
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = c.Read(make([]byte, 16))
				_ = c.Close()
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestMTLSValidLeafAcceptedRandomRejected is the W3-1 DoD at the transport
// layer: a leaf signed by the pinned org root completes the mTLS handshake;
// a leaf from a DIFFERENT root (a "random" / external CA) is rejected by the
// server, which only trusts its own org root as client CA.
func TestMTLSValidLeafAcceptedRandomRejected(t *testing.T) {
	org := newTestCA(t, "RMMWay Org Root CA (test)")
	rogue := newTestCA(t, "Some Other Org CA")

	// A server cert the client verifies against the org root.
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	srvDer, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: srvSerial,
		Subject:      pkix.Name{CommonName: "rmmway-server"},
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, org.cert, &srvKey.PublicKey, org.key)
	if err != nil {
		t.Fatalf("server cert: %v", err)
	}
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDer})
	srvKeyDer, _ := x509.MarshalPKCS8PrivateKey(srvKey)
	srvCert, err := tls.X509KeyPair(srvCertPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDer}))
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caRootPEM(org))
	target := startTLSListener(t, pool, srvCert)

	dial := func(identity TLSIdentity) error {
		conn, err := tls.Dial("tcp", target, clientTLS(t, identity, "127.0.0.1"))
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}

	if err := dial(newTestIdentity(t, "dev-001", "127.0.0.1", org)); err != nil {
		t.Fatalf("a leaf from the org root must complete the mTLS handshake, got %v", err)
	}
	if err := dial(newTestIdentity(t, "dev-rogue", "127.0.0.1", rogue)); err == nil {
		t.Fatal("a leaf from a DIFFERENT CA must be rejected (random-cert DoD)")
	}
}

// TestTransportCredentialsIncompleteIdentity: an identity missing any of the
// three PEMs must not produce transport credentials (no half-configured mTLS).
func TestTransportCredentialsIncompleteIdentity(t *testing.T) {
	org := newTestCA(t, "root")
	full := newTestIdentity(t, "dev-001", "127.0.0.1", org)

	cases := map[string]TLSIdentity{
		"nil":      nil,
		"no root":  &testIdentity{leafCertPEM: full.leafCertPEM, leafKeyPEM: full.leafKeyPEM},
		"no key":   &testIdentity{leafCertPEM: full.leafCertPEM, orgRootPEM: full.orgRootPEM},
		"no leaf":  &testIdentity{leafKeyPEM: full.leafKeyPEM, orgRootPEM: full.orgRootPEM},
	}
	for name, id := range cases {
		if _, err := New(id).TransportCredentials("127.0.0.1"); err == nil {
			t.Errorf("%s identity: expected an error, got nil", name)
		}
	}
	// A complete identity produces usable credentials.
	if _, err := New(full).TransportCredentials("127.0.0.1"); err != nil {
		t.Fatalf("complete identity: expected credentials, got %v", err)
	}
}
