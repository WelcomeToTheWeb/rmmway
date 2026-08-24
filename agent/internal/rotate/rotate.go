// Package rotate is the agent's W3-2 leaf-cert rotation loop: it watches the
// persisted mTLS leaf and, well before the cert's not-after, calls the
// server's RefreshLeaf RPC — over the mTLS channel the agent ALREADY holds,
// presenting the still-valid current leaf — and swaps the fresh leaf into the
// identity (in memory + persisted).
//
// Because rotation always happens inside the old cert's validity window, the
// uplink is never dropped for a renewal: the old leaf authenticates the
// RefreshLeaf call, the agent atomically swaps the material, and only future
// handshakes (reconnects, or a process restart) use the new cert. If the
// uplink drops during the swap, the normal reconnect loop retries — and by
// then presents the fresh leaf.
package rotate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Refresher is the generated client's RefreshLeaf method (satisfied by
// agentv1.AgentServiceClient).
type Refresher interface {
	RefreshLeaf(ctx context.Context, in *agentv1.RefreshLeafRequest, opts ...grpc.CallOption) (*agentv1.RefreshLeafResponse, error)
}

// Identity is the mTLS material the rotator swaps in. It is satisfied by
// *enroll.TLSIdentity (which carries the persisted leaf pair + org root),
// keeping this package generic and testable with synthetic certs.
type Identity interface {
	// CurrentLeafPEM returns the currently-presented leaf cert (PEM).
	CurrentLeafPEM() []byte
	// SwapLeaf atomically replaces the leaf cert + key in memory. The
	// caller persists it (the identity store) after a successful swap.
	SwapLeaf(certPEM, keyPEM []byte)
	// Valid reports whether the identity is complete.
	Valid() bool
}

// Config tunes the rotation loop.
type Config struct {
	// RotateFrac is the fraction of the cert's lifetime that must remain
	// before rotation fires (0 -> default 0.25: renew when a quarter of the
	// life is left), capped at MaxRotateLead (0 -> 30m) so a long-lived
	// legacy cert (a 24h W3-1 leaf) converges onto the short-lived schedule
	// instead of sitting out hours of its tail.
	RotateFrac float64
	// MaxRotateLead caps how much life must remain at a rotation (see
	// RotateFrac); 0 -> 30m.
	MaxRotateLead time.Duration
	// MinInterval bounds how often a refresh may fire (0 -> 1m): a cert with
	// a very short TTL (tests) won't hammer the server.
	MinInterval time.Duration
	// MaxBackoff caps the retry backoff after a failed refresh (0 -> 5m).
	MaxBackoff time.Duration
	// RotateAfter forces the first rotation this long after process start,
	// regardless of the cert's remaining life (0 = disabled). The e2e
	// milestone sets it via RMMWAY_ROTATE_AFTER so a ~1h leaf rotates in
	// seconds and the rotation is observed live without waiting an hour.
	// It applies to the first rotation only; afterwards the normal
	// threshold governs.
	RotateAfter time.Duration
	Logger     *slog.Logger // nil -> default
}

func (c *Config) withDefaults() {
	if c.RotateFrac <= 0 || c.RotateFrac >= 1 {
		c.RotateFrac = 0.25
	}
	if c.MaxRotateLead <= 0 {
		c.MaxRotateLead = 30 * time.Minute
	}
	if c.MinInterval <= 0 {
		c.MinInterval = time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// rotateThreshold is how much life must remain before a rotation: the smaller
// of RotateFrac of the cert's lifetime and MaxRotateLead. It is always at
// least MinInterval, so a refresh has room to retry before expiry.
func (c *Config) rotateThreshold(lifetime time.Duration) time.Duration {
	thr := time.Duration(float64(lifetime) * c.RotateFrac)
	if thr > c.MaxRotateLead {
		thr = c.MaxRotateLead
	}
	if thr < c.MinInterval && c.MinInterval > 0 {
		thr = c.MinInterval
	}
	return thr
}

// Rotator drives refreshes until ctx is canceled.
type Rotator struct {
	client   Refresher
	identity Identity
	deviceID string
	hostname string
	cfg      Config
	// persist, when set, is called after a successful in-memory swap so the
	// fresh leaf survives a process restart.
	persist func() error
}

// Option customizes a Rotator.
type Option func(*Rotator)

// WithPersist wires the identity-store save so a rotated leaf is persisted.
func WithPersist(f func() error) Option { return func(r *Rotator) { r.persist = f } }

// New builds a Rotator for an already-enrolled device. identity is the
// persisted mTLS identity (rotated in place); hostname is the SAN to request
// for each new leaf (matches what enroll sent).
func New(client Refresher, identity Identity, deviceID, hostname string, cfg Config, opts ...Option) *Rotator {
	cfg.withDefaults()
	r := &Rotator{client: client, identity: identity, deviceID: deviceID, hostname: hostname, cfg: cfg}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run blocks until ctx is done, refreshing the leaf before each cert expires.
// It is safe to run concurrently with the uplink: it only talks to the server
// over the same mTLS channel (the current leaf is still valid for the whole
// rotation window) and swaps material atomically.
func (r *Rotator) Run(ctx context.Context) error {
	backoff := r.cfg.MinInterval
	start := time.Now()
	first := true
	for {
		cert, err := parseCert(r.identity.CurrentLeafPEM())
		if err != nil {
			// No parseable leaf (identity not ready / not mTLS): wait and
			// retry rather than spinning.
			if err := sleepCtx(ctx, r.cfg.MinInterval); err != nil {
				return err
			}
			continue
		}
		// Rotation deadline: refresh when the remaining life drops to the
		// threshold (min of RotateFrac of the lifetime and MaxRotateLead —
		// see Config), well inside the cert's validity so a failed refresh
		// still has room to retry before expiry. A legacy long-lived cert
		// (24h W3-1 leaf) hits its 30m lead quickly and converges onto the
		// short-lived schedule after the first rotation.
		lifetime := cert.NotAfter.Sub(cert.NotBefore)
		thr := r.cfg.rotateThreshold(lifetime)
		remaining := time.Until(cert.NotAfter)
		wait := remaining - thr
		if wait < 0 {
			wait = 0 // already inside the rotation window — refresh now
		}
		// W3-2 e2e knob: force the FIRST rotation at a fixed offset after
		// process start (RotateAfter), so a ~1h leaf can be observed
		// rotating live in seconds. Only the first rotation; afterwards the
		// threshold above governs (the fresh leaf is far from expiry).
		if first && r.cfg.RotateAfter > 0 {
			forced := r.cfg.RotateAfter - time.Since(start)
			if forced < wait {
				wait = forced
			}
			first = false
		}
		r.cfg.Logger.Debug("rotator: leaf watch",
			"device", r.deviceID,
			"not_before", cert.NotBefore.UTC().Format(time.RFC3339),
			"not_after", cert.NotAfter.UTC().Format(time.RFC3339),
			"remaining", remaining.Round(time.Second),
			"rotate_at", cert.NotAfter.Add(-thr).UTC().Format(time.RFC3339),
			"wait", wait.Round(time.Second))
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
		if err := r.refresh(ctx); err != nil {
			r.cfg.Logger.Warn("leaf refresh failed; will retry",
				"device", r.deviceID, "err", err, "backoff", backoff)
			if err := sleepCtx(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			if backoff > r.cfg.MaxBackoff {
				backoff = r.cfg.MaxBackoff
			}
			continue
		}
		backoff = r.cfg.MinInterval
	}
}

// refresh performs one RefreshLeaf round-trip and swaps the new leaf in.
func (r *Rotator) refresh(ctx context.Context) error {
	cur := r.identity.CurrentLeafPEM()
	cert, err := parseCert(cur)
	if err != nil {
		return fmt.Errorf("parse current leaf: %w", err)
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := r.client.RefreshLeaf(rpcCtx, &agentv1.RefreshLeafRequest{
		Hostname:              r.hostname,
		CurrentLeafExpiresMs:  cert.NotAfter.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("refresh RPC: %w", err)
	}
	if resp.GetLeafCertPem() == "" || resp.GetLeafKeyPem() == "" {
		return fmt.Errorf("refresh returned an incomplete leaf (cert=%q key=%q)",
			yesNo(resp.GetLeafCertPem()), yesNo(resp.GetLeafKeyPem()))
	}
	// Sanity: the new leaf must parse (guards against a server clock far in
	// the past handing us a not-yet-valid cert).
	if _, err := parseCert([]byte(resp.GetLeafCertPem())); err != nil {
		return fmt.Errorf("new leaf unparsable: %w", err)
	}
	// Swap atomically (in memory) then persist. If persist fails the
	// in-memory leaf still works for this process lifetime; the next boot
	// re-rotates (and the log below makes the gap visible).
	r.identity.SwapLeaf([]byte(resp.GetLeafCertPem()), []byte(resp.GetLeafKeyPem()))
	if r.persist != nil {
		if perr := r.persist(); perr != nil {
			r.cfg.Logger.Warn("leaf rotated in memory but persist failed (next boot re-rotates)",
				"device", r.deviceID, "err", perr)
		}
	}
	r.cfg.Logger.Info("leaf rotated",
		"device", r.deviceID,
		"old_not_after", cert.NotAfter.UTC().Format(time.RFC3339),
		"new_not_after", time.UnixMilli(resp.GetExpiresMs()).UTC().Format(time.RFC3339))
	return nil
}

// parseCert decodes a single PEM block and parses it.
func parseCert(leafCertPEM []byte) (*x509.Certificate, error) {
	cb, _ := pem.Decode(leafCertPEM)
	if cb == nil {
		return nil, fmt.Errorf("no PEM block in leaf cert")
	}
	return x509.ParseCertificate(cb.Bytes)
}

// sleepCtx waits d or until ctx is done (returns ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func yesNo(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "<present>"
}
