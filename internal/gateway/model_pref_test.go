package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func newPrefServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return New(lane.NewLane(), st)
}

// resolveEffort is the single choke point where a request's thinking level
// is decided. Order: model-name suffix > client-declared effort > the
// model's own preference (the card's clicked rung) > the global default.
func TestResolveEffortPrefOrder(t *testing.T) {
	s := newPrefServer(t)

	// No pref: falls to the global default (balanced).
	if got := s.resolveEffort("some-model"); got != "balanced" {
		t.Fatalf("no pref: %q", got)
	}

	// A per-model pref outranks the global default.
	s.Store.Mutate(func(c *store.Config) {
		c.ModelPrefs = map[string]string{"some-model": "deep"}
	})
	if got := s.resolveEffort("some-model"); got != "deep" {
		t.Fatalf("with pref: %q, want deep", got)
	}
	// The suffix id form and the pref key agree (both are base ids).
	if got := s.resolveEffort("some-model (light)"); got != "light" {
		t.Fatalf("suffix must win: %q", got)
	}
	// A client-declared effort beats the pref.
	if got := s.resolveEffort("some-model", "low"); got != "light" {
		t.Fatalf("declared beats pref: %q", got)
	}

	// A stale/invalid pref value silently falls through, never breaks a turn.
	s.Store.Mutate(func(c *store.Config) { c.ModelPrefs["some-model"] = "quantum" })
	if got := s.resolveEffort("some-model"); got != "balanced" {
		t.Fatalf("invalid pref: %q, want global default", got)
	}
}

// The pref endpoint normalizes spellings, clears with an empty effort, and
// rejects unknown ones loudly.
func TestModelPrefEndpoint(t *testing.T) {
	s := newPrefServer(t)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	url := ts.URL + "/admin/api/models/pref"

	j := postAdmin(t, url, `{"id":"m (deep)","effort":"high"}`)
	if !j["ok"].(bool) {
		t.Fatalf("set: %v", j)
	}
	// The stored key is the base id and the value is canonical.
	if prefs := s.Store.Config().ModelPrefs; prefs["m"] != "deep" {
		t.Fatalf("prefs = %v, want m→deep", prefs)
	}
	if got := s.resolveEffort("m"); got != "deep" {
		t.Fatalf("resolve after set: %q", got)
	}

	if j := postAdmin(t, url, `{"id":"m","effort":""}`); !j["ok"].(bool) {
		t.Fatalf("clear: %v", j)
	}
	if len(s.Store.Config().ModelPrefs) != 0 {
		t.Fatalf("clear left %v", s.Store.Config().ModelPrefs)
	}

	if j := postAdmin(t, url, `{"id":"m","effort":"quantum"}`); j["ok"].(bool) {
		t.Fatal("an unknown rung must answer ok:false, not store garbage")
	}
}
