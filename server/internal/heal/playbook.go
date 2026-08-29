// Package heal is the W5-1 self-healing playbook engine.
//
// A playbook is a declarative rule:
//
//	detect      — the device's LATEST sample of a metric satisfies a
//	              threshold condition (freshness + os filter are the
//	              verify-safe guards: no acting on stale data or on a
//	              platform the playbook can't remediate).
//	verify-safe — cooldown since the last ACTUAL remediation, no active
//	              run for this (playbook, device, source), device online.
//	remediate   — dispatch a per-OS script (RunScript + W3-3 capability
//	              token) to the device's agent.
//	confirm     — RE-measure the metric from a sample strictly after the
//	              remediation was dispatched. This is what makes it
//	              self-HEALING rather than self-acting: a run only counts
//	              as resolved when the measurement says so.
//	escalate    — on confirm-fail (or a failed/refused/timed-out
//	              remediation) the run becomes a ticket (its DB row) and a
//	              Notifier is invoked (W6-2 wires the NATS/webhook one).
//
// The engine is a deterministic state machine over Postgres: every stage
// is a conditional UPDATE (WHERE status = <from>) + an append to
// heal_events, so re-applying a transition is a no-op and a restart mid-run
// resumes from the last persisted stage. One active run per
// (playbook, device, source) is enforced by a partial unique index —
// double-remediation is impossible at the database layer.
package heal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Playbook is one self-healing rule (row in the playbooks table).
type Playbook struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// Detect rule: latest sample of (Metric, Source) must satisfy the op.
	// Source "" = any source (the detected source drives the run).
	Metric          string  `json:"metric"`
	Source          string  `json:"source"`
	DetectOp        string  `json:"detect_op"`
	DetectThreshold float64 `json:"detect_threshold"`

	// Safety guards.
	OSFilter     string `json:"os_filter"` // "" = all; comma list of linux|windows|darwin
	FreshWithinS int    `json:"fresh_within_seconds"`
	CooldownS    int    `json:"cooldown_seconds"`

	// Remediate: per-OS scripts; {{source}} is substituted per run.
	RemediateSH string `json:"remediate_sh"`
	RemediatePS string `json:"remediate_powershell"`

	// Confirm rule: the post-remediation re-measurement must satisfy the op.
	ConfirmOp        string  `json:"confirm_op"`
	ConfirmThreshold float64 `json:"confirm_threshold"`

	RemediateTimeoutS int `json:"remediate_timeout_seconds"`
	ConfirmWaitS      int `json:"confirm_wait_seconds"`

	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EvalOp evaluates one comparison. Supported ops: > >= == < <=.
func EvalOp(op string, val, threshold float64) (bool, error) {
	switch op {
	case ">":
		return val > threshold, nil
	case ">=":
		return val >= threshold, nil
	case "==":
		return val == threshold, nil
	case "<":
		return val < threshold, nil
	case "<=":
		return val <= threshold, nil
	default:
		return false, fmt.Errorf("unsupported comparison %q (want >, >=, ==, <, <=)", op)
	}
}

// Detects reports whether a measurement satisfies the detect condition.
func (p *Playbook) Detects(v float64) bool {
	ok, err := EvalOp(p.DetectOp, v, p.DetectThreshold)
	if err != nil {
		return false
	}
	return ok
}

// Confirms reports whether a re-measurement satisfies the confirm condition
// (i.e. the remediation WORKED).
func (p *Playbook) Confirms(v float64) bool {
	ok, err := EvalOp(p.ConfirmOp, v, p.ConfirmThreshold)
	if err != nil {
		return false
	}
	return ok
}

// DescribeCondition renders "value OP threshold" for run reasons/logs.
func (p *Playbook) DescribeCondition(v float64) string {
	return fmt.Sprintf("%s %s %s", p.Metric, p.DetectOp, strconv.FormatFloat(p.DetectThreshold, 'g', -1, 64))
}

// AppliesToOS reports whether the playbook's os_filter admits the device.
// An empty filter admits every OS.
func (p *Playbook) AppliesToOS(os string) bool {
	if strings.TrimSpace(p.OSFilter) == "" {
		return true
	}
	os = strings.ToLower(strings.TrimSpace(os))
	for _, want := range strings.Split(p.OSFilter, ",") {
		if strings.ToLower(strings.TrimSpace(want)) == os {
			return true
		}
	}
	return false
}

// RemediateScriptFor picks the script for the device OS, substitutes the
// {{source}} placeholder with the detected source, and reports (lang,
// script, ok). ok is false when the playbook ships no script for that OS.
func (p *Playbook) RemediateScriptFor(os, source string) (lang, script string, ok bool) {
	os = strings.ToLower(strings.TrimSpace(os))
	var body string
	switch os {
	case "windows":
		lang, body = "powershell", p.RemediatePS
	default: // linux, darwin, anything unix-ish: the sh script
		lang, body = "sh", p.RemediateSH
	}
	if strings.TrimSpace(body) == "" {
		return "", "", false
	}
	script = strings.ReplaceAll(body, "{{source}}", source)
	return lang, script, true
}

// Detect is one failing series found by the detect stage: the device's
// latest fresh sample of a playbook's metric satisfies the detect
// condition. The engine decides (and records) what happens to it.
type Detect struct {
	DeviceID string
	Source   string
	Value    float64
	At       time.Time // ts of the measured sample
	OS       string
	Hostname string
	Online   bool
}

// Run is one remediation attempt (row in heal_runs).
type Run struct {
	ID           int64      `json:"id"`
	PlaybookKey  string     `json:"playbook_key"`
	DeviceID     string     `json:"device_id"`
	Source       string     `json:"source"`
	Status       string     `json:"status"` // detected|verifying|remediating|confirming|resolved|escalated|failed|skipped
	Reason       string     `json:"reason,omitempty"`
	DetectValue  *float64   `json:"detect_value,omitempty"`
	DetectAt     *time.Time `json:"detect_at,omitempty"`
	CommandID    *string    `json:"command_id,omitempty"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	RemediatedAt *time.Time `json:"remediated_at,omitempty"`
	ConfirmValue *float64   `json:"confirm_value,omitempty"`
	ConfirmedAt  *time.Time `json:"confirmed_at,omitempty"`
	EscalatedAt  *time.Time `json:"escalated_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ActiveStates are the non-terminal states of the run state machine.
var ActiveStates = []string{"detected", "verifying", "remediating", "confirming"}

// IsActive reports whether the run is still in flight.
func (r *Run) IsActive() bool {
	for _, s := range ActiveStates {
		if r.Status == s {
			return true
		}
	}
	return false
}
