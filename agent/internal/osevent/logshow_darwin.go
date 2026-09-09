//go:build darwin

package osevent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// logShowReader tails the macOS unified log via `log show --style json`
// (exec-based; no cgo OSLog dependency).
type logShowReader struct{}

// defaultReader selects the platform's OS event log reader.
func defaultReader() (Reader, error) { return &logShowReader{}, nil }

func (l *logShowReader) Name() string { return "macos-log" }

// logShowWindow is how far back each poll reaches: ~2× the default 60 s
// poll cadence so a slow poll cannot drop a boundary event (the state
// filter dedups the overlap).
const logShowWindow = "2m"

// Read returns the most recent unified-log entries in the look-back window.
func (l *logShowReader) Read(ctx context.Context, limit int) ([]*agentv1.LogEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// `log show` scans the store; allow a long window — this is the most
	// expensive OS probe the agent runs, which is why the poll cadence
	// default (60 s) is generous.
	out, err := exec.CommandContext(ctx, "log", "show", "--style", "json", "--last", logShowWindow).Output()
	if err != nil {
		return nil, fmt.Errorf("log show: %w", err)
	}
	entries, err := parseLogShowJSON(string(out))
	if err != nil {
		return nil, err
	}
	// The unified log is verbose (every process logs defaults); cap at the
	// caller's limit, newest first (log show emits chronological order).
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// logShowEntry is one record of `log show --style json` (the fields we use).
type logShowEntry struct {
	Timestamp string `json:"timestamp"` // "2006-01-02 15:04:05.999999 -0700"
	Level     string `json:"level"`
	Message   string `json:"message"`
	Process   string `json:"process"`
	Subsystem string `json:"subsystem"`
	Category  string `json:"category"`
}

// parseLogShowJSON maps the JSON array from `log show --style json` to
// LogEntry (chronological order, as emitted).
func parseLogShowJSON(out string) ([]*agentv1.LogEntry, error) {
	var raw []logShowEntry
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse log show output: %w", err)
	}
	var entries []*agentv1.LogEntry
	for _, r := range raw {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05.999999 -0700", r.Timestamp, time.Local)
		if err != nil {
			continue
		}
		attrs := map[string]string{}
		if r.Process != "" {
			attrs["process"] = r.Process
		}
		if r.Subsystem != "" {
			attrs["subsystem"] = r.Subsystem
		}
		if r.Category != "" {
			attrs["category"] = r.Category
		}
		entries = append(entries, &agentv1.LogEntry{
			Id:          entryID("oslog", ts.UTC().Format(time.RFC3339Nano), r.Process, r.Message),
			TimestampMs: ts.UnixMilli(),
			Level:       logShowLevelToLevel(r.Level),
			Msg:         r.Message,
			Attrs:       attrs,
		})
	}
	return entries, nil
}

// logShowLevelToLevel maps unified-log levels to wire levels.
func logShowLevelToLevel(level string) string {
	switch level {
	case "ERROR", "FAULT":
		return "ERROR"
	case "NOTICE":
		return "WARN"
	case "DEBUG":
		return "DEBUG"
	default: // DEFAULT, INFO
		return "INFO"
	}
}
