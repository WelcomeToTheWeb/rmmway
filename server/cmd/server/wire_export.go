package main

import (
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/export"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// wireExport builds the per-client full-export service (W4-3 — the
// no-lock-in promise). One request builds a self-describing ZIP bundle:
// device inventory + config, raw metrics + 1-minute rollups (standard
// Parquet), complete alert history, and a manifest that drives
// verification (export.Verify). Postgres-backed (the data lives in the
// hypertable); in-memory mode has no history to export, so the routes
// 503. Returns nil when disabled. Pure move out of main() (wave-0 F2).
func wireExport(hasPG bool, pgPool *pgxpool.Pool, devicesStore store.DeviceStore, version string) *export.Service {
	var exportSvc *export.Service
	if hasPG {
		exportSvc = export.New(export.Config{
			Devices: devicesStore,
			Metrics: export.NewPostgresMetrics(pgPool),
			Rollups: export.NewPostgresRollups(pgPool),
			Alerts:  export.NewPostgresAlerts(pgPool),
			Version: "rmmway-server/" + version,
		})
		log.Println("export: per-client full export enabled (GET /api/devices/{id}/export)")
	}
	return exportSvc
}
