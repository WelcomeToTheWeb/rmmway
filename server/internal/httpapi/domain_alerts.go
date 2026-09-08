package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/baseline"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- W2-3: dynamic baselining ----------------------------------------------

// handleBaselineAnomalies serves the anomaly feed.
//
// GET /api/baseline/anomalies?limit=100
//
//	200 [{id, device_id, name, source, at, value, score, channel, ...}]
//	503  baseline engine not wired
func (s *Server) handleBaselineAnomalies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.baseline == nil {
		http.Error(w, "baseline engine not configured", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.baseline.Recent(r.Context(), limit)
	if err != nil {
		http.Error(w, "baseline anomalies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []store.StoredAnomaly{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleBaselineRun forces one deterministic scoring pass (e2e / ops).
//
// POST /api/baseline/run
//
//	200 {anomalies: [...], series: N, runs: M}
//	503  baseline engine not wired
//	500  source error (e.g. DB down)
func (s *Server) handleBaselineRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.baseline == nil {
		http.Error(w, "baseline engine not configured", http.StatusServiceUnavailable)
		return
	}
	anoms, err := s.baseline.RunNow(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if anoms == nil {
		anoms = []baseline.Anomaly{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"anomalies": anoms,
		"series":    len(s.baseline.Job.Series()),
		"runs":      s.baseline.Job.RunCount(),
	})
}

// ---- W2-4: alert inbox -----------------------------------------------------

// handleAlerts serves the deduped alert inbox.
//
// GET /api/alerts?status=open&device_id=...&limit=100
//
// \t200 [{id, device_id, hostname, name, source, status, score, channel,
// \t      value, expected, events, first_at, last_at, resolved_at, …}]
// \t400  unknown status
// \t503  alert store not wired
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	deviceID := q.Get("device_id")
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.alerts.List(r.Context(), status, deviceID, limit)
	if err != nil {
		http.Error(w, "alerts: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rows == nil {
		rows = []store.Alert{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// alertCounts returns the per-status counts for the inbox badge.
//
// GET /api/alerts/counts
//
// \t200 {open: n, acked: n, resolved: n}
// \t503  alert store not wired
func (s *Server) alertCounts(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	counts, err := s.alerts.Counts(r.Context())
	if err != nil {
		http.Error(w, "alert counts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// handleAlertSub routes the /api/alerts/{sub} paths: "counts" -> the
// per-status badge counts, otherwise the alert id for a status PATCH.
func (s *Server) handleAlertSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api","alerts","<id|counts>"]
	if len(parts) == 3 && parts[2] == "counts" {
		s.alertCounts(w, r)
		return
	}
	s.handleAlertStatus(w, r)
}

// handleAlertStatus applies a manual inbox transition.
//
// PATCH /api/alerts/{id}   {"status":"acked"|"resolved"}
//
// \t200 {alert}
// \t400  bad body / invalid transition
// \t404  unknown id
// \t503  alert store not wired
func (s *Server) handleAlertStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	// /api/alerts/{id}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api","alerts","<id>"]
	if len(parts) != 3 || parts[2] == "" {
		http.Error(w, "expected /api/alerts/{id}", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "alert id must be a positive integer", http.StatusBadRequest)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.alerts.SetStatus(r.Context(), id, in.Status)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// registerAlerts mounts the alert-inbox + baseline-anomaly routes (W2-3 baselining and W2-4 alerts live in one domain file).
func registerAlerts(s *Server, mux *http.ServeMux) {
	// W2-3: dynamic baselining — anomaly feed (auth-gated) + manual pass.
	mux.HandleFunc("/api/baseline/anomalies", s.requireOperator(s.handleBaselineAnomalies))
	mux.HandleFunc("/api/baseline/run", s.requireOperator(s.handleBaselineRun))
	// W2-4: deduped alert inbox (auth-gated) + ack/resolve + counts.
	mux.HandleFunc("/api/alerts", s.requireOperator(s.handleAlerts))
	mux.HandleFunc("/api/alerts/", s.requireOperator(s.handleAlertSub))
	mux.HandleFunc("/admin/baseline/anomalies", s.requireOperator(s.handleBaselineAnomalies))
	mux.HandleFunc("/admin/baseline/run", s.requireOperator(s.handleBaselineRun))
	mux.HandleFunc("/admin/alerts", s.requireOperator(s.handleAlerts))
	mux.HandleFunc("/admin/alerts/", s.requireOperator(s.handleAlertSub))
}
