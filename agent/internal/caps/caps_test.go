package caps

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// newRoot mints a throwaway ECDSA P-256 self-signed CA (the shape of the
// server's org root) and returns (rootPEM, signingKey).
func newRoot(t *testing.T) (rootPEM []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Test Org Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

// mint signs a capability token under the root key (the server-side Mint,
// mirrored for the agent's test fixtures).
func mint(t *testing.T, key *ecdsa.PrivateKey, devID, capName, cmdID string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodES256, Claims{
		Cap: capName,
		Cmd: cmdID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   devID,
			Issuer:    TokenIssuer,
			ID:        cmdID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}).SignedString(key)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

func TestCheckRoundTrip(t *testing.T) {
	rootPEM, key := newRoot(t)
	v, err := FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	tok := mint(t, key, "dev-abc", CapRunScript, "cmd-1", time.Minute)
	if err := v.Check(tok, CapRunScript, "cmd-1"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestCheckRefusals(t *testing.T) {
	rootPEM, key := newRoot(t)
	v, err := FromRootPEM(rootPEM, "dev-abc")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	good := mint(t, key, "dev-abc", CapRunScript, "cmd-1", time.Minute)

	// Missing token.
	if err := v.Check("", CapRunScript, "cmd-1"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing token: %v", err)
	}
	// Expired.
	expired := mint(t, key, "dev-abc", CapRunScript, "cmd-1", -2*time.Minute)
	if err := v.Check(expired, CapRunScript, "cmd-1"); err == nil {
		t.Fatal("expired token accepted")
	}
	// Bound to a different device (the cross-device replay case).
	otherDev := mint(t, key, "dev-elsewhere", CapRunScript, "cmd-1", time.Minute)
	if err := v.Check(otherDev, CapRunScript, "cmd-1"); err == nil || !strings.Contains(err.Error(), "bound to device") {
		t.Fatalf("wrong device: %v", err)
	}
	// Wrong capability (run_script token for a reboot command).
	if err := v.Check(good, CapReboot, "cmd-1"); err == nil || !strings.Contains(err.Error(), "grants") {
		t.Fatalf("wrong cap: %v", err)
	}
	// Signed by a DIFFERENT org root (not the pinned one).
	_, otherKey := newRoot(t)
	foreign := mint(t, otherKey, "dev-abc", CapRunScript, "cmd-1", time.Minute)
	if err := v.Check(foreign, CapRunScript, "cmd-1"); err == nil {
		t.Fatal("token from the wrong root accepted")
	}
	// Bound to a different command (a replayed frame from an earlier
	// dispatch carries a token that no longer matches this command id).
	otherCmd := mint(t, key, "dev-abc", CapRunScript, "cmd-OTHER", time.Minute)
	if err := v.Check(otherCmd, CapRunScript, "cmd-1"); err == nil || !strings.Contains(err.Error(), "bound to command") {
		t.Fatalf("wrong command binding: %v", err)
	}
	// Wrong issuer.
	issuer, err := jwt.NewWithClaims(jwt.SigningMethodES256, Claims{
		Cap: CapRunScript, Cmd: "cmd-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "dev-abc", Issuer: "someone-else",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}).SignedString(key)
	if err != nil {
		t.Fatalf("mint issuer: %v", err)
	}
	if err := v.Check(issuer, CapRunScript, "cmd-1"); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("wrong issuer: %v", err)
	}
	// Garbage.
	if err := v.Check("garbage", CapRunScript, "cmd-1"); err == nil {
		t.Fatal("garbage token accepted")
	}
	// Non-ES256 (HS256) is rejected on method, not just signature.
	hmac, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Cap: CapRunScript, Cmd: "cmd-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "dev-abc", Issuer: TokenIssuer,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}).SignedString([]byte("hmac-secret"))
	if err != nil {
		t.Fatalf("mint hmac: %v", err)
	}
	if err := v.Check(hmac, CapRunScript, "cmd-1"); err == nil || !strings.Contains(err.Error(), "signing method") {
		t.Fatalf("HS256 token: %v", err)
	}
}

func TestForCommand(t *testing.T) {
	cap, tok, ok := ForCommand(&agentv1.Command{
		Action: &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{CapabilityToken: "tok-rs"}},
	})
	if !ok || cap != CapRunScript || tok != "tok-rs" {
		t.Fatalf("run_script: %q %q %v", cap, tok, ok)
	}
	cap, tok, ok = ForCommand(&agentv1.Command{
		Action: &agentv1.Command_Reboot{Reboot: &agentv1.Reboot{CapabilityToken: "tok-rb"}},
	})
	if !ok || cap != CapReboot || tok != "tok-rb" {
		t.Fatalf("reboot: %q %q %v", cap, tok, ok)
	}
	if _, _, ok := ForCommand(&agentv1.Command{}); ok {
		t.Fatal("no action must not map to a capability")
	}
}
