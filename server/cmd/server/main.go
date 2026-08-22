// Package main is the RMMWay backend server.
//
// W1-5: serves HTTP :8080 (health + admin JSON) and gRPC :50051 (agent
// ingest: Enroll + Stream). W0-1's /healthz still probes the stack.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type probe struct {
	Service string `json:"service"`
	OK      bool   `json:"ok"`
	Latency string `json:"latency,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type health struct {
	OK      bool    `json:"ok"`
	Version string  `json:"version"`
	Probes  []probe `json:"probes"`
}

func runProbes(ctx context.Context) []probe {
	probes := make([]probe, 0, 5)

	{
		dsn := env("RMMWAY_PG_DSN", "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable")
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := pgx.Connect(cctx, dsn)
		cancel()
		p := probe{Service: "timescale"}
		if err == nil {
			pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
			var now time.Time
			qerr := conn.QueryRow(pctx, "SELECT now()").Scan(&now)
			pcancel()
			if qerr == nil {
				p.OK = true
				p.Latency = time.Since(start).Round(time.Millisecond).String()
			} else {
				p.Detail = qerr.Error()
			}
			conn.Close(ctx)
		} else {
			p.Detail = err.Error()
		}
		probes = append(probes, p)
	}
	{
		url := env("RMMWAY_NATS_URL", "nats://localhost:4222")
		start := time.Now()
		p := probe{Service: "nats"}
		nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
		if err == nil {
			p.OK = true
			p.Latency = time.Since(start).Round(time.Millisecond).String()
			nc.Close()
		} else {
			p.Detail = err.Error()
		}
		probes = append(probes, p)
	}
	{
		addr := env("RMMWAY_REDIS_ADDR", "localhost:6379")
		start := time.Now()
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := rdb.Ping(rctx).Result()
		cancel()
		p := probe{Service: "redis"}
		if err == nil {
			p.OK = true
			p.Latency = time.Since(start).Round(time.Millisecond).String()
		} else {
			p.Detail = err.Error()
		}
		rdb.Close()
		probes = append(probes, p)
	}
	{
		endpoint := env("RMMWAY_MINIO_ENDPOINT", "http://localhost:9000")
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, endpoint+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		p := probe{Service: "minio"}
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode < 500 {
				p.OK = true
				p.Latency = time.Since(start).Round(time.Millisecond).String()
				p.Detail = fmt.Sprintf("http %d", resp.StatusCode)
			} else {
				p.Detail = fmt.Sprintf("unexpected status %d", resp.StatusCode)
			}
		} else {
			p.Detail = err.Error()
		}
		probes = append(probes, p)
	}
	{
		endpoint := env("RMMWAY_MEILI_ENDPOINT", "http://localhost:7700")
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, endpoint+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		p := probe{Service: "meilisearch"}
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				p.OK = true
				p.Latency = time.Since(start).Round(time.Millisecond).String()
			} else {
				p.Detail = fmt.Sprintf("status %d", resp.StatusCode)
			}
		} else {
			p.Detail = err.Error()
		}
		probes = append(probes, p)
	}
	return probes
}

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply SQL migrations and exit")
	flag.Parse()

	version := env("RMMWAY_VERSION", "0.1.0")
	httpAddr := env("RMMWAY_ADDR", ":8080")
	grpcAddr := env("RMMWAY_GRPC_ADDR", ":50051")
	jwtSecret := []byte(env("RMMWAY_JWT_SECRET", "rmmway-dev-secret-change-me"))
	// Operator (human/UI) credentials for the frontend login (W2-1).
	// Single admin account; override both in prod.
	adminUser := env("RMMWAY_ADMIN_USER", "admin")
	adminPassword := env("RMMWAY_ADMIN_PASSWORD", "admin")

	// ---- data layer (W1-6) ---------------------------------------------
	dsn := env("RMMWAY_PG_DSN", "postgres://rmmway:rmmway@localhost:5432/rmmway?sslmode=disable")
	pgPool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("pg pool: %v", err)
	}
	var devicesStore store.DeviceStore
	var metricsSink store.MetricsSink
	migrationsDir := env("RMMWAY_MIGRATIONS_DIR", "migrations")
	ctxBg, cancelBg := context.WithTimeout(context.Background(), 5*time.Second)
	if n, err := store.Migrate(ctxBg, pgPool, migrationsDir); err != nil {
		if *migrateOnly {
			log.Fatalf("migrations failed: %v", err)
		}
		log.Printf("WARN: migrations failed (%v) — running with in-memory stores (Postgres down?)", err)
		devicesStore = store.NewMemoryDeviceStore()
		metricsSink = store.NewMemoryMetricsSink(100000)
	} else {
		log.Printf("migrations: %d applied (dir=%s)", n, migrationsDir)
		devicesStore = store.NewPostgresDevices(pgPool)
		metricsSink = store.NewPostgresMetricsSink(pgPool)
	}
	cancelBg()
	if *migrateOnly {
		log.Println("migrate-only: done")
		return
	}

	// ---- device search index (W1-7) ------------------------------------
	// FullSync at boot heals any drift (Meili was down, data changed by
	// hand); IndexerHook keeps it current on enroll + (re)connect.
	// RMMWAY_MEILI_ENDPOINT empty (or "off") disables indexing entirely.
	var (
		indexer *store.IndexerHook
		mSearch *store.Meili
	)
	meiliEndpoint := env("RMMWAY_MEILI_ENDPOINT", "http://localhost:7700")
	if meiliEndpoint != "" && meiliEndpoint != "off" {
		m := store.NewMeili(meiliEndpoint, env("RMMWAY_MEILI_MASTER_KEY", "rmmway-dev-master-key"))
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := m.FullSync(fctx, devicesStore); err != nil {
			log.Printf("WARN: meilisearch FullSync failed (%v) — device search disabled until next boot", err)
		} else {
			indexer = store.NewIndexerHook(m, devicesStore)
			mSearch = m
			log.Printf("meilisearch: full sync ok (endpoint=%s)", meiliEndpoint)
		}
		fcancel()
	} else {
		log.Println("meilisearch: disabled (RMMWAY_MEILI_ENDPOINT empty/off)")
	}

	// ---- gRPC ingest (W1-5) -------------------------------------------
	svc := ingest.NewService(ingest.Config{JWTSecret: jwtSecret, Indexer: indexer}, metricsSink, devicesStore)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(svc.JWTInterceptor),
	)
	agentv1.RegisterAgentServiceServer(grpcServer, svc)

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("grpc listen %s: %v", grpcAddr, err)
	}
	go func() {
		log.Printf("rmmway-server %s: gRPC agent ingest on %s", version, grpcAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("grpc server: %v", err)
		}
	}()

	// ---- HTTP (health + operator API + admin JSON) ---------------------
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		probes := runProbes(r.Context())
		ok := true
		for _, p := range probes {
			if !p.OK {
				ok = false
			}
		}
		h := health{OK: ok, Version: version, Probes: probes}
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(h)
	})

	// W2-1: operator login + auth-gated device list + legacy /admin/*.
	// W2-2: + /api/search (Cmd-K) and /api/devices/{id}/commands (dispatch).
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devicesStore,
		Search:        mSearch,
		JWTSecret:     jwtSecret,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
		MintBootstrap: svc.MintBootstrapToken,
		Dispatch:      svc.Dispatcher().Dispatch,
	})
	apiSrv.Register(mux)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service": "rmmway-server",
			"version": version,
			"grpc":    grpcAddr,
		})
	})

	httpServer := &http.Server{Addr: httpAddr, Handler: mux}
	go func() {
		log.Printf("rmmway-server %s: HTTP on %s", version, httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	indexer.Stop()
	grpcServer.GracefulStop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
