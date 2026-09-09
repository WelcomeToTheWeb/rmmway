// wire_reports.go — gap #8b: reports (wave 3, lane C).
// Wires the reports store into the server for scheduled + on-demand
// report generation.
package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/reports"
)

// wireReports builds the reports store (Postgres-backed). Returns nil
// only if hasPG is false.
func wireReports(hasPG bool, pgPool *pgxpool.Pool) *reports.Store {
	if !hasPG {
		return nil
	}
	return reports.NewStore(pgPool)
}
