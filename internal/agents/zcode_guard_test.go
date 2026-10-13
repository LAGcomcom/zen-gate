package agents

import (
	"os"
	"path/filepath"
	"testing"

	"zen-gate/internal/lane"
)

// Issue #45: enabling the ZCode adapter emptied the user's model picker.
// ZCode parses provider_config.json as one strict zod document — any single
// violation rejects the WHOLE file and boots ZCode with an empty in-memory
// config, taking the user's own providers with ours. These tests pin the two
// write-side guards that keep zen-gate from ever producing such a file.

// A cold-start blip can hand the adapter an empty roster (the catalog has not
// loaded yet; #41 saw exactly this window). Overwriting a live file with
// personalModelIds: [] is the reported symptom — refuse instead, leave the
// previous injection on disk, and let a later round with a real roster write.
func TestZCodeEnableRefusesEmptyRosterOverLiveFile(t *testing.T) {
	path := zcodeFile(t, zcodeWithOtherProviders)
	z := newZCode()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Enable(Options{BaseURL: "http://127.0.0.1:8787/v1", APIKey: "k"}); err == nil {
		t.Fatal("empty roster must not overwrite a live provider_config.json")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("file changed despite the refusal — the guard must leave it untouched")
	}
	// The user's own provider survives, and the refusal is a retryable error:
	// ResyncEnabled will not fingerprint the broken state.
	doc := readJSON(t, path)
	rules, _ := doc["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	if len(rules) != 1 || entryName(rules[0]) != "Z.AI" {
		t.Errorf("providerRules = %v, want the user's Z.AI rule intact", rules)
	}
}

// With no file at all there is nothing to destroy: a first enable writes the
// provider skeleton even before the catalog answers, and the next resync
// fills it in.
func TestZCodeEnableEmptyRosterWithoutFileWrites(t *testing.T) {
	home := fakeAgentHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".zcode", "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	z := newZCode()
	if err := z.Enable(Options{BaseURL: "http://127.0.0.1:8787/v1", APIKey: "k"}); err != nil {
		t.Fatalf("first enable with no existing file must not refuse: %v", err)
	}
}

// properties.contextWindow must be a positive integer: zod .int().positive(),
// and one 0 anywhere fails the whole document. A model the lane carries with
// no capacity (a tag merge gone wrong, a provider row without declared
// numbers) must never reach the file as 0.
func TestZCodeEnableFloorsZeroContextWindow(t *testing.T) {
	path := zcodeFile(t, `{"schemaVersion":1,"config":{}}`)
	z := newZCode()
	err := z.Enable(Options{
		BaseURL: "http://127.0.0.1:8787/v1",
		APIKey:  "k",
		Models: []lane.ModelInfo{
			{ID: "no-caps-free", Name: "NoCaps"},                // ContextWindow 0
			{ID: "neg-free", Name: "Neg", ContextWindow: -5},    // nonsense
			{ID: "ok-free", Name: "OK", ContextWindow: 1048576}, // untouched
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := readJSON(t, path)
	rules := doc["config"].(map[string]any)["modelConfigRules"].(map[string]any)["providerModelRules"].([]any)
	want := map[string]int{"no-caps-free": defaultContextWindow, "neg-free": defaultContextWindow, "ok-free": 1048576}
	if len(rules) != len(want) {
		t.Fatalf("model rules = %d, want %d", len(rules), len(want))
	}
	for _, raw := range rules {
		r := raw.(map[string]any)
		props := r["config"].(map[string]any)["properties"].(map[string]any)
		got := int(props["contextWindow"].(float64))
		if exp, ok := want[r["modelId"].(string)]; !ok {
			t.Errorf("unexpected model rule %v", r["modelId"])
		} else if got != exp {
			t.Errorf("contextWindow for %s = %d, want %d — a non-positive value fails ZCode's whole-file schema", r["modelId"], got, exp)
		}
	}
}

// ids that are blank or duplicated would trip string().min(1) and
// checkUniqueIds respectively — both reject the entire file.
func TestSanitizeModelsDropsBlankAndDuplicateIds(t *testing.T) {
	in := []lane.ModelInfo{
		{ID: "a-free"},
		{ID: "  "}, // blank after trim
		{ID: ""},
		{ID: "a-free"},   // duplicate
		{ID: " b-free "}, // trimmed
	}
	got := sanitizeModels(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != "a-free" || got[1].ID != "b-free" {
		t.Errorf("ids = %q, %q, want a-free, b-free", got[0].ID, got[1].ID)
	}
}

// A roster of only-blank ids must behave exactly like an empty one: never
// overwrite a live file.
func TestZCodeEnableAllBlankIdsRefusedOverLiveFile(t *testing.T) {
	path := zcodeFile(t, zcodeWithOtherProviders)
	z := newZCode()
	err := z.Enable(Options{
		BaseURL: "http://127.0.0.1:8787/v1",
		APIKey:  "k",
		Models:  []lane.ModelInfo{{ID: "  "}, {ID: ""}},
	})
	if err == nil {
		t.Fatal("a roster that sanitizes to empty must be refused over a live file")
	}
	_ = path
}
