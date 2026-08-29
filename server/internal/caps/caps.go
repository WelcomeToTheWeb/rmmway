// Package caps implements W3-3: per-action capability tokens.
//
// Every dispatched command carries a short-lived capability token that binds
// ONE device (sub) + ONE capability (cap) + ONE command id (cmd/jti), and
// expires after a short window (the issuer's TTL, server RMMWAY_CAP_TTL,
// default 10m). Tokens are compact JWTs signed ES256 by the ORG ROOT CA key
// — the same trust anchor the agent already pins from enroll (W3-1), so no
// new key material has to reach the agent: it verifies a token with the root
// it trusts for mTLS.
//
// This is what makes "an agent can't act beyond its minted scope" true at
// the agent: a fully valid mTLS + JWT channel is NOT enough to make the
// agent act — the command must also carry a token the agent's own verifier
// accepts (signature, device, capability, not expired). A command failing
// that check is refused (CommandResult.status=REFUSED) and not executed.
package caps

import (
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
)

// Capability names. The agent-side verifier (agent/internal/caps) mirrors
// these constants — the wire contract is documented in commands.proto.
const (
	CapRunScript = "rmmway.run_script"
	CapReboot    = "rmmway.reboot"

	// TokenIssuer is the `iss` claim of every capability token.
	TokenIssuer = "rmmway"
)

// AllCapabilities is the full Phase 1 capability set (the default admin
// grant). New actions must add their capability here.
var AllCapabilities = []string{CapRunScript, CapReboot}

// ForAction maps a dispatch action (the Command oneof member) to the
// capability it requires. Unknown actions are reported (ok=false).
func ForAction(action any) (string, bool) {
	switch action.(type) {
	case *agentv1.Command_RunScript:
		return CapRunScript, true
	case *agentv1.Command_Reboot:
		return CapReboot, true
	default:
		return "", false
	}
}

// Claims is the capability-token claim set. sub is the device the token is
// bound to; cap is the capability; cmd/jti is the one command id the token
// authorizes (a token is single-use-by-design per command, and bound to it
// so a replayed command frame carries a token that no longer matches).
type Claims struct {
	Cap string `json:"cap"`
	Cmd string `json:"cmd"`
	jwt.RegisteredClaims
}

// Issuer mints capability tokens with the org root key.
//
// The root is read under a mutex: A-2's setup wizard can re-issue the org
// root (under the operator's org name) before the first enroll, and every
// token minted after the swap must be signed by the NEW root — the one
// fresh agents will pin.
type Issuer struct {
	mu   sync.Mutex
	root *ca.Root
	ttl  time.Duration
	now  func() time.Time
}

// NewIssuer builds an Issuer. ttl <= 0 falls back to the default 10m.
func NewIssuer(root *ca.Root, ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Issuer{root: root, ttl: ttl, now: time.Now}
}

// ReplaceRoot (A-2) swaps the signing root (called after the setup wizard
// re-issues the org CA). Concurrent Mint calls observe either the old or
// the new root, never a torn state.
func (i *Issuer) ReplaceRoot(root *ca.Root) {
	if root == nil {
		return
	}
	i.mu.Lock()
	i.root = root
	i.mu.Unlock()
}

// WithNow injects the clock (tests).
func (i *Issuer) WithNow(f func() time.Time) *Issuer {
	i.now = f
	return i
}

// TTL returns the minted token lifetime.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Mint signs a capability token for (deviceID, capability, commandID),
// valid from now for the issuer TTL.
func (i *Issuer) Mint(deviceID, capability, commandID string) (string, error) {
	if deviceID == "" || capability == "" || commandID == "" {
		return "", fmt.Errorf("caps: device, capability and command id are all required")
	}
	i.mu.Lock()
	root := i.root
	i.mu.Unlock()
	now := i.now()
	claims := Claims{
		Cap: capability,
		Cmd: commandID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   deviceID,
			Issuer:    TokenIssuer,
			ID:        commandID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(root.Key())
}

// Key exposes the org root's ECDSA private key (tests / harnesses that mint
// rogue tokens with the same authority).
func (i *Issuer) Key() *ecdsa.PrivateKey {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.root.Key()
}

// DecodeClaims parses a token into its claims WITHOUT verifying the
// signature — test/harness introspection only (the e2e harness inspects
// minted tokens; never trust this for authorization).
func DecodeClaims(token string) (*Claims, error) {
	claims := &Claims{}
	_, _, err := jwt.NewParser().ParseUnverified(token, claims)
	return claims, err
}

// Verify checks a capability token against the org ROOT CERT (the pinned
// trust anchor) and the expected device + capability + command binding.
// It is the server-side reference implementation of the agent-side check
// (agent/internal/caps): signature (ES256, org root only), issuer, device
// binding, capability binding, command binding (a token authorizes exactly
// one command — a replayed frame carries a token that no longer matches),
// and expiry (a small leeway absorbs agent/server clock skew).
// commandID may be empty (server-side audit callers that don't bind).
func Verify(token string, rootCert *x509.Certificate, deviceID, capability, commandID string, now time.Time) error {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v (want ES256)", t.Header["alg"])
		}
		pub, ok := rootCert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("org root has no ECDSA public key")
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodES256.Alg()}),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("capability token invalid: %w", err)
	}
	if claims.Issuer != TokenIssuer {
		return fmt.Errorf("capability token has unexpected issuer %q", claims.Issuer)
	}
	if claims.Subject != deviceID {
		return fmt.Errorf("capability token is bound to device %q, not %q", claims.Subject, deviceID)
	}
	if claims.Cap != capability {
		return fmt.Errorf("capability token grants %q, command requires %q", claims.Cap, capability)
	}
	if commandID != "" && claims.Cmd != commandID {
		return fmt.Errorf("capability token is bound to command %q, not %q", claims.Cmd, commandID)
	}
	_ = now
	return nil
}
