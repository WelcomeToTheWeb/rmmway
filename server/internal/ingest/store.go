// Package ingest implements the RMMWay agent ingest service (W1-5):
// JWT auth, enroll, metric receive, command dispatch.
//
// Storage is in-memory for W1-5 (Definition of Done: accept an enrolled
// agent's stream, reject unauthenticated agents). W1-6 swaps the sinks for
// TimescaleDB + the devices table behind the same interfaces, so the gRPC
// service code does not change when persistence lands.
package ingest

import (
	"fmt"
	"sort"
	"sync"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// ---- interfaces (W1-6 implementations replace the in-memory ones) --------

// MetricsSink receives metric batches keyed by device.
type MetricsSink interface {
	Write(deviceID string, batch *agentv1.MetricBatch) error
}

// CommandSink fans a minted command out to the owning agent's stream.
type CommandSink interface {
	Push(deviceID string, cmd *agentv1.Command) bool
}

// ---- in-memory metrics sink ------------------------------------------------

// MemoryMetricsSink keeps the most recent N samples per device in a bounded
// ring. Sufficient for the W1-5 DoD (metrics "land" in the server); W1-6
// replaces this with the Timescale hypertable writer.
type MemoryMetricsSink struct {
	mu     sync.Mutex
	cap    int
	perDev map[string][]*agentv1.Metric
}

func NewMemoryMetricsSink(cap int) *MemoryMetricsSink {
	if cap <= 0 {
		cap = 10000
	}
	return &MemoryMetricsSink{cap: cap, perDev: make(map[string][]*agentv1.Metric)}
}

func (m *MemoryMetricsSink) Write(deviceID string, batch *agentv1.MetricBatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range batch.GetSamples() {
		m.perDev[deviceID] = append(m.perDev[deviceID], s)
	}
	if n := len(m.perDev[deviceID]); n > m.cap {
		m.perDev[deviceID] = m.perDev[deviceID][n-m.cap:]
	}
	return nil
}

// Samples returns the stored samples for a device (test/inspect helper).
func (m *MemoryMetricsSink) Samples(deviceID string) []*agentv1.Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*agentv1.Metric, len(m.perDev[deviceID]))
	copy(out, m.perDev[deviceID])
	return out
}

// Count is the total stored sample count across devices.
func (m *MemoryMetricsSink) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, s := range m.perDev {
		total += len(s)
	}
	return total
}

// ---- device registry ---------------------------------------------------------

type Device struct {
	ID            string
	Hostname      string
	OS            string
	Arch          string
	AgentVersion  string
	Interfaces    []string
	JTI           string // current JWT id (rotation support, W3-2)
	Online        bool
	LastSeen      time.Time
	MetricIntS    int32
	HeartbeatIntS int32
}

type DeviceRegistry struct {
	mu      sync.RWMutex
	devices map[string]*Device
}

func NewDeviceRegistry() *DeviceRegistry {
	return &DeviceRegistry{devices: make(map[string]*Device)}
}

func (r *DeviceRegistry) Get(id string) (*Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[id]
	return d, ok
}

func (r *DeviceRegistry) Upsert(d *Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices[d.ID] = d
}

// List returns a stable copy of all devices (sorted by id).
func (r *DeviceRegistry) List() []*Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Device, 0, len(r.devices))
	for _, d := range r.devices {
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Touch marks a device seen (online, lastseen=now).
func (r *DeviceRegistry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[id]; ok {
		d.Online = true
		d.LastSeen = time.Now().UTC()
	}
}

// ---- command dispatch -------------------------------------------------------

// Dispatcher mints commands and tracks their results. Commands are queued
// onto the owning agent's stream via a CommandSink.
type Dispatcher struct {
	mu      sync.Mutex
	idSeq   int64
	sink    CommandSink
	pending map[string]*agentv1.Command
	results map[string]*agentv1.CommandResult
}

func NewDispatcher(sink CommandSink) *Dispatcher {
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
