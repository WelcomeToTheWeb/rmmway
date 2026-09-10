package enroll

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Enroller is the server-side Enroll RPC (the generated client satisfies it).
type Enroller interface {
	Enroll(ctx context.Context, in *agentv1.EnrollRequest, opts ...grpc.CallOption) (*agentv1.EnrollResponse, error)
}

// Agent is the self-enrollment orchestrator.
type Agent struct {
	store     *Store
	enroller  Enroller
	facts     Facts
	bootstrap string
	logf      func(string, ...any)
	// httpEnroll, when set, is tried FIRST: the bootstrap enroll over the
	// operator's HTTPS origin (POST {server}/agent/enroll). This is the path
	// remote agents use — only the operator origin + the mTLS gRPC port need
	// to be open, not the internal-only plain gRPC bootstrap port. On a
	// transient failure (connection error / 5xx) the agent falls back to the
	// plain gRPC enroller (local dev / split deployments).
	httpEnroll *HTTPEnroller
}

// Option customizes an Agent.
type Option func(*Agent)

// WithLogf sets a logger (default: no-op).
func WithLogf(f func(string, ...any)) Option { return func(a *Agent) { a.logf = f } }

// WithHTTPEenroller enables the operator-origin bootstrap enroll (tried
// first, with a plain-gRPC fallback on transient failure). See Agent.httpEnroll.
func WithHTTPEenroller(h *HTTPEnroller) Option { return func(a *Agent) { a.httpEnroll = h } }

// New builds an Agent. client is the connected AgentService client; store
// persists the identity; facts are the host facts; bootstrap is the one-time
// token (only needed on first enroll).
func New(client Enroller, store *Store, facts Facts, bootstrap string, opts ...Option) *Agent {
	a := &Agent{
		store:     store,
		enroller:  client,
		facts:     facts,
		bootstrap: bootstrap,
	}
	for _, o := range opts {
		o(a)
	}
	if a.logf == nil {
		a.logf = func(string, ...any) {}
	}
	return a
}

// Result reports what EnsureEnrolled did, for logging / the status command.
type Result struct {
	Identity *Identity
	Enrolled bool // true only if a fresh enroll happened this call (first boot)
}

// EnsureEnrolled returns this agent's Identity, enrolling on first boot and
// reusing the persisted identity thereafter. It never re-enrolls when a valid
// identity is already on disk — that is the W1-4 DoD ("restart doesn't
// re-enroll").
func (a *Agent) EnsureEnrolled(ctx context.Context) (*Result, error) {
	// 1. Already enrolled? Reuse — no server round-trip, no re-enroll.
	if id, err := a.store.Load(); err != nil {
		return nil, err
	} else if id != nil {
		a.logf("reusing persisted identity device=%s (no re-enroll)", id.DeviceID)
		return &Result{Identity: id}, nil
	}

	// 2. First boot — a bootstrap token is required to get an identity.
	if a.bootstrap == "" {
		return nil, fmt.Errorf("no persisted identity and no bootstrap token; nothing to enroll with (re-run the bootstrap installer or restore the identity file %s)", a.store.Path())
	}

	a.logf("first boot — enrolling with bootstrap token ...")
	req := &agentv1.EnrollRequest{
		BootstrapToken: a.bootstrap,
		Hostname:       a.facts.Hostname,
		Os:             a.facts.OS,
		Arch:           a.facts.Arch,
		AgentVersion:   a.facts.AgentVersion,
		Interfaces:     a.facts.Interfaces,
	}

	// Prefer the operator-origin HTTP enroll (a remote agent needs only the
	// operator origin + the mTLS gRPC port open). Fall back to the plain
	// gRPC channel ONLY on a transient failure (connection error / 5xx) — a
	// definitive 4xx means the server answered (bad/unknown token) and the
	// gRPC channel would just repeat the refusal after the token is gone.
	var (
		resp *agentv1.EnrollResponse
		err  error
		via  = "plain gRPC"
	)
	if a.httpEnroll != nil {
		resp, err = a.httpEnroll.Enroll(ctx, req)
		if err != nil {
			var he *httpEnrollError
			if !errors.As(err, &he) || !he.transient {
				return nil, fmt.Errorf("enroll: %w", err)
			}
			a.logf("http enroll failed (%v) — falling back to plain gRPC", err)
			resp, via = nil, "plain gRPC"
		} else {
			via = "the operator origin (HTTP)"
		}
	}
	if resp == nil {
		resp, err = a.enroller.Enroll(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("enroll: %w", err)
		}
	}
	if resp.GetDeviceId() == "" || resp.GetJwt() == "" {
		return nil, fmt.Errorf("enroll returned an incomplete identity (device=%q jwt=%q)",
			resp.GetDeviceId(), yesNo(resp.GetJwt()))
	}

	id := &Identity{
		DeviceID:   resp.GetDeviceId(),
		JWT:        resp.GetJwt(),
		Hostname:   a.facts.Hostname,
		EnrolledAt: time.Now().UnixMilli(),
	}
	// W3-1: the server minted this device's mTLS identity (leaf cert + key,
	// signed by the org root) in the same enroll response. Persist it so the
	// agent switches to the mTLS channel from the next connection onward.
	if resp.GetLeafCertPem() != "" && resp.GetLeafKeyPem() != "" && resp.GetOrgRootCaPem() != "" {
		id.TLS = &TLSIdentity{
			LeafCertPEM: resp.GetLeafCertPem(),
			LeafKeyPEM:  resp.GetLeafKeyPem(),
			OrgRootPEM:  resp.GetOrgRootCaPem(),
		}
		a.logf("mTLS identity issued by the org root (cert + key + root CA)")
	}
	if err := a.store.Save(id); err != nil {
		// We got an identity but couldn't persist it. Return it so the agent
		// can still run this session, but make the failure loud — the next
		// boot would otherwise try to enroll again with a now-consumed token.
		a.logf("WARNING: enrolled (device=%s) but failed to persist identity: %v", id.DeviceID, err)
		return &Result{Identity: id, Enrolled: true}, fmt.Errorf("enroll ok but persist failed: %w", err)
	}
	a.logf("enrolled device=%s via %s; identity persisted to %s", id.DeviceID, via, a.store.Path())
	return &Result{Identity: id, Enrolled: true}, nil
}

// BearerMetadata returns the (device_id, token) pair the Stream/heartbeat loop
// uses to authenticate every subsequent RPC.
func (r *Result) BearerMetadata() (string, string) {
	if r == nil || r.Identity == nil {
		return "", ""
	}
	return r.Identity.DeviceID, r.Identity.JWT
}

func yesNo(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "<present>"
}
