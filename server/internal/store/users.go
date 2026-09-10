package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/pbkdf2"
)

// ---- operator users + RBAC (gap #3, wave 2, lane B) -------------------------

// Roles: admin bypasses client scoping entirely; tech/viewer are scoped to
// their user_clients grants (empty grants = no clients visible).
const (
	RoleAdmin  = "admin"
	RoleTech   = "tech"
	RoleViewer = "viewer"
)

// ValidRole reports whether r is one of the three RBAC roles (0011's
// CHECK constraint enforces the same set at the database level).
func ValidRole(r string) bool {
	return r == RoleAdmin || r == RoleTech || r == RoleViewer
}

// User is one operator account (users table, 0011_users.sql). Salt/Hash is
// the PBKDF2-SHA256 pair (100k iterations / 32 bytes — the same params the
// legacy admin_users rows use, so hashes are interchangeable).
type User struct {
	ID         string
	Username   string
	Role       string
	Enabled    bool
	Salt       []byte
	Hash       []byte
	TotpSecret string // base32; "" = MFA not enrolled
	// TotpVerified is non-nil once a code has been accepted (enrolled +
	// verified); nil while enrolled-but-unverified (login still works,
	// the Users UI flags it).
	TotpVerified *time.Time
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Clients is the granted client-id list (admin role: unused — the
	// admin sees every client). Filled by the store, never written by
	// callers.
	Clients []string
}

// APIToken is one operator API token (api_tokens table). Only the hash of
// the full "rmm_…" token is stored; TokenPrefix is the displayable head.
type APIToken struct {
	ID          string
	UserID      string
	Name        string
	TokenPrefix string
	TokenHash   string
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	CreatedAt   time.Time
}

// ErrUsernameExists is returned when a create/update would violate the
// unique users.username (case-insensitive).
var ErrUsernameExists = errors.New("username already exists")

// ErrTotpNotEnrolled is returned by VerifyTotp when the account has no
// TOTP secret on file.
var ErrTotpNotEnrolled = errors.New("totp not enrolled")

// UserStore persists operator accounts, their per-client grants, and their
// API tokens (gap #3). Mirrors the ClientStore dual-store pattern: a
// Postgres store (0011_users.sql) and an in-memory store for tests and the
// httpapi suite's 503-vs-wired distinction.
type UserStore interface {
	// List returns every account sorted by username, with grants filled.
	List(ctx context.Context) ([]*User, error)
	// Count is the account count (the unified login's DB-only gate: the
	// env bootstrap pair is valid only while this is 0).
	Count(ctx context.Context) (int, error)
	// Get returns one account by id; ErrNotFound when unknown.
	Get(ctx context.Context, id string) (*User, error)
	// GetByUsername is case-insensitive (the unique index is on
	// lower(username)); ErrNotFound when unknown.
	GetByUsername(ctx context.Context, username string) (*User, error)
	// Create mints a new account (server-minted "usr-" id) with grants;
	// ErrUsernameExists on a duplicate (case-insensitive), an error on a
	// bad role.
	Create(ctx context.Context, username, role, password string, clientIDs []string) (*User, error)
	// Update patches role and/or enabled and/or the grant list (nil
	// clientIDs = keep the current grants). ErrNotFound /
	// ErrUsernameExists (renaming is not supported — the username is the
	// login name) map onto 404/409 at the API layer.
	Update(ctx context.Context, id string, role *string, enabled *bool, clientIDs []string) (*User, error)
	// SetPassword re-hashes the account's password (a fresh 16-byte salt).
	SetPassword(ctx context.Context, id, password string) error
	// SetTotpSecret enrolls TOTP for the account (base32 secret).
	SetTotpSecret(ctx context.Context, id, secret string) error
	// VerifyTotp stamps the first-verified time (the account already has a
	// secret; the code itself is validated by the users package).
	VerifyTotp(ctx context.Context, id string) error
	// ClearTotp removes the secret + verified stamp (2FA reset).
	ClearTotp(ctx context.Context, id string) error
	// MarkLoggedIn stamps last_login_at (best-effort at login).
	MarkLoggedIn(ctx context.Context, id string) error
	// CreateToken mints an API token for the account and returns the FULL
	// token (shown once), its display prefix, and the expiry (nil = none).
	CreateToken(ctx context.Context, userID, name string, ttl time.Duration) (token, prefix string, expiresAt *time.Time, err error)
	// ListTokens returns the account's tokens (hashes, never full tokens).
	ListTokens(ctx context.Context, userID string) ([]*APIToken, error)
	// RevokeToken deletes one token; ErrNotFound when unknown.
	RevokeToken(ctx context.Context, id string) error
	// VerifyToken authenticates a full "rmm_…" bearer: hash lookup, expiry
	// and enabled-account checks. Returns the owning account (with
	// grants) on success; ErrNotFound / ErrTokenExpired /
	// ErrUserDisabled otherwise. Best-effort stamps last_used_at.
	VerifyToken(ctx context.Context, token string) (*User, error)
}

// ErrTokenExpired / ErrUserDisabled are the API-token auth outcomes the
// httpapi layer maps onto 401.
var (
	ErrTokenExpired = errors.New("api token expired")
	ErrUserDisabled = errors.New("user disabled")
)

// ---- id mints + hashing ------------------------------------------------------

// newUserID mints an account id the way clients are minted: "usr-" + 12 hex
// from crypto/rand.
func newUserID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "usr-" + hex.EncodeToString(raw), nil
}

// newTokenID mints an API-token row id: "apit-" + 12 hex.
func newTokenID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "apit-" + hex.EncodeToString(raw), nil
}

// MintAPIKey builds the operator's full API token: "rmm_" + 64 hex chars
// (32 random bytes). The DB stores only SHA-256(token).
func MintAPIKey() (token, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = "rmm_" + hex.EncodeToString(raw)
	return token, token[:12], nil
}

func hashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashPassword / VerifyPassword re-export the setup package's PBKDF2 params
// without a store→setup import cycle: identical algorithm (PBKDF2-SHA256,
// 100k iterations, 32-byte output), so user and admin_users hashes are
// interchangeable. (The httpapi layer hashes via setup directly; the store
// needs its own copy to store/verify without the dependency.)
const (
	userPBKDF2Iterations = 100_000
	userPBKDF2KeyLen     = 32
)

func HashUserPassword(password string, salt []byte) []byte {
	return pbkdf2.Key([]byte(password), salt, userPBKDF2Iterations, userPBKDF2KeyLen, sha256.New)
}

func VerifyUserPassword(password string, salt, hash []byte) bool {
	candidate := HashUserPassword(password, salt)
	return subtle.ConstantTimeCompare(candidate, hash) == 1
}

func usersUsernameExists(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_users_username"
}

// ---- Postgres implementation ----------------------------------------------

// PostgresUserStore is the pgx-backed UserStore (0011_users.sql).
type PostgresUserStore struct {
	db *pgxpool.Pool
}

func NewPostgresUserStore(db *pgxpool.Pool) *PostgresUserStore {
	return &PostgresUserStore{db: db}
}

const userColumns = `id, username, role, enabled, totp_secret, totp_verified_at,
	password_salt, password_hash, last_login_at, created_at, updated_at`

func (s *PostgresUserStore) List(ctx context.Context) ([]*User, error) {
	users, err := s.listRows(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.fillGrants(ctx, users); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *PostgresUserStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *PostgresUserStore) listRows(ctx context.Context) ([]*User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY lower(username)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Enabled, &u.TotpSecret,
			&u.TotpVerified, &u.Salt, &u.Hash, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// fillGrants loads the user_clients grants for the given accounts in one
// round-trip (ids are caller-owned rows; Clients is replaced).
func (s *PostgresUserStore) fillGrants(ctx context.Context, users []*User) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	rows, err := s.db.Query(ctx, `SELECT user_id, client_id FROM user_clients WHERE user_id = ANY($1) ORDER BY client_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]*[]string, len(users))
	for _, u := range users {
		byID[u.ID] = &u.Clients
	}
	for rows.Next() {
		var userID, clientID string
		if err := rows.Scan(&userID, &clientID); err != nil {
			return err
		}
		if dst, ok := byID[userID]; ok {
			*dst = append(*dst, clientID)
		}
	}
	return rows.Err()
}

func (s *PostgresUserStore) Get(ctx context.Context, id string) (*User, error) {
	u, err := s.getRow(ctx, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if err := s.fillGrants(ctx, []*User{u}); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *PostgresUserStore) GetByUsername(ctx context.Context, username string) (*User, error) {
	u, err := s.getRow(ctx, `lower(username) = lower($1)`, username)
	if err != nil {
		return nil, err
	}
	if err := s.fillGrants(ctx, []*User{u}); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *PostgresUserStore) getRow(ctx context.Context, where string, arg any) (*User, error) {
	var u User
	err := s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE `+where, arg).
		Scan(&u.ID, &u.Username, &u.Role, &u.Enabled, &u.TotpSecret, &u.TotpVerified,
			&u.Salt, &u.Hash, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *PostgresUserStore) Create(ctx context.Context, username, role, password string, clientIDs []string) (*User, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	u := &User{
		ID: id, Username: username, Role: role, Enabled: true,
		Salt: salt, Hash: HashUserPassword(password, salt),
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx,
		`INSERT INTO users (id, username, role, password_salt, password_hash)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING created_at, updated_at`,
		u.ID, u.Username, u.Role, u.Salt, u.Hash).
		Scan(&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if usersUsernameExists(err) {
			return nil, ErrUsernameExists
		}
		return nil, err
	}
	if err := insertGrants(ctx, tx, u.ID, clientIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	u.Clients = normalizeClientIDs(clientIDs)
	return u, nil
}

func (s *PostgresUserStore) Update(ctx context.Context, id string, role *string, enabled *bool, clientIDs []string) (*User, error) {
	if role != nil && !ValidRole(*role) {
		return nil, fmt.Errorf("invalid role %q", *role)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if role != nil || enabled != nil {
		sets := make([]string, 0, 2)
		args := []any{}
		if role != nil {
			args = append(args, *role)
			sets = append(sets, fmt.Sprintf("role = $%d", len(args)))
		}
		if enabled != nil {
			args = append(args, *enabled)
			sets = append(sets, fmt.Sprintf("enabled = $%d", len(args)))
		}
		args = append(args, id)
		sets = append(sets, "updated_at = now()")
		if _, err := tx.Exec(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = $`+fmt.Sprint(len(args)), args...); err != nil {
			return nil, err
		}
	}
	if clientIDs != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM user_clients WHERE user_id = $1`, id); err != nil {
			return nil, err
		}
		if err := insertGrants(ctx, tx, id, clientIDs); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func insertGrants(ctx context.Context, tx pgx.Tx, userID string, clientIDs []string) error {
	for _, cid := range normalizeClientIDs(clientIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO user_clients (user_id, client_id) VALUES ($1, $2)`, userID, cid); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresUserStore) SetPassword(ctx context.Context, id, password string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	res, err := s.db.Exec(ctx,
		`UPDATE users SET password_salt = $1, password_hash = $2, updated_at = now() WHERE id = $3`,
		salt, HashUserPassword(password, salt), id)
	if err != nil {
		return err
	}
	return rowsToErr(res)
}

func (s *PostgresUserStore) SetTotpSecret(ctx context.Context, id, secret string) error {
	res, err := s.db.Exec(ctx,
		`UPDATE users SET totp_secret = $1, totp_verified_at = NULL, updated_at = now() WHERE id = $2`,
		secret, id)
	if err != nil {
		return err
	}
	return rowsToErr(res)
}

func (s *PostgresUserStore) VerifyTotp(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx,
		`UPDATE users SET totp_verified_at = now(), updated_at = now() WHERE id = $1 AND totp_secret <> ''`, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		// Zero rows: the user is unknown OR enrolled-free — the API maps
		// both onto 404/409, so disambiguate with one existence probe.
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrTotpNotEnrolled
		}
		return ErrNotFound
	}
	return nil
}

func (s *PostgresUserStore) ClearTotp(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx,
		`UPDATE users SET totp_secret = '', totp_verified_at = NULL, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return rowsToErr(res)
}

func (s *PostgresUserStore) MarkLoggedIn(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}

func (s *PostgresUserStore) CreateToken(ctx context.Context, userID, name string, ttl time.Duration) (string, string, *time.Time, error) {
	var ownerID string
	if err := s.db.QueryRow(ctx, `SELECT id FROM users WHERE id = $1`, userID).Scan(&ownerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil, ErrNotFound
		}
		return "", "", nil, err
	}
	token, prefix, err := MintAPIKey()
	if err != nil {
		return "", "", nil, err
	}
	id, err := newTokenID()
	if err != nil {
		return "", "", nil, err
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expiresAt = &t
	}
	if _, err := s.db.Exec(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_prefix, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, name, prefix, hashAPIToken(token), expiresAt); err != nil {
		return "", "", nil, err
	}
	return token, prefix, expiresAt, nil
}

func (s *PostgresUserStore) ListTokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, user_id, name, token_prefix, token_hash, expires_at, last_used_at, created_at
		 FROM api_tokens WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenPrefix, &t.TokenHash,
			&t.ExpiresAt, &t.LastUsedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *PostgresUserStore) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresUserStore) VerifyToken(ctx context.Context, token string) (*User, error) {
	var (
		u         User
		expiresAt *time.Time
	)
	err := s.db.QueryRow(ctx,
		`SELECT u.id, u.username, u.role, u.enabled, u.totp_secret, u.totp_verified_at,
		        u.password_salt, u.password_hash, u.last_login_at, u.created_at, u.updated_at,
		        t.expires_at
		   FROM api_tokens t JOIN users u ON u.id = t.user_id
		  WHERE t.token_hash = $1`, hashAPIToken(token)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Enabled, &u.TotpSecret, &u.TotpVerified,
			&u.Salt, &u.Hash, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expiresAt != nil && time.Now().After(*expiresAt) {
		return nil, ErrTokenExpired
	}
	if !u.Enabled {
		return nil, ErrUserDisabled
	}
	// Best-effort activity stamp (a failed stamp must not fail the request).
	if _, err := s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1`, hashAPIToken(token)); err != nil {
		// the auth already succeeded; the stamp is cosmetic
	}
	if err := s.fillGrants(ctx, []*User{&u}); err != nil {
		return nil, err
	}
	return &u, nil
}

// rowsToErr maps a zero-row UPDATE to ErrNotFound (the callers that must
// distinguish "unknown row" from "store error").
func rowsToErr(res pgconn.CommandTag) error {
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// normalizeClientIDs trims/dedupes/sorts a grant list (nil → empty slice).
func normalizeClientIDs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ---- in-memory implementation (unit tests) ---------------------------------

// MemoryUserStore is the in-memory UserStore (unit tests, and the httpapi
// suite's 503-vs-wired distinction).
type MemoryUserStore struct {
	mu      sync.RWMutex
	users   map[string]*User
	tokens  map[string]*APIToken
	tokenBy map[string]string // token_hash -> token id
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{
		users:   make(map[string]*User),
		tokens:  make(map[string]*APIToken),
		tokenBy: make(map[string]string),
	}
}

func (s *MemoryUserStore) List(_ context.Context) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		cp := *u
		cp.Clients = append([]string(nil), u.Clients...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username)
	})
	return out, nil
}

func (s *MemoryUserStore) Count(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users), nil
}

func (s *MemoryUserStore) Get(_ context.Context, id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	cp.Clients = append([]string(nil), u.Clients...)
	return &cp, nil
}

func (s *MemoryUserStore) GetByUsername(_ context.Context, username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if strings.EqualFold(u.Username, username) {
			cp := *u
			cp.Clients = append([]string(nil), u.Clients...)
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (s *MemoryUserStore) Create(_ context.Context, username, role, password string, clientIDs []string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidRole(role) {
		return nil, fmt.Errorf("invalid role %q", role)
	}
	for _, u := range s.users {
		if strings.EqualFold(u.Username, username) {
			return nil, ErrUsernameExists
		}
	}
	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	u := &User{
		ID: id, Username: username, Role: role, Enabled: true,
		Salt: salt, Hash: HashUserPassword(password, salt),
		Clients: normalizeClientIDs(clientIDs), CreatedAt: now, UpdatedAt: now,
	}
	s.users[id] = u
	cp := *u
	cp.Clients = append([]string(nil), u.Clients...)
	return &cp, nil
}

func (s *MemoryUserStore) Update(_ context.Context, id string, role *string, enabled *bool, clientIDs []string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	if role != nil {
		if !ValidRole(*role) {
			return nil, fmt.Errorf("invalid role %q", *role)
		}
		u.Role = *role
	}
	if enabled != nil {
		u.Enabled = *enabled
	}
	if clientIDs != nil {
		u.Clients = normalizeClientIDs(clientIDs)
	}
	u.UpdatedAt = time.Now().UTC()
	cp := *u
	cp.Clients = append([]string(nil), u.Clients...)
	return &cp, nil
}

func (s *MemoryUserStore) SetPassword(_ context.Context, id, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	u.Salt = salt
	u.Hash = HashUserPassword(password, salt)
	u.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryUserStore) SetTotpSecret(_ context.Context, id, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	u.TotpSecret = secret
	u.TotpVerified = nil
	u.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryUserStore) VerifyTotp(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	if u.TotpSecret == "" {
		return ErrTotpNotEnrolled
	}
	now := time.Now().UTC()
	u.TotpVerified = &now
	u.UpdatedAt = now
	return nil
}

func (s *MemoryUserStore) ClearTotp(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	u.TotpSecret = ""
	u.TotpVerified = nil
	u.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryUserStore) MarkLoggedIn(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	u.LastLoginAt = &now
	return nil
}

func (s *MemoryUserStore) CreateToken(_ context.Context, userID, name string, ttl time.Duration) (string, string, *time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return "", "", nil, ErrNotFound
	}
	token, prefix, err := MintAPIKey()
	if err != nil {
		return "", "", nil, err
	}
	id, err := newTokenID()
	if err != nil {
		return "", "", nil, err
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expiresAt = &t
	}
	t := &APIToken{
		ID: id, UserID: userID, Name: name, TokenPrefix: prefix,
		TokenHash: hashAPIToken(token), ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC(),
	}
	s.tokens[id] = t
	s.tokenBy[t.TokenHash] = id
	return token, prefix, expiresAt, nil
}

func (s *MemoryUserStore) ListTokens(_ context.Context, userID string) ([]*APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*APIToken
	for _, t := range s.tokens {
		if t.UserID == userID {
			cp := *t
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryUserStore) RevokeToken(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.tokens, id)
	delete(s.tokenBy, t.TokenHash)
	return nil
}

func (s *MemoryUserStore) VerifyToken(_ context.Context, token string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokenBy[hashAPIToken(token)]
	if !ok {
		return nil, ErrNotFound
	}
	t := s.tokens[id]
	if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	u, ok := s.users[t.UserID]
	if !ok {
		return nil, ErrNotFound
	}
	if !u.Enabled {
		return nil, ErrUserDisabled
	}
	now := time.Now().UTC()
	t.LastUsedAt = &now
	cp := *u
	cp.Clients = append([]string(nil), u.Clients...)
	return &cp, nil
}
