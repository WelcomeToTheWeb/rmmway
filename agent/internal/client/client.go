// Package client proves (W0-2 DoD) that the generated agent-protocol stubs
// compile on the AGENT side. W1-1/W1-4 turn this into the real transport
// (dial, enroll, heartbeat loop, reconnect + replay).
package client

import (
	"context"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"google.golang.org/grpc"
)

// Compile-time check that the agent can call the generated client.
var _ agentv1.AgentServiceClient = (*Client)(nil)

// Client wraps the generated AgentService client.
type Client struct {
	agentv1.AgentServiceClient
	conn *grpc.ClientConn
}

// NewClient dials the server and wraps the generated client.
func NewClient(ctx context.Context, conn *grpc.ClientConn) *Client {
	return &Client{AgentServiceClient: agentv1.NewAgentServiceClient(conn), conn: conn}
}

// Enroll requests the agent's persistent identity (W1-4 implements).
func (c *Client) Enroll(ctx context.Context, req *agentv1.EnrollRequest, opts ...grpc.CallOption) (*agentv1.EnrollResponse, error) {
	return c.AgentServiceClient.Enroll(ctx, req, opts...)
}

// Close closes the underlying transport.
func (c *Client) Close() error { return c.conn.Close() }
