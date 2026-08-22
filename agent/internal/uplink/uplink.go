// Package uplink is the agent's long-lived gRPC Stream (W1-4 "report back"):
// an authenticated heartbeat/metric loop over the Server's Stream RPC.
//
// It authenticates every stream with the enroll JWT (W1-4) via the
// `Authorization: Bearer *** metadata header, sends a Heartbeat (piggybacking
// a collected MetricBatch) at the server-assigned cadence, and reconnects with
// exponential backoff on drop. Server-side dedup by (device_id, metric, ts)
// makes the at-least-once replay safe.
package uplink

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Streamer is the generated client's Stream method (satisfied by
// agentv1.AgentServiceClient).
type Streamer interface {
	Stream(ctx context.Context, opts ...grpc.CallOption) (agentv1.AgentService_StreamClient, error)
}

// Config tunes the uplink loop.
type Config struct {
	HeartbeatInterval time.Duration // 0 -> default 30s
	MetricInterval    time.Duration // 0 -> same as heartbeat
	MinBackoff        time.Duration // 0 -> default 1s
	MaxBackoff        time.Duration // 0 -> default 30s
	Logger            *slog.Logger  // 0 -> default
}

func (c *Config) withDefaults() {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.MetricInterval <= 0 {
		c.MetricInterval = c.HeartbeatInterval
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Uplink owns one authenticated Stream for a device.
type Uplink struct {
	client  Streamer
	devID   string
	jwt     string
	cfg     Config
	collect func(ctx context.Context) (*agentv1.MetricBatch, error) // W1-2 collector
	rng     *rand.Rand
}

// Option customizes a Uplink.
type Option func(*Uplink)

// WithCollector injects the metric source (defaults to a no-op batch).
func WithCollector(fn func(ctx context.Context) (*agentv1.MetricBatch, error)) Option {
	return func(u *Uplink) {
		if fn != nil {
			u.collect = fn
		}
	}
}

// WithRand injects the jitter RNG (tests).
func WithRand(r *rand.Rand) Option { return func(u *Uplink) { u.rng = r } }

// New builds an Uplink for an already-enrolled device (devID + jwt come from
// the enroll.Store identity).
func New(client Streamer, devID, jwt string, cfg Config, opts ...Option) *Uplink {
	cfg.withDefaults()
	u := &Uplink{
		client:  client,
		devID:   devID,
		jwt:     jwt,
		cfg:     cfg,
		collect: func(context.Context) (*agentv1.MetricBatch, error) { return &agentv1.MetricBatch{}, nil },
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for _, o := range opts {
		o(u)
	}
	return u
}

// DeviceID returns the uplink's device id.
func (u *Uplink) DeviceID() string { return u.devID }

// Run drives the stream until ctx is canceled. It reconnects with exponential
// backoff (capped, jittered) on any stream error and never exits on a dropped
// connection — that is the whole point of an RMM agent uplink.
func (u *Uplink) Run(ctx context.Context) error {
	backoff := u.cfg.MinBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := u.streamSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			u.cfg.Logger.Warn("uplink stream ended; reconnecting",
				"device", u.devID, "err", err, "backoff", backoff)
		}
		// sleep with jitter, but stay responsive to ctx cancellation
		jitter := time.Duration(u.rng.Int63n(int64(backoff/2) + 1))
		wait := backoff + jitter
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		// double backoff, capped
		backoff *= 2
		if backoff > u.cfg.MaxBackoff {
			backoff = u.cfg.MaxBackoff
		}
	}
}

// streamSession opens one stream and runs the heartbeat loop until it drops.
func (u *Uplink) streamSession(ctx context.Context) error {
	md := metadata.Pairs("authorization", "Bearer "+u.jwt)
	ctx = metadata.NewOutgoingContext(ctx, md)
	stream, err := u.client.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Downlink reader: drain acks/commands so the client-side send buffer
	// doesn't wedge. W2 command handling hooks in here; for now we just log
	// a command so the wire is exercised.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			switch p := resp.GetPayload().(type) {
			case *agentv1.StreamResponse_Command:
				// W1-4 only proves the downlink is alive and authenticated.
				// Executing commands (RunScript/Reboot) + CommandResult
				// reporting is a later task; we log receipt here so the
				// full round-trip is exercised on the wire.
				u.cfg.Logger.Info("command received",
					"id", p.Command.GetId(), "action", actionName(p.Command.GetAction()))
			case *agentv1.StreamResponse_HeartbeatAck:
				// cadence steering is a future hook; keep current for now.
			}
		}
	}()

	tick := time.NewTicker(u.cfg.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			// server closed the stream (downlink Recv failed) — reconnect.
			return fmt.Errorf("server closed stream")
		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		default:
		}

		if err := u.sendHeartbeat(ctx, stream); err != nil {
			return err
		}
		select {
		case <-done:
			return fmt.Errorf("server closed stream")
		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// sendHeartbeat samples metrics (W1-2) and sends one Heartbeat frame carrying
// them.
func (u *Uplink) sendHeartbeat(ctx context.Context, stream agentv1.AgentService_StreamClient) error {
	now := time.Now().UnixMilli()
	hb := &agentv1.Heartbeat{
		TimestampMs: now,
		State:       "active",
	}
	if u.collect != nil {
		batch, err := u.collect(ctx)
		if err == nil && batch != nil {
			hb.Metrics = batch
		}
	}
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: hb},
	})
}

// actionName renders the command's action oneof for logging.
func actionName(a any) string {
	switch a := a.(type) {
	case *agentv1.Command_RunScript:
		return "run_script(" + a.RunScript.GetLang() + ")"
	case *agentv1.Command_Reboot:
		return "reboot"
	case nil:
		return "none"
	default:
		return fmt.Sprintf("%T", a)
	}
}
