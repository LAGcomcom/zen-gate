package agents

import (
	"testing"

	"zen-gate/internal/lane"
)

// Issue #27: every model rule must declare the full option set. What zen-gate
// leaves out, ZCode fills from its own built-in metadata, and the mixed rule
// contradicts itself — the picker shows ZCode's [disabled,enabled,max] ladder
// while the gateway honours [light,balanced,deep], and an unset
// maxOutputTokens pins every model to ZCode's 128K default: too high for a
// 32K model, unreachable for a 943K one.
func TestZCodeEnableWritesFullOptionSpecs(t *testing.T) {
	path := zcodeFile(t, `{"schemaVersion":1,"config":{"providerConfigRules":{"providerRules":[]},"modelConfigRules":{"providerModelRules":[]}}}`)
	z := newZCode()
	o := Options{
		BaseURL: "http://127.0.0.1:8787/v1",
		APIKey:  "k",
		Models: []lane.ModelInfo{
			{ID: "think", ContextWindow: 1048576, MaxOutput: 262144, Reasoning: true},
			{ID: "plain", ContextWindow: 131072, MaxOutput: 8192},
		},
	}
	if err := z.Enable(o); err != nil {
		t.Fatal(err)
	}
	mcr := readJSON(t, path)["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	rules, _ := mcr["providerModelRules"].([]any)
	byID := map[string]map[string]any{}
	for _, r := range rules {
		if m, _ := r.(map[string]any); m != nil {
			byID[m["modelId"].(string)] = m
		}
	}
	if len(byID) != 2 {
		t.Fatalf("both injected models must land, got %d rules", len(byID))
	}

	// A reasoning model with a declared output cap carries the full selector.
	cfg, _ := byID["think"]["config"].(map[string]any)
	specs, _ := cfg["optionSpecs"].(map[string]any)
	if specs == nil {
		t.Fatalf("reasoning model must get optionSpecs, got %#v", cfg)
	}
	if specs["reasoningLevel"] == nil {
		t.Errorf("reasoning model lost the thought-level selector: %#v", specs)
	}
	mos, _ := specs["maxOutputTokens"].(map[string]any)
	if mos == nil {
		t.Fatalf("maxOutputTokens must be pinned so ZCode cannot default to 128K: %#v", specs)
	}
	if max, _ := mos["max"].(float64); int(max) != 262144 {
		t.Errorf("maxOutputTokens.max = %v, want 262144", mos["max"])
	}

	// A non-reasoning model gets the cap without a thought-level selector it
	// cannot honour.
	cfg, _ = byID["plain"]["config"].(map[string]any)
	specs, _ = cfg["optionSpecs"].(map[string]any)
	if _, hasLevel := specs["reasoningLevel"]; hasLevel {
		t.Errorf("non-reasoning model must not advertise reasoning levels: %#v", specs)
	}
	mos, _ = specs["maxOutputTokens"].(map[string]any)
	if max, _ := mos["max"].(float64); int(max) != 8192 {
		t.Errorf("plain maxOutputTokens.max = %v, want 8192", mos["max"])
	}
}
