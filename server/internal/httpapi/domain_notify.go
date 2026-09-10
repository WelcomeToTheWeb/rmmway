package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/notify"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- notification routes (gap #6, wave 3, lane B) --------------------------

// registerNotify mounts the notification routes:
//
//	GET   /{api|admin}/notify/channels          list channels
//	POST  /{api|admin}/notify/channels          create channel
//	PATCH /{api|admin}/notify/channels/{id}     update channel
//	POST  /{api|admin}/notify/channels/{id}/test test channel
//	GET   /{api|admin}/notify/policies          list policies
//	POST  /{api|admin}/notify/policies          create policy
//	PATCH /{api|admin}/notify/policies/{id}     update policy
//	POST  /{api|admin}/notify/policies/test     test-fire a policy
func registerNotify(s *Server, mux *http.ServeMux) {
	for _, p := range []string{"/api", "/admin"} {
		mux.HandleFunc(p+"/notify/channels", s.rbacGate(s.handleNotifyChannels))
		mux.HandleFunc(p+"/notify/channels/", s.rbacGate(s.handleNotifyChannelsSub))
		mux.HandleFunc(p+"/notify/policies", s.rbacGate(s.handleNotifyPolicies))
		mux.HandleFunc(p+"/notify/policies/", s.rbacGate(s.handleNotifyPoliciesSub))
	}
}

func (s *Server) notifyUnwired(w http.ResponseWriter) {
	http.Error(w, "notification system not configured", http.StatusServiceUnavailable)
}

// handleNotifyChannels routes GET (list) and POST (create) on channels.
func (s *Server) handleNotifyChannels(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		s.notifyUnwired(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.notifyChannelList(w, r)
	case http.MethodPost:
		s.notifyChannelCreate(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) notifyChannelList(w http.ResponseWriter, r *http.Request) {
	channels, err := s.notifyStore.ListChannels(r.Context())
	if err != nil {
		http.Error(w, "list channels: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, channels)
}

func (s *Server) notifyChannelCreate(w http.ResponseWriter, r *http.Request) {
	if !requireRole(w, r, "admin") {
		return
	}
	var in struct {
		Type    string                 `json:"type"`
		Name    string                 `json:"name"`
		Config  map[string]interface{} `json:"config"`
		Enabled bool                   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
	}
	if in.Type == "" {
		http.Error(w, "type is required", http.StatusBadRequest)
	}
	cfg := &notify.ChannelConfig{
		Type:    notify.ChannelType(in.Type),
		Name:    in.Name,
		Config:  in.Config,
		Enabled: in.Enabled,
	}
	created, err := s.notifyStore.CreateChannel(r.Context(), cfg)
	if err != nil {
		http.Error(w, "create channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleNotifyChannelsSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/notify/channels/")
	rest = strings.TrimPrefix(rest, "/admin/notify/channels/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		s.notifyChannelUpdate(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "test" {
		s.notifyChannelTest(w, r, parts[0])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) notifyChannelUpdate(w http.ResponseWriter, r *http.Request, id string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var in struct {
		Name    string                 `json:"name"`
		Config  map[string]interface{} `json:"config"`
		Enabled *bool                  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := s.notifyStore.GetChannel(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		http.Error(w, "get channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if in.Name != "" {
		cfg.Name = in.Name
	}
	if in.Config != nil {
		cfg.Config = in.Config
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	updated, err := s.notifyStore.UpdateChannel(r.Context(), cfg)
	if err != nil {
		http.Error(w, "update channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) notifyChannelTest(w http.ResponseWriter, r *http.Request, id string) {
	cfg, err := s.notifyStore.GetChannel(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}
		http.Error(w, "get channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.notifySender == nil {
		s.notifyUnwired(w)
		return
	}
	ch, err := s.notifySender.For(cfg)
	if err != nil {
		http.Error(w, "create channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err = ch.Test(r.Context())
	if err != nil {
		http.Error(w, "test failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleNotifyPolicies routes GET (list) and POST (create) on policies.
func (s *Server) handleNotifyPolicies(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		s.notifyUnwired(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.notifyPolicyList(w, r)
	case http.MethodPost:
		s.notifyPolicyCreate(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) notifyPolicyList(w http.ResponseWriter, r *http.Request) {
	policies, err := s.notifyStore.ListPolicies(r.Context())
	if err != nil {
		http.Error(w, "list policies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, policies)
}

func (s *Server) notifyPolicyCreate(w http.ResponseWriter, r *http.Request) {
	if !requireRole(w, r, "admin") {
		return
	}
	var in struct {
		Category string   `json:"category"`
		ClientID *string  `json:"client_id"`
		Role     *string  `json:"role"`
		Channels []string `json:"channels"`
		Enabled  bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Category == "" {
		http.Error(w, "category is required", http.StatusBadRequest)
	}
	policy := &store.NotificationPolicy{
		Category: in.Category,
		ClientID: in.ClientID,
		Role:     in.Role,
		Channels: in.Channels,
		Enabled:  true,
	}
	created, err := s.notifyStore.CreatePolicy(r.Context(), policy)
	if err != nil {
		http.Error(w, "create policy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleNotifyPoliciesSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/notify/policies/")
	rest = strings.TrimPrefix(rest, "/admin/notify/policies/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		s.notifyPolicyUpdate(w, r, parts[0])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) notifyPolicyUpdate(w http.ResponseWriter, r *http.Request, id string) {
	if !requireRole(w, r, "admin") {
		return
	}
	var in struct {
		Category string   `json:"category"`
		ClientID *string  `json:"client_id"`
		Role     *string  `json:"role"`
		Channels []string `json:"channels"`
		Enabled  *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	policy, err := s.notifyStore.GetPolicy(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrPolicyNotFound) {
			http.Error(w, "policy not found", http.StatusNotFound)
			return
		}
		http.Error(w, "get policy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if in.Category != "" {
		policy.Category = in.Category
	}
	policy.ClientID = in.ClientID
	policy.Role = in.Role
	if in.Channels != nil {
		policy.Channels = in.Channels
	}
	if in.Enabled != nil {
		policy.Enabled = *in.Enabled
	}
	updated, err := s.notifyStore.UpdatePolicy(r.Context(), id, policy)
	if err != nil {
		http.Error(w, "update policy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
