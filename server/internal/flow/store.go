// Store is the flow engine's data layer (Postgres, 0006_flows.sql).
//
// Replay-safety lives here, not in the engine (same pattern as the heal
// package):
//   - InsertRun uses the partial unique index uq_flow_one_active: a
//     colliding INSERT (an active run already exists for the same
//     flow/device/source) returns (nil, false, nil) — a re-delivered
//     trigger event is a no-op, never a second chain in flight.
//   - every node hop is a conditional UPDATE (WHERE status='running'
//     AND current_node=<node>) inside one transaction with the
//     flow_events append; a re-delivered step event affects 0 rows and
//     reports false, so an at-least-once bus (JetStream) is safe by
//     construction.
package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the pool for flows + flow_runs + flow_events.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// DB exposes the pool (tests / e2e insert synthetic samples).
func (s *Store) DB() *pgxpool.Pool { return s.db }

// ---- flows ------------------------------------------------------------------

const flowCols = `id, name, description, graph, cooldown_seconds, enabled, created_at, updated_at`

func scanFlow(row interface{ Scan(...any) error }) (Flow, error) {
	var f Flow
	var g []byte
	err := row.Scan(&f.ID, &f.Name, &f.Description, &g, &f.CooldownS, &f.Enabled, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(g, &f.Graph); err != nil {
		return f, fmt.Errorf("decode flow %d graph: %w", f.ID, err)
	}
	return f, nil
}

// CreateFlow inserts a new flow. The caller must have validated the graph
// (Graph.Validate).
func (s *Store) CreateFlow(ctx context.Context, name, description string, g Graph, cooldownS int, enabled bool) (*Flow, error) {
	gj, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.db.QueryRow(ctx, `
		INSERT INTO flows (name, description, graph, cooldown_seconds, enabled)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, description, gj, cooldownS, enabled).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Flow(ctx, id)
}

// ListFlows returns every flow (enabled filter), oldest first.
func (s *Store) ListFlows(ctx context.Context, onlyEnabled bool) ([]Flow, error) {
	q := `SELECT ` + flowCols + ` FROM flows`
	if onlyEnabled {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Flow fetches one flow by id.
func (s *Store) Flow(ctx context.Context, id int64) (*Flow, error) {
	f, err := scanFlow(s.db.QueryRow(ctx, `SELECT `+flowCols+` FROM flows WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("flow %d not found", id)
		}
		return nil, err
	}
	return &f, nil
}

// UpdateFlow applies a partial update; nil fields are left untouched.
func (s *Store) UpdateFlow(ctx context.Context, id int64, name, description *string, g *Graph, cooldownS *int, enabled *bool) (*Flow, error) {
	sets := []string{"updated_at = now()"}
	args := []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if name != nil {
		add("name", *name)
	}
	if description != nil {
		add("description", *description)
	}
	if g != nil {
		gj, err := json.Marshal(*g)
		if err != nil {
			return nil, err
		}
		add("graph", gj)
	}
	if cooldownS != nil {
		add("cooldown_seconds", *cooldownS)
	}
	if enabled != nil {
		add("enabled", *enabled)
	}
	if len(sets) == 1 {
		return s.Flow(ctx, id)
	}
	args = append(args, id)
	q := `UPDATE flows SET ` + join(sets, ", ") + ` WHERE id = $` + fmt.Sprint(len(args)) + ` RETURNING id`
	var rid int64
	if err := s.db.QueryRow(ctx, q, args...).Scan(&rid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("flow %d not found", id)
		}
		return nil, err
	}
	return s.Flow(ctx, rid)
}

// DeleteFlow removes a flow (its runs keep their denormalized flow_name;
// the flow_id column is SET NULL by the FK).
func (s *Store) DeleteFlow(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM flows WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("flow %d not found", id)
	}
	return nil
}

// ---- runs -------------------------------------------------------------------

// Run is one chain execution (row in flow_runs).
type Run struct {
	ID           int64      `json:"id"`
	FlowID       *int64     `json:"flow_id,omitempty"`
	FlowName     string     `json:"flow_name"`
	DeviceID     string     `json:"device_id"`
	Source       string     `json:"source"`
	Status       string     `json:"status"` // running|succeeded|failed|timeout
	Reason       string     `json:"reason,omitempty"`
	TriggerValue *float64   `json:"trigger_value,omitempty"`
	TriggeredAt  *time.Time `json:"triggered_at,omitempty"`
	CurrentNode  string     `json:"current_node"`
	CheckAfter   *time.Time `json:"check_after,omitempty"`
	CommandID    *string    `json:"command_id,omitempty"`
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// HopEvent is one flow_events row (a node hop in the audit trail).
type HopEvent struct {
	ID     int64     `json:"id"`
	RunID  int64     `json:"run_id"`
	Node   string    `json:"node"`
	Status string    `json:"status"` // entered|dispatched|waiting|ok|branched|failed|timeout
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

const runCols = `id, flow_id, flow_name, device_id, source, status, reason,
	trigger_value, triggered_at, current_node, check_after, command_id,
	dispatched_at, finished_at, started_at, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.FlowID, &r.FlowName, &r.DeviceID, &r.Source, &r.Status, &r.Reason,
		&r.TriggerValue, &r.TriggeredAt, &r.CurrentNode, &r.CheckAfter, &r.CommandID,
		&r.DispatchedAt, &r.FinishedAt, &r.StartedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// InsertRun starts a new run at the flow's trigger node. If an active run
// for the same (flow, device, source) already exists, the partial unique
// index rejects the INSERT and (nil, false, nil) is returned — the
// replay-safe "someone is already on it" outcome.
func (s *Store) InsertRun(ctx context.Context, f *Flow, deviceID, source string, value *float64, at time.Time) (*Run, bool, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO flow_runs (flow_id, flow_name, device_id, source, status,
			trigger_value, triggered_at, current_node, check_after)
		VALUES ($1, $2, $3, $4, 'running', $5, $6, $7, now())
		RETURNING id`,
		f.ID, f.Name, deviceID, source, value, at, f.Graph.Trigger().ID).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := s.appendEvent(ctx, id, f.Graph.Trigger().ID, "entered",
		fmt.Sprintf("run started (trigger %s = %v)", f.Graph.Trigger().DescribeCondition(), *value)); err != nil {
		return nil, false, err
	}
	r, err := s.Run(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return r, true, nil
}

// appendEvent adds one audit row (the run's node log).
func (s *Store) appendEvent(ctx context.Context, runID int64, node, status, reason string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, $3, $4)`,
		runID, node, status, reason)
	return err
}

// Dispatched records that the run's current script node dispatched command
// commandID. Conditional: only applies while the run is still at that node
// with no command yet (a racing duplicate step event loses = no-op).
func (s *Store) Dispatched(ctx context.Context, runID int64, node, commandID string, at time.Time) (applied bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE flow_runs SET command_id=$2, dispatched_at=$3, updated_at=now()
		WHERE id=$1 AND status='running' AND current_node=$4 AND command_id IS NULL`,
		runID, commandID, at, node)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, 'dispatched', $3)`,
		runID, node, commandID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// AdvanceTo moves a run from node to the next node — or to the terminal
// succeeded state when to is "". One conditional UPDATE + two event rows
// (the source node's outcome + the next node's "entered"), atomically.
// Applied is false when the run was not at `from` (a replay).
//
// checkAfter sets the re-measurement watermark for the NEXT check node:
// a check measures samples strictly after this instant. When the hop is
// "script succeeded", the engine passes the script's dispatched_at (the
// remediation's effect is reported by the agent around the command result,
// which may predate this very hop) — otherwise now().
func (s *Store) AdvanceTo(ctx context.Context, runID int64, from, to, reason string, checkAfter time.Time) (applied bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if to == "" {
		tag, err := tx.Exec(ctx, `
			UPDATE flow_runs SET status='succeeded', reason=$2, finished_at=now(),
				command_id=NULL, dispatched_at=NULL, updated_at=now()
			WHERE id=$1 AND status='running' AND current_node=$3`, runID, reason, from)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, 'ok', $3)`,
			runID, from, reason); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE flow_runs SET current_node=$2, check_after=$4,
			command_id=NULL, dispatched_at=NULL, updated_at=now()
		WHERE id=$1 AND status='running' AND current_node=$3`, runID, to, from, checkAfter)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, 'branched', $3)`,
		runID, from, reason); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, 'entered', '')`,
		runID, to); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FailRun moves a run from node to a terminal failure state ('failed' or
// 'timeout'). Conditional like AdvanceTo; the event row records the reason.
func (s *Store) FailRun(ctx context.Context, runID int64, from, status, reason string) (applied bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE flow_runs SET status=$2, reason=$3, finished_at=now(), updated_at=now()
		WHERE id=$1 AND status='running' AND current_node=$4`, runID, status, reason, from)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO flow_events (run_id, node, status, reason) VALUES ($1, $2, $3, $4)`,
		runID, from, status, reason); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// WaitEvent records that a check node found no fresh sample yet. It is
// appended at most once per node (a re-sweep does not spam the audit log).
func (s *Store) WaitEvent(ctx context.Context, runID int64, node, reason string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO flow_events (run_id, node, status, reason)
		SELECT $1, $2, 'waiting', $3
		WHERE NOT EXISTS (
			SELECT 1 FROM flow_events WHERE run_id=$1 AND node=$2 AND status='waiting')`,
		runID, node, reason)
	return err
}

// Run fetches one run by id.
func (s *Store) Run(ctx context.Context, id int64) (*Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT `+runCols+` FROM flow_runs WHERE id=$1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("flow run %d not found", id)
		}
		return nil, err
	}
	return &r, nil
}

// RunByCommand finds the active run whose current script node dispatched
// commandID (the command.result event's lookup path).
func (s *Store) RunByCommand(ctx context.Context, commandID string) (*Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx,
		`SELECT `+runCols+` FROM flow_runs WHERE command_id=$1 AND status='running' LIMIT 1`, commandID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// ActiveRuns returns every in-flight run, oldest first — the set the sweep
// re-covers.
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+` FROM flow_runs WHERE status='running' ORDER BY id`)
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

// Runs lists runs (filters "" = all), newest first.
func (s *Store) Runs(ctx context.Context, status, deviceID string, flowID *int64, limit int) ([]Run, error) {
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
	if flowID != nil {
		args = append(args, *flowID)
		where += fmt.Sprintf(" AND flow_id = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.db.Query(ctx, `SELECT `+runCols+` FROM flow_runs `+where+
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

// Events returns a run's node log (the audit trail), oldest first.
func (s *Store) Events(ctx context.Context, runID int64) (out []HopEvent, err error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, run_id, node, status, reason, at FROM flow_events WHERE run_id=$1 ORDER BY at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e HopEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.Node, &e.Status, &e.Reason, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CooldownStarted reports whether ANY run for (flow, device, source)
// started within the last since window (the trigger's anti-storm guard).
func (s *Store) CooldownStarted(ctx context.Context, flowID int64, deviceID, source string, since time.Time) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM flow_runs
			WHERE flow_id=$1 AND device_id=$2 AND source=$3 AND started_at >= $4)`,
		flowID, deviceID, source, since).Scan(&ok)
	return ok, err
}

// LatestSample returns the device's newest sample of (metric, source) with
// ts strictly after since — the check node's re-measurement. ok is false
// when no such sample exists yet.
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

// FreshSamples returns, per (device, source), the latest sample of metric
// with ts in [now-fresh, now+60s] — the sampler's trigger source for real
// (non-synthetic) events.
func (s *Store) FreshSamples(ctx context.Context, metric, source string, now time.Time, fresh time.Duration) (out []FreshSample, err error) {
	const q = `
		SELECT DISTINCT ON (m.device_id, m.source) m.device_id, m.source, m.value, m.ts
		FROM metrics m
		WHERE m.name = $1 AND ($2 = '' OR m.source = $2)
		  AND m.ts >= $3 AND m.ts <= $4
		ORDER BY m.device_id, m.source, m.ts DESC`
	rows, err := s.db.Query(ctx, q, metric, source, now.Add(-fresh), now.Add(60*time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FreshSample
		if err := rows.Scan(&f.DeviceID, &f.Source, &f.Value, &f.At); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FreshSample is one (device, source) latest-sample row.
type FreshSample struct {
	DeviceID string
	Source   string
	Value    float64
	At       time.Time
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
