// A-2: the first-boot setup wizard endpoints.
//
//	GET  /api/setup/status   open — {available, setup, org_name?, admin_user?,
//	                           smtp_configured}. available=false means the
//	                           server runs in-memory (no Postgres): the UI
//	                           skips the wizard and uses the env admin login.
//	POST /api/setup/complete open BEFORE setup (409 after) — mints the root
//	                           admin, re-issues the org CA under the org name,
//	                           and persists the SMTP outbox config.
//	POST /api/setup/smtp/test open before setup, operator-auth after —
//	                           sends a verification mail.
//	GET  /api/setup          open — the stored config (never the password).
//
// Gating: once setup is complete, POST routes require a valid operator JWT
// (the wizard is a once-in-a-lifetime surface; keeping it reachable without
// auth afterwards would be an open door).
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/smtp"
)

// setupGate passes the request through while the server is uninitialized
// (the wizard's window) and requires an operator JWT afterwards.
func (s *Server) setupGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.setup == nil {
			http.Error(w, "setup is unavailable (no database)", http.StatusServiceUnavailable)
			return
		}
		done, err := s.setup.Done(r.Context())
		if err != nil {
			http.Error(w, "setup status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if done {
			s.requireOperator(next)(w, r)
			return
		}
		next(w, r)
	}
}

// handleSetupStatus reports the initialization state. Always 200: a fresh
// database answers {available:true, setup:false}, a running one {setup:true},
// and in-memory mode {available:false, setup:true} (the UI skips the wizard).
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	st := setup.Status{Available: false, Setup: true}
	if s.setup != nil {
		out, err := s.setup.Status(r.Context())
		if err != nil {
			http.Error(w, "setup status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		st = out
	}
	writeJSON(w, http.StatusOK, st)
}

// handleSetup reads back the stored wizard choices (the password is never
// returned — it only exists in hashed form). Before setup there is nothing
// stored, so it reports the not-initialized status.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	st := setup.Status{Available: false, Setup: true}
	if s.setup != nil {
		out, err := s.setup.Status(r.Context())
		if err != nil {
			http.Error(w, "setup status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		st = out
	}
	writeJSON(w, http.StatusOK, st)
}

// setupCompleteRequest is the wizard's payload.
type setupCompleteRequest struct {
	AdminUser     string      `json:"admin_user"`
	AdminPassword string      `json:"admin_password"`
	OrgName       string      `json:"org_name"`
	SMTP          smtp.Config `json:"smtp"`
}

// handleSetupComplete runs the one-time initialization.
//
//	200 {setup:true, admin_user, org_name, smtp_configured}
//	400 validation
//	409 already complete / devices enrolled
//	500 CA re-issue or persistence failure (nothing persisted — retry ok)
func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.setup == nil {
		http.Error(w, "setup is unavailable (no database)", http.StatusServiceUnavailable)
		return
	}
	var in setupCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req := setup.CompleteRequest{
		AdminUser:     in.AdminUser,
		AdminPassword: in.AdminPassword,
		OrgName:       in.OrgName,
		SMTP:          in.SMTP,
	}
	if err := s.setup.Complete(r.Context(), req); err != nil {
		switch {
		case errors.Is(err, setup.ErrAlreadyComplete):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "setup is already complete"})
		case errors.Is(err, setup.ErrDevicesEnrolled):
			writeJSON(w, http.StatusConflict, map[string]string{"error": setup.ErrDevicesEnrolled.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	st, err := s.setup.Status(r.Context())
	if err != nil {
		http.Error(w, "setup status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// smtpTestRequest carries the outbox config to try + an optional recipient
// (defaults to the configured From address).
type smtpTestRequest struct {
	SMTP smtp.Config `json:"smtp"`
	To   string      `json:"to"`
}

// handleSetupSMTPTest sends the outbox verification mail.
//
//	200 {ok:true, to}
//	400 bad config
//	502 the SMTP server refused / was unreachable
func (s *Server) handleSetupSMTPTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.setup == nil {
		http.Error(w, "setup is unavailable (no database)", http.StatusServiceUnavailable)
		return
	}
	var in smtpTestRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.setup.TestSMTP(r.Context(), in.SMTP, in.To); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cfg, _ := in.SMTP.Normalize()
	to := in.To
	if to == "" {
		to = cfg.From
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": to})
}

// handlePublicURL returns the configured public operator URL (RMMWAY_PUBLIC_URL).
// Open (no auth) — the Add Device UI reads this to prefill the server URL field.
//
// GET /api/public-url
//
//	200 {url: "https://rmm.example.com"} when set
//	200 {url: ""} when unset (UI falls back to window.location.origin)
func (s *Server) handlePublicURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": s.publicURL})
}
