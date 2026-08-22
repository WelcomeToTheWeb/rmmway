package store

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestMeiliSyncAndSearch verifies the W1-7 loop end-to-end against a live
// Meilisearch instance: FullSync puts every device in the index, Search
// finds a device by hostname / id / IP, and a deleted device disappears
// from search. Uses a scratch index so the dev "devices" index is never
// touched. Requires RMMWAY_MEILI_TEST_ENDPOINT (e.g.
// http://localhost:7700 with RMMWAY_MEILI_TEST_KEY for the master key);
// skipped otherwise.
func TestMeiliSyncAndSearch(t *testing.T) {
	endpoint := os.Getenv("RMMWAY_MEILI_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("RMMWAY_MEILI_TEST_ENDPOINT not set — skipping Meilisearch live test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m := NewMeili(endpoint, os.Getenv("RMMWAY_MEILI_TEST_KEY"))
	const scratch = "devices_w17_test"
	prevIndex := IndexName
	defer func() {
		// Drop the scratch index (by its captured name — the deferred
		// IndexName restore below could race this otherwise).
		if uid, _ := m.do(ctx, http.MethodDelete, "/indexes/"+scratch, nil, nil); uid != nil {
			_ = m.waitTask(ctx, uid)
		}
		IndexName = prevIndex
	}()
	IndexName = scratch

	// Clean slate: delete the scratch index if a previous run left one —
	// and WAIT for the deletion to land, or the index re-created below
	// would race the still-pending deletion.
	if uid, _ := m.do(ctx, http.MethodDelete, "/indexes/"+scratch, nil, nil); uid != nil {
		_ = m.waitTask(ctx, uid)
	}

	ds := NewMemoryDeviceStore()
	devs := []*Device{
		{ID: "dev-a", Hostname: "fileserver-01", OS: "linux", Arch: "amd64",
			AgentVersion: "0.1.0", Interfaces: []string{"10.1.1.5"}, Tags: []string{"prod"}, Online: true},
		{ID: "dev-b", Hostname: "workstation-17", OS: "windows", Arch: "amd64",
			AgentVersion: "0.1.0", Interfaces: []string{"10.1.1.17"}, Tags: []string{"office"}, Online: true},
	}
	for _, d := range devs {
		if err := ds.Register(ctx, d.ID, d.Hostname, d.OS, d.Arch, d.AgentVersion,
			d.Interfaces, 30, 30); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
		// The gRPC enroll path has no tags field (admin sets them later);
		// tag the stored device directly for the tag-search assertion.
		ds.mu.Lock()
		ds.devices[d.ID].Tags = d.Tags
		ds.mu.Unlock()
	}

	if err := m.FullSync(ctx, ds); err != nil {
		t.Fatalf("full sync: %v", err)
	}

	// Search by hostname fragment.
	res, err := m.Search(ctx, "fileserver", 10)
	if err != nil {
		t.Fatalf("search fileserver: %v", err)
	}
	if len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-a" {
		t.Fatalf("expected dev-a top hit for 'fileserver', got %+v", res.Hits)
	}

	// Search by exact id.
	res, err = m.Search(ctx, "dev-b", 10)
	if err != nil {
		t.Fatalf("search dev-b: %v", err)
	}
	if len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-b" {
		t.Fatalf("expected dev-b top hit for 'dev-b', got %+v", res.Hits)
	}

	// Search by IP.
	res, err = m.Search(ctx, "10.1.1.5", 10)
	if err != nil {
		t.Fatalf("search ip: %v", err)
	}
	top := false
	for _, h := range res.Hits {
		if h["id"] == "dev-a" {
			top = true
		}
	}
	if !top {
		t.Fatalf("expected dev-a in hits for ip 10.1.1.5, got %+v", res.Hits)
	}

	// Search by tag.
	res, err = m.Search(ctx, "office", 10)
	if err != nil {
		t.Fatalf("search tag: %v", err)
	}
	if len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-b" {
		t.Fatalf("expected dev-b top hit for tag 'office', got %+v", res.Hits)
	}

	// Single-device Sync picks up a freshly registered device.
	if err := ds.Register(ctx, "dev-c", "printer-03", "linux", "arm64", "0.1.0",
		[]string{"10.1.1.33"}, 30, 30); err != nil {
		t.Fatalf("register dev-c: %v", err)
	}
	if err := m.Sync(ctx, ds, "dev-c"); err != nil {
		t.Fatalf("sync dev-c: %v", err)
	}
	res, err = m.Search(ctx, "printer", 10)
	if err != nil {
		t.Fatalf("search printer: %v", err)
	}
	if len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-c" {
		t.Fatalf("expected dev-c top hit for 'printer', got %+v", res.Hits)
	}

	// A device still in the store stays indexed when Sync runs.
	if err := m.Sync(ctx, ds, "dev-c"); err != nil {
		t.Fatalf("sync dev-c (present): %v", err)
	}
	res, err = m.Search(ctx, "printer", 10)
	if err != nil {
		t.Fatalf("search printer (present): %v", err)
	}
	if len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-c" {
		t.Fatalf("dev-c should still be indexed, got %+v", res.Hits)
	}

	// A device absent from the store is removed from the index by Sync.
	ds.mu.Lock()
	delete(ds.devices, "dev-c")
	ds.mu.Unlock()
	if err := m.Sync(ctx, ds, "dev-c"); err != nil {
		t.Fatalf("sync removal: %v", err)
	}
	res, err = m.Search(ctx, "printer", 10)
	if err != nil {
		t.Fatalf("search after removal: %v", err)
	}
	for _, h := range res.Hits {
		if h["id"] == "dev-c" {
			t.Fatalf("dev-c should be gone from index, still found: %+v", h)
		}
	}

	// Idempotent re-run of FullSync (server restart).
	if err := m.FullSync(ctx, ds); err != nil {
		t.Fatalf("full sync (2nd): %v", err)
	}
	res, err = m.Search(ctx, "fileserver", 10)
	if err != nil || len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-a" {
		t.Fatalf("after 2nd full sync, dev-a missing: %+v err=%v", res, err)
	}

	// Teardown handled by the deferred cleanup above.
}

// TestMeiliIndexSelfHeal verifies ensureIndex recreates an index that
// exists without the expected primary key (the failure mode hit in the
// local stack: an index created by an older client blocked all
// document imports).
func TestMeiliIndexSelfHeal(t *testing.T) {
	endpoint := os.Getenv("RMMWAY_MEILI_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("RMMWAY_MEILI_TEST_ENDPOINT not set — skipping Meilisearch live test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := NewMeili(endpoint, os.Getenv("RMMWAY_MEILI_TEST_KEY"))
	const scratch = "devices_w17_heal"
	prevIndex := IndexName
	IndexName = scratch
	defer func() { IndexName = prevIndex }()

	// Clean slate: delete then wait (deletion is async).
	if uid, _ := m.do(ctx, http.MethodDelete, "/indexes/"+scratch, nil, nil); uid != nil {
		_ = m.waitTask(ctx, uid)
	}
	// Create the index WITHOUT a primary key (the stale state).
	uid, err := m.do(ctx, http.MethodPost, "/indexes", map[string]any{"uid": scratch}, nil)
	if err != nil {
		t.Fatalf("create stale index: %v", err)
	}
	_ = m.waitTask(ctx, uid)

	// ensureIndex must delete + recreate with the right PK.
	if err := m.ensureIndex(ctx); err != nil {
		t.Fatalf("ensureIndex self-heal: %v", err)
	}
	var idx struct {
		PrimaryKey *string `json:"primaryKey"`
	}
	_, err = m.do(ctx, http.MethodGet, "/indexes/"+scratch, nil, &idx)
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	if idx.PrimaryKey == nil || *idx.PrimaryKey != "devices_id" {
		t.Fatalf("expected primary key devices_id after self-heal, got %v", idx.PrimaryKey)
	}

	// Documents now import cleanly.
	ds := NewMemoryDeviceStore()
	if err := ds.Register(ctx, "dev-h", "heal-host", "linux", "amd64", "0.1.0", nil, 30, 30); err != nil {
		t.Fatal(err)
	}
	if err := m.FullSync(ctx, ds); err != nil {
		t.Fatalf("full sync after self-heal: %v", err)
	}
	res, err := m.Search(ctx, "heal-host", 5)
	if err != nil || len(res.Hits) < 1 || res.Hits[0]["id"] != "dev-h" {
		t.Fatalf("expected heal-host searchable, got %+v err=%v", res, err)
	}

	uid, _ = m.do(ctx, http.MethodDelete, "/indexes/"+scratch, nil, nil)
	if uid != nil {
		_ = m.waitTask(ctx, uid)
	}
}
