package collectors

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/mem"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// fakeCPU returns canned values or an error, so tests never sleep.
func fakeCPU(vals []float64, err error) cpuMeasure {
	return func(interval time.Duration, percpu bool) ([]float64, error) {
		return vals, err
	}
}

func TestCollectProducesAllFiveFamilies(t *testing.T) {
	c := NewCollectorWithCPU(fakeCPU([]float64{42.5}, nil))
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if batch.CollectedAtMs <= 0 {
		t.Fatalf("missing collected_at_ms: %+v", batch)
	}
	if len(batch.Samples) == 0 {
		t.Fatal("no samples collected")
	}

	// Assert each of the five families is present (with at least one sample).
	want := map[string]string{
		"cpu.utilization_percent": "",
		"memory.used_percent":     "",
		"disk.used_percent":       "",
		"net.bytes_total":         "",
		"system.uptime_seconds":   "",
	}
	got := map[string]bool{}
	for _, s := range batch.Samples {
		got[s.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("missing family %q in %+v", name, got)
		}
	}

	// CPU value must be the injected one (proves the sampler is wired in).
	for _, s := range batch.Samples {
		if s.Name == "cpu.utilization_percent" && s.Value != 42.5 {
			t.Fatalf("cpu: got %g want 42.5", s.Value)
		}
		if s.TimestampMs != batch.CollectedAtMs {
			t.Fatalf("sample %s timestamp %d != batch %d", s.Name, s.TimestampMs, batch.CollectedAtMs)
		}
	}
}

func TestCollectPartialFailureStillEmitsBatch(t *testing.T) {
	// CPU fails, but the other four families must still be delivered.
	c := NewCollectorWithCPU(fakeCPU(nil, errors.New("cpu probe down")))
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when cpu fails")
	}
	if len(batch.Samples) == 0 {
		t.Fatal("expected samples from the working families even when cpu fails")
	}
	got := map[string]bool{}
	for _, s := range batch.Samples {
		got[s.Name] = true
	}
	for _, want := range []string{"memory.used_percent", "system.uptime_seconds", "disk.used_percent"} {
		if !got[want] {
			t.Fatalf("expected %q in partial batch, got %v", want, got)
		}
	}
	if got["cpu.utilization_percent"] {
		t.Fatal("cpu must be absent when the sampler fails")
	}
}

// fakeService returns canned statuses per service name; names in errFor
// error, names in neither map are ErrServiceUnknown.
func fakeService(statuses map[string]float64, errFor map[string]error) ServiceSampler {
	return func(_ context.Context, name string) (float64, error) {
		if e, ok := errFor[name]; ok {
			return 0, e
		}
		if v, ok := statuses[name]; ok {
			return v, nil
		}
		return 0, ErrServiceUnknown
	}
}

// serviceSamples isolates the service.status family from a batch by source.
func serviceSamples(batch *agentv1.MetricBatch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range batch.Samples {
		if s.Name == "service.status" {
			out[s.Source] = s.Value
		}
	}
	return out
}

// TestServiceStatusFamily (wave1 A gap5) pins the wire contract the seeded
// "service.down" playbook detects on: name service.status, source =
// service name, value 1 running / 0 stopped.
func TestServiceStatusFamily(t *testing.T) {
	c := NewCollectorWithCPUServices(
		fakeCPU([]float64{1}, nil),
		[]string{"nginx", "postgresql", "ghost"},
		fakeService(map[string]float64{"nginx": 1, "postgresql": 0}, nil),
	)
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := serviceSamples(batch)
	if got["nginx"] != 1 {
		t.Fatalf("nginx: got %v want 1 (running)", got["nginx"])
	}
	if got["postgresql"] != 0 {
		t.Fatalf("postgresql: got %v want 0 (stopped)", got["postgresql"])
	}
	if _, ok := got["ghost"]; ok {
		t.Fatal("unknown service must emit no sample")
	}
}

// TestServiceStatusPartialFailure: one failing sampler degrades to a
// partial error exactly like any other family — the other services' samples
// still ship, the failed one is absent.
func TestServiceStatusPartialFailure(t *testing.T) {
	c := NewCollectorWithCPUServices(
		fakeCPU([]float64{1}, nil),
		[]string{"ok", "bad"},
		fakeService(map[string]float64{"ok": 1}, map[string]error{"bad": errors.New("scm probe failed")}),
	)
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when a service sampler fails")
	}
	got := serviceSamples(batch)
	if got["ok"] != 1 {
		t.Fatalf("working service must still ship: %v", got)
	}
	if _, ok := got["bad"]; ok {
		t.Fatal("failed service must emit no sample")
	}
}

// TestServiceStatusEmptyAllowlist: unset RMMWAY_SERVICES (nil list) adds no
// samples and no partial error — the default five families are unchanged.
func TestServiceStatusEmptyAllowlist(t *testing.T) {
	c := NewCollectorWithCPUServices(fakeCPU([]float64{1}, nil), nil, nil)
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect with no services: %v", err)
	}
	if got := serviceSamples(batch); len(got) != 0 {
		t.Fatalf("expected no service samples, got %v", got)
	}
}

func TestParseServiceList(t *testing.T) {
	if got := parseServiceList(""); got != nil {
		t.Fatalf("empty: got %v want nil", got)
	}
	if got := parseServiceList("   "); got != nil {
		t.Fatalf("blank: got %v want nil", got)
	}
	got := parseServiceList(" nginx, postgresql ,,redis ,nginx ")
	want := []string{"nginx", "postgresql", "redis"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order-preserving trim/dedupe: got %v want %v", got, want)
		}
	}
	big := make([]string, 60)
	for i := range big {
		big[i] = string(rune('a'+i/26)) + string(rune('0'+i%26)) + string(rune('A'+i%23))
	}
	if got := parseServiceList(strings.Join(big, ",")); len(got) != maxMonitoredServices {
		t.Fatalf("cap: got %d entries want %d", len(got), maxMonitoredServices)
	}
}

// fakeLoad returns canned 1/5/15-min averages or an error.
func fakeLoad(l1, l5, l15 float64, err error) LoadSampler {
	return func(context.Context) (float64, float64, float64, error) {
		return l1, l5, l15, err
	}
}

// loadSamples isolates the load.avg* families from a batch.
func loadSamples(batch *agentv1.MetricBatch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "load.avg") {
			out[s.Name] = s.Value
		}
	}
	return out
}

func TestLoadAvgFamily(t *testing.T) {
	c := NewCollectorWithCPUServicesLoad(
		fakeCPU([]float64{1}, nil), nil, nil, fakeLoad(1.5, 2.5, 3.5, nil))
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := loadSamples(batch)
	if got["load.avg1"] != 1.5 || got["load.avg5"] != 2.5 || got["load.avg15"] != 3.5 {
		t.Fatalf("load averages: got %v", got)
	}
}

// TestLoadAvgOmitsUnreported: values the platform cannot report (0 or
// negative) emit no sample rather than reading as "idle".
func TestLoadAvgOmitsUnreported(t *testing.T) {
	c := NewCollectorWithCPUServicesLoad(
		fakeCPU([]float64{1}, nil), nil, nil, fakeLoad(0, -1, 4.0, nil))
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := loadSamples(batch)
	if len(got) != 1 || got["load.avg15"] != 4.0 {
		t.Fatalf("only load.avg15 should ship: %v", got)
	}
}

// TestLoadAvgPartialFailure: a failing probe degrades to a partial error
// exactly like any other family — the rest of the batch still ships.
func TestLoadAvgPartialFailure(t *testing.T) {
	c := NewCollectorWithCPUServicesLoad(
		fakeCPU([]float64{1}, nil), nil, nil, fakeLoad(0, 0, 0, errors.New("load probe down")))
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when the load sampler fails")
	}
	if got := loadSamples(batch); len(got) != 0 {
		t.Fatalf("failing load sampler must emit no samples: %v", got)
	}
	if len(batch.Samples) == 0 {
		t.Fatal("other families must still ship on a load failure")
	}
}

func TestSwapUsedPercent(t *testing.T) {
	var got map[string]float64
	add := func(name, source string, value float64) {
		if got == nil {
			got = map[string]float64{}
		}
		got[name] = value
	}
	// 50% of a 8 GiB swap partition used.
	emitSwap(8192, 4096, add)
	if got["swap.used_percent"] != 50 {
		t.Fatalf("swap: got %v want 50", got["swap.used_percent"])
	}
	// No swap on the host: no sample at all.
	got = nil
	emitSwap(0, 0, add)
	if got != nil {
		t.Fatalf("no-swap host must emit no sample: %v", got)
	}
}

// TestSwapShipsFromMemoryProbe: the swap sample rides the memory probe —
// an injected memory sampler with swap must surface swap.used_percent, and
// a memory failure must drop both memory and swap samples.
func TestSwapShipsFromMemoryProbe(t *testing.T) {
	c := NewCollectorWithSamplers(Samplers{
		CPU: fakeCPU([]float64{1}, nil),
		Memory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{UsedPercent: 42, SwapTotal: 2048, SwapFree: 512}, nil
		},
	})
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, s := range batch.Samples {
		got[s.Name] = s.Value
	}
	if got["swap.used_percent"] != 75 {
		t.Fatalf("swap: got %v want 75", got["swap.used_percent"])
	}

	c = NewCollectorWithSamplers(Samplers{
		CPU:    fakeCPU([]float64{1}, nil),
		Memory: func(context.Context) (*mem.VirtualMemoryStat, error) { return nil, errors.New("mem down") },
	})
	batch, err = c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when the memory sampler fails")
	}
	for _, s := range batch.Samples {
		if s.Name == "memory.used_percent" || s.Name == "swap.used_percent" {
			t.Fatalf("failing memory probe must drop %s", s.Name)
		}
	}
}
