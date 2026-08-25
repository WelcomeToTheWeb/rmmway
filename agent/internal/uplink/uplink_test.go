package uplink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
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
