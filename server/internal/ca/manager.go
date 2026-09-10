package ca

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"sync/atomic"
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

	// W3-2: the server cert is served through a GetCertificate hook that
	// reads this atomic pointer, so it can be ROTATED in place (the listener
	// is never restarted, no handshake in flight is disturbed).
	currentServer atomic.Pointer[tls.Certificate]
	sans          []string
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
// Safe for concurrent use (A-2's ReissueRoot swaps the pointer in place).
func (m *Manager) Root() *Root {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root
}

// ReissueRoot (A-2) generates a FRESH org root under the given organization
// name, persists it (replacing the boot-generated default), and swaps it in
// atomically. The rotating server cert is invalidated so the next mTLS
// handshake re-issues it from the new root, and the trust pool used for
// client-cert verification picks the new root up on the next handshake
// (TLSConfig reads it dynamically). Safe only before any device has
// enrolled — after that, a re-issue would orphan existing leaf certs; the
// caller (the setup service) guards on the device count.
func (m *Manager) ReissueRoot(ctx context.Context, orgName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := GenerateRootNamed(orgName)
	if err != nil {
		return err
	}
	if err := m.store.ReplaceRoot(ctx, root.CertPEM(), root.KeyPEM()); err != nil {
		return fmt.Errorf("ca: persist reissued root: %w", err)
	}
	m.root = root
	m.currentServer.Store(nil)
	return nil
}

// RootCertPEM returns the org root CA certificate (PEM) — the trust anchor a
// fresh agent pins so it can verify the server on the mTLS channel.
func (m *Manager) RootCertPEM() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root.CertPEM()
}

// LeafTTL is the lifetime this manager issues leaves (and its own server
// cert) for. W3-2 makes this the ~1h short-lived window by default
// (overridable via RMMWAY_LEAF_TTL).
func (m *Manager) LeafTTL() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ttl
}

// IssueDevice signs a leaf for deviceID (hostname as a SAN) and records it.
// It returns the leaf cert + key (PEM) plus the org root (PEM) so the enroll
// response can hand the agent its full mTLS identity in one round-trip.
func (m *Manager) IssueDevice(ctx context.Context, deviceID, hostname string) (leafCert, leafKey, rootCA []byte, err error) {
	m.mu.Lock()
	root, ttl := m.root, m.ttl
	m.mu.Unlock()
	leafCert, leafKey, err = root.IssueLeaf(deviceID, hostname, ttl)
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

// RefreshLeaf (W3-2) re-issues deviceID's leaf — a fresh short-lived cert
// signed by the same org root — and records it in the store. It returns the
// new leaf cert + key (PEM) and the new cert's not-after, so the caller (the
// RefreshLeaf RPC handler) can hand the agent its next rotation deadline.
// The org root is NOT returned (the agent already pins it from enroll).
func (m *Manager) RefreshLeaf(ctx context.Context, deviceID, hostname string) (leafCert, leafKey []byte, expiresAt time.Time, err error) {
	m.mu.Lock()
	root, ttl := m.root, m.ttl
	m.mu.Unlock()
	leafCert, leafKey, err = root.IssueLeaf(deviceID, hostname, ttl)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if err := m.store.SaveLeaf(ctx, deviceID, leafCert, leafKey); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("ca: record refreshed leaf: %w", err)
	}
	cert, err := ParseLeafPEM(leafCert)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("ca: parse refreshed leaf: %w", err)
	}
	return leafCert, leafKey, cert.NotAfter, nil
}

// LeafNotAfter parses a leaf cert PEM (as issued by this org root) and
// returns its not-after. Agents use this to seed their first rotation
// deadline from a persisted cert (a cert minted before W3-2, e.g. a 24h W3-1
// leaf, still rotates on its own schedule).
func LeafNotAfter(leafCertPEM []byte) (time.Time, error) {
	c, err := ParseLeafPEM(leafCertPEM)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

// ParseLeafPEM decodes + parses a single PEM-encoded certificate.
func ParseLeafPEM(leafCertPEM []byte) (*x509.Certificate, error) {
	cb, _ := pem.Decode(leafCertPEM)
	if cb == nil {
		return nil, fmt.Errorf("ca: no PEM block in leaf cert")
	}
	return x509.ParseCertificate(cb.Bytes)
}

// ServerCertNow returns the server cert to serve RIGHT NOW, re-issuing it
// when it is (a) not yet minted, or (b) within the last ~20% of its
// validity window (W3-2 rotation: the fresh cert is published atomically to
// the GetCertificate hook, so the mTLS listener keeps serving without a
// restart and in-flight handshakes are undisturbed).
func (m *Manager) ServerCertNow(names []string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ttl := m.ttl
	rot := rotateThreshold(ttl)
	if sc := m.currentServer.Load(); sc != nil {
		// Reuse the current cert unless it is inside its rotation window.
		if sc.Leaf != nil && time.Until(sc.Leaf.NotAfter) > rot {
			return sc, nil
		}
	}
	if m.sans == nil {
		m.sans = names
	}
	certPEM, keyPEM, err := m.root.IssueServerCert(m.sans, ttl)
	if err != nil {
		return nil, err
	}
	sc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca: server keypair: %w", err)
	}
	cert := &sc
	m.currentServer.Store(cert)
	m.server = cert // back-compat cache (ServerCert callers)
	return cert, nil
}

// ServerCert is the W3-1 API: return a *tls.Certificate for the mTLS
// listener, signed by the org root and carrying the given SAN names. It
// delegates to ServerCertNow, which rotates it in place as it nears expiry
// (W3-2).
func (m *Manager) ServerCert(names []string) (*tls.Certificate, error) {
	return m.ServerCertNow(names)
}

// rotateThreshold is how long before a cert's not-after rotation should
// happen: the smaller of ~20% of the cert's lifetime and 1h, so a 1h leaf
// rotates ~12m before expiry and a short test cert rotates promptly too.
func rotateThreshold(ttl time.Duration) time.Duration {
	frac := ttl / 5
	if frac <= 0 || ttl < time.Minute {
		// Tiny TTLs (tests) rotate as soon as they are due.
		return 0
	}
	if frac > time.Hour {
		frac = time.Hour
	}
	return frac
}

// TLSConfig builds the tls.Config for the mTLS gRPC listener: it serves the
// (rotating, via GetCertificate) server cert and requires a client cert
// signed by the org root.
//
// The client-cert trust pool is read via GetConfigForClient (A-2): the setup
// wizard can re-issue the org root (ReissueRoot) and the next handshake
// verifies against the NEW root without a listener restart or a stale pool.
// Go's handshake REPLACES the config with GetConfigForClient's return value
// (no field merging), so we reconstruct it field-by-field — a tls.Config
// contains a mutex and must not be struct-copied (go vet copylocks). We
// inherit the base's certificate hook + ALPN (h2, set by gRPC/http2 at setup)
// and swap in the CURRENT root's client-CA pool per handshake.
func (m *Manager) TLSConfig(names []string) (*tls.Config, error) {
	// Mint the first server cert so the hook never returns a nil cert.
	if _, err := m.ServerCertNow(names); err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAndVerifyClientCert,
		// Every handshake goes through ServerCertNow, which re-issues the
		// server cert in place once it enters its rotation window (W3-2:
		// no listener restart, in-flight handshakes undisturbed). The check
		// is a mutex + time comparison — cheap enough per handshake.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.ServerCertNow(names)
		},
	}
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := &tls.Config{
			MinVersion:               cfg.MinVersion,
			MaxVersion:               cfg.MaxVersion,
			CipherSuites:             cfg.CipherSuites,
			PreferServerCipherSuites: cfg.PreferServerCipherSuites,
			SessionTicketsDisabled:   cfg.SessionTicketsDisabled,
			GetCertificate:           cfg.GetCertificate,
			NextProtos:               cfg.NextProtos,
			ClientAuth:               tls.RequireAndVerifyClientCert,
			ClientCAs:                m.Root().CertPool(),
		}
		return c, nil
	}
	return cfg, nil
}
