// Package agents auto-detects locally installed AI agents and injects (or
// removes) a zen-gate provider entry so the agent's model picker shows the
// free lane. Every adapter follows the same contract: Detect → Enable (backup,
// atomic write, rollback on failure) → Disable (restore).
package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	agents      []Agent
	st          *store.Store
	baseURL     string
	modelsCache []lane.ModelInfo
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
		},
	}
}

// SetEndpoints supplies the live base URL and model list (main wires this).
func (r *Registry) SetEndpoints(baseURL string, models []lane.ModelInfo) {
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
		if installed && id == "dsh" && processRunning("DeepSeek Harness.exe") {
			v.Warn = "DeepSeek Harness 正在运行，启用/停用后需重启应用生效"
		}
		if installed && id == "zcode" && processRunning("ZCode.exe", "zcode.exe") {
			v.Warn = "ZCode 正在运行，启用后需重启 ZCode 生效"
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
	if err := a.Enable(r.options()); err != nil {
		return err
	}
	r.st.Config().EnabledAgents[id] = true
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
	r.st.Config().EnabledAgents[id] = false
	_ = r.st.Save()
	return err
}

func (r *Registry) byID(id string) (Agent, bool) {
	for _, a := range r.agents {
		if aid, _, _ := a.Meta(); aid == id {
			return a, true
		}
	}
	return nil, false
}

func (r *Registry) options() Options {
	def := ""
	if len(r.modelsCache) > 0 {
		def = r.modelsCache[0].ID
	}
	return Options{
		BaseURL:      r.baseURL,
		APIKey:       r.st.Config().MainKey,
		Models:       r.servableModels(),
		DefaultModel: def,
	}
}

// servableModels is the lane's models plus every user-configured upstream's,
// in the shape the adapters expect.
//
// The adapters write a static sidecar catalog (codex writes
// ~/.codex/zen-gate-catalog.json), while the gateway also serves a dynamic
// /v1/codex-catalog. Both must list the same set: Codex prefers the dynamic one
// when it can reach the gateway, but falls back to the static file when it
// cannot, and a model that appears in only one of them looks like a flaky
// entry. Feeding both from here keeps them identical by construction.
//
// A configured id that collides with a free-lane model keeps the free entry:
// the lane entry already has curated blurbs and live availability, and the
// gateway routes the id to the lane first anyway.
func (r *Registry) servableModels() []lane.ModelInfo {
	models := r.modelsCache
	if ups := r.st.Config().Upstreams; len(ups) > 0 {
		models = make([]lane.ModelInfo, 0, len(r.modelsCache)+len(ups))
		models = append(models, r.modelsCache...)
		seen := map[string]bool{}
		for _, m := range r.modelsCache {
			seen[m.ID] = true
		}
		for _, u := range ups {
			for _, m := range u.Models {
				id := strings.TrimSpace(m.ID)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				name := strings.TrimSpace(m.Name)
				if name == "" {
					name = id
				}
				cw, mo := m.ContextWindow, m.MaxOutput
				if cw <= 0 {
					cw = store.DefaultContextWindow
				}
				if mo <= 0 {
					mo = store.DefaultMaxOutput
				}
				models = append(models, lane.ModelInfo{
					ID: id, Name: name,
					Blurb:         firstNonEmptyString(strings.TrimSpace(m.Blurb), "自定义上游 · "+u.Name),
					Vision:        m.Vision,
					Reasoning:     m.Reasoning,
					ContextWindow: cw,
					MaxOutput:     mo,
					// Custom entries sort below the free lane. Pickers already
					// have a "sink this" lever (the region-gated marker), and
					// reusing it would also attach a wrong "可能被地区门拦截"
					// label to a model running on the user's own disk.
					Custom:          true,
					RegionSensitive: u.ExposeRegion,
				})
			}
		}
	}
	return models
}

func firstNonEmptyString(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	cmd := exec.Command("tasklist", "/FO", "CSV", "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(out))
	for _, n := range imageNames {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
