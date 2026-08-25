// Package main is the RMMWay backend server.
//
// W1-5: serves HTTP :8080 (health + admin JSON) and gRPC :50051 (agent
// ingest: Enroll + Stream). W0-1's /healthz still probes the stack.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/ca"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/httpapi"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/releases"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// mtlsSANs derives the SAN names for the mTLS server cert from the listen
// addresses: whatever hosts the server is reachable on. Agents typically
// dial by the RMMWAY_SERVER hostname or by loopback, so we cover both the
// host of each configured listener and the common local names.
func mtlsSANs(listenAddrs ...string) []string {
	seen := map[string]bool{}
	sans := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		sans = append(sans, s)
	}
	// Common local dial names, always.
	add("localhost")
	add("127.0.0.1")
	for _, l := range listenAddrs {
		// Strip a leading scheme if an http(s) URL was passed.
		if i := strings.Index(l, "://"); i >= 0 {
			l = l[i+3:]
		}
		// host:port -> host (a bare port / ":50051" means all interfaces).
		host := l
		if i := strings.LastIndex(l, ":"); i >= 0 {
			if net.ParseIP(l[i+1:]) != nil {
				host = l[:i]
			}
		}
		if host == "" {
			continue
		}
		add(host)
		// ":50051" (all interfaces) is also dialable by the machine's own
		// hostname — the caller's env is unknown here, so the "localhost"
		// defaults above are the reliable fallback.
	}
	return sans
}

// env returns the first non-empty of the env var / default.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// baselineInterval is the W2-3 scoring cadence (RMMWAY_BASELINE_INTERVAL,
// default 5m). One pass re-scores every series' latest hourly mean against
// its rolling baseline — cheap on the hourly-bucketed hypertable.
func baselineInterval() time.Duration {
	if v := os.Getenv("RMMWAY_BASELINE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("WARN: bad RMMWAY_BASELINE_INTERVAL %q — using 5m", v)
	}
	return 5 * time.Minute
}

// sha256Short is a 12-hex-char fingerprint of a PEM blob (log-only: which
// org root is active without dumping the certificate).
func sha256Short(pemBytes []byte) string {
	sum := sha256.Sum256(pemBytes)
	return hex.EncodeToString(sum[:])[:12]
}

// leafTTL is the device leaf (and server cert) lifetime (W3-2): the default
// is the ~1h short-lived window the org CA package defines; RMMWAY_LEAF_TTL
// overrides it for tests / long dev sessions (e.g. "24h").
func leafTTL() time.Duration {
	v := os.Getenv("RMMWAY_LEAF_TTL")
	if v == "" {
		return 0 // 0 -> the ca package default (~1h)
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("WARN: bad RMMWAY_LEAF_TTL %q — using the default ~1h", v)
		return 0
	}
	return d
}

// alertAutoResolve is the number of consecutive clean passes before an
// open alert auto-resolves (RMMWAY_ALERT_AUTO_RESOLVE, default 1). With the
// default 5-min engine cadence, 1 = an alert closes ~5 min after the metric
// returns to baseline. 0 disables auto-resolve (manual only).
func alertAutoResolve() int {
	v := os.Getenv("RMMWAY_ALERT_AUTO_RESOLVE")
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("WARN: bad RMMWAY_ALERT_AUTO_RESOLVE %q — using 1", v)
		return 1
	}
	return n
}

// capTTL is the lifetime of minted capability tokens (W3-3): the default is
// a 10-minute time-box for a command to reach + be accepted by the agent;
// RMMWAY_CAP_TTL overrides it (tests / long-running dispatch queues).
func capTTL() time.Duration {
	v := os.Getenv("RMMWAY_CAP_TTL")
	if v == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("WARN: bad RMMWAY_CAP_TTL %q — using 10m", v)
		return 10 * time.Minute
	}
	return d
}

// healInterval is the W5-1 self-healing pass cadence (RMMWAY_HEAL_INTERVAL,
// default 5m). One pass detects failing conditions, drives in-flight runs
// forward (confirm re-measures), and escalates stuck ones. "off" disables.
func healInterval() (time.Duration, bool) {
	v := os.Getenv("RMMWAY_HEAL_INTERVAL")
	if v == "off" {
		return 0, false
	}
	if v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d, true
		}
		log.Printf("WARN: bad RMMWAY_HEAL_INTERVAL %q — using 5m", v)
	}
	return 5 * time.Minute, true
}

// flowInterval parses a duration env for a flow-engine ticker
// (RMMWAY_FLOW_SWEEP / RMMWAY_FLOW_SAMPLE); "off" returns -1 (disabled),
// a bad value falls back to def with a warning.
func flowInterval(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if v == "off" {
		return -1
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("WARN: bad %s %q — using %s", key, v, def)
		return def
	}
	return d
}

// adminCaps is the capability set minted into operator session tokens
// (W3-3). RMMWAY_ADMIN_CAPS is a comma-separated list (e.g. "rmmway.run_script");
// empty = the full Phase 1 set (all capabilities).
func adminCaps() []string {
	v := os.Getenv("RMMWAY_ADMIN_CAPS")
	if v == "" {
		return caps.AllCapabilities
	}
	out := []string{}
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return caps.AllCapabilities
	}
	return out
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
	// W3-1: the mTLS agent channel. A second gRPC listener that REQUIRES a
	// client leaf cert issued by the org root. "off" disables it (dev / the
	// pre-W3-1 bootstrap path keeps working on the plain listener).
	grpcMTLSAddr := env("RMMWAY_GRPC_MTLS_ADDR", ":50052")
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
	hasPG := false
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
		hasPG = true
		devicesStore = store.NewPostgresDevices(pgPool)
		metricsSink = store.NewPostgresMetricsSink(pgPool)
	}
	cancelBg()
	if *migrateOnly {
		log.Println("migrate-only: done")
		return
	}

	// ---- org PKI (W3-1 mTLS agent channel) ---------------------------
	// One org root CA for the whole estate: generated + persisted at first
	// boot, reused across restarts so enrolled devices' leaf certs stay
	// valid. Enroll hands each device its leaf (signed by the root); the
	// mTLS gRPC listener serves a root-signed server cert and requires the
	// leaf. W3-2: leaves are short-lived (~1h, RMMWAY_LEAF_TTL) and the
	// agent rotates them via RefreshLeaf while the old leaf is still valid;
	// the server cert the listener serves rotates in place via
	// GetCertificate (no listener restart). With Postgres the root lives in
	// org_ca; in-memory mode keeps the whole flow self-contained (root
	// lives for the process lifetime).
	var caMgr *ca.Manager
	if hasPG {
		caMgr, err = ca.NewManager(ca.NewPostgresOrgStore(pgPool), leafTTL())
	} else {
		caMgr, err = ca.NewManager(ca.NewMemoryOrgStore(), leafTTL())
	}
	if err != nil {
		log.Fatalf("org CA: %v", err)
	}
	log.Printf("org CA ready (cert sha256:%s; leaf TTL %s)", sha256Short(caMgr.RootCertPEM()), caMgr.LeafTTL())

	// W3-3: per-action capability tokens. Every dispatched command carries a
	// short-lived token signed by the org root, bound to (device, capability,
	// command id); the agent verifies it against its pinned root before
	// acting and refuses (REFUSED) anything outside the minted scope.
	capsIssuer := caps.NewIssuer(caMgr.Root(), capTTL())
	log.Printf("capability tokens: enabled (TTL %s; admin caps %v)", capsIssuer.TTL(), adminCaps())

	// ---- dynamic baselining (W2-3) + alerts (W2-4) -------------------
	// Deterministic background job: scores every series' latest hourly
	// mean against its (dow, hour) seasonal baseline + a same-day trend
	// baseline, persisting findings to baseline_anomalies and folding them
	// into the deduped alert inbox (one open alert per anomalous series).
	// Runs against the metrics hypertable whenever Postgres is up; with
	// in-memory stores (PG down) there is no source, so it's disabled.
	var baselineJob *store.Baseline
	var alertStore *store.AlertStore
	if hasPG {
		alertStore = store.NewAlertStore(pgPool, alertAutoResolve())
		baselineJob = store.NewBaseline(
			store.NewPostgresBaselineSource(pgPool),
			store.NewPostgresAnomalySink(pgPool),
			alertStore,
		)
		baseErrCh := make(chan error, 1)
		go func() {
			baselineJob.Start(context.Background(), baselineInterval(), baseErrCh)
		}()
		go func() {
			for err := range baseErrCh {
				log.Printf("baseline: pass failed: %v", err)
			}
		}()
		log.Println("baseline: dynamic baselining engine started")
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

	// ---- event bus (W5-2) -------------------------------------------------
	// The NATS/JetStream stream that carries every flow hop. Flows are
	// Postgres-backed (the replay-safe run state), so the engine needs
	// hasPG; when NATS is down the server degrades to in-memory mode for
	// the rest of the stack, so the flow engine is disabled too (its whole
	// point is that the chain runs OVER the bus).
	var flowBus flow.Bus
	if hasPG {
		fb, err := flow.NewNatsBus(context.Background(), env("RMMWAY_NATS_URL", "nats://localhost:4222"), "RMMWAY_EVENTS", "flow-engine")
		if err != nil {
			log.Printf("WARN: nats event bus unavailable (%v) — flow engine disabled", err)
		} else {
			flowBus = fb
			log.Println("nats event bus ready (stream RMMWAY_EVENTS)")
		}
	}

	// ---- gRPC ingest (W1-5) + mTLS agent channel (W3-1) ---------------
	svc := ingest.NewService(ingest.Config{JWTSecret: jwtSecret, Indexer: indexer, OrgCA: caMgr, Caps: capsIssuer,
		OnCommandResult: func(res *agentv1.CommandResult) {
			// W5-2: a FINAL agent command answer becomes a bus event so a
			// waiting flow script node advances (event-driven chain hop).
			if flowBus == nil {
				return
			}
			_ = flowBus.Publish(context.Background(), flow.SubjectCommand, &flow.Event{
				Type:      flow.SubjectCommand,
				CommandID: res.GetCommandId(),
				Status:    res.GetStatus().String(),
				Message:   res.GetError(),
				At:        time.Now().UTC(),
			})
		},
	}, metricsSink, devicesStore)
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

	// W3-1: second gRPC listener, mTLS. Same AgentService, but the TLS
	// layer requires a client leaf signed by the org root before any RPC
	// is processed (a random cert is rejected at the handshake), and the
	// server presents a root-signed cert so the agent verifies us too.
	// RMMWAY_GRPC_MTLS_ADDR=off disables it (plain-listener deployments).
	var mtlsServer *grpc.Server
	if grpcMTLSAddr != "off" && grpcMTLSAddr != "" {
		mtlsCfg, err := caMgr.TLSConfig(mtlsSANs(grpcMTLSAddr, grpcAddr, httpAddr))
		if err != nil {
			log.Fatalf("grpc mTLS: %v", err)
		}
		mtlsServer = grpc.NewServer(
			grpc.Creds(credentials.NewTLS(mtlsCfg)),
			grpc.UnaryInterceptor(svc.JWTInterceptor),
		)
		agentv1.RegisterAgentServiceServer(mtlsServer, svc)
		mtlsLis, err := net.Listen("tcp", grpcMTLSAddr)
		if err != nil {
			log.Fatalf("grpc mTLS listen %s: %v", grpcMTLSAddr, err)
		}
		go func() {
			log.Printf("rmmway-server %s: gRPC mTLS agent channel on %s (client cert required)", version, grpcMTLSAddr)
			if err := mtlsServer.Serve(mtlsLis); err != nil {
				log.Printf("grpc mTLS server: %v", err)
			}
		}()
	}

	// ---- self-healing playbook engine (W5-1) ---------------------------
	// Detect -> verify-safe -> remediate -> confirm (re-measure) -> escalate.
	// Postgres-backed (the run state machine + replay-safety live in the DB),
	// so it needs hasPG; remediations go through the same capability-gated
	// command dispatch as operator-run actions (W3-3 token on every script).
	var healEngine *heal.Engine
	if hasPG {
		if d, on := healInterval(); on {
			hst := heal.NewStore(pgPool)
			remediate := func(ctx context.Context, deviceID, lang, script string) (string, error) {
				return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
					RunScript: &agentv1.RunScript{
						Lang:      lang,
						ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
					},
				})
			}
			healEngine = heal.New(hst, remediate, svc.Dispatcher().Result, nil)
			healErrCh := make(chan error, 1)
			go healEngine.Run(context.Background(), d, healErrCh)
			go func() {
				for err := range healErrCh {
					log.Printf("selfheal: %v", err)
				}
			}()
			log.Printf("selfheal: playbook engine started (interval %s; playbooks seeded by 0005_selfheal.sql)", d)
		} else {
			log.Println("selfheal: disabled (RMMWAY_HEAL_INTERVAL=off)")
		}
	}

	// ---- event-driven automation chains (W5-2) -------------------------
	// Automations are DAGs of trigger -> script/check/notify nodes executed
	// OVER the NATS event bus: every hop of every run is a bus event, the
	// Postgres tables hold only the replay-safe run state. Real triggers
	// come from the sampler (polls the metrics hypertable); synthetic ones
	// from POST /api/flows/{id}/trigger.
	var flowEngine *flow.Engine
	if hasPG && flowBus != nil {
		remediate := func(ctx context.Context, deviceID, lang, script string) (string, error) {
			return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
				RunScript: &agentv1.RunScript{
					Lang:      lang,
					ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
				},
			})
		}
		flowEngine = flow.New(flow.NewStore(pgPool), flowBus, remediate, svc.Dispatcher().Result, nil,
			flowInterval("RMMWAY_FLOW_SWEEP", 5*time.Second), flowInterval("RMMWAY_FLOW_SAMPLE", 60*time.Second))
		flowEngine = flowEngine.WithLogger(log.New(os.Stderr, "flow: ", 0))
		if err := flowEngine.Start(context.Background()); err != nil {
			log.Printf("WARN: flow engine start failed (%v) — flows disabled", err)
			flowEngine = nil
		} else {
			log.Println("flow engine: event-driven chains started (sampler + sweep on the NATS bus)")
		}
	} else if hasPG {
		log.Println("flow engine: disabled (nats event bus unavailable)")
	}

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
	// W4-2: signed agent release distribution. When RMMWAY_RELEASES_DIR is set
	// to a directory holding release.json + signed binaries, the server serves
	// them at /agent/releases/* for the agents' auto-update. Unset = the routes
	// 404 and agents treat themselves as up-to-date.
	var relSrv *releases.Server
	if releasesDir := env("RMMWAY_RELEASES_DIR", ""); releasesDir != "" {
		relSrv, err = releases.New(releasesDir)
		if err != nil {
			log.Fatalf("releases: %v", err)
		}
		log.Printf("agent releases: serving signed releases from %s", relSrv.Dir())
	}

	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devicesStore,
		Search:        mSearch,
		JWTSecret:     jwtSecret,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
		MintBootstrap: svc.MintBootstrapToken,
		Dispatch:      svc.Dispatcher().Dispatch,
		CommandState: func(deviceID string) ([]*agentv1.Command, []*agentv1.CommandResult) {
			return svc.Dispatcher().PendingFor(deviceID), svc.Dispatcher().ResultsFor(deviceID)
		},
		AdminCaps: adminCaps(),
		Baseline:  baselineJob,
		Alerts:    alertStore,
		Heal:      healEngine,
		Releases:  relSrv,
		Flows:     flowEngine,
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
	if flowBus != nil {
		flowBus.Close()
	}
	indexer.Stop()
	grpcServer.GracefulStop()
	if mtlsServer != nil {
		mtlsServer.GracefulStop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
