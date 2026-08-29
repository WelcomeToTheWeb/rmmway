package ingest

import (
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// newCapsServer boots the ingest service WITH a capability issuer (fresh
// org root) and returns the root for token verification.
func newCapsServer(t *testing.T) (*Service, agentv1.AgentServiceClient, *ca.Root, func()) {
	t.Helper()
	root, err := ca.GenerateRoot()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	devices := store.NewMemoryDeviceStore()
	svc := NewService(Config{
		JWTSecret: []byte("test-secret"),
		Caps:      caps.NewIssuer(root, time.Minute),
	}, store.NewMemoryMetricsSink(1000), devices)
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
	return svc, agentv1.NewAgentServiceClient(conn), root, func() {
		_ = conn.Close()
		grpcServer.Stop()
	}
}

// openAgentStream enrolls a fresh device and opens its authenticated stream,
// with a drain goroutine feeding dispatched commands into cmdCh.
func openAgentStream(t *testing.T, ctx context.Context, svc *Service, client agentv1.AgentServiceClient) (string, agentv1.AgentService_StreamClient, <-chan *agentv1.Command) {
	t.Helper()
	bootTok, devID := svc.MintBootstrapToken()
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: bootTok, Hostname: "h1", Os: "linux", Arch: "amd64",
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cmdCh := make(chan *agentv1.Command, 8)
	ackCh := make(chan struct{}, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				return
			}
			switch {
			case resp.GetCommand() != nil:
				cmdCh <- resp.GetCommand()
			case resp.GetHeartbeatAck() != nil:
				select {
				case ackCh <- struct{}{}:
				default:
				}
			}
		}
	}()
	// Round-trip a heartbeat: the ack proves the server's Stream handler is
	// running AND the stream is registered for dispatch (no race).
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
	}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	select {
	case <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat ack (stream not registered?)")
	}
	return devID, stream, cmdCh
}

func b64(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func waitForCmd(t *testing.T, ch <-chan *agentv1.Command) *agentv1.Command {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no command arrived on the stream")
		return nil
	}
}

// TestDispatchMintsCapabilityToken (W3-3): a dispatched command carries a
// capability token bound to (device, capability, command id), signed by the
// org root.
func TestDispatchMintsCapabilityToken(t *testing.T) {
	svc, client, root, stop := newCapsServer(t)
	defer stop()
	ctx := context.Background()
	devID, stream, cmdCh := openAgentStream(t, ctx, svc, client)
	defer func() { _ = stream.CloseSend() }()

	// run_script: the pushed command must carry a verifiable token.
	cmdID, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "echo hi")},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	cmd := waitForCmd(t, cmdCh)
	if cmd.GetId() != cmdID {
		t.Fatalf("command id mismatch: %q vs %q", cmd.GetId(), cmdID)
	}
	tok := cmd.GetRunScript().GetCapabilityToken()
	if tok == "" {
		t.Fatal("dispatched run_script carries no capability token")
	}
	if err := caps.Verify(tok, root.Cert(), devID, caps.CapRunScript, cmdID, time.Now()); err != nil {
		t.Fatalf("token does not verify for (device, run_script, command): %v", err)
	}
	if got := capClaimCmd(t, tok); got != cmdID {
		t.Fatalf("token bound to cmd %q, dispatched as %q", got, cmdID)
	}

	// reboot: same story with the reboot capability.
	cmdID2, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_Reboot{Reboot: &agentv1.Reboot{DelayS: 0}})
	if err != nil {
		t.Fatalf("dispatch reboot: %v", err)
	}
	cmd2 := waitForCmd(t, cmdCh)
	if cmd2.GetId() != cmdID2 {
		t.Fatalf("command id mismatch: %q vs %q", cmd2.GetId(), cmdID2)
	}
	if err := caps.Verify(cmd2.GetReboot().GetCapabilityToken(), root.Cert(), devID, caps.CapReboot, cmdID2, time.Now()); err != nil {
		t.Fatalf("reboot token does not verify: %v", err)
	}
	// A run_script token must NOT verify for the reboot capability (scope).
	if err := caps.Verify(tok, root.Cert(), devID, caps.CapReboot, cmdID, time.Now()); err == nil {
		t.Fatal("run_script token accepted for the reboot capability")
	}
	// Nor for a DIFFERENT command id (the replay case).
	if err := caps.Verify(tok, root.Cert(), devID, caps.CapRunScript, cmdID2, time.Now()); err == nil {
		t.Fatal("run_script token accepted for a different command id")
	}
}

// TestDispatchLegacyWithoutIssuer: a server without a capability issuer
// (pre-W3-3 / plain-listener deployments) dispatches with empty tokens.
func TestDispatchLegacyWithoutIssuer(t *testing.T) {
	svc, client, _, stop := newTestServer(t)
	defer stop()
	ctx := context.Background()
	devID, stream, cmdCh := openAgentStream(t, ctx, svc, client)
	defer func() { _ = stream.CloseSend() }()

	if _, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "x")},
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	cmd := waitForCmd(t, cmdCh)
	if cmd.GetRunScript().GetCapabilityToken() != "" {
		t.Fatal("legacy dispatch must not carry a capability token")
	}
}

// TestStreamCommandResultRecorded (W3-3): the agent's CommandResult uplink
// frames (RECEIVED, then a final status) are recorded by the dispatcher —
// including REFUSED (capability check failed on the agent).
func TestStreamCommandResultRecorded(t *testing.T) {
	svc, client, _, stop := newCapsServer(t)
	defer stop()
	ctx := context.Background()
	devID, stream, cmdCh := openAgentStream(t, ctx, svc, client)
	defer func() { _ = stream.CloseSend() }()

	cmdID, err := svc.Dispatcher().Dispatch(devID, &agentv1.Command_RunScript{
		RunScript: &agentv1.RunScript{Lang: "sh", ScriptB64: b64(t, "x")},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitForCmd(t, cmdCh)

	// The agent answers RECEIVED, then REFUSED (its capability check failed
	// — e.g. the token was bound to a different device).
	for _, st := range []agentv1.CommandResult_Status{
		agentv1.CommandResult_RECEIVED,
		agentv1.CommandResult_REFUSED,
	} {
		if err := stream.Send(&agentv1.StreamRequest{
			Payload: &agentv1.StreamRequest_CommandResult{
				CommandResult: &agentv1.CommandResult{
					CommandId:     cmdID,
					Status:        st,
					CompletedAtMs: time.Now().UnixMilli(),
					Error:         "capability token is bound to a different device",
				},
			},
		}); err != nil {
			t.Fatalf("send result %v: %v", st, err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		res, ok := svc.Dispatcher().Result(cmdID)
		if ok && res.GetStatus() == agentv1.CommandResult_REFUSED {
			if res.GetError() == "" {
				t.Fatal("REFUSED result lost its error detail")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("REFUSED result never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Refused commands leave the pending set.
	for _, p := range svc.Dispatcher().Pending() {
		if p.GetId() == cmdID {
			t.Fatal("refused command still pending")
		}
	}
	// ResultsFor scopes by device: this device sees its REFUSED result,
	// another device sees nothing.
	mine := svc.Dispatcher().ResultsFor(devID)
	if len(mine) != 1 || mine[0].GetCommandId() != cmdID {
		t.Fatalf("ResultsFor(self): %+v", mine)
	}
	if got := svc.Dispatcher().ResultsFor("dev-someone-else"); len(got) != 0 {
		t.Fatalf("ResultsFor(other) must be empty: %+v", got)
	}
}

// capClaimCmd decodes a capability token's cmd claim (test introspection).
func capClaimCmd(t *testing.T, tok string) string {
	t.Helper()
	claims, err := caps.DecodeClaims(tok)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	return claims.Cmd
}
