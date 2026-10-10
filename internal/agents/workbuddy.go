package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"zen-gate/internal/lane"
)

// workBuddy injects custom models into <config-dir>/models.json for the two
// CodeBuddy-kernel desktop variants: WorkBuddy (~/.workbuddy) and its
// international sibling WorkBuddy AI (~/.workbuddy-ai). Same kernel, same
// schema, same hot-reload (~1s debounce) — only the directory, the display
// name and the process name differ, so one adapter carries both as fields.
// Injected entries carry a full chat-completions endpoint url and are
// self-identified by their 127.0.0.1 host; the pristine file is backed up
// before the first write and restored verbatim on disable.

type workBuddy struct {
	id      string // adapter id: "workbuddy" | "workbuddy-ai"
	name    string // display name
	dirEnv  string // env var that overrides the config dir
	dirName string // home-relative config dir
	process string // running-app name for the dashboard hint
}

func newWorkBuddy() *workBuddy {
	return &workBuddy{
		id:      "workbuddy",
		name:    "WorkBuddy",
		dirEnv:  "WORKBUDDY_CONFIG_DIR",
		dirName: ".workbuddy",
		process: "WorkBuddy",
	}
}

func newWorkBuddyAI() *workBuddy {
	return &workBuddy{
		id:      "workbuddy-ai",
		name:    "WorkBuddy AI",
		dirEnv:  "WORKBUDDY_AI_CONFIG_DIR",
		dirName: ".workbuddy-ai",
		process: "WorkBuddy AI",
	}
}

func (w *workBuddy) Meta() (string, string, string) {
	return w.id, w.name, filepath.Join("~", w.dirName, "models.json") + " 注入自定义模型（CodeBuddy 内核，保存后自动热加载）"
}

func (w *workBuddy) configDir() string {
	if p := os.Getenv(w.dirEnv); p != "" {
		return p
	}
	return homePath(w.dirName)
}

func (w *workBuddy) configPath() string {
	return filepath.Join(w.configDir(), "models.json")
}

func (w *workBuddy) Detect() (bool, string, string) {
	dir := w.configDir()
	if _, err := os.Stat(dir); err != nil {
		return false, "", "未检测到 " + w.name
	}
	version := ""
	if data, err := os.ReadFile(filepath.Join(dir, "last-launch.json")); err == nil {
		var launch struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &launch) == nil {
			version = launch.Version
		}
	}
	detail := "检测到 ~/" + w.dirName
	if _, err := os.Stat(w.configPath()); err != nil {
		detail = "检测到 ~/" + w.dirName + "（models.json 尚未创建）"
	}
	return true, version, detail
}

func (w *workBuddy) IsEnabled() (bool, string, error) {
	doc, err := w.read()
	if err != nil {
		return false, "", nil
	}
	return hasOwnEntry(doc), "", nil
}

// read parses models.json. A top-level array is what WorkBuddy writes for "no
// custom models" — the CLI's parser treats it the same as an empty object, so
// zen-gate does too. Unparseable JSON refuses the write (tier-2 rule).
func (w *workBuddy) read() (map[string]any, error) {
	data, err := os.ReadFile(w.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return parseModelsDoc(data)
}

func parseModelsDoc(data []byte) (map[string]any, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("models.json 无法解析（%w）；zen-gate 拒绝写入", err)
	}
	if _, ok := raw.([]any); ok {
		return map[string]any{}, nil
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return doc, nil
}

func (w *workBuddy) Enable(o Options) error {
	path := w.configPath()
	if raw, err := os.ReadFile(path); err == nil {
		// Re-enabling must not overwrite the pristine restore point with
		// already-injected content, so only back up files without our entries.
		doc, perr := parseModelsDoc(raw)
		if perr != nil || !hasOwnEntry(doc) {
			_, _ = backupFile(w.id, path, raw)
		}
	}
	doc, err := w.read()
	if err != nil {
		return err
	}

	entries := foreignEntries(doc)
	for _, m := range o.Models {
		entries = append(entries, map[string]any{
			"id":               m.ID,
			"name":             m.Name,
			"vendor":           "Zen Gate",
			"apiKey":           o.APIKey,
			"url":              o.BaseURL + "/chat/completions",
			"supportsToolCall": true,
			"supportsImages":   m.Vision,
		})
	}
	doc["models"] = entries
	// availableModels 是模型下拉框的白名单：一旦写入，WorkBuddy 内置模型全部
	// 被隐藏。留空时 CLI 以 MergeStrategy.Merge 并入自定义模型，内置模型不受
	// 影响——因此只有用户本就维护了非空白名单时才追加。
	if avail := availableIDs(doc["availableModels"]); len(avail) > 0 {
		doc["availableModels"] = mergeAvailable(doc["availableModels"], o.Models)
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	return atomicWrite(path, data)
}

func (w *workBuddy) Disable() error {
	path := w.configPath()
	if b := latestBackup(w.id, "models.json"); b != "" {
		if data, err := os.ReadFile(b); err == nil {
			return atomicWrite(path, data)
		}
	}
	// No backup: strip our own entries only.
	doc, err := w.read()
	if err != nil {
		return nil
	}
	ours := ownIDs(doc)
	if len(ours) == 0 {
		return nil
	}
	entries := foreignEntries(doc)
	doc["models"] = entries
	if _, has := doc["availableModels"]; has {
		// 只从既有白名单里摘除我们的 id（与 CLI deleteCustomModel 一致）。
		doc["availableModels"] = availableIDsWithout(doc["availableModels"], ours)
	}
	if len(entries) == 0 {
		delete(doc, "models")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// --- models.json helpers -----------------------------------------------------

// modelEntries returns doc["models"] as a list, whatever it currently holds.
func modelEntries(doc map[string]any) []any {
	if m, ok := doc["models"].([]any); ok {
		return m
	}
	return nil
}

// isOwnEntry: injected entries always point at the local gateway; user models
// from other providers keep their real hosts.
func isOwnEntry(entry map[string]any) bool {
	url, _ := entry["url"].(string)
	return strings.Contains(url, "127.0.0.1")
}

// foreignEntries drops zen-gate entries, preserving the user's own models
// as-is.
func foreignEntries(doc map[string]any) []any {
	out := []any{}
	for _, e := range modelEntries(doc) {
		if em, ok := e.(map[string]any); ok && isOwnEntry(em) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func hasOwnEntry(doc map[string]any) bool { return len(ownIDs(doc)) > 0 }

// ownIDs collects the ids of zen-gate-injected entries.
func ownIDs(doc map[string]any) []string {
	out := []string{}
	for _, e := range modelEntries(doc) {
		em, ok := e.(map[string]any)
		if !ok || !isOwnEntry(em) {
			continue
		}
		if id, _ := em["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// mergeAvailable unions the user's visible model ids with ours, keeping the
// user's order first.
func mergeAvailable(raw any, models []lane.ModelInfo) []string {
	out := availableIDs(raw)
	seen := map[string]bool{}
	for _, id := range out {
		seen[id] = true
	}
	for _, m := range models {
		if !seen[m.ID] {
			out = append(out, m.ID)
			seen[m.ID] = true
		}
	}
	return out
}

func availableIDsWithout(raw any, ids []string) []string {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	out := []string{}
	for _, id := range availableIDs(raw) {
		if !drop[id] {
			out = append(out, id)
		}
	}
	return out
}

func availableIDs(raw any) []string {
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
