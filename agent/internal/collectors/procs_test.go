package collectors

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

func fakeProcs(stats []ProcStat, err error) ProcSampler {
	return func(context.Context) ([]ProcStat, error) { return stats, err }
}

// procFamilies isolates the process.* families from a batch.
func procFamilies(batch *agentv1.MetricBatch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "process.") {
			out[s.Name+"@"+s.Source] = s.Value
		}
	}
	return out
}

// TestTopProcsByCPU: collects 60 s apart; CPU seconds advanced by 30 on
// "hot" (50% of one core), 10 on "warm" (16.7%), 0 on "cold".
func TestTopProcsByCPU(t *testing.T) {
	base := time.Now()
	nowVal := base
	hotCPU, warmCPU, coldCPU := 100.0, 50.0, 10.0
	c := NewCollectorWithSamplers(Samplers{
		CPU:  fakeCPU([]float64{1}, nil),
		Now:  func() time.Time { return nowVal },
		TopN: 10,
		Procs: func(context.Context) ([]ProcStat, error) {
			return []ProcStat{
				{Name: "hot", CPUSecs: hotCPU, RSS: 10 << 20},
				{Name: "warm", CPUSecs: warmCPU, RSS: 5 << 20},
				{Name: "cold", CPUSecs: coldCPU, RSS: 1 << 20},
			}, nil
		},
	}).(*defaultCollector)

	// First collect: no previous state — RSS only, no process.cpu_percent.
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	got := procFamilies(batch)
	if len(got) != 3 {
		t.Fatalf("first collect should emit 3 RSS samples only: %v", got)
	}
	for name := range got {
		if strings.HasPrefix(name, "process.cpu_percent") {
			t.Fatalf("first collect must not emit cpu%%: %v", got)
		}
	}

	// Advance 60 s: hot used 30 s (50%), warm 10 s (16.7%), cold 0.
	nowVal = base.Add(60 * time.Second)
	hotCPU += 30
	warmCPU += 10
	batch, err = c.Collect(context.Background())
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	got = procFamilies(batch)
	if v := got["process.cpu_percent@hot"]; v <= 49 || v >= 51 {
		t.Errorf("hot cpu%%: got %v want ~50", v)
	}
	if v := got["process.cpu_percent@warm"]; v <= 15 || v >= 18 {
		t.Errorf("warm cpu%%: got %v want ~16.7", v)
	}
	if _, ok := got["process.cpu_percent@cold"]; ok {
		t.Errorf("cold (0%% CPU) must not emit a cpu sample: %v", got)
	}
	if got["process.memory_rss_bytes@hot"] != float64(10<<20) {
		t.Errorf("hot rss: got %v", got["process.memory_rss_bytes@hot"])
	}
}

// TestTopProcsCap: N=2 of 5 processes → exactly 2 names, ranked by RSS
// (no CPU deltas on the first collect).
func TestTopProcsCap(t *testing.T) {
	c := NewCollectorWithSamplers(Samplers{
		CPU:  fakeCPU([]float64{1}, nil),
		TopN: 2,
		Procs: fakeProcs([]ProcStat{
			{Name: "a", CPUSecs: 0, RSS: 1},
			{Name: "b", CPUSecs: 0, RSS: 5},
			{Name: "c", CPUSecs: 0, RSS: 3},
			{Name: "d", CPUSecs: 0, RSS: 4},
			{Name: "e", CPUSecs: 0, RSS: 2},
		}, nil),
	}).(*defaultCollector)

	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := procFamilies(batch)
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 RSS samples (N=2), got %v", got)
	}
	if _, ok := got["process.memory_rss_bytes@b"]; !ok {
		t.Errorf("top by RSS should include b (5): %v", got)
	}
	if _, ok := got["process.memory_rss_bytes@d"]; !ok {
		t.Errorf("top by RSS should include d (4): %v", got)
	}
}

// TestTopProcsDefaultCap: TopN unset → default 10.
func TestTopProcsDefaultCap(t *testing.T) {
	stats := make([]ProcStat, 15)
	for i := range stats {
		stats[i] = ProcStat{Name: string(rune('a' + i)), RSS: uint64(15 - i)}
	}
	c := NewCollectorWithSamplers(Samplers{
		CPU:   fakeCPU([]float64{1}, nil),
		Procs: fakeProcs(stats, nil),
	}).(*defaultCollector)

	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := procFamilies(batch); len(got) != defaultTopProcs {
		t.Fatalf("expected %d samples (default N), got %d", defaultTopProcs, len(got))
	}
}

func TestTopProcsPartialFailure(t *testing.T) {
	c := NewCollectorWithSamplers(Samplers{
		CPU:   fakeCPU([]float64{1}, nil),
		Procs: fakeProcs(nil, errors.New("process table down")),
	})
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when the process sampler fails")
	}
	if got := procFamilies(batch); len(got) != 0 {
		t.Fatalf("failing process sampler must emit no samples: %v", got)
	}
	if len(batch.Samples) == 0 {
		t.Fatal("other families must still ship on a process failure")
	}
}
