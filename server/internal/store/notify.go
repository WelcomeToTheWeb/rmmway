package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/notify"
)

// NotificationPolicy defines routing rules for notification categories.
type NotificationPolicy struct {
	ID        string    `json:"id"`
	Category  string    `json:"category"`
	ClientID  *string   `json:"client_id,omitempty"`
	Role      *string   `json:"role,omitempty"`
	Channels  []string  `json:"channels"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrPolicyNotFound is returned when a policy is unknown.
var ErrPolicyNotFound = errors.New("notification policy not found")

// NotifyStore persists notification channels and routing policies.
type NotifyStore interface {
	// Channels
	ListChannels(ctx context.Context) ([]*notify.ChannelConfig, error)
	GetChannel(ctx context.Context, id string) (*notify.ChannelConfig, error)
	CreateChannel(ctx context.Context, ch *notify.ChannelConfig) (*notify.ChannelConfig, error)
	UpdateChannel(ctx context.Context, ch *notify.ChannelConfig) (*notify.ChannelConfig, error)
	DeleteChannel(ctx context.Context, id string) error
	// Policies
	ListPolicies(ctx context.Context) ([]*NotificationPolicy, error)
	GetPolicy(ctx context.Context, id string) (*NotificationPolicy, error)
	CreatePolicy(ctx context.Context, p *NotificationPolicy) (*NotificationPolicy, error)
	UpdatePolicy(ctx context.Context, id string, p *NotificationPolicy) (*NotificationPolicy, error)
	DeletePolicy(ctx context.Context, id string) error
}

func newPolicyID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "pol-" + hex.EncodeToString(raw), nil
}

func newChannelID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "ch-" + hex.EncodeToString(raw), nil
}

// InMemoryNotifyStore is a simple in-memory store for notification
// channels and policies (tests + in-memory mode).
type InMemoryNotifyStore struct {
	mu       chan struct{}
	channels map[string]*notify.ChannelConfig
	policies map[string]*NotificationPolicy
}

func NewInMemoryNotifyStore() *InMemoryNotifyStore {
	return &InMemoryNotifyStore{
		mu:       make(chan struct{}, 1),
		channels: make(map[string]*notify.ChannelConfig),
		policies: make(map[string]*NotificationPolicy),
	}
}

func (s *InMemoryNotifyStore) ListChannels(_ context.Context) ([]*notify.ChannelConfig, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	var out []*notify.ChannelConfig
	for _, c := range s.channels {
		out = append(out, c)
	}
	return out, nil
}

func (s *InMemoryNotifyStore) GetChannel(_ context.Context, id string) (*notify.ChannelConfig, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	c, ok := s.channels[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (s *InMemoryNotifyStore) CreateChannel(_ context.Context, ch *notify.ChannelConfig) (*notify.ChannelConfig, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	id, err := newChannelID()
	if err != nil {
		return nil, err
	}
	ch.ID = id
	s.channels[id] = ch
	return ch, nil
}

func (s *InMemoryNotifyStore) UpdateChannel(_ context.Context, ch *notify.ChannelConfig) (*notify.ChannelConfig, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	_, ok := s.channels[ch.ID]
	if !ok {
		return nil, ErrNotFound
	}
	s.channels[ch.ID] = ch
	return ch, nil
}

func (s *InMemoryNotifyStore) DeleteChannel(_ context.Context, id string) error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	_, ok := s.channels[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.channels, id)
	return nil
}

func (s *InMemoryNotifyStore) ListPolicies(_ context.Context) ([]*NotificationPolicy, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	var out []*NotificationPolicy
	for _, p := range s.policies {
		out = append(out, p)
	}
	return out, nil
}

func (s *InMemoryNotifyStore) GetPolicy(_ context.Context, id string) (*NotificationPolicy, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	p, ok := s.policies[id]
	if !ok {
		return nil, ErrPolicyNotFound
	}
	return p, nil
}

func (s *InMemoryNotifyStore) CreatePolicy(_ context.Context, policy *NotificationPolicy) (*NotificationPolicy, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	id, err := newPolicyID()
	if err != nil {
		return nil, err
	}
	policy.ID = id
	policy.CreatedAt = time.Now().UTC()
	policy.UpdatedAt = time.Now().UTC()
	s.policies[id] = policy
	return policy, nil
}

func (s *InMemoryNotifyStore) UpdatePolicy(_ context.Context, id string, policy *NotificationPolicy) (*NotificationPolicy, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	existing, ok := s.policies[id]
	if !ok {
		return nil, ErrPolicyNotFound
	}
	if policy.Category != "" {
		existing.Category = policy.Category
	}
	existing.ClientID = policy.ClientID
	existing.Role = policy.Role
	if policy.Channels != nil {
		existing.Channels = policy.Channels
	}
	existing.Enabled = policy.Enabled
	existing.UpdatedAt = time.Now().UTC()
	return existing, nil
}

func (s *InMemoryNotifyStore) DeletePolicy(_ context.Context, id string) error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	_, ok := s.policies[id]
	if !ok {
		return ErrPolicyNotFound
	}
	delete(s.policies, id)
	return nil
}
