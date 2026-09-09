// wire_maintenance.go — gap #10b: maintenance windows + snooze (wave 3, lane C).
// Wires the maintenance store into the server for the API + baseline/heal
// suppression checks.
package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/maintenance"
)

// wireMaintenance builds the maintenance-window store (Postgres-backed,
// like the flow store). Returns nil only if hasPG is false.
func wireMaintenance(hasPG bool, pgPool *pgxpool.Pool) *maintenance.Store {
	if !hasPG {
		return nil
	}
	return maintenance.NewStore(pgPool)
}
