package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ---- MSP client/tenant routes (gap #2, wave 1, lane B) ---------------------
//
// The client/tenant model: devices belong to an MSP client, and the list
// views scope per client. A deployment starts with the seeded "Unassigned"
// default client (store.DefaultClientID) so the retrofit never strands a
// device — unassigned (NULL client_id) devices are always surfaced under
// it, in both the client's device list and the ?client= scoping.

const (
	maxClientName        = 128
	maxClientDescription = 2048
)

// registerClients mounts the client-registry routes (gap #2):
//
//	GET   /{api|admin}/clients               list + per-client fleet rollup
//	POST  /{api|admin}/clients               create (409 duplicate name)
//	PATCH /{api|admin}/clients/{id}          rename / description (404 unknown)
//	GET   /{api|admin}/clients/{id}/devices  the client's devices
//	PATCH /{api|admin}/devices/{id}/client   assign a device to a client
//
// /{api|admin}/devices/{id}/client is registered as a MORE SPECIFIC pattern
// than registerDevices' /{api|admin}/devices/ subtree (Go 1.22+ ServeMux
// serves the most specific match), so it wins before deviceSub sees it.
//
// Every route is operator-auth-gated and 503s when the client store is not
// wired (in-memory-mode deployments).
func registerClients(s *Server, mux *http.ServeMux) {
	for _, p := range []string{"/api", "/admin"} {
		mux.HandleFunc(p+"/clients", s.requireOperator(s.handleClients))
		mux.HandleFunc(p+"/clients/", s.requireOperator(s.handleClientsSub))
		mux.HandleFunc(p+"/devices/{id}/client", s.requireOperator(s.handleDeviceClient))
	}
}

func (s *Server) clientsUnwired(w http.ResponseWriter) {
	http.Error(w, "client model not configured (Postgres required)", http.StatusServiceUnavailable)
}

func validateClientName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}
	if len(name) > maxClientName {
		return "", errors.New("name too long (max " + strconv.Itoa(maxClientName) + " chars)")
	}
	return name, nil
}

// handleClients routes GET (list) and POST (create) on /{api|admin}/clients.
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.clientList(w, r)
	case http.MethodPost:
		s.createClient(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// clientList — GET /{api|admin}/clients
//
// The registry plus a per-client device-count rollup (computed from the
// device registry; unassigned devices count under the default client).
//
//	200  [Client{…, device_count}] name-ordered
//	503  client store not wired
func (s *Server) clientList(w http.ResponseWriter, r *http.Request) {
	if s.clients == nil {
		s.clientsUnwired(w)
		return
	}
	clients, err := s.clients.List(r.Context())
	if err != nil {
		http.Error(w, "client list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	devices, err := s.devices.List(r.Context())
	if err != nil {
		http.Error(w, "device rollup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	counts := make(map[string]int, len(devices))
	for _, d := range devices {
		if d.ClientID == "" {
			counts[store.DefaultClientID]++
		} else {
			counts[d.ClientID]++
		}
	}
	for _, c := range clients {
		c.DeviceCount = counts[c.ID]
	}
	writeJSON(w, http.StatusOK, clients)
}

// createClient — POST /{api|admin}/clients
//
// Body: {"name": string, "description"?: string}.
//
//	201  the created client (server-minted "clt-" id)
//	400  missing/oversized name, oversized description, bad body
//	409  name already taken
//	503  client store not wired
func (s *Server) createClient(w http.ResponseWriter, r *http.Request) {
	if s.clients == nil {
		s.clientsUnwired(w)
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, err := validateClientName(in.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	description := strings.TrimSpace(in.Description)
	if len(description) > maxClientDescription {
		http.Error(w, "description too long (max "+strconv.Itoa(maxClientDescription)+" chars)", http.StatusBadRequest)
		return
	}
	c, err := s.clients.Create(r.Context(), name, description)
	if err != nil {
		if errors.Is(err, store.ErrClientNameExists) {
			http.Error(w, "client name already exists", http.StatusConflict)
			return
		}
		http.Error(w, "create client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// handleClientsSub dispatches /{api|admin}/clients/{id}[…]:
//
//	/{id}          → patchClient
//	/{id}/devices  → clientDevices
func (s *Server) handleClientsSub(w http.ResponseWriter, r *http.Request) {
	rest := r.URL.Path
	rest = strings.TrimPrefix(rest, "/api/clients/")
	rest = strings.TrimPrefix(rest, "/admin/clients/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		s.patchClient(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "devices" {
		s.clientDevices(w, r, parts[0])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// patchClient — PATCH /{api|admin}/clients/{id}
//
// Body: {"name"?: string, "description"?: string} — at least one;
// omitted fields keep their value.
//
//	200  the updated client
//	400  bad body / empty patch / invalid name
//	404  unknown client
//	409  new name already taken
//	503  client store not wired
func (s *Server) patchClient(w http.ResponseWriter, r *http.Request, id string) {
	if s.clients == nil {
		s.clientsUnwired(w)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if in.Name == nil && in.Description == nil {
		http.Error(w, "name or description required", http.StatusBadRequest)
		return
	}
	if in.Name != nil {
		name, err := validateClientName(*in.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in.Name = &name
	}
	if in.Description != nil {
		desc := strings.TrimSpace(*in.Description)
		if len(desc) > maxClientDescription {
			http.Error(w, "description too long (max "+strconv.Itoa(maxClientDescription)+" chars)", http.StatusBadRequest)
			return
		}
		in.Description = &desc
	}
	c, err := s.clients.Update(r.Context(), id, in.Name, in.Description)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "unknown client", http.StatusNotFound)
		case errors.Is(err, store.ErrClientNameExists):
			http.Error(w, "client name already exists", http.StatusConflict)
		default:
			http.Error(w, "update client: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// clientDevices — GET /{api|admin}/clients/{id}/devices
//
// The devices assigned to the client; for the default client this also
// includes unassigned (NULL) devices.
//
//	200  [deviceOut…]
//	404  unknown client
//	503  client store not wired
func (s *Server) clientDevices(w http.ResponseWriter, r *http.Request, id string) {
	if s.clients == nil {
		s.clientsUnwired(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.clients.Get(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown client", http.StatusNotFound)
			return
		}
		http.Error(w, "client lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	list, err := s.devices.ListByClient(r.Context(), id)
	if err != nil {
		http.Error(w, "client devices: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := []deviceOut{}
	for _, d := range list {
		out = append(out, toDeviceOut(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeviceClient — PATCH /{api|admin}/devices/{id}/client
//
// Body: {"client_id": string|null} — assign the device to the client;
// null (or absent) moves it back to the default "Unassigned" client.
//
//	200  {"device": deviceOut} with the new client_id
//	400  bad body
//	404  unknown device or unknown client
//	503  client store not wired
func (s *Server) handleDeviceClient(w http.ResponseWriter, r *http.Request) {
	if s.clients == nil {
		s.clientsUnwired(w)
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	deviceID := r.PathValue("id")
	var in struct {
		ClientID *string `json:"client_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	clientID := ""
	if in.ClientID != nil {
		clientID = strings.TrimSpace(*in.ClientID)
	}
	if clientID != "" {
		if _, err := s.clients.Get(r.Context(), clientID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "unknown client", http.StatusNotFound)
				return
			}
			http.Error(w, "client lookup: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := s.devices.SetClient(r.Context(), deviceID, clientID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown device", http.StatusNotFound)
			return
		}
		http.Error(w, "set client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d, err := s.devices.Get(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": toDeviceOut(d)})
}
