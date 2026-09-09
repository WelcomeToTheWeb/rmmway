package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/webhook"
)

// streamFilterFromQuery reads the subscription filter from the query string:
// category (alert|inventory|automation|other), device (exact id) and type
// (exact event type / bus subject). Any may be omitted. It reports an error
// only for an unknown category (device/type are free-form exact matches).
func streamFilterFromQuery(q url.Values) (webhook.Filter, error) {
	fl := webhook.Filter{
		Category: q.Get("category"),
		Device:   q.Get("device"),
		Type:     q.Get("type"),
	}
	if !fl.Valid() {
		return fl, fmt.Errorf("unknown category %s", fl.Category)
	}
	return fl, nil
}

// handleEvents is the REST catch-up query over the event journal — the
// non-streaming twin of the SSE route, so a client can page through history
// (or the events since a given seq) without holding a connection.
//
//	GET /{api|admin}/events?after=0&limit=200&category=&device=&type=
//
//	200  [event, ...] (oldest first; each an Envelope)
//	400  unknown category
//	503  webhook framework not wired (in-memory mode)
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	fl, err := streamFilterFromQuery(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var after int64
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
	evs, err := s.webhooks.Store().EventsAfterFilter(r.Context(), after, fl, limit)
	if err != nil {
		http.Error(w, "events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]webhook.Envelope, 0, len(evs))
	for i := range evs {
		out = append(out, evs[i].Envelope())
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventStream is the live SSE subscription (W6-2's "SSE/subscription").
// It sends recent journal events as catch-up (honoring Last-Event-ID), then
// streams new events as they are journaled. Each frame is an SSE `data:` with
// the Envelope JSON and an `id:` of the journal seq (so a client can resume
// with Last-Event-ID).
//
//	GET /{api|admin}/events/stream[?category=&device=&type=]
//
//	200  text/event-stream (catch-up + live)
//	400  unknown category
//	503  webhook framework not wired / response can't flush
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusServiceUnavailable)
		return
	}
	flt, err := streamFilterFromQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	// Subscribe FIRST so no journaled event is missed between the catch-up
	// read and the live loop (the seq-dedupe below collapses any overlap).
	ch, cancel := s.webhooks.AddLiveFilter(r.Context(), flt)
	defer cancel()
	st := s.webhooks.Store()

	lastSent := int64(0)
	if lid := r.Header.Get("Last-Event-ID"); lid != "" {
		if n, err := strconv.ParseInt(lid, 10, 64); err == nil && n >= 0 {
			lastSent = n
		}
	}
	if lastSent == 0 {
		// No resume point: seed with the most recent events as context.
		if mx, err := st.MaxSeq(r.Context()); err == nil && mx > 0 {
			lastSent = mx - 200
			if lastSent < 0 {
				lastSent = 0
			}
		}
	}
	writeSSE := func(ev webhook.Event) {
		if ev.Seq <= lastSent {
			return
		}
		env := ev.Envelope()
		b, _ := json.Marshal(env)
		_, _ = w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\n"))
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		fl.Flush()
		lastSent = ev.Seq
	}

	catchUp, err := st.EventsAfterFilter(r.Context(), lastSent, flt, 200)
	if err == nil {
		for i := range catchUp {
			writeSSE(catchUp[i])
		}
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(ev)
		case <-keepalive.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			fl.Flush()
		}
	}
}

// registerEvents mounts the event-journal routes: the REST catch-up query and the live SSE stream (the stream route accepts the JWT via ?token=).
func registerEvents(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("/api/events", s.rbacRoleGate(s.handleEvents, "admin"))
	mux.HandleFunc("/api/events/stream", s.rbacRoleGate(s.handleEventStream, "admin"))
	mux.HandleFunc("/admin/events", s.rbacRoleGate(s.handleEvents, "admin"))
	mux.HandleFunc("/admin/events/stream", s.rbacRoleGate(s.handleEventStream, "admin"))
}
