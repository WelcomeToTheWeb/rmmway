package collectors

import (
	"context"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcStat is one process-name aggregate: cumulative CPU seconds across all
// processes with that name, and total RSS. Aggregating by name (not PID)
// keeps the family bounded and stable across forks/restarts.
type ProcStat struct {
	Name    string
	CPUSecs float64
	RSS     uint64
}

// ProcSampler returns per-name process aggregates. The default enumerates
// the host's processes via gopsutil (pure-Go /proc on Linux); tests inject
// a fake list.
type ProcSampler func(ctx context.Context) ([]ProcStat, error)

// defaultProcSampler is the production ProcSampler.
func defaultProcSampler(ctx context.Context) ([]ProcStat, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	agg := map[string]*ProcStat{}
	for _, p := range procs {
		name, err := p.Name()
		if err != nil || name == "" {
			continue
		}
		var cpuSecs float64
		if ts, err := p.Times(); err == nil {
			cpuSecs = ts.User + ts.System + ts.Nice + ts.Iowait + ts.Irq +
				ts.Softirq + ts.Steal + ts.Guest + ts.GuestNice
		}
		var rss uint64
		if mi, err := p.MemoryInfo(); err == nil {
			rss = mi.RSS
		}
		e, ok := agg[name]
		if !ok {
			e = &ProcStat{Name: name}
			agg[name] = e
		}
		e.CPUSecs += cpuSecs
		e.RSS += rss
	}
	out := make([]ProcStat, 0, len(agg))
	for _, e := range agg {
		out = append(out, *e)
	}
	return out, nil
}

// maxTopProcs caps RMMWAY_TOP_PROCS so an env typo cannot fan out into an
// unbounded number of per-process samples per heartbeat.
const maxTopProcs = 50

// defaultTopProcs is the top-N when RMMWAY_TOP_PROCS is unset.
const defaultTopProcs = 10

// procPrev is one name's previous sample for CPU-rate derivation.
type procPrev struct {
	cpuSecs float64
	at      time.Time
}

// emitTopProcs appends the top-N process families. CPU% is derived from
// deltas between consecutive Collect calls (gopsutil's own Percent needs
// two samples internally; a 30 s heartbeat would otherwise see zeros).
// The first collect has no delta, so process.cpu_percent is omitted and
// the top-N ranks by RSS.
func (c *defaultCollector) emitTopProcs(stats []ProcStat, add func(name, source string, value float64)) {
	now := c.now()
	n := c.topN
	if n <= 0 {
		n = defaultTopProcs
	}
	if n > maxTopProcs {
		n = maxTopProcs
	}

	type entry struct {
		name   string
		cpuPct float64
		hasPct bool
		rss    uint64
	}
	entries := make([]entry, 0, len(stats))

	c.procMu.Lock()
	prev := c.procPrev
	c.procPrev = make(map[string]procPrev, len(stats))
	for _, st := range stats {
		e := entry{name: st.Name, rss: st.RSS}
		if p, ok := prev[st.Name]; ok {
			if dt := now.Sub(p.at).Seconds(); dt > 0 {
				if pct := (st.CPUSecs - p.cpuSecs) / dt * 100; pct > 0 {
					e.cpuPct = pct
					e.hasPct = true
				}
			}
		}
		c.procPrev[st.Name] = procPrev{cpuSecs: st.CPUSecs, at: now}
		entries = append(entries, e)
	}
	c.procMu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.cpuPct != b.cpuPct {
			return a.cpuPct > b.cpuPct
		}
		if a.rss != b.rss {
			return a.rss > b.rss
		}
		return a.name < b.name
	})
	if len(entries) > n {
		entries = entries[:n]
	}
	for _, e := range entries {
		if e.hasPct {
			add("process.cpu_percent", e.name, e.cpuPct)
		}
		add("process.memory_rss_bytes", e.name, float64(e.rss))
	}
}
