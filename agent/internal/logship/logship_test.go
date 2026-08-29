package logship

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeLine appends one JSON-lines event to p (the shape JSONLHandler writes).
func writeLine(t *testing.T, p, msg, level string, ts time.Time) {
	t.Helper()
	b, _ := json.Marshal(jsonLine{
		TS:    ts.UTC().Format(time.RFC3339Nano),
		Level: strings.ToUpper(level),
		Msg:   msg,
		Attrs: map[string]any{"device": "dev-1"},
	})
	b = append(b, '\n')
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
}

func TestParseLineAndStableID(t *testing.T) {
	line := `{"ts":"2026-08-25T10:00:00Z","level":"info","msg":"agent ready","attrs":{"device":"dev-1"}}`
	e, ok := ParseLine("dev-1", line)
	if !ok {
		t.Fatalf("parse failed")
	}
	if e.Msg != "agent ready" || e.Level != "INFO" {
		t.Fatalf("bad parse: %+v", e)
	}
	if e.Time != time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("bad time: %v", e.Time)
	}
	if e.Attrs["device"] != "dev-1" {
		t.Fatalf("bad attrs: %v", e.Attrs)
	}
	if IDForLine("dev-1", line) != e.ID {
		t.Fatalf("id mismatch")
	}
	// Content-derived: same line -> same id (the replay-safe dedup key);
	// different device -> different id.
	if IDForLine("dev-1", line) != IDForLine("dev-1", line) {
		t.Fatalf("id not stable")
	}
	if IDForLine("dev-2", line) == e.ID {
		t.Fatalf("id should differ per device")
	}
	// Not a log event: unparseable / missing msg.
	if _, ok := ParseLine("dev-1", "not json"); ok {
		t.Fatalf("expected unparseable")
	}
	if _, ok := ParseLine("dev-1", `{"ts":"2026-08-25T10:00:00Z","level":"info"}`); ok {
		t.Fatalf("expected missing-msg to be rejected")
	}
}

func TestJSONLHandlerAndTee(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.jsonl")
	h, err := NewJSONLFile(p, slog.LevelDebug)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The stderr side is a bytes.Buffer handler (a trivial second sink).
	var buf strings.Builder
	tee := Tee(h, slog.NewTextHandler(&buf, nil))
	log := slog.New(tee).With("device", "dev-1")
	log.Info("agent ready", "reused", false)
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var jl jsonLine
	if err := json.Unmarshal(b, &jl); err != nil {
		t.Fatalf("line not json: %v (%s)", err, b)
	}
	if jl.Msg != "agent ready" || jl.Level != "INFO" {
		t.Fatalf("bad line: %+v", jl)
	}
	if jl.Attrs["device"] != "dev-1" {
		t.Fatalf("group attr missing: %v", jl.Attrs)
	}
	if !strings.Contains(buf.String(), "agent ready") {
		t.Fatalf("tee missed the stderr side: %q", buf.String())
	}
}

// lokiPusher is a throwaway Loki for tests: captures pushed lines per label.
type lokiPusher struct {
	mu     sync.Mutex
	lines  []string
	labels map[string]string
}

func (l *lokiPusher) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/push" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Streams []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(400)
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, s := range req.Streams {
			l.labels = s.Stream
			for _, v := range s.Values {
				l.lines = append(l.lines, v[1])
			}
		}
		w.WriteHeader(204)
	})
}

// TestShipperTailsAndShipsBothSinks is the core W6-1 agent-side proof: new
// lines in the JSONL file are tailed, shipped to Loki (with the device
// label) AND the uplink, and the offset advances so they are not re-shipped.
func TestShipperTailsAndShipsBothSinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.jsonl")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	loki := &lokiPusher{}
	lokiSrv := httptest.NewServer(loki.handler())
	defer lokiSrv.Close()

	var uplinkMu sync.Mutex
	var uplinkBatches [][]string
	uplinkOK := int32(1)
	ship, err := New(Config{
		FilePath:      p,
		DeviceID:      "dev-1",
		Loki:          NewLokiClient(lokiSrv.URL, "dev-1", lokiSrv.Client()),
		FlushInterval: 30 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Logger:        quiet(),
		StartAtEnd:    false,
		Uplink: func(ctx context.Context, entries []Entry) error {
			if atomic.LoadInt32(&uplinkOK) == 0 {
				return errors.New("no stream")
			}
			uplinkMu.Lock()
			batch := make([]string, len(entries))
			for i, e := range entries {
				batch[i] = e.Msg
			}
			uplinkBatches = append(uplinkBatches, batch)
			uplinkMu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ship.Close()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := runShip(ship, ctx)
	defer func() {
		cancel()
		<-runDone
	}()

	// Give the tail loop a moment to come up, then append the lines.
	time.Sleep(50 * time.Millisecond)

	writeLine(t, p, "agent ready", "info", time.Now())
	writeLine(t, p, "command received", "info", time.Now())
	writeLine(t, p, "uplink stream ended; reconnecting", "warn", time.Now())

	// Both sinks must receive all three lines, in order.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		loki.mu.Lock()
		nLoki := len(loki.lines)
		loki.mu.Unlock()
		uplinkMu.Lock()
		nUp := 0
		for _, b := range uplinkBatches {
			nUp += len(b)
		}
		uplinkMu.Unlock()
		if nLoki >= 3 && nUp >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	loki.mu.Lock()
	if len(loki.lines) != 3 {
		t.Fatalf("loki lines = %d (%v), want 3", len(loki.lines), loki.lines)
	}
	if !strings.Contains(loki.lines[0], "agent ready") ||
		!strings.Contains(loki.lines[1], "command received") ||
		!strings.Contains(loki.lines[2], "uplink stream ended") {
		t.Fatalf("loki line order/content: %v", loki.lines)
	}
	if loki.labels["device_id"] != "dev-1" || loki.labels["job"] != "rmmway-agent" {
		t.Fatalf("loki labels = %v", loki.labels)
	}
	loki.mu.Unlock()
	uplinkMu.Lock()
	got := []string{}
	for _, b := range uplinkBatches {
		got = append(got, b...)
	}
	uplinkMu.Unlock()
	if len(got) != 3 || got[0] != "agent ready" || got[2] != "uplink stream ended; reconnecting" {
		t.Fatalf("uplink batches = %v, want 3 ordered lines", got)
	}
	// No re-ship: give it more time, counts must not move.
	before := len(got)
	time.Sleep(120 * time.Millisecond)
	uplinkMu.Lock()
	after := 0
	for _, b := range uplinkBatches {
		after += len(b)
	}
	uplinkMu.Unlock()
	if after != before {
		t.Fatalf("entries re-shipped: before=%d after=%d", before, after)
	}
}

// TestShipperRetriesOnUplinkFailure: a failed uplink push keeps the batch
// queued (offset not advanced); once the stream is back the SAME entries
// ship (and the id is stable, so the server dedups the replay).
func TestShipperRetriesOnUplinkFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.jsonl")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	uplinkOK := int32(0) // start down
	var got [][]string
	var mu sync.Mutex
	ship := NewMust(Config{
		FilePath:      p,
		DeviceID:      "dev-9",
		FlushInterval: 20 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Logger:        quiet(),
		StartAtEnd:    false,
		Uplink: func(ctx context.Context, entries []Entry) error {
			if atomic.LoadInt32(&uplinkOK) == 0 {
				return errors.New("no stream")
			}
			mu.Lock()
			b := make([]string, len(entries))
			for i, e := range entries {
				b[i] = e.ID
			}
			got = append(got, b)
			mu.Unlock()
			return nil
		},
	})
	defer ship.Close()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := runShip(ship, ctx)
	defer func() {
		cancel()
		<-runDone
	}()

	// Line lands while the uplink is down.
	writeLine(t, p, "while down", "info", time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ship.Failures() > 0 && ship.Pending() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ship.Pending() != 1 {
		t.Fatalf("pending = %d, want 1 (failed flush must keep the entry)", ship.Pending())
	}
	if ship.Pushed() != 0 {
		t.Fatalf("pushed = %d, want 0 while down", ship.Pushed())
	}

	// Stream comes back: the SAME entry (same id) ships.
	atomic.StoreInt32(&uplinkOK, 1)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ship.Pushed() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ship.Pushed() != 1 {
		t.Fatalf("pushed = %d, want 1 after recovery", ship.Pushed())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("batches = %v, want one batch of the one retried entry", got)
	}
	// Stability: the id shipped is the content-derived one.
	wantID := IDForLine("dev-9", lastLine(t, p))
	if got[0][0] != wantID {
		t.Fatalf("shipped id %s != content-derived %s (replay would double-store)", got[0][0], wantID)
	}
}

// TestShipperStartAtEndSkipsHistory: a fresh shipper (no offset state) must
// only ship events logged AFTER it started — a restart must not re-ship the
// whole file.
func TestShipperStartAtEndSkipsHistory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.jsonl")
	writeLine(t, p, "old line before shipper", "info", time.Now().Add(-time.Hour))
	var mu sync.Mutex
	var n int
	ship := NewMust(Config{
		FilePath:      p,
		DeviceID:      "dev-1",
		FlushInterval: 20 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Logger:        quiet(),
		StartAtEnd:    true, // default
		Uplink: func(ctx context.Context, entries []Entry) error {
			mu.Lock()
			n += len(entries)
			mu.Unlock()
			return nil
		},
	})
	defer ship.Close()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := runShip(ship, ctx)
	defer func() {
		cancel()
		<-runDone
	}()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	hist := n
	mu.Unlock()
	if hist != 0 {
		t.Fatalf("start-at-end re-shipped %d history lines", hist)
	}
	writeLine(t, p, "new line after shipper", "info", time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := n
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("n = %d, want exactly the 1 new line", n)
	}
}

// TestShipperOffsetPersistsAcrossRestart: the delivered offset state lets a
// restarted shipper resume where it left off (no full re-ship, no gap).
func TestShipperOffsetPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.jsonl")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		FilePath:      p,
		DeviceID:      "dev-1",
		FlushInterval: 20 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Logger:        quiet(),
		StartAtEnd:    false,
	}
	count := func(s *Shipper) int { return int(s.Pushed()) }

	ship1 := NewMust(cfg)
	defer ship1.Close()
	ctx1, cancel1 := context.WithCancel(context.Background())
	runDone1 := runShip(ship1, ctx1)
	writeLine(t, p, "first run line", "info", time.Now())
	waitPushed(t, ship1, 1)
	cancel1()
	<-runDone1 // let the final flush + offset state write settle
	if count(ship1) != 1 {
		t.Fatalf("ship1 pushed = %d, want 1", count(ship1))
	}

	// Restart: the state file exists; nothing new in the file yet.
	ship2 := NewMust(cfg)
	defer ship2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	runDone2 := runShip(ship2, ctx2)
	time.Sleep(120 * time.Millisecond)
	if ship2.Pushed() != 0 {
		t.Fatalf("ship2 re-shipped %d already-delivered entries", ship2.Pushed())
	}
	// A NEW line ships (no gap left behind).
	writeLine(t, p, "second run line", "info", time.Now())
	waitPushed(t, ship2, 1)
	cancel2()
	<-runDone2
}

// TestShipperSkipsUnparseableLines: garbage lines must not wedge the tail
// (they are skipped + counted; real lines after them still ship).
func TestShipperSkipsUnparseableLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.jsonl")
	if err := os.WriteFile(p, []byte("garbage-not-json\n{\"no\":\"msg\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var msgs []string
	ship := NewMust(Config{
		FilePath:      p,
		DeviceID:      "dev-1",
		FlushInterval: 20 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Logger:        quiet(),
		StartAtEnd:    false,
		Uplink: func(ctx context.Context, entries []Entry) error {
			mu.Lock()
			for _, e := range entries {
				msgs = append(msgs, e.Msg)
			}
			mu.Unlock()
			return nil
		},
	})
	defer ship.Close()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := runShip(ship, ctx)
	defer func() {
		cancel()
		<-runDone
	}()
	writeLine(t, p, "after garbage", "info", time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(msgs)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 1 || msgs[0] != "after garbage" {
		t.Fatalf("msgs = %v, want exactly [after garbage]", msgs)
	}
}

// ---- helpers ----------------------------------------------------------------

func NewMust(cfg Config) *Shipper {
	s, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return s
}

func waitPushed(t *testing.T, s *Shipper, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Pushed() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pushed = %d, want >= %d", s.Pushed(), want)
}

func lastLine(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// runShip runs s.Run in a goroutine and returns a channel closed when it
// exits, so tests can join it before TempDir cleanup.
func runShip(s *Shipper, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()
	return done
}
