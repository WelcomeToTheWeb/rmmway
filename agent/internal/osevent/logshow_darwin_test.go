//go:build darwin

package osevent

import "testing"

const logShowJSON = `[
{"timestamp":"2026-09-09 00:00:00.123456 -0700","level":"DEFAULT","message":"agent started","process":"rmmway-agent","subsystem":"rmmway.agent","category":"boot"},
{"timestamp":"2026-09-09 00:00:05.000000 -0700","level":"ERROR","message":"disk read failed","process":"kernel"},
{"timestamp":"2026-09-09 00:00:06.000000 -0700","level":"NOTICE","message":"pressure high","process":"sysmond"},
{"timestamp":"garbage","level":"INFO","message":"skipped"}
]`

func TestParseLogShowJSON(t *testing.T) {
	entries, err := parseLogShowJSON(logShowJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (bad timestamp skipped), got %d", len(entries))
	}
	if entries[0].GetLevel() != "INFO" || entries[1].GetLevel() != "ERROR" || entries[2].GetLevel() != "WARN" {
		t.Fatalf("levels: %s %s %s", entries[0].GetLevel(), entries[1].GetLevel(), entries[2].GetLevel())
	}
	if entries[0].GetAttrs()["process"] != "rmmway-agent" {
		t.Errorf("process attr: %v", entries[0].GetAttrs())
	}
	// Stable ids across identical reads.
	if got, _ := parseLogShowJSON(logShowJSON); got[0].GetId() != entries[0].GetId() {
		t.Fatal("id must be stable across identical reads")
	}
}

func TestLogShowLevelToLevel(t *testing.T) {
	cases := map[string]string{
		"DEFAULT": "INFO", "INFO": "INFO", "NOTICE": "WARN",
		"ERROR": "ERROR", "FAULT": "ERROR", "DEBUG": "DEBUG", "x": "INFO",
	}
	for in, want := range cases {
		if got := logShowLevelToLevel(in); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
}
