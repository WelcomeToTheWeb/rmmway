//go:build windows

package osevent

import "testing"

const wevtText = `Log Name: System
Source: Service Control Manager
Date: 2026-09-09T00:00:00.1234567Z
Event ID: 7036
Task: 145 (Service Lifecycle)
Level: Information
Opcode: Info
Keyword: 
User: 
Computer: host.example.com
Description: 
The rmmway-agent service was stopped.

Log Name: System
Source: Disk
Date: 2026-09-09T00:01:02Z
Event ID: 7
Task: 
Level: Error
Opcode: 
Keyword: 
User: 
Computer: host.example.com
Description: 
The description for this Event cannot be found.
A second line of the description.
`

func TestParseWevtutilText(t *testing.T) {
	entries, err := parseWevtutilText(wevtText)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 events, got %d", len(entries))
	}
	e0 := entries[0]
	if e0.GetLevel() != "INFO" {
		t.Errorf("Information → INFO, got %s", e0.GetLevel())
	}
	if e0.GetMsg() != "The rmmway-agent service was stopped." {
		t.Errorf("desc: %q", e0.GetMsg())
	}
	if e0.GetAttrs()["event_id"] != "7036" {
		t.Errorf("event_id attr: %v", e0.GetAttrs())
	}
	e1 := entries[1]
	if e1.GetLevel() != "ERROR" {
		t.Errorf("Error → ERROR, got %s", e1.GetLevel())
	}
	if e1.GetMsg() != "The description for this Event cannot be found.\nA second line of the description." {
		t.Errorf("multi-line desc: %q", e1.GetMsg())
	}
	// Stable ids across identical reads.
	if got, _ := parseWevtutilText(wevtText); got[0].GetId() != e0.GetId() {
		t.Fatal("id must be stable across identical reads")
	}
}

func TestWevtLevelToLevel(t *testing.T) {
	cases := map[string]string{
		"Critical": "ERROR", "Error": "ERROR", "Warning": "WARN",
		"Information": "INFO", "Verbose": "DEBUG", "weird": "INFO",
	}
	for in, want := range cases {
		if got := wevtLevelToLevel(in); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
}
