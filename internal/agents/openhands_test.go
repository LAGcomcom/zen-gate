package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

func openHandsHome(t *testing.T) string {
	t.Helper()
	home := fakeAgentHome(t)
	dir := filepath.Join(home, ".openhands")
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ohTestOptions() Options {
	return Options{
		BaseURL:      "http://127.0.0.1:8787/v1",
		APIKey:       "test-key",
		DefaultModel: "mimo-v2.6-flash-free",
		Models: []lane.ModelInfo{
			{ID: "mimo-v2.6-flash-free", ContextWindow: 1048576, MaxOutput: 131072, Vision: true, Reasoning: true},
			{ID: "kilo-auto/free", ContextWindow: 131072, MaxOutput: 32768},
			{ID: "inclusionai/ling-3.2-flash:free", ContextWindow: 262144, MaxOutput: 32768},
		},
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestOpenHandsEnableWritesProfilesAndSettings(t *testing.T) {
	dir := openHandsHome(t)
	settingsPath := filepath.Join(dir, "settings.json")
	writeJSONFile(t, settingsPath, map[string]any{
		"active_profile": nil,
		"agent_settings": map[string]any{
			"agent_kind": "openhands",
			"llm":        map[string]any{"model": "old-model", "temperature": 0.3},
		},
		"misc_settings": map[string]any{"app_preferences": map[string]any{"x": 1}},
	})
	userProfile := filepath.Join(dir, "profiles", "my-profile.json")
	writeJSONFile(t, userProfile, map[string]any{"model": "user-model", "api_key": "user-key"})

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}

	settings := readMap(t, settingsPath)
	agent := settings["agent_settings"].(map[string]any)
	llm := agent["llm"].(map[string]any)
	if got := llm["model"]; got != "openai/mimo-v2.6-flash-free" {
		t.Errorf("settings llm.model = %v, want openai/mimo-v2.6-flash-free", got)
	}
	if got := llm["base_url"]; got != "http://127.0.0.1:8787/v1" {
		t.Errorf("settings llm.base_url = %v", got)
	}
	if got := llm["api_key"]; got != "test-key" {
		t.Errorf("settings llm.api_key = %v", got)
	}
	if got := llm["temperature"]; got != 0.3 {
		t.Errorf("settings llm.temperature = %v, want preserved 0.3", got)
	}
	if got := settings["active_profile"]; got != "default" {
		t.Errorf("active_profile = %v, want default", got)
	}
	if got := settings["llm_model"]; got != "openai/mimo-v2.6-flash-free" {
		t.Errorf("legacy llm_model = %v", got)
	}
	misc := settings["misc_settings"].(map[string]any)
	if got := misc["zen_gate_managed"]; got != true {
		t.Errorf("misc_settings.zen_gate_managed = %v, want true", got)
	}
	if _, ok := misc["app_preferences"]; !ok {
		t.Errorf("misc_settings.app_preferences was dropped")
	}

	def := readMap(t, filepath.Join(dir, "profiles", "default.json"))
	if got := def["model"]; got != "openai/mimo-v2.6-flash-free" {
		t.Errorf("default profile model = %v", got)
	}
	if got := def["api_key"]; got != "test-key" {
		t.Errorf("default profile api_key = %v", got)
	}
	if got := def["base_url"]; got != "http://127.0.0.1:8787/v1" {
		t.Errorf("default profile base_url = %v", got)
	}
	if got := def["zen_gate_managed"]; got != true {
		t.Errorf("default profile marker = %v", got)
	}
	if got := def["max_input_tokens"]; got != float64(1048576) {
		t.Errorf("default profile max_input_tokens = %v", got)
	}

	// Every model gets a legal, unique profile, and the "/" / ":" in model ids
	// does not leak into the filename.
	entries, err := os.ReadDir(filepath.Join(dir, "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	models := map[string]bool{}
	re := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if !re.MatchString(name) {
			t.Errorf("profile name %q does not match OpenHands' pattern", name)
		}
		names[name] = true
		if e.Name() == "my-profile.json" {
			continue
		}
		payload := readMap(t, filepath.Join(dir, "profiles", e.Name()))
		models[payload["model"].(string)] = true
	}
	if len(names) != 4 {
		t.Errorf("profile count = %d, want 4 (default + 2 non-default models + user)", len(names))
	}
	for _, want := range []string{
		"openai/mimo-v2.6-flash-free",
		"openai/kilo-auto/free",
		"openai/inclusionai/ling-3.2-flash:free",
	} {
		if !models[want] {
			t.Errorf("missing profile for model %q (got %v)", want, models)
		}
	}
	if !names["zen-gate-kilo-auto-free"] {
		t.Errorf("kilo profile name not sanitized as expected: %v", names)
	}

	// A pre-existing user profile is never touched.
	if got := readMap(t, userProfile)["model"]; got != "user-model" {
		t.Errorf("user profile was overwritten: model = %v", got)
	}

	if enabled, _, err := o.IsEnabled(); err != nil || !enabled {
		t.Errorf("IsEnabled = %v, %v; want true", enabled, err)
	}
}

func TestOpenHandsResyncAndDisableRestore(t *testing.T) {
	dir := openHandsHome(t)
	settingsPath := filepath.Join(dir, "settings.json")
	originalSettings := map[string]any{
		"active_profile": nil,
		"agent_settings": map[string]any{
			"agent_kind": "openhands",
			"llm":        map[string]any{"model": "old-model", "temperature": 0.3},
		},
		"misc_settings": map[string]any{"app_preferences": map[string]any{"x": 1}},
	}
	writeJSONFile(t, settingsPath, originalSettings)
	userProfile := filepath.Join(dir, "profiles", "my-profile.json")
	writeJSONFile(t, userProfile, map[string]any{"model": "user-model"})

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(os.Getenv("ZEN_GATE_HOME"), "backups", "openhands", "profiles")
	before := countFiles(t, backupDir)

	// Resync with the same roster must not stack another restore point.
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	after := countFiles(t, backupDir)
	if after != before {
		t.Errorf("second Enable stacked backups: %d -> %d", before, after)
	}

	// A model leaving the roster drops its profile; a new one gets a profile.
	next := ohTestOptions()
	next.Models = []lane.ModelInfo{
		{ID: "mimo-v2.6-flash-free", ContextWindow: 1048576, MaxOutput: 131072, Vision: true},
		{ID: "new-model-free", ContextWindow: 65536, MaxOutput: 8192},
	}
	if err := o.Enable(next); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "zen-gate-kilo-auto-free.json")); !os.IsNotExist(err) {
		t.Errorf("removed model's profile still exists (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "zen-gate-new-model-free.json")); err != nil {
		t.Errorf("new model's profile missing: %v", err)
	}

	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}

	restored := readMap(t, settingsPath)
	agent := restored["agent_settings"].(map[string]any)
	llm := agent["llm"].(map[string]any)
	if got := llm["model"]; got != "old-model" {
		t.Errorf("restored llm.model = %v, want old-model", got)
	}
	if got := llm["temperature"]; got != 0.3 {
		t.Errorf("restored llm.temperature = %v", got)
	}
	if _, ok := restored["active_profile"]; !ok {
		t.Errorf("restored settings lost active_profile key")
	}
	if got := restored["active_profile"]; got != nil {
		t.Errorf("restored active_profile = %v, want nil", got)
	}
	if _, ok := restored["llm_model"]; ok {
		t.Errorf("restored settings still carry legacy llm_model")
	}
	misc := restored["misc_settings"].(map[string]any)
	if _, ok := misc["zen_gate_managed"]; ok {
		t.Errorf("restored settings still carry misc marker")
	}
	if _, ok := misc["app_preferences"]; !ok {
		t.Errorf("restored settings lost app_preferences")
	}

	if _, err := os.Stat(filepath.Join(dir, "profiles", "default.json")); !os.IsNotExist(err) {
		t.Errorf("default profile created by zen-gate was not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "zen-gate-new-model-free.json")); !os.IsNotExist(err) {
		t.Errorf("generated profile was not removed (err=%v)", err)
	}
	if _, err := os.Stat(userProfile); err != nil {
		t.Errorf("user profile was removed: %v", err)
	}
	if enabled, _, err := o.IsEnabled(); err != nil || enabled {
		t.Errorf("IsEnabled after Disable = %v, %v; want false", enabled, err)
	}
}

func TestOpenHandsRestoresPreexistingDefaultProfile(t *testing.T) {
	dir := openHandsHome(t)
	writeJSONFile(t, filepath.Join(dir, "settings.json"), map[string]any{
		"active_profile": "default",
		"agent_settings": map[string]any{"llm": map[string]any{"model": "user-default"}},
	})
	defPath := filepath.Join(dir, "profiles", "default.json")
	original := map[string]any{"schema_version": 1, "model": "user-default", "api_key": "user-key"}
	writeJSONFile(t, defPath, original)

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	if got := readMap(t, defPath)["model"]; got != "openai/mimo-v2.6-flash-free" {
		t.Errorf("default profile not overwritten: %v", got)
	}
	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}
	if got := readMap(t, defPath)["model"]; got != "user-default" {
		t.Errorf("default profile not restored: %v", got)
	}
}

func TestOpenHandsLegacyEnableRestoresOldestBackup(t *testing.T) {
	dir := openHandsHome(t)
	settingsPath := filepath.Join(dir, "settings.json")

	original := []byte(`{"agent_settings":{"llm":{"model":"original-model"}}}`)
	oldBackup, err := backupFile("openhands", settingsPath, original)
	if err != nil {
		t.Fatal(err)
	}
	// The old adapter left a managed settings.json and one backup per Enable.
	writeJSONFile(t, settingsPath, map[string]any{
		"agent_settings": map[string]any{
			"llm":              map[string]any{"model": "already-managed", "base_url": "http://127.0.0.1:8787/v1"},
			"zen_gate_managed": true,
		},
	})

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("Disable restored %q, want the oldest pre-injection backup from %s", got, oldBackup)
	}
}

func TestOpenHandsManagedSettingsWithoutBackupIsStripped(t *testing.T) {
	dir := openHandsHome(t)
	settingsPath := filepath.Join(dir, "settings.json")
	writeJSONFile(t, settingsPath, map[string]any{
		"agent_settings": map[string]any{
			"llm":              map[string]any{"model": "openai/already-managed", "base_url": "http://127.0.0.1:8787/v1"},
			"zen_gate_managed": true,
		},
		"misc_settings": map[string]any{"zen_gate_managed": true, "app_preferences": map[string]any{"x": 1}},
	})

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}

	restored := readMap(t, settingsPath)
	if ohSettingsManaged(restored) {
		t.Errorf("managed markers survived Disable with no restore point: %v", restored)
	}
	if _, ok := restored["misc_settings"].(map[string]any)["app_preferences"]; !ok {
		t.Errorf("unrelated misc settings were dropped")
	}
	if enabled, _, err := o.IsEnabled(); err != nil || enabled {
		t.Errorf("IsEnabled = %v, %v; want false", enabled, err)
	}
}

func TestOpenHandsPrunesLeftoverManagedProfiles(t *testing.T) {
	dir := openHandsHome(t)
	writeJSONFile(t, filepath.Join(dir, "settings.json"), map[string]any{
		"agent_settings": map[string]any{"llm": map[string]any{"model": "user-model"}},
	})
	stale := filepath.Join(dir, "profiles", "zen-gate-stale.json")
	writeJSONFile(t, stale, map[string]any{"schema_version": 1, "model": "openai/stale", "zen_gate_managed": true})
	userProfile := filepath.Join(dir, "profiles", "user.json")
	writeJSONFile(t, userProfile, map[string]any{"schema_version": 1, "model": "user-model"})

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("leftover managed profile was not pruned (err=%v)", err)
	}
	if _, err := os.Stat(userProfile); err != nil {
		t.Errorf("user profile was pruned: %v", err)
	}
	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("leftover managed profile reappeared on Disable (err=%v)", err)
	}
	if _, err := os.Stat(userProfile); err != nil {
		t.Errorf("user profile was removed on Disable: %v", err)
	}
}

func TestOpenHandsCreatesAndRemovesMissingSettings(t *testing.T) {
	dir := openHandsHome(t)
	settingsPath := filepath.Join(dir, "settings.json")

	o := newOpenHands()
	if err := o.Enable(ohTestOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath); err != nil {
		t.Fatalf("Enable did not create settings.json: %v", err)
	}
	if err := o.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("Disable left a settings.json it created (err=%v)", err)
	}
}

func TestOpenHandsProfileNames(t *testing.T) {
	cases := map[string]string{
		"kilo-auto/free":                  "zen-gate-kilo-auto-free",
		"inclusionai/ling-3.2-flash:free": "zen-gate-inclusionai-ling-3.2-flash-free",
		"a:b":                             "zen-gate-a-b",
		"...":                             "zen-gate-model",
	}
	for in, want := range cases {
		if got := ohProfileNameFor(in); got != want {
			t.Errorf("ohProfileNameFor(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("x", 100)
	name := ohProfileNameFor(long)
	if len(name) > 64 {
		t.Errorf("long profile name = %d bytes, want <= 64", len(name))
	}
	if !ohProfileNameRe.MatchString(name) {
		t.Errorf("long profile name %q is invalid", name)
	}

	used := map[string]bool{"zen-gate-kilo-auto-free": true}
	next := ohUniqueName("zen-gate-kilo-auto-free", used)
	if next == "zen-gate-kilo-auto-free" || !ohProfileNameRe.MatchString(next) {
		t.Errorf("collision not resolved: %q", next)
	}
}
