package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zen-gate/internal/lane"
)

// fakeAgentHome points os.UserHomeDir at a temp tree so an adapter writes into
// the test, not into the user's real ~/.zcode.
func fakeAgentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ZEN_GATE_HOME", filepath.Join(home, "data"))
	return home
}

func zcodeFile(t *testing.T, content string) string {
	t.Helper()
	home := fakeAgentHome(t)
	path := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(path, "provider_config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("written file is not valid JSON: %v", err)
	}
	return doc
}

func zOptions() Options {
	return Options{
		BaseURL: "http://127.0.0.1:8787/v1",
		APIKey:  "k",
		Models: []lane.ModelInfo{
			{ID: "a-free", Name: "A", ContextWindow: 131072, Reasoning: true},
		},
	}
}

const zcodeWithOtherProviders = `{
  "schemaVersion": 1,
  "config": {
    "providerConfigRules": {
      "providerRules": [
        {"providerId": "p-glm", "providerName": "Z.AI", "enabled": true, "config": {"api": {"baseUrl": "https://glm.example/v1"}}}
      ]
    },
    "modelConfigRules": {
      "providerModelRules": [{"modelId": "glm-4.6", "providerId": "p-glm"}],
      "manualProviderModelRules": []
    },
    "featureFlags": {"personalProviders": true}
  },
  "updatedAt": "2026-10-01T00:00:00Z"
}`

// ZCode's schema requires manualProviderModelRules to be an array — an empty one
// is fine, a missing one is a ZodError, and on that error ZCode discards the
// whole file and boots with an empty in-memory config. Every provider and every
// model the user had disappears from the picker.
func TestZCodeEnableKeepsManualProviderModelRules(t *testing.T) {
	path := zcodeFile(t, zcodeWithOtherProviders)
	z := newZCode()
	if err := z.Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	mcr := readJSON(t, path)["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	v, ok := mcr["manualProviderModelRules"]
	if !ok {
		t.Fatalf("manualProviderModelRules vanished: %v — ZCode would refuse this file and lose every provider", keysOf(mcr))
	}
	if arr, ok := v.([]any); !ok || len(arr) != 0 {
		t.Errorf("manualProviderModelRules = %#v, want the empty array preserved", v)
	}
}

// The same enable path with a field that was never there must still produce it,
// because the file is rewritten from whatever zen-gate last saw.
func TestZCodeEnableAddsMissingManualProviderModelRules(t *testing.T) {
	path := zcodeFile(t, `{"schemaVersion":1,"config":{"providerConfigRules":{"providerRules":[]},"modelConfigRules":{"providerModelRules":[]}}}`)
	z := newZCode()
	if err := z.Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	mcr := readJSON(t, path)["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	if _, ok := mcr["manualProviderModelRules"].([]any); !ok {
		t.Errorf("manualProviderModelRules = %#v, want an array even when the input had none", mcr["manualProviderModelRules"])
	}
}

// zen-gate rewrites a file it does not fully model. Decoding into a struct that
// lists three fields and marshalling it back deletes everything else — ZCode's
// own feature flags, timestamps, and any provider section zen-gate has never
// heard of. Round-tripping the document is the only safe contract.
func TestZCodeEnablePreservesFieldsItDoesNotModel(t *testing.T) {
	path := zcodeFile(t, zcodeWithOtherProviders)
	z := newZCode()
	if err := z.Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	doc := readJSON(t, path)
	if doc["updatedAt"] != "2026-10-01T00:00:00Z" {
		t.Errorf("top-level updatedAt = %v, want it preserved", doc["updatedAt"])
	}
	cfg := doc["config"].(map[string]any)
	if cfg["featureFlags"] == nil {
		t.Errorf("config.featureFlags was dropped — zen-gate does not know that key")
	}
	mcr := cfg["modelConfigRules"].(map[string]any)
	rules, _ := mcr["providerModelRules"].([]any)
	var sawGLM bool
	for _, r := range rules {
		if m, _ := r.(map[string]any); m["providerId"] == "p-glm" {
			sawGLM = true
		}
	}
	if !sawGLM {
		t.Errorf("the user's own model rules vanished: %#v", mcr["providerModelRules"])
	}
}

func TestZCodeDisablePreservesTheDocumentShape(t *testing.T) {
	path := zcodeFile(t, zcodeWithOtherProviders)
	z := newZCode()
	if err := z.Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	if err := z.Disable(); err != nil {
		t.Fatal(err)
	}
	doc := readJSON(t, path)
	cfg := doc["config"].(map[string]any)
	mcr := cfg["modelConfigRules"].(map[string]any)
	if _, ok := mcr["manualProviderModelRules"].([]any); !ok {
		t.Errorf("after Disable, manualProviderModelRules = %#v, want the array still present", mcr["manualProviderModelRules"])
	}
	if cfg["featureFlags"] == nil {
		t.Errorf("Disable dropped config keys zen-gate does not model")
	}
	rules, _ := cfg["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	for _, r := range rules {
		if m, _ := r.(map[string]any); m["providerName"] == "Zen Gate" {
			t.Errorf("Disable left our own provider behind")
		}
	}
}

func keysOf(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
