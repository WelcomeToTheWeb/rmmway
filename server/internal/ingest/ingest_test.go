package ingest

import (
	"context"
	"net"
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

// newTestServer boots the ingest service on an in-process gRPC server and
// returns a connected client + the service + the device store (for
// asserting on sinks).
func newTestServer(t *testing.T) (*Service, agentv1.AgentServiceClient, *store.MemoryDeviceStore, func()) {
	t.Helper()
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
	return svc, agentv1.NewAgentServiceClient(conn), devices, func() {
		_ = conn.Close()
		grpcServer.Stop()
	}
}

// firstStreamErr opens a client stream, sends one heartbeat frame, and
// returns the error the server surfaces (server-side interceptor rejections
// arrive on the client's first Recv, not on Stream/Send).
func firstStreamErr(t *testing.T, ctx context.Context, client agentv1.AgentServiceClient) error {
	t.Helper()
	stream, err := client.Stream(ctx)
	if err != nil {
		return err
	}
	_ = stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{}},
	})
	_, rerr := stream.Recv()
	return rerr
}

// TestRejectsUnauthenticatedAgent is the core W1-5 DoD: an agent with no /
// bad credentials cannot open a stream.
func TestRejectsUnauthenticatedAgent(t *testing.T) {
	_, client, _, stop := newTestServer(t)
	defer stop()
	ctx := context.Background()

	// No Authorization header at all.
	err := firstStreamErr(t, ctx, client)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: expected Unauthenticated, got %v (%v)", status.Code(err), err)
	}

	// Bogus token.
	badCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer garbage"))
	if err := firstStreamErr(t, badCtx, client); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("garbage token: expected Unauthenticated, got %v (%v)", status.Code(err), err)
	}
}

// TestStreamRejectsUnknownDevice: a well-formed JWT for a device the server
// doesn't know must be rejected (a valid-signature token from another org).
func TestStreamRejectsUnknownDevice(t *testing.T) {
	svc, client, _, stop := newTestServer(t)
	defer stop()
	ctx := context.Background()

	tok, err := svc.mintJWT("dev-ghost")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+tok))
	if err := firstStreamErr(t, mdCtx, client); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown device: expected Unauthenticated, got %v (%v)", status.Code(err), err)
	}
}

// TestEnrollThenStreamAndMetrics proves the happy path: a freshly
// bootstrapped agent enrolls, receives a JWT, opens a stream, pushes
// metrics, and the server records them.
func TestEnrollThenStreamAndMetrics(t *testing.T) {
	svc, client, devices, stop := newTestServer(t)
	defer stop()
	ctx := context.Background()

	// 1. Mint a bootstrap token + enroll.
	bootTok, devID := svc.MintBootstrapToken()
	enroll, err := client.Enroll(ctx, &agentv1.EnrollRequest{
		BootstrapToken: bootTok,
		Hostname:       "fileserver-01",
		Os:             "linux",
		Arch:           "amd64",
		AgentVersion:   "0.1.0",
		Interfaces:     []string{"eth0:10.0.0.5/24"},
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if enroll.DeviceId != devID {
		t.Fatalf("device id mismatch: got %q want %q", enroll.DeviceId, devID)
	}
	if enroll.Jwt == "" {
		t.Fatal("enroll returned no JWT")
	}

	// 2. Re-enroll with the SAME (now consumed) bootstrap token must fail —
	// a stolen/replayed code can't mint a second identity.
	_, err = client.Enroll(ctx, &agentv1.EnrollRequest{BootstrapToken: bootTok, Hostname: "clone"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("replay of consumed bootstrap token: expected PermissionDenied, got %v (%v)", status.Code(err), err)
	}

	// 3. Open a stream with the JWT and push a heartbeat carrying metrics.
	mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+enroll.Jwt))
	stream, err := client.Stream(mdCtx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := stream.Send(&agentv1.StreamRequest{
		Payload: &agentv1.StreamRequest_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{
				TimestampMs:   now,
				CpuPercent:    12.5,
				MemoryPercent: 42.0,
				Metrics: &agentv1.MetricBatch{
					CollectedAtMs: now,
					Samples: []*agentv1.Metric{
						{Name: "cpu.utilization_percent", Value: 12.5, TimestampMs: now},
						{Name: "memory.used_percent", Value: 42.0, TimestampMs: now},
						{Name: "disk.used_percent", Source: "sda1", Value: 77.0, TimestampMs: now},
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send heartbeat+metrics: %v", err)
	}

	// 4. Read the heartbeat ack (proves the server accepted the frame).
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if resp.GetHeartbeatAck() == nil {
		t.Fatalf("expected a heartbeat ack, got %+v", resp)
	}

	// 5. Server recorded all three samples for the device.
	sink := svc.Metrics().(*store.MemoryMetricsSink)
	if got := sink.Count(); got != 3 {
		t.Fatalf("expected 3 samples stored, got %d", got)
	}
	names := map[string]bool{}
	for _, s := range sink.Samples(devID) {
		names[s.Name+s.Source] = true
	}
	for _, want := range []string{"cpu.utilization_percent", "memory.used_percent", "disk.used_percentsda1"} {
		if !names[want] {
			t.Fatalf("missing metric %q in %v", want, names)
		}
	}

	// 6. The device is registered and online.
	d, ok := devices.GetByID(devID)
	if !ok {
		t.Fatalf("device %s not in registry", devID)
	}
	if d.Hostname != "fileserver-01" || !d.Online {
		t.Fatalf("device state wrong: %+v", d)
	}

	_ = stream.CloseSend()
}
