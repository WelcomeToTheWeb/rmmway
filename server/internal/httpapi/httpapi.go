// Package httpapi is the operator-facing HTTP API (W2-1): operator login,
// the auth-gated device list the frontend renders, and the pre-existing
// /admin/* endpoints the bootstrap installer and device search rely on.
//
// Auth model: a human operator logs in with a username + password
// (single admin account, configured via env) and receives a short-lived
// operator JWT (subject "operator", issuer "rmmway"). /api/* routes are
// gated on that token; /admin/* stays open for machine callers (the
// curl|sh installer, the e2e harness, the search CLI in the README).
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/baseline"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/export"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/releases"
	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/setup"
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
	// webhooks (W6-2) is the webhook + event-stream framework; nil disables
	// /{api|admin}/webhooks* and /{api|admin}/events/stream (in-memory mode).
	webhooks *webhook.Service
	// setup (A-2) is the first-boot wizard backend; nil = in-memory mode
	// (the wizard is unavailable, the env admin login is the only one).
	setup *setup.Service
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
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand.Read fails only on a broken /dev/urandom — fatal.
		panic("httpapi: generate salt: " + err.Error())
	}
	return &Server{
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
		webhooks:      cfg.Webhooks,
		setup:         cfg.Setup,
	}
}

// Register mounts the operator + admin routes on mux. Call once at boot.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/devices", s.requireOperator(s.deviceList))
	// W2-2: fuzzy device search (Cmd-K backing) + command dispatch, both auth-gated.
	mux.HandleFunc("/api/search", s.requireOperator(s.handleSearch))
	mux.HandleFunc("/api/devices/", s.requireOperator(s.deviceSub))
	// W3-3: the device's dispatched commands + results, open for e2e/ops.
	mux.HandleFunc("/admin/devices/", s.deviceSub)
	// W2-3: dynamic baselining — anomaly feed (auth-gated) + manual pass.
	mux.HandleFunc("/api/baseline/anomalies", s.requireOperator(s.handleBaselineAnomalies))
	mux.HandleFunc("/api/baseline/run", s.requireOperator(s.handleBaselineRun))
	// W2-4: deduped alert inbox (auth-gated) + ack/resolve + counts.
	mux.HandleFunc("/api/alerts", s.requireOperator(s.handleAlerts))
	mux.HandleFunc("/api/alerts/", s.requireOperator(s.handleAlertSub))
	// W5-1: self-healing playbook engine — playbooks, runs (+ stage log),
	// and a manual pass trigger. Open mirrors below for e2e/ops.
	mux.HandleFunc("/api/heal/playbooks", s.requireOperator(s.handleHealPlaybooks))
	mux.HandleFunc("/api/heal/runs", s.requireOperator(s.handleHealRuns))
	mux.HandleFunc("/api/heal/runs/", s.requireOperator(s.handleHealRunSub))
	mux.HandleFunc("/api/heal/pass", s.requireOperator(s.handleHealPass))
	// W5-2: event-driven automation chains — compose (DAG CRUD), trigger
	// (synthetic), runs (+ node log), and a manual sweep. Open mirrors
	// below for e2e/ops.
	mux.HandleFunc("/api/flows", s.requireOperator(s.handleFlows))
	mux.HandleFunc("/api/flows/", s.requireOperator(s.handleFlowSub))
	mux.HandleFunc("/admin/flows", s.handleFlows)
	mux.HandleFunc("/admin/flows/", s.handleFlowSub)
	// W6-2: signed webhooks + live event stream. Endpoints are user-defined
	// (HMAC secret + categories); /events/stream is the SSE subscription.
	// Open mirrors below for e2e/ops.
	mux.HandleFunc("/api/webhooks", s.requireOperator(s.handleWebhooks))
	mux.HandleFunc("/api/webhooks/", s.requireOperator(s.handleWebhookSub))
	mux.HandleFunc("/api/events/stream", s.requireOperator(s.handleEventStream))
	mux.HandleFunc("/admin/webhooks", s.handleWebhooks)
	mux.HandleFunc("/admin/webhooks/", s.handleWebhookSub)
	mux.HandleFunc("/admin/events/stream", s.handleEventStream)
	// A-2: first-boot setup wizard. /api/setup/status is always open (the UI
	// needs it to decide between wizard and login, pre-auth); the POST routes
	// are open only while the server is uninitialized, then operator-gated.
	mux.HandleFunc("/api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/api/setup/complete", s.setupGate(s.handleSetupComplete))
	mux.HandleFunc("/api/setup/smtp/test", s.setupGate(s.handleSetupSMTPTest))
	mux.HandleFunc("/admin/bootstrap", s.handleBootstrap)
	// "Add a device": the auth-gated mint (the UI mints a token to hand to an
	// installer) and the OPEN bootstrap enroll a remote agent calls over the
	// operator's HTTPS origin (machine caller, like /agent/releases).
	mux.HandleFunc("/api/bootstrap", s.requireOperator(s.handleBootstrapMint))
	mux.HandleFunc("/agent/enroll", s.handleAgentEnroll)
	mux.HandleFunc("/admin/devices", s.deviceList) // open: installer / e2e / README
	mux.HandleFunc("/admin/search", s.handleSearch)
	mux.HandleFunc("/admin/baseline/anomalies", s.handleBaselineAnomalies) // open: e2e
	mux.HandleFunc("/admin/baseline/run", s.handleBaselineRun)             // open: e2e
	mux.HandleFunc("/admin/alerts", s.handleAlerts)                        // open: e2e
	mux.HandleFunc("/admin/alerts/", s.handleAlertSub)                     // open: e2e
	mux.HandleFunc("/admin/heal/playbooks", s.handleHealPlaybooks)         // open: e2e
	mux.HandleFunc("/admin/heal/runs", s.handleHealRuns)                   // open: e2e
	mux.HandleFunc("/admin/heal/runs/", s.handleHealRunSub)                // open: e2e
	mux.HandleFunc("/admin/heal/pass", s.handleHealPass)                   // open: e2e

	// W4-2: signed agent release distribution (open — the AGENT fetches these,
	// not an operator). The manifest + assets are only served when a releases
	// directory is configured; otherwise the routes 404 (agent = up-to-date).
	if s.releases != nil {
		mux.HandleFunc("/agent/releases/latest", s.handleReleasesLatest)
		mux.HandleFunc("/agent/releases/latest/", s.handleReleaseAsset)
	}
}

// ---- W4-2: signed agent release distribution --------------------------------

// handleReleasesLatest serves the current release manifest. The agent
// compares its public_key to the pinned key and verifies each asset's
// signature before installing — the server is a carrier, not a trust anchor.
func (s *Server) handleReleasesLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m, err := s.releases.Manifest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleReleaseAsset serves one asset (binary or .minisig) by name, limited
// to what the current manifest allows.
func (s *Server) handleReleaseAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/agent/releases/latest/")
	p, err := s.releases.AssetPath(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, p)
}

// ---- operator auth ----------------------------------------------------------

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
	// A-2: the wizard-minted root admin (database-backed, survives restarts)
	// is checked FIRST; the RMMWAY_ADMIN_USER/PASSWORD env pair remains a
	// fallback (dev mode, and the pre-setup window on a fresh server).
	if s.setup != nil && s.setup.CheckCredentials(r.Context(), in.Username, in.Password) {
		tok, err := ingest.MintOperatorJWT(s.jwtSecret, s.tokenLifetime, s.adminCaps)
		if err != nil {
			http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":        tok,
			"expiry":       time.Now().Add(s.tokenLifetime).UTC().Format(time.RFC3339),
			"capabilities": s.adminCaps,
		})
		return
	}
	// Always compute the candidate hash (even on a wrong username) so
	// response timing doesn't reveal which field was wrong.
	candidate := pbkdf2.Key([]byte(in.Password), s.adminSalt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
	userOK := subtle.ConstantTimeCompare([]byte(in.Username), []byte(s.adminUser)) == 1
	passOK := subtle.ConstantTimeCompare(candidate, s.adminHash) == 1
	if !userOK || !passOK {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}
	tok, err := ingest.MintOperatorJWT(s.jwtSecret, s.tokenLifetime, s.adminCaps)
	if err != nil {
		http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
		return
	}
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

// ---- handlers ---------------------------------------------------------------

type deviceOut struct {
	ID           string    `json:"id"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	Interfaces   []string  `json:"interfaces"`
	Tags         []string  `json:"tags"`
	Online       bool      `json:"online"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

// deviceList is served at both /api/devices (auth-gated) and /admin/devices
// (open). It returns every enrolled device with live status.
func (s *Server) deviceList(w http.ResponseWriter, r *http.Request) {
	out := []deviceOut{}
	list, err := s.devices.List(r.Context())
	if err != nil {
		http.Error(w, "device list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, d := range list {
		if d.Interfaces == nil {
			d.Interfaces = []string{}
		}
		if d.Tags == nil {
			d.Tags = []string{}
		}
		out = append(out, deviceOut{
			ID: d.ID, Hostname: d.Hostname, OS: d.OS, Arch: d.Arch,
			AgentVersion: d.AgentVersion, Interfaces: d.Interfaces, Tags: d.Tags,
			Online: d.Online, FirstSeen: d.FirstSeen, LastSeen: d.LastSeen,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSearch serves Meilisearch device search. Degraded (503) when the
// index is unavailable so the rest of the API keeps working.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		http.Error(w, "search index not available (meilisearch down or disabled)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query().Get("q")
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	res, err := s.search.Search(r.Context(), q, limit)
	if err != nil {
		http.Error(w, "search: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// deviceSub routes the /{api|admin}/devices/{id}/... subtree (W2-2 + W3-3 +
// W4-3 + W6-1):
//
//	POST {id}/commands  — dispatch (auth-gated under /api, open under /admin)
//	GET  {id}/commands  — pending commands + recorded results (W3-3)
//	GET  {id}/export    — the per-client full export bundle (W4-3)
//	GET  {id}/events    — recent indexed agent-log events (W6-1)
func (s *Server) deviceSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "devices", "<id>", "commands"|"export"|"events"]
	if len(parts) != 4 || parts[1] != "devices" || parts[2] == "" ||
		(parts[3] != "commands" && parts[3] != "export" && parts[3] != "events") {
		http.Error(w, "expected /devices/{id}/(commands|export|events)", http.StatusNotFound)
		return
	}
	switch parts[3] {
	case "commands":
		if r.Method == http.MethodGet {
			s.deviceCommands(w, r, parts[2])
			return
		}
		s.dispatchCommand(w, r, parts[2])
	case "export":
		s.handleDeviceExport(w, r, parts[2])
	case "events":
		s.deviceEvents(w, r, parts[2])
	default:
		http.Error(w, "expected /devices/{id}/(commands|export|events)", http.StatusNotFound)
	}
}

// deviceEvents serves the W6-1 per-device log view: the device's recent
// indexed agent-log events (the Timescale copy of what also ships to Loki).
//
//	GET /{api|admin}/devices/{id}/events?limit=50&level=warn
//
//	200 {device_id, events: [{id, level, msg, attrs, timestamp_ms, time}]} (newest first)
//	400 bad limit/level
//	404 unknown device
//	503 log events not wired (pre-W6-1 server)
func (s *Server) deviceEvents(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logEvents == nil {
		http.Error(w, "log events not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		l, err := strconv.Atoi(v)
		if err != nil || l <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = l
	}
	level := q.Get("level")
	switch strings.ToLower(level) {
	case "", "debug", "info", "warn", "error":
	default:
		http.Error(w, "level must be one of debug|info|warn|error", http.StatusBadRequest)
		return
	}
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	events, err := s.logEvents(deviceID, limit, strings.ToLower(level))
	if err != nil {
		http.Error(w, "log events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []store.LogEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"events":    events,
	})
}

// deviceCommands serves the W3-3 command audit view for one device.
//
//	GET /{api|admin}/devices/{id}/commands
//
//	200 {device_id, pending: [...], results: [...]}
//	404 unknown device
//	503 command state not wired
func (s *Server) deviceCommands(w http.ResponseWriter, r *http.Request, deviceID string) {
	if s.commandState == nil {
		http.Error(w, "command state not configured", http.StatusServiceUnavailable)
		return
	}
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	pending, results := s.commandState(deviceID)
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"pending":   pending,
		"results":   results,
	})
}

// handleDeviceExport streams the client's FULL export bundle (W4-3 — the
// no-lock-in promise): one request, one self-describing ZIP with the
// device's inventory + config, all metrics (Parquet), 1-minute rollups
// (Parquet), complete alert history, and a manifest that drives
// verification (export.Verify).
//
//	GET /{api|admin}/devices/{id}/export[?since=RFC3339&until=RFC3339&rollups=0]
//
//	200  application/zip attachment (the bundle)
//	400  bad since/until window
//	404  unknown device
//	503  export not wired (in-memory-mode deployments)
//	500  build error (after headers are out the stream just ends short —
//	     the manifest is the integrity contract, a truncated bundle fails
//	     export.Verify instead of passing as complete)
func (s *Server) handleDeviceExport(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.export == nil {
		http.Error(w, "export not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	var since, until time.Time
	if v := q.Get("since"); v != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("until"); v != "" {
		var err error
		if until, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "until must be RFC3339", http.StatusBadRequest)
			return
		}
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		http.Error(w, "until must be after since", http.StatusBadRequest)
		return
	}
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	withRollups := q.Get("rollups") != "0"
	fname := "rmmway-export-" + deviceID + "-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.WriteHeader(http.StatusOK)
	if _, err := s.export.Export(r.Context(), deviceID, since, until, withRollups, w); err != nil {
		// Headers already sent: the bundle is truncated. The manifest
		// contract makes that detectable (export.Verify fails), but log it.
		fmt.Fprintf(w, "\nexport error: %v\n", err)
	}
}

// dispatchCommand mints a command for a device and pushes it to its live
// stream (W2-2 "a known action is runnable from the palette"). W3-3: the
// operator's session token must carry the action's capability (403 if not),
// and the pushed command carries a short-lived capability token the agent
// verifies before acting.
//
// POST /api/devices/{id}/commands   {"action":"run_script"|"reboot", "lang":"sh", "script":"…", "timeout_s":0}
//
//	200 {command_id}            — pushed to the live stream
//	503                          — dispatch not wired (tests)
//	400                          — bad body / unknown action / unsupported lang
//	403                          — session lacks the action's capability (W3-3)
//	404                          — unknown device
//	502                          — device has no live stream (offline)
func (s *Server) dispatchCommand(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.dispatch == nil {
		http.Error(w, "command dispatch not configured", http.StatusServiceUnavailable)
		return
	}
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	var in dispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	action, err := buildCommandAction(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// W3-3: the operator's session token must carry this action's capability.
	capName, ok := caps.ForAction(action)
	if !ok {
		http.Error(w, "no capability for action", http.StatusBadRequest)
		return
	}
	if !hasCapability(r.Context(), capName) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "session lacks capability " + capName,
		})
		return
	}
	cmdID, err := s.dispatch(deviceID, action)
	if err != nil {
		if strings.Contains(err.Error(), "not reachable") {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "device is offline (no live stream)"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"command_id": cmdID, "device_id": deviceID})
}

// dispatchRequest is the JSON body for POST /api/devices/{id}/commands.
type dispatchRequest struct {
	Action   string   `json:"action"`
	Lang     string   `json:"lang"`
	Script   string   `json:"script"` // base64
	Args     []string `json:"args"`
	TimeoutS int32    `json:"timeout_s"`
}

// buildCommandAction maps the JSON body onto the proto oneof action.
func buildCommandAction(in dispatchRequest) (any, error) {
	switch in.Action {
	case "run_script":
		lang := in.Lang
		if lang == "" {
			lang = "sh"
		}
		switch lang {
		case "sh", "powershell", "python":
		default:
			return nil, fmt.Errorf("unsupported script lang %q (want sh|powershell|python)", lang)
		}
		if _, err := base64.StdEncoding.DecodeString(in.Script); err != nil {
			return nil, fmt.Errorf("script must be base64: %v", err)
		}
		return &agentv1.Command_RunScript{RunScript: &agentv1.RunScript{
			Lang:      lang,
			ScriptB64: in.Script,
			Args:      in.Args,
		}}, nil
	case "reboot":
		return &agentv1.Command_Reboot{Reboot: &agentv1.Reboot{DelayS: 0}}, nil
	default:
		return nil, fmt.Errorf("unknown action %q (want run_script|reboot)", in.Action)
	}
}

// ---- W2-3: dynamic baselining ----------------------------------------------

// handleBaselineAnomalies serves the anomaly feed.
//
// GET /api/baseline/anomalies?limit=100
//
//	200 [{id, device_id, name, source, at, value, score, channel, ...}]
//	503  baseline engine not wired
func (s *Server) handleBaselineAnomalies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.baseline == nil {
		http.Error(w, "baseline engine not configured", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.baseline.Recent(r.Context(), limit)
	if err != nil {
		http.Error(w, "baseline anomalies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []store.StoredAnomaly{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleBaselineRun forces one deterministic scoring pass (e2e / ops).
//
// POST /api/baseline/run
//
//	200 {anomalies: [...], series: N, runs: M}
//	503  baseline engine not wired
//	500  source error (e.g. DB down)
func (s *Server) handleBaselineRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.baseline == nil {
		http.Error(w, "baseline engine not configured", http.StatusServiceUnavailable)
		return
	}
	anoms, err := s.baseline.RunNow(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if anoms == nil {
		anoms = []baseline.Anomaly{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"anomalies": anoms,
		"series":    len(s.baseline.Job.Series()),
		"runs":      s.baseline.Job.RunCount(),
	})
}

// ---- W2-4: alert inbox -----------------------------------------------------

// handleAlerts serves the deduped alert inbox.
//
// GET /api/alerts?status=open&device_id=...&limit=100
//
// \t200 [{id, device_id, hostname, name, source, status, score, channel,
// \t      value, expected, events, first_at, last_at, resolved_at, …}]
// \t400  unknown status
// \t503  alert store not wired
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	deviceID := q.Get("device_id")
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := s.alerts.List(r.Context(), status, deviceID, limit)
	if err != nil {
		http.Error(w, "alerts: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rows == nil {
		rows = []store.Alert{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// alertCounts returns the per-status counts for the inbox badge.
//
// GET /api/alerts/counts
//
// \t200 {open: n, acked: n, resolved: n}
// \t503  alert store not wired
func (s *Server) alertCounts(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	counts, err := s.alerts.Counts(r.Context())
	if err != nil {
		http.Error(w, "alert counts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// handleAlertSub routes the /api/alerts/{sub} paths: "counts" -> the
// per-status badge counts, otherwise the alert id for a status PATCH.
func (s *Server) handleAlertSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api","alerts","<id|counts>"]
	if len(parts) == 3 && parts[2] == "counts" {
		s.alertCounts(w, r)
		return
	}
	s.handleAlertStatus(w, r)
}

// handleAlertStatus applies a manual inbox transition.
//
// PATCH /api/alerts/{id}   {"status":"acked"|"resolved"}
//
// \t200 {alert}
// \t400  bad body / invalid transition
// \t404  unknown id
// \t503  alert store not wired
func (s *Server) handleAlertStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alert inbox not configured", http.StatusServiceUnavailable)
		return
	}
	// /api/alerts/{id}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api","alerts","<id>"]
	if len(parts) != 3 || parts[2] == "" {
		http.Error(w, "expected /api/alerts/{id}", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "alert id must be a positive integer", http.StatusBadRequest)
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.alerts.SetStatus(r.Context(), id, in.Status)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleBootstrap mints a one-time enroll code (W1-3 installer / e2e).
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.mintBootstrap == nil {
		http.Error(w, "bootstrap mint not configured", http.StatusServiceUnavailable)
		return
	}
	tok, devID := s.mintBootstrap()
	writeJSON(w, http.StatusOK, map[string]string{"bootstrap_token": tok, "device_id": devID})
}

// handleBootstrapMint is the auth-gated "Add a device" action: it mints a
// one-time bootstrap token for the operator UI to hand to an installer. Same
// mint as /admin/bootstrap, but only a signed-in operator may call it.
//
//	POST /api/bootstrap   200 {bootstrap_token, device_id}   401 no/bad token
//	503 mint not wired
func (s *Server) handleBootstrapMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.mintBootstrap == nil {
		http.Error(w, "bootstrap mint not configured", http.StatusServiceUnavailable)
		return
	}
	tok, devID := s.mintBootstrap()
	writeJSON(w, http.StatusOK, map[string]string{"bootstrap_token": tok, "device_id": devID})
}

// enrollRequest / enrollResponse are the JSON shape of the bootstrap enroll
// over the operator's HTTPS origin (POST /agent/enroll). They mirror the
// gRPC EnrollRequest/EnrollResponse 1:1 — the agent speaks HTTP here so a
// fresh agent (no mTLS material yet) can join without the internal-only plain
// gRPC bootstrap port; only 443 + the mTLS gRPC port need to be open.
type enrollRequest struct {
	BootstrapToken string   `json:"bootstrap_token"`
	Hostname       string   `json:"hostname"`
	OS             string   `json:"os"`
	Arch           string   `json:"arch"`
	AgentVersion   string   `json:"agent_version"`
	Interfaces     []string `json:"interfaces"`
}

type enrollResponse struct {
	DeviceID           string `json:"device_id"`
	JWT                string `json:"jwt"`
	HeartbeatIntervalS int32  `json:"heartbeat_interval_s"`
	MetricIntervalS    int32  `json:"metric_interval_s"`
	LeafCertPem        string `json:"leaf_cert_pem"`
	LeafKeyPem         string `json:"leaf_key_pem"`
	OrgRootCaPem       string `json:"org_root_ca_pem"`
}

// httpStatusFromGRPC maps a gRPC status code (as returned by the ingest
// service's Enroll) onto an HTTP status, so the agent's HTTP enroll can tell
// a definitive business error (4xx — do not retry another channel) from a
// transient one (5xx / connection error — fall back to plain gRPC).
func httpStatusFromGRPC(err error) int {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument:
			return http.StatusBadRequest
		case codes.Unauthenticated, codes.PermissionDenied:
			return http.StatusForbidden
		case codes.NotFound:
			return http.StatusNotFound
		case codes.Unavailable:
			return http.StatusServiceUnavailable
		default:
			return http.StatusInternalServerError
		}
	}
	return http.StatusInternalServerError
}

// handleAgentEnroll is the bootstrap enroll over the operator's HTTPS origin
// (POST /agent/enroll). It reuses the ingest service's Enroll — the exact
// same one-time-token -> identity (device_id + agent JWT) + mTLS-leaf
// issuance as the gRPC RPC — so a remote agent can enroll with only the
// operator origin (443) and the mTLS gRPC port open; the plain gRPC bootstrap
// port stays internal. Open for machine callers (the agent), protected by the
// one-time bootstrap token, like /agent/releases.
//
//	POST /agent/enroll  {bootstrap_token, hostname, os, arch, agent_version, interfaces[]}
//
//	200 {device_id, jwt, leaf_cert_pem, leaf_key_pem, org_root_ca_pem, ...}
//	400 bad body / missing bootstrap_token
//	403 unknown or already-used bootstrap token
//	503 enroll not wired (in-memory mode without an org CA path)
//	500 server error
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.enroll == nil {
		http.Error(w, "enroll not configured", http.StatusServiceUnavailable)
		return
	}
	var in enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req := &agentv1.EnrollRequest{
		BootstrapToken: in.BootstrapToken,
		Hostname:       in.Hostname,
		Os:             in.OS,
		Arch:           in.Arch,
		AgentVersion:   in.AgentVersion,
		Interfaces:     in.Interfaces,
	}
	resp, err := s.enroll(r.Context(), req)
	if err != nil {
		code := httpStatusFromGRPC(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, enrollResponse{
		DeviceID:           resp.GetDeviceId(),
		JWT:                resp.GetJwt(),
		HeartbeatIntervalS: resp.GetHeartbeatIntervalS(),
		MetricIntervalS:    resp.GetMetricIntervalS(),
		LeafCertPem:        resp.GetLeafCertPem(),
		LeafKeyPem:         resp.GetLeafKeyPem(),
		OrgRootCaPem:       resp.GetOrgRootCaPem(),
	})
}

// ---- W5-1: self-healing playbook engine ------------------------------------

// handleHealPlaybooks lists the playbook library.
//
// GET /api/heal/playbooks
//
//	200 [playbook, ...]
//	503  heal engine not wired (in-memory mode)
func (s *Server) handleHealPlaybooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	pbs, err := s.heal.Store().Playbooks(r.Context(), r.URL.Query().Get("enabled") != "false")
	if err != nil {
		http.Error(w, "playbooks: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if pbs == nil {
		pbs = []heal.Playbook{}
	}
	writeJSON(w, http.StatusOK, pbs)
}

// handleHealRuns lists self-heal runs (the remediation audit trail).
//
// GET /api/heal/runs?status=&device_id=&limit=
//
//	200 [run, ...]
//	503  heal engine not wired
func (s *Server) handleHealRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.heal.Store().Runs(r.Context(), q.Get("status"), q.Get("device_id"), limit)
	if err != nil {
		http.Error(w, "heal runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []heal.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// healRunDetail serves one run with its stage log (the audit trail).
//
// GET /api/heal/runs/{id}
//
//	200 {run, events: [event, ...]}
//	404  unknown id
//	503  heal engine not wired
func (s *Server) healRunDetail(w http.ResponseWriter, r *http.Request, id int64) {
	st := s.heal.Store()
	run, err := st.Run(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	events, err := st.Events(r.Context(), id)
	if err != nil {
		http.Error(w, "heal events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events})
}

// handleHealRunSub routes the /api/heal/runs/{id} detail path.
func (s *Server) handleHealRunSub(w http.ResponseWriter, r *http.Request) {
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin","heal","runs","<id>"]
	if len(parts) != 4 || parts[3] == "" {
		http.Error(w, "expected /api/heal/runs/{id}", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "run id must be a positive integer", http.StatusBadRequest)
		return
	}
	s.healRunDetail(w, r, id)
}

// handleHealPass runs one detect + advance pass synchronously and reports
// the outcome (e2e trigger; the background loop uses the same RunOnce).
//
// POST /api/heal/pass
//
//	200 {pass summary}
//	503  heal engine not wired
func (s *Server) handleHealPass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.heal == nil {
		http.Error(w, "heal engine not configured", http.StatusServiceUnavailable)
		return
	}
	pass := s.heal.RunOnce(r.Context(), time.Now())
	writeJSON(w, http.StatusOK, pass)
}

// ---- W5-2: event-driven automation chains ----------------------------------

// flowRequest is the JSON body for POST/PATCH /api/flows.
type flowRequest struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Graph       *flow.Graph `json:"graph"`
	CooldownS   *int        `json:"cooldown_seconds"`
	Enabled     *bool       `json:"enabled"`
}

// handleFlows serves the flow list (GET) and flow creation (POST).
//
//	GET  /api/flows?enabled=  -> [flow, ...]
//	POST /api/flows           -> 201 {flow} (graph validated server-side)
//	503 flow engine not wired
func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		http.Error(w, "flow engine not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		flows, err := s.flows.Store().ListFlows(r.Context(), r.URL.Query().Get("enabled") == "true")
		if err != nil {
			http.Error(w, "flows: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if flows == nil {
			flows = []flow.Flow{}
		}
		writeJSON(w, http.StatusOK, flows)
	case http.MethodPost:
		var in flowRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.Graph == nil {
			http.Error(w, "name + graph are required", http.StatusBadRequest)
			return
		}
		if err := in.Graph.Validate(); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		cd, en := 0, true
		if in.CooldownS != nil {
			cd = *in.CooldownS
		}
		if in.Enabled != nil {
			en = *in.Enabled
		}
		f, err := s.flows.Store().CreateFlow(r.Context(), in.Name, in.Description, *in.Graph, cd, en)
		if err != nil {
			if isFlowUniqueViolation(err) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "flow name already exists"})
				return
			}
			http.Error(w, "create flow: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, f)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleFlowSub routes the /{api|admin}/flows/{...} subtree:
//
//	GET    {id}             -> one flow
//	PATCH  {id}             -> partial update (name/description/graph/cooldown/enabled)
//	DELETE {id}             -> delete (runs keep their denormalized name)
//	POST   {id}/trigger     -> synthetic trigger {device_id, source?, value?}
//	GET    runs             -> chain executions (?status=&flow_id=&device_id=&limit=)
//	GET    runs/{id}        -> one run + its node log (the audit trail)
//	POST   sweep            -> one sweep pass (re-cover in-flight runs)
//	POST   sample           -> one sampler pass (real-metric triggers)
func (s *Server) handleFlowSub(w http.ResponseWriter, r *http.Request) {
	if s.flows == nil {
		http.Error(w, "flow engine not configured", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "flows", ...rest]
	rest := parts[2:]

	// The runs subtree lives under flows/ (not behind an id).
	if len(rest) >= 1 && rest[0] == "runs" {
		if len(rest) == 1 {
			s.handleFlowRuns(w, r)
			return
		}
		if len(rest) == 2 {
			s.handleFlowRunDetail(w, r, rest[1])
			return
		}
		http.Error(w, "expected /flows/runs/{id}", http.StatusNotFound)
		return
	}
	if len(rest) == 1 && (rest[0] == "sweep" || rest[0] == "sample") {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now()
		switch rest[0] {
		case "sweep":
			s.flows.Sweep(r.Context(), now)
			active, _ := s.flows.Store().ActiveRuns(r.Context())
			writeJSON(w, http.StatusOK, map[string]int{"active_runs": len(active)})
		case "sample":
			published := s.flows.SampleOnce(r.Context(), now)
			writeJSON(w, http.StatusOK, map[string]int{"trigger_events": published})
		}
		return
	}
	if len(rest) < 1 {
		http.Error(w, "expected /flows/{id}...", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "flow id must be a positive integer", http.StatusBadRequest)
		return
	}

	// {id}/trigger
	if len(rest) == 2 && rest[1] == "trigger" {
		s.handleFlowTrigger(w, r, id)
		return
	}
	if len(rest) != 1 {
		http.Error(w, "expected /flows/{id} or /flows/{id}/trigger", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f, err := s.flows.Store().Flow(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, f)
	case http.MethodPatch:
		var in flowRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Graph != nil {
			if err := in.Graph.Validate(); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				return
			}
		}
		name, desc := &in.Name, &in.Description
		if in.Name == "" {
			name = nil
		}
		if in.Description == "" {
			desc = nil
		}
		f, err := s.flows.Store().UpdateFlow(r.Context(), id, name, desc, in.Graph, in.CooldownS, in.Enabled)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, f)
	case http.MethodDelete:
		if err := s.flows.Store().DeleteFlow(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PATCH or DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleFlowTrigger fires a SYNTHETIC trigger: the API publishes the
// trigger event on the bus (value nil = measure the latest fresh sample)
// and the chain proceeds asynchronously (W5-2 DoD's "fires on the
// synthetic trigger").
//
//	POST /api/flows/{id}/trigger   {"device_id":"…", "source":"", "value":95}
//
//	202 {flow_id, device_id, published: true}
//	404 unknown flow / unknown device
//	400 bad body
func (s *Server) handleFlowTrigger(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.flows.Store().Flow(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var in struct {
		DeviceID string   `json:"device_id"`
		Source   string   `json:"source"`
		Value    *float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.DeviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	if ok, err := s.devices.Contains(r.Context(), in.DeviceID); err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown device"})
		return
	}
	if err := s.flows.Trigger(r.Context(), id, in.DeviceID, in.Source, in.Value); err != nil {
		http.Error(w, "publish trigger: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"flow_id": id, "device_id": in.DeviceID, "published": true,
	})
}

// handleFlowRuns lists chain executions.
//
//	GET /api/flows/runs?status=&flow_id=&device_id=&limit=
//
//	200 [run, ...]
func (s *Server) handleFlowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var flowID *int64
	if v := q.Get("flow_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			flowID = &n
		}
	}
	runs, err := s.flows.Store().Runs(r.Context(), q.Get("status"), q.Get("device_id"), flowID, 100)
	if err != nil {
		http.Error(w, "flow runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []flow.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleFlowRunDetail serves one run with its node log (the audit trail).
//
//	GET /api/flows/runs/{id}
//
//	200 {run, events: [hop, ...]}
//	404 unknown id
func (s *Server) handleFlowRunDetail(w http.ResponseWriter, r *http.Request, idStr string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "run id must be a positive integer", http.StatusBadRequest)
		return
	}
	run, err := s.flows.Store().Run(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	events, err := s.flows.Store().Events(r.Context(), id)
	if err != nil {
		http.Error(w, "flow events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events})
}

// isFlowUniqueViolation reports the uq_flow_name duplicate-name error.
func isFlowUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "uq_flow_name")
}

// ---- W6-2: signed webhooks + live event stream -----------------------------

// webhookRequest is the JSON body for POST/PATCH /{api|admin}/webhooks.
type webhookRequest struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Secret      string   `json:"secret"`
	Categories  []string `json:"categories"` // empty/absent = all
	Enabled     *bool    `json:"enabled"`
	MaxAttempts *int     `json:"max_attempts"`
	TimeoutMS   *int     `json:"timeout_ms"`
}

// normalizeCategories filters the requested categories to the known set
// (deduped). An empty result means "all categories".
func normalizeCategories(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range in {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return webhook.AllCategories
	}
	return out
}

func unknownCategories(in []string) []string {
	var out []string
	for _, c := range in {
		if c != "" && !containsStr(webhook.AllCategories, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// validWebhookURL reports whether u is an absolute http(s) URL.
func validWebhookURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// handleWebhooks serves the endpoint list (GET) and creation (POST).
//
//	GET  /{api|admin}/webhooks   -> [endpoint, ...] (secret omitted)
//	POST /{api|admin}/webhooks   -> 201 {endpoint} (secret omitted)
//	503 webhook framework not wired
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	st := s.webhooks.Store()
	switch r.Method {
	case http.MethodGet:
		eps, err := st.ListEndpoints(r.Context())
		if err != nil {
			http.Error(w, "webhooks: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]webhook.Public, 0, len(eps))
		for i := range eps {
			out = append(out, eps[i].Public())
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in webhookRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.Name == "" || in.URL == "" || in.Secret == "" {
			http.Error(w, "name, url and secret are required", http.StatusBadRequest)
			return
		}
		if !validWebhookURL(in.URL) {
			http.Error(w, "url must be an absolute http(s) URL", http.StatusBadRequest)
			return
		}
		if bad := unknownCategories(in.Categories); len(bad) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "unknown categories", "categories": bad,
				"valid": webhook.AllCategories,
			})
			return
		}
		cats := normalizeCategories(in.Categories)
		maxA, tmo := 0, 0
		if in.MaxAttempts != nil {
			maxA = *in.MaxAttempts
		}
		if in.TimeoutMS != nil {
			tmo = *in.TimeoutMS
		}
		ep, err := st.CreateEndpoint(r.Context(), in.Name, in.URL, in.Secret, cats, maxA, tmo, 0)
		if err != nil {
			http.Error(w, "create webhook: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, ep.Public())
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleWebhookSub routes the /{api|admin}/webhooks/{id} subtree:
//
//	GET    {id}            -> one endpoint (secret omitted)
//	PATCH  {id}            -> partial update (name/url/categories/enabled)
//	DELETE {id}            -> delete
//	POST   {id}/replay     -> re-drive from {from_seq} (0 = from the start)
//	GET    {id}/events     -> journaled events for this endpoint (?after=&limit=&category=)
func (s *Server) handleWebhookSub(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	st := s.webhooks.Store()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "webhooks", "<id>", "replay"|"events"]
	if len(parts) < 3 || parts[1] != "webhooks" || parts[2] == "" {
		http.Error(w, "expected /webhooks/{id}[/replay|/events]", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "webhook id must be a positive integer", http.StatusBadRequest)
		return
	}

	// {id}/replay and {id}/events
	if len(parts) == 4 {
		switch parts[3] {
		case "replay":
			s.handleWebhookReplay(w, r, id)
		case "events":
			s.handleWebhookEvents(w, r, id)
		default:
			http.Error(w, "expected /webhooks/{id}/replay or /events", http.StatusNotFound)
		}
		return
	}
	if len(parts) != 3 {
		http.Error(w, "expected /webhooks/{id}[/replay|/events]", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		ep, err := st.Endpoint(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ep.Public())
	case http.MethodPatch:
		var in webhookRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		var name, urlp *string
		if in.Name != "" {
			name = &in.Name
		}
		if in.URL != "" {
			if !validWebhookURL(in.URL) {
				http.Error(w, "url must be an absolute http(s) URL", http.StatusBadRequest)
				return
			}
			urlp = &in.URL
		}
		var cats *[]string
		if in.Categories != nil {
			if bad := unknownCategories(in.Categories); len(bad) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "unknown categories", "categories": bad})
				return
			}
			n := normalizeCategories(in.Categories)
			cats = &n
		}
		ep, err := st.UpdateEndpoint(r.Context(), id, name, urlp, cats, in.Enabled)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ep.Public())
	case http.MethodDelete:
		if err := st.DeleteEndpoint(r.Context(), id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "GET, PATCH or DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleWebhookReplay resets an endpoint's cursor to re-drive a range of the
// journal.
//
//	POST /{api|admin}/webhooks/{id}/replay   {"from_seq": 0}
//
//	200 {webhook_id, from_seq, last_seq, status}
//	400 bad body / negative from_seq
//	404 unknown id
func (s *Server) handleWebhookReplay(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	st := s.webhooks.Store()
	var in struct {
		FromSeq int64 `json:"from_seq"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in) // body optional
	if in.FromSeq < 0 {
		http.Error(w, "from_seq must be >= 0", http.StatusBadRequest)
		return
	}
	if err := st.SetCursor(r.Context(), id, in.FromSeq); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	ep, err := st.Endpoint(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook_id": id, "from_seq": in.FromSeq, "last_seq": ep.LastSeq, "status": ep.Status,
	})
}

// handleWebhookEvents serves the journaled events an endpoint would have
// received (its categories), for catch-up / debugging.
//
//	GET /{api|admin}/webhooks/{id}/events?after=0&limit=200&category=
//
//	200 [event, ...] (oldest first)
//	404 unknown id
func (s *Server) handleWebhookEvents(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	st := s.webhooks.Store()
	ep, err := st.Endpoint(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	q := r.URL.Query()
	after := int64(0)
	if v := q.Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	cat := q.Get("category")
	evs, err := st.EventsAfter(r.Context(), after, cat, limit)
	if err != nil {
		http.Error(w, "events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Restrict to the categories this endpoint actually subscribes to.
	out := evs[:0]
	for i := range evs {
		if containsStr(ep.Categories, evs[i].Category) {
			out = append(out, evs[i])
		}
	}
	if out == nil {
		out = []webhook.Event{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEventStream is the live SSE subscription (W6-2's "SSE/subscription").
// It sends recent journal events as catch-up (honoring Last-Event-ID), then
// streams new events as they are journaled. Each frame is an SSE `data:` with
// the Envelope JSON and an `id:` of the journal seq (so a client can resume
// with Last-Event-ID).
//
//	GET /{api|admin}/events/stream[?category=]
//
//	200  text/event-stream (catch-up + live)
//	400  unknown category
//	503  webhook framework not wired / response can't flush
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	if s.webhooks == nil {
		http.Error(w, "webhook framework not configured", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusServiceUnavailable)
		return
	}
	category := r.URL.Query().Get("category")
	if category != "" && !containsStr(webhook.AllCategories, category) {
		http.Error(w, "unknown category "+category, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	// Subscribe FIRST so no journaled event is missed between the catch-up
	// read and the live loop (the seq-dedupe below collapses any overlap).
	ch, cancel := s.webhooks.AddLive(r.Context(), category)
	defer cancel()
	st := s.webhooks.Store()

	lastSent := int64(0)
	if lid := r.Header.Get("Last-Event-ID"); lid != "" {
		if n, err := strconv.ParseInt(lid, 10, 64); err == nil && n >= 0 {
			lastSent = n
		}
	}
	if lastSent == 0 {
		// No resume point: seed with the most recent events as context.
		if mx, err := st.MaxSeq(r.Context()); err == nil && mx > 0 {
			lastSent = mx - 200
			if lastSent < 0 {
				lastSent = 0
			}
		}
	}
	writeSSE := func(ev webhook.Event) {
		if ev.Seq <= lastSent {
			return
		}
		env := ev.Envelope()
		b, _ := json.Marshal(env)
		_, _ = w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\n"))
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		fl.Flush()
		lastSent = ev.Seq
	}

	catchUp, err := st.EventsAfter(r.Context(), lastSent, category, 200)
	if err == nil {
		for i := range catchUp {
			writeSSE(catchUp[i])
		}
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(ev)
		case <-keepalive.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			fl.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
