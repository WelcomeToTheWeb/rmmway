package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- helpdesk / ticketing routes (gap #7, wave 3, lane B) ------------------

// registerTickets mounts the ticket routes:
//
//	GET   /{api|admin}/tickets                     list (status/priority queue filters)
//	POST  /{api|admin}/tickets                     create ticket
//	GET   /{api|admin}/tickets/{id}                get ticket
//	PATCH /{api|admin}/tickets/{id}                update ticket
//	POST  /{api|admin}/tickets/{id}/transition     transition status
//	POST  /{api|admin}/tickets/{id}/notes          add note
//	GET   /{api|admin}/tickets/{id}/notes          list notes
func registerTickets(s *Server, mux *http.ServeMux) {
	for _, p := range []string{"/api", "/admin"} {
		mux.HandleFunc(p+"/tickets", s.rbacGate(s.handleTickets))
		mux.HandleFunc(p+"/tickets/", s.rbacGate(s.handleTicketsSub))
	}
}

func (s *Server) ticketsUnwired(w http.ResponseWriter) {
	http.Error(w, "ticket system not configured (Postgres required)", http.StatusServiceUnavailable)
}

// handleTickets routes GET (list) and POST (create) on /{api|admin}/tickets.
func (s *Server) handleTickets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.ticketList(w, r)
	case http.MethodPost:
		s.createTicket(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// ticketList — GET /{api|admin}/tickets
//
// Query params: status=open,in_progress (comma-separated), queue,
// assigned_to, client_id, priority=low,medium (comma-separated), source.
//
//	200  [Ticket{…}] most recent first (capped at 500)
//	503  ticket store not wired
func (s *Server) ticketList(w http.ResponseWriter, r *http.Request) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	filter := &store.TicketListFilter{}
	if vals := r.URL.Query()["status"]; len(vals) > 0 {
		filter.Status = strings.Split(vals[0], ",")
	}
	if q := r.URL.Query().Get("queue"); q != "" {
		filter.Queue = q
	}
	if q := r.URL.Query().Get("assigned_to"); q != "" {
		filter.AssignedTo = &q
	}
	if q := r.URL.Query().Get("client_id"); q != "" {
		filter.ClientID = &q
	}
	if vals := r.URL.Query()["priority"]; len(vals) > 0 {
		filter.Priority = strings.Split(vals[0], ",")
	}
	if q := r.URL.Query().Get("source"); q != "" {
		filter.Source = &q
	}
	tickets, err := s.tickets.List(r.Context(), filter)
	if err != nil {
		http.Error(w, "ticket list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tickets)
}

// createTicket — POST /{api|admin}/tickets
//
// Body: {title, description, queue?, priority?, device_id?, client_id?, assigned_to?, source?, heal_run_id?}
//
//	201  the created ticket
//	400  missing title, bad priority, bad source, bad body
//	503  ticket store not wired
func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	var in struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Queue       string  `json:"queue"`
		Priority    string  `json:"priority"`
		DeviceID    *string `json:"device_id"`
		ClientID    *string `json:"client_id"`
		AssignedTo  *string `json:"assigned_to"`
		Reporter    *string `json:"reporter"`
		Source      string  `json:"source"`
		HealRunID   *int64  `json:"heal_run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(title) > 256 {
		http.Error(w, "title too long (max 256 chars)", http.StatusBadRequest)
		return
	}
	priority := strings.TrimSpace(in.Priority)
	if priority == "" {
		priority = "medium"
	}
	if !store.ValidTicketPriority(priority) {
		http.Error(w, "priority must be low, medium, high, or critical", http.StatusBadRequest)
		return
	}
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = "manual"
	}
	if source != "manual" && source != "heal" && source != "flow" && source != "alert" {
		http.Error(w, "source must be manual, heal, flow, or alert", http.StatusBadRequest)
		return
	}
	queue := strings.TrimSpace(in.Queue)
	if queue == "" {
		queue = "general"
	}
	ticket := &store.Ticket{
		Title:       title,
		Description: in.Description,
		Queue:       queue,
		Priority:    priority,
		DeviceID:    in.DeviceID,
		ClientID:    in.ClientID,
		AssignedTo:  in.AssignedTo,
		Reporter:    in.Reporter,
		Source:      source,
		HealRunID:   in.HealRunID,
	}
	created, err := s.tickets.Create(r.Context(), ticket)
	if err != nil {
		http.Error(w, "create ticket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleTicketsSub dispatches /{api|admin}/tickets/{id}[…]:
//
//	/{id}            → getTicket | patchTicket
//	/{id}/transition → transitionTicket
//	/{id}/notes      → listTicketNotes | createTicketNote
func (s *Server) handleTicketsSub(w http.ResponseWriter, r *http.Request) {
	rest := r.URL.Path
	rest = strings.TrimPrefix(rest, "/api/tickets/")
	rest = strings.TrimPrefix(rest, "/admin/tickets/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		switch r.Method {
		case http.MethodGet:
			s.getTicket(w, r, parts[0])
		case http.MethodPatch:
			s.patchTicket(w, r, parts[0])
		default:
			http.Error(w, "GET or PATCH only", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "transition" {
		s.transitionTicket(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "notes" {
		switch r.Method {
		case http.MethodGet:
			s.listTicketNotes(w, r, parts[0])
		case http.MethodPost:
			s.createTicketNote(w, r, parts[0])
		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// getTicket — GET /{api|admin}/tickets/{id}
func (s *Server) getTicket(w http.ResponseWriter, r *http.Request, id string) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	ticket, err := s.tickets.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		http.Error(w, "get ticket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}

// patchTicket — PATCH /{api|admin}/tickets/{id}
//
// Body: {"title"?: string, "description"?: string, "queue"?: string,
//
//	  "priority"?: string, "device_id"?: string, "client_id"?: string,
//	  "assigned_to"?: string}
//
//		200  the updated ticket
//		400  bad body / bad priority
//		404  unknown ticket
//		503  ticket store not wired
func (s *Server) patchTicket(w http.ResponseWriter, r *http.Request, id string) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	var in struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Queue       *string `json:"queue"`
		Priority    *string `json:"priority"`
		DeviceID    *string `json:"device_id"`
		ClientID    *string `json:"client_id"`
		AssignedTo  *string `json:"assigned_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Priority != nil && !store.ValidTicketPriority(*in.Priority) {
		http.Error(w, "priority must be low, medium, high, or critical", http.StatusBadRequest)
		return
	}
	updates := &store.TicketUpdate{
		Title:       in.Title,
		Description: in.Description,
		Queue:       in.Queue,
		Priority:    in.Priority,
		DeviceID:    in.DeviceID,
		ClientID:    in.ClientID,
		AssignedTo:  in.AssignedTo,
	}
	updated, err := s.tickets.Update(r.Context(), id, updates)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		http.Error(w, "update ticket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// transitionTicket — POST /{api|admin}/tickets/{id}/transition
//
// Body: {"status": "in_progress"} (or "resolved", "closed", "open")
//
//	200  the ticket with its new status and timestamps
//	400  missing/invalid status
//	404  unknown ticket
//	503  ticket store not wired
func (s *Server) transitionTicket(w http.ResponseWriter, r *http.Request, id string) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !store.ValidTicketStatus(in.Status) {
		http.Error(w, "status must be open, in_progress, resolved, or closed", http.StatusBadRequest)
		return
	}
	updated, err := s.tickets.Transition(r.Context(), id, in.Status)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		http.Error(w, "transition ticket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// listTicketNotes — GET /{api|admin}/tickets/{id}/notes
func (s *Server) listTicketNotes(w http.ResponseWriter, r *http.Request, id string) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	notes, err := s.tickets.ListNotes(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		http.Error(w, "list notes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, notes)
}

// createTicketNote — POST /{api|admin}/tickets/{id}/notes
//
// Body: {author?, content, is_internal?}
//
//	201  the created note
//	400  missing content
//	404  unknown ticket
//	503  ticket store not wired
func (s *Server) createTicketNote(w http.ResponseWriter, r *http.Request, id string) {
	if s.tickets == nil {
		s.ticketsUnwired(w)
		return
	}
	var in struct {
		Author     *string `json:"author"`
		Content    string  `json:"content"`
		IsInternal *bool   `json:"is_internal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	note := &store.TicketNote{
		TicketID: id,
		Author:   in.Author,
		Content:  in.Content,
	}
	if in.IsInternal != nil {
		note.IsInternal = *in.IsInternal
	}
	created, err := s.tickets.CreateNote(r.Context(), note)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			http.Error(w, "ticket not found", http.StatusNotFound)
			return
		}
		http.Error(w, "create note: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}
