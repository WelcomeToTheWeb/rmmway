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
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- command dispatch -------------------------------------------------------

// Dispatcher mints commands and tracks their results. Commands are queued
// onto the owning agent's stream via a store.CommandSink.
type Dispatcher struct {
	mu      sync.Mutex
	idSeq   int64
	sink    store.CommandSink
	pending map[string]*agentv1.Command
	results map[string]*agentv1.CommandResult
}

func NewDispatcher(sink store.CommandSink) *Dispatcher {
	return &Dispatcher{sink: sink, pending: make(map[string]*agentv1.Command), results: make(map[string]*agentv1.CommandResult)}
}

// Dispatch mints a command for a device and pushes it to the agent's stream.
// Returns the command id, or an error if the device is unknown/offline.
func (d *Dispatcher) Dispatch(deviceID string, action any) (string, error) {
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
	d.mu.Unlock()

	if !d.sink.Push(deviceID, cmd) {
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
