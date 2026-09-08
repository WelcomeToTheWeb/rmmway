package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- in-memory ClientStore --------------------------------------------------

func seedDefaultClient(s *MemoryClientStore) {
	s.SeedDefaultClient()
}

func TestMemoryClientStoreCRUD(t *testing.T) {
	s := NewMemoryClientStore()
	seedDefaultClient(s)
	ctx := context.Background()

	acme, err := s.Create(ctx, "Acme Corp", "top client")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acme.ID == "" || acme.ID == DefaultClientID {
		t.Fatalf("create: expected a server-minted id, got %q", acme.ID)
	}
	if _, err := s.Create(ctx, "Acme Corp", "dup"); !errors.Is(err, ErrClientNameExists) {
		t.Fatalf("duplicate create: want ErrClientNameExists, got %v", err)
	}

	got, err := s.Get(ctx, acme.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Acme Corp" || got.Description != "top client" {
		t.Fatalf("get: got %+v", got)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: want ErrNotFound, got %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "Acme Corp" || list[1].Name != "Unassigned" {
		t.Fatalf("list: want Acme Corp then Unassigned (name order), got %+v", list)
	}

	desc := "updated desc"
	upd, err := s.Update(ctx, acme.ID, nil, &desc)
	if err != nil {
		t.Fatalf("update desc: %v", err)
	}
	if upd.Name != "Acme Corp" || upd.Description != "updated desc" {
		t.Fatalf("update desc: got %+v", upd)
	}

	name := "Acme Corporation"
	upd, err = s.Update(ctx, acme.ID, &name, nil)
	if err != nil {
		t.Fatalf("update name: %v", err)
	}
	if upd.Name != "Acme Corporation" {
		t.Fatalf("update name: got %+v", upd)
	}
	un := "Unassigned"
	if _, err := s.Update(ctx, acme.ID, &un, nil); !errors.Is(err, ErrClientNameExists) {
		t.Fatalf("rename to taken name: want ErrClientNameExists, got %v", err)
	}
	if _, err := s.Update(ctx, "nope", &name, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}

	// A no-op update (both nil) returns the current row.
	noop, err := s.Update(ctx, acme.ID, nil, nil)
	if err != nil || noop.Name != "Acme Corporation" {
		t.Fatalf("no-op update: got %+v, %v", noop, err)
	}
}

// ---- in-memory device scoping ------------------------------------------------

func TestMemoryDeviceClientScoping(t *testing.T) {
	d := NewMemoryDeviceStore()
	s := NewMemoryClientStore()
	seedDefaultClient(s)
	ctx := context.Background()

	for _, id := range []string{"dev-1", "dev-2", "dev-3"} {
		if err := d.Register(ctx, id, id, "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	acme, err := s.Create(ctx, "Acme", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.SetClient(ctx, "dev-1", acme.ID); err != nil {
		t.Fatalf("setclient dev-1: %v", err)
	}
	if err := d.SetClient(ctx, "dev-2", acme.ID); err != nil {
		t.Fatalf("setclient dev-2: %v", err)
	}
	if err := d.SetClient(ctx, "dev-404", acme.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setclient unknown: want ErrNotFound, got %v", err)
	}

	all, err := d.ListByClient(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("list all: got %d, %v", len(all), err)
	}
	acmeDevs, err := d.ListByClient(ctx, acme.ID)
	if err != nil || len(acmeDevs) != 2 {
		t.Fatalf("list acme: got %d, %v", len(acmeDevs), err)
	}
	un, err := d.ListByClient(ctx, DefaultClientID)
	if err != nil || len(un) != 1 || un[0].ID != "dev-3" {
		t.Fatalf("list unassigned: got %+v, %v", un, err)
	}

	// Unassigning ("" client) moves dev-1 back under the default client.
	if err := d.SetClient(ctx, "dev-1", ""); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	acmeDevs, _ = d.ListByClient(ctx, acme.ID)
	un, _ = d.ListByClient(ctx, DefaultClientID)
	if len(acmeDevs) != 1 || len(un) != 2 {
		t.Fatalf("after unassign: acme=%d unassigned=%d", len(acmeDevs), len(un))
	}

	// The device row carries the assignment for the API layer.
	got, err := d.Get(ctx, "dev-2")
	if err != nil || got.ClientID != acme.ID {
		t.Fatalf("get client_id: got %+v, %v", got, err)
	}
}

// ---- Postgres ClientStore (scratch DB) ---------------------------------------

// TestPostgresClientsLive exercises the pgx ClientStore + the 0010
// migration's device scoping against a scratch database:
//
//   - the 'unassigned' client is seeded exactly once (idempotent re-run),
//   - Create/Get/List/Update round-trip, duplicate names are refused,
//   - SetClient/ListByClient, including NULL (unassigned) semantics.
//
// Requires RMMWAY_TEST_PG_DSN; skipped otherwise.
func TestPostgresClientsLive(t *testing.T) {
	dsn := os.Getenv("RMMWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RMMWAY_TEST_PG_DSN not set — skipping clients Postgres test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	dbName := "rmmway_test_" + time.Now().Format("20060102150405")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName)

	u.Path = "/" + dbName
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping scratch db: %v", err)
	}

	t.Chdir("../../..")
	if n, err := Migrate(ctx, db, "server/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	} else if n != 10 {
		t.Fatalf("expected 10 migrations applied, got %d", n)
	}
	// Idempotent re-run: nothing new, seed still exactly one row.
	if n, err := Migrate(ctx, db, "server/migrations"); err != nil || n != 0 {
		t.Fatalf("re-migrate: want 0 applied, got %d, %v", n, err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM clients WHERE id = 'unassigned'`).Scan(&count); err != nil {
		t.Fatalf("seed check: %v", err)
	}
	if count != 1 {
		t.Fatalf("seed: want exactly 1 'unassigned' client, got %d", count)
	}

	s := NewPostgresClientStore(db)
	devices := NewPostgresDevices(db)

	acme, err := s.Create(ctx, "Acme Corp", "top client")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acme.ID == "" || acme.ID == DefaultClientID {
		t.Fatalf("create: expected a server-minted id, got %q", acme.ID)
	}
	if _, err := s.Create(ctx, "Acme Corp", "dup"); !errors.Is(err, ErrClientNameExists) {
		t.Fatalf("duplicate create: want ErrClientNameExists, got %v", err)
	}
	// Uniqueness is case-insensitive (memory store + API contract).
	if _, err := s.Create(ctx, "ACME corp", "case dup"); !errors.Is(err, ErrClientNameExists) {
		t.Fatalf("case-variant duplicate create: want ErrClientNameExists, got %v", err)
	}

	got, err := s.Get(ctx, acme.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Acme Corp" || got.Description != "top client" {
		t.Fatalf("get: got %+v", got)
	}
	def, err := s.Get(ctx, DefaultClientID)
	if err != nil || def.Name != "Unassigned" {
		t.Fatalf("get default: got %+v, %v", def, err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: want ErrNotFound, got %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "Acme Corp" || list[1].Name != "Unassigned" {
		t.Fatalf("list: want name-ordered [Acme Corp Unassigned], got %+v", list)
	}

	name, desc := "Acme Corporation", "updated"
	upd, err := s.Update(ctx, acme.ID, &name, &desc)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "Acme Corporation" || upd.Description != "updated" {
		t.Fatalf("update: got %+v", upd)
	}
	un := "Unassigned"
	if _, err := s.Update(ctx, acme.ID, &un, nil); !errors.Is(err, ErrClientNameExists) {
		t.Fatalf("rename to taken name: want ErrClientNameExists, got %v", err)
	}
	if _, err := s.Update(ctx, "nope", &name, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}

	// Device scoping: enroll two devices directly (client_id NULL),
	// assign one, and verify the default client sees the unassigned one.
	for _, id := range []string{"dev-1", "dev-2"} {
		if err := devices.Register(ctx, id, id, "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	if err := devices.SetClient(ctx, "dev-1", acme.ID); err != nil {
		t.Fatalf("setclient: %v", err)
	}
	if err := devices.SetClient(ctx, "dev-404", acme.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setclient unknown: want ErrNotFound, got %v", err)
	}

	all, err := devices.ListByClient(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: got %d, %v", len(all), err)
	}
	acmeDevs, err := devices.ListByClient(ctx, acme.ID)
	if err != nil || len(acmeDevs) != 1 || acmeDevs[0].ID != "dev-1" {
		t.Fatalf("list acme: got %+v, %v", acmeDevs, err)
	}
	unDevs, err := devices.ListByClient(ctx, DefaultClientID)
	if err != nil || len(unDevs) != 1 || unDevs[0].ID != "dev-2" {
		t.Fatalf("list unassigned (NULL): got %+v, %v", unDevs, err)
	}

	// The Get/List round-trip carries the assignment.
	d1, err := devices.Get(ctx, "dev-1")
	if err != nil || d1.ClientID != acme.ID {
		t.Fatalf("get client_id: got %+v, %v", d1, err)
	}
	d2, err := devices.Get(ctx, "dev-2")
	if err != nil || d2.ClientID != "" {
		t.Fatalf("unassigned client_id: want empty, got %+v, %v", d2, err)
	}

	// Unassigning moves dev-1 back under the default client (NULL).
	if err := devices.SetClient(ctx, "dev-1", ""); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	acmeDevs, _ = devices.ListByClient(ctx, acme.ID)
	unDevs, _ = devices.ListByClient(ctx, DefaultClientID)
	if len(acmeDevs) != 0 || len(unDevs) != 2 {
		t.Fatalf("after unassign: acme=%d unassigned=%d", len(acmeDevs), len(unDevs))
	}

	// Alert scoping (ListClient): one open alert per device, then the
	// ?client= subqueries must isolate each client's inbox — including
	// the default client's NULL-client devices.
	alertsStore := NewAlertStore(db, 3)
	for _, devID := range []string{"dev-1", "dev-2"} {
		if _, err := db.Exec(ctx, `INSERT INTO alerts (device_id, name, score, channel, value, first_at, last_at)
			VALUES ($1, 'cpu.utilization_percent', 5.0, 'trend', 99.0, now(), now())`, devID); err != nil {
			t.Fatalf("seed alert %s: %v", devID, err)
		}
	}
	if err := devices.SetClient(ctx, "dev-2", acme.ID); err != nil {
		t.Fatalf("reassign dev-2: %v", err)
	}
	acmeAlerts, err := alertsStore.ListClient(ctx, acme.ID, "", "", 100)
	if err != nil || len(acmeAlerts) != 1 || acmeAlerts[0].DeviceID != "dev-2" {
		t.Fatalf("listclient acme: got %+v, %v", acmeAlerts, err)
	}
	unAlerts, err := alertsStore.ListClient(ctx, DefaultClientID, "", "", 100)
	if err != nil || len(unAlerts) != 1 || unAlerts[0].DeviceID != "dev-1" {
		t.Fatalf("listclient default (NULL devices): got %+v, %v", unAlerts, err)
	}
	allAlerts, err := alertsStore.List(ctx, "", "", 100)
	if err != nil || len(allAlerts) != 2 {
		t.Fatalf("list all: got %+v, %v", allAlerts, err)
	}
}
