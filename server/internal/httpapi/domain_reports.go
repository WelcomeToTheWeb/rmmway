// domain_reports.go — gap #8b: reports API (scheduled + on-demand).
// Owned by lane C (Surfaces & Ops).
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/reports"
)

// registerReports adds report routes to the API.
func registerReports(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("/api/reports/schedules", s.rbacRoleGate(s.handleReportSchedules, "admin"))
	mux.HandleFunc("/api/reports/schedules/", s.rbacRoleGate(s.handleReportScheduleSub, "admin"))
	mux.HandleFunc("/api/reports/runs", s.rbacRoleGate(s.handleReportRuns, "admin"))
	mux.HandleFunc("/api/reports/generate", s.rbacRoleGate(s.handleGenerateReport, "admin"))
	// /admin mirrors.
	mux.HandleFunc("/admin/reports/schedules", s.rbacRoleGate(s.handleReportSchedules, "admin"))
	mux.HandleFunc("/admin/reports/schedules/", s.rbacRoleGate(s.handleReportScheduleSub, "admin"))
	mux.HandleFunc("/admin/reports/runs", s.rbacRoleGate(s.handleReportRuns, "admin"))
	mux.HandleFunc("/admin/reports/generate", s.rbacRoleGate(s.handleGenerateReport, "admin"))
}

// handleReportSchedules serves GET (list) and POST (create) schedules.
func (s *Server) handleReportSchedules(w http.ResponseWriter, r *http.Request) {
	if s.reportsStore == nil {
		http.Error(w, "reports store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		schedules, err := s.reportsStore.ListSchedules(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if schedules == nil {
			schedules = []reports.Schedule{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"schedules": schedules})
	case http.MethodPost:
		var body struct {
			Name         string  `json:"name"`
			ReportType   string  `json:"report_type"`
			ClientID     *string `json:"client_id"`
			Schedule     string  `json:"schedule"`
			OutputFormat string  `json:"output_format"`
			Note         string  `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		if body.Name == "" || body.ReportType == "" || body.Schedule == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, report_type, schedule required"})
			return
		}
		if body.OutputFormat == "" {
			body.OutputFormat = "csv"
		}
		sch := reports.Schedule{
			Name:         body.Name,
			ReportType:   body.ReportType,
			ClientID:     body.ClientID,
			Schedule:     body.Schedule,
			OutputFormat: body.OutputFormat,
			Enabled:      true,
			Note:         body.Note,
			CreatedBy:    "api",
		}
		created, err := s.reportsStore.CreateSchedule(r.Context(), sch)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create schedule: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"schedule": created})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleReportScheduleSub handles DELETE on specific schedule ids.
func (s *Server) handleReportScheduleSub(w http.ResponseWriter, r *http.Request) {
	if s.reportsStore == nil {
		http.Error(w, "reports store not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/reports/schedules/"), "/")
	idStr := parts[0]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid schedule id"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.reportsStore.DeleteSchedule(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delete schedule: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	default:
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleReportRuns returns recent report runs.
func (s *Server) handleReportRuns(w http.ResponseWriter, r *http.Request) {
	if s.reportsStore == nil {
		http.Error(w, "reports store not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	runs, err := s.reportsStore.ListRuns(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if runs == nil {
		runs = []reports.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleGenerateReport triggers an on-demand report generation.
func (s *Server) handleGenerateReport(w http.ResponseWriter, r *http.Request) {
	if s.reportsStore == nil {
		http.Error(w, "reports store not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ReportType   string  `json:"report_type"`
		ClientID     *string `json:"client_id"`
		DeviceID     *string `json:"device_id"`
		OutputFormat string  `json:"output_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if body.ReportType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "report_type required"})
		return
	}
	if body.OutputFormat == "" {
		body.OutputFormat = "csv"
	}

	ctx := r.Context()
	run := reports.Run{
		ReportType:   body.ReportType,
		ClientID:     body.ClientID,
		OutputFormat: body.OutputFormat,
		TriggeredBy:  "api",
		Status:       "running",
	}
	run, err := s.reportsStore.CreateRun(ctx, run)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create run: " + err.Error()})
		return
	}

	// Generate the report content.
	var content io.Reader
	var format string
	switch body.OutputFormat {
	case "csv":
		format = "csv"
	case "pdf":
		format = "pdf"
	default:
		format = "csv"
	}
	switch body.ReportType {
	case reports.TypeFleetStatus:
		if format == "pdf" {
			content, err = s.reportsStore.GenerateFleetStatusPDF(ctx)
		} else {
			content, err = s.reportsStore.GenerateFleetStatusCSV(ctx)
		}
	case reports.TypeDevice:
		if body.DeviceID == nil || *body.DeviceID == "" {
			s.reportsStore.FailRun(ctx, run.ID, "device_id required for device report")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id required"})
			return
		}
		if format == "pdf" {
			content, err = s.reportsStore.GenerateDeviceReportPDF(ctx, *body.DeviceID)
		} else {
			content, err = s.reportsStore.GenerateDeviceReportCSV(ctx, *body.DeviceID)
		}
	case reports.TypePatchCompliance:
		if format == "pdf" {
			content, err = s.reportsStore.GeneratePatchCompliancePDF(ctx)
		} else {
			content, err = s.reportsStore.GeneratePatchComplianceCSV(ctx)
		}
	case reports.TypeLicenseCompliance:
		if format == "pdf" {
			content, err = s.reportsStore.GenerateLicenseCompliancePDF(ctx)
		} else {
			content, err = s.reportsStore.GenerateLicenseComplianceCSV(ctx)
		}
	case reports.TypeUptimeSLA:
		if format == "pdf" {
			content, err = s.reportsStore.GenerateUptimeSLAPDF(ctx)
		} else {
			content, err = s.reportsStore.GenerateUptimeSLACSV(ctx)
		}
	default:
		s.reportsStore.FailRun(ctx, run.ID, "unsupported report type")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported report type"})
		return
	}
	if err != nil {
		s.reportsStore.FailRun(ctx, run.ID, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "generate report: " + err.Error()})
		return
	}

	// Write report content to response directly for now (MinIO integration
	// would store the blob and return the download URL).
	switch body.OutputFormat {
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%d.csv", run.ID))
	case "pdf":
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%d.pdf", run.ID))
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=report-%d.json", run.ID))
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	io.Copy(w, content)
	s.reportsStore.CompleteRun(ctx, run.ID, fmt.Sprintf("report-%d.%s", run.ID, body.OutputFormat))
}
