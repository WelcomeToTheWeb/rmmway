// Package ca is the server-side org PKI for W3-1 (mTLS agent channel).
//
// The server owns one org root CA. At first boot it generates (and persists,
// via an OrgStore) a self-signed root; from then on every device enrolling
// gets a leaf certificate signed by that root (IssueLeaf), and the mTLS
// listener serves a server certificate also signed by the root (IssueServerCert)
// so the agent can pin the root and verify the server too.
//
// The crypto here is pure (stdlib only) so the chain logic is unit-testable
// without a database; the Postgres-backed OrgStore lives in store.go.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	// orgCN is the CommonName on the org root CA.
	orgCN = "RMMWay Org Root CA"
	// orgDefault is the Subject Organization when no org name is given.
	orgDefault = "RMMWay"
	// rootTTL is how long a generated org root is valid (10y).
	rootTTL = 10 * 365 * 24 * time.Hour
	// leafTTL is the default lifetime of a device leaf. W3-2 makes leaves
	// short-lived (~1h) and rotates them automatically (RefreshLeaf) well
	// inside the window, so the default is the ~1h the task calls for.
	// Overridable at boot via RMMWAY_LEAF_TTL (tests / long dev sessions).
	leafTTL = 1 * time.Hour
)

// Root is the org root CA: a self-signed CA cert + the private key that signs
// everything.
type Root struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

// ---- PEM helpers -------------------------------------------------------------

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

func keyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pemEncode("PRIVATE KEY", der), nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// ---- root lifecycle ----------------------------------------------------------

// GenerateRoot mints a fresh self-signed org root CA (ECDSA P-256, 10y)
// under the default organization name.
func GenerateRoot() (*Root, error) { return GenerateRootNamed(orgDefault) }

// GenerateRootNamed (A-2) mints a fresh self-signed org root CA whose
// Subject carries the organization's name: CN stays "RMMWay Org Root CA",
// Organization becomes orgName (orgName empty -> the default "RMMWay").
// The first-boot setup wizard uses this so the operator's org is stamped
// into the trust anchor every agent pins.
func GenerateRootNamed(orgName string) (*Root, error) {
	if orgName == "" {
		orgName = orgDefault
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("root key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: orgCN, Organization: []string{orgName}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(rootTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("sign root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse root: %w", err)
	}
	kpem, err := keyPEM(key)
	if err != nil {
		return nil, err
	}
	return &Root{cert: cert, key: key, certPEM: pemEncode("CERTIFICATE", der), keyPEM: kpem}, nil
}

// OrgName returns the organization name stamped in the root's Subject
// ("RMMWay" for pre-A-2 roots).

// RootFromPEM reconstructs a Root from its persisted PEM pair.
func RootFromPEM(certPEM, keyPEM []byte) (*Root, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("ca: no PEM block in root cert")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse root cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("ca: no PEM block in root key")
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca: parse root key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("ca: root key is not ECDSA")
	}
	return &Root{cert: cert, key: ec, certPEM: certPEM, keyPEM: keyPEM}, nil
}

// CertPEM returns the root CA certificate (PEM) — what clients pin as a trust
// anchor.
func (r *Root) CertPEM() []byte { return r.certPEM }

// OrgName returns the organization name stamped in the root's Subject
// (A-2: the wizard's org is carried here; "RMMWay" for pre-A-2 roots).
func (r *Root) OrgName() string {
	if len(r.cert.Subject.Organization) > 0 {
		return r.cert.Subject.Organization[0]
	}
	return orgDefault
}

// KeyPEM returns the root signing key (PEM) — persisted, never transmitted.
func (r *Root) KeyPEM() []byte { return r.keyPEM }

// Cert returns the parsed root CA certificate (W3-3: capability-token
// verification needs its ECDSA public key).
func (r *Root) Cert() *x509.Certificate { return r.cert }

// Key returns the root ECDSA private key (W3-3: capability tokens are
// signed with it — the same key that signs device leaves).
func (r *Root) Key() *ecdsa.PrivateKey { return r.key }

// CertPool builds an x509 pool containing just the org root, for use as
// tls.Config.ClientCAs (server) or RootCAs (a client that only trusts us).
func (r *Root) CertPool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(r.certPEM)
	return p
}

// ---- leaf issuance -----------------------------------------------------------

// IssueLeaf signs a device leaf certificate (client-auth) for deviceID.
// hostname, when non-empty, is added as a SAN (DNS or IP, auto-detected).
func (r *Root) IssueLeaf(deviceID, hostname string, ttl time.Duration) (leafCertPEM, leafKeyPEM []byte, err error) {
	if ttl <= 0 {
		ttl = leafTTL
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: deviceID, Organization: []string{"RMMWay", "agents"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if hostname != "" {
		if ip := net.ParseIP(hostname); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, hostname)
		}
	}
	// Pin the issuer's subject key id so chains are verifiable without
	// relying on the (absent) AKID in the self-signed root's extension set.
	tmpl.AuthorityKeyId = r.cert.SubjectKeyId
	if len(tmpl.AuthorityKeyId) == 0 {
		tmpl.AuthorityKeyId = x509v3SubjectKeyId(r.cert)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, r.cert, &key.PublicKey, r.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign leaf: %w", err)
	}
	kpem, err := keyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE", der), kpem, nil
}

// ---- server cert (for the agent to verify us) --------------------------------

// IssueServerCert signs a server certificate for the mTLS listener, with the
// given hostnames / IPs as SANs. The agent pins the org root and verifies this
// cert chains to it, so the TLS handshake is genuinely mutual.
func (r *Root) IssueServerCert(names []string, ttl time.Duration) (certPEMOut, keyPEMOut []byte, err error) {
	if ttl <= 0 {
		ttl = leafTTL
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("server key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	dns, ips := splitNames(names)
	tmpl := &x509.Certificate{
		SerialNumber:   serial,
		Subject:        pkix.Name{CommonName: "rmmway-server", Organization: []string{"RMMWay"}},
		NotBefore:      time.Now().Add(-time.Hour),
		NotAfter:       time.Now().Add(ttl),
		KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:       dns,
		IPAddresses:    ips,
		AuthorityKeyId: x509v3SubjectKeyId(r.cert),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, r.cert, &key.PublicKey, r.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign server cert: %w", err)
	}
	kpem, err := keyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE", der), kpem, nil
}

func splitNames(names []string) (dns []string, ips []net.IP) {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, n)
		}
	}
	return dns, ips
}

// ---- TLS config for the mTLS gRPC server -------------------------------------

// ServerTLSConfig builds the tls.Config for the mTLS gRPC listener: it serves
// serverCert and REQUIRES + verifies a client certificate signed by the org
// root. A client presenting a cert not issued by us fails the handshake before
// any RPC is processed (the W3-1 DoD: a random cert is rejected).
func (r *Root) ServerTLSConfig(serverCertPEM, serverKeyPEM []byte) (*tls.Config, error) {
	sc, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("server keypair: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{sc},
		ClientCAs:    r.CertPool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

// x509v3SubjectKeyId computes the SHA-1 subject key id for a cert whose
// template didn't populate it (used to set AuthorityKeyId on issued certs).
func x509v3SubjectKeyId(c *x509.Certificate) []byte {
	if len(c.SubjectKeyId) > 0 {
		return c.SubjectKeyId
	}
	h := sha1.Sum(c.RawSubjectPublicKeyInfo)
	return h[:]
}
