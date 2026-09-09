//go:build windows

package osevent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// wevtutilReader tails the Windows System Event Log via `wevtutil qe`
// (exec-based; no cgo event-log dependency).
type wevtutilReader struct{}

// defaultReader selects the platform's OS event log reader.
func defaultReader() (Reader, error) { return &wevtutilReader{}, nil }

func (w *wevtutilReader) Name() string { return "win-event-log" }

// Read returns the most recent `limit` System log events, newest first
// (wevtutil /rd:true).
func (w *wevtutilReader) Read(ctx context.Context, limit int) ([]*agentv1.LogEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wevtutil", "qe", "System",
		"/c:"+strconv.Itoa(limit), "/rd:true", "/f:text").Output()
	if err != nil {
		return nil, fmt.Errorf("wevtutil: %w", err)
	}
	return parseWevtutilText(string(out))
}

// parseWevtutilText maps `wevtutil qe /f:text` output to LogEntry. The text
// format is one block per event, blocks separated by blank lines, fields as
// "Label: value" lines, and the Description field holding the (possibly
// multi-line) remainder of the block.
func parseWevtutilText(out string) ([]*agentv1.LogEntry, error) {
	var entries []*agentv1.LogEntry
	for _, block := range strings.Split(out, "\r\n\r\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var (
			date, source, eventID, level string
			descStart                    int = -1
		)
		lines := strings.Split(block, "\n")
		for i, line := range lines {
			line = strings.TrimRight(line, "\r")
			switch {
			case strings.HasPrefix(line, "Date:"):
				date = strings.TrimSpace(strings.TrimPrefix(line, "Date:"))
			case strings.HasPrefix(line, "Source:"):
				source = strings.TrimSpace(strings.TrimPrefix(line, "Source:"))
			case strings.HasPrefix(line, "Event ID:"):
				eventID = strings.TrimSpace(strings.TrimPrefix(line, "Event ID:"))
			case strings.HasPrefix(line, "Level:"):
				level = strings.TrimSpace(strings.TrimPrefix(line, "Level:"))
			case strings.HasPrefix(line, "Description:"):
				descStart = i
			}
		}
		if date == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, date)
		if err != nil {
			continue // no orderable timestamp: skip
		}
		var desc string
		if descStart >= 0 {
			desc = strings.TrimSpace(strings.Join(lines[descStart+1:], "\n"))
		}
		entries = append(entries, &agentv1.LogEntry{
			Id:          entryID("win", ts.UTC().Format(time.RFC3339Nano), eventID, source, desc),
			TimestampMs: ts.UnixMilli(),
			Level:       wevtLevelToLevel(level),
			Msg:         desc,
			Attrs:       map[string]string{"event_id": eventID, "source": source},
		})
	}
	return entries, nil
}

// wevtLevelToLevel maps wevtutil level names to wire levels.
func wevtLevelToLevel(level string) string {
	switch strings.ToLower(level) {
	case "critical", "error":
		return "ERROR"
	case "warning":
		return "WARN"
	case "verbose", "debug":
		return "DEBUG"
	default: // "information" or unknown
		return "INFO"
	}
}
