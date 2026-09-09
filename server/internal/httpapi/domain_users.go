package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/users"
)

// ---- unified operator login (gap #3, wave 2, lane B) ------------------------

// usersAuthAdapter adapts store.UserStore to setup.UsersAuth (the setup
// package must not import store — the DeviceCounter pattern).
type usersAuthAdapter struct{ u store.UserStore }

func (a usersAuthAdapter) Count(ctx context.Context) (int, error) {
	return a.u.Count(ctx)
}

func (a usersAuthAdapter) Lookup(ctx context.Context, username string) (setup.UserCredential, bool, error) {
	user, err := a.u.GetByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return setup.UserCredential{}, false, nil
	}
	if err != nil {
		return setup.UserCredential{}, false, err
	}
	return setup.UserCredential{
		AccountID:  user.ID,
		Username:   user.Username,
		Role:       user.Role,
		Enabled:    user.Enabled,
		Salt:       user.Salt,
		Hash:       user.Hash,
		TotpSecret: user.TotpSecret,
	}, true, nil
}

// handleLoginUsers is the gap #3 unified /api/login. It replaces the
// legacy handleLogin registration (Register); when the users store is not
// wired (in-memory mode) it delegates to the legacy handler unchanged —
// body included, so no double decode / double rate-limit.
//
// Contract:
//
//	POST {"username", "password", "totp"?}
//
//   - 200 {"token","expiry","capabilities","role","username"} — the token
//     is a session JWT (role + username claims; legacy accounts are minted
//     role "admin", grandfathered).
//   - 401 {"error":"mfa_required"} — the matched users row has TOTP
//     enrolled and no code was sent. The UI shows the 6-digit field and
//     re-submits; this outcome does NOT count against the IP's failure
//     budget (it's a challenge, not a credential failure).
//   - 401 {"error":"invalid username or password"} — every other
//     credential failure (wrong password, wrong TOTP code, disabled
//     account, dead env pair). One shape, no enumeration oracle.
//
// Credential resolution (setup.Authenticate): users row → wizard
// admin_users row → env pair (only while the users table is empty —
// DB-only once any operator account exists).
func (s *Server) handleLoginUsers(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		s.handleLogin(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Totp     string `json:"totp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// L8: the same per-IP failure budget as the legacy handler (before the
	// PBKDF2 work — brute force + work-amplification DoS).
	ip := ""
	if s.loginLimiter != nil {
		ip = clientIP(r)
		if until, ok := s.loginLimiter.allow(ip); !ok {
			secs := int64(time.Until(until).Seconds()) + 1
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "too many failed login attempts; try again later",
			})
			return
		}
	}
	limitFail := func() {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFail(ip)
		}
	}
	limitOK := func() {
		if s.loginLimiter != nil {
			s.loginLimiter.recordOK(ip)
		}
	}
	// Env bootstrap pair (per-boot salt/hash, timing-safe compare — the
	// same check the legacy handler inlines).
	env := func(username, password string) bool {
		candidate := pbkdf2.Key([]byte(password), s.adminSalt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.adminUser)) == 1
		passOK := subtle.ConstantTimeCompare(candidate, s.adminHash) == 1
		return userOK && passOK
	}

	res, err := s.setup.Authenticate(r.Context(), in.Username, in.Password, in.Totp, usersAuthAdapter{s.users}, env)
	if err != nil {
		http.Error(w, "login: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch res.Outcome {
	case setup.AuthMFARequired:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "mfa_required"})
		return
	case setup.AuthInvalid:
		limitFail()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	case setup.AuthOK:
	default:
		http.Error(w, "login: unknown outcome "+res.Outcome, http.StatusInternalServerError)
		return
	}
	// Best-effort stamps (a stamp failure must not fail the login).
	if res.AccountID != "" {
		if res.TotpEnrolled {
			_ = s.users.VerifyTotp(r.Context(), res.AccountID) // first-verified stamp
		}
		_ = s.users.MarkLoggedIn(r.Context(), res.AccountID)
	}
	tok, err := users.MintSessionJWT(s.jwtSecret, s.tokenLifetime, users.SessionClaims{
		Role:     res.Role,
		Username: res.Username,
		Caps:     s.adminCaps,
	})
	if err != nil {
		http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	limitOK()
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        tok,
		"expiry":       time.Now().Add(s.tokenLifetime).UTC().Format(time.RFC3339),
		"capabilities": s.adminCaps, // W3-3: what this session may dispatch
		"role":         res.Role,
		"username":     res.Username,
	})
}
