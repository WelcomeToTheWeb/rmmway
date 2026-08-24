package ca

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"
)

// Manager owns the org PKI for the server's lifetime: it loads (or generates
// and persists) the org root at boot, issues device leaves at enroll, and
// issues the server cert the mTLS listener serves. It is safe for concurrent
// use.
type Manager struct {
	store OrgStore
	ttl   time.Duration

	mu     sync.Mutex
	root   *Root
	server *tls.Certificate // lazily built from a server cert issued by the root
}

// NewManager loads the persisted org root from store, generating + persisting
// a fresh one when the store is empty (first boot). ttl is the default leaf
// lifetime (0 -> package default).
func NewManager(store OrgStore, ttl time.Duration) (*Manager, error) {
	if store == nil {
		store = NewMemoryOrgStore()
	}
	if ttl <= 0 {
		ttl = leafTTL
	}
	m := &Manager{store: store, ttl: ttl}
	certPEM, keyPEM, err := store.LoadRoot(context.Background())
	if err != nil {
		return nil, fmt.Errorf("ca: load root: %w", err)
	}
	if certPEM == nil {
		root, err := GenerateRoot()
		if err != nil {
			return nil, err
		}
		if err := store.SaveRoot(context.Background(), root.CertPEM(), root.KeyPEM()); err != nil {
			return nil, fmt.Errorf("ca: persist fresh root: %w", err)
		}
		m.root = root
	} else {
		root, err := RootFromPEM(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("ca: restore root: %w", err)
		}
		m.root = root
	}
	return m, nil
}

// Root returns the org root (never nil after a successful NewManager).
func (m *Manager) Root() *Root { return m.root }

// RootCertPEM returns the org root CA certificate (PEM) — the trust anchor a
// fresh agent pins so it can verify the server on the mTLS channel.
func (m *Manager) RootCertPEM() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root.CertPEM()
}

// IssueDevice signs a leaf for deviceID (hostname as a SAN) and records it.
// It returns the leaf cert + key (PEM) plus the org root (PEM) so the enroll
// response can hand the agent its full mTLS identity in one round-trip.
func (m *Manager) IssueDevice(ctx context.Context, deviceID, hostname string) (leafCert, leafKey, rootCA []byte, err error) {
	leafCert, leafKey, err = m.root.IssueLeaf(deviceID, hostname, m.ttl)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := m.store.SaveLeaf(ctx, deviceID, leafCert, leafKey); err != nil {
		// Record-keeping failure must not block enroll: the agent already has
		// its material. Log-style return (caller decides).
		return nil, nil, nil, fmt.Errorf("ca: record leaf: %w", err)
	}
	return leafCert, leafKey, m.RootCertPEM(), nil
}

// ServerCert returns a *tls.Certificate for the mTLS listener, signed by the
// org root and carrying the given SAN names (hostnames / IPs). It is cached:
// the same server cert is served for the process's lifetime (a restart mints
// a fresh one — acceptable for W3-1; rotation is W3-2).
func (m *Manager) ServerCert(names []string) (*tls.Certificate, error) {
	m.mu.Lock()
	if m.server != nil {
		defer m.mu.Unlock()
		return m.server, nil
	}
	defer m.mu.Unlock()
	certPEM, keyPEM, err := m.root.IssueServerCert(names, m.ttl)
	if err != nil {
		return nil, err
	}
	sc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: server keypair: %w", err)
	}
	m.server = &sc
	return m.server, nil
}

// TLSConfig builds the tls.Config for the mTLS gRPC listener: it serves the
// server cert and requires a client cert signed by the org root.
func (m *Manager) TLSConfig(names []string) (*tls.Config, error) {
	sc, err := m.ServerCert(names)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{*sc},
		ClientCAs:    m.root.CertPool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}
