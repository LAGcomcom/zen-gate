package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// The Qoder 账号渠道: a managed provider backed by a local qodercn-gateway
// sidecar (github.com/Morpheus799/qodercn-gateway) that harvests the QoderCN
// IDE's cached login and speaks OpenAI against gateway.qoder.com.cn. zen-gate
// never logs in itself — the Qoder IDE owns the credential and refreshes it —
// so the channel's state is observed, not driven:
//
//	IDE 登录且网关运行 → 供应商启用，模型进入所有选择器
//	其余状态           → 供应商停用，模型从选择器消失，卡片给出指引
//
// The sidecar's short-lived token means "logged in" can lapse between page
// views; every status probe re-syncs the managed provider to what is true.

const (
	qoderGatewayURL = "http://127.0.0.1:8095" // the sidecar's default bind
	qoderProviderID = "qoder"
	qoderProbeFast  = 2500 * time.Millisecond // /health is local, fast
	qoderProbeSlow  = 8 * time.Second         // /quota and /v1/models ride the upstream
)

type qoderStatus struct {
	Running       bool               `json:"running"`
	LoggedIn      bool               `json:"loggedIn"`
	ModelIDs      []string           `json:"models"`
	Detail        string             `json:"detail"`
	ProviderWired bool               `json:"providerWired"`
	Catalog       []lane.ListingMeta `json:"-"`
}

func qoderGet(path string) (int, []byte, error) {
	timeout := qoderProbeFast
	if strings.HasPrefix(path, "/quota") || strings.HasPrefix(path, "/v1/") {
		timeout = qoderProbeSlow
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(qoderGatewayURL + path)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 2048)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
		if len(body) > 1<<20 {
			break
		}
	}
	return resp.StatusCode, body, nil
}

var (
	qoderProbeMu   sync.Mutex
	qoderProbeAt   time.Time
	qoderProbeLast qoderStatus
)

// probeQoderCached serves the dashboard: the probe's /quota rides the
// upstream, so repeated page refreshes share one result for a minute instead
// of hammering it every 10-second tick.
func probeQoderCached(force bool) qoderStatus {
	qoderProbeMu.Lock()
	defer qoderProbeMu.Unlock()
	if !force && time.Since(qoderProbeAt) < time.Minute {
		return qoderProbeLast
	}
	st := probeQoder()
	qoderProbeAt = time.Now()
	qoderProbeLast = st
	return st
}

// probeQoder observes the sidecar: running? token active? which models?
func probeQoder() qoderStatus {
	st := qoderStatus{}
	code, body, err := qoderGet("/health")
	if err != nil || code != 200 {
		st.Detail = "qodercn-gateway 未运行（127.0.0.1:8095）"
		return st
	}
	st.Running = true

	// The sidecar has two auth planes: model calls sign with Cosy credentials
	// (the IDE login, long-lived), while /quota uses a short-lived access
	// token that expires independently. Liveness is therefore judged by the
	// thing that matters — can it still list and serve models — never by
	// /quota, whose expiry is cosmetic.
	code, body, err = qoderGet("/v1/models")
	if err != nil {
		st.Detail = "网关在线但模型列表查询失败: " + err.Error()
		return st
	}
	if code == 401 || strings.Contains(string(body), "TOKEN_EXPIRE") {
		st.Detail = "token 已过期——打开 Qoder IDE 登录一次，网关每个请求都会重读凭据，下一个请求自动恢复"
		return st
	}
	if code != 200 {
		st.Detail = fmt.Sprintf("模型列表查询返回 HTTP %d", code)
		return st
	}
	st.LoggedIn = true
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &listing) != nil {
		st.Detail = "已登录但模型列表无法解析"
		return st
	}
	for _, m := range listing.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			st.ModelIDs = append(st.ModelIDs, id)
		}
	}
	sort.Strings(st.ModelIDs)
	st.Catalog = parseQoderCatalog(body)
	if len(st.ModelIDs) == 0 {
		st.LoggedIn = false
		st.Detail = "已登录但渠道没有返回任何模型"
	}
	return st
}

// qoderRow mirrors one listing row's capability flags.
type qoderRow struct {
	ID          string `json:"id"`
	IsVL        bool   `json:"is_vl"`
	IsReasoning bool   `json:"is_reasoning"`
}

// qoderCorrections carries user-verified facts that override the sidecar's
// own listing: the vendor UI states Qwen3.8-Flash serves 1M context with a
// top thinking rung, while the listing publishes neither.
var qoderCorrections = map[string]struct {
	contextWindow int
	reasoning     bool
}{
	"Qwen3.8-Flash": {contextWindow: 1000000, reasoning: true},
}

// parseQoderCatalog turns the sidecar's listing rows into ListingMeta through
// the shared row reader, applies the verified corrections, and marks every row
// the listing says anything about as declared — the vendor states is_vl /
// is_reasoning and the context capacity per model, and without the declared
// flags the capability chain discards all of it as unknown.
func parseQoderCatalog(body []byte) []lane.ListingMeta {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	rows := lane.ParseListingMeta(payload)
	var raw []qoderRow
	_ = json.Unmarshal(body, &raw)
	byID := map[string]qoderRow{}
	for _, r := range raw {
		if r.ID != "" {
			byID[r.ID] = r
		}
	}
	for i := range rows {
		if r, ok := byID[rows[i].ID]; ok {
			rows[i].Vision = r.IsVL
			rows[i].Reasoning = r.IsReasoning
			rows[i].InputDeclared = true
			rows[i].OutputDeclared = true
		}
		if c, ok := qoderCorrections[rows[i].ID]; ok {
			rows[i].ContextWindow = c.contextWindow
			rows[i].Reasoning = c.reasoning
		}
		if rows[i].ContextWindow > 0 {
			rows[i].InputDeclared = true
		}
	}
	return rows
}

// ensureQoderProvider syncs the managed provider to the observed state: a
// logged-in sidecar means enabled with the fresh roster; anything else means
// disabled with the models hidden from every picker. Models the user unchecked
// on the 模型 page survive via the hidden set, which is separate from the
// provider roster.
func (s *Server) ensureQoderProvider(st qoderStatus) bool {
	cfg := s.Store.Config()
	idx := -1
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == qoderProviderID {
			idx = i
			break
		}
	}
	if !st.LoggedIn {
		// Hide the models while the login is down; nothing to create.
		if idx == -1 || !cfg.Providers[idx].Enabled {
			return false
		}
		s.Store.Mutate(func(c *store.Config) {
			for i := range c.Providers {
				if c.Providers[i].ID == qoderProviderID {
					c.Providers[i].Enabled = false
				}
			}
		})
		_ = s.Store.Save()
		s.logCat(logx.CatAdmin, "info", "Qoder 账号渠道未登录，供应商已停用")
		return false
	}
	// Logged in: create on first sight, sync the roster on later probes.
	if idx == -1 {
		p := store.Provider{
			ID: qoderProviderID, Name: "Qoder 账号渠道",
			BaseURL: qoderGatewayURL + "/v1", Protocol: store.ProtocolOpenAI,
			Enabled: true, Models: st.ModelIDs,
			Note: "由 zen-gate 托管的 qodercn-gateway sidecar 自动管理，登录状态跟随 Qoder IDE",
		}
		mergeCatalog(&p, st.Catalog)
		s.Store.Mutate(func(c *store.Config) {
			c.Providers = append(c.Providers, p)
		})
		_ = s.Store.Save()
		if s.tagger != nil && len(p.Models) > 0 {
			s.tagger.Enqueue(p.Models...)
		}
		s.logCat(logx.CatAdmin, "info", "Qoder 账号渠道已登录：自动接入 %d 个模型", len(p.Models))
		return true
	}
	p := cfg.Providers[idx]
	// The roster is the user's: they may have unchecked models in the edit
	// form, and this sync must never resurrect them. Only capacities (the
	// listing meta, with verified corrections applied) re-sync here.
	changed := !p.Enabled
	s.Store.Mutate(func(c *store.Config) {
		for i := range c.Providers {
			if c.Providers[i].ID != qoderProviderID {
				continue
			}
			if !c.Providers[i].Enabled {
				c.Providers[i].Enabled = true
			}
			if len(st.Catalog) > 0 {
				mergeCatalog(&c.Providers[i], st.Catalog)
			}
		}
	})
	_ = s.Store.Save()
	if changed {
		s.logCat(logx.CatAdmin, "info", "Qoder 账号渠道已登录：供应商重新启用")
	}
	if s.registry != nil {
		time.AfterFunc(1500*time.Millisecond, func() {
			if s.modelsSync != nil {
				s.modelsSync() // the resync reads the cached model set: refresh it first
			}
			if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
				s.logger.Infof("Qoder 渠道元数据更新，已重新注入 %d 个已开启 Agent 的配置", n)
			}
		})
	}
	return true
}

// adminQoderStatus answers the dashboard's Qoder card: observe the sidecar,
// sync the managed provider, report.
func (s *Server) adminQoderStatus(w http.ResponseWriter, r *http.Request) {
	st := probeQoderCached(r.URL.Query().Get("force") == "1")
	st.ProviderWired = s.ensureQoderProvider(st)
	writeJSON(w, 200, st)
}
