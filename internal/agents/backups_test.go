package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The backups directory is the only undo for an injection into a config
// zen-gate does not own. Two writes in the same clock second used to collide on
// one filename, so the second one overwrote the first restore point.
func TestBackupFileKeepsEveryCopyFromTheSameSecond(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	first, err := backupFile("zcode", filepath.Join("cfg", "provider_config.json"), []byte(`{"v":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := backupFile("zcode", filepath.Join("cfg", "provider_config.json"), []byte(`{"v":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both backups resolved to %s; the first copy was overwritten", first)
	}
	if got := readFile(t, first); got != `{"v":1}` {
		t.Errorf("first backup holds %q, want the original it was given", got)
	}
	if got := readFile(t, second); got != `{"v":2}` {
		t.Errorf("second backup holds %q", got)
	}
}

func TestLatestBackupPicksTheNewestCopy(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	if _, err := backupFile("zcode", filepath.Join("cfg", "provider_config.json"), []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	second, err := backupFile("zcode", filepath.Join("cfg", "provider_config.json"), []byte(`{"v":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := latestBackup("zcode", "provider_config.json"); got != second {
		t.Errorf("latestBackup = %q, want %q", got, second)
	}
}

// Both of these adapters rewrite a whole foreign document (provider rules,
// model rules, other vendors' entries), so a copy has to exist before the
// write — a schema zen-gate does not recognise should be recoverable.
func TestZCodeEnableBacksUpBeforeRewriting(t *testing.T) {
	home := fakeAgentHome(t)
	path := zcodeFileAt(t, home, `{"schemaVersion":1,"config":{"note":"mine"}}`)
	if err := newZCode().Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	if !hasBackupContaining(t, "zcode", `"note":"mine"`) {
		t.Errorf("Enable rewrote %s without backing up the previous content", path)
	}
}

func TestZCodeDisableBacksUpBeforeRewriting(t *testing.T) {
	home := fakeAgentHome(t)
	path := zcodeFileAt(t, home, `{"schemaVersion":1,"config":{"note":"mine"}}`)
	z := newZCode()
	if err := z.Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	if err := z.Disable(); err != nil {
		t.Fatal(err)
	}
	if !hasBackupContaining(t, "zcode", `"note":"mine"`) {
		t.Errorf("Disable rewrote %s without a backup of the state it replaced", path)
	}
}

func TestOpenCodeEnableBacksUpBeforeRewriting(t *testing.T) {
	dir := openCodeHome(t)
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newOpenCode().Enable(zOptions()); err != nil {
		t.Fatal(err)
	}
	if !hasBackupContaining(t, "opencode", `"theme":"dark"`) {
		t.Errorf("Enable rewrote %s without backing up the previous content", path)
	}
}

// read() promises to refuse an unknown format, but Enable used to swallow that
// error and start from an empty document — which then replaced the file the
// adapter had just declared it would not touch.
func TestZCodeEnableRefusesAnUnparseableConfig(t *testing.T) {
	home := fakeAgentHome(t)
	broken := `{"schemaVersion":1, "config": { oops`
	path := zcodeFileAt(t, home, broken)
	if err := newZCode().Enable(zOptions()); err == nil {
		t.Fatalf("Enable reported success after rewriting a file it could not parse")
	}
	if got := readFile(t, path); got != broken {
		t.Errorf("the unreadable config was overwritten:\n%s", got)
	}
}

func zcodeFileAt(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider_config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasBackupContaining(t *testing.T, agentID, substr string) bool {
	t.Helper()
	dir := filepath.Join(os.Getenv("ZEN_GATE_HOME"), "backups", agentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.Contains(readFile(t, filepath.Join(dir, e.Name())), substr) {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
