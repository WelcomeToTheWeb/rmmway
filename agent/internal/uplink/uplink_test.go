package uplink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// fakeBidi implements agentv1.AgentService_StreamClient (a type alias for
// grpc.BidiStreamingClient[StreamRequest, StreamResponse]).
type fakeBidi struct {
	sentMu  sync.Mutex
	sent    []*agentv1.StreamRequest
	recvOut chan *agentv1.StreamResponse
	recvErr chan error
	ctx     context.Context
}

func (f *fakeBidi) Send(req *agentv1.StreamRequest) error {
	f.sentMu.Lock()
	f.sent = append(f.sent, req)
	f.sentMu.Unlock()
	return nil
}

func (f *fakeBidi) Recv() (*agentv1.StreamResponse, error) {
	select {
	case r := <-f.recvOut:
		if r != nil {
			return r, nil
		}
		return nil, io.EOF
	case err := <-f.recvErr:
		return nil, err
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

// low-level transport methods (embedded ClientStream) — unused by the loop.
func (f *fakeBidi) SendMsg(m any) error                            { return nil }
func (f *fakeBidi) RecvMsg(m any) error                            { return nil }
func (f *fakeBidi) CloseSend() error                               { return nil }
func (f *fakeBidi) CloseAndRecv() (*agentv1.StreamResponse, error) { return f.Recv() }
func (f *fakeBidi) Context() context.Context                       { return f.ctx }
func (f *fakeBidi) Header() (metadata.MD, error)                   { return nil, nil }
func (f *fakeBidi) Trailer() metadata.MD                           { return nil }
func (f *fakeBidi) Close() error                                   { return nil }

var _ agentv1.AgentService_StreamClient = (*fakeBidi)(nil)

// fakeStreamer returns a configured bidi stream, or an error.
type fakeStreamer struct {
	err    error
	stream agentv1.AgentService_StreamClient
}

func (f *fakeStreamer) Stream(ctx context.Context, opts ...grpc.CallOption) (agentv1.AgentService_StreamClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

var _ Streamer = (*fakeStreamer)(nil)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testUplink(t *testing.T, stream agentv1.AgentService_StreamClient) *Uplink {
	t.Helper()
	return New(
		&fakeStreamer{stream: stream},
		"dev-123",
		"jwt-abc",
		Config{HeartbeatInterval: 10 * time.Millisecond, Logger: quiet()},
		WithCollector(func(context.Context) (*agentv1.MetricBatch, error) {
			return &agentv1.MetricBatch{
				Samples: []*agentv1.Metric{{
					Name:        "cpu.utilization_percent",
					Value:       42,
					TimestampMs: time.Now().UnixMilli(),
				}},
			}, nil
		}),
		WithRand(rand.New(rand.NewSource(1))),
	)
}

// TestUplinkSendsAuthenticatedHeartbeats proves the W1-4 "report back" path:
// heartbeats carry collected metrics, at the configured cadence.
func TestUplinkSendsHeartbeatsWithMetrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	stream := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}
	u := testUplink(t, stream)

	go func() { _ = u.Run(ctx) }()

	deadline := time.Now().Add(120 * time.Millisecond)
	for {
		stream.sentMu.Lock()
		n := len(stream.sent)
		stream.sentMu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected >=2 heartbeats, got %d", n)
		}
		time.Sleep(2 * time.Millisecond)
	}

	stream.sentMu.Lock()
	defer stream.sentMu.Unlock()

	if len(stream.sent) < 2 {
		t.Fatalf("expected >=2 sends, got %d", len(stream.sent))
	}
	for i, req := range stream.sent {
		hb := req.GetHeartbeat()
		if hb == nil {
			t.Fatalf("send %d: not a heartbeat: %v", i, req.GetPayload())
		}
		if hb.GetState() != "active" {
			t.Errorf("send %d: state = %q, want active", i, hb.GetState())
		}
		if hb.GetMetrics() == nil || len(hb.GetMetrics().GetSamples()) != 1 {
			t.Errorf("send %d: heartbeat missing collected metrics", i)
		} else if hb.GetMetrics().GetSamples()[0].GetName() != "cpu.utilization_percent" {
			t.Errorf("send %d: unexpected metric %q", i, hb.GetMetrics().GetSamples()[0].GetName())
		}
	}
}

// TestUplinkReconnectsOnStreamError proves the agent keeps reporting back
// after the stream drops: a first stream that errors immediately is followed
// by a second, healthy stream that receives heartbeats.
func TestUplinkReconnectsOnStreamError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	second := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}
	streamer := &reconnectingStreamer{first: errors.New("stream dropped"), second: second}
	u := New(
		streamer,
		"dev-123",
		"jwt-abc",
		Config{
			HeartbeatInterval: 10 * time.Millisecond,
			MinBackoff:        time.Millisecond,
			MaxBackoff:        5 * time.Millisecond,
			Logger:            quiet(),
		},
		WithCollector(func(context.Context) (*agentv1.MetricBatch, error) { return &agentv1.MetricBatch{}, nil }),
		WithRand(rand.New(rand.NewSource(1))),
	)

	go func() { _ = u.Run(ctx) }()

	deadline := time.Now().Add(280 * time.Millisecond)
	for {
		second.sentMu.Lock()
		n := len(second.sent)
		second.sentMu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second stream never received a heartbeat (reconnect failed)")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// reconnectingStreamer: first Stream() call errors, later calls succeed.
type reconnectingStreamer struct {
	mu     sync.Mutex
	calls  int
	first  error
	second agentv1.AgentService_StreamClient
}

func (r *reconnectingStreamer) Stream(ctx context.Context, opts ...grpc.CallOption) (agentv1.AgentService_StreamClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == 0 && r.first != nil {
		r.calls++
		return nil, r.first
	}
	r.calls++
	return r.second, nil
}

// TestUplinkBearsJwt proves the JWT is attached to the outgoing stream's
// context metadata (this is what the server's requireJWT reads).
func TestUplinkBearsJwt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}
	captured := make(chan context.Context, 1)
	streamer := &captureStreamer{
		base:     &fakeStreamer{stream: stream},
		captured: captured,
	}
	u := New(streamer, "dev-123", "jwt-secret-token",
		Config{HeartbeatInterval: 10 * time.Millisecond, Logger: quiet()})

	go func() { _ = u.Run(ctx) }()
	var gotCtx context.Context
	select {
	case gotCtx = <-captured:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Stream() was never called")
	}
	md, _ := metadata.FromOutgoingContext(gotCtx)
	got := md.Get("authorization")
	if len(got) != 1 || got[0] != "Bearer jwt-secret-token" {
		t.Fatalf("authorization metadata = %v, want [Bearer jwt-secret-token]", got)
	}
}

type captureStreamer struct {
	base     Streamer
	captured chan context.Context
}

func (c *captureStreamer) Stream(ctx context.Context, opts ...grpc.CallOption) (agentv1.AgentService_StreamClient, error) {
	select {
	case c.captured <- ctx:
	default:
	}
	return c.base.Stream(ctx, opts...)
}

// TestUplinkAdoptsRenewedJWT (H3) proves the agent-side rotation: a
// HeartbeatAck carrying a fresh JWT updates the uplink's live token (fires
// the change hook once) and the NEXT reconnect authenticates with it.
func TestUplinkAdoptsRenewedJWT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	stream := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}
	// The server's renewal lands in the first ack of the session.
	stream.recvOut <- &agentv1.StreamResponse{
		Payload: &agentv1.StreamResponse_HeartbeatAck{
			HeartbeatAck: &agentv1.HeartbeatAck{ServerTimeMs: time.Now().UnixMilli(), Jwt: "jwt-renewed"},
		},
	}
	captured := make(chan context.Context, 4)
	streamer := &captureStreamer{base: &fakeStreamer{stream: stream}, captured: captured}

	hookCalls := make(chan string, 4)
	u := New(streamer, "dev-123", "jwt-old",
		Config{
			HeartbeatInterval: 10 * time.Millisecond,
			MinBackoff:        time.Millisecond,
			MaxBackoff:        5 * time.Millisecond,
			Logger:            quiet(),
		},
		WithJWTChangeHook(func(tok string) { hookCalls <- tok }),
	)
	go func() { _ = u.Run(ctx) }()

	// 1. The live token flips to the renewed one and the hook fires once.
	deadline := time.Now().Add(300 * time.Millisecond)
	for u.JWT() != "jwt-renewed" {
		if time.Now().After(deadline) {
			t.Fatalf("JWT() = %q, want jwt-renewed (renewal not adopted)", u.JWT())
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case tok := <-hookCalls:
		if tok != "jwt-renewed" {
			t.Fatalf("hook fired with %q, want jwt-renewed", tok)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("JWT change hook never fired")
	}

	// 2. Drop the session; the reconnect must bear the RENEWED token.
	stream.recvErr <- io.EOF
	deadline = time.Now().Add(300 * time.Millisecond)
	var secondCtx context.Context
	for {
		select {
		case c := <-captured:
			if secondCtx == nil {
				secondCtx = c
				continue // skip the first session's capture
			}
			md, _ := metadata.FromOutgoingContext(c)
			got := md.Get("authorization")
			if len(got) == 1 && got[0] == "Bearer jwt-renewed" {
				return
			}
			t.Fatalf("reconnect authorization = %v, want [Bearer jwt-renewed]", got)
		case <-time.After(time.Until(deadline)):
			t.Fatalf("reconnect never used the renewed JWT (last ctx %v)", secondCtx)
		}
	}
}

// TestUplinkPartialCollectionStillSendsMetrics (M2): a PARTIAL collector
// failure (samples + error) must still ship the collected samples — only a
// total collection failure downgrades to a bare heartbeat.
func TestUplinkPartialCollectionStillSendsMetrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stream := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}
	u := New(
		&fakeStreamer{stream: stream},
		"dev-123",
		"jwt-abc",
		Config{HeartbeatInterval: 10 * time.Millisecond, Logger: quiet()},
		WithCollector(func(context.Context) (*agentv1.MetricBatch, error) {
			return &agentv1.MetricBatch{
				Samples: []*agentv1.Metric{{Name: "cpu.utilization_percent", Value: 42, TimestampMs: time.Now().UnixMilli()}},
			}, errors.New("disk probe: permission denied")
		}),
		WithRand(rand.New(rand.NewSource(1))),
	)
	go func() { _ = u.Run(ctx) }()

	deadline := time.Now().Add(120 * time.Millisecond)
	for {
		stream.sentMu.Lock()
		n := len(stream.sent)
		stream.sentMu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat sent")
		}
		time.Sleep(2 * time.Millisecond)
	}
	stream.sentMu.Lock()
	hb := stream.sent[0].GetHeartbeat()
	stream.sentMu.Unlock()
	if hb == nil {
		t.Fatal("first frame is not a heartbeat")
	}
	if hb.GetMetrics() == nil || len(hb.GetMetrics().GetSamples()) != 1 {
		t.Fatalf("partial collection dropped the collected samples: %+v", hb)
	}
}

// TestBackoffResetsAfterHealthySession (M3): a session that died BEFORE its
// first ack escalates the backoff; the drop of a HEALTHY session (got an
// ack) must reconnect after a fresh MinBackoff instead of the escalated
// value, and must not compound further toward MaxBackoff.
func TestBackoffResetsAfterHealthySession(t *testing.T) {
	const minBackoff = 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// Session 0: dies immediately, no ack (unhealthy) -> escalates the
	// backoff to 2xMin for the next drop.
	s0 := &timedBidi{fakeBidi: &fakeBidi{recvErr: make(chan error, 1), ctx: ctx}}
	s0.recvErr <- io.EOF
	// Session A: healthy (one ack), then dies. Its drop must wait a RESET
	// MinBackoff — not the escalated 2xMin session 0 left behind.
	sA := &timedBidi{fakeBidi: &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     ctx,
	}}
	sA.recvOut <- &agentv1.StreamResponse{
		Payload: &agentv1.StreamResponse_HeartbeatAck{HeartbeatAck: &agentv1.HeartbeatAck{ServerTimeMs: 1}},
	}
	// Session B: stays alive until ctx ends.
	sB := &timedBidi{fakeBidi: &fakeBidi{recvErr: make(chan error, 1), ctx: ctx}}

	streamer := &sequenceStreamer{streams: []agentv1.AgentService_StreamClient{s0, sA, sB}}
	u := New(streamer, "dev-123", "jwt-abc",
		Config{
			HeartbeatInterval: 5 * time.Millisecond,
			MinBackoff:        minBackoff,
			MaxBackoff:        30 * time.Second,
			Logger:            quiet(),
		},
		WithRand(rand.New(rand.NewSource(1))),
	)
	go func() { _ = u.Run(ctx) }()
	// Drop session A shortly after its ack has been delivered, so its
	// (healthy) death is what triggers the wait under test.
	go func() {
		for sA.openedAt().IsZero() {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // ack delivered + a beat or two
		sA.recvErr <- io.EOF
	}()

	// Wait for session B to be opened (all three calls happened).
	deadline := time.Now().Add(5 * time.Second)
	for streamer.calls() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d stream() calls, want 3", streamer.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Precondition: session 0's (unhealthy) drop waited at least ~MinBackoff.
	if gap := sA.openedAt().Sub(s0.diedAt()); gap < minBackoff/2 {
		t.Fatalf("unhealthy gap = %v, expected the MinBackoff wait (test precondition broken)", gap)
	}
	// THE ASSERTION: session A was HEALTHY, so the wait back to session B is
	// a fresh MinBackoff (+ jitter <= half: at most 1.5xMin = 225ms) — well
	// below the escalated 2xMin (=300ms, + jitter) a non-resetting backoff
	// would have slept.
	gap := sB.openedAt().Sub(sA.diedAt())
	if gap > 270*time.Millisecond {
		t.Fatalf("post-healthy gap = %v, want a reset wait (<= 225ms + slack); backoff did not reset", gap)
	}
}

// timedBidi records when the session opened and when Recv first failed.
// atomic: Send (heartbeat loop) and Recv (downlink) run in different
// goroutines and both stamp openedAt.
type timedBidi struct {
	*fakeBidi
	openedNs atomic.Int64
	diedNs   atomic.Int64
}

func (t *timedBidi) markOpened() {
	t.openedNs.CompareAndSwap(0, time.Now().UnixNano())
}

func (t *timedBidi) openedAt() time.Time {
	if v := t.openedNs.Load(); v != 0 {
		return time.Unix(0, v)
	}
	return time.Time{}
}

func (t *timedBidi) diedAt() time.Time {
	if v := t.diedNs.Load(); v != 0 {
		return time.Unix(0, v)
	}
	return time.Time{}
}

func (t *timedBidi) Send(req *agentv1.StreamRequest) error {
	t.markOpened()
	return t.fakeBidi.Send(req)
}

func (t *timedBidi) Recv() (*agentv1.StreamResponse, error) {
	t.markOpened()
	r, err := t.fakeBidi.Recv()
	if err != nil {
		t.diedNs.CompareAndSwap(0, time.Now().UnixNano())
	}
	return r, err
}

// sequenceStreamer hands out one stream per Stream() call (in order).
type sequenceStreamer struct {
	mu      sync.Mutex
	streams []agentv1.AgentService_StreamClient
	n       int
}

func (s *sequenceStreamer) Stream(ctx context.Context, opts ...grpc.CallOption) (agentv1.AgentService_StreamClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n >= len(s.streams) {
		return s.streams[len(s.streams)-1], nil
	}
	st := s.streams[s.n]
	s.n++
	return st, nil
}

func (s *sequenceStreamer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// TestPushLogsRequiresLiveStream (W6-1): PushLogs before a session is an
// error (the shipper keeps the batch queued), and during a session it sends
// a LogBatch frame on the stream.
func TestPushLogsRequiresLiveStream(t *testing.T) {
	batch := &agentv1.LogBatch{Entries: []*agentv1.LogEntry{{
		Id: "abc123", TimestampMs: 1, Level: "INFO", Msg: "agent ready",
	}}}
	// No session yet: the streamer hasn't been asked for a stream, so cur
	// is nil and PushLogs must fail (retryable for the shipper).
	stream := &fakeBidi{
		recvOut: make(chan *agentv1.StreamResponse, 1),
		recvErr: make(chan error, 1),
		ctx:     context.Background(),
	}
	u := testUplink(t, stream)
	if err := u.PushLogs(context.Background(), batch); err == nil {
		t.Fatalf("PushLogs with no live stream: expected error, got nil")
	}
	// An empty batch is a no-op even with no stream.
	if err := u.PushLogs(context.Background(), &agentv1.LogBatch{}); err != nil {
		t.Fatalf("PushLogs(empty) = %v, want nil", err)
	}

	// Run a session (heartbeats flow), then PushLogs must land on the wire.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stream.ctx = ctx
	go func() { _ = u.Run(ctx) }()

	// Wait for the session to register a stream (first heartbeat sent).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := u.PushLogs(ctx, batch); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream.sentMu.Lock()
	var logsFrames int
	var last *agentv1.LogBatch
	for _, f := range stream.sent {
		if lb := f.GetLogs(); lb != nil {
			logsFrames++
			last = lb
		}
	}
	stream.sentMu.Unlock()
	if logsFrames != 1 {
		t.Fatalf("LogBatch frames on the wire = %d, want 1", logsFrames)
	}
	if last.GetEntries()[0].GetId() != "abc123" || last.GetEntries()[0].GetMsg() != "agent ready" {
		t.Fatalf("LogBatch content = %v", last)
	}
}
