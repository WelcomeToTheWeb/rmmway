package osevent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// fakeReader serves a fixed entry list (newest-first, as the OS readers do).
type fakeReader struct {
	name    string
	entries []*agentv1.LogEntry
	err     error
}

func (f *fakeReader) Name() string { return f.name }
func (f *fakeReader) Read(context.Context, int) ([]*agentv1.LogEntry, error) {
	return f.entries, f.err
}

func entry(id string, ts int64, msg string) *agentv1.LogEntry {
	return &agentv1.LogEntry{Id: id, TimestampMs: ts, Level: "INFO", Msg: msg}
}

// shipRecorder records every batch the uplink receives.
type shipRecorder struct {
	batches []*agentv1.LogBatch
	err     error
}

func (s *shipRecorder) fn(ctx context.Context, b *agentv1.LogBatch) error {
	if s.err != nil {
		return s.err
	}
	s.batches = append(s.batches, b)
	return nil
}

func shippedIDs(rec *shipRecorder) []string {
	var ids []string
	for _, b := range rec.batches {
		for _, e := range b.Entries {
			ids = append(ids, e.GetId())
		}
	}
	return ids
}

func TestFirstPollShipsAllOldestFirst(t *testing.T) {
	rec := &shipRecorder{}
	tail, err := New(Config{
		StatePath:    t.TempDir() + "/state",
		PollInterval: time.Hour,
		Reader:       &fakeReader{name: "fake", entries: []*agentv1.LogEntry{entry("c", 3000, "c"), entry("a", 1000, "a"), entry("b", 2000, "b")}},
		Uplink:       rec.fn,
	})
	if err != nil {
		t.Fatal(err)
	}
	tail.poll(context.Background())
	if got := shippedIDs(rec); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("shipped: %v want a,b,c", got)
	}
	// attrs["source"] must be stamped by poll, not the reader.
	if src := rec.batches[0].Entries[0].GetAttrs()["source"]; src != "fake" {
		t.Fatalf("attrs[source]: %q", src)
	}
}

func TestSecondPollShipsNothingNew(t *testing.T) {
	statePath := t.TempDir() + "/state"
	rec := &shipRecorder{}
	entries := []*agentv1.LogEntry{entry("a", 1000, "a")}
	mk := func() *Tail {
		tail, err := New(Config{StatePath: statePath, Reader: &fakeReader{name: "fake", entries: entries}, Uplink: rec.fn})
		if err != nil {
			t.Fatal(err)
		}
		return tail
	}
	mk().poll(context.Background())
	if len(rec.batches) != 1 {
		t.Fatalf("first poll: %d batches", len(rec.batches))
	}
	mk().poll(context.Background())
	if len(rec.batches) != 1 {
		t.Fatalf("second poll must not ship: %d batches total", len(rec.batches))
	}
}

func TestNewEntriesShip(t *testing.T) {
	statePath := t.TempDir() + "/state"
	rec := &shipRecorder{}
	fr := &fakeReader{name: "fake", entries: []*agentv1.LogEntry{entry("a", 1000, "a")}}
	tail, _ := New(Config{StatePath: statePath, Reader: fr, Uplink: rec.fn})
	tail.poll(context.Background())
	// A new event appears (and an older duplicate of the window edge).
	fr.entries = []*agentv1.LogEntry{entry("b", 2000, "b"), entry("a", 1000, "a")}
	tail.poll(context.Background())
	if got := shippedIDs(rec); len(got) != 2 || got[1] != "b" {
		t.Fatalf("shipped: %v want a,b", got)
	}
}

// TestBoundarySameTimestamp: an event at exactly the last shipped
// timestamp is fresh unless its id was already seen.
func TestBoundarySameTimestamp(t *testing.T) {
	statePath := t.TempDir() + "/state"
	rec := &shipRecorder{}
	fr := &fakeReader{name: "fake", entries: []*agentv1.LogEntry{entry("a", 1000, "a")}}
	tail, _ := New(Config{StatePath: statePath, Reader: fr, Uplink: rec.fn})
	tail.poll(context.Background())
	// Same ts, different content → different id → must ship.
	fr.entries = []*agentv1.LogEntry{entry("b", 1000, "b")}
	tail.poll(context.Background())
	if got := shippedIDs(rec); len(got) != 2 || got[1] != "b" {
		t.Fatalf("shipped: %v want a,b", got)
	}
	// Re-reading the original event at the same ts → seen id → no ship.
	fr.entries = []*agentv1.LogEntry{entry("a", 1000, "a")}
	tail.poll(context.Background())
	if len(rec.batches) != 2 {
		t.Fatalf("seen-id boundary event must not re-ship: %d batches", len(rec.batches))
	}
}

func TestShipFailureRetainsState(t *testing.T) {
	statePath := t.TempDir() + "/state"
	rec := &shipRecorder{}
	entries := []*agentv1.LogEntry{entry("a", 1000, "a"), entry("b", 2000, "b")}
	tail, _ := New(Config{StatePath: statePath, Reader: &fakeReader{name: "fake", entries: entries}, Uplink: rec.fn})
	rec.err = errors.New("no live stream")
	tail.poll(context.Background())
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state must not be written on a failed ship: stat err=%v", err)
	}
	// Stream recovers: the same entries re-ship (server dedupes by id).
	rec.err = nil
	tail.poll(context.Background())
	if got := shippedIDs(rec); len(got) != 2 || got[0] != "a" {
		t.Fatalf("re-ship: %v", got)
	}
}

func TestRestartDedups(t *testing.T) {
	statePath := t.TempDir() + "/state"
	rec := &shipRecorder{}
	entries := []*agentv1.LogEntry{entry("a", 1000, "a"), entry("b", 2000, "b")}
	mk := func() *Tail {
		tail, err := New(Config{StatePath: statePath, Reader: &fakeReader{name: "fake", entries: entries}, Uplink: rec.fn})
		if err != nil {
			t.Fatal(err)
		}
		return tail
	}
	mk().poll(context.Background())
	if len(rec.batches) != 1 {
		t.Fatalf("first tail: %d batches", len(rec.batches))
	}
	mk().poll(context.Background()) // a "new" Tail, same state file
	if len(rec.batches) != 1 {
		t.Fatalf("restart must not re-ship: %d batches", len(rec.batches))
	}
}

func TestReaderErrorIsLoggedNotFatal(t *testing.T) {
	rec := &shipRecorder{}
	tail, err := New(Config{
		StatePath: t.TempDir() + "/state",
		Reader:    &fakeReader{name: "fake", err: errors.New("journalctl down")},
		Uplink:    rec.fn,
	})
	if err != nil {
		t.Fatal(err)
	}
	tail.poll(context.Background()) // must not panic / must not ship
	if len(rec.batches) != 0 {
		t.Fatalf("read error must ship nothing: %d batches", len(rec.batches))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tail, err := New(Config{
		StatePath:    t.TempDir() + "/state",
		PollInterval: 10 * time.Millisecond,
		Reader:       &fakeReader{name: "fake", entries: []*agentv1.LogEntry{entry("a", 1000, "a")}},
		Uplink:       (&shipRecorder{}).fn,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tail.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// entryID must be stable for identical content and distinct for different
// content — it is the replay-safe dedup key.
func TestEntryIDStability(t *testing.T) {
	a1 := entryID("journal", "1700000000000", "sshd", "failed")
	a2 := entryID("journal", "1700000000000", "sshd", "failed")
	b := entryID("journal", "1700000000001", "sshd", "failed")
	c := entryID("journal", "1700000000000", "sshd", "ok")
	d := entryID("win", "1700000000000", "sshd", "failed")
	if a1 != a2 {
		t.Fatal("identical content must yield identical ids")
	}
	for _, other := range []string{b, c, d} {
		if other == a1 {
			t.Fatalf("distinct content must yield distinct ids: %s", other)
		}
	}
	if !strings.Contains(a1, "a") && a1 == "" {
		t.Fatal("empty id")
	}
}
