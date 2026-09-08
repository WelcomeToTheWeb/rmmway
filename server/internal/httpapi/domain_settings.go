// C #10a: the Settings/Profile page backend — operator-visible server
// settings reachable after first boot (the wizard in domain_setup.go is the
// once-in-a-lifetime surface; this is the recurring one):
//
//	GET  /{api|admin}/settings                 the read model (org name,
//	                                          SMTP config minus password,
//	                                          profile + MFA status)
//	PATCH /{api|admin}/settings                save the SMTP outbox config
//	POST /{api|admin}/settings/smtp/test       send a test mail using the
//	                                          STORED config
//	POST /{api|admin}/settings/profile/password
//	                                          change the current operator's
//	                                          password
//
// All routes are operator-gated (requireOperator) — unlike the wizard's
// pre-initialization window, settings are only reachable with a session.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"golang.org/x/crypto/pbkdf2"

	"github.com/welcometotheweb/rmmway/server/internal/setup"
	"github.com/welcometotheweb/rmmway/server/internal/smtp"
)

func registerSettings(s *Server, mux *http.ServeMux) {
	gated := s.requireOperator(s.handleSettings)
	mux.HandleFunc("/api/settings", gated)
	mux.HandleFunc("/admin/settings", gated)
	test := s.requireOperator(s.handleSettingsSMTPTest)
	mux.HandleFunc("/api/settings/smtp/test", test)
	mux.HandleFunc("/admin/settings/smtp/test", test)
	password := s.requireOperator(s.handleSettingsProfilePassword)
	mux.HandleFunc("/api/settings/profile/password", password)
	mux.HandleFunc("/admin/settings/profile/password", password)
}

// settingsSMTPOut is the SMTP read model: the stored config with the
// password redacted — pass_set tells the UI whether one is on file (the
// form then pre-fills "leave blank to keep the current password").
type settingsSMTPOut struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	From       string `json:"from"`
	Username   string `json:"username"`
	PassSet    bool   `json:"pass_set"`
	Configured bool   `json:"configured"`
}

func settingsSMTPReadModel(c smtp.Config, passSet bool) settingsSMTPOut {
	norm, _ := c.Normalize()
	return settingsSMTPOut{
		Host:       norm.Host,
		Port:       norm.Port,
		From:       norm.From,
		Username:   norm.Username,
		PassSet:    passSet,
		Configured: norm.IsConfigured(),
	}
}

// currentUsername resolves the account the settings page speaks for: the
// wizard-minted DB admin when one exists (login checks it first), otherwise
// the env admin. The operator JWT itself is subject-less ("operator"), so
// the session cannot carry the username — this mirrors the login order in
// handleLogin.
func (s *Server) currentUsername(ctx context.Context) (string, setup.Stored, error) {
	stored := setup.Stored{}
	if s.setup != nil {
		var err error
		if stored, err = s.setup.Load(ctx); err != nil {
			return "", stored, err
		}
	}
	if stored.Done && stored.AdminUser != "" {
		return stored.AdminUser, stored, nil
	}
	return s.adminUser, stored, nil
}

// handleSettings serves GET (read model) and PATCH (save SMTP config).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSettingsGet(w, r)
	case http.MethodPatch:
		s.handleSettingsPatch(w, r)
	default:
		http.Error(w, "GET or PATCH only", http.StatusMethodNotAllowed)
	}
}

// handleSettingsGet reports the operator-visible settings.
//
//	200 { org_name, smtp: {host, port, from, username, pass_set,
//	      configured}, profile: {username, mfa_enabled} }
//
// mfa_enabled is pinned to false: TOTP MFA lands in wave 2 (B #3) and
// flips this from its own lane — the UI renders the row as "not yet
// enabled" meanwhile.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	username, stored, err := s.currentUsername(r.Context())
	if err != nil {
		http.Error(w, "settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org_name": stored.OrgName,
		"smtp":     settingsSMTPReadModel(stored.SMTP, stored.SMTP.Password != ""),
		"profile": map[string]any{
			"username":    username,
			"mfa_enabled": false,
		},
	})
}

// handleSettingsPatch persists the SMTP outbox config.
//
//	200 { ok, smtp: <sanitized read model> }
//	400 invalid config (smtp.Config.Normalize)
//	503 no database (in-memory mode)
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	if s.setup == nil {
		http.Error(w, "settings require a database (in-memory mode)", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		SMTP smtp.Config `json:"smtp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.setup.SaveSMTP(r.Context(), in.SMTP); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"smtp": settingsSMTPReadModel(in.SMTP, in.SMTP.Password != ""),
	})
}

// settingsSMTPTestRequest carries an optional recipient (defaults to the
// stored From address). The config itself comes from the STORED outbox —
// the point of the settings test is "does what is saved actually send".
type settingsSMTPTestRequest struct {
	To string `json:"to"`
}

// handleSettingsSMTPTest sends the verification mail via the stored config.
//
//	200 { ok, to }
//	400 bad body (a missing/empty body is fine — it means "send to the
//	    default recipient", e.g. `curl -X POST`)
//	502 the SMTP server refused / was unreachable / not configured
//	503 no database (in-memory mode)
func (s *Server) handleSettingsSMTPTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.setup == nil {
		http.Error(w, "settings require a database (in-memory mode)", http.StatusServiceUnavailable)
		return
	}
	var in settingsSMTPTestRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, stored, err := s.currentUsername(r.Context())
	if err != nil {
		http.Error(w, "settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.setup.TestSMTP(r.Context(), stored.SMTP, in.To); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cfg, _ := stored.SMTP.Normalize()
	to := in.To
	if to == "" {
		to = cfg.From
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": to})
}

// settingsPasswordRequest is the change-password payload.
type settingsPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleSettingsProfilePassword changes the current operator's password.
// The account resolved is the same one login checks first (the DB admin
// when the wizard ran, else the env admin — a change in that case MINTS a
// DB row so the new password wins on the next login; the env fallback
// keeps working with the old pair, same quirk as the wizard's grandfather
// path).
//
//	200 { ok }
//	400 wrong current password / new password length
//	503 no database (in-memory mode)
func (s *Server) handleSettingsProfilePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.setup == nil {
		http.Error(w, "settings require a database (in-memory mode)", http.StatusServiceUnavailable)
		return
	}
	var in settingsPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(in.NewPassword) < 8 || len(in.NewPassword) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "new password must be 8-128 characters",
		})
		return
	}
	username, stored, err := s.currentUsername(r.Context())
	if err != nil {
		http.Error(w, "settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if stored.Done && stored.AdminUser != "" {
		if !s.setup.CheckCredentials(r.Context(), username, in.CurrentPassword) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "current password is incorrect",
			})
			return
		}
	} else {
		// Env-admin session: constant-time compare against the per-boot
		// env credential (the same material handleLogin falls back to).
		candidate := pbkdf2.Key([]byte(in.CurrentPassword), s.adminSalt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
		if subtle.ConstantTimeCompare(candidate, s.adminHash) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "current password is incorrect",
			})
			return
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		http.Error(w, "generate salt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.setup.UpdatePassword(r.Context(), username, salt, setup.HashPassword(in.NewPassword, salt)); err != nil {
		http.Error(w, "password update: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
