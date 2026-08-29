// NATS/JetStream implementation of the W5-2 Bus.
//
// One JetStream stream (RMMWAY_EVENTS, subjects rmmway.events.>) carries
// every flow hop; the engine consumes it with a single durable consumer.
// JetStream is at-least-once, which is exactly what the engine wants: a
// step event delivered twice is a no-op (the run's conditional transition
// already moved on), and a step event lost to a restart is re-covered by
// the engine's sweep, which re-publishes the pending hop.

package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// NATSBus is a JetStream-backed Bus.
type NATSBus struct {
	nc      *nats.Conn
	js      nats.JetStreamContext
	sub     *nats.Subscription
	stream  string
	durable string
}

// NewNatsBus connects to the NATS server at url and ensures the stream for
// the flow subjects exists. stream/durable are fixed names so a server
// restart resumes the same consumer instead of forking a second one.
func NewNatsBus(ctx context.Context, url, stream, durable string) (*NATSBus, error) {
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	cfg := &nats.StreamConfig{
		Name:     stream,
		Subjects: []string{SubjectAll},
		MaxAge:   24 * time.Hour,
	}
	// Idempotent stream bootstrap: create it if absent, keep it if present.
	if _, err := js.StreamInfo(stream); err != nil {
		if _, aerr := js.AddStream(cfg); aerr != nil {
			nc.Close()
			return nil, fmt.Errorf("nats stream %s: %w", stream, aerr)
		}
	}
	return &NATSBus{nc: nc, js: js, stream: stream, durable: durable}, nil
}

// Publish appends the event to the stream (publish-ack before return).
func (b *NATSBus) Publish(ctx context.Context, subject string, ev *Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	_ = ctx // the publish-ack is bounded by the connection timeout
	_, err = b.js.Publish(subject, data)
	return err
}

// Subscribe binds the durable consumer to the stream and starts dispatching
// events to handler (one at a time, acked after the handler returns).
func (b *NATSBus) Subscribe(ctx context.Context, filter string, handler func(ctx context.Context, subject string, ev *Event) error) error {
	sub, err := b.js.Subscribe(filter, func(m *nats.Msg) {
		var ev Event
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			// Undecodable payload: ack it (redelivery would loop forever);
			// the engine's sweep recovers the run state from Postgres.
			_ = m.Ack()
			return
		}
		if err := handler(ctx, m.Subject, &ev); err != nil {
			// NAK -> requeue: transient errors (DB blip) are retried; the
			// handler is idempotent so a replay is safe.
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	}, nats.BindStream(b.stream), nats.Durable(b.durable), nats.AckExplicit())
	if err != nil {
		return fmt.Errorf("nats subscribe %s: %w", filter, err)
	}
	b.sub = sub
	return nil
}

// Close tears the consumer + connection down.
func (b *NATSBus) Close() {
	if b.sub != nil {
		_ = b.sub.Unsubscribe()
	}
	if b.nc != nil {
		b.nc.Close()
	}
}

// Connected reports whether the transport is usable (health probe / e2e).
func (b *NATSBus) Connected() bool { return b.nc != nil && b.nc.IsConnected() }
