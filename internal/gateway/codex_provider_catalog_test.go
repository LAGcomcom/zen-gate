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

// A provider's models must reach the agent menus, not just /v1/models. The
// gap this covers: a user configures a provider, the models work when
// requested by hand, and no picker offers them — the feature is half-built
// from the user's side, and it looks broken although requests succeed.

// providerServer builds a server with cfg applied to a throwaway store.
func providerServer(t *testing.T, providers []store.Provider, hidden []string) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.Providers = providers
	cfg.HiddenModels = hidden
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return New(lane.NewLane(), st)
}

func sampleProviders() []store.Provider {
	return []store.Provider{
		{ID: "local", Name: "本地模型", BaseURL: "http://127.0.0.1:8090",
			Protocol: store.ProtocolOpenAI, Enabled: true,
			Models: []string{"meissa-local", "qwen-local"}},
		{ID: "off", Name: "已停用", BaseURL: "http://127.0.0.1:9999",
			Protocol: store.ProtocolOpenAI, Enabled: false,
			Models: []string{"should-not-appear"}},
	}
}

func codexCatalog(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	return out.Models
}

func TestCodexCatalogIncludesProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	var mine map[string]any
	for _, m := range codexCatalog(t, s) {
		if m["slug"] == "local/meissa-local" {
			mine = m
		}
	}
	if mine == nil {
		t.Fatal("provider model missing from codex catalog")
	}
	// Every field a free-lane entry carries must be present, or Codex discards
	// the entire catalog rather than the one bad entry.
	required := []string{
		"base_instructions", "default_reasoning_level", "description",
		"display_name", "experimental_supported_tools", "input_modalities",
		"shell_type", "slug", "support_verbosity", "supported_in_api",
		"supported_reasoning_levels", "truncation_policy", "visibility",
		"priority", "provider_id", "context_window", "max_output_tokens",
	}
	var missing []string
	for _, k := range required {
		if _, ok := mine[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("provider catalog entry missing %v", missing)
	}
	if mine["provider_id"] != "zen_gate" {
		t.Errorf("provider_id = %v, want zen_gate (must match [model_providers.zen_gate])", mine["provider_id"])
	}
	// Providers declare no reasoning levels; claiming low/medium/high would
	// let a user pick (deep) on a model that ignores the parameter.
	lv, _ := mine["supported_reasoning_levels"].([]any)
	if len(lv) != 1 {
		t.Errorf("provider model should advertise one effort level, got %d", len(lv))
	}
	// Capacity defaults: a 0 context window reads as "truncate immediately"
	// downstream, so provider entries must carry the same defaults the store
	// fills in elsewhere.
	if cw, _ := mine["context_window"].(float64); cw != 131072 {
		t.Errorf("context_window = %v, want 131072", mine["context_window"])
	}
	if mo, _ := mine["max_output_tokens"].(float64); mo != 32768 {
		t.Errorf("max_output_tokens = %v, want 32768", mine["max_output_tokens"])
	}
	// Priority 5: below every free model, including region-gated ones.
	if p, _ := mine["priority"].(float64); p != 5 {
		t.Errorf("priority = %v, want 5", mine["priority"])
	}
}

func TestCodexCatalogExcludesDisabledProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	for _, m := range codexCatalog(t, s) {
		if slug, _ := m["slug"].(string); strings.Contains(slug, "should-not-appear") {
			t.Errorf("disabled provider leaked into the codex catalog: %q", slug)
		}
	}
}

// TestCatalogListsAgree pins the two supply points together: the static
// sidecar is written from InjectableModels(), the endpoint recomputes from
// the same call — but through a different serialization path. If they drift,
// a model looks intermittently present depending on whether Codex could
// reach the gateway — the hardest kind of bug to report.
func TestCatalogListsAgree(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)

	fromInjectable := map[string]bool{}
	for _, m := range s.InjectableModels() {
		fromInjectable[m.ID] = true
	}
	fromEndpoint := map[string]bool{}
	for _, m := range codexCatalog(t, s) {
		if id, ok := m["slug"].(string); ok {
			fromEndpoint[id] = true
		}
	}
	for id := range fromInjectable {
		if !fromEndpoint[id] {
			t.Errorf("%q is in InjectableModels (sidecar) but missing from /v1/codex-catalog", id)
		}
	}
	for id := range fromEndpoint {
		if !fromInjectable[id] {
			t.Errorf("%q is in /v1/codex-catalog but missing from InjectableModels (sidecar)", id)
		}
	}
}

// A provider model id that repeats a free-lane id is a *different* gateway id
// ("clash/x" vs "x"), and providerRoute only matches on the prefix before
// "/". Both entries are legitimate and each routes where its name says —
// pinned so a future dedup pass does not silently drop one.
func TestProviderIdRepeatingLaneIdIsDistinct(t *testing.T) {
	s := providerServer(t, []store.Provider{{
		ID: "clash", Name: "撞名", BaseURL: "http://127.0.0.1:8090",
		Protocol: store.ProtocolOpenAI, Enabled: true,
		Models: []string{"mimo-v2.6-flash-free"},
	}}, nil)
	got := map[string]bool{}
	for _, m := range s.InjectableModels() {
		got[m.ID] = true
	}
	if !got["clash/mimo-v2.6-flash-free"] {
		t.Errorf("provider entry missing: the ids are distinct and both should be offered")
	}
	if !got["mimo-v2.6-flash-free"] {
		t.Errorf("free-lane entry missing: a provider repeating the name must not displace it")
	}
}

// Provider models must rank after every free model — including region-gated
// ones: the free lane is the product, a provider is the user's own addition.
func TestProviderModelsRankAfterAllFreeModels(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	var lastFreeIdx, providerIdx = -1, -1
	for i, m := range codexCatalog(t, s) {
		slug, _ := m["slug"].(string)
		if isProviderID(slug) {
			providerIdx = i
		} else {
			lastFreeIdx = i
		}
	}
	if providerIdx < 0 || lastFreeIdx < 0 {
		t.Fatalf("expected both free and provider entries, got free@%d provider@%d", lastFreeIdx, providerIdx)
	}
	if providerIdx < lastFreeIdx {
		t.Fatalf("provider model at %d ranks before a free model at %d", providerIdx, lastFreeIdx)
	}
}

// /v1/models must not mint (light)/(deep) variants for provider models: they
// ignore the effort parameter, and a phantom id would 404 on use.
func TestListModelsNoEffortVariantsForProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	rec := httptest.NewRecorder()
	s.route(rec, req)
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, d := range out.Data {
		seen[d.ID]++
		if strings.HasSuffix(d.ID, "(light)") || strings.HasSuffix(d.ID, "(deep)") {
			if strings.HasPrefix(d.ID, "local/") {
				t.Errorf("provider model got an effort variant: %q", d.ID)
			}
		}
	}
	if seen["local/meissa-local"] != 1 {
		t.Errorf("local/meissa-local appears %d times, want exactly 1 (no variants)", seen["local/meissa-local"])
	}
}

// The capability tags feed input_modalities: a provider model the tag
// pipeline marked vision-capable is advertised with image, unknown stays
// text-only (conservative).
func TestCodexCatalogInputModalitiesFromTags(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	s.Store.SetModelTag("qwen-local", store.ModelTag{Vision: true, Source: store.TagSourceAI})
	var visionModel, textModel map[string]any
	for _, m := range codexCatalog(t, s) {
		switch m["slug"] {
		case "local/qwen-local":
			visionModel = m
		case "local/meissa-local":
			textModel = m
		}
	}
	if visionModel == nil || textModel == nil {
		t.Fatal("expected both provider entries in the catalog")
	}
	hasImage := func(m map[string]any) bool {
		mods, _ := m["input_modalities"].([]any)
		for _, mod := range mods {
			if mod == "image" {
				return true
			}
		}
		return false
	}
	if !hasImage(visionModel) {
		t.Errorf("AI-tagged vision model must advertise image input, got %v", visionModel["input_modalities"])
	}
	if hasImage(textModel) {
		t.Errorf("untagged model must stay text-only, got %v", textModel["input_modalities"])
	}
}
