// Gap #4: deep inventory query API.
package httpapi

import (
	"net/http"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// handleDeviceInventory queries the inventory for a specific device.
//
// GET /api/devices/{id}/inventory
//
// 200 {hardware: {...}, software: [...], collected_at: "..."}
// 404 unknown device
// 400 bad request
func (s *Server) handleDeviceInventory(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify device exists
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}

	// Query hardware inventory
	hardware, err := s.devices.GetDeviceHardware(r.Context(), deviceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query hardware: " + err.Error()})
		return
	}

	// Query software inventory
	software, err := s.devices.GetDeviceSoftware(r.Context(), deviceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query software: " + err.Error()})
		return
	}

	// If hardware has collected_at, include it in the response
	collectedAt := ""
	if hw, ok := hardware["collected_at"]; ok {
		if t, ok := hw.(time.Time); ok {
			collectedAt = t.Format(time.RFC3339)
		}
	}

	resp := map[string]any{
		"device_id":    deviceID,
		"hardware":     hardware,
		"software":     software,
		"collected_at": collectedAt,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleTriggerInventoryCollect triggers an inventory collection on a device.
//
// POST /api/devices/{id}/inventory/collect
//
// 200 {command_id: "..."}
// 404 unknown device
func (s *Server) handleTriggerInventoryCollect(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.dispatch == nil {
		http.Error(w, "command dispatch not configured", http.StatusServiceUnavailable)
		return
	}

	// Verify device exists
	ok, err := s.devices.Contains(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}

	// Build and dispatch the inventory collection command
	action := &agentv1.Command_CollectInventory{CollectInventory: &agentv1.CollectInventory{}}
	cmdID, err := s.dispatch(deviceID, action)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"command_id": cmdID,
		"device_id":  deviceID,
	})
}