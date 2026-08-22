// Package ingest proves (W0-2 DoD) that the generated agent-protocol stubs
// compile on the SERVER side. W1-5 turns this into the real ingest service:
// JWT auth, metric fan-in to TimescaleDB, command dispatch.
package ingest

import (
	"context"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Compile-time check that the server can implement the generated service.
var _ agentv1.AgentServiceServer = (*Service)(nil)

// Service is the gRPC AgentService implementation skeleton.
type Service struct {
	agentv1.UnimplementedAgentServiceServer
}

// NewService builds an AgentService handler.
func NewService() *Service { return &Service{} }

// Enroll issues a device JWT for a bootstrap token (W1-5 implements).
func (s *Service) Enroll(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	return nil, context.DeadlineExceeded // TODO(W1-5)
}

// Stream is the bidi agent uplink (W1-5 implements).
func (s *Service) Stream(stream agentv1.AgentService_StreamServer) error {
	return nil // TODO(W1-5)
}
