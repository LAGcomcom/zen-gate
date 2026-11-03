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

// serveModelsDev answers with one document shaped like models.dev/api.json:
// provider → models → id → limit.
func serveModelsDev(t *testing.T, doc map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

// A sectioned document: the ids are the lane's own, the capacities are the
// provider's declaration, rows without limits and sections we do not read are
// dropped.
func TestFetchModelsDevReadsExactIds(t *testing.T) {
	up := serveModelsDev(t, map[string]any{
		"opencode": map[string]any{"models": map[string]any{
			"deepseek-v4.1-flash-free": map[string]any{"limit": map[string]any{"context": 200000, "output": 32000}},
			"cat-jam-free":             map[string]any{"limit": map[string]any{"context": 1048576, "output": 131072}},
			"no-limits":                map[string]any{"limit": map[string]any{}},
		}},
		"kilo": map[string]any{"models": map[string]any{
			"cohere/north-mini-code:free": map[string]any{"limit": map[string]any{"context": 256000, "output": 64000}},
		}},
	})
	defer up.Close()
	old := ModelsDevBase
	ModelsDevBase = up.URL
	defer func() { ModelsDevBase = old }()

	meta, err := FetchModelsDev(context.Background(), up.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 2 {
		t.Fatalf("meta = %+v, want the two sections' rows with limits only", meta)
	}
	if m := meta["deepseek-v4.1-flash-free"]; m.ContextWindow != 200000 || m.MaxOutput != 32000 {
		t.Errorf("deepseek-v4.1-flash-free = %+v, want 200000/32000", m)
	}
	if _, ok := meta["cohere/north-mini-code:free"]; ok {
		t.Errorf("the kilo section carries its own capacities and must not be read")
	}
}

// The whole point of this source: a model the curated table answers for with a
// family-wide regex must still get the capacity the provider declares for it.
// The reported case is a trimmed free derivative inheriting its flagship's 1M.
func TestApplyExactCorrectsTheCuratedGuess(t *testing.T) {
	st := testStore(t)
	cat := []lane.ModelInfo{
		{ID: "deepseek-v4.1-flash-free", ContextWindow: 1048576, MaxOutput: 65536},
		{ID: "mimo-v2.6-flash-free", ContextWindow: 1048576, MaxOutput: 131072},
		{ID: "kilo-row", Channel: lane.ChannelKilo},
	}
	meta := map[string]ModelMeta{
		"deepseek-v4.1-flash-free": {ContextWindow: 200000, MaxOutput: 32000},
		"mimo-v2.6-flash-free":     {ContextWindow: 200000, MaxOutput: 32000},
		"kilo-row":                 {ContextWindow: 1, MaxOutput: 1},
	}
	applied := ApplyExact(st, cat, meta)
	if len(applied) != 2 {
		t.Fatalf("applied = %+v, want both zen-lane rows", applied)
	}
	tag, ok := st.ModelTagOf("deepseek-v4.1-flash-free")
	if !ok {
		t.Fatal("row missing")
	}
	if tag.ContextWindow != 200000 || tag.MaxOutput != 32000 {
		t.Errorf("tag = %+v, want the declared 200000/32000", tag)
	}
	if tag.Source != store.TagSourceListing || !tag.CapsOnly {
		t.Errorf("tag = %+v, want 上游声明 + CapsOnly", tag)
	}
	if _, ok := st.ModelTagOf("kilo-row"); ok {
		t.Errorf("the kilo pool declares its own capacities")
	}
}

// 实测 outranks 上游声明: a measured row is never rewritten.
func TestApplyExactLeavesAMeasuredRowAlone(t *testing.T) {
	st := testStore(t)
	st.SetModelTag("deepseek-v4.1-flash-free", store.ModelTag{
		ContextWindow: 4096, MaxOutput: 1000, Source: store.TagSourceProbe,
	})
	applied := ApplyExact(st, []lane.ModelInfo{{ID: "deepseek-v4.1-flash-free"}},
		map[string]ModelMeta{"deepseek-v4.1-flash-free": {ContextWindow: 200000, MaxOutput: 32000}})
	if len(applied) != 0 {
		t.Fatalf("applied = %+v, want a probed row untouched", applied)
	}
	tag, _ := st.ModelTagOf("deepseek-v4.1-flash-free")
	if tag.ContextWindow != 4096 {
		t.Errorf("tag = %+v, want the measured window kept", tag)
	}
}

// A lower-rung row that carries modality verdicts keeps them: this source
// states capacities only, and dropping the bools would silence a modality.
func TestApplyExactCarriesModalityVerdicts(t *testing.T) {
	st := testStore(t)
	st.SetModelTag("space-bunny-free", store.ModelTag{
		Vision: true, Reasoning: true, ContextWindow: 1048576, MaxOutput: 65536,
		Source: store.TagSourceAI,
	})
	ApplyExact(st, []lane.ModelInfo{{ID: "space-bunny-free"}},
		map[string]ModelMeta{"space-bunny-free": {ContextWindow: 1048576, MaxOutput: 524288}})
	tag, _ := st.ModelTagOf("space-bunny-free")
	if !tag.Vision || !tag.Reasoning {
		t.Errorf("tag = %+v, want the AI verdicts carried over", tag)
	}
	if tag.CapsOnly {
		t.Errorf("tag = %+v, want CapsOnly false so the bools are read", tag)
	}
	if tag.MaxOutput != 524288 {
		t.Errorf("tag = %+v, want the declared output cap", tag)
	}
}

// Re-pulling the same declarations must not churn tags.json.
func TestApplyExactIsIdempotent(t *testing.T) {
	st := testStore(t)
	cat := []lane.ModelInfo{{ID: "cat-jam-free"}}
	meta := map[string]ModelMeta{"cat-jam-free": {ContextWindow: 1048576, MaxOutput: 131072}}
	if got := ApplyExact(st, cat, meta); len(got) != 1 {
		t.Fatalf("first pass = %+v, want one row", got)
	}
	if got := ApplyExact(st, cat, meta); len(got) != 0 {
		t.Errorf("second pass = %+v, want nothing rewritten", got)
	}
}
