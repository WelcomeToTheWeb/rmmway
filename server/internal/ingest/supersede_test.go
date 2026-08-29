package ingest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// TestSupersededStreamAbortsAndReconnects is the W3-3 regression for the
// orphaned-dispatch bug: a second live stream for the same device (an
// operator mTLS probe, a flapping agent, a duplicate install) supersedes
// the first. The evicted stream must end with Aborted — so its client
// reconnects and re-registers — and the device must be dispatchable again
// once the superseding stream is gone.
func TestSupersededStreamAbortsAndReconnects(t *testing.T) {
	ctx := context.Background()

	devices := store.NewMemoryDeviceStore()
	svc := NewService(Config{JWTSecret: []byte("test-secret")}, store.NewMemoryMetricsSink(1000), devices)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(svc.JWTInterceptor))
	agentv1.RegisterAgentServiceServer(grpcServer, svc)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go grpcServer.Serve(lis)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
		grpcServer.Stop()
	}()
	client := agentv1.NewAgentServiceClient(conn)

	bootTok, devID := svc.MintBootstrapToken()
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: bootTok, Hostname: "sup-h", Os: "linux", Arch: "amd64",
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))

	// gen 1: the "agent's" stream.
	streamA, err := client.Stream(mdCtx)
	if err != nil {
		t.Fatalf("stream A: %v", err)
	}
	heartbeat(t, streamA)
	dA := drain(t, streamA)

	// Dispatch reaches gen 1.
	if _, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "echo gen1")},
	}); err != nil {
		t.Fatalf("dispatch to gen1: %v", err)
	}
	waitForCmd(t, dA.cmds)

	// gen 2: the SAME device opens a second stream (the operator-probe
	// case). Gen 1 must be superseded — its Recv ends with Aborted.
	streamB, err := client.Stream(mdCtx)
	if err != nil {
		t.Fatalf("stream B: %v", err)
	}
	heartbeat(t, streamB)
	select {
	case aerr := <-dA.errs:
		st, ok := status.FromError(aerr)
		if !ok || st.Code() != codes.Aborted || !strings.Contains(st.Message(), "superseded") {
			t.Fatalf("gen1 ended with %v, want Aborted/superseded", aerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gen1 stream was not superseded by gen2")
	}

	// Gen 2 goes away (the probe closes). Gen 1 is gone, gen 2 is closed,
	// nobody has reconnected: the device must NOT be dispatchable (wait for
	// gen 2's handler cleanup to finish, then assert it stays unreachable).
	_ = streamB.CloseSend()
	deadline := time.Now().Add(3 * time.Second)
	cleaned := false
	for time.Now().Before(deadline) {
		if _, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
			RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "echo gone")},
		}); err != nil {
			cleaned = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !cleaned {
		t.Fatal("device still dispatchable after its last stream closed (cleanup never landed?)")
	}
	if _, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "echo gone2")},
	}); err == nil {
		t.Fatal("device dispatchable between streams (gen1 aborted, gen2 closed)")
	}

	// The agent reconnects: gen 3 re-registers and dispatch works again.
	streamC, err := client.Stream(mdCtx)
	if err != nil {
		t.Fatalf("stream C: %v", err)
	}
	heartbeat(t, streamC)
	dC := drain(t, streamC)
	if _, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "echo gen3")},
	}); err != nil {
		t.Fatalf("dispatch to gen3 (reconnected): %v", err)
	}
	waitForCmd(t, dC.cmds)
}

// heartbeat round-trips one heartbeat synchronously (ack = the server's
// handler is live and the stream is registered for dispatch). It owns
// Recv until the ack; the caller then starts exactly ONE drain goroutine.
func heartbeat(t *testing.T, s agentv1.AgentService_StreamClient) {
	t.Helper()
	if err := s.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := s.Recv()
		if err != nil {
			t.Fatalf("recv heartbeat ack: %v", err)
		}
		if resp.GetHeartbeatAck() != nil {
			return
		}
	}
	t.Fatal("no heartbeat ack (stream not registered?)")
}

// drain owns a stream's Recv, reporting commands and the terminal error.
type drainer struct {
	cmds chan *agentv1.Command
	errs chan error
}

func drain(t *testing.T, s agentv1.AgentService_StreamClient) *drainer {
	t.Helper()
	d := &drainer{cmds: make(chan *agentv1.Command, 8), errs: make(chan error, 1)}
	cmds, errs := d.cmds, d.errs
	go func() {
		for {
			resp, err := s.Recv()
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
			if c := resp.GetCommand(); c != nil {
				cmds <- c
			}
		}
	}()
	return d
}
