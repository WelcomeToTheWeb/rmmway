// Package caps is the agent-side half of W3-3 (per-action capability
// tokens).
//
// The server signs every dispatched command with a short-lived capability
// token: a compact ES256 JWT under the ORG ROOT CA key, bound to one device
// (sub), one capability (cap) and one command id (cmd/jti), expiring after
// the server's RMMWAY_CAP_TTL (default 10m). The agent verifies the token
// with the org root it already pins from enroll (W3-1 — the same trust
// anchor that makes its mTLS channel valid) and REFUSES the command
// (CommandResult.status=REFUSED, not executed) unless the token is
// signature-valid, device-bound, capability-bound, and unexpired.
//
// That is the point: a fully valid mTLS + JWT channel is NOT enough to make
// the agent act — it can't act beyond its minted scope.
//
// The claim contract mirrors server/internal/caps (documented in
// commands.proto); the agent and server are separate Go modules, so the
// small claim set is duplicated here rather than shared.
package caps

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Capability names (mirror of server/internal/caps).
const (
	CapRunScript = "rmmway.run_script"
	CapReboot    = "rmmway.reboot"

	// TokenIssuer is the `iss` claim of every capability token.
	TokenIssuer = "rmmway"
)

// Claims is the capability-token claim set (sub = device, cap, cmd/jti).
type Claims struct {
	Cap string `json:"cap"`
	Cmd string `json:"cmd"`
	jwt.RegisteredClaims
}

// Verifier checks capability tokens for ONE device against the pinned org
// root.
type Verifier struct {
	root  *x509.Certificate
	devID string
	now   func() time.Time
}

// FromRootPEM builds a Verifier from the pinned org root CA PEM (the agent's
// identity file, W3-1) and the device id the tokens must be bound to.
func FromRootPEM(rootPEM []byte, devID string) (*Verifier, error) {
	block, _ := pem.Decode(rootPEM)
	if block == nil {
		return nil, fmt.Errorf("no certificate in the org root PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse org root: %w", err)
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("org root is not an ECDSA CA (capability tokens need ES256)")
	}
	return &Verifier{root: cert, devID: devID, now: time.Now}, nil
}

// WithNow injects the clock (tests).
func (v *Verifier) WithNow(f func() time.Time) *Verifier { v.now = f; return v }

// Check verifies that token authorizes capability cap for command cmdID
// on this device. Any failure (missing token, bad signature, wrong issuer,
// wrong device, wrong capability, wrong command binding, expired) is an
// error — the caller refuses the command.
func (v *Verifier) Check(token, cap, cmdID string) error {
	if token == "" {
		return fmt.Errorf("missing capability token (server capability enforcement is on)")
	}
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v (want ES256)", t.Header["alg"])
		}
		return v.root.PublicKey.(*ecdsa.PublicKey), nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodES256.Alg()}),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("capability token invalid: %w", err)
	}
	if claims.Issuer != TokenIssuer {
		return fmt.Errorf("capability token has unexpected issuer %q", claims.Issuer)
	}
	if claims.Subject != v.devID {
		return fmt.Errorf("capability token is bound to device %q, not %q", claims.Subject, v.devID)
	}
	if claims.Cap != cap {
		return fmt.Errorf("capability token grants %q, command requires %q", claims.Cap, cap)
	}
	if cmdID != "" && claims.Cmd != cmdID {
		return fmt.Errorf("capability token is bound to command %q, not %q", claims.Cmd, cmdID)
	}
	_ = v.now
	return nil
}

// ForCommand extracts (capability, token) for a dispatched command. ok is
// false for unknown action types (the agent answers UNSUPPORTED, not
// REFUSED — there is no capability to check).
func ForCommand(cmd *agentv1.Command) (capability, token string, ok bool) {
	switch a := cmd.GetAction().(type) {
	case *agentv1.Command_RunScript:
		return CapRunScript, a.RunScript.GetCapabilityToken(), true
	case *agentv1.Command_Reboot:
		return CapReboot, a.Reboot.GetCapabilityToken(), true
	default:
		return "", "", false
	}
}
