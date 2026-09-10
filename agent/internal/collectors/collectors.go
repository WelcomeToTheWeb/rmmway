// Package collectors implements the W1-2 core collectors: the metric
// families the agent emits over the wire.
//
//	Family                     source
//	cpu.utilization_percent    ""  (host-wide, 0–100)
//	memory.used_percent        ""  (0–100, excluding cached/buffered)
//	swap.used_percent          ""  (0–100; omitted on hosts without swap)
//	load.avg1 / load.avg5 /    ""  (host-wide load averages; a value the
//	load.avg15                 platform cannot report is omitted)
//	disk.used_percent          <device>@<mountpoint> (per mounted volume, 0–100)
//	disk.io_read_bytes_total /  <device> (cumulative since boot, per real
//	disk.io_write_bytes_total,    device — loop/ram/zram pseudo-devices
//	disk.io_reads_total /        excluded; cumulative counters, same
//	disk.io_writes_total          convention as net.bytes_total)
//	smart.health                 <device> (1 = SMART self-assessment PASSED,
//	                             0 = failure predicted; needs the smartctl
//	                             binary — absent or unreadable device means
//	                             no sample, not an error)
//	smart.reallocated_sectors    <device> (SMART attribute 5 raw value)
//	process.cpu_percent          <process name> (top N by CPU%, per-name
//	                             aggregate; N = RMMWAY_TOP_PROCS, default
//	                             10, cap 50 — first heartbeat after start
//	                             has no delta and omits this family)
//	process.memory_rss_bytes     <process name> (top N, RSS bytes)
//	cert.days_to_expiry          <cert file path> (per PEM certificate found
//	                             under RMMWAY_CERT_DIRS, default
//	                             /etc/ssl/certs, scan capped at
//	                             RMMWAY_CERT_SCAN_CAP — negative when
//	                             already expired)
//	net.bytes_total            <iface> (total rx+tx bytes since boot, per
//	                           interface — loopback excluded)
//	system.uptime_seconds      ""
//	service.status             <service name> (per RMMWAY_SERVICES entry:
//	                           1.0 = running, 0.0 = stopped)
//
// Implementation: gopsutil/v4 (pure-Go on Linux — reads /proc directly, so
// the static-binary property from W1-1 is preserved). CPU utilization is
// measured over a short blocking window (cpu.Percent(interval)), which is
// deterministic and free of per-core summation drift. service.status uses
// per-OS probes (systemctl / launchctl exec on unix, gopsutil winservices
// on Windows) behind the same no-cgo constraint.
package collectors

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// Collector samples all core metric families once.
type Collector interface {
	Collect(ctx context.Context) (*agentv1.MetricBatch, error)
}

// cpuMeasure abstracts the (blocking) CPU utilization sample so tests can
// inject a fake without sleeping.
type cpuMeasure func(interval time.Duration, percpu bool) ([]float64, error)

// memMeasure abstracts the host memory sample (used_percent + swap) so
// tests never read the host's real memory state.
type memMeasure func(ctx context.Context) (*mem.VirtualMemoryStat, error)

// ErrServiceUnknown is returned by a ServiceSampler for a name the host has
// no such service under. It is NOT a probe failure: the sample is simply
// omitted, so an allowlist entry that references a since-removed service
// does not read as a per-push collection error.
var ErrServiceUnknown = errors.New("service unknown")

// ServiceSampler reports one monitored service's status: 1 (running) or
// 0 (stopped), or ErrServiceUnknown when the host has no such service.
// The default sampler is per-OS (see service_<os>.go); tests inject a fake.
type ServiceSampler func(ctx context.Context, name string) (float64, error)

// maxMonitoredServices caps the RMMWAY_SERVICES allowlist so one
// misspelled "a,b,c,d" is not a way to mint an unbounded sample fan-out
// into the metrics hypertable.
const maxMonitoredServices = 50

type defaultCollector struct {
	cpu      cpuMeasure
	memory   memMeasure
	services []string
	service  ServiceSampler
	load     LoadSampler
	diskIO   DiskIOSampler
	smart    SmartSampler
	procs    ProcSampler
	topN     int
	now      func() time.Time
	certDirs []string
	certCap  int

	procMu   sync.Mutex
	procPrev map[string]procPrev
}

// NewCollector returns the production collector (real gopsutil CPU window;
// the RMMWAY_SERVICES allowlist read from the environment drives the
// service.status family). Every call site that builds a collector (the
// heartbeat push and the one-shot "collect" command) goes through here, so
// the env var is honored in both.
func NewCollector() Collector {
	return &defaultCollector{
		cpu:      cpu.Percent,
		services: parseServiceList(os.Getenv("RMMWAY_SERVICES")),
		service:  defaultServiceSampler,
		load:     defaultLoadSampler,
		diskIO:   defaultDiskIOSampler,
		smart:    defaultSmartSampler,
		procs:    defaultProcSampler,
		topN:     parseTopProcs(os.Getenv("RMMWAY_TOP_PROCS")),
		certDirs: parseCertDirs(os.Getenv("RMMWAY_CERT_DIRS")),
		certCap:  parseCertScanCap(os.Getenv("RMMWAY_CERT_SCAN_CAP")),
		now:      time.Now,
	}
}

// parseTopProcs reads RMMWAY_TOP_PROCS (default 10; invalid values fall
// back to the default rather than erroring at agent startup).
func parseTopProcs(raw string) int {
	if raw == "" {
		return defaultTopProcs
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultTopProcs
	}
	return n
}

// Samplers bundles every injectable probe for tests. A nil sampler falls
// back to the production default; an unset list/cap keeps its default
// (zero TopN/CertCap = the built-in defaults). Production wiring uses
// NewCollector, not this struct.
type Samplers struct {
	CPU      cpuMeasure
	Memory   memMeasure
	Services []string
	Service  ServiceSampler
	Load     LoadSampler
	DiskIO   DiskIOSampler
	Smart    SmartSampler
	Procs    ProcSampler
	TopN     int
	Now      func() time.Time
	CertDirs []string
	CertCap  int
}

// NewCollectorWithSamplers returns a collector with the given probes
// injected; nil fields keep the production defaults.
func NewCollectorWithSamplers(s Samplers) Collector {
	c := NewCollector().(*defaultCollector)
	if s.CPU != nil {
		c.cpu = s.CPU
	}
	if s.Memory != nil {
		c.memory = s.Memory
	}
	if s.Services != nil {
		c.services = s.Services
	}
	if s.Service != nil {
		c.service = s.Service
	}
	if s.Load != nil {
		c.load = s.Load
	}
	if s.DiskIO != nil {
		c.diskIO = s.DiskIO
	}
	if s.Smart != nil {
		c.smart = s.Smart
	}
	if s.Procs != nil {
		c.procs = s.Procs
	}
	if s.TopN > 0 {
		c.topN = s.TopN
	}
	if s.Now != nil {
		c.now = s.Now
	}
	if s.CertDirs != nil {
		c.certDirs = s.CertDirs
	}
	if s.CertCap > 0 {
		c.certCap = s.CertCap
	}
	return c
}

// NewCollectorWithCPU returns a collector with an injected CPU sampler
// (used by tests to avoid the real sleep window).
func NewCollectorWithCPU(sample cpuMeasure) Collector {
	return &defaultCollector{cpu: sample, now: time.Now}
}

// NewCollectorWithCPUServices returns a collector with injected CPU and
// service samplers (tests: no real CPU window, no host service manager).
func NewCollectorWithCPUServices(cpu cpuMeasure, services []string, svc ServiceSampler) Collector {
	return NewCollectorWithCPUServicesLoad(cpu, services, svc, nil)
}

// NewCollectorWithCPUServicesLoad returns a collector with injected CPU,
// service and load samplers (tests: no real CPU window, no host service
// manager, no host load averages). A nil load sampler emits no
// load.avg* samples.
func NewCollectorWithCPUServicesLoad(cpu cpuMeasure, services []string, svc ServiceSampler, ld LoadSampler) Collector {
	return &defaultCollector{cpu: cpu, services: services, service: svc, load: ld, now: time.Now}
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
	memFn := c.memory
	if memFn == nil {
		memFn = mem.VirtualMemoryWithContext
	}
	vm, err := memFn(ctx)
	if err != nil {
		errs = append(errs, "memory: "+err.Error())
	} else {
		add("memory.used_percent", "", vm.UsedPercent)
		emitSwap(vm.SwapTotal, vm.SwapFree, add)
	}

	// 3. Disk — per mounted volume. L9: source is device@mountpoint — the
	// same block device mounted at several mountpoints (bind mounts, btrfs
	// subvolumes) otherwise collides on (name, source, timestamp_ms) and
	// the server's ON CONFLICT DO NOTHING keeps only the first silently.
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		errs = append(errs, "disk.partitions: "+err.Error())
	}
	for _, p := range parts {
		usage, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || usage == nil {
			continue // permission-denied on odd mounts is expected noise
		}
		add("disk.used_percent", p.Device+"@"+p.Mountpoint, usage.UsedPercent)
	}

	// 4. Network — total bytes (rx+tx) per non-loopback interface. M1:
	// pernic=true — the pernic=false aggregate is a single pseudo-interface
	// named "all" (verified on Linux), which collapsed every real interface
	// and made the loopback skip dead code.
	counters, err := net.IOCountersWithContext(ctx, true)
	if err != nil {
		errs = append(errs, "net: "+err.Error())
	}
	for _, nc := range counters {
		if isLoopback(nc.Name) {
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

	// 6. Service status — per allowlisted service (RMMWAY_SERVICES):
	// 1.0 running / 0.0 stopped, source = service name (the wire shape the
	// seeded "service.down" playbook detects on: metric service.status,
	// == 0, source = the service the restart script targets).
	if c.service != nil {
		for _, name := range c.services {
			v, err := c.service(ctx, name)
			if err != nil {
				if errors.Is(err, ErrServiceUnknown) {
					continue // allowlist entry for an absent service: no sample
				}
				errs = append(errs, "service["+name+"]: "+err.Error())
				continue
			}
			add("service.status", name, v)
		}
	}

	// 7. Disk I/O + SMART — per-device cumulative counters and SMART health
	// (loop/ram/zram pseudo-devices excluded; smartctl absence is silent).
	if c.diskIO != nil {
		counters, err := c.diskIO(ctx)
		if err != nil {
			errs = append(errs, "diskio: "+err.Error())
		} else {
			emitDiskIOStats(counters, add)
			if c.smart != nil {
				for _, dev := range realDevices(counters) {
					res, sErr := c.smart(ctx, dev)
					if serr := emitSmart(res, sErr, dev, add); serr != nil {
						errs = append(errs, "smart["+dev+"]: "+serr.Error())
					}
				}
			}
		}
	}

	// 8. Load averages — 1/5/15 min (host-wide; no source).
	if cerr := c.emitLoad(ctx, add); cerr != nil {
		errs = append(errs, "load: "+cerr.Error())
	}

	// 9. Top-N processes — CPU% from deltas between consecutive collects;
	// the first collect ranks by RSS and emits no process.cpu_percent.
	if c.procs != nil {
		stats, err := c.procs(ctx)
		if err != nil {
			errs = append(errs, "procs: "+err.Error())
		} else {
			c.emitTopProcs(stats, add)
		}
	}

	// 10. Certificate expiry — PEM scan of RMMWAY_CERT_DIRS (default
	// /etc/ssl/certs); absent dirs and non-cert files are skipped silently.
	if c.certDirs != nil {
		if cerr := c.emitCerts(add); cerr != nil {
			errs = append(errs, "certs: "+cerr.Error())
		}
	}

	if len(errs) > 0 {
		return batch, &partialError{errs: errs}
	}
	return batch, nil
}

// parseServiceList normalizes the RMMWAY_SERVICES value: comma-separated,
// lenient on whitespace, empties dropped, order-preserving dedupe, capped
// at maxMonitoredServices. Unset/blank input yields nil (no samples).
func parseServiceList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) == maxMonitoredServices {
			break
		}
	}
	return out
}

// isLoopback reports whether a NIC name is the host's loopback interface:
// "lo" (Linux) / "lo0" (macOS, BSD) by exact name, the Windows
// "Loopback Pseudo-Interface 1" adapter by substring (its display name
// varies across Windows versions).
func isLoopback(name string) bool {
	if name == "lo" || name == "lo0" {
		return true
	}
	return strings.Contains(strings.ToLower(name), "loopback")
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
