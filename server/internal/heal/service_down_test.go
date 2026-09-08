package heal

// Wave 1 lane A, gap #5: the agent now emits a real service.status family
// (source = service name, 1.0 running / 0.0 stopped). This test proves the
// seeded "service.down" playbook is live end-to-end against exactly that
// wire shape: a stopped service is detected and restarted; the agent's
// post-restart re-measurement (== 1) resolves the run. It complements
// TestHealServiceDownAndWSUS (which checks dispatch scripts + os_filter)
// with the lifecycle details: seeded shape, run-row state, replay safety,
// the FAILED-remediation escalation path, and the audit trail.
//
// New file only — the heal package is lane B's; no existing heal files
// were modified. Live-Postgres, same scratch-DB convention as store_test.go
// (skipped when RMMWAY_TEST_PG_DSN is unset).

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

func TestServiceDownPlaybookLifecycle(t *testing.T) {
	db, _ := scratchPool(t)
	ctx := context.Background()
	st := NewStore(db)
	agent := newFakeAgent()
	notify := &captureNotifier{}
	eng := New(st, agent.remediator, agent.lookup, notify).WithLogger(log.New(io.Discard, "", 0))

	// 1. Seeded shape: the starter library row is exactly what the agent's
	// service.status family + RMMWAY_SERVICES allowlist feed into it.
	pbs, err := st.Playbooks(ctx, true)
	if err != nil {
		t.Fatalf("playbooks: %v", err)
	}
	var down *Playbook
	for i := range pbs {
		if pbs[i].Key == "service.down" {
			down = &pbs[i]
		}
	}
	if down == nil {
		t.Fatal("seeded service.down playbook missing")
	}
	if down.Metric != "service.status" || down.Source != "" ||
		down.DetectOp != "==" || down.DetectThreshold != 0.0 ||
		down.ConfirmOp != "==" || down.ConfirmThreshold != 1.0 ||
		down.OSFilter != "" || down.FreshWithinS != 900 || down.CooldownS != 1800 {
		t.Fatalf("service.down seed shape wrong: %+v", down)
	}
	if !strings.Contains(down.RemediateSH, `systemctl restart "{{source}}"`) ||
		!strings.Contains(down.RemediateSH, `systemctl is-active "{{source}}"`) {
		t.Fatalf("remediate_sh wrong: %q", down.RemediateSH)
	}
	if !strings.Contains(down.RemediatePS, `Restart-Service -Name "{{source}}" -Force`) {
		t.Fatalf("remediate_powershell wrong: %q", down.RemediatePS)
	}

	// 2. Synthetic estate, one facet per device:
	//    devA (linux)   nginx stopped      -> heals (the main path)
	//    devB (linux)   postgresql stopped -> restart FAILS -> escalated
	//    devC (windows) spoolsv stopped    -> powershell dispatch
	t0 := time.Now().UTC()
	cases := []struct {
		id, os, service string
	}{
		{"dev-sd-a", "linux", "nginx"},
		{"dev-sd-b", "linux", "postgresql"},
		{"dev-sd-c", "windows", "spoolsv"},
	}
	for _, c := range cases {
		insertDevice(t, db, c.id, c.os, true)
		insertSample(t, db, c.id, "service.status", c.service, 0, t0.Add(-10*time.Second))
	}

	// 3. Pass 1: every stopped service is detected and dispatched.
	pass := eng.RunOnce(ctx, t0)
	if len(pass.Errors) > 0 {
		t.Fatalf("pass 1 errors: %v", pass.Errors)
	}
	if pass.Detections != 3 || pass.Started != 3 || pass.ActiveRuns != 3 {
		t.Fatalf("pass 1: detections=%d started=%d active=%d, want 3/3/3",
			pass.Detections, pass.Started, pass.ActiveRuns)
	}
	if d := agent.dispatches(); len(d) != 3 {
		t.Fatalf("dispatches: got %d, want 3", len(d))
	}
	byDev := func(id string) (string, string) {
		for _, dd := range agent.dispatches() {
			if dd.DeviceID == id {
				return dd.Lang, dd.Script
			}
		}
		t.Fatalf("no dispatch for %s", id)
		return "", ""
	}
	if lang, script := byDev("dev-sd-a"); lang != "sh" ||
		!strings.Contains(script, `systemctl restart "nginx"`) ||
		!strings.Contains(script, `systemctl is-active "nginx"`) {
		t.Fatalf("devA linux dispatch wrong: lang=%q script=%q", lang, script)
	}
	if lang, script := byDev("dev-sd-c"); lang != "powershell" ||
		!strings.Contains(script, `Restart-Service -Name "spoolsv" -Force`) {
		t.Fatalf("devC windows dispatch wrong: lang=%q script=%q", lang, script)
	}
	runs := runRows(t, db)
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	for _, c := range cases {
		r := findRun(runs, "service.down", c.id)
		if r == nil || r.Status != "remediating" || r.CommandID == nil ||
			r.Source != c.service || r.DetectValue == nil || *r.DetectValue != 0 {
			t.Fatalf("device %s: run = %+v, want remediating source=%s detect=0", c.id, r, c.service)
		}
	}

	// 4. Pass 2 (REPLAY, nothing answered yet): no second run, no second
	// dispatch — the partial unique index holds.
	pass = eng.RunOnce(ctx, t0.Add(30*time.Second))
	if pass.Started != 0 || pass.Skipped != 3 || len(agent.dispatches()) != 3 {
		t.Fatalf("replay: started=%d skipped=%d dispatches=%d, want 0/3/3",
			pass.Started, pass.Skipped, len(agent.dispatches()))
	}

	// 5. The agents answer: devA/devC fixed the service (next sample = 1);
	// devB's restart failed.
	cmdOf := func(deviceID string) string {
		for _, dd := range agent.dispatches() {
			if dd.DeviceID == deviceID {
				return dd.Command
			}
		}
		t.Fatalf("no dispatch for %s", deviceID)
		return ""
	}
	agent.report(cmdOf("dev-sd-a"), agentv1.CommandResult_SUCCEEDED, "")
	agent.report(cmdOf("dev-sd-b"), agentv1.CommandResult_FAILED, "exit status 3: Unit nginx2.service not loaded")
	agent.report(cmdOf("dev-sd-c"), agentv1.CommandResult_SUCCEEDED, "")
	insertSample(t, db, "dev-sd-a", "service.status", "nginx", 1, t0.Add(45*time.Second))
	insertSample(t, db, "dev-sd-c", "service.status", "spoolsv", 1, t0.Add(45*time.Second))

	// 6. Pass 3: devA/devC -> confirming (SUCCEEDED); devB -> escalated
	// (the FAILED command is the ticket) + notify #1.
	pass = eng.RunOnce(ctx, t0.Add(60*time.Second))
	if pass.Escalated != 1 || notify.count() != 1 {
		t.Fatalf("pass 3: escalated=%d notify=%d, want 1/1", pass.Escalated, notify.count())
	}
	runs = runRows(t, db)
	if r := findRun(runs, "service.down", "dev-sd-a"); r.Status != "confirming" || r.RemediatedAt == nil {
		t.Fatalf("devA: %+v, want confirming", r)
	}
	if r := findRun(runs, "service.down", "dev-sd-c"); r.Status != "confirming" {
		t.Fatalf("devC: %+v, want confirming", r)
	}
	var bEsc *Run
	for i := range runs {
		if runs[i].DeviceID == "dev-sd-b" && runs[i].Status == "escalated" {
			bEsc = &runs[i]
		}
	}
	if bEsc == nil || bEsc.EscalatedAt == nil || !strings.Contains(bEsc.Reason, "FAILED") {
		t.Fatalf("devB escalated run missing/wrong: %+v", bEsc)
	}
	if !strings.Contains(notify.reasons[0], "dev-sd-b") {
		t.Fatalf("notify[0] = %q, want devB", notify.reasons[0])
	}

	// 7. Pass 4: the CONFIRM re-measurement resolves both healed devices.
	// devB's still-stopped detect now lands as a cooldown-skip row (its
	// escalated run is terminal, but the last dispatch was < 1800s ago).
	pass = eng.RunOnce(ctx, t0.Add(75*time.Second))
	if pass.Confirmed != 2 {
		t.Fatalf("pass 4: confirmed=%d errors=%v, want 2", pass.Confirmed, pass.Errors)
	}
	if notify.count() != 1 {
		t.Fatalf("pass 4 must not escalate again, notify=%d", notify.count())
	}
	runs = runRows(t, db)
	if r := findRun(runs, "service.down", "dev-sd-a"); r.Status != "resolved" ||
		r.ConfirmValue == nil || *r.ConfirmValue != 1 || r.ConfirmedAt == nil {
		t.Fatalf("devA: %+v, want resolved confirm=1", r)
	}
	if r := findRun(runs, "service.down", "dev-sd-c"); r.Status != "resolved" || *r.ConfirmValue != 1 {
		t.Fatalf("devC: %+v, want resolved confirm=1", r)
	}
	var bSkip *Run
	for i := range runs {
		if runs[i].DeviceID == "dev-sd-b" && runs[i].Status == "skipped" {
			bSkip = &runs[i]
		}
	}
	if bSkip == nil || !strings.Contains(bSkip.Reason, "cooldown") {
		t.Fatalf("devB cooldown skip missing: %+v", bSkip)
	}

	// 8. Audit trail: the healed run's event log is the full state machine.
	var resolvedID int64
	for i := range runs {
		if runs[i].DeviceID == "dev-sd-a" && runs[i].Status == "resolved" {
			resolvedID = runs[i].ID
		}
	}
	events, err := st.Events(ctx, resolvedID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var seq []string
	for _, e := range events {
		seq = append(seq, e.Status)
	}
	want := []string{"detected", "verifying", "remediating", "confirming", "resolved"}
	if len(seq) != len(want) {
		t.Fatalf("event log = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("event log = %v, want %v", seq, want)
		}
	}
}
