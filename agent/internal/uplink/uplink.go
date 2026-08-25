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
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/welcometotheweb/rmmway/agent/internal/caps"
	"github.com/welcometotheweb/rmmway/agent/internal/exec"
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
	client    Streamer
	devID     string
	jwt       string
	cfg       Config
	collect   func(ctx context.Context) (*agentv1.MetricBatch, error) // W1-2 collector
	commander *Commander                                              // W3-3 (nil = legacy log-only)
	rng       *rand.Rand

	// cur is the live stream of the current session (nil between
	// sessions). W6-1: PushLogs (the log shipper) sends LogBatch frames
	// on it from another goroutine; gRPC client-stream Send is safe for
	// concurrent use by the heartbeat loop and the shipper.
	curMu sync.Mutex
	cur   agentv1.AgentService_StreamClient
}

// Commander (W3-3) turns a dispatched command into verified action: it
// checks the command's capability token (Verifier, against the pinned org
// root), and only then runs it (Exec), reporting CommandResults on the
// stream. A nil Verifier = legacy mode (log-only, no capability
// enforcement — pre-W3-3 plain-listener deployments keep working).
type Commander struct {
	DevID    string
	Verifier *caps.Verifier
	Exec     *exec.Executor
	Logger   *slog.Logger
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

// WithCommander enables W3-3 command handling (capability-gated execution
// + CommandResult reporting). Without it the agent only logs commands.
func WithCommander(c *Commander) Option {
	return func(u *Uplink) {
		if c != nil && c.Logger == nil {
			c.Logger = slog.Default()
		}
		u.commander = c
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

// setCur publishes the live stream of the current session (nil on exit).
func (u *Uplink) setCur(st agentv1.AgentService_StreamClient) {
	u.curMu.Lock()
	u.cur = st
	u.curMu.Unlock()
}

// PushLogs (W6-1) ships one batch of structured log events on the live
// stream (a LogBatch uplink frame; the server indexes them in the
// log_events hypertable). It returns an error when no session is live or
// the send fails — the caller (the log shipper) keeps the batch queued
// and retries on its next tick.
func (u *Uplink) PushLogs(ctx context.Context, batch *agentv1.LogBatch) error {
	if batch == nil || len(batch.GetEntries()) == 0 {
		return nil
	}
	u.curMu.Lock()
	st := u.cur
	u.curMu.Unlock()
	if st == nil {
		return errors.New("no live uplink stream")
	}
	return st.Send(&agentv1.StreamRequest{Payload: &agentv1.StreamRequest_Logs{Logs: batch}})
}

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
	u.setCur(stream)
	defer u.setCur(nil)

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
				// W1-4 proves the downlink is alive and authenticated.
				// W3-3 adds the capability gate: verify the command's
				// token against the pinned org root, act only inside the
				// minted scope, and report CommandResults (RECEIVED, then
				// the final status). Commands run SEQUENTIALLY in this
				// goroutine (a reboot kills the process anyway); a send
				// failure ends the session so the reconnect loop re-establishes.
				if err := u.handleCommand(ctx, stream, p.Command); err != nil {
					u.cfg.Logger.Warn("command handling ended the downlink", "err", err)
					return
				}
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

// handleCommand (W3-3) is the agent's capability gate + executor for one
// dispatched command. It runs in the downlink goroutine (commands execute
// sequentially) and reports via CommandResult frames:
//
//	unknown action            -> UNSUPPORTED (nothing to check)
//	bad/missing/expired token -> REFUSED (NOT executed)
//	valid                     -> RECEIVED, execute, final SUCCEEDED/FAILED/TIMED_OUT
//
// A nil Commander (legacy deployment, no pinned org root) keeps the
// pre-W3-3 behavior: log receipt only.
func (u *Uplink) handleCommand(ctx context.Context, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	if u.commander == nil {
		u.cfg.Logger.Info("command received (no capability enforcement — legacy channel)",
			"id", cmd.GetId(), "action", actionName(cmd.GetAction()))
		return nil
	}
	c := u.commander
	capName, token, ok := caps.ForCommand(cmd)
	if !ok {
		_ = u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_UNSUPPORTED, 0, nil, nil, "unsupported action type")
		return nil
	}
	// THE GATE: the token must verify against the pinned org root for this
	// device AND this capability. A valid mTLS channel alone is not enough.
	if err := c.Verifier.Check(token, capName, cmd.GetId()); err != nil {
		c.Logger.Warn("command refused", "id", cmd.GetId(), "capability", capName, "err", err)
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_REFUSED, 0, nil, nil, err.Error())
	}
	if err := u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_RECEIVED, 0, nil, nil, ""); err != nil {
		return err
	}
	switch cmd.GetAction().(type) {
	case *agentv1.Command_RunScript:
		return u.runScriptCommand(ctx, c, stream, cmd)
	case *agentv1.Command_Reboot:
		return u.rebootCommand(ctx, c, stream, cmd)
	}
	return nil
}

func (u *Uplink) runScriptCommand(ctx context.Context, c *Commander, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	rs := cmd.GetRunScript()
	script, err := base64.StdEncoding.DecodeString(rs.GetScriptB64())
	if err != nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, "script_b64: "+err.Error())
	}
	timeout := c.Exec.TimeoutFor(cmd.GetTimeoutS())
	c.Logger.Info("executing command", "id", cmd.GetId(), "lang", rs.GetLang(), "timeout", timeout.String())
	exitCode, out, errTail, err := c.Exec.RunScript(ctx, rs.GetLang(), script, rs.GetArgs(), timeout)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_TIMED_OUT, int32(exitCode), out, errTail, "timeout after "+timeout.String())
	case err != nil:
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, int32(exitCode), out, errTail, err.Error())
	case exitCode != 0:
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, int32(exitCode), out, errTail, "exit code "+itoa(exitCode))
	default:
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, int32(exitCode), out, errTail, "")
	}
}

func (u *Uplink) rebootCommand(ctx context.Context, c *Commander, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	// The host is about to go away: report first, wait the delay (0 = a
	// short flush window so the result actually reaches the server), then
	// reboot. The process usually does not return from Reboot.
	if err := u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, nil, nil, "reboot scheduled"); err != nil {
		return err
	}
	delay := time.Duration(cmd.GetReboot().GetDelayS()) * time.Second
	if delay <= 0 {
		delay = 2 * time.Second
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.Exec.Reboot(ctx); err != nil {
		c.Logger.Error("reboot failed", "id", cmd.GetId(), "err", err)
	}
	return nil // the reboot is in motion; keep the session if the exec survived
}

// sendResult reports one CommandResult uplink frame (W3-3).
func (u *Uplink) sendResult(stream agentv1.AgentService_StreamClient, cmdID string, st agentv1.CommandResult_Status, exitCode int32, stdout, stderr []byte, errMsg string) error {
	res := &agentv1.CommandResult{
		CommandId:     cmdID,
		Status:        st,
		ExitCode:      exitCode,
		StdoutTail:    tail(stdout),
		StderrTail:    tail(stderr),
		Error:         errMsg,
		CompletedAtMs: time.Now().UnixMilli(),
	}
	return stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_CommandResult{CommandResult: res},
	})
}

// tail returns the last 4KB of b (the CommandResult stdout/stderr contract).
func tail(b []byte) string {
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(b)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

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
