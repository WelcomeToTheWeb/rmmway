//go:build linux

package osevent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// journalReader tails the systemd journal via `journalctl` (exec-based, so
// the static-binary property is preserved — no cgo sd-journal dependency).
type journalReader struct{}

// defaultReader selects the platform's OS event log reader.
func defaultReader() (Reader, error) { return &journalReader{}, nil }

func (j *journalReader) Name() string { return "systemd-journal" }

// journalLine is one `journalctl --output=json` record (the fields we use).
type journalLine struct {
	RealtimeTimestamp string `json:"__REALTIME_TIMESTAMP"` // microseconds
	Priority          string `json:"PRIORITY"`             // syslog 0–7
	SyslogIdentifier  string `json:"SYSLOG_IDENTIFIER"`
	Unit              string `json:"_SYSTEMD_UNIT"`
	PID               string `json:"_PID"`
	Message           string `json:"MESSAGE"`
}

// Read returns the most recent `limit` journal entries (exec-based).
func (j *journalReader) Read(ctx context.Context, limit int) ([]*agentv1.LogEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl",
		"-n", strconv.Itoa(limit), "--output=json", "--no-pager", "-q", "-r").Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return parseJournalJSON(string(out))
}

// parseJournalJSON maps `journalctl --output=json` lines (newest first) to
// LogEntry. Lines without a usable timestamp are skipped (nothing to order
// or dedup them by).
func parseJournalJSON(out string) ([]*agentv1.LogEntry, error) {
	var entries []*agentv1.LogEntry
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var jl journalLine
		if err := json.Unmarshal([]byte(line), &jl); err != nil {
			continue
		}
		var tsMs int64
		if v, err := strconv.ParseInt(jl.RealtimeTimestamp, 10, 64); err == nil {
			tsMs = v / 1000 // µs → ms
		}
		if tsMs == 0 {
			continue
		}
		attrs := map[string]string{}
		if jl.SyslogIdentifier != "" {
			attrs["identifier"] = jl.SyslogIdentifier
		}
		if jl.Unit != "" {
			attrs["unit"] = jl.Unit
		}
		if jl.PID != "" {
			attrs["pid"] = jl.PID
		}
		entries = append(entries, &agentv1.LogEntry{
			Id:          entryID("journal", intToStr(tsMs), jl.SyslogIdentifier, jl.Message),
			TimestampMs: tsMs,
			Level:       journalPriorityToLevel(jl.Priority),
			Msg:         jl.Message,
			Attrs:       attrs,
		})
	}
	return entries, nil
}

// journalPriorityToLevel maps syslog priorities to the wire levels:
// 0–3 (emerg..err) → ERROR, 4 (warning) → WARN, 7 (debug) → DEBUG,
// 5–6 (notice/info) → INFO.
func journalPriorityToLevel(p string) string {
	n, err := strconv.Atoi(p)
	if err != nil {
		return "INFO"
	}
	switch {
	case n <= 3:
		return "ERROR"
	case n == 4:
		return "WARN"
	case n == 7:
		return "DEBUG"
	default:
		return "INFO"
	}
}
