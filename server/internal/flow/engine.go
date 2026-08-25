// Engine is the W5-2 event-driven flow executor. It is deliberately a thin
// consumer of the event bus:
//
//	<bus: flow.trigger>     -> evaluate the trigger condition, insert the
//	                           run (replay-safe), publish the first hop.
//	<bus: flow.step>        -> execute ONE node (script dispatch / metric
//	                           re-measure / notify), persist the hop,
//	                           publish the next hop.
//	<bus: command.result>   -> a script node's agent answer: advance or fail.
//
// plus two in-process tickers that keep the event-driven loop honest:
//
//	sweep   — for every in-flight run, re-publish its pending hop (a step
//	          event lost to a restart, or a result event the agent reported
//	          before the consumer caught it, is recovered) and enforce the
//	          node timeout.
//	sampler — the REAL trigger path: poll the metrics hypertable for the
//	          latest fresh sample of each enabled flow's trigger metric and
//	          publish a trigger event when the condition holds. Synthetic
//	          triggers (the API / another system) publish the same event.
//
// Every handler is idempotent: the state hop is a conditional UPDATE
// (WHERE status='running' AND current_node=<node>), so at-least-once bus
// delivery (and the sweep re-publishing the same hop) never double-acts.
// A flow can therefore only act through events on the bus — take the bus
// out and the chain stops.
package flow

import (
	"context"
	"fmt"
	"log"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Remediator dispatches one script to a device and returns the command id
// (production: the ingest Dispatcher, W3-3 capability token included).
type Remediator func(ctx context.Context, deviceID, lang, script string) (commandID string, err error)

// ResultLookup returns the recorded result for a command id (production:
// the ingest Dispatcher's result table).
type ResultLookup func(commandID string) (*agentv1.CommandResult, bool)

// Notifier is the notification seam for notify nodes and run failures
// (log now; W6-2's NATS/webhook notifier plugs into the same interface).
type Notifier interface {
	Notify(ctx context.Context, run *Run, nodeID, reason string)
}

// LogNotifier is the default Notifier.
type LogNotifier struct{ Log *log.Logger }

func (n LogNotifier) Notify(ctx context.Context, r *Run, nodeID, reason string) {
	l := n.Log
	if l == nil {
		l = log.Default()
	}
	l.Printf("flow: NOTIFY run %d (%s) node=%s device=%s: %s",
		r.ID, r.FlowName, nodeID, r.DeviceID, reason)
}

// Engine drives the flows.
type Engine struct {
	store     *Store
	bus       Bus
	remediate Remediator
	results   ResultLookup
	notify    Notifier
	log       *log.Logger

	sweepInterval  time.Duration
	sampleInterval time.Duration
}

// New builds an Engine. bus, remediate and results are required; notify
// defaults to LogNotifier; intervals <= 0 take the defaults (sweep 5s,
// sampler 60s). sampleInterval == -1 disables the sampler (synthetic
// triggers only).
func New(st *Store, bus Bus, remediate Remediator, results ResultLookup, notify Notifier, sweepInterval, sampleInterval time.Duration) *Engine {
	if notify == nil {
		notify = LogNotifier{}
	}
	if sweepInterval == 0 {
		sweepInterval = 5 * time.Second
	}
	if sampleInterval == 0 {
		sampleInterval = 60 * time.Second
	}
	return &Engine{
		store:          st,
		bus:            bus,
		remediate:      remediate,
		results:        results,
		notify:         notify,
		sweepInterval:  sweepInterval,
		sampleInterval: sampleInterval,
	}
}

// Store exposes the data layer (API / e2e).
func (e *Engine) Store() *Store { return e.store }

// WithLogger sets the engine's logger (nil = silent).
func (e *Engine) WithLogger(l *log.Logger) *Engine { e.log = l; return e }

// Start subscribes to the bus and starts the sweep + sampler tickers.
// Returns once the subscription is live (or with a subscription error).
func (e *Engine) Start(ctx context.Context) error {
	if err := e.bus.Subscribe(ctx, SubjectAll, e.onEvent); err != nil {
		return fmt.Errorf("flow engine subscribe: %w", err)
	}
	e.logf("flow engine: subscribed to %s (sweep %s, sampler %s)",
		SubjectAll, e.sweepInterval, describeInterval(e.sampleInterval))

	go func() {
		if e.sweepInterval <= 0 {
			<-ctx.Done()
			return
		}
		t := time.NewTicker(e.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.Sweep(ctx, time.Now())
			}
		}
	}()
	if e.sampleInterval > 0 {
		go func() {
			t := time.NewTicker(e.sampleInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					n := e.SampleOnce(ctx, time.Now())
					if n > 0 {
						e.logf("flow sampler: published %d trigger event(s)", n)
					}
				}
			}
		}()
	}
	return nil
}

func describeInterval(d time.Duration) string {
	if d < 0 {
		return "off"
	}
	return d.String()
}

// ---- event dispatch ----------------------------------------------------------

// onEvent is the bus handler: route by event type. Each handler must be
// idempotent (the bus is at-least-once and the sweep re-publishes).
func (e *Engine) onEvent(ctx context.Context, subject string, ev *Event) error {
	switch ev.Type {
	case SubjectTrigger:
		return e.handleTrigger(ctx, ev)
	case SubjectStep:
		return e.handleStep(ctx, ev)
	case SubjectCommand:
		return e.handleCommand(ctx, ev)
	default:
		return nil // fan-out subjects (flow.notify) — not for the engine
	}
}

// handleTrigger: a flow's trigger condition fired (from the sampler, the
// API, or any other event source). Insert the run (replay-safe) and
// publish the first hop.
func (e *Engine) handleTrigger(ctx context.Context, ev *Event) error {
	if ev.FlowID == 0 || ev.DeviceID == "" {
		return nil
	}
	f, err := e.store.Flow(ctx, ev.FlowID)
	if err != nil {
		e.logf("trigger: %v (dropped)", err)
		return nil
	}
	if !f.Enabled {
		return nil
	}
	trig := f.Graph.Trigger()
	if trig == nil {
		return nil
	}

	// Resolve the measurement: the event carries it (synthetic), else the
	// device's latest fresh sample (real). No measurement = no trigger.
	var value float64
	var at time.Time
	if ev.Value != nil {
		value, at = *ev.Value, ev.At
	} else if v, t, ok, err := e.store.LatestSample(ctx, ev.DeviceID, trig.Metric, trig.Source, time.Now().Add(-15*time.Minute)); err != nil {
		return err
	} else if !ok {
		return nil
	} else {
		value, at = v, t
	}
	if !trig.Holds(value) {
		return nil // condition not actually met (e.g. synthetic value below threshold)
	}
	// Anti-storm: a run already in flight (the DB's job) or one started
	// within the flow's cooldown.
	if f.CooldownS > 0 {
		if cd, err := e.store.CooldownStarted(ctx, f.ID, ev.DeviceID, ev.Source, time.Now().Add(-time.Duration(f.CooldownS)*time.Second)); err == nil && cd {
			e.logf("trigger: %s/%s within cooldown (%ds) — dropped", f.Name, ev.DeviceID, f.CooldownS)
			return nil
		}
	}

	run, created, err := e.store.InsertRun(ctx, f, ev.DeviceID, ev.Source, &value, at)
	if err != nil {
		return err
	}
	if !created {
		return nil // an active run for this (flow, device, source) already exists
	}
	e.logf("trigger: %s fired for %s (%s): %s = %v -> run %d",
		f.Name, ev.DeviceID, ev.Source, trig.DescribeCondition(), value, run.ID)

	// Hop the run onto its first action node (trigger-only flows terminate
	// here), then publish the step — every chain move is a bus event.
	applied, err := e.store.AdvanceTo(ctx, run.ID, trig.ID, trig.Next,
		fmt.Sprintf("trigger %s = %v", trig.DescribeCondition(), value), time.Now().UTC())
	if err != nil {
		return err
	}
	if applied && trig.Next != "" {
		return e.publishStep(ctx, run.ID, trig.Next)
	}
	return nil
}

// handleStep: execute one node of one run, then publish the next hop.
func (e *Engine) handleStep(ctx context.Context, ev *Event) error {
	if ev.RunID == 0 || ev.NodeID == "" {
		return nil
	}
	run, err := e.store.Run(ctx, ev.RunID)
	if err != nil {
		e.logf("step: %v (dropped)", err)
		return nil
	}
	// Replay / stale guard: only the run's CURRENT node may advance it.
	if run.Status != "running" || run.CurrentNode != ev.NodeID {
		return nil
	}
	f := e.flowForRun(ctx, run)
	if f == nil {
		return nil
	}
	node := f.Graph.Node(ev.NodeID)
	if node == nil {
		if applied, _ := e.store.FailRun(ctx, run.ID, ev.NodeID, "failed", "unknown node in flow graph"); applied {
			e.notifyFail(run, ev.NodeID, "unknown node in flow graph")
		}
		return nil
	}
	switch node.Kind {
	case KindTrigger:
		return nil // the run starts AT the trigger; a step for it is a stale replay
	case KindScript:
		return e.stepScript(ctx, run, node)
	case KindCheck:
		return e.stepCheck(ctx, run, node)
	case KindNotify:
		return e.stepNotify(ctx, run, node)
	}
	return nil
}

// stepScript dispatches the node's script once (or checks its result when
// already dispatched), then advances or fails the run.
func (e *Engine) stepScript(ctx context.Context, run *Run, node *Node) error {
	if run.CommandID == nil {
		// First time at this node: dispatch.
		cmdID, err := e.remediate(ctx, run.DeviceID, node.Lang, node.Script)
		if err != nil {
			if applied, _ := e.store.FailRun(ctx, run.ID, node.ID, "failed",
				fmt.Sprintf("dispatch to %s failed: %v", run.DeviceID, err)); applied {
				e.notifyFail(run, node.ID, err.Error())
			}
			return nil
		}
		if applied, err := e.store.Dispatched(ctx, run.ID, node.ID, cmdID, time.Now().UTC()); err != nil {
			return err
		} else if !applied {
			// A racing duplicate already recorded a dispatch — re-read and
			// fall through to the result check below.
			if fresh, err := e.store.Run(ctx, run.ID); err == nil {
				run = fresh
			}
		}
	}
	// Already dispatched: act on the result if the agent reported one.
	if run.CommandID == nil {
		return nil // nothing to do yet; the sweep / command.result drives it
	}
	res, ok := e.results(*run.CommandID)
	if !ok || res == nil {
		return nil
	}
	return e.finishScript(ctx, run, node, res)
}

// finishScript applies the script node's outcome: SUCCEEDED advances to
// the next node (publishing the hop); anything final-but-not-success
// fails the run (+notify); RECEIVED is still in flight.
func (e *Engine) finishScript(ctx context.Context, run *Run, node *Node, res *agentv1.CommandResult) error {
	switch res.GetStatus() {
	case agentv1.CommandResult_SUCCEEDED:
		reason := fmt.Sprintf("script %s succeeded", *run.CommandID)
		// The next check node re-measures from the script's dispatch: the
		// agent reports the remediation's effect (a fresh metric batch)
		// around the command result, which may predate this very hop.
		ca := time.Now().UTC()
		if run.DispatchedAt != nil {
			ca = *run.DispatchedAt
		}
		applied, err := e.store.AdvanceTo(ctx, run.ID, node.ID, node.Next, reason, ca)
		if err != nil {
			return err
		}
		if applied && node.Next != "" {
			return e.publishStep(ctx, run.ID, node.Next)
		}
		return nil
	case agentv1.CommandResult_FAILED, agentv1.CommandResult_TIMED_OUT,
		agentv1.CommandResult_REFUSED, agentv1.CommandResult_UNSUPPORTED:
		reason := fmt.Sprintf("script %s: %s %s", *run.CommandID, res.GetStatus(),
			firstNonEmpty(res.GetError(), res.GetStderrTail()))
		if applied, _ := e.store.FailRun(ctx, run.ID, node.ID, "failed", reason); applied {
			e.notifyFail(run, node.ID, reason)
		}
		return nil
	default: // RECEIVED / RUNNING / STATUS_UNSPECIFIED: still in flight
		return nil
	}
}

// stepCheck re-measures the metric (a sample strictly after the previous
// node completed) and branches: holds -> then-edge, not -> else-edge (""
// ends the run successfully). No fresh sample yet: record the wait (once)
// and let the sweep re-cover; past the node timeout, fail.
func (e *Engine) stepCheck(ctx context.Context, run *Run, node *Node) error {
	since := run.StartedAt
	if run.CheckAfter != nil {
		since = *run.CheckAfter
	}
	v, at, ok, err := e.store.LatestSample(ctx, run.DeviceID, node.Metric, node.Source, since)
	if err != nil {
		return err
	}
	if !ok {
		if err := e.store.WaitEvent(ctx, run.ID, node.ID,
			fmt.Sprintf("waiting for a fresh %s sample after %s", node.Metric, since.UTC().Format(time.RFC3339))); err != nil {
			return err
		}
		if e.nodeTimeoutExceeded(run, node, since) {
			reason := fmt.Sprintf("no fresh %s sample within %ds after %s", node.Metric, nodeTimeoutS(node), since.UTC().Format(time.RFC3339))
			if applied, _ := e.store.FailRun(ctx, run.ID, node.ID, "timeout", reason); applied {
				e.notifyFail(run, node.ID, reason)
			}
		}
		return nil
	}
	if node.Holds(v) {
		reason := fmt.Sprintf("%s = %v at %s -> then", node.Metric, v, at.UTC().Format(time.RFC3339))
		applied, err := e.store.AdvanceTo(ctx, run.ID, node.ID, node.Then, reason, at)
		if err != nil {
			return err
		}
		if applied && node.Then != "" {
			return e.publishStep(ctx, run.ID, node.Then)
		}
		return nil
	}
	// Condition NOT held.
	if node.Else == "" {
		reason := fmt.Sprintf("%s = %v at %s -> chain complete (condition not held)", node.Metric, v, at.UTC().Format(time.RFC3339))
		_, err := e.store.AdvanceTo(ctx, run.ID, node.ID, "", reason, at)
		return err
	}
	reason := fmt.Sprintf("%s = %v at %s -> else", node.Metric, v, at.UTC().Format(time.RFC3339))
	applied, err := e.store.AdvanceTo(ctx, run.ID, node.ID, node.Else, reason, at)
	if err != nil {
		return err
	}
	if applied && node.Else != "" {
		return e.publishStep(ctx, run.ID, node.Else)
	}
	return nil
}

// stepNotify fires the notification exactly once (the conditional hop
// applies first; only the winning transition notifies), then continues.
func (e *Engine) stepNotify(ctx context.Context, run *Run, node *Node) error {
	msg := node.Message
	if msg == "" {
		msg = fmt.Sprintf("flow %s ran on %s", run.FlowName, run.DeviceID)
	}
	applied, err := e.store.AdvanceTo(ctx, run.ID, node.ID, node.Next, msg, time.Now().UTC())
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	e.notify.Notify(ctx, run, node.ID, msg)
	if node.Next != "" {
		return e.publishStep(ctx, run.ID, node.Next)
	}
	return nil
}

// handleCommand: an agent reported a command result (the ingest hook
// published it on the bus). Find the run whose current script node owns
// the command and apply the outcome.
func (e *Engine) handleCommand(ctx context.Context, ev *Event) error {
	if ev.CommandID == "" {
		return nil
	}
	run, err := e.store.RunByCommand(ctx, ev.CommandID)
	if err != nil {
		return err
	}
	if run == nil {
		return nil // no flow run owns this command (operator dispatch, or already advanced)
	}
	f := e.flowForRun(ctx, run)
	if f == nil {
		return nil
	}
	node := f.Graph.Node(run.CurrentNode)
	if node == nil || node.Kind != KindScript {
		return nil
	}
	res := &agentv1.CommandResult{
		CommandId: ev.CommandID,
		Status:    commandStatusFromString(ev.Status),
		Error:     ev.Message,
	}
	return e.finishScript(ctx, run, node, res)
}

// ---- tickers -----------------------------------------------------------------

// Sweep re-covers every in-flight run: re-publish the pending hop (a step
// event lost to a restart, or a dispatch/result the bus missed) and enforce
// the node timeout. Idempotent — a healthy run just gets its hop
// re-published, which handleStep treats as a replay no-op.
func (e *Engine) Sweep(ctx context.Context, now time.Time) {
	runs, err := e.store.ActiveRuns(ctx)
	if err != nil {
		e.logf("sweep: %v", err)
		return
	}
	for i := range runs {
		run := &runs[i]
		f := e.flowForRun(ctx, run)
		if f == nil {
			continue
		}
		node := f.Graph.Node(run.CurrentNode)
		if node == nil {
			if applied, _ := e.store.FailRun(ctx, run.ID, run.CurrentNode, "failed", "unknown node in flow graph"); applied {
				e.notifyFail(run, run.CurrentNode, "unknown node in flow graph")
			}
			continue
		}
		// Timeout: a script past its window with no final result.
		if node.Kind == KindScript && run.CommandID != nil && run.DispatchedAt != nil {
			to := nodeTimeoutS(node)
			if now.Sub(*run.DispatchedAt) > time.Duration(to)*time.Second {
				res, ok := e.results(*run.CommandID)
				if ok && res != nil && isFinalStatus(res.GetStatus()) {
					_ = e.finishScript(ctx, run, node, res)
					continue
				}
				reason := fmt.Sprintf("script %s: no final result within %ds", *run.CommandID, to)
				if applied, _ := e.store.FailRun(ctx, run.ID, node.ID, "timeout", reason); applied {
					e.notifyFail(run, node.ID, reason)
				}
				continue
			}
		}
		// Re-publish the pending hop (no-op if a racing event already
		// advanced the run).
		_ = e.publishStep(ctx, run.ID, run.CurrentNode)
	}
}

// SampleOnce is the REAL trigger path: for every enabled flow whose
// trigger is a metric condition, look at the latest fresh sample per
// (device, source) and publish a trigger event where the condition holds.
// Returns the number of trigger events published.
func (e *Engine) SampleOnce(ctx context.Context, now time.Time) int {
	flows, err := e.store.ListFlows(ctx, true)
	if err != nil {
		e.logf("sampler: %v", err)
		return 0
	}
	n := 0
	for i := range flows {
		f := &flows[i]
		trig := f.Graph.Trigger()
		if trig == nil || trig.Metric == "" {
			continue
		}
		samples, err := e.store.FreshSamples(ctx, trig.Metric, trig.Source, now, 5*time.Minute)
		if err != nil {
			e.logf("sampler: %s: %v", f.Name, err)
			continue
		}
		for i := range samples {
			s := samples[i]
			if !trig.Holds(s.Value) {
				continue
			}
			ev := &Event{Type: SubjectTrigger, FlowID: f.ID, DeviceID: s.DeviceID,
				Source: s.Source, Value: &s.Value, At: s.At}
			if err := e.bus.Publish(ctx, SubjectTrigger, ev); err != nil {
				e.logf("sampler: publish %s/%s: %v", f.Name, s.DeviceID, err)
				continue
			}
			n++
		}
	}
	return n
}

// Trigger publishes a synthetic trigger event for a flow + device (the
// API's POST /flows/{id}/trigger). value nil = "measure it" (the latest
// fresh sample of the trigger metric).
func (e *Engine) Trigger(ctx context.Context, flowID int64, deviceID, source string, value *float64) error {
	ev := &Event{Type: SubjectTrigger, FlowID: flowID, DeviceID: deviceID, Source: source, Value: value, At: time.Now().UTC()}
	return e.bus.Publish(ctx, SubjectTrigger, ev)
}

// flowForRun loads the run's flow, or — if the flow was deleted mid-run —
// fails the run (exactly once, via the conditional transition) and returns
// nil. A nil FlowID means the FK was SET NULL by the flow's DELETE.
func (e *Engine) flowForRun(ctx context.Context, run *Run) *Flow {
	if run.FlowID == nil {
		if applied, _ := e.store.FailRun(ctx, run.ID, run.CurrentNode, "failed", "flow deleted mid-run"); applied {
			e.notifyFail(run, run.CurrentNode, "flow deleted mid-run")
		}
		return nil
	}
	f, err := e.store.Flow(ctx, *run.FlowID)
	if err != nil {
		if applied, _ := e.store.FailRun(ctx, run.ID, run.CurrentNode, "failed", "flow deleted mid-run"); applied {
			e.notifyFail(run, run.CurrentNode, "flow deleted mid-run")
		}
		return nil
	}
	return f
}

// publishStep is the engine's single "move the chain forward" call: every
// hop of every flow travels the bus through here.
func (e *Engine) publishStep(ctx context.Context, runID int64, nodeID string) error {
	ev := &Event{Type: SubjectStep, RunID: runID, NodeID: nodeID, At: time.Now().UTC()}
	return e.bus.Publish(ctx, SubjectStep, ev)
}

func (e *Engine) notifyFail(run *Run, nodeID, reason string) {
	e.logf("flow: run %d (%s) node=%s device=%s FAILED: %s", run.ID, run.FlowName, nodeID, run.DeviceID, reason)
	e.notify.Notify(context.Background(), run, nodeID, "run failed: "+reason)
}

func (e *Engine) logf(format string, args ...any) {
	if e.log != nil {
		e.log.Printf(format, args...)
	}
}

// ---- helpers -----------------------------------------------------------------

func nodeTimeoutS(node *Node) int {
	if node.TimeoutS > 0 {
		return node.TimeoutS
	}
	return 300
}

func (e *Engine) nodeTimeoutExceeded(run *Run, node *Node, since time.Time) bool {
	return time.Since(since) > time.Duration(nodeTimeoutS(node))*time.Second
}

func isFinalStatus(s agentv1.CommandResult_Status) bool {
	switch s {
	case agentv1.CommandResult_SUCCEEDED, agentv1.CommandResult_FAILED,
		agentv1.CommandResult_TIMED_OUT, agentv1.CommandResult_REFUSED,
		agentv1.CommandResult_UNSUPPORTED:
		return true
	}
	return false
}

func commandStatusFromString(s string) agentv1.CommandResult_Status {
	switch s {
	case "SUCCEEDED":
		return agentv1.CommandResult_SUCCEEDED
	case "FAILED":
		return agentv1.CommandResult_FAILED
	case "TIMED_OUT":
		return agentv1.CommandResult_TIMED_OUT
	case "REFUSED":
		return agentv1.CommandResult_REFUSED
	case "UNSUPPORTED":
		return agentv1.CommandResult_UNSUPPORTED
	case "RECEIVED", "RUNNING":
		return agentv1.CommandResult_RECEIVED
	default:
		return agentv1.CommandResult_STATUS_UNSPECIFIED
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
