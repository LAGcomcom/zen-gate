package lane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A failed listing must not shrink the catalog.
//
// The rebuild used to start from an empty slice, so whichever source failed
// simply contributed nothing and l.catalog was replaced with the remainder.
// Observed in production: one boot where the Zen listing lost its race with
// sing-box's first inbound left the catalog holding only the four Kilo rows.
// All six Zen models were still listed upstream and kept answering requests,
// but they were absent from the gateway's own /v1/models and — because the
// agent adapters are re-injected from that list — absent from ZCode too, until
// a later restart happened to win the race.
func TestRefreshCatalogKeepsASourceWhoseListingFailed(t *testing.T) {
	var zenUp atomic.Bool
	zenUp.Store(true)
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !zenUp.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "space-bunny-free", "owned_by": "opencode"}},
		})
	}))
	defer zen.Close()

	kilo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "poolside/laguna-s-2.1:free", "isFree": true}},
		})
	}))
	defer kilo.Close()

	oldUp, oldKilo := UpstreamBase, KiloBase
	UpstreamBase, KiloBase = zen.URL, kilo.URL
	t.Cleanup(func() { UpstreamBase, KiloBase = oldUp, oldKilo })

	l := NewLane()
	l.RefreshCatalog(context.Background())

	// Sanity: both sources landed, so the test is actually exercising a mixed
	// catalog rather than a single-source one.
	hasZen, hasKilo := false, false
	cat, _, _ := l.Snapshot()
	for _, m := range cat {
		if m.Channel == ChannelKilo {
			hasKilo = true
		} else {
			hasZen = true
		}
	}
	if !hasZen || !hasKilo {
		t.Fatalf("setup: catalog should hold both sources, got zen=%v kilo=%v", hasZen, hasKilo)
	}

	// Now the Zen listing starts failing. The catalog must keep serving the Zen
	// models it already knows about.
	zenUp.Store(false)
	l.RefreshCatalog(context.Background())

	hasZen, hasKilo = false, false
	cat, _, _ = l.Snapshot()
	for _, m := range cat {
		if m.Channel == ChannelKilo {
			hasKilo = true
		} else {
			hasZen = true
		}
	}
	if !hasZen {
		t.Error("a failed Zen listing dropped every Zen model from the catalog")
	}
	if !hasKilo {
		t.Error("a failed Zen listing must not disturb the Kilo slice")
	}
}

// The same rule from the other side: when the Kilo listing fails, the Zen slice
// stays put and the catalog is not emptied.
func TestRefreshCatalogKeepsKiloWhenItsListingFails(t *testing.T) {
	zen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "space-bunny-free", "owned_by": "opencode"}},
		})
	}))
	defer zen.Close()

	var kiloUp atomic.Bool
	kiloUp.Store(true)
	kilo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !kiloUp.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "poolside/laguna-s-2.1:free", "isFree": true}},
		})
	}))
	defer kilo.Close()

	oldUp, oldKilo := UpstreamBase, KiloBase
	UpstreamBase, KiloBase = zen.URL, kilo.URL
	t.Cleanup(func() { UpstreamBase, KiloBase = oldUp, oldKilo })

	l := NewLane()
	l.RefreshCatalog(context.Background())

	kiloUp.Store(false)
	l.RefreshCatalog(context.Background())

	hasZen, hasKilo := false, false
	cat, _, _ := l.Snapshot()
	for _, m := range cat {
		if m.Channel == ChannelKilo {
			hasKilo = true
		} else {
			hasZen = true
		}
	}
	if !hasZen {
		t.Error("a failed Kilo listing must not disturb the Zen slice")
	}
	if !hasKilo {
		t.Error("a failed Kilo listing dropped every Kilo model from the catalog")
	}
}

// A rebuild that genuinely has nothing cached and gets nothing back must leave
// the catalog alone rather than replacing it with the empty set.
func TestRefreshCatalogWithEverythingFailingIsANoOp(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fail.Close()

	oldUp, oldKilo := UpstreamBase, KiloBase
	UpstreamBase, KiloBase = fail.URL, fail.URL
	t.Cleanup(func() { UpstreamBase, KiloBase = oldUp, oldKilo })

	l := NewLane()
	l.mu.Lock()
	l.catalog = []ModelInfo{{ID: "space-bunny-free", Wire: "chat"}}
	l.mu.Unlock()

	l.RefreshCatalog(context.Background())

	cat, _, _ := l.Snapshot()
	if got := len(cat); got != 1 {
		t.Errorf("catalog holds %d models after an all-failed refresh, want the cached 1", got)
	}
}
