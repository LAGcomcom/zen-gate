package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// qoderBaseURL is where the probes and pool actions are sent. It equals the
// sidecar's bind in production and exists as a variable so a test can point the
// card's read path at a stub instead of a real gateway on a fixed port. The
// managed provider's BaseURL is NOT built from this: that one must stay the
// real bind even under test.
var qoderBaseURL = qoderGatewayURL

type qoderStatus struct {
	Running       bool               `json:"running"`
	LoggedIn      bool               `json:"loggedIn"`
	ModelIDs      []string           `json:"models"`
	Detail        string             `json:"detail"`
	ProviderWired bool               `json:"providerWired"`
	Catalog       []lane.ListingMeta `json:"-"`

	// The account pool the sidecar exposes. PoolEnabled is false when the
	// sidecar runs on the live IDE login alone, which is a perfectly good way
	// to run — the card then says so instead of showing an empty roster.
	PoolEnabled bool               `json:"poolEnabled"`
	PoolDir     string             `json:"poolDir"`
	Pool        []qoderPoolAccount `json:"pool"`
	IDELogin    bool               `json:"ideLogin"`
	PoolDetail  string             `json:"poolDetail"`
}

// qoderPoolAccount is one sidecar pool row as the card needs it.
type qoderPoolAccount struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	UID           string `json:"uid"`
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	CooldownUntil string `json:"cooldownUntil"`
	LastError     string `json:"lastError"`
	ExpiresAt     string `json:"expiresAt"`
	DaysLeft      int    `json:"daysLeft"`
}

func qoderGet(path string) (int, []byte, error) {
	timeout := qoderProbeFast
	if strings.HasPrefix(path, "/quota") || strings.HasPrefix(path, "/v1/") {
		timeout = qoderProbeSlow
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(qoderBaseURL + path)
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
	// The roster is read here, not in the status handler, because the cached
	// probe is what a page repaint reuses: reading it outside the cache would
	// put a live request behind every 10-second tick and still show a roster
	// that disagrees with the rest of the same response.
	st.PoolEnabled, st.PoolDir, st.Pool, st.IDELogin, st.PoolDetail = probeQoderPool()
	if len(st.ModelIDs) == 0 {
		st.LoggedIn = false
		st.Detail = "已登录但渠道没有返回任何模型"
	}
	return st
}

// qoderRow mirrors one listing row's capability declarations. QoderCN states
// more than the OpenAI surface exposes: thinking rungs per model, and the
// context tiers it sells.
type qoderRow struct {
	ID          string `json:"id"`
	IsVL        bool   `json:"is_vl"`
	IsReasoning bool   `json:"is_reasoning"`

	// ThinkingConfig is the vendor's own picker definition: "disabled" is
	// present when thinking can be switched off, "enabled.efforts" names the
	// rungs, and the rung starred is what a request with no level gets.
	ThinkingConfig struct {
		Disabled json.RawMessage `json:"disabled"`
		Enabled  *struct {
			IsDefault bool                       `json:"is_default"`
			Efforts   map[string]qoderEffortRung `json:"efforts"`
		} `json:"enabled"`
	} `json:"thinking_config"`

	// ContextConfig is the tier table ("200K"/"1M" → token_count). The row's
	// max_input_tokens is the tier the IDE currently has selected, so it is
	// not the model's capacity — the widest tier is.
	ContextConfig map[string]struct {
		IsDefault  bool `json:"is_default"`
		TokenCount int  `json:"token_count"`
	} `json:"context_config"`
}

type qoderEffortRung struct {
	IsDefault bool `json:"is_default"`
}

// effortRungRank orders a vendor's rung names lightest first. JSON objects have
// no order, so without this the picker would shuffle between listings: "1M"
// before "200K", "xhigh" before "low".
var effortRungRank = map[string]int{
	"none": 0, "disabled": 0, "off": 0,
	"minimal": 10, "low": 20, "light": 20,
	"medium": 30, "balanced": 30,
	"high":  40,
	"xhigh": 50,
	"max":   60,
}

// sortEffortRungs orders the names the listing declared. Unknown names keep a
// place at the end, ordered by length then alphabetically, so an unrecognised
// vendor ladder still reads light→heavy instead of random.
func sortEffortRungs(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		ri, oki := effortRungRank[names[i]]
		rj, okj := effortRungRank[names[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			if len(names[i]) != len(names[j]) {
				return len(names[i]) < len(names[j])
			}
			return names[i] < names[j]
		}
	})
}

// qoderThinkingRungs returns the vendor's rung names (ordered) and the starred
// default. Empty rungs means the row declared no levels, which the caller must
// read as "nothing stated", not "thinking is broken".
func qoderThinkingRungs(row qoderRow) ([]string, string) {
	if row.ThinkingConfig.Enabled == nil || len(row.ThinkingConfig.Enabled.Efforts) == 0 {
		return nil, ""
	}
	names := make([]string, 0, len(row.ThinkingConfig.Enabled.Efforts))
	def := ""
	for name, rung := range row.ThinkingConfig.Enabled.Efforts {
		names = append(names, name)
		if rung.IsDefault {
			def = name
		}
	}
	sortEffortRungs(names)
	return names, def
}

// qoderWidestContext is the largest tier the row sells. Zero means the row
// declared no table, and the caller keeps whatever max_input_tokens said.
func qoderWidestContext(row qoderRow) int {
	best := 0
	for _, tier := range row.ContextConfig {
		if tier.TokenCount > best {
			best = tier.TokenCount
		}
	}
	return best
}

// parseQoderRows reads the capability flags out of the listing body. The
// sidecar wraps its rows in {"data":[…]}, so that shape is tried first; a bare
// array is accepted too because the flag reader must not depend on which
// envelope a given sidecar build chose.
func parseQoderRows(body []byte) []qoderRow {
	var wrapped struct {
		Data []qoderRow `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Data) > 0 {
		return wrapped.Data
	}
	var bare []qoderRow
	if err := json.Unmarshal(body, &bare); err == nil {
		return bare
	}
	return nil
}

// parseQoderCatalog turns the sidecar's listing rows into ListingMeta through
// the shared row reader. Everything the card and the picker claim about a Qoder
// model comes from the row itself: the vendor states is_vl / is_reasoning, the
// thinking rungs and the context tiers per model, and without the declared
// flags the capability chain discards all of it as unknown. There is no local
// override table: the listing now states the facts that used to need one.
func parseQoderCatalog(body []byte) []lane.ListingMeta {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	rows := lane.ParseListingMeta(payload)
	byID := map[string]qoderRow{}
	for _, r := range parseQoderRows(body) {
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
			rungs, def := qoderThinkingRungs(r)
			rows[i].Efforts = rungs
			rows[i].EffortDefault = def
			// A model that publishes rungs thinks, whatever the flag said:
			// the picker would otherwise be built for a model the listing
			// calls plain text.
			if len(rungs) > 0 {
				rows[i].Reasoning = true
			}
			// The widest tier outranks the row's max_input_tokens, which only
			// reflects the tier the IDE happens to have selected.
			if w := qoderWidestContext(r); w > rows[i].ContextWindow {
				rows[i].ContextWindow = w
			}
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

// probeQoderPool reads the sidecar's account roster. It is separate from
// probeQoder because a sidecar older than the pool endpoint must still answer
// the card: a 404 here means "single-account sidecar", not "broken".
func probeQoderPool() (bool, string, []qoderPoolAccount, bool, string) {
	code, body, err := qoderGet("/pool")
	if err != nil || code != 200 {
		return false, "", nil, false, ""
	}
	var payload struct {
		Enabled  bool               `json:"enabled"`
		Dir      string             `json:"dir"`
		IDELogin bool               `json:"ideLogin"`
		Detail   string             `json:"detail"`
		Accounts []qoderPoolAccount `json:"accounts"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false, "", nil, false, ""
	}
	return payload.Enabled, payload.Dir, payload.Accounts, payload.IDELogin, payload.Detail
}

// adminQoderPool drives one pool action through the sidecar. The gateway does
// not own this pool — the hub does — so every action is a request, never a
// local mutation, and the card re-reads the roster afterwards.
func (s *Server) adminQoderPool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action  string `json:"action"`
		ID      string `json:"id"`
		Label   string `json:"label"`
		Enabled *bool  `json:"enabled"`
		// State/Realm drive the device-authorization actions; they are forwarded
		// verbatim because the hub, not the gateway, owns the login window.
		State string `json:"state"`
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	payload, err := json.Marshal(in)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	client := &http.Client{Timeout: qoderProbeSlow}
	resp, err := client.Post(qoderBaseURL+"/pool/account", "application/json", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": "无法连接内置 Qoder 网关：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		// The sidecar speaks OpenAI's error envelope; the card speaks a plain
		// string. Unwrap the message so the toast reads the actual reason
		// ("未知的 Qoder 区域…") instead of a JSON blob.
		msg := strings.TrimSpace(string(respBody))
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &envelope) == nil && strings.TrimSpace(envelope.Error.Message) != "" {
			msg = strings.TrimSpace(envelope.Error.Message)
		}
		writeJSON(w, resp.StatusCode, map[string]any{"ok": false, "error": msg})
		return
	}
	// The roster changed, so the next page load must not read a stale probe.
	qoderProbeMu.Lock()
	qoderProbeAt = time.Time{}
	qoderProbeMu.Unlock()
	if s.Lane != nil {
		go s.Lane.RefreshCatalog(context.Background())
	}
	// The sidecar's answer carries the login window (state/authUrl) and the
	// poll's verdict, so it goes back as-is instead of being flattened to ok.
	out := map[string]any{"ok": true}
	if len(respBody) > 0 {
		var pass map[string]any
		if json.Unmarshal(respBody, &pass) == nil {
			out = pass
			out["ok"] = true
		}
	}
	writeJSON(w, 200, out)
}
