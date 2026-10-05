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

// A provider's models must reach the agent menus, not just /v1/models. The gap
// this covers: a user configures a provider, the models work when requested by
// hand, and no picker offers them — so the feature is half-built from the
// user's side.

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

func TestVisibleModelsIncludesEnabledProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	got := map[string]bool{}
	for _, m := range s.VisibleModels() {
		got[m.ID] = true
	}
	for _, want := range []string{"local/meissa-local", "local/qwen-local"} {
		if !got[want] {
			t.Errorf("VisibleModels missing %q", want)
		}
	}
	if got["off/should-not-appear"] {
		t.Errorf("a disabled provider's model leaked into VisibleModels")
	}
}

func TestVisibleModelsRespectsHidden(t *testing.T) {
	// Hiding works on the gateway id, same as for free-lane models.
	s := providerServer(t, sampleProviders(), []string{"local/qwen-local"})
	got := map[string]bool{}
	for _, m := range s.VisibleModels() {
		got[m.ID] = true
	}
	if !got["local/meissa-local"] {
		t.Errorf("visible provider model missing")
	}
	if got["local/qwen-local"] {
		t.Errorf("hidden provider model still listed")
	}
}

func TestVisibleModelsProviderFieldsPopulated(t *testing.T) {
	// Codex rejects a whole catalog when one entry lacks a required field, and
	// an empty ContextWindow reads as "truncate immediately" downstream.
	s := providerServer(t, sampleProviders(), nil)
	for _, m := range s.VisibleModels() {
		if m.ID != "local/meissa-local" {
			continue
		}
		if m.Name == "" {
			t.Errorf("provider model has no display name")
		}
		if m.Blurb == "" {
			t.Errorf("provider model has no description")
		}
		if m.ContextWindow <= 0 {
			t.Errorf("ContextWindow = %d, want > 0", m.ContextWindow)
		}
		if m.MaxOutput <= 0 {
			t.Errorf("MaxOutput = %d, want > 0", m.MaxOutput)
		}
		return
	}
	t.Errorf("provider model not in VisibleModels")
}

func TestCodexCatalogIncludesProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
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
	var mine map[string]any
	for _, m := range out.Models {
		if m["slug"] == "local/meissa-local" {
			mine = m
		}
	}
	if mine == nil {
		t.Fatalf("provider model missing from codex catalog: %s", rec.Body.String())
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
	// A provider declares no reasoning levels; claiming them would let a user
	// pick (deep) on a model that ignores the parameter.
	lv, _ := mine["supported_reasoning_levels"].([]any)
	if len(lv) != 1 {
		t.Errorf("provider model should advertise one effort level, got %d", len(lv))
	}
}

// A disabled provider must not appear even in the dynamic catalog.
func TestCodexCatalogExcludesDisabledProviders(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)
	if body := rec.Body.String(); strings.Contains(body, "should-not-appear") {
		t.Errorf("disabled provider leaked into the codex catalog")
	}
}

// The two lists must agree: the sidecar comes from VisibleModels(), the
// endpoint from the loop above. If they drift, a model looks intermittently
// present depending on whether Codex could reach the gateway — the hardest
// kind of bug to report.
func TestCatalogListsAgree(t *testing.T) {
	s := providerServer(t, sampleProviders(), nil)

	fromVisible := map[string]bool{}
	for _, m := range s.VisibleModels() {
		fromVisible[m.ID] = true
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	fromEndpoint := map[string]bool{}
	for _, m := range out.Models {
		if id, ok := m["slug"].(string); ok {
			fromEndpoint[id] = true
		}
	}
	for id := range fromVisible {
		if !fromEndpoint[id] {
			t.Errorf("%q is in VisibleModels (sidecar) but missing from /v1/codex-catalog", id)
		}
	}
	for id := range fromEndpoint {
		if !fromVisible[id] {
			t.Errorf("%q is in /v1/codex-catalog but missing from VisibleModels (sidecar)", id)
		}
	}
}

// A provider model id that repeats a free-lane id is a *different* gateway id
// ("clash/x" vs "x"), and providerRoute only matches on the prefix before "/".
// So both entries are legitimate and each routes where its name says — the test
// pins that down so a future dedup pass does not silently drop one.
func TestProviderIdRepeatingLaneIdIsDistinct(t *testing.T) {
	s := providerServer(t, []store.Provider{{
		ID: "clash", Name: "撞名", BaseURL: "http://127.0.0.1:8090",
		Protocol: store.ProtocolOpenAI, Enabled: true,
		Models: []string{"mimo-v2.6-flash-free"},
	}}, nil)
	got := map[string]bool{}
	for _, m := range s.VisibleModels() {
		got[m.ID] = true
	}
	if !got["clash/mimo-v2.6-flash-free"] {
		t.Errorf("provider entry missing: the ids are distinct and both should be offered")
	}
	if !got["mimo-v2.6-flash-free"] {
		t.Errorf("free-lane entry missing: a provider repeating the name must not displace it")
	}
}
