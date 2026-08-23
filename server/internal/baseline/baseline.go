// Package baseline is the RMMWay dynamic baselining engine (W2-3).
//
// Per-metric rolling baselines keyed by day-of-week + hour: each
// observation is scored against the same (dow, hour) slot's history with a
// robust z-score (median + MAD), and a short-lookback trend channel
// (median + MAD over the last few hours) catches sudden shifts before the
// seasonal history has enough cells. EWMA is tracked per slot as the
// smoothed level (trend signal + future alerting input).
//
// Deterministic by construction: pure functions of the input samples, no
// randomness, no ML dependencies (IDEA.md "AIOps without a data-science
// team"). W2-4 turns emitted Anomalies into deduped inbox alerts.
package baseline

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"
)

// Default thresholds and windows (configurable via Job).
const (
	// DefaultZFlag is the robust z-score at which an observation is anomalous.
	DefaultZFlag = 4.0
	// DefaultMinCells is the minimum same-slot observations before the
	// seasonal score is considered meaningful.
	DefaultMinCells = 3
	// DefaultEWMAAlpha is the EWMA smoothing factor per (dow, hour) slot.
	DefaultEWMAAlpha = 0.3
	// DefaultLookback is how far back a run pulls samples. 45 days gives 6
	// same-weekday observations per (dow, hour) slot — enough for a stable
	// MAD.
	DefaultLookback = 45 * 24 * time.Hour
	// DefaultTrendHours is how many preceding hours feed the trend baseline.
	DefaultTrendHours = 4
	// clockTolerance: samples may lead "now" by this much (agent clock skew,
	// ingest latency) and are still scored.
	clockTolerance = 50 * time.Second
)

// ---- statistics ------------------------------------------------------------

func median(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, xs)
	sort.Float64s(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func madAround(around float64, xs []float64) float64 {
	devs := make([]float64, len(xs))
	for i, x := range xs {
		d := x - around
		if d < 0 {
			d = -d
		}
		devs[i] = d
	}
	return median(devs)
}

// robustZ scores x against a distribution summarised by (med, mad).
// A 1.4826 MAD is a consistent scale estimator for normal data; a relative
// floor on the scale keeps a perfectly flat baseline (MAD = 0) from
// dividing by zero. The floor is 1% of the level (or 1e-3 absolute for a
// zero-level metric), so on a flat baseline z = 1 corresponds to a 1%
// deviation and the 4.0 default flag means "flat metric moved >= 4%".
func robustZ(x, med, mad float64) (float64, bool) {
	m := x - med
	if m < 0 {
		m = -m
	}
	scale := 1.4826 * mad
	floor := 1e-3
	if med > 0 {
		floor = med * 0.01
	} else if med < 0 {
		floor = (-med) * 0.01
	}
	if scale < floor {
		scale = floor
	}
	if scale == 0 {
		return 0, false // unreachable: floor >= 1e-3
	}
	return m/scale, true
}

// ---- cell: per (device, metric, source, dow, hour) rolling stats -----------

// Cell is the rolling baseline state for one (day-of-week, hour) slot.
type Cell struct {
	values []float64
	med    float64
	mad    float64
	ewma   float64
	count  int
	// has records whether ewma/med/mad have ever been set.
	has bool
}

// Add folds an hourly mean into the cell (recomputes the summary stats;
// cheap at slot scale).
func (c *Cell) Add(v float64) {
	c.values = append(c.values, v)
	sort.Float64s(c.values)
	c.med = median(c.values)
	c.mad = madAround(c.med, c.values)
	if !c.has {
		c.ewma = v
		c.has = true
	} else {
		c.ewma = c.ewma + DefaultEWMAAlpha*(v-c.ewma)
	}
	c.count++
}

// Len is the number of observations folded in.
func (c *Cell) Len() int { return len(c.values) }

// Score returns the robust z-score of x against the cell's history.
// ok is false until the cell holds at least one observation.
func (c *Cell) Score(x float64) (z float64, ok bool) {
	if !c.has {
		return 0, false
	}
	return robustZ(x, c.med, c.mad)
}

// EWMA is the slot's smoothed level.
func (c *Cell) EWMA() float64 { return c.ewma }

// ---- time series -----------------------------------------------------------

// SeriesKey identifies one (device, metric, source) stream.
type SeriesKey struct {
	DeviceID string
	Name     string
	Source   string
}

func (k SeriesKey) String() string {
	return k.DeviceID + "/" + k.Name + "/" + k.Source
}

type sample struct {
	at  time.Time
	val float64
}

type seriesData struct {
	key SeriesKey
	pts []sample // sorted ascending by at
}

// ---- anomaly ---------------------------------------------------------------

// Anomaly is one flagged observation.
type Anomaly struct {
	At       time.Time   // observation time (UTC)
	DeviceID string      `json:"device_id"`
	Name     string      `json:"name"`
	Source   string      `json:"source"`
	Value    float64     `json:"value"`
	Seasonal *CellScore  `json:"seasonal,omitempty"`
	Trend    *CellScore  `json:"trend,omitempty"`
	Score    float64     `json:"score"` // max(zSeasonal, zTrend) of the fired channels
}

// CellScore is the summary of one scoring channel for an Anomaly.
type CellScore struct {
	Z        float64 `json:"z"`
	Median   float64 `json:"median"`
	MAD      float64 `json:"mad"`
	EWMA     float64 `json:"ewma"`
	Cells    int     `json:"cells"`
}

// ---- data source -----------------------------------------------------------

// Source supplies hourly mean samples for scoring. Implementations must
// return samples within [since, until] inclusive of since, ascending.
type Source interface {
	// Samples returns hourly means for every series in [since, until].
	Samples(ctx context.Context, since, until time.Time) ([]TimeSeries, error)
}

// TimeSeries is one (device, metric, source) stream of hourly means.
type TimeSeries struct {
	DeviceID string
	Name     string
	Source   string
	// Points are ascending by At.
	Points []Point
}

// Point is one hourly mean sample.
type Point struct {
	At  time.Time
	Mean float64
}

// ---- job -------------------------------------------------------------------

// Job runs deterministic baseline scoring over samples pulled from a Source.
type Job struct {
	Source Source

	Lookback   time.Duration // default DefaultLookback
	TrendHours int           // default DefaultTrendHours
	MinCells   int           // default DefaultMinCells
	ZFlag      float64       // default DefaultZFlag

	// Handle, if set, is invoked for every anomaly (in run order).
	Handle func(Anomaly)

	// interval for Run (background mode).
	interval time.Duration

	mu       sync.Mutex
	anoms    []Anomaly
	runCount int
	series   map[SeriesKey]bool
}

func (j *Job) defaults() {
	if j.Lookback <= 0 {
		j.Lookback = DefaultLookback
	}
	if j.TrendHours <= 0 {
		j.TrendHours = DefaultTrendHours
	}
	if j.MinCells <= 0 {
		j.MinCells = DefaultMinCells
	}
	if j.ZFlag <= 0 {
		j.ZFlag = DefaultZFlag
	}
	if j.series == nil {
		j.series = make(map[SeriesKey]bool)
	}
}

// RunOnce performs one deterministic scoring pass over the samples the
// source returns for [now-Lookback, now] and returns the anomalies found,
// most recent first. It also records them (Anomalies) for the API.
func (j *Job) RunOnce(ctx context.Context, now time.Time) ([]Anomaly, error) {
	j.defaults()
	now = now.UTC()
	since := now.Add(-j.Lookback)
	trendWindow := time.Duration(j.TrendHours) * time.Hour

	samples, err := j.Source.Samples(ctx, since, now.Add(clockTolerance))
	if err != nil {
		return nil, fmt.Errorf("baseline source: %w", err)
	}

	var out []Anomaly
	var seriesSeen map[SeriesKey]bool

	for _, ts := range samples {
		if len(ts.Points) == 0 {
			continue
		}
		key := SeriesKey{DeviceID: ts.DeviceID, Name: ts.Name, Source: ts.Source}
		seriesSeen = upsertSeries(seriesSeen, key)

		// Latest valid observation ("now").
		var latest *Point
		for i := range ts.Points {
			p := &ts.Points[i]
			if p.At.After(now.Add(clockTolerance)) {
				continue // future-dated: clock skew beyond tolerance
			}
			latest = p
		}
		if latest == nil {
			continue
		}

		// Seasonal baseline: every same-(dow, hour) observation strictly
		// before the latest one.
		slot := slotOf(latest.At)
		var seasonal []float64
		// Trend baseline: preceding observations within the trend window,
		// restricted to the SAME calendar day as the latest point. A 4h
		// lookback would otherwise cross midnight and score a strong
		// weekly (dow) step at hour 0 as a "spike" — that step is the
		// normal weekly pattern, not a trend. Same-day scoping keeps the
		// channel for genuine same-day drift and leaves hour-0 normalcy
		// to the seasonal channel.
		var trend []float64
		for i := range ts.Points {
			p := &ts.Points[i]
			if !p.At.Before(latest.At) || p.At.After(now.Add(clockTolerance)) {
				continue
			}
			if sameSlot(slotOf(p.At), slot) {
				seasonal = append(seasonal, p.Mean)
			}
			if !p.At.Before(latest.At.Add(-trendWindow)) && sameDay(p.At, latest.At) {
				trend = append(trend, p.Mean)
			}
		}
		sort.Float64s(seasonal)
		sort.Float64s(trend)

		var seasonCell, trendCell *CellScore
		var zSeasonal, zTrend float64

		if len(seasonal) >= j.MinCells {
			cell := &Cell{}
			for _, v := range seasonal {
				cell.Add(v)
			}
			if z, ok := cell.Score(latest.Mean); ok {
				zSeasonal = z
				seasonCell = &CellScore{
					Z: z, Median: cell.med, MAD: cell.mad,
					EWMA: cell.EWMA(), Cells: cell.Len(),
				}
			}
		}
		if len(trend) >= j.MinCells {
			cell := &Cell{}
			for _, v := range trend {
				cell.Add(v)
			}
			if z, ok := cell.Score(latest.Mean); ok {
				zTrend = z
				trendCell = &CellScore{
					Z: z, Median: cell.med, MAD: cell.mad,
					EWMA: cell.EWMA(), Cells: cell.Len(),
				}
			}
		}

		if seasonCell != nil && seasonCell.Z >= j.ZFlag || trendCell != nil && trendCell.Z >= j.ZFlag {
			score := zSeasonal
			if zTrend > score {
				score = zTrend
			}
			if math.IsNaN(score) {
				continue
			}
			a := Anomaly{
				At:       latest.At,
				DeviceID: key.DeviceID,
				Name:     key.Name,
				Source:   key.Source,
				Value:    latest.Mean,
				Seasonal: seasonCell,
				Trend:    trendCell,
				Score:    score,
			}
			out = append(out, a)
			if j.Handle != nil {
				j.Handle(a)
			}
		}
	}

	sort.Slice(out, func(i, k int) bool { return out[i].At.After(out[k].At) })

	j.mu.Lock()
	j.anoms = out
	j.runCount++
	if seriesSeen != nil {
		for k := range seriesSeen {
			j.series[k] = true
		}
	}
	j.mu.Unlock()

	return out, nil
}

func upsertSeries(m map[SeriesKey]bool, k SeriesKey) map[SeriesKey]bool {
	if m == nil {
		m = make(map[SeriesKey]bool)
	}
	m[k] = true
	return m
}

// slot identifies one (day-of-week, hour) baseline slot.
type slot struct {
	dow  int
	hour int
}

// slotOf maps a time to its (day-of-week, hour) baseline slot. All
// baselining runs in UTC — deterministic across restarts and hosts.
func slotOf(t time.Time) slot {
	t = t.UTC()
	return slot{dow: int(t.Weekday()), hour: t.Hour()}
}

func sameSlot(a, b slot) bool { return a.dow == b.dow && a.hour == b.hour }

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// Anomalies returns the anomalies from the most recent run (newest first).
func (j *Job) Anomalies() []Anomaly {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Anomaly, len(j.anoms))
	copy(out, j.anoms)
	return out
}

// Series returns the series observed so far, sorted by String().
func (j *Job) Series() []SeriesKey {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]SeriesKey, 0, len(j.series))
	for k := range j.series {
		out = append(out, k)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].String() < out[k].String() })
	return out
}

// RunCount is the number of completed passes.
func (j *Job) RunCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.runCount
}

// Run executes one pass immediately, then every interval until the context
// is cancelled (the "Go background job"). Errors are returned to errCh
// (buffered; a full errCh is dropped, never blocks the ticker).
func (j *Job) Run(ctx context.Context, interval time.Duration, errCh chan<- error) {
	j.defaults()
	if interval <= 0 {
		interval = time.Minute
	}
	j.interval = interval
	run := func() {
		anoms, err := j.RunOnce(ctx, time.Now())
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		for _, a := range anoms {
			// Grep-friendly line for W2-4's alert sink to consume (it also
			// persists these to baseline_anomalies).
			channel := "trend"
			if a.Seasonal != nil && (a.Trend == nil || a.Seasonal.Z >= a.Trend.Z) {
				channel = "seasonal"
			}
			log.Printf("baseline: anomaly %s %s %q at %s: value=%.3f z=%.2f (%s)",
				a.DeviceID, a.Name, a.Source, a.At.Format(time.RFC3339), a.Value, a.Score, channel)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
