package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/caps"
)

// ---- W4-2: signed agent release distribution --------------------------------

// handleReleasesLatest serves the current release manifest. The agent
// compares its public_key to the pinned key and verifies each asset's
// signature before installing — the server is a carrier, not a trust anchor.
func (s *Server) handleReleasesLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m, err := s.releases.Manifest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleReleaseAsset serves one asset (binary or .minisig) by name, limited
// to what the current manifest allows.
func (s *Server) handleReleaseAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/agent/releases/latest/")
	p, err := s.releases.AssetPath(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, p)
}

// ---- operator auth ----------------------------------------------------------

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
	Action     string   `json:"action"`
	Lang       string   `json:"lang"`
	Script     string   `json:"script"` // base64
	Args       []string `json:"args"`
	TimeoutS   int32    `json:"timeout_s"`
	Path       string   `json:"path"`        // gap #1a: file path for pull/push
	ContentB64 string   `json:"content_b64"` // gap #1a: inline content for push
	Mode       string   `json:"mode"`        // gap #1a: octal mode for push
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
	case "file_pull": // gap #1a
		if in.Path == "" {
			return nil, fmt.Errorf("file_pull requires path")
		}
		return &agentv1.Command_FilePull{FilePull: &agentv1.FilePull{
			Path: in.Path,
		}}, nil
	case "file_push": // gap #1a
		if in.Path == "" {
			return nil, fmt.Errorf("file_push requires path")
		}
		push := &agentv1.FilePush{Path: in.Path}
		if in.ContentB64 != "" {
			if _, err := base64.StdEncoding.DecodeString(in.ContentB64); err != nil {
				return nil, fmt.Errorf("file_push content_b64 must be valid base64: %v", err)
			}
			push.ContentB64 = in.ContentB64
		}
		if in.Mode != "" {
			push.Mode = in.Mode
		}
		return &agentv1.Command_FilePush{FilePush: push}, nil
	case "collect_inventory": // gap #4
		return &agentv1.Command_CollectInventory{CollectInventory: &agentv1.CollectInventory{}}, nil
	case "patch_query": // gap #4
		return &agentv1.Command_PatchQuery{PatchQuery: &agentv1.PatchQuery{
			SeverityFilter: in.Path, // severity filter via path field
		}}, nil
	case "patch_apply": // gap #4
		patchIDs := []string{}
		if in.Script != "" {
			// patch_ids passed as base64 JSON array
			decoded, err := base64.StdEncoding.DecodeString(in.Script)
			if err != nil {
				return nil, fmt.Errorf("patch_ids must be base64 JSON array: %v", err)
			}
			if err := json.Unmarshal(decoded, &patchIDs); err != nil {
				return nil, fmt.Errorf("patch_ids must decode to string array: %v", err)
			}
		}
		scheduleReboot := false
		rebootDelay := uint32(0)
		if in.TimeoutS > 0 {
			scheduleReboot = true
			rebootDelay = uint32(in.TimeoutS)
		}
		return &agentv1.Command_PatchApply{PatchApply: &agentv1.PatchApply{
			PatchIds:           patchIDs,
			ScheduleReboot:     scheduleReboot,
			RebootDelaySeconds: rebootDelay,
		}}, nil
	default:
		return nil, fmt.Errorf("unknown action %q (want run_script|reboot|file_pull|file_push|collect_inventory|patch_query|patch_apply)", in.Action)
	}
}

// bulkCommand is the B-2 fan-out: ONE capability-gated command dispatched
// to every device carrying a tag (a "group" like tag:windows-servers).
//
//	POST /{api|admin}/devices/bulk/commands
//	{"tag":"web", "action":"run_script", "lang":"sh", "script":"<b64>", "args":[...], "timeout_s":0}
//
//	200 {tag, requested, pushed:[{device_id,command_id}], offline:[id], failed:{id:err}}
//	400  bad body / unknown action / unsupported lang / fan-out above the cap
//	403  session lacks the action's capability (W3-3) — checked BEFORE any
//	      device is touched
//	404  no device carries the tag
//	503  dispatch not wired (tests)
//
// A bulk fan-out is exactly N single-device grants: each pushed command
// carries its own per-device capability token the agent verifies (caps), so
// the bulk route mints no blanket authority beyond what dispatching to each
// device individually would grant.
const maxBulkFanout = 500

// bulkRequest is the JSON body for POST /devices/bulk/commands: a tag plus
// the same fields as a single dispatch.
type bulkRequest struct {
	Tag      string   `json:"tag"`
	Action   string   `json:"action"`
	Lang     string   `json:"lang"`
	Script   string   `json:"script"` // base64
	Args     []string `json:"args"`
	TimeoutS int32    `json:"timeout_s"`
}

func (s *Server) bulkCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.dispatch == nil {
		http.Error(w, "command dispatch not configured", http.StatusServiceUnavailable)
		return
	}
	var in bulkRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	tags, err := normalizeTags([]string{in.Tag})
	if err != nil || len(tags) != 1 {
		http.Error(w, "tag is required (want a single non-empty tag)", http.StatusBadRequest)
		return
	}
	tag := tags[0]
	action, err := buildCommandAction(dispatchRequest{
		Action: in.Action, Lang: in.Lang, Script: in.Script,
		Args: in.Args, TimeoutS: in.TimeoutS,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// W3-3: gate on the operator's capability BEFORE touching any device.
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
	list, err := s.devices.List(r.Context())
	if err != nil {
		http.Error(w, "device list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var ids []string
	for _, d := range list {
		for _, t := range d.Tags {
			if t == tag {
				ids = append(ids, d.ID)
				break
			}
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no devices carry tag " + tag,
		})
		return
	}
	if len(ids) > maxBulkFanout {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("tag %s matches %d devices, above the %d fan-out cap", tag, len(ids), maxBulkFanout),
		})
		return
	}
	pushed := []map[string]string{}
	var offline []string
	failed := map[string]string{}
	for _, id := range ids {
		// A FRESH action per device: the dispatcher stamps the capability
		// token into the action struct in place, so reusing one action for
		// the whole cohort would leave every queued command carrying the
		// LAST device's token (agents would REFUSE them as misbound).
		one, err := buildCommandAction(dispatchRequest{
			Action: in.Action, Lang: in.Lang, Script: in.Script,
			Args: in.Args, TimeoutS: in.TimeoutS,
		})
		if err != nil {
			// Unreachable — the action was validated above the capability gate.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cmdID, err := s.dispatch(id, one)
		if err != nil {
			if strings.Contains(err.Error(), "not reachable") {
				offline = append(offline, id)
			} else {
				failed[id] = err.Error()
			}
			continue
		}
		pushed = append(pushed, map[string]string{"device_id": id, "command_id": cmdID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":       tag,
		"requested": len(ids),
		"pushed":    pushed,
		"offline":   offline,
		"failed":    failed,
	})
}

// registerCommands mounts the signed agent release distribution (W4-2, open — the agent fetches these): only when a releases directory is configured.
func registerCommands(s *Server, mux *http.ServeMux) {
	// W4-2: signed agent release distribution (open — the AGENT fetches these,
	// not an operator). The manifest + assets are only served when a releases
	// directory is configured; otherwise the routes 404 (agent = up-to-date).
	if s.releases != nil {
		mux.HandleFunc("/agent/releases/latest", s.handleReleasesLatest)
		mux.HandleFunc("/agent/releases/latest/", s.handleReleaseAsset)
	}
}
