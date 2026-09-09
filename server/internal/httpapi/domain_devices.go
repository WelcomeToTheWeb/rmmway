package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/welcometotheweb/rmmway/server/internal/store"
	"github.com/welcometotheweb/rmmway/server/internal/users"
)

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
	// ClientID (gap #2) is the owning MSP client; nil = unassigned
	// (rendered under the default "Unassigned" client in the UI).
	ClientID *string `json:"client_id"`
}

// toDeviceOut maps a store row to its JSON shape (nil slices rendered []).
func toDeviceOut(d *store.Device) deviceOut {
	if d.Interfaces == nil {
		d.Interfaces = []string{}
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	out := deviceOut{
		ID: d.ID, Hostname: d.Hostname, OS: d.OS, Arch: d.Arch,
		AgentVersion: d.AgentVersion, Interfaces: d.Interfaces, Tags: d.Tags,
		Online: d.Online, FirstSeen: d.FirstSeen, LastSeen: d.LastSeen,
	}
	if d.ClientID != "" {
		cid := d.ClientID
		out.ClientID = &cid
	}
	return out
}

// deviceList is served at both /api/devices (auth-gated) and /admin/devices
// (open). It returns every enrolled device with live status.
func (s *Server) deviceList(w http.ResponseWriter, r *http.Request) {
	out := []deviceOut{}
	// gap #2: ?client=<id> scopes the list to one client's devices
	// ("unassigned" also matches unassigned devices; "" = all).
	client := r.URL.Query().Get("client")
	list, err := s.devices.ListByClient(r.Context(), client)
	if err != nil {
		http.Error(w, "device list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// gap #3: a non-admin session with no ?client= (already grant-validated
	// when present) sees the union of its granted clients.
	if sess, ok := users.SessionFromContext(r.Context()); ok && !sess.AllClients && client == "" {
		list = filterDevicesByClients(list, sess)
	}
	for _, d := range list {
		out = append(out, toDeviceOut(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// filterDevicesByClients (gap #3) keeps the devices whose client the session
// is granted; an empty device client id = the default client.
func filterDevicesByClients(list []*store.Device, sess users.Session) []*store.Device {
	out := make([]*store.Device, 0, len(list))
	for _, d := range list {
		cid := d.ClientID
		if cid == "" {
			cid = store.DefaultClientID
		}
		if sess.CanSeeClient(cid) {
			out = append(out, d)
		}
	}
	return out
}

// handleSearch serves Meilisearch device search. Degraded (503) when the
// index is unavailable so the rest of the API keeps working.
//
// B-2: a `tag:<name>` prefix (or a ?tag=<name> param) switches to an exact
// tag filter (`tags = "<name>"`) over the index instead of a keyword query,
// so the palette and the device view can jump to a whole tag group.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.search == nil {
		http.Error(w, "search index not available (meilisearch down or disabled)", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query().Get("q")
	filter := ""
	if t := r.URL.Query().Get("tag"); t != "" {
		filter = tagFilterExpr(t)
		q = ""
	} else if strings.HasPrefix(q, "tag:") {
		filter = tagFilterExpr(strings.TrimSpace(q[len("tag:"):]))
		q = ""
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	res, err := s.search.SearchFiltered(r.Context(), q, filter, limit)
	if err != nil {
		http.Error(w, "search: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// tagFilterExpr builds the Meilisearch filter for an exact tag. Quotes and
// backslashes are stripped so the expression can never be broken out of
// (stored tags are normalized to [a-z0-9._-]* anyway; a non-matching value
// simply yields zero hits).
func tagFilterExpr(tag string) string {
	t := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' {
			return -1
		}
		return r
	}, tag)
	return `tags = "` + t + `"`
}

// normalizeTags (B-2) validates an operator-supplied tag list: trims,
// lowercases, dedupes, and enforces shape/limits. A tag is
// [a-z0-9][a-z0-9._-]* at most maxTagLen characters; at most
// maxTagsPerDevice tags per device. Returns an empty slice for empty input.
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const (
	maxTagLen        = 64
	maxTagsPerDevice = 20
)

func normalizeTags(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		if len(t) > maxTagLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", t, maxTagLen)
		}
		if !tagPattern.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q (want [a-z0-9][a-z0-9._-]*)", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > maxTagsPerDevice {
		return nil, fmt.Errorf("at most %d tags per device", maxTagsPerDevice)
	}
	return out, nil
}

// deviceSub routes the /{api|admin}/devices/{id}/... subtree (W2-2 + W3-3 +
// W4-3 + W6-1) plus the B-2 tag endpoints:
//
//	POST {id}/commands  — dispatch (auth-gated under /api, open under /admin)
//	GET  {id}/commands  — pending commands + recorded results (W3-3)
//	GET  {id}/export    — the per-client full export bundle (W4-3)
//	GET  {id}/events    — recent indexed agent-log events (W6-1)
//	GET  {id}/metrics    — the device's metric series (viewer picker)
//	GET  {id}/metrics/series — bucketed samples of one series over a range
//	GET  {id}/inventory   — deep inventory for the device (gap #4)
//	POST {id}/inventory/collect — trigger inventory collection (gap #4)
//	PATCH {id}          — replace the device's tag list (B-2)
//	POST bulk/commands  — capability-gated fan-out to a tag group (B-2)
func (s *Server) deviceSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// parts: ["api"|"admin", "devices", ...]
	if len(parts) < 3 || parts[1] != "devices" || parts[2] == "" {
		http.Error(w, "expected /devices/{id}/(commands|export|events|inventory) or /devices/bulk/commands", http.StatusNotFound)
		return
	}
	// gap #3: single-device routes scope to the device's client (an empty
	// client = the default client) — a non-admin session that is not
	// granted that client gets a 403 here, before the sub-route dispatch.
	// Admin/legacy sessions skip the extra store fetch (they can see
	// everything, and unknown-device 404s come from the sub-hander).
	if parts[2] != "bulk" {
		if sess, ok := users.SessionFromContext(r.Context()); ok && !sess.AllClients {
			dev, err := s.devices.Get(r.Context(), parts[2])
			if err != nil {
				http.Error(w, "unknown device", http.StatusNotFound)
				return
			}
			cid := dev.ClientID
			if cid == "" {
				cid = store.DefaultClientID
			}
			if !requireClientAccess(w, r, cid) {
				return
			}
		}
	}
	if len(parts) == 3 {
		if !requireRole(w, r, "admin", "tech") { // gap #3: viewers are read-only
			return
		}
		s.patchDeviceTags(w, r, parts[2])
		return
	}
	if parts[2] == "bulk" {
		if len(parts) == 4 && parts[3] == "commands" {
			if !requireRole(w, r, "admin", "tech") { // gap #3
				return
			}
			s.bulkCommand(w, r)
			return
		}
		http.Error(w, "expected /devices/bulk/commands", http.StatusNotFound)
		return
	}
	if len(parts) == 5 {
		if parts[3] == "metrics" && parts[4] == "series" {
			s.deviceMetricSeries(w, r, parts[2])
			return
		}
		http.Error(w, "expected /devices/{id}/metrics/series", http.StatusNotFound)
		return
	}
	if len(parts) == 5 {
		if parts[3] == "inventory" && parts[4] == "collect" {
			if !requireRole(w, r, "admin", "tech") { // gap #3: operational
				return
			}
			s.handleTriggerInventoryCollect(w, r, parts[2])
			return
		}
		http.Error(w, "expected /devices/{id}/(commands|export|events|metrics|inventory)", http.StatusNotFound)
		return
	}
	if len(parts) != 4 ||
		(parts[3] != "commands" && parts[3] != "export" && parts[3] != "events" && parts[3] != "metrics" && parts[3] != "inventory") {
		http.Error(w, "expected /devices/{id}/(commands|export|events|metrics|inventory)", http.StatusNotFound)
		return
	}
	switch parts[3] {
	case "commands":
		if r.Method == http.MethodGet {
			s.deviceCommands(w, r, parts[2])
			return
		}
		if !requireRole(w, r, "admin", "tech") { // gap #3: dispatch is operational
			return
		}
		s.dispatchCommand(w, r, parts[2])
	case "export":
		s.handleDeviceExport(w, r, parts[2])
	case "events":
		s.deviceEvents(w, r, parts[2])
	case "metrics":
		s.deviceMetrics(w, r, parts[2])
	case "inventory":
		s.handleDeviceInventory(w, r, parts[2])
	default:
		http.Error(w, "expected /devices/{id}/(commands|export|events|metrics|inventory)", http.StatusNotFound)
	}
}

// deviceEvents serves the W6-1 per-device log view: the device's recent
// indexed agent-log events (the Timescale copy of what also ships to Loki).
//
//	GET /{api|admin}/devices/{id}/events?limit=50&level=warn
//
//	200 {device_id, events: [{id, level, msg, attrs, timestamp_ms, time}]} (newest first)
//	400 bad limit/level
//	404 unknown device
//	503 log events not wired (pre-W6-1 server)
func (s *Server) deviceEvents(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.logEvents == nil {
		http.Error(w, "log events not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		l, err := strconv.Atoi(v)
		if err != nil || l <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = l
	}
	level := q.Get("level")
	switch strings.ToLower(level) {
	case "", "debug", "info", "warn", "error":
	default:
		http.Error(w, "level must be one of debug|info|warn|error", http.StatusBadRequest)
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
	events, err := s.logEvents(deviceID, limit, strings.ToLower(level))
	if err != nil {
		http.Error(w, "log events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []store.LogEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"events":    events,
	})
}

// metricRanges maps the viewer's range selector to (window, bucket):
// the query looks back `window`, and the raw samples are averaged into
// one point per `bucket`, so every range stays a few hundred points
// regardless of the agent's sample rate.
var metricRanges = map[string]struct {
	window time.Duration
	bucket time.Duration
}{
	"1h":  {time.Hour, 30 * time.Second},
	"6h":  {6 * time.Hour, 2 * time.Minute},
	"24h": {24 * time.Hour, 10 * time.Minute},
	"7d":  {7 * 24 * time.Hour, time.Hour},
	"30d": {30 * 24 * time.Hour, 6 * time.Hour},
}

// deviceMetrics serves the per-device metrics viewer's series picker:
// which (name, source) series the device has reported over the range.
//
//	GET /{api|admin}/devices/{id}/metrics?range=7d
//
//	200 {device_id, range, series: [{name, source, last, count}]}
//	400 bad range
//	404 unknown device
//	503 metric history not wired (in-memory mode)
func (s *Server) deviceMetrics(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.metricNames == nil {
		http.Error(w, "metric history not configured", http.StatusServiceUnavailable)
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	rr, ok := metricRanges[rng]
	if !ok {
		http.Error(w, "range must be one of 1h|6h|24h|7d|30d", http.StatusBadRequest)
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
	series, err := s.metricNames(deviceID, time.Now().Add(-rr.window))
	if err != nil {
		http.Error(w, "metric series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if series == nil {
		series = []store.MetricSeries{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"range":     rng,
		"series":    series,
	})
}

// deviceMetricSeries serves the bucketed samples of one metric series:
// the operator UI's device-detail chart.
//
//	GET /{api|admin}/devices/{id}/metrics/series?name=cpu.utilization_percent&source=&range=24h
//
//	200 {device_id, name, source, range, bucket_s, count, min, max, last,
//	      points: [[ts_ms, value], ...]} (ascending)
//	400 missing name or bad range
//	404 unknown device
//	503 metric history not wired (in-memory mode)
func (s *Server) deviceMetricSeries(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.metricSeries == nil {
		http.Error(w, "metric history not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	source := q.Get("source")
	rng := q.Get("range")
	if rng == "" {
		rng = "24h"
	}
	rr, ok := metricRanges[rng]
	if !ok {
		http.Error(w, "range must be one of 1h|6h|24h|7d|30d", http.StatusBadRequest)
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
	points, err := s.metricSeries(deviceID, name, source, time.Now().Add(-rr.window), rr.bucket)
	if err != nil {
		http.Error(w, "metric series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if points == nil {
		points = []store.MetricPoint{}
	}
	min, max, last := 0.0, 0.0, 0.0
	if len(points) > 0 {
		min, max, last = points[0].Value, points[0].Value, points[0].Value
		for _, p := range points[1:] {
			if p.Value < min {
				min = p.Value
			}
			if p.Value > max {
				max = p.Value
			}
			last = p.Value
		}
	}
	jsonPoints := make([][2]any, 0, len(points))
	for _, p := range points {
		jsonPoints = append(jsonPoints, [2]any{p.T.UnixMilli(), p.Value})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": deviceID,
		"name":      name,
		"source":    source,
		"range":     rng,
		"bucket_s":  int64(rr.bucket.Seconds()),
		"count":     len(points),
		"min":       min,
		"max":       max,
		"last":      last,
		"points":    jsonPoints,
	})
}

// handleDeviceExport streams the client's FULL export bundle (W4-3 — the
// no-lock-in promise): one request, one self-describing ZIP with the
// device's inventory + config, all metrics (Parquet), 1-minute rollups
// (Parquet), complete alert history, and a manifest that drives
// verification (export.Verify).
//
//	GET /{api|admin}/devices/{id}/export[?since=RFC3339&until=RFC3339&rollups=0]
//
//	200  application/zip attachment (the bundle)
//	400  bad since/until window
//	404  unknown device
//	503  export not wired (in-memory-mode deployments)
//	500  build error (after headers are out the stream just ends short —
//	     the manifest is the integrity contract, a truncated bundle fails
//	     export.Verify instead of passing as complete)
func (s *Server) handleDeviceExport(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.export == nil {
		http.Error(w, "export not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	var since, until time.Time
	if v := q.Get("since"); v != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("until"); v != "" {
		var err error
		if until, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "until must be RFC3339", http.StatusBadRequest)
			return
		}
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		http.Error(w, "until must be after since", http.StatusBadRequest)
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
	withRollups := q.Get("rollups") != "0"
	fname := "rmmway-export-" + deviceID + "-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.WriteHeader(http.StatusOK)
	if _, err := s.export.Export(r.Context(), deviceID, since, until, withRollups, w); err != nil {
		// Headers already sent: the bundle is truncated. The manifest
		// contract makes that detectable (export.Verify fails), but log it.
		fmt.Fprintf(w, "\nexport error: %v\n", err)
	}
}

// patchDeviceTags is the B-2 tag editor backend: the operator replaces a
// device's whole tag list from the UI (add/remove chips in the device view).
//
//	PATCH /{api|admin}/devices/{id}   {"tags":["web","prod"]}
//
//	200 {device, indexed}  — tags persisted; indexed=false when the search
//	                          index is down (best-effort re-sync)
//	400  bad body / invalid tags
//	404  unknown device
//	500  store error
//
// The Meilisearch re-index is best-effort: a tag edit must not fail because
// the search index is down (the next heartbeat re-index or boot FullSync
// re-covers it), but when it succeeds the new tag is searchable at once.
func (s *Server) patchDeviceTags(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodPatch {
		http.Error(w, "PATCH only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.devices.SetTags(r.Context(), deviceID, tags); err != nil {
		if err == store.ErrNotFound {
			http.Error(w, "unknown device", http.StatusNotFound)
			return
		}
		http.Error(w, "set tags: "+err.Error(), http.StatusInternalServerError)
		return
	}
	indexed := false
	if s.search != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		indexed = s.search.Sync(ctx, s.devices, deviceID) == nil
		cancel()
	}
	d, err := s.devices.Get(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": toDeviceOut(d), "indexed": indexed})
}

// registerDevices mounts the device-domain routes: the device list, search, and the /{api|admin}/devices/ subtree (dispatch, tags, export, events, metrics, bulk fan-out).
func registerDevices(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("/api/devices", s.rbacScopeGate(s.deviceList))
	// W2-2: fuzzy device search (Cmd-K backing) + command dispatch, both auth-gated.
	mux.HandleFunc("/api/search", s.rbacGate(s.handleSearch))
	mux.HandleFunc("/api/devices/", s.rbacGate(s.deviceSub))
	// W3-3: the device's dispatched commands + results (C1: auth-gated).
	mux.HandleFunc("/admin/devices/", s.rbacGate(s.deviceSub))
	mux.HandleFunc("/admin/devices", s.rbacScopeGate(s.deviceList)) // C1: was open
	mux.HandleFunc("/admin/search", s.rbacGate(s.handleSearch))
}
