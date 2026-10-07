package gateway

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
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
	"zen-gate/internal/announce"
	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/notify"
	"zen-gate/internal/relay"
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
	ResyncEnabled() int
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
	case rest == "update/check" && r.Method == http.MethodPost:
		if s.updateCheck == nil {
			writeJSON(w, 503, map[string]any{"ok": false, "error": "更新检查未就绪"})
			return
		}
		has, ver := s.updateCheck()
		writeJSON(w, 200, map[string]any{"ok": true, "has": has, "version": ver})
	case rest == "key/rotate" && r.Method == http.MethodPost:
		s.Store.Config().MainKey = store.GenerateKey("")
		_ = s.Store.Save()
		writeJSON(w, 200, map[string]any{"ok": true, "key": s.Store.Config().MainKey})
	case rest == "proxy-test" && r.Method == http.MethodPost:
		s.adminProxyTest(w, r)
	case rest == "logs" && r.Method == http.MethodGet:
		s.adminLogs(w, r)
	case rest == "logs/download" && r.Method == http.MethodGet:
		s.adminLogsDownload(w, r)
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
	case rest == "import" && r.Method == http.MethodPost:
		s.adminImport(w, r)
	case strings.HasPrefix(rest, "agent/"):
		s.adminAgent(w, r, strings.TrimPrefix(rest, "agent/"))
	case rest == "providers" && r.Method == http.MethodPost:
		s.adminProviderSave(w, r)
	case rest == "providers/delete" && r.Method == http.MethodPost:
		s.adminProviderDelete(w, r)
	case rest == "providers/models" && r.Method == http.MethodPost:
		s.adminProviderModels(w, r)
	case rest == "models/visibility" && r.Method == http.MethodPost:
		s.adminModelVisibility(w, r)
	case rest == "tags/retag" && r.Method == http.MethodPost:
		s.adminRetag(w)
	case rest == "tags/probe" && r.Method == http.MethodPost:
		s.adminCapabilityProbe(w, r)
	case rest == "announcements/read" && r.Method == http.MethodPost:
		s.adminAnnouncementRead(w, r)
	case rest == "announcements/refresh" && r.Method == http.MethodPost:
		if s.announcementPull == nil {
			writeJSON(w, 503, map[string]any{"ok": false, "error": "公告拉取未就绪"})
			return
		}
		go s.announcementPull()
		writeJSON(w, 200, map[string]any{"ok": true})
	case rest == "subs/save" && r.Method == http.MethodPost:
		s.adminSubsSave(w, r)
	case rest == "subs/refresh" && r.Method == http.MethodPost:
		s.adminSubsRefresh(w, r)
	case rest == "subs/probe" && r.Method == http.MethodPost:
		if s.subs == nil {
			writeJSON(w, 503, map[string]any{"ok": false, "error": "订阅轮询未就绪"})
			return
		}
		go s.subs.ProbeAll(r.Context())
		writeJSON(w, 200, map[string]any{"ok": true})
	case rest == "subs/singbox" && r.Method == http.MethodPost:
		s.adminSubsSingBox(w, r)
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
		AudioInput    bool               `json:"audioInput,omitempty"`
		FileInput     bool               `json:"fileInput,omitempty"`
		TagSource     string             `json:"tagSource,omitempty"`
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
		// Custom marks a user-added provider's model (no lane probe, no quota).
		Custom     bool   `json:"custom,omitempty"`
		ProviderID string `json:"providerId,omitempty"`
		// Hidden marks a model the user unchecked: servable but not advertised
		// to agent pickers.
		Hidden bool `json:"hidden,omitempty"`
	}
	days, recent := s.Store.SnapshotStats()
	notes := s.Lane.ThrottleNotes()
	hidden := s.hiddenSet()
	models := []modelRow{}
	for _, m := range cat {
		p := av[m.ID]
		note := notes[m.ID]
		row := modelRow{
			ID: m.ID, Name: m.Name, Blurb: m.Blurb, State: stateOrDefault(p), Detail: p.Detail,
			TTFTMs: p.TTFTMs, LatencyMs: p.LatencyMs,
			Vision: m.Vision, AudioInput: m.AudioInput, FileInput: m.FileInput,
			Reasoning:     m.Reasoning,
			SystemOne:     m.SystemOne,
			ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput,
			Efforts: lane.EffortsFor(m, 0, cfg.DefaultMaxTokens),
			Hidden:  hidden[m.ID],
		}
		if tag, ok := s.Store.ModelTagOf(m.ID); ok {
			row.TagSource = tag.Source
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
	// Custom-provider models follow the free-lane ones so the 模型 page shows
	// everything the gateway serves in one place. Their state starts at
	// "custom" and becomes a real verdict once the user probes them.
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled {
			continue
		}
		for _, mid := range p.Models {
			id := p.ID + "/" + mid
			row := modelRow{
				ID: id, Name: mid, State: "custom", Custom: true,
				ProviderID: p.ID,
				Blurb:      "自定义供应商「" + p.Name + "」",
				Hidden:     hidden[id],
			}
			if caps := s.modalityCapsOf(mid); caps.Known {
				row.Vision, row.AudioInput, row.FileInput = caps.Vision, caps.Audio, caps.File
				row.TagSource = caps.Source
				if caps.ContextWindow > 0 {
					row.ContextWindow = caps.ContextWindow
				}
			}
			if pr, ok := s.customProbeOf(id); ok {
				row.State = pr.State
				row.Detail = pr.Detail
				row.TTFTMs = pr.TTFTMs
			}
			row.TTFTAvgMs, row.TTFTCount = s.Store.TTFTStats(id)
			if row.TTFTMs == 0 && row.TTFTAvgMs > 0 {
				row.TTFTMs = row.TTFTAvgMs
			}
			models = append(models, row)
		}
	}
	agentsView := []agents.View{}
	if s.registry != nil {
		agentsView = s.registry.Views()
	}
	activeAnnouncements, endedAnnouncements := s.announcementViews()
	settings := map[string]any{
		"defaultMaxTokens":     cfg.DefaultMaxTokens,
		"defaultEffort":        cfg.DefaultEffort,
		"probeIntervalMinutes": cfg.ProbeIntervalMinutes,
		"exposeRegion":         cfg.ExposeRegion,
		"closeToTray":          cfg.CloseToTray,
		"notifications":        cfg.Notifications,
		"updateFeed":           cfg.UpdateFeed,
		"announcementFeed":     cfg.AnnouncementFeed,
		"statsServerUrl":       cfg.StatsServerURL,
		"updateAvailable":      s.updateAvailable,
		"updateVersion":        s.updateVersion,
		"updateURL":            s.updateURL,
		// Bundled builds cannot swap their own binary; the dashboard hides
		// 一键更新 and just links to the release page.
		"updateSelfUpdate":        update.SelfUpdateSupported,
		"proxyMode":               cfg.ProxyMode,
		"proxyURL":                cfg.ProxyURL,
		"autostart":               s.autostartState(),
		"failoverEnabled":         cfg.FailoverEnabled,
		"failoverMax":             cfg.FailoverMax,
		"smartRouting":            cfg.SmartRouting,
		"routingStrategy":         cfg.RoutingStrategy,
		"laneFallbackToProviders": cfg.LaneFallbackToProviders,
		"autoTagEnabled":          cfg.AutoTagEnabled,
	}
	for k, v := range logSettingsView(cfg) {
		settings[k] = v
	}
	writeJSON(w, 200, map[string]any{
		"baseURL":             s.BaseURL(),
		"port":                cfg.Port,
		"mainKey":             cfg.MainKey,
		"agentKeys":           cfg.AgentKeys,
		"egress":              egress,
		"models":              models,
		"agents":              agentsView,
		"stats":               map[string]any{"days": days, "recent": recent},
		"settings":            settings,
		"probingModel":        s.currentProbingModel(),
		"providers":           s.providerViews(),
		"providerPresets":     relay.Presets,
		"subscriptions":       s.subsState(),
		"announcements":       activeAnnouncements,
		"announcementArchive": endedAnnouncements,
		"version":             Version,
		"startedAt":           startedAt.Format("2006-01-02 15:04:05"),
		"uptime":              time.Since(startedAt).Round(time.Second).String(),
		"uptimeSec":           int64(time.Since(startedAt).Seconds()),
		"dataDir":             s.Store.Home,
		"now":                 time.Now().Format("2006-01-02 15:04:05"),
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
		Port                    *int             `json:"port"`
		DefaultMaxTokens        *int             `json:"defaultMaxTokens"`
		DefaultEffort           *string          `json:"defaultEffort"`
		ProbeIntervalMinutes    *int             `json:"probeIntervalMinutes"`
		ExposeRegion            *bool            `json:"exposeRegion"`
		CloseToTray             *bool            `json:"closeToTray"`
		Notifications           *bool            `json:"notifications"`
		ProxyMode               *string          `json:"proxyMode"`
		ProxyURL                *string          `json:"proxyUrl"`
		UpdateFeed              *string          `json:"updateFeed"`
		AnnouncementFeed        *string          `json:"announcementFeed"`
		StatsServerURL          *string          `json:"statsServerUrl"`
		FailoverEnabled         *bool            `json:"failoverEnabled"`
		FailoverMax             *int             `json:"failoverMax"`
		SmartRouting            *bool            `json:"smartRouting"`
		RoutingStrategy         *string          `json:"routingStrategy"`
		LaneFallbackToProviders *bool            `json:"laneFallbackToProviders"`
		AutoTagEnabled          *bool            `json:"autoTagEnabled"`
		LogCategories           *map[string]bool `json:"logCategories"`
		LogLevel                *string          `json:"logLevel"`
		LogKeepDays             *int             `json:"logKeepDays"`
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
	if in.AnnouncementFeed != nil {
		cfg.AnnouncementFeed = strings.TrimSpace(*in.AnnouncementFeed)
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
	if in.SmartRouting != nil {
		cfg.SmartRouting = *in.SmartRouting
		s.Lane.SetSmartRouting(cfg.SmartRouting)
		changed = true
	}
	if in.RoutingStrategy != nil {
		cfg.RoutingStrategy = lane.NormalizeStrategy(*in.RoutingStrategy)
		s.Lane.SetStrategy(cfg.RoutingStrategy)
		changed = true
	}
	if in.LaneFallbackToProviders != nil {
		cfg.LaneFallbackToProviders = *in.LaneFallbackToProviders
		changed = true
	}
	if in.AutoTagEnabled != nil {
		cfg.AutoTagEnabled = *in.AutoTagEnabled
		if s.tagger != nil {
			s.tagger.SetEnabled(cfg.AutoTagEnabled)
		}
		changed = true
	}
	logChanged := false
	if in.LogCategories != nil {
		next := make(map[string]bool, len(*in.LogCategories))
		for k, v := range *in.LogCategories {
			next[strings.ToLower(strings.TrimSpace(k))] = v
		}
		cfg.LogCategories = next
		changed = true
		logChanged = true
	}
	if in.LogLevel != nil {
		switch *in.LogLevel {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = *in.LogLevel
			changed = true
			logChanged = true
		}
	}
	if in.LogKeepDays != nil && *in.LogKeepDays >= 1 && *in.LogKeepDays <= 365 {
		cfg.LogKeepDays = *in.LogKeepDays
		changed = true
		logChanged = true
	}
	if logChanged {
		s.ApplyLogSettings(cfg)
	}
	if changed {
		_ = s.Store.Save()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed})
}

// adminModelVisibility updates which models appear in agent pickers and
// /v1/models. Accepts one id or a batch. A short debounce later, every enabled
// agent's config is re-injected with the new list.
func (s *Server) adminModelVisibility(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string   `json:"id"`
		IDs    []string `json:"ids"`
		Hidden bool     `json:"hidden"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ids := in.IDs
	if in.ID != "" {
		ids = append(ids, in.ID)
	}
	if len(ids) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "missing id"})
		return
	}
	cfg := s.Store.Config()
	set := map[string]bool{}
	for _, h := range cfg.HiddenModels {
		set[h] = true
	}
	changed := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if in.Hidden && !set[id] {
			set[id] = true
			changed++
		}
		if !in.Hidden && set[id] {
			delete(set, id)
			changed++
		}
	}
	if changed > 0 {
		out := make([]string, 0, len(set))
		for _, h := range cfg.HiddenModels {
			if set[h] {
				out = append(out, h)
				delete(set, h)
			}
		}
		for h := range set {
			out = append(out, h)
		}
		cfg.HiddenModels = out
		if err := s.Store.Save(); err != nil {
			writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if s.logger != nil {
			action := "恢复显示"
			if in.Hidden {
				action = "隐藏"
			}
			s.logger.Infof("模型可见性: %s %v（当前隐藏 %d 个）", action, ids, len(out))
		}
		if s.registry != nil {
			time.AfterFunc(1500*time.Millisecond, func() {
				if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
					s.logger.Infof("可见性变化，已重新注入 %d 个已开启 Agent 的配置", n)
				}
			})
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed})
}

// adminAnnouncementRead marks one announcement 已读 so the dashboard stops
// highlighting it. The seen-id ring is capped to keep config.json tidy.
func (s *Server) adminAnnouncementRead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "missing id"})
		return
	}
	cfg := s.Store.Config()
	for _, seen := range cfg.SeenAnnouncements {
		if seen == in.ID {
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	cfg.SeenAnnouncements = append(cfg.SeenAnnouncements, in.ID)
	if n := len(cfg.SeenAnnouncements); n > 100 {
		cfg.SeenAnnouncements = cfg.SeenAnnouncements[n-100:]
	}
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// announcementViews is the admin-facing snapshot: active items (with an 已读
// flag computed against the seen ring) plus a capped archive of ended ones so
// the 公告 card on the dash always has something pinned.
func (s *Server) announcementViews() (active, archive []map[string]any) {
	none := func() []map[string]any { return []map[string]any{} }
	v, ok := s.announcements.Load().([]announce.Item)
	if !ok || len(v) == 0 {
		return none(), none()
	}
	seen := map[string]bool{}
	for _, id := range s.Store.Config().SeenAnnouncements {
		seen[id] = true
	}
	view := func(it announce.Item) map[string]any {
		return map[string]any{
			"id": it.ID, "title": it.Title, "body": it.Body, "level": it.Level,
			"start": it.Start, "end": it.End, "url": it.URL,
			"unread": !seen[it.ID],
		}
	}
	now := time.Now()
	for _, it := range announce.Active(v, now) {
		active = append(active, view(it))
	}
	archiveItems := announce.EndedItems(v, now)
	if len(archiveItems) > 5 {
		archiveItems = archiveItems[:5]
	}
	for _, it := range archiveItems {
		archive = append(archive, view(it))
	}
	if active == nil {
		active = none()
	}
	if archive == nil {
		archive = none()
	}
	return active, archive
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

// --- 订阅轮询 (sing-box sidecar) ---------------------------------------------

// adminSubsSave replaces the subscription list and the feature switch in one
// call; enabling runs a full refresh cycle (fetch → parse → sidecar restart).
func (s *Server) adminSubsSave(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		writeJSON(w, 503, map[string]any{"ok": false, "error": "订阅轮询未就绪"})
		return
	}
	var in struct {
		Enabled bool                 `json:"enabled"`
		Items   []store.Subscription `json:"items"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	clean := []store.Subscription{}
	for _, it := range in.Items {
		it.URL = strings.TrimSpace(it.URL)
		if it.URL == "" {
			continue
		}
		if !strings.Contains(it.URL, "://") {
			it.URL = "https://" + it.URL
		}
		if it.ID == "" {
			it.ID = "sub-" + store.GenerateKey("sub")[4:12]
		}
		clean = append(clean, it)
	}
	cfg := s.Store.Config()
	cfg.SubsEnabled = in.Enabled
	cfg.Subscriptions = clean
	_ = s.Store.Save()
	if s.logger != nil {
		s.logger.Infof("订阅轮询 = %v (%d 个订阅)", in.Enabled, len(clean))
	}
	if !in.Enabled {
		s.subs.Stop()
		if cfg.ProxyMode == "rotate" {
			cfg.ProxyMode = "env"
			_ = s.Store.Save()
			lane.SetProxy("env", "")
			lane.SetRotator(nil)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.subs.Apply(ctx); err != nil {
			if s.logger != nil {
				s.logger.Errorf("订阅刷新失败: %v", err)
			}
			return
		}
		lane.SetProxy("rotate", "")
	}()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminSubsRefresh re-runs the fetch/parse/restart/probe cycle on demand.
func (s *Server) adminSubsRefresh(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		writeJSON(w, 503, map[string]any{"ok": false, "error": "订阅轮询未就绪"})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.subs.Refresh(ctx); err != nil {
			if s.logger != nil {
				s.logger.Errorf("订阅刷新失败: %v", err)
			}
		}
	}()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminSubsSingBox sets the user-provided sing-box path or triggers the
// auto-download of the official release.
func (s *Server) adminSubsSingBox(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		writeJSON(w, 503, map[string]any{"ok": false, "error": "订阅轮询未就绪"})
		return
	}
	var in struct {
		Path     string `json:"path,omitempty"`
		Download bool   `json:"download,omitempty"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if in.Download {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if _, err := s.subs.Download(ctx); err != nil && s.logger != nil {
				s.logger.Errorf("下载 sing-box 失败: %v", err)
			}
		}()
		writeJSON(w, 200, map[string]any{"ok": true, "started": true})
		return
	}
	if err := s.subs.SetPath(in.Path); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// subsState builds the 订阅轮询 view for the dashboard: config, sidecar health
// and per-node live state in one payload.
func (s *Server) subsState() map[string]any {
	cfg := s.Store.Config()
	items := []any{}
	for _, sub := range cfg.Subscriptions {
		items = append(items, map[string]any{
			"id":      sub.ID,
			"name":    sub.Name,
			"url":     sub.URL,
			"enabled": sub.Enabled,
		})
	}
	singbox := map[string]any{"running": false, "nodes": []any{}, "alive": 0}
	if s.subs != nil {
		singbox = s.subs.Status()
	}
	singbox["configured"] = cfg.SubsEnabled
	singbox["path"] = cfg.SingBoxPath
	return map[string]any{
		"enabled": cfg.SubsEnabled,
		"items":   items,
		"singbox": singbox,
	}
}

// logQueryOf reads the viewer's filters: class(es), a day (empty = the live
// ring), a keyword, a minimum level and a line budget.
func logQueryOf(r *http.Request) logx.Query {
	q := logx.Query{
		MinLevel: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level"))),
		Contains: r.URL.Query().Get("q"),
		Limit:    300,
	}
	// Only a real date reaches the filename and the file reader below.
	if day := strings.TrimSpace(r.URL.Query().Get("day")); isLogDay(day) {
		q.Day = day
	}
	if csv := strings.TrimSpace(r.URL.Query().Get("cat")); csv != "" {
		for _, part := range strings.Split(csv, ",") {
			if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
				q.Cats = append(q.Cats, part)
			}
		}
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 2000 {
		q.Limit = n
	}
	return q
}

// isLogDay accepts only the daily-file shape, so a day= value can never be
// used to name a file outside the log directory or forge a download name.
func isLogDay(day string) bool {
	if len(day) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", day)
	return err == nil
}

// adminLogs lists log lines: the live ring, or one day's file when ?day= is set.
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	if s.logger == nil {
		writeJSON(w, 200, map[string]any{"entries": []any{}, "days": []string{}})
		return
	}
	q := logQueryOf(r)
	writeJSON(w, 200, map[string]any{"entries": s.logger.Query(q), "days": s.logger.Days()})
}

// adminLogsDownload is the same filters as plain text — the shape you attach to
// a bug report instead of a screenshot.
func (s *Server) adminLogsDownload(w http.ResponseWriter, r *http.Request) {
	q := logQueryOf(r)
	q.Limit = 0
	name := "zen-gate-log"
	if q.Day != "" {
		name += "-" + q.Day
	} else {
		name += "-live"
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("content-disposition", `attachment; filename="`+name+`.txt"`)
	if s.logger == nil {
		return
	}
	for _, e := range s.logger.Query(q) {
		if e.Cat == "" {
			fmt.Fprintf(w, "[%s] [%s] %s\n", e.At.Format("2006-01-02 15:04:05.000"), e.Level, e.Msg)
			continue
		}
		fmt.Fprintf(w, "[%s] [%s] {%s} %s\n", e.At.Format("2006-01-02 15:04:05.000"), e.Level, e.Cat, e.Msg)
	}
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

// adminRetag queues every provider model for a fresh AI-tagging round (the
// 标注 button). Probe verdicts are skipped; lane models are not queued —
// their ids are private and the curated table plus live probes own them.
func (s *Server) adminRetag(w http.ResponseWriter) {
	if s.tagger == nil {
		writeJSON(w, 503, map[string]any{"ok": false, "error": "AI 标注器未就绪"})
		return
	}
	ids := []string{}
	for _, p := range s.Store.Config().Providers {
		ids = append(ids, p.Models...)
	}
	s.tagger.Enqueue(ids...)
	s.logCat(logx.CatAdmin, "info", "已排队 %d 个模型等待 AI 标注", len(ids))
	writeJSON(w, 200, map[string]any{"ok": true, "queued": len(ids)})
}

// adminCapabilityProbe runs the live 实测 probe (image/audio/PDF) for one
// custom-provider model in the background; verdicts land in tags.json and
// surface on the next state refresh.
func (s *Server) adminCapabilityProbe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	p, upstream, ok := s.providerRoute(in.ID)
	if !ok {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "只支持自定义供应商模型（provider/模型名）"})
		return
	}
	if !p.Enabled {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "供应商已停用，先到「自定义 API」开启"})
		return
	}
	go func() {
		verdicts := s.probeCapabilities(context.Background(), p, upstream)
		s.logCat(logx.CatProbe, "info", "能力实测 %s: 视觉=%s 音频=%s 文件=%s", in.ID,
			verdicts["vision"], verdicts["audio"], verdicts["file"])
	}()
	writeJSON(w, 200, map[string]any{"ok": true, "started": true})
}

// adminProbeOne re-tests a single model on demand and returns its fresh
// verdict — the backend of the per-card 测试 button. Synchronous: the fetch
// resolves when the probe verdict is in. Namespaced model ids probe their
// custom provider instead of the free lane.
func (s *Server) adminProbeOne(w http.ResponseWriter, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "missing model"})
		return
	}
	if p, upstream, ok := s.providerRoute(model); ok {
		if !p.Enabled {
			writeJSON(w, 400, map[string]any{"ok": false, "error": "供应商已停用，先到「自定义 API」开启"})
			return
		}
		s.customProbing.Store(model)
		defer s.customProbing.Store("")
		r := relay.ProbeModel(context.Background(), p, upstream)
		r.Model = model
		s.setCustomProbe(model, r)
		if r.TTFTMs > 0 {
			s.Store.AddTTFTSample(model, r.TTFTMs)
		}
		s.logCat(logx.CatProbe, "info", "探测自定义模型 %s → %s (首字 %dms)", model, r.State, r.TTFTMs)
		writeJSON(w, 200, map[string]any{"ok": true, "result": r})
		return
	}
	r := s.Lane.ProbeOne(model)
	if s.logger != nil {
		s.logger.Infof("手动探测 %s → %s (首字 %dms)", model, r.State, r.TTFTMs)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": r})
}

// currentProbingModel merges the free-lane and custom-provider probe states:
// whichever model is under a probe right now, so the dashboard can show
// 探测中… during a long NVIDIA queue instead of looking frozen.
func (s *Server) currentProbingModel() string {
	if v, ok := s.customProbing.Load().(string); ok && v != "" {
		return v
	}
	return s.Lane.ProbingModel()
}

// setCustomProbe records one custom model's probe verdict for /state.
func (s *Server) setCustomProbe(id string, r lane.ProbeResult) {
	s.customProbeMu.Lock()
	defer s.customProbeMu.Unlock()
	if s.customProbe == nil {
		s.customProbe = map[string]lane.ProbeResult{}
	}
	s.customProbe[id] = r
}

// customProbeOf returns the stored verdict, if any.
func (s *Server) customProbeOf(id string) (lane.ProbeResult, bool) {
	s.customProbeMu.Lock()
	defer s.customProbeMu.Unlock()
	r, ok := s.customProbe[id]
	return r, ok
}

// adminUpdateApply downloads the pending release asset and swaps the running
// binary; the process relaunches itself and the dashboard reconnects to the new
// instance. Gate: an update must have been detected first, and this platform
// must support an in-place swap.
func (s *Server) adminUpdateApply(w http.ResponseWriter) {
	if !s.updateAvailable || s.updateURL == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "没有检测到可用更新"})
		return
	}
	if !update.SelfUpdateSupported {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "此平台不支持一键更新，请从发布页下载新版本"})
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
