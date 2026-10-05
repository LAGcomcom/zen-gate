package gateway

import (
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	neturl "net/url"
	"zen-gate/internal/agents"
	"zen-gate/internal/lane"
	"zen-gate/internal/notify"
	"zen-gate/internal/store"
	"zen-gate/internal/update"
)

//go:embed web/index.html
var dashboardHTML []byte

// AgentRegistry is the set of installable agent adapters, wired in main.
type AgentRegistry interface {
	Views() []agents.View
	Enable(id string) error
	Disable(id string) error
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if !adminAllowed(w, r, false) {
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, rest string) {
	if !adminAllowed(w, r, r.Method != http.MethodGet) {
		return
	}
	switch {
	case rest == "state" && r.Method == http.MethodGet:
		s.adminState(w)
	case rest == "settings" && r.Method == http.MethodPost:
		s.adminSettings(w, r)
	case rest == "reprobe" && r.Method == http.MethodPost:
		go s.Lane.ProbeRound(r.Context(), true)
		writeJSON(w, 200, map[string]any{"ok": true})
	case rest == "update/apply" && r.Method == http.MethodPost:
		s.adminUpdateApply(w)
	case rest == "key/rotate" && r.Method == http.MethodPost:
		s.Store.Config().MainKey = store.GenerateKey("")
		_ = s.Store.Save()
		writeJSON(w, 200, map[string]any{"ok": true, "key": s.Store.Config().MainKey})
	case rest == "proxy-test" && r.Method == http.MethodPost:
		s.adminProxyTest(w, r)
	case rest == "logs" && r.Method == http.MethodGet:
		s.adminLogs(w, r)
	case rest == "usage.csv" && r.Method == http.MethodGet:
		s.adminUsageCSV(w, r)
	case strings.HasPrefix(rest, "probe/") && r.Method == http.MethodPost:
		s.adminProbeOne(w, strings.TrimPrefix(rest, "probe/"))
	case rest == "autostart" && r.Method == http.MethodPost:
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := agents.AutostartSet(in.Enabled); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if s.logger != nil {
			s.logger.Infof("开机自启 = %v", in.Enabled)
		}
		writeJSON(w, 200, map[string]any{"ok": true, "enabled": in.Enabled})
	case rest == "export" && r.Method == http.MethodGet:
		s.adminExport(w, r)
	case rest == "upstreams" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"ok": true, "upstreams": redactUpstreams(s.upstreams())})
	case rest == "upstreams" && r.Method == http.MethodPost:
		s.adminUpstreamsSet(w, r)
	case strings.HasPrefix(rest, "upstreams/") && r.Method == http.MethodDelete:
		s.adminUpstreamDelete(w, strings.TrimSuffix(strings.TrimPrefix(rest, "upstreams/"), "/"))
	case rest == "import" && r.Method == http.MethodPost:
		s.adminImport(w, r)
	case strings.HasPrefix(rest, "agent/"):
		s.adminAgent(w, r, strings.TrimPrefix(rest, "agent/"))
	default:
		writeJSON(w, 404, map[string]any{"error": "not found"})
	}
}

// adminAllowed enforces the dashboard fence: loopback Host (DNS-rebinding
// guard) + loopback remote; mutating requests additionally require a
// same-origin Origin/Referer when the client supplies one.
func adminAllowed(w http.ResponseWriter, r *http.Request, mutating bool) bool {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host, "]") {
		hname := host[:i]
		if hname != "127.0.0.1" && hname != "localhost" && hname != "[::1]" && hname != "::1" {
			writeJSON(w, 403, map[string]any{"error": "admin is loopback-only"})
			return false
		}
	}
	remote := r.RemoteAddr
	if h, _, err := net.SplitHostPort(remote); err == nil {
		remote = h
	}
	if remote != "127.0.0.1" && remote != "::1" {
		writeJSON(w, 403, map[string]any{"error": "admin is loopback-only"})
		return false
	}
	if mutating {
		for _, h := range []string{"origin", "referer"} {
			v := strings.TrimSpace(r.Header.Get(h))
			if v == "" {
				continue
			}
			if !strings.Contains(v, "://127.0.0.1") && !strings.Contains(v, "://localhost") && !strings.Contains(v, "://[::1]") {
				writeJSON(w, 403, map[string]any{"error": "cross-site admin request rejected"})
				return false
			}
		}
	}
	return true
}

func (s *Server) adminState(w http.ResponseWriter) {
	cfg := s.Store.Config()
	cat, av, egress := s.Lane.Snapshot()
	type modelRow struct {
		ID            string             `json:"id"`
		Name          string             `json:"name"`
		Blurb         string             `json:"blurb,omitempty"`
		State         string             `json:"state"`
		Detail        string             `json:"detail,omitempty"`
		TTFTMs        int64              `json:"ttftMs,omitempty"`
		LatencyMs     int64              `json:"latencyMs,omitempty"`
		Vision        bool               `json:"vision"`
		Reasoning     bool               `json:"reasoning"`
		SystemOne     bool               `json:"systemOne"`
		ContextWindow int                `json:"contextWindow"`
		MaxOutput     int                `json:"maxOutput"`
		Efforts       []lane.LevelBudget `json:"efforts"`
		// Observed-quota fields (no official balance API exists upstream).
		QuotaUsed     int   `json:"quotaUsed,omitempty"`
		QuotaEstimate int   `json:"quotaEstimate,omitempty"`
		ThrottledAt   int64 `json:"throttledAt,omitempty"`
		RecoverEta    int64 `json:"recoverEta,omitempty"`
		// Probe first-token history (persisted, survives restarts).
		TTFTAvgMs int64 `json:"ttftAvgMs,omitempty"`
		TTFTCount int   `json:"ttftCount,omitempty"`
		// Custom marks a user-configured upstream's model: shown in the list,
		// but not probed, so it carries no quota or latency fields.
		Custom bool `json:"custom,omitempty"`
	}
	days, recent := s.Store.SnapshotStats()
	notes := s.Lane.ThrottleNotes()
	models := []modelRow{}
	for _, m := range cat {
		p := av[m.ID]
		note := notes[m.ID]
		row := modelRow{
			ID: m.ID, Name: m.Name, Blurb: m.Blurb, State: stateOrDefault(p), Detail: p.Detail,
			TTFTMs: p.TTFTMs, LatencyMs: p.LatencyMs,
			Vision: m.Vision, Reasoning: m.Reasoning,
			SystemOne:     m.SystemOne,
			ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput,
			Efforts: lane.EffortsFor(m, 0, cfg.DefaultMaxTokens),
		}
		row.TTFTAvgMs, row.TTFTCount = s.Store.TTFTStats(m.ID)
		// Right after a boot the fresh probe has not run yet — fall back to
		// the persisted average so the picker shows yesterday's latency.
		if row.TTFTMs == 0 && row.TTFTAvgMs > 0 {
			row.TTFTMs = row.TTFTAvgMs
		}
		if !m.SystemOne {
			row.QuotaUsed = todayTokens(days, m.ID)
			row.QuotaEstimate = estimateDailyQuota(days, m.ID, note.Episodes)
			if note.ThrottledAt != 0 {
				row.ThrottledAt = note.ThrottledAt
				row.RecoverEta = s.Lane.RecoveryETA(m.ID)
			}
		}
		models = append(models, row)
	}
	// Custom upstream models appear here too, marked so the dashboard can show
	// them without pretending they carry the free lane's live probe state.
	for _, um := range s.upstreamModels() {
		f := um.info
		models = append(models, modelRow{
			ID: f.ID, Name: f.Name, Blurb: f.Blurb,
			// A local endpoint is reachable whenever the user's machine is up —
			// there is nothing to probe for, so "unknown" is the honest state.
			State:         lane.StateUnknown,
			Detail:        "自定义上游 " + um.upstreamID + " · 不参与探测",
			Vision:        f.Vision,
			Reasoning:     f.Reasoning,
			ContextWindow: f.ContextWindow,
			MaxOutput:     f.MaxOutput,
			Efforts:       lane.EffortsFor(lane.ModelInfo{ID: f.ID, Reasoning: f.Reasoning}, 0, cfg.DefaultMaxTokens),
			Custom:        true,
		})
	}
	agentsView := []agents.View{}
	if s.registry != nil {
		agentsView = s.registry.Views()
	}
	writeJSON(w, 200, map[string]any{
		"baseURL":   s.BaseURL(),
		"port":      cfg.Port,
		"mainKey":   cfg.MainKey,
		"agentKeys": cfg.AgentKeys,
		"egress":    egress,
		"models":    models,
		"agents":    agentsView,
		"upstreams": redactUpstreams(s.upstreams()),
		"stats":     map[string]any{"days": days, "recent": recent},
		"settings": map[string]any{
			"defaultMaxTokens":     cfg.DefaultMaxTokens,
			"defaultEffort":        cfg.DefaultEffort,
			"probeIntervalMinutes": cfg.ProbeIntervalMinutes,
			"exposeRegion":         cfg.ExposeRegion,
			"closeToTray":          cfg.CloseToTray,
			"notifications":        cfg.Notifications,
			"updateFeed":           cfg.UpdateFeed,
			"statsServerUrl":       cfg.StatsServerURL,
			"updateAvailable":      s.updateAvailable,
			"updateVersion":        s.updateVersion,
			"updateURL":            s.updateURL,
			"proxyMode":            cfg.ProxyMode,
			"proxyURL":             cfg.ProxyURL,
			"autostart":            s.autostartState(),
			"failoverEnabled":      cfg.FailoverEnabled,
			"failoverMax":          cfg.FailoverMax,
		},
		"probingModel": s.Lane.ProbingModel(),
		"version":      Version,
		"startedAt":    startedAt.Format("2006-01-02 15:04:05"),
		"uptime":       time.Since(startedAt).Round(time.Second).String(),
		"uptimeSec":    int64(time.Since(startedAt).Seconds()),
		"dataDir":      s.Store.Home,
		"now":          time.Now().Format("2006-01-02 15:04:05"),
	})
}

func stateOrDefault(p lane.ProbeResult) string {
	if p.State == "" {
		return lane.StateUnknown
	}
	return p.State
}

// estimateDailyQuota approximates a model's daily free-quota ceiling from
// history: for each day where a throttle episode opened, the model's total
// tokens that day approximates "the amount that tripped the limit". The
// median over those days is the estimate; fewer than two samples → 0
// (the UI shows 统计中 rather than inventing a number).
func estimateDailyQuota(days map[string]*store.DayStat, model string, episodes []lane.ThrottleEpisode) int {
	samples := []int{}
	for _, e := range episodes {
		if e.End == 0 {
			continue
		}
		d := days[time.UnixMilli(e.Start).Format("2006-01-02")]
		if d == nil {
			continue
		}
		if tok := d.Models[model]; tok > 0 {
			samples = append(samples, tok)
		}
	}
	if len(samples) < 2 {
		return 0
	}
	sort.Ints(samples)
	return samples[len(samples)/2]
}

// todayTokens is one model's output tokens so far today.
func todayTokens(days map[string]*store.DayStat, model string) int {
	if d := days[time.Now().Format("2006-01-02")]; d != nil {
		return d.Models[model]
	}
	return 0
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Port                 *int    `json:"port"`
		DefaultMaxTokens     *int    `json:"defaultMaxTokens"`
		DefaultEffort        *string `json:"defaultEffort"`
		ProbeIntervalMinutes *int    `json:"probeIntervalMinutes"`
		ExposeRegion         *bool   `json:"exposeRegion"`
		CloseToTray          *bool   `json:"closeToTray"`
		Notifications        *bool   `json:"notifications"`
		ProxyMode            *string `json:"proxyMode"`
		ProxyURL             *string `json:"proxyUrl"`
		UpdateFeed           *string `json:"updateFeed"`
		StatsServerURL       *string `json:"statsServerUrl"`
		FailoverEnabled      *bool   `json:"failoverEnabled"`
		FailoverMax          *int    `json:"failoverMax"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	cfg := s.Store.Config()
	changed := false
	if in.Port != nil && *in.Port > 0 && *in.Port < 65536 && *in.Port != cfg.Port {
		cfg.Port = *in.Port
		changed = true
	}
	if in.DefaultMaxTokens != nil && *in.DefaultMaxTokens > 0 {
		cfg.DefaultMaxTokens = *in.DefaultMaxTokens
		s.Lane.SetDefaultMaxTokens(*in.DefaultMaxTokens)
		changed = true
	}
	if in.DefaultEffort != nil {
		switch *in.DefaultEffort {
		case "light", "balanced", "deep":
			cfg.DefaultEffort = *in.DefaultEffort
			changed = true
		}
	}
	if in.ProbeIntervalMinutes != nil && *in.ProbeIntervalMinutes > 0 {
		cfg.ProbeIntervalMinutes = *in.ProbeIntervalMinutes
		changed = true
	}
	if in.CloseToTray != nil {
		cfg.CloseToTray = *in.CloseToTray
		changed = true
	}
	if in.Notifications != nil {
		cfg.Notifications = *in.Notifications
		notify.SetEnabled(*in.Notifications)
		changed = true
	}
	if in.ProxyMode != nil {
		switch *in.ProxyMode {
		case "env", "direct", "custom", "system":
			if *in.ProxyMode == "custom" && (in.ProxyURL == nil || strings.TrimSpace(*in.ProxyURL) == "") && strings.TrimSpace(cfg.ProxyURL) == "" {
				writeJSON(w, 400, map[string]any{"ok": false, "error": "自定义代理需要填写代理地址"})
				return
			}
			if cfg.ProxyMode != *in.ProxyMode || (in.ProxyURL != nil && cfg.ProxyURL != *in.ProxyURL) {
				cfg.ProxyMode = *in.ProxyMode
				if in.ProxyURL != nil {
					cfg.ProxyURL = strings.TrimSpace(*in.ProxyURL)
				}
				lane.SetProxy(cfg.ProxyMode, cfg.ProxyURL)
				changed = true
			}
		}
	}
	if in.ProxyURL != nil && in.ProxyMode == nil {
		cfg.ProxyURL = strings.TrimSpace(*in.ProxyURL)
		lane.SetProxy(cfg.ProxyMode, cfg.ProxyURL)
		changed = true
	}
	if in.UpdateFeed != nil {
		cfg.UpdateFeed = strings.TrimSpace(*in.UpdateFeed)
		changed = true
	}
	if in.StatsServerURL != nil {
		cfg.StatsServerURL = strings.TrimRight(strings.TrimSpace(*in.StatsServerURL), "/")
		changed = true
	}
	if in.ExposeRegion != nil {
		cfg.ExposeRegion = *in.ExposeRegion
		s.Lane.SetExposeRegion(*in.ExposeRegion)
		changed = true
	}
	if in.FailoverEnabled != nil {
		cfg.FailoverEnabled = *in.FailoverEnabled
		s.Lane.SetFailover(cfg.FailoverEnabled, cfg.FailoverMax)
		changed = true
	}
	if in.FailoverMax != nil && *in.FailoverMax > 0 && *in.FailoverMax <= 5 {
		cfg.FailoverMax = *in.FailoverMax
		s.Lane.SetFailover(cfg.FailoverEnabled, cfg.FailoverMax)
		changed = true
	}
	if changed {
		_ = s.Store.Save()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed})
}

// adminProxyTest tests a proxy (or the saved one) against the upstream and
// reports reachability, latency and the exit it would present.
func (s *Server) adminProxyTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
		URL  string `json:"url"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	mode := in.Mode
	if mode == "" {
		mode = s.Store.Config().ProxyMode
	}
	proxyURL := in.URL
	if proxyURL == "" {
		proxyURL = s.Store.Config().ProxyURL
	}
	transport := &http.Transport{}
	switch mode {
	case "direct":
		transport.Proxy = nil
	case "custom":
		u, err := neturl.Parse(strings.TrimSpace(proxyURL))
		if err != nil || u.Host == "" {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "代理地址不合法"})
			return
		}
		fixed := u
		transport.Proxy = func(*http.Request) (*neturl.URL, error) { return fixed, nil }
	default:
		transport.Proxy = http.ProxyFromEnvironment
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

	t0 := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		lane.UpstreamBase+"/zen/v1/models", nil)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for k, v := range lane.GatewayHeaders(lane.SessionForConversation("proxy-test"),
		lane.MintRequestId(0), false, "application/json") {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "latencyMs": latency,
			"error": "经此代理无法访问上游: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	eg := lane.DetectEgressVia(r.Context(), client)
	writeJSON(w, 200, map[string]any{
		"ok":            resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":        resp.StatusCode,
		"latencyMs":     latency,
		"egressIP":      eg.IP,
		"egressCountry": eg.Country,
	})
}

// adminLogs tails the in-memory ring with an optional level filter.
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	level := strings.ToLower(r.URL.Query().Get("level"))
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	if s.logger != nil {
		writeJSON(w, 200, map[string]any{"entries": s.logger.Tail(level, limit)})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": []any{}})
}

// adminUsageCSV exports the per-day stats table (with per-model columns).
func (s *Server) adminUsageCSV(w http.ResponseWriter, r *http.Request) {
	days, _ := s.Store.SnapshotStats()
	w.Header().Set("content-type", "text/csv; charset=utf-8")
	w.Header().Set("content-disposition", `attachment; filename="zen-gate-usage.csv"`)
	models := map[string]bool{}
	for _, d := range days {
		for m := range d.Models {
			models[m] = true
		}
	}
	modelList := make([]string, 0, len(models))
	for m := range models {
		modelList = append(modelList, m)
	}
	sort.Strings(modelList)
	cw := csv.NewWriter(w)
	_ = cw.Write(append([]string{"date", "requests", "failed", "input", "output"}, modelList...))
	dayKeys := make([]string, 0, len(days))
	for k := range days {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, day := range dayKeys {
		d := days[day]
		row := []string{day, strconv.Itoa(d.Requests), strconv.Itoa(d.Failed),
			strconv.Itoa(d.Input), strconv.Itoa(d.Output)}
		for _, m := range modelList {
			row = append(row, strconv.Itoa(d.Models[m]))
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

// adminExport downloads config.json.
func (s *Server) adminExport(w http.ResponseWriter, r *http.Request) {
	data, err := json.MarshalIndent(s.Store.Config(), "", "  ")
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-disposition", `attachment; filename="zen-gate-config.json"`)
	_, _ = w.Write(data)
}

// adminImport applies an uploaded config (validated; port stays bound).
func (s *Server) adminImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var incoming store.Config
	if err := json.Unmarshal(body, &incoming); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "不是合法的 zen-gate 配置文件: " + err.Error()})
		return
	}
	if incoming.MainKey == "" || incoming.Port <= 0 || incoming.Port >= 65536 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "配置缺少 MainKey 或端口不合法"})
		return
	}
	port := s.Store.Config().Port
	incoming.Port = port
	if incoming.AgentKeys == nil {
		incoming.AgentKeys = map[string]string{}
	}
	if incoming.EnabledAgents == nil {
		incoming.EnabledAgents = map[string]bool{}
	}
	if incoming.DefaultMaxTokens <= 0 {
		incoming.DefaultMaxTokens = 32768
	}
	if incoming.DefaultEffort != "light" && incoming.DefaultEffort != "deep" {
		incoming.DefaultEffort = "balanced"
	}
	if incoming.ProbeIntervalMinutes <= 0 {
		incoming.ProbeIntervalMinutes = 15
	}
	if incoming.ProxyMode == "" {
		incoming.ProxyMode = "env"
	}
	*s.Store.Config() = incoming
	_ = s.Store.Save()
	if s.logger != nil {
		s.logger.Infof("config imported; %d agent keys", len(incoming.AgentKeys))
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminProbeOne re-tests a single model on demand and returns its fresh
// verdict — the backend of the per-card 测试 button. Synchronous: the fetch
// resolves when the probe verdict is in.
func (s *Server) adminProbeOne(w http.ResponseWriter, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "missing model"})
		return
	}
	r := s.Lane.ProbeOne(model)
	if s.logger != nil {
		s.logger.Infof("手动探测 %s → %s (首字 %dms)", model, r.State, r.TTFTMs)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": r})
}

// adminUpdateApply downloads the pending release asset and swaps the running
// exe; the process relaunches itself and the dashboard reconnects to the new
// instance. Gate: an update must have been detected first.
func (s *Server) adminUpdateApply(w http.ResponseWriter) {
	if !s.updateAvailable || s.updateURL == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "没有检测到可用更新"})
		return
	}
	if _, err := update.Apply(s.updateURL, lane.Client()); err != nil {
		if s.logger != nil {
			s.logger.Errorf("一键更新失败: %v", err)
		}
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if s.logger != nil {
		s.logger.Infof("一键更新完成 (v%s → %s)，重启中…", Version, s.updateVersion)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "restarting": true})
	go func() {
		time.Sleep(800 * time.Millisecond) // let the response reach the dashboard
		if err := update.Relaunch(); err != nil {
			if s.logger != nil {
				s.logger.Errorf("重启新版本失败: %v", err)
			}
			return
		}
		os.Exit(0)
	}()
}

func (s *Server) adminAgent(w http.ResponseWriter, r *http.Request, rest string) {
	if s.registry == nil {
		writeJSON(w, 503, map[string]any{"error": "agent registry unavailable"})
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	id, action := parts[0], parts[1]
	var err error
	switch action {
	case "enable":
		err = s.registry.Enable(id)
	case "disable":
		err = s.registry.Disable(id)
	default:
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
