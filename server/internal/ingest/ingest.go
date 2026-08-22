package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Config holds ingest service settings.
type Config struct {
	// JWTSecret signs/verifies agent JWTs (HS256). From RMMWAY_JWT_SECRET.
	JWTSecret []byte
	// JWTLifetime is how long a minted agent JWT is valid (default 720h).
	JWTLifetime time.Duration
	// DefaultHeartbeatIntS / DefaultMetricIntS seed a new device's cadence.
	DefaultHeartbeatIntS int32
	DefaultMetricIntS    int32
}

func (c *Config) withDefaults() {
	if len(c.JWTSecret) == 0 {
		c.JWTSecret = []byte("rmmway-dev-secret-change-me")
	}
	if c.JWTLifetime <= 0 {
		c.JWTLifetime = 720 * time.Hour
	}
	if c.DefaultHeartbeatIntS <= 0 {
		c.DefaultHeartbeatIntS = 30
	}
	if c.DefaultMetricIntS <= 0 {
		c.DefaultMetricIntS = 30
	}
}

// Service is the gRPC AgentService implementation (W1-5).
type Service struct {
	agentv1.UnimplementedAgentServiceServer

	cfg      Config
	devices  *DeviceRegistry
	metrics  MetricsSink
	dispatch *Dispatcher

	// bootstrapTokens: one-time enroll codes -> pre-allocated device_id.
	// The operator mints a code when bootstrapping a machine (W1-3 wires
	// the mint endpoint; for W1-5, MintBootstrapToken is the API).
	mu              sync.Mutex
	bootstrapTokens map[string]string
	// streams: live agent downlink writers, device_id -> chan.
	streams map[string]chan *agentv1.StreamResponse
	streamW map[string]*streamWriter
}

// streamWriter couples a device's response channel to its gRPC server stream
// so command dispatch can push to a live agent.
type streamWriter struct {
	devID string
	ch    chan *agentv1.StreamResponse
}

// NewService builds an AgentService with in-memory sinks. W1-6 injects the
// Timescale-backed MetricsSink + DeviceRegistry here.
func NewService(cfg Config, metrics MetricsSink) *Service {
	cfg.withDefaults()
	devices := NewDeviceRegistry()
	s := &Service{
		cfg:             cfg,
		devices:         devices,
		metrics:         metrics,
		bootstrapTokens: make(map[string]string),
		streams:         make(map[string]chan *agentv1.StreamResponse),
		streamW:         make(map[string]*streamWriter),
	}
	s.dispatch = NewDispatcher(s)
	return s
}

// Devices exposes the registry for tests/admin.
func (s *Service) Devices() *DeviceRegistry { return s.devices }

// Dispatcher exposes the command dispatcher for tests/admin.
func (s *Service) Dispatcher() *Dispatcher { return s.dispatch }

// MetricsSink exposes the metrics sink for tests/admin.
func (s *Service) Metrics() MetricsSink { return s.metrics }

// CommandSink implementation: push a minted command to the live stream.
func (s *Service) Push(deviceID string, cmd *agentv1.Command) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.streamW[deviceID]; ok {
		select {
		case w.ch <- &agentv1.StreamResponse{Payload: &agentv1.StreamResponse_Command{Command: cmd}}:
			return true
		default:
			return false // agent's stream is backed up — treat as unreachable
		}
	}
	return false
}

// MintBootstrapToken creates a one-time enroll code bound to a fresh
// device_id. W1-3's bootstrap installer consumes the returned token.
func (s *Service) MintBootstrapToken() (token, deviceID string) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	token = "bt-" + hex.EncodeToString(raw)
	deviceID = "dev-" + hex.EncodeToString(raw)[:12]
	s.mu.Lock()
	s.bootstrapTokens[token] = deviceID
	s.mu.Unlock()
	return token, deviceID
}

// ---- auth -------------------------------------------------------------------

// JWTClaims for an agent token.
type JWTClaims struct {
	DeviceID string `json:"did"`
	jwt.RegisteredClaims
}

func (s *Service) mintJWT(deviceID string) (string, error) {
	now := time.Now()
	claims := JWTClaims{
		DeviceID: deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   deviceID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.JWTLifetime)),
			Issuer:    "rmmway",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.cfg.JWTSecret)
}

// verifyJWT parses + validates the Bearer token and returns the device id.
func (s *Service) verifyJWT(tok string) (string, error) {
	claims := &JWTClaims{}
	_, err := jwt.ParseWithClaims(tok, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.cfg.JWTSecret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return "", err
	}
	if claims.DeviceID == "" {
		return "", errors.New("token missing device id")
	}
	return claims.DeviceID, nil
}

// bearerFromMD extracts `Authorization: Bearer <tok>`.
func bearerFromMD(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization header")
	}
	v := vals[0]
	if !strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return "", status.Error(codes.Unauthenticated, "authorization must be Bearer <token>")
	}
	tok := strings.TrimSpace(v[len("bearer "):])
	if tok == "" {
		return "", status.Error(codes.Unauthenticated, "empty bearer token")
	}
	return tok, nil
}

// JWTInterceptor enforces agent JWTs on every unary RPC except Enroll.
// Exported so cmd/server can wire it into the gRPC server.
func (s *Service) JWTInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == "/rmmway.agent.v1.AgentService/Enroll" {
		return handler(ctx, req)
	}
	tok, err := bearerFromMD(ctx)
	if err != nil {
		return nil, err
	}
	devID, err := s.verifyJWT(tok)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid agent token: "+err.Error())
	}
	if _, ok := s.devices.Get(devID); !ok {
		return nil, status.Error(codes.Unauthenticated, "unknown device")
	}
	// Bind the authenticated device to the context for the handler.
	return handler(withDeviceID(ctx, devID), req)
}

type deviceIDKey struct{}

func withDeviceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, deviceIDKey{}, id)
}

// DeviceIDFromContext returns the JWT-verified device id (set by jwtInterceptor).
func DeviceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(deviceIDKey{}).(string)
	return id, ok
}

// ---- AgentService implementation -------------------------------------------

// Enroll exchanges a one-time bootstrap token for the device's persistent
// identity (device_id + agent JWT). Re-enroll is refused: a device that
// already exists keeps its identity (restart must not re-enroll).
func (s *Service) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if req.GetBootstrapToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "bootstrap_token is required")
	}
	s.mu.Lock()
	devID, ok := s.bootstrapTokens[req.GetBootstrapToken()]
	if ok {
		delete(s.bootstrapTokens, req.GetBootstrapToken()) // one-time
	}
	s.mu.Unlock()
	if !ok {
		// If this token was already consumed, it maps to a known device:
		// refuse silent re-enroll (a cloned agent with a stolen token
		// should not mint a second identity).
		return nil, status.Error(codes.PermissionDenied, "unknown or already-used bootstrap token")
	}

	tok, err := s.mintJWT(devID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "mint jwt: %v", err)
	}
	now := time.Now().UTC()
	s.devices.Upsert(&Device{
		ID:            devID,
		Hostname:      req.GetHostname(),
		OS:            req.GetOs(),
		Arch:          req.GetArch(),
		AgentVersion:  req.GetAgentVersion(),
		Interfaces:    req.GetInterfaces(),
		Online:        true,
		LastSeen:      now,
		HeartbeatIntS: s.cfg.DefaultHeartbeatIntS,
		MetricIntS:    s.cfg.DefaultMetricIntS,
	})
	return &agentv1.EnrollResponse{
		DeviceId:           devID,
		Jwt:                tok,
		HeartbeatIntervalS: s.cfg.DefaultHeartbeatIntS,
		MetricIntervalS:    s.cfg.DefaultMetricIntS,
	}, nil
}

// Stream is the agent's long-lived uplink/downlink.
func (s *Service) Stream(stream agentv1.AgentService_StreamServer) error {
	tok, err := bearerFromMD(stream.Context())
	if err != nil {
		return err
	}
	devID, err := s.verifyJWT(tok)
	if err != nil {
		return status.Error(codes.Unauthenticated, "invalid agent token: "+err.Error())
	}
	if _, ok := s.devices.Get(devID); !ok {
		return status.Error(codes.Unauthenticated, "unknown device")
	}

	// Register the live stream for command dispatch.
	ch := make(chan *agentv1.StreamResponse, 16)
	s.mu.Lock()
	old, existed := s.streamW[devID]
	if existed {
		// Close the prior stream's writer so the old connection gets told
		// to go away (one live stream per device).
		close(old.ch)
	}
	s.streamW[devID] = &streamWriter{devID: devID, ch: ch}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if w := s.streamW[devID]; w != nil && w.ch == ch {
			delete(s.streamW, devID)
		}
		s.mu.Unlock()
	}()

	// Downlink pump: forward dispatched commands / heartbeats to the agent.
	go func() {
		for resp := range ch {
			if err := stream.Send(resp); err != nil {
				return
			}
		}
	}()

	s.devices.Touch(devID)
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err // client disconnect / RPC error
		}
		switch p := frame.GetPayload().(type) {
		case *agentv1.StreamRequest_Heartbeat:
			hb := p.Heartbeat
			s.devices.Touch(devID)
			if b := hb.GetMetrics(); b != nil && len(b.GetSamples()) > 0 {
				_ = s.metrics.Write(devID, b)
			}
			ack := &agentv1.StreamResponse{Payload: &agentv1.StreamResponse_HeartbeatAck{
				HeartbeatAck: &agentv1.HeartbeatAck{
					ServerTimeMs:       time.Now().UnixMilli(),
					HeartbeatIntervalS: 0, // keep current
					MetricIntervalS:    0,
				},
			}}
			if err := stream.Send(ack); err != nil {
				return err
			}
		case *agentv1.StreamRequest_Metrics:
			if len(p.Metrics.GetSamples()) > 0 {
				if err := s.metrics.Write(devID, p.Metrics); err != nil {
					// Sink errors (e.g. DB down) must not kill the stream;
					// the agent's offline spool + replay covers the gap.
					continue
				}
			}
		}
	}
}
