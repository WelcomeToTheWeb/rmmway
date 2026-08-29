package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	smtpoutbox "github.com/welcometotheweb/rmmway/server/internal/smtp"
)

// Reissuer re-issues the org root CA under the given organization name
// (implemented by *ca.Manager).
type Reissuer interface {
	ReissueRoot(ctx context.Context, orgName string) error
	// RootCertPEM is the current root cert (status reporting / e2e checks).
	RootCertPEM() []byte
}

// DeviceCounter reports the enrolled device count (the re-issue guard:
// once devices exist, their leaf certs are chained to the current root,
// so the wizard can no longer swap the root under them). main.go adapts
// store.DeviceStore (List) to this without the setup package importing store.
type DeviceCounter interface {
	Count(ctx context.Context) (int, error)
}

// NewService wires the wizard orchestration.
type Config struct {
	// Store persists the wizard state (required).
	Store Store
	// OrgCA re-issues the org root under the wizard's org name (required
	// for Complete; nil disables the CA part — in-memory-mode tests).
	OrgCA Reissuer
	// Devices guards the re-issue (0 enrolled = safe to re-issue).
	Devices DeviceCounter
	// OnReissued is called after a successful re-issue, so the caller can
	// refresh components that cache the root (the capability-token issuer).
	OnReissued func()
	// SendSMTP delivers outbox mail (defaults to the stdlib outbox; tests
	// inject the in-process sink).
	SendSMTP func(ctx context.Context, cfg smtpoutbox.Config, to, subject, body string) error
}

// Service runs the first-boot setup flow. A nil Store (no Postgres) means
// the wizard is unavailable — the server runs in degraded in-memory mode
// where the env admin credentials are the only login.
type Service struct {
	store   Store
	ca      Reissuer
	devices DeviceCounter
	onRe    func()
	send    func(ctx context.Context, cfg smtpoutbox.Config, to, subject, body string) error
}

func New(cfg Config) *Service {
	send := cfg.SendSMTP
	if send == nil {
		send = smtpoutbox.Send
	}
	return &Service{store: cfg.Store, ca: cfg.OrgCA, devices: cfg.Devices, onRe: cfg.OnReissued, send: send}
}

// Available reports whether the wizard can run (a backing store exists).
func (s *Service) Available() bool { return s != nil && s.store != nil }

// Done reports the persisted initialization state.
func (s *Service) Done(ctx context.Context) (bool, error) {
	if !s.Available() {
		return true, nil // no store -> nothing to initialize (env admin mode)
	}
	return s.store.Done(ctx)
}

// Status is the API's read model for GET /api/setup(/status).
type Status struct {
	Available       bool   `json:"available"`
	Setup           bool   `json:"setup"`
	OrgName         string `json:"org_name,omitempty"`
	AdminUser       string `json:"admin_user,omitempty"`
	SMTPHost        string `json:"smtp_host,omitempty"`
	SMTPConfigured  bool   `json:"smtp_configured"`
	DevicesEnrolled bool   `json:"devices_enrolled"`
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{Available: s.Available()}
	if !st.Available {
		st.Setup = true // treat degraded mode as "initialized" (env admin)
		return st, nil
	}
	stored, err := s.store.Load(ctx)
	if err != nil {
		return st, err
	}
	st.Setup = stored.Done
	st.OrgName = stored.OrgName
	st.AdminUser = stored.AdminUser
	st.SMTPHost = stored.SMTP.Host
	st.SMTPConfigured = stored.SMTPConfigured
	// Grandfather in deployments that predate the wizard: if devices are
	// already enrolled, the root CA is pinned by those agents and this is not a
	// "first boot". Treat it as set up so the wizard is not forced on it (the
	// env-admin fallback login still works). Best-effort: if the count query
	// fails we fall back to the persisted row only.
	if s.devices != nil {
		if n, err := s.devices.Count(ctx); err == nil {
			st.DevicesEnrolled = n > 0
			if !st.Setup {
				st.Setup = n > 0
			}
		}
	}
	return st, nil
}

// ValidateRequest checks the wizard payload before anything is persisted.
func ValidateRequest(req CompleteRequest) error {
	u := strings.TrimSpace(req.AdminUser)
	if u == "" {
		return fmt.Errorf("admin_user is required")
	}
	if len(u) < 3 || len(u) > 64 {
		return fmt.Errorf("admin_user must be 3-64 characters")
	}
	if strings.ContainsAny(u, " \t\n") {
		return fmt.Errorf("admin_user must not contain spaces")
	}
	if len(req.AdminPassword) < 8 {
		return fmt.Errorf("admin_password must be at least 8 characters")
	}
	if len(req.AdminPassword) > 128 {
		return fmt.Errorf("admin_password must be at most 128 characters")
	}
	if strings.TrimSpace(req.OrgName) == "" {
		return fmt.Errorf("org_name is required (it is stamped into the org root CA)")
	}
	if len(req.OrgName) > 128 {
		return fmt.Errorf("org_name must be at most 128 characters")
	}
	if _, err := req.SMTP.Normalize(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

// Complete runs the wizard flow, in this order:
//
//  1. validation (the request + "not already complete" + "no devices yet");
//  2. re-issue the org root under the org name (the CA is the one piece of
//     state that must NOT half-exist: if this fails nothing is persisted
//     and the operator can retry with the same values);
//  3. refresh the capability-token issuer (OnReissued) so every token
//     minted from here on is signed by the new root;
//  4. persist done + admin + org_name + smtp in ONE transaction.
func (s *Service) Complete(ctx context.Context, req CompleteRequest) error {
	if !s.Available() {
		return fmt.Errorf("setup is unavailable (no database)")
	}
	if err := ValidateRequest(req); err != nil {
		return err
	}
	req.AdminUser = strings.TrimSpace(req.AdminUser)
	req.OrgName = strings.TrimSpace(req.OrgName)
	var err error
	if req.SMTP, err = req.SMTP.Normalize(); err != nil {
		return err
	}

	done, err := s.store.Done(ctx)
	if err != nil {
		return err
	}
	if done {
		return ErrAlreadyComplete
	}

	// The re-issue guard: leaves already exist when devices are enrolled,
	// and swapping the root would orphan them (their mTLS + capability
	// tokens chain to the old root).
	if s.devices != nil {
		n, err := s.devices.Count(ctx)
		if err != nil {
			return fmt.Errorf("device count: %w", err)
		}
		if n > 0 {
			return ErrDevicesEnrolled
		}
	}

	if s.ca != nil {
		if err := s.ca.ReissueRoot(ctx, req.OrgName); err != nil {
			return fmt.Errorf("org CA re-issue: %w", err)
		}
		if s.onRe != nil {
			s.onRe()
		}
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	hash := HashPassword(req.AdminPassword, salt)
	if err := s.store.Complete(ctx, req, salt, hash); err != nil {
		if err == ErrAlreadyComplete {
			return err
		}
		return fmt.Errorf("persist setup: %w", err)
	}
	return nil
}

// CheckCredentials verifies operator login against the wizard-minted
// account (false = not a DB admin; the caller falls back to the env pair).
func (s *Service) CheckCredentials(ctx context.Context, username, password string) bool {
	if !s.Available() {
		return false
	}
	return VerifyAdmin(ctx, s.store, username, password)
}

// TestSMTP sends the outbox verification mail (the wizard's test button).
func (s *Service) TestSMTP(ctx context.Context, cfg smtpoutbox.Config, to string) error {
	cfg, err := cfg.Normalize()
	if err != nil {
		return err
	}
	if !cfg.IsConfigured() {
		return fmt.Errorf("smtp outbox not configured (host is required)")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body := "This is a test message from the RMMWay SMTP outbox.\n" +
		"If you can read this, the outbox is configured correctly and\n" +
		"the server can send mail through " + cfg.Host + ":" + fmt.Sprint(cfg.Port) + ".\n"
	return s.send(ctx, cfg, to, "RMMWay: SMTP outbox test", body)
}

// HashPassword hashes a password with PBKDF2-SHA256 (100k iterations,
// 32-byte output) — the same params the env admin uses (httpapi).
func HashPassword(password string, salt []byte) []byte {
	return pbkdf2.Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New)
}

// VerifyPassword constant-time compares a password against a stored
// (salt, hash) pair.
func VerifyPassword(password string, salt, hash []byte) bool {
	candidate := HashPassword(password, salt)
	return subtle.ConstantTimeCompare(candidate, hash) == 1
}
