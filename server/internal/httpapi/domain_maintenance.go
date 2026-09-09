// domain_maintenance.go — gap #10b: maintenance windows + snooze API.
// Owned by lane C (Surfaces & Ops).
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/maintenance"
)

// ---- maintenance windows + snooze ------------------------------------------

// handleMaintenanceWindows serves GET (list) and POST (create) windows.
func (s *Server) handleMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	if s.maintStore == nil {
		http.Error(w, "maintenance store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		windows, err := s.maintStore.ListWindows(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list maintenance windows: " + err.Error()})
			return
		}
		if windows == nil {
			windows = []maintenance.Window{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"windows": windows})
	case http.MethodPost:
		var body struct {
			Name       string  `json:"name"`
			DeviceID   *string `json:"device_id"`
			Tag        *string `json:"tag"`
			ClientID   *string `json:"client_id"`
			StartsAt   string  `json:"starts_at"`
			EndsAt     string  `json:"ends_at"`
			Recurrence *string `json:"recurrence"`
			Note       string  `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		if body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
			return
		}
		starts, err := time.Parse(time.RFC3339, body.StartsAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "starts_at invalid"})
			return
		}
		ends, err := time.Parse(time.RFC3339, body.EndsAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ends_at invalid"})
			return
		}
		if ends.Before(starts) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ends_at must be after starts_at"})
			return
		}
		wnd := maintenance.Window{
			Name:       body.Name,
			DeviceID:   body.DeviceID,
			Tag:        body.Tag,
			ClientID:   body.ClientID,
			StartsAt:   starts.UTC(),
			EndsAt:     ends.UTC(),
			Recurrence: body.Recurrence,
			Note:       body.Note,
			CreatedBy:  "api",
		}
		created, err := s.maintStore.CreateWindow(r.Context(), wnd)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create window: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"window": created})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleMaintenanceWindowSub handles DELETE on specific window ids.
func (s *Server) handleMaintenanceWindowSub(w http.ResponseWriter, r *http.Request) {
	if s.maintStore == nil {
		http.Error(w, "maintenance store not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/maintenance/windows/"), "/")
	idStr := parts[0]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid window id"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.maintStore.DeleteWindow(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delete window: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	default:
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleSnoozes serves GET (list) and POST (create) snoozes.
func (s *Server) handleSnoozes(w http.ResponseWriter, r *http.Request) {
	if s.maintStore == nil {
		http.Error(w, "maintenance store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		snoozes, err := s.maintStore.ListSnoozes(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list snoozes: " + err.Error()})
			return
		}
		if snoozes == nil {
			snoozes = []maintenance.Snooze{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"snoozes": snoozes})
	case http.MethodPost:
		var body struct {
			DeviceID *string `json:"device_id"`
			Tag      *string `json:"tag"`
			ClientID *string `json:"client_id"`
			Metric   *string `json:"metric"`
			AlertID  *int64  `json:"alert_id"`
			Duration string  `json:"duration"` // "1h", "4h", "8h", "24h"
			Note     string  `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		if body.Duration == "" {
			body.Duration = "1h"
		}
		d, err := time.ParseDuration(body.Duration)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid duration"})
			return
		}
		now := time.Now().UTC()
		snooze := maintenance.Snooze{
			DeviceID:  body.DeviceID,
			Tag:       body.Tag,
			ClientID:  body.ClientID,
			Metric:    body.Metric,
			AlertID:   body.AlertID,
			StartsAt:  now,
			EndsAt:    now.Add(d),
			Note:      body.Note,
			CreatedBy: "api",
		}
		created, err := s.maintStore.CreateSnooze(r.Context(), snooze)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create snooze: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"snooze": created})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// registerMaintenance adds maintenance-window routes to the API.
func registerMaintenance(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("/api/maintenance/windows", s.rbacRoleGate(s.handleMaintenanceWindows, "admin"))
	mux.HandleFunc("/api/maintenance/windows/", s.rbacRoleGate(s.handleMaintenanceWindowSub, "admin"))
	mux.HandleFunc("/api/maintenance/snoozes", s.rbacRoleGate(s.handleSnoozes, "admin"))
	// /admin mirrors for e2e/ops.
	mux.HandleFunc("/admin/maintenance/windows", s.rbacRoleGate(s.handleMaintenanceWindows, "admin"))
	mux.HandleFunc("/admin/maintenance/windows/", s.rbacRoleGate(s.handleMaintenanceWindowSub, "admin"))
	mux.HandleFunc("/admin/maintenance/snoozes", s.rbacRoleGate(s.handleSnoozes, "admin"))
}
