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
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/welcometotheweb/rmmway/agent/internal/caps"
	"github.com/welcometotheweb/rmmway/agent/internal/exec"
	"github.com/welcometotheweb/rmmway/agent/internal/files"
	"github.com/welcometotheweb/rmmway/agent/internal/inventory"
	"github.com/welcometotheweb/rmmway/agent/internal/patchmanager"
	"github.com/welcometotheweb/rmmway/agent/internal/session"
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
	jwtMu   sync.RWMutex // H3: guards jwt (it rotates in place via ack renewals)
	jwt     string
	jwtHook func(string) // H3: fired once per renewal (persist + share)
	cfg     Config
	collect func(ctx context.Context) (*agentv1.MetricBatch, error) // W1-2 collector
	commander *Commander                                            // W3-3 (nil = legacy log-only)
	sessions  *session.Driver                                       // gap #1a (nil = no remote session)
	rng       *rand.Rand

	// pushes (gap #1a): in-flight chunked file_push transfers,
	// command_id -> session. The downlink reader routes FileChunk frames
	// to them; the command's finish goroutine waits for the eof chunk.
	pushMu   sync.Mutex
	pushes   map[string]*files.PushSession

	// acked (M3) is set once the current session has received a heartbeat
	// ack — i.e. it was actually healthy. Run() uses it to reset the
	// reconnect backoff instead of compounding it after a healthy session
	// that merely got disconnected.
	acked atomic.Bool

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

// WithJWTChangeHook (H3) is fired once per JWT renewal — when the server
// hands back a fresh agent token in a HeartbeatAck. The caller typically
// persists the new token (so the next boot reconnects with it) and shares
// it with any other consumer of the current JWT (e.g. the mTLS unary
// interceptor for RefreshLeaf).
func WithJWTChangeHook(fn func(string)) Option {
	return func(u *Uplink) { u.jwtHook = fn }
}

// SetSessionDriver (gap #1a) wires the remote-session capture loop after
// the uplink is created. Must be called before streamSession starts
// (i.e. immediately after New()).
func (u *Uplink) SetSessionDriver(d *session.Driver) {
	u.sessions = d
}

// WithSessionDriver (gap #1a) wires the remote-session capture loop:
// SessionControl downlink frames drive it and its frames ship as
// SessionFrame uplink frames via PushSessionFrame. Nil = the agent ignores
// session controls (pre-#1a servers stay happy).
func WithSessionDriver(d *session.Driver) Option {
	return func(u *Uplink) {
		if d != nil {
			u.sessions = d
		}
	}
}

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
		pushes:  map[string]*files.PushSession{},
	}
	for _, o := range opts {
		o(u)
	}
	return u
}

// DeviceID returns the uplink's device id.
func (u *Uplink) DeviceID() string { return u.devID }

// JWT returns the current agent token (H3: it rotates in place when the
// server renews it via a HeartbeatAck).
func (u *Uplink) JWT() string {
	u.jwtMu.RLock()
	defer u.jwtMu.RUnlock()
	return u.jwt
}

// applyRenewedJWT (H3) adopts a fresh agent token from a HeartbeatAck. It
// fires the change hook (persist + share) only when the token actually
// changed. The current session keeps using the token it opened with — the
// new one takes effect from the next reconnect.
func (u *Uplink) applyRenewedJWT(tok string) {
	if tok == "" {
		return
	}
	u.jwtMu.Lock()
	changed := tok != u.jwt
	u.jwt = tok
	u.jwtMu.Unlock()
	if changed && u.jwtHook != nil {
		u.jwtHook(tok)
	}
}

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

// PushSessionFrame (gap #1a) ships one session frame on the live stream.
// Same contract as PushLogs: an error means no live session (the caller's
// capture loop stops; the uplink reconnect + the server's re-open control
// re-establish the feed).
func (u *Uplink) PushSessionFrame(ctx context.Context, f *agentv1.SessionFrame) error {
	_ = ctx
	u.curMu.Lock()
	st := u.cur
	u.curMu.Unlock()
	if st == nil {
		return errors.New("no live uplink stream")
	}
	return st.Send(&agentv1.StreamRequest{Payload: &agentv1.StreamRequest_SessionFrame{SessionFrame: f}})
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
		// M3: decide the reconnect wait from whether the session that JUST
		// ended was healthy. A session that received at least one heartbeat
		// ack was HEALTHY — its drop is a routine disconnect (network blip,
		// server deploy), so start the backoff over instead of compounding
		// it toward the 30s cap. A session that died before a single ack
		// (Unauthenticated, connection refused) doubles.
		if u.acked.Load() {
			backoff = u.cfg.MinBackoff
		}
		// sleep with jitter, but stay responsive to ctx cancellation
		jitter := time.Duration(u.rng.Int63n(int64(backoff/2) + 1))
		wait := backoff + jitter
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if !u.acked.Load() {
			backoff *= 2
			if backoff > u.cfg.MaxBackoff {
				backoff = u.cfg.MaxBackoff
			}
		}
	}
}

// streamSession opens one stream and runs the heartbeat loop until it drops.
func (u *Uplink) streamSession(ctx context.Context) error {
	// M3: this session is not healthy until it has received an ack.
	u.acked.Store(false)
	// H3: authenticate with the CURRENT token (a prior session's renewal,
	// if any, is already in place — the point of the rotation).
	md := metadata.Pairs("authorization", "Bearer "+u.JWT())
	ctx = metadata.NewOutgoingContext(ctx, md)
	stream, err := u.client.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}
	u.setCur(stream)
	defer func() {
		u.setCur(nil)
		u.abandonAllPushes() // gap #1a: a stream drop kills in-flight pushes
	}()

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
			case *agentv1.StreamResponse_SessionControl:
				// gap #1a: open/close the capture loop (driver is nil-safe).
				if u.sessions != nil {
					u.sessions.Control(ctx, p.SessionControl)
				}
			case *agentv1.StreamResponse_FileChunk:
				// gap #1a: a block of an in-flight chunked file_push.
				u.routeChunk(p.FileChunk)
			case *agentv1.StreamResponse_HeartbeatAck:
				// M3: the server accepted a beat — this session is healthy.
				u.acked.Store(true)
				// H3: adopt a renewed agent JWT when the server hands one
				// back (it renews once the stream's token is into its last
				// quarter of life, so a live device never hits the 720h
				// expiry wall). Takes effect from the next reconnect.
				if j := p.HeartbeatAck.GetJwt(); j != "" {
					u.applyRenewedJWT(j)
				}
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
		switch {
		case batch != nil && len(batch.GetSamples()) > 0:
			if err != nil {
				// M2: a PARTIAL collection failure already returned the
				// families that worked — send them. An RMM that goes blind
				// on every family because one probe failed is worse than
				// one that is blind on just one.
				u.cfg.Logger.Warn("partial metric collection; sending what was collected", "err", err)
			}
			hb.Metrics = batch
		case err != nil:
			// Nothing collected — still send the bare heartbeat so the
			// device stays "online" (presence outranks data).
			u.cfg.Logger.Warn("metric collection failed; heartbeat without metrics", "err", err)
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
	case *agentv1.Command_FilePull: // gap #1a
		return u.filePullCommand(ctx, stream, cmd)
	case *agentv1.Command_FilePush: // gap #1a
		return u.filePushCommand(ctx, c, stream, cmd)
	case *agentv1.Command_CollectInventory: // gap #4
		return u.collectInventoryCommand(ctx, stream, cmd)
	case *agentv1.Command_PatchQuery: // gap #4
		return u.patchQueryCommand(ctx, stream, cmd)
	case *agentv1.Command_PatchApply: // gap #4
		return u.patchApplyCommand(ctx, stream, cmd)
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

// ---- gap #1a: file transfer commands ----

// abandonAllPushes is called when the stream drops; in-flight chunked pushes
// will never complete.
func (u *Uplink) abandonAllPushes() {
	u.pushMu.Lock()
	defer u.pushMu.Unlock()
	for id, p := range u.pushes {
		p.Abandon()
		delete(u.pushes, id)
	}
}

// routeChunk delivers one FileChunk downlink frame to the matching push session.
func (u *Uplink) routeChunk(chunk *agentv1.FileChunk) {
	u.pushMu.Lock()
	p, ok := u.pushes[chunk.GetCommandId()]
	u.pushMu.Unlock()
	if ok {
		_ = p.Chunk(chunk)
	}
}

// filePullCommand reads a file from the agent and streams it to the server.
func (u *Uplink) filePullCommand(ctx context.Context, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	pull := cmd.GetFilePull()
	if pull == nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, "nil file_pull")
	}
	u.cfg.Logger.Info("file_pull", "cmd", cmd.GetId(), "path", pull.GetPath())

	sendChunk := func(chunk *agentv1.FileChunk) error {
		return stream.Send(&agentv1.StreamRequest{
			Payload: &agentv1.StreamRequest_FileChunk{FileChunk: chunk},
		})
	}

	size, mode, err := files.SendPull(ctx, cmd.GetId(), pull.GetPath(), sendChunk)
	if err != nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, err.Error())
	}
	u.cfg.Logger.Info("file_pull complete", "cmd", cmd.GetId(), "size", size, "mode", mode)
	return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, []byte(fmt.Sprintf("%d bytes", size)), nil, "")
}

// filePushCommand writes a file on the agent (inline or chunked).
func (u *Uplink) filePushCommand(ctx context.Context, c *Commander, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	push := cmd.GetFilePush()
	if push == nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, "nil file_push")
	}
	u.cfg.Logger.Info("file_push", "cmd", cmd.GetId(), "path", push.GetPath(), "inline", push.GetContentB64() != "")

	// Inline push (small files).
	if push.GetContentB64() != "" {
		if err := files.InlinePush(cmd.GetId(), push.GetPath(), push.GetMode(), push.GetContentB64()); err != nil {
			return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, err.Error())
		}
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, nil, nil, "")
	}

	// Chunked push: register the session, wait for the eof chunk.
	session, err := files.StartPush(cmd.GetId(), push.GetPath(), push.GetMode())
	if err != nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, err.Error())
	}
	u.pushMu.Lock()
	u.pushes[cmd.GetId()] = session
	u.pushMu.Unlock()
	defer func() {
		u.pushMu.Lock()
		delete(u.pushes, cmd.GetId())
		u.pushMu.Unlock()
	}()

	size, err := session.Wait(ctx)
	if err != nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, err.Error())
	}
	u.cfg.Logger.Info("file_push complete", "cmd", cmd.GetId(), "size", size)
	return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, []byte(fmt.Sprintf("%d bytes", size)), nil, "")
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// actionName renders the command's action oneof for logging.
func actionName(a any) string {
	switch a := a.(type) {
	case *agentv1.Command_RunScript:
		return "run_script(" + a.RunScript.GetLang() + ")"
	case *agentv1.Command_Reboot:
		return "reboot"
	case *agentv1.Command_CollectInventory:
		return "collect_inventory"
	case *agentv1.Command_PatchQuery:
		return "patch_query"
	case *agentv1.Command_PatchApply:
		return "patch_apply"
	case nil:
		return "none"
	default:
		return fmt.Sprintf("%T", a)
	}
}

// collectInventoryCommand (gap #4) collects deep inventory for this device.
func (u *Uplink) collectInventoryCommand(ctx context.Context, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	u.cfg.Logger.Info("collect_inventory", "cmd", cmd.GetId())

	// Collect all inventory categories
	inv := &agentv1.InventoryReport{
		CommandId:     cmd.GetId(),
		CollectedAtMs: time.Now().UnixMilli(),
	}

	// Hardware
	if hw, err := inventory.CollectHardware(ctx); err == nil {
		inv.Hardware = hw.ToProto()
	} else {
		u.cfg.Logger.Warn("inventory: hardware collection failed", "err", err)
	}

	// Software
	if sw, err := inventory.CollectSoftware(ctx); err == nil {
		for _, s := range sw {
			inv.Software = append(inv.Software, s.ToProto())
		}
	} else {
		u.cfg.Logger.Warn("inventory: software collection failed", "err", err)
	}

	// Services
	if svcs, err := inventory.CollectServices(ctx); err == nil {
		for _, s := range svcs {
			inv.Services = append(inv.Services, &agentv1.ServiceInfo{
				Name:       s.Name,
				Status:     s.Status,
				Type:       s.Type,
				Enabled:    s.Enabled,
			})
		}
	} else {
		u.cfg.Logger.Warn("inventory: services collection failed", "err", err)
	}

	// Users
	if users, err := inventory.CollectUsers(ctx); err == nil {
		for _, u := range users {
			inv.Users = append(inv.Users, &agentv1.UserAccount{
				Username:   u.Username,
				Uid:        u.Uid,
				HomeDir:    u.HomeDir,
				Shell:      u.Shell,
				Enabled:    u.Enabled,
				AccountType: u.AccountType,
			})
		}
	} else {
		u.cfg.Logger.Warn("inventory: users collection failed", "err", err)
	}

	// Send inventory report
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_InventoryReport{InventoryReport: inv},
	}); err != nil {
		return err
	}

	u.cfg.Logger.Info("inventory: report sent", "cmd", cmd.GetId())
	return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, []byte("inventory collected"), nil, "")
}

// patchQueryCommand (gap #4) queries available patches.
func (u *Uplink) patchQueryCommand(ctx context.Context, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	u.cfg.Logger.Info("patch_query", "cmd", cmd.GetId())

	pm := patchmanager.NewPatchManager()
	result, err := pm.Query(ctx, "")
	if err != nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, err.Error())
	}

	// Send patch status
	patchStatus := &agentv1.PatchStatus{
		CommandId:  cmd.GetId(),
		StatusAtMs: time.Now().UnixMilli(),
	}

	patchStatus.Query = &agentv1.PatchQueryResult{}
	for _, p := range result.Available {
		patchStatus.Query.Available = append(patchStatus.Query.Available, &agentv1.AvailablePatch{
			Id:             p.ID,
			Title:          p.Title,
			Severity:       p.Severity,
			RebootRequired: p.RebootRequired,
		})
	}

	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_PatchStatus{PatchStatus: patchStatus},
	}); err != nil {
		return err
	}

	u.cfg.Logger.Info("patch_query: results sent", "cmd", cmd.GetId(), "available", len(result.Available))
	return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, []byte(fmt.Sprintf("%d patches available", len(result.Available))), nil, "")
}

// patchApplyCommand (gap #4) applies patches.
func (u *Uplink) patchApplyCommand(ctx context.Context, stream agentv1.AgentService_StreamClient, cmd *agentv1.Command) error {
	apply := cmd.GetPatchApply()
	if apply == nil {
		return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_FAILED, 0, nil, nil, "nil patch_apply")
	}
	u.cfg.Logger.Info("patch_apply", "cmd", cmd.GetId(), "schedule_reboot", apply.GetScheduleReboot())

	pm := patchmanager.NewPatchManager()
	progressCh := make(chan agentv1.PatchApplyProgress, 10)

	go func() {
		defer close(progressCh)
		err := pm.Apply(ctx, apply.GetPatchIds(), apply.GetScheduleReboot(), apply.GetRebootDelaySeconds(), func(p patchmanager.PatchApplyProgress) {
			select {
			case progressCh <- agentv1.PatchApplyProgress{
				Phase:           p.Phase,
				Message:         p.Message,
				ProgressPercent: p.ProgressPercent,
				RebootRequired:  p.RebootRequired,
				Errors:          p.Errors,
				}:
			default:
			}
		})
		if err != nil {
			progressCh <- agentv1.PatchApplyProgress{
			Phase:  "failed",
			Errors: []string{err.Error()},
		}
		}
	}()

	// Report progress
	for progress := range progressCh {
		status := &agentv1.PatchStatus{
			CommandId:  cmd.GetId(),
			StatusAtMs: time.Now().UnixMilli(),
		}
		status.Apply = &agentv1.PatchApplyProgress{
			Phase:           progress.Phase,
			Message:         progress.Message,
			ProgressPercent: progress.ProgressPercent,
			RebootRequired:  progress.RebootRequired,
			Errors:          progress.Errors,
		}
		if err := stream.Send(&agentv1.StreamRequest{
			Payload: &agentv1.StreamRequest_PatchStatus{PatchStatus: status},
		}); err != nil {
			return err
		}
	}

	u.cfg.Logger.Info("patch_apply: completed", "cmd", cmd.GetId())
	return u.sendResult(stream, cmd.GetId(), agentv1.CommandResult_SUCCEEDED, 0, []byte("patches applied"), nil, "")
}
