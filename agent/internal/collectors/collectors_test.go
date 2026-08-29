package collectors

import (
	"context"
	"errors"
	"testing"
	"time"
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
