package lane

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Preset "OpenAI-compatible + private headers" channels.
//
// Six of the upstreams this gateway serves speak the SAME wire — POST a chat
// body, read `data:` frames — and differ only in three things:
//
//   - the base URL and the path;
//   - a handful of identity/capability headers (loomy wants its token in a
//     lowercase `token` header as well as Authorization; lobsterai refuses to
//     list its full roster unless a capability header names the features it
//     supports; raccoon wants an org code and a language);
//   - a body quirk each one needs to behave at all (an effort rung that must be
//     one of ITS names, a usage flag, a thinking object in a private envelope).
//
// Writing six near-identical transports would mean six places to fix the same
// SSE bug. So the shared shape lives here once, and each preset is a declaration
// of those three differences.
//
// Anything that does NOT fit this shape has its own file: ZCode (Anthropic
// wire), TRAE (SOLO events), minimax (Anthropic wire), accio (Gemini variant),
// codely and autoclaw (request signing).

// The preset channel ids. They double as `channels/<id>.json` file names and
// catalog Channel tags.
const (
	ChannelCline      = "cline"
	ChannelLoomy      = "loomy"
	ChannelRaccoon    = "raccoon"
	ChannelLobsterAI  = "lobsterai"
	ChannelAStudio    = "astudio"
	ChannelModelScope = "modelscope"
)

// Upstream roots, as vars so tests can point them at one stub server.
var (
	ExtClineBase      = "https://api.cline.bot"
	ExtLoomyBase      = "https://loomyad.xunfei.cn"
	ExtRaccoonBase    = "https://xiaohuanxiong.com"
	ExtLobsterBase    = "https://lobsterai-server.youdao.com"
	ExtAStudioBase    = "https://astronstudio-api-volces-prod.xf-yun.com"
	ExtModelScopeBase = "https://api-inference.modelscope.cn"
)

// ExtAccount is one preset-channel login. Secret is whatever the vendor hands
// the user (a JWT, a session string, an API key); Extra carries the identity
// fields a particular preset needs on the wire, so one struct serves all six
// without six near-identical types.
type ExtAccount struct {
	Kind    string            `json:"kind"`
	ID      string            `json:"id"`
	Label   string            `json:"label,omitempty"`
	Secret  string            `json:"secret,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
	Enabled bool              `json:"enabled"`
	AddedAt int64             `json:"addedAt,omitempty"`
}

// extModel is one roster row before it becomes a catalog entry.
type extModel struct {
	ID         string
	Name       string
	ContextWin int
	MaxOutput  int
	Vision     bool
	Reasoning  bool
}

// extPreset declares one preset channel.
type extPreset struct {
	Kind  string
	Label string
	Blurb string
	// ChatURL / CatalogURL are absolute bases; the preset owns its path because
	// the six do not share one.
	ChatURL    string
	CatalogURL string
	// AuthProbeURL, when set, is an endpoint that actually validates the
	// credential. It exists because some catalogs are public: Cline answers
	// /api/v1/models 200 for any bearer and ModelScope's roster endpoint is
	// open, so "the catalog listed models" proves nothing about the token.
	// Probing the identity endpoint is the difference between a card that looks
	// healthy and one that can serve a turn.
	AuthProbeURL string
	// Headers builds the chat request headers. stream decides Accept.
	Headers func(a ExtAccount, stream bool) map[string]string
	// CatalogHeaders builds the catalog headers; nil means "same as chat".
	CatalogHeaders func(a ExtAccount) map[string]string
	// FixBody applies the body quirk this preset needs to behave at all.
	FixBody func(body map[string]any, effort string)
	// Extract pulls ids (and any capacities) out of the catalog response.
	Extract func(root map[string]any) []extModel
}

// extPresets is the table. Order is stable for the dashboard.
var extPresets = map[string]*extPreset{
	ChannelCline: {
		Kind:       ChannelCline,
		Label:      "Cline",
		Blurb:      "Cline 账号渠道（WorkOS 登录），每日免费额度按模型计；提示词可能被上游记录",
		ChatURL:    ExtClineBase + "/api/v1/chat/completions",
		CatalogURL: ExtClineBase + "/api/v1/models",
		// The roster is public, so the token has to be proved elsewhere: this is
		// the identity endpoint the desktop client uses, and it is the only
		// call that answers 401 for a bad (or prefix-stripped) credential.
		AuthProbeURL: ExtClineBase + "/api/v1/users/me",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			// The `workos:` prefix is part of the credential, not decoration:
			// stripping it turns a valid login into a 401.
			return map[string]string{
				"Authorization":  "Bearer " + a.Secret,
				"Accept":         accept,
				"Content-Type":   "application/json",
				"HTTP-Referer":   "https://cline.bot",
				"X-Title":        "Cline",
				"X-IS-MULTIROOT": "false",
				"X-CLIENT-TYPE":  "cline-sdk",
			}
		},
		FixBody: func(body map[string]any, effort string) {
			// A missing max_tokens makes the gateway cap output at 4096 and stop
			// mid-answer with finish_reason=length — no error, just a truncated
			// answer, so it must always be present.
			setIntClamped(body, "max_tokens", 64000, 943718)
			if rung := extRung(effort, map[string]string{
				"light": "low", "balanced": "medium", "deep": "high",
			}); rung != "" {
				body["reasoning_effort"] = rung
			}
		},
		Extract: func(root map[string]any) []extModel {
			return extCollect(root, []string{"data"}, "id")
		},
	},

	ChannelLoomy: {
		Kind:       ChannelLoomy,
		Label:      "Loomy (讯飞)",
		Blurb:      "Loomy 账号渠道（讯飞），每日赠送额度当日有效；提示词可能被上游记录",
		ChatURL:    ExtLoomyBase + "/api/v1/chat/completions",
		CatalogURL: ExtLoomyBase + "/api/v1/models",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			// Both headers carry the same session. The business endpoints only
			// honour the lowercase `token` one; a Bearer-only request answers
			// HTTP 200 + code 100002 ("missing token"), which reads as an empty
			// roster rather than an auth failure.
			return map[string]string{
				"Authorization": "Bearer " + a.Secret,
				"token":         a.Secret,
				"Content-Type":  "application/json",
				"Accept":        accept,
			}
		},
		FixBody: func(body map[string]any, effort string) {
			setIntClamped(body, "max_tokens", 8192, 64000)
			if rung := extRung(effort, map[string]string{
				"light": "low", "balanced": "medium", "deep": "high",
			}); rung != "" {
				body["reasoning_effort"] = rung
			}
		},
		Extract: func(root map[string]any) []extModel {
			rows := extCollect(root, []string{"data"}, "id")
			if len(rows) == 0 {
				rows = extCollectArray(root, "id")
			}
			return rows
		},
	},

	ChannelRaccoon: {
		Kind:       ChannelRaccoon,
		Label:      "Raccoon (商汤)",
		Blurb:      "Raccoon 账号渠道（商汤），每日积分服务端自动发放；提示词可能被上游记录",
		ChatURL:    ExtRaccoonBase + "/api/web/llm/v2/chat/completions",
		CatalogURL: ExtRaccoonBase + "/api/web/llm/v2/model_catalog",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			h := map[string]string{
				"Authorization":      "Bearer " + a.Secret,
				"X-Raccoon-Language": "zh",
				"X-Client-Platform":  "desktop-windows",
				"Content-Type":       "application/json",
				"Accept":             accept,
			}
			if org := a.Extra["officeIdentity"]; org != "" {
				h["X-Org-Code"] = org
			}
			return h
		},
		FixBody: func(body map[string]any, effort string) {
			setIntClamped(body, "max_tokens", 8192, 64000)
			// The ONLY working thinking switch is this private envelope. A
			// top-level reasoning_effort is accepted by the upstream and has no
			// effect at all, so passing it would silently disable the picker.
			mode := "disabled"
			if effort != "" && !strings.EqualFold(effort, "none") && !strings.EqualFold(effort, "off") {
				mode = "enabled"
			}
			body["extra_body"] = map[string]any{
				"thinking": map[string]any{"type": mode},
			}
		},
		Extract: func(root map[string]any) []extModel {
			var out []extModel
			data, _ := root["data"].(map[string]any)
			cats, _ := data["categories"].([]any)
			for _, rawCat := range cats {
				cat, ok := rawCat.(map[string]any)
				if !ok {
					continue
				}
				if t := jsonString(cat["type"]); t != "" && t != "chat" {
					continue
				}
				models, _ := cat["models"].([]any)
				for _, rawM := range models {
					m, ok := rawM.(map[string]any)
					if !ok {
						continue
					}
					// The model id on this wire is the `name` field, not `id`.
					id := jsonString(m["name"])
					if id == "" {
						continue
					}
					if v, ok := m["visible"].(bool); ok && !v {
						continue
					}
					row := extModel{ID: id, Name: id}
					if params, ok := m["params"].(map[string]any); ok {
						if f, ok := toNum(params["context_window"]); ok {
							row.ContextWin = int(f)
						}
						if f, ok := toNum(params["max_tokens"]); ok {
							row.MaxOutput = int(f)
						}
					}
					out = append(out, row)
				}
			}
			return out
		},
	},

	ChannelLobsterAI: {
		Kind:       ChannelLobsterAI,
		Label:      "LobsterAI (有道)",
		Blurb:      "LobsterAI 账号渠道（有道），积分按天与包计；提示词可能被上游记录",
		ChatURL:    ExtLobsterBase + "/api/proxy/v1/chat/completions",
		CatalogURL: ExtLobsterBase + "/api/models/available",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream, application/json"
			}
			ver := a.Extra["version"]
			if ver == "" {
				ver = "2026.9.4"
			}
			// The capability header is load-bearing: without it the roster call
			// silently returns a truncated model set (and `reasoning_effort:"off"`
			// draws a 500), which looks like "this account only has old models".
			return map[string]string{
				"Authorization":                   "Bearer " + a.Secret,
				"Accept":                          accept,
				"Content-Type":                    "application/json",
				"User-Agent":                      "LobsterAI/0.1.0",
				"X-LobsterAI-Client-Capabilities": "kimi-k3-agentic-v1,thinking-level-control-v1",
				"X-LobsterAI-Client-Version":      ver,
			}
		},
		CatalogHeaders: func(a ExtAccount) map[string]string {
			// The roster endpoint wants the same identity as chat, but asks for
			// JSON rather than a stream. It is spelled out here instead of
			// calling back into the table: a table entry that reads the table
			// while the table is being built is an initialization cycle.
			ver := a.Extra["version"]
			if ver == "" {
				ver = "2026.9.4"
			}
			return map[string]string{
				"Authorization":                   "Bearer " + a.Secret,
				"Accept":                          "application/json",
				"Content-Type":                    "application/json",
				"User-Agent":                      "LobsterAI/0.1.0",
				"X-LobsterAI-Client-Capabilities": "kimi-k3-agentic-v1,thinking-level-control-v1",
				"X-LobsterAI-Client-Version":      ver,
			}
		},
		FixBody: func(body map[string]any, effort string) {
			setIntClamped(body, "max_tokens", 8192, 64000)
			if rung := extRung(effort, map[string]string{
				"light": "low", "balanced": "medium", "deep": "high",
			}); rung != "" {
				body["reasoning_effort"] = rung
			}
		},
		Extract: func(root map[string]any) []extModel {
			// This endpoint is the only one that does NOT wrap in a
			// {code,msg,data} envelope — its data is a bare array — so it must
			// not go through the envelope reader.
			var out []extModel
			var list []any
			switch d := root["data"].(type) {
			case []any:
				list = d
			case map[string]any:
				list, _ = d["data"].([]any)
			}
			for _, raw := range list {
				m, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id := firstNonEmptyStr(jsonString(m["modelId"]), jsonString(m["id"]))
				if id == "" {
					continue
				}
				row := extModel{ID: id, Name: firstNonEmptyStr(jsonString(m["name"]), id)}
				if f, ok := toNum(m["contextWindow"]); ok {
					row.ContextWin = int(f)
				}
				if f, ok := toNum(m["maxTokens"]); ok {
					row.MaxOutput = int(f)
				}
				if v, ok := m["supportsImage"].(bool); ok {
					row.Vision = v
				}
				if v, ok := m["supportsThinking"].(bool); ok {
					row.Reasoning = v
				}
				out = append(out, row)
			}
			return out
		},
	},

	ChannelAStudio: {
		Kind:       ChannelAStudio,
		Label:      "AStudio (讯飞星火)",
		Blurb:      "AStudio 账号渠道（讯飞星火），积分按池计并带到期；提示词可能被上游记录",
		ChatURL:    ExtAStudioBase + "/model-gateway/v1/chat/completions",
		CatalogURL: ExtAStudioBase + "/api/v1/model-manager/models",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			h := map[string]string{
				"Authorization": "Bearer " + a.Secret,
				"Accept":        accept,
				"Content-Type":  "application/json",
				"clientType":    "21",
				"studioVersion": "3.5.2",
			}
			if acc := a.Extra["accountId"]; acc != "" {
				h["X-Account-Id"] = acc
			}
			return h
		},
		FixBody: func(body map[string]any, effort string) {
			// Without include_usage the stream carries NO usage frame at all:
			// billing and statistics silently lose every turn.
			body["stream_options"] = map[string]any{"include_usage": true}
			setIntClamped(body, "max_tokens", 8192, 128000)
			if rung := extRung(effort, map[string]string{
				"light": "low", "balanced": "medium", "deep": "high",
			}); rung != "" {
				body["reasoning_effort"] = rung
			}
		},
		Extract: func(root map[string]any) []extModel {
			// Chat wants a bare slug; a namespaced id answers 502 from a
			// downstream node, so only `slug` is a usable id here.
			rows := extCollect(root, []string{"data", "models"}, "slug")
			out := make([]extModel, 0, len(rows))
			for _, r := range rows {
				out = append(out, r)
			}
			return out
		},
	},

	ChannelModelScope: {
		Kind:       ChannelModelScope,
		Label:      "ModelScope (魔搭)",
		Blurb:      "ModelScope 账号渠道（魔搭），每日赠送额度按魔粒计；提示词可能被上游记录",
		ChatURL:    ExtModelScopeBase + "/v1/chat/completions",
		CatalogURL: ExtModelScopeBase + "/v1/models",
		// Same problem as Cline: /v1/models is open, so any string lists a
		// roster. The 魔粒 balance endpoint validates the key and lives on a
		// different host under /openapi/v1 — a full URL here on purpose, since
		// concatenating it onto the base yields a 404.
		AuthProbeURL: "https://www.modelscope.cn/openapi/v1/magicubes/balance",
		Headers: func(a ExtAccount, stream bool) map[string]string {
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			return map[string]string{
				"content-type":  "application/json",
				"accept":        accept,
				"authorization": "Bearer " + a.Secret,
			}
		},
		FixBody: func(body map[string]any, effort string) {
			setIntClamped(body, "max_tokens", 8192, 64000)
			// tools travels even when empty: this upstream validates the key's
			// presence, and a flat schema without the {type,function} wrapper
			// draws a 400 on every real turn (which is every turn, since the
			// agent always declares tools).
			if _, ok := body["tools"]; !ok {
				body["tools"] = []any{}
			}
			if rung := extRung(effort, map[string]string{
				"light": "low", "balanced": "medium", "deep": "high",
			}); rung != "" {
				body["reasoning_effort"] = rung
			}
		},
		Extract: func(root map[string]any) []extModel {
			return extCollect(root, []string{"data"}, "id")
		},
	},
}

// extKindOrder is the stable display order of the preset channels.
var extKindOrder = []string{
	ChannelCline, ChannelLoomy, ChannelRaccoon,
	ChannelLobsterAI, ChannelAStudio, ChannelModelScope,
}

// ExtKinds lists the preset channel ids in display order.
func ExtKinds() []string {
	out := make([]string, len(extKindOrder))
	copy(out, extKindOrder)
	return out
}

// Channels lists every channel this gateway serves, in display order.
//
// This is the single source of truth for "which channels exist": the store
// materializes one channels/<id>.json per entry, the dashboard wall renders one
// card per entry, and the catalog publishes each one's roster into its own
// file. Adding a channel anywhere else (a card with no channel file, a file
// with no card) is how the two views drift apart, so they all read this list.
func Channels() []string {
	out := make([]string, 0, 6+len(extKindOrder))
	out = append(out, ChannelZen, ChannelKilo, ChannelWorkBuddy,
		ChannelCodeArts, ChannelZCode, ChannelTrae)
	return append(out, extKindOrder...)
}

// ExtPresetLabel returns a preset's display name ("" for a non-preset).
func ExtPresetLabel(kind string) string {
	if p, ok := extPresets[kind]; ok {
		return p.Label
	}
	return ""
}

// ExtPreset exposes one preset's declarative facts (label, blurb) to callers
// outside this package. Read-only: the wire details stay private, because a
// caller that can rewrite a preset's URL can make a channel post a user's
// credential somewhere else.
func ExtPreset(kind string) (struct{ Label, Blurb string }, bool) {
	p, ok := extPresets[kind]
	if !ok {
		return struct{ Label, Blurb string }{}, false
	}
	return struct{ Label, Blurb string }{Label: p.Label, Blurb: p.Blurb}, true
}

// extRung maps the gateway's three rungs onto a vendor's own names.
func extRung(effort string, table map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return ""
	case "light", "low", "minimal":
		return table["light"]
	case "balanced", "medium":
		return table["balanced"]
	case "deep", "high", "xhigh", "max", "maximum", "highest":
		return table["deep"]
	}
	return ""
}

// setIntClamped writes a numeric field, defaulting when absent and clamping to
// the vendor's ceiling.
func setIntClamped(body map[string]any, key string, def, max int) {
	v := def
	if cur, ok := toNum(body[key]); ok && cur > 0 {
		v = int(cur)
	}
	if v > max {
		v = max
	}
	body[key] = v
}

// extCollect walks a nested path and collects every object carrying idField.
// A list may sit at any depth, so this recurses rather than assuming one shape;
// what it never does is invent rows — an object without the id field is skipped.
func extCollect(root map[string]any, path []string, idField string) []extModel {
	var node any = root
	for _, seg := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[seg]
	}
	var out []extModel
	extWalkIDs(node, idField, &out)
	return dedupeExt(out)
}

// extCollectArray walks the root as if it were the list itself.
func extCollectArray(root map[string]any, idField string) []extModel {
	var out []extModel
	extWalkIDs(root, idField, &out)
	return dedupeExt(out)
}

func extWalkIDs(node any, idField string, out *[]extModel) {
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			extWalkIDs(item, idField, out)
		}
	case map[string]any:
		if id := jsonString(v[idField]); id != "" {
			row := extModel{ID: id, Name: firstNonEmptyStr(jsonString(v["name"]), jsonString(v["display_name"]), id)}
			if f, ok := toNum(v["contextWindow"]); ok {
				row.ContextWin = int(f)
			}
			if f, ok := toNum(v["context_window"]); ok && row.ContextWin == 0 {
				row.ContextWin = int(f)
			}
			if f, ok := toNum(v["maxOutput"]); ok {
				row.MaxOutput = int(f)
			}
			if f, ok := toNum(v["max_output_tokens"]); ok && row.MaxOutput == 0 {
				row.MaxOutput = int(f)
			}
			if b, ok := v["vision"].(bool); ok {
				row.Vision = b
			}
			if b, ok := v["reasoning"].(bool); ok {
				row.Reasoning = b
			}
			*out = append(*out, row)
			return
		}
		// No id at this level: keep looking inside, since wrappers nest.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			extWalkIDs(v[k], idField, out)
		}
	}
}

func dedupeExt(rows []extModel) []extModel {
	seen := map[string]bool{}
	out := make([]extModel, 0, len(rows))
	for _, r := range rows {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- pool ------------------------------------------------------------------

// SetExtAccounts replaces the whole preset-channel pool.
func (l *Lane) SetExtAccounts(accts []ExtAccount) {
	l.extMu.Lock()
	l.extAccts = accts
	l.extMu.Unlock()
	l.fireChange()
}

// ExtAccounts snapshots the pool, optionally narrowed to one kind.
func (l *Lane) ExtAccounts(kind string) []ExtAccount {
	l.extMu.RLock()
	defer l.extMu.RUnlock()
	out := make([]ExtAccount, 0, len(l.extAccts))
	for _, a := range l.extAccts {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// SetExtPersist wires the durable store.
func (l *Lane) SetExtPersist(fn func(ExtAccount)) {
	l.extMu.Lock()
	l.extPersist = fn
	l.extMu.Unlock()
}

// extUpsert adds or replaces one account, keyed by (kind, id).
func (l *Lane) extUpsert(acct ExtAccount) {
	l.extMu.Lock()
	replaced := false
	for i := range l.extAccts {
		if l.extAccts[i].Kind == acct.Kind && l.extAccts[i].ID == acct.ID {
			l.extAccts[i] = acct
			replaced = true
			break
		}
	}
	if !replaced {
		l.extAccts = append(l.extAccts, acct)
	}
	persist := l.extPersist
	l.extMu.Unlock()
	if persist != nil {
		persist(acct)
	}
	l.fireChange()
}

// ExtRemove drops one account.
func (l *Lane) ExtRemove(kind, id string) {
	l.extMu.Lock()
	kept := make([]ExtAccount, 0, len(l.extAccts))
	for _, a := range l.extAccts {
		if !(a.Kind == kind && a.ID == id) {
			kept = append(kept, a)
		}
	}
	l.extAccts = kept
	l.extMu.Unlock()
	l.fireChange()
}

// ExtSetEnabled flips one account.
func (l *Lane) ExtSetEnabled(kind, id string, enabled bool) bool {
	l.extMu.Lock()
	var updated *ExtAccount
	for i := range l.extAccts {
		if l.extAccts[i].Kind == kind && l.extAccts[i].ID == id {
			l.extAccts[i].Enabled = enabled
			cp := l.extAccts[i]
			updated = &cp
			break
		}
	}
	persist := l.extPersist
	l.extMu.Unlock()
	if updated == nil {
		return false
	}
	if persist != nil {
		persist(*updated)
	}
	l.fireChange()
	return true
}

// extSpendable lists the usable accounts of one kind.
func (l *Lane) extSpendable(kind string) []ExtAccount {
	out := []ExtAccount{}
	for _, a := range l.ExtAccounts(kind) {
		if a.Enabled && strings.TrimSpace(a.Secret) != "" {
			out = append(out, a)
		}
	}
	return out
}

// ExtAddLogin stores a pasted credential and verifies it against the roster.
//
// Verification is the honest gate: a token that cannot list models cannot serve
// a turn either, and reporting that at add time is far better than letting the
// card look healthy until the first request fails.
func (l *Lane) ExtAddLogin(kind, label, secret string, extra map[string]string) (ExtAccount, error) {
	p, ok := extPresets[kind]
	if !ok {
		return ExtAccount{}, &UpstreamError{Code: CodeClient, Message: "未知的渠道：" + kind}
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ExtAccount{}, &UpstreamError{Code: CodeClient, Message: "凭据不能为空"}
	}
	acct := ExtAccount{
		Kind: kind, Secret: secret, Extra: extra,
		Enabled: true, AddedAt: time.Now().UnixMilli(),
		Label: strings.TrimSpace(label),
	}
	if acct.Label == "" {
		acct.Label = kind + "-" + shortFingerprint(secret)
	}
	acct.ID = kind + "-" + shortFingerprint(secret)

	if _, err := extFetchCatalog(context.Background(), p, acct); err != nil {
		return acct, err
	}
	if err := extProbeAuth(context.Background(), p, acct); err != nil {
		return acct, err
	}
	l.extUpsert(acct)
	go l.RefreshCatalog(context.Background())
	return acct, nil
}

// extProbeAuth proves the credential on an endpoint that validates it, for the
// presets whose catalog is public. A catalog that lists models for any bearer
// (Cline, ModelScope) would otherwise accept a typo, a truncated paste, or a
// token from the wrong field as a working login — the card turns green and the
// first real turn is the first failure.
//
// Presets without AuthProbeURL skip this; their catalog already answers 401.
func extProbeAuth(ctx context.Context, p *extPreset, acct ExtAccount) error {
	if p.AuthProbeURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.AuthProbeURL, nil)
	if err != nil {
		return err
	}
	for k, v := range p.Headers(acct, false) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		hint := ""
		if p.Kind == ChannelCline {
			hint = "（Cline 的凭据必须以 workos: 开头）"
		}
		if p.Kind == ChannelModelScope {
			hint = "（要粘贴 ModelScope 的 API Key，不是网页会话 cookie）"
		}
		return &UpstreamError{Code: CodeCredential,
			Message: p.Label + " 凭据被上游拒绝（HTTP " + itoa(resp.StatusCode) + "）" + hint}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &UpstreamError{Code: codeForHTTP(resp.StatusCode),
			Message: p.Label + " 凭据校验 HTTP " + itoa(resp.StatusCode)}
	}
	return nil
}

// shortFingerprint derives a stable, non-secret account handle.
func shortFingerprint(secret string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:4])
}

// extFetchCatalog pulls one preset's roster from one account.
func extFetchCatalog(ctx context.Context, p *extPreset, acct ExtAccount) ([]ModelInfo, error) {
	headers := func(a ExtAccount, stream bool) map[string]string { return p.Headers(a, stream) }
	if p.CatalogHeaders != nil {
		ch := p.CatalogHeaders
		headers = func(a ExtAccount, _ bool) map[string]string { return ch(a) }
	}
	catalogURL := p.CatalogURL
	// LobsterAI's roster endpoint takes the account identity in the query.
	if p.Kind == ChannelLobsterAI {
		q := url.Values{}
		for _, k := range []string{"firstKeyfrom", "latestKeyfrom", "version", "uuid", "userId"} {
			if v := acct.Extra[k]; v != "" {
				q.Set(k, v)
			}
		}
		if !q.Has("version") {
			q.Set("version", "2026.9.4")
		}
		if enc := q.Encode(); enc != "" {
			catalogURL += "?" + enc
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers(acct, false) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UpstreamError{Code: codeForHTTP(resp.StatusCode),
			Message: p.Label + " 模型目录 HTTP " + itoa(resp.StatusCode) + "：" + truncate(string(raw), 200)}
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil, &UpstreamError{Code: CodeServer,
			Message: p.Label + " 模型目录响应不是 JSON：" + truncate(string(raw), 200)}
	}
	rows := p.Extract(root)
	if len(rows) == 0 {
		return nil, &UpstreamError{Code: CodeClient,
			Message: p.Label + " 模型目录为空（凭据可能无效）"}
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelInfo{
			ID: r.ID, Name: r.Name, Blurb: p.Blurb,
			Wire: "chat", Channel: p.Kind,
			Vision: r.Vision, Reasoning: r.Reasoning,
			ContextWindow: r.ContextWin, MaxOutput: r.MaxOutput,
			CanDisableThinking: r.Reasoning,
		})
	}
	return out, nil
}

// extChat is one physical turn on a preset channel.
func (l *Lane) extChat(ctx context.Context, p *extPreset, body map[string]any, effort string, acct ExtAccount, onData func([]byte) error) (*Usage, error) {
	// The body the caller built is OpenAI-shaped already; the preset only adds
	// its own quirk.
	built := make(map[string]any, len(body)+2)
	for k, v := range body {
		built[k] = v
	}
	if p.FixBody != nil {
		p.FixBody(built, effort)
	}
	built["stream"] = true

	payload, err := json.Marshal(built)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ChatURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range p.Headers(acct, true) {
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
		return nil, extClassify(p, resp.StatusCode, string(head)+string(rest))
	}
	// A 200 that is an HTML page is a risk-control or error page, not a stream;
	// parsing it as SSE yields zero frames and reads as an empty answer.
	if isHTMLPage(string(head)) {
		return nil, &UpstreamError{Code: CodeServer, Message: p.Label + " 返回了 HTML 页面（可能被风控拦截）"}
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

// extPost walks a preset's account pool for one turn.
func (l *Lane) extPost(ctx context.Context, p *extPreset, body map[string]any, effort string, onData func([]byte) error) (*Usage, error) {
	accts := l.extSpendable(p.Kind)
	if len(accts) == 0 {
		return nil, &UpstreamError{Code: CodeCredential,
			Message: "还没有添加任何 " + p.Label + " 账号，请先在渠道页添加"}
	}
	emitted := false
	tracked := func(b []byte) error {
		emitted = true
		return onData(b)
	}
	var lastErr *UpstreamError
	for i, acct := range accts {
		if i >= extMaxAccountTries {
			break
		}
		usage, err := l.extChat(ctx, p, body, effort, acct, tracked)
		if err == nil {
			return usage, nil
		}
		ue := asUpstream(err)
		if ue == nil {
			return usage, err
		}
		lastErr = ue
		switch ue.Code {
		case CodeQuota, CodeCredential:
			l.logf("%s 账号 %s 不可用：%s", p.Label, acct.Label, truncate(ue.Message, 120))
		default:
			// A request-shaped defect answers identically on every account.
			return usage, ue
		}
		if emitted {
			return usage, ue
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &UpstreamError{Code: CodeQuota, Message: p.Label + " 账号都不可用"}
}

const extMaxAccountTries = 3

// codeForHTTP maps a bare status onto the lane vocabulary.
func codeForHTTP(status int) string {
	switch {
	case status == 401 || status == 403:
		return CodeCredential
	case status == 429:
		return CodeQuota
	case status == 402:
		return CodeQuota
	case status >= 500:
		return CodeServer
	default:
		return CodeClient
	}
}

// extClassify maps a preset-channel refusal onto the lane vocabulary. The
// content checks come first because several of these vendors answer quota and
// auth problems with a 400 or a 200-carrying body rather than a status code.
func extClassify(p *extPreset, status int, payload string) *UpstreamError {
	low := strings.ToLower(payload)
	msg := truncate(payload, 300)

	// Quota-shaped wording outranks the status code: these upstreams report a
	// spent balance as 400 or 402 and the two need opposite handling.
	for _, mark := range []string{
		"credit balance", "insufficient_quota", "insufficient balance",
		"quota exceeded", "out of credits", "积分不足", "额度已用完",
		"free limit", "daily free limit", "credits exhausted", "余额不足",
	} {
		if strings.Contains(low, mark) {
			return &UpstreamError{Code: CodeQuota, Message: msg, Scope: ScopeAccount}
		}
	}
	// Context overflow must NOT be read as quota: benching the account would
	// leave the user unable to serve anything until tomorrow.
	for _, mark := range []string{
		"context length", "context window", "maximum context",
		"too many tokens", "exceeds the maximum",
	} {
		if strings.Contains(low, mark) {
			return &UpstreamError{Code: CodeClient, Message: "上下文超出模型容量：" + msg}
		}
	}
	if strings.Contains(low, "not available in your region") ||
		strings.Contains(low, "region not supported") {
		return &UpstreamError{Code: CodeRegion, Message: msg}
	}

	switch codeForHTTP(status) {
	case CodeCredential:
		return &UpstreamError{Code: CodeCredential, Message: p.Label + " 登录态失效（HTTP " + itoa(status) + "）"}
	case CodeQuota:
		retry := 60
		if status == 402 {
			retry = 3600
		}
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: retry}
	case CodeServer:
		return &UpstreamError{Code: CodeServer, Message: msg}
	default:
		return &UpstreamError{Code: CodeClient, Message: msg}
	}
}
