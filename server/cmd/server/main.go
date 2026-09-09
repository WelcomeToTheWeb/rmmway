// Package main is the RMMWay backend server.
//
// W1-5: serves HTTP :8080 (health + admin JSON) and gRPC :50051 (agent
// ingest: Enroll + Stream). W0-1's /healthz still probes the stack.
package main

import (
	"context"
	"crypto/sha256"
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
	"sync"
	"syscall"
	"time"

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
	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/sessionrelay"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// hostIPv4s returns the machine's non-loopback unicast IPv4 addresses —
// the names a LAN agent will actually dial the server with when the server
// is bound to all interfaces.
func hostIPv4s() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

// publicURL returns the configured public operator URL (RMMWAY_PUBLIC_URL).
// If set, its host is used as the authoritative dial target for agents and
// seeded into the mTLS server cert's SANs so remote agents' x509 verification
// passes. Returns "" when unset.
func publicURL() string {
	return strings.TrimSpace(os.Getenv("RMMWAY_PUBLIC_URL"))
}

// publicURLHost extracts the host from a URL (stripping scheme and port), or
// the whole string if it's already a bare host. Returns "" for empty input.
func publicURLHost(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	// Strip scheme if present.
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	// host:port -> host.
	host, _, err := net.SplitHostPort(url)
	if err != nil {
		return url // no host:port separator — treat the whole thing as the host.
	}
	return host
}

// mtlsSANs derives the SAN names for the mTLS server cert from the listen
// addresses: whatever hosts the server is reachable on. Agents typically
// dial by the RMMWAY_SERVER hostname or by loopback, so we cover both the
// host of each configured listener and the common local names.
//
// A-1: in production agents dial the mTLS channel by the PUBLIC hostname
// (e.g. rmm.example.com), which a bare listen address (":50052") can't
// express — RMMWAY_GRPC_MTLS_SANs (comma-separated DNS names / IPs) adds
// those to the cert so hostname verification passes for remote agents.
//
// T3: a bare all-interfaces bind (":50052" / 0.0.0.0 / ::) also carries no
// hostname, but LAN agents dial the machine's IP — those are seeded into
// the cert too, so "server on the LAN, agent dials by IP" works without
// configuring the env var.
//
// RMMWAY_PUBLIC_URL (if set) is the authoritative public dial target: its
// host is ALWAYS included (no need to also set RMMWAY_GRPC_MTLS_SANs or
// RMMWAY_DOMAIN).
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
	// RMMWAY_PUBLIC_URL's host (the authoritative public dial target).
	if h := publicURLHost(publicURL()); h != "" {
		add(h)
	}
	for _, l := range listenAddrs {
		// Strip a leading scheme if an http(s) URL was passed.
		if i := strings.Index(l, "://"); i >= 0 {
			l = l[i+3:]
		}
		// host:port -> host. A bare port (":50051") or 0.0.0.0/"::" means
		// all interfaces: the address carries no hostname of its own.
		host, _, err := net.SplitHostPort(l)
		if err != nil {
			host = l // no host:port separator — the whole string is the name.
		}
		switch host {
		case "", "0.0.0.0", "::":
			// T3: seed the host's own IPv4 addresses — that is what LAN
			// agents dial.
			for _, ip := range hostIPv4s() {
				add(ip)
			}
		default:
			add(host)
		}
	}
	// A-1: explicitly configured public names (production domain, etc.).
	for _, s := range strings.Split(os.Getenv("RMMWAY_GRPC_MTLS_SANs"), ",") {
		add(s)
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

// retryMeiliSync (M7) retries the boot FullSync every 30s until Meilisearch
// answers, so a Meili that starts after the server recovers device search
// without a restart (pre-M7 a failed boot FullSync disabled search for the
// whole process lifetime).
func retryMeiliSync(m *store.Meili, devices store.DeviceStore) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for range tick.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := m.FullSync(ctx, devices)
		cancel()
		if err == nil {
			log.Println("meilisearch: full sync ok (background recovery)")
			return
		}
		log.Printf("meilisearch FullSync retry: %v", err)
	}
}

// healthCache (L7) caches /healthz probe results for a few seconds — each
// pass opens a FRESH connection to every backend, so an unthrottled health
// endpoint is a connection storm under load balancer probes.
var healthCache = struct {
	sync.Mutex
	at     time.Time
	probes []probe
}{}

const healthCacheTTL = 5 * time.Second

func runProbesCached(ctx context.Context) []probe {
	healthCache.Lock()
	defer healthCache.Unlock()
	if time.Since(healthCache.at) < healthCacheTTL {
		return healthCache.probes
	}
	p := runProbes(ctx)
	healthCache.at = time.Now()
	healthCache.probes = p
	return p
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

// deviceCounter adapts store.DeviceStore (List) to setup.DeviceCounter
// (the A-2 re-issue guard: the wizard may swap the org root only while no
// device has enrolled).
type deviceCounter struct{ s store.DeviceStore }

func (d deviceCounter) Count(ctx context.Context) (int, error) {
	list, err := d.s.List(ctx)
	if err != nil {
		return 0, err
	}
	return len(list), nil
}

// yesno renders a bool for log lines.
func yesno(b bool) string {
	if b {
		return "configured"
	}
	return "not configured"
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

	// C2: the built-in insecure defaults (dev JWT secret, admin/admin, dev
	// Meili master key) must not run silently in production — an operator
	// JWT is full /api/* access and a forged agent JWT (any device id is
	// guessable) is stream access. RMMWAY_ENV=dev (the default) warns;
	// anything else refuses to boot while a default is in use.
	rmmwayEnv := strings.ToLower(strings.TrimSpace(os.Getenv("RMMWAY_ENV")))
	if rmmwayEnv == "" {
		rmmwayEnv = "dev"
	}
	var insecureDefaults []string
	if os.Getenv("RMMWAY_JWT_SECRET") == "" {
		insecureDefaults = append(insecureDefaults,
			"RMMWAY_JWT_SECRET is unset — using the built-in dev secret (anyone who reads the source can forge agent AND operator JWTs)")
	}
	if os.Getenv("RMMWAY_ADMIN_USER") == "" && os.Getenv("RMMWAY_ADMIN_PASSWORD") == "" {
		insecureDefaults = append(insecureDefaults,
			"operator login is the built-in admin/admin (set RMMWAY_ADMIN_USER/RMMWAY_ADMIN_PASSWORD)")
	}
	if os.Getenv("RMMWAY_MEILI_MASTER_KEY") == "" {
		insecureDefaults = append(insecureDefaults,
			"Meilisearch master key is unset — using the built-in dev key (set RMMWAY_MEILI_MASTER_KEY)")
	}
	switch {
	case rmmwayEnv != "dev" && len(insecureDefaults) > 0:
		var b strings.Builder
		fmt.Fprintf(&b, "refusing to boot (RMMWAY_ENV=%s) with insecure built-in defaults:\n", rmmwayEnv)
		for _, s := range insecureDefaults {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
		b.WriteString("set the variables above, or RMMWAY_ENV=dev to allow the dev defaults\n")
		log.Fatal(b.String())
	case rmmwayEnv != "dev":
		log.Printf("RMMWAY_ENV=%s: no built-in dev defaults in use", rmmwayEnv)
	default:
		for _, s := range insecureDefaults {
			log.Printf("WARN: RMMWAY_ENV=dev — %s", s)
		}
	}

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
	// L6: first-boot migrations (CREATE EXTENSION timescaledb, hypertables)
	// can take longer than a few seconds on cold storage — 60s budget.
	ctxBg, cancelBg := context.WithTimeout(context.Background(), 60*time.Second)
	if n, err := store.Migrate(ctxBg, pgPool, migrationsDir); err != nil {
		if *migrateOnly {
			log.Fatalf("migrations failed: %v", err)
		}
		// H6: silently falling back to in-memory stores after a PG failure
		// turns a transient blip into a full data loss (every enrollment +
		// metric vanishes at the next restart) — refuse to boot unless the
		// operator opts into data-loss mode.
		if os.Getenv("RMMWAY_ALLOW_MEMORY_FALLBACK") != "1" {
			log.Fatalf("migrations failed (%v) — refusing to boot on in-memory stores (data-loss mode); set RMMWAY_ALLOW_MEMORY_FALLBACK=1 to allow", err)
		}
		log.Printf("WARN: migrations failed (%v) — running with in-memory stores (RMMWAY_ALLOW_MEMORY_FALLBACK=1; data will NOT survive a restart)", err)
		devicesStore = store.NewMemoryDeviceStore()
		metricsSink = store.NewMemoryMetricsSink(100000)
	} else {
		log.Printf("migrations: %d applied (dir=%s)", n, migrationsDir)
		hasPG = true
		devicesStore = store.NewPostgresDevices(pgPool)
		metricsSink = store.NewPostgresMetricsSink(pgPool)
	}
	cancelBg()

	// W6-1: the log store (indexed agent log events). Postgres-backed when
	// the hypertable is available (the per-device Timescale copy the RMM
	// surfaces); in-memory otherwise, so the ingest + API behave identically
	// in degraded mode — the agent still ships to Loki independently.
	var logSink store.LogSink
	var logReader store.LogEventReader
	if hasPG {
		ls := store.NewPostgresLogStore(pgPool)
		logSink = ls
		logReader = ls
		log.Println("log events: indexed in Timescale (log_events hypertable)")
	} else {
		mem := store.NewMemoryLogStore(100000)
		logSink = mem
		logReader = mem
	}
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

	if pub := publicURL(); pub != "" {
		log.Printf("public URL: %s (mTLS cert includes %s; the Add Device UI will prefill this)",
			pub, publicURLHost(pub))
	}

	// W3-3: per-action capability tokens. Every dispatched command carries a
	// short-lived token signed by the org root, bound to (device, capability,
	// command id); the agent verifies it against its pinned root before
	// acting and refuses (REFUSED) anything outside the minted scope.
	capsIssuer := caps.NewIssuer(caMgr.Root(), capTTL())
	log.Printf("capability tokens: enabled (TTL %s; admin caps %v)", capsIssuer.TTL(), adminCaps())

	// ---- first-boot setup wizard (A-2) ---------------------------------
	// A FRESH database (no server_setup row) means the server is not yet
	// initialized: the UI redirects to the setup wizard, and
	// POST /api/setup/complete mints the root admin, re-issues the org CA
	// under the operator's org name (safe pre-enroll: 0 devices), and
	// persists the SMTP outbox config — in one shot. Every later boot reads
	// done=true from the database and bypasses it. Postgres-backed only:
	// in-memory mode has no durable state, so it runs on the env admin.
	var setupSvc *setup.Service
	if hasPG {
		setupSvc = setup.New(setup.Config{
			Store:   setup.NewPostgresStore(pgPool),
			OrgCA:   caMgr,
			Devices: deviceCounter{s: devicesStore},
			OnReissued: func() {
				// The capability tokens are signed by the org root key —
				// after the wizard re-issues the root, mint with the NEW key.
				capsIssuer.ReplaceRoot(caMgr.Root())
			},
		})
		if st, err := setupSvc.Status(context.Background()); err != nil {
			log.Printf("WARN: setup status: %v", err)
		} else if !st.Available {
			log.Println("setup: store unavailable (no database) — env admin login mode, wizard inactive")
		} else if st.DevicesEnrolled && st.AdminUser == "" {
			log.Println("setup: grandfathered (devices enrolled before the wizard existed) — wizard skipped, env admin login active")
		} else if st.Setup {
			log.Printf("setup: complete (org %q; admin %s; smtp %s)",
				st.OrgName, st.AdminUser, yesno(st.SMTPConfigured))
		} else {
			log.Println("setup: NOT complete — first-boot wizard active (the UI redirects to it; POST /api/setup/complete finalizes)")
		}
	}

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
		// M7: construct the indexer even when the boot FullSync fails —
		// Meilisearch coming up a few seconds AFTER the server used to
		// disable device search for the whole process lifetime (no retry,
		// /admin/search 503, no index updates). A background retry recovers
		// without a restart; each FullSync re-heals drift as well.
		indexer = store.NewIndexerHook(m, devicesStore)
		mSearch = m
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		syncErr := m.FullSync(fctx, devicesStore)
		fcancel()
		if syncErr != nil {
			log.Printf("WARN: meilisearch FullSync failed (%v) — retrying in the background", syncErr)
			go retryMeiliSync(m, devicesStore)
		} else {
			log.Printf("meilisearch: full sync ok (endpoint=%s)", meiliEndpoint)
		}
	} else {
		log.Println("meilisearch: disabled (RMMWAY_MEILI_ENDPOINT empty/off)")
	}

	// ---- event bus (W5-2) + event fan-out (W6-2) ------------------------
	// Declared BEFORE the offline sweeper below (which publishes device
	// offline events onto it). The bus is wired to NATS a few blocks later;
	// the sweeper goroutine is async, so by the time it first ticks the bus
	// var is assigned (a nil bus — NATS down — makes publishEvent a no-op).
	var flowBus flow.Bus
	// A single publish helper the alert / device / notify emitters share. It
	// references the (possibly nil) flowBus var, so emitters wired before or
	// after the bus is up all behave: a nil bus (NATS down) is a no-op.
	publishEvent := func(subject, deviceID, message string, data map[string]any) {
		if flowBus == nil {
			return
		}
		_ = flowBus.Publish(context.Background(), subject, &flow.Event{
			Type: subject, DeviceID: deviceID, Message: message, Data: data, At: time.Now().UTC(),
		})
	}

	// ---- offline sweeper (M4) ---------------------------------------------
	// A device that stops heartbeating must stop showing as online: flip
	// online->false once last_seen goes stale (3× its heartbeat interval,
	// minimum 90s) and re-sync its search document.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		for range tick.C {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flipped, err := devicesStore.SweepOffline(sctx)
			cancel()
			if err != nil {
				log.Printf("offline sweep: %v", err)
				continue
			}
			for _, id := range flipped {
				log.Printf("device %s marked offline (stale last_seen)", id)
				indexer.Touch(id)
				// W6-2: a device going OFFLINE is an inventory event on the
				// bus, so SSE subscribers + webhooks see the status flip
				// immediately (the DoD for the reactive UI: a device that
				// stops heartbeating updates every open operator session).
				publishEvent(flow.SubjectDevice, id, "offline device", map[string]any{
					"action":    "offline",
					"device_id": id,
					"reason":    "stale last_seen",
				})
			}
		}
	}()

	// ---- event bus wiring (W5-2) ------------------------------------------
	flowBus = wireFlowBus(hasPG)

	// Alerts (W2-4) surface lifecycle events (fired/updated/resolved) on the
	// bus so the webhook / SSE framework journals + delivers them.
	if hasPG && alertStore != nil {
		alertStore.SetEventSink(func(action string, payload map[string]any) {
			devID, _ := payload["device_id"].(string)
			publishEvent(flow.SubjectAlert, devID, action+" alert "+fmt.Sprint(payload["name"]), payload)
		})
	}

	// gap #1a: session relay registry (shared between ingest and httpapi).
	sessionRelay := sessionrelay.New()

	// ---- gRPC ingest (W1-5) + mTLS agent channel (W3-1) ---------------
	svc, grpcServer, mtlsServer := wireIngest(version, jwtSecret, grpcAddr, grpcMTLSAddr, httpAddr,
		indexer, caMgr, capsIssuer, logSink, flowBus, publishEvent, metricsSink, devicesStore, sessionRelay)

	// gap #7: helpdesk ticketing (see wire_tickets.go). The ticket store
	// is created early so heal escalations can create real tickets.
	ticketStore, _ := wireTickets(hasPG, pgPool)
	defer func() { if ticketStore != nil { log.Println("tickets: shutdown") } }()

	// gap #6: notification channels + policies (see wire_notify.go).
	notifyStore, notifySender := wireNotify(hasPG, pgPool)

	// ---- self-healing playbook engine (W5-1) ---------------------------
	// When the ticket store is available, heal escalations create real
	// tickets via the ticket notifier (gap #7).
	var ticketNotifier heal.Notifier = nil
	if ticketStore != nil {
		ticketNotifier = ticketHealNotifier{
			log:  log.New(os.Stderr, "selfheal: ", 0),
			store: ticketStore,
			pub: publishEvent,
		}
	}
	healEngine := wireHealEngine(hasPG, pgPool, svc, publishEvent, ticketNotifier)

	// ---- event-driven automation chains (W5-2) -------------------------
	flowEngine := wireFlowEngine(hasPG, flowBus, pgPool, svc, publishEvent)

	// ---- webhook + event-stream framework (W6-2) ------------------------
	webhookSvc, webhookBus := wireWebhook(hasPG, flowBus, pgPool)

	// ---- HTTP (health + operator API + admin JSON) ---------------------
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// L7: cached probe results (a full pass opens fresh connections to
		// every backend on each call).
		probes := runProbesCached(r.Context())
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

	// W4-2: signed agent release distribution (see wire_releases.go).
	relSrv := wireReleases()

	// W4-3: per-client full export (see wire_export.go).
	exportSvc := wireExport(hasPG, pgPool, devicesStore, version)

	// gap #2: MSP client/tenant registry (see wire_clients.go).
	clientsStore := wireClients(hasPG, pgPool)

	// gap #3: operator accounts + RBAC (see wire_users.go).
	usersStore := wireUsers(hasPG, pgPool)

	// Per-device metrics viewer: the operator UI's device-detail charts read
	// the metric series the agents report (Timescale hypertable).
	metricsView := store.NewPostgresMetricsView(pgPool)
	// W2-1: operator login + auth-gated device list + legacy /admin/*.
	// W2-2: + /api/search (Cmd-K) and /api/devices/{id}/commands (dispatch).
	apiSrv := httpapi.New(httpapi.Config{
		Devices:       devicesStore,
		Search:        mSearch,
		JWTSecret:     jwtSecret,
		AdminUser:     adminUser,
		AdminPassword: adminPassword,
		Sessions:      sessionRelay,
		SendSessionControl: func(deviceID string, sc *agentv1.SessionControl) bool {
			return svc.SendSessionControl(deviceID, sc)
		},
		MintBootstrap: svc.MintBootstrapToken,
		Enroll:        svc.Enroll,
		Dispatch:      svc.Dispatcher().Dispatch,
		CommandState: func(deviceID string) ([]*agentv1.Command, []*agentv1.CommandResult) {
			return svc.Dispatcher().PendingFor(deviceID), svc.Dispatcher().ResultsFor(deviceID)
		},
		LogEvents: func(deviceID string, limit int, level string) ([]store.LogEvent, error) {
			return logReader.Recent(context.Background(), deviceID, limit, level)
		},
		MetricNames: func(deviceID string, since time.Time) ([]store.MetricSeries, error) {
			return metricsView.Names(context.Background(), deviceID, since)
		},
		MetricSeries: func(deviceID, name, source string, since time.Time, bucket time.Duration) ([]store.MetricPoint, error) {
			return metricsView.Series(context.Background(), deviceID, name, source, since, bucket)
		},
		AdminCaps: adminCaps(),
		Baseline:  baselineJob,
		Alerts:    alertStore,
		Heal:      healEngine,
		Releases:  relSrv,
		Flows:     flowEngine,
		Export:    exportSvc,
		Webhooks:  webhookSvc,
		Setup:     setupSvc,
		Clients:   clientsStore,
		Users:     usersStore,
		Tickets:   ticketStore,
		NotifyStore:   notifyStore,
		NotifySender:  notifySender,
		PublicURL: publicURL(),
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
	if webhookBus != nil {
		webhookBus.Close()
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
