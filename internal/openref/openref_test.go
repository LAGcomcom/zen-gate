package openref

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"stepfun/step-5-preview-free": "step-5-preview",
		"step-5-preview":              "step-5-preview",
		"stealth/glyph-cluster":       "glyph-cluster",
		"muse-spark-1.3-contributor":  "muse-spark-1.3",
		"Muse_Spark_1.3-free":         "muse-spark-1.3",
		"anthropic/claude-sonnet-4":   "claude-sonnet-4",
		"":                            "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func serveListing(t *testing.T, rows []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
}

func TestFetchKeepsCapacityRowsAndDropsCollisions(t *testing.T) {
	up := serveListing(t, []map[string]any{
		{"id": "stepfun/step-5-preview", "context_length": 1000000,
			"top_provider": map[string]any{"max_completion_tokens": 64000}},
		{"id": "vendor-a/inkling-small", "context_length": 1048576,
			"top_provider": map[string]any{"max_completion_tokens": 262144}},
		{"id": "vendor-b/inkling-small", "context_length": 200000,
			"top_provider": map[string]any{"max_completion_tokens": 8000}},
		{"id": "vendor-c/inkling-small", "context_length": 300000,
			"top_provider": map[string]any{"max_completion_tokens": 32000}},
		{"id": "textonly/model", "context_length": 0},
	})
	defer up.Close()
	old := Base
	Base = up.URL
	defer func() { Base = old }()

	meta, err := Fetch(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if m := meta["step-5-preview"]; m.ContextWindow != 1000000 || m.MaxOutput != 64000 {
		t.Errorf("step-5-preview = %+v", m)
	}
	// Two vendor rows normalizing to one base id is the same-name hazard:
	// the key must be dropped, not guessed between.
	if _, ok := meta["inkling-small"]; ok {
		t.Errorf("collision key must be dropped: %+v", meta["inkling-small"])
	}
	if _, ok := meta["model"]; ok {
		t.Errorf("capacity-less rows must not enter the map")
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The gate is the whole design: only models the local table states nothing
// about AND that carry no tags.json row at all may receive capacities.
func TestApplyGate(t *testing.T) {
	st := testStore(t)
	meta := map[string]ModelMeta{
		"zeta-new":       {ContextWindow: 1000000, MaxOutput: 64000},
		"muse-spark-1.3": {ContextWindow: 1048576, MaxOutput: 943718},
		"brand-new":      {ContextWindow: 131072, MaxOutput: 16384},
		"kilo-only":      {ContextWindow: 99, MaxOutput: 9},
	}
	cat := []lane.ModelInfo{
		{ID: "zeta-new"},
		{ID: "muse-spark-1.3"},
		{ID: "brand-new"},
		{ID: "kilo-only", Channel: lane.ChannelKilo},
	}
	applied := Apply(st, cat, meta)
	got := map[string]bool{}
	for _, a := range applied {
		got[a.ID] = true
	}
	if got["muse-spark-1.3"] {
		t.Errorf("curated-table ids must never be touched")
	}
	if got["kilo-only"] {
		t.Errorf("kilo rows carry their own declared capacities")
	}
	if !got["brand-new"] || !got["zeta-new"] {
		t.Fatalf("models nothing states must be filled: %+v", applied)
	}

	// zeta-new lands first…
	if _, ok := st.ModelTagOf("zeta-new"); !ok {
		t.Fatalf("apply missed zeta-new: %+v", applied)
	}

	// …then a probe verdict arrives. The next round must not overwrite it:
	// Apply sees the existing tags row and skips the id.
	st.SetModelTag("zeta-new", store.ModelTag{Source: store.TagSourceProbe, Vision: true})
	second := Apply(st, cat, meta)
	for _, a := range second {
		if a.ID == "zeta-new" {
			t.Errorf("a probed model must not be re-filled")
		}
	}
	// CapsOnly rows also block a later fill (one row = one chance).
	third := Apply(st, []lane.ModelInfo{{ID: "brand-new"}}, meta)
	if len(third) != 0 {
		t.Errorf("a model filled once must stay filled: %+v", third)
	}
}

// An applied row must be CapsOnly/上游声明 so the merged-capability layer
// treats it as windows-only (modality questions stay with the heuristics).
func TestApplyWritesCapsOnlyListingRows(t *testing.T) {
	st := testStore(t)
	Apply(st, []lane.ModelInfo{{ID: "brand-new"}}, map[string]ModelMeta{
		"brand-new": {ContextWindow: 131072, MaxOutput: 16384},
	})
	tag, ok := st.ModelTagOf("brand-new")
	if !ok {
		t.Fatal("row missing")
	}
	if !tag.CapsOnly || tag.Source != store.TagSourceListing {
		t.Fatalf("tag = %+v, want CapsOnly + 上游声明", tag)
	}
	if tag.Vision || tag.Audio || tag.File || tag.Reasoning {
		t.Errorf("CapsOnly row must state no modalities: %+v", tag)
	}
}
