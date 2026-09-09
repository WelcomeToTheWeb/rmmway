// Package osevent tails the host's OS event log (the systemd journal on
// Linux, the Windows Event Log on Windows, the unified log on macOS) and
// ships it as LogBatch frames on the uplink.
//
// It is a second, independent shipper alongside logship: logship ships the
// agent's own JSON-lines file, while osevent ships OS-originated events
// (service crashes, OOM kills, auth failures, disk errors) that the agent
// never wrote.
//
// Delivery is at-least-once. Every entry's id is derived from its content
// (see entryID), so the server's ON CONFLICT DO NOTHING makes any re-send
// a no-op; a persisted state file (last shipped timestamp + recent ids)
// keeps re-sends minimal across agent restarts. The readers return the
// most recent window of events, so a very long outage can leave a gap at
// the old edge of the window — accepted: the journal/event store itself
// retains the full history for on-box forensics.
package osevent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Reader is one platform's OS event log access.
type Reader interface {
	// Name identifies the source log (shipped as attrs["source"]).
	Name() string
	// Read returns up to limit recent events, mapped to LogEntry with
	// stable content-derived ids. Ordering is not guaranteed; poll sorts
	// before shipping.
	Read(ctx context.Context, limit int) ([]*agentv1.LogEntry, error)
}

// Config wires one event-log tail.
type Config struct {
	// StatePath persists the ship offset. Missing/corrupt file = start
	// fresh (ids make re-sends no-ops on the server).
	StatePath string
	// PollInterval is the poll cadence (default 60s).
	PollInterval time.Duration
	// Limit caps entries per poll (default 50, hard cap 500).
	Limit int
	// Logger receives warn/debug lines (slog.Default when nil).
	Logger *slog.Logger
	// Uplink ships one batch; it should return an error when the stream
	// is not live so poll retries on the next tick.
	Uplink func(ctx context.Context, batch *agentv1.LogBatch) error
	// Reader selects the source (nil = the platform default).
	Reader Reader
}

// Tail is one running event-log shipper.
type Tail struct {
	cfg    Config
	reader Reader
	logger *slog.Logger
}

// New validates the config and selects the reader.
func New(cfg Config) (*Tail, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 50
	}
	if cfg.Limit > 500 {
		cfg.Limit = 500
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Uplink == nil {
		return nil, errors.New("osevent: Config.Uplink is required")
	}
	r := cfg.Reader
	if r == nil {
		var err error
		r, err = defaultReader()
		if err != nil {
			return nil, err
		}
	}
	return &Tail{cfg: cfg, reader: r, logger: cfg.Logger}, nil
}

// SourceName is the reader's log name (for operator-visible startup logs).
func (t *Tail) SourceName() string { return t.reader.Name() }

// Run polls until ctx is canceled (first poll is immediate).
func (t *Tail) Run(ctx context.Context) error {
	t.poll(ctx)
	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			t.poll(ctx)
		}
	}
}

// state is the persisted ship offset.
type state struct {
	LastTS  int64    `json:"last_ts"`
	SeenIDs []string `json:"seen_ids"`
}

// maxSeenIDs bounds the id ring kept for timestamp-boundary dedup.
const maxSeenIDs = 200

func loadState(path string) state {
	var st state
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// saveState writes atomically (tmp + rename) so a crash mid-write cannot
// corrupt the offset.
func saveState(path string, st state) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// poll reads the recent window, filters out already-shipped entries, ships
// the rest oldest-first, and advances the state only after a successful
// send (a failed send retries the same entries next tick; the server
// dedupes by id either way).
func (t *Tail) poll(ctx context.Context) {
	entries, err := t.reader.Read(ctx, t.cfg.Limit)
	if err != nil {
		t.logger.Warn("osevent read", "source", t.reader.Name(), "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	st := loadState(t.cfg.StatePath)
	seen := make(map[string]struct{}, len(st.SeenIDs))
	for _, id := range st.SeenIDs {
		seen[id] = struct{}{}
	}
	fresh := make([]*agentv1.LogEntry, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if _, dup := seen[e.GetId()]; dup {
			continue
		}
		if e.GetTimestampMs() < st.LastTS {
			continue
		}
		attrs := make(map[string]string, len(e.GetAttrs())+1)
		for k, v := range e.GetAttrs() {
			attrs[k] = v
		}
		attrs["source"] = t.reader.Name()
		e.Attrs = attrs
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return
	}
	sort.Slice(fresh, func(i, j int) bool {
		return fresh[i].GetTimestampMs() < fresh[j].GetTimestampMs()
	})

	if err := t.cfg.Uplink(ctx, &agentv1.LogBatch{Entries: fresh}); err != nil {
		t.logger.Warn("osevent ship", "source", t.reader.Name(), "count", len(fresh), "err", err)
		return // state not advanced: retry next poll
	}

	last := fresh[0].GetTimestampMs()
	for _, e := range fresh {
		if e.GetTimestampMs() > last {
			last = e.GetTimestampMs()
		}
		st.SeenIDs = append(st.SeenIDs, e.GetId())
	}
	st.LastTS = last
	if len(st.SeenIDs) > maxSeenIDs {
		st.SeenIDs = st.SeenIDs[len(st.SeenIDs)-maxSeenIDs:]
	}
	if err := saveState(t.cfg.StatePath, st); err != nil {
		// Already shipped; the next poll may re-send (server dedupes).
		t.logger.Warn("osevent state", "err", err)
	}
	t.logger.Debug("osevent shipped", "source", t.reader.Name(), "count", len(fresh))
}

// entryID derives a stable id from the entry's content: re-reads of the
// same OS event (agent restart, overlapping poll windows) produce the same
// id, so the server-side dedup key (device_id, id) absorbs replays.
func entryID(source string, parts ...string) string {
	h := sha256.Sum256([]byte("rmmway-osevent|" + source + "|" + strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

// intToStr keeps the id inputs uniformly string-encoded.
func intToStr(v int64) string { return fmt.Sprint(v) }
