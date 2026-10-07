// zen-gate: a local tray gateway that exposes the OpenCode Zen free lane as
// OpenAI/Anthropic-compatible APIs and auto-configures installed agents.
//
// Protocol behaviour is ported from the MIT-licensed dsh-our-free-model
// plugin (github.com/zouyuxuan122/dsh-our-free-model); usage of the free lane
// remains subject to the upstream provider's terms.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"zen-gate/internal/agents"
	"zen-gate/internal/announce"
	"zen-gate/internal/autotag"
	"zen-gate/internal/gateway"
	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/notify"
	"zen-gate/internal/store"
	"zen-gate/internal/subs"
	"zen-gate/internal/tray"
	"zen-gate/internal/update"
	"zen-gate/internal/window"
)

// mainHwnd is the main window handle, captured at OnReady.
var mainHwnd uintptr

// windowHidden tracks tray-hide state for focus-aware notifications.
var windowHidden bool

func main() {
	// DPI awareness before any window (main window or tray) exists.
	window.SetProcessDPIAwareness()

	noTray := flag.Bool("no-tray", false, "console mode: no tray icon/window, log to stdout")
	port := flag.Int("port", 0, "override listen port")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("zen-gate", gateway.Version)
		return
	}

	st, err := store.Open()
	if err != nil {
		fmt.Println("open store:", err)
		os.Exit(1)
	}
	logger := logx.New(st.Home)
	defer logger.Close()
	logger.SetStdout(*noTray)

	window.SetAppUserModelID("zen-gate.gateway")

	if !window.AcquireSingleInstance(`Local\zen-gate-instance`, "Zen Gate · 本地免费模型网关") {
		logger.Infof("second launch: focused the running instance instead")
		return
	}

	cfg := st.Config()
	if *port > 0 {
		cfg.Port = *port
	}
	// v5: providers saved before model selection existed hold full catalog
	// dumps — trim them to the recommended picks so the 模型 page shows only
	// what was actually chosen.
	if n := gateway.MigrateProviderSelections(st); n > 0 {
		logger.Infof("已按推荐列表精简 %d 个自定义供应商的模型（旧版保存了整个目录）", n)
	}
	update.CleanupBackup() // remove the previous binary left by 一键更新
	logger.Infof("zen-gate %s 启动 (port %d, proxy %s)", gateway.Version, cfg.Port, cfg.ProxyMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := lane.NewLane()
	ln.SetDefaultMaxTokens(cfg.DefaultMaxTokens)
	ln.SetExposeRegion(cfg.ExposeRegion)
	ln.SetFailover(cfg.FailoverEnabled, cfg.FailoverMax)
	// Smart routing: master switch + failover ordering strategy + the runtime
	// first-token sample ring that the latency strategy ranks by.
	ln.SetSmartRouting(cfg.SmartRouting)
	ln.SetStrategy(cfg.RoutingStrategy)
	ln.SetTTFTSource(func(model string) int64 {
		avg, _ := st.TTFTStats(model)
		return avg
	})
	// Re-apply persisted capability tags (AI tagger / live probes) so the
	// catalog's modality verdicts survive restarts.
	for id, tag := range st.SnapshotTags() {
		audio, file, vision := tag.Audio, tag.File, tag.Vision
		ln.ApplyCapabilityTags(id, lane.CapabilityTags{
			Audio: &audio, File: &file, Vision: &vision,
			ContextWindow: tag.ContextWindow, MaxOutput: tag.MaxOutput,
		})
	}
	ln.LoadThrottleNotes(quotaNotesFromStore(st.SnapshotQuota()))
	ln.SetThrottleUpdate(func(model string, note lane.ThrottleNote) {
		qn := store.QuotaNote{ThrottledAt: note.ThrottledAt, CooldownUntil: note.CooldownUntil, LastOK: note.LastOK}
		for _, e := range note.Episodes {
			qn.Episodes = append(qn.Episodes, store.QuotaEpisode{Start: e.Start, End: e.End})
		}
		st.SetQuotaNote(model, qn)
	})
	lane.SetProxy(cfg.ProxyMode, cfg.ProxyURL)
	// 订阅轮询:sidecar manager wires into the gateway, the lane rotator and
	// every shutdown path below.
	mgr := subs.NewManager(st, logger.Infof)
	if cfg.SubsEnabled && len(cfg.Subscriptions) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := mgr.Apply(ctx); err != nil {
				logger.Errorf("订阅轮询启动失败: %v", err)
				return
			}
			if cfg.ProxyMode == "rotate" {
				lane.SetRotator(mgr)
				logger.Infof("订阅轮询已启动(%d 个健康节点)", mgr.Healthy())
			}
		}()
	}
	notify.SetEnabled(cfg.Notifications)
	// Probe first-token samples feed the persisted per-model average.
	ln.OnProbeResult = func(r lane.ProbeResult) {
		st.AddTTFTSample(r.Model, r.TTFTMs)
	}
	reg := agents.NewRegistry(st)
	gw := gateway.New(ln, st)
	gw.SetAgents(reg)
	gw.SetSubs(mgr)
	gw.SetLogger(logger)
	gw.ApplyLogSettings(cfg)
	// AI capability tagger: classifies provider model ids (and fills the lane
	// catalog's unverified audio/file fields) using the free lane itself.
	tagger := autotag.New(ln, st, logger.Infof)
	tagger.SetEnabled(cfg.AutoTagEnabled)
	tagger.Start(ctx)
	gw.SetTagger(tagger)
	enqueueUntagged := func() {
		ids := []string{}
		for _, p := range st.Config().Providers {
			for _, m := range p.Models {
				if _, ok := st.ModelTagOf(m); !ok {
					ids = append(ids, m)
				}
			}
		}
		// Lane models are deliberately NOT tagged: their ids are private, the
		// LLM cannot know them, and asking only burns thinking budget — the
		// curated table plus live probes own the lane's verdicts.
		if len(ids) > 0 {
			tagger.Enqueue(ids...)
		}
	}
	enqueueUntagged()
	window.SetDiag(func(msg string) { logger.Infof("%s", msg) })
	gw.SetAutostartState(agents.AutostartEnabled)

	stateText := func(s string) string {
		return map[string]string{lane.StateAvailable: "可用", lane.StateUnknown: "未知",
			lane.StateThrottled: "已限额", lane.StateRegionBlock: "地区受限",
			lane.StateUnavailable: "不可用"}[s]
	}
	ln.OnProbeEdge = func(model, from, to string) {
		if model == "" {
			logger.Warnf("免费车道整轮 429 限额，探测进入指数退避")
			if windowHidden {
				notify.Toast("免费车道已达限额", "本轮探测全部 429，稍后自动恢复")
			}
			return
		}
		logger.Infof("模型状态变化: %s %s → %s", model, stateText(from), stateText(to))
		if windowHidden {
			notify.Toast("模型状态变化", fmt.Sprintf("%s: %s → %s", model, stateText(from), stateText(to)))
		}
	}

	syncEndpoints := func() {
		reg.SetEndpoints(gw.BaseURL(), gw.InjectableModels())
		tray.SetStatus(trayStatus(st, ln))
	}
	ln.OnChange = syncEndpoints
	ln.StartLoops(ctx, time.Duration(cfg.ProbeIntervalMinutes)*time.Minute)
	syncEndpoints()

	if err := gw.Start(); err != nil {
		logger.Errorf("listen on port %d: %v", cfg.Port, err)
		fmt.Println("listen error:", err)
		os.Exit(1)
	}
	dashURL := strings.TrimSuffix(gw.BaseURL(), "/v1")
	logger.Infof("dashboard ready at %s", dashURL)
	if lan := gw.LANBaseURL(); lan != "" {
		logger.Infof("局域网 API 可用: %s", lan)
	}

	// periodic stats flush
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = st.FlushStats()
				st.FlushPerf()
			}
		}
	}()

	// update check loop (disabled until a feed URL is configured)
	checkUpdates := func() (bool, string) {
		cfg := st.Config()
		feed := strings.TrimSpace(cfg.UpdateFeed)
		if feed == "" {
			feed = update.FeedURL // compiled-in default (release builds)
		}
		if feed == "" {
			return false, ""
		}
		has, ver, url, _, err := update.Check(feed, update.HTTP{Timeout: 15 * time.Second, Client: lane.Client()})
		if err != nil {
			logger.Warnf("update check failed: %v", err)
			return false, ""
		}
		gw.SetUpdateState(has, ver, url)
		if has && cfg.LastVersion != ver {
			logger.Infof("发现新版本 %s (当前 %s)", ver, gateway.Version)
			if windowHidden {
				notify.Toast("Zen Gate 有新版本 "+ver, "当前 "+gateway.Version+" · 打开管理页查看下载链接")
			}
			st.Config().LastVersion = ver
			_ = st.Save()
			syncEndpoints()
		}
		return has, ver
	}
	gw.SetUpdateCheck(checkUpdates)
	go func() {
		time.Sleep(45 * time.Second)
		checkUpdates()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				checkUpdates()
			}
		}
	}()

	// announcement feed: pull the author's notices at boot and every 6h.
	// An empty feed URL disables the pull entirely.
	pullAnnouncements := func() {
		feed := strings.TrimSpace(st.Config().AnnouncementFeed)
		if feed == "" {
			gw.SetAnnouncements(nil)
			return
		}
		items, err := announce.Fetch(ctx, feed, lane.Client())
		if err != nil {
			logger.Warnf("公告拉取失败: %v", err)
			return
		}
		gw.SetAnnouncements(items)
		if len(items) > 0 {
			logger.Infof("公告已更新: %d 条生效中", len(items))
		}
	}
	gw.SetAnnouncementPull(pullAnnouncements)
	go func() {
		time.Sleep(20 * time.Second)
		pullAnnouncements()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pullAnnouncements()
			}
		}
	}()

	// anonymous usage heartbeat: fires only when a stats server is configured.
	// Payload is {installId, version, country} — nothing else leaves the machine.
	go func() {
		ping := func() {
			url := strings.TrimSpace(st.Config().StatsServerURL)
			if url == "" {
				return
			}
			_, _, eg := ln.Snapshot()
			body, _ := json.Marshal(map[string]any{
				"installId": st.Config().InstallID,
				"version":   gateway.Version,
				"country":   eg.Country,
			})
			req, err := http.NewRequest(http.MethodPost, strings.TrimRight(url, "/")+"/api/ping", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("content-type", "application/json")
			client := *lane.Client()
			client.Timeout = 10 * time.Second
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
		time.Sleep(90 * time.Second) // let the first egress detect finish
		ping()
		pt := time.NewTicker(6 * time.Hour)
		defer pt.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pt.C:
				ping()
			}
		}
	}()

	if *noTray {
		logger.Infof("serving %s (dashboard %s)", gw.BaseURL(), dashURL)
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		cancel()
		gw.Stop()
		mgr.Stop()
		_ = st.FlushStats()
		return
	}

	// tray lives on its own locked thread on Windows; window.Run owns the
	// platform's UI thread (main) and hosts the tray there — on macOS AppKit
	// requires both objects on that one thread.

	window.Run(window.Options{
		Title:    "Zen Gate · 本地免费模型网关",
		URL:      dashURL,
		DataPath: st.Home + string(os.PathSeparator) + "webview",
		Width:    1160,
		Height:   820,
		Tray: &window.TrayOptions{
			DashboardURL: dashURL,
			OnQuit: func() {
				saveWindowState(st)
				cancel()
				gw.Stop()
				mgr.Stop() // before os.Exit — the deferred path never runs here
				_ = st.FlushStats()
				os.Exit(0)
			},
			OnReprobe: func() { go ln.ProbeRound(context.Background(), true) },
			OnShow: func() {
				windowHidden = false
				if mainHwnd != 0 {
					window.ShowWindowWin(mainHwnd)
				}
			},
		},
		Bounds: func() *window.Bounds {
			w := cfg.Window
			if w.W > 0 {
				return &window.Bounds{X: w.X, Y: w.Y, W: w.W, H: w.H, Maximized: w.Maximized}
			}
			return nil
		}(),
		OnReady: func(hwnd uintptr) {
			mainHwnd = hwnd
			saveWindowState(st) // persist migrated defaults + restored bounds
		},
		OnCloseButton: func() bool {
			saveWindowState(st)
			if st.Config().CloseToTray {
				windowHidden = true
				if mainHwnd != 0 {
					window.HideWindow(mainHwnd)
				}
				logger.Infof("窗口已最小化到托盘")
				return false
			}
			return true
		},
	})

	// window closed → quit
	cancel()
	gw.Stop()
	mgr.Stop()
	_ = st.FlushStats()
	logger.Infof("zen-gate 已退出")
}

// quotaNotesFromStore converts persisted quota notes back into the lane's
// shape for boot-time seeding.
func quotaNotesFromStore(in map[string]store.QuotaNote) map[string]lane.ThrottleNote {
	out := map[string]lane.ThrottleNote{}
	for m, n := range in {
		note := lane.ThrottleNote{ThrottledAt: n.ThrottledAt, CooldownUntil: n.CooldownUntil, LastOK: n.LastOK}
		for _, e := range n.Episodes {
			note.Episodes = append(note.Episodes, lane.ThrottleEpisode{Start: e.Start, End: e.End})
		}
		out[m] = note
	}
	return out
}

func saveWindowState(st *store.Store) {
	if mainHwnd == 0 {
		return
	}
	// A minimized window reports the sentinel off-screen rect; persisting it
	// would make the next launch open invisibly. Keep the last good bounds.
	if window.IsMinimized(mainHwnd) {
		return
	}
	x, y, r, b := window.GetBounds(mainHwnd)
	if r-x < 400 || b-y < 300 {
		return // collapsed or garbage rect — keep the last good state
	}
	cfg := st.Config()
	cfg.Window = store.WindowState{X: x, Y: y, W: r - x, H: b - y, Maximized: window.IsMaximized(mainHwnd)}
	_ = st.Save()
}

func trayStatus(st *store.Store, ln *lane.Lane) string {
	days, _ := st.SnapshotStats()
	today := time.Now().Format("2006-01-02")
	tok := 0
	if d, ok := days[today]; ok {
		tok = d.Output
	}
	return fmt.Sprintf("运行中 · 今日输出 %d tok · %d 模型可用", tok, len(ln.ServableModels()))
}
