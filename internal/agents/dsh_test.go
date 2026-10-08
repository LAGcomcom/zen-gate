package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

const dshSamplePatch = `# 用户注释必须原样保留
- id: ui-settings
  config:
    enabled: true
- id: llm-pi-ai
  config:
    providers:
      jiyuan:
        displayName: 基元
        api: openai-completions
        baseURL: https://example.studio/v1
        models:
          - id: glm-5.3-flash
            contextWindow: 1048576
`

func writeDSHFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"p","private":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cordis.patch.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dshOptions() Options {
	return Options{
		BaseURL: "http://127.0.0.1:8321/v1",
		APIKey:  "k-123",
		Models: []lane.ModelInfo{
			{ID: "mimo-v2.6-flash-free", Name: "MiMo V2.6 Flash", ContextWindow: 1048576, MaxOutput: 131072},
			{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash", ContextWindow: 1048576, MaxOutput: 65536, Vision: true},
		},
	}
}

func TestDSHUpsertMergesIntoExistingEntry(t *testing.T) {
	d := newDSH()
	dir := writeDSHFixture(t, dshSamplePatch)
	if err := d.upsertProvider(dir, dshOptions()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "zen-gate") || !strings.Contains(s, "http://127.0.0.1:8321/v1") {
		t.Fatalf("provider not merged:\n%s", s)
	}
	if !strings.Contains(s, "jiyuan") || !strings.Contains(s, "example.studio") || !strings.Contains(s, "用户注释必须原样保留") {
		t.Fatalf("existing entry damaged:\n%s", s)
	}
	if strings.Count(s, "id: llm-pi-ai") != 1 {
		t.Fatalf("must merge in place, not add a second entry:\n%s", s)
	}
	if !strings.Contains(s, "- image") {
		t.Fatalf("vision modality not written:\n%s", s)
	}
}

func TestDSHDisableKeepsUserContent(t *testing.T) {
	d := newDSH()
	dir := writeDSHFixture(t, dshSamplePatch)
	if err := d.upsertProvider(dir, dshOptions()); err != nil {
		t.Fatal(err)
	}
	if err := d.removeProvider(dir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"用户注释必须原样保留", "jiyuan", "example.studio", "glm-5.3-flash"} {
		if !strings.Contains(s, want) {
			t.Fatalf("user content lost (%q):\n%s", want, s)
		}
	}
	if strings.Contains(s, "zen-gate") || strings.Contains(s, "Bearer k-123") {
		t.Fatalf("zen-gate provider survived disable:\n%s", s)
	}
}

func TestDSHAppendCreatesEntryWhenAbsent(t *testing.T) {
	d := newDSH()
	dir := writeDSHFixture(t, "# 仅注释的空层\n")
	if err := d.upsertProvider(dir, dshOptions()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "id: llm-pi-ai") || !strings.Contains(s, "zen-gate") {
		t.Fatalf("entry not appended:\n%s", s)
	}
	if !strings.Contains(s, "仅注释的空层") {
		t.Fatalf("existing comments lost:\n%s", s)
	}
}

func TestDSHStripLegacyPlugin(t *testing.T) {
	d := newDSH()
	dir := t.TempDir()
	pkg := `{"dependencies":{"dsh-our-free-model":"1.3.2"},"dsh":{"profile":{"bundles":["@deepseek-ai/dsh-base","dsh-our-free-model"]}}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "dsh-our-free-model"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.stripLegacyPlugin(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	s := string(data)
	if strings.Contains(s, "dsh-our-free-model") {
		t.Fatalf("legacy plugin wiring survived:\n%s", s)
	}
	if !strings.Contains(s, "@deepseek-ai/dsh-base") {
		t.Fatalf("unrelated bundles must survive:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "dsh-our-free-model")); !os.IsNotExist(err) {
		t.Fatal("node_modules copy must be removed")
	}
}

func TestDSHIsEnabledRequiresZenGateKey(t *testing.T) {
	d := newDSH()
	// A hand-added jiyuan provider alone must not read as enabled.
	dir := writeDSHFixture(t, dshSamplePatch)
	enabled := false
	// drive the same code path IsEnabled uses, scoped to the fixture
	doc, err := d.readPatch(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if providers := d.findProviders(doc); providers != nil && mapValue(providers, "zen-gate") != nil {
		enabled = true
	}
	if enabled {
		t.Fatal("jiyuan-only patch must not read as zen-gate-enabled")
	}
	if err := d.upsertProvider(dir, dshOptions()); err != nil {
		t.Fatal(err)
	}
	doc, err = d.readPatch(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	providers := d.findProviders(doc)
	if providers == nil || mapValue(providers, "zen-gate") == nil {
		t.Fatal("zen-gate key missing after upsert")
	}
}
