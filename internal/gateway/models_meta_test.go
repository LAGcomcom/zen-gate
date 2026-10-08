package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// listModels pulls /v1/models and indexes the entries by id.
func listModels(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" {
		t.Fatalf("object = %q", out.Object)
	}
	byID := map[string]map[string]any{}
	for _, e := range out.Data {
		id, _ := e["id"].(string)
		byID[id] = e
	}
	return byID
}

func modsOf(e map[string]map[string]any, id string) []string {
	raw, ok := e[id]["input_modalities"].([]any)
	if !ok {
		return nil
	}
	mods := []string{}
	for _, m := range raw {
		mods = append(mods, m.(string))
	}
	return mods
}

func numField(e map[string]map[string]any, id, field string) int {
	v, ok := e[id][field].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// 测试 / 能力实测 write verdicts into tags.json and a provider's listing writes
// declared capacities into config.json. Both have to reach an agent through the
// standard endpoint, or the only way to learn them is the admin dashboard.
func TestListModelsCarriesCapabilityMetadata(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	// A live probe says the table's text-only nemotron takes images, on a
	// window the curated table does not know about.
	s.Store.SetModelTag("nemotron-3-ultra-free", store.ModelTag{
		Vision: true, Source: store.TagSourceProbe,
		ContextWindow: 200000, MaxOutput: 40000,
	})
	provUp := fakeUpstream(t, nil)
	defer provUp.Close()
	s.Store.Mutate(func(cfg *store.Config) {
		cfg.Providers = append(cfg.Providers, store.Provider{
			ID: "pv1", Name: "PV One", BaseURL: provUp.URL, Protocol: store.ProtocolOpenAI,
			Enabled: true, Models: []string{"glm-4.6"},
			ModelMeta: map[string]store.ModelMeta{"glm-4.6": {
				ContextWindow: 1048576, MaxOutput: 131072, Vision: true,
				InputDeclared: true, OutputDeclared: true,
			}},
		})
	})
	if err := s.Store.Save(); err != nil {
		t.Fatal(err)
	}

	entries := listModels(t, s)
	probe := entries["nemotron-3-ultra-free"]
	if probe == nil {
		t.Fatal("nemotron-3-ultra-free missing from /v1/models")
	}
	if got := numField(entries, "nemotron-3-ultra-free", "context_window"); got != 200000 {
		t.Errorf("probed context window = %d, want the 实测 number to beat the table's 128000", got)
	}
	if got := numField(entries, "nemotron-3-ultra-free", "max_output_tokens"); got != 40000 {
		t.Errorf("probed max_output_tokens = %d, want 40000", got)
	}
	if !hasString(modsOf(entries, "nemotron-3-ultra-free"), "image") {
		t.Errorf("input_modalities = %v, want image from the probe verdict", modsOf(entries, "nemotron-3-ultra-free"))
	}
	if src, _ := probe["capability_source"].(string); src != store.TagSourceProbe {
		t.Errorf("capability_source = %q, want %q so a client knows the verdict was measured", src, store.TagSourceProbe)
	}

	declared := entries["pv1/glm-4.6"]
	if declared == nil {
		t.Fatal("pv1/glm-4.6 missing from /v1/models")
	}
	if got := numField(entries, "pv1/glm-4.6", "context_window"); got != 1048576 {
		t.Errorf("provider context_window = %d, want the listing's 1048576", got)
	}
	if got := numField(entries, "pv1/glm-4.6", "max_output_tokens"); got != 131072 {
		t.Errorf("provider max_output_tokens = %d, want 131072", got)
	}
	if !hasString(modsOf(entries, "pv1/glm-4.6"), "image") {
		t.Errorf("provider input_modalities = %v, want image from the listing", modsOf(entries, "pv1/glm-4.6"))
	}
	if src, _ := declared["capability_source"].(string); src != store.TagSourceListing {
		t.Errorf("provider capability_source = %q, want %q", src, store.TagSourceListing)
	}
}

func hasString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Every advertised modality is a routing decision: audio and file are only
// useful to a client if the listing names them too.
func TestListModelsNamesAudioAndFileInputs(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	s.Store.SetModelTag("mimo-v2.6-flash-free", store.ModelTag{
		Vision: true, Audio: true, File: true, Reasoning: true, Source: store.TagSourceProbe,
	})

	entries := listModels(t, s)
	mods := modsOf(entries, "mimo-v2.6-flash-free")
	for _, want := range []string{"text", "image", "audio", "file"} {
		if !hasString(mods, want) {
			t.Errorf("input_modalities = %v, want %s in it", mods, want)
		}
	}
}

// The effort variants are separate ids a client can pick, so each one has to
// describe the model it routes to rather than an empty shell.
func TestEffortVariantsCarryTheParentMetadata(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	entries := listModels(t, s)
	parent := entries["nemotron-3-ultra-free"]
	variant := entries["nemotron-3-ultra-free (deep)"]
	if parent == nil || variant == nil {
		t.Fatalf("missing entries: parent=%v variant=%v", parent != nil, variant != nil)
	}
	for _, field := range []string{"context_window", "max_output_tokens"} {
		if numField(entries, "nemotron-3-ultra-free (deep)", field) != numField(entries, "nemotron-3-ultra-free", field) {
			t.Errorf("%s = %v on the variant, want the parent's %v", field,
				variant[field], parent[field])
		}
	}
	if mods := modsOf(entries, "nemotron-3-ultra-free (deep)"); !hasString(mods, "text") {
		t.Errorf("variant input_modalities = %v", mods)
	}
}

// The picker's modality list came from vision alone, so a model that was
// measured taking audio and PDF still looked text-only to Codex.
func TestCodexCatalogNamesAudioAndFileInputs(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	old := lane.UpstreamBase
	lane.UpstreamBase = laneUp.URL
	t.Cleanup(func() { lane.UpstreamBase = old })

	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.SetModelTag("space-bunny-free", store.ModelTag{
		Vision: true, Audio: true, File: true, Source: store.TagSourceProbe,
	})
	// A second process: the verdict is only on disk, and the boot merge is
	// what carries it into the catalog the picker is built from.
	st2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := New(lane.NewLane(), st2)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/codex-catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cat struct {
		Models []struct {
			Slug            string   `json:"slug"`
			InputModalities []string `json:"input_modalities"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	for _, m := range cat.Models {
		if m.Slug == "space-bunny-free" {
			for _, want := range []string{"image", "audio", "file"} {
				if !hasString(m.InputModalities, want) {
					t.Fatalf("catalog input_modalities = %v, want %s — the probe says it takes it", m.InputModalities, want)
				}
			}
			return
		}
	}
	t.Fatal("space-bunny-free missing from the catalog")
}

// A restart must not downgrade a measured verdict back to a name guess: the
// tag is on disk, and the lane builds its catalog from the curated table.
func TestStoredTagsSurviveRestartIntoTheLane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.SetModelTag("nemotron-3-ultra-free", store.ModelTag{Audio: true, Source: store.TagSourceProbe})

	// Fresh process: new store handle, new lane.
	st2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	l := lane.NewLane()
	New(l, st2)
	for _, m := range l.ServableModels() {
		if m.ID == "nemotron-3-ultra-free" {
			if !m.AudioInput {
				t.Fatalf("after a restart the lane forgot the 实测 audio verdict: %+v", m)
			}
			return
		}
	}
	t.Fatal("nemotron-3-ultra-free is not on the lane")
}

// 能力实测 answers the three modality questions and nothing about capacities,
// so storing its verdict must not erase the window the listing declared.
func TestCapabilityProbeKeepsKnownCapacities(t *testing.T) {
	gate := fakeUpstream(t, nil)
	defer gate.Close()
	s := newTestServer(t, gate)
	up := capsUpstream(t)
	defer up.Close()

	s.Store.SetModelTag("m1", store.ModelTag{
		Reasoning: true, ContextWindow: 131072, MaxOutput: 8192, Source: store.TagSourceListing,
	})
	p := &store.Provider{ID: "p1", Name: "P1", BaseURL: up.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI, Enabled: true}
	verdicts := s.probeCapabilities(t.Context(), p, "m1")
	for _, v := range verdicts {
		if v == verdictUnknown {
			t.Fatalf("the fake upstream answers every modality, got %v", verdicts)
		}
	}
	tag, ok := s.Store.ModelTagOf("m1")
	if !ok {
		t.Fatal("no tag after a conclusive probe")
	}
	if tag.Source != store.TagSourceProbe {
		t.Errorf("source = %q, want the probe verdict to win", tag.Source)
	}
	if !tag.Reasoning {
		t.Errorf("the probe said nothing about 思考, but it erased it: %+v", tag)
	}
	if tag.ContextWindow != 131072 || tag.MaxOutput != 8192 {
		t.Errorf("capacities = %d/%d, want the previously known 131072/8192", tag.ContextWindow, tag.MaxOutput)
	}
}

// Keeping the endpoint's OpenAI shape is what makes the extra fields free.
func TestListModelsStaysOpenAIShaped(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	for _, e := range listModels(t, s) {
		if e["object"] != "model" {
			t.Fatalf("entry %v has no object=model", e)
		}
		if id, _ := e["id"].(string); id == "" || strings.TrimSpace(id) != id {
			t.Fatalf("bad id %q", e["id"])
		}
		if own, _ := e["owned_by"].(string); own == "" {
			t.Fatalf("entry %v has no owned_by", e)
		}
	}
}
