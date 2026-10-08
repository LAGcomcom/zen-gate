package agents

import (
	"encoding/json"
	"fmt"
	"os"
)

// zCode injects a personal provider rule into ZCode's provider_config.json.
// The structure mirrors the file ZCode itself writes for third-party
// OpenAI-compatible providers ("api.type": "openai-chat-completions").

type zcode struct{}

func newZCode() *zcode { return &zcode{} }

func (z *zcode) Meta() (string, string, string) {
	return "zcode", "ZCode", "~/.zcode/v2/provider_config.json 注入 openai-chat-completions 渠道"
}

func (z *zcode) configPath() string { return homePath(".zcode", "v2", "provider_config.json") }

func (z *zcode) Detect() (bool, string, string) {
	if _, err := os.Stat(z.configPath()); err == nil {
		return true, "", "检测到 ZCode 配置"
	}
	if _, err := os.Stat(homePath(".zcode")); err == nil {
		return true, "", "检测到 ~/.zcode，但尚无 provider_config.json（首次使用后生成）"
	}
	return false, "", "未检测到 ZCode"
}

const providerName = "Zen Gate"

// stableProviderID is a fixed identity for our provider rule so the matching
// modelConfigRules entries can be replaced idempotently across enables.
const stableProviderID = "5a3e8f10-1c2b-4d3e-9f4a-0b7c6d5e4a3b"

// reasoningLevelSpec declares the thought-level selector for one model:
// values feed ZCode's picker; the CEL map merges {"reasoning_effort": level}
// into the outgoing request body, which the gateway maps onto the effort
// budget (2048 / 8192 / model capacity).
func reasoningLevelSpec() map[string]any {
	return map[string]any{
		"values": []string{"light", "balanced", "deep"},
		"map":    `{'reasoning_effort': reasoningLevel}`,
	}
}

// The document is walked as generic JSON, never decoded into a struct that
// lists the fields zen-gate cares about. Re-marshalling such a struct deletes
// every key zen-gate has not modelled — ZCode's feature flags, its updatedAt,
// and any provider section a plugin added — and deleting
// manualProviderModelRules outright is what made ZCode reject the file and boot
// with an empty in-memory config, losing the user's whole provider list.

func (z *zcode) read() (map[string]any, error) {
	data, err := os.ReadFile(z.configPath())
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("provider_config.json 无法解析（%w）；为避免破坏未知格式，zen-gate 拒绝写入", err)
	}
	return doc, nil
}

func (z *zcode) write(doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	path := z.configPath()
	backupBeforeWrite("zcode", path)
	return atomicWrite(path, data)
}

func (z *zcode) IsEnabled() (bool, string, error) {
	doc, err := z.read()
	if err != nil {
		return false, "", nil
	}
	for _, raw := range ruleList(doc) {
		if entryName(raw) == providerName {
			m, _ := raw.(map[string]any)
			enabled, _ := m["enabled"].(bool)
			return enabled, "", nil
		}
	}
	return false, "", nil
}

func (z *zcode) Enable(o Options) error {
	doc, err := z.read()
	if err != nil {
		if !os.IsNotExist(err) {
			// Present but unreadable: starting from an empty document here would
			// overwrite the very file read() just refused to interpret.
			return err
		}
		doc = map[string]any{}
	}
	if v, ok := doc["schemaVersion"].(float64); !ok || v == 0 {
		doc["schemaVersion"] = 1
	}
	cfg := childMap(doc, "config")

	pcr := childMap(cfg, "providerConfigRules")
	providerRules, err := arrayField(pcr, "providerRules")
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(o.Models))
	for _, m := range o.Models {
		ids = append(ids, m.ID)
	}
	rule := map[string]any{
		"providerId":   stableProviderID,
		"providerName": providerName,
		"enabled":      true,
		"config": map[string]any{
			"group": "standard-personal",
			"access": map[string]any{
				"type":   "api-key",
				"apiKey": o.APIKey,
			},
			"api": map[string]any{
				"type":    "openai-chat-completions",
				"baseUrl": o.BaseURL,
			},
			"personalModelIds": ids,
			"modelOrder":       ids,
		},
	}
	kept := make([]any, 0, len(providerRules)+1)
	for _, existing := range providerRules {
		if entryName(existing) == providerName {
			continue // replace our own stale entry, keep everything else
		}
		kept = append(kept, existing)
	}
	kept = append(kept, rule)
	pcr["providerRules"] = kept

	// Model rules: capability metadata per model — context window plus the
	// thought-level selector for reasoning models.
	mcr := childMap(cfg, "modelConfigRules")
	modelRules, err := arrayField(mcr, "providerModelRules")
	if err != nil {
		return err
	}
	kept = make([]any, 0, len(modelRules)+len(o.Models))
	for _, existing := range modelRules {
		if entryProviderID(existing) == stableProviderID {
			continue // replace our own stale entries, keep everything else
		}
		kept = append(kept, existing)
	}
	for _, m := range o.Models {
		entry := map[string]any{
			"modelId":    m.ID,
			"providerId": stableProviderID,
			"config": map[string]any{
				"properties": map[string]any{"contextWindow": m.ContextWindow},
			},
		}
		if m.Reasoning {
			entry["config"].(map[string]any)["optionSpecs"] = map[string]any{
				"reasoningLevel": reasoningLevelSpec(),
			}
		}
		kept = append(kept, entry)
	}
	mcr["providerModelRules"] = kept
	// ZCode's zod schema demands this key be an array, and an empty one is
	// valid — but it is a list zen-gate never writes, so it has to survive
	// being absent from our side too.
	if _, present := mcr["manualProviderModelRules"]; !present {
		mcr["manualProviderModelRules"] = []any{}
	}
	return z.write(doc)
}

func (z *zcode) Disable() error {
	doc, err := z.read()
	if err != nil {
		return nil // nothing (or unreadable) — leave the file alone
	}
	cfg, _ := doc["config"].(map[string]any)
	if cfg == nil {
		return nil
	}
	if pcr, ok := cfg["providerConfigRules"].(map[string]any); ok {
		rules, err := arrayField(pcr, "providerRules")
		if err != nil {
			return err
		}
		kept := make([]any, 0, len(rules))
		for _, existing := range rules {
			if entryName(existing) == providerName {
				continue
			}
			kept = append(kept, existing)
		}
		pcr["providerRules"] = kept
	}
	if mcr, ok := cfg["modelConfigRules"].(map[string]any); ok {
		rules, err := arrayField(mcr, "providerModelRules")
		if err != nil {
			return err
		}
		if _, present := mcr["providerModelRules"]; present {
			kept := make([]any, 0, len(rules))
			for _, existing := range rules {
				if entryProviderID(existing) == stableProviderID {
					continue
				}
				kept = append(kept, existing)
			}
			mcr["providerModelRules"] = kept
		}
	}
	return z.write(doc)
}

// ruleList reads the provider rules array out of a document that may not have
// been written by zen-gate at all.
func ruleList(doc map[string]any) []any {
	cfg, _ := doc["config"].(map[string]any)
	pcr, _ := cfg["providerConfigRules"].(map[string]any)
	rules, _ := pcr["providerRules"].([]any)
	return rules
}

// childMap returns m[key] as a writable object, creating it when the key is
// absent. A value of another type is replaced: these three keys are the path
// zen-gate owns, and a non-object there is not a shape it can add itself to.
func childMap(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok && sub != nil {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

// arrayField reads a key that must hold an array. Absent (or JSON null) reads
// as empty; a value of any other type is somebody else's data in a shape
// zen-gate will not guess about, so the write is refused instead of replacing it.
func arrayField(m map[string]any, key string) ([]any, error) {
	v, present := m[key]
	if !present || v == nil {
		return []any{}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 不是数组，为避免破坏已有内容，zen-gate 不会改写它", key)
	}
	return arr, nil
}

func entryName(raw any) string {
	m, _ := raw.(map[string]any)
	if s, ok := m["providerName"].(string); ok {
		return s
	}
	return ""
}

func entryProviderID(raw any) string {
	m, _ := raw.(map[string]any)
	if s, ok := m["providerId"].(string); ok {
		return s
	}
	return ""
}
