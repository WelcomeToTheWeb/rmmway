package caps

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
)

func TestMintVerifyRoundTrip(t *testing.T) {
	root, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	iss := NewIssuer(root, time.Minute)
	tok, err := iss.Mint("dev-abc", CapRunScript, "cmd-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := Verify(tok, root.Cert(), "dev-abc", CapRunScript, "cmd-1", time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyExpired(t *testing.T) {
	root, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	now := time.Now()
	iss := NewIssuer(root, time.Minute).WithNow(func() time.Time { return now.Add(-2 * time.Minute) })
	tok, err := iss.Mint("dev-abc", CapRunScript, "cmd-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Beyond the 10s leeway: must be refused.
	if err := Verify(tok, root.Cert(), "dev-abc", CapRunScript, "cmd-1", now.Add(-11*time.Second)); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestVerifyBindings(t *testing.T) {
	root, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	iss := NewIssuer(root, time.Minute)
	tok, err := iss.Mint("dev-abc", CapRunScript, "cmd-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Wrong device: the token is bound to ONE device.
	if err := Verify(tok, root.Cert(), "dev-other", CapRunScript, "cmd-1", time.Now()); err == nil ||
		!strings.Contains(err.Error(), "bound to device") {
		t.Fatalf("wrong-device check: %v", err)
	}
	// Wrong capability: run_script token does not authorize reboot.
	if err := Verify(tok, root.Cert(), "dev-abc", CapReboot, "cmd-1", time.Now()); err == nil ||
		!strings.Contains(err.Error(), "grants") {
		t.Fatalf("wrong-cap check: %v", err)
	}
	// Wrong command: a token bound to another command id must fail (this is
	// what stops a replayed command frame from riding an old token).
	if err := Verify(tok, root.Cert(), "dev-abc", CapRunScript, "cmd-OTHER", time.Now()); err == nil ||
		!strings.Contains(err.Error(), "bound to command") {
		t.Fatalf("wrong-command check: %v", err)
	}
	// Unsigned-by-us: a token signed by a DIFFERENT org root must fail.
	other, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("other root: %v", err)
	}
	otherTok, err := NewIssuer(other, time.Minute).Mint("dev-abc", CapRunScript, "cmd-1")
	if err != nil {
		t.Fatalf("mint other: %v", err)
	}
	if err := Verify(otherTok, root.Cert(), "dev-abc", CapRunScript, "cmd-1", time.Now()); err == nil {
		t.Fatal("token signed by the wrong root accepted")
	}
}

func TestVerifyWrongAlgorithm(t *testing.T) {
	root, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	// An HS256 token (any key bytes) must be rejected: only ES256 by the org
	// root is trusted (blocks alg-confusion against the HMAC secret path).
	hmacTok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Cap: CapRunScript,
		Cmd: "cmd-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "dev-abc",
			Issuer:    TokenIssuer,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}).SignedString([]byte("some-hmac-secret"))
	if err != nil {
		t.Fatalf("mint hmac: %v", err)
	}
	if err := Verify(hmacTok, root.Cert(), "dev-abc", CapRunScript, "cmd-1", time.Now()); err == nil ||
		!strings.Contains(err.Error(), "signing method") {
		t.Fatalf("HS256 token accepted: %v", err)
	}
	// Garbage is rejected too.
	if err := Verify("not.a.jwt", root.Cert(), "dev-abc", CapRunScript, "cmd-1", time.Now()); err == nil {
		t.Fatal("garbage token accepted")
	}
}

func TestForAction(t *testing.T) {
	if c, ok := ForAction(&agentv1.Command_RunScript{}); !ok || c != CapRunScript {
		t.Fatalf("run_script: %q %v", c, ok)
	}
	if c, ok := ForAction(&agentv1.Command_Reboot{}); !ok || c != CapReboot {
		t.Fatalf("reboot: %q %v", c, ok)
	}
	if _, ok := ForAction("unknown"); ok {
		t.Fatal("unknown action must not map to a capability")
	}
}
