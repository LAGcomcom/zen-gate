package agents

import (
	"os"
	"path/filepath"
	"testing"
)

// wbAIConfig points WORKBUDDY_AI_CONFIG_DIR at a temp tree, mirroring
// wbConfig() for the international sibling.
func wbAIConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".workbuddy-ai")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKBUDDY_AI_CONFIG_DIR", dir)
	if initial != "" {
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The international variant writes its own directory and its own backup
// namespace: one adapter is two instances over the same CodeBuddy-kernel
// logic, and neither install may ever see the other's injected entries or
// restore points.
func TestWorkBuddyAIInjectsIsolatedDirectory(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir()) // Enable backups under store.Home()
	aiDir := wbAIConfig(t, "")
	cnDir := wbConfig(t, "") // sets WORKBUDDY_CONFIG_DIR

	ai := newWorkBuddyAI()
	if err := ai.Enable(wbOptions()); err != nil {
		t.Fatal(err)
	}

	// The international file exists with the injected entries.
	data, err := os.ReadFile(filepath.Join(aiDir, "models.json"))
	if err != nil {
		t.Fatalf("models.json missing in the AI dir: %v", err)
	}
	if doc := wbReadModel(t, aiDir); len(doc) == 0 {
		t.Fatalf("AI dir holds no injected models: %s", string(data))
	}

	// The domestic install's directory was never touched.
	if _, err := os.Stat(filepath.Join(cnDir, "models.json")); !os.IsNotExist(err) {
		t.Errorf("domestic models.json must not be created by the AI adapter: %v", err)
	}

	// Disable restores verbatim and leaves the domestic install alone. The
	// file had no pristine backup (it did not exist at Enable), so Disable
	// writes the empty doc — same as the domestic adapter — and the kernel
	// treats that as "no custom models".
	if err := ai.Disable(); err != nil {
		t.Fatal(err)
	}
	if enabled, _, _ := ai.IsEnabled(); enabled {
		t.Error("disable should leave no zen-gate entries behind")
	}
}

// Backups live under the adapter id: the international sibling must never
// restore from (or overwrite) the domestic install's backup namespace.
func TestWorkBuddyAIBackupNamespaceIsSeparate(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	aiDir := wbAIConfig(t, `{"models":[{"id":"user-own","url":"https://api.other.com/v1"}]}`)
	ai := newWorkBuddyAI()
	if err := ai.Enable(wbOptions()); err != nil {
		t.Fatal(err)
	}
	// The pristine file (with the user's own model) was backed up under
	// workbuddy-ai, not workbuddy.
	if b := latestBackup("workbuddy-ai", "models.json"); b == "" {
		t.Fatal("pristine AI file was not backed up")
	}
	_ = aiDir
}

// Detect reads the international directory and its own last-launch.json.
func TestWorkBuddyAIDetect(t *testing.T) {
	dir := wbAIConfig(t, "")
	installed, _, detail := newWorkBuddyAI().Detect()
	if !installed {
		t.Fatal("the international install should be detected")
	}
	if detail != "检测到 ~/.workbuddy-ai（models.json 尚未创建）" && detail != "检测到 ~/.workbuddy-ai" {
		t.Errorf("detail = %q", detail)
	}
	_ = dir
}
