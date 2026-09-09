package collectors

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
)

// DiskIOSampler returns per-device cumulative I/O counters (since boot).
// The default is gopsutil's disk.IOCounters (pure-Go /proc on Linux); tests
// inject a fake map.
type DiskIOSampler func(ctx context.Context) (map[string]disk.IOCountersStat, error)

// defaultDiskIOSampler is the production DiskIOSampler (gopsutil v4 has no
// context variant of IOCounters; the call is a /proc read, not a blocking
// wait).
func defaultDiskIOSampler(context.Context) (map[string]disk.IOCountersStat, error) {
	return disk.IOCounters()
}

// isPseudoDisk reports whether a device name is a virtual disk with no
// physical media (and therefore no meaningful I/O or SMART data).
func isPseudoDisk(name string) bool {
	if name == "" {
		return true
	}
	for _, prefix := range []string{"loop", "ram", "zram"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// realDevices returns the sorted real (non-pseudo) device names from an
// I/O counter map, for deterministic SMART probing.
func realDevices(counters map[string]disk.IOCountersStat) []string {
	devs := make([]string, 0, len(counters))
	for name := range counters {
		if !isPseudoDisk(name) {
			devs = append(devs, name)
		}
	}
	sort.Strings(devs)
	return devs
}

// emitDiskIOStats appends the cumulative per-device I/O counter families.
// Counters since boot, same cumulative convention as net.bytes_total.
func emitDiskIOStats(counters map[string]disk.IOCountersStat, add func(name, source string, value float64)) {
	for name, st := range counters {
		if isPseudoDisk(name) {
			continue
		}
		add("disk.io_read_bytes_total", name, float64(st.ReadBytes))
		add("disk.io_write_bytes_total", name, float64(st.WriteBytes))
		add("disk.io_reads_total", name, float64(st.ReadCount))
		add("disk.io_writes_total", name, float64(st.WriteCount))
	}
}

// smartResult is one device's parsed smartctl output.
type smartResult struct {
	health         float64 // 1 = self-assessment PASSED, 0 = failure predicted
	reallocated    float64 // Reallocated_Sector_Ct raw value (attribute 5)
	hasHealth      bool
	hasReallocated bool
}

// ErrSmartAbsent: the host has no smartctl binary. Not a probe failure —
// the family is simply absent (most containers do not ship smartmontools).
var ErrSmartAbsent = errors.New("smartctl not available on this host")

// ErrSmartUnknown: smartctl exists but could not read this device (USB
// bridge, NVMe quirk, in-standby short-circuit). The device is skipped.
var ErrSmartUnknown = errors.New("smartctl could not read this device")

// SmartSampler probes one device's SMART data. The default runs
// `smartctl -H -A -n standby -o off <device>` (health summary plus
// attributes in one call); tests inject a fake or a script-backed one.
type SmartSampler func(ctx context.Context, device string) (smartResult, error)

// defaultSmartSampler is the production SmartSampler (exec-based, no cgo).
func defaultSmartSampler(ctx context.Context, device string) (smartResult, error) {
	out, err := exec.CommandContext(ctx, "smartctl", "-H", "-A", "-n", "standby", "-o", "off", device).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return smartResult{}, ErrSmartUnknown
		}
		if errors.Is(err, exec.ErrNotFound) {
			return smartResult{}, ErrSmartAbsent
		}
		return smartResult{}, ErrSmartUnknown
	}
	return parseSmartctl(string(out))
}

// parseSmartctl extracts the overall-health verdict and the
// Reallocated_Sector_Ct raw value from `smartctl -H -A` output.
func parseSmartctl(out string) (smartResult, error) {
	var res smartResult
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "5" && fields[1] == "Reallocated_Sector_Ct":
			// Attribute table row: 5 Reallocated_Sector_Ct ... <raw>.
			if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
				res.reallocated = v
				res.hasReallocated = true
			}
		case strings.Contains(line, "SMART overall-health self-assessment test result:"):
			if strings.Contains(line, "FAILED") {
				res.health = 0
				res.hasHealth = true
			} else if strings.Contains(line, "PASSED") {
				res.health = 1
				res.hasHealth = true
			}
		}
	}
	return res, nil
}

// emitSmart appends the SMART families for one probed device, translating
// the sentinel errors into the "no sample" semantics.
func emitSmart(res smartResult, err error, device string, add func(name, source string, value float64)) error {
	switch {
	case errors.Is(err, ErrSmartAbsent), errors.Is(err, ErrSmartUnknown):
		return nil // capability/device absence, not a failure
	case err != nil:
		return err
	}
	if res.hasHealth {
		add("smart.health", device, res.health)
	}
	if res.hasReallocated {
		add("smart.reallocated_sectors", device, res.reallocated)
	}
	return nil
}
