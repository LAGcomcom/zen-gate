package agents

import (
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// The injected DefaultModel is the user's chosen pick when the roster carries
// it; anything else (empty, hidden, retired, channel off) falls back to the
// first model — a stale pick must never inject a dead default.
func TestSnapshotHonorsConfiguredDefaultModel(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(st)
	roster := []lane.ModelInfo{
		{ID: "aaa-first", Wire: "chat"},
		{ID: "mmm-chosen", Wire: "chat"},
		{ID: "zzz-last", Wire: "chat"},
	}
	r.SetEndpoints("http://127.0.0.1:8787/v1", roster)
	if o, _ := r.snapshot(); o.DefaultModel != "aaa-first" {
		t.Fatalf("no configured default: %q, want first model", o.DefaultModel)
	}

	st.Mutate(func(c *store.Config) { c.DefaultModel = "mmm-chosen" })
	if o, _ := r.snapshot(); o.DefaultModel != "mmm-chosen" {
		t.Fatalf("configured default: %q, want mmm-chosen", o.DefaultModel)
	}

	// A default naming a model the roster no longer carries must fall back —
	// and the fingerprint must differ, so the reinjection actually runs.
	_, keyChosen := r.snapshot()
	st.Mutate(func(c *store.Config) { c.DefaultModel = "retired-model" })
	withStale, keyStale := r.snapshot()
	if withStale.DefaultModel != "aaa-first" {
		t.Fatalf("stale default: %q, want fallback aaa-first", withStale.DefaultModel)
	}
	if keyChosen == keyStale {
		t.Fatal("changing only the default must change the injection fingerprint")
	}
}
