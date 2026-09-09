package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
)

// Policy defines routing rules for notification categories.
type Policy struct {
	ID         string         `json:"id"`
	Category   string         `json:"category"`   // "alert" or "escalation"
	ClientID   *string        `json:"client_id"`  // nil = all clients
	Role       *string        `json:"role"`       // nil = all roles
	Channels   []string       `json:"channels"`   // channel IDs to fire
	Enabled    bool           `json:"enabled"`
}

// Router consults notification policies to determine which channels to fire
// for each event. Policies are loaded from the notification_policies table
// (0016) and cached in memory.
type Router struct {
	sender    *Sender
	policies  []*Policy
	log       *log.Logger
}

// NewRouter creates a notification router.
func NewRouter(sender *Sender, policies []*Policy, log *log.Logger) *Router {
	return &Router{sender: sender, policies: policies, log: log}
}

// SetPolicies replaces the router's policies (used after policy updates).
func (r *Router) SetPolicies(policies []*Policy) {
	r.policies = policies
}

// Notify fires notifications for an event, consulting the policies to
// determine which channels to use.
func (r *Router) Notify(ctx context.Context, req SendRequest, clientID *string, role *string) []ChannelResult {
	var matchedChannels []*ChannelConfig
	for _, policy := range r.policies {
		if !policy.Enabled {
			continue
		}
		if policy.Category != req.Category {
			continue
		}
		if policy.ClientID != nil && clientID != nil && *policy.ClientID != *clientID {
			continue
		}
		if policy.Role != nil && role != nil && *policy.Role != *role {
			continue
		}
		for _, channelID := range policy.Channels {
			for _, ch := range r.sender.chans {
				if ch.ID == channelID && ch.Enabled {
					matchedChannels = append(matchedChannels, ch)
					break
				}
			}
		}
	}
	var results []ChannelResult
	for _, ch := range matchedChannels {
		result := ChannelResult{ChannelID: ch.ID, ChannelName: ch.Name}
		channel, err := r.sender.For(ch)
		if err != nil {
			result.Error = fmt.Sprintf("failed to create channel: %v", err)
		} else {
			err = channel.Send(ctx, req)
			if err != nil {
				result.Error = err.Error()
			}
		}
		if result.Error != "" && r.log != nil {
			r.log.Printf("notify: channel %s failed: %v", ch.Name, result.Error)
		}
		results = append(results, result)
	}
	return results
}

// ChannelResult is the outcome of sending to one channel.
type ChannelResult struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	Error       string `json:"error,omitempty"`
}

// ParsePolicies parses JSON-encoded policies from server_config.
func ParsePolicies(jsonStr string) ([]*Policy, error) {
	if jsonStr == "" {
		return nil, nil
	}
	var policies []*Policy
	err := json.Unmarshal([]byte(jsonStr), &policies)
	return policies, err
}

// MarshalPolicies serializes policies for storage in server_config.
func MarshalPolicies(policies []*Policy) (string, error) {
	b, err := json.Marshal(policies)
	return string(b), err
}