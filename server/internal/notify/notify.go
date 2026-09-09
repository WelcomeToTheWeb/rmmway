// Package notify provides human notification channels for alerts and
// escalations (gap #6, wave 3, lane B). Channels are adapters over the
// existing event bus: email (reusing the smtp outbox), Slack, Microsoft
// Teams, PagerDuty, and generic webhooks.
//
// Channel configuration is stored in server_config (JSON-serialized).
// Each channel has a Type, Name, Config map, and Enabled flag.
//
// The Router consults notification policies (0016_notification_policies)
// to determine which channels to fire for each event category:
// alert → users by role/client, escalation → on-call schedule.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
)

// ChannelType identifies a notification channel implementation.
type ChannelType string

const (
	ChannelEmail     ChannelType = "email"
	ChannelSlack     ChannelType = "slack"
	ChannelTeams     ChannelType = "teams"
	ChannelPagerDuty ChannelType = "pagerduty"
	ChannelWebhook   ChannelType = "webhook"
)

// ChannelConfig is the persisted channel definition.
type ChannelConfig struct {
	ID      string                 `json:"id"`
	Type    ChannelType            `json:"type"`
	Name    string                 `json:"name"`
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config"`
}

// SendRequest is what the router sends to a channel.
type SendRequest struct {
	Category string // "alert" or "escalation"
	Title    string
	Message  string
	Data     map[string]interface{}
}

// Channel is the interface each notification adapter implements.
type Channel interface {
	// Send delivers the notification. Returns nil on success, or an
	// error describing the failure.
	Send(ctx context.Context, req SendRequest) error
	// Test sends a test notification to verify the channel is configured
	// correctly. Returns nil on success.
	Test(ctx context.Context) error
}

// Sender creates channel instances from their configurations.
type Sender struct {
	// smtpSend is the SMTP Send function (injected from main).
	smtpSend func(ctx context.Context, host, port string, from, to, username, password, subject, body string) error
	// chans is the channel configs (populated by the router).
	chans []*ChannelConfig
}

// NewSender creates a Sender with the SMTP send function.
func NewSender(smtpSend func(ctx context.Context, host, port string, from, to, username, password, subject, body string) error) *Sender {
	return &Sender{smtpSend: smtpSend}
}

// For creates a Channel from a ChannelConfig.
func (s *Sender) For(cfg *ChannelConfig) (Channel, error) {
	switch cfg.Type {
	case ChannelEmail:
		return newEmailChannel(cfg, s.smtpSend), nil
	case ChannelSlack:
		return newSlackChannel(cfg), nil
	case ChannelTeams:
		return newTeamsChannel(cfg), nil
	case ChannelPagerDuty:
		return newPagerDutyChannel(cfg), nil
	case ChannelWebhook:
		return newWebhookChannel(cfg), nil
	default:
		return nil, fmt.Errorf("unknown channel type %q", cfg.Type)
	}
}

// ParseChannelConfig parses a JSON-encoded channel config from server_config.
func ParseChannelConfig(jsonStr string) ([]*ChannelConfig, error) {
	if jsonStr == "" {
		return nil, nil
	}
	var cfgs []*ChannelConfig
	err := json.Unmarshal([]byte(jsonStr), &cfgs)
	return cfgs, err
}

// MarshalChannelConfig serializes channel configs for storage in server_config.
func MarshalChannelConfig(cfgs []*ChannelConfig) (string, error) {
	b, err := json.Marshal(cfgs)
	return string(b), err
}