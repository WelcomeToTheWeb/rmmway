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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

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
	devices store.DeviceStore
	search  *store.Meili

	jwtSecret     []byte
	tokenLifetime time.Duration

	adminUser string
	adminSalt []byte
	adminHash []byte

	mintBootstrap func() (token, deviceID string)
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
		jwtSecret:     cfg.JWTSecret,
		tokenLifetime: cfg.TokenLifetime,
		adminUser:     cfg.AdminUser,
		adminSalt:     salt,
		adminHash:     pbkdf2.Key([]byte(cfg.AdminPassword), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New),
		mintBootstrap: cfg.MintBootstrap,
	}
}

// Register mounts the operator + admin routes on mux. Call once at boot.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/devices", s.requireOperator(s.deviceList))
	mux.HandleFunc("/admin/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/admin/devices", s.deviceList) // open: installer / e2e / README
	mux.HandleFunc("/admin/search", s.handleSearch)
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
	tok, err := ingest.MintOperatorJWT(s.jwtSecret, s.tokenLifetime)
	if err != nil {
		http.Error(w, "mint token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":  tok,
		"expiry": time.Now().Add(s.tokenLifetime).UTC().Format(time.RFC3339),
	})
}

// requireOperator gates a handler behind a valid operator JWT.
func (s *Server) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !ingest.ParseOperatorJWT(s.jwtSecret, tok) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
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
