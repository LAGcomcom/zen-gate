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
// Observed in production: one boot where a listing lost its race with
// sing-box's first inbound left the catalog holding only one source's rows.
// The rest were still listed upstream and kept answering requests, but they
// were absent from the gateway's own /v1/models and — because the agent
// adapters are re-injected from that list — absent from ZCode too, until a
// later restart happened to win the race.
//
// The Zen lane that test was written against is retired, so the pair is now
// the fetched Kilo pool and the static WorkBuddy roster: two sources with
// different failure modes, which is what the rule is about.
func TestRefreshCatalogKeepsASourceWhoseListingFailed(t *testing.T) {
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

	oldKilo := KiloBase
	KiloBase = kilo.URL
	t.Cleanup(func() { KiloBase = oldKilo })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	l.RefreshCatalog(context.Background())

	// Sanity: both sources landed, so the test is actually exercising a mixed
	// catalog rather than a single-source one.
	hasKilo, hasWB := false, false
	cat, _, _ := l.Snapshot()
	for _, m := range cat {
		switch m.Channel {
		case ChannelKilo:
			hasKilo = true
		case ChannelWorkBuddy:
			hasWB = true
		}
	}
	if !hasKilo || !hasWB {
		t.Fatalf("setup: catalog should hold both sources, got kilo=%v workbuddy=%v", hasKilo, hasWB)
	}

	// Now the Kilo listing starts failing. The catalog must keep serving the
	// rows it already knows about, and must not disturb the other source.
	kiloUp.Store(false)
	l.RefreshCatalog(context.Background())

	hasKilo, hasWB = false, false
	cat, _, _ = l.Snapshot()
	for _, m := range cat {
		switch m.Channel {
		case ChannelKilo:
			hasKilo = true
		case ChannelWorkBuddy:
			hasWB = true
		}
	}
	if !hasKilo {
		t.Error("a failed Kilo listing dropped every Kilo model from the catalog")
	}
	if !hasWB {
		t.Error("a failed Kilo listing must not disturb the WorkBuddy slice")
	}
}

// The same rule from the other side: logging out of WorkBuddy empties only its
// own bucket, and a logged-out roster never takes the Kilo rows with it.
func TestRefreshCatalogKeepsKiloWhenItsListingFails(t *testing.T) {
	kilo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{"id": "poolside/laguna-s-2.1:free", "isFree": true}},
		})
	}))
	defer kilo.Close()

	oldKilo := KiloBase
	KiloBase = kilo.URL
	t.Cleanup(func() { KiloBase = oldKilo })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	l.RefreshCatalog(context.Background())

	l.SetWorkBuddyAccount(WBAccount{})
	l.RefreshCatalog(context.Background())

	hasKilo, hasWB := false, false
	cat, _, _ := l.Snapshot()
	for _, m := range cat {
		switch m.Channel {
		case ChannelKilo:
			hasKilo = true
		case ChannelWorkBuddy:
			hasWB = true
		}
	}
	if hasWB {
		t.Error("a logged-out WorkBuddy account must empty its bucket")
	}
	if !hasKilo {
		t.Error("logging out of WorkBuddy dropped every Kilo model from the catalog")
	}
}

// A rebuild that genuinely has nothing cached and gets nothing back must leave
// the catalog alone rather than replacing it with the empty set.
func TestRefreshCatalogWithEverythingFailingIsANoOp(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fail.Close()

	oldKilo := KiloBase
	KiloBase = fail.URL
	t.Cleanup(func() { KiloBase = oldKilo })

	l := NewLane()
	l.mu.Lock()
	l.catalog = []ModelInfo{{ID: "poolside/laguna-s-2.1:free", Wire: "chat", Channel: ChannelKilo}}
	l.mu.Unlock()

	l.RefreshCatalog(context.Background())

	cat, _, _ := l.Snapshot()
	if got := len(cat); got != 1 {
		t.Errorf("catalog holds %d models after an all-failed refresh, want the cached 1", got)
	}
}
