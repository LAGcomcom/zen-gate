// Package gateway serves the OpenAI/Anthropic-compatible API and the admin
// dashboard. The dashboard and its key-free /admin/api surface are
// loopback-only by construction; the API may additionally be reached from the
// LAN when store.Config.AllowLan is on, always behind an API key.
package gateway

import (
	"context"
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
	"sync"
	"sync/atomic"

	_ "embed"
	"time"
	"zen-gate/internal/announce"
	"zen-gate/internal/autotag"
	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// Version is the running build; overridable via -ldflags.
var Version = "1.1.0"

// maxRequestBody bounds one chat/responses/messages request body. The old
// 8MB cap rejected normal vision traffic — a handful of base64 screenshots
// (4/3 inflation) plus conversation history sail past it and the client just
// sees "invalid request body: http: request body too large" (issue #30).
// 64MB stays a sane ceiling for pasted images without inviting memory abuse.
const maxRequestBody = 64 << 20

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
	// listen opens the TCP listener; tests swap it to force a bind refusal.
	listen func(network, addr string) (net.Listener, error)
	// logger is injected by main; nil-safe helpers fall back to empty.
	logger logSink
	// audits maps a live request's correlation id to its attempt tally so the
	// per-attempt reporters (the lane callback and the relay recorder) can feed
	// the one access line written when the response completes.
	auditMu         sync.Mutex
	audits          map[string]*turnAudit
	autostart       func() bool
	updateAvailable bool
	updateVersion   string
	updateURL       string
	// customProbe remembers per-model verdicts for user-added providers; they
	// are probed manually from the dashboard, never on a schedule.
	customProbeMu sync.Mutex
	customProbe   map[string]lane.ProbeResult
	// customProbing is the namespaced id of the custom model under probe, so
	// a long queue (NVIDIA free lane can take 30s+) shows as 探测中 instead
	// of looking dead.
	customProbing atomic.Value // string
	// announcements is the author's published notice list, refreshed by the
	// feed loop in main; nil until the first pull succeeds.
	announcements atomic.Value // []announce.Item
	// announcementPull re-runs the feed fetch on demand (dashboard 刷新 button);
	// wired from main alongside the periodic loop.
	announcementPull func()
	modelsSync       func()
	// updateCheck re-runs the release check on demand (设置页 检测更新 button).
	updateCheck func() (bool, string)
	// subs is the sing-box sidecar manager, injected by main when the
	// subscription feature is wired; nil-safe handlers fall back to errors.
	subs SubsController
	// tagger is the AI capability tagger, injected by main; nil-safe admin
	// handlers degrade to a no-op.
	tagger *autotag.Tagger
	// probeTasks is the in-memory log of what the 模型 page's 测试 / 能力实测
	// buttons are doing right now — kept server-side because the dashboard
	// rebuilds its card grid every 10s.
	probeTasks *taskRegistry
}

// SetTagger wires the AI capability tagger.
func (s *Server) SetTagger(t *autotag.Tagger) { s.tagger = t }

// SubsController is the dashboard-facing surface of the sing-box sidecar
// manager (internal/subs.Manager): refresh subscriptions, probe nodes,
// download the binary, and report live node health.
type SubsController interface {
	Apply(ctx context.Context) error
	Refresh(ctx context.Context) error
	ProbeAll(ctx context.Context)
	Stop()
	Download(ctx context.Context) (string, error)
	SetPath(path string) error
	Healthy() int
	Status() map[string]any
}

// SetSubs wires the sidecar manager.
func (s *Server) SetSubs(m SubsController) { s.subs = m }

// SetAgents wires the agent registry.
func (s *Server) SetAgents(reg AgentRegistry) { s.registry = reg }

// SetAnnouncements records the latest feed pull — every parsed entry, active
// or ended (nil = none/no feed). adminState splits them for the dashboard so
// the 公告 card always has content to pin.
func (s *Server) SetAnnouncements(items []announce.Item) { s.announcements.Store(items) }

// SetAnnouncementPull wires the on-demand feed refresh.
func (s *Server) SetAnnouncementPull(fn func()) { s.announcementPull = fn }

// SetUpdateCheck wires the on-demand release check.
func (s *Server) SetUpdateCheck(fn func() (bool, string)) { s.updateCheck = fn }

// logSink is the logging surface the gateway needs, injected by main as a
// structural interface so logx stays free of an import cycle.
type logSink interface {
	Query(q logx.Query) []logx.Entry
	Days() []string
	Logf(cat, level, format string, args ...any)
	Enabled(cat string) bool
	SetCategories(on map[string]bool)
	SetFileLevel(level string)
	SetKeepDays(days int)
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// SetModelsSync wires the "model set changed" callback: agents' static
// configs only re-inject when this runs, so anything that edits provider
// metadata must fire it before asking for a resync.
func (s *Server) SetModelsSync(fn func()) { s.modelsSync = fn }

// SetLogger wires the logx logger.
func (s *Server) SetLogger(l logSink) { s.logger = l }

// ApplyLogSettings installs the persisted logging switches onto the live
// logger and records what is now kept — the boot line and every settings
// change need one audit trail, because a silenced class is invisible.
func (s *Server) ApplyLogSettings(cfg *store.Config) {
	if s.logger == nil {
		return
	}
	cats := logx.EffectiveCategories(cfg.LogCategories)
	s.logger.SetCategories(cats)
	s.logger.SetFileLevel(cfg.LogLevel)
	s.logger.SetKeepDays(cfg.LogKeepDays)
	s.logCat(logx.CatLifecycle, "info", "日志：记录 %s，文件级别 %s，保留 %d 天",
		strings.Join(enabledCats(cats), ","), effectiveLogLevel(cfg.LogLevel), effectiveKeepDays(cfg.LogKeepDays))
}

// enabledCats lists the classes that record, sorted so the line is stable.
func enabledCats(cats map[string]bool) []string {
	out := make([]string, 0, len(cats))
	for k, v := range cats {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// logSettingsView is the effective logging state, for the dashboard chips.
func logSettingsView(cfg *store.Config) map[string]any {
	return map[string]any{
		"logCategories": logx.EffectiveCategories(cfg.LogCategories),
		"logLevel":      effectiveLogLevel(cfg.LogLevel),
		"logKeepDays":   effectiveKeepDays(cfg.LogKeepDays),
	}
}

func effectiveLogLevel(level string) string {
	if strings.TrimSpace(level) == "" {
		return logx.DefaultFileLevel
	}
	return strings.ToLower(strings.TrimSpace(level))
}

func effectiveKeepDays(days int) int {
	if days <= 0 {
		return logx.DefaultKeepDays
	}
	return days
}

// trackAudit registers a live request's tally under its correlation id.
func (s *Server) trackAudit(rid string, a *turnAudit) {
	s.auditMu.Lock()
	if s.audits == nil {
		s.audits = map[string]*turnAudit{}
	}
	s.audits[rid] = a
	s.auditMu.Unlock()
}

func (s *Server) forgetAudit(rid string) {
	s.auditMu.Lock()
	delete(s.audits, rid)
	s.auditMu.Unlock()
}

func (s *Server) auditFor(rid string) *turnAudit {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return s.audits[rid]
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
	s := &Server{Lane: l, Store: st, listen: net.Listen, probeTasks: newTaskRegistry()}
	// The lane reports every physical attempt here — including the failovers a
	// client never sees — so both the usage stats and the per-request log lines
	// are fed from the server itself.
	l.OnCall = func(rec lane.CallRecord) {
		st.Record(rec)
		_ = st.FlushStats()
		s.NoteCall(rec)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.safeRoute)
	s.mux = mux
	// tags.json is the durable half of a verdict; the catalog is rebuilt from
	// the curated table on every boot AND on every RefreshCatalog (every probe
	// interval), so without a replay hook a measured 音频/文件 capability or an
	// openref capacity would vanish a poll later and routing/agent injection
	// would fall back to the name guess. Replay synchronously so readers woken
	// by the rebuild never see the raw catalog.
	l.OnCatalogReload = s.applyStoredTags
	s.applyStoredTags()
	return s
}

// applyStoredTags patches every persisted capability verdict into the live
// catalog. Ids that are not on the free lane (custom providers) are skipped by
// the lane itself; an effort-suffixed tag id retries on the bare id.
func (s *Server) applyStoredTags() {
	for id, tag := range s.Store.SnapshotTags() {
		caps := lane.CapabilityTags{
			ContextWindow: tag.ContextWindow, MaxOutput: tag.MaxOutput,
		}
		// A CapsOnly row (the public capacity reference) states windows but
		// nothing about modalities — feeding its unset false bools in would
		// silence audio/file the name heuristics or a curated row allowed.
		if !tag.CapsOnly {
			audio, file, vision := tag.Audio, tag.File, tag.Vision
			caps.Audio, caps.File, caps.Vision = &audio, &file, &vision
		}
		if s.Lane.ApplyCapabilityTags(id, caps) {
			continue
		}
		if base := lane.BaseModelId(id); base != id {
			s.Lane.ApplyCapabilityTags(base, caps)
		}
	}
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

// Start binds the listener and serves in the background: loopback, or every
// interface when the user turned on 局域网访问.
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.bindHost(), s.Store.Config().Port)
	s.srv = &http.Server{Addr: addr, Handler: s.mux}
	ln, err := s.listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[zen-gate] http serve: %v", err)
		}
	}()
	return nil
}

// bindHost is the interface the listener claims.
func (s *Server) bindHost() string {
	if s.Store.Config().AllowLan {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// Rebind re-opens the listener on whatever the config now asks for. In-flight
// requests are cut, so callers only use it for a deliberate change. A refused
// routable bind falls back to loopback — turning the setting off to match —
// instead of leaving the gateway with no listener at all.
func (s *Server) Rebind() error {
	allowLan := s.Store.Config().AllowLan
	if s.srv != nil {
		_ = s.srv.Close()
	}
	err := s.Start()
	if err == nil || !allowLan {
		return err
	}
	s.Store.Mutate(func(cfg *store.Config) { cfg.AllowLan = false })
	if err2 := s.Start(); err2 != nil {
		return err2
	}
	_ = s.Store.Save()
	return err
}

// Stop shuts the listener down.
func (s *Server) Stop() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

// BaseURL is the locally advertised endpoint. It stays on loopback even while
// the LAN is served: the agent adapters recognise zen-gate's own entries by
// the literal 127.0.0.1 in the URL they are handed.
func (s *Server) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", s.Store.Config().Port)
}

// LANBaseURL is the endpoint other machines on the network reach, empty while
// the LAN toggle is off or the box has no usable address.
func (s *Server) LANBaseURL() string {
	if !s.Store.Config().AllowLan {
		return ""
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	ip := lanIPv4(addrs)
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/v1", ip, s.Store.Config().Port)
}

// lanIPv4 picks the address a neighbouring machine uses. Private ranges beat
// overlay/VPN (Tailscale hands out 100/64, useless on a LAN), and within
// private, 192.168 wins because 172.16-31 tends to be VM/WSL NAT and 10/8
// corporate or container networking.
func lanIPv4(addrs []net.Addr) string {
	best, bestRank := "", 9
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		rank := 3
		switch {
		case ip4[0] == 192 && ip4[1] == 168:
			rank = 0
		case ip4[0] == 10:
			rank = 1
		case ip4.IsPrivate():
			rank = 2
		}
		if rank < bestRank {
			best, bestRank = ip4.String(), rank
		}
	}
	return best
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
		s.serveTurn(w, r, "chat", s.handleChatCompletions)
	case path == "/v1/responses" && r.Method == http.MethodPost:
		s.serveTurn(w, r, "responses", s.handleResponses)
	case path == "/v1/messages" && r.Method == http.MethodPost:
		s.serveTurn(w, r, "messages", s.handleAnthropicMessages)
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

// handleListModels advertises the servable models in OpenAI shape, plus the
// capability metadata the gateway actually believes about each one: the same
// merged verdict the router filters on. Without it an agent can only learn
// what 测试 / 能力实测 measured by scraping the dashboard.
//
// Reasoning models additionally expose (light)/(deep) variants so a client's
// model picker can select the effort directly. User-added providers ("自定义
// API") append their discovered models under "<providerID>/<model>" ids. Models
// the user unchecked on the 模型 page are not advertised.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.Store.Config()
	data := []map[string]any{}
	for _, m := range s.VisibleModels() {
		data = append(data, s.modelCard(m, m.ID, "zen-gate", cfg, true))
		if m.Reasoning {
			for _, suffix := range []string{"(light)", "(deep)"} {
				data = append(data, s.modelCard(m, m.ID+" "+suffix, "zen-gate", cfg, true))
			}
		}
	}
	hidden := s.hiddenSet()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			id := p.ID + "/" + m
			if hidden[id] {
				continue
			}
			data = append(data, s.modelCard(lane.ModelInfo{ID: m}, id, p.Name, cfg, false))
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// modelCard is one /v1/models entry: the OpenAI fields plus the capability
// half. A capacity nothing states is omitted — an absent number is honest
// where a default would be a claim the gateway cannot support.
//
// floor carries what the free lane's curated table states; the merged verdict
// (实测 > 上游声明 > AI标注 > 规则) overrides it wherever it reached a verdict,
// so a probe result is what a client reads instead of the name guess.
func (s *Server) modelCard(floor lane.ModelInfo, id, ownedBy string, cfg *store.Config, freeLane bool) map[string]any {
	upstream := lane.BaseModelId(floor.ID)
	caps := s.modalityCapsOf(upstream)
	vision, audio, file, reasoning := floor.Vision, floor.AudioInput, floor.FileInput, floor.Reasoning
	contextWindow, maxOutput := floor.ContextWindow, floor.MaxOutput
	source := store.TagSourceUnknown
	if freeLane && lane.CapabilityMatched(upstream) {
		source = store.TagSourceHeuris
	}
	// For a free-lane model the curated table is the baseline, so only a real
	// verdict (实测 / 上游声明 / AI标注) may override it — the name regexes are
	// weaker evidence than the table, and they used to demote a probe-verified
	// 思考 model to plain text.
	if caps.Known && (!freeLane || caps.Source != store.TagSourceHeuris) {
		vision, audio, file, reasoning = caps.Vision, caps.Audio, caps.File, caps.Reasoning
		source = caps.Source
	}
	// A CapsOnly row (public capacity reference) states windows without
	// touching modalities, so it never flips Known — but the badge should
	// still say where the numbers came from.
	if !caps.Known && caps.Source == store.TagSourceListing {
		source = store.TagSourceListing
	}
	if caps.ContextWindow > 0 {
		contextWindow = caps.ContextWindow
	}
	if caps.MaxOutput > 0 {
		maxOutput = caps.MaxOutput
	}
	mods := []string{"text"}
	if vision {
		mods = append(mods, "image")
	}
	if audio {
		mods = append(mods, "audio")
	}
	if file {
		mods = append(mods, "file")
	}
	entry := map[string]any{
		"id":                id,
		"object":            "model",
		"owned_by":          ownedBy,
		"input_modalities":  mods,
		"output_modalities": []string{"text"},
		"reasoning":         reasoning,
		"capability_source": source,
	}
	if contextWindow > 0 {
		entry["context_window"] = contextWindow
	}
	if maxOutput > 0 {
		entry["max_output_tokens"] = maxOutput
	}
	if reasoning && freeLane {
		if levels := lane.EffortsFor(floor, 0, cfg.DefaultMaxTokens, cfg.DefaultEffort); len(levels) > 0 {
			ids := make([]string, 0, len(levels))
			for _, l := range levels {
				ids = append(ids, l.ID)
			}
			entry["supported_reasoning_levels"] = ids
		}
	}
	return entry
}

// hiddenSet snapshots the user-hidden model ids.
func (s *Server) hiddenSet() map[string]bool {
	set := map[string]bool{}
	for _, h := range s.Store.Config().HiddenModels {
		set[h] = true
	}
	return set
}

// isProviderID reports whether a gateway id names a custom-provider model.
// Free-lane ids never contain "/", so any id with one (e.g.
// "nvidia-nim/deepseek-ai/deepseek-v4.1-flash") routes through providerRoute.
func isProviderID(id string) bool { return strings.Contains(id, "/") }

// VisibleModels lists the free-lane models a picker should show: everything
// the lane advertises minus the ids the user unchecked. Routing is unaffected
// — a hidden model requested explicitly still works.
func (s *Server) VisibleModels() []lane.ModelInfo {
	hidden := s.hiddenSet()
	out := []lane.ModelInfo{}
	for _, m := range s.Lane.ServableModels() {
		if !hidden[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// InjectableModels is the model set handed to the agent adapters: the visible
// free-lane models plus every enabled custom-provider model as
// "<providerID>/<model>", minus the ids unchecked on the 模型 page. Without
// the custom half, 自定义 API models never reach the agents' static config and
// their pickers show the free lane only.
//
// Provider entries carry the capacity defaults the Codex catalog schema
// requires (a 0 context window surfaces downstream as "truncate immediately")
// and the merged capability verdicts from the tag pipeline, so a picker's
// input-modalities match what the router actually believes about the model.
func (s *Server) InjectableModels() []lane.ModelInfo {
	out := s.VisibleModels()
	hidden := s.hiddenSet()
	cfg := s.Store.Config()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			id := p.ID + "/" + m
			if hidden[id] {
				continue
			}
			entry := lane.ModelInfo{
				ID:   id,
				Name: id,
				Wire: "chat",
				// Providers do not declare capacities, but the catalog schema
				// demands non-zero ones — 0 reads as "truncate immediately".
				ContextWindow: 131072,
				MaxOutput:     32768,
			}
			if caps := s.modalityCapsOf(m); caps.Known {
				entry.Vision = caps.Vision
				entry.AudioInput = caps.Audio
				entry.FileInput = caps.File
				entry.Reasoning = caps.Reasoning
				if caps.ContextWindow > 0 {
					entry.ContextWindow = caps.ContextWindow
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// handleCodexCatalog serves the model list in Codex's remote-catalog shape so
// the desktop model picker can offer the free models under the zen_gate
// provider. Deliberately unauthenticated: the listing is non-sensitive, and a
// 401 here makes Codex surface a login prompt instead of the models.
func (s *Server) handleCodexCatalog(w http.ResponseWriter, r *http.Request) {
	// InjectableModels (not the raw lane snapshot) so 自定义 API models survive
	// the picker's live refresh instead of being drowned back out.
	models := s.InjectableModels()
	_, av, _ := s.Lane.Snapshot()
	customName := map[string]string{}
	providers := s.Store.Config().Providers
	for i := range providers {
		p := &providers[i]
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			customName[p.ID+"/"+m] = p.Name
		}
	}
	priority := map[string]int{
		lane.StateAvailable: 100,
		lane.StateUnknown:   50,
		lane.StateThrottled: 20,
	}
	stateOf := func(id string) string {
		if p, ok := av[id]; ok {
			return lane.FreshState(p)
		}
		return lane.StateUnknown
	}
	// Provider models sink below every free model — including region-gated
	// ones. The free lane is the product; a provider is the user's own
	// addition, and its entries carry no live state to rank by anyway.
	sort.SliceStable(models, func(i, j int) bool {
		pi, pj := isProviderID(models[i].ID), isProviderID(models[j].ID)
		if pi != pj {
			return pj // free models first
		}
		// Region-gated models sink to the bottom of the free half: with a CN
		// egress they only ever answer with a RegionError.
		if models[i].RegionSensitive != models[j].RegionSensitive {
			return models[j].RegionSensitive
		}
		return priority[stateOf(models[i].ID)] > priority[stateOf(models[j].ID)]
	})
	levels := []map[string]any{
		{"effort": "low", "description": "轻量，最省额度"},
		{"effort": "medium", "description": "均衡"},
		{"effort": "high", "description": "深思"},
	}
	out := []map[string]any{}
	for _, m := range models {
		desc := "免费车道 · 当前不可用"
		if name, ok := customName[m.ID]; ok {
			desc = "自定义 API · " + name
		} else {
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
		}
		lv := levels
		if !m.Reasoning {
			lv = []map[string]any{{"effort": "medium", "description": "默认"}}
		}
		mods := []string{"text"}
		if m.Vision {
			mods = append(mods, "image")
		}
		if m.AudioInput {
			mods = append(mods, "audio")
		}
		if m.FileInput {
			mods = append(mods, "file")
		}
		// Provider entries rank at priority 5: below every free model in the
		// picker's ordering, above nothing that the product owns.
		entryPriority := priority[stateOf(m.ID)]
		if isProviderID(m.ID) {
			entryPriority = 5
		}
		out = append(out, map[string]any{
			"slug":                         m.ID,
			"display_name":                 m.Name,
			"description":                  desc,
			"base_instructions":            fmt.Sprintf("You are %s (model id: %s), a coding agent serving the user's Codex app through the Zen Gate local gateway. Work toward the user's goal with the available tools and verify your changes. Always reply in the same language as the user's latest message — when the user writes Chinese, reply in Simplified Chinese. Be concise.", m.Name, m.ID),
			"default_reasoning_level":      "medium",
			"supported_reasoning_levels":   lv,
			"shell_type":                   "unified_exec",
			"support_verbosity":            false,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"experimental_supported_tools": []string{},
			"input_modalities":             mods,
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     entryPriority,
			"provider_id":                  "zen_gate",
			"context_window":               m.ContextWindow,
			"max_output_tokens":            m.MaxOutput,
		})
	}
	writeJSON(w, 200, map[string]any{"models": out})
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

// stampRequestID ties one logical client request to the log lines and the
// upstream calls made for it. A client-supplied trace id wins (that is the only
// way its logs and ours line up), and the id is echoed on the response before
// any handler can write, so even a rejected request is greppable by id.
// Returns r carrying the id, which the lane and relay reporters read back.
func stampRequestID(w http.ResponseWriter, r *http.Request) *http.Request {
	rid := ""
	for _, h := range []string{"X-Request-Id", "X-Client-Request-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			rid = v
			break
		}
	}
	if rid == "" {
		rid = randomID("zreq-")
	}
	w.Header().Set("x-zen-gate-request-id", rid)
	return r.WithContext(lane.WithTrace(r.Context(), rid))
}

// requestID reads the correlation id stamped at the route; it mints one if a
// handler is reached without stamping (direct calls from tests).
func requestID(r *http.Request) string {
	if rid := lane.TraceFrom(r.Context()); rid != "" {
		return rid
	}
	return randomID("zreq-")
}
