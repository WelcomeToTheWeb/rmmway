package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/webhook"
)

// ---- W6-2: signed webhooks + live event stream -----------------------------

// webhookRequest is the JSON body for POST/PATCH /{api|admin}/webhooks.
type webhookRequest struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Secret      string   `json:"secret"`
	Categories  []string `json:"categories"` // empty/absent = all
	Enabled     *bool    `json:"enabled"`
	MaxAttempts *int     `json:"max_attempts"`
	TimeoutMS   *int     `json:"timeout_ms"`
}

// normalizeCategories filters the requested categories to the known set
// (deduped). An empty result means "all categories".
func normalizeCategories(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range in {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return webhook.AllCategories
	}
	return out
}

func unknownCategories(in []string) []string {
	var out []string
	for _, c := range in {
		if c != "" && !containsStr(webhook.AllCategories, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// validWebhookURL reports whether u is an absolute http(s) URL.
func validWebhookURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// handleWebhooks serves the endpoint list (GET) and creation (POST).
//
//	GET  /{api|admin}/webhooks   -> [endpoint, ...] (secret omitted)
//	POST /{api|admin}/webhooks   -> 201 {endpoint} (secret omitted)
//	503 webhook framework not wired
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	st := s.webhooks.Store()
	switch r.Method {
	case http.MethodGet:
		eps, err := st.ListEndpoints(r.Context())
		if err != nil {
			http.Error(w, "webhooks: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]webhook.Public, 0, len(eps))
		for i := range eps {
			out = append(out, eps[i].Public())
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in webhookRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.URL == "" || in.Secret == "" {
			http.Error(w, "name, url and secret are required", http.StatusBadRequest)
			return
		}
		if !validWebhookURL(in.URL) {
			http.Error(w, "url must be an absolute http(s) URL", http.StatusBadRequest)
			return
		}
		if bad := unknownCategories(in.Categories); len(bad) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "unknown categories", "categories": bad,
				"valid": webhook.AllCategories,
			})
			return
		}
		cats := normalizeCategories(in.Categories)
		maxA, tmo := 0, 0
		if in.MaxAttempts != nil {
			maxA = *in.MaxAttempts
		}
		if in.TimeoutMS != nil {
			tmo = *in.TimeoutMS
		}
		ep, err := st.CreateEndpoint(r.Context(), in.Name, in.URL, in.Secret, cats, maxA, tmo, 0)
		if err != nil {
			http.Error(w, "create webhook: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, ep.Public())
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleWebhookSub routes the /{api|admin}/webhooks/{id} subtree:
//
//	GET    {id}            -> one endpoint (secret omitted)
//	PATCH  {id}            -> partial update (name/url/categories/enabled)
//	DELETE {id}            -> delete
//	POST   {id}/replay     -> re-drive from {from_seq} (0 = from the start)
//	GET    {id}/events     -> journaled events for this endpoint (?after=&limit=&category=)
func (s *Server) handleWebhookSub(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	st := s.webhooks.Store()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "webhooks", "<id>", "replay"|"events"]
	if len(parts) < 3 || parts[1] != "webhooks" || parts[2] == "" {
		http.Error(w, "expected /webhooks/{id}[/replay|/events]", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "webhook id must be a positive integer", http.StatusBadRequest)
		return
	}

	// {id}/replay and {id}/events
	if len(parts) == 4 {
		switch parts[3] {
		case "replay":
			s.handleWebhookReplay(w, r, id)
		case "events":
			s.handleWebhookEvents(w, r, id)
		default:
			http.Error(w, "expected /webhooks/{id}/replay or /events", http.StatusNotFound)
		}
		return
	}
	if len(parts) != 3 {
		http.Error(w, "expected /webhooks/{id}[/replay|/events]", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		ep, err := st.Endpoint(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ep.Public())
	case http.MethodPatch:
		var in webhookRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		var name, urlp *string
		if in.Name != "" {
			name = &in.Name
		}
		if in.URL != "" {
			if !validWebhookURL(in.URL) {
				http.Error(w, "url must be an absolute http(s) URL", http.StatusBadRequest)
				return
			}
			urlp = &in.URL
		}
		var cats *[]string
		if in.Categories != nil {
			if bad := unknownCategories(in.Categories); len(bad) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "unknown categories", "categories": bad})
				return
			}
			n := normalizeCategories(in.Categories)
			cats = &n
		}
		ep, err := st.UpdateEndpoint(r.Context(), id, name, urlp, cats, in.Enabled)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ep.Public())
	case http.MethodDelete:
		if err := st.DeleteEndpoint(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PATCH or DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleWebhookReplay resets an endpoint's cursor to re-drive a range of the
// journal.
//
//	POST /{api|admin}/webhooks/{id}/replay   {"from_seq": 0}
//
//	200 {webhook_id, from_seq, last_seq, status}
//	400 bad body / negative from_seq
//	404 unknown id
func (s *Server) handleWebhookReplay(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	st := s.webhooks.Store()
	var in struct {
		FromSeq int64 `json:"from_seq"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in) // body optional
	if in.FromSeq < 0 {
		http.Error(w, "from_seq must be >= 0", http.StatusBadRequest)
		return
	}
	if err := st.SetCursor(r.Context(), id, in.FromSeq); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	ep, err := st.Endpoint(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook_id": id, "from_seq": in.FromSeq, "last_seq": ep.LastSeq, "status": ep.Status,
	})
}

// handleWebhookEvents serves the journaled events an endpoint would have
// received (its categories), for catch-up / debugging.
//
//	GET /{api|admin}/webhooks/{id}/events?after=0&limit=200&category=
//
//	200 [event, ...] (oldest first)
//	404 unknown id
func (s *Server) handleWebhookEvents(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	st := s.webhooks.Store()
	ep, err := st.Endpoint(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	q := r.URL.Query()
	after := int64(0)
	if v := q.Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	cat := q.Get("category")
	evs, err := st.EventsAfter(r.Context(), after, cat, limit)
	if err != nil {
		http.Error(w, "events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Restrict to the categories this endpoint actually subscribes to.
	out := evs[:0]
	for i := range evs {
		if containsStr(ep.Categories, evs[i].Category) {
			out = append(out, evs[i])
		}
	}
	if out == nil {
		out = []webhook.Event{}
	}
	writeJSON(w, http.StatusOK, out)
}

// registerWebhooks mounts the signed-webhook endpoint routes (W6-2): endpoint CRUD, replay, event catch-up.
func registerWebhooks(s *Server, mux *http.ServeMux) {
	// W6-2: signed webhooks + live event stream. Endpoints are user-defined
	// (HMAC secret + categories); /events/stream is the SSE subscription,
	// /events is the REST catch-up query. The stream route accepts the JWT
	// via ?token= (EventSource can't set headers). /admin mirrors for e2e/ops.
	mux.HandleFunc("/api/webhooks", s.requireOperator(s.handleWebhooks))
	mux.HandleFunc("/api/webhooks/", s.requireOperator(s.handleWebhookSub))
	mux.HandleFunc("/admin/webhooks", s.requireOperator(s.handleWebhooks))
	mux.HandleFunc("/admin/webhooks/", s.requireOperator(s.handleWebhookSub))
}
