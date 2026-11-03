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

// ZCode account pool, roster and the inference turn.
//
// The roster is remote (GET /api/v1/client/configs), not curated: the platform
// publishes the models an account may call, their context window, their vision
// flag and the thinking rungs each one accepts. That endpoint's shape has three
// traps worth stating where they are handled:
//
//   - `data.builtinModels` is an OBJECT keyed by an index, not an array. A
//     typed array decode silently yields zero rows — "no models available" for
//     an account that has plenty.
//   - `platform` must be the literal "unknown". win32 / windows / electron are
//     answered 400 + code 3001.
//   - `reasoning.levels` is a MAP: its key order is the insertion order of the
//     upstream JSON, not the ladder. Sorting is what makes the effort picker
//     read low → high instead of low, max, high.
//
// The stream is Anthropic's SSE dialect, whose usage is split across two events
// (message_start carries input + cache, message_delta carries output), and whose
// cache fields are EXCLUSIVE of input_tokens.

// SetZCodeAccounts replaces the pool (boot, import).
func (l *Lane) SetZCodeAccounts(accts []ZCodeAccount) {
	l.zcMu.Lock()
	l.zcAccts = accts
	l.zcMu.Unlock()
	l.fireChange()
}

// ZCodeAccounts snapshots the pool.
func (l *Lane) ZCodeAccounts() []ZCodeAccount {
	l.zcMu.RLock()
	defer l.zcMu.RUnlock()
	out := make([]ZCodeAccount, len(l.zcAccts))
	copy(out, l.zcAccts)
	return out
}

// SetZCodePersist wires the durable store.
func (l *Lane) SetZCodePersist(fn func(ZCodeAccount)) {
	l.zcMu.Lock()
	l.zcPersist = fn
	l.zcMu.Unlock()
}

// zcodeUpsert folds a freshly minted account into the pool. One user logging in
// twice updates in place: a second row would double the apparent quota without
// adding any.
func (l *Lane) zcodeUpsert(acct ZCodeAccount) {
	if strings.TrimSpace(acct.ID) == "" {
		acct.ID = "zcode-" + acct.UserID
	}
	l.zcMu.Lock()
	replaced := false
	for i := range l.zcAccts {
		if l.zcAccts[i].ID == acct.ID {
			l.zcAccts[i] = acct
			replaced = true
			break
		}
	}
	if !replaced {
		l.zcAccts = append(l.zcAccts, acct)
	}
	persist := l.zcPersist
	l.zcMu.Unlock()
	if persist != nil {
		persist(acct)
	}
}

// ZCodeLogout removes one account.
func (l *Lane) ZCodeLogout(id string) {
	l.zcMu.Lock()
	kept := make([]ZCodeAccount, 0, len(l.zcAccts))
	for _, a := range l.zcAccts {
		if a.ID != id {
			kept = append(kept, a)
		}
	}
	l.zcAccts = kept
	l.zcMu.Unlock()
	l.fireChange()
}

// ZCodeSetEnabled flips one account's enable flag.
func (l *Lane) ZCodeSetEnabled(id string, enabled bool) bool {
	l.zcMu.Lock()
	var updated *ZCodeAccount
	for i := range l.zcAccts {
		if l.zcAccts[i].ID == id {
			l.zcAccts[i].Enabled = enabled
			cp := l.zcAccts[i]
			updated = &cp
			break
		}
	}
	persist := l.zcPersist
	l.zcMu.Unlock()
	if updated == nil {
		return false
	}
	if persist != nil {
		persist(*updated)
	}
	l.fireChange()
	return true
}

// zcodeSpendable returns the accounts a turn may use.
func (l *Lane) zcodeSpendable() []ZCodeAccount {
	all := l.ZCodeAccounts()
	out := make([]ZCodeAccount, 0, len(all))
	for _, a := range all {
		if a.Enabled && strings.TrimSpace(a.JWT) != "" && !zcodeParked(a.ID) {
			out = append(out, a)
		}
	}
	return out
}

// zcodeConfigRow is one model as the client-configs endpoint declares it.
type zcodeConfigRow struct {
	ModelID      string
	Name         string
	ContextWin   int
	MaxOutput    int
	Vision       bool
	Levels       []string
	DefaultLevel string
}

// zcodeFetchConfigs is the authoritative roster. A failure returns an error so
// the caller keeps the cached bucket (a blip must never empty a picker).
func zcodeFetchConfigs(ctx context.Context, acct ZCodeAccount) ([]zcodeConfigRow, error) {
	u := ZCodeOrigin + "/api/v1/client/configs?app_version=" + zcodeAppVersion + "&platform=" + zcodePlatform
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range zcodeHeaders(acct, false) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{Code: CodeServer,
			Message: "ZCode 模型目录 HTTP " + itoa(resp.StatusCode) + ": " + truncate(string(raw), 200)}
	}

	var parsed struct {
		Data struct {
			// An OBJECT keyed by index — see the file header.
			BuiltinModels map[string]struct {
				ModelID          string `json:"modelId"`
				ContextWindow    int    `json:"contextWindow"`
				MaxCompletionTok int    `json:"maxCompletionTokens"`
				Capabilities     struct {
					Vision bool `json:"vision"`
				} `json:"capabilities"`
				Reasoning struct {
					Levels       map[string]any `json:"levels"`
					DefaultLevel string         `json:"defaultLevel"`
				} `json:"reasoning"`
			} `json:"builtinModels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &UpstreamError{Code: CodeServer,
			Message: "ZCode 模型目录响应无法解析: " + truncate(string(raw), 200)}
	}

	rows := make([]zcodeConfigRow, 0, len(parsed.Data.BuiltinModels))
	for _, m := range parsed.Data.BuiltinModels {
		if strings.TrimSpace(m.ModelID) == "" {
			continue
		}
		row := zcodeConfigRow{
			ModelID:      m.ModelID,
			Name:         m.ModelID,
			ContextWin:   m.ContextWindow,
			MaxOutput:    m.MaxCompletionTok,
			Vision:       m.Capabilities.Vision,
			DefaultLevel: m.Reasoning.DefaultLevel,
		}
		if row.ContextWin <= 0 {
			row.ContextWin = 200000
		}
		if row.MaxOutput <= 0 {
			row.MaxOutput = 32768
		}
		for lv := range m.Reasoning.Levels {
			row.Levels = append(row.Levels, lv)
		}
		sortEffortLadder(row.Levels)
		rows = append(rows, row)
	}
	return rows, nil
}

// effortOrder is the thinking ladder the platform actually means. The map's own
// key order is not it.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// sortEffortLadder orders rungs low → high, keeping unknown rungs last in a
// stable relative order.
func sortEffortLadder(levels []string) {
	rank := map[string]int{}
	for i, v := range effortOrder {
		rank[v] = i
	}
	rankOf := func(s string) int {
		if r, ok := rank[strings.ToLower(s)]; ok {
			return r
		}
		return len(effortOrder)
	}
	for i := 1; i < len(levels); i++ {
		for j := i; j > 0 && rankOf(levels[j]) < rankOf(levels[j-1]); j-- {
			levels[j], levels[j-1] = levels[j-1], levels[j]
		}
	}
}

// zcodeBuildCatalog turns config rows into lane catalog entries.
func zcodeBuildCatalog(rows []zcodeConfigRow) []ModelInfo {
	out := make([]ModelInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelInfo{
			ID:                 r.ModelID,
			Name:               r.Name,
			Blurb:              "ZCode 账号渠道（智谱），额度按 token 计量；提示词可能被上游记录",
			Wire:               "messages",
			Channel:            ChannelZCode,
			Vision:             r.Vision,
			Reasoning:          len(r.Levels) > 0,
			ContextWindow:      r.ContextWin,
			MaxOutput:          r.MaxOutput,
			CanDisableThinking: hasRung(r.Levels, "none"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func hasRung(levels []string, want string) bool {
	for _, v := range levels {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// zcodeIdentitySystem is the official client's identity block. It is not
// cosmetic: a messages body without it draws 405 + code 3012, and 3012 is a
// cooling penalty on the ACCOUNT, not a per-request refusal. It travels as the
// first system block on every turn.
const zcodeIdentitySystem = `You are ZCode, a coding assistant made by Zhipu AI. You help with software engineering tasks: reading and writing code, debugging, refactoring, and explaining technical decisions. Answer directly, prefer concrete code over prose, and never invent APIs you have not seen in the user's files.`

// zcodeLevels holds model → accepted rungs, filled by the roster sync.
var zcodeLevels = map[string][]string{}

func modelLevels(model string) []string { return zcodeLevels[BaseModelId(model)] }

func setZcodeLevels(rows []zcodeConfigRow) {
	next := map[string][]string{}
	for _, r := range rows {
		next[r.ModelID] = r.Levels
	}
	zcodeLevels = next
}

// zcodeBuildBody converts a unified Anthropic-shaped body into the one this wire
// expects, including the two things the upstream actually reads:
//
//   - the identity block FIRST in `system`;
//   - the effort rung in `output_config.effort`, lower-cased. Sending
//     `reasoning_effort` here is silently ignored, and an upper-case rung is
//     silently dropped — both leave the picker looking broken with no error.
func zcodeBuildBody(base map[string]any, model, effort, callerSystem string) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		switch k {
		case "model", "stream", "anthropic_version", "system":
			continue
		}
		out[k] = v
	}
	out["model"] = model
	out["stream"] = true
	out["anthropic_version"] = "2023-06-01"

	var blocks []any
	blocks = append(blocks, map[string]any{"type": "text", "text": zcodeIdentitySystem})
	if s := strings.TrimSpace(callerSystem); s != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	out["system"] = blocks

	if effort != "" {
		low := strings.ToLower(strings.TrimSpace(effort))
		if hasRung(modelLevels(model), low) {
			out["output_config"] = map[string]any{"effort": low}
		}
	}
	return out
}

// zcodeChat sends one turn. The caller rotates accounts; this is one physical
// attempt.
func (l *Lane) zcodeChat(ctx context.Context, body map[string]any, acct ZCodeAccount, onData func([]byte) error) (*Usage, error) {
	model, _ := body["model"].(string)
	callerSystem, _ := body["system"].(string)
	built := zcodeBuildBody(body, model, effortOfBody(body), callerSystem)

	payload, err := json.Marshal(built)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	endpoint := ZCodeOrigin + "/api/v1/zcode-plan/anthropic/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range zcodeHeaders(acct, true) {
		req.Header.Set(k, v)
	}

	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, &UpstreamError{Code: classifyConnErr(err), Message: err.Error()}
	}
	defer resp.Body.Close()

	pump := newStreamReader(ctx, resp.Body, streamIdleTimeout)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest, _ := io.ReadAll(io.LimitReader(pump, 1<<20))
		return nil, zcodeClassify(resp.StatusCode, string(head)+string(rest))
	}
	// An HTML edge page must be caught BEFORE anything looks for a code inside
	// it: a marketing page containing the digits "1005" would otherwise bench a
	// healthy account until midnight.
	if isHTMLPage(string(head)) {
		return nil, &UpstreamError{Code: CodeServer, Message: "ZCode 返回了 HTML 页面（边缘节点）"}
	}

	pump.Unread(head)
	usage, err := readSSE(pump, onData)
	if err != nil {
		if ctx.Err() != nil {
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, ue
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	return usage, nil
}

// effortOfBody reads the rung the caller put on the body, if any.
func effortOfBody(body map[string]any) string {
	for _, k := range []string{"reasoning_effort", "effort"} {
		if v, ok := body[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// zcodeClassify maps a ZCode refusal onto the lane vocabulary.
//
// 3012 is handled first and never retried: it means "unusual activity", the
// platform parks the account for 30 minutes and escalates on a repeat, so
// retrying is how a short cooldown becomes a long ban.
func zcodeClassify(status int, payload string) *UpstreamError {
	snippet := truncate(payload, 300)

	var doc struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(payload), &doc)
	code := 0
	if f, ok := toNum(doc.Code); ok {
		code = int(f)
	}
	msg := firstNonEmptyStr(doc.Message, doc.Error.Message, snippet)

	switch code {
	case 3012:
		return &UpstreamError{Code: CodeCredential, Message: "ZCode 风控拦截（3012）：" + msg, Scope: ScopeAccount}
	case 1005, 1113:
		return &UpstreamError{Code: CodeQuota, Message: msg, Scope: ScopeAccount}
	case 3007, 3009:
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: 30, Scope: ScopeModel}
	case 1002:
		return &UpstreamError{Code: CodeCredential, Message: "ZCode 登录态失效（1002）：" + msg}
	}

	switch {
	case status == 401 || status == 403:
		return &UpstreamError{Code: CodeCredential, Message: "ZCode 登录态失效（HTTP " + itoa(status) + "）"}
	case status == 429:
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: 60}
	case status == 405:
		// The identity-block gate. Do not retry: the account is already cooling.
		return &UpstreamError{Code: CodeCredential, Message: "ZCode 拒绝请求形状（405，身份块缺失或被风控）：" + msg, Scope: ScopeAccount}
	case status >= 500:
		return &UpstreamError{Code: CodeServer, Message: msg}
	case status >= 400:
		return &UpstreamError{Code: CodeClient, Message: msg}
	}
	if strings.Contains(strings.ToLower(payload), "overloaded_error") {
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: 30}
	}
	return &UpstreamError{Code: CodeServer, Message: msg}
}
