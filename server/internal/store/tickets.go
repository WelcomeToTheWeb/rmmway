package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- tickets + ticket notes (gap #7, wave 3, lane B) ------------------------

// Ticket statuses.
const (
	TicketStatusOpen      = "open"
	TicketStatusInProgress = "in_progress"
	TicketStatusResolved  = "resolved"
	TicketStatusClosed    = "closed"
)

// Ticket priorities.
const (
	TicketPriorityLow      = "low"
	TicketPriorityMedium   = "medium"
	TicketPriorityHigh     = "high"
	TicketPriorityCritical = "critical"
)

// TicketSources.
const (
	TicketSourceManual = "manual"
	TicketSourceHeal   = "heal"
	TicketSourceFlow   = "flow"
	TicketSourceAlert  = "alert"
)

// Ticket is one helpdesk item.
type Ticket struct {
	ID                  string
	Title               string
	Description         string
	Queue               string
	Status              string
	Priority            string
	DeviceID            *string
	ClientID            *string
	AssignedTo          *string
	Reporter            *string
	FirstResponseDue    *time.Time
	ResolutionDue       *time.Time
	FirstResponseAt     *time.Time
	ResolvedAt          *time.Time
	ClosedAt            *time.Time
	HealRunID           *int64
	Source              string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// TicketNote is one activity note on a ticket.
type TicketNote struct {
	ID          string
	TicketID    string
	Author      *string
	Content     string
	IsInternal  bool
	CreatedAt   time.Time
}

// ErrTicketNotFound is returned when a ticket is unknown.
var ErrTicketNotFound = errors.New("ticket not found")

// ValidTicketStatus reports whether s is a known status.
func ValidTicketStatus(s string) bool {
	switch s {
	case TicketStatusOpen, TicketStatusInProgress, TicketStatusResolved, TicketStatusClosed:
		return true
	}
	return false
}

// ValidTicketPriority reports whether p is a known priority.
func ValidTicketPriority(p string) bool {
	switch p {
	case TicketPriorityLow, TicketPriorityMedium, TicketPriorityHigh, TicketPriorityCritical:
		return true
	}
	return false
}

// TicketStore persists tickets and their notes.
type TicketStore interface {
	// List returns tickets filtered by the given criteria, most recent
	// first. A nil filter returns all tickets.
	List(ctx context.Context, filter *TicketListFilter) ([]*Ticket, error)
	// Count returns the number of tickets matching the filter.
	Count(ctx context.Context, filter *TicketListFilter) (int, error)
	// Get returns one ticket by id; ErrTicketNotFound when unknown.
	Get(ctx context.Context, id string) (*Ticket, error)
	// Create mints a new ticket (server-minted "tkt-" id).
	Create(ctx context.Context, ticket *Ticket) (*Ticket, error)
	// Update patches the ticket (title, description, queue, status,
	// priority, assignment, client, device, SLA timestamps, etc.).
	// Status transitions are validated.
	Update(ctx context.Context, id string, updates *TicketUpdate) (*Ticket, error)
	// Transition changes the ticket's status, handling the side effects
	// (first_response_at, resolved_at, closed_at).
	Transition(ctx context.Context, id, newStatus string) (*Ticket, error)
	// CreateNote adds an activity note to a ticket.
	CreateNote(ctx context.Context, note *TicketNote) (*TicketNote, error)
	// ListNotes returns all notes for a ticket, oldest first.
	ListNotes(ctx context.Context, ticketID string) ([]*TicketNote, error)
}

// TicketListFilter selects a subset of tickets.
type TicketListFilter struct {
	Status     []string // empty = all statuses
	Queue      string
	AssignedTo *string
	ClientID   *string
	DeviceID   *string
	Priority   []string
	Source     *string
}

// TicketUpdate holds the fields to patch on an update.
type TicketUpdate struct {
	Title           *string
	Description     *string
	Queue           *string
	Priority        *string
	DeviceID        *string
	ClientID        *string
	AssignedTo      *string
	FirstResponseDue *time.Time
	ResolutionDue   *time.Time
}

// ---- id mints ----------------------------------------------------------------

func newTicketID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "tkt-" + hex.EncodeToString(raw), nil
}

func newNoteID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "note-" + hex.EncodeToString(raw), nil
}

// ---- Postgres implementation ------------------------------------------------

const ticketColumns = `id, title, description, queue, status, priority,
	device_id, client_id, assigned_to, reporter,
	sla_first_response_due, sla_resolution_due, first_response_at,
	resolved_at, closed_at, heal_run_id, source, created_at, updated_at`

type PostgresTicketStore struct {
	db *pgxpool.Pool
}

func NewPostgresTicketStore(db *pgxpool.Pool) *PostgresTicketStore {
	return &PostgresTicketStore{db: db}
}

func (s *PostgresTicketStore) List(ctx context.Context, filter *TicketListFilter) ([]*Ticket, error) {
	query := `SELECT ` + ticketColumns + ` FROM tickets`
	var args []any
	var where []string
	if filter != nil {
		if len(filter.Status) > 0 {
			where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)+1))
			args = append(args, filter.Status)
		}
		if filter.Queue != "" {
			where = append(where, fmt.Sprintf("queue = $%d", len(args)+1))
			args = append(args, filter.Queue)
		}
		if filter.AssignedTo != nil {
			where = append(where, fmt.Sprintf("assigned_to = $%d", len(args)+1))
			args = append(args, *filter.AssignedTo)
		}
		if filter.ClientID != nil {
			where = append(where, fmt.Sprintf("client_id = $%d", len(args)+1))
			args = append(args, *filter.ClientID)
		}
		if filter.DeviceID != nil {
			where = append(where, fmt.Sprintf("device_id = $%d", len(args)+1))
			args = append(args, *filter.DeviceID)
		}
		if len(filter.Priority) > 0 {
			where = append(where, fmt.Sprintf("priority = ANY($%d)", len(args)+1))
			args = append(args, filter.Priority)
		}
		if filter.Source != nil {
			where = append(where, fmt.Sprintf("source = $%d", len(args)+1))
			args = append(args, *filter.Source)
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at DESC LIMIT 500`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresTicketStore) Count(ctx context.Context, filter *TicketListFilter) (int, error) {
	query := `SELECT count(*) FROM tickets`
	var args []any
	var where []string
	if filter != nil {
		if len(filter.Status) > 0 {
			where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)+1))
			args = append(args, filter.Status)
		}
		if filter.Queue != "" {
			where = append(where, fmt.Sprintf("queue = $%d", len(args)+1))
			args = append(args, filter.Queue)
		}
		if filter.AssignedTo != nil {
			where = append(where, fmt.Sprintf("assigned_to = $%d", len(args)+1))
			args = append(args, *filter.AssignedTo)
		}
		if filter.ClientID != nil {
			where = append(where, fmt.Sprintf("client_id = $%d", len(args)+1))
			args = append(args, *filter.ClientID)
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	err := s.db.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

func (s *PostgresTicketStore) Get(ctx context.Context, id string) (*Ticket, error) {
	t, err := scanTicket(s.db.QueryRow(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	return t, nil
}

func (s *PostgresTicketStore) Create(ctx context.Context, ticket *Ticket) (*Ticket, error) {
	id, err := newTicketID()
	if err != nil {
		return nil, err
	}
	ticket.ID = id
	now := time.Now().UTC()
	ticket.CreatedAt = now
	ticket.UpdatedAt = now
	if ticket.Status == "" {
		ticket.Status = TicketStatusOpen
	}
	if ticket.Priority == "" {
		ticket.Priority = TicketPriorityMedium
	}
	if ticket.Queue == "" {
		ticket.Queue = "general"
	}
	if ticket.Source == "" {
		ticket.Source = TicketSourceManual
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO tickets (id, title, description, queue, status, priority, device_id, client_id, assigned_to, reporter, sla_first_response_due, sla_resolution_due, heal_run_id, source, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		ticket.ID, ticket.Title, ticket.Description, ticket.Queue, ticket.Status, ticket.Priority,
		ticket.DeviceID, ticket.ClientID, ticket.AssignedTo, ticket.Reporter,
		ticket.FirstResponseDue, ticket.ResolutionDue, ticket.HealRunID, ticket.Source,
		ticket.CreatedAt, ticket.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return ticket, nil
}

func (s *PostgresTicketStore) Update(ctx context.Context, id string, updates *TicketUpdate) (*Ticket, error) {
	sets := make([]string, 0)
	args := make([]any, 0)
	if updates.Title != nil {
		args = append(args, *updates.Title)
		sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
	}
	if updates.Description != nil {
		args = append(args, *updates.Description)
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
	}
	if updates.Queue != nil {
		args = append(args, *updates.Queue)
		sets = append(sets, fmt.Sprintf("queue = $%d", len(args)))
	}
	if updates.Priority != nil {
		args = append(args, *updates.Priority)
		sets = append(sets, fmt.Sprintf("priority = $%d", len(args)))
	}
	if updates.DeviceID != nil {
		args = append(args, *updates.DeviceID)
		sets = append(sets, fmt.Sprintf("device_id = $%d", len(args)))
	}
	if updates.ClientID != nil {
		args = append(args, *updates.ClientID)
		sets = append(sets, fmt.Sprintf("client_id = $%d", len(args)))
	}
	if updates.AssignedTo != nil {
		args = append(args, *updates.AssignedTo)
		sets = append(sets, fmt.Sprintf("assigned_to = $%d", len(args)))
	}
	if updates.FirstResponseDue != nil {
		args = append(args, *updates.FirstResponseDue)
		sets = append(sets, fmt.Sprintf("sla_first_response_due = $%d", len(args)))
	}
	if updates.ResolutionDue != nil {
		args = append(args, *updates.ResolutionDue)
		sets = append(sets, fmt.Sprintf("sla_resolution_due = $%d", len(args)))
	}
	if len(sets) == 0 {
		return s.Get(ctx, id)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)
	_, err := s.db.Exec(ctx, `UPDATE tickets SET `+strings.Join(sets, ", ")+` WHERE id = $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *PostgresTicketStore) Transition(ctx context.Context, id, newStatus string) (*Ticket, error) {
	if !ValidTicketStatus(newStatus) {
		return nil, fmt.Errorf("invalid ticket status %q", newStatus)
	}
	var extraSets []string
	switch newStatus {
	case TicketStatusResolved:
		extraSets = append(extraSets, "resolved_at = now()")
	case TicketStatusClosed:
		extraSets = append(extraSets, "closed_at = now()")
	}
	baseSets := "status = $1, updated_at = now()"
	if len(extraSets) > 0 {
		sets := append([]string{baseSets}, extraSets...)
		_, err := s.db.Exec(ctx,
			`UPDATE tickets SET `+strings.Join(sets, ", ")+` WHERE id = $2`,
			newStatus, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrTicketNotFound
			}
			return nil, err
		}
	} else {
		_, err := s.db.Exec(ctx,
			`UPDATE tickets SET `+baseSets+` WHERE id = $2`,
			newStatus, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrTicketNotFound
			}
			return nil, err
		}
	}
	return s.Get(ctx, id)
}

func (s *PostgresTicketStore) CreateNote(ctx context.Context, note *TicketNote) (*TicketNote, error) {
	id, err := newNoteID()
	if err != nil {
		return nil, err
	}
	note.ID = id
	note.CreatedAt = time.Now().UTC()
	_, err = s.db.Exec(ctx,
		`INSERT INTO ticket_notes (id, ticket_id, author, content, is_internal, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		note.ID, note.TicketID, note.Author, note.Content, note.IsInternal, note.CreatedAt)
	if err != nil {
		return nil, err
	}
	return note, nil
}

func (s *PostgresTicketStore) ListNotes(ctx context.Context, ticketID string) ([]*TicketNote, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, ticket_id, author, content, is_internal, created_at
		 FROM ticket_notes WHERE ticket_id = $1 ORDER BY created_at ASC`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TicketNote
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func scanTicket(row pgx.Row) (*Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.Title, &t.Description, &t.Queue, &t.Status, &t.Priority,
		&t.DeviceID, &t.ClientID, &t.AssignedTo, &t.Reporter,
		&t.FirstResponseDue, &t.ResolutionDue, &t.FirstResponseAt,
		&t.ResolvedAt, &t.ClosedAt,
		&t.HealRunID, &t.Source, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func scanNote(row pgx.Row) (*TicketNote, error) {
	var n TicketNote
	err := row.Scan(&n.ID, &n.TicketID, &n.Author, &n.Content, &n.IsInternal, &n.CreatedAt)
	return &n, err
}

// ---- in-memory implementation (unit tests) ----------------------------------

type MemoryTicketStore struct {
	mu      sync.RWMutex
	tickets map[string]*Ticket
	notes   map[string][]*TicketNote
}

func NewMemoryTicketStore() *MemoryTicketStore {
	return &MemoryTicketStore{
		tickets: make(map[string]*Ticket),
		notes:   make(map[string][]*TicketNote),
	}
}

func (s *MemoryTicketStore) List(_ context.Context, filter *TicketListFilter) ([]*Ticket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Ticket
	for _, t := range s.tickets {
		if matchesFilter(t, filter) {
			out = append(out, cloneTicket(t))
		}
	}
	return out, nil
}

func (s *MemoryTicketStore) Count(_ context.Context, filter *TicketListFilter) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, t := range s.tickets {
		if matchesFilter(t, filter) {
			n++
		}
	}
	return n, nil
}

func (s *MemoryTicketStore) Get(_ context.Context, id string) (*Ticket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tickets[id]
	if !ok {
		return nil, ErrTicketNotFound
	}
	return cloneTicket(t), nil
}

func (s *MemoryTicketStore) Create(_ context.Context, ticket *Ticket) (*Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := newTicketID()
	if err != nil {
		return nil, err
	}
	ticket.ID = id
	now := time.Now().UTC()
	ticket.CreatedAt = now
	ticket.UpdatedAt = now
	if ticket.Status == "" {
		ticket.Status = TicketStatusOpen
	}
	if ticket.Priority == "" {
		ticket.Priority = TicketPriorityMedium
	}
	if ticket.Queue == "" {
		ticket.Queue = "general"
	}
	if ticket.Source == "" {
		ticket.Source = TicketSourceManual
	}
	s.tickets[id] = ticket
	return cloneTicket(ticket), nil
}

func (s *MemoryTicketStore) Update(_ context.Context, id string, updates *TicketUpdate) (*Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[id]
	if !ok {
		return nil, ErrTicketNotFound
	}
	if updates.Title != nil {
		t.Title = *updates.Title
	}
	if updates.Description != nil {
		t.Description = *updates.Description
	}
	if updates.Queue != nil {
		t.Queue = *updates.Queue
	}
	if updates.Priority != nil {
		t.Priority = *updates.Priority
	}
	if updates.DeviceID != nil {
		t.DeviceID = updates.DeviceID
	}
	if updates.ClientID != nil {
		t.ClientID = updates.ClientID
	}
	if updates.AssignedTo != nil {
		t.AssignedTo = updates.AssignedTo
	}
	if updates.FirstResponseDue != nil {
		t.FirstResponseDue = updates.FirstResponseDue
	}
	if updates.ResolutionDue != nil {
		t.ResolutionDue = updates.ResolutionDue
	}
	t.UpdatedAt = time.Now().UTC()
	return cloneTicket(t), nil
}

func (s *MemoryTicketStore) Transition(_ context.Context, id, newStatus string) (*Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidTicketStatus(newStatus) {
		return nil, fmt.Errorf("invalid ticket status %q", newStatus)
	}
	t, ok := s.tickets[id]
	if !ok {
		return nil, ErrTicketNotFound
	}
	t.Status = newStatus
	now := time.Now().UTC()
	t.UpdatedAt = now
	switch newStatus {
	case TicketStatusResolved:
		t.ResolvedAt = &now
	case TicketStatusClosed:
		t.ClosedAt = &now
	}
	return cloneTicket(t), nil
}

func (s *MemoryTicketStore) CreateNote(_ context.Context, note *TicketNote) (*TicketNote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tickets[note.TicketID]; !ok {
		return nil, ErrTicketNotFound
	}
	id, err := newNoteID()
	if err != nil {
		return nil, err
	}
	note.ID = id
	note.CreatedAt = time.Now().UTC()
	s.notes[note.TicketID] = append(s.notes[note.TicketID], note)
	return note, nil
}

func (s *MemoryTicketStore) ListNotes(_ context.Context, ticketID string) ([]*TicketNote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.tickets[ticketID]; !ok {
		return nil, ErrTicketNotFound
	}
	return s.notes[ticketID], nil
}

func matchesFilter(t *Ticket, filter *TicketListFilter) bool {
	if filter == nil {
		return true
	}
	if len(filter.Status) > 0 {
		found := false
		for _, s := range filter.Status {
			if t.Status == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if filter.Queue != "" && t.Queue != filter.Queue {
		return false
	}
	if filter.AssignedTo != nil && (t.AssignedTo == nil || *t.AssignedTo != *filter.AssignedTo) {
		return false
	}
	if filter.ClientID != nil && (t.ClientID == nil || *t.ClientID != *filter.ClientID) {
		return false
	}
	return true
}

func cloneTicket(t *Ticket) *Ticket {
	cp := *t
	return &cp
}