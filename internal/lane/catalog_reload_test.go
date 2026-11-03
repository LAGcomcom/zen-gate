package lane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// RefreshCatalog rebuilds the catalog from the curated table + listing, which
// drops every tag-patched verdict — the reload hook must fire synchronously
// before the async notify broadcast, so readers woken by the change (the
// dashboard, agent injection, the capability filter) never observe the raw
// rebuild without its replayed verdicts.
func TestRefreshCatalogFiresReloadHookBeforeBroadcast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "brand-new-free", "owned_by": "opencode"}},
		})
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()
	oldKilo := KiloBase
	KiloBase = srv.URL + "/kilo" // rows lack isFree: the kilo slice comes up empty
	defer func() { KiloBase = oldKilo }()

	l := NewLane()
	var mu sync.Mutex
	var events []string
	record := func(name string) {
		mu.Lock()
		events = append(events, name)
		mu.Unlock()
	}
	l.OnCatalogReload = func() { record("reload") }
	l.OnChange = func() { record("change") }
	l.RefreshCatalog(context.Background())

	// Reload must already have fired when RefreshCatalog returns — it is the
	// synchronous half of the rebuild.
	mu.Lock()
	done := len(events) >= 1 && events[0] == "reload"
	mu.Unlock()
	if !done {
		t.Fatalf("OnCatalogReload must run synchronously before RefreshCatalog returns, events = %v", events)
	}
	// The broadcast is a goroutine: give it a moment, then insist reload
	// strictly preceded it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[1] != "change" {
		t.Fatalf("broadcast must follow the reload hook, events = %v", events)
	}
}
