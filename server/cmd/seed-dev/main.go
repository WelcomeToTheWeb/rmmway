// Command seed-dev populates the DEV database with a synthetic fleet so UI
// and domain work (clients, alerts, reports) doesn't wait on real agents —
// TEAM-PLAN wave 0, F6.
//
// DEV-ONLY: it writes fixed-id devices (seed-*) carrying 3 days of metric
// history in the EXACT five families the real agent emits
// (agent/internal/collectors — the names the UI charts, baseline engine, and
// heal playbooks key off). Idempotent: device ids are fixed and every
// INSERT is ON CONFLICT DO NOTHING, so re-running never duplicates. Each run
// appends a now-anchored 3-day window (timestamps derive from time.Now at
// run start), so repeated runs keep the fixture fresh rather than stale.
//
// Usage (after `make migrate`):
//
//	make seed-dev                                   # 12 devices
//	RMMWAY_SEED_COUNT=40 go run ./cmd/seed-dev      # 40 devices
//	go run ./cmd/seed-dev --fresh                   # wipe seed-% first
//
// Env: RMMWAY_PG_DSN (same name/default as cmd/server), RMMWAY_SEED_COUNT
// (default 12).
package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The five families the real collector emits — do not invent others; the
// baseline engine, UI metric picker, and seeded "Service down" playbook all
// reference these names.
const (
	famCPU    = "cpu.utilization_percent" // source ""
	famMemory = "memory.used_percent"     // source ""
	famDisk   = "disk.used_percent"       // source "<device>@<mountpoint>"
	famNet    = "net.bytes_total"         // source "<iface>"
	famUptime = "system.uptime_seconds"   // source ""
)

const (
	history = 3 * 24 * time.Hour // fixture reach-back
	step    = 15 * time.Minute   // sample spacing (UI buckets server-side)
)

var (
	roles     = []string{"web", "db", "api", "worker", "edge", "cache"}
	osPattern = []string{"linux", "linux", "windows", "linux", "darwin", "windows"}
)

// device is one synthetic fleet member; every attribute derives
// deterministically from its index so re-runs agree.
type device struct {
	id       string
	os, arch string
	tags     []string
	ifaces   []string
	online   bool
	lastSeen time.Time
	volumes  []string // disk.used_percent sources
	iface    string   // net.bytes_total source
}

func main() {
	fresh := flag.Bool("fresh", false, "delete seed-% devices (metrics cascade) before seeding")
	flag.Parse()

	dsn := env("RMMWAY_PG_DSN", "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable")
	count := 12
	if v := os.Getenv("RMMWAY_SEED_COUNT"); v != "" {
		fmt.Sscanf(v, "%d", &count)
	}
	if count < 1 || count > 200 {
		fatal("RMMWAY_SEED_COUNT must be 1..200, got %d", count)
	}

	ctx, stop := context.WithTimeout(context.Background(), 3*time.Minute)
	defer stop()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fatal("pgxpool.New: %v (is the dev stack up? `make dev`)", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fatal("ping %s: %v", dsn, err)
	}

	now := time.Now()
	devs := make([]device, count)
	for i := range devs {
		devs[i] = makeDevice(i, count, now)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		fatal("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	inserted, rows, err := seed(ctx, tx, devs, *fresh)
	if err != nil {
		fatal("seed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		fatal("commit: %v", err)
	}

	online := 0
	for _, d := range devs {
		if d.online {
			online++
		}
	}
	fmt.Printf("RMMWAY DEV FIXTURE: done — %d/%d seed devices present (%d online), %d metric samples across 5 families, %s history @ %s\n",
		inserted, len(devs), online, rows, (history).String(), step)
	fmt.Println("RMMWAY DEV FIXTURE: synthetic dev data — `go run ./cmd/seed-dev --fresh` wipes it")
}

// makeDevice derives the i-th fixture device. Online devices keep sampling
// until a minute or two before `now`; offline ones stopped hours to days
// earlier, and their metric history ends there too.
func makeDevice(i, count int, now time.Time) device {
	r := rngFor(fmt.Sprintf("dev-%02d", i))
	id := fmt.Sprintf("seed-%s-%02d", roles[i%len(roles)], i+1)
	os := osPattern[i%len(osPattern)]

	arch := "amd64"
	switch os {
	case "linux":
		if i%2 == 1 {
			arch = "arm64"
		}
	case "darwin":
		arch = "arm64"
	}

	online := i%4 != 3 // ~75% online
	var lastSeen time.Time
	if online {
		lastSeen = now.Add(-time.Duration(60+int(r.Intn(240))) * time.Second)
	} else {
		lastSeen = now.Add(-time.Duration((2+r.Float64()*46)*60) * time.Minute) // 2–48 h ago
	}

	envTag := "production"
	if i%3 == 2 {
		envTag = "staging"
	}
	ip1 := fmt.Sprintf("10.20.%d.%d", i%5, 10+i)
	ifaces := []string{ip1}
	if os == "windows" {
		ifaces = append(ifaces, fmt.Sprintf("192.168.88.%d", 10+i))
	}

	var volumes []string
	var iface string
	switch os {
	case "linux":
		volumes = []string{"/dev/sda1@/"}
		if i%2 == 0 {
			volumes = append(volumes, "/dev/sdb1@/var/lib")
		}
		iface = "eth0"
	case "windows":
		volumes = []string{"C:@/", "D:@/"}
		iface = "Ethernet0"
	default: // darwin
		volumes = []string{"/dev/disk3s1@/"}
		iface = "en0"
	}

	return device{
		id:       id,
		os:       os,
		arch:     arch,
		tags:     []string{roles[i%len(roles)], envTag},
		ifaces:   ifaces,
		online:   online,
		lastSeen: lastSeen,
		volumes:  volumes,
		iface:    iface,
	}
}

const deviceSQL = `INSERT INTO devices
    (id, hostname, os, arch, agent_version, interfaces, tags, online, first_seen, last_seen,
     heartbeat_interval_s, metric_interval_s)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,30,30)
ON CONFLICT (id) DO NOTHING`

// Same idempotent shape as the store's own MetricsSink.Write (ON CONFLICT DO
// NOTHING on the (device_id, name, source, timestamp_ms, ts) PK).
const metricSQL = `INSERT INTO metrics (device_id, name, source, value, labels, timestamp_ms, ts)
VALUES ($1,$2,$3,$4,'{}',$5::bigint, to_timestamp($5::bigint/1000.0)) ON CONFLICT DO NOTHING`

// seed runs the whole fixture inside the caller's transaction: optional
// wipe, device insert (first run wins), then the metric history. Returns
// devices actually inserted and samples written.
func seed(ctx context.Context, tx pgx.Tx, devs []device, fresh bool) (int, int, error) {
	if fresh {
		tag, err := tx.Exec(ctx, `DELETE FROM devices WHERE id LIKE 'seed-%'`)
		if err != nil {
			return 0, 0, fmt.Errorf("fresh wipe: %w", err)
		}
		if tag.RowsAffected() > 0 {
			fmt.Printf("RMMWAY DEV FIXTURE: --fresh: removed %d existing seed devices (metrics cascade away)\n", tag.RowsAffected())
		}
	}

	inserted := 0
	for _, d := range devs {
		firstSeen := d.lastSeen.Add(-30 * 24 * time.Hour)
		// pgx caches the prepared statement by SQL text after first use, so
		// passing the literal each Exec is the v5 idiom (no separate handle).
		tag, err := tx.Exec(ctx, deviceSQL, d.id, d.id, d.os, d.arch, "0.14.0", d.ifaces, d.tags, d.online, firstSeen, d.lastSeen)
		if err != nil {
			return 0, 0, fmt.Errorf("insert device %s: %w", d.id, err)
		}
		inserted += int(tag.RowsAffected())
	}

	samples := 0
	for _, d := range devs {
		for _, s := range metricSamples(d) {
			if _, err := tx.Exec(ctx, metricSQL, d.id, s.name, s.source, s.value, s.t.UnixMilli()); err != nil {
				return 0, 0, fmt.Errorf("insert %s/%s/%s: %w", d.id, s.name, s.source, err)
			}
			samples++
		}
	}
	return inserted, samples, nil
}

// sample is one point on one series; t is the agent wall-clock sample time
// (timestamp_ms = ts, same as the real ingest path).
type sample struct {
	name   string
	source string
	value  float64
	t      time.Time
}

// metricSamples builds the device's 3-day history. The sample grid is
// anchored to absolute 15-minute UTC boundaries (Truncate), so a re-run at
// the same wall clock regenerates the exact same rows — ON CONFLICT DO
// NOTHING makes it a no-op — and a later re-run only appends the window's
// new tail. The last sample lands on the grid point at or before lastSeen,
// so an offline device's curves stop where its heartbeat stopped.
func metricSamples(d device) []sample {
	n := int(history / step)
	end := d.lastSeen.Truncate(step)
	start := end.Add(-time.Duration(n-1) * step)

	cpuR := rngFor(d.id + "/" + famCPU)
	cpuBase, cpuAmp, cpuPhase := 12+cpuR.Float64()*30, 5+cpuR.Float64()*15, cpuR.Float64()*2*math.Pi
	memR := rngFor(d.id + "/" + famMemory)
	memBase, memAmp, memPhase := 40+memR.Float64()*40, 1+memR.Float64()*2, memR.Float64()*2*math.Pi
	netR := rngFor(d.id + "/" + famNet)
	netCounter := 1e8 + netR.Float64()*4e9 // bytes since boot
	netRate := 20e3 + netR.Float64()*380e3 // bytes/s average
	netPhase := netR.Float64() * 2 * math.Pi
	upR := rngFor(d.id + "/" + famUptime)
	// Boot strictly before the window (≥4 days back) so every sample in it
	// reports positive uptime — a boot inside the window would leave the
	// pre-boot samples with negative elapsed seconds.
	boot := end.Add(-time.Duration((4+upR.Float64()*86)*24) * time.Hour)
	diskStart, diskGrowth := map[string]float64{}, map[string]float64{}
	for _, v := range d.volumes {
		dr := rngFor(d.id + "/" + famDisk + "/" + v)
		diskStart[v] = 55 + dr.Float64()*37
		diskGrowth[v] = 0.2 + dr.Float64()*1.3 // slow fill across the window
	}

	out := make([]sample, 0, n*(5+len(d.volumes)))
	for k := 0; k < n; k++ {
		t := start.Add(time.Duration(k) * step)
		progress := float64(t.Sub(start)) / history.Seconds()
		out = append(out,
			sample{famCPU, "", clamp(cpuBase+cpuAmp*cosPeak(t, cpuPhase)+(cpuR.Float64()*2-1)*3, 1, 99), t},
			sample{famMemory, "", clamp(memBase+memAmp*math.Sin(2*math.Pi*progress+memPhase)+(memR.Float64()*2-1)*1.5, 5, 97), t},
		)
		for _, v := range d.volumes {
			out = append(out, sample{famDisk, v, clamp(diskStart[v]+diskGrowth[v]*progress, 5, 98.5), t})
		}
		counterMult := 0.25 + 0.75*(0.5+0.5*cosPeak(t, netPhase)) // diurnal traffic
		netCounter += netRate * counterMult * step.Seconds() * (0.85 + netR.Float64()*0.3)
		out = append(out,
			sample{famNet, d.iface, netCounter, t},
			sample{famUptime, "", t.Sub(boot).Seconds(), t},
		)
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed-dev: "+format+"\n", args...)
	os.Exit(1)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// rngFor returns a deterministic RNG for one (device, family) series so
// every run regenerates identical curves for the same ids.
func rngFor(key string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(key))
	return rand.New(rand.NewSource(int64(h.Sum64())))
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// cosPeak is a -1..1 daily curve peaking at 14:00 local (office hours),
// which is what makes the fixture read as a real fleet on a chart.
func cosPeak(t time.Time, phase float64) float64 {
	hours := float64(t.Hour()) + float64(t.Minute())/60.0
	return math.Cos(2*math.Pi*(hours-14.0)/24.0 + phase)
}
