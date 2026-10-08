package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// codex injects a [model_providers.zen_gate] block plus a model catalog into
// ~/.codex/config.toml. The provider block is appended textually so the rest
// of the user's TOML is untouched; marker comments make enable/disable
// idempotent. The catalog is written as a sidecar JSON file referenced by the
// top-level `model_catalog_json` key — that key must sit before the first
// [table] header, so it gets its own marker pair inserted there.
//
// A config that already carries a top-level `model_catalog_json` (the user's
// own, or one left behind without markers) must not grow a second one — TOML
// rejects duplicate keys and Codex refuses to parse the whole file ("Cannot
// overwrite a value"). Enable therefore strips the managed blocks first and
// moves any pre-existing top-level catalog key into the stash before
// inserting the managed line; Disable puts it back verbatim.
//
// Schema notes (validated against `codex debug models`, v0.158): every model
// needs base_instructions, supported_reasoning_levels, shell_type,
// support_verbosity, truncation_policy{mode,limit},
// experimental_supported_tools and input_modalities, or the catalog is
// rejected wholesale.

type codex struct{}

const codexMarkerBegin = "# >>> zen-gate managed block (do not edit)"
const codexMarkerEnd = "# <<< zen-gate managed block"
const codexCatalogKeyBegin = "# >>> zen-gate catalog key (do not edit)"
const codexCatalogKeyEnd = "# <<< zen-gate catalog key"

type codexLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncation struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexCatalogModel struct {
	Slug              string          `json:"slug"`
	DisplayName       string          `json:"display_name"`
	Description       string          `json:"description"`
	BaseInstructions  string          `json:"base_instructions"`
	DefaultReasoning  string          `json:"default_reasoning_level"`
	ReasoningLevels   []codexLevel    `json:"supported_reasoning_levels"`
	ShellType         string          `json:"shell_type"`
	SupportVerbosity  bool            `json:"support_verbosity"`
	TruncationPolicy  codexTruncation `json:"truncation_policy"`
	ExperimentalTools []string        `json:"experimental_supported_tools"`
	InputModalities   []string        `json:"input_modalities"`
	Visibility        string          `json:"visibility"`
	SupportedInAPI    bool            `json:"supported_in_api"`
	Priority          int             `json:"priority"`
	ProviderID        string          `json:"provider_id"`
	ContextWindow     int             `json:"context_window,omitempty"`
	MaxOutputTokens   int             `json:"max_output_tokens,omitempty"`
}

type codexCatalog struct {
	Models []codexCatalogModel `json:"models"`
}

func newCodex() *codex { return &codex{} }

func (c *codex) Meta() (string, string, string) {
	return "codex", "Codex CLI / 桌面版", "~/.codex/config.toml 注入 provider 与模型目录（wire_api=responses）"
}

func (c *codex) configPath() string { return homePath(".codex", "config.toml") }
func (c *codex) catalogPath() string {
	return homePath(".codex", "zen-gate-catalog.json")
}

func (c *codex) Detect() (bool, string, string) {
	if _, err := os.Stat(c.configPath()); err == nil {
		return true, "", "检测到 config.toml"
	}
	if _, err := os.Stat(homePath(".codex")); err == nil {
		return true, "", "检测到 ~/.codex（config.toml 尚未创建）"
	}
	return false, "", "未检测到 Codex"
}

func (c *codex) IsEnabled() (bool, string, error) {
	data, err := os.ReadFile(c.configPath())
	if err != nil {
		return false, "", nil
	}
	return strings.Contains(string(data), codexMarkerBegin), "", nil
}

func (c *codex) block(o Options) string {
	// requires_openai_auth mirrors the shape of working custom providers on
	// desktop Codex: without it the app treats the provider as unauthenticated
	// and blocks model loading behind a ChatGPT login prompt. wire_api must be
	// "responses" — current Codex builds refuse to load "chat" providers.
	// model_catalog_url lets the picker refresh the list live from the gateway;
	// model_catalog_json (top of file) is the verified offline path.
	return fmt.Sprintf(`%s
[model_providers.zen_gate]
name = "Zen Gate 免费模型"
base_url = "%s"
wire_api = "responses"
requires_openai_auth = true
experimental_bearer_token = "%s"
model_catalog_url = "%s/codex-catalog"
%s
`, codexMarkerBegin, o.BaseURL, o.APIKey, o.BaseURL, codexMarkerEnd)
}

// catalogJSON builds the picker catalog from the lane's model table.
func catalogJSON(o Options, def string) ([]byte, error) {
	cat := codexCatalog{Models: []codexCatalogModel{}}
	for _, m := range o.Models {
		levels := []codexLevel{
			{Effort: "low", Description: "轻量，最省额度"},
			{Effort: "medium", Description: "均衡"},
			{Effort: "high", Description: "深思"},
		}
		if !m.Reasoning {
			levels = []codexLevel{{Effort: "medium", Description: "默认"}}
		}
		mods := []string{"text"}
		if m.Vision {
			mods = append(mods, "image")
		}
		// Region-gated models sink to the bottom of the picker: with a CN
		// egress they only ever answer with a RegionError. Applied after the
		// default bump so a region-gated default can't win the top slot.
		pri := 50
		if m.ID == def {
			pri = 60
		}
		if m.RegionSensitive {
			pri = 10
		}
		desc := "Zen Gate 免费车道模型"
		if m.Blurb != "" {
			desc = m.Blurb
		}
		cat.Models = append(cat.Models, codexCatalogModel{
			Slug:        m.ID,
			DisplayName: m.Name,
			Description: desc,
			BaseInstructions: fmt.Sprintf(
				"You are %s (model id: %s), a coding agent serving the user's Codex app through the Zen Gate local gateway. Work toward the user's goal with the available tools and verify your changes. Always reply in the same language as the user's latest message — when the user writes Chinese, reply in Simplified Chinese. Be concise.",
				m.Name, m.ID,
			),
			DefaultReasoning:  "medium",
			ReasoningLevels:   levels,
			ShellType:         "unified_exec",
			SupportVerbosity:  false,
			TruncationPolicy:  codexTruncation{Mode: "tokens", Limit: 10000},
			ExperimentalTools: []string{},
			InputModalities:   mods,
			Visibility:        "list",
			SupportedInAPI:    true,
			Priority:          pri,
			ProviderID:        "zen_gate",
			ContextWindow:     m.ContextWindow,
			MaxOutputTokens:   m.MaxOutput,
		})
	}
	// The picker treats the top entry as the default model — keep
	// region-gated ones out of that slot.
	sort.SliceStable(cat.Models, func(i, j int) bool {
		if cat.Models[i].Priority != cat.Models[j].Priority {
			return cat.Models[i].Priority > cat.Models[j].Priority
		}
		return cat.Models[i].Slug < cat.Models[j].Slug
	})
	return json.MarshalIndent(cat, "", "  ")
}

// insertTopLevelKey puts the managed model_catalog_json line before the first
// [table] header — TOML top-level keys must precede every table.
func insertTopLevelKey(src, line string) string {
	lines := strings.Split(src, "\n")
	at := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			at = i
			break
		}
	}
	block := []string{codexCatalogKeyBegin, line, codexCatalogKeyEnd, ""}
	out := append([]string{}, lines[:at]...)
	out = append(out, block...)
	out = append(out, lines[at:]...)
	return strings.Join(out, "\n")
}

func stripMarkers(src string, begin, end string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == begin:
			inBlock = true
		case strings.TrimSpace(line) == end:
			inBlock = false
		case !inBlock:
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// codexStatePath stores the pre-existing top-level model_provider/model lines
// this adapter replaces — and any top-level lines it removes outright (a
// pre-existing model_catalog_json) — so Disable can put them all back
// verbatim.
func (c *codex) codexStatePath() string {
	return homePath(".codex", "zen-gate-state.json")
}

// replaceTopLevelKeys rewrites key=value lines in the top-level region (before
// the first [table] header) and records the originals in stash.
func replaceTopLevelKeys(src string, repl map[string]string, stash map[string]string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			break
		}
		eq := strings.Index(t, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(t[:eq])
		if val, ok := repl[key]; ok {
			if _, recorded := stash[key]; !recorded {
				stash[key] = l
			}
			comment := ""
			if idx := strings.Index(l, "#"); idx >= 0 && !strings.HasPrefix(strings.TrimSpace(l[idx:]), "#>") {
				comment = " " + l[idx:]
			}
			lines[i] = key + " = " + val + comment
		}
	}
	return strings.Join(lines, "\n")
}

// topLevelKeyOf extracts the bare key name from a top-level `key = value`
// line; "" when the line is not one.
func topLevelKeyOf(t string) string {
	eq := strings.Index(t, "=")
	if eq <= 0 {
		return ""
	}
	return strings.TrimSpace(t[:eq])
}

// removeTopLevelKeys deletes every top-level occurrence of the given keys
// (before the first [table] header) and records the first-seen original line
// per key in stash, so Disable can put them back verbatim. This is what keeps
// a pre-existing `model_catalog_json = "..."` from turning into a duplicate
// top-level key — TOML forbids the same key twice and Codex would fail to
// parse the whole file.
func removeTopLevelKeys(src string, keys map[string]bool, stash map[string]string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	top := true
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if top && strings.HasPrefix(t, "[") {
			top = false
		}
		key := ""
		if top {
			key = topLevelKeyOf(t)
		}
		if key != "" && keys[key] {
			if _, recorded := stash[key]; !recorded {
				stash[key] = l
			}
			continue // drop the line entirely
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func (c *codex) Enable(o Options) error {
	path := c.configPath()
	var src string
	if data, err := os.ReadFile(path); err == nil {
		src = string(data)
	} else if !os.IsNotExist(err) {
		return err
	}

	def := o.DefaultModel
	for _, m := range o.Models {
		if !m.RegionSensitive {
			def = m.ID
			break
		}
	}

	cat, err := catalogJSON(o, def)
	if err != nil {
		return err
	}
	if err := atomicWrite(c.catalogPath(), cat); err != nil {
		return err
	}

	// Seed the stash from a previous enable: on re-enable the config carries
	// zen-gate's own rewritten lines, and recording them as "originals" would
	// clobber the user's real ones.
	stash := map[string]string{}
	if b, err := os.ReadFile(c.codexStatePath()); err == nil {
		var prev map[string]string
		if json.Unmarshal(b, &prev) == nil {
			for k, v := range prev {
				stash[k] = v
			}
		}
	}

	out := c.applyEnable(src, o, def, c.catalogPath(), stash)
	if len(stash) > 0 {
		if b, err := json.Marshal(map[string]string(stash)); err == nil {
			_ = atomicWrite(c.codexStatePath(), b)
		}
	}
	return atomicWrite(path, []byte(out))
}

// applyEnable builds the new config.toml text from the user's current config
// text. stash collects the pre-enable originals — lines rewritten in place
// and lines removed entirely — so Disable can undo the whole operation.
func (c *codex) applyEnable(src string, o Options, def, catalogPath string, stash map[string]string) string {
	// Strip previous managed blocks first: on re-enable the managed
	// model_catalog_json line must not reach the stashing passes below, or
	// zen-gate's own line would be recorded as the user's "original".
	base := stripMarkers(stripMarkers(src, codexMarkerBegin, codexMarkerEnd), codexCatalogKeyBegin, codexCatalogKeyEnd)
	// The desktop picker changes the model name but keeps routing through the
	// config's active provider, so zen_gate must become the active provider
	// for picker selections to work at all. The user's original top-level
	// model_provider/model lines are stashed and restored on Disable.
	base = replaceTopLevelKeys(base, map[string]string{
		"model_provider": `"zen_gate"`,
		"model":          fmt.Sprintf("%q", def),
	}, stash)
	// A config that already carries a top-level model_catalog_json — the
	// user's own, or one left behind without markers — must not end up with
	// the key twice: TOML rejects duplicate keys and Codex refuses to parse
	// the whole file. Remove the pre-existing line (stashed for Disable)
	// before the managed one goes in.
	base = removeTopLevelKeys(base, map[string]bool{"model_catalog_json": true}, stash)
	if base != "" && !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	keyLine := fmt.Sprintf("model_catalog_json = '%s'", filepath.ToSlash(catalogPath))
	return insertTopLevelKey(base+"\n"+c.block(o), keyLine)
}

func (c *codex) Disable() error {
	path := c.configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		_ = os.Remove(c.codexStatePath())
		return nil
	}
	// Managed blocks go first: the stash undo below must not mistake
	// zen-gate's own managed model_catalog_json line for the user's.
	clean := stripMarkers(stripMarkers(string(data), codexMarkerBegin, codexMarkerEnd), codexCatalogKeyBegin, codexCatalogKeyEnd)
	// Restore the user's original top-level lines: rewritten ones in place,
	// lines that Enable removed entirely (e.g. a pre-existing
	// model_catalog_json) re-inserted before the first [table] header.
	if b, err := os.ReadFile(c.codexStatePath()); err == nil {
		var stash map[string]string
		if json.Unmarshal(b, &stash) == nil && len(stash) > 0 {
			clean = applyDisable(clean, stash)
		}
		_ = os.Remove(c.codexStatePath())
	}
	return atomicWrite(path, []byte(clean+"\n"))
}

// applyDisable undoes applyEnable's text transforms: stashed lines are put
// back verbatim — rewritten keys in place, keys whose lines were removed
// entirely re-inserted before the first [table] header (top-level keys must
// precede every table).
func applyDisable(src string, stash map[string]string) string {
	lines := strings.Split(src, "\n")
	present := map[string]bool{}
	at := len(lines)
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			at = i
			break
		}
		if key := topLevelKeyOf(t); key != "" {
			if orig, ok := stash[key]; ok {
				lines[i] = orig // restore the rewritten line verbatim
			}
			present[key] = true
		}
	}
	var missing []string
	for k := range stash {
		if !present[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		add := make([]string, 0, len(missing))
		for _, k := range missing {
			add = append(add, stash[k])
		}
		out := append([]string{}, lines[:at]...)
		out = append(out, add...)
		out = append(out, lines[at:]...)
		lines = out
	}
	return strings.Join(lines, "\n")
}
