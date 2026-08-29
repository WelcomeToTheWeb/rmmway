package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// LogEvent is one indexed agent log event as served by the RMM
// (GET /{api|admin}/devices/{id}/events).
type LogEvent struct {
	ID          string         `json:"id"`
	Level       string         `json:"level"`
	Msg         string         `json:"msg"`
	Attrs       map[string]any `json:"attrs,omitempty"`
	TimestampMs int64          `json:"timestamp_ms"`
	Time        time.Time      `json:"time"`
}

// LogSink receives log batches keyed by device (the W6-1 ingest path).
type LogSink interface {
	Write(ctx context.Context, deviceID string, batch *agentv1.LogBatch) error
}

// LogEventReader serves recent indexed events for one device, newest first.
// level filters on severity (""); limit caps the page (0 -> 50, max 500).
type LogEventReader interface {
	Recent(ctx context.Context, deviceID string, limit int, level string) ([]LogEvent, error)
}

// normalizeEventLimit clamps a requested page size.
func normalizeEventLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// ---- Postgres log store (log_events hypertable) -----------------------------

// PostgresLogStore implements LogSink + LogEventReader on log_events.
// Writes are idempotent: the entry id is agent-generated and stable, so a
// re-sent batch (reconnect replay) is a no-op (ON CONFLICT DO NOTHING).
type PostgresLogStore struct {
	db *pgxpool.Pool
}

// NewPostgresLogStore builds a store over the log_events hypertable
// (0007_log_events.sql).
func NewPostgresLogStore(db *pgxpool.Pool) *PostgresLogStore {
	return &PostgresLogStore{db: db}
}

// Write implements LogSink: batch insert, one tx, dedup by entry id.
func (s *PostgresLogStore) Write(ctx context.Context, deviceID string, batch *agentv1.LogBatch) error {
	if batch == nil || len(batch.GetEntries()) == 0 {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tx, err := s.db.Begin(tctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(tctx) }()
	for _, e := range batch.GetEntries() {
		if e == nil || e.GetId() == "" {
			continue // no dedup key: skip rather than guess one
		}
		level := strings.ToLower(e.GetLevel())
		if level == "" {
			level = "info"
		}
		// ts is derived from timestamp_ms so it always matches the PK
		// (same derivation rule as the metrics table).
		_, err = tx.Exec(tctx, `
			INSERT INTO log_events (id, device_id, level, msg, attrs, timestamp_ms, ts)
			VALUES ($1, $2, $3, $4, $5, $6::bigint, to_timestamp(($6::bigint) / 1000.0))
			ON CONFLICT DO NOTHING`,
			e.GetId(), deviceID, level, e.GetMsg(),
			labelsJSON(e.GetAttrs()), e.GetTimestampMs())
		if err != nil {
			return fmt.Errorf("insert log event %s: %w", e.GetId(), err)
		}
	}
	return tx.Commit(tctx)
}

// Recent implements LogEventReader: the device's newest events, optional
// level filter.
func (s *PostgresLogStore) Recent(ctx context.Context, deviceID string, limit int, level string) ([]LogEvent, error) {
	limit = normalizeEventLimit(limit)
	q := `
		SELECT id, level, msg, attrs, timestamp_ms, ts
		FROM log_events
		WHERE device_id = $1`
	args := []any{deviceID}
	if lv := strings.ToLower(strings.TrimSpace(level)); lv != "" {
		args = append(args, lv)
		q += fmt.Sprintf(` AND level = $%d`, len(args))
	}
	q += ` ORDER BY ts DESC LIMIT ` + itoa(limit)

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEvent
	for rows.Next() {
		var (
			id  string
			lvl string
			msg string
			att []byte
			ms  int64
			ts  time.Time
		)
		if err := rows.Scan(&id, &lvl, &msg, &att, &ms, &ts); err != nil {
			return nil, err
		}
		out = append(out, LogEvent{
			ID: id, Level: lvl, Msg: msg,
			Attrs: attrsMap(att), TimestampMs: ms, Time: ts,
		})
	}
	return out, rows.Err()
}

func attrsMap(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---- in-memory log store (no-Postgres deployments / tests) ------------------

// MemoryLogStore is a bounded per-device ring of the newest events. It
// implements both LogSink and LogEventReader so the API + ingest work in
// in-memory mode (Postgres down) exactly like metrics do.
type MemoryLogStore struct {
	capPerDevice int

	mu sync.Mutex
	m  map[string][]LogEvent // device_id -> events, NEWEST first
}

// NewMemoryLogStore keeps at most capPerDevice events per device (0 -> 5000).
func NewMemoryLogStore(capPerDevice int) *MemoryLogStore {
	if capPerDevice <= 0 {
		capPerDevice = 5000
	}
	return &MemoryLogStore{capPerDevice: capPerDevice, m: map[string][]LogEvent{}}
}

// Write implements LogSink (dedup by entry id within the kept window).
func (s *MemoryLogStore) Write(_ context.Context, deviceID string, batch *agentv1.LogBatch) error {
	if batch == nil || len(batch.GetEntries()) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.m[deviceID]
	seen := make(map[string]bool, len(cur))
	for _, e := range cur {
		seen[e.ID] = true
	}
	added := make([]LogEvent, 0, len(batch.GetEntries()))
	for _, e := range batch.GetEntries() {
		if e == nil || e.GetId() == "" || seen[e.GetId()] {
			continue
		}
		level := strings.ToLower(e.GetLevel())
		if level == "" {
			level = "info"
		}
		ev := LogEvent{
			ID: e.GetId(), Level: level, Msg: e.GetMsg(),
			Attrs: attrsMap(mustJSON(e.GetAttrs())), TimestampMs: e.GetTimestampMs(),
			Time: time.UnixMilli(e.GetTimestampMs()).UTC(),
		}
		added = append(added, ev)
	}
	// Newest first: added entries may interleave; sort by time desc, id asc
	// (stable tiebreak), then cap.
	merged := append(added, cur...)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].TimestampMs != merged[j].TimestampMs {
			return merged[i].TimestampMs > merged[j].TimestampMs
		}
		return merged[i].ID < merged[j].ID
	})
	if len(merged) > s.capPerDevice {
		merged = merged[:s.capPerDevice]
	}
	s.m[deviceID] = merged
	return nil
}

// Recent implements LogEventReader.
func (s *MemoryLogStore) Recent(_ context.Context, deviceID string, limit int, level string) ([]LogEvent, error) {
	limit = normalizeEventLimit(limit)
	lv := strings.ToLower(strings.TrimSpace(level))
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LogEvent
	for _, e := range s.m[deviceID] {
		if lv != "" && !strings.EqualFold(e.Level, lv) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func mustJSON(m map[string]string) []byte {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}
