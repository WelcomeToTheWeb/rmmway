// Store is the webhook framework's data layer (Postgres, 0007_webhooks.sql).
//
// Two responsibilities:
//
//   - the endpoint rows (user-defined webhook targets + their delivery
//     state: cursor, retry backoff, dead-letter status);
//   - the append-only event journal (the monotonic `seq` that is both the
//     delivery cursor and the receiver's dedupe key).
//
// Replay-safety is by cursor, not by a delivery log: `last_seq` only ever
// moves forward on a 2xx (AdvanceCursor is a conditional forward UPDATE), so
// the sweep simply re-drives `seq > last_seq` until it succeeds. A replay
// (SetCursor) may move it back to resend a range.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the pool for webhooks + webhook_events.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// DB exposes the pool (tests / e2e).
func (s *Store) DB() *pgxpool.Pool { return s.db }

// ---- endpoints --------------------------------------------------------------

// Endpoint is one user-defined webhook target.
type Endpoint struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Secret      string    `json:"-"` // never serialized by the API
	Categories  []string  `json:"categories"`
	Enabled     bool      `json:"enabled"`
	MaxAttempts int       `json:"max_attempts"`
	TimeoutMS   int       `json:"timeout_ms"`
	LastSeq     int64     `json:"last_seq"`
	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"next_retry_at"`
	Status      string    `json:"status"` // ok | failing
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Public is the Endpoint minus its secret (safe to return from the API).
type Public struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Categories  []string  `json:"categories"`
	Enabled     bool      `json:"enabled"`
	MaxAttempts int       `json:"max_attempts"`
	TimeoutMS   int       `json:"timeout_ms"`
	LastSeq     int64     `json:"last_seq"`
	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"next_retry_at"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (e Endpoint) Public() Public {
	return Public{
		ID: e.ID, Name: e.Name, URL: e.URL, Categories: e.Categories,
		Enabled: e.Enabled, MaxAttempts: e.MaxAttempts, TimeoutMS: e.TimeoutMS,
		LastSeq: e.LastSeq, Attempts: e.Attempts, NextRetryAt: e.NextRetryAt,
		Status: e.Status, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

const endpointCols = `id, name, url, secret, categories, enabled, max_attempts,
	timeout_ms, last_seq, attempts, next_retry_at, status, created_at, updated_at`

func scanEndpoint(row interface{ Scan(...any) error }) (Endpoint, error) {
	var e Endpoint
	if e.Categories == nil {
		e.Categories = []string{}
	}
	err := row.Scan(&e.ID, &e.Name, &e.URL, &e.Secret, &e.Categories, &e.Enabled,
		&e.MaxAttempts, &e.TimeoutMS, &e.LastSeq, &e.Attempts, &e.NextRetryAt,
		&e.Status, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// CreateEndpoint inserts a new endpoint. Its cursor starts at the current
// maximum journal seq so it receives only NEW events (not the whole history);
// pass startSeq <= 0 to default to "now".
func (s *Store) CreateEndpoint(ctx context.Context, name, url, secret string, categories []string, maxAttempts, timeoutMS int, startSeq int64) (*Endpoint, error) {
	if name == "" || url == "" || secret == "" {
		return nil, fmt.Errorf("name, url and secret are required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if timeoutMS <= 0 {
		timeoutMS = 5000
	}
	if startSeq < 0 {
		startSeq = 0
	}
	if startSeq == 0 {
		if mx, err := s.MaxSeq(ctx); err == nil {
			startSeq = mx
		}
	}
	if len(categories) == 0 {
		// Empty = "all categories" (the API normalizes the same way).
		categories = AllCategories
	}
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO webhooks (name, url, secret, categories, max_attempts, timeout_ms, last_seq)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		name, url, secret, categories, maxAttempts, timeoutMS, startSeq).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Endpoint(ctx, id)
}

// ListEndpoints returns every endpoint, oldest first.
func (s *Store) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	rows, err := s.db.Query(ctx, `SELECT `+endpointCols+` FROM webhooks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Endpoint fetches one endpoint by id.
func (s *Store) Endpoint(ctx context.Context, id int64) (*Endpoint, error) {
	e, err := scanEndpoint(s.db.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhooks WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook %d not found", id)
		}
		return nil, err
	}
	return &e, nil
}

// UpdateEndpoint applies a partial update; nil fields are left untouched.
func (s *Store) UpdateEndpoint(ctx context.Context, id int64, name, url *string, categories *[]string, enabled *bool) (*Endpoint, error) {
	sets := []string{"updated_at = now()"}
	args := []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if name != nil {
		add("name", *name)
	}
	if url != nil {
		add("url", *url)
	}
	if categories != nil {
		add("categories", categories)
	}
	if enabled != nil {
		// Re-enabling a dead-lettered endpoint clears its failure state.
		add("enabled", *enabled)
		if *enabled {
			sets = append(sets, "attempts = 0", "status = 'ok'", "next_retry_at = now()")
		}
	}
	if len(sets) == 1 {
		return s.Endpoint(ctx, id)
	}
	args = append(args, id)
	q := `UPDATE webhooks SET ` + joinStr(sets, ", ") + ` WHERE id = $` + fmt.Sprint(len(args)) + ` RETURNING id`
	var rid int64
	if err := s.db.QueryRow(ctx, q, args...).Scan(&rid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("webhook %d not found", id)
		}
		return nil, err
	}
	return s.Endpoint(ctx, rid)
}

// DeleteEndpoint removes an endpoint (its journal events are kept — the
// journal is a global record, not per-endpoint).
func (s *Store) DeleteEndpoint(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM webhooks WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("webhook %d not found", id)
	}
	return nil
}

// ---- journal ----------------------------------------------------------------

// Event is one journaled bus event.
type Event struct {
	Seq      int64           `json:"id"`
	Category string          `json:"category"`
	Type     string          `json:"type"`
	DeviceID string          `json:"device_id,omitempty"`
	At       time.Time       `json:"at"`
	Data     json.RawMessage `json:"event"` // the full flow.Event, lossless
}

// AppendEvent journals one event and returns its new sequence. `data` is the
// full bus event already marshaled to JSON.
func (s *Store) AppendEvent(ctx context.Context, category, typ, deviceID string, at time.Time, data json.RawMessage) (int64, error) {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	var seq int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO webhook_events (category, type, device_id, at, data)
		VALUES ($1, $2, $3, $4, $5) RETURNING seq`,
		category, typ, deviceID, at, data).Scan(&seq)
	return seq, err
}

// MaxSeq returns the highest journal seq (0 when empty).
func (s *Store) MaxSeq(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM webhook_events`).Scan(&seq)
	return seq, err
}

// EventsAfter returns journaled events with seq > after, oldest first,
// limited to limit. category "" = all. Kept for simple callers; use
// EventsAfterFilter for device/type-scoped queries.
func (s *Store) EventsAfter(ctx context.Context, after int64, category string, limit int) ([]Event, error) {
	return s.EventsAfterFilter(ctx, after, Filter{Category: category}, limit)
}

// EventsAfterFilter returns journaled events with seq > after, oldest first,
// limited to limit, matching every set field of fl (category/device/type).
// An empty field matches everything. This is the catch-up / REST query behind
// GET /{api|admin}/events and the SSE stream's initial window.
func (s *Store) EventsAfterFilter(ctx context.Context, after int64, fl Filter, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT seq, category, type, device_id, at, data FROM webhook_events WHERE seq > $1`
	args := []any{after}
	if fl.Category != "" {
		args = append(args, fl.Category)
		q += fmt.Sprintf(` AND category = $%d`, len(args))
	}
	if fl.Device != "" {
		args = append(args, fl.Device)
		q += fmt.Sprintf(` AND device_id = $%d`, len(args))
	}
	if fl.Type != "" {
		args = append(args, fl.Type)
		q += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	args = append(args, limit)
	q += ` ORDER BY seq ASC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Category, &e.Type, &e.DeviceID, &e.At, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingEvents returns the endpoint's undelivered events (seq > last_seq,
// limited to its categories), oldest first. This is what the sweep delivers.
func (s *Store) PendingEvents(ctx context.Context, epID, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(ctx, `
		SELECT seq, category, type, device_id, at, data
		FROM webhook_events e
		WHERE e.seq > $1 AND e.category = ANY(
			SELECT unnest(categories) FROM webhooks WHERE id=$2)
		ORDER BY e.seq ASC LIMIT $3`,
		afterSeq, epID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Category, &e.Type, &e.DeviceID, &e.At, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- delivery state ---------------------------------------------------------

// AdvanceCursor moves the endpoint's cursor forward to seq (only if seq is
// ahead of the current last_seq — a replay of an older event can't move it
// back). Applied is false when the cursor was already >= seq (a replay).
func (s *Store) AdvanceCursor(ctx context.Context, epID, seq int64) (applied bool, err error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE webhooks SET last_seq=$2, updated_at=now()
		WHERE id=$1 AND last_seq < $2`, epID, seq)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetCursor sets the cursor to an arbitrary seq (replay may move it BACK to
// resend). It also clears the failure state so the sweep immediately
// re-drives from the new cursor.
func (s *Store) SetCursor(ctx context.Context, epID, seq int64) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE webhooks SET last_seq=$2, attempts=0, status='ok', next_retry_at=now(), updated_at=now()
		WHERE id=$1`, epID, seq)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("webhook %d not found", epID)
	}
	return nil
}

// RecordAttempt updates the endpoint's consecutive-failure counter, backoff
// watermark, and (optionally) dead-letter status.
func (s *Store) RecordAttempt(ctx context.Context, epID int64, attempts int, nextRetryAt time.Time, status string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE webhooks SET attempts=$2, next_retry_at=$3, status=$4, updated_at=now()
		WHERE id=$1`, epID, attempts, nextRetryAt, status)
	return err
}

// SetNextRetry moves only the backoff watermark (tests / ops re-pacing) without
// touching the attempt counter or status.
func (s *Store) SetNextRetry(ctx context.Context, epID int64, when time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE webhooks SET next_retry_at=$2, updated_at=now() WHERE id=$1`, epID, when)
	return err
}

func joinStr(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
