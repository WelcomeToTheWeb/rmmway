package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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

// ---- RBAC route gates (gap #3 route audit) -----------------------------------

// withCaps re-binds the legacy caps context key from the RBAC session so
// hasCapability (the dispatch capability check in domain_commands.go)
// keeps working under the new gates — the frozen requireOperator used to
// set it from the JWT's caps claim; API tokens have no caps, so their
// dispatches fail the capability check exactly like pre-gap-#3.
func withCaps(ctx context.Context) context.Context {
	sess, _ := users.SessionFromContext(ctx)
	return context.WithValue(ctx, capsKey{}, sess.Caps)
}

// rbacGate authenticates (session JWT or rmm_ token) and binds session +
// caps — the audited replacement for s.requireOperator on routes that
// do their own scoping (single-device paths, the alert inbox).
func (s *Server) rbacGate(next http.HandlerFunc) http.HandlerFunc {
	return s.rbac.Require(func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(withCaps(r.Context())))
	})
}

// rbacScopeGate = rbacGate + ?client= validation against the grants
// (403 when a non-admin names a client they cannot see; no param = the
// handler unions the grants).
func (s *Server) rbacScopeGate(next http.HandlerFunc) http.HandlerFunc {
	return s.rbac.RequireClientScope(func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(withCaps(r.Context())))
	})
}

// rbacRoleGate = rbacGate + a role requirement (legacy = admin).
func (s *Server) rbacRoleGate(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return s.rbac.RequireRole(roles...)(func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(withCaps(r.Context())))
	})
}

// denyForbidden writes the audit's standard 403.
func denyForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"forbidden"}`))
}

// requireClientAccess (gap #3): the session must see the client the given
// device/alert belongs to. Empty client id = the default client.
func requireClientAccess(w http.ResponseWriter, r *http.Request, clientID string) bool {
	sess, _ := users.SessionFromContext(r.Context())
	if !sess.CanSeeClient(clientID) {
		denyForbidden(w)
		return false
	}
	return true
}

// requireRole (gap #3): handler-level role check for the multiplex
// handlers the registration-level gate can't discriminate (e.g. PATCH on
// the device/alert sub-routes while GET stays open to viewers).
func requireRole(w http.ResponseWriter, r *http.Request, roles ...string) bool {
	sess, _ := users.SessionFromContext(r.Context())
	if !sess.Allows(roles...) {
		denyForbidden(w)
		return false
	}
	return true
}

// ---- users + API-token management (gap #3) ------------------------------------

// registerUsers mounts the operator-account surface. All routes are
// admin-only (managing the operator surface is an admin act; tech/viewer
// sessions authenticate but get 403 here).
func registerUsers(s *Server, mux *http.ServeMux) {
	gated := s.rbacRoleGate(s.handleUsers, "admin")
	sub := s.rbacRoleGate(s.handleUserSub, "admin")
	mux.HandleFunc("/api/users", gated)
	mux.HandleFunc("/admin/users", gated)
	mux.HandleFunc("/api/users/", sub)
	mux.HandleFunc("/admin/users/", sub)
}

// usersUnwired is the 503 for in-memory mode (no account model).
func (s *Server) usersUnwired(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "operator accounts require a database (in-memory mode)",
	})
}

// userOut is the API read model — never the salt/hash (or token hashes).
func userOut(u *store.User) map[string]any {
	clients := u.Clients
	if clients == nil {
		clients = []string{}
	}
	return map[string]any{
		"id":            u.ID,
		"username":      u.Username,
		"role":          u.Role,
		"enabled":       u.Enabled,
		"totp_enrolled": u.TotpSecret != "",
		"totp_verified": u.TotpVerified != nil,
		"last_login_at": u.LastLoginAt,
		"clients":       clients,
		"created_at":    u.CreatedAt,
		"updated_at":    u.UpdatedAt,
	}
}

// handleUsers — GET: the account list; POST: create.
//
//	POST /api/users  {username, password, role, clients?}
//
// \t200/201 {user}
// \t400 bad body / bad role / short password
// \t409 username exists
// \t503 in-memory mode
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		s.usersUnwired(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := s.users.List(r.Context())
		if err != nil {
			http.Error(w, "users: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]map[string]any, 0, len(users))
		for _, u := range users {
			out = append(out, userOut(u))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Username string   `json:"username"`
			Password string   `json:"password"`
			Role     string   `json:"role"`
			Clients  []string `json:"clients"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Username == "" || len(in.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and a password of at least 8 characters are required"})
			return
		}
		if !store.ValidRole(in.Role) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin, tech, or viewer"})
			return
		}
		u, err := s.users.Create(r.Context(), in.Username, in.Role, in.Password, in.Clients)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrUsernameExists):
				writeJSON(w, http.StatusConflict, map[string]string{"error": "username already exists"})
			default:
				http.Error(w, "users: "+err.Error(), http.StatusInternalServerError)
			}
			return
		}
		writeJSON(w, http.StatusCreated, userOut(u))
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleUserSub routes /api/users/{id} and its /totp + /tokens subtrees.
//
//	PATCH   /api/users/{id}            {role?, enabled?, clients?, password?}
//	POST    /api/users/{id}/totp/start      -> {secret, uri}
//	POST    /api/users/{id}/totp/confirm    {code}  (verifies + stamps)
//	DELETE  /api/users/{id}/totp            (2FA reset)
//	POST    /api/users/{id}/tokens          {name, ttl_days?}  -> {token, prefix, expires_at} (token shown once)
//	GET     /api/users/{id}/tokens          -> [{id, name, prefix, created_at, expires_at, last_used_at}]
//	DELETE  /api/users/{id}/tokens/{tid}    (revoke)
func (s *Server) handleUserSub(w http.ResponseWriter, r *http.Request) {
	if s.users == nil {
		s.usersUnwired(w)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
	rest = strings.TrimPrefix(rest, "/admin/users/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		s.patchUser(w, r, parts[0])
		return
	}
	if len(parts) >= 2 && parts[0] != "" && parts[1] == "totp" {
		s.totpSub(w, r, parts[0], parts[2:])
		return
	}
	if len(parts) >= 2 && parts[0] != "" && parts[1] == "tokens" {
		s.tokensSub(w, r, parts[0], parts[2:])
		return
	}
	http.Error(w, "expected /users/{id}, /users/{id}/totp/{start|confirm}, or /users/{id}/tokens[/{tid}]", http.StatusNotFound)
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Role     *string  `json:"role"`
		Enabled  *bool    `json:"enabled"`
		Clients  *[]string `json:"clients"`
		Password string   `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var clients []string
	if in.Clients != nil {
		clients = *in.Clients
	}
	if in.Role != nil && !store.ValidRole(*in.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin, tech, or viewer"})
		return
	}
	u, err := s.users.Update(r.Context(), id, in.Role, in.Enabled, clients)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		case errors.Is(err, store.ErrUsernameExists):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "username already exists"})
		default:
			http.Error(w, "users: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if in.Password != "" {
		if len(in.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
			return
		}
		if err := s.users.SetPassword(r.Context(), id, in.Password); err != nil {
			http.Error(w, "users: "+err.Error(), http.StatusInternalServerError)
			return
		}
		u, _ = s.users.Get(r.Context(), id)
	}
	writeJSON(w, http.StatusOK, userOut(u))
}

func (s *Server) totpSub(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	switch {
	case len(rest) == 1 && rest[0] == "start" && r.Method == http.MethodPost:
		secret, err := users.TotpSecret()
		if err != nil {
			http.Error(w, "totp: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := s.users.Get(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if err := s.users.SetTotpSecret(r.Context(), id, secret); err != nil {
			http.Error(w, "totp: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// From this moment the account needs a code to log in — the UI
		// must complete confirm before closing (v1 lockout window,
		// documented in the Users screen).
		writeJSON(w, http.StatusOK, map[string]any{
			"secret": secret,
			"uri":    users.TotpURI(id, secret),
		})
	case len(rest) == 1 && rest[0] == "confirm" && r.Method == http.MethodPost:
		var in struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		u, err := s.users.Get(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		if u.TotpSecret == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "totp not enrolled — call start first"})
			return
		}
		if !users.VerifyTotp(u.TotpSecret, in.Code, time.Now()) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid code"})
			return
		}
		if err := s.users.VerifyTotp(r.Context(), id); err != nil {
			http.Error(w, "totp: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case len(rest) == 0 && r.Method == http.MethodDelete:
		if err := s.users.ClearTotp(r.Context(), id); err != nil {
			http.Error(w, "totp: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "expected /users/{id}/totp/{start|confirm} or DELETE /users/{id}/totp", http.StatusNotFound)
	}
}

func (s *Server) tokensSub(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		var in struct {
			Name    string `json:"name"`
			TTLDays int    `json:"ttl_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		ttl := 30 * 24 * time.Hour
		if in.TTLDays > 0 {
			ttl = time.Duration(in.TTLDays) * 24 * time.Hour
		}
		token, prefix, expiresAt, err := s.users.CreateToken(r.Context(), id, in.Name, ttl)
		if err != nil {
			http.Error(w, "tokens: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// The full token is shown exactly once (only the hash is stored).
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      token,
			"prefix":     prefix,
			"expires_at": expiresAt,
		})
	case len(rest) == 0 && r.Method == http.MethodGet:
		tokens, err := s.users.ListTokens(r.Context(), id)
		if err != nil {
			http.Error(w, "tokens: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]map[string]any, 0, len(tokens))
		for _, tk := range tokens {
			out = append(out, map[string]any{
				"id":           tk.ID,
				"name":         tk.Name,
				"prefix":       tk.TokenPrefix,
				"created_at":   tk.CreatedAt,
				"expires_at":   tk.ExpiresAt,
				"last_used_at": tk.LastUsedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		if err := s.users.RevokeToken(r.Context(), rest[0]); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
				return
			}
			http.Error(w, "tokens: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "expected /users/{id}/tokens or /users/{id}/tokens/{tid}", http.StatusNotFound)
	}
}
