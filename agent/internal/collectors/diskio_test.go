package collectors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

// fakeDiskIO returns a canned per-device counter map or an error.
func fakeDiskIO(counters map[string]disk.IOCountersStat, err error) DiskIOSampler {
	return func(context.Context) (map[string]disk.IOCountersStat, error) {
		return counters, err
	}
}

func TestDiskIOFamily(t *testing.T) {
	counters := map[string]disk.IOCountersStat{
		"sda":     {Name: "sda", ReadBytes: 1000, WriteBytes: 2000, ReadCount: 10, WriteCount: 20},
		"loop0":   {Name: "loop0", ReadBytes: 999, WriteBytes: 999, ReadCount: 9, WriteCount: 9},
		"ram0":    {Name: "ram0", ReadBytes: 888, WriteBytes: 888, ReadCount: 8, WriteCount: 8},
		"zram1":   {Name: "zram1", ReadBytes: 777, WriteBytes: 777, ReadCount: 7, WriteCount: 7},
		"nvme0n1": {Name: "nvme0n1", ReadBytes: 3000, WriteBytes: 4000, ReadCount: 30, WriteCount: 40},
	}
	c := NewCollectorWithSamplers(Samplers{
		CPU:    fakeCPU([]float64{1}, nil),
		DiskIO: fakeDiskIO(counters, nil),
	})
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "disk.io_") {
			got[s.Name+"@"+s.Source] = s.Value
		}
	}
	want := map[string]float64{
		"disk.io_read_bytes_total@sda":      1000,
		"disk.io_write_bytes_total@sda":     2000,
		"disk.io_reads_total@sda":           10,
		"disk.io_writes_total@sda":          20,
		"disk.io_read_bytes_total@nvme0n1":  3000,
		"disk.io_write_bytes_total@nvme0n1": 4000,
		"disk.io_reads_total@nvme0n1":       30,
		"disk.io_writes_total@nvme0n1":      40,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("missing/wrong %s: got %v want %v (all: %v)", k, got[k], v, got)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected disk.io sample %s (pseudo-device leak?)", k)
		}
	}
}

func TestDiskIOPartialFailure(t *testing.T) {
	c := NewCollectorWithSamplers(Samplers{
		CPU:    fakeCPU([]float64{1}, nil),
		DiskIO: fakeDiskIO(nil, errors.New("io counters down")),
	})
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error when the disk I/O sampler fails")
	}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "disk.io_") {
			t.Fatalf("failing disk I/O sampler must emit no samples: %s", s.Name)
		}
	}
	if len(batch.Samples) == 0 {
		t.Fatal("other families must still ship on a disk I/O failure")
	}
}

func TestSmartFamily(t *testing.T) {
	counters := map[string]disk.IOCountersStat{"sda": {Name: "sda"}}
	c := NewCollectorWithSamplers(Samplers{
		CPU:    fakeCPU([]float64{1}, nil),
		DiskIO: fakeDiskIO(counters, nil),
		Smart: func(context.Context, string) (smartResult, error) {
			return smartResult{health: 1, reallocated: 7, hasHealth: true, hasReallocated: true}, nil
		},
	})
	batch, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]float64{}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "smart.") {
			got[s.Name+"@"+s.Source] = s.Value
		}
	}
	if got["smart.health@sda"] != 1 || got["smart.reallocated_sectors@sda"] != 7 {
		t.Fatalf("smart families: got %v", got)
	}
}

func TestSmartAbsentAndUnknownAreSilent(t *testing.T) {
	counters := map[string]disk.IOCountersStat{"sda": {Name: "sda"}}
	for _, tc := range []struct {
		name string
		err  error
	}{{"absent", ErrSmartAbsent}, {"unknown", ErrSmartUnknown}} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCollectorWithSamplers(Samplers{
				CPU:    fakeCPU([]float64{1}, nil),
				DiskIO: fakeDiskIO(counters, nil),
				Smart: func(context.Context, string) (smartResult, error) {
					return smartResult{}, tc.err
				},
			})
			batch, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("collect must not fail when smartctl is %s: %v", tc.name, err)
			}
			for _, s := range batch.Samples {
				if strings.HasPrefix(s.Name, "smart.") {
					t.Fatalf("no smart samples expected when %s: %s", tc.name, s.Name)
				}
			}
		})
	}
}

func TestSmartOtherErrorIsPartial(t *testing.T) {
	counters := map[string]disk.IOCountersStat{"sda": {Name: "sda"}}
	c := NewCollectorWithSamplers(Samplers{
		CPU:    fakeCPU([]float64{1}, nil),
		DiskIO: fakeDiskIO(counters, nil),
		Smart: func(context.Context, string) (smartResult, error) {
			return smartResult{}, errors.New("smartctl timed out")
		},
	})
	batch, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("expected a partial error on a non-sentinel smart error")
	}
	for _, s := range batch.Samples {
		if strings.HasPrefix(s.Name, "smart.") {
			t.Fatalf("failing smart probe must emit no samples: %s", s.Name)
		}
	}
}

const smartctlPassedOutput = `Model Family:     Western Digital Caviar Green
Device Model:     WDC10EADS-221MBN0
SMART overall-health self-assessment test result: PASSED
...
  Num  Value       Raw_Value
  ID NAME
  5 Reallocated_Sector_Ct   0x0033   200   200   140    Old    Always   -   7
  9 Power_On_Hours          0x0032   081   074   000    Old    Always   -   4321
`

const smartctlFailedOutput = `SMART overall-health self-assessment test result: FAILED
Run a SMART self-test
  5 Reallocated_Sector_Ct   0x0033   180   173   140    Old    Always   -   1543
`

func TestParseSmartctl(t *testing.T) {
	res, err := parseSmartctl(smartctlPassedOutput)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !res.hasHealth || res.health != 1 {
		t.Errorf("health: got %v hasHealth=%v want 1/true", res.health, res.hasHealth)
	}
	if !res.hasReallocated || res.reallocated != 7 {
		t.Errorf("reallocated: got %v hasReallocated=%v want 7/true", res.reallocated, res.hasReallocated)
	}

	res, err = parseSmartctl(smartctlFailedOutput)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !res.hasHealth || res.health != 0 {
		t.Errorf("health: got %v hasHealth=%v want 0/true", res.health, res.hasHealth)
	}
	if !res.hasReallocated || res.reallocated != 1543 {
		t.Errorf("reallocated: got %v want 1543", res.reallocated)
	}

	// Empty output: nothing parsed, no error.
	res, err = parseSmartctl("")
	if err != nil || res.hasHealth || res.hasReallocated {
		t.Errorf("empty output: got %+v err=%v want empty", res, err)
	}
}

// TestDefaultSmartSamplerWithScript runs the real exec path against a fake
// smartctl shell script (unix only; the script mechanism does not exist on
// Windows, where the same code path is covered by the injected samplers).
func TestDefaultSmartSamplerWithScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake smartctl script is unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "smartctl")
	body := "#!/bin/sh\n" + fmt.Sprintf("printf '%%s\\n' %s\n", strconvQuote(smartctlPassedOutput))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := defaultSmartSampler(context.Background(), "sda")
	if err != nil {
		t.Fatalf("defaultSmartSampler: %v", err)
	}
	if !res.hasHealth || res.health != 1 || res.reallocated != 7 {
		t.Fatalf("parsed: %+v", res)
	}
}

func TestDefaultSmartSamplerNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH manipulation differs on Windows")
	}
	t.Setenv("PATH", t.TempDir()) // an empty directory: no smartctl
	_, err := defaultSmartSampler(context.Background(), "sda")
	if !errors.Is(err, ErrSmartAbsent) {
		t.Fatalf("err: %v want ErrSmartAbsent", err)
	}
}

func strconvQuote(s string) string {
	// Single-quoted for the shell, escaping embedded single quotes.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
