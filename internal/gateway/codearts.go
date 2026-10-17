package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/codeartshub"
	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// The CodeArts 账号渠道: a managed provider backed by the embedded
// CodeArts2API gateway (vendored, MIT — see internal/codeartshub). It turns a
// Huawei Cloud CodeArts Agent account into an OpenAI-compatible endpoint, and
// zen-gate observes it the same way it observes the Qoder sidecar:
//
//	已登录且有模型 → 供应商启用，模型进入所有选择器
//	其余状态       → 供应商停用，模型从选择器消失，卡片给出指引
//
// The difference from Qoder is where the credential comes from: Qoder reads the
// IDE's cached login, while CodeArts needs a Huawei Cloud OAuth round-trip. The
// card therefore offers the login link the embedded panel already mints, and
// the state becomes "logged in" once an account file lands.

const (
	codeartsProviderID = "codearts"
	codeartsProbeFast  = 2500 * time.Millisecond
	codeartsProbeSlow  = 8 * time.Second
)

type codeartsStatus struct {
	Running       bool               `json:"running"`
	LoggedIn      bool               `json:"loggedIn"`
	Accounts      int                `json:"accounts"`
	ModelIDs      []string           `json:"models"`
	Detail        string             `json:"detail"`
	ProviderWired bool               `json:"providerWired"`
	LoginURL      string             `json:"loginUrl,omitempty"`
	Catalog       []lane.ListingMeta `json:"-"`
}

func codeartsGet(path string) (int, []byte, error) {
	timeout := codeartsProbeFast
	if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/status") {
		timeout = codeartsProbeSlow
	}
	url := fmt.Sprintf("http://%s:%d%s", codeartshub.HubHost, codeartshub.HubPort, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("authorization", "Bearer "+codeartshub.APIKey)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 2048)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil || len(body) > 1<<20 {
			break
		}
	}
	return resp.StatusCode, body, nil
}

var (
	codeartsProbeMu   sync.Mutex
	codeartsProbeAt   time.Time
	codeartsProbeLast codeartsStatus
)

// probeCodeartsCached serves the dashboard; /v1/models rides the upstream, so
// repeated page refreshes share one answer for a minute.
func probeCodeartsCached(force bool) codeartsStatus {
	codeartsProbeMu.Lock()
	defer codeartsProbeMu.Unlock()
	if !force && time.Since(codeartsProbeAt) < time.Minute {
		return codeartsProbeLast
	}
	st := probeCodearts()
	codeartsProbeAt = time.Now()
	codeartsProbeLast = st
	return st
}

// parseCodeartsAccounts reads the embedded hub's /status answer. The field is
// an array of account objects, not a count: decoding it into an int failed
// silently and kept the channel logged out forever after a successful OAuth
// (issue #84).
func parseCodeartsAccounts(body []byte) (int, error) {
	var status struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return 0, err
	}
	return len(status.Accounts), nil
}

// probeCodearts observes the embedded gateway: running? accounts loaded? which
// models? Liveness is judged by what matters — can it list models — never by a
// cosmetic endpoint.
func probeCodearts() codeartsStatus {
	st := codeartsStatus{}
	code, body, err := codeartsGet("/status")
	if err != nil || code != 200 {
		st.Detail = "内置 CodeArts 网关未运行（127.0.0.1:8096）"
		return st
	}
	st.Running = true

	n, err := parseCodeartsAccounts(body)
	if err != nil {
		st.Detail = "内置 CodeArts 网关状态无法解析: " + err.Error()
		return st
	}
	st.Accounts = n

	code, body, err = codeartsGet("/v1/models")
	if err != nil {
		st.Detail = "网关在线但模型列表查询失败: " + err.Error()
		return st
	}
	if code == 401 {
		st.Detail = "尚未登录华为云账号——点「授权登录」完成一次 OAuth"
		return st
	}
	if code != 200 {
		st.Detail = fmt.Sprintf("模型列表查询返回 HTTP %d", code)
		return st
	}
	// A logged-in state needs an actual account. The vendored server serves a
	// static fallback roster when no account is loaded, so "models came back"
	// alone would report a login the user never did — and then the managed
	// provider would point at a hub that cannot answer a single turn.
	st.LoggedIn = st.Accounts > 0

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
	st.Catalog = parseCodeartsCatalog(body)
	if len(st.ModelIDs) == 0 {
		st.LoggedIn = false
		st.Detail = "已登录但渠道没有返回任何模型（可能尚未领取限时福利）"
	}
	if !st.LoggedIn {
		st.Detail = "尚未登录华为云账号——点「授权登录」完成一次 OAuth"
	}
	return st
}

// parseCodeartsCatalog turns the listing rows into ListingMeta through the
// shared reader. CodeArts2API publishes per-model context windows and a
// benefit flag, so every row it describes is marked declared — without the
// flags the capability chain would discard all of it as unknown.
func parseCodeartsCatalog(body []byte) []lane.ListingMeta {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	rows := lane.ParseListingMeta(payload)
	for i := range rows {
		if rows[i].ContextWindow > 0 {
			rows[i].InputDeclared = true
			rows[i].OutputDeclared = true
		}
	}
	return rows
}

// ensureCodeartsProvider syncs the managed provider to the observed state: a
// logged-in hub means enabled with the fresh roster; anything else means
// disabled with the models hidden from every picker. Models the user unchecked
// survive via the hidden set, exactly like the Qoder channel.
func (s *Server) ensureCodeartsProvider(st codeartsStatus) bool {
	cfg := s.Store.Config()
	idx := -1
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == codeartsProviderID {
			idx = i
			break
		}
	}
	if !st.LoggedIn {
		if idx == -1 || !cfg.Providers[idx].Enabled {
			return false
		}
		s.Store.Mutate(func(c *store.Config) {
			for i := range c.Providers {
				if c.Providers[i].ID == codeartsProviderID {
					c.Providers[i].Enabled = false
				}
			}
		})
		_ = s.Store.Save()
		s.logCat(logx.CatAdmin, "info", "CodeArts 账号渠道未登录，供应商已停用")
		return false
	}
	if idx == -1 {
		p := store.Provider{
			ID: codeartsProviderID, Name: "CodeArts 账号渠道",
			BaseURL: codeartshub.BaseURL, APIKey: codeartshub.APIKey,
			Protocol: store.ProtocolOpenAI,
			Enabled:  true, Models: st.ModelIDs,
			Note: "由 zen-gate 内置的 CodeArts2API 网关自动管理（华为云账号授权登录）",
		}
		mergeCatalog(&p, st.Catalog)
		s.Store.Mutate(func(c *store.Config) { c.Providers = append(c.Providers, p) })
		_ = s.Store.Save()
		if s.tagger != nil && len(p.Models) > 0 {
			s.tagger.Enqueue(p.Models...)
		}
		s.logCat(logx.CatAdmin, "info", "CodeArts 账号渠道已登录：自动接入 %d 个模型", len(p.Models))
		return true
	}
	p := cfg.Providers[idx]
	changed := !p.Enabled
	s.Store.Mutate(func(c *store.Config) {
		for i := range c.Providers {
			if c.Providers[i].ID != codeartsProviderID {
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
		s.logCat(logx.CatAdmin, "info", "CodeArts 账号渠道已登录：供应商重新启用")
	}
	if s.registry != nil {
		time.AfterFunc(1500*time.Millisecond, func() {
			if s.modelsSync != nil {
				s.modelsSync()
			}
			if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
				s.logger.Infof("CodeArts 渠道元数据更新，已重新注入 %d 个已开启 Agent 的配置", n)
			}
		})
	}
	return true
}

// adminCodeartsStatus answers the dashboard's CodeArts card: observe the
// embedded hub, sync the managed provider, report.
func (s *Server) adminCodeartsStatus(w http.ResponseWriter, r *http.Request) {
	st := probeCodeartsCached(r.URL.Query().Get("force") == "1")
	st.ProviderWired = s.ensureCodeartsProvider(st)
	if !st.LoggedIn {
		// The embedded panel owns the OAuth round-trip; point the card at it.
		st.LoginURL = fmt.Sprintf("http://%s:%d/", codeartshub.HubHost, codeartshub.HubPort)
	}
	writeJSON(w, 200, st)
}
