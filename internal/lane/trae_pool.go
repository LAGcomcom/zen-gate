package lane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// TRAE account pool, roster and the SOLO turn.
//
// The roster is one batch call that reports, per channel, which models it
// offers. That attribution is the whole reason the fetch cannot be replaced by a
// flat id list: a model is only callable through a channel that listed it, and
// asking elsewhere answers an in-stream code 4001 whose message ("param is
// invalid") reads like a malformed body.
//
// The stream is the SOLO dialect, not SSE `data:` frames — see readTraeSSE.

const traeMaxAccountTries = 3

// SetTraeAccounts replaces the pool.
func (l *Lane) SetTraeAccounts(accts []TraeAccount) {
	l.trMu.Lock()
	l.trAccts = accts
	l.trMu.Unlock()
	l.fireChange()
}

// TraeAccounts snapshots the pool.
func (l *Lane) TraeAccounts() []TraeAccount {
	l.trMu.RLock()
	defer l.trMu.RUnlock()
	out := make([]TraeAccount, len(l.trAccts))
	copy(out, l.trAccts)
	return out
}

// SetTraePersist wires the durable store.
func (l *Lane) SetTraePersist(fn func(TraeAccount)) {
	l.trMu.Lock()
	l.trPersist = fn
	l.trMu.Unlock()
}

// traeUpsert folds one account into the pool, keyed by uid so a second login
// updates instead of duplicating.
func (l *Lane) traeUpsert(acct TraeAccount) {
	if strings.TrimSpace(acct.ID) == "" {
		acct.ID = "trae-" + acct.UID
	}
	l.trMu.Lock()
	replaced := false
	for i := range l.trAccts {
		if l.trAccts[i].ID == acct.ID {
			l.trAccts[i] = acct
			replaced = true
			break
		}
	}
	if !replaced {
		l.trAccts = append(l.trAccts, acct)
	}
	persist := l.trPersist
	l.trMu.Unlock()
	if persist != nil {
		persist(acct)
	}
}

// TraeLogout removes one account.
func (l *Lane) TraeLogout(id string) {
	l.trMu.Lock()
	kept := make([]TraeAccount, 0, len(l.trAccts))
	for _, a := range l.trAccts {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	l.trAccts = kept
	l.trMu.Unlock()
	l.fireChange()
}

// TraeSetEnabled flips one account.
func (l *Lane) TraeSetEnabled(id string, enabled bool) bool {
	l.trMu.Lock()
	var updated *TraeAccount
	for i := range l.trAccts {
		if l.trAccts[i].ID == id {
			l.trAccts[i].Enabled = enabled
			cp := l.trAccts[i]
			updated = &cp
			break
		}
	}
	persist := l.trPersist
	l.trMu.Unlock()
	if updated == nil {
		return false
	}
	if persist != nil {
		persist(*updated)
	}
	l.fireChange()
	return true
}

// traeSpendable returns the accounts a turn may use.
func (l *Lane) traeSpendable() []TraeAccount {
	all := l.TraeAccounts()
	out := make([]TraeAccount, 0, len(all))
	for _, a := range all {
		if a.Enabled && strings.TrimSpace(a.AccessToken) != "" {
			out = append(out, a)
		}
	}
	return out
}

// traeFetchModels discovers the roster and the per-model channel routing.
//
// What matters is `config_info_list[].config_name` (the id) and which `function`
// block listed it (the route). Entries the upstream hides or disables are
// skipped: they answer an error if called, so publishing them would put dead
// rows in every picker.
func traeFetchModels(ctx context.Context, acct TraeAccount) ([]ModelInfo, map[string]traeRoute, error) {
	body, _ := json.Marshal(map[string]any{
		"functions":                append([]string{}, traeChannels...),
		"agent_type":               "",
		"current_config_info":      map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                0,
		"access_type":              0,
		"ab_force_vids":            "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":        true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeAgentBase+traeBatchModelsPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range traeHeaders(acct, "application/json") {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 25 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, &UpstreamError{Code: CodeServer,
			Message: "TRAE 模型目录 HTTP " + itoa(resp.StatusCode) + ": " + truncate(string(raw), 200)}
	}

	var parsed struct {
		FunctionConfigs []struct {
			Function       string `json:"function"`
			ConfigInfoList []struct {
				ConfigName    string `json:"config_name"`
				ConfigNameAlt string `json:"ConfigName"`
				ConfigSwitch  *bool  `json:"config_switch"`
				DisplayConfig struct {
					DisplayName   string `json:"display_name"`
					IsCustomModel bool   `json:"is_custom_model"`
					IsInvisible   bool   `json:"is_invisible_to_user"`
					Multimodal    bool   `json:"multimodal"`
					MaxMode       bool   `json:"max_mode"`
				} `json:"display_config"`
				Usage               string `json:"usage"`
				ContextWindowTokens struct {
					Dev int `json:"dev"`
					Max int `json:"max"`
				} `json:"context_window_tokens"`
				ModelDetailList []struct {
					Name      string `json:"name"`
					MaxTokens int    `json:"max_tokens"`
				} `json:"model_detail_list"`
				ReasoningEffortConfig struct {
					DefaultLevel    string   `json:"default_level"`
					Options         []string `json:"options"`
					SupportThinking bool     `json:"support_thinking"`
				} `json:"reasoning_effort_config"`
			} `json:"config_info_list"`
		} `json:"function_configs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, &UpstreamError{Code: CodeServer,
			Message: "TRAE 模型目录响应无法解析: " + truncate(string(raw), 200)}
	}

	seen := map[string]bool{}
	var rows []ModelInfo
	routes := map[string]traeRoute{}
	for _, fn := range parsed.FunctionConfigs {
		for _, c := range fn.ConfigInfoList {
			id := firstNonEmptyStr(c.ConfigName, c.ConfigNameAlt)
			if id == "" || seen[id] {
				continue
			}
			// A disabled or hidden entry is one the upstream refuses to serve;
			// publishing it would put a dead model in every picker.
			if c.ConfigSwitch != nil && !*c.ConfigSwitch {
				continue
			}
			if c.DisplayConfig.IsInvisible || c.DisplayConfig.IsCustomModel {
				continue
			}
			if c.Usage != "" && c.Usage != "chat_completion" {
				continue
			}
			seen[id] = true

			// Context: the `dev` rung is what the non-Max path may use. Mixing
			// it with `max` makes the client send a 1M prompt to a 200K window.
			ctxWin := c.ContextWindowTokens.Dev
			if ctxWin <= 0 {
				ctxWin = c.ContextWindowTokens.Max
			}
			maxOut := 0
			for _, d := range c.ModelDetailList {
				if d.MaxTokens > maxOut {
					maxOut = d.MaxTokens
				}
			}
			if maxOut <= 0 {
				maxOut = traeMaxTokensCap
			}

			route := traeRoute{
				Function:   fn.Function,
				ContextWin: ctxWin,
				MaxOutput:  maxOut,
				Reasoning:  c.ReasoningEffortConfig.SupportThinking,
				Vision:     c.DisplayConfig.Multimodal,
				Options:    c.ReasoningEffortConfig.Options,
			}
			if route.Function == "" {
				route.Function = traeDefaultFunction
			}
			routes[id] = route

			rows = append(rows, ModelInfo{
				ID:                 id,
				Name:               firstNonEmptyStr(c.DisplayConfig.DisplayName, id),
				Blurb:              "TRAE 账号渠道（字节），额度按套餐计；提示词可能被上游记录",
				Wire:               "chat",
				Channel:            ChannelTrae,
				Vision:             route.Vision,
				Reasoning:          route.Reasoning,
				ContextWindow:      route.ContextWin,
				MaxOutput:          route.MaxOutput,
				CanDisableThinking: route.Reasoning,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, routes, nil
}

// traeChat is one physical TRAE turn.
//
// SOLO events are translated into OpenAI chat chunks and fed to the caller's
// onData, so this channel reuses the same decoder as every other one instead of
// maintaining a second chunk path. Two in-stream facts stay load-bearing:
//
//   - an `error` event arrives on an HTTP 200, so it must be classified or a
//     refused turn looks like an empty answer;
//   - the first content-bearing event commits the turn, so the caller checks
//     `emitted` before switching accounts.
func (l *Lane) traeChat(ctx context.Context, body map[string]any, acct TraeAccount, onData func([]byte) error) (*Usage, bool, *UpstreamError) {
	model, _ := body["model"].(string)
	built := traeBuildBody(model, body)

	payload, err := json.Marshal(built)
	if err != nil {
		return nil, false, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeAgentBase+traeChatPath, strings.NewReader(string(payload)))
	if err != nil {
		return nil, false, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range traeHeaders(acct, "text/event-stream") {
		req.Header.Set(k, v)
	}

	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, false, &UpstreamError{Code: classifyConnErr(err), Message: err.Error()}
	}
	defer resp.Body.Close()

	pump := newStreamReader(ctx, resp.Body, traeTurnTimeout)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest, _ := io.ReadAll(io.LimitReader(pump, 1<<20))
		return nil, false, traeClassifyHTTP(resp.StatusCode, string(head)+string(rest))
	}
	pump.Unread(head)

	var (
		emitted bool
		sawTool bool
		toolIdx = map[int]bool{}
	)
	// chunkOf renders one OpenAI chat chunk the shared decoder understands.
	chunkOf := func(delta map[string]any, finish any) []byte {
		frame := map[string]any{
			"id":      "trae-" + model,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(frame)
		return b
	}

	usage, rerr := readTraeSSE(pump, func(ev traeEvent) error {
		switch ev.Name {
		case "output":
			if txt := jsonString(ev.Data["response"]); txt != "" {
				emitted = true
				if err := onData(chunkOf(map[string]any{"content": txt}, nil)); err != nil {
					return err
				}
			}
			if think := jsonString(ev.Data["reasoning_content"]); think != "" {
				emitted = true
				if err := onData(chunkOf(map[string]any{"reasoning_content": think}, nil)); err != nil {
					return err
				}
			}
			if calls, ok := ev.Data["tool_calls"].([]any); ok {
				for i, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					idx := i
					if f, ok := toNum(call["index"]); ok {
						idx = int(f)
					}
					fc, ok := call["function_call"].(map[string]any)
					if !ok {
						if fc, ok = call["function"].(map[string]any); !ok {
							continue
						}
					}
					// SOLO's own envelope keys must not travel into the
					// OpenAI-shaped function object.
					fnPart := map[string]any{}
					if name := jsonString(fc["name"]); name != "" {
						fnPart["name"] = name
					}
					if args := jsonString(fc["arguments"]); args != "" {
						fnPart["arguments"] = args
					}
					if len(fnPart) == 0 {
						continue
					}
					tc := map[string]any{"index": idx, "function": fnPart}
					if id := jsonString(call["id"]); id != "" {
						tc["id"] = id
						tc["type"] = "function"
					}
					sawTool = true
					toolIdx[idx] = true
					emitted = true
					if err := onData(chunkOf(map[string]any{"tool_calls": []any{tc}}, nil)); err != nil {
						return err
					}
				}
			}
		case "done":
			reason := traeFinish(jsonString(ev.Data["finish_reason"]), sawTool)
			return onData(chunkOf(map[string]any{}, reason))
		case "error":
			code := 0
			if f, ok := toNum(ev.Data["code"]); ok {
				code = int(f)
			}
			return traeClassifyEvent(code, jsonString(ev.Data["message"]))
		}
		return nil
	})
	if rerr != nil {
		if ue, ok := rerr.(*UpstreamError); ok {
			return usage, emitted, ue
		}
		if ctx.Err() != nil {
			return usage, emitted, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return usage, emitted, &UpstreamError{Code: CodeTransport, Message: rerr.Error()}
	}
	if !emitted && len(toolIdx) == 0 {
		// An HTTP 200 with no events is the SOLO equivalent of a cut stream.
		return usage, false, &UpstreamError{Code: CodeTransport, Message: "上游未返回任何事件"}
	}
	return usage, emitted, nil
}

// traePost walks the TRAE pool for one turn, switching only on per-account
// refusals and only before the first byte reached the client.
func (l *Lane) traePost(ctx context.Context, body map[string]any, onData func([]byte) error) (*Usage, error) {
	accts := l.traeSpendable()
	if len(accts) == 0 {
		return nil, &UpstreamError{Code: CodeCredential,
			Message: "还没有登录任何 TRAE 账号，请先在渠道页登录"}
	}
	var lastErr *UpstreamError
	for i, acct := range accts {
		if i >= traeMaxAccountTries {
			break
		}
		usage, emitted, uerr := l.traeChat(ctx, body, acct, onData)
		if uerr == nil {
			return usage, nil
		}
		lastErr = uerr
		switch uerr.Code {
		case CodeQuota, CodeCredential:
			l.logf("TRAE 账号 %s 不可用：%s", acct.Nickname, truncate(uerr.Message, 120))
		default:
			// A request-shaped defect answers identically on every account.
			return usage, uerr
		}
		if emitted {
			return usage, uerr
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &UpstreamError{Code: CodeQuota, Message: "TRAE 账号都不可用"}
}

// traeFinish maps the SOLO finish_reason onto the lane vocabulary.
func traeFinish(raw string, sawTools bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop":
		return FinishStop
	case "tool_calls":
		return FinishToolCalls
	case "length":
		return FinishMaxTokens
	}
	if sawTools {
		// No terminal reason but tool calls arrived: the upstream cut the turn.
		return FinishToolCalls
	}
	return FinishStop
}

// traeClassifyHTTP maps an HTTP-level refusal. SOLO mostly reports errors
// in-stream, so this only covers transport-shaped failures.
func traeClassifyHTTP(status int, payload string) *UpstreamError {
	msg := truncate(payload, 300)
	switch {
	case status == 401 || status == 403:
		return &UpstreamError{Code: CodeCredential, Message: "TRAE 登录态失效（HTTP " + itoa(status) + "）"}
	case status == 429:
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: 60}
	case status == 404:
		return &UpstreamError{Code: CodeClient, Message: msg}
	case status >= 500:
		return &UpstreamError{Code: CodeServer, Message: msg}
	default:
		return &UpstreamError{Code: CodeClient, Message: msg}
	}
}
