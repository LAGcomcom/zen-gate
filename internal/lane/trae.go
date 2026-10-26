package lane

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The TRAE (字节) 账号车道: ByteDance's TRAE SOLO free tier, ported from the
// desktop client's own wire.
//
// Unlike every other lane in this package, TRAE is not OpenAI-shaped:
//
//   - the chat endpoint is /api/agent/v3/llm_utils_chat on the agent host
//     (trae-api-cn.mchost.guru), and the body carries the model TWICE
//     (config_name + model) plus a `function` naming which agent channel the
//     model may be called through;
//   - the stream is the SOLO SSE dialect (event:/data: lines, empty line
//     terminates an event), not `data:` frames;
//   - auth is a Cloud-IDE-JWT triple: the same token rides Authorization,
//     X-Cloudide-Token and X-Ide-Token, and the desktop identity (app id,
//     ide version, device type, OS) is part of the fingerprint.
//
// Two consequences shape this file:
//
//   - A model is only callable through the channel that listed it. Calling
//     glm-5.1 through solo_work_lite answers an in-stream code 4001 "param is
//     invalid", which reads like a bad request and is actually a routing
//     mistake — so the roster records each model's channel and the body uses
//     it, instead of guessing one function for everything.
//   - An in-stream error is still an HTTP 200. Every SOLO `error` event must be
//     classified and returned, or a refused turn looks like an empty answer.
//
// Prompts may be logged by the upstream; the blurb says so in front of the user.

// TraeAgentBase is the chat host; override for tests.
var TraeAgentBase = "https://trae-api-cn.mchost.guru"

// TraeUGBase is the credits/checkin host; override for tests.
var TraeUGBase = "https://api.trae.cn"

const (
	traeChatPath        = "/api/agent/v3/llm_utils_chat"
	traeBatchModelsPath = "/api/ide/v1/batch_get_detail_param"
	traeExchangePath    = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeUserInfoPath    = "/cloudide/api/v3/trae/GetUserInfo"

	traeEntUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"
	traeCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"

	// The desktop identity the upstream fingerprints. Version constants move
	// with the client, so they live here as one edit-point.
	traeAppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeIDEVersion     = "0.1.52"
	traeIDEVersionCode = "20260811"
	traeUserAgent      = "Trae/0.1.52"
	traeOSVersion      = "macOS 15.7.4"
	traeDeviceBrand    = "Apple"
	traeClientID       = "en1oxy7wnw8j9n"

	// traeDefaultModel is the fallback model and the value the client itself
	// falls back to when a config_name is empty.
	traeDefaultModel = "glm-5.2"
	// traeDefaultFunction is the channel used when a model's own channel is
	// unknown. solo_work_lite is the client's default and the widest channel.
	traeDefaultFunction = "solo_work_lite"
	// traeMaxTokensCap is the clamp the client applies to max_tokens.
	traeMaxTokensCap = 64000

	traeTurnTimeout  = streamIdleTimeout
	traeRefreshEarly = 20 * time.Minute
)

// traeChannels is the client's channel whitelist, in the order it probes them.
// It is not a request field: it decides which channels the roster asks about
// and in what order a model is looked up when one id appears in several.
var traeChannels = []string{
	"solo_agent", "solo_work_lite", "solo_agent_remote", "solo_work_remote",
	"solo_agent_lite", "chat_v3", "builder_v3", "solo_coder",
	"solo_design_lite", "solo_design_remote", "git_ai", "code_reviewer",
	"code_review_summary", "multimodal", "system_diagnosis",
}

// traeRoute is the per-model routing the body needs: the channel that listed
// it, plus the capacities that channel declared.
type traeRoute struct {
	Function   string
	ContextWin int
	MaxOutput  int
	Reasoning  bool
	Vision     bool
	Options    []string // reasoning_effort values the upstream accepts
}

var (
	traeRouteMu sync.RWMutex
	traeRoutes  = map[string]traeRoute{}
)

// traeRouteOf returns the recorded route for a model id.
func traeRouteOf(model string) (traeRoute, bool) {
	traeRouteMu.RLock()
	defer traeRouteMu.RUnlock()
	r, ok := traeRoutes[model]
	return r, ok
}

// setTraeRoutes replaces the whole table (one roster sync).
func setTraeRoutes(next map[string]traeRoute) {
	traeRouteMu.Lock()
	defer traeRouteMu.Unlock()
	traeRoutes = next
}

// TraeAccount is one TRAE login. It is the credential the desktop client mints
// through its own OAuth handoff, narrowed to what the wire needs.
type TraeAccount struct {
	ID           string `json:"id,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Phone        string `json:"phone,omitempty"`
	// EnterpriseID is the console's `TenantID` (that exact spelling). It
	// identifies a team seat; an empty value means a personal account. Kept
	// separately from UID because the two key different upstream scopes.
	EnterpriseID string `json:"enterpriseId,omitempty"`
	ExpiresAtMS  int64  `json:"expiresAtMs,omitempty"`
	// MachineID / DeviceID are the desktop fingerprint. Both must persist and
	// must differ per account: a shared device id makes the checkin endpoint
	// answer "this device already claimed today", and a regenerated machine id
	// makes the account look like a new device on every restart.
	MachineID string `json:"machineId,omitempty"`
	DeviceID  string `json:"deviceId,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// traeJWTExp reads the exp claim out of a JWT, in milliseconds.
func traeJWTExp(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		if payload, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(parts[1])); err != nil {
			return 0
		}
	}
	var claims struct {
		Exp any `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0
	}
	f, ok := toNum(claims.Exp)
	if !ok || f <= 0 {
		return 0
	}
	if f < 1e12 {
		return int64(f) * 1000
	}
	return int64(f)
}

// traeTokenNearExpiry is the proactive-refresh trigger.
func traeTokenNearExpiry(a TraeAccount) bool {
	exp := a.ExpiresAtMS
	if exp <= 0 {
		exp = traeJWTExp(a.AccessToken)
	}
	if exp <= 0 {
		return false // unknown lifetime: let an upstream 401 drive it
	}
	return time.Now().UnixMilli()+traeRefreshEarly.Milliseconds() >= exp
}

// traeHeaders assembles the Cloud-IDE-JWT identity set. The same token rides
// three headers; the upstream rejects a request missing any of them, and a
// duplicated key (two spellings of content-type) is answered with a 400 whose
// code reads as "param is invalid" rather than "bad header".
func traeHeaders(a TraeAccount, accept string) map[string]string {
	h := map[string]string{
		"Content-Type":         "application/json",
		"Accept":               accept,
		"User-Agent":           traeUserAgent,
		"Authorization":        "Cloud-IDE-JWT " + a.AccessToken,
		"X-Cloudide-Token":     a.AccessToken,
		"X-Ide-Token":          a.AccessToken,
		"X-Uid":                a.UID,
		"X-App-Id":             traeAppID,
		"X-App-Version":        "default",
		"X-Ide-Version":        traeIDEVersion,
		"X-Ide-Version-Code":   traeIDEVersionCode,
		"X-App-Version-Code":   traeIDEVersionCode,
		"X-Ide-Version-Type":   "stable",
		"X-Device-Type":        "macos",
		"X-OS-Version":         traeOSVersion,
		"X-Device-Brand":       traeDeviceBrand,
		"Request-Traffic-Type": "prod",
	}
	if a.MachineID != "" {
		h["X-Machine-Id"] = a.MachineID
	}
	if a.DeviceID != "" {
		h["X-Device-Id"] = a.DeviceID
	}
	return h
}

// traeBuildBody converts a unified OpenAI-shaped body into the SOLO body.
//
// The two rewrites that are load-bearing:
//
//   - `parameters` inside each tool must be a JSON *string*; the upstream
//     refuses an object schema with code 4001.
//   - an assistant's tool_calls key is `function_call`, and an entry without a
//     name must be dropped outright — an empty name poisons the stored
//     conversation so that every later turn of it is refused.
func traeBuildBody(model string, base map[string]any) map[string]any {
	route, _ := traeRouteOf(model)

	fn := route.Function
	if fn == "" {
		fn = traeDefaultFunction
	}
	cfgName := BaseModelId(model)
	if cfgName == "" {
		cfgName = traeDefaultModel
	}

	out := map[string]any{
		"model":       cfgName,
		"config_name": cfgName,
		"function":    fn,
		"stream":      true,
	}

	for k, v := range base {
		switch k {
		case "model", "stream":
			continue // already set, and stream is forced
		}
		out[k] = v
	}

	// tools: parameters must travel as a string.
	if tools, ok := out["tools"].([]any); ok {
		rewritten := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fnPart, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			clean := map[string]any{"name": jsonString(fnPart["name"])}
			if d := jsonString(fnPart["description"]); d != "" {
				clean["description"] = d
			}
			if p, ok := fnPart["parameters"]; ok {
				if b, err := json.Marshal(p); err == nil {
					clean["parameters"] = string(b)
				}
			}
			rewritten = append(rewritten, map[string]any{"type": "function", "function": clean})
		}
		if len(rewritten) == 0 {
			delete(out, "tools")
		} else {
			out["tools"] = rewritten
		}
	}

	// max_tokens is clamped: the client never sends more than 64k.
	if v, ok := out["max_tokens"]; ok {
		if f, ok := toNum(v); ok && f > traeMaxTokensCap {
			out["max_tokens"] = traeMaxTokensCap
		}
	}

	// messages: assistant tool_calls → function_call, nameless entries dropped.
	if msgs, ok := out["messages"].([]any); ok {
		for _, raw := range msgs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if jsonString(m["role"]) != RoleAssistant {
				continue
			}
			calls, ok := m["tool_calls"].([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(calls))
			for _, c := range calls {
				call, ok := c.(map[string]any)
				if !ok {
					continue
				}
				cfn, ok := call["function"].(map[string]any)
				if !ok || jsonString(cfn["name"]) == "" {
					continue // a nameless call must not reach the upstream
				}
				kept = append(kept, map[string]any{
					"id":   jsonString(call["id"]),
					"type": "function",
					"function_call": map[string]any{
						"name":      jsonString(cfn["name"]),
						"arguments": jsonString(cfn["arguments"]),
					},
				})
			}
			delete(m, "tool_calls")
			if len(kept) > 0 {
				m["function_call"] = kept
			}
		}
	}

	return out
}

// traeUsageFrom converts SOLO token_usage into the lane's Usage. TRAE's
// prompt_tokens is INCLUSIVE of cache hits, so the fresh count is a subtraction
// clamped at zero — reporting the raw number would tell the client it paid for
// tokens it did not.
func traeUsageFrom(m map[string]any) *Usage {
	u := &Usage{CacheInclusive: true}
	got := false
	got = num(&u.Input, m, "prompt_tokens") || got
	got = num(&u.Output, m, "completion_tokens") || got
	got = num(&u.CacheRead, m, "cache_read_input_tokens") || got
	got = num(&u.CacheWrite, m, "cache_creation_input_tokens") || got
	got = num(&u.TotalTokens, m, "total_tokens") || got
	if rv, ok := m["reasoning_tokens"]; ok {
		if f, ok := toNum(rv); ok && f >= 0 {
			u.Reasoning = int(f)
			got = true
		}
	}
	if !got {
		return nil
	}
	return u
}

// traeEvent is one parsed SOLO SSE event.
type traeEvent struct {
	Name string
	Data map[string]any
}

// readTraeSSE decodes the SOLO dialect: `event:` and `data:` lines accumulate
// until a blank line closes the event, and `:` starts a comment. A frame whose
// data spans several lines is joined before parsing.
func readTraeSSE(src io.Reader, onEvent func(traeEvent) error) (*Usage, error) {
	br := bufio.NewReaderSize(src, 64*1024)
	var usage *Usage
	var name string
	var data []string
	flush := func() error {
		if len(data) == 0 && name == "" {
			return nil
		}
		ev := traeEvent{Name: name}
		if len(data) > 0 {
			joined := strings.Join(data, "\n")
			var m map[string]any
			if json.Unmarshal([]byte(joined), &m) == nil {
				ev.Data = m
			}
		}
		name, data = "", nil
		if ev.Data != nil && ev.Name == "token_usage" {
			if u := traeUsageFrom(ev.Data); u != nil {
				if usage == nil {
					usage = u
				} else {
					usage.MergeMax(u)
				}
			}
		}
		return onEvent(ev)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if ferr := flush(); ferr != nil {
					return usage, ferr
				}
				return usage, nil
			}
			return usage, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if ferr := flush(); ferr != nil {
				return usage, ferr
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			continue // comment
		}
		if v, ok := strings.CutPrefix(trimmed, "event:"); ok {
			name = strings.TrimSpace(v)
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "data:"); ok {
			data = append(data, strings.TrimSpace(v))
		}
	}
}

// traeClassifyEvent turns one SOLO error event into the lane's vocabulary.
func traeClassifyEvent(code int, message string) *UpstreamError {
	msg := truncate(strings.TrimSpace(message), 300)
	if msg == "" {
		msg = "TRAE 返回 code " + itoa(code)
	}
	switch code {
	case 4006:
		return &UpstreamError{Code: CodeClient, Message: "上下文超出模型容量：" + msg}
	case 1005:
		// The plan does not include this model. Deterministic for the day.
		return &UpstreamError{Code: CodeQuota, Message: msg, Scope: ScopeModel, RetryAfter: 12 * 3600}
	case 4008:
		// ide_credits exhausted for the day.
		return &UpstreamError{Code: CodeQuota, Message: msg, Scope: ScopeAccount, RetryAfter: 24 * 3600}
	case 4011:
		return &UpstreamError{Code: CodeQuota, Message: msg, Scope: ScopeModel, RetryAfter: 60}
	case 4001, 4023, 4027:
		// 4001 = wrong channel / custom model / duplicated header;
		// 4023 = model not on this channel; 4027 = tool pairing defect.
		// All three are request defects: another account answers identically.
		return &UpstreamError{Code: CodeClient, Message: msg}
	case 9004:
		return &UpstreamError{Code: CodeCredential, Message: "TRAE 凭据缺 device_id：" + msg}
	case 3003:
		return &UpstreamError{Code: CodeServer, Message: msg}
	default:
		if code >= 500 {
			return &UpstreamError{Code: CodeServer, Message: msg}
		}
		return &UpstreamError{Code: CodeClient, Message: msg}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
