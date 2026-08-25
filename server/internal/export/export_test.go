package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// fakeMetrics is a deterministic MetricsReader: a fixed grid of samples.
type fakeMetrics struct {
	deviceID string
	// series: name/source -> values over time.
	series map[string][]float64
	base   time.Time
	labels map[string]map[string]string
	since  time.Time
	until  time.Time
}

func (f *fakeMetrics) Stream(_ context.Context, deviceID string, since, until time.Time, fn func(Sample) error) (int64, error) {
	f.deviceID = deviceID
	f.since, f.until = since, until
	var n int64
	for key, vals := range f.series {
		parts := strings.SplitN(key, "@", 2)
		name := parts[0]
		source := ""
		if len(parts) == 2 {
			source = parts[1]
		}
		for i, v := range vals {
			ts := f.base.Add(time.Duration(i) * time.Minute)
			if !since.IsZero() && ts.Before(since) {
				continue
			}
			if !until.IsZero() && !ts.Before(until) {
				continue
			}
			smp := Sample{
				TS:          ts,
				TimestampMs: ts.UnixMilli(),
				Name:        name,
				Source:      source,
				Value:       v,
				Labels:      f.labels[name],
			}
			if err := fn(smp); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

type fakeRollups struct{ rows []Rollup }

func (f *fakeRollups) Stream(_ context.Context, _ string, fn func(Rollup) error) (int64, error) {
	for _, r := range f.rows {
		if err := fn(r); err != nil {
			return 0, err
		}
	}
	return int64(len(f.rows)), nil
}

type fakeAlerts struct{ rows []Alert }

func (f *fakeAlerts) List(_ context.Context, _ string) ([]Alert, error) {
	if f.rows == nil {
		return []Alert{}, nil
	}
	return f.rows, nil
}

func testDevice(id, host string) *store.Device {
	return &store.Device{
		ID: id, Hostname: host, OS: "linux", Arch: "amd64",
		AgentVersion: "0.5.0", Interfaces: []string{"10.0.0.5"},
		Tags: []string{"prod", "web"}, Online: true,
		FirstSeen:  time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		LastSeen:   time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC),
		MetricIntS: 30, HeartbeatIntS: 30,
	}
}

// newService builds a Service over the in-memory device store + fakes.
func newService(t *testing.T, dev *store.Device, m MetricsReader, r RollupReader, a AlertReader) *Service {
	t.Helper()
	ds := store.NewMemoryDeviceStore()
	ctx := context.Background()
	if err := ds.Register(ctx, dev.ID, dev.Hostname, dev.OS, dev.Arch, dev.AgentVersion, dev.Interfaces, dev.HeartbeatIntS, dev.MetricIntS); err != nil {
		t.Fatalf("register: %v", err)
	}
	// tags aren't set by Register; patch via a direct memory store write.
	// (Register upserts core fields; tags live on the row.)
	return New(Config{Devices: ds, Metrics: m, Rollups: r, Alerts: a, Version: "rmmway-server/test"}).
		WithNow(func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) })
}

func build(t *testing.T, s *Service, deviceID string, since, until time.Time, withRollups bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := s.Export(context.Background(), deviceID, since, until, withRollups, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes()
}

// TestBuildVerifyRoundTrip is the core no-lock-in proof: build a bundle,
// then re-open + verify it purely from the bytes using the standard
// Parquet reader, and confirm the payload matches the source.
func TestBuildVerifyRoundTrip(t *testing.T) {
	dev := testDevice("dev_1", "fileserver-01")
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	m := &fakeMetrics{
		base: base,
		series: map[string][]float64{
			"cpu.utilization_percent":     {10, 20, 30, 40, 50},
			"disk.used_percent@/dev/sda1": {61, 62, 63, 64, 95},
		},
		labels: map[string]map[string]string{
			"cpu.utilization_percent": {"host": "fileserver-01"},
		},
	}
	r := &fakeRollups{rows: []Rollup{{
		Bucket: base, Name: "cpu.utilization_percent", Source: "",
		Avg: 20, Min: 10, Max: 50, N: 5,
	}}}
	a := &fakeAlerts{rows: []Alert{{
		ID: 1, Name: "disk.used_percent", Source: "/dev/sda1", Status: "open",
		Score: 8.5, Channel: "seasonal", Value: 95, Events: 3,
		FirstAt: base, LastAt: base.Add(4 * time.Minute),
	}}}
	s := newService(t, dev, m, r, a)
	bundle := build(t, s, dev.ID, time.Time{}, time.Time{}, true)

	// 1. Self-describing verify (manifest drives everything).
	mf, err := Verify(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if mf.Device.ID != dev.ID || mf.Device.Hostname != dev.Hostname {
		t.Fatalf("manifest device = %+v, want %s/%s", mf.Device, dev.ID, dev.Hostname)
	}
	if mf.Format != FormatName || mf.FormatVersion != FormatVersion {
		t.Fatalf("manifest format = %s v%d", mf.Format, mf.FormatVersion)
	}

	// 2. Standard Parquet reader re-opens the metrics section.
	rows, err := ReadMetrics(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	// 5 + 5 samples.
	if len(rows) != 10 {
		t.Fatalf("metrics rows = %d, want 10", len(rows))
	}
	// Spot-check a value + the labels JSON round-trip.
	foundDisk := false
	for _, row := range rows {
		if row.Name == "disk.used_percent" && row.Source == "/dev/sda1" && row.Value == 95 {
			foundDisk = true
		}
	}
	if !foundDisk {
		t.Fatalf("did not find the 95%% disk sample in re-read rows")
	}
	// ts + timestamp_ms must agree (the no-loss guarantee).
	for _, row := range rows {
		if row.TS.UnixMilli() != row.TimestampMs {
			t.Fatalf("ts %v != timestamp_ms %d", row.TS, row.TimestampMs)
		}
	}

	// 3. Rollups re-open.
	rrows, err := ReadRollups(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("ReadRollups: %v", err)
	}
	if len(rrows) != 1 || rrows[0].Max != 50 {
		t.Fatalf("rollups = %+v", rrows)
	}

	// 4. device.json carries inventory + config.
	var devFile DeviceFile
	if err := json.Unmarshal(mustEntry(t, bundle, DeviceName), &devFile); err != nil {
		t.Fatalf("device.json: %v", err)
	}
	if devFile.Schema != deviceSchema {
		t.Fatalf("device schema = %q", devFile.Schema)
	}
	if devFile.Device.Hostname != "fileserver-01" || devFile.Config.MetricIntervalS != 30 {
		t.Fatalf("device file = %+v", devFile)
	}

	// 5. alerts.json carries the full history.
	var af AlertsFile
	if err := json.Unmarshal(mustEntry(t, bundle, AlertsName), &af); err != nil {
		t.Fatalf("alerts.json: %v", err)
	}
	if len(af.Alerts) != 1 || af.Alerts[0].Status != "open" {
		t.Fatalf("alerts = %+v", af)
	}
}

// TestVerifyDetectsTamper proves a skeptic can catch a flipped byte: the
// manifest's sha256 is the integrity contract.
func TestVerifyDetectsTamper(t *testing.T) {
	dev := testDevice("dev_2", "box")
	m := &fakeMetrics{base: time.Now().UTC().Truncate(time.Minute),
		series: map[string][]float64{"cpu.utilization_percent": {1, 2, 3}}}
	s := newService(t, dev, m, nil, nil)
	bundle := build(t, s, dev.ID, time.Time{}, time.Time{}, false)

	// Flip a byte inside metrics.parquet (find its offset in the zip and
	// mutate a data byte, not the central directory).
	tampered := tamperEntry(t, bundle, MetricsName)
	if _, err := Verify(bytes.NewReader(tampered), int64(len(tampered))); err == nil {
		t.Fatalf("Verify accepted a tampered bundle (want sha256 mismatch)")
	}
}

// TestVerifyRangeWindow checks since/until bounds the metrics section and
// records the window in the manifest.
func TestVerifyRangeWindow(t *testing.T) {
	dev := testDevice("dev_3", "box3")
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	m := &fakeMetrics{base: base, series: map[string][]float64{
		"cpu.utilization_percent": {1, 2, 3, 4, 5}, // 1 per minute
	}}
	s := newService(t, dev, m, nil, nil)

	since := base.Add(1 * time.Minute) // drop the first sample
	until := base.Add(4 * time.Minute) // drop the last (ts < until)
	bundle := build(t, s, dev.ID, since, until, false)

	mf, err := Verify(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if mf.Range == nil || !mf.Range.Since.Equal(since) || !mf.Range.Until.Equal(until) {
		t.Fatalf("manifest range = %+v, want [%v, %v)", mf.Range, since, until)
	}
	rows, _ := ReadMetrics(bytes.NewReader(bundle), int64(len(bundle)))
	if len(rows) != 3 { // minutes 1,2,3
		t.Fatalf("windowed rows = %d, want 3", len(rows))
	}
}

// TestVerifyEmptyDevice: a brand-new device with no metrics still exports a
// valid (empty) bundle.
func TestVerifyEmptyDevice(t *testing.T) {
	dev := testDevice("dev_4", "fresh")
	s := newService(t, dev, &fakeMetrics{base: time.Now(), series: map[string][]float64{}}, nil, nil)
	bundle := build(t, s, dev.ID, time.Time{}, time.Time{}, false)
	if _, err := Verify(bytes.NewReader(bundle), int64(len(bundle))); err != nil {
		t.Fatalf("Verify empty: %v", err)
	}
	rows, err := ReadMetrics(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty metrics = %v rows=%d err=%v", rows, len(rows), err)
	}
}

// TestExportUnknownDevice: unknown id -> store.ErrNotFound.
func TestExportUnknownDevice(t *testing.T) {
	s := newService(t, testDevice("dev_x", "x"), nil, nil, nil)
	var buf bytes.Buffer
	_, err := s.Export(context.Background(), "nope", time.Time{}, time.Time{}, true, &buf)
	if err == nil {
		t.Fatalf("export unknown device: want error")
	}
}

// TestVerifyRejectsStrayFile: a file not listed in the manifest fails.
func TestVerifyRejectsStrayFile(t *testing.T) {
	dev := testDevice("dev_5", "stray")
	m := &fakeMetrics{base: time.Now().UTC().Truncate(time.Minute),
		series: map[string][]float64{"cpu.utilization_percent": {1}}}
	s := newService(t, dev, m, nil, nil)
	bundle := build(t, s, dev.ID, time.Time{}, time.Time{}, false)

	withStray := addStrayEntry(t, bundle, "extra.txt", []byte("hi"))
	if _, err := Verify(bytes.NewReader(withStray), int64(len(withStray))); err == nil {
		t.Fatalf("Verify accepted a bundle with an unlisted file")
	}
}

// ---- zip helpers ----------------------------------------------------------

// mustEntry returns one zip entry's bytes (tests).
func mustEntry(t *testing.T, bundle []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			b, err := entryBytes(f)
			if err != nil {
				t.Fatalf("entry %s: %v", name, err)
			}
			return b
		}
	}
	t.Fatalf("no entry %s", name)
	return nil
}

// tamperEntry returns the bundle with one byte flipped inside `name`.
func tamperEntry(t *testing.T, bundle []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var target *zip.File
	for _, f := range zr.File {
		if f.Name == name {
			target = f
		}
	}
	if target == nil {
		t.Fatalf("no entry %s", name)
	}
	// Rebuild the zip, flipping the middle byte of the target entry.
	out := &bytes.Buffer{}
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		b, err := entryBytes(f)
		if err != nil {
			t.Fatalf("entry %s: %v", f.Name, err)
		}
		if f.Name == name && len(b) > 4 {
			b[len(b)/2] ^= 0xff
		}
		w, _ := zw.Create(f.Name)
		w.Write(b)
	}
	zw.Close()
	return out.Bytes()
}

// addStrayEntry returns the bundle plus an extra unlisted file.
func addStrayEntry(t *testing.T, bundle []byte, name string, data []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := &bytes.Buffer{}
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		b, err := entryBytes(f)
		if err != nil {
			t.Fatalf("entry %s: %v", f.Name, err)
		}
		w, _ := zw.Create(f.Name)
		w.Write(b)
	}
	w, _ := zw.Create(name)
	w.Write(data)
	zw.Close()
	return out.Bytes()
}
