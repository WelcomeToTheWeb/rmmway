package ingest

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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

	// 1. Mint a bootstrap token + enroll. (Direct service calls for the
	// token-lifecycle steps: BootstrapGraceWindow is test-tuned, and direct
	// calls keep those steps in this goroutine — no cross-goroutine read of
	// the tuned window via the gRPC handler.)
	bootTok, devID := svc.MintBootstrapToken()
	enroll, err := svc.Enroll(ctx, &agentv1.EnrollRequest{
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

	// 2. H2: an IMMEDIATE replay of the consumed token is idempotent — the
	// grace window covers the case where the agent's identity persist failed
	// after a successful enroll (the agent re-enrolls with the same token
	// and must get the SAME device back, not a second identity or an error).
	// (Default window — a fresh consumption is always inside it.)
	re, err := svc.Enroll(ctx, &agentv1.EnrollRequest{BootstrapToken: bootTok, Hostname: "fileserver-01"})
	if err != nil {
		t.Fatalf("in-grace re-enroll: %v", err)
	}
	if re.DeviceId != devID {
		t.Fatalf("grace re-enroll minted a new identity: got %q want %q", re.DeviceId, devID)
	}

	// 3. Replay once the grace window has LAPSED must fail — a
	// stolen/replayed code can't mint a second identity. Age the
	// consumption record past the (default 15m) window directly: the
	// machine clock can be coarser than the test timescale, so a
	// zeroed window would race the next clock tick.
	svc.mu.Lock()
	if ct, ok := svc.consumedTokens[bootTok]; ok {
		ct.at = time.Now().Add(-20 * time.Minute)
		svc.consumedTokens[bootTok] = ct
	} else {
		svc.mu.Unlock()
		t.Fatal("consumed token record missing")
	}
	svc.mu.Unlock()
	replay, err := svc.Enroll(ctx, &agentv1.EnrollRequest{BootstrapToken: bootTok, Hostname: "fileserver-01"})
	if err == nil {
		t.Fatalf("post-grace replay: expected PermissionDenied, got OK (device %q)", replay.DeviceId)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("post-grace replay of consumed bootstrap token: expected PermissionDenied, got %v (%v)", status.Code(err), err)
	}

	// 4. Open a stream with the JWT and push a heartbeat carrying metrics.
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

	// 5. Read the heartbeat ack (proves the server accepted the frame).
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if resp.GetHeartbeatAck() == nil {
		t.Fatalf("expected a heartbeat ack, got %+v", resp)
	}

	// 6. Server recorded all three samples for the device.
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

	// 7. The device is registered and online.
	d, ok := devices.GetByID(devID)
	if !ok {
		t.Fatalf("device %s not in registry", devID)
	}
	if d.Hostname != "fileserver-01" || !d.Online {
		t.Fatalf("device state wrong: %+v", d)
	}

	_ = stream.CloseSend()
}

// TestHeartbeatAckRenewsExpiringJWT (H3) proves the renewal piggyback: a
// stream authenticated with a token in its last quarter of life gets a fresh
// JWT in the heartbeat ack; a healthy token gets a plain ack.
func TestHeartbeatAckRenewsExpiringJWT(t *testing.T) {
	secret := []byte("test-secret")
	lifetime := time.Hour
	devices := store.NewMemoryDeviceStore()
	if err := devices.Register(context.Background(), "dev-1", "fs-01", "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := NewService(Config{JWTSecret: secret, JWTLifetime: lifetime}, store.NewMemoryMetricsSink(1000), devices)
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
	defer func() { _ = conn.Close(); grpcServer.Stop() }()
	client := agentv1.NewAgentServiceClient(conn)
	ctx := context.Background()

	ackFor := func(tok string) *agentv1.HeartbeatAck {
		mdCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+tok))
		stream, err := client.Stream(mdCtx)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		if err := stream.Send(&agentv1.StreamRequest{
			Payload: &agentv1.StreamRequest_Heartbeat{Heartbeat: &agentv1.Heartbeat{TimestampMs: time.Now().UnixMilli()}},
		}); err != nil {
			t.Fatalf("send heartbeat: %v", err)
		}
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv ack: %v", err)
		}
		_ = stream.CloseSend()
		return resp.GetHeartbeatAck()
	}

	// 1. A fresh token (full life left) must NOT trigger a renewal.
	fresh, err := svc.mintJWT("dev-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ack := ackFor(fresh); ack == nil {
		t.Fatal("no ack for fresh token")
	} else if ack.Jwt != "" {
		t.Fatalf("fresh token: unexpected renewal in ack")
	}

	// 2. A token with 10m of life left (< lifetime/4 = 15m) MUST trigger a
	// renewal that verifies as a valid token for the SAME device.
	now := time.Now()
	claims := JWTClaims{
		DeviceID: "dev-1",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "dev-1",
			Issuer:    JWTIssuer,
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
	}
	expiring, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("sign expiring: %v", err)
	}
	ack := ackFor(expiring)
	if ack == nil {
		t.Fatal("no ack for expiring token")
	}
	if ack.Jwt == "" {
		t.Fatal("expiring token: ack carried no renewal JWT")
	}
	if devID, err := svc.verifyJWT(ack.Jwt); err != nil || devID != "dev-1" {
		t.Fatalf("renewal token: dev=%q err=%v, want dev-1/nil", devID, err)
	}
	if rem := svc.jwtRemaining(ack.Jwt); rem <= 0 || rem >= lifetime {
		t.Fatalf("renewal remaining = %v, want ~full lifetime", rem)
	}
}
