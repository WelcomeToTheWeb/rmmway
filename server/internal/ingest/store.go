// Package ingest implements the RMMWay agent ingest service (W1-5):
// JWT auth, enroll, metric receive, command dispatch.
//
// Storage lives behind the interfaces in internal/store so W1-6
// (TimescaleDB) swaps in without touching the gRPC layer.
package ingest

import (
	"fmt"
	"sort"
	"sync"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- command dispatch -------------------------------------------------------

// Dispatcher mints commands and tracks their results. Commands are queued
// onto the owning agent's stream via a store.CommandSink.
//
// W3-3: when built with a caps.Issuer, every dispatched command carries a
// short-lived capability token bound to (device, capability, command id) —
// the agent verifies it against its pinned org root before acting and
// refuses (CommandResult REFUSED) anything outside the minted scope.
type Dispatcher struct {
	mu      sync.Mutex
	idSeq   int64
	sink    store.CommandSink
	caps    *caps.Issuer
	pending map[string]*agentv1.Command
	results map[string]*agentv1.CommandResult
	devices map[string]string // command id -> device id (per-device queries)
}

// NewDispatcher builds a Dispatcher. caps nil = legacy dispatch (no
// capability tokens; pre-W3-3 servers / plain-listener deployments).
func NewDispatcher(sink store.CommandSink, caps *caps.Issuer) *Dispatcher {
	return &Dispatcher{
		sink:    sink,
		caps:    caps,
		pending: make(map[string]*agentv1.Command),
		results: make(map[string]*agentv1.CommandResult),
		devices: make(map[string]string),
	}
}

// Dispatch mints a command for a device and pushes it to the agent's stream.
// Returns the command id, or an error if the device is unknown/offline.
//
// W3-3: when the Dispatcher has a capability issuer, the minted command
// carries a capability token for (device, action's capability, command id)
// embedded in the action — the agent refuses to act without a valid one.
func (d *Dispatcher) Dispatch(deviceID string, action any) (string, error) {
	return d.dispatch(deviceID, action, "", false)
}

// DispatchWith mints a command for a device carrying EXACTLY the given
// capability token (empty = NO token) and pushes it. Test seam for W3-3:
// the e2e harness uses it to hand an agent a misbound / expired / missing
// token over a fully valid mTLS channel and assert the refusal.
func (d *Dispatcher) DispatchWith(deviceID string, token string, action any) (string, error) {
	return d.dispatch(deviceID, action, token, true)
}

func (d *Dispatcher) dispatch(deviceID string, action any, explicitToken string, explicit bool) (string, error) {
	cmd := &agentv1.Command{IssuedAtMs: time.Now().UnixMilli()}
	switch a := action.(type) {
	case *agentv1.Command_RunScript:
		cmd.Action = a
	case *agentv1.Command_Reboot:
		cmd.Action = a
	default:
		return "", fmt.Errorf("unsupported action type %T", action)
	}
	d.mu.Lock()
	d.idSeq++
	cmd.Id = fmt.Sprintf("cmd-%d", d.idSeq)
	d.pending[cmd.Id] = cmd
	d.devices[cmd.Id] = deviceID
	d.mu.Unlock()

	// W3-3: the capability token is bound to the command id, so it is minted
	// after the id is assigned. Production dispatch always mints; DispatchWith
	// stamps exactly the given token (empty stays empty — a tokenless command
	// is how the "missing token" refusal is exercised on the wire). Legacy
	// dispatch (no issuer) leaves the token field empty.
	if d.caps != nil {
		var tok string
		if explicit {
			tok = explicitToken
		} else {
			capName, _ := caps.ForAction(action)
			var err error
			if tok, err = d.caps.Mint(deviceID, capName, cmd.Id); err != nil {
				return "", fmt.Errorf("mint capability token: %w", err)
			}
		}
		switch a := action.(type) {
		case *agentv1.Command_RunScript:
			a.RunScript.CapabilityToken = tok
		case *agentv1.Command_Reboot:
			a.Reboot.CapabilityToken = tok
		}
	}

	if !d.sink.Push(deviceID, cmd) {
		// The agent never saw it: drop the pending entry so a 502 dispatch
		// doesn't leak a command that can never be answered.
		d.mu.Lock()
		delete(d.pending, cmd.Id)
		delete(d.devices, cmd.Id)
		d.mu.Unlock()
		return "", fmt.Errorf("device %s not reachable (no live stream)", deviceID)
	}
	return cmd.Id, nil
}

// RecordResult stores an agent's command result (W1-4 sends these; the
// StreamRequest result extension lands with W1-4, so the server side is
// ready now).
func (d *Dispatcher) RecordResult(res *agentv1.CommandResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results[res.GetCommandId()] = res
	delete(d.pending, res.GetCommandId())
}

// Pending returns commands awaiting a final result.
func (d *Dispatcher) Pending() []*agentv1.Command {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*agentv1.Command, 0, len(d.pending))
	for _, c := range d.pending {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// Result returns the recorded result for a command id.
func (d *Dispatcher) Result(id string) (*agentv1.CommandResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.results[id]
	return r, ok
}

// PendingFor returns the device's commands still awaiting a final result
// (oldest first) — W3-3's /admin/devices/{id}/commands surface.
func (d *Dispatcher) PendingFor(deviceID string) []*agentv1.Command {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []*agentv1.Command{}
	for id, c := range d.pending {
		if d.devices[id] == deviceID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// ResultsFor returns the device's recorded command results (oldest first).
func (d *Dispatcher) ResultsFor(deviceID string) []*agentv1.CommandResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []*agentv1.CommandResult{}
	for id, r := range d.results {
		if d.devices[id] == deviceID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetCommandId() < out[j].GetCommandId()
	})
	return out
}
