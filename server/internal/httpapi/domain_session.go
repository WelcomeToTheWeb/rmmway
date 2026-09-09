// Package httpapi: domain_session.go implements gap #1a's remote session
// operator API: start/stop a session on a device, stream frames to a
// browser viewer via SSE, and download files pulled via file_pull.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/sessionrelay"
)

// handleSessionStart opens a remote session on a device.
//
//	POST /api/devices/{id}/session/start {"fps":2}
//
//	200 {session_id, fps, device_id}
//	400 bad body / unknown fps range
//	403 session lacks role
//	404 unknown device
//	502 device has no live stream (offline)
//	503 session relay not wired (in-memory mode)
func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request, deviceID string) {
	if s.sessions == nil {
		http.Error(w, "session relay not configured", http.StatusServiceUnavailable)
		return
	}
	if s.sendSessionControl == nil {
		http.Error(w, "session control not configured", http.StatusServiceUnavailable)
		return
	}

	// Parse optional fps (default 0 = agent default).
	var body struct {
		FPS *int `json:"fps"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	fps := 0
	if body.FPS != nil {
		fps = *body.FPS
		if fps < 0 || fps > 30 {
			http.Error(w, "fps must be 0-30 (0 = default)", http.StatusBadRequest)
			return
		}
	}

	// Generate a session ID.
	sessionID := fmt.Sprintf("sess-%d-%d", time.Now().UnixNano(), randInt31())

	// Register in the session relay so viewers can subscribe.
	s.sessions.Open(deviceID, sessionID, fps)

	// Send the open control downlink to the agent.
	sc := &agentv1.SessionControl{
		Action: &agentv1.SessionControl_Open{
			Open: &agentv1.SessionControl_OpenSession{
				SessionId: sessionID,
				Fps:       int32(fps),
			},
		},
	}
	if !s.sendSessionControl(deviceID, sc) {
		s.sessions.Close(deviceID, sessionID)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "device is offline (no live stream)",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"fps":        fps,
		"device_id":  deviceID,
	})
}

// handleSessionStop closes a remote session on a device.
//
//	POST /api/devices/{id}/session/stop {"session_id":"sess-..."}
//
//	200 {stopped: true}
//	404 unknown device / no active session
//	502 device has no live stream
//	503 session relay not wired
func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request, deviceID string) {
	if s.sessions == nil {
		http.Error(w, "session relay not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		SessionID string `json:"session_id"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}

	// Look up the active session ID from the relay.
	activeID, hasActive := s.sessions.ActiveSession(deviceID)
	if !hasActive && body.SessionID == "" {
		http.Error(w, "no active session for device", http.StatusNotFound)
		return
	}
	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = activeID
	}

	// Send the close control downlink.
	sc := &agentv1.SessionControl{
		Action: &agentv1.SessionControl_Close{
			Close: &agentv1.SessionControl_CloseSession{
				SessionId: sessionID,
			},
		},
	}
	if s.sendSessionControl != nil && !s.sendSessionControl(deviceID, sc) {
		// Agent is offline; mark session as closed locally.
		s.sessions.Close(deviceID, sessionID)
		writeJSON(w, http.StatusOK, map[string]any{
			"stopped": false,
			"error":   "device is offline; session marked closed locally",
		})
		return
	}

	s.sessions.Close(deviceID, sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"stopped":     true,
		"session_id":  sessionID,
		"device_id":   deviceID,
	})
}

// handleSessionStream streams session frames to a browser viewer via SSE.
//
//	GET /api/devices/{id}/session/stream
//
//	200 text/event-stream (frames until disconnected)
//	404 unknown device
//	503 session relay not wired
func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request, deviceID string) {
	if s.sessions == nil {
		http.Error(w, "session relay not configured", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	viewer, cancel := s.sessions.Subscribe(deviceID)
	defer cancel()

	// Send initial status if available.
	if status := s.sessions.Status(deviceID); status != "" {
		evt := sessionrelay.FrameEvent{Kind: "status", Status: status}
		s.writeSSEEvent(w, flusher, "status", evt)
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-viewer.Done():
			return
		case evt, ok := <-viewer.Chan():
			if !ok {
				return
			}
			s.writeSSEEvent(w, flusher, evt.Kind, evt)
		}
	}
}

// writeSSEEvent writes one SSE event frame.
func (s *Server) writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, kind string, evt sessionrelay.FrameEvent) {
	data, _ := json.Marshal(evt)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, string(data))
	flusher.Flush()
}

// handleFileDownload serves a file that was pulled via file_pull.
//
//	GET /api/devices/{id}/session/files/{command_id}
//
//	200 file bytes (with Content-Disposition)
//	202 transfer in progress
//	404 unknown command / transfer not found
//	503 session relay not wired
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request, deviceID, commandID string) {
	if s.sessions == nil {
		http.Error(w, "session relay not configured", http.StatusServiceUnavailable)
		return
	}

	t, ok := s.sessions.PullState(commandID)
	if !ok {
		http.Error(w, "transfer not found", http.StatusNotFound)
		return
	}

	if !t.Done {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"state":    "in_progress",
			"received": t.Received,
			"total":    t.Total,
		})
		return
	}

	if t.Err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": t.Err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(t.Path)))
	w.Header().Set("Content-Length", strconv.FormatInt(t.Received, 10))
	w.Write(t.Data)
}

// randInt31 returns a random int31.
func randInt31() int32 {
	return int32(time.Now().UnixNano() >> 32)
}

// registerSession registers the remote session routes.
func registerSession(s *Server, mux *http.ServeMux) {
	// POST /api/devices/{id}/session/start (admin/tech)
	mux.HandleFunc("/api/devices/{id}/session/start", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStart(w, r, id)
		}, "admin", "tech"))

	// POST /api/devices/{id}/session/stop (admin/tech)
	mux.HandleFunc("/api/devices/{id}/session/stop", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStop(w, r, id)
		}, "admin", "tech"))

	// GET /api/devices/{id}/session/stream (any authenticated operator)
	mux.HandleFunc("/api/devices/{id}/session/stream", s.rbacGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStream(w, r, id)
		}))

	// GET /api/devices/{id}/session/files/{command_id} (admin/tech)
	mux.HandleFunc("/api/devices/{id}/session/files/{command_id}", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			cmd := r.PathValue("command_id")
			s.handleFileDownload(w, r, id, cmd)
		}, "admin", "tech"))

	// Mirror under /admin prefix.
	mux.HandleFunc("/admin/devices/{id}/session/start", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStart(w, r, id)
		}, "admin", "tech"))

	mux.HandleFunc("/admin/devices/{id}/session/stop", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStop(w, r, id)
		}, "admin", "tech"))

	mux.HandleFunc("/admin/devices/{id}/session/stream", s.rbacGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			s.handleSessionStream(w, r, id)
		}))

	mux.HandleFunc("/admin/devices/{id}/session/files/{command_id}", s.rbacRoleGate(
		func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			cmd := r.PathValue("command_id")
			s.handleFileDownload(w, r, id, cmd)
		}, "admin", "tech"))
}
