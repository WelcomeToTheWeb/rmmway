// Engine drives the playbook state machine.
//
// Each pass (RunOnce) does two things:
//  1. DETECT: for every enabled playbook, find devices whose latest fresh
//     sample fails the detect condition; for each, run the verify-safe
//     checks (cooldown, no active run, device online) and — if safe —
//     dispatch the remediation script (remediate stage).
//  2. ADVANCE: for every in-flight run, move it forward based on persisted
//     state only (command results, post-remediation samples): remediating
//     -> confirming (SUCCEEDED) or escalated (FAILED/REFUSED/TIMED_OUT/
//     no result by timeout); confirming -> resolved (re-measurement
//     satisfies the confirm condition) or escalated (confirm-fail, or no
//     fresh measurement by the confirm deadline).
//
// Nothing here is stateful: every decision reads from Postgres, and every
// state change goes through Store.Transition (conditional UPDATE + event
// append). A pass is therefore idempotent and replay-safe — running it
// twice, or re-running it after a restart, never double-remediates.
package heal

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Remediator dispatches one remediation script to a device and returns the
// command id (production: the ingest Dispatcher, which mints the W3-3
// capability token; tests/e2e: a fake agent).
type Remediator func(ctx context.Context, deviceID, lang, script string) (commandID string, err error)

// ResultLookup returns the recorded result for a command id (production:
// the ingest Dispatcher's in-memory result table).
type ResultLookup func(commandID string) (*agentv1.CommandResult, bool)

// Notifier is the escalation channel: W6-2 wires the NATS/webhook
// notifier; until then the default emits a structured log line. The
// escalated heal_runs row itself is the ticket — the notification is the
// "someone should look" half of DoD "escalate (ticket+notify)".
type Notifier interface {
	Escalate(run *Run, reason string)
}

// LogNotifier is the default Notifier.
type LogNotifier struct{ Log *log.Logger }

func (n LogNotifier) Escalate(r *Run, reason string) {
	l := n.Log
	if l == nil {
		l = log.Default()
	}
	l.Printf("selfheal: ESCALATED run %d playbook=%s device=%s source=%q: %s (ticket=heal_runs.id=%d)",
		r.ID, r.PlaybookKey, r.DeviceID, r.Source, reason, r.ID)
}

// Pass is the outcome of one RunOnce (API/e2e summary).
type Pass struct {
	Detections int      `json:"detections"` // failing series found
	Started    int      `json:"started"`    // new runs dispatched to remediate
	Skipped    int      `json:"skipped"`    // verify-safe refusals (cooldown / offline / active run)
	Confirmed  int      `json:"confirmed"`  // runs resolved (re-measurement passed)
	Escalated  int      `json:"escalated"`  // runs escalated (ticket + notify fired)
	Failed     int      `json:"failed"`     // runs failed (dispatch error / stuck)
	ActiveRuns int      `json:"active_runs"`
	Errors     []string `json:"errors,omitempty"`
}

// Engine is the self-healing playbook engine.
type Engine struct {
	store     *Store
	remediate Remediator
	results   ResultLookup
	notify    Notifier
	log       *log.Logger

	mu       sync.Mutex
	interval time.Duration
}

// New builds an Engine. remediate and results are required; notify defaults
// to LogNotifier.
func New(st *Store, remediate Remediator, results ResultLookup, notify Notifier) *Engine {
	if notify == nil {
		notify = LogNotifier{}
	}
	return &Engine{store: st, remediate: remediate, results: results, notify: notify}
}

// RunOnce performs one full detect + advance pass. Safe to call
// concurrently (two passes interleaving is a no-op by construction —
// transitions are conditional and the active-run slot is DB-unique).
func (e *Engine) RunOnce(ctx context.Context, now time.Time) *Pass {
	pass := &Pass{}

	all, err := e.store.Playbooks(ctx, false)
	if err != nil {
		pass.Errors = append(pass.Errors, "load playbooks: "+err.Error())
		return pass
	}
	pbBykey := make(map[string]*Playbook, len(all))
	var enabled []*Playbook
	for i := range all {
		p := &all[i]
		pbBykey[p.Key] = p
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}

	// ---- stage 1: detect + verify-safe + remediate ----------------------
	for _, p := range enabled {
		dets, err := e.store.Detect(ctx, *p, now)
		if err != nil {
			pass.Errors = append(pass.Errors, fmt.Sprintf("detect %s: %v", p.Key, err))
			continue
		}
		for i := range dets {
			d := dets[i]
			if !p.Detects(d.Value) {
				continue
			}
			pass.Detections++
			outcome, err := e.handleDetection(ctx, p, d, now)
			if err != nil {
				pass.Errors = append(pass.Errors, fmt.Sprintf("%s/%s: %v", p.Key, d.DeviceID, err))
				continue
			}
			switch outcome {
			case "started":
				pass.Started++
			case "skipped":
				pass.Skipped++
			case "failed":
				pass.Failed++
			}
		}
	}

	// ---- stage 2: advance in-flight runs --------------------------------
	active, err := e.store.ActiveRuns(ctx)
	if err != nil {
		pass.Errors = append(pass.Errors, "load active runs: "+err.Error())
	} else {
		sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
		for i := range active {
			p := pbBykey[active[i].PlaybookKey]
			if p == nil {
				if _, err := e.store.Transition(ctx, active[i].ID, active[i].Status, "failed",
					map[string]any{"reason": "playbook deleted while run in flight"}); err != nil {
					pass.Errors = append(pass.Errors, fmt.Sprintf("run %d: %v", active[i].ID, err))
				}
				continue
			}
			adv, err := e.advance(ctx, p, &active[i], now)
			if err != nil {
				pass.Errors = append(pass.Errors, fmt.Sprintf("run %d: %v", active[i].ID, err))
				continue
			}
			switch adv {
			case "resolved":
				pass.Confirmed++
			case "escalated":
				pass.Escalated++
			case "failed":
				pass.Failed++
			}
		}
	}

	if rem, err := e.store.ActiveRuns(ctx); err == nil {
		pass.ActiveRuns = len(rem)
	}
	return pass
}

// handleDetection runs the verify-safe gate + remediation for one failing
// series. Returns "started", "skipped", or "pass" (no row: the playbook
// does not apply to this OS / ships no script for it — a config issue,
// logged not audited).
func (e *Engine) handleDetection(ctx context.Context, p *Playbook, d Detect, now time.Time) (string, error) {
	if !p.AppliesToOS(d.OS) {
		e.logf("selfheal: %s: %s (%s) detected %s=%v but playbook has no %s remediation — no action",
			p.Key, d.Hostname, d.DeviceID, p.Metric, d.Value, d.OS)
		return "pass", nil
	}
	lang, script, ok := p.RemediateScriptFor(d.OS, d.Source)
	if !ok {
		e.logf("selfheal: %s: %s (%s) detected %s=%v but playbook ships no script for %s — no action",
			p.Key, d.Hostname, d.DeviceID, p.Metric, d.Value, d.OS)
		return "pass", nil
	}

	// verify-safe: cooldown since the last ACTUAL remediation.
	cooldown := time.Duration(p.CooldownS) * time.Second
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	if cd, err := e.store.CooldownActive(ctx, p.Key, d.DeviceID, d.Source, now.Add(-cooldown)); err != nil {
		return "", err
	} else if cd {
		r, _, err := e.store.InsertRun(ctx, p.Key, d.DeviceID, d.Source, d.Value, d.At)
		if err != nil {
			return "", err
		}
		if r == nil { // an active run already covers this series
			return "skipped", nil
		}
		if _, err := e.store.Transition(ctx, r.ID, "detected", "skipped",
			map[string]any{"reason": fmt.Sprintf("cooldown: last remediation within %ds", p.CooldownS)}); err != nil {
			return "", err
		}
		return "skipped", nil
	}

	// Start the run. A colliding INSERT (active run) is the replay-safe
	// "already on it" — a no-op skip, never a double remediation.
	r, created, err := e.store.InsertRun(ctx, p.Key, d.DeviceID, d.Source, d.Value, d.At)
	if err != nil {
		return "", err
	}
	if !created {
		return "skipped", nil
	}
	if _, err := e.store.Transition(ctx, r.ID, "detected", "verifying", nil); err != nil {
		return "", err
	}
	if !d.Online {
		if _, err := e.store.Transition(ctx, r.ID, "verifying", "skipped",
			map[string]any{"reason": "device not online at verify time"}); err != nil {
			return "", err
		}
		return "skipped", nil
	}

	// remediate: dispatch the script (W3-3 capability token travels with
	// the command in production dispatch).
	cmdID, err := e.dispatch(ctx, d.DeviceID, lang, script)
	if err != nil {
		if _, err := e.store.Transition(ctx, r.ID, "verifying", "failed",
			map[string]any{"reason": "dispatch failed: " + err.Error()}); err != nil {
			return "", err
		}
		e.logf("selfheal: %s: dispatch to %s failed: %v", p.Key, d.DeviceID, err)
		return "failed", nil
	}
	if _, err := e.store.Transition(ctx, r.ID, "verifying", "remediating", map[string]any{
		"command_id":    cmdID,
		"dispatched_at": now,
	}); err != nil {
		return "", err
	}
	e.logf("selfheal: %s: REMEDIATING %s (%s) source=%q (detected %s=%v) cmd=%s",
		p.Key, d.Hostname, d.DeviceID, d.Source, p.Metric, d.Value, cmdID)
	return "started", nil
}

// advance moves one in-flight run forward from its persisted state.
// Returns the terminal outcome applied this pass ("", "resolved",
// "escalated", "failed") — "" for "still in flight".
func (e *Engine) advance(ctx context.Context, p *Playbook, r *Run, now time.Time) (string, error) {
	// Stuck-run guard: a run older than remediate+confirm deadlines plus
	// margin is wedged (typically a server restart lost the in-memory
	// command result). Mark it failed; the condition is re-detected on a
	// later pass (the cooldown, keyed on dispatched_at, is now burned, so
	// the operator gets one escalated/failed ticket, not a retry storm).
	deadline := time.Duration(p.RemediateTimeoutS+p.ConfirmWaitS+600) * time.Second
	if now.Sub(r.CreatedAt) > deadline && r.CommandID == nil {
		if _, err := e.store.Transition(ctx, r.ID, r.Status, "failed",
			map[string]any{"reason": "run stuck before dispatch (engine restart?)"}); err != nil {
			return "", err
		}
		return "failed", nil
	}

	switch r.Status {
	case "detected", "verifying":
		// Crashed between the insert and the dispatch (or mid-verify).
		// Resume: dispatch if not yet dispatched.
		if r.CommandID != nil {
			return e.advanceRemediating(ctx, p, r, now)
		}
		return e.resumeDispatch(ctx, p, r, now)

	case "remediating":
		return e.advanceRemediating(ctx, p, r, now)

	case "confirming":
		return e.advanceConfirming(ctx, p, r, now)
	}
	return "", nil
}

// resumeDispatch re-dispatches a run that was inserted but never
// dispatched (crash between verify and dispatch). The original script
// choice is recomputed from the current device row via the playbook.
func (e *Engine) resumeDispatch(ctx context.Context, p *Playbook, r *Run, now time.Time) (string, error) {
	var os string
	if err := e.store.DB().QueryRow(ctx, `SELECT os FROM devices WHERE id=$1`, r.DeviceID).Scan(&os); err != nil {
		if _, err := e.store.Transition(ctx, r.ID, r.Status, "failed",
			map[string]any{"reason": "resume: device row gone: " + err.Error()}); err != nil {
			return "", err
		}
		return "failed", nil
	}
	lang, script, ok := p.RemediateScriptFor(os, r.Source)
	if !ok {
		if _, err := e.store.Transition(ctx, r.ID, r.Status, "failed",
			map[string]any{"reason": "resume: no remediation script for os " + os}); err != nil {
			return "", err
		}
		return "failed", nil
	}
	cmdID, err := e.dispatch(ctx, r.DeviceID, lang, script)
	if err != nil {
		if _, err := e.store.Transition(ctx, r.ID, r.Status, "failed",
			map[string]any{"reason": "resume dispatch failed: " + err.Error()}); err != nil {
			return "", err
		}
		return "failed", nil
	}
	if _, err := e.store.Transition(ctx, r.ID, r.Status, "remediating", map[string]any{
		"command_id":    cmdID,
		"dispatched_at": now,
	}); err != nil {
		return "", err
	}
	return "", nil
}

// advanceRemediating: remediating -> confirming (SUCCEEDED) | escalated
// (FAILED/REFUSED/TIMED_OUT/UNSUPPORTED, or no result by the timeout).
func (e *Engine) advanceRemediating(ctx context.Context, p *Playbook, r *Run, now time.Time) (string, error) {
	res, ok := e.results(*r.CommandID)
	if ok && res != nil {
		switch res.GetStatus() {
		case agentv1.CommandResult_SUCCEEDED:
			remediatedAt := now
			if res.GetCompletedAtMs() > 0 {
				remediatedAt = time.UnixMilli(res.GetCompletedAtMs()).UTC()
			}
			if _, err := e.store.Transition(ctx, r.ID, "remediating", "confirming", map[string]any{
				"remediated_at": remediatedAt,
			}); err != nil {
				return "", err
			}
			e.logf("selfheal: run %d: remediation SUCCEEDED — confirming (re-measure %s)", r.ID, p.Metric)
			return "", nil
		case agentv1.CommandResult_FAILED, agentv1.CommandResult_TIMED_OUT,
			agentv1.CommandResult_REFUSED, agentv1.CommandResult_UNSUPPORTED:
			reason := fmt.Sprintf("remediation command %s: %s", res.GetStatus(), firstNonEmpty(res.GetError(), res.GetStderrTail()))
			return e.escalate(ctx, r, reason)
		default: // RECEIVED / RUNNING / STATUS_UNSPECIFIED: still in flight
			return e.checkRemediateTimeout(ctx, p, r, now)
		}
	}
	return e.checkRemediateTimeout(ctx, p, r, now)
}

func (e *Engine) checkRemediateTimeout(ctx context.Context, p *Playbook, r *Run, now time.Time) (string, error) {
	if r.DispatchedAt == nil {
		return "", nil
	}
	if now.Sub(*r.DispatchedAt) > time.Duration(p.RemediateTimeoutS)*time.Second {
		return e.escalate(ctx, r, fmt.Sprintf("remediation command %s: no final result within %ds",
			*r.CommandID, p.RemediateTimeoutS))
	}
	return "", nil
}

// advanceConfirming: the re-measurement. A sample strictly after the
// remediation was dispatched:
//   - satisfies the confirm condition -> resolved (self-HEALED);
//   - violates it -> confirm-FAIL -> escalated (the DoD escalation);
//   - no fresh sample yet: wait until the confirm deadline, then escalate
//     (the metric simply never came back — still a human problem).
func (e *Engine) advanceConfirming(ctx context.Context, p *Playbook, r *Run, now time.Time) (string, error) {
	since := time.Time{}
	if r.DispatchedAt != nil {
		since = *r.DispatchedAt
	}
	v, at, ok, err := e.store.LatestSample(ctx, r.DeviceID, p.Metric, r.Source, since)
	if err != nil {
		return "", err
	}
	if ok {
		if p.Confirms(v) {
			if _, err := e.store.Transition(ctx, r.ID, "confirming", "resolved", map[string]any{
				"confirm_value": v,
				"confirmed_at":  at,
			}); err != nil {
				return "", err
			}
			e.logf("selfheal: run %d: CONFIRMED — re-measured %s=%v (was %v) — healed", r.ID, p.Metric, v, deref(r.DetectValue))
			return "resolved", nil
		}
		cond := fmt.Sprintf("%s %s %g", p.Metric, p.ConfirmOp, p.ConfirmThreshold)
		return e.escalate(ctx, r, fmt.Sprintf("confirm FAILED: re-measurement %s=%v does not satisfy %q — remediation did not fix it",
			p.Metric, v, cond))
	}
	if r.DispatchedAt != nil && now.Sub(*r.DispatchedAt) > time.Duration(p.ConfirmWaitS)*time.Second {
		return e.escalate(ctx, r, fmt.Sprintf("no fresh %s measurement within %ds after remediation",
			p.Metric, p.ConfirmWaitS))
	}
	return "", nil
}

// escalate applies the confirming|remediating -> escalated transition and
// fires the Notifier exactly when the transition actually applied
// (replays never double-notify).
func (e *Engine) escalate(ctx context.Context, r *Run, reason string) (string, error) {
	applied, err := e.store.Transition(ctx, r.ID, r.Status, "escalated", map[string]any{
		"reason":       reason,
		"escalated_at": time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	if !applied {
		return "", nil
	}
	fresh, err := e.store.Run(ctx, r.ID)
	if err != nil {
		return "escalated", err
	}
	e.notify.Escalate(fresh, reason)
	return "escalated", nil
}

// dispatch is the injectable dispatch seam (ctx-scoped).
func (e *Engine) dispatch(ctx context.Context, deviceID, lang, script string) (string, error) {
	if e.remediate == nil {
		return "", fmt.Errorf("engine has no remediator wired")
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return e.remediate(rctx, deviceID, lang, script)
}

func (e *Engine) logf(format string, args ...any) {
	if e.log != nil {
		e.log.Printf(format, args...)
	}
}

// Store exposes the data layer (API / e2e).
func (e *Engine) Store() *Store { return e.store }

// WithLogger sets the engine's logger (nil = silent).
func (e *Engine) WithLogger(l *log.Logger) *Engine { e.log = l; return e }

// Run executes one pass immediately, then every interval until ctx is
// cancelled (mirrors the W2-3 baseline job cadence). Errors go to errCh
// (buffered; a full channel drops, never blocks the ticker).
func (e *Engine) Run(ctx context.Context, interval time.Duration, errCh chan<- error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	e.mu.Lock()
	e.interval = interval
	e.mu.Unlock()
	do := func() {
		pass := e.RunOnce(ctx, time.Now())
		for _, err := range pass.Errors {
			select {
			case errCh <- fmt.Errorf("selfheal pass: %s", err):
			default:
			}
		}
		if pass.Detections > 0 || pass.Started > 0 || pass.Confirmed > 0 || pass.Escalated > 0 {
			e.logf("selfheal: pass: detections=%d started=%d skipped=%d confirmed=%d escalated=%d failed=%d active=%d",
				pass.Detections, pass.Started, pass.Skipped, pass.Confirmed, pass.Escalated, pass.Failed, pass.ActiveRuns)
		}
	}
	do()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "(no detail)"
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
