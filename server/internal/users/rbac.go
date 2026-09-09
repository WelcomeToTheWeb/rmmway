package users

import (
	"context"
	"net/http"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- RBAC middleware (gap #3, wave 2, lane B) --------------------------------

// RBAC resolves a bearer token (session JWT or rmm_ API token) into a
// scoped Session for the operator surface.
type RBAC struct {
	Secret []byte
	// Users is the operator account store; nil = not wired (in-memory
	// mode): every valid session is a grandfathered admin and client
	// scoping is off — the pre-gap-#3 flat-operator behavior.
	Users store.UserStore
}

// Session is one authenticated operator for a single request.
type Session struct {
	Username string
	Role     string // always admin|tech|viewer (Legacy → admin)
	Caps     []string
	Legacy   bool // token predates the role claim (grandfathered admin)
	// AllClients: admin / legacy / unwired — ?client= passes through and
	// no grant filter applies.
	AllClients bool
	// ClientIDs: the granted client ids (non-admin, wired). "" entries are
	// possible (the "unassigned" default client is a real client id).
	ClientIDs []string
	// ActiveClient: the validated ?client= parameter ("" = not passed).
	ActiveClient string
}

// CanSeeClient reports whether the session may touch the given client id
// (device/alert grant check). Empty client id = the default client.
func (s Session) CanSeeClient(clientID string) bool {
	if s.AllClients {
		return true
	}
	for _, id := range s.ClientIDs {
		if id == clientID {
			return true
		}
	}
	return false
}

type sessionKey struct{}

// WithSession binds the resolved session to the request context.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFromContext reads the session bound by the middleware (zero +
// false when the route wasn't gated by RBAC).
func SessionFromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// TokenFromRequest extracts the operator credential: the Authorization
// bearer header (curl/API clients) or the ?token= query param (the
// EventSource browser API cannot set headers — the stream route only).
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		if t := strings.TrimSpace(h[len("bearer "):]); t != "" {
			return t
		}
	}
	return r.URL.Query().Get("token")
}

// SessionFromRequest resolves the request's credential into a Session.
// ok=false means 401: no token, unknown/expired API token, bad signature,
// expired JWT, or a non-admin whose account no longer exists / is disabled
// (grants load live, so disablement is immediate — not at token expiry).
func (rb *RBAC) SessionFromRequest(r *http.Request) (Session, bool) {
	tok := TokenFromRequest(r)
	if tok == "" {
		return Session{}, false
	}
	// API token: rmm_ + 64 hex (hash-only storage, Users store required).
	if strings.HasPrefix(tok, "rmm_") {
		if rb.Users == nil {
			return Session{}, false
		}
		u, err := rb.Users.VerifyToken(r.Context(), tok)
		if err != nil {
			return Session{}, false
		}
		return sessionFromUser(u), true
	}
	claims, ok := ParseSessionJWT(rb.Secret, tok)
	if !ok {
		return Session{}, false
	}
	id := IdentityFromClaims(claims)
	sess := Session{
		Username:   id.Username,
		Role:       id.Role,
		Caps:       id.Caps,
		Legacy:     id.Legacy,
		AllClients: id.Role == RoleAdminValue,
	}
	if rb.Users != nil && !sess.AllClients {
		// Non-admin: load the account live — grants + enabled state.
		// (The JWT alone is a 12-hour-old snapshot.)
		u, err := rb.Users.GetByUsername(r.Context(), sess.Username)
		if err != nil || !u.Enabled {
			return Session{}, false
		}
		sess = sessionFromUser(u)
		sess.Caps = claims.Caps
	}
	return sess, true
}

// sessionFromUser builds a Session from a users row (admin = no grant
// table; non-admin = the row's client grants).
func sessionFromUser(u *store.User) Session {
	sess := Session{Username: u.Username, Role: u.Role, AllClients: u.Role == RoleAdminValue}
	if !sess.AllClients {
		sess.ClientIDs = u.Clients
	}
	return sess
}

// Allows reports whether the session may pass a role gate (legacy
// sessions count as admin). Exported for the handler-level role checks
// the route audit adds (multiplex handlers can't use RequireRole at the
// registration).
func (s Session) Allows(roles ...string) bool {
	role := s.Role
	if s.Legacy {
		role = RoleAdminValue
	}
	for _, want := range roles {
		if role == want {
			return true
		}
	}
	return false
}

// RoleAdminValue is the admin role (mirrors store.RoleAdmin without the
// middleware depending on the store's naming).
const RoleAdminValue = "admin"

// Require authenticates (401) and binds the Session to the context — the
// gap #3 replacement for the flat requireOperator on audited routes.
func (rb *RBAC) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := rb.SessionFromRequest(r)
		if !ok {
			writeUnauthorized(w)
			return
		}
		next(w, r.WithContext(WithSession(r.Context(), sess)))
	}
}

// RequireRole authenticates and demands one of roles (403 otherwise).
// Legacy (grandfathered) sessions always count as admin.
func (rb *RBAC) RequireRole(roles ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return rb.Require(func(w http.ResponseWriter, r *http.Request) {
			sess, _ := SessionFromContext(r.Context())
			if !roleAllowed(sess, roles) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"forbidden: insufficient role"}`))
				return
			}
			next(w, r)
		})
	}
}

// RequireClientScope authenticates and validates ?client= against the
// session's grants (403 when a non-admin names a client they cannot see).
// A non-admin who passes NO ?client= gets the union of their grants — the
// handler reads Session.ActiveClient ("" = union) and Session.ClientIDs.
func (rb *RBAC) RequireClientScope(next http.HandlerFunc) http.HandlerFunc {
	return rb.Require(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := SessionFromContext(r.Context())
		if c := r.URL.Query().Get("client"); c != "" && !sess.CanSeeClient(c) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden: no access to client "}` + jsonString(c)+`"}`))
			return
		}
		sess.ActiveClient = r.URL.Query().Get("client")
		next(w, r.WithContext(WithSession(r.Context(), sess)))
	})
}

// roleAllowed reports whether the session may pass a role gate.
func roleAllowed(sess Session, roles []string) bool { return sess.Allows(roles...) }

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// jsonString is a minimal JSON string escape for the 403 body (client ids
// are server-minted slugs, but stay correct anyway).
func jsonString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 {
				b.WriteString("\\u")
				const hexdigits = "0123456789abcdef"
				b.WriteByte(hexdigits[byte(r)>>4])
				b.WriteByte(hexdigits[byte(r)&0x0f])
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
