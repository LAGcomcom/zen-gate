package agents

import (
	"crypto/rand"
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

func (z *zcode) IsEnabled() (bool, string, error) {
	cfg, err := z.read()
	if err != nil {
		return false, "", nil
	}
	for _, rule := range cfg.Config.ProviderConfigRules.ProviderRules {
		if nameOf(rule) == providerName {
			enabled, _ := rule["enabled"].(bool)
			return enabled, "", nil
		}
	}
	return false, "", nil
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

type zcodeProviderConfig struct {
	SchemaVersion int `json:"schemaVersion"`
	Config        struct {
		ProviderConfigRules struct {
			ProviderRules []map[string]any `json:"providerRules"`
		} `json:"providerConfigRules"`
		ModelConfigRules struct {
			ProviderModelRules       []map[string]any `json:"providerModelRules"`
			ManualProviderModelRules []map[string]any `json:"manualProviderModelRules,omitempty"`
		} `json:"modelConfigRules"`
	} `json:"config"`
}

func (z *zcode) read() (*zcodeProviderConfig, error) {
	data, err := os.ReadFile(z.configPath())
	if err != nil {
		return nil, err
	}
	cfg := &zcodeProviderConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("provider_config.json 无法解析（%w）；为避免破坏未知格式，zen-gate 拒绝写入", err)
	}
	return cfg, nil
}

func (z *zcode) Enable(o Options) error {
	path := z.configPath()
	var cfg *zcodeProviderConfig
	if data, err := os.ReadFile(path); err == nil {
		cfg = &zcodeProviderConfig{}
		if err := json.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("provider_config.json 无法解析（%w）；为避免破坏未知格式，zen-gate 拒绝写入", err)
		}
	} else {
		cfg = &zcodeProviderConfig{SchemaVersion: 1}
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	if cfg.Config.ProviderConfigRules.ProviderRules == nil {
		cfg.Config.ProviderConfigRules.ProviderRules = []map[string]any{}
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
	rules := []map[string]any{}
	for _, existing := range cfg.Config.ProviderConfigRules.ProviderRules {
		if nameOf(existing) == providerName {
			continue // replace our own stale entry, keep everything else
		}
		rules = append(rules, existing)
	}
	rules = append(rules, rule)
	cfg.Config.ProviderConfigRules.ProviderRules = rules

	// Model rules: capability metadata per model — context window plus the
	// thought-level selector for reasoning models.
	mc := cfg.Config.ModelConfigRules
	if mc.ProviderModelRules == nil {
		mc.ProviderModelRules = []map[string]any{}
	}
	kept := []map[string]any{}
	for _, existing := range mc.ProviderModelRules {
		if pid, _ := existing["providerId"].(string); pid == stableProviderID {
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
	mc.ProviderModelRules = kept
	cfg.Config.ModelConfigRules = mc

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func (z *zcode) Disable() error {
	path := z.configPath()
	cfg, err := z.read()
	if err != nil {
		return nil // nothing (or unreadable) — leave the file alone
	}
	rules := []map[string]any{}
	for _, existing := range cfg.Config.ProviderConfigRules.ProviderRules {
		if nameOf(existing) == providerName {
			continue
		}
		rules = append(rules, existing)
	}
	cfg.Config.ProviderConfigRules.ProviderRules = rules
	if mc := cfg.Config.ModelConfigRules; mc.ProviderModelRules != nil {
		kept := []map[string]any{}
		for _, existing := range mc.ProviderModelRules {
			if pid, _ := existing["providerId"].(string); pid == stableProviderID {
				continue
			}
			kept = append(kept, existing)
		}
		mc.ProviderModelRules = kept
		cfg.Config.ModelConfigRules = mc
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func nameOf(rule map[string]any) string {
	if s, ok := rule["providerName"].(string); ok {
		return s
	}
	return ""
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
