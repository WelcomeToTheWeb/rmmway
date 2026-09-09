package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/heal"
)

// ---- W5-1: self-healing playbook engine ------------------------------------

// handleHealPlaybooks lists the playbook library.
//
// GET /api/heal/playbooks
//
//	200 [playbook, ...]
//	503  heal engine not wired (in-memory mode)
func (s *Server) handleHealPlaybooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	pbs, err := s.heal.Store().Playbooks(r.Context(), r.URL.Query().Get("enabled") != "false")
	if err != nil {
		http.Error(w, "playbooks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if pbs == nil {
		pbs = []heal.Playbook{}
	}
	writeJSON(w, http.StatusOK, pbs)
}

// handleHealRuns lists self-heal runs (the remediation audit trail).
//
// GET /api/heal/runs?status=&device_id=&limit=
//
//	200 [run, ...]
//	503  heal engine not wired
func (s *Server) handleHealRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.heal.Store().Runs(r.Context(), q.Get("status"), q.Get("device_id"), limit)
	if err != nil {
		http.Error(w, "heal runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []heal.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// healRunDetail serves one run with its stage log (the audit trail).
//
// GET /api/heal/runs/{id}
//
//	200 {run, events: [event, ...]}
//	404  unknown id
//	503  heal engine not wired
func (s *Server) healRunDetail(w http.ResponseWriter, r *http.Request, id int64) {
	st := s.heal.Store()
	run, err := st.Run(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	events, err := st.Events(r.Context(), id)
	if err != nil {
		http.Error(w, "heal events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events})
}

// handleHealRunSub routes the /api/heal/runs/{id} detail path.
func (s *Server) handleHealRunSub(w http.ResponseWriter, r *http.Request) {
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin","heal","runs","<id>"]
	if len(parts) != 4 || parts[3] == "" {
		http.Error(w, "expected /api/heal/runs/{id}", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "run id must be a positive integer", http.StatusBadRequest)
		return
	}
	s.healRunDetail(w, r, id)
}

// handleHealPass runs one detect + advance pass synchronously and reports
// the outcome (e2e trigger; the background loop uses the same RunOnce).
//
// POST /api/heal/pass
//
//	200 {pass summary}
//	503  heal engine not wired
func (s *Server) handleHealPass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	pass := s.heal.RunOnce(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, pass)
}

// registerHeal mounts the self-healing playbook routes (W5-1): playbooks, runs + stage log, manual pass.
func registerHeal(s *Server, mux *http.ServeMux) {
	// W5-1: self-healing playbook engine — playbooks, runs (+ stage log),
	// and a manual pass trigger. /admin mirrors below for e2e/ops (C1:
	// auth-gated).
	mux.HandleFunc("/api/heal/playbooks", s.rbacRoleGate(s.handleHealPlaybooks, "admin"))
	mux.HandleFunc("/api/heal/runs", s.rbacRoleGate(s.handleHealRuns, "admin"))
	mux.HandleFunc("/api/heal/runs/", s.rbacRoleGate(s.handleHealRunSub, "admin"))
	mux.HandleFunc("/api/heal/pass", s.rbacRoleGate(s.handleHealPass, "admin"))
	mux.HandleFunc("/admin/heal/playbooks", s.rbacRoleGate(s.handleHealPlaybooks, "admin"))
	mux.HandleFunc("/admin/heal/runs", s.rbacRoleGate(s.handleHealRuns, "admin"))
	mux.HandleFunc("/admin/heal/runs/", s.rbacRoleGate(s.handleHealRunSub, "admin"))
	mux.HandleFunc("/admin/heal/pass", s.rbacRoleGate(s.handleHealPass, "admin"))
}
