// Service is the W6-2 webhook + event-stream framework. It is a thin,
// Postgres-backed consumer of the NATS event bus:
//
//	<bus: any rmmway.events.> event>
//	    -> journal it (append-only, monotonic seq)
//	    -> fan it out to the live SSE subscribers
//	<every sweepInterval>
//	    -> for each enabled endpoint past its backoff watermark, deliver the
//	       next undelivered event from the journal (seq > last_seq, filtered
//	       to the endpoint's categories); advance the cursor on a 2xx, back
//	       off + dead-letter on repeated failure.
//
// The cursor IS the retry/replay mechanism: an endpoint only moves forward
// on a 2xx, so a downed receiver is re-driven from where it stopped. A manual
// replay (SetCursor) re-drives a range. Deliveries are HMAC-signed; each event
// carries its seq as the payload id + X-RMMway-Id so receivers dedupe the
// at-least-once redeliveries.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/flow"
)

// Event categories (the endpoint's subscription filter + the journal column).
const (
	CategoryAlert      = "alert"      // rmmway.events.alert
	CategoryInventory  = "inventory"  // rmmway.events.device
	CategoryAutomation = "automation" // rmmway.events.flow.* + rmmway.events.command.result
	CategoryOther      = "other"      // anything else on the bus
)

// AllCategories is the full set a user may subscribe to.
var AllCategories = []string{CategoryAlert, CategoryInventory, CategoryAutomation, CategoryOther}

// validCategory reports whether c is a known category.
func validCategory(c string) bool {
	for _, k := range AllCategories {
		if c == k {
			return true
		}
	}
	return false
}

// CategoryForSubject maps a bus subject to its category (the webhook/SSE
// taxonomy). Flow hops + command results are "automation"; the W6-2 alert and
// device subjects are "alert"/"inventory".
func CategoryForSubject(subject string) string {
	switch subject {
	case flow.SubjectAlert:
		return CategoryAlert
	case flow.SubjectDevice:
		return CategoryInventory
	case flow.SubjectTrigger, flow.SubjectStep, flow.SubjectCommand, flow.SubjectNotify:
		return CategoryAutomation
	default:
		return CategoryOther
	}
}

// Envelope is the signed payload delivered to an endpoint and emitted on the
// SSE stream. `Event` is the full bus event (lossless); the envelope adds the
// journal sequence (id), the category, and provenance.
type Envelope struct {
	ID       int64           `json:"id"`
	Version  string          `json:"version"`
	Source   string          `json:"source"`
	Category string          `json:"category"`
	Type     string          `json:"type"`
	DeviceID string          `json:"device_id,omitempty"`
	At       time.Time       `json:"at"`
	Event    json.RawMessage `json:"event"`
}

const envelopeVersion = "rmmway-event/v1"

func (e Event) Envelope() Envelope {
	raw := e.Data
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return Envelope{
		ID: e.Seq, Version: envelopeVersion, Source: "rmmway",
		Category: e.Category, Type: e.Type, DeviceID: e.DeviceID,
		At: e.At, Event: raw,
	}
}

// Service drives journaling + delivery + the live stream.
type Service struct {
	store  *Store
	bus    flow.Bus
	client *http.Client

	sweepInterval time.Duration

	log *log.Logger

	mu   sync.Mutex
	live map[chan Event]liveFilter
}

type liveFilter struct {
	category string // "" = all
}

// New builds a Service. bus is the NATS (or in-memory) event bus; store is
// the Postgres journal + endpoint store.
func New(st *Store, bus flow.Bus) *Service {
	return &Service{
		store:         st,
		bus:           bus,
		client:        &http.Client{},
		sweepInterval: 2 * time.Second,
		live:          make(map[chan Event]liveFilter),
	}
}

// WithLogger sets the framework logger (nil = silent).
func (s *Service) WithLogger(l *log.Logger) *Service { s.log = l; return s }

// WithSweepInterval overrides the delivery sweep cadence (<=0 = 2s).
func (s *Service) WithSweepInterval(d time.Duration) *Service {
	if d > 0 {
		s.sweepInterval = d
	}
	return s
}

// Store exposes the data layer (API / e2e).
func (s *Service) Store() *Store { return s.store }

// Start subscribes to the bus and starts the delivery sweep. It returns once
// the subscription is live (or with a subscription error).
func (s *Service) Start(ctx context.Context) error {
	if err := s.bus.Subscribe(ctx, flow.SubjectAll, s.onEvent); err != nil {
		return fmt.Errorf("webhook subscribe: %w", err)
	}
	s.logf("webhook framework: subscribed to %s (sweep %s)", flow.SubjectAll, s.sweepInterval)
	go func() {
		// An immediate sweep catches events journaled before Start returned
		// (a receiver created right after boot shouldn't wait a full tick).
		s.Sweep(ctx, time.Now())
		t := time.NewTicker(s.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.Sweep(ctx, time.Now())
			}
		}
	}()
	return nil
}

// Close is a no-op (the bus is owned by the caller); it exists for symmetry.
func (s *Service) Close() {}

// ---- bus handler -------------------------------------------------------------

// onEvent journals one bus event and fans it out to the live subscribers. It
// is idempotent enough for at-least-once delivery: a re-delivered bus event
// appends a new journal row (a new seq), which is the intended behavior — the
// bus is the source of truth for "an event happened"; dedupe is the receiver's
// job via the seq. Journal errors are logged and swallowed (returning nil
// ACKs the bus message) so a transient DB blip doesn't NAK-and-redeliver every
// event in the stream.
func (s *Service) onEvent(ctx context.Context, subject string, ev *flow.Event) error {
	if ev == nil {
		return nil
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	seq := int64(0)
	if s.store != nil {
		seq, err = s.store.AppendEvent(ctx, CategoryForSubject(subject), ev.Type, ev.DeviceID, at, raw)
		if err != nil {
			s.logf("journal %s: %v (dropped)", subject, err)
			return nil
		}
	}
	s.broadcast(Event{
		Seq: seq, Category: CategoryForSubject(subject), Type: ev.Type,
		DeviceID: ev.DeviceID, At: at, Data: raw,
	})
	return nil
}

// ---- live SSE fan-out --------------------------------------------------------

// AddLive registers a live subscriber and returns its channel + a cancel func
// (also released when ctx is done). category "" = all categories. The channel
// is buffered; a slow consumer that overflows it drops the oldest event (SSE
// is best-effort live; the journal is the durable record for catch-up).
func (s *Service) AddLive(ctx context.Context, category string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.live[ch] = liveFilter{category: category}
	s.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.live[ch]; ok {
				delete(s.live, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
	go func() { <-ctx.Done(); cancel() }()
	return ch, cancel
}

// broadcast pushes ev to every matching live subscriber (non-blocking).
func (s *Service) broadcast(ev Event) {
	s.mu.Lock()
	var chs []chan Event
	for ch, f := range s.live {
		if f.category != "" && f.category != ev.Category {
			continue
		}
		chs = append(chs, ch)
	}
	s.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- ev:
		default: // slow consumer: drop (journal retains it for catch-up)
		}
	}
}

// ---- delivery ----------------------------------------------------------------

// Sweep delivers pending events to every eligible endpoint. It is idempotent:
// the cursor only advances on a 2xx, so re-running it is safe.
func (s *Service) Sweep(ctx context.Context, now time.Time) {
	if s.store == nil {
		return
	}
	eps, err := s.store.ListEndpoints(ctx)
	if err != nil {
		s.logf("sweep: list endpoints: %v", err)
		return
	}
	for i := range eps {
		ep := &eps[i]
		if !ep.Enabled || ep.Status == "failing" {
			continue
		}
		if now.Before(ep.NextRetryAt) {
			continue // still in backoff
		}
		s.sweepEndpoint(ctx, now, ep)
	}
}

// sweepEndpoint delivers the next pending event for one endpoint, or records a
// failure. It delivers at most one event per sweep (deliberate: a bad endpoint
// must not starve the others, and the backoff watermark paces retries).
func (s *Service) sweepEndpoint(ctx context.Context, now time.Time, ep *Endpoint) {
	pending, err := s.store.PendingEvents(ctx, ep.ID, ep.LastSeq, 1)
	if err != nil {
		s.logf("sweep %d: pending: %v", ep.ID, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	ev := pending[0]
	ok, err := s.Deliver(ctx, *ep, ev)
	if ok {
		if _, aerr := s.store.AdvanceCursor(ctx, ep.ID, ev.Seq); aerr != nil {
			s.logf("sweep %d: advance: %v", ep.ID, aerr)
		}
		_ = s.store.RecordAttempt(ctx, ep.ID, 0, now, "ok")
		ep.LastSeq = ev.Seq
		ep.Attempts = 0
		s.logf("delivered %d -> %s (id=%d %s)", ep.ID, ep.URL, ev.Seq, ev.Category)
		return
	}

	reason := "unknown"
	if err != nil {
		reason = err.Error()
	}
	attempts := ep.Attempts + 1
	if attempts >= ep.MaxAttempts {
		// Dead-letter: stop until an operator re-enables or replays.
		_ = s.store.RecordAttempt(ctx, ep.ID, attempts, now.Add(backoffFor(attempts)), "failing")
		s.logf("webhook %d FAILING after %d attempts (id=%d): %s", ep.ID, attempts, ev.Seq, reason)
		return
	}
	backoff := backoffFor(attempts)
	_ = s.store.RecordAttempt(ctx, ep.ID, attempts, now.Add(backoff), "ok")
	s.logf("webhook %d retry %d/%d (id=%d) in %s: %s", ep.ID, attempts, ep.MaxAttempts, ev.Seq, backoff, reason)
}

// Deliver makes one signed HTTP POST for (endpoint, event) and reports whether
// it was accepted (2xx). It is pure with respect to the endpoint's persisted
// state (the caller records the outcome), so it is unit-testable.
func (s *Service) Deliver(ctx context.Context, ep Endpoint, ev Event) (bool, error) {
	env := ev.Envelope()
	body, err := json.Marshal(env)
	if err != nil {
		return false, fmt.Errorf("encode envelope: %w", err)
	}
	at := time.Now().UTC()
	timeout := time.Duration(ep.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignHeader, Sign([]byte(ep.Secret), at, body))
	req.Header.Set(IdHeader, strconv.FormatInt(ev.Seq, 10))
	req.Header.Set(EventHeader, ev.Type)
	req.Header.Set(TimestampHeader, at.Format(time.RFC3339))
	resp, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("post %s: %w", ep.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	return true, nil
}

// backoffFor is the exponential backoff for a given attempt count (capped).
func backoffFor(attempts int) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	d := time.Second * (1 << (attempts - 1)) // 1s, 2s, 4s, 8s, ...
	const cap = 5 * time.Minute
	if d > cap {
		d = cap
	}
	return d
}

func (s *Service) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}
