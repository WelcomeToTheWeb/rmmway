package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/flow"
)

// ---- W5-2: event-driven automation chains ----------------------------------

// flowRequest is the JSON body for POST/PATCH /api/flows.
type flowRequest struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Graph       *flow.Graph `json:"graph"`
	CooldownS   *int        `json:"cooldown_seconds"`
	Enabled     *bool       `json:"enabled"`
}

// handleFlows serves the flow list (GET) and flow creation (POST).
//
//	GET  /api/flows?enabled=  -> [flow, ...]
//	POST /api/flows           -> 201 {flow} (graph validated server-side)
//	503 flow engine not wired
func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		http.Error(w, "flow engine not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		flows, err := s.flows.Store().ListFlows(r.Context(), r.URL.Query().Get("enabled") == "true")
		if err != nil {
			http.Error(w, "flows: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if flows == nil {
			flows = []flow.Flow{}
		}
		writeJSON(w, http.StatusOK, flows)
	case http.MethodPost:
		var in flowRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.Graph == nil {
			http.Error(w, "name + graph are required", http.StatusBadRequest)
			return
		}
		if err := in.Graph.Validate(); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		cd, en := 0, true
		if in.CooldownS != nil {
			cd = *in.CooldownS
		}
		if in.Enabled != nil {
			en = *in.Enabled
		}
		f, err := s.flows.Store().CreateFlow(r.Context(), in.Name, in.Description, *in.Graph, cd, en)
		if err != nil {
			if isFlowUniqueViolation(err) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "flow name already exists"})
				return
			}
			http.Error(w, "create flow: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, f)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleFlowSub routes the /{api|admin}/flows/{...} subtree:
//
//	GET    {id}             -> one flow
//	PATCH  {id}             -> partial update (name/description/graph/cooldown/enabled)
//	DELETE {id}             -> delete (runs keep their denormalized name)
//	POST   {id}/trigger     -> synthetic trigger {device_id, source?, value?}
//	GET    runs             -> chain executions (?status=&flow_id=&device_id=&limit=)
//	GET    runs/{id}        -> one run + its node log (the audit trail)
//	POST   sweep            -> one sweep pass (re-cover in-flight runs)
//	POST   sample           -> one sampler pass (real-metric triggers)
func (s *Server) handleFlowSub(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		http.Error(w, "flow engine not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "flows", ...rest]
	rest := parts[2:]

	// The runs subtree lives under flows/ (not behind an id).
	if len(rest) >= 1 && rest[0] == "runs" {
		if len(rest) == 1 {
			s.handleFlowRuns(w, r)
			return
		}
		if len(rest) == 2 {
			s.handleFlowRunDetail(w, r, rest[1])
			return
		}
		http.Error(w, "expected /flows/runs/{id}", http.StatusNotFound)
		return
	}
	if len(rest) == 1 && (rest[0] == "sweep" || rest[0] == "sample") {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now()
		switch rest[0] {
		case "sweep":
			s.flows.Sweep(r.Context(), now)
			active, _ := s.flows.Store().ActiveRuns(r.Context())
			writeJSON(w, http.StatusOK, map[string]int{"active_runs": len(active)})
		case "sample":
			published := s.flows.SampleOnce(r.Context(), now)
			writeJSON(w, http.StatusOK, map[string]int{"trigger_events": published})
		}
		return
	}
	if len(rest) < 1 {
		http.Error(w, "expected /flows/{id}...", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "flow id must be a positive integer", http.StatusBadRequest)
		return
	}

	// {id}/trigger
	if len(rest) == 2 && rest[1] == "trigger" {
		s.handleFlowTrigger(w, r, id)
		return
	}
	if len(rest) != 1 {
		http.Error(w, "expected /flows/{id} or /flows/{id}/trigger", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f, err := s.flows.Store().Flow(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, f)
	case http.MethodPatch:
		var in flowRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Graph != nil {
			if err := in.Graph.Validate(); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				return
			}
		}
		name, desc := &in.Name, &in.Description
		if in.Name == "" {
			name = nil
		}
		if in.Description == "" {
			desc = nil
		}
		f, err := s.flows.Store().UpdateFlow(r.Context(), id, name, desc, in.Graph, in.CooldownS, in.Enabled)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, f)
	case http.MethodDelete:
		if err := s.flows.Store().DeleteFlow(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PATCH or DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleFlowTrigger fires a SYNTHETIC trigger: the API publishes the
// trigger event on the bus (value nil = measure the latest fresh sample)
// and the chain proceeds asynchronously (W5-2 DoD's "fires on the
// synthetic trigger").
//
//	POST /api/flows/{id}/trigger   {"device_id":"…", "source":"", "value":95}
//
//	202 {flow_id, device_id, published: true}
//	404 unknown flow / unknown device
//	400 bad body
func (s *Server) handleFlowTrigger(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.flows.Store().Flow(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var in struct {
		DeviceID string   `json:"device_id"`
		Source   string   `json:"source"`
		Value    *float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.DeviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	if ok, err := s.devices.Contains(r.Context(), in.DeviceID); err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
		return
	}
	if err := s.flows.Trigger(r.Context(), id, in.DeviceID, in.Source, in.Value); err != nil {
		http.Error(w, "publish trigger: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"flow_id": id, "device_id": in.DeviceID, "published": true,
	})
}

// handleFlowRuns lists chain executions.
//
//	GET /api/flows/runs?status=&flow_id=&device_id=&limit=
//
//	200 [run, ...]
func (s *Server) handleFlowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var flowID *int64
	if v := q.Get("flow_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			flowID = &n
		}
	}
	runs, err := s.flows.Store().Runs(r.Context(), q.Get("status"), q.Get("device_id"), flowID, 100)
	if err != nil {
		http.Error(w, "flow runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []flow.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleFlowRunDetail serves one run with its node log (the audit trail).
//
//	GET /api/flows/runs/{id}
//
//	200 {run, events: [hop, ...]}
//	404 unknown id
func (s *Server) handleFlowRunDetail(w http.ResponseWriter, r *http.Request, idStr string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "run id must be a positive integer", http.StatusBadRequest)
		return
	}
	run, err := s.flows.Store().Run(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	events, err := s.flows.Store().Events(r.Context(), id)
	if err != nil {
		http.Error(w, "flow events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events})
}

// isFlowUniqueViolation reports the uq_flow_name duplicate-name error.
func isFlowUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "uq_flow_name")
}

// registerFlows mounts the event-driven automation routes (W5-2): flow CRUD, synthetic trigger, runs + node log.
func registerFlows(s *Server, mux *http.ServeMux) {
	// W5-2: event-driven automation chains — compose (DAG CRUD), trigger
	// (synthetic), runs (+ node log), and a manual sweep. /admin mirrors
	// below for e2e/ops (C1: auth-gated).
	mux.HandleFunc("/api/flows", s.requireOperator(s.handleFlows))
	mux.HandleFunc("/api/flows/", s.requireOperator(s.handleFlowSub))
	mux.HandleFunc("/admin/flows", s.requireOperator(s.handleFlows))
	mux.HandleFunc("/admin/flows/", s.requireOperator(s.handleFlowSub))
}
