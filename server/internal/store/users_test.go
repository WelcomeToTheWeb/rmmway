package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- in-memory UserStore -----------------------------------------------------

func TestMemoryUserStoreCRUD(t *testing.T) {
	s := NewMemoryUserStore()
	ctx := context.Background()

	alice, err := s.Create(ctx, "alice", RoleTech, "s3cret-pass", []string{"clt-b", "clt-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if alice.ID == "" || len(alice.ID) != 16 {
		t.Fatalf("create: expected a server-minted usr- id, got %q", alice.ID)
	}
	// Grants are normalized (trimmed, deduped, sorted).
	if len(alice.Clients) != 2 || alice.Clients[0] != "clt-a" || alice.Clients[1] != "clt-b" {
		t.Fatalf("create grants: want [clt-a clt-b], got %v", alice.Clients)
	}
	// Username uniqueness is case-insensitive (the PG index is on lower()).
	if _, err := s.Create(ctx, "ALICE", RoleViewer, "other-pass", nil); !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("case-variant duplicate create: want ErrUsernameExists, got %v", err)
	}
	if _, err := s.Create(ctx, "bob", "superuser", "pw", nil); err == nil {
		t.Fatal("bad role: want error, got nil")
	}

	got, err := s.GetByUsername(ctx, "Alice")
	if err != nil {
		t.Fatalf("get by username (case): %v", err)
	}
	if got.ID != alice.ID || got.Username != "alice" || got.Role != RoleTech {
		t.Fatalf("get by username: got %+v", got)
	}
	if _, err := s.GetByUsername(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: want ErrNotFound, got %v", err)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Username != "alice" {
		t.Fatalf("list: got %+v, %v", list, err)
	}

	// Password verification round-trip (PBKDF2, same params as admin_users).
	if !VerifyUserPassword("s3cret-pass", got.Salt, got.Hash) {
		t.Fatal("verify: correct password rejected")
	}
	if VerifyUserPassword("wrong", got.Salt, got.Hash) {
		t.Fatal("verify: wrong password accepted")
	}

	// Update: role + grants replace, enabled flip, no-op keep.
	viewer := RoleViewer
	upd, err := s.Update(ctx, alice.ID, &viewer, nil, []string{"clt-c"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Role != RoleViewer || len(upd.Clients) != 1 || upd.Clients[0] != "clt-c" {
		t.Fatalf("update: got %+v", upd)
	}
	keep, err := s.Update(ctx, alice.ID, nil, nil, nil)
	if err != nil || keep.Role != RoleViewer || len(keep.Clients) != 1 {
		t.Fatalf("no-op update: got %+v, %v", keep, err)
	}
	disabled := false
	upd, err = s.Update(ctx, alice.ID, nil, &disabled, nil)
	if err != nil || upd.Enabled {
		t.Fatalf("disable: got %+v, %v", upd, err)
	}
	if _, err := s.Update(ctx, "nope", &viewer, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}
	if _, err := s.Update(ctx, alice.ID, ptrRole("nope"), nil, nil); err == nil {
		t.Fatal("update bad role: want error, got nil")
	}
}

func ptrRole(r string) *string { return &r }

func TestMemoryUserStoreTotpAndLoginStamp(t *testing.T) {
	s := NewMemoryUserStore()
	ctx := context.Background()
	u, err := s.Create(ctx, "carol", RoleAdmin, "pw-pw-pw-pw", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.TotpSecret != "" || u.TotpVerified != nil {
		t.Fatalf("fresh user must be MFA-unenrolled, got %+v", u)
	}
	if err := s.VerifyTotp(ctx, u.ID); !errors.Is(err, ErrTotpNotEnrolled) {
		t.Fatalf("verify before enroll: want ErrTotpNotEnrolled, got %v", err)
	}
	if err := s.SetTotpSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	got, _ := s.Get(ctx, u.ID)
	if got.TotpSecret == "" || got.TotpVerified != nil {
		t.Fatalf("enrolled-not-verified: got %+v", got)
	}
	if err := s.VerifyTotp(ctx, u.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ = s.Get(ctx, u.ID)
	if got.TotpVerified == nil {
		t.Fatal("verified stamp missing")
	}
	if err := s.ClearTotp(ctx, u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.Get(ctx, u.ID)
	if got.TotpSecret != "" || got.TotpVerified != nil {
		t.Fatalf("after reset: got %+v", got)
	}
	if err := s.MarkLoggedIn(ctx, u.ID); err != nil {
		t.Fatalf("mark login: %v", err)
	}
	got, _ = s.Get(ctx, u.ID)
	if got.LastLoginAt == nil {
		t.Fatal("last_login stamp missing")
	}

	// Password reset re-salts and the old password stops working.
	if !VerifyUserPassword("pw-pw-pw-pw", got.Salt, got.Hash) {
		t.Fatal("pre-reset verify failed")
	}
	if err := s.SetPassword(ctx, u.ID, "new-password-1"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	got, _ = s.Get(ctx, u.ID)
	if VerifyUserPassword("pw-pw-pw-pw", got.Salt, got.Hash) {
		t.Fatal("old password still verifies after reset")
	}
	if !VerifyUserPassword("new-password-1", got.Salt, got.Hash) {
		t.Fatal("new password does not verify")
	}
}

func TestMemoryUserStoreTokens(t *testing.T) {
	s := NewMemoryUserStore()
	ctx := context.Background()
	u, err := s.Create(ctx, "dave", RoleTech, "pw-pw-pw-pw", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	full, prefix, expires, err := s.CreateToken(ctx, u.ID, "ci runner", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if len(full) != 4+32*2 || full[:4] != "rmm_" {
		t.Fatalf("token shape: got %q (want rmm_ + 64 hex)", full)
	}
	if prefix != full[:12] {
		t.Fatalf("prefix: got %q, want %q", prefix, full[:12])
	}
	if expires == nil {
		t.Fatal("ttl token: expected an expiry")
	}

	got, err := s.VerifyToken(ctx, full)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != u.ID || got.Username != "dave" {
		t.Fatalf("verify owner: got %+v", got)
	}
	if _, err := s.VerifyToken(ctx, "rmm_deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: want ErrNotFound, got %v", err)
	}

	toks, err := s.ListTokens(ctx, u.ID)
	if err != nil || len(toks) != 1 || toks[0].TokenPrefix != prefix {
		t.Fatalf("list tokens: got %+v, %v", toks, err)
	}
	if toks[0].TokenHash == "" {
		t.Fatal("hash stored but empty?")
	}
	if err := s.RevokeToken(ctx, toks[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.VerifyToken(ctx, full); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token still verifies: %v", err)
	}
	if err := s.RevokeToken(ctx, toks[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: want ErrNotFound, got %v", err)
	}

	// A disabled account's tokens stop authenticating.
	full2, _, _, err := s.CreateToken(ctx, u.ID, "no expiry", 0)
	if err != nil {
		t.Fatalf("create no-expiry token: %v", err)
	}
	disabled := false
	if _, err := s.Update(ctx, u.ID, nil, &disabled, nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.VerifyToken(ctx, full2); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled user token: want ErrUserDisabled, got %v", err)
	}
}

func TestMemoryUserStoreExpiredToken(t *testing.T) {
	s := NewMemoryUserStore()
	ctx := context.Background()
	u, _ := s.Create(ctx, "erin", RoleViewer, "pw-pw-pw-pw", nil)
	full, _, expires, err := s.CreateToken(ctx, u.ID, "short-lived", time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Backdate the expiry directly (the memory store is whitebox).
	past := time.Now().Add(-time.Minute)
	s.mu.Lock()
	for _, t := range s.tokens {
		t.ExpiresAt = &past
	}
	s.mu.Unlock()
	if _, err := s.VerifyToken(ctx, full); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token: want ErrTokenExpired, got %v", err)
	}
	_ = expires
}

// ---- Postgres UserStore (scratch DB) ----------------------------------------

// TestPostgresUsersLive exercises the pgx UserStore + the 0011 migration
// against a scratch database: the schema (users/user_clients/api_tokens),
// the case-insensitive username index, grants, TOTP stamps, and the
// hash-only API-token lifecycle (incl. expiry + disabled-user refusal).
//
// Requires RMMWAY_TEST_PG_DSN; skipped otherwise.
func TestPostgresUsersLive(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping users Postgres test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	dbName := "rmmway_test_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName)

	u.Path = "/" + dbName
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping scratch db: %v", err)
	}

	t.Chdir("../../..")
	if n, err := Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	} else if n != 11 {
		t.Fatalf("expected 11 migrations applied, got %d", n)
	}

	s := NewPostgresUserStore(db)

	// Grant targets: the clients FK (0010) needs real client rows.
	if _, err := db.Exec(ctx, `INSERT INTO clients (id, name) VALUES
		('clt-a', 'Seed A'), ('clt-b', 'Seed B'), ('clt-c', 'Seed C')`); err != nil {
		t.Fatalf("seed clients: %v", err)
	}

	alice, err := s.Create(ctx, "alice", RoleTech, "s3cret-pass", []string{"clt-b", "clt-a", "clt-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(alice.Clients) != 2 || alice.Clients[0] != "clt-a" || alice.Clients[1] != "clt-b" {
		t.Fatalf("create grants: want [clt-a clt-b], got %v", alice.Clients)
	}
	if _, err := s.Create(ctx, "Alice", RoleViewer, "other", nil); !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("case-variant duplicate: want ErrUsernameExists, got %v", err)
	}
	if _, err := s.Create(ctx, "bob", "superuser", "pw", nil); err == nil {
		t.Fatal("bad role: want CHECK-constraint error, got nil")
	}

	got, err := s.GetByUsername(ctx, "ALICE")
	if err != nil || got.ID != alice.ID {
		t.Fatalf("get by username (case): got %+v, %v", got, err)
	}
	if !VerifyUserPassword("s3cret-pass", got.Salt, got.Hash) || VerifyUserPassword("nope", got.Salt, got.Hash) {
		t.Fatal("password verify mismatch")
	}

	viewer := RoleViewer
	upd, err := s.Update(ctx, alice.ID, &viewer, nil, []string{"clt-c"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Role != RoleViewer || len(upd.Clients) != 1 || upd.Clients[0] != "clt-c" {
		t.Fatalf("update: got %+v", upd)
	}
	disabled := false
	upd, err = s.Update(ctx, alice.ID, nil, &disabled, nil)
	if err != nil || upd.Enabled {
		t.Fatalf("disable: got %+v, %v", upd, err)
	}

	// TOTP lifecycle: enroll -> verify -> reset.
	if err := s.VerifyTotp(ctx, alice.ID); !errors.Is(err, ErrTotpNotEnrolled) {
		t.Fatalf("verify before enroll: %v", err)
	}
	if err := s.SetTotpSecret(ctx, alice.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	got, _ = s.Get(ctx, alice.ID)
	if got.TotpSecret == "" || got.TotpVerified != nil {
		t.Fatalf("enrolled-not-verified: %+v", got)
	}
	if err := s.VerifyTotp(ctx, alice.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ = s.Get(ctx, alice.ID)
	if got.TotpVerified == nil {
		t.Fatal("verified stamp missing")
	}
	if err := s.ClearTotp(ctx, alice.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.Get(ctx, alice.ID)
	if got.TotpSecret != "" || got.TotpVerified != nil {
		t.Fatalf("after reset: %+v", got)
	}

	if err := s.MarkLoggedIn(ctx, alice.ID); err != nil {
		t.Fatalf("mark login: %v", err)
	}
	got, _ = s.Get(ctx, alice.ID)
	if got.LastLoginAt == nil {
		t.Fatal("last_login stamp missing")
	}

	// Re-enable (disabled above for the disable-path checks) so the token
	// lifecycle below runs against a live account.
	enabled := true
	if _, err := s.Update(ctx, alice.ID, nil, &enabled, nil); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	// API tokens: hash-only storage + auth outcomes.
	full, prefix, expires, err := s.CreateToken(ctx, alice.ID, "ci runner", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if full[:4] != "rmm_" || prefix != full[:12] || expires == nil {
		t.Fatalf("token shape: %q %q %v", full[:4], prefix, expires)
	}
	var stored int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE token_hash = $1`, hashAPIToken(full)).Scan(&stored); err != nil {
		t.Fatalf("hash lookup: %v", err)
	}
	if stored != 1 {
		t.Fatalf("token hash storage: want 1 row, got %d", stored)
	}
	owner, err := s.VerifyToken(ctx, full)
	if err != nil || owner.ID != alice.ID || len(owner.Clients) != 1 {
		t.Fatalf("verify token: got %+v, %v", owner, err)
	}
	if _, err := s.VerifyToken(ctx, "rmm_0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	toks, err := s.ListTokens(ctx, alice.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("list tokens: %+v, %v", toks, err)
	}
	// Disabled owner: the token stops authenticating.
	if _, err := s.Update(ctx, alice.ID, nil, &disabled, nil); err != nil {
		t.Fatalf("disable for token check: %v", err)
	}
	if _, err := s.VerifyToken(ctx, full); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled-owner token: want ErrUserDisabled, got %v", err)
	}
	if err := s.RevokeToken(ctx, toks[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.VerifyToken(ctx, full); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token: %v", err)
	}

	// Deleting the account cascades grants + tokens (FK ON DELETE CASCADE).
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = $1`, alice.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM user_clients`).Scan(&stored); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if stored != 0 {
		t.Fatalf("grants did not cascade: %d rows", stored)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM api_tokens`).Scan(&stored); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if stored != 0 {
		t.Fatalf("tokens did not cascade: %d rows", stored)
	}
}
