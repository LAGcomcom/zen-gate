package agents

import (
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

// Regression coverage for the Codex config.toml injection: a config that
// already carries a top-level `model_catalog_json` must never end up with the
// key twice — TOML rejects duplicate keys ("Cannot overwrite a value") and
// Codex refuses to parse the whole file.

func codexOpts() Options {
	return Options{
		BaseURL:      "http://127.0.0.1:8787/v1",
		APIKey:       "ofm-test-key",
		DefaultModel: "mimo-v2.6-flash-free",
		Models: []lane.ModelInfo{
			{ID: "mimo-v2.6-flash-free", Name: "MiMo V2.6 Flash", Reasoning: true, Vision: true, ContextWindow: 1048576, MaxOutput: 131072},
			{ID: "locked-free", Name: "Region Locked", RegionSensitive: true, ContextWindow: 128000, MaxOutput: 8192},
		},
	}
}

const codexUserCatalogLine = `model_catalog_json = "C:/Users/u/.codex/models.json"`

func codexUserConfig() string {
	return strings.Join([]string{
		`model_provider = "custom"`,
		`model = "gpt-5.1"`,
		codexUserCatalogLine,
		``,
		`[model_providers.custom]`,
		`name = "custom"`,
		`base_url = "https://example.com/v1"`,
		`wire_api = "responses"`,
	}, "\n")
}

// codexTopLevelKeys maps each top-level key (before the first [table] header)
// to every line carrying it — duplicate keys show up as len > 1.
func codexTopLevelKeys(t *testing.T, src string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, l := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "[") {
			break
		}
		if eq := strings.Index(trim, "="); eq > 0 {
			key := strings.TrimSpace(trim[:eq])
			out[key] = append(out[key], l)
		}
	}
	return out
}

func TestCodexEnableDedupesExistingCatalogKey(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	stash := map[string]string{}
	out := c.applyEnable(codexUserConfig(), o, o.DefaultModel, "C:/Users/u/.codex/zen-gate-catalog.json", stash)

	keys := codexTopLevelKeys(t, out)
	if n := len(keys["model_catalog_json"]); n != 1 {
		t.Fatalf("model_catalog_json appears %d times, want 1\n%s", n, out)
	}
	if !strings.Contains(keys["model_catalog_json"][0], "zen-gate-catalog.json") {
		t.Fatalf("active catalog key is not zen-gate's: %q", keys["model_catalog_json"][0])
	}
	if got := stash["model_catalog_json"]; got != codexUserCatalogLine {
		t.Fatalf("stash lost the user's original line: got %q, want %q", got, codexUserCatalogLine)
	}
	// model_provider/model are rewritten in place — still exactly one each.
	if n := len(keys["model_provider"]); n != 1 || !strings.Contains(keys["model_provider"][0], `"zen_gate"`) {
		t.Fatalf("model_provider not rewritten in place: %q", keys["model_provider"])
	}
	// The managed catalog key must sit before the first [table] header.
	keyIdx := strings.Index(out, "model_catalog_json")
	tblIdx := strings.Index(out, "[model_providers.custom]")
	if keyIdx < 0 || tblIdx < 0 || keyIdx > tblIdx {
		t.Fatalf("catalog key must precede the first table (key at %d, table at %d)", keyIdx, tblIdx)
	}
}

func TestCodexEnableIsIdempotent(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	catPath := "C:/Users/u/.codex/zen-gate-catalog.json"

	stash := map[string]string{}
	once := c.applyEnable(codexUserConfig(), o, o.DefaultModel, catPath, stash)

	// Second enable seeds the stash from the persisted state file, exactly
	// like Enable does — zen-gate's own rewritten lines must not be recorded
	// as the user's originals.
	two := map[string]string{}
	for k, v := range stash {
		two[k] = v
	}
	twice := c.applyEnable(once, o, o.DefaultModel, catPath, two)

	if n := len(codexTopLevelKeys(t, twice)["model_catalog_json"]); n != 1 {
		t.Fatalf("model_catalog_json appears %d times after re-enable\n%s", n, twice)
	}
	if n := strings.Count(twice, codexMarkerBegin); n != 1 {
		t.Fatalf("provider block appears %d times after re-enable\n%s", n, twice)
	}
	for _, k := range []string{"model_catalog_json", "model_provider", "model"} {
		if two[k] != stash[k] {
			t.Fatalf("re-enable clobbered stash[%q]: got %q, want %q", k, two[k], stash[k])
		}
	}
}

// A config already broken by the old bug — two top-level model_catalog_json
// lines — must come out valid after Enable, not stay broken.
func TestCodexEnableRepairsAlreadyBrokenConfig(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	broken := strings.Replace(codexUserConfig(), codexUserCatalogLine,
		codexUserCatalogLine+"\nmodel_catalog_json = 'C:/Users/u/.codex/zen-gate-catalog.json'", 1)

	stash := map[string]string{}
	out := c.applyEnable(broken, o, o.DefaultModel, "C:/Users/u/.codex/zen-gate-catalog.json", stash)

	if n := len(codexTopLevelKeys(t, out)["model_catalog_json"]); n != 1 {
		t.Fatalf("broken config still has %d model_catalog_json keys after enable\n%s", n, out)
	}
	if stash["model_catalog_json"] != codexUserCatalogLine {
		t.Fatalf("stash should record the first-seen original, got %q", stash["model_catalog_json"])
	}
}

func TestCodexEnableFreshConfig(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	stash := map[string]string{}
	out := c.applyEnable("disable_response_storage = true\n", o, o.DefaultModel, "C:/Users/u/.codex/zen-gate-catalog.json", stash)

	if n := len(codexTopLevelKeys(t, out)["model_catalog_json"]); n != 1 {
		t.Fatalf("model_catalog_json appears %d times on a fresh config\n%s", n, out)
	}
	if _, ok := stash["model_catalog_json"]; ok {
		t.Fatalf("stash must not record a catalog key that never existed: %q", stash["model_catalog_json"])
	}
}

// A config that starts with a [table] (no top-level region at all) still gets
// exactly one catalog key, placed before that first table.
func TestCodexEnableInsertsKeyWhenConfigStartsWithTable(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	stash := map[string]string{}
	out := c.applyEnable("[model_providers.custom]\nname = \"x\"\n", o, o.DefaultModel, "C:/Users/u/.codex/zen-gate-catalog.json", stash)

	keys := codexTopLevelKeys(t, out)
	if n := len(keys["model_catalog_json"]); n != 1 {
		t.Fatalf("model_catalog_json appears %d times\n%s", n, out)
	}
	if keyIdx, tblIdx := strings.Index(out, "model_catalog_json"), strings.Index(out, "[model_providers.custom]"); keyIdx > tblIdx {
		t.Fatalf("catalog key must precede the first table (key at %d, table at %d)", keyIdx, tblIdx)
	}
}

func TestCodexDisableRestoresRemovedCatalogKey(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	catPath := "C:/Users/u/.codex/zen-gate-catalog.json"

	stash := map[string]string{}
	enabled := c.applyEnable(codexUserConfig(), o, o.DefaultModel, catPath, stash)

	// Same order as Disable: managed blocks out first, then the stash undo.
	clean := stripMarkers(stripMarkers(enabled, codexMarkerBegin, codexMarkerEnd), codexCatalogKeyBegin, codexCatalogKeyEnd)
	restored := applyDisable(clean, stash)

	keys := codexTopLevelKeys(t, restored)
	if n := len(keys["model_catalog_json"]); n != 1 {
		t.Fatalf("model_catalog_json appears %d times after disable\n%s", n, restored)
	}
	if keys["model_catalog_json"][0] != codexUserCatalogLine {
		t.Fatalf("user's catalog line not restored verbatim: %q", keys["model_catalog_json"][0])
	}
	if keys["model_provider"][0] != `model_provider = "custom"` {
		t.Fatalf("model_provider not restored: %q", keys["model_provider"])
	}
	if keys["model"][0] != `model = "gpt-5.1"` {
		t.Fatalf("model not restored: %q", keys["model"])
	}
	if strings.Contains(restored, codexMarkerBegin) {
		t.Fatal("managed block survived disable")
	}
	if !strings.Contains(restored, "[model_providers.custom]") {
		t.Fatal("user's own provider table vanished")
	}
}

// The restored key must land in the top-level region, not inside a table.
func TestCodexDisableRestoresKeyBeforeFirstTable(t *testing.T) {
	c := newCodex()
	o := codexOpts()
	catPath := "C:/Users/u/.codex/zen-gate-catalog.json"

	stash := map[string]string{}
	enabled := c.applyEnable(codexUserConfig(), o, o.DefaultModel, catPath, stash)
	clean := stripMarkers(stripMarkers(enabled, codexMarkerBegin, codexMarkerEnd), codexCatalogKeyBegin, codexCatalogKeyEnd)
	restored := applyDisable(clean, stash)

	keyIdx := strings.Index(restored, codexUserCatalogLine)
	tblIdx := strings.Index(restored, "[model_providers.custom]")
	if keyIdx < 0 || tblIdx < 0 || keyIdx > tblIdx {
		t.Fatalf("restored catalog key must precede the first table (key at %d, table at %d)\n%s", keyIdx, tblIdx, restored)
	}
}
