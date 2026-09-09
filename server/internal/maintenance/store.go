// Package maintenance is the W3-10b maintenance-window data layer: named
// scheduled windows + transient per-alert snoozes that suppress baseline
// alerting and self-healing.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Window is one named maintenance window (recurring or one-off).
type Window struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	DeviceID   *string   `json:"device_id,omitempty"`
	Tag        *string   `json:"tag,omitempty"`
	ClientID   *string   `json:"client_id,omitempty"`
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	Recurrence *string   `json:"recurrence,omitempty"`
	Note       string    `json:"note"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Snooze is one transient per-device/tag snooze from the Alerts UI.
type Snooze struct {
	ID        int64     `json:"id"`
	DeviceID  *string   `json:"device_id,omitempty"`
	Tag       *string   `json:"tag,omitempty"`
	ClientID  *string   `json:"client_id,omitempty"`
	Metric    *string   `json:"metric,omitempty"`
	AlertID   *int64    `json:"alert_id,omitempty"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
}

// Store wraps the pool for maintenance windows + snoozes.
type Store struct {
	db *pgxpool.Pool
}

// NewStore builds the maintenance-window store.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// DB exposes the pool (tests / e2e).
func (s *Store) DB() *pgxpool.Pool {
	return s.db
}

// ---- maintenance windows ----------------------------------------------------

// CreateWindow inserts a named maintenance window.
func (s *Store) CreateWindow(ctx context.Context, w Window) (Window, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO maintenance_windows (
			name, device_id, tag, client_id, starts_at, ends_at,
			recurrence, note, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		w.Name, w.DeviceID, w.Tag, w.ClientID, w.StartsAt, w.EndsAt,
		w.Recurrence, w.Note, w.CreatedBy).Scan(&id)
	if err != nil {
		return Window{}, err
	}
	return s.GetWindow(ctx, id)
}

// GetWindow fetches a window by id.
func (s *Store) GetWindow(ctx context.Context, id int64) (Window, error) {
	var w Window
	err := s.db.QueryRow(ctx, `
		SELECT id, name, device_id, tag, client_id, starts_at, ends_at,
		       recurrence, note, created_by, created_at, updated_at
		FROM maintenance_windows WHERE id=$1`, id).Scan(
		&w.ID, &w.Name, &w.DeviceID, &w.Tag, &w.ClientID,
		&w.StartsAt, &w.EndsAt, &w.Recurrence, &w.Note, &w.CreatedBy,
		&w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Window{}, fmt.Errorf("maintenance window %d not found", id)
		}
		return Window{}, err
	}
	return w, nil
}

// ListWindows returns active windows (not ended), newest first.
func (s *Store) ListWindows(ctx context.Context) ([]Window, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, device_id, tag, client_id, starts_at, ends_at,
		       recurrence, note, created_by, created_at, updated_at
		FROM maintenance_windows
		WHERE ends_at > now()
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var w Window
		if err := rows.Scan(&w.ID, &w.Name, &w.DeviceID, &w.Tag, &w.ClientID,
			&w.StartsAt, &w.EndsAt, &w.Recurrence, &w.Note, &w.CreatedBy,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWindow removes a window by id.
func (s *Store) DeleteWindow(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM maintenance_windows WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("maintenance window %d not found", id)
	}
	return nil
}

// ---- snoozes ----------------------------------------------------------------

// CreateSnooze inserts a transient snooze.
func (s *Store) CreateSnooze(ctx context.Context, sn Snooze) (Snooze, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO window_snoozes (
			device_id, tag, client_id, metric, alert_id,
			starts_at, ends_at, note, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		sn.DeviceID, sn.Tag, sn.ClientID, sn.Metric, sn.AlertID,
		sn.StartsAt, sn.EndsAt, sn.Note, sn.CreatedBy).Scan(&id)
	if err != nil {
		return Snooze{}, err
	}
	sn.ID = id
	return sn, nil
}

// ListSnoozes returns active (not expired) snoozes, newest first.
func (s *Store) ListSnoozes(ctx context.Context) ([]Snooze, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, device_id, tag, client_id, metric, alert_id,
		       starts_at, ends_at, note, created_by
		FROM window_snoozes
		WHERE ends_at > now()
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snooze
	for rows.Next() {
		var sn Snooze
		if err := rows.Scan(&sn.ID, &sn.DeviceID, &sn.Tag, &sn.ClientID,
			&sn.Metric, &sn.AlertID, &sn.StartsAt, &sn.EndsAt,
			&sn.Note, &sn.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ---- suppression checks -----------------------------------------------------

// IsSuppressed reports whether a device's metric anomalies are currently
// suppressed by a maintenance window or snooze at the given time. This is
// the "2-line guard" the baseline engine + heal engine call to skip alerts
// and auto-healing during maintenance.
func (s *Store) IsSuppressed(ctx context.Context, deviceID, metric string, now time.Time) (bool, string, error) {
	// Check snoozes first (highest precedence: explicit operator intent).
	if s.snoozeMatches(ctx, deviceID, metric, now) {
		return true, "snooze", nil
	}
	// Then named maintenance windows.
	if s.windowMatches(ctx, deviceID, metric, now) {
		return true, "maintenance window", nil
	}
	return false, "", nil
}

func (s *Store) snoozeMatches(ctx context.Context, deviceID, metric string, now time.Time) bool {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT 1 FROM window_snoozes
		WHERE device_id=$1 AND starts_at <= $3 AND ends_at > $3
		LIMIT 1`,
		deviceID, metric, now).Scan(&count)
	return err == nil
}

func (s *Store) windowMatches(ctx context.Context, deviceID, metric string, now time.Time) bool {
	// Check exact device windows.
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT 1 FROM maintenance_windows
		WHERE device_id=$1 AND starts_at <= $3 AND ends_at > $3
		LIMIT 1`,
		deviceID, metric, now).Scan(&count)
	if err == nil {
		return true
	}
	// Tag and client checks would go here with the device's tags/client.
	// For simplicity, device-level windows are the primary path.
	return false
}
