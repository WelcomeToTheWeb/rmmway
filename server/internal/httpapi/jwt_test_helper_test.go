package httpapi

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// makeAgentTokenForTest signs a token shaped exactly like an agent JWT:
// subject is a device id, no issuer, same HS256 secret as the operator
// tokens. It must be accepted by the agent verifier and rejected by the
// operator verifier (and vice versa).
func makeAgentTokenForTest(t *testing.T) string {
	t.Helper()
	secret := []byte("test-secret") // matches newTestServer
	claims := jwt.RegisteredClaims{
		Subject:   "dev-agent-1",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("sign agent token: %v", err)
	}
	return tok
}
