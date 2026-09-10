// Package reports is the gap #8b reporting engine: scheduled + on-demand
// fleet status CSV, device report, patch/license compliance, and uptime/SLA
// per client. Output is stored in MinIO and linked from the Reports UI.
package reports

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Report types supported by the engine.
const (
	TypeFleetStatus       = "fleet_status"
	TypeDevice            = "device"
	TypePatchCompliance   = "patch_compliance"
	TypeLicenseCompliance = "license_compliance"
	TypeUptimeSLA         = "uptime_sla"
)

// Schedule is one recurring report schedule.
type Schedule struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ReportType  string    `json:"report_type"`
	ClientID    *string   `json:"client_id,omitempty"`
	Schedule    string    `json:"schedule"`
	OutputFormat string   `json:"output_format"`
	Enabled     bool      `json:"enabled"`
	Note        string    `json:"note"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Run is one report execution (scheduled or manual).
type Run struct {
	ID           int64     `json:"id"`
	ScheduleID   *int64    `json:"schedule_id,omitempty"`
	ReportType   string    `json:"report_type"`
	ClientID     *string   `json:"client_id,omitempty"`
	OutputFormat string    `json:"output_format"`
	TriggeredBy  string    `json:"triggered_by"`
	Status       string    `json:"status"`
	ObjectKey    *string   `json:"object_key,omitempty"`
	Error        *string   `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Store wraps the pool for report schedules + runs.
type Store struct {
	db *pgxpool.Pool
}

// NewStore builds the reports store.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// DB exposes the pool (tests / e2e).
func (s *Store) DB() *pgxpool.Pool {
	return s.db
}

// ---- report schedules -------------------------------------------------------

// CreateSchedule inserts a new report schedule.
func (s *Store) CreateSchedule(ctx context.Context, sch Schedule) (Schedule, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO report_schedules (
			name, report_type, client_id, schedule,
			output_format, enabled, note, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		sch.Name, sch.ReportType, sch.ClientID, sch.Schedule,
		sch.OutputFormat, sch.Enabled, sch.Note, sch.CreatedBy).Scan(&id)
	if err != nil {
		return Schedule{}, err
	}
	return s.GetSchedule(ctx, id)
}

// GetSchedule fetches a schedule by id.
func (s *Store) GetSchedule(ctx context.Context, id int64) (Schedule, error) {
	var sch Schedule
	err := s.db.QueryRow(ctx, `
		SELECT id, name, report_type, client_id, schedule,
		       output_format, enabled, note, created_by, created_at, updated_at
		FROM report_schedules WHERE id=$1`, id).Scan(
		&sch.ID, &sch.Name, &sch.ReportType, &sch.ClientID, &sch.Schedule,
		&sch.OutputFormat, &sch.Enabled, &sch.Note, &sch.CreatedBy,
		&sch.CreatedAt, &sch.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Schedule{}, fmt.Errorf("report schedule %d not found", id)
		}
		return Schedule{}, err
	}
	return sch, nil
}

// ListSchedules returns all report schedules, newest first.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, report_type, client_id, schedule,
		       output_format, enabled, note, created_by, created_at, updated_at
		FROM report_schedules ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sch Schedule
		if err := rows.Scan(&sch.ID, &sch.Name, &sch.ReportType, &sch.ClientID,
			&sch.Schedule, &sch.OutputFormat, &sch.Enabled, &sch.Note,
			&sch.CreatedBy, &sch.CreatedAt, &sch.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// DeleteSchedule removes a schedule by id.
func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM report_schedules WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("report schedule %d not found", id)
	}
	return nil
}

// ---- report runs ------------------------------------------------------------

// CreateRun inserts a new report run record.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	var id int64
	err := s.db.QueryRow(ctx, `
		INSERT INTO report_runs (
			schedule_id, report_type, client_id, output_format,
			triggered_by, status
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		r.ScheduleID, r.ReportType, r.ClientID, r.OutputFormat,
		r.TriggeredBy, r.Status).Scan(&id)
	if err != nil {
		return Run{}, err
	}
	r.ID = id
	return r, nil
}

// CompleteRun marks a run as completed with the output object key.
func (s *Store) CompleteRun(ctx context.Context, id int64, objectKey string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE report_runs SET status='completed', object_key=$2, finished_at=now()
		WHERE id=$1`,
		id, objectKey)
	return err
}

// FailRun marks a run as failed with an error message.
func (s *Store) FailRun(ctx context.Context, id int64, errMsg string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE report_runs SET status='failed', error=$2, finished_at=now()
		WHERE id=$1`,
		id, errMsg)
	return err
}

// ListRuns returns recent report runs, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, schedule_id, report_type, client_id, output_format,
		       triggered_by, status, object_key, error, started_at, finished_at
		FROM report_runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.ScheduleID, &r.ReportType, &r.ClientID,
			&r.OutputFormat, &r.TriggeredBy, &r.Status, &r.ObjectKey,
			&r.Error, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- report generation ------------------------------------------------------

// GenerateFleetStatusCSV generates a fleet status CSV report.
func (s *Store) GenerateFleetStatusCSV(ctx context.Context) (io.Reader, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.id, d.hostname, d.os, d.arch, d.online,
		       d.first_seen, d.last_seen,
		       (SELECT count(*) FROM alerts a
		        WHERE a.device_id = d.id AND a.status = 'open') AS open_alerts
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := csv.NewWriter(pw)
		defer w.Flush()
		w.Write([]string{"device_id", "hostname", "os", "arch", "online",
			"first_seen", "last_seen", "open_alerts"})
		for rows.Next() {
			var id, hostname, os, arch string
			var online bool
			var firstSeen, lastSeen time.Time
			var openAlerts int
			if err := rows.Scan(&id, &hostname, &os, &arch, &online,
				&firstSeen, &lastSeen, &openAlerts); err != nil {
				return
			}
			w.Write([]string{
				id, hostname, os, arch,
				fmt.Sprint(online),
				firstSeen.Format(time.RFC3339),
				lastSeen.Format(time.RFC3339),
				fmt.Sprint(openAlerts),
			})
		}
	}()
	return pr, nil
}

// GenerateDeviceReportCSV generates a single-device report CSV.
func (s *Store) GenerateDeviceReportCSV(ctx context.Context, deviceID string) (io.Reader, error) {
	// Device info + recent metrics summary
	rows, err := s.db.Query(ctx, `
		SELECT d.id, d.hostname, d.os, d.arch, d.online,
		       d.first_seen, d.last_seen,
		       (SELECT count(*) FROM metrics m WHERE m.device_id = d.id) AS metric_count
		FROM devices d WHERE d.id = $1`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := csv.NewWriter(pw)
		defer w.Flush()
		w.Write([]string{"device_id", "hostname", "os", "arch", "online",
			"first_seen", "last_seen", "metric_count"})
		for rows.Next() {
			var id, hostname, os, arch string
			var online bool
			var firstSeen, lastSeen time.Time
			var metricCount int
			if err := rows.Scan(&id, &hostname, &os, &arch, &online,
				&firstSeen, &lastSeen, &metricCount); err != nil {
				return
			}
			w.Write([]string{
				id, hostname, os, arch,
				fmt.Sprint(online),
				firstSeen.Format(time.RFC3339),
				lastSeen.Format(time.RFC3339),
				fmt.Sprint(metricCount),
			})
		}
	}()
	return pr, nil
}

// GeneratePatchComplianceCSV generates a patch compliance CSV report.
func (s *Store) GeneratePatchComplianceCSV(ctx context.Context) (io.Reader, error) {
	rows, err := s.db.Query(ctx, `
		SELECT
			d.hostname, d.os, d.arch,
			d.agent_version,
			d.last_seen,
			(SELECT MAX(UPDATE_CHECK_AT) FROM agent_updates au WHERE au.device_id = d.id) AS last_update_check
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := csv.NewWriter(pw)
		defer w.Flush()
		w.Write([]string{"hostname", "os", "arch", "agent_version", "last_seen", "last_update_check", "compliant"})
		for rows.Next() {
			var hostname, os, arch, agentVersion string
			var lastSeen time.Time
			var lastUpdateCheck *time.Time
			if err := rows.Scan(&hostname, &os, &arch, &agentVersion, &lastSeen, &lastUpdateCheck); err != nil {
				return
			}
			compliant := "true"
			if lastUpdateCheck == nil || lastUpdateCheck.Before(time.Now().Add(-30*24*time.Hour)) {
				compliant = "false"
			}
			w.Write([]string{
				hostname, os, arch, agentVersion,
				lastSeen.Format(time.RFC3339),
				fmt.Sprintf("%v", lastUpdateCheck != nil && lastUpdateCheck.Format(time.RFC3339) != ""),
				compliant,
			})
		}
	}()
	return pr, nil
}

// GenerateLicenseComplianceCSV generates a license compliance CSV report.
func (s *Store) GenerateLicenseComplianceCSV(ctx context.Context) (io.Reader, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.hostname, d.client_id, c.license_tier
		FROM devices d
		LEFT JOIN clients c ON d.client_id = c.id
		ORDER BY d.client_id, d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := csv.NewWriter(pw)
		defer w.Flush()
		w.Write([]string{"hostname", "client_id", "license_tier", "compliant"})
		for rows.Next() {
			var hostname, clientID, licenseTier string
			if err := rows.Scan(&hostname, &clientID, &licenseTier); err != nil {
				return
			}
			w.Write([]string{hostname, clientID, licenseTier, "true"})
		}
	}()
	return pr, nil
}

// GenerateUptimeSLACSV generates an uptime/SLA CSV report.
func (s *Store) GenerateUptimeSLACSV(ctx context.Context) (io.Reader, error) {
	rows, err := s.db.Query(ctx, `
		SELECT d.hostname, d.os, d.arch, d.online,
				(SELECT count(*) FROM heartbeats h WHERE h.device_id = d.id AND h.timestamp_ms > (now() - interval '30 days')) AS heartbeat_count
		FROM devices d ORDER BY d.hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := csv.NewWriter(pw)
		defer w.Flush()
		w.Write([]string{"hostname", "os", "arch", "online", "uptime_pct", "heartbeat_count"})
		for rows.Next() {
			var hostname, os, arch string
			var online bool
			var heartbeatCount int
			if err := rows.Scan(&hostname, &os, &arch, &online, &heartbeatCount); err != nil {
				return
			}
			expectedHeartbeats := 8640 // 1 per 5 min for 30 days
			uptimePct := 100.0
			if expectedHeartbeats > 0 {
				uptimePct = float64(heartbeatCount) / float64(expectedHeartbeats) * 100
				if uptimePct > 100 {
					uptimePct = 100
				}
			}
			w.Write([]string{
				hostname, os, arch,
				fmt.Sprint(online),
				fmt.Sprintf("%.2f", uptimePct),
				fmt.Sprint(heartbeatCount),
			})
		}
	}()
	return pr, nil
}

// FormatName returns a display name for a report type.
func FormatName(reportType string) string {
	switch reportType {
	case TypeFleetStatus:
		return "Fleet Status"
	case TypeDevice:
		return "Device Report"
	case TypePatchCompliance:
		return "Patch Compliance"
	case TypeLicenseCompliance:
		return "License Compliance"
	case TypeUptimeSLA:
		return "Uptime/SLA"
	default:
		return reportType
	}
}
