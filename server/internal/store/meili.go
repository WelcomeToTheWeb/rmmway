package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Meili indexes enrolled devices into Meilisearch (W1-7): the Cmd-K
// palette (W2-2) searches this index, so a freshly enrolled device must
// be findable by hostname, IP, tag, or id almost immediately.
//
// Minimal Meilisearch v1 HTTP client (stdlib only): index setup,
// document add/delete, search, and task waiting. Write calls in
// Meilisearch are async (they return a task), so Sync/FullSync wait for
// the resulting task before reporting success — that's what makes
// "enroll → immediately findable" true rather than eventual.
type Meili struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func NewMeili(endpoint, apiKey string) *Meili {
	return &Meili{
		endpoint: endpoint,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// IndexName is the Meilisearch index holding device documents. It is a
// var (not a const) so tests can redirect it to a scratch index.
var IndexName = "devices"

type meiliTask struct {
	TaskUid any `json:"taskUid"` // responses to write ops
	UID     any `json:"uid"`     // GET /tasks/{id} uses "uid"
	Status  string
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// uid returns the task id under whichever field name the response used.
func (t meiliTask) uid() any {
	if t.TaskUid != nil {
		return t.TaskUid
	}
	return t.UID
}

// ensureIndex creates the devices index if missing and configures its
// attributes. Idempotent: an existing index is left alone (re-creating
// it would wipe its documents). Meili v1.12 rejects PUT /indexes/{uid}
// with 405 — index creation is POST /indexes with the uid in the body.
func (m *Meili) ensureIndex(ctx context.Context) error {
	var idx struct {
		UID        any     `json:"uid"`
		PrimaryKey *string `json:"primaryKey"`
	}
	_, err := m.do(ctx, http.MethodGet, "/indexes/"+IndexName, nil, &idx)
	switch {
	case err != nil && strings.Contains(err.Error(), "404"):
		// Index missing → create (async; wait before touching settings,
		// or the settings PATCH races the creation).
		uid, err := m.do(ctx, http.MethodPost, "/indexes", map[string]any{
			"uid":        IndexName,
			"primaryKey": "devices_id",
		}, nil)
		if err != nil {
			return err
		}
		return m.waitTask(ctx, uid)
	case err != nil:
		return err
	case idx.PrimaryKey == nil || *idx.PrimaryKey != "devices_id":
		// Stale index without our primary key (e.g. created by an older
		// client): delete and recreate so document import can't fail on
		// primary-key inference.
		uid, err := m.do(ctx, http.MethodDelete, "/indexes/"+IndexName, nil, nil)
		if err != nil {
			return err
		}
		if err := m.waitTask(ctx, uid); err != nil {
			return err
		}
		uid, err = m.do(ctx, http.MethodPost, "/indexes", map[string]any{
			"uid":        IndexName,
			"primaryKey": "devices_id",
		}, nil)
		if err != nil {
			return err
		}
		return m.waitTask(ctx, uid)
	}
	// Index is healthy — just (re)assert the search settings. The settings
	// update is async too: wait for it, or a search issued right after
	// (e.g. by a test or a freshly enrolled device) can miss the new
	// searchable attributes.
	uid, err := m.do(ctx, http.MethodPatch, "/indexes/"+IndexName+"/settings", map[string]any{
		"searchableAttributes": []string{
			"hostname", "id", "interfaces", "tags",
			"os", "arch", "agent_version",
		},
		// B-2: tag groups filter by exact tag (the `tag:web` syntax in the
		// UI/palette maps to `tags = "web"` here).
		"filterableAttributes": []string{"tags"},
		"displayedAttributes": []string{"*"},
	}, nil)
	if err != nil {
		return err
	}
	return m.waitTask(ctx, uid)
}

// EnsureIndex is the exported form of ensureIndex (admin re-sync + tests).
func (m *Meili) EnsureIndex(ctx context.Context) error { return m.ensureIndex(ctx) }

// waitTask polls a task uid until it succeeds/fails or ctx expires.
func (m *Meili) waitTask(ctx context.Context, uid any) error {
	for {
		var t meiliTask
		if _, err := m.do(ctx, http.MethodGet, fmt.Sprintf("/tasks/%v", uid), nil, &t); err != nil {
			return err
		}
		switch t.Status {
		case "succeeded":
			return nil
		case "failed":
			if t.Error != nil {
				return fmt.Errorf("meili task %v: %s", uid, t.Error.Message)
			}
			return fmt.Errorf("meili task %v failed (no message)", uid)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("meili task %v: %w", uid, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FullSync re-indexes every device from the store (boot + admin manual).
func (m *Meili) FullSync(ctx context.Context, ds DeviceStore) error {
	if err := m.ensureIndex(ctx); err != nil {
		return err
	}
	list, err := ds.List(ctx)
	if err != nil {
		return err
	}
	docs := make([]map[string]any, 0, len(list))
	for _, d := range list {
		docs = append(docs, DeviceToDoc(d))
	}
	uid, err := m.do(ctx, http.MethodPost, "/indexes/"+IndexName+"/documents", docs, nil)
	if err != nil {
		return err
	}
	return m.waitTask(ctx, uid)
}

// Sync re-indexes one device from the store (enroll / heartbeat path).
// If the device no longer exists in the store it is removed from the
// index, keeping the two in step.
func (m *Meili) Sync(ctx context.Context, ds DeviceStore, id string) error {
	list, err := ds.List(ctx)
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.ID != id {
			continue
		}
		if err := m.ensureIndex(ctx); err != nil {
			return err
		}
		uid, err := m.do(ctx, http.MethodPost, "/indexes/"+IndexName+"/documents",
			[]map[string]any{DeviceToDoc(d)}, nil)
		if err != nil {
			return err
		}
		return m.waitTask(ctx, uid)
	}
	if err := m.ensureIndex(ctx); err != nil {
		return err
	}
	uid, err := m.do(ctx, http.MethodDelete,
		"/indexes/"+IndexName+"/documents/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	return m.waitTask(ctx, uid)
}

// DeviceToDoc maps a store.Device to its Meilisearch document.
// "devices_id" is a deterministic alias of id used as the Meilisearch
// primary key, so the searchable "id" field never collides with
// reserved document fields.
func DeviceToDoc(d *Device) map[string]any {
	if d.Interfaces == nil {
		d.Interfaces = []string{}
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
	return map[string]any{
		"devices_id":    d.ID,
		"id":            d.ID,
		"hostname":      d.Hostname,
		"os":            d.OS,
		"arch":          d.Arch,
		"agent_version": d.AgentVersion,
		"interfaces":    d.Interfaces,
		"tags":          d.Tags,
		"online":        d.Online,
		"last_seen":     d.LastSeen.UTC().Format(time.RFC3339),
	}
}

// SearchResult is one page of search hits. Meilisearch v1.12 reports
// the match count as estimatedTotalHits (not totalHits).
type SearchResult struct {
	Hits               []map[string]any `json:"hits"`
	EstimatedTotalHits int              `json:"estimatedTotalHits"`
}

// Search runs a keyword query over the device index (Cmd-K backing).
// An empty query returns the first `limit` devices.
func (m *Meili) Search(ctx context.Context, query string, limit int) (*SearchResult, error) {
	return m.SearchFiltered(ctx, query, "", limit)
}

// SearchFiltered is Search with an optional Meilisearch filter expression
// (B-2 tag groups: `tags = "web"`). An empty query + filter returns the
// first `limit` devices matching the filter.
func (m *Meili) SearchFiltered(ctx context.Context, query, filter string, limit int) (*SearchResult, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	body := map[string]any{"q": query, "limit": limit}
	if filter != "" {
		body["filter"] = filter
	}
	var out SearchResult
	_, err := m.do(ctx, http.MethodPost, "/indexes/"+IndexName+"/search", body, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Health reports whether the Meilisearch instance answers /health.
func (m *Meili) Health(ctx context.Context) error {
	_, err := m.do(ctx, http.MethodGet, "/health", nil, nil)
	return err
}

// do performs an authenticated request against the Meilisearch API and
// returns the async task uid (nil for non-task responses).
func (m *Meili) do(ctx context.Context, method, path string, body any, out any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.endpoint+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meili %s %s: %d: %s", method, path, resp.StatusCode, snippet(rb))
	}
	if out != nil && len(rb) > 0 {
		if err := json.Unmarshal(rb, out); err != nil {
			return nil, fmt.Errorf("meili decode: %w", err)
		}
	}
	var t meiliTask
	_ = json.Unmarshal(rb, &t)
	return t.uid(), nil
}

func snippet(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// DeviceIndexer is what the ingest layer programs against, so it never
// imports Meilisearch types directly.
type DeviceIndexer interface {
	Sync(ctx context.Context, ds DeviceStore, id string) error
	FullSync(ctx context.Context, ds DeviceStore) error
}

var _ DeviceIndexer = (*Meili)(nil)

// IndexerHook is a fire-and-forget sync trigger with per-device
// single-flight + 500ms debounce, so heartbeat storms (30s per device,
// thousands of devices) can't flood Meilisearch with redundant writes.
type IndexerHook struct {
	idx DeviceIndexer
	ds  DeviceStore

	mu      sync.Mutex
	pending map[string]bool
	stopped chan struct{}
}

func NewIndexerHook(idx DeviceIndexer, ds DeviceStore) *IndexerHook {
	h := &IndexerHook{
		idx:     idx,
		ds:      ds,
		pending: make(map[string]bool),
		stopped: make(chan struct{}),
	}
	go h.loop()
	return h
}

// Touch queues a device for re-indexing (non-blocking, idempotent).
func (h *IndexerHook) Touch(id string) {
	if h == nil || id == "" {
		return
	}
	h.mu.Lock()
	h.pending[id] = true
	h.mu.Unlock()
}

func (h *IndexerHook) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-h.stopped:
			return
		case <-t.C:
			for _, id := range h.claimDue() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				if err := h.idx.Sync(ctx, h.ds, id); err != nil {
					// Transient (Meili down / task timeout) — the next
					// heartbeat or boot FullSync will retry.
					log.Printf("meili index sync %s: %v", id, err)
				}
				cancel()
			}
		}
	}
}

func (h *IndexerHook) claimDue() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	due := make([]string, 0, len(h.pending))
	for id := range h.pending {
		due = append(due, id)
	}
	h.pending = make(map[string]bool)
	return due
}

// Stop ends the background loop (shutdown path).
func (h *IndexerHook) Stop() {
	if h == nil {
		return
	}
	close(h.stopped)
}
