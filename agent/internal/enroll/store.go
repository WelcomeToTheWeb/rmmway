package enroll

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
type TLSIdentity struct {
	LeafCertPEM string `json:"leaf_cert_pem"`
	LeafKeyPEM  string `json:"leaf_key_pem"`
	OrgRootPEM  string `json:"org_root_ca_pem"`
}

// Valid reports whether all three PEM fields are present.
func (t *TLSIdentity) Valid() bool {
	return t != nil && t.LeafCertPEM != "" && t.LeafKeyPEM != "" && t.OrgRootPEM != ""
}

// KeyPair loads the agent's leaf cert + key as a tls.Certificate (the client
// identity presented on the mTLS handshake).
func (t *TLSIdentity) KeyPair() (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(t.LeafCertPEM), []byte(t.LeafKeyPEM))
}

// RootCAs returns an x509 pool containing only the org root — the trust
// anchor the agent uses to verify the server on the mTLS channel.
func (t *TLSIdentity) RootCAs() (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM([]byte(t.OrgRootPEM)) {
		return nil, fmt.Errorf("agent: no cert in the org root PEM")
	}
	return p, nil
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
