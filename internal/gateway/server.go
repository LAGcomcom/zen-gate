// Package gateway serves the local OpenAI/Anthropic-compatible API and the
// admin dashboard. Loopback-only by construction: the listener binds the
// resolved loopback address and nothing else.
package gateway

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	_ "embed"
	"time"
	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// Version is the running build; overridable via -ldflags.
var Version = "1.1.0"

//go:embed web/favicon.png
var faviconPNG []byte

// startedAt records process boot for the uptime display.
var startedAt = time.Now()

// Server is the HTTP surface.
type Server struct {
	Lane  *lane.Lane
	Store *store.Store
	// registry is the agent-adapter set, injected by main after construction.
	registry AgentRegistry
	mux      *http.ServeMux
	srv      *http.Server
	addr     atomic.Value // string
	// logger is injected by main; nil-safe helpers fall back to empty.
	logger interface {
		Tail(minLevel string, limit int) []logx.Entry
		Infof(format string, args ...interface{})
		Warnf(format string, args ...interface{})
		Errorf(format string, args ...interface{})
	}
	autostart       func() bool
	updateAvailable bool
	updateVersion   string
	updateURL       string
}

// SetAgents wires the agent registry.
func (s *Server) SetAgents(reg AgentRegistry) { s.registry = reg }

// SetLogger wires the logx logger (as a structural interface, no import cycle).
func (s *Server) SetLogger(l interface {
	Tail(minLevel string, limit int) []logx.Entry
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}) {
	s.logger = l
}

// SetAutostartState wires the registry-backed autostart state reader.
func (s *Server) SetAutostartState(f func() bool) { s.autostart = f }

// SetUpdateState records the latest update-check result for the banner.
func (s *Server) SetUpdateState(available bool, version, url string) {
	s.updateAvailable, s.updateVersion, s.updateURL = available, version, url
}

// autostartState reads the registry-backed autostart toggle.
func (s *Server) autostartState() bool {
	if s.autostart != nil {
		return s.autostart()
	}
	return false
}

// New builds a server; Handler is immediately usable (for tests).
func New(l *lane.Lane, st *store.Store) *Server {
	s := &Server{Lane: l, Store: st}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.safeRoute)
	s.mux = mux
	return s
}

// safeRoute keeps a handler panic from killing the connection silently.
func (s *Server) safeRoute(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			if s.logger != nil {
				s.logger.Errorf("handler panic: %v", p)
			}
			writeJSON(w, 500, map[string]any{"error": map[string]any{
				"message": "internal panic (recovered)", "type": "api_error"}})
		}
	}()
	s.route(w, r)
}

// Start binds loopback and serves in the background.
func (s *Server) Start() error {
	port := s.Store.Config().Port
	// Bind the resolved loopback IP, never a routable interface.
	ip := net.ParseIP("127.0.0.1")
	addr := fmt.Sprintf("%s:%d", ip, port)
	s.srv = &http.Server{Addr: addr, Handler: s.mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.addr.Store(ln.Addr().String())
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[zen-gate] http serve: %v", err)
		}
	}()
	return nil
}

// Stop shuts the listener down.
func (s *Server) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

// BaseURL is the externally advertised endpoint.
func (s *Server) BaseURL() string {
	if a, ok := s.addr.Load().(string); ok {
		return "http://" + a + "/v1"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", s.Store.Config().Port)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/health":
		writeJSON(w, 200, map[string]any{"ok": true, "service": "zen-gate"})
	case path == "/v1/models" && r.Method == http.MethodGet:
		if !s.authorized(w, r) {
			return
		}
		s.handleListModels(w, r)
	case path == "/v1/codex-catalog" && r.Method == http.MethodGet:
		s.handleCodexCatalog(w, r)
	case path == "/v1/chat/completions" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleChatCompletions(w, r)
	case path == "/v1/responses" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleResponses(w, r)
	case path == "/v1/messages" && r.Method == http.MethodPost:
		if !s.authorized(w, r) {
			return
		}
		s.handleAnthropicMessages(w, r)
	case path == "/favicon.png":
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(faviconPNG)
	case path == "" || path == "/" || path == "/admin":
		s.serveDashboard(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		s.handleAdmin(w, r, strings.TrimPrefix(path, "/admin/api/"))
	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "not found", "type": "invalid_request_error", "code": "not_found"}})
	}
}

// authorized checks the Bearer / x-api-key key against the main key and every
// agent subkey, returning the caller's label via context-free lookup below.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	key := bearerOf(r)
	if key == "" {
		writeJSON(w, 401, openaiError("missing API key", "authentication_error"))
		return false
	}
	if !s.keyMatches(key) {
		writeJSON(w, 401, openaiError("invalid API key", "authentication_error"))
		return false
	}
	return true
}

// agentOf resolves which label a key belongs to ("" = main key / unknown).
func (s *Server) agentOf(r *http.Request) string {
	key := bearerOf(r)
	cfg := s.Store.Config()
	if subtle.ConstantTimeCompare([]byte(key), []byte(cfg.MainKey)) == 1 {
		return ""
	}
	for id, k := range cfg.AgentKeys {
		if k != "" && subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			return id
		}
	}
	return ""
}

func (s *Server) keyMatches(key string) bool {
	cfg := s.Store.Config()
	if len(key) > 0 && subtle.ConstantTimeCompare([]byte(key), []byte(cfg.MainKey)) == 1 {
		return true
	}
	for _, k := range cfg.AgentKeys {
		if k != "" && len(k) == len(key) && subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// handleListModels advertises the servable models in OpenAI shape. Reasoning
// models additionally expose (light)/(deep) variants so a client's model
// picker can select the effort directly. User-configured upstreams are
// appended: they are servable through this same gateway, so a client that
// discovers models here can reach them.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.Lane.ServableModels() {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "zen-gate",
		})
		if m.Reasoning {
			for _, suffix := range []string{"(light)", "(deep)"} {
				data = append(data, map[string]any{
					"id":       m.ID + " " + suffix,
					"object":   "model",
					"owned_by": "zen-gate",
				})
			}
		}
	}
	for _, um := range s.upstreamModels() {
		data = append(data, map[string]any{
			"id":       um.info.ID,
			"object":   "model",
			"owned_by": "zen-gate/" + um.upstreamID,
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// handleCodexCatalog serves the model list in Codex's remote-catalog shape so
// the desktop model picker can offer the free models under the zen_gate
// provider. Deliberately unauthenticated: the listing is non-sensitive, and a
// 401 here makes Codex surface a login prompt instead of the models.
func (s *Server) handleCodexCatalog(w http.ResponseWriter, r *http.Request) {
	models, av, _ := s.Lane.Snapshot()
	priority := map[string]int{
		lane.StateAvailable: 100,
		lane.StateUnknown:   50,
		lane.StateThrottled: 20,
	}
	stateOf := func(id string) string {
		if p, ok := av[id]; ok {
			return p.State
		}
		return lane.StateUnknown
	}
	sort.SliceStable(models, func(i, j int) bool {
		pi, pj := priority[stateOf(models[i].ID)], priority[stateOf(models[j].ID)]
		// Region-gated models sink to the bottom: with a CN egress they only
		// ever answer with a RegionError.
		if models[i].RegionSensitive != models[j].RegionSensitive {
			return models[j].RegionSensitive
		}
		return pi > pj
	})
	levels := []map[string]any{
		{"effort": "low", "description": "轻量，最省额度"},
		{"effort": "medium", "description": "均衡"},
		{"effort": "high", "description": "深思"},
	}
	out := []map[string]any{}
	for _, m := range models {
		desc := "免费车道 · 当前不可用"
		switch stateOf(m.ID) {
		case lane.StateAvailable:
			desc = "免费车道 · 实测可用"
		case lane.StateThrottled:
			desc = "免费车道 · 已限额，稍后恢复"
		case lane.StateUnknown:
			desc = "免费车道 · 尚未探测"
		}
		if m.RegionSensitive {
			desc += " · 可能被地区门拦截"
		}
		lv := levels
		if !m.Reasoning {
			lv = []map[string]any{{"effort": "medium", "description": "默认"}}
		}
		mods := []string{"text"}
		if m.Vision {
			mods = append(mods, "image")
		}
		out = append(out, map[string]any{
			"slug":                         m.ID,
			"display_name":                 m.Name,
			"description":                  desc,
			"base_instructions":            codexInstructions(m.Name, m.ID),
			"default_reasoning_level":      "medium",
			"supported_reasoning_levels":   lv,
			"shell_type":                   "unified_exec",
			"support_verbosity":            false,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"experimental_supported_tools": []string{},
			"input_modalities":             mods,
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     priority[stateOf(m.ID)],
			"provider_id":                  codexProviderID,
			"context_window":               m.ContextWindow,
			"max_output_tokens":            m.MaxOutput,
		})
	}
	// User-configured upstream models ride the same catalog so the picker shows
	// local models next to the free ones and keeps following upstream changes.
	// They sort below the free lane by priority, which is deliberate: the free
	// models are the product, a custom endpoint is the user's own addition.
	// Priority 5 mirrors the codex adapter's sidecar catalog so the two agree.
	for _, um := range s.upstreamModels() {
		f := um.info
		desc := f.Blurb
		if f.RegionGated {
			desc += " · 可能被地区门拦截"
		}
		lv := levels
		if !f.Reasoning {
			lv = []map[string]any{{"effort": "medium", "description": "默认"}}
		}
		mods := []string{"text"}
		if f.Vision {
			mods = append(mods, "image")
		}
		out = append(out, map[string]any{
			"slug":                         f.ID,
			"display_name":                 f.Name,
			"description":                  desc,
			"base_instructions":            codexInstructions(f.Name, f.ID),
			"default_reasoning_level":      "medium",
			"supported_reasoning_levels":   lv,
			"shell_type":                   "unified_exec",
			"support_verbosity":            false,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"experimental_supported_tools": []string{},
			"input_modalities":             mods,
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     5,
			"provider_id":                  codexProviderID,
			"context_window":               f.ContextWindow,
			"max_output_tokens":            f.MaxOutput,
		})
	}
	writeJSON(w, 200, map[string]any{"models": out})
}

// codexProviderID is the provider the free-lane models are pinned to. Custom
// upstream models get one provider each so the picker can tell them apart and
// so a name collision with a free model cannot misroute a request.
const codexProviderID = "zen_gate"

// codexInstructions is the system preamble Codex bakes into every model it
// loads. One wording for both free-lane and custom models: the coding-agent
// contract does not change with the provider.
func codexInstructions(name, model string) string {
	return fmt.Sprintf("You are %s (model id: %s), a coding agent serving the user's Codex app through the Zen Gate local gateway. Work toward the user's goal with the available tools and verify your changes. Always reply in the same language as the user's latest message — when the user writes Chinese, reply in Simplified Chinese. Be concise.", name, model)
}

// resolveEffort maps a requested model id onto an effort level: an explicit
// "(light)/(balanced)/(deep)" model-name suffix wins, then an effort the
// client declared in the request body (reasoning_effort and friends, as sent
// by ZCode's thought-level selector), then the configured default.
func (s *Server) resolveEffort(model string, declared ...string) string {
	if e := lane.EffortOf(model); e != "" {
		return e
	}
	for _, d := range declared {
		if e := normalizeEffortParam(d); e != "" {
			return e
		}
	}
	if e := s.Store.Config().DefaultEffort; e == "light" || e == "deep" {
		return e
	}
	return "balanced"
}

// normalizeEffortParam maps client effort spellings onto the three budgets.
// The lane cannot truly switch thinking off, so off-ish levels land on light.
func normalizeEffortParam(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light":
		return "light"
	case "balanced", "medium":
		return "balanced"
	case "deep", "high", "xhigh":
		return "deep"
	case "minimal", "low", "none", "off", "disabled":
		return "light"
	}
	return ""
}

// --- session seeding ---------------------------------------------------------

// SessionSeed derives a stable upstream session identity for one conversation.
// Quota is accounted per session upstream, so the same conversation must keep
// landing on the same session — across restarts too.
func SessionSeed(agent, userField string, messages []lane.Message) string {
	first := ""
	for _, m := range messages {
		if m.Role == lane.RoleUser {
			first = m.TextOf()
			break
		}
	}
	if len(first) > 500 {
		first = first[:500]
	}
	parts := []string{agent, userField, first}
	joined := ""
	for _, p := range parts {
		joined += p + "\x00"
	}
	return joined
}

// TurnSeed is stable across retries of one turn, changes next turn.
func TurnSeed(messages []lane.Message) string {
	total := 0
	for _, m := range messages {
		total += len(m.TextOf()) + 8
	}
	// A cheap, stable fingerprint: roles + total text length + last message.
	last := ""
	if len(messages) > 0 {
		last = messages[len(messages)-1].TextOf()
		if len(last) > 200 {
			last = last[:200]
		}
	}
	return fmt.Sprintf("%d|%d|%s", len(messages), total, last)
}

// --- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func openaiError(message, typ string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": typ, "code": nil}}
}

// setRetryAfter surfaces the upstream Retry-After (already parsed in the lane)
// to clients that back off on 429s.
func setRetryAfter(w http.ResponseWriter, uerr *lane.UpstreamError) {
	if uerr != nil && uerr.Code == lane.CodeQuota && uerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(uerr.RetryAfter))
	}
}

// withSuggestions appends failover candidates to a 429 error body so a client
// whose request burned out can immediately retry with a model that answers.
func withSuggestions(body map[string]any, sugg []string) map[string]any {
	if len(sugg) == 0 {
		return body
	}
	if errObj, ok := body["error"].(map[string]any); ok {
		errObj["suggestions"] = sugg
	}
	return body
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
