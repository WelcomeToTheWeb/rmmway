package users

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/welcometotheweb/rmmway/server/internal/ingest"
)

// SessionClaims is the operator session token (gap #3): the legacy
// ingest.OperatorJWT contract (HS256, subject "operator", issuer "rmmway",
// "caps" claim) extended with the RBAC role and the account's username.
//
// Legacy tokens (minted before this wave — no role claim) parse with an
// EMPTY Role; the middleware normalizes that to admin (grandfathered: an
// existing signed-in operator keeps full access until re-login, and the
// ingest path never has to change).
type SessionClaims struct {
	Role     string   `json:"role,omitempty"`
	Username string   `json:"username,omitempty"`
	Caps     []string `json:"caps,omitempty"`
	jwt.RegisteredClaims
}

// MintSessionJWT signs an operator session token valid for lifetime.
// Role "" is accepted (it mints a legacy-shaped token).
func MintSessionJWT(secret []byte, lifetime time.Duration, c SessionClaims) (string, error) {
	now := time.Now()
	c.RegisteredClaims = jwt.RegisteredClaims{
		Subject:   ingest.OperatorSubject,
		Issuer:    ingest.JWTIssuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
}

// ParseSessionJWT validates a bearer token against the operator contract.
// It returns the claims (role may be empty for legacy tokens) and ok.
// Wrong kind, bad signature, expired, or malformed → ok=false.
func ParseSessionJWT(secret []byte, tok string) (*SessionClaims, bool) {
	claims := &SessionClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, false
	}
	if claims.Subject != ingest.OperatorSubject || claims.Issuer != ingest.JWTIssuer {
		return nil, false
	}
	return claims, true
}

// Identity is the normalized view of a session's claims — what the
// middleware and the Users API render.
type Identity struct {
	Username string
	Role     string // always one of admin|tech|viewer (never "")
	Caps     []string
	Legacy   bool // token predates the role claim (grandfathered admin)
}

// IdentityFromClaims normalizes parsed claims: an empty role is the legacy
// admin (see SessionClaims).
func IdentityFromClaims(c *SessionClaims) Identity {
	id := Identity{Username: c.Username, Role: c.Role, Caps: c.Caps}
	if id.Role == "" {
		id.Role = "admin"
		id.Legacy = true
	}
	return id
}
