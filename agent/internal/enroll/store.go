package enroll

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Identity is the agent's persistent enrollment record, written to disk so a
// restarted agent reuses its identity instead of re-enrolling.
type Identity struct {
	DeviceID   string `json:"device_id"`
	JWT        string `json:"jwt"`
	Hostname   string `json:"hostname"`
	EnrolledAt int64  `json:"enrolled_at_unix_ms"`

	// W3-1: the device's mTLS identity, minted by the server's org root at
	// enroll and persisted here. From the first enroll onward the agent
	// speaks to the server's mTLS gRPC port using these.
	TLS *TLSIdentity `json:"tls,omitempty"`
}

// TLSIdentity is the PEM material the agent uses for the mTLS channel: its
// leaf cert + key (presented to the server) and the org root CA (pinned, so
// the agent verifies the server's own certificate too — genuinely mutual).
//
// W3-2: the leaf pair is ROTATED in place (SwapLeaf) well before expiry; the
// org root never changes. The leaf fields are guarded by a mutex because the
// uplink reads them at handshake time while the rotator goroutine writes them.
type TLSIdentity struct {
	mu          sync.RWMutex
	LeafCertPEM string `json:"leaf_cert_pem"`
	LeafKeyPEM  string `json:"leaf_key_pem"`
	OrgRootPEM  string `json:"org_root_ca_pem"`
}

// Valid reports whether all three PEM fields are present.
func (t *TLSIdentity) Valid() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.LeafCertPEM != "" && t.LeafKeyPEM != "" && t.OrgRootPEM != ""
}

// CurrentLeafPEM returns the current leaf cert (PEM) for the rotator's watch.
func (t *TLSIdentity) CurrentLeafPEM() []byte {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return []byte(t.LeafCertPEM)
}

// SwapLeaf (W3-2) atomically replaces the leaf cert + key (the rotator runs
// this inside the old cert's validity window, so the uplink never has to
// present an expired leaf).
func (t *TLSIdentity) SwapLeaf(certPEM, keyPEM []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.LeafCertPEM = string(certPEM)
	t.LeafKeyPEM = string(keyPEM)
}

// SetLeaf is kept for callers that prefer the rotate.Identity name; it
// delegates to SwapLeaf.
func (t *TLSIdentity) SetLeaf(certPEM, keyPEM []byte) { t.SwapLeaf(certPEM, keyPEM) }

// KeyPair loads the agent's current leaf cert + key as a tls.Certificate
// (the client identity presented on the mTLS handshake). A fresh handshake
// after a swap presents the new leaf; an in-flight connection keeps using the
// one it already negotiated.
func (t *TLSIdentity) KeyPair() (tls.Certificate, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return tls.X509KeyPair([]byte(t.LeafCertPEM), []byte(t.LeafKeyPEM))
}

// RootCAs returns an x509 pool containing only the org root — the trust
// anchor the agent uses to verify the server on the mTLS channel.
func (t *TLSIdentity) RootCAs() (*x509.CertPool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM([]byte(t.OrgRootPEM)) {
		return nil, fmt.Errorf("agent: no cert in the org root PEM")
	}
	return p, nil
}

// MarshalJSON reads all fields under the lock so a persist that races the
// rotator's SwapLeaf can't capture a torn (cert/key) pair.
func (t *TLSIdentity) MarshalJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	type wire TLSIdentity
	return json.Marshal((*wire)(t))
}

// UnmarshalJSON populates the identity (fresh load / first enroll).
func (t *TLSIdentity) UnmarshalJSON(b []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	type wire TLSIdentity
	w := (*wire)(t)
	return json.Unmarshal(b, &w)
}

// Store persists the Identity to a single root-only file.
type Store struct {
	path string
}

// NewStore returns a Store rooted at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path exposes the backing file (for logging / the status command).
func (s *Store) Path() string { return s.path }

// Load returns the persisted identity, or nil (and a nil error) when none
// exists yet — i.e. this is a first boot.
func (s *Store) Load() (*Identity, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read identity %s: %w", s.path, err)
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		// A corrupt identity file must not wedge the agent into re-enrolling
		// (which would mint a second identity). Surface it so the operator
		// restores the file or re-bootstraps deliberately.
		return nil, fmt.Errorf("parse identity %s: %w", s.path, err)
	}
	if id.DeviceID == "" || id.JWT == "" {
		return nil, fmt.Errorf("identity %s is empty", s.path)
	}
	return &id, nil
}

// Save writes the identity atomically (temp + rename) and tightens perms so
// the token file is readable only by root.
func (s *Store) Save(id *Identity) error {
	if id == nil || id.DeviceID == "" || id.JWT == "" {
		return fmt.Errorf("refusing to save empty identity")
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return fmt.Errorf("write identity temp: %w", err)
	}
	// Belt and braces: 0600 regardless of umask (WriteFile's mode is
	// umask-masked; chmod is not).
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod identity: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename identity into place: %w", err)
	}
	return nil
}
