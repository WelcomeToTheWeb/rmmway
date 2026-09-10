// Package secure builds the agent's gRPC transport credentials.
//
// W3-1 mTLS: once the agent has its persisted mTLS identity (leaf cert + key
// + the org root), it connects to the server's mTLS gRPC port presenting its
// leaf and trusting ONLY the org root (so it verifies the server too). Before
// enroll it has no material and uses a plain insecure transport for the one
// bootstrap Enroll call.
package secure

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TLSIdentity is the mTLS material shape secure needs. It is satisfied by
// *enroll.TLSIdentity (which has KeyPair/RootCAs/Valid with matching
// signatures) so this package does not have to import enroll — keeping the
// transport layer generic and testable with synthetic certs.
type TLSIdentity interface {
	// KeyPair returns the client's leaf cert + key.
	KeyPair() (tls.Certificate, error)
	// RootCAs returns the trust-anchor pool (the org root).
	RootCAs() (*x509.CertPool, error)
	// Valid reports whether the identity is complete.
	Valid() bool
}

// Credentials wraps a persisted TLS identity and mints transport credentials
// for the mTLS channel.
type Credentials struct {
	identity TLSIdentity
}

// New wraps a persisted TLS identity.
func New(identity TLSIdentity) *Credentials {
	return &Credentials{identity: identity}
}

// Identity returns the wrapped identity (for callers that need to re-derive).
func (c *Credentials) Identity() TLSIdentity { return c.identity }

// TransportCredentials returns credentials.TransportCredentials for the mTLS
// channel: it presents keypair (the device's leaf) and verifies the server's
// certificate against the pinned org root. serverName pins the hostname used
// for verification — it must match a SAN on the server's certificate (empty
// falls back to the dial target's host).
//
// W3-2: the client leaf is read through GetClientCertificate at EACH
// handshake, so the rotator's in-place leaf swap (TLSIdentity.SwapLeaf) is
// picked up automatically on the next connection — no channel rebuild, no
// downtime. In-flight connections keep the cert they negotiated.
func (c *Credentials) TransportCredentials(serverName string) (credentials.TransportCredentials, error) {
	cfg, err := c.TLSConfig(serverName)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

// TLSConfig returns the underlying *tls.Config for the mTLS channel. It is
// exposed (beyond TransportCredentials) so callers and tests can drive the
// real handshake semantics — in particular to observe that GetClientCertificate
// re-reads the leaf on every handshake (W3-2 rotation).
func (c *Credentials) TLSConfig(serverName string) (*tls.Config, error) {
	if c.identity == nil || !c.identity.Valid() {
		return nil, fmt.Errorf("secure: incomplete mTLS identity (need leaf cert, leaf key, and org root)")
	}
	if _, err := c.identity.KeyPair(); err != nil {
		return nil, fmt.Errorf("secure: load leaf keypair: %w", err)
	}
	roots, err := c.identity.RootCAs()
	if err != nil {
		return nil, fmt.Errorf("secure: load org root: %w", err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			// Re-read on every handshake: the rotator may have swapped the
			// leaf since the channel was built.
			kp, err := c.identity.KeyPair()
			if err != nil {
				return nil, err
			}
			return &kp, nil
		},
		RootCAs: roots,
	}
	if serverName != "" {
		cfg.ServerName = serverName
	}
	return cfg, nil
}

// Insecure is the plain transport used only for the initial bootstrap Enroll
// (the agent has no mTLS material yet).
func Insecure() credentials.TransportCredentials {
	return insecure.NewCredentials()
}
