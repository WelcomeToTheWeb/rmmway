// Package collectors implements the W1-2 core collectors: the five metric
// families the agent emits over the wire.
//
//	Family                     source
//	cpu.utilization_percent    ""  (host-wide, 0–100)
//	memory.used_percent        ""  (0–100, excluding cached/buffered)
//	disk.used_percent          <device> (per mounted volume, 0–100)
//	net.bytes_total            <iface> (total rx+tx bytes since boot)
//	system.uptime_seconds      ""
//
// Implementation: gopsutil/v4 (pure-Go on Linux — reads /proc directly, so
// the static-binary property from W1-1 is preserved). CPU utilization is
// measured over a short blocking window (cpu.Percent(interval)), which is
// deterministic and free of per-core summation drift.
package collectors

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Collector samples all five metric families once.
type Collector interface {
	Collect(ctx context.Context) (*agentv1.MetricBatch, error)
}

// cpuMeasure abstracts the (blocking) CPU utilization sample so tests can
// inject a fake without sleeping.
type cpuMeasure func(interval time.Duration, percpu bool) ([]float64, error)

type defaultCollector struct {
	cpu cpuMeasure
}

// NewCollector returns the production collector (real gopsutil CPU window).
func NewCollector() Collector {
	return &defaultCollector{cpu: cpu.Percent}
}

// NewCollectorWithCPU returns a collector with an injected CPU sampler
// (used by tests to avoid the real sleep window).
func NewCollectorWithCPU(sample cpuMeasure) Collector {
	return &defaultCollector{cpu: sample}
}

// Collect samples every family and packages them as one MetricBatch.
// Partial failures degrade gracefully: a family that errors contributes no
// samples rather than failing the whole push, so the server still gets the
// families that worked (an RMM that goes blind on one probe is worse than a
// partial metric).
func (c *defaultCollector) Collect(ctx context.Context) (*agentv1.MetricBatch, error) {
	batch := &agentv1.MetricBatch{CollectedAtMs: time.Now().UnixMilli()}
	var errs []string

	add := func(name, source string, value float64) {
		batch.Samples = append(batch.Samples, &agentv1.Metric{
			Name:        name,
			Source:      source,
			Value:       value,
			TimestampMs: batch.CollectedAtMs,
		})
	}

	// 1. CPU — one blocking window (50ms keeps the push cadence snappy;
	// the server dedupes by timestamp so a slightly wide window is fine).
	pcts, err := c.cpu(50*time.Millisecond, false)
	if err != nil {
		errs = append(errs, "cpu: "+err.Error())
	} else if len(pcts) > 0 {
		add("cpu.utilization_percent", "", pcts[0])
	}

	// 2. Memory — used_percent excludes buffers/cache (what RMM cares about).
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		errs = append(errs, "memory: "+err.Error())
	} else {
		add("memory.used_percent", "", vm.UsedPercent)
	}

	// 3. Disk — per mounted volume, keyed by the device name.
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		errs = append(errs, "disk.partitions: "+err.Error())
	}
	for _, p := range parts {
		usage, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || usage == nil {
			continue // permission-denied on odd mounts is expected noise
		}
		add("disk.used_percent", p.Device, usage.UsedPercent)
	}

	// 4. Network — total bytes (rx+tx) per non-loopback interface.
	counters, err := net.IOCountersWithContext(ctx, false)
	if err != nil {
		errs = append(errs, "net: "+err.Error())
	}
	for _, nc := range counters {
		if nc.Name == "lo" {
			continue
		}
		add("net.bytes_total", nc.Name, float64(nc.BytesSent+nc.BytesRecv))
	}

	// 5. Uptime.
	secs, err := host.UptimeWithContext(ctx)
	if err != nil {
		errs = append(errs, "uptime: "+err.Error())
	} else {
		add("system.uptime_seconds", "", float64(secs))
	}

	if len(errs) > 0 {
		return batch, &partialError{errs: errs}
	}
	return batch, nil
}

// partialError reports per-family failures; the batch still carries the
// families that succeeded.
type partialError struct{ errs []string }

func (e *partialError) Error() string {
	out := "partial collection failure:"
	for _, s := range e.errs {
		out += " " + s
	}
	return out
}
