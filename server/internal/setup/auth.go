package setup

import (
	"context"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/users"
)

// ---- unified operator login (gap #3, wave 2, lane B) ------------------------

// Authenticate outcomes (the /api/login contract).
const (
	// AuthOK: credentials (+ TOTP, when enrolled) verified.
	AuthOK = "ok"
	// AuthMFARequired: the matched users row has TOTP enrolled and the
	// request carried no code. The API answers 401 {"error":"mfa_required"};
	// the UI re-submits with the 6-digit code. (A wrong code is AuthInvalid
	// — one 401 shape for every credential failure, no enumeration oracle.)
	AuthMFARequired = "mfa_required"
	// AuthInvalid: wrong password, wrong TOTP code, or disabled account.
	AuthInvalid = "invalid"
)

// AuthResult is one credential-check resolution. For AuthOK, Username/Role
// identify the account (legacy accounts report role "admin", Legacy=true)
// and AccountID is the users row id ("" for the legacy path);
// TotpEnrolled tells the caller the matched users row has 2FA on.
type AuthResult struct {
	Outcome      string
	Username     string
	Role         string
	AccountID    string
	Legacy       bool
	TotpEnrolled bool
}

// UsersAuth is the users-table side of the unified login (0011_users).
// wire_users.go / the httpapi layer adapt store.UserStore to this so the
// setup package does not import store (the DeviceCounter pattern).
type UsersAuth interface {
	// Count is the operator account count. 0 = the users feature is not in
	// use, so the env bootstrap pair stays valid (dev mode, pre-setup
	// window, in-memory mode).
	Count(ctx context.Context) (int, error)
	// Lookup returns the login-relevant slice of a users row (case-
	// insensitive username). ok=false means no users row for the username.
	Lookup(ctx context.Context, username string) (UserCredential, bool, error)
}

// UserCredential is the login-relevant slice of a users row. The salt/hash
// pair uses the same PBKDF2-SHA256 100k/32-byte params as admin_users, so
// this package's VerifyPassword checks both.
type UserCredential struct {
	AccountID  string
	Username   string // canonical (stored) form — login is case-insensitive
	Role       string
	Enabled    bool
	Salt       []byte
	Hash       []byte
	TotpSecret string // base32; "" = not enrolled
}

// EnvPairCheck verifies the RMMWAY_ADMIN_USER/PASSWORD bootstrap pair. The
// API layer supplies it (it holds the per-boot salt + hash and does the
// timing-safe compare); nil disables the env fallback entirely.
type EnvPairCheck func(username, password string) bool

// Authenticate resolves one login attempt (the gap #3 contract):
//
//  1. users row (case-insensitive) — authoritative when present: the row's
//     password, enabled flag, and TOTP state decide. Enrolled + no code →
//     AuthMFARequired; wrong code / disabled / wrong password →
//     AuthInvalid. No other table is consulted for that username.
//  2. wizard admin_users row for the username — its password is
//     authoritative (a password changed on the settings page cannot be
//     bypassed by the env pair, C #10a).
//  3. env bootstrap pair — only while the users table is EMPTY (dev mode /
//     pre-setup window). Once any operator account exists, login is
//     DB-only.
//
// ua == nil (no users store — in-memory mode) skips straight to
// admin_users/env.
func (s *Service) Authenticate(ctx context.Context, username, password, totpCode string, ua UsersAuth, env EnvPairCheck) (AuthResult, error) {
	if ua != nil {
		cred, found, err := ua.Lookup(ctx, username)
		if err != nil {
			return AuthResult{}, err
		}
		if found {
			res := AuthResult{
				Username: cred.Username, Role: cred.Role, AccountID: cred.AccountID,
				TotpEnrolled: cred.TotpSecret != "",
			}
			// Disabled accounts and wrong passwords share one outcome
			// (the 401 body must not reveal which).
			if !cred.Enabled || !VerifyPassword(password, cred.Salt, cred.Hash) {
				res.Outcome = AuthInvalid
				return res, nil
			}
			if cred.TotpSecret != "" {
				if totpCode == "" {
					res.Outcome = AuthMFARequired
					return res, nil
				}
				if !users.VerifyTotp(cred.TotpSecret, totpCode, time.Now()) {
					res.Outcome = AuthInvalid
					return res, nil
				}
			}
			res.Outcome = AuthOK
			return res, nil
		}
	}
	// Step 2: the wizard-minted admin_users row (exact username — the
	// wizard stores what it was given).
	if s.Available() {
		if salt, hash, exists := s.store.AdminCredentials(ctx, username); exists {
			res := AuthResult{Username: username, Role: RoleAdmin, Legacy: true}
			if VerifyPassword(password, salt, hash) {
				res.Outcome = AuthOK
				return res, nil
			}
			// A row exists: only its credential may win — the env pair
			// must not bypass a changed password (C #10a).
			res.Outcome = AuthInvalid
			return res, nil
		}
	}
	// Step 3: env bootstrap — DB-only once operator accounts exist.
	if env != nil {
		if ua == nil {
			// In-memory mode: no users table at all, env pair always valid.
			if env(username, password) {
				return AuthResult{Username: username, Role: RoleAdmin, Legacy: true, Outcome: AuthOK}, nil
			}
			return AuthResult{Outcome: AuthInvalid}, nil
		}
		n, err := ua.Count(ctx)
		if err != nil {
			return AuthResult{}, err
		}
		if n == 0 && env(username, password) {
			return AuthResult{Username: username, Role: RoleAdmin, Legacy: true, Outcome: AuthOK}, nil
		}
	}
	return AuthResult{Outcome: AuthInvalid}, nil
}

// RoleAdmin is the role reported for legacy (non-users-table) accounts:
// the pre-gap-#3 single-role operator.
const RoleAdmin = "admin"
