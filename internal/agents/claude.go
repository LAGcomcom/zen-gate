package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claude swaps the env block in ~/.claude/settings.json (ANTHROPIC_BASE_URL,
// ANTHROPIC_AUTH_TOKEN and the model selectors) to point Claude Code at the
// local gateway. The whole file is backed up before the first write and the
// pre-injection values are restored verbatim on disable.

type claude struct{}

func newClaude() *claude { return &claude{} }

func (c *claude) Meta() (string, string, string) {
	return "claude", "Claude Code", "~/.claude/settings.json 的 env 切换到本地网关（原值自动备份还原）"
}

func (c *claude) settingsPath() string { return homePath(".claude", "settings.json") }

func (c *claude) Detect() (bool, string, string) {
	if _, err := os.Stat(c.settingsPath()); err == nil {
		return true, "", "检测到 settings.json"
	}
	if _, err := os.Stat(homePath(".claude")); err == nil {
		return true, "", "检测到 ~/.claude（settings.json 尚未创建）"
	}
	return false, "", "未检测到 Claude Code"
}

func (c *claude) IsEnabled() (bool, string, error) {
	doc, err := c.read()
	if err != nil {
		return false, "", nil
	}
	env, _ := doc["env"].(map[string]any)
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	return strings.Contains(base, "127.0.0.1"), "", nil
}

func (c *claude) read() (map[string]any, error) {
	data, err := os.ReadFile(c.settingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("settings.json 无法解析（%w）；zen-gate 拒绝写入", err)
	}
	return doc, nil
}

// hasOwnClaudeEnv reports whether a settings document already carries
// zen-gate's injected env — i.e. it is not a pristine restore point. A file
// that cannot be parsed is treated as foreign so it still gets backed up before
// read() reports the parse error.
func hasOwnClaudeEnv(data []byte) bool {
	doc := map[string]any{}
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	env, _ := doc["env"].(map[string]any)
	base, _ := env["ANTHROPIC_BASE_URL"].(string)
	return strings.Contains(base, "127.0.0.1")
}

func (c *claude) Enable(o Options) error {
	path := c.settingsPath()
	if data, err := os.ReadFile(path); err == nil {
		// Re-enabling (ResyncEnabled calls Enable on every model-list change)
		// must not overwrite the pristine restore point with already-injected
		// content: that would make Disable restore the gateway URL and leave
		// Claude Code pointing at a local port.
		if !hasOwnClaudeEnv(data) {
			_, _ = backupFile("claude", path, data)
		}
	}
	doc, err := c.read()
	if err != nil {
		return err
	}
	env, _ := doc["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	// The Anthropic SDK appends /v1/messages itself, so advertise the origin.
	root := strings.TrimSuffix(o.BaseURL, "/v1")
	env["ANTHROPIC_BASE_URL"] = root
	env["ANTHROPIC_AUTH_TOKEN"] = o.APIKey
	env["ANTHROPIC_MODEL"] = o.DefaultModel
	env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = o.DefaultModel
	env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = o.DefaultModel
	env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = o.DefaultModel
	doc["env"] = env
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// A fresh install has no ~/.claude yet; Detect only reports "detected"
	// from the directory, so Enable is routinely the first writer.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func (c *claude) Disable() error {
	path := c.settingsPath()
	if b := latestBackup("claude", "settings.json"); b != "" {
		data, err := os.ReadFile(b)
		if err == nil {
			return atomicWrite(path, data)
		}
	}
	// No backup: strip our own values only.
	doc, err := c.read()
	if err != nil {
		return nil
	}
	if env, ok := doc["env"].(map[string]any); ok {
		if base, _ := env["ANTHROPIC_BASE_URL"].(string); strings.Contains(base, "127.0.0.1") {
			delete(env, "ANTHROPIC_BASE_URL")
			delete(env, "ANTHROPIC_AUTH_TOKEN")
			delete(env, "ANTHROPIC_MODEL")
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}
