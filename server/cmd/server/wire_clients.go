package main

import (
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// wireClients builds the MSP client/tenant registry (gap #2, wave 1).
// Postgres-backed (0010_clients: the clients table + devices.client_id);
// in-memory mode has no client model, so /{api|admin}/clients* 503.
// Returns nil when disabled. Pure move out of main() (wave-0 F2 pattern).
func wireClients(hasPG bool, pgPool *pgxpool.Pool) store.ClientStore {
	if !hasPG {
		return nil
	}
	log.Println("clients: MSP client/tenant model enabled (0010_clients)")
	return store.NewPostgresClientStore(pgPool)
}
