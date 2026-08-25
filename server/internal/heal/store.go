// Store is the heal engine's data layer (Postgres).
//
// Replay-safety lives here, not in the engine:
//   - InsertRun uses the partial unique index uq_heal_one_active: a
//     colliding INSERT (an active run already exists for the same
//     playbook/device/source) returns (nil, false, nil) — a no-op skip,
//     never a second remediation in flight.
//   - Transition is a conditional UPDATE (WHERE status = from) inside one
//     transaction with the heal_events append; re-applying an
//     already-applied transition affects 0 rows and reports false, so a
//     restarted engine driving the same run twice changes nothing.
package heal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the pool for playbooks + heal_runs + heal_events.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// DB exposes the pool (tests / e2e need to insert synthetic samples).
func (s *Store) DB() *pgxpool.Pool { return s.db }

const playbookCols = `key, name, description, metric, source, detect_op, detect_threshold,
	os_filter, fresh_within_seconds, cooldown_seconds,
	remediate_sh, remediate_powershell, confirm_op, confirm_threshold,
	remediate_timeout_seconds, confirm_wait_seconds, enabled, updated_at`

// Playbooks returns every playbook (enabled filter).
func (s *Store) Playbooks(ctx context.Context, onlyEnabled bool) ([]Playbook, error) {
	q := `SELECT ` + playbookCols + ` FROM playbooks`
	if onlyEnabled {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY key`
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playbook
	for rows.Next() {
		var p Playbook
		if err := rows.Scan(&p.Key, &p.Name, &p.Description, &p.Metric, &p.Source,
			&p.DetectOp, &p.DetectThreshold, &p.OSFilter, &p.FreshWithinS, &p.CooldownS,
			&p.RemediateSH, &p.RemediatePS, &p.ConfirmOp, &p.ConfirmThreshold,
			&p.RemediateTimeoutS, &p.ConfirmWaitS, &p.Enabled, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Detect returns the latest fresh sample per (device, source) for the
// playbook's metric, joined to the device registry (os/online come from
// there). The detect CONDITION itself is evaluated by the caller (so the
// op/threshold semantics live in one place: playbook.Detects).
//
// Freshness: samples with ts >= now - freshWithinS. (Clock skew tolerance:
// a sample may lead now by up to 60s, mirroring the baseline engine.)
func (s *Store) Detect(ctx context.Context, p Playbook, now time.Time) ([]Detect, error) {
	const q = `
		WITH latest AS (
			SELECT DISTINCT ON (m.device_id, m.source)
			       m.device_id, m.source, m.value, m.ts
			FROM metrics m
			WHERE m.name = $1
			  AND ($2 = '' OR m.source = $2)
			ORDER BY m.device_id, m.source, m.ts DESC
		)
		SELECT l.device_id, l.source, l.value, l.ts, d.os, d.hostname, d.online
		FROM latest l
		JOIN devices d ON d.id = l.device_id
		WHERE l.ts >= $3
		  AND l.ts <= $4
		ORDER BY l.device_id, l.source`
	fresh := time.Duration(p.FreshWithinS) * time.Second
	if fresh <= 0 {
		fresh = 15 * time.Minute
	}
	rows, err := s.db.Query(ctx, q, p.Metric, p.Source, now.Add(-fresh), now.Add(60*time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Detect
	for rows.Next() {
		var d Detect
		if err := rows.Scan(&d.DeviceID, &d.Source, &d.Value, &d.At, &d.OS, &d.Hostname, &d.Online); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LatestSample returns the device's newest sample of (metric, source) with
// ts strictly after since — the confirm stage's re-measurement. ok is false
// when no such sample exists yet (the agent hasn't reported since).
func (s *Store) LatestSample(ctx context.Context, deviceID, metric, source string, since time.Time) (value float64, at time.Time, ok bool, err error) {
	err = s.db.QueryRow(ctx, `
		SELECT value, ts FROM metrics
		WHERE device_id=$1 AND name=$2 AND ($3 = '' OR source=$3)
		  AND ts > $4
		ORDER BY ts DESC LIMIT 1`, deviceID, metric, source, since).Scan(&value, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return value, at, true, nil
}

const runCols = `id, playbook_key, device_id, source, status, reason,
	detect_value, detect_at, command_id, dispatched_at, remediated_at,
	confirm_value, confirmed_at, escalated_at, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.PlaybookKey, &r.DeviceID, &r.Source, &r.Status, &r.Reason,
		&r.DetectValue, &r.DetectAt, &r.CommandID, &r.DispatchedAt, &r.RemediatedAt,
		&r.ConfirmValue, &r.ConfirmedAt, &r.EscalatedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// InsertRun starts a new run in the detected state. If an active run for
// the same (playbook, device, source) already exists, the partial unique
// index rejects the INSERT and (nil, false, nil) is returned — the
// replay-safe "someone is already on it" outcome.
func (s *Store) InsertRun(ctx context.Context, playbookKey, deviceID, source string, detectValue float64, detectAt time.Time) (*Run, bool, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO heal_runs (playbook_key, device_id, source, status, detect_value, detect_at)
		VALUES ($1, $2, $3, 'detected', $4, $5)
		RETURNING id`, playbookKey, deviceID, source, detectValue, detectAt).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := s.appendEvent(ctx, id, "detected", ""); err != nil {
		return nil, false, err
	}
	r, err := s.Run(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return r, true, nil
}

// Transition applies one state machine transition for a run: a conditional
// UPDATE (status must equal from) + the heal_events append, in one
// transaction. applied is false when the run was not in the expected state
// (a replay — e.g. the engine restarted and re-drove an already-advanced
// run); that is success, not error.
//
// fields carries the stage bookkeeping (e.g. command_id on
// verifying->remediating, confirm_value on confirming->resolved); keys are
// column names restricted to the run's own columns.
func (s *Store) Transition(ctx context.Context, id int64, from, to string, fields map[string]any) (applied bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sets := []string{"status = $2", "updated_at = now()"}
	args := []any{id, to}
	for col, v := range fields {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	q := "UPDATE heal_runs SET " + join(sets, ", ") +
		" WHERE id = $1 AND status = $" + fmt.Sprint(len(args)+1)
	args = append(args, from)
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // replay: already past `from`
	}
	var reason string
	if v, ok := fields["reason"]; ok {
		reason, _ = v.(string)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO heal_events (run_id, status, reason) VALUES ($1, $2, $3)`,
		id, to, reason); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) appendEvent(ctx context.Context, runID int64, status, reason string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO heal_events (run_id, status, reason) VALUES ($1, $2, $3)`,
		runID, status, reason)
	return err
}

// Run fetches one run by id.
func (s *Store) Run(ctx context.Context, id int64) (*Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT `+runCols+` FROM heal_runs WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("heal run %d not found", id)
		}
		return nil, err
	}
	return &r, nil
}

// ActiveRuns returns every in-flight run (all playbooks), oldest first —
// the set the engine advances on each pass.
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+` FROM heal_runs WHERE status = ANY($1) ORDER BY id`, ActiveStates)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CooldownActive reports whether a run for this (playbook, device, source)
// that actually remediated (dispatched_at set — a skipped/failed dispatch
// did not "act" and must not burn the cooldown) started within the last
// cooldown window.
func (s *Store) CooldownActive(ctx context.Context, playbookKey, deviceID, source string, since time.Time) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM heal_runs
			WHERE playbook_key=$1 AND device_id=$2 AND source=$3
			  AND dispatched_at IS NOT NULL
			  AND dispatched_at >= $4)`,
		playbookKey, deviceID, source, since).Scan(&ok)
	return ok, err
}

// Runs lists runs (status "" = all; deviceID "" = all), newest first.
func (s *Store) Runs(ctx context.Context, status, deviceID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := "WHERE 1=1"
	args := []any{}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if deviceID != "" {
		args = append(args, deviceID)
		where += fmt.Sprintf(" AND device_id = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.db.Query(ctx, `SELECT `+runCols+` FROM heal_runs `+where+
		` ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Events returns a run's stage log (the audit trail), oldest first.
func (s *Store) Events(ctx context.Context, runID int64) (out []Event, err error) {
	rows, err := s.db.Query(ctx, `SELECT id, run_id, status, reason, at FROM heal_events WHERE run_id=$1 ORDER BY at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.Status, &e.Reason, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Event is one heal_events row (a stage transition).
type Event struct {
	ID     int64     `json:"id"`
	RunID  int64     `json:"run_id"`
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
