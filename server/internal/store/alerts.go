// W2-4: baseline-driven alerts + inbox.
//
// The W2-3 engine flags a series on every pass while it stays anomalous.
// AlertStore turns those repeated flags into ONE deduped inbox alert per
// series: a re-fired anomaly bumps the open alert (events++, max score,
// freshest observation) instead of creating a sibling row — the partial
// unique index uq_alerts_one_open_per_series makes that invariant
// database-enforced, not just code-enforced. An alert auto-resolves when
// its series returns to baseline for enough consecutive passes, and can
// also be acked / resolved manually from the UI.
package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/baseline"
)

// Alert is one inbox row.
type Alert struct {
	ID         int64      `json:"id"`
	DeviceID   string     `json:"device_id"`
	Hostname   string     `json:"hostname"`
	Name       string     `json:"name"`
	Source     string     `json:"source"`
	Status     string     `json:"status"` // open | acked | resolved
	Score      float64    `json:"score"`
	Channel    string     `json:"channel"`
	Value      float64    `json:"value"`
	Expected   *float64   `json:"expected,omitempty"`
	Events     int        `json:"events"`
	FirstAt    time.Time  `json:"first_at"`
	LastAt     time.Time  `json:"last_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// AlertStore persists the deduped alert inbox.
type AlertStore struct {
	db *pgxpool.Pool

	// quietNeeded: consecutive clean passes (series scored AND not
	// flagged) before an open alert is auto-resolved. <=0 means 1.
	quietNeeded int

	mu    sync.Mutex
	quiet map[baseline.SeriesKey]int // clean-pass streak per series

	// onEvent (W6-2) is a sink for alert lifecycle events (fired / updated /
	// resolved) so the event bus / webhook framework can publish them. Nil =
	// no sink (tests / pre-W6-2).
	onEvent AlertEventSink
}

// AlertEventSink receives one alert lifecycle event. action is "fired" (a new
// open alert), "updated" (an existing open alert re-fired), or "resolved"
// (auto- or manually resolved). payload carries the alert's identifying +
// observation fields (device_id, name, source, value, score, channel, status).
type AlertEventSink func(action string, payload map[string]any)

// NewAlertStore builds an AlertStore. quietNeeded is the number of
// consecutive clean passes before auto-resolve (1 with the default 5-min
// engine cadence = "resolved within ~5 min of returning to baseline").
func NewAlertStore(db *pgxpool.Pool, quietNeeded int) *AlertStore {
	if quietNeeded <= 0 {
		quietNeeded = 1
	}
	return &AlertStore{db: db, quietNeeded: quietNeeded, quiet: make(map[baseline.SeriesKey]int)}
}

// SetEventSink installs the W6-2 alert-event sink (nil clears it). It is
// called from Reconcile for fired / updated / resolved alerts.
func (s *AlertStore) SetEventSink(sink AlertEventSink) {
	s.mu.Lock()
	s.onEvent = sink
	s.mu.Unlock()
}

// emit fires one alert lifecycle event to the installed sink (no-op when
// unset). It never fails the reconcile.
func (s *AlertStore) emit(action string, payload map[string]any) {
	s.mu.Lock()
	sink := s.onEvent
	s.mu.Unlock()
	if sink != nil {
		sink(action, payload)
	}
}

// alertPayload builds the W6-2 alert event payload from a series key + the
// fired anomaly.
func alertPayload(k baseline.SeriesKey, a baseline.Anomaly, channel string, status string) map[string]any {
	m := map[string]any{
		"action":    "alert",
		"device_id": k.DeviceID,
		"name":      k.Name,
		"source":    k.Source,
		"value":     a.Value,
		"score":     a.Score,
		"channel":   channel,
		"status":    status,
		"at":        a.At.UTC().Format(time.RFC3339),
	}
	if a.Seasonal != nil {
		m["expected"] = a.Seasonal.Median
	} else if a.Trend != nil {
		m["expected"] = a.Trend.Median
	}
	return m
}

const alertSelect = `
	SELECT a.id, a.device_id, COALESCE(d.hostname, ''), a.name, a.source,
	       a.status, a.score, a.channel, a.value, a.expected, a.events,
	       a.first_at, a.last_at, a.resolved_at, a.acked_at, a.created_at, a.updated_at
	FROM alerts a
	LEFT JOIN devices d ON d.id = a.device_id`

// List returns alerts, newest last_at first. status "" = open (the inbox
// default); deviceID "" = all devices.
func (s *AlertStore) List(ctx context.Context, status, deviceID string, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var st string
	switch status {
	case "":
		st = "open"
	case "acked", "resolved", "open":
		st = status
	default:
		return nil, fmt.Errorf("unknown alert status %q", status)
	}
	where := "WHERE a.status = $1"
	args := []any{st}
	if deviceID != "" {
		args = append(args, deviceID)
		where += fmt.Sprintf(" AND a.device_id = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.db.Query(ctx, alertSelect+" "+where+
		" ORDER BY a.last_at DESC, a.id DESC LIMIT $"+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.Hostname, &a.Name, &a.Source,
			&a.Status, &a.Score, &a.Channel, &a.Value, &a.Expected, &a.Events,
			&a.FirstAt, &a.LastAt, &a.ResolvedAt, &a.AckedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Counts returns alert counts by status (open/acked/resolved) for the
// inbox badge + tabs.
func (s *AlertStore) Counts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{"open": 0, "acked": 0, "resolved": 0}
	rows, err := s.db.Query(ctx, `SELECT status, count(*) FROM alerts GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// SetStatus applies a manual inbox transition: open -> acked | resolved,
// acked -> resolved. Anything else (including re-opening) is refused.
func (s *AlertStore) SetStatus(ctx context.Context, id int64, status string) (*Alert, error) {
	switch status {
	case "acked", "resolved":
	default:
		return nil, fmt.Errorf("invalid alert status %q (want acked|resolved)", status)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur string
	if err := tx.QueryRow(ctx, `SELECT status FROM alerts WHERE id=$1 FOR UPDATE`, id).Scan(&cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("alert %d not found", id)
		}
		return nil, err
	}
	switch {
	case cur == "open" && status == "acked":
		if _, err := tx.Exec(ctx, `UPDATE alerts SET status='acked', acked_at=now(), updated_at=now() WHERE id=$1`, id); err != nil {
			return nil, err
		}
	case cur == "open" && status == "resolved",
		cur == "acked" && status == "resolved":
		if _, err := tx.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now(), updated_at=now() WHERE id=$1`, id); err != nil {
			return nil, err
		}
	case cur == status:
		// Idempotent: already in the requested state.
	default:
		return nil, fmt.Errorf("invalid alert transition %s -> %s", cur, status)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Get fetches one alert by id.
func (s *AlertStore) Get(ctx context.Context, id int64) (*Alert, error) {
	var a Alert
	err := s.db.QueryRow(ctx, alertSelect+` WHERE a.id=$1`, id).Scan(
		&a.ID, &a.DeviceID, &a.Hostname, &a.Name, &a.Source,
		&a.Status, &a.Score, &a.Channel, &a.Value, &a.Expected, &a.Events,
		&a.FirstAt, &a.LastAt, &a.ResolvedAt, &a.AckedAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("alert %d not found", id)
		}
		return nil, err
	}
	return &a, nil
}

// ---- the reconciler (baseline.Job.PostRun) ----------------------------------

// reconcilePlan is the pure decision for one pass: which series to create
// an alert for, which to bump, and which to auto-resolve (by streak).
type reconcilePlan struct {
	create  map[baseline.SeriesKey]baseline.Anomaly // no open alert
	bump    map[baseline.SeriesKey]baseline.Anomaly // an open alert exists
	resolve []baseline.SeriesKey                    // clean streak hit quietNeeded
}

// planReconcile decides, from the pass's anomalies + scored series + the
// current clean-streak state, exactly what the inbox must change. It is
// pure (no I/O, no time) so the dedup + quiet-streak semantics are unit
// tested without a database; Reconcile just executes the plan.
//
//   - each anomalous series gets ONE alert: create if none is open, bump
//     if one is (the partial unique index is the backstop).
//   - a series that was scored AND is no longer flagged accumulates a
//     clean streak; at >= quietNeeded consecutive clean passes it
//     auto-resolves. A series the source no longer returns (device
//     offline) is NOT in scored, so silence never resolves an alert —
//     only an actual return to baseline does.
func planReconcile(anoms []baseline.Anomaly, scored map[baseline.SeriesKey]bool,
	open map[baseline.SeriesKey]bool, quiet map[baseline.SeriesKey]int, quietNeeded int) reconcilePlan {

	if quietNeeded <= 0 {
		quietNeeded = 1
	}
	anomalous := make(map[baseline.SeriesKey]baseline.Anomaly, len(anoms))
	for _, a := range anoms {
		k := baseline.SeriesKey{DeviceID: a.DeviceID, Name: a.Name, Source: a.Source}
		if cur, ok := anomalous[k]; !ok || a.Score > cur.Score {
			anomalous[k] = a
		}
	}

	plan := reconcilePlan{
		create: make(map[baseline.SeriesKey]baseline.Anomaly),
		bump:   make(map[baseline.SeriesKey]baseline.Anomaly),
	}
	for k, a := range anomalous {
		if open[k] {
			plan.bump[k] = a
		} else {
			plan.create[k] = a
		}
		// Hot again: reset any clean streak so it can't auto-resolve.
		delete(quiet, k)
	}

	for k := range scored {
		if _, hot := anomalous[k]; hot {
			continue
		}
		if _, isOpen := open[k]; !isOpen {
			// Quiet with no open alert: nothing to resolve; don't track.
			delete(quiet, k)
			continue
		}
		quiet[k]++
		if quiet[k] >= quietNeeded {
			plan.resolve = append(plan.resolve, k)
			delete(quiet, k) // resolved: nothing left to track
		}
	}
	return plan
}

// Reconcile is the engine's post-pass hook: it plans (pure) then executes
// the dedup — create/bump one open alert per anomalous series and
// auto-resolve series back at baseline. A pass that errors out before
// scoring (DB down) never reaches this hook, so a flaky source can neither
// storm the inbox nor falsely resolve live alerts.
func (s *AlertStore) Reconcile(anoms []baseline.Anomaly, scored map[baseline.SeriesKey]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Which series currently have a non-resolved alert (drives create vs
	// bump + what's eligible to resolve).
	open := make(map[baseline.SeriesKey]bool)
	rows, err := s.db.Query(ctx, `SELECT device_id, name, source FROM alerts WHERE status IN ('open','acked')`)
	if err != nil {
		log.Printf("alerts: reconcile load open: %v", err)
		return
	}
	for rows.Next() {
		var k baseline.SeriesKey
		if err := rows.Scan(&k.DeviceID, &k.Name, &k.Source); err != nil {
			rows.Close()
			log.Printf("alerts: reconcile scan: %v", err)
			return
		}
		open[k] = true
	}
	rows.Close()

	s.mu.Lock()
	plan := planReconcile(anoms, scored, open, s.quiet, s.quietNeeded)
	s.mu.Unlock()

	for k, a := range plan.create {
		channel, expected := alertFields(a)
		if err := s.upsertAlert(ctx, k, a, channel, expected); err != nil {
			log.Printf("alerts: create %s: %v", k, err)
		} else {
			s.emit("fired", alertPayload(k, a, channel, "open"))
		}
	}
	for k, a := range plan.bump {
		channel, expected := alertFields(a)
		if err := s.bumpAlert(ctx, k, a, channel, expected); err != nil {
			log.Printf("alerts: bump %s: %v", k, err)
		} else {
			s.emit("updated", alertPayload(k, a, channel, "open"))
		}
	}
	for _, k := range plan.resolve {
		tag, err := s.db.Exec(ctx, `
			UPDATE alerts SET status='resolved', resolved_at=now(), updated_at=now()
			WHERE device_id=$1 AND name=$2 AND source=$3 AND status IN ('open','acked')`,
			k.DeviceID, k.Name, k.Source)
		if err != nil {
			log.Printf("alerts: resolve %s: %v", k, err)
			continue
		}
		if tag.RowsAffected() > 0 {
			log.Printf("alerts: auto-resolved %s (clean for %d pass(es))", k, s.quietNeeded)
			s.emit("resolved", map[string]any{
				"action": "alert", "device_id": k.DeviceID, "name": k.Name,
				"source": k.Source, "status": "resolved",
			})
		}
	}
}

// upsertAlert creates a new open alert for a series. A true create-race
// (API-forced pass + background tick in the same instant) trips the
// partial unique index and is retried as a bump — never a second row.
func (s *AlertStore) upsertAlert(ctx context.Context, k baseline.SeriesKey, a baseline.Anomaly, channel string, expected *float64) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO alerts (device_id, name, source, status, score, channel, value,
		                    expected, events, first_at, last_at)
		VALUES ($1, $2, $3, 'open', $4, $5, $6, $7, 1, $8, $8)`,
		k.DeviceID, k.Name, k.Source, a.Score, channel, a.Value, expected, a.At)
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		// Someone beat us to the open slot: fold into it instead.
		return s.bumpAlert(ctx, k, a, channel, expected)
	}
	return err
}

// bumpAlert folds an anomaly into the series' existing open/acked alert.
func (s *AlertStore) bumpAlert(ctx context.Context, k baseline.SeriesKey, a baseline.Anomaly, channel string, expected *float64) error {
	_, err := s.db.Exec(ctx, `
		UPDATE alerts SET
			events = events + 1,
			score = GREATEST(score, $4),
			value = $5,
			channel = $6,
			expected = $7,
			last_at = $8,
			updated_at = now()
		WHERE device_id=$1 AND name=$2 AND source=$3 AND status IN ('open','acked')`,
		k.DeviceID, k.Name, k.Source, a.Score, a.Value, channel, expected, a.At)
	return err
}

// alertFields picks the dominant fired channel and its baseline median
// ("expected" — what the metric should have been).
func alertFields(a baseline.Anomaly) (channel string, expected *float64) {
	if a.Seasonal != nil && (a.Trend == nil || a.Seasonal.Z >= a.Trend.Z) {
		m := a.Seasonal.Median
		return "seasonal", &m
	}
	if a.Trend != nil {
		m := a.Trend.Median
		return "trend", &m
	}
	return "", nil
}

// isUniqueViolation is the PG error-code check (23505) behind a failed
// partial-unique INSERT.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
