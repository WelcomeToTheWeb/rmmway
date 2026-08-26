// Package setup is the A-2 first-boot setup wizard backend: the server's
// initialization state.
//
// A FRESH database (server_setup absent or done=false) means the server is
// not initialized: the UI redirects to the setup wizard, and
// POST /api/setup/complete is open to finish initialization in one shot —
// minting the initial root admin credentials, stamping the organization's
// name into the org root CA, and persisting the SMTP outbox configuration.
// Once done=true, every subsequent boot bypasses the wizard (status reads
// done=true straight from the database).
//
// The Postgres store lives here (tables from migration 0009_setup.sql);
// the Service orchestrates the CA re-issue + persistence so the wizard's
// choices are atomic with the org PKI state.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/smtp"
)

// Sentinel errors the API layer maps onto status codes.
var (
	ErrAlreadyComplete = errors.New("setup already complete")
	ErrDevicesEnrolled = errors.New("devices are enrolled — the first-boot wizard can no longer define the org CA")
)

// CompleteRequest is the wizard's payload: the initial root admin
// credentials, the organization name (stamped into the org root CA), and
// the SMTP outbox configuration (optional — Host "" = not configured).
type CompleteRequest struct {
	AdminUser     string      `json:"admin_user"`
	AdminPassword string      `json:"admin_password"`
	OrgName       string      `json:"org_name"`
	SMTP          smtp.Config `json:"smtp"`
}

// Stored is what the wizard persisted (the API's read model). Password is
// the only secret and is never included.
type Stored struct {
	Done            bool
	AdminUser       string
	OrgName         string
	SMTP            smtp.Config
	SMTPConfigured  bool
}

// Store persists the initialization state + the wizard's choices.
type Store interface {
	// Done reports whether the wizard has run (false = fresh server).
	Done(ctx context.Context) (bool, error)
	// Complete persists everything atomically: the done flag, the root
	// admin account, and the org_name + smtp config rows.
	Complete(ctx context.Context, req CompleteRequest, salt, hash []byte) error
	// Load reads back the stored choices (zero-value Stored when absent).
	Load(ctx context.Context) (Stored, error)
	// AdminCredentials returns (salt, hash) for one admin account, or
	// (nil, nil, false) when the username is unknown.
	AdminCredentials(ctx context.Context, username string) (salt, hash []byte, ok bool)
	// AdminUsernames lists the wizard-minted operator accounts.
	AdminUsernames(ctx context.Context) ([]string, error)
}

// ---- in-memory implementation (tests / degraded mode) -----------------------

type MemoryStore struct {
	done      bool
	adminUser string
	salt      []byte
	hash      []byte
	orgName   string
	smtp      smtp.Config
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Done(context.Context) (bool, error) { return m.done, nil }

func (m *MemoryStore) Complete(_ context.Context, req CompleteRequest, salt, hash []byte) error {
	if m.done {
		return ErrAlreadyComplete
	}
	m.done = true
	m.adminUser = req.AdminUser
	m.salt, m.hash = salt, hash
	m.orgName = req.OrgName
	m.smtp = req.SMTP
	return nil
}

func (m *MemoryStore) Load(context.Context) (Stored, error) {
	s := Stored{Done: m.done, AdminUser: m.adminUser, OrgName: m.orgName, SMTP: m.smtp}
	s.SMTPConfigured = m.smtp.IsConfigured()
	return s, nil
}

func (m *MemoryStore) AdminCredentials(_ context.Context, username string) ([]byte, []byte, bool) {
	if !m.done || username != m.adminUser {
		return nil, nil, false
	}
	return m.salt, m.hash, true
}

func (m *MemoryStore) AdminUsernames(context.Context) ([]string, error) {
	if !m.done {
		return nil, nil
	}
	return []string{m.adminUser}, nil
}

// ---- Postgres implementation ------------------------------------------------

// PostgresStore backs the wizard with Timescale/Postgres (tables from
// migration 0009_setup.sql, applied at boot by store.Migrate).
type PostgresStore struct{ db *pgxpool.Pool }

func NewPostgresStore(db *pgxpool.Pool) *PostgresStore { return &PostgresStore{db: db} }

func (p *PostgresStore) Done(ctx context.Context) (bool, error) {
	var done bool
	err := p.db.QueryRow(ctx, `SELECT done FROM server_setup WHERE id = 1`).Scan(&done)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // fresh server (or pre-A-2 database)
		}
		return false, fmt.Errorf("setup status: %w", err)
	}
	return done, nil
}

func (p *PostgresStore) Complete(ctx context.Context, req CompleteRequest, salt, hash []byte) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var done bool
	err = tx.QueryRow(ctx, `
		INSERT INTO server_setup (id, done, completed_at) VALUES (1, true, now())
		ON CONFLICT (id) DO UPDATE SET done = EXCLUDED.done, completed_at = EXCLUDED.completed_at
		RETURNING done`).Scan(&done)
	if err != nil {
		return fmt.Errorf("setup mark done: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		INSERT INTO admin_users (username, salt, pass_hash) VALUES ($1, $2, $3)
		ON CONFLICT (username) DO UPDATE SET salt = EXCLUDED.salt, pass_hash = EXCLUDED.pass_hash`,
		req.AdminUser, salt, hash); err != nil {
		return fmt.Errorf("setup admin user: %w", err)
	}

	for key, value := range map[string]string{
		"org_name": string(orgJSON(req.OrgName)),
		"smtp":     string(smtpJSON(req.SMTP)),
	} {
		if _, err = tx.Exec(ctx, `
			INSERT INTO server_config (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			key, value); err != nil {
			return fmt.Errorf("setup config %s: %w", key, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("setup commit: %w", err)
	}
	return nil
}

func (p *PostgresStore) Load(ctx context.Context) (Stored, error) {
	out := Stored{}
	err := p.db.QueryRow(ctx, `SELECT done FROM server_setup WHERE id = 1`).Scan(&out.Done)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("setup load: %w", err)
	}
	if !out.Done {
		return out, nil
	}
	var names []string
	rows, err := p.db.Query(ctx, `SELECT username FROM admin_users ORDER BY username`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err == nil {
			names = append(names, u)
		}
	}
	rows.Close()
	if len(names) > 0 {
		out.AdminUser = names[0]
	}
	if err := p.loadConfig(ctx, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (p *PostgresStore) loadConfig(ctx context.Context, out *Stored) error {
	var orgVal, smtpVal []byte
	err := p.db.QueryRow(ctx, `SELECT value FROM server_config WHERE key = 'org_name'`).Scan(&orgVal)
	if err == nil {
		_ = json.Unmarshal(orgVal, &out.OrgName)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("setup org_name: %w", err)
	}
	err = p.db.QueryRow(ctx, `SELECT value FROM server_config WHERE key = 'smtp'`).Scan(&smtpVal)
	if err == nil {
		_ = json.Unmarshal(smtpVal, &out.SMTP)
		out.SMTPConfigured = out.SMTP.IsConfigured()
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("setup smtp: %w", err)
	}
	return nil
}

func (p *PostgresStore) AdminCredentials(ctx context.Context, username string) ([]byte, []byte, bool) {
	var salt, hash []byte
	err := p.db.QueryRow(ctx, `SELECT salt, pass_hash FROM admin_users WHERE username = $1`, username).
		Scan(&salt, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false
		}
		return nil, nil, false
	}
	return salt, hash, true
}

func (p *PostgresStore) AdminUsernames(ctx context.Context) ([]string, error) {
	rows, err := p.db.Query(ctx, `SELECT username FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// smtpJSON marshals the config the way it is persisted in server_config.
func smtpJSON(c smtp.Config) []byte {
	b, _ := json.Marshal(c)
	return b
}

// orgJSON marshals the org name as a JSON string (server_config is jsonb).
func orgJSON(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// ---- password hashing (PBKDF2-SHA256, same params as the env admin) --------

const (
	pbkdf2Iterations = 100_000
	pbkdf2KeyLen     = 32
)

// VerifyAdmin reports whether username/password matches the wizard-minted
// account (constant-time; always computes a hash so timing doesn't leak
// whether the username exists).
func VerifyAdmin(ctx context.Context, s Store, username, password string) bool {
	salt, hash, ok := s.AdminCredentials(ctx, username)
	if !ok {
		// Burn one hash anyway (username-existence timing parity).
		HashPassword(password, []byte("dummy-salt-16byt"))
		return false
	}
	return VerifyPassword(password, salt, hash)
}
