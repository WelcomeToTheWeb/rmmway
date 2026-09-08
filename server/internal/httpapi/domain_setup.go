package httpapi

import (
	"net/http"
)

// registerSetup mounts the first-boot wizard routes (A-2) plus the open /api/public-url read the Add Device UI uses.
func registerSetup(s *Server, mux *http.ServeMux) {
	// Public URL configuration. Open (no auth) — the Add Device UI reads this
	// to prefill the server URL field so agents enroll to the correct target.
	mux.HandleFunc("/api/public-url", s.handlePublicURL)
	// A-2: first-boot setup wizard. /api/setup/status is always open (the UI
	// needs it to decide between wizard and login, pre-auth); the POST routes
	// are open only while the server is uninitialized, then operator-gated.
	mux.HandleFunc("/api/setup/status", s.handleSetupStatus)
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/api/setup/complete", s.setupGate(s.handleSetupComplete))
	mux.HandleFunc("/api/setup/smtp/test", s.setupGate(s.handleSetupSMTPTest))
}
