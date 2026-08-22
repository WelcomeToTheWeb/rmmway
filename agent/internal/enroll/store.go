package enroll

import (
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
