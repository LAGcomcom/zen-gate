package lane

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// UpstreamBase is the gateway root; override for tests.
var UpstreamBase = "https://opencode.ai"

// ClientUA satisfies the gateway's User-Agent floor (>= 1.17).
const ClientUA = "opencode/1.18.31"

// FingerprintTools is the lowercase tool quartet the free tier requires.
var FingerprintTools = []string{"bash", "glob", "grep", "read"}

// quartetDonors maps a required slot to real tool names that can answer for it.
var quartetDonors = map[string][]string{"bash": {"pwsh"}}

var (
	responsesModels = map[string]bool{
		"muse-spark-1.2-contributor-free": true,
		"muse-spark-1.3-contributor-free": true,
	}
	messagesModels = map[string]bool{"union-alpha": true}
)

var (
	sessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	requestRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	museRe    = regexp.MustCompile(`(?i)^muse[-_]?spark(?:$|[-_:.\s])`)
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62From(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = base62[int(c)%62]
	}
	return string(out)
}

var lastStamp atomic.Int64
var seqCounter atomic.Int64

// MintSessionId mints a gateway-shaped canonical session id
// (time-prefixed, monotonic counter), byte for byte the shape a real client mints.
func MintSessionId(nowMS int64) string {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	prev := lastStamp.Load()
	if prev != nowMS {
		lastStamp.CompareAndSwap(prev, nowMS)
		seqCounter.Store(0)
	}
	seq := seqCounter.Add(1)
	value := ^(uint64(nowMS)<<12 | uint64(seq&0xFFF))
	var raw [6]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte((value >> (40 - 8*i)) & 0xFF)
	}
	rnd := make([]byte, 14)
	_, _ = rand.Read(rnd)
	return "ses_" + hex.EncodeToString(raw[:]) + base62From(rnd)
}

// MintRequestId mints a gateway-shaped per-turn request id.
func MintRequestId(nowMS int64) string {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	value := ^(uint64(nowMS)<<12 | 1)
	var raw [6]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte((value >> (40 - 8*i)) & 0xFF)
	}
	rnd := make([]byte, 14)
	_, _ = rand.Read(rnd)
	return "msg_" + hex.EncodeToString(raw[:]) + base62From(rnd)
}

// SessionForConversation maps one downstream conversation onto one stable
// upstream session. Free-tier quota is accounted per session, so a fresh id
// per request would exhaust it and surface as 429.
func SessionForConversation(seed string) string {
	seed = strings.TrimSpace(seed)
	if sessionRe.MatchString(seed) {
		return seed
	}
	if seed == "" {
		seed = "global"
	}
	sum := sha256.Sum256([]byte("zen-gate\x00" + seed))
	return "ses_" + hex.EncodeToString(sum[:6]) + base62From(sum[6:20])
}

// RequestIdFor derives a stable per-turn request id; retries of one turn share it.
func RequestIdFor(session, turnSeed string) string {
	if strings.TrimSpace(turnSeed) == "" {
		return MintRequestId(0)
	}
	sum := sha256.Sum256([]byte("zen-gate-req\x00" + session + "\x00" + turnSeed))
	id := "msg_" + hex.EncodeToString(sum[:6]) + base62From(sum[6:20])
	if !requestRe.MatchString(id) {
		return MintRequestId(0)
	}
	return id
}

// BaseModelId strips a trailing "(…)" effort suffix so lookups hit the base id.
func BaseModelId(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// EffortOf extracts a trailing "(light|balanced|deep)" suffix, if any.
func EffortOf(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				inner := strings.ToLower(strings.TrimSpace(s[i+1 : len(s)-1]))
				if inner == "light" || inner == "balanced" || inner == "deep" {
					return inner
				}
				return ""
			}
		}
	}
	return ""
}

func isMuseSpark(modelID string) bool {
	base := BaseModelId(modelID)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return museRe.MatchString(base)
}

// WireFor returns which upstream wire protocol serves this model.
func WireFor(modelID string) string {
	base := BaseModelId(modelID)
	if responsesModels[base] || isMuseSpark(base) {
		return "responses"
	}
	if messagesModels[base] {
		return "messages"
	}
	if isSystemOneModel(base) {
		return "systemone"
	}
	return "chat"
}

// SystemOneRe matches Jev-style "System One" decision models. They do not
// generate text: they evaluate a state against typed questions and answer
// with structured probabilities on their own endpoint — never chat.
var systemOneRe = regexp.MustCompile(`^jev`)

func isSystemOneModel(base string) bool { return systemOneRe.MatchString(base) }

// IsSystemOneModel reports whether the id is a decision model.
func IsSystemOneModel(modelID string) bool { return isSystemOneModel(BaseModelId(modelID)) }

// EndpointFor returns the upstream path for one model.
func EndpointFor(modelID string) string {
	switch WireFor(modelID) {
	case "responses":
		return "/zen/v1/responses"
	case "messages":
		return "/zen/v1/messages"
	case "systemone":
		return "/zen/v1/systemone"
	default:
		return "/zen/v1/chat/completions"
	}
}

// GatewayHeaders builds the header set the gateway fingerprints a genuine
// desktop client by. "Bearer public" is the pooled free credential — there is
// no per-user secret on this lane.
func GatewayHeaders(session, requestID string, stream bool, accept string) map[string]string {
	if accept == "" {
		if stream {
			accept = "text/event-stream"
		} else {
			accept = "*/*"
		}
	}
	return map[string]string{
		"content-type":       "application/json",
		"authorization":      "Bearer public",
		"user-agent":         ClientUA,
		"x-opencode-client":  "desktop",
		"x-opencode-session": session,
		"x-opencode-request": requestID,
		"x-opencode-project": "global",
		"accept":             accept,
	}
}

// toolNameOf reads a tool's declared name from either tool shape.
func toolNameOf(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	if n, ok := tool["name"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	return ""
}

func functionOf(tool map[string]any) map[string]any {
	fn, _ := tool["function"].(map[string]any)
	return fn
}

func quartetKey(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, t := range FingerprintTools {
		if t == lower {
			return lower
		}
	}
	return ""
}

// toolsOf normalizes body.tools from either []any or []map[string]any (Go
// builders produce the latter; decoded JSON the former).
func toolsOf(body map[string]any) []map[string]any {
	var out []map[string]any
	switch t := body["tools"].(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case []map[string]any:
		out = append(out, t...)
	}
	return out
}

// ApplyFingerprint satisfies the free-tier gate on body.tools: all four
// lowercase quartet names must be declared. A slot the caller already fields
// is canonicalised in place; one it does not field is filled by promoting a
// real tool that can answer for it (pwsh → bash), and only a slot with nothing
// to promote gets a self-disabling decoy. The returned map is
// sent-spelling → caller-spelling, so responses can restore real names.
//
// body is mutated in place. style: "chat" | "flat" (Responses) | "claude".
func ApplyFingerprint(body map[string]any, style string) map[string]string {
	claude := style == "claude"
	flat := style == "flat"
	renameMap := map[string]string{}
	tools := toolsOf(body)
	hadClientTools := len(tools) > 0

	seen := map[string]bool{}
	out := make([]any, 0, len(tools)+4)
	for _, tool := range tools {
		current := toolNameOf(tool)
		key := quartetKey(current)
		if key == "" {
			out = append(out, tool)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if current != key {
			renameMap[key] = current
			if fn := functionOf(tool); fn != nil {
				clone := map[string]any{"type": tool["type"], "function": cloneMap(fn)}
				clone["function"].(map[string]any)["name"] = key
				out = append(out, clone)
			} else {
				clone := cloneMap(tool)
				clone["name"] = key
				out = append(out, clone)
			}
		} else {
			out = append(out, tool)
		}
	}

	promoted := map[string]bool{}
	for _, name := range FingerprintTools {
		if seen[name] {
			continue
		}
		donors := quartetDonors[name]
		idx := -1
		for i, t := range out {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			orig := toolNameOf(tm)
			if orig == "" || quartetKey(orig) != "" {
				continue
			}
			lower := strings.ToLower(orig)
			if promoted[lower] || !contains(donors, lower) {
				continue
			}
			idx = i
			break
		}
		if idx == -1 {
			continue
		}
		tool := out[idx].(map[string]any)
		orig := toolNameOf(tool)
		promoted[strings.ToLower(orig)] = true
		renameMap[name] = orig
		if fn := functionOf(tool); fn != nil {
			clone := map[string]any{"type": tool["type"], "function": cloneMap(fn)}
			clone["function"].(map[string]any)["name"] = name
			out[idx] = clone
		} else {
			clone := cloneMap(tool)
			clone["name"] = name
			out[idx] = clone
		}
		seen[name] = true
	}

	for _, name := range FingerprintTools {
		if seen[name] {
			continue
		}
		desc := "This tool is currently unavailable and must not be used."
		switch {
		case claude:
			out = append(out, map[string]any{"name": name, "description": desc,
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}}})
		case flat:
			out = append(out, map[string]any{"type": "function", "name": name, "description": desc,
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
		default:
			out = append(out, map[string]any{"type": "function",
				"function": map[string]any{"name": name, "description": desc,
					"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
		}
	}

	body["tools"] = out
	if _, has := body["tool_choice"]; !has {
		switch {
		case flat:
			body["tool_choice"] = "auto"
		case !hadClientTools:
			if claude {
				body["tool_choice"] = map[string]any{"type": "none"}
			} else {
				body["tool_choice"] = "none"
			}
		}
	}
	return renameMap
}

// RestoreToolName maps a sent spelling back to the caller's.
func RestoreToolName(name string, rename map[string]string) string {
	if len(rename) == 0 {
		return name
	}
	if orig, ok := rename[name]; ok {
		return orig
	}
	return name
}

// DeclaredToolNames lists the tool names a request body declares.
func DeclaredToolNames(body map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, m := range toolsOf(body) {
		if n := toolNameOf(m); n != "" {
			names[n] = true
		}
	}
	return names
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
