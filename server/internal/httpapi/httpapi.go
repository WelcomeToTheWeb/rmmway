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
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/baseline"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
	"github.com/welcometotheweb/rmmway/server/internal/store"
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

	jwtSecret     []byte
	tokenLifetime time.Duration
	// adminCaps (W3-3) is the capability set minted into every operator
	// session token; dispatching an action outside it is refused (403).
	adminCaps []string

	adminUser string
	adminSalt []byte
	adminHash []byte

	mintBootstrap func() (token, deviceID string)
	// dispatch mints + pushes a command to a device's live stream (W2-2).
	// Nil disables /api/devices/{id}/commands.
	dispatch func(deviceID string, action any) (commandID string, err error)
	// commandState (W3-3) serves the device's pending commands + recorded
	// results. Nil disables GET /{api|admin}/devices/{id}/commands.
	commandState func(deviceID string) (pending []*agentv1.Command, results []*agentv1.CommandResult)
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
	// Dispatch mints + pushes a command to a device's live stream (W2-2);
	// nil disables /api/devices/{id}/commands.
	Dispatch func(deviceID string, action any) (commandID string, err error)
	// CommandState serves GET {/api|/admin}/devices/{id}/commands (W3-3):
	// the device's pending commands + recorded results (nil disables).
	CommandState func(deviceID string) (pending []*agentv1.Command, results []*agentv1.CommandResult)
	// AdminCaps is the capability set granted to operator sessions
	// (W3-3); empty = the full Phase 1 set.
	AdminCaps []string
	// Baseline is the W2-3 dynamic baselining job; nil disables
	// /api/baseline/* and /admin/baseline/*.
	Baseline *store.Baseline
	// Alerts is the W2-4 deduped alert inbox; nil disables /api/alerts*
	// and /admin/alerts*.
	Alerts *store.AlertStore
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
		jwtSecret:     cfg.JWTSecret,
		tokenLifetime: cfg.TokenLifetime,
		adminUser:     cfg.AdminUser,
		adminSalt:     salt,
		adminHash:     pbkdf2.Key([]byte(cfg.AdminPassword), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New),
		adminCaps:     cfg.AdminCaps,
		mintBootstrap: cfg.MintBootstrap,
		dispatch:      cfg.Dispatch,
		commandState:  cfg.CommandState,
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
	mux.HandleFunc("/admin/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/admin/devices", s.deviceList) // open: installer / e2e / README
	mux.HandleFunc("/admin/search", s.handleSearch)
	mux.HandleFunc("/admin/baseline/anomalies", s.handleBaselineAnomalies) // open: e2e
	mux.HandleFunc("/admin/baseline/run", s.handleBaselineRun)             // open: e2e
	mux.HandleFunc("/admin/alerts", s.handleAlerts)                        // open: e2e
	mux.HandleFunc("/admin/alerts/", s.handleAlertSub)                     // open: e2e
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

// deviceSub routes the /{api|admin}/devices/{id}/... subtree (W2-2 + W3-3):
//
//	POST {id}/commands  — dispatch (auth-gated under /api, open under /admin)
//	GET  {id}/commands  — pending commands + recorded results (W3-3)
func (s *Server) deviceSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "devices", "<id>", "commands"]
	if len(parts) != 4 || parts[1] != "devices" || parts[2] == "" || parts[3] != "commands" {
		http.Error(w, "expected /devices/{id}/commands", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		s.deviceCommands(w, r, parts[2])
		return
	}
	s.dispatchCommand(w, r, parts[2])
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
