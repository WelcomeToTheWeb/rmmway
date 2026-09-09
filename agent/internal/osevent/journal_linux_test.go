//go:build linux

package osevent

import (
	"testing"
)

const journalJSON = `{"__REALTIME_TIMESTAMP":"1700000000123456","PRIORITY":"3","SYSLOG_IDENTIFIER":"sshd","_SYSTEMD_UNIT":"sshd.service","_PID":"4242","MESSAGE":"Failed password for root"}
{"__REALTIME_TIMESTAMP":"1700000000500000","PRIORITY":"6","SYSLOG_IDENTIFIER":"systemd","MESSAGE":"Started Session 5 of user alice."}
{"PRIORITY":"4","SYSLOG_IDENTIFIER":"notime","MESSAGE":"no timestamp — skipped"}
not-json-at-all
`

func TestParseJournalJSON(t *testing.T) {
	entries, err := parseJournalJSON(journalJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (no-timestamp + garbage skipped), got %d", len(entries))
	}
	e0 := entries[0]
	if e0.GetTimestampMs() != 1700000000123 {
		t.Errorf("ts µs→ms: %d", e0.GetTimestampMs())
	}
	if e0.GetLevel() != "ERROR" {
		t.Errorf("priority 3 → ERROR, got %s", e0.GetLevel())
	}
	if e0.GetAttrs()["unit"] != "sshd.service" || e0.GetAttrs()["pid"] != "4242" {
		t.Errorf("attrs: %v", e0.GetAttrs())
	}
	if e1 := entries[1]; e1.GetLevel() != "INFO" || e1.GetMsg() != "Started Session 5 of user alice." {
		t.Errorf("second entry: %v %q", e1.GetLevel(), e1.GetMsg())
	}
	// Content-derived id: stable across parses.
	if got, _ := parseJournalJSON(journalJSON); got[0].GetId() != e0.GetId() {
		t.Fatal("id must be stable across identical reads")
	}
}

func TestJournalPriorityToLevel(t *testing.T) {
	cases := map[string]string{
		"0": "ERROR", "1": "ERROR", "2": "ERROR", "3": "ERROR",
		"4": "WARN", "5": "INFO", "6": "INFO", "7": "DEBUG",
		"": "INFO", "junk": "INFO",
	}
	for p, want := range cases {
		if got := journalPriorityToLevel(p); got != want {
			t.Errorf("priority %q: %s want %s", p, got, want)
		}
	}
}
