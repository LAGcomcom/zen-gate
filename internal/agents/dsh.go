package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// dsh injects a zen-gate provider into DeepSeek Harness. The host's llm-pi-ai
// plugin carries the providers dict every model picker reads from
// (displayName, api, baseURL, models), and the supported way to set it is the
// profile's own cordis.patch.yml — the same layer a hand-added provider lands
// in. The edit is a targeted merge on the yaml node tree (comments, key order
// and !!js expressions survive round-trip): an existing llm-pi-ai entry gains
// a "zen-gate" key under config.providers, a profile without one gets the
// entry appended.
//
// Profiles keep their own layer last in the composition order, so this never
// fights bundle patches; and installs of the legacy dsh-our-free-model plugin
// (wired by earlier zen-gate versions) are stripped on enable/disable so the
// two routes never run at once.
const (
	dshPluginID   = "dsh-our-free-model"
	dshProviderID = "zen-gate"
	dshPatchID    = "llm-pi-ai"
	dshPatchName  = "@deepseek-ai/dsh-llm-pi-ai"
)

type dsh struct{}

func newDSH() *dsh { return &dsh{} }

func (d *dsh) Meta() (string, string, string) {
	return "dsh", "DeepSeek Harness", "注入 llm-pi-ai 自定义供应商（cordis.patch.yml，关闭即还原）"
}

func (d *dsh) profilesDir() string { return homePath(".dsh", "profiles") }

// profileDirs lists profile folders — each carries a package.json (the
// profile manifest) beside its user patch layer.
func (d *dsh) profileDirs() []string {
	entries, err := os.ReadDir(d.profilesDir())
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "node_modules" {
			continue
		}
		dir := filepath.Join(d.profilesDir(), e.Name())
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			out = append(out, dir)
		}
	}
	return out
}

// hostVersion reads the @deepseek-ai/dsh runtime version, from the shared
// hoisted store or any profile's own node_modules.
func (d *dsh) hostVersion() string {
	candidates := []string{
		filepath.Join(d.profilesDir(), "node_modules", "@deepseek-ai", "dsh", "package.json"),
	}
	for _, dir := range d.profileDirs() {
		candidates = append(candidates, filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh", "package.json"))
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Version != "" {
			return pkg.Version
		}
	}
	return ""
}

func (d *dsh) Detect() (bool, string, string) {
	profiles := d.profileDirs()
	if len(profiles) == 0 {
		return false, "", "未检测到 DeepSeek Harness profile"
	}
	detail := fmt.Sprintf("%d 个 profile", len(profiles))
	if v := d.hostVersion(); v != "" {
		return true, v, detail
	}
	return true, "", detail
}

// IsEnabled reports whether any profile's patch layer carries the zen-gate
// provider — the providers dict itself may well hold hand-added providers.
func (d *dsh) IsEnabled() (bool, string, error) {
	for _, dir := range d.profileDirs() {
		doc, err := d.readPatch(filepath.Join(dir, "cordis.patch.yml"))
		if err != nil || doc == nil {
			continue
		}
		if providers := d.findProviders(doc); providers != nil && mapValue(providers, dshProviderID) != nil {
			return true, "", nil
		}
	}
	return false, "", nil
}

// Enable wires every profile: strip the legacy plugin install, then merge the
// zen-gate provider into the patch layer (backed up first).
func (d *dsh) Enable(o Options) error {
	profiles := d.profileDirs()
	if len(profiles) == 0 {
		return fmt.Errorf("未找到 ~/.dsh/profiles 下的 profile")
	}
	for _, dir := range profiles {
		if err := d.stripLegacyPlugin(dir); err != nil {
			return fmt.Errorf("profile %s 清理旧插件失败: %w", filepath.Base(dir), err)
		}
		if err := d.upsertProvider(dir, o); err != nil {
			return fmt.Errorf("profile %s 注入失败: %w", filepath.Base(dir), err)
		}
	}
	return nil
}

// Disable removes the zen-gate provider from every profile's patch layer and
// strips any legacy plugin install. Hand-added providers are untouched.
func (d *dsh) Disable() error {
	for _, dir := range d.profileDirs() {
		if err := d.removeProvider(dir); err != nil {
			return fmt.Errorf("profile %s 还原失败: %w", filepath.Base(dir), err)
		}
		if err := d.stripLegacyPlugin(dir); err != nil {
			return fmt.Errorf("profile %s 清理旧插件失败: %w", filepath.Base(dir), err)
		}
	}
	return nil
}

// --- patch layer -----------------------------------------------------------

// readPatch loads a cordis.patch.yml as a yaml node tree; a missing file
// yields nil (callers create one). An empty or comment-only file yields an
// empty sequence so upserts have somewhere to land.
func (d *dsh) readPatch(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cordis.patch.yml 无法解析: %w", err)
	}
	// A comments-only (or empty) file decodes to a bare node — rebuild an
	// empty patch list for the upsert path, carrying the leading comment
	// block over by hand (a bare document drops it).
	if doc.Kind == 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		var head []string
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			if strings.HasPrefix(t, "#") {
				head = append(head, t)
				continue
			}
			break
		}
		if len(head) > 0 {
			seq.HeadComment = strings.Join(head, "\n")
		}
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{seq}}, nil
	}
	if doc.Kind == yaml.DocumentNode && (len(doc.Content) == 0 || doc.Content[0].Kind == 0) {
		doc.Content = []*yaml.Node{{Kind: yaml.SequenceNode}}
	}
	return &doc, nil
}

// patchEntries walks the top-level sequence of patch entries (mappings).
func patchEntries(doc *yaml.Node) []*yaml.Node {
	if doc.Kind != yaml.DocumentNode {
		return nil
	}
	body := doc.Content[0]
	if body.Kind != yaml.SequenceNode {
		return nil
	}
	return body.Content
}

// findProviders returns the providers mapping node of the llm-pi-ai patch
// entry, creating the entry and its config shells when missing.
func (d *dsh) findProviders(doc *yaml.Node) *yaml.Node {
	for _, entry := range patchEntries(doc) {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if id := mapValue(entry, "id"); id != nil && id.Value == dshPatchID {
			config := mapValue(entry, "config")
			if config == nil || config.Kind != yaml.MappingNode {
				return nil
			}
			return mapValue(config, "providers")
		}
	}
	return nil
}

// mapValue returns the value node for one mapping key.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapKey overwrites or appends one key on a mapping node.
func setMapKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	m.Content = append(m.Content, k, value)
}

// deleteMapKey removes one key from a mapping node.
func deleteMapKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func (d *dsh) upsertProvider(dir string, o Options) error {
	path := filepath.Join(dir, "cordis.patch.yml")
	doc, err := d.readPatch(path)
	if err != nil {
		return err
	}
	if doc == nil {
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.SequenceNode}}}
	}
	provider := d.providerNode(o)

	// Fast path: an llm-pi-ai entry already exists — merge into it.
	for _, entry := range patchEntries(doc) {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if id := mapValue(entry, "id"); id != nil && id.Value == dshPatchID {
			config := mapValue(entry, "config")
			if config == nil || config.Kind != yaml.MappingNode {
				config = &yaml.Node{Kind: yaml.MappingNode}
				setMapKey(entry, "config", config)
			}
			providers := mapValue(config, "providers")
			if providers == nil || providers.Kind != yaml.MappingNode {
				providers = &yaml.Node{Kind: yaml.MappingNode}
				setMapKey(config, "providers", providers)
			}
			setMapKey(providers, dshProviderID, provider)
			return d.writePatch(path, doc)
		}
	}

	// No llm-pi-ai entry anywhere: append one.
	providers := &yaml.Node{Kind: yaml.MappingNode}
	setMapKey(providers, dshProviderID, provider)
	config := &yaml.Node{Kind: yaml.MappingNode}
	setMapKey(config, "providers", providers)
	entry := &yaml.Node{Kind: yaml.MappingNode}
	setMapKey(entry, "id", strNode(dshPatchID))
	setMapKey(entry, "name", strNode(dshPatchName))
	setMapKey(entry, "config", config)
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.SequenceNode {
		return fmt.Errorf("cordis.patch.yml 顶层不是 patch 列表，拒绝追加")
	}
	doc.Content[0].Content = append(doc.Content[0].Content, entry)
	return d.writePatch(path, doc)
}

func (d *dsh) removeProvider(dir string) error {
	path := filepath.Join(dir, "cordis.patch.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil // unparseable: never "fix" it by rewriting
	}
	changed := false
	for _, entry := range patchEntries(&doc) {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if id := mapValue(entry, "id"); id == nil || id.Value != dshPatchID {
			continue
		}
		config := mapValue(entry, "config")
		if config == nil || config.Kind != yaml.MappingNode {
			continue
		}
		providers := mapValue(config, "providers")
		if providers == nil || providers.Kind != yaml.MappingNode {
			continue
		}
		if mapValue(providers, dshProviderID) != nil {
			deleteMapKey(providers, dshProviderID)
			changed = true
			if len(providers.Content) == 0 {
				deleteMapKey(config, "providers")
			}
		}
	}
	if !changed {
		return nil
	}
	_, _ = backupFile("dsh", path, data)
	return writeYAML(path, &doc)
}

// providerNode builds the llm-pi-ai provider profile: an OpenAI-compatible
// endpoint carrying the gateway's key as a static Authorization header, and
// the model set with capacities the picker can show. Auth via headers — an
// apiKeyEnv reference would need a separately provisioned host credential.
func (d *dsh) providerNode(o Options) *yaml.Node {
	provider := &yaml.Node{Kind: yaml.MappingNode}
	setMapKey(provider, "displayName", strNode("Zen Gate"))
	setMapKey(provider, "api", strNode("openai-completions"))
	setMapKey(provider, "baseURL", strNode(o.BaseURL))
	headers := &yaml.Node{Kind: yaml.MappingNode}
	setMapKey(headers, "authorization", strNode("Bearer "+o.APIKey))
	setMapKey(provider, "headers", headers)
	models := &yaml.Node{Kind: yaml.SequenceNode}
	for _, m := range o.Models {
		model := &yaml.Node{Kind: yaml.MappingNode}
		setMapKey(model, "id", strNode(m.ID))
		setMapKey(model, "name", strNode(m.Name))
		if m.ContextWindow > 0 {
			setMapKey(model, "contextWindow", intNode(m.ContextWindow))
		}
		if m.MaxOutput > 0 {
			setMapKey(model, "maxTokens", intNode(m.MaxOutput))
		}
		input := []string{"text"}
		if m.Vision {
			input = append(input, "image")
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, v := range input {
			seq.Content = append(seq.Content, strNode(v))
		}
		setMapKey(model, "input", seq)
		models.Content = append(models.Content, model)
	}
	setMapKey(provider, "models", models)
	return provider
}

// --- legacy plugin strip ----------------------------------------------------

// stripLegacyPlugin removes an earlier zen-gate release's wiring of the
// dsh-our-free-model plugin (package.json deps + bundle registration +
// node_modules copy) so the plugin route and the gateway provider never run
// side by side. A plugin the user installed by other means carries a
// different provenance and is left alone.
func (d *dsh) stripLegacyPlugin(dir string) error {
	pkgPath := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil
	}
	var pkg map[string]any
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	changed := false
	if deps, ok := pkg["dependencies"].(map[string]any); ok {
		if _, has := deps[dshPluginID]; has {
			delete(deps, dshPluginID)
			changed = true
		}
	}
	// Both spellings exist across dsh versions: the nested manifest and the
	// flat legacy key.
	for _, key := range []string{"dsh.profile.bundles", "dsh"} {
		switch v := pkg[key].(type) {
		case []any:
			kept := []any{}
			for _, b := range v {
				if s, ok := b.(string); ok && s == dshPluginID {
					changed = true
					continue
				}
				kept = append(kept, b)
			}
			pkg[key] = kept
		case map[string]any:
			profile, ok := v["profile"].(map[string]any)
			if !ok {
				continue
			}
			bundles, ok := profile["bundles"].([]any)
			if !ok {
				continue
			}
			kept := []any{}
			for _, b := range bundles {
				if s, ok := b.(string); ok && s == dshPluginID {
					changed = true
					continue
				}
				kept = append(kept, b)
			}
			profile["bundles"] = kept
		}
	}
	if !changed {
		return nil
	}
	if _, err := backupFile("dsh", pkgPath, data); err != nil {
		return err
	}
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(pkgPath, out); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(dir, "node_modules", dshPluginID))
	return nil
}

// --- shared ------------------------------------------------------------------

func (d *dsh) writePatch(path string, doc *yaml.Node) error {
	if data, err := os.ReadFile(path); err == nil {
		if _, err := backupFile("dsh", path, data); err != nil {
			return err
		}
	}
	return writeYAML(path, doc)
}

func writeYAML(path string, doc *yaml.Node) error {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return atomicWrite(path, []byte(buf.String()))
}

func strNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", v)}
}
