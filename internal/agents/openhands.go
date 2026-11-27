package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// openhands writes the LLM config for OpenHands Agent Canvas.
//
// OpenHands 1.46 reads the model picker from ~/.openhands/profiles/*.json, not
// from the legacy llm_* fields in settings.json. Every profile is a full LLM
// payload (the same shape LLM.to_persisted() writes); the active one is also
// mirrored into agent_settings.llm so conversations launched without an
// explicit profile still work.
//
// The adapter owns the `default` profile (the agent profile seeded by
// OpenHands points at it) and one `zen-gate-<model>` profile per catalog model.
// Originals are backed up once per enable cycle and restored on disable.
type openhands struct {
	mu sync.Mutex
}

const (
	ohDefaultProfile    = "default"
	ohProfilePrefix     = "zen-gate-"
	ohMaxProfiles       = 50
	ohManifestVersion   = 1
	ohProfilesBackupDir = "openhands/profiles"
)

var ohProfileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ohProfileState is what a profile looked like before the current enable
// cycle. Existed=false means the adapter created it and must remove it again.
type ohProfileState struct {
	Existed bool   `json:"existed"`
	Backup  string `json:"backup,omitempty"`
}

// ohManifest records the first-touch backups for one enable cycle. Resync calls
// Enable again whenever the roster changes; without this, every pass would
// stack another backup and Disable would restore zen-gate's own previous write
// instead of the user's original config.
type ohManifest struct {
	Version         int                       `json:"version"`
	SettingsExisted bool                      `json:"settings_existed"`
	SettingsBackup  string                    `json:"settings_backup,omitempty"`
	Profiles        map[string]ohProfileState `json:"profiles"`
}

type ohProfile struct {
	Name    string
	ModelID string
	Info    lane.ModelInfo
}

func newOpenHands() *openhands { return &openhands{} }

func (o *openhands) Meta() (string, string, string) {
	return "openhands", "OpenHands", "~/.openhands 的 LLM 配置与模型档案指向本地网关（原值自动备份还原）"
}

func (o *openhands) configDir() string { return homePath(".openhands") }
func (o *openhands) configPath() string {
	return filepath.Join(o.configDir(), "settings.json")
}
func (o *openhands) profilesDir() string {
	return filepath.Join(o.configDir(), "profiles")
}
func (o *openhands) profilePath(name string) string {
	return filepath.Join(o.profilesDir(), name+".json")
}
func (o *openhands) manifestPath() string {
	return filepath.Join(store.Home(), "backups", "openhands", "manifest.json")
}

func (o *openhands) Detect() (bool, string, string) {
	if _, err := os.Stat(o.configDir()); err == nil {
		return true, "", "检测到 ~/.openhands"
	}
	return false, "", "未检测到 OpenHands"
}

func (o *openhands) IsEnabled() (bool, string, error) {
	data, err := os.ReadFile(o.configPath())
	if err == nil {
		var settings map[string]interface{}
		if json.Unmarshal(data, &settings) == nil && ohSettingsManaged(settings) {
			return true, "", nil
		}
	}
	// OpenHands validates agent_settings and drops unknown keys when it saves,
	// which can erase the marker. The default profile is a file we own and is
	// not rewritten by a settings save, so it is a durable fallback.
	if o.profileManaged(ohDefaultProfile) {
		return true, "", nil
	}
	return false, "", nil
}

func (o *openhands) Enable(opts Options) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := os.MkdirAll(o.configDir(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(o.profilesDir(), 0o700); err != nil {
		return err
	}

	settings, settingsData, err := o.readSettings()
	if err != nil {
		return err
	}

	profiles := o.desiredProfiles(opts)
	keep := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		keep[p.Name] = true
	}

	// Record the originals before touching anything. The manifest is written
	// before the first mutation so a crash mid-write can still be undone.
	manifest, err := o.ensureManifest(settingsData, profiles)
	if err != nil {
		return err
	}

	if err := o.pruneProfiles(keep, manifest); err != nil {
		return err
	}

	for _, p := range profiles {
		payload := o.profilePayload(opts, p)
		out, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(o.profilePath(p.Name), out); err != nil {
			return err
		}
	}

	o.applySettings(settings, opts, profiles, keep)

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(o.configPath(), out)
}

func (o *openhands) Disable() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	manifest, err := o.readManifest()
	if err != nil {
		return err
	}
	if manifest != nil {
		if manifest.SettingsExisted {
			restored := false
			if manifest.SettingsBackup != "" {
				if data, err := os.ReadFile(manifest.SettingsBackup); err == nil {
					if err := atomicWrite(o.configPath(), data); err != nil {
						return err
					}
					restored = true
				}
			}
			if !restored {
				// The restore point vanished; at least drop the markers so the
				// adapter does not report itself enabled forever.
				if err := o.stripSettings(); err != nil {
					return err
				}
			}
		} else {
			if err := os.Remove(o.configPath()); err != nil && !os.IsNotExist(err) {
				return err
			}
		}

		restoredProfiles := make(map[string]bool, len(manifest.Profiles))
		for name, st := range manifest.Profiles {
			restoredProfiles[name] = true
			path := o.profilePath(name)
			if st.Existed {
				if st.Backup != "" {
					if data, err := os.ReadFile(st.Backup); err == nil {
						if err := atomicWrite(path, data); err != nil {
							return err
						}
						continue
					}
				}
				o.unmarkProfile(name)
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		o.removeMarkedProfiles(restoredProfiles)
		if err := os.Remove(o.manifestPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	// No manifest: either the user enabled with an older zen-gate build, or a
	// backup exists from one. The oldest settings.json backup is the true
	// pre-injection copy (older builds wrote one on every Enable).
	if b := oldestBackup("openhands", "settings.json"); b != "" {
		if data, err := os.ReadFile(b); err == nil {
			if err := atomicWrite(o.configPath(), data); err != nil {
				return err
			}
			o.removeMarkedProfiles(nil)
			return nil
		}
	}

	if err := o.stripSettings(); err != nil {
		return err
	}
	o.removeMarkedProfiles(nil)
	return nil
}

// desiredProfiles maps the catalog to OpenHands profiles: the default first
// (its name is referenced by OpenHands' seeded agent profile), then one profile
// per remaining model.
func (o *openhands) desiredProfiles(opts Options) []ohProfile {
	byID := make(map[string]lane.ModelInfo, len(opts.Models))
	order := make([]string, 0, len(opts.Models))
	for _, m := range opts.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = m
		order = append(order, id)
	}

	def := strings.TrimSpace(opts.DefaultModel)
	if _, ok := byID[def]; !ok && len(order) > 0 {
		def = order[0]
	}
	if def == "" {
		return nil
	}

	out := []ohProfile{{Name: ohDefaultProfile, ModelID: def, Info: byID[def]}}
	used := map[string]bool{ohDefaultProfile: true}
	for _, id := range order {
		if id == def {
			continue
		}
		if len(out) >= ohMaxProfiles {
			break
		}
		name := ohUniqueName(ohProfileNameFor(id), used)
		used[name] = true
		out = append(out, ohProfile{Name: name, ModelID: id, Info: byID[id]})
	}
	return out
}

// ohProfileNameFor turns a model id into a legal OpenHands profile name.
// Model ids may contain '/', ':' or other separators; the name pattern only
// allows letters, digits, '.', '_' and '-'.
func ohProfileNameFor(modelID string) string {
	var b strings.Builder
	for _, r := range modelID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), ".-_")
	if s == "" {
		s = "model"
	}
	const maxLen = 64 - len(ohProfilePrefix)
	if len(s) > maxLen {
		sum := sha256.Sum256([]byte(modelID))
		suffix := "-" + hex.EncodeToString(sum[:4])
		keep := maxLen - len(suffix)
		if keep < 1 {
			keep = 1
		}
		s = s[:keep] + suffix
	}
	return ohProfilePrefix + s
}

func ohUniqueName(base string, used map[string]bool) string {
	if !used[base] && ohProfileNameRe.MatchString(base) {
		return base
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		cand := base
		if len(cand)+len(suffix) > 64 {
			cand = cand[:64-len(suffix)]
		}
		cand += suffix
		if !used[cand] && ohProfileNameRe.MatchString(cand) {
			return cand
		}
	}
}

// ohModelRef forces LiteLLM onto its OpenAI-compatible transport. A bare model
// name with a custom base_url makes LiteLLM raise "LLM Provider NOT provided";
// the "openai/" prefix is stripped again before the request reaches the
// gateway, so the gateway still sees the catalog id.
func ohModelRef(modelID string) string {
	return "openai/" + modelID
}

func (o *openhands) profilePayload(opts Options, p ohProfile) map[string]interface{} {
	payload := map[string]interface{}{
		"schema_version":   1,
		"model":            ohModelRef(p.ModelID),
		"base_url":         opts.BaseURL,
		"auth_type":        "api_key",
		"usage_id":         p.Name,
		"drop_params":      true,
		"zen_gate_managed": true,
	}
	if opts.APIKey != "" {
		payload["api_key"] = opts.APIKey
	}
	if p.Info.ContextWindow > 0 {
		payload["max_input_tokens"] = p.Info.ContextWindow
	}
	if p.Info.MaxOutput > 0 {
		payload["max_output_tokens"] = p.Info.MaxOutput
	}
	if p.Info.Vision {
		payload["capability_overrides"] = map[string]interface{}{"supports_vision": true}
	}
	return payload
}

func (o *openhands) applySettings(settings map[string]interface{}, opts Options, profiles []ohProfile, keep map[string]bool) {
	agentSettings, _ := settings["agent_settings"].(map[string]interface{})
	if agentSettings == nil {
		agentSettings = map[string]interface{}{}
		settings["agent_settings"] = agentSettings
	}
	llm, _ := agentSettings["llm"].(map[string]interface{})
	if llm == nil {
		llm = map[string]interface{}{}
		agentSettings["llm"] = llm
	}

	modelRef := ""
	if len(profiles) > 0 {
		modelRef = ohModelRef(profiles[0].ModelID)
	}
	if modelRef != "" {
		llm["model"] = modelRef
	}
	llm["api_key"] = opts.APIKey
	llm["base_url"] = opts.BaseURL
	llm["auth_type"] = "api_key"
	delete(llm, "provider_connection_id")
	delete(llm, "subscription_vendor")
	delete(llm, "model_canonical_name")
	if _, ok := llm["api_mode"]; ok {
		llm["api_mode"] = "auto"
	}

	if len(profiles) > 0 {
		info := profiles[0].Info
		if info.ContextWindow > 0 {
			llm["max_input_tokens"] = info.ContextWindow
		}
		if info.MaxOutput > 0 {
			llm["max_output_tokens"] = info.MaxOutput
		}
		if info.Vision {
			caps, _ := llm["capability_overrides"].(map[string]interface{})
			if caps == nil {
				caps = map[string]interface{}{}
				llm["capability_overrides"] = caps
			}
			caps["supports_vision"] = true
		}
	}

	agentSettings["zen_gate_managed"] = true

	// misc_settings is opaque to the agent-server, so this marker survives a
	// settings save even when the validated agent_settings block drops it.
	misc, _ := settings["misc_settings"].(map[string]interface{})
	if misc == nil {
		misc = map[string]interface{}{}
		settings["misc_settings"] = misc
	}
	misc["zen_gate_managed"] = true

	// Legacy top-level fields, kept for older OpenHands builds.
	if modelRef != "" {
		settings["llm_model"] = modelRef
	}
	settings["llm_base_url"] = opts.BaseURL
	settings["llm_api_key"] = opts.APIKey
	settings["llm_api_key_set"] = true

	if len(profiles) > 0 {
		active, _ := settings["active_profile"].(string)
		switch {
		case active == "":
			settings["active_profile"] = ohDefaultProfile
		case !keep[active]:
			// A user profile is left alone; a managed profile that left the
			// roster (or was deleted) falls back to default.
			if _, err := os.Stat(o.profilePath(active)); os.IsNotExist(err) {
				settings["active_profile"] = ohDefaultProfile
			}
		}
	}
}

func (o *openhands) readSettings() (map[string]interface{}, []byte, error) {
	data, err := os.ReadFile(o.configPath())
	if os.IsNotExist(err) {
		return map[string]interface{}{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, nil, fmt.Errorf("openhands: parse settings.json: %w", err)
	}
	return settings, data, nil
}

func (o *openhands) stripSettings() error {
	data, err := os.ReadFile(o.configPath())
	if err != nil {
		return nil
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}
	if agentSettings, ok := settings["agent_settings"].(map[string]interface{}); ok {
		delete(agentSettings, "zen_gate_managed")
	}
	if misc, ok := settings["misc_settings"].(map[string]interface{}); ok {
		delete(misc, "zen_gate_managed")
	}
	delete(settings, "llm_model")
	delete(settings, "llm_base_url")
	delete(settings, "llm_api_key")
	delete(settings, "llm_api_key_set")
	if active, _ := settings["active_profile"].(string); ohManagedProfileName(active) {
		settings["active_profile"] = nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil
	}
	return atomicWrite(o.configPath(), out)
}

func ohSettingsManaged(settings map[string]interface{}) bool {
	if agentSettings, ok := settings["agent_settings"].(map[string]interface{}); ok {
		if managed, _ := agentSettings["zen_gate_managed"].(bool); managed {
			return true
		}
	}
	if misc, ok := settings["misc_settings"].(map[string]interface{}); ok {
		if managed, _ := misc["zen_gate_managed"].(bool); managed {
			return true
		}
	}
	return false
}

func ohManagedProfileName(name string) bool {
	return name == ohDefaultProfile || strings.HasPrefix(name, ohProfilePrefix)
}

func (o *openhands) profileManaged(name string) bool {
	data, err := os.ReadFile(o.profilePath(name))
	if err != nil {
		return false
	}
	var payload map[string]interface{}
	if json.Unmarshal(data, &payload) != nil {
		return false
	}
	managed, _ := payload["zen_gate_managed"].(bool)
	return managed
}

// unmarkProfile drops the ownership marker without touching the rest of the
// profile. Used when the original backup is gone: the file cannot be restored,
// but it must not keep IsEnabled true after a disable.
func (o *openhands) unmarkProfile(name string) {
	path := o.profilePath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var payload map[string]interface{}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	if _, ok := payload["zen_gate_managed"]; !ok {
		return
	}
	delete(payload, "zen_gate_managed")
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWrite(path, out)
}

func (o *openhands) readManifest() (*ohManifest, error) {
	data, err := os.ReadFile(o.manifestPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m ohManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("openhands: parse backup manifest: %w", err)
	}
	if m.Profiles == nil {
		m.Profiles = map[string]ohProfileState{}
	}
	return &m, nil
}

func (o *openhands) saveManifest(m *ohManifest) error {
	if err := os.MkdirAll(filepath.Dir(o.manifestPath()), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(o.manifestPath(), out)
}

func (o *openhands) ensureManifest(settingsData []byte, profiles []ohProfile) (*ohManifest, error) {
	m, err := o.readManifest()
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = &ohManifest{Version: ohManifestVersion, Profiles: map[string]ohProfileState{}}
		if settingsData != nil {
			m.SettingsExisted = true
			if ohSettingsManagedBytes(settingsData) {
				// Already injected by an older build: its oldest backup is the
				// user's original settings. If that backup is gone, keep
				// SettingsBackup empty so Disable strips our markers instead of
				// restoring a copy of our own injection.
				m.SettingsBackup = oldestBackup("openhands", "settings.json")
			} else if b, err := backupFile("openhands", o.configPath(), settingsData); err == nil {
				m.SettingsBackup = b
			} else {
				return nil, err
			}
		}
		if err := o.saveManifest(m); err != nil {
			return nil, err
		}
	}

	changed := false
	for _, p := range profiles {
		if _, ok := m.Profiles[p.Name]; ok {
			continue
		}
		path := o.profilePath(p.Name)
		data, err := os.ReadFile(path)
		if err == nil && o.profileManaged(p.Name) {
			// Left behind by an earlier cycle whose manifest was lost. It is
			// our content, not a user original, so record it as ours to remove.
			m.Profiles[p.Name] = ohProfileState{}
		} else if err == nil {
			b, err := backupFile(ohProfilesBackupDir, path, data)
			if err != nil {
				return nil, err
			}
			m.Profiles[p.Name] = ohProfileState{Existed: true, Backup: b}
		} else if os.IsNotExist(err) {
			m.Profiles[p.Name] = ohProfileState{}
		} else {
			return nil, err
		}
		changed = true
	}
	if changed {
		if err := o.saveManifest(m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func ohSettingsManagedBytes(data []byte) bool {
	var settings map[string]interface{}
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	return ohSettingsManaged(settings)
}

func (o *openhands) pruneProfiles(keep map[string]bool, manifest *ohManifest) error {
	entries, err := os.ReadDir(o.profilesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	changed := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if keep[name] {
			continue
		}
		path := o.profilePath(name)
		if st, ok := manifest.Profiles[name]; ok {
			// This profile was ours or one we overwrote in an earlier pass.
			// Hand the original back instead of leaving our content behind.
			if st.Existed {
				restored := false
				if st.Backup != "" {
					if data, err := os.ReadFile(st.Backup); err == nil {
						if err := atomicWrite(path, data); err != nil {
							return err
						}
						restored = true
					}
				}
				if !restored {
					// The original is gone; at least stop claiming the file.
					o.unmarkProfile(name)
				}
			} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			delete(manifest.Profiles, name)
			changed = true
			continue
		}
		if ohManagedProfileName(name) && o.profileManaged(name) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if changed {
		return o.saveManifest(manifest)
	}
	return nil
}

func (o *openhands) removeMarkedProfiles(skip map[string]bool) {
	entries, err := os.ReadDir(o.profilesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if skip[name] || !ohManagedProfileName(name) {
			continue
		}
		if o.profileManaged(name) {
			_ = os.Remove(o.profilePath(name))
		}
	}
}

var _ Agent = (*openhands)(nil)
