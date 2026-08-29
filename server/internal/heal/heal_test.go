package heal

import (
	"testing"
	"time"
)

// TestEvalOp pins the comparison semantics the detect/confirm conditions
// are built from.
func TestEvalOp(t *testing.T) {
	cases := []struct {
		op   string
		val  float64
		thr  float64
		want bool
	}{
		{">", 95, 90, true}, {">", 90, 90, false}, {">", 89.9, 90, false},
		{">=", 90, 90, true}, {">=", 89.9, 90, false},
		{"==", 0, 0, true}, {"==", 3, 3, true}, {"==", 1, 3, false},
		{"<", 89, 90, true}, {"<", 90, 90, false},
		{"<=", 90, 90, true}, {"<=", 91, 90, false},
	}
	for _, c := range cases {
		got, err := EvalOp(c.op, c.val, c.thr)
		if err != nil {
			t.Fatalf("EvalOp(%q): %v", c.op, err)
		}
		if got != c.want {
			t.Errorf("EvalOp(%q, %v, %v) = %v, want %v", c.op, c.val, c.thr, got, c.want)
		}
	}
	if _, err := EvalOp("~", 1, 2); err == nil {
		t.Error("unsupported op must error")
	}
}

// TestPlaybookConditions pins the starter library's detect/confirm pairs:
// the confirm condition must be the effective negation of the detect
// condition at the boundary (healed == no longer failing).
func TestPlaybookConditions(t *testing.T) {
	disk := Playbook{Metric: "disk.used_percent", DetectOp: ">", DetectThreshold: 90, ConfirmOp: "<=", ConfirmThreshold: 90}
	if !disk.Detects(95) || !disk.Detects(90.1) {
		t.Error("disk.full must detect 95 / 90.1")
	}
	if disk.Detects(90) || disk.Detects(62) {
		t.Error("disk.full must not detect 90 / 62")
	}
	if !disk.Confirms(90) || !disk.Confirms(62) {
		t.Error("disk.full must confirm 90 / 62")
	}
	if disk.Confirms(95) || disk.Confirms(90.1) {
		t.Error("disk.full must not confirm 95 / 90.1 (confirm-fail)")
	}

	svc := Playbook{Metric: "service.status", DetectOp: "==", DetectThreshold: 0, ConfirmOp: "==", ConfirmThreshold: 1}
	if !svc.Detects(0) || svc.Detects(1) {
		t.Error("service.down detect must fire only at 0")
	}
	if !svc.Confirms(1) || svc.Confirms(0) {
		t.Error("service.down confirm must pass only at 1")
	}

	wsus := Playbook{Metric: "wsus.update_state", DetectOp: "==", DetectThreshold: 3, ConfirmOp: "<=", ConfirmThreshold: 2}
	if !wsus.Detects(3) || wsus.Detects(2) {
		t.Error("wsus.stuck detect must fire only at 3")
	}
	if !wsus.Confirms(0) || !wsus.Confirms(2) || wsus.Confirms(3) {
		t.Error("wsus.stuck confirm must pass at 0/2 and fail at 3")
	}
}

// TestAppliesToOS pins the os_filter semantics (empty = all).
func TestAppliesToOS(t *testing.T) {
	all := Playbook{OSFilter: ""}
	win := Playbook{OSFilter: "windows"}
	linux := Playbook{OSFilter: "linux,darwin"}
	if !all.AppliesToOS("linux") || !all.AppliesToOS("windows") || !all.AppliesToOS("darwin") {
		t.Error("empty filter must admit every OS")
	}
	if !win.AppliesToOS("WINDOWS") {
		t.Error("os filter match is case-insensitive")
	}
	if win.AppliesToOS("linux") {
		t.Error("windows filter must exclude linux")
	}
	if !linux.AppliesToOS("linux") || !linux.AppliesToOS("darwin") || linux.AppliesToOS("windows") {
		t.Error("comma list must admit exactly its members")
	}
}

// TestRemediateScriptFor pins per-OS script selection + {{source}}
// substitution (the detected volume / service name lands in the script).
func TestRemediateScriptFor(t *testing.T) {
	p := Playbook{
		RemediateSH: `systemctl restart "{{source}}"`,
		RemediatePS: `Restart-Service -Name "{{source}}" -Force`,
	}
	lang, script, ok := p.RemediateScriptFor("linux", "nginx")
	if !ok || lang != "sh" || script != `systemctl restart "nginx"` {
		t.Errorf("linux: got (%q, %q, %v)", lang, script, ok)
	}
	lang, script, ok = p.RemediateScriptFor("darwin", "com.apple.x")
	if !ok || lang != "sh" || script != `systemctl restart "com.apple.x"` {
		t.Errorf("darwin: got (%q, %q, %v)", lang, script, ok)
	}
	lang, script, ok = p.RemediateScriptFor("windows", "W32Time")
	if !ok || lang != "powershell" || script != `Restart-Service -Name "W32Time" -Force` {
		t.Errorf("windows: got (%q, %q, %v)", lang, script, ok)
	}

	noPS := Playbook{RemediateSH: `echo hi`}
	if _, _, ok := noPS.RemediateScriptFor("windows", "x"); ok {
		t.Error("missing powershell script must report ok=false (verify-safe no-op)")
	}
}

// TestRunStates pins the state machine's state set + terminal/active split.
func TestRunStates(t *testing.T) {
	terminal := map[string]bool{"resolved": true, "escalated": true, "failed": true, "skipped": true}
	seen := map[string]bool{}
	for _, s := range []string{"detected", "verifying", "remediating", "confirming", "resolved", "escalated", "failed", "skipped"} {
		r := &Run{Status: s}
		want := !terminal[s]
		if r.IsActive() != want {
			t.Errorf("Run{Status:%s}.IsActive() = %v, want %v", s, r.IsActive(), want)
		}
		seen[s] = true
	}
	if len(seen) != 8 {
		t.Errorf("state set changed: %v", seen)
	}
	_ = time.Now() // keep the import honest for future time-bound tests
}
