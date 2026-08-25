// Bus is the event transport the flows execute over (W5-2). Production is
// NATS/JetStream (nats.go); tests use the in-process memBus so the engine
// logic can be exercised without a broker. Both are at-least-once, so the
// engine treats handlers as idempotent.

package flow

import (
	"context"
	"sync"
	"time"
)

// Event is one hop on the bus. The engine publishes step events; the
// sampler publishes trigger events; the ingest hook publishes command
// result events. Fields apply per type:
//
//	flow.trigger:     FlowID, DeviceID, Source, Value, DeviceHostname
//	flow.step:        RunID, NodeID
//	command.result:   CommandID, Status, DeviceID
//	flow.notify:      RunID, NodeID, DeviceID, Message (fan-out for W6-2)
type Event struct {
	Type      string    `json:"type"`
	FlowID    int64     `json:"flow_id,omitempty"`
	RunID     int64     `json:"run_id,omitempty"`
	NodeID    string    `json:"node_id,omitempty"`
	DeviceID  string    `json:"device_id,omitempty"`
	Source    string    `json:"source,omitempty"`
	Value     *float64  `json:"value,omitempty"`
	CommandID string    `json:"command_id,omitempty"`
	Status    string    `json:"status,omitempty"`
	Message   string    `json:"message,omitempty"`
	At        time.Time `json:"at"`
}

// Bus subject names (NATS subject per event type; the engine subscribes to
// all of them under the rmmway.events.> wildcard).
const (
	SubjectTrigger = "rmmway.events.flow.trigger"
	SubjectStep    = "rmmway.events.flow.step"
	SubjectCommand = "rmmway.events.command.result"
	SubjectNotify  = "rmmway.events.flow.notify"
	SubjectAll     = "rmmway.events.>"
)

// Bus is the transport interface.
type Bus interface {
	// Publish delivers ev to every subscriber of subject (at-least-once).
	Publish(ctx context.Context, subject string, ev *Event) error
	// Subscribe registers a handler for every event matching filter
	// (a NATS subject filter, or an exact subject on the memBus). The
	// handler runs on the bus's dispatch goroutine; it must be safe to
	// call concurrently with itself.
	Subscribe(ctx context.Context, filter string, handler func(ctx context.Context, subject string, ev *Event) error) error
	// Close shuts the bus down and stops dispatching.
	Close()
}

// ---- in-memory bus (tests / dev without NATS) --------------------------------

// memBus is a fan-out in-process Bus: Publish delivers synchronously to
// every matching handler. Synchronous delivery keeps tests deterministic
// (publish a trigger, the run has already advanced one hop by the time
// Publish returns).
type memBus struct {
	mu   sync.Mutex
	hdrs []memHandler
}

type memHandler struct {
	filter  string
	handler func(ctx context.Context, subject string, ev *Event) error
}

// NewMemBus builds an in-process Bus (used by unit tests and by the dev
// server when NATS is down, so a flow still runs in-process).
func NewMemBus() Bus { return &memBus{} }

func (b *memBus) Publish(ctx context.Context, subject string, ev *Event) error {
	b.mu.Lock()
	hdrs := make([]memHandler, len(b.hdrs))
	copy(hdrs, b.hdrs)
	b.mu.Unlock()
	for _, h := range hdrs {
		if h.filter != subject && h.filter != SubjectAll {
			continue
		}
		_ = h.handler(ctx, subject, ev)
	}
	return nil
}

func (b *memBus) Subscribe(ctx context.Context, filter string, handler func(ctx context.Context, subject string, ev *Event) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hdrs = append(b.hdrs, memHandler{filter: filter, handler: handler})
	return nil
}

func (b *memBus) Close() {}
