package baseline

import (
	"context"
	"math"
	"testing"
	"time"
)

// ---- statistics unit tests -------------------------------------------------

func TestMedianMAD(t *testing.T) {
	if got := median([]float64{1, 2, 3, 4, 5}); got != 3 {
		t.Fatalf("median odd = %v, want 3", got)
	}
	if got := median([]float64{1, 2, 3, 4}); got != 2.5 {
		t.Fatalf("median even = %v, want 2.5", got)
	}
	if got := median([]float64{}); got != 0 {
		t.Fatalf("median empty = %v, want 0", got)
	}
	// MAD around 3 for {1,2,3,4,5} is |{2,1,0,1,2}|'s median = 1.
	if got := madAround(3, []float64{1, 2, 3, 4, 5}); got != 1 {
		t.Fatalf("mad = %v, want 1", got)
	}
}

func TestRobustZScaleFloor(t *testing.T) {
	// Flat baseline (mad 0) at a non-zero level: the scale floor is 1% of
	// the level, so z = 1 corresponds to a 1% deviation and the 4.0
	// default flag means "flat metric moved >= 4%".
	z, ok := robustZ(101, 100, 0)
	if !ok || z < 0.99 || z > 1.01 {
		t.Fatalf("flat 1%% dev: z=%v ok=%v, want ~1", z, ok)
	}
	z, _ = robustZ(120, 100, 0)
	if z < 19.9 || z > 20.1 {
		t.Fatalf("flat 20%% dev: z=%v, want ~20", z)
	}
	// Zero level with zero MAD: floor is the absolute 1e-3.
	z, _ = robustZ(0.004, 0, 0)
	if z < 3.9 || z > 4.1 {
		t.Fatalf("zero-level 4e-3 dev: z=%v, want ~4", z)
	}
	// Normal scale path: median 50, MAD 1 -> z of 54.9 is 4.9/1.4826 ≈ 3.305.
	z, _ = robustZ(54.9, 50, 1)
	if math.Abs(z-3.305) > 0.01 {
		t.Fatalf("z = %v, want ~3.305", z)
	}
}

func TestCellRolling(t *testing.T) {
	c := &Cell{}
	for _, v := range []float64{10, 12, 11, 10.5} {
		c.Add(v)
	}
	if c.Len() != 4 {
		t.Fatalf("len = %d, want 4", c.Len())
	}
	if _, ok := c.Score(10); !ok {
		t.Fatalf("score on a populated cell must be ok")
	}
	// Median of {10,10.5,11,12} = 10.75; MAD = median of {|-0.75|,|-0.25|,|0.25|,|1.25|} = 0.5.
	if math.Abs(c.med-10.75) > 1e-9 || math.Abs(c.mad-0.5) > 1e-9 {
		t.Fatalf("med=%v mad=%v, want 10.75/0.5", c.med, c.mad)
	}
	// EWMA after 4 adds (alpha 0.3): 10 -> 10.6 -> 11.12 -> 10.654.
	if math.Abs(c.ewma-10.654) > 1e-9 {
		t.Fatalf("ewma = %v, want 10.654", c.ewma)
	}
}

// ---- synthetic weekly-pattern metric (W2-3 DoD) ----------------------------

// valueAt is the "true" generator for the synthetic metric: a smooth
// weekly pattern — a day-of-week offset plus a 24h hour-of-day sine.
// No noise term: the DoD is that a metric WITH a known weekly pattern is
// scored against that pattern, and determinism must be exact.
func valueAt(at time.Time) float64 {
	at = at.UTC()
	return 40 + float64(at.Weekday())*8 + 15*math.Sin(2*math.Pi*float64(at.Hour())/24)
}

// synthSource is a Source over one device's hourly series.
type synthSource struct {
	ts TimeSeries
}

func (s *synthSource) Samples(_ context.Context, since, until time.Time) ([]TimeSeries, error) {
	var pts []Point
	for _, p := range s.ts.Points {
		if p.At.Before(since) || p.At.After(until) {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) == 0 {
		return nil, nil
	}
	return []TimeSeries{{DeviceID: s.ts.DeviceID, Name: s.ts.Name, Source: s.ts.Source, Points: pts}}, nil
}

// buildWeekly builds hourly points from start up to and including start+days-1
// days' last hour before `end`. Every value follows valueAt (weekly pattern).
func buildWeekly(start, end time.Time) []Point {
	var pts []Point
	for at := start; !at.After(end); at = at.Add(time.Hour) {
		pts = append(pts, Point{At: at, Mean: valueAt(at)})
	}
	return pts
}

func newWeeklyJob() (*Job, time.Time) {
	// now pinned to an exact hour so slots are unambiguous.
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	src := &synthSource{ts: TimeSeries{
		DeviceID: "dev-synth", Name: "cpu.utilization_percent", Source: "",
		Points: buildWeekly(now.Add(-44*24*time.Hour), now),
	}}
	j := &Job{Source: src}
	return j, now
}

func anomalyCountFor(a []Anomaly, device, name string) int {
	n := 0
	for _, x := range a {
		if x.DeviceID == device && x.Name == name {
			n++
		}
	}
	return n
}

// TestWeeklyPatternFlaggedAtRightTime is the W2-3 definition of done: for a
// synthetic metric with a known weekly pattern, anomalies are flagged at the
// right times and quiet otherwise.
func TestWeeklyPatternFlaggedAtRightTime(t *testing.T) {
	j, now := newWeeklyJob()

	// 1. Quiet: the series follows its pattern -> no anomaly on any run.
	anoms, err := j.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := anomalyCountFor(anoms, "dev-synth", "cpu.utilization_percent"); n != 0 {
		t.Fatalf("quiet run: expected 0 anomalies, got %d: %+v", n, anoms)
	}

	// 2. Spike at `now` (the 15:00 slot): the seasonal baseline is 6 same
	// (dow, hour) observations from prior weeks, all ~valueAt(now); +35 is
	// far beyond 4 robust-sigma of a near-flat slot.
	j2, now2 := newWeeklyJob()
	src := j2.Source.(*synthSource)
	last := src.ts.Points[len(src.ts.Points)-1]
	if !last.At.Equal(now2) {
		t.Fatalf("last point %v != now %v", last.At, now2)
	}
	src.ts.Points[len(src.ts.Points)-1] = Point{At: last.At, Mean: valueAt(last.At) + 35}

	anoms, err = j2.RunOnce(context.Background(), now2)
	if err != nil {
		t.Fatalf("spike run: %v", err)
	}
	if n := anomalyCountFor(anoms, "dev-synth", "cpu.utilization_percent"); n != 1 {
		t.Fatalf("spike run: expected exactly 1 anomaly, got %d: %+v", n, anoms)
	}
	a := anoms[0]
	if !a.At.Equal(now2) {
		t.Fatalf("anomaly at %v, want %v", a.At, now2)
	}
	if a.Seasonal == nil || a.Seasonal.Z < DefaultZFlag {
		t.Fatalf("seasonal channel must flag with z >= %v: %+v", DefaultZFlag, a.Seasonal)
	}
	if a.Seasonal.Z < 5 {
		t.Fatalf("expected a clearly large seasonal z, got %v", a.Seasonal.Z)
	}
	if a.Trend == nil || a.Trend.Z < DefaultZFlag {
		t.Fatalf("trend channel must flag too (spike vs preceding hours): %+v", a.Trend)
	}
	if a.Score < DefaultZFlag {
		t.Fatalf("score %v below flag threshold", a.Score)
	}
}

// TestWeeklyPatternIsSlotScoped pins the core property: scoring is against
// this device's SAME (dow, hour) slot history, not a global baseline. A
// value that follows the true weekly pattern for another weekday — i.e.
// "globally" far from this slot's history — is flagged; a value that
// matches the slot history is quiet even though the same numeric value at
// another weekday would be far outside that weekday's own slot.
func TestSeasonalSlotIsDayOfWeekScoped(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC) // Saturday 00:00 UTC
	mk := func(dev string, nowVal float64) *Job {
		start := now.Add(-44 * 24 * time.Hour)
		var pts []Point
		for at := start; !at.After(now.Add(-time.Hour)); at = at.Add(time.Hour) {
			pts = append(pts, Point{At: at, Mean: valueAt(at)})
		}
		pts = append(pts, Point{At: now, Mean: nowVal})
		return &Job{Source: &synthSource{ts: TimeSeries{DeviceID: dev, Name: "cpu.utilization_percent", Points: pts}}}
	}
	slotNow := valueAt(now)
	// valueAt uses the dow offset: Saturday's slot baseline is ~slotNow;
	// a Tuesday-shaped value at the same hour is ~4*8 = 32 lower.
	tueShaped := valueAt(now) - 4*8

	anomsFlagged, err := mk("dev-a", tueShaped).RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("A run: %v", err)
	}
	if n := anomalyCountFor(anomsFlagged, "dev-a", "cpu.utilization_percent"); n != 1 {
		t.Fatalf("A (value from another weekday's pattern): expected 1 anomaly, got %d: %+v", n, anomsFlagged)
	}
	// The same numeric value would be perfectly normal on a Tuesday 00:00
	// slot: B's history is shifted 4 days so its dow-offset matches, and
	// its now value follows B's own pattern -> quiet.
	mkB := func() *Job {
		start := now.Add(-44 * 24 * time.Hour)
		var pts []Point
		for at := start; !at.After(now); at = at.Add(time.Hour) {
			pts = append(pts, Point{At: at, Mean: valueAt(at.Add(4 * 24 * time.Hour))})
		}
		return &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-b", Name: "cpu.utilization_percent", Points: pts}}}
	}
	anomsQuiet, err := mkB().RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("B run: %v", err)
	}
	if n := anomalyCountFor(anomsQuiet, "dev-b", "cpu.utilization_percent"); n != 0 {
		t.Fatalf("B (self-consistent weekly pattern): expected 0 anomalies, got %d: %+v", n, anomsQuiet)
	}
	_ = slotNow
}
// TestTrendChannelCatchesSuddenShift verifies the short-lookback channel
// fires before the seasonal baseline has cells (a device only a few hours
// old, then a jump) — and stays quiet when the level just settles.
func TestTrendChannelCatchesSuddenShift(t *testing.T) {
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	const level = 30
	pts := make([]Point, 0, 6)
	for i := 5; i >= 1; i-- {
		pts = append(pts, Point{At: now.Add(-time.Duration(i) * time.Hour), Mean: level})
	}
	pts = append(pts, Point{At: now, Mean: level + 12}) // jump now; 5 flat cells before
	j := &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-trend", Name: "memory.used_percent", Points: pts}}}
	anoms, err := j.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(anoms) != 1 || anoms[0].DeviceID != "dev-trend" {
		t.Fatalf("expected exactly 1 anomaly for the jump, got %+v", anoms)
	}
	if anoms[0].Trend == nil || anoms[0].Trend.Z < DefaultZFlag {
		t.Fatalf("trend channel must flag the jump: %+v", anoms[0].Trend)
	}
	if anoms[0].Seasonal != nil {
		t.Fatalf("seasonal should not have fired (too few cells): %+v", anoms[0].Seasonal)
	}

	// Same history but no jump: quiet.
	pts[len(pts)-1] = Point{At: now, Mean: level}
	j2 := &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-trend", Name: "memory.used_percent", Points: pts}}}
	if anoms, err := j2.RunOnce(context.Background(), now); err != nil || len(anoms) != 0 {
		t.Fatalf("flat series must be quiet, got %+v err=%v", anoms, err)
	}
}

// TestFlatSeriesQuiet: a perfectly constant metric (MAD 0, seasonal and
// trend) never flags, and a tiny sub-floor wobble doesn't either.
func TestFlatSeriesQuiet(t *testing.T) {
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	pts := buildWeeklyFlat(now)
	j := &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-flat", Name: "disk.io_wait_ms", Points: pts}}}
	anoms, err := j.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(anoms) != 0 {
		t.Fatalf("flat series must be quiet, got %+v", anoms)
	}
	// A 0.5% wobble on a level-30 flat metric is under the 1% scale floor
	// (z = 0.5 < 4.0) -> not anomalous.
	pts[len(pts)-1] = Point{At: now, Mean: 30 * 1.005}
	j2 := &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-flat", Name: "disk.io_wait_ms", Points: pts}}}
	if anoms, err := j2.RunOnce(context.Background(), now); err != nil || len(anoms) != 0 {
		t.Fatalf("sub-floor wobble must be quiet, got %+v err=%v", anoms, err)
	}
	// A 10% wobble on the same flat metric crosses the 4.0 floor-based
	// z -> flagged (flat metrics still get watched).
	pts[len(pts)-1] = Point{At: now, Mean: 30 * 1.10}
	j3 := &Job{Source: &synthSource{ts: TimeSeries{DeviceID: "dev-flat", Name: "disk.io_wait_ms", Points: pts}}}
	if anoms, err := j3.RunOnce(context.Background(), now); err != nil || len(anoms) != 1 {
		t.Fatalf("10%% wobble on a flat metric must flag once, got %+v err=%v", anoms, err)
	}
}

func buildWeeklyFlat(now time.Time) []Point {
	var pts []Point
	for at := now.Add(-44 * 24 * time.Hour); !at.After(now); at = at.Add(time.Hour) {
		pts = append(pts, Point{At: at, Mean: 30})
	}
	return pts
}

// TestSourceRangeFiltering: the job asks for [now-Lookback, now+tolerance]
// and the source honours the bounds; older data is not scored.
func TestSourceRangeFiltering(t *testing.T) {
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	all := buildWeekly(now.Add(-60*24*time.Hour), now) // 60 days, more than the 45-day lookback
	seenSince, seenUntil := time.Time{}, time.Time{}
	src := &rangeSource{ts: TimeSeries{DeviceID: "dev-range", Name: "m", Points: all},
		seenSince: &seenSince, seenUntil: &seenUntil}
	j := &Job{Source: src}
	if _, err := j.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantSince := now.Add(-j.Lookback)
	if !seenSince.Equal(wantSince) {
		t.Fatalf("source since = %v, want %v", seenSince, wantSince)
	}
	if seenUntil != now.Add(clockTolerance) {
		t.Fatalf("source until = %v, want %v", seenUntil, now.Add(clockTolerance))
	}
}

type rangeSource struct {
	ts        TimeSeries
	seenSince *time.Time
	seenUntil *time.Time
}

func (s *rangeSource) Samples(_ context.Context, since, until time.Time) ([]TimeSeries, error) {
	*s.seenSince = since
	*s.seenUntil = until
	var pts []Point
	for _, p := range s.ts.Points {
		if p.At.Before(since) || p.At.After(until) {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) == 0 {
		return nil, nil
	}
	return []TimeSeries{{DeviceID: s.ts.DeviceID, Name: s.ts.Name, Points: pts}}, nil
}

// TestRunDeterministicAndOrdering pins determinism (same input -> same
// output) and newest-first ordering across series, plus the Handle
// callback.
func TestRunDeterministicAndOrdering(t *testing.T) {
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	mk := func() *Job {
		// Series 1: spike at 15:00 (its latest). Series 2: spike at 14:00
		// (its latest) -> the run must flag both, newest first.
		pts1 := buildWeekly(now.Add(-44*24*time.Hour), now)
		pts1[len(pts1)-1] = Point{At: now, Mean: valueAt(now) + 35}
		pts2 := buildWeekly(now.Add(-45*24*time.Hour), now.Add(-time.Hour))
		pts2[len(pts2)-1] = Point{At: now.Add(-time.Hour), Mean: valueAt(now.Add(-time.Hour)) + 35}
		return &Job{Source: &multiSource{ts: []TimeSeries{
			{DeviceID: "dev-d", Name: "cpu.utilization_percent", Points: pts1},
			{DeviceID: "dev-d2", Name: "cpu.utilization_percent", Points: pts2},
		}}}
	}
	j1, j2 := mk(), mk()
	a1, err1 := j1.RunOnce(context.Background(), now)
	a2, err2 := j2.RunOnce(context.Background(), now)
	if err1 != nil || err2 != nil {
		t.Fatalf("runs: %v / %v", err1, err2)
	}
	if len(a1) != 2 || len(a2) != 2 {
		t.Fatalf("expected 2 anomalies (one per spiked series), got %d / %d: %+v", len(a1), len(a2), a1)
	}
	for i := range a1 {
		if !sameAnom(a1[i], a2[i]) {
			t.Fatalf("run %d not deterministic: %+v vs %+v", i, a1[i], a2[i])
		}
	}
	if !a1[0].At.After(a1[1].At) {
		t.Fatalf("anomalies must be newest-first: %v then %v", a1[0].At, a1[1].At)
	}
	// Handle callback fired for both, in the same order.
	var got []Anomaly
	j3 := mk()
	j3.Handle = func(a Anomaly) { got = append(got, a) }
	if _, err := j3.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("j3 run: %v", err)
	}
	if len(got) != 2 || !sameAnom(got[0], a1[0]) || !sameAnom(got[1], a1[1]) {
		t.Fatalf("handle saw %+v, want the same two anomalies", got)
	}
}

func sameAnom(x, y Anomaly) bool {
	if x.At != y.At || x.DeviceID != y.DeviceID || x.Name != y.Name ||
		x.Source != y.Source || x.Value != y.Value || x.Score != y.Score {
		return false
	}
	cx, cy := x.Seasonal != nil, y.Seasonal != nil
	if cx != cy {
		return false
	}
	if cx && *x.Seasonal != *y.Seasonal {
		return false
	}
	cx, cy = x.Trend != nil, y.Trend != nil
	if cx != cy {
		return false
	}
	if cx && *x.Trend != *y.Trend {
		return false
	}
	return true
}

type multiSource struct{ ts []TimeSeries }

func (s *multiSource) Samples(_ context.Context, since, until time.Time) ([]TimeSeries, error) {
	out := make([]TimeSeries, 0, len(s.ts))
	for _, tr := range s.ts {
		var pts []Point
		for _, p := range tr.Points {
			if p.At.Before(since) || p.At.After(until) {
				continue
			}
			pts = append(pts, p)
		}
		if len(pts) > 0 {
			out = append(out, TimeSeries{DeviceID: tr.DeviceID, Name: tr.Name, Source: tr.Source, Points: pts})
		}
	}
	return out, nil
}
