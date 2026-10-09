// Package agents auto-detects locally installed AI agents and injects (or
// removes) a zen-gate provider entry so the agent's model picker shows the
// free lane. Every adapter follows the same contract: Detect → Enable (backup,
// atomic write, rollback on failure) → Disable (restore).
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// View is one agent's state for the dashboard.
type View struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Enabled   bool   `json:"enabled"`
	Warn      string `json:"warn,omitempty"`
}

// Options carries what an adapter needs to inject.
type Options struct {
	BaseURL      string // http://127.0.0.1:PORT/v1
	APIKey       string
	Models       []lane.ModelInfo
	DefaultModel string
}

// Agent is one adapter.
type Agent interface {
	Meta() (id, name, describe string)
	Detect() (installed bool, version, detail string)
	IsEnabled() (bool, string, error)
	Enable(o Options) error
	Disable() error
}

// Registry holds all adapters.
type Registry struct {
	agents []Agent
	st     *store.Store
	mu     sync.Mutex
	// baseURL/modelsCache are written by SetEndpoints (main's syncEndpoints,
	// which OnChange fires from lane goroutines) and read by ResyncEnabled —
	// both can run off the UI goroutine, hence the mutex.
	baseURL     string
	modelsCache []lane.ModelInfo
	// injectedKey fingerprints the model roster the enabled adapters were
	// last written with; see ResyncEnabled.
	injectedKey string
}

// NewRegistry builds the built-in adapter set.
func NewRegistry(st *store.Store) *Registry {
	return &Registry{
		st: st,
		agents: []Agent{
			newZCode(),
			newOpenCode(),
			newCodex(),
			newClaude(),
			newDSH(),
			newCrush(),
			newChatBox(),
			newAider(),
			newQwenCode(),
			newContinueIDE(),
			newWorkBuddy(),
			newQoder(),
		},
	}
}

// SetEndpoints supplies the live base URL and model list (main wires this).
func (r *Registry) SetEndpoints(baseURL string, models []lane.ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseURL = baseURL
	r.modelsCache = models
}

// Views snapshots every adapter's state.
func (r *Registry) Views() []View {
	out := []View{}
	for _, a := range r.agents {
		id, name, describe := a.Meta()
		installed, version, detail := a.Detect()
		enabled, enDetail, _ := a.IsEnabled()
		v := View{
			ID:        id,
			Name:      name,
			Detail:    describe,
			Installed: installed,
			Version:   version,
			Enabled:   enabled,
		}
		if detail != "" {
			v.Detail = detail
		}
		if enDetail != "" {
			v.Detail = enDetail
		}
		if installed && id == "dsh" && processRunning("DeepSeek Harness") {
			v.Warn = "DeepSeek Harness 正在运行，启用/停用后需重启应用生效"
		}
		if installed && id == "zcode" && processRunning("ZCode") {
			v.Warn = "ZCode 正在运行，启用后需重启 ZCode 生效"
		}
		if installed && id == "workbuddy" && processRunning("WorkBuddy") {
			v.Warn = "WorkBuddy 正在运行，配置保存后约 1 秒自动热加载，无需重启"
		}
		if installed && id == "qoder" && processRunning("Qoder CN", "Qoder") {
			v.Warn = "Qoder 正在运行，若模型列表未刷新请重启 Qoder 生效"
		}
		out = append(out, v)
	}
	return out
}

// Enable enables one adapter by id.
func (r *Registry) Enable(id string) error {
	a, ok := r.byID(id)
	if !ok {
		return fmt.Errorf("unknown agent %q", id)
	}
	opts, key := r.snapshot()
	if err := a.Enable(opts); err != nil {
		return err
	}
	// The manual enable just wrote the current list; record that so a Resync
	// of the same content does not stack a second backup for nothing.
	r.commit(key)
	r.st.Mutate(func(cfg *store.Config) {
		if cfg.EnabledAgents == nil {
			cfg.EnabledAgents = map[string]bool{}
		}
		cfg.EnabledAgents[id] = true
	})
	_ = r.st.Save()
	return nil
}

// Disable disables one adapter by id.
func (r *Registry) Disable(id string) error {
	a, ok := r.byID(id)
	if !ok {
		return fmt.Errorf("unknown agent %q", id)
	}
	err := a.Disable()
	if err == nil {
		// Nothing is injected now; reset the fingerprint so the next Enable
		// of the same list still writes instead of reading "unchanged".
		r.commit("")
	}
	r.st.Mutate(func(cfg *store.Config) {
		if cfg.EnabledAgents == nil {
			cfg.EnabledAgents = map[string]bool{}
		}
		cfg.EnabledAgents[id] = false
	})
	_ = r.st.Save()
	return err
}

// ResyncEnabled re-injects the config of every enabled adapter with the
// current model list — but only when that list changed since the last
// successful pass. main wires it into OnChange (issue #26: a model joining
// the catalog never reached any agent config), and OnChange fires on every
// probe round; without the fingerprint gate, every idle round would rewrite
// every enabled agent and stack another timestamped backup — backups are
// never pruned. Adapters back up and write atomically, so a repeat Enable of
// the same content is harmless but wasteful; the gate makes it a no-op.
func (r *Registry) ResyncEnabled() int {
	opts, key := r.snapshot()
	if key == r.currentKey() {
		return 0
	}
	var attempted, done int
	for _, a := range r.agents {
		if enabled, _, err := a.IsEnabled(); err != nil || !enabled {
			continue
		}
		attempted++
		if err := a.Enable(opts); err == nil {
			done++
		}
	}
	// Commit only when every enabled adapter took the write: a partial pass
	// must retry the whole list next round, or a failing agent would sit on
	// stale content forever behind the fingerprint.
	if done == attempted {
		r.commit(key)
	}
	return done
}

func (r *Registry) byID(id string) (Agent, bool) {
	for _, a := range r.agents {
		if aid, _, _ := a.Meta(); aid == id {
			return a, true
		}
	}
	return nil, false
}

// snapshot returns the current injection options plus a fingerprint of
// everything they are made of (base URL and per-model id/context/max-output/
// reasoning/modalities — exactly what the injected files carry). Both derive
// from one locked read so a caller can compare against the previous key and
// later write the same content without a window where the cache shifted.
// Models sort by id before fingerprinting: the picker order is the upstream
// listing order and can include transient availability wobble, and a
// reorder-only roster writes byte-identical content — committing on it would
// freeze out a later genuine change that sorts into the same prefix.
func (r *Registry) snapshot() (Options, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	def := ""
	if len(r.modelsCache) > 0 {
		def = r.modelsCache[0].ID
	}
	sorted := make([]lane.ModelInfo, len(r.modelsCache))
	copy(sorted, r.modelsCache)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var b strings.Builder
	b.WriteString(r.baseURL)
	for _, m := range sorted {
		fmt.Fprintf(&b, "\x00%s|%d|%d|%v|%v|%v|%v", m.ID, m.ContextWindow, m.MaxOutput, m.Reasoning, m.Vision, m.AudioInput, m.FileInput)
	}
	o := Options{
		BaseURL:      r.baseURL,
		APIKey:       r.st.Config().MainKey,
		Models:       r.modelsCache,
		DefaultModel: def,
	}
	return o, b.String()
}

// currentKey reads the last committed fingerprint under the registry lock.
func (r *Registry) currentKey() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.injectedKey
}

// commit records that the enabled adapters now hold exactly this fingerprint.
func (r *Registry) commit(key string) {
	r.mu.Lock()
	r.injectedKey = key
	r.mu.Unlock()
}

// --- shared helpers ----------------------------------------------------------

func homePath(rel ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, rel...)...)
}

// atomicWrite writes a file via temp+rename, mode 0600.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".zengate.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// processRunning checks whether any of the named executables is running via
// tasklist. Best-effort: on failure it reports false. The subprocess console
// is hidden — a GUI process spawning tasklist would otherwise flash a black
// console window on every poll.
func processRunning(imageNames ...string) bool {
	return processRunningAny(imageNames)
}
