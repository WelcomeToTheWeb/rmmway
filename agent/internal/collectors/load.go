package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/load"
)

// LoadSampler reports the host's 1/5/15-minute load averages. The default
// is gopsutil's load.AvgWithContext (pure-Go /proc/loadavg on Linux); tests
// inject a fake so no host state is read.
type LoadSampler func(ctx context.Context) (load1, load5, load15 float64, err error)

// defaultLoadSampler is the production LoadSampler.
func defaultLoadSampler(ctx context.Context) (float64, float64, float64, error) {
	stat, err := load.AvgWithContext(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	return stat.Load1, stat.Load5, stat.Load15, nil
}

// emitLoad appends the load-average families. Values that are negative or
// zero (platforms that cannot report an average) emit no sample — a
// missing sample reads as "unknown", never as "the box is idle".
func (c *defaultCollector) emitLoad(ctx context.Context, add func(name, source string, value float64)) error {
	if c.load == nil {
		return nil
	}
	l1, l5, l15, err := c.load(ctx)
	if err != nil {
		return err
	}
	if l1 > 0 {
		add("load.avg1", "", l1)
	}
	if l5 > 0 {
		add("load.avg5", "", l5)
	}
	if l15 > 0 {
		add("load.avg15", "", l15)
	}
	return nil
}

// emitSwap appends swap.used_percent from the memory sample Collect already
// took (no second probe). Hosts with no swap partition emit nothing.
// gopsutil v4's VirtualMemoryStat has no precomputed used-percent, so it is
// derived from total/free here.
func emitSwap(swapTotal, swapFree uint64, add func(name, source string, value float64)) {
	if swapTotal == 0 {
		return
	}
	used := swapTotal - swapFree
	add("swap.used_percent", "", float64(used)/float64(swapTotal)*100)
}
