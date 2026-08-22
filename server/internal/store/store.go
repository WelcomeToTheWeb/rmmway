// Package store provides the server's data layer (W1-6).
//
// It defines the storage interfaces the ingest gRPC layer programs against
// (MetricsSink, DeviceStore) plus the in-memory implementations used by
// tests, and the Postgres/Timescale implementations used in production.
//
// Migrations are plain SQL files in server/migrations, applied at server
// boot (Migrate) and exposed via `make migrate`. They are tracked in
// schema_migrations and must be idempotent anyway (defense in depth: a
// partially-applied migration must not break restart).
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// ---- interfaces -------------------------------------------------------------

// MetricsSink receives metric batches keyed by device.
type MetricsSink interface {
	Write(deviceID string, batch *agentv1.MetricBatch) error
}

// CommandSink fans a minted command out to the owning agent's stream.
type CommandSink interface {
	Push(deviceID string, cmd *agentv1.Command) bool
}

// DeviceStore persists enrolled devices.
type DeviceStore interface {
	// Register upserts a device at enroll time (keeps first_seen).
	Register(ctx context.Context, id, hostname, os, arch, agentVersion string, interfaces []string, heartbeatIntS, metricIntS int32) error
	// Contains reports whether a device id is enrolled (JWT auth path).
	Contains(ctx context.Context, id string) (bool, error)
	// Touch marks a device online/seen (heartbeat path).
	Touch(ctx context.Context, id string) error
	// List returns all devices (admin / W2-1 / W1-7 indexing source).
	List(ctx context.Context) ([]*Device, error)
}

// Device is the registry row shape shared by all DeviceStore implementations.
type Device struct {
	ID            string
	Hostname      string
	OS            string
	Arch          string
	AgentVersion  string
	Interfaces    []string
	Tags          []string
	Online        bool
	FirstSeen     time.Time
	LastSeen      time.Time
	MetricIntS    int32
	HeartbeatIntS int32
}

// ---- in-memory implementations (tests / standalone) -------------------------

// MemoryMetricsSink keeps the most recent N samples per device in a bounded
// ring. Sufficient for unit tests; production uses PostgresMetricsSink.
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

// MemoryDeviceStore is the in-memory DeviceStore (unit tests).
type MemoryDeviceStore struct {
	mu      sync.RWMutex
	devices map[string]*Device
}

func NewMemoryDeviceStore() *MemoryDeviceStore {
	return &MemoryDeviceStore{devices: make(map[string]*Device)}
}

func (r *MemoryDeviceStore) Register(_ context.Context, id, hostname, os, arch, agentVersion string, interfaces []string, heartbeatIntS, metricIntS int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	d, ok := r.devices[id]
	if !ok {
		d = &Device{ID: id, FirstSeen: now}
		r.devices[id] = d
	}
	d.Hostname, d.OS, d.Arch, d.AgentVersion = hostname, os, arch, agentVersion
	d.Interfaces, d.HeartbeatIntS, d.MetricIntS = interfaces, heartbeatIntS, metricIntS
	d.Online, d.LastSeen = true, now
	return nil
}

func (r *MemoryDeviceStore) Contains(_ context.Context, id string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.devices[id]
	return ok, nil
}

func (r *MemoryDeviceStore) Touch(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[id]; ok {
		d.Online = true
		d.LastSeen = time.Now().UTC()
	}
	return nil
}

// List returns a stable copy of all devices (sorted by id).
func (r *MemoryDeviceStore) List(_ context.Context) ([]*Device, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Device, 0, len(r.devices))
	for _, d := range r.devices {
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get is a test helper for the memory store.
func (r *MemoryDeviceStore) Get(id string) (*Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// ---- migrations -------------------------------------------------------------

// Migrate applies every migration in dir that hasn't been recorded in
// schema_migrations, each in its own transaction.
func Migrate(ctx context.Context, db *pgxpool.Pool, dir string) (applied int, err error) {
	_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		name := strings.TrimSuffix(f, ".sql")
		var exists bool
		if err := db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&exists); err != nil {
			return applied, fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		sqlText, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return applied, fmt.Errorf("read %s: %w", f, err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("commit %s: %w", name, err)
		}
		applied++
	}
	return applied, nil
}
