//go:build integration

// W1-4 end-to-end integration test: a REAL gRPC AgentService server over a
// real socket, driving the real enroll + uplink packages together. Proves the
// full DoD: first boot enrolls with the bootstrap token and persists the
// identity; a restart reuses it (no second Enroll); and the uplink reports
// heartbeats back authenticated with the issued JWT.
package uplink_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/welcometotheweb/rmmway/agent/internal/collectors"
	"github.com/welcometotheweb/rmmway/agent/internal/enroll"
	"github.com/welcometotheweb/rmmway/agent/internal/uplink"
	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const testBootstrapToken = "bootstrap-secret-123"

// testServer is a real AgentService backed by an in-memory device registry.
type testServer struct {
	agentv1.UnimplementedAgentServiceServer

	mu          sync.Mutex
	enrollCalls int
	enrolledIDs map[string]string // device_id -> hostname at enroll
	gotAuth     []string          // authorization headers seen on streams

	// stream bookkeeping
	streamMu   sync.Mutex
	streams    int
	hbReceived []*agentv1.Heartbeat
}

func newTestServer() *testServer {
	return &testServer{enrolledIDs: map[string]string{}}
}

// Enroll validates the bootstrap token and mints a stable identity. A device
// that already enrolled is not re-counted (mirrors the server's "one identity
// per token" rule) — but it does not error, since the agent never calls it
// again anyway.
func (s *testServer) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if req.GetBootstrapToken() != testBootstrapToken {
		return nil, status.Errorf(codes.Unauthenticated, "bad bootstrap token")
	}
	s.mu.Lock()
	s.enrollCalls++
	id := "dev-" + s.stableHost(req.GetHostname())
	s.enrolledIDs[id] = req.GetHostname()
	s.mu.Unlock()
	return &agentv1.EnrollResponse{
		DeviceId:           id,
		Jwt:                "jwt-for-" + id,
		HeartbeatIntervalS: 1,
		MetricIntervalS:    1,
	}, nil
}

// stableHost maps a hostname to a short stable id (deterministic in tests).
func (s *testServer) stableHost(host string) string {
	return host
}

// Stream requires the Bearer JWT, resolves the device, and records heartbeats.
func (s *testServer) Stream(stream agentv1.AgentService_StreamServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	auth := md.Get("authorization")
	s.mu.Lock()
	s.gotAuth = append(s.gotAuth, auth...)
	s.mu.Unlock()

	// Resolve the device from the JWT (a real server looks it up in the store).
	devID := deviceFromJWT(auth)
	if devID == "" {
		return status.Error(codes.Unauthenticated, "no bearer token")
	}

	s.streamMu.Lock()
	s.streams++
	s.streamMu.Unlock()

	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if hb := req.GetHeartbeat(); hb != nil {
			s.streamMu.Lock()
			s.hbReceived = append(s.hbReceived, hb)
			s.streamMu.Unlock()
		}
	}
}

// deviceFromJWT is a stand-in for the server's JWT→device lookup.
func deviceFromJWT(auth []string) string {
	if len(auth) == 0 || !isBearer(auth[0]) {
		return ""
	}
	token := stripBearer(auth[0])
	// our test tokens are "jwt-for-<devID>"
	if len(token) > len("jwt-for-") && token[:len("jwt-for-")] == "jwt-for-" {
		return token[len("jwt-for-"):]
	}
	return ""
}

func isBearer(h string) bool { return len(h) > 7 && h[:7] == "Bearer " }
func stripBearer(h string) string {
	if isBearer(h) {
		return h[7:]
	}
	return h
}

func TestW14EndToEnd(t *testing.T) {
	// 1. real gRPC server on a localhost socket
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newTestServer()
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	target := lis.Addr().String()
	dial := func(t *testing.T) agentv1.AgentServiceClient {
		t.Helper()
		conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return agentv1.NewAgentServiceClient(conn)
	}

	ctx := context.Background()
	identityPath := filepath.Join(t.TempDir(), "agent-identity.json")

	// ---- First boot: no identity on disk -> enroll + persist -------------
	client1 := dial(t)
	store := enroll.NewStore(identityPath)
	a1 := enroll.New(client1, store, enroll.Gather("0.0.0-inttest"), testBootstrapToken)
	res1, err := a1.EnsureEnrolled(ctx)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if !res1.Enrolled {
		t.Fatal("first boot should enroll")
	}
	devID, jwt := res1.BearerMetadata()
	if devID == "" || jwt == "" {
		t.Fatalf("incomplete identity: dev=%q jwt=%q", devID, jwt)
	}
	// persisted on disk
	onDisk, err := store.Load()
	if err != nil || onDisk == nil {
		t.Fatalf("identity not persisted: %v", err)
	}
	if onDisk.DeviceID != devID {
		t.Fatalf("persisted device %q != %q", onDisk.DeviceID, devID)
	}

	// ---- Restart: identity on disk -> reuse, NO second enroll ------------
	client2 := dial(t)
	a2 := enroll.New(client2, store, enroll.Gather("0.0.0-inttest"), testBootstrapToken)
	res2, err := a2.EnsureEnrolled(ctx)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if res2.Enrolled {
		t.Fatal("restart must NOT re-enroll")
	}
	if res2.Identity.DeviceID != devID {
		t.Fatalf("restart reused wrong device: %q vs %q", res2.Identity.DeviceID, devID)
	}
	srv.mu.Lock()
	enrollCalls := srv.enrollCalls
	srv.mu.Unlock()
	if enrollCalls != 1 {
		t.Fatalf("Enroll called %d times, want exactly 1 (no re-enroll)", enrollCalls)
	}

	// ---- Report back: authenticated uplink sends heartbeats w/ metrics ----
	coll := collectors.NewCollector()
	u := uplink.New(client2, devID, jwt, uplink.Config{
		HeartbeatInterval: 20 * time.Millisecond,
		MinBackoff:        time.Millisecond,
		MaxBackoff:        5 * time.Millisecond,
		Logger:            discardLogger(t),
	}, uplink.WithCollector(coll.Collect))

	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	go func() { _ = u.Run(rctx) }()

	// wait for at least one server-side heartbeat
	deadline := time.Now().Add(280 * time.Millisecond)
	for {
		srv.streamMu.Lock()
		n := len(srv.hbReceived)
		srv.streamMu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never received a heartbeat")
		}
		time.Sleep(3 * time.Millisecond)
	}

	// the heartbeat must have carried collected metrics
	srv.streamMu.Lock()
	hbs := append([]*agentv1.Heartbeat(nil), srv.hbReceived...)
	srv.streamMu.Unlock()
	var withMetrics bool
	for _, hb := range hbs {
		if hb.GetMetrics() != nil && len(hb.GetMetrics().GetSamples()) > 0 {
			withMetrics = true
			break
		}
	}
	if !withMetrics {
		t.Error("no heartbeat carried a collected MetricBatch")
	}

	// and it was authenticated with the issued JWT (not the bootstrap token)
	srv.mu.Lock()
	auths := append([]string(nil), srv.gotAuth...)
	srv.mu.Unlock()
	foundJWT := false
	for _, a := range auths {
		if a == "Bearer "+jwt {
			foundJWT = true
		}
		if a == "Bearer "+testBootstrapToken {
			t.Error("uplink used the bootstrap token for auth instead of the JWT")
		}
	}
	if !foundJWT {
		t.Errorf("no stream authenticated as Bearer %q; got %v", jwt, auths)
	}
}
