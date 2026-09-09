package main

import (
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// wireUsers builds the operator account store (gap #3, wave 2, lane B).
// Postgres-backed (0011_users: users + user_clients + api_tokens);
// in-memory mode has no account model, so /api/login keeps the legacy
// env/admin_users behavior (grandfathered admin, no scoping).
// Returns nil when disabled. Pure move out of main() (wave-0 F2 pattern).
func wireUsers(hasPG bool, pgPool *pgxpool.Pool) store.UserStore {
	if !hasPG {
		return nil
	}
	log.Println("users: operator accounts + RBAC enabled (0011_users)")
	return store.NewPostgresUserStore(pgPool)
}
