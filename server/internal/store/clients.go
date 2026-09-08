package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- MSP client/tenant model (gap #2, wave 1, lane B) ---------------------

// DefaultClientID is the id of the seeded "Unassigned" default client
// (0010_clients.sql). Devices whose client_id is NULL or this id are
// surfaced under it, so the retrofit never strands a device.
const DefaultClientID = "unassigned"

// Client is one MSP client/tenant row.
type Client struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// DeviceCount is the fleet rollup the list view renders. The store
	// leaves it 0; the API layer fills it from the device registry (the
	// store does not own the devices table's read path).
	DeviceCount int `json:"device_count"`
}

// ErrClientNameExists is returned when a create or update would violate
// the unique client name (clients.name).
var ErrClientNameExists = errors.New("client name already exists")

// ClientStore persists the MSP client/tenant registry (gap #2).
type ClientStore interface {
	// List returns every client sorted by name.
	List(ctx context.Context) ([]*Client, error)
	// Get returns one client by id; ErrNotFound when unknown.
	Get(ctx context.Context, id string) (*Client, error)
	// Create inserts a new client with a server-minted id;
	// ErrClientNameExists on a duplicate name.
	Create(ctx context.Context, name, description string) (*Client, error)
	// Update patches name and/or description (nil = keep); ErrNotFound
	// when the client is unknown, ErrClientNameExists on a duplicate
	// name taken by another client.
	Update(ctx context.Context, id string, name, description *string) (*Client, error)
}

// newClientID mints a client id the way ingest mints device ids
// ("dev-" + 12 hex): "clt-" + 12 hex from crypto/rand.
func newClientID() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "clt-" + hex.EncodeToString(raw), nil
}

// clientNameExists reports whether a unique-name violation error names the
// clients.name index.
func clientNameExists(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_clients_name"
}

// ---- Postgres implementation ----------------------------------------------

// PostgresClientStore is the pgx-backed ClientStore.
type PostgresClientStore struct {
	db *pgxpool.Pool
}

func NewPostgresClientStore(db *pgxpool.Pool) *PostgresClientStore {
	return &PostgresClientStore{db: db}
}

const clientColumns = `id, name, description, created_at, updated_at`

func (s *PostgresClientStore) List(ctx context.Context) ([]*Client, error) {
	rows, err := s.db.Query(ctx, `SELECT `+clientColumns+` FROM clients ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Client
	for rows.Next() {
		var c Client
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *PostgresClientStore) Get(ctx context.Context, id string) (*Client, error) {
	var c Client
	err := s.db.QueryRow(ctx, `SELECT `+clientColumns+` FROM clients WHERE id = $1`, id).
		Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PostgresClientStore) Create(ctx context.Context, name, description string) (*Client, error) {
	id, err := newClientID()
	if err != nil {
		return nil, err
	}
	c := &Client{ID: id, Name: name, Description: description}
	err = s.db.QueryRow(ctx,
		`INSERT INTO clients (id, name, description) VALUES ($1, $2, $3)
		 RETURNING created_at, updated_at`, id, name, description).
		Scan(&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if clientNameExists(err) {
			return nil, ErrClientNameExists
		}
		return nil, err
	}
	return c, nil
}

func (s *PostgresClientStore) Update(ctx context.Context, id string, name, description *string) (*Client, error) {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 3)
	add := func(col string, v any) {
		args = append(args, v)
		// $1 is the id (prepended at query time); patched columns start at $2.
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)+1))
	}
	if name != nil {
		add("name", *name)
	}
	if description != nil {
		add("description", *description)
	}
	if len(sets) == 0 {
		return s.Get(ctx, id)
	}
	sets = append(sets, "updated_at = now()")
	q := `UPDATE clients SET ` + strings.Join(sets, ", ") +
		` WHERE id = $1 RETURNING ` + clientColumns
	c := &Client{ID: id}
	err := s.db.QueryRow(ctx, q, append([]any{id}, args...)...).
		Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		if clientNameExists(err) {
			return nil, ErrClientNameExists
		}
		return nil, err
	}
	return c, nil
}

// ---- in-memory implementation (unit tests) ---------------------------------

// MemoryClientStore is the in-memory ClientStore (unit tests, and the
// httpapi suite's 503-vs-wired distinction).
type MemoryClientStore struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

func NewMemoryClientStore() *MemoryClientStore {
	return &MemoryClientStore{clients: make(map[string]*Client)}
}

func (s *MemoryClientStore) List(_ context.Context) ([]*Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryClientStore) Get(_ context.Context, id string) (*Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *MemoryClientStore) Create(_ context.Context, name, description string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.Name == name {
			return nil, ErrClientNameExists
		}
	}
	id, err := newClientID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &Client{ID: id, Name: name, Description: description, CreatedAt: now, UpdatedAt: now}
	s.clients[id] = c
	cp := *c
	return &cp, nil
}

func (s *MemoryClientStore) Update(_ context.Context, id string, name, description *string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, ErrNotFound
	}
	if name != nil {
		for _, other := range s.clients {
			if other.ID != id && other.Name == *name {
				return nil, ErrClientNameExists
			}
		}
		c.Name = *name
	}
	if description != nil {
		c.Description = *description
	}
	c.UpdatedAt = time.Now().UTC()
	cp := *c
	return &cp, nil
}
