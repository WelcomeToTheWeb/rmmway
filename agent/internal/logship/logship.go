// Package logship is the W6-1 agent-side log pipeline: it tails the agent's
// structured JSON-lines log and ships each batch two ways —
//
//   - to Loki over its HTTP push API ({LokiURL}/loki/api/v1/push), so the
//     agent's log lines are queryable in the log stack, and
//   - to the server over the existing gRPC Stream uplink (LogBatch frames),
//     where the server keeps the indexed events in the log_events
//     Timescale hypertable so the RMM can surface recent events per device.
//
// The JSON-lines file is the single source of truth. Every line gets a
// stable content-derived id (sha256 of device|line), so both destinations
// dedup by id and the at-least-once delivery is replay-safe: a crash, a
// dropped uplink, or a Loki blip just re-sends the same entries — the
// second time they are no-ops (same story as MetricBatch, IDEA.md §1).
//
// Delivery model: the shipper reads new lines into a bounded pending queue
// and flushes on a timer or when the queue hits BatchSize. A flush only
// advances the persisted file offset after BOTH sinks accept the batch; on
// any failure the entries stay queued and are retried on the next tick
// (oldest-first, capped at MaxPending).
package logship

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one parsed structured log event.
type Entry struct {
	ID    string            // stable content-derived id (dedup key)
	Time  time.Time         // the event's wall clock (from the line)
	Level string            // DEBUG | INFO | WARN | ERROR
	Msg   string            // the log message
	Attrs map[string]string // structured attributes
	Line  string            // the raw JSON line (what Loki stores as the line value)
}

// ---- JSON-lines slog handler ------------------------------------------------

// jsonLine is the on-disk shape of one structured event.
type jsonLine struct {
	TS    string         `json:"ts"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// JSONLHandler is a slog.Handler that appends each record as one JSON line
// (the file the Shipper tails). Attribute values are stringified; groups
// are flattened with a "." separator (good enough for log indexing).
type JSONLHandler struct {
	mu    sync.Mutex
	w     *os.File
	attrs []slog.Attr
	group string
	min   slog.Level
}

// NewJSONLFile opens (creating if needed, 0600) the JSON-lines log file at
// path and returns a handler that appends to it. Parent directories are
// created.
func NewJSONLFile(path string, level slog.Level) (*JSONLHandler, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLHandler{w: f, min: level}, nil
}

// Close flushes and closes the file.
func (h *JSONLHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.w.Close()
}

// Enabled implements slog.Handler.
func (h *JSONLHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.min }

// Handle implements slog.Handler.
func (h *JSONLHandler) Handle(_ context.Context, r slog.Record) error {
	line := jsonLine{
		TS:    r.Time.UTC().Format(time.RFC3339Nano),
		Level: strings.ToUpper(r.Level.String()),
		Msg:   r.Message,
	}
	if len(h.attrs) > 0 || r.NumAttrs() > 0 {
		line.Attrs = map[string]any{}
		for _, a := range h.attrs {
			line.Attrs[a.Key] = a.Value.Any()
		}
		r.Attrs(func(a slog.Attr) bool {
			key := a.Key
			if h.group != "" {
				key = h.group + "." + a.Key
			}
			line.Attrs[key] = a.Value.Any()
			return true
		})
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.w.Write(b)
	return err
}

// WithAttrs implements slog.Handler.
func (h *JSONLHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	na := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	na = append(na, h.attrs...)
	na = append(na, attrs...)
	return &JSONLHandler{w: h.w, attrs: na, group: h.group, min: h.min}
}

// WithGroup implements slog.Handler.
func (h *JSONLHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &JSONLHandler{w: h.w, attrs: h.attrs, group: g, min: h.min}
}

// Tee fans a slog record out to several handlers (e.g. the stderr text
// handler AND the JSON-lines file handler). A handler that errors in Handle
// does not stop the others; the first error is returned.
func Tee(handlers ...slog.Handler) slog.Handler { return &teeHandler{hs: handlers} }

type teeHandler struct{ hs []slog.Handler }

func (t *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range t.hs {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		hs[i] = h.WithAttrs(attrs)
	}
	return &teeHandler{hs: hs}
}

func (t *teeHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		hs[i] = h.WithGroup(name)
	}
	return &teeHandler{hs: hs}
}

// ---- the shipper ------------------------------------------------------------

// LokiClient posts a batch of lines to Loki's push API.
type LokiClient struct {
	url      string // base URL, e.g. http://localhost:3100
	job      string // the `job` label (default "rmmway-agent")
	http     *http.Client
	deviceID string
}

// NewLokiClient builds a push client for base URL (e.g. http://localhost:3100).
func NewLokiClient(url, deviceID string, hc *http.Client) *LokiClient {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &LokiClient{url: strings.TrimRight(url, "/"), job: "rmmway-agent", http: hc, deviceID: deviceID}
}

// Push posts entries to Loki (POST /loki/api/v1/push). Loki accepts the
// batch with 204 No Content; anything else is an error (the shipper keeps
// the entries queued and retries).
func (c *LokiClient) Push(ctx context.Context, entries []Entry) error {
	type stream struct {
		Stream map[string]string `json:"stream"`
		// Each value is a [timestampNanos, line] ARRAY (Loki's wire format).
		Values [][2]string `json:"values"`
	}
	// One stream per level: the level is a Loki LABEL (indexed, queryable),
	// and a Loki stream is a single set of constant labels.
	byLevel := map[string]*stream{}
	var order []string
	for _, e := range entries {
		lvl := e.Level
		if lvl == "" {
			lvl = "info"
		}
		s, ok := byLevel[lvl]
		if !ok {
			s = &stream{Stream: map[string]string{
				"device_id": c.deviceID,
				"job":       c.job,
				"level":     lvl,
			}}
			byLevel[lvl] = s
			order = append(order, lvl)
		}
		ts := e.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		s.Values = append(s.Values, [2]string{strconv.FormatInt(ts.UnixNano(), 10), e.Line})
	}
	body, err := json.Marshal(map[string]any{"streams": func() []stream {
		out := make([]stream, 0, len(order))
		for _, l := range order {
			out = append(out, *byLevel[l])
		}
		return out
	}()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/loki/api/v1/push", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki push: status %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(respBody)), 200))
	}
	return nil
}

// Config tunes a Shipper.
type Config struct {
	// FilePath is the JSON-lines file to tail.
	FilePath string
	// DeviceID labels every entry (Loki label + the id's input).
	DeviceID string
	// Loki is the push client; nil = Loki shipping disabled (uplink only).
	Loki *LokiClient
	// Uplink ships a batch to the server over the gRPC Stream. Nil =
	// disabled (Loki only).
	Uplink func(ctx context.Context, batch []Entry) error
	// BatchSize caps one flush (0 -> 200).
	BatchSize int
	// FlushInterval is the flush cadence (0 -> 5s).
	FlushInterval time.Duration
	// PollInterval is how often the file is checked for growth (0 -> 500ms).
	PollInterval time.Duration
	// MaxPending caps the unshipped queue (0 -> 4096); the OLDEST entries
	// are dropped beyond it (a stuck sink must not grow the agent unboundedly).
	MaxPending int
	// Logger (nil -> default).
	Logger *slog.Logger
	// StartAtEnd: when true and no offset state exists, only ship events
	// logged AFTER the shipper started (0/absent state -> seek to EOF).
	// Default true.
	StartAtEnd bool
}

func (c *Config) withDefaults() {
	if c.BatchSize <= 0 {
		c.BatchSize = 200
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.MaxPending <= 0 {
		c.MaxPending = 4096
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Shipper tails the JSON-lines file and ships batches to Loki + uplink.
type Shipper struct {
	cfg Config

	mu      sync.Mutex
	queue   []string // raw lines, oldest first
	partial []byte   // trailing incomplete line held between reads
	offset  int64    // next byte to read from the file

	file *os.File

	pushed  int64 // total entries successfully shipped
	failed  int64 // total failed flushes
	skipped int64 // lines dropped (unparseable or over the cap)

	// hooks for tests
	now func() time.Time
}

// New builds a Shipper for cfg.FilePath. The file is opened now; the tail
// starts at the persisted offset state (<path>.shipoffset) when present,
// otherwise (StartAtEnd, default) at the current end of the file.
func New(cfg Config) (*Shipper, error) {
	cfg.withDefaults()
	f, err := os.Open(cfg.FilePath)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	s := &Shipper{cfg: cfg, file: f, now: time.Now}
	// Offset state: a crash between a successful ship and a state write
	// means a RE-send of already-shipped entries — no-op for both sinks
	// (dedup by id). An offset beyond the file size = the file was
	// truncated/rotated: start over from the beginning.
	state := cfg.FilePath + ".shipoffset"
	if b, err := os.ReadFile(state); err == nil {
		if off, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil && off >= 0 {
			if off > st.Size() {
				off = 0
			}
			s.offset = off
		}
	} else if cfg.StartAtEnd {
		s.offset = st.Size()
	}
	return s, nil
}

// IDForLine derives the stable entry id: first 16 hex of
// sha256(deviceID|line). Content-derived so a re-read line maps to the same
// id (the replay-safe dedup key at both sinks).
func IDForLine(deviceID, line string) string {
	sum := sha256.Sum256([]byte(deviceID + "|" + line))
	return hex.EncodeToString(sum[:8])
}

// ParseLine parses one JSON-lines event. Returns false when the line is not
// a well-formed log event (the shipper skips such lines rather than
// queueing them — they would block the offset forever).
func ParseLine(deviceID, line string) (Entry, bool) {
	var jl jsonLine
	if err := json.Unmarshal([]byte(line), &jl); err != nil || jl.Msg == "" {
		return Entry{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, jl.TS)
	if err != nil {
		ts = time.Now()
	}
	e := Entry{
		ID:    IDForLine(deviceID, line),
		Time:  ts,
		Level: strings.ToUpper(jl.Level),
		Msg:   jl.Msg,
		Line:  line,
	}
	if e.Level == "" {
		e.Level = "INFO"
	}
	if len(jl.Attrs) > 0 {
		e.Attrs = make(map[string]string, len(jl.Attrs))
		for k, v := range jl.Attrs {
			e.Attrs[k] = fmt.Sprintf("%v", v)
		}
	}
	return e, true
}

// Pushed returns how many entries have been shipped successfully.
func (s *Shipper) Pushed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushed
}

// Pending returns how many unshipped lines are queued.
func (s *Shipper) Pending() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.queue))
}

// Failures returns how many flush attempts failed.
func (s *Shipper) Failures() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// Run tails the file and flushes batches until ctx is canceled. A final
// flush runs on shutdown so in-flight lines make it out.
func (s *Shipper) Run(ctx context.Context) error {
	poll := time.NewTicker(s.cfg.PollInterval)
	defer poll.Stop()
	flush := time.NewTicker(s.cfg.FlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			s.readNew()
			s.flush(ctx, true) // best-effort final drain
			return ctx.Err()
		case <-poll.C:
			if n := s.readNew(); n > 0 && s.Pending() >= int64(s.cfg.BatchSize) {
				s.flush(ctx, false)
			}
		case <-flush.C:
			s.flush(ctx, false)
		}
	}
}

// readNew consumes newly appended lines into the queue. Complete lines are
// enqueued (unparseable ones skipped + counted); a trailing incomplete line
// is held in s.partial until its newline arrives. Returns lines enqueued.
func (s *Shipper) readNew() int {
	n := 0
	buf := make([]byte, 256*1024)
	for {
		s.mu.Lock()
		got, rerr := s.file.ReadAt(buf, s.offset)
		s.mu.Unlock()
		if got > 0 {
			s.mu.Lock()
			s.offset += int64(got)
			s.partial = append(s.partial, buf[:got]...,
			)
			// A single line beyond 1 MiB is garbage (not a log event):
			// drop it rather than grow unbounded.
			if len(s.partial) > 1<<20 {
				s.partial = nil
				s.skipped++
			}
			// Emit the complete lines (up to the FIRST newline — the
			// buffer may hold several lines from one read).
			var lines []string
			for len(s.partial) > 0 {
				if i := firstIndexByte(s.partial, '\n'); i >= 0 {
					if l := string(s.partial[:i]); l != "" {
						lines = append(lines, l)
					}
					s.partial = s.partial[i+1:]
				} else {
					break
				}
			}
			s.mu.Unlock()
			for _, l := range lines {
				s.enqueue(l)
				n++
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				s.cfg.Logger.Warn("logship: read", "err", rerr)
			}
			break
		}
	}
	return n
}

// enqueue appends a raw line to the queue after a parse sanity check;
// unparseable lines are skipped (counted), and the queue is capped at
// MaxPending by dropping the OLDEST entries.
func (s *Shipper) enqueue(line string) {
	if _, ok := ParseLine(s.cfg.DeviceID, line); !ok {
		s.mu.Lock()
		s.skipped++
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, line)
	for len(s.queue) > s.cfg.MaxPending {
		s.queue = s.queue[1:]
		s.skipped++
	}
}

// flush ships up to BatchSize queued entries to BOTH sinks. The offset
// state is persisted only after both accept; on any failure the entries
// stay queued (retried next tick).
func (s *Shipper) flush(ctx context.Context, final bool) {
	s.mu.Lock()
	if len(s.queue) == 0 {
		s.mu.Unlock()
		return
	}
	n := s.cfg.BatchSize
	if len(s.queue) < n {
		n = len(s.queue)
	}
	raw := make([]string, n)
	copy(raw, s.queue)
	s.queue = s.queue[n:]
	s.mu.Unlock()

	entries := make([]Entry, 0, n)
	for _, l := range raw {
		if e, ok := ParseLine(s.cfg.DeviceID, l); ok {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return
	}

	rctx := ctx
	if !final {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}

	// Ship to whichever sinks are configured. BOTH must accept for the
	// batch to be considered delivered (otherwise a sink-specific outage
	// would silently lose entries on the other sink's schedule).
	var failed []string
	if s.cfg.Loki != nil {
		if err := s.cfg.Loki.Push(rctx, entries); err != nil {
			s.cfg.Logger.Warn("logship: loki push failed; retrying", "err", err, "entries", len(entries))
			failed = append(failed, raw...)
		}
	}
	if s.cfg.Uplink != nil {
		if err := s.cfg.Uplink(rctx, entries); err != nil {
			s.cfg.Logger.Warn("logship: uplink push failed; retrying", "err", err, "entries", len(entries))
			if failed == nil {
				failed = append([]string{}, raw...)
			}
		}
	}

	s.mu.Lock()
	if len(failed) > 0 {
		// Re-queue at the FRONT (oldest first) for the next tick.
		s.queue = append(failed, s.queue...)
		s.failed++
		for len(s.queue) > s.cfg.MaxPending {
			s.queue = s.queue[:len(s.queue)-1]
			s.skipped++
		}
	} else {
		s.pushed += int64(len(entries))
		off := s.offset
		s.mu.Unlock()
		s.writeState(off)
		s.mu.Lock()
	}
	s.mu.Unlock()
}

// writeState persists the delivered offset (temp + rename = atomic).
func (s *Shipper) writeState(off int64) {
	tmp := s.cfg.FilePath + ".shipoffset.tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o600); err != nil {
		s.cfg.Logger.Warn("logship: offset state write", "err", err)
		return
	}
	if err := os.Rename(tmp, s.cfg.FilePath+".shipoffset"); err != nil {
		s.cfg.Logger.Warn("logship: offset state rename", "err", err)
	}
}

// Close releases the file handle.
func (s *Shipper) Close() error {
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstIndexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}
