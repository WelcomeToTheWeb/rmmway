package users

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/welcometotheweb/rmmway/server/internal/ingest"
)

var testSecret = []byte("rmmway-test-jwt-secret-0123456789")

func TestSessionJWTRoundtrip(t *testing.T) {
	tok, err := MintSessionJWT(testSecret, time.Hour, SessionClaims{
		Role: "tech", Username: "alice", Caps: []string{"command.restart-service"},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	c, ok := ParseSessionJWT(testSecret, tok)
	if !ok {
		t.Fatal("parse: ok=false on a fresh token")
	}
	if c.Role != "tech" || c.Username != "alice" || len(c.Caps) != 1 || c.Caps[0] != "command.restart-service" {
		t.Fatalf("claims: got %+v", c)
	}
	id := IdentityFromClaims(c)
	if id.Role != "tech" || id.Username != "alice" || id.Legacy {
		t.Fatalf("identity: got %+v", id)
	}
}

// TestSessionJWTLegacyCompatibility pins the cross-contract:
//  - a legacy ingest.OperatorJWT (no role claim) parses as a session with
//    Role "" → IdentityFromClaims normalizes to a grandfathered admin;
//  - a session JWT parses through ingest.ParseOperatorJWT (the frozen
//    httpapi requireOperator path) with its caps intact.
func TestSessionJWTLegacyCompatibility(t *testing.T) {
	legacy, err := ingest.MintOperatorJWT(testSecret, time.Hour, []string{"command.restart-service"})
	if err != nil {
		t.Fatalf("mint legacy: %v", err)
	}
	c, ok := ParseSessionJWT(testSecret, legacy)
	if !ok {
		t.Fatal("legacy token must parse as a session token")
	}
	id := IdentityFromClaims(c)
	if id.Role != "admin" || !id.Legacy {
		t.Fatalf("legacy token identity: got %+v (want grandfathered admin)", id)
	}

	sess, err := MintSessionJWT(testSecret, time.Hour, SessionClaims{
		Role: "viewer", Username: "bob", Caps: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	caps, ok := ingest.ParseOperatorJWT(testSecret, sess)
	if !ok {
		t.Fatal("session token must parse through the legacy requireOperator path")
	}
	if len(caps) != 2 || caps[0] != "a" || caps[1] != "b" {
		t.Fatalf("legacy parse of session token: caps %v", caps)
	}
}

func TestSessionJWTRejections(t *testing.T) {
	good, err := MintSessionJWT(testSecret, time.Hour, SessionClaims{Role: "admin", Username: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := ParseSessionJWT([]byte("a-different-secret-0123456789-abc"), good); ok {
		t.Fatal("wrong-secret token accepted")
	}
	if _, ok := ParseSessionJWT(testSecret, good+"x"); ok {
		t.Fatal("tampered token accepted")
	}
	if _, ok := ParseSessionJWT(testSecret, "not-a-jwt"); ok {
		t.Fatal("non-token accepted")
	}

	// Expired: mint with a negative lifetime.
	expired, err := MintSessionJWT(testSecret, -time.Minute, SessionClaims{Role: "admin", Username: "admin"})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if _, ok := ParseSessionJWT(testSecret, expired); ok {
		t.Fatal("expired token accepted")
	}

	// Wrong subject/issuer are refused even with a valid signature.
	now := time.Now()
	wrongSub, err := jwt.NewWithClaims(jwt.SigningMethodHS256, SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "agent", Issuer: ingest.JWTIssuer,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}).SignedString(testSecret)
	if err != nil {
		t.Fatalf("mint wrong-subject: %v", err)
	}
	if _, ok := ParseSessionJWT(testSecret, wrongSub); ok {
		t.Fatal("wrong-subject token accepted")
	}
}
