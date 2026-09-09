// Package httpapi is the operator-facing HTTP API (W2-1): operator login,
// the auth-gated device list the frontend renders, and the pre-existing
// /admin/* endpoints the bootstrap installer and device search rely on.
//
// Auth model: a human operator logs in with a username + password
// (single admin account, configured via env) and receives a short-lived
// operator JWT (subject "operator", issuer "rmmway"). BOTH /api/* and
// /admin/* routes are gated on that token (C1: /admin/* was open for
// machine callers, but the one caller that mattered — minting enroll
// tokens — is an operator action the UI does via /api/bootstrap; leaving
// the join gate + inventory unauthenticated let anyone on the network
// enroll arbitrary devices). The only open routes are the machine
// endpoints the AGENT itself calls with no operator session:
//
//	/agent/enroll      (the bootstrap enroll, guarded by the one-time token)
//	/agent/releases/*  (signed release distribution; signatures, not auth)
//	/api/login, /api/setup/status (+ the setup POST routes pre-initialization)
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/export"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/notify"
	"github.com/welcometotheweb/rmmway/server/internal/users"
	"github.com/welcometotheweb/rmmway/server/internal/releases"
	"github.com/welcometotheweb/rmmway/server/internal/sessionrelay"
	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/webhook"
)

// pbkdf2 params for hashing the operator password (per boot).
const (
	pbkdf2Iterations = 100_000
	pbkdf2KeyLen     = 32
)

// Server owns the operator API state: credential material, the JWT secret,
// and the data-layer handles it serves from.
type Server struct {
	devices  store.DeviceStore
	search   *store.Meili
	baseline *store.Baseline
	alerts   *store.AlertStore
	heal     *heal.Engine
	releases *releases.Server
	flows    *flow.Engine

	jwtSecret     []byte
	tokenLifetime time.Duration
	// adminCaps (W3-3) is the capability set minted into every operator
	// session token; dispatching an action outside it is refused (403).
	adminCaps []string

	adminUser string
	adminSalt []byte
	adminHash []byte

	// loginLimiter (L8) caps failed /api/login attempts per client IP.
	loginLimiter *loginLimiter

	mintBootstrap func() (token, deviceID string)
	// enroll ("Add a device" over the operator's HTTPS origin) performs the
	// bootstrap enroll — the same one-time-token -> identity logic as the
	// gRPC Enroll RPC. Nil disables /agent/enroll (503).
	enroll func(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error)
	// dispatch mints + pushes a command to a device's live stream (W2-2).
	// Nil disables /api/devices/{id}/commands.
	dispatch func(deviceID string, action any) (commandID string, err error)
	// commandState (W3-3) serves the device's pending commands + recorded
	// results. Nil disables GET /{api|admin}/devices/{id}/commands.
	commandState func(deviceID string) (pending []*agentv1.Command, results []*agentv1.CommandResult)
	// export (W4-3) builds the per-client full export bundle.
	export *export.Service
	// logEvents (W6-1) serves the device's recent indexed log events.
	// Nil disables GET /{api|admin}/devices/{id}/events.
	logEvents func(deviceID string, limit int, level string) ([]store.LogEvent, error)
	// metricNames (per-device metrics viewer) serves GET
	// {/api|admin}/devices/{id}/metrics: the (name, source) series the
	// device has reported since `since`. Nil disables (in-memory mode).
	metricNames func(deviceID string, since time.Time) ([]store.MetricSeries, error)
	// metricSeries serves GET {/api|admin}/devices/{id}/metrics/series:
	// the bucketed samples of one series over a range. Nil disables.
	metricSeries func(deviceID, name, source string, since time.Time, bucket time.Duration) ([]store.MetricPoint, error)
	// webhooks (W6-2) is the webhook + event-stream framework; nil disables
	// /{api|admin}/webhooks* and /{api|admin}/events/stream (in-memory mode).
	webhooks *webhook.Service
	// setup (A-2) is the first-boot wizard backend; nil = in-memory mode
	// (the wizard is unavailable, the env admin login is the only one).
	setup *setup.Service
	// clients (gap #2) is the MSP client/tenant registry; nil disables
	// /{api|admin}/clients* (in-memory-mode deployments).
	clients store.ClientStore
	// users (gap #3) is the operator account store (0011_users); nil =
	// in-memory mode: /api/login delegates to the legacy env/admin_users
	// path and scoping is off (every session is a grandfathered admin).
	users store.UserStore
	// tickets (gap #7) is the helpdesk ticket store (0012_tickets); nil =
	// in-memory mode: /{api|admin}/tickets* returns 503.
	tickets store.TicketStore
	// notifyStore (gap #6) is the notification channel/policy store;
	// nil = in-memory mode: /{api|admin}/notify* returns 503.
	notifyStore store.NotifyStore
	// notifySender (gap #6) creates channel instances for sending;
	// nil = channels cannot send (test-fire unavailable).
	notifySender *notify.Sender
	// rbac (gap #3) resolves session JWTs + rmm_ API tokens into scoped
	// Sessions for the route gates (rbacGate/rbacScopeGate/rbacRoleGate
	// in domain_users.go). Always built; the Users store may be nil.
	rbac *users.RBAC
	// publicURL (if set) is the configured public operator URL
	// (RMMWAY_PUBLIC_URL). The Add Device UI reads this via
	// GET /api/public-url to prefill the server URL field.
	publicURL string
	// gap #1a: remote session support.
	sessions           *sessionrelay.Registry
	sendSessionControl func(deviceID string, sc *agentv1.SessionControl) bool
}

// Config wires a Server. AdminPassword is hashed with a fresh per-boot salt
// in New; never stored in plaintext.
type Config struct {
	Devices       store.DeviceStore
	Search        *store.Meili
	JWTSecret     []byte
	TokenLifetime time.Duration
	AdminUser     string
	AdminPassword string
	// MintBootstrap mints a one-time enroll code; nil disables /admin/bootstrap.
	MintBootstrap func() (token, deviceID string)
	// Enroll performs the bootstrap enroll (POST /agent/enroll), letting a
	// remote agent join over the operator's HTTPS origin (only 443 + the mTLS
	// gRPC port need to be open — the plain gRPC bootstrap port stays
	// internal). Nil disables the route (503).
	Enroll func(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error)
	// Dispatch mints + pushes a command to a device's live stream (W2-2);
	// nil disables /api/devices/{id}/commands.
	Dispatch func(deviceID string, action any) (commandID string, err error)
	// CommandState serves GET {/api|/admin}/devices/{id}/commands (W3-3):
	// the device's pending commands + recorded results (nil disables).
	CommandState func(deviceID string) (pending []*agentv1.Command, results []*agentv1.CommandResult)
	// LogEvents (W6-1) serves GET {/api|/admin}/devices/{id}/events: the
	// device's recent indexed agent-log events (newest first). Nil disables.
	LogEvents func(deviceID string, limit int, level string) ([]store.LogEvent, error)
	// MetricNames serves GET {/api|admin}/devices/{id}/metrics: the
	// (name, source) series the device has reported (the viewer's metric
	// picker). Nil disables (in-memory mode has no metric history).
	MetricNames func(deviceID string, since time.Time) ([]store.MetricSeries, error)
	// MetricSeries serves GET {/api|admin}/devices/{id}/metrics/series:
	// the bucketed samples of one series over a range. Nil disables.
	MetricSeries func(deviceID, name, source string, since time.Time, bucket time.Duration) ([]store.MetricPoint, error)
	// AdminCaps is the capability set granted to operator sessions
	// (W3-3); empty = the full Phase 1 set.
	AdminCaps []string
	// Baseline is the W2-3 dynamic baselining job; nil disables
	// /api/baseline/* and /admin/baseline/*.
	Baseline *store.Baseline
	// Alerts is the W2-4 deduped alert inbox; nil disables /api/alerts*
	// and /admin/alerts*.
	Alerts *store.AlertStore
	// Heal is the W5-1 self-healing playbook engine; nil disables
	// /api/heal* and /admin/heal* (in-memory-mode deployments).
	Heal *heal.Engine
	// Releases (W4-2) serves signed agent release artifacts for the agent's
	// auto-update; nil disables /agent/releases/*.
	Releases *releases.Server
	// Flows is the W5-2 event-driven automation engine; nil disables
	// /api/flows* and /admin/flows* (in-memory-mode deployments).
	Flows *flow.Engine
	// Export (W4-3) builds the per-client full export bundle; nil disables
	// /{api|admin}/devices/{id}/export (in-memory-mode deployments).
	Export *export.Service
	// Webhooks (W6-2) is the webhook + event-stream framework; nil disables
	// /{api|admin}/webhooks* and /{api|admin}/events/stream (in-memory mode).
	Webhooks *webhook.Service
	// Setup (A-2) is the first-boot setup wizard backend; nil disables
	// /api/setup* (in-memory mode: the UI skips the wizard, env admin only).
	Setup *setup.Service
	// Clients (gap #2) is the MSP client/tenant registry; nil disables
	// /{api|admin}/clients* (in-memory-mode deployments). The ?client=
	// scoping on the device list / alert inbox keeps working — it scopes
	// the device and alert stores, not this registry.
	Clients store.ClientStore
	// Users (gap #3) is the operator account store (0011_users). Nil =
	// in-memory mode: /api/login keeps the legacy behavior and every
	// session is a grandfathered admin (no scoping).
	Users store.UserStore
	// Tickets (gap #7) is the helpdesk ticket store (0012_tickets). Nil =
	// in-memory mode: /{api|admin}/tickets* returns 503.
	Tickets store.TicketStore
	// NotifyStore (gap #6) is the notification channel/policy store.
	// Nil = in-memory mode: /{api|admin}/notify* returns 503.
	NotifyStore store.NotifyStore
	// NotifySender (gap #6) creates channel instances for sending.
	// Nil = channels cannot send (test-fire unavailable).
	NotifySender *notify.Sender
	// PublicURL (if set) is the operator's public URL (RMMWAY_PUBLIC_URL).
	// Exposed via GET /api/public-url so the Add Device UI can prefill the
	// server URL with the configured public target instead of guessing
	// window.location.origin (wrong when behind a reverse proxy).
	PublicURL string
	// LoginRateLimit (L8) enables the per-IP failed-login limiter on
	// /api/login. Defaults to true; tests that hammer the login route set
	// it false.
	LoginRateLimit *bool
	// gap #1a: remote session support.
	// Sessions is the frame relay registry; nil disables /api/devices/{id}/session/*.
	Sessions *sessionrelay.Registry
	// SendSessionControl pushes a SessionControl downlink (open/close);
	// nil disables session start/stop.
	SendSessionControl func(deviceID string, sc *agentv1.SessionControl) bool
}

// New builds a Server. A nil Devices falls back to an in-memory store.
func New(cfg Config) *Server {
	if cfg.TokenLifetime <= 0 {
		cfg.TokenLifetime = 12 * time.Hour
	}
	if len(cfg.JWTSecret) == 0 {
		cfg.JWTSecret = []byte("rmmway-dev-secret-change-me")
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = "admin"
	}
	if len(cfg.AdminCaps) == 0 {
		cfg.AdminCaps = caps.AllCapabilities
	}
	devices := cfg.Devices
	if devices == nil {
		devices = store.NewMemoryDeviceStore()
	}
	// L8: the login limiter is on unless explicitly disabled (tests).
	var limiter *loginLimiter
	if cfg.LoginRateLimit == nil || *cfg.LoginRateLimit {
		limiter = newLoginLimiter()
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand.Read fails only on a broken /dev/urandom — fatal.
		panic("httpapi: generate salt: " + err.Error())
	}
	return &Server{
		loginLimiter:  limiter,
		devices:       devices,
		search:        cfg.Search,
		baseline:      cfg.Baseline,
		alerts:        cfg.Alerts,
		heal:          cfg.Heal,
		releases:      cfg.Releases,
		flows:         cfg.Flows,
		jwtSecret:     cfg.JWTSecret,
		tokenLifetime: cfg.TokenLifetime,
		adminUser:     cfg.AdminUser,
		adminSalt:     salt,
		adminHash:     pbkdf2.Key([]byte(cfg.AdminPassword), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New),
		adminCaps:     cfg.AdminCaps,
		mintBootstrap: cfg.MintBootstrap,
		enroll:        cfg.Enroll,
		dispatch:      cfg.Dispatch,
		commandState:  cfg.CommandState,
		export:        cfg.Export,
		logEvents:     cfg.LogEvents,
		metricNames:   cfg.MetricNames,
		metricSeries:  cfg.MetricSeries,
		webhooks:      cfg.Webhooks,
		setup:         cfg.Setup,
		clients:       cfg.Clients,
		users:         cfg.Users,
		tickets:       cfg.Tickets,
		notifyStore:   cfg.NotifyStore,
		notifySender:  cfg.NotifySender,
		rbac:               &users.RBAC{Secret: cfg.JWTSecret, Users: cfg.Users},
		publicURL:          cfg.PublicURL,
		sessions:           cfg.Sessions,
		sendSessionControl: cfg.SendSessionControl,
	}
}

// Register mounts the operator + admin routes on mux. Call once at boot.
// Each domain registers its own routes in its own file (wave-0 F1 — the old
// single ~80-line mux list is now one register* call per domain); this
// function is the single point that decides which domains are mounted.
func (s *Server) Register(mux *http.ServeMux) {
	// gap #3: unified login (users table + admin_users + env bootstrap,
	// DB-only once operator accounts exist) — see domain_users.go. The
	// legacy s.handleLogin stays reachable for the unwired (in-memory) path.
	mux.HandleFunc("/api/login", s.handleLoginUsers)
	registerDevices(s, mux)
	registerAlerts(s, mux)
	registerHeal(s, mux)
	registerFlows(s, mux)
	registerWebhooks(s, mux)
	registerEvents(s, mux)
	registerSetup(s, mux)
	registerEnroll(s, mux)
	registerCommands(s, mux)
	registerSettings(s, mux)
	registerClients(s, mux)
	registerUsers(s, mux)      // gap #3: operator accounts + API tokens (admin-only)
	registerTickets(s, mux)    // gap #7: helpdesk tickets (queue, SLA, notes)
	registerNotify(s, mux)     // gap #6: notification channels + policies
	registerSession(s, mux)    // gap #1a: remote session + file download routes
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in loginRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// L8: per-IP failure budget BEFORE the PBKDF2 work (brute-force +
	// work-amplification DoS).
	ip := ""
	if s.loginLimiter != nil {
		ip = clientIP(r)
		if until, ok := s.loginLimiter.allow(ip); !ok {
			secs := int64(time.Until(until).Seconds()) + 1
			w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "too many failed login attempts; try again later",
			})
			return
		}
	}
	limitFail := func() {
		if s.loginLimiter != nil {
			s.loginLimiter.recordFail(ip)
		}
	}
	limitOK := func() {
		if s.loginLimiter != nil {
			s.loginLimiter.recordOK(ip)
		}
	}
	// A-2: the wizard-minted root admin (database-backed, survives restarts)
	// is checked FIRST; the RMMWAY_ADMIN_USER/PASSWORD env pair remains a
	// fallback (dev mode, and the pre-setup window on a fresh server).
	if s.setup != nil && s.setup.CheckCredentials(r.Context(), in.Username, in.Password) {
		tok, err := ingest.MintOperatorJWT(s.jwtSecret, s.tokenLifetime, s.adminCaps)
		if err != nil {
			http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		limitOK()
		writeJSON(w, http.StatusOK, map[string]any{
			"token":        tok,
			"expiry":       time.Now().Add(s.tokenLifetime).UTC().Format(time.RFC3339),
			"capabilities": s.adminCaps,
		})
		return
	}
	// C #10a: if a DB row exists for this username (the wizard ran, or a
	// password was changed from the settings page), only that row's
	// credential is valid — falling back to the env pair would let a stale
	// env password bypass a changed one. (CheckCredentials above returned
	// false, so this is the "row exists, wrong password" case.)
	if s.setup != nil {
		if _, _, exists := s.setup.AdminCredentials(r.Context(), in.Username); exists {
			limitFail()
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
			return
		}
	}
	// Always compute the candidate hash (even on a wrong username) so
	// response timing doesn't reveal which field was wrong.
	candidate := pbkdf2.Key([]byte(in.Password), s.adminSalt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
	userOK := subtle.ConstantTimeCompare([]byte(in.Username), []byte(s.adminUser)) == 1
	passOK := subtle.ConstantTimeCompare(candidate, s.adminHash) == 1
	if !userOK || !passOK {
		limitFail()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}
	tok, err := ingest.MintOperatorJWT(s.jwtSecret, s.tokenLifetime, s.adminCaps)
	if err != nil {
		http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	limitOK()
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        tok,
		"expiry":       time.Now().Add(s.tokenLifetime).UTC().Format(time.RFC3339),
		"capabilities": s.adminCaps, // W3-3: what this session may dispatch
	})
}

// capsKey carries the authenticated operator's capability set (W3-3) on the
// request context, set by requireOperator from the session token's caps claim.
type capsKey struct{}

func sessionCaps(ctx context.Context) []string {
	if c, ok := ctx.Value(capsKey{}).([]string); ok {
		return c
	}
	return nil
}

func hasCapability(ctx context.Context, want string) bool {
	for _, c := range sessionCaps(ctx) {
		if c == want {
			return true
		}
	}
	return false
}

// requireOperator gates a handler behind a valid operator JWT and binds the
// session's capability set (W3-3) to the request context.
func (s *Server) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r.Header.Get("Authorization"))
		capList, ok2 := ingest.ParseOperatorJWT(s.jwtSecret, tok)
		if !ok || !ok2 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), capsKey{}, capList)))
	}
}

// requireOperatorStream is the SSE variant of requireOperator: it also accepts
// the operator JWT via ?token= (the EventSource browser API cannot set an
// Authorization header, so the UI passes the short-lived JWT as a query param
// for the stream route only — the header form is still honored for curl/API
// clients).
func (s *Server) requireOperatorStream(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The operator JWT arrives as an Authorization header (curl/API
		// clients) or a ?token= query param (EventSource can't set headers).
		// Header wins if present.
		var tok string
		if htok, ok := bearerToken(r.Header.Get("Authorization")); ok {
			tok = htok
		} else if q := r.URL.Query().Get("token"); q != "" {
			tok = q
		}
		capList, ok2 := ingest.ParseOperatorJWT(s.jwtSecret, tok)
		if tok == "" || !ok2 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), capsKey{}, capList)))
	}
}

func bearerToken(h string) (string, bool) {
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	t := strings.TrimSpace(h[len(p):])
	if t == "" {
		return "", false
	}
	return t, true
}

// ---- L8: login rate limiting -------------------------------------------------
//
// /api/login is open (it's how you get a token) and each attempt does a
// 100k-iteration PBKDF2 — an unbounded brute force is both an offline
// dictionary race and a work-amplification DoS. This is a small per-IP
// limiter: up to loginMaxFails failures per loginWindow per IP, then a
// loginLockout lockout. A success clears the IP's failure count. In-memory
// (per process) — good enough for a single-server RMM; a fronted deployment
// should still rate-limit at the edge.

const (
	loginWindow   = 5 * time.Minute
	loginMaxFails = 10
	loginLockout  = 15 * time.Minute
	loginPurgeAgo = loginWindow + loginLockout
)

type loginIPState struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu    sync.Mutex
	perIP map[string]*loginIPState
	now   func() time.Time // injectable for tests
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{perIP: make(map[string]*loginIPState), now: time.Now}
}

// allow reports whether ip may attempt a login right now; if not, it
// returns when the lockout ends.
func (l *loginLimiter) allow(ip string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if st := l.perIP[ip]; st != nil {
		if !st.lockedUntil.IsZero() {
			if now.Before(st.lockedUntil) {
				return st.lockedUntil, false
			}
			st.lockedUntil = time.Time{}
		}
	}
	return now, true
}

// recordFail counts a failed attempt and locks the IP out when the budget
// is spent.
func (l *loginLimiter) recordFail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.purgeLocked(now)
	st := l.perIP[ip]
	if st == nil {
		st = &loginIPState{}
		l.perIP[ip] = st
	}
	if now.Sub(st.windowStart) > loginWindow || st.fails == 0 {
		st.windowStart = now
		st.fails = 0
	}
	st.fails++
	if st.fails >= loginMaxFails {
		st.lockedUntil = now.Add(loginLockout)
		st.fails = 0
	}
}

// recordOK clears the failure count for a successful login.
func (l *loginLimiter) recordOK(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perIP[ip] = nil
}

// purgeLocked drops stale entries (callers hold the lock).
func (l *loginLimiter) purgeLocked(now time.Time) {
	for ip, st := range l.perIP {
		if st == nil { // recordOK clears an IP by nil-ing its entry
			delete(l.perIP, ip)
			continue
		}
		if !st.lockedUntil.IsZero() {
			if now.After(st.lockedUntil) {
				delete(l.perIP, ip)
			}
			continue
		}
		if now.Sub(st.windowStart) > loginPurgeAgo {
			delete(l.perIP, ip)
		}
	}
}

// clientIP best-effort extraction: the first X-Forwarded-For hop (behind a
// proxy) or the TCP peer.
func clientIP(r *http.Request) string {
	if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
		if ip := strings.TrimSpace(strings.Split(ff, ",")[0]); ip != "" {
			return ip
		}
	}
	host := r.RemoteAddr // "ip:port"
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

// ---- handlers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
