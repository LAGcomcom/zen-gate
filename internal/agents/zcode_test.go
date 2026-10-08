package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zen-gate/internal/lane"
)

// Regression coverage for ZCode's provider_config.json: ZCode validates the
// file with a strict Zod schema and degrades the whole personal-provider
// registry to empty on failure. Two zen-gate write bugs triggered exactly
// that: a missing `manualProviderModelRules` array (field was omitempty) and
// `contextWindow: 0` model rules. The user saw "ZCode 没有注入" plus a broken
// manual-add flow, because every provider — not just ours — vanished.

func zcodeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir on Windows
	t.Setenv("HOME", dir)        // os.UserHomeDir elsewhere
	return filepath.Join(dir, ".zcode", "v2", "provider_config.json")
}

func zcodeWriteInitial(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func zcodeOptions() Options {
	return Options{
		BaseURL: "http://127.0.0.1:8787/v1",
		APIKey:  "ofm-test",
		Models: []lane.ModelInfo{
			{ID: "mimo-v2.6-flash-free", Name: "MiMo V2.6 Flash", Reasoning: true, ContextWindow: 1048576, MaxOutput: 131072},
			{ID: "space-bunny-free", Name: "Space Bunny", ContextWindow: 128000, MaxOutput: 4096},
			// Unknown capacity: must be skipped in modelConfigRules, never
			// written with a 0 context window.
			{ID: "unknown-cap-free", Name: "Unknown Cap"},
		},
		DefaultModel: "mimo-v2.6-flash-free",
	}
}

func zcodeReadDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("provider_config.json 不是合法 JSON: %v\n%s", err, data)
	}
	return doc
}

// zcodeAssertSchemaValid asserts the two constraints ZCode's Zod schema is
// known to enforce: manualProviderModelRules/providerModelRules must be
// arrays (present, not null) and every contextWindow must be > 0.
func zcodeAssertSchemaValid(t *testing.T, doc map[string]any) {
	t.Helper()
	cfg, _ := doc["config"].(map[string]any)
	if cfg == nil {
		t.Fatal("config 缺失")
	}
	pcr, _ := cfg["providerConfigRules"].(map[string]any)
	if pcr == nil {
		t.Fatal("providerConfigRules 缺失")
	}
	if _, ok := pcr["providerRules"].([]any); !ok {
		t.Fatalf("providerRules 必须是数组, got %T", pcr["providerRules"])
	}
	mcr, ok := cfg["modelConfigRules"].(map[string]any)
	if !ok {
		t.Fatal("modelConfigRules 缺失")
	}
	for _, key := range []string{"providerModelRules", "manualProviderModelRules"} {
		if _, ok := mcr[key].([]any); !ok {
			t.Fatalf("%s 必须是数组 (Zod: expected array, received %v), got %T", key, mcr[key], mcr[key])
		}
	}
	for i, r := range mcr["providerModelRules"].([]any) {
		rule, _ := r.(map[string]any)
		cfgAny, _ := rule["config"].(map[string]any)
		props, _ := cfgAny["properties"].(map[string]any)
		if props == nil {
			continue
		}
		if cw, ok := props["contextWindow"].(float64); ok && cw <= 0 {
			t.Fatalf("providerModelRules[%d].contextWindow = %v (Zod: expected number to be >0)", i, cw)
		}
	}
}

// The field-report shape: a config zen-gate wrote before the fix — no
// manualProviderModelRules, a stale model rule with contextWindow 0. Enable
// must rewrite it into a schema-valid file without losing the other provider.
const zcodeBrokenFile = `{
  "schemaVersion": 1,
  "config": {
    "providerConfigRules": {
      "providerRules": [
        {
          "providerId": "d580e93f-6982-4f3a-97eb-1e8e60d2139f",
          "providerName": "其他供应商",
          "enabled": true,
          "config": {
            "group": "standard-personal",
            "access": {"type": "api-key", "apiKey": "sk-other"},
            "api": {"type": "openai-chat-completions", "baseUrl": "https://other.example/v1"},
            "personalModelIds": ["other-model"],
            "modelOrder": ["other-model"]
          }
        }
      ]
    },
    "modelConfigRules": {
      "providerModelRules": [
        {
          "modelId": "other-model",
          "providerId": "d580e93f-6982-4f3a-97eb-1e8e60d2139f",
          "config": {"properties": {"contextWindow": 1000000}}
        },
        {
          "modelId": "old-zero-cap",
          "providerId": "5a3e8f10-1c2b-4d3e-9f4a-0b7c6d5e4a3b",
          "config": {"properties": {"contextWindow": 0}}
        }
      ]
    }
  }
}`

func TestZcodeEnableProducesSchemaValidFile(t *testing.T) {
	path := zcodeHome(t)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)

	mcr := doc["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	// The unknown-capacity model got a provider entry (personalModelIds) but
	// no model rule — a 0 context window must never reach the file.
	for _, r := range mcr["providerModelRules"].([]any) {
		if r.(map[string]any)["modelId"] == "unknown-cap-free" {
			t.Fatal("contextWindow 未知的模型不应生成 modelConfigRules 条目")
		}
	}
	// Reasoning model carries the thought-level selector.
	found := false
	for _, r := range mcr["providerModelRules"].([]any) {
		rule := r.(map[string]any)
		if rule["modelId"] != "mimo-v2.6-flash-free" {
			continue
		}
		found = true
		rc := rule["config"].(map[string]any)
		if _, ok := rc["optionSpecs"]; !ok {
			t.Fatal("推理模型缺少 reasoningLevel optionSpecs")
		}
	}
	if !found {
		t.Fatal("推理模型的 modelConfigRules 条目缺失")
	}
}

func TestZcodeEnableRepairsBrokenFileAndKeepsOthers(t *testing.T) {
	path := zcodeHome(t)
	zcodeWriteInitial(t, path, zcodeBrokenFile)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)

	cfg := doc["config"].(map[string]any)
	// The third-party provider rule and its model rule survive.
	rules := cfg["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	others := 0
	for _, r := range rules {
		if r.(map[string]any)["providerName"] == "其他供应商" {
			others++
		}
	}
	if others != 1 {
		t.Fatalf("其他供应商规则应原样保留 1 条, got %d", others)
	}
	mcr := cfg["modelConfigRules"].(map[string]any)
	otherRules := 0
	for _, r := range mcr["providerModelRules"].([]any) {
		if r.(map[string]any)["modelId"] == "other-model" {
			otherRules++
		}
	}
	if otherRules != 1 {
		t.Fatalf("其他供应商的 model 规则应原样保留, got %d", otherRules)
	}
	// zen-gate's own stale rule (contextWindow 0) is gone, not kept.
	for _, r := range mcr["providerModelRules"].([]any) {
		if r.(map[string]any)["modelId"] == "old-zero-cap" {
			t.Fatal("旧的 contextWindow=0 条目应被替换移除")
		}
	}
}

func TestZcodeEnablePreservesManualRules(t *testing.T) {
	path := zcodeHome(t)
	// ZCode itself writes manual rules when the user adds models by hand;
	// they must survive zen-gate's rewrite untouched.
	manual := replaceOnce(t, zcodeBrokenFile,
		`"providerModelRules": [`,
		`"manualProviderModelRules": [{"modelId":"manual-one","providerId":"d580e93f-6982-4f3a-97eb-1e8e60d2139f","config":{"properties":{"contextWindow":8192}}}],
      "providerModelRules": [`)
	zcodeWriteInitial(t, path, manual)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)

	mcr := doc["config"].(map[string]any)["modelConfigRules"].(map[string]any)
	manuals, ok := mcr["manualProviderModelRules"].([]any)
	if !ok || len(manuals) != 1 {
		t.Fatalf("用户的 manualProviderModelRules 应原样保留 1 条, got %v", mcr["manualProviderModelRules"])
	}
	if manuals[0].(map[string]any)["modelId"] != "manual-one" {
		t.Fatalf("manual 规则内容被改写: %v", manuals[0])
	}
}

func TestZcodeEnableIsIdempotent(t *testing.T) {
	path := zcodeHome(t)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)

	rules := doc["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	n := 0
	for _, r := range rules {
		if r.(map[string]any)["providerName"] == providerName {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("重复 Enable 后 Zen Gate 规则应恰好 1 条, got %d", n)
	}
}

func TestZcodeEnableCreatesMissingDirs(t *testing.T) {
	// A machine where ZCode never ran has no ~/.zcode/v2 — Enable must create
	// it instead of failing with "cannot find the path".
	path := zcodeHome(t)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)
}

func TestZcodeDisableKeepsSchemaValidFile(t *testing.T) {
	path := zcodeHome(t)
	zcodeWriteInitial(t, path, zcodeBrokenFile)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	if err := z.Disable(); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	zcodeAssertSchemaValid(t, doc)

	rules := doc["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	for _, r := range rules {
		if r.(map[string]any)["providerName"] == providerName {
			t.Fatal("Disable 后 Zen Gate 规则应被移除")
		}
	}
}

func TestZcodeProviderRuleShape(t *testing.T) {
	path := zcodeHome(t)
	z := newZCode()

	if err := z.Enable(zcodeOptions()); err != nil {
		t.Fatal(err)
	}
	doc := zcodeReadDoc(t, path)
	rules := doc["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	var rule map[string]any
	for _, r := range rules {
		if r.(map[string]any)["providerName"] == providerName {
			rule = r.(map[string]any)
		}
	}
	if rule == nil {
		t.Fatal("Zen Gate provider 规则缺失")
	}
	if rule["enabled"] != true {
		t.Fatalf("enabled 应为 true, got %v", rule["enabled"])
	}
	cfg := rule["config"].(map[string]any)
	api := cfg["api"].(map[string]any)
	if api["type"] != "openai-chat-completions" || api["baseUrl"] != "http://127.0.0.1:8787/v1" {
		t.Fatalf("api 形状不对: %v", api)
	}
	access := cfg["access"].(map[string]any)
	if access["type"] != "api-key" || access["apiKey"] != "ofm-test" {
		t.Fatalf("access 形状不对: %v", access)
	}
	ids := cfg["personalModelIds"].([]any)
	if len(ids) != 3 {
		t.Fatalf("personalModelIds 应含全部 3 个模型(含未知容量的), got %v", ids)
	}
}

func replaceOnce(t *testing.T, src, old, new string) string {
	t.Helper()
	idx := indexOf(src, old)
	if idx < 0 {
		t.Fatalf("替换锚点未找到: %q", old)
	}
	return src[:idx] + new + src[idx+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
