// Package export implements W4-3: per-client full export — the no-lock-in
// promise.
//
// A client (device) export is a single SELF-DESCRIBING ZIP bundle containing
// everything RMMWay knows about that client:
//
//	manifest.json       — the bundle contract: format name + version, export
//	                      metadata, and every other file with its size,
//	                      sha256 and (for data files) row count.
//	device.json         — inventory (identity, os/arch, agent version,
//	                      interfaces) + configuration (intervals, tags) as
//	                      the server sees it.
//	metrics.parquet     — raw metric samples for the requested window, in
//	                      standard Apache Parquet (opens in pandas, duckdb,
//	                      polars, ...).
//	metrics_1m.parquet  — 1-minute rollups (full history; survives the raw
//	                      90-day retention window).
//	alerts.json         — the client's complete alert history (all statuses).
//	README.md           — how to open and verify the bundle with standard
//	                      tools.
//
// Self-describing: manifest.json alone drives Verify — it re-hashes every
// file, rejects stray entries, and re-reads each Parquet section with an
// independent standard Parquet reader, checking row counts and decodability.
// The bundle is portable data, not an RMMWay database dump: no internal
// table names, no server-only types in the payloads.
package export

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// Bundle format identifiers (bump FormatVersion when the layout or the
// Parquet schemas change).
const (
	FormatName    = "rmmway-client-bundle"
	FormatVersion = 1

	ManifestName = "manifest.json"
	DeviceName   = "device.json"
	MetricsName  = "metrics.parquet"
	RollupsName  = "metrics_1m.parquet"
	AlertsName   = "alerts.json"
	ReadmeName   = "README.md"

	deviceSchema = "rmmway.device/v1"
	alertsSchema = "rmmway.alerts/v1"
)

// ---- Parquet section schemas (flat + standard) ------------------------------

// MetricRow is one row of metrics.parquet.
//
//	timestamp_ms — raw agent wall clock, Unix ms (exact, idempotency key)
//	ts           — derived timestamptz (Timestamp[ns]; datetime64[ns] in
//	               pandas, TIMESTAMP in duckdb)
//	name         — metric name (cpu.utilization_percent, ...)
//	source       — sda1, eth0, ... "" = host-wide
//	value        — the sample
//	labels       — JSON object as a string (Parquet has no JSON type; a
//	               string keeps the file openable everywhere)
type MetricRow struct {
	TimestampMs int64     `parquet:"timestamp_ms"`
	TS          time.Time `parquet:"ts"`
	Name        string    `parquet:"name"`
	Source      string    `parquet:"source"`
	Value       float64   `parquet:"value"`
	Labels      string    `parquet:"labels"`
}

// RollupRow is one row of metrics_1m.parquet (the 1-minute continuous
// aggregate: avg/min/max + sample count per bucket).
type RollupRow struct {
	Bucket time.Time `parquet:"bucket"`
	Name   string    `parquet:"name"`
	Source string    `parquet:"source"`
	Avg    float64   `parquet:"avg_value"`
	Min    float64   `parquet:"min_value"`
	Max    float64   `parquet:"max_value"`
	N      int64     `parquet:"n"`
}

// ---- data sources (interfaces keep the builder unit-testable) ---------------

// Sample is one raw metric sample flowing into metrics.parquet.
type Sample struct {
	TS          time.Time
	TimestampMs int64
	Name        string
	Source      string
	Value       float64
	Labels      map[string]string
}

// Rollup is one 1-minute aggregate flowing into metrics_1m.parquet.
type Rollup struct {
	Bucket time.Time
	Name   string
	Source string
	Avg    float64
	Min    float64
	Max    float64
	N      int64
}

// MetricsReader streams a device's raw samples in ts order. since/ununtil
// are the optional window (zero value = unbounded). fn is called per
// sample; a non-nil return stops the stream. Returns the rows streamed.
type MetricsReader interface {
	Stream(ctx context.Context, deviceID string, since, until time.Time, fn func(Sample) error) (int64, error)
}

// RollupReader streams a device's 1-minute rollups (full history).
type RollupReader interface {
	Stream(ctx context.Context, deviceID string, fn func(Rollup) error) (int64, error)
}

// Alert is one row of alerts.json — the alert-inbox record for the client,
// server-side (no internal state beyond what the API already serves).
type Alert struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Source     string     `json:"source"`
	Status     string     `json:"status"`
	Score      float64    `json:"score"`
	Channel    string     `json:"channel"`
	Value      float64    `json:"value"`
	Expected   *float64   `json:"expected,omitempty"`
	Events     int        `json:"events"`
	FirstAt    time.Time  `json:"first_at"`
	LastAt     time.Time  `json:"last_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// AlertsFile is alerts.json.
type AlertsFile struct {
	Schema string  `json:"schema"`
	Alerts []Alert `json:"alerts"`
}

// AlertReader returns the device's complete alert history, oldest first.
type AlertReader interface {
	List(ctx context.Context, deviceID string) ([]Alert, error)
}

// ---- device.json -------------------------------------------------------------

// DeviceFile is device.json: the client's inventory + configuration.
type DeviceFile struct {
	Schema string       `json:"schema"`
	Device DeviceOut    `json:"device"`
	Config DeviceConfig `json:"config"`
}

// DeviceOut is the inventory half: identity + observed facts.
type DeviceOut struct {
	ID           string    `json:"id"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	Interfaces   []string  `json:"interfaces"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

// DeviceConfig is the configuration half: operator-set values.
type DeviceConfig struct {
	MetricIntervalS    int32    `json:"metric_interval_s"`
	HeartbeatIntervalS int32    `json:"heartbeat_interval_s"`
	Tags               []string `json:"tags"`
}

// ---- manifest ----------------------------------------------------------------

// ManifestFile describes one file of the bundle.
type ManifestFile struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"` // empty for manifest.json (itself)
	Rows        int64  `json:"rows,omitempty"`   // data files only
	Description string `json:"description"`
}

// ManifestRange is the metrics window of the export (nil = full retention).
type ManifestRange struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// ManifestDevice identifies the client the bundle belongs to.
type ManifestDevice struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

// Manifest is manifest.json — the self-describing bundle contract.
type Manifest struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	ExportedAt    time.Time      `json:"exported_at"`
	GeneratedBy   string         `json:"generated_by"`
	Device        ManifestDevice `json:"device"`
	Range         *ManifestRange `json:"range,omitempty"`
	Files         []ManifestFile `json:"files"`
}

// ---- service -------------------------------------------------------------------

// Config wires a Service.
type Config struct {
	Devices store.DeviceStore
	// Metrics streams metrics.parquet; nil produces an empty (valid) file.
	Metrics MetricsReader
	// Rollups streams metrics_1m.parquet; nil omits the section.
	Rollups RollupReader
	// Alerts fills alerts.json; nil produces an empty list.
	Alerts AlertReader
	// Version is stamped into manifest.generated_by.
	Version string
}

// Service builds client export bundles.
type Service struct {
	devices store.DeviceStore
	metrics MetricsReader
	rollups RollupReader
	alerts  AlertReader
	version string
	now     func() time.Time
}

// New builds a Service.
func New(cfg Config) *Service {
	if cfg.Version == "" {
		cfg.Version = "rmmway-server"
	}
	return &Service{
		devices: cfg.Devices,
		metrics: cfg.Metrics,
		rollups: cfg.Rollups,
		alerts:  cfg.Alerts,
		version: cfg.Version,
		now:     time.Now,
	}
}

// WithNow injects the clock (tests).
func (s *Service) WithNow(f func() time.Time) *Service {
	s.now = f
	return s
}

// parquetBatch is the row buffer between the source stream and the Parquet
// writer (keeps memory flat while streaming a full device history).
const parquetBatch = 512

// Export writes the client bundle for deviceID as a ZIP stream to w and
// returns the manifest it embedded. The metrics window is [since, until) —
// zero values are unbounded. withRollups=false omits metrics_1m.parquet.
func (s *Service) Export(ctx context.Context, deviceID string, since, until time.Time, withRollups bool, w io.Writer) (*Manifest, error) {
	if since.IsZero() && !until.IsZero() {
		return nil, fmt.Errorf("export: until without since")
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return nil, fmt.Errorf("export: until must be after since")
	}
	dev, err := s.devices.Get(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	zipw := zip.NewWriter(w)
	files := make([]ManifestFile, 0, 6)
	addFile := func(name string, data []byte, rows int64, desc string) error {
		sha := sha256.Sum256(data)
		if rows > 0 {
			files = append(files, ManifestFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sha[:]), Rows: rows, Description: desc})
		} else {
			files = append(files, ManifestFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sha[:]), Description: desc})
		}
		zw, err := zipw.Create(name)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := zw.Write(data); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		return nil
	}

	// 1. README.md (static).
	if err := addFile(ReadmeName, []byte(s.readme(dev)), 0, "how to open and verify this bundle"); err != nil {
		return nil, err
	}

	// 2. device.json (inventory + config).
	devFile := DeviceFile{
		Schema: deviceSchema,
		Device: DeviceOut{
			ID: dev.ID, Hostname: dev.Hostname, OS: dev.OS, Arch: dev.Arch,
			AgentVersion: dev.AgentVersion,
			Interfaces:   nonNil(dev.Interfaces),
			FirstSeen:    dev.FirstSeen.UTC(), LastSeen: dev.LastSeen.UTC(),
		},
		Config: DeviceConfig{
			MetricIntervalS:    dev.MetricIntS,
			HeartbeatIntervalS: dev.HeartbeatIntS,
			Tags:               nonNil(dev.Tags),
		},
	}
	devJSON, err := json.MarshalIndent(devFile, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal device.json: %w", err)
	}
	if err := addFile(DeviceName, append(devJSON, '\n'), 0, "device inventory + server-side configuration"); err != nil {
		return nil, err
	}

	// 3. metrics.parquet (raw samples, streamed + hashed in one pass).
	metricsRows, err := s.writeParquet(ctx, zipw, MetricsName, deviceID, since, until)
	if err != nil {
		return nil, err
	}
	files = append(files, metricsRows)

	// 4. metrics_1m.parquet (rollups, full history).
	if withRollups && s.rollups != nil {
		rollupEntry, err := s.writeRollups(ctx, zipw, deviceID)
		if err != nil {
			return nil, err
		}
		files = append(files, rollupEntry)
	}

	// 5. alerts.json (complete history).
	alerts := []Alert{}
	if s.alerts != nil {
		alerts, err = s.alerts.List(ctx, deviceID)
		if err != nil {
			return nil, fmt.Errorf("alerts: %w", err)
		}
	}
	alertsFile := AlertsFile{Schema: alertsSchema, Alerts: alerts}
	alertsJSON, err := json.MarshalIndent(alertsFile, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal alerts.json: %w", err)
	}
	if err := addFile(AlertsName, append(alertsJSON, '\n'), int64(len(alerts)), "complete alert history (all statuses)"); err != nil {
		return nil, err
	}

	// 6. manifest.json (last — it describes everything above).
	m := Manifest{
		Format:        FormatName,
		FormatVersion: FormatVersion,
		ExportedAt:    s.now().UTC(),
		GeneratedBy:   s.version,
		Device:        ManifestDevice{ID: dev.ID, Hostname: dev.Hostname},
	}
	if !since.IsZero() || !until.IsZero() {
		m.Range = &ManifestRange{Since: since.UTC(), Until: until.UTC()}
	}
	// The manifest's own entry: the manifest is written INTO the manifest
	// (it cannot carry its own sha256 — self-reference — but its size is
	// knowable): iterate to the fixed point where the recorded size equals
	// the serialized length (converges in <=2 rounds — only the digit
	// count of the size field itself moves).
	entry := ManifestFile{
		Name:        ManifestName,
		Description: "this manifest (the bundle contract; sha256 omitted — self-reference)",
	}
	var manifestJSON []byte
	for i := 0; i < 8; i++ {
		m.Files = append(append([]ManifestFile{}, files...), entry)
		manifestJSON, err = json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal manifest: %w", err)
		}
		if entry.Size == int64(len(manifestJSON)) {
			break
		}
		entry.Size = int64(len(manifestJSON))
	}
	if entry.Size != int64(len(manifestJSON)) {
		return nil, fmt.Errorf("manifest self-size did not converge")
	}
	zw, err := zipw.Create(ManifestName)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", ManifestName, err)
	}
	if _, err := zw.Write(manifestJSON); err != nil {
		return nil, fmt.Errorf("write %s: %w", ManifestName, err)
	}

	if err := zipw.Close(); err != nil {
		return nil, fmt.Errorf("close bundle: %w", err)
	}
	return &m, nil
}

// countingWriter tracks the bytes written (the parquet entry size for the
// manifest; the zip writer only knows the size after Close).
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeParquet streams the device's raw samples into a Parquet entry.
// Returns the manifest entry for it (size + sha256 + row count).
func (s *Service) writeParquet(ctx context.Context, zipw *zip.Writer, name, deviceID string, since, until time.Time) (ManifestFile, error) {
	zw, err := zipw.Create(name)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("create %s: %w", name, err)
	}
	var h hash.Hash = sha256.New()
	counted := &countingWriter{w: io.MultiWriter(zw, h)}
	pw := parquet.NewGenericWriter[MetricRow](counted)

	var buf []MetricRow
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := pw.Write(buf); err != nil {
			return err
		}
		buf = buf[:0]
		return nil
	}
	var n int64
	stream := func(smp Sample) error {
		labels := "{}"
		if len(smp.Labels) > 0 {
			b, err := json.Marshal(smp.Labels)
			if err != nil {
				return fmt.Errorf("labels for %s: %w", smp.Name, err)
			}
			labels = string(b)
		}
		buf = append(buf, MetricRow{
			TimestampMs: smp.TimestampMs,
			TS:          smp.TS.UTC(),
			Name:        smp.Name,
			Source:      smp.Source,
			Value:       smp.Value,
			Labels:      labels,
		})
		n++
		if len(buf) >= parquetBatch {
			return flush()
		}
		return nil
	}
	if s.metrics != nil {
		if _, err := s.metrics.Stream(ctx, deviceID, since, until, stream); err != nil {
			_ = pw.Close()
			return ManifestFile{}, fmt.Errorf("stream %s: %w", name, err)
		}
	}
	if err := flush(); err != nil {
		_ = pw.Close()
		return ManifestFile{}, fmt.Errorf("write %s: %w", name, err)
	}
	if err := pw.Close(); err != nil {
		return ManifestFile{}, fmt.Errorf("close %s: %w", name, err)
	}
	return ManifestFile{Name: name, Size: counted.n, SHA256: hex.EncodeToString(h.Sum(nil)), Rows: n,
		Description: "raw metric samples (timestamp_ms, ts, name, source, value, labels-JSON)"}, nil
}

// writeRollups is the rollup equivalent of writeParquet.
func (s *Service) writeRollups(ctx context.Context, zipw *zip.Writer, deviceID string) (ManifestFile, error) {
	zw, err := zipw.Create(RollupsName)
	if err != nil {
		return ManifestFile{}, fmt.Errorf("create %s: %w", RollupsName, err)
	}
	var h hash.Hash = sha256.New()
	counted := &countingWriter{w: io.MultiWriter(zw, h)}
	pw := parquet.NewGenericWriter[RollupRow](counted)

	var buf []RollupRow
	var n int64
	_, err = s.rollups.Stream(ctx, deviceID, func(r Rollup) error {
		buf = append(buf, RollupRow{Bucket: r.Bucket.UTC(), Name: r.Name, Source: r.Source,
			Avg: r.Avg, Min: r.Min, Max: r.Max, N: r.N})
		n++
		if len(buf) >= parquetBatch {
			if _, err := pw.Write(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
		return nil
	})
	if err != nil {
		_ = pw.Close()
		return ManifestFile{}, fmt.Errorf("stream %s: %w", RollupsName, err)
	}
	if len(buf) > 0 {
		if _, err := pw.Write(buf); err != nil {
			_ = pw.Close()
			return ManifestFile{}, fmt.Errorf("write %s: %w", RollupsName, err)
		}
	}
	if err := pw.Close(); err != nil {
		return ManifestFile{}, fmt.Errorf("close %s: %w", RollupsName, err)
	}
	return ManifestFile{Name: RollupsName, Size: counted.n, SHA256: hex.EncodeToString(h.Sum(nil)), Rows: n,
		Description: "1-minute rollups (bucket, name, source, avg_value, min_value, max_value, n)"}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// readme renders the in-bundle README.md.
func (s *Service) readme(dev *store.Device) string {
	return fmt.Sprintf(`# RMMWay client export

Self-describing bundle exported by %s on %s for device %s (%s).
The contract is in manifest.json: every file's size, sha256 and row count.

## Contents

| file               | what                                                        | open with            |
|--------------------|-------------------------------------------------------------|----------------------|
| manifest.json      | bundle contract + per-file sha256/size/rows                 | any JSON tool        |
| device.json        | inventory + server-side configuration                       | any JSON tool        |
| metrics.parquet    | raw samples: timestamp_ms, ts, name, source, value, labels  | duckdb, pandas, ...  |
| metrics_1m.parquet | 1-min rollups: bucket, name, source, avg/min/max, n         | duckdb, pandas, ...  |
| alerts.json        | complete alert history (all statuses)                       | any JSON tool        |
| README.md          | this file                                                   | —                    |

## Open with a standard tool

    # duckdb
    SELECT * FROM 'metrics.parquet'
    WHERE name = 'cpu.utilization_percent'
    ORDER BY ts DESC LIMIT 100;

    # pandas
    import pandas as pd
    df = pd.read_parquet('metrics.parquet')

    # polars
    import polars as pl
    df = pl.read_parquet('metrics.parquet')

Notes:
- labels is a JSON object serialized as a string column (Parquet has no
  JSON type); parse it with your usual JSON parser.
- ts is a nanosecond-precision timestamp (datetime64[ns] in pandas,
  TIMESTAMP in duckdb); timestamp_ms is the raw agent wall clock.
- metrics_1m.parquet is a continuous aggregate: it can lag the newest raw
  samples by a few minutes, and covers history beyond the raw retention
  window.

## Verify integrity

Every file's sha256 + size is in manifest.json (the manifest itself is
exempt — a file cannot carry its own hash). Recompute:

    jq -r '.files[] | select(.sha256 != null) | "\(.sha256)  \(.name)"' manifest.json | sha256sum -c -

## Re-import

The Parquet schema is flat and stable (column names above; see
manifest.json files[].description). Re-import is a plain bulk load, e.g.
in duckdb:

    CREATE TABLE metrics AS SELECT * FROM 'metrics.parquet';

The bundle is portable data, not an RMMWay database dump.
`, s.version, s.now().UTC().Format(time.RFC3339), dev.ID, dev.Hostname)
}
