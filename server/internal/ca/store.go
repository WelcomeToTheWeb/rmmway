package ca

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OrgStore persists the org PKI (the root CA and each device's issued leaf)
// so a server restart reuses the same root (existing devices' certs stay
// valid) instead of minting a fresh root that would orphan every device.
//
// The Postgres implementation lives here; a memory implementation keeps unit
// tests and in-memory-mode servers self-contained.
type OrgStore interface {
	// LoadRoot returns the persisted org root (cert PEM + key PEM), or
	// (nil, nil, nil) when none has been generated yet (fresh server).
	LoadRoot(ctx context.Context) (certPEM, keyPEM []byte, err error)
	// SaveRoot persists a newly generated org root. It is idempotent on a
	// fresh server but must be a one-time event per server lifetime.
	SaveRoot(ctx context.Context, certPEM, keyPEM []byte) error
	// SaveLeaf records a device's most recent issued leaf cert + key. This is
	// the audit/reissue record; the device keeps its own copy on its disk.
	SaveLeaf(ctx context.Context, deviceID string, leafCertPEM, leafKeyPEM []byte) error
}

// ---- in-memory implementation -----------------------------------------------

type MemoryOrgStore struct {
	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
	leaves  map[string][2][]byte // deviceID -> {certPEM, keyPEM}
}

func NewMemoryOrgStore() *MemoryOrgStore {
	return &MemoryOrgStore{leaves: make(map[string][2][]byte)}
}

func (m *MemoryOrgStore) LoadRoot(_ context.Context) ([]byte, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.certPEM, m.keyPEM, nil
}

func (m *MemoryOrgStore) SaveRoot(_ context.Context, certPEM, keyPEM []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.certPEM, m.keyPEM = certPEM, keyPEM
	return nil
}

func (m *MemoryOrgStore) SaveLeaf(_ context.Context, deviceID string, leafCertPEM, leafKeyPEM []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaves[deviceID] = [2][]byte{leafCertPEM, leafKeyPEM}
	return nil
}

// Leaf returns a recorded leaf (test helper).
func (m *MemoryOrgStore) Leaf(deviceID string) (certPEM, keyPEM []byte, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leaves[deviceID]
	if !ok {
		return nil, nil, false
	}
	return l[0], l[1], true
}

// ---- Postgres implementation ------------------------------------------------

// isNoRows reports whether err is a pgx "no rows" error (empty result set).
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// PostgresOrgStore backs the org PKI with Timescale/Postgres. The tables are
// created by migration 0004_org_ca.sql (applied at boot by store.Migrate).
type PostgresOrgStore struct{ db *pgxpool.Pool }

func NewPostgresOrgStore(db *pgxpool.Pool) *PostgresOrgStore {
	return &PostgresOrgStore{db: db}
}

func (p *PostgresOrgStore) LoadRoot(ctx context.Context) ([]byte, []byte, error) {
	var certPEM, keyPEM []byte
	err := p.db.QueryRow(ctx,
		`SELECT root_cert_pem, root_key_pem FROM org_ca WHERE id = 1`).
		Scan(&certPEM, &keyPEM)
	if err != nil {
		if isNoRows(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("load org root: %w", err)
	}
	return certPEM, keyPEM, nil
}

func (p *PostgresOrgStore) SaveRoot(ctx context.Context, certPEM, keyPEM []byte) error {
	_, err := p.db.Exec(ctx,
		`INSERT INTO org_ca (id, root_cert_pem, root_key_pem) VALUES (1, $1, $2)
		 ON CONFLICT (id) DO NOTHING`, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("save org root: %w", err)
	}
	return nil
}

func (p *PostgresOrgStore) SaveLeaf(ctx context.Context, deviceID string, leafCertPEM, leafKeyPEM []byte) error {
	_, err := p.db.Exec(ctx,
		`INSERT INTO device_certs (device_id, leaf_cert_pem, leaf_key_pem)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (device_id) DO UPDATE
		   SET leaf_cert_pem = EXCLUDED.leaf_cert_pem,
		       leaf_key_pem  = EXCLUDED.leaf_key_pem,
		       issued_at     = now()`, deviceID, leafCertPEM, leafKeyPEM)
	if err != nil {
		return fmt.Errorf("save leaf %s: %w", deviceID, err)
	}
	return nil
}
